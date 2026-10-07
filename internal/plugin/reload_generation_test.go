package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"bruce-go/internal/jsengine"
	"bruce-go/internal/tool"
)

const generationManifest = `{
  "apiVersion": "bruce.plugin/v1", "name": "generation", "version": "1.0.0",
  "description": "Reports which generation of its code served a call", "entry": "index.js",
  "concurrency": {"maxRuntimes": 1},
  "tools": [{"name": "generation_run", "description": "Report the generation", "handler": "run"}]
}`

// blockingGate lets a test hold an invocation inside a host function, so a
// reload can happen while that invocation is genuinely in flight.
type blockingGate struct {
	entered chan struct{}
	release chan struct{}
}

func newBlockingGate() *blockingGate {
	return &blockingGate{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *blockingGate) globals(source string) map[string]jsengine.GlobalObject {
	return map[string]jsengine.GlobalObject{
		"bruce": {
			"wait": jsengine.HostFunc(func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
				select {
				case g.entered <- struct{}{}:
				default:
				}
				select {
				case <-g.release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return json.Marshal(map[string]any{"source": source})
			}),
		},
	}
}

// TestReloadUsesGenerationsDeterministically is docs/plugin.md section 19: an
// invocation already in flight finishes on the code it started with, and the
// next invocation uses the new code.
//
// The documentation asserts this policy; this test is what makes the assertion
// checkable. A reload that instead mutated the live plugin would either fail
// the in-flight call or silently serve it the new code.
func TestReloadUsesGenerationsDeterministically(t *testing.T) {
	gate := newBlockingGate()
	fixture := newManagerFixture(t, permissivePolicy(), func(opts *ManagerOptions) {
		opts.ExtraGlobals = gate.globals("gen1")
	})
	fixture.write(t, pluginFixture{
		name: "generation", manifest: generationManifest,
		files: map[string]string{"index.js": `
export function run() {
  const host = bruce.wait();
  return "served-by:gen1:" + host.source;
}`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Start an invocation and wait until it is inside the host function, which
	// means the old runtime is genuinely busy.
	oldResult := make(chan tool.ExecutionOutcome, 1)
	go func() {
		oldResult <- fixture.registry.ExecuteResult(context.Background(), "generation_run", nil)
	}()
	select {
	case <-gate.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the first invocation never reached the host function")
	}

	// Reload while it is in flight, to different code.
	fixture.write(t, pluginFixture{
		name: "generation", manifest: generationManifest,
		files: map[string]string{"index.js": `
export function run() {
  const host = bruce.wait();
  return "served-by:gen2:" + host.source;
}`},
	})
	if err := fixture.manager.Reload(context.Background(), "generation"); err != nil {
		t.Fatalf("reload during an in-flight invocation failed: %v", err)
	}

	// Let the in-flight call finish.
	close(gate.release)
	var first tool.ExecutionOutcome
	select {
	case first = <-oldResult:
	case <-time.After(20 * time.Second):
		t.Fatal("the in-flight invocation never completed after the reload")
	}

	// The old invocation must have finished on the old code, not been
	// interrupted and not silently switched to the new code.
	if first.Status != tool.ToolCallSuccess {
		t.Fatalf("the in-flight invocation was not allowed to complete: %+v", first)
	}
	if !strings.Contains(first.Output, "served-by:gen1:") {
		t.Fatalf("the in-flight invocation did not finish on its own generation: %q", first.Output)
	}

	// The next invocation must use the new code.
	next := fixture.registry.ExecuteResult(context.Background(), "generation_run", nil)
	if next.Status != tool.ToolCallSuccess {
		t.Fatalf("the invocation after the reload failed: %+v", next)
	}
	if !strings.Contains(next.Output, "served-by:gen2:") {
		t.Fatalf("the invocation after the reload used the old code: %q", next.Output)
	}

	// And the plugin is at the new generation with a clean pool.
	plugins := fixture.manager.Plugins()
	if len(plugins) != 1 || plugins[0].Generation != 2 {
		t.Fatalf("plugins = %+v, want one plugin at generation 2", plugins)
	}
	if plugins[0].Runtimes.InFlight != 0 {
		t.Fatalf("a runtime is still marked in flight: %+v", plugins[0].Runtimes)
	}
}

// TestReloadDuringManyInFlightInvocations proves the policy holds under load:
// every in-flight call finishes on the generation it started with, and every
// later call uses the new one.
func TestReloadDuringManyInFlightInvocations(t *testing.T) {
	gate := newBlockingGate()
	fixture := newManagerFixture(t, permissivePolicy(), func(opts *ManagerOptions) {
		opts.ExtraGlobals = gate.globals("gen1")
	})
	fixture.write(t, pluginFixture{
		name: "generation", manifest: generationManifest,
		files: map[string]string{"index.js": `
export function run() { return "served-by:gen1:" + bruce.wait().source; }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	const callers = 8
	results := make(chan tool.ExecutionOutcome, callers)
	for range callers {
		go func() {
			results <- fixture.registry.ExecuteResult(context.Background(), "generation_run", nil)
		}()
	}
	// Wait for one to be inside the host function; the rest queue on the pool.
	select {
	case <-gate.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("no invocation reached the host function")
	}
	time.Sleep(50 * time.Millisecond)

	fixture.write(t, pluginFixture{
		name: "generation", manifest: generationManifest,
		files: map[string]string{"index.js": `
export function run() { return "served-by:gen2:" + bruce.wait().source; }`},
	})
	if err := fixture.manager.Reload(context.Background(), "generation"); err != nil {
		t.Fatalf("reload during in-flight invocations failed: %v", err)
	}
	close(gate.release)

	// Every call that was already running the old plugin finishes on it; the
	// ones that had not started yet run the new code. Either way no call may
	// fail, hang or report a mixed result.
	old, updated := 0, 0
	for range callers {
		select {
		case outcome := <-results:
			if outcome.Status != tool.ToolCallSuccess {
				t.Fatalf("an invocation failed across the reload: %+v", outcome)
			}
			switch {
			case strings.Contains(outcome.Output, "served-by:gen1:"):
				old++
			case strings.Contains(outcome.Output, "served-by:gen2:"):
				updated++
			default:
				t.Fatalf("an invocation reported an unknown generation: %q", outcome.Output)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("an invocation never returned across the reload")
		}
	}
	if old+updated != callers {
		t.Fatalf("accounted for %d of %d invocations", old+updated, callers)
	}
	if updated == 0 {
		t.Errorf("no invocation used the new generation after the reload")
	}
}
