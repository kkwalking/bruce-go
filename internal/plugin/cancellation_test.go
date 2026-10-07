package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"bruce-go/internal/jsengine"
	"bruce-go/internal/jsengine/moejs"
	"bruce-go/internal/tool"
)

// cancellationPlugin is a plugin whose handlers exercise the cancellation
// matrix from docs/plugin.md section 13: an infinite loop, a long but bounded
// computation, and an await.
const cancellationManifest = `{
  "apiVersion": "bruce.plugin/v1", "name": "cancel", "version": "1.0.0",
  "description": "Cancellation fixtures", "entry": "index.js",
  "concurrency": {"maxRuntimes": 2},
  "tools": [
    {"name": "cancel_loop", "description": "Loop forever", "handler": "loop"},
    {"name": "cancel_compute", "description": "Long computation", "handler": "compute"},
    {"name": "cancel_await", "description": "Await a promise", "handler": "awaitIt"},
    {"name": "cancel_quick", "description": "Return immediately", "handler": "quick"}
  ]
}`

const cancellationEntry = `
export function loop() { let i = 0; while (true) { i++; } }
export function compute() {
  // Bounded but long: it must be stoppable before it finishes.
  let total = 0;
  for (let i = 0; i < 1e12; i++) { total += i; }
  return total;
}
export async function awaitIt() {
  // A realistic async handler: it awaits a host capability. The host call
  // blocks until the context ends, which is exactly how an async plugin is
  // cancelled.
  return await bruce.slow();
}
export function neverSettles() { return new Promise(() => {}); }
export function quick() { return "quick"; }
`

// The cancellation fixture needs the host capability the async handler awaits.
func cancellationGlobals() map[string]jsengine.GlobalObject {
	return map[string]jsengine.GlobalObject{
		"bruce": {
			"slow": jsengine.HostFunc(func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}),
		},
	}
}

func newCancellationFixture(t *testing.T) managerFixture {
	t.Helper()
	fixture := newManagerFixture(t, permissivePolicy(), func(opts *ManagerOptions) {
		// The async fixture awaits this host capability.
		opts.ExtraGlobals = cancellationGlobals()
	})
	fixture.write(t, pluginFixture{
		name: "cancel", manifest: cancellationManifest,
		files: map[string]string{"index.js": cancellationEntry},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cancel_loop", "cancel_compute", "cancel_await", "cancel_quick"} {
		if _, ok := fixture.registry.Lookup(name); !ok {
			t.Fatalf("tool %s was not registered: %v", name, fixture.manager.Diagnostics())
		}
	}
	return fixture
}

// runCancellable starts a tool call and returns a channel with its outcome.
func runCancellable(ctx context.Context, fixture managerFixture, name string) <-chan tool.ExecutionOutcome {
	out := make(chan tool.ExecutionOutcome, 1)
	go func() { out <- fixture.registry.ExecuteResult(ctx, name, nil) }()
	return out
}

// awaitOutcome waits for a cancellation result within a generous bound.
func awaitOutcome(t *testing.T, name string, out <-chan tool.ExecutionOutcome) tool.ExecutionOutcome {
	t.Helper()
	select {
	case outcome := <-out:
		return outcome
	case <-time.After(30 * time.Second):
		t.Fatalf("%s: cancellation never returned; the plugin call is stuck", name)
		return tool.ExecutionOutcome{}
	}
}

// TestCancellationMatrix covers the four fixtures docs/plugin.md section 29
// requires: an infinite loop, a long computation, an async handler, and a
// cancelled tool invocation.
func TestCancellationMatrix(t *testing.T) {
	cases := []struct {
		name string
		tool string
	}{
		{"infinite loop", "cancel_loop"},
		{"long computation", "cancel_compute"},
		{"await", "cancel_await"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCancellationFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			start := time.Now()
			outcome := awaitOutcome(t, testCase.name, runCancellable(ctx, fixture, testCase.tool))

			// ctx cancel must produce an explicit cancellation error, not a
			// success and not a hang.
			if outcome.Status != tool.ToolCallTimeout && outcome.Status != tool.ToolCallInterrupted {
				t.Fatalf("status = %q, want a cancellation status; output = %q", outcome.Status, outcome.Output)
			}
			if elapsed := time.Since(start); elapsed > 20*time.Second {
				t.Fatalf("cancellation took %s", elapsed)
			}
			// Bruce must not be stuck: the pool still serves the next call.
			next, cancelNext := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancelNext()
			recovered := awaitOutcome(t, "recovery", runCancellable(next, fixture, "cancel_quick"))
			if recovered.Status != tool.ToolCallSuccess || recovered.Output != "quick" {
				t.Fatalf("the pool did not recover after cancellation: %+v", recovered)
			}
		})
	}
}

// TestCancellationRaceWithCompletion cancels at the same moment the call
// finishes. Either outcome is acceptable, but neither a lost result nor a
// poisoned runtime is.
func TestCancellationRaceWithCompletion(t *testing.T) {
	fixture := newCancellationFixture(t)
	for i := range 30 {
		ctx, cancel := context.WithCancel(context.Background())
		out := runCancellable(ctx, fixture, "cancel_quick")
		// Cancel immediately: sometimes before the call starts, sometimes
		// during it, sometimes after it finished.
		cancel()
		outcome := awaitOutcome(t, "race", out)
		switch outcome.Status {
		case tool.ToolCallSuccess, tool.ToolCallInterrupted, tool.ToolCallTimeout:
		default:
			t.Fatalf("iteration %d: status = %q, output = %q", i, outcome.Status, outcome.Output)
		}
	}
	// After the races, the pool must still work.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	outcome := awaitOutcome(t, "post-race", runCancellable(ctx, fixture, "cancel_quick"))
	if outcome.Status != tool.ToolCallSuccess {
		t.Fatalf("the pool was left unusable after cancellation races: %+v", outcome)
	}
}

// TestCancellationUnderConcurrency cancels some calls while others run, and
// proves the survivors are unaffected.
func TestCancellationUnderConcurrency(t *testing.T) {
	fixture := newCancellationFixture(t)
	var wg sync.WaitGroup
	results := make([]tool.ExecutionOutcome, 24)
	for i := range 24 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if index%2 == 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
				defer cancel()
				results[index] = fixture.registry.ExecuteResult(ctx, "cancel_loop", nil)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			results[index] = fixture.registry.ExecuteResult(ctx, "cancel_quick", nil)
		}(i)
	}
	wg.Wait()

	for i, outcome := range results {
		if i%2 == 0 {
			if outcome.Status != tool.ToolCallTimeout && outcome.Status != tool.ToolCallInterrupted {
				t.Errorf("cancelled call %d: status = %q", i, outcome.Status)
			}
			continue
		}
		if outcome.Status != tool.ToolCallSuccess || outcome.Output != "quick" {
			t.Errorf("concurrent call %d was affected by the cancellations: %+v", i, outcome)
		}
	}
}

// TestNeverSettlingPromiseIsReportedNotSilentlyEmpty is a regression guard: a
// promise the host cannot settle used to serialize as "{}", which the model
// would read as a successful empty result for work that never happened.
func TestNeverSettlingPromiseIsReportedNotSilentlyEmpty(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "pending",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "pending", "version": "1.0.0",
          "description": "Returns a promise that never settles", "entry": "index.js",
          "tools": [{"name": "pending_run", "description": "pending", "handler": "run"}]
        }`,
		files: map[string]string{"index.js": `export function run() { return new Promise(() => {}); }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	outcome := fixture.registry.ExecuteResult(context.Background(), "pending_run", nil)
	if outcome.Status == tool.ToolCallSuccess {
		t.Fatalf("a never-settling promise was reported as success: %+v", outcome)
	}
	if strings.TrimSpace(outcome.Output) == "{}" {
		t.Fatal("a never-settling promise was reported as an empty object")
	}
	if !strings.Contains(outcome.Output, "never settled") {
		t.Fatalf("output = %q, want an explanation", outcome.Output)
	}
}

// TestAsyncHandlerResolves proves a normal async handler works, so the promise
// handling is not a blanket refusal of async code.
func TestAsyncHandlerResolves(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy(), func(opts *ManagerOptions) {
		opts.ExtraGlobals = map[string]jsengine.GlobalObject{
			"bruce": {
				"double": jsengine.HostFunc(func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
					var parsed struct {
						N int `json:"n"`
					}
					if err := json.Unmarshal(args, &parsed); err != nil {
						return nil, err
					}
					return json.Marshal(map[string]any{"doubled": parsed.N * 2})
				}),
			},
		}
	})
	fixture.write(t, pluginFixture{
		name: "async",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "async", "version": "1.0.0",
          "description": "Async handler", "entry": "index.js",
          "tools": [{"name": "async_run", "description": "async", "handler": "run"}]
        }`,
		files: map[string]string{"index.js": `
export async function run(input) {
  const result = await bruce.double({ n: input.n });
  return { doubled: result.doubled, source: "async" };
}`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := fixture.registry.ExecuteJSON(context.Background(), "async_run", `{"n":21}`)
	if !strings.Contains(out, `"doubled": 42`) || !strings.Contains(out, `"source": "async"`) {
		t.Fatalf("an async handler did not resolve: %q", out)
	}
}

// TestCancellationLeavesNoGoroutineLeak proves the interrupt watcher always
// terminates: a leaked watcher per call would show up as unbounded growth.
func TestCancellationLeavesNoGoroutineLeak(t *testing.T) {
	engine := moejs.Engine{}
	module, err := engine.Compile("plugin.js", `export function quick() { return "ok"; }`)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := module.Handler("quick")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := jsengine.NewPooledModule(engine, module, jsengine.RuntimeOptions{DisableDynamicCode: true}, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Warm up so the pool's steady state is reached before measuring.
	for range 10 {
		if _, err := pool.Invoke(context.Background(), handler, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	baseline := runtimeGoroutines()
	for range 200 {
		if _, err := pool.Invoke(context.Background(), handler, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	after := runtimeGoroutines()
	if after > baseline+20 {
		t.Fatalf("goroutines grew from %d to %d over 200 invocations", baseline, after)
	}

	// Cancelled invocations must not leak either.
	baseline = runtimeGoroutines()
	for range 50 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		_, _ = pool.Invoke(ctx, handler, []byte(`{}`))
		cancel()
	}
	after = runtimeGoroutines()
	if after > baseline+20 {
		t.Fatalf("goroutines grew from %d to %d over 50 cancelled invocations", baseline, after)
	}
}

// runtimeGoroutines reports the current goroutine count.
func runtimeGoroutines() int {
	runtime.GC()
	return runtime.NumGoroutine()
}

// TestCancellationErrorIsClassified proves a cancelled plugin call is
// reported as a cancellation, so a caller can tell it apart from a plugin bug.
func TestCancellationErrorIsClassified(t *testing.T) {
	fixture := newCancellationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	outcome := awaitOutcome(t, "classify", runCancellable(ctx, fixture, "cancel_loop"))
	if outcome.Status != tool.ToolCallTimeout {
		t.Fatalf("status = %q, want timeout", outcome.Status)
	}
	if !strings.Contains(outcome.Output, "timed out") && !strings.Contains(outcome.Output, "canceled") {
		t.Errorf("output = %q, want a clear cancellation message", outcome.Output)
	}
	// A cancelled call must be reported through the observation channel too.
	if !fixture.events.has(EventTimeout) && !fixture.events.has(EventCancellation) {
		t.Errorf("no timeout or cancellation event was published: %v", fixture.events.kinds())
	}
}

// TestShutdownCancellationStopsPlugins covers the process-shutdown path: the
// root context ending must stop a running plugin just like a user Ctrl-C.
func TestShutdownCancellationStopsPlugins(t *testing.T) {
	fixture := newCancellationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	out := runCancellable(ctx, fixture, "cancel_loop")
	time.Sleep(100 * time.Millisecond)
	cancel()
	outcome := awaitOutcome(t, "shutdown", out)
	if outcome.Status != tool.ToolCallInterrupted && outcome.Status != tool.ToolCallTimeout {
		t.Fatalf("shutdown did not interrupt the plugin: %+v", outcome)
	}
	// Closing the manager after a cancelled call must not hang or panic.
	fixture.manager.Close()
}

// TestInvocationTimeoutAppliesWithoutCallerDeadline proves a plugin cannot run
// forever just because its caller passed a context without a deadline.
func TestInvocationTimeoutAppliesWithoutCallerDeadline(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "slow",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "slow", "version": "1.0.0",
          "description": "Loops forever", "entry": "index.js",
          "tools": [{"name": "slow_run", "description": "loop", "handler": "run", "timeoutMs": 200}]
        }`,
		files: map[string]string{"index.js": `export function run() { let i = 0; while (true) { i++; } }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	// No deadline on the caller's context: the declared tool timeout must be
	// the bound. Both layers enforce it -- tool.Policy.Timeout in the registry
	// and the plugin manager's own invocation bound -- and this asserts the
	// observable result, which is that the call does not run for 30 seconds.
	start := time.Now()
	out := runCancellable(context.Background(), fixture, "slow_run")
	outcome := awaitOutcome(t, "declared timeout", out)
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("the declared 200ms tool timeout did not apply: took %s", elapsed)
	}
	if outcome.Status != tool.ToolCallTimeout && outcome.Status != tool.ToolCallInterrupted {
		t.Fatalf("status = %q, want a cancellation status", outcome.Status)
	}
}

// TestDeclaredTimeoutReachesTheRegistryPolicy proves the manifest's timeoutMs
// lands in the tool policy the registry enforces, so the two layers cannot
// drift apart.
func TestDeclaredTimeoutReachesTheRegistryPolicy(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "timed",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "timed", "version": "1.0.0",
          "description": "Declares a timeout", "entry": "index.js",
          "tools": [{"name": "timed_run", "description": "x", "handler": "run", "timeoutMs": 1234}]
        }`,
		files: map[string]string{"index.js": `export function run() { return "ok"; }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	registered, ok := fixture.registry.Lookup("timed_run")
	if !ok {
		t.Fatal("the tool was not registered")
	}
	if registered.Policy.Timeout != 1234*time.Millisecond {
		t.Fatalf("Policy.Timeout = %v, want 1234ms", registered.Policy.Timeout)
	}
}

// TestMalformedResultIsReported proves a handler returning something that is
// not JSON is reported as a malformed result rather than reaching the model.
func TestMalformedResultIsReported(t *testing.T) {
	engine := moejs.Engine{}
	module, err := engine.Compile("plugin.js", `export function bad() { return { toJSON() { throw new Error("cannot serialize"); } }; }`)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := module.Handler("bad")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := engine.NewRuntime(jsengine.RuntimeOptions{DisableDynamicCode: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	_, err = rt.Call(context.Background(), handler, []byte(`{}`))
	if err == nil {
		t.Fatal("a throwing toJSON must be reported as an error")
	}
	if kind, _ := jsengine.KindOf(err); kind != jsengine.KindException {
		t.Fatalf("Kind = %v, want exception (err = %v)", kind, err)
	}
	var engineErr *jsengine.EngineError
	if !errors.As(err, &engineErr) {
		t.Fatal("the error is not an EngineError")
	}
	if !strings.Contains(engineErr.Message, "cannot serialize") {
		t.Errorf("message = %q", engineErr.Message)
	}
}

// TestPanicInHostFunctionDoesNotCrashBruce proves the host bridge contains a
// panicking host function.
func TestPanicInHostFunctionDoesNotCrashBruce(t *testing.T) {
	engine := moejs.Engine{}
	module, err := engine.Compile("plugin.js", `
export function boom() { return globalThis.bruce.host.panic(); }
export function fine() { return "fine"; }`)
	if err != nil {
		t.Fatal(err)
	}
	globals := map[string]jsengine.GlobalObject{
		"bruce": {"host": jsengine.GlobalObject{
			"panic": jsengine.HostFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				panic("host bridge panic")
			}),
		}},
	}
	rt, err := engine.NewRuntime(jsengine.RuntimeOptions{DisableDynamicCode: true, Globals: globals})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	boom, _ := module.Handler("boom")
	_, err = rt.Call(context.Background(), boom, []byte(`{}`))
	if err == nil {
		t.Fatal("a panicking host function must surface as an error")
	}
	// A Go panic that escaped the engine leaves the runtime unsafe, so the
	// pool must discard it. The classification is what drives that decision.
	if kind, _ := jsengine.KindOf(err); kind != jsengine.KindInternal {
		t.Fatalf("Kind = %v, want internal (err = %v)", kind, err)
	}
	if !jsengine.IsCorrupted(err) {
		t.Error("a panic through the engine must be classified as corrupting the runtime")
	}
}
