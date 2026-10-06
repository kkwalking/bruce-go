package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bruce-go/internal/approval"
	"bruce-go/internal/jsengine"
	"bruce-go/internal/jsengine/moejs"
	"bruce-go/internal/sandbox"
	"bruce-go/internal/tool"
)

// pluginFixture describes a plugin to write to disk.
type pluginFixture struct {
	name     string
	manifest string
	files    map[string]string
	source   Source
}

// writeFixture writes a plugin under the right search root.
func writeFixture(t *testing.T, workspace, home string, fixture pluginFixture) {
	t.Helper()
	root := filepath.Join(workspace, ".bruce", "plugins")
	if fixture.source == SourceUser {
		root = filepath.Join(home, ".bruce", "plugins")
	}
	dir := filepath.Join(root, fixture.name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(fixture.manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for relative, content := range fixture.files {
		target := filepath.Join(dir, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// openSandbox reports a sandbox that can enforce a workspace-write mode, so
// capability checks can succeed in tests.
func openSandbox() func() SandboxStatus {
	return func() SandboxStatus {
		return SandboxStatus{
			Mode: string(sandbox.ModeWorkspaceWrite), NetworkAccess: true,
			Available: true, Backend: "test", Generation: 1,
		}
	}
}

type managerFixture struct {
	manager   *Manager
	registry  *tool.Registry
	workspace string
	home      string
	events    *eventRecorder
}

type eventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	kind   string
	fields map[string]any
}

func (r *eventRecorder) PluginEvent(kind string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{kind: kind, fields: fields})
}

func (r *eventRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, event := range r.events {
		out = append(out, event.kind)
	}
	return out
}

func (r *eventRecorder) has(kind string) bool {
	for _, candidate := range r.kinds() {
		if candidate == kind {
			return true
		}
	}
	return false
}

func (r *eventRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

// newManagerFixture builds a manager over a temporary workspace.
func newManagerFixture(t *testing.T, policy HostPolicy, mutate ...func(*ManagerOptions)) managerFixture {
	t.Helper()
	workspace := t.TempDir()
	home := t.TempDir()
	registry := tool.EmptyRegistry(workspace)
	recorder := &eventRecorder{}
	opts := ManagerOptions{
		Engine:    moejs.Engine{},
		Workspace: workspace,
		HomeDir:   home,
		Policy:    policy,
		Sandbox:   openSandbox(),
		Observer:  recorder,
	}
	for _, apply := range mutate {
		apply(&opts)
	}
	manager, err := NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	manager.WithRegistry(registry)
	t.Cleanup(func() { _ = manager.Close() })
	return managerFixture{manager: manager, registry: registry, workspace: workspace, home: home, events: recorder}
}

// permissivePolicy grants every permission a plugin declares. It is the
// "trusted deployment" setting used by most lifecycle tests; the security
// tests use the default deny-all policy instead.
func permissivePolicy() HostPolicy {
	return NewHostPolicy(Permissions()...)
}

const echoManifest = `{
  "apiVersion": "bruce.plugin/v1",
  "name": "echo",
  "version": "1.0.0",
  "description": "Echoes structured input back",
  "entry": "index.js",
  "tools": [
    {
      "name": "echo_run",
      "description": "Echo the arguments back",
      "handler": "run",
      "promptSnippet": "Echo structured input",
      "schema": {
        "type": "object",
        "properties": {
          "query": {"type": "string"},
          "options": {"type": "object"},
          "files": {"type": "array"},
          "nothing": {"type": "null"}
        },
        "required": ["query"]
      }
    }
  ]
}`

// minimalEchoManifest declares one handler with no schema, for lifecycle tests
// that are not about argument validation.
const minimalEchoManifest = `{
  "apiVersion": "bruce.plugin/v1",
  "name": "echo",
  "version": "1.0.0",
  "description": "Echoes",
  "entry": "index.js",
  "tools": [{"name": "echo_run", "description": "Echo", "handler": "run"}]
}`

const echoEntry = `export function run(input) {
  return {
    query: input.query,
    recursive: input.options ? input.options.recursive : null,
    depth: input.options ? input.options.depth : null,
    files: input.files || [],
    nothing: input.nothing === null ? "was-null" : "not-null",
  };
}`

func TestLoadRegistersToolInBruceRegistry(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	writeFixture(t, fixture.workspace, fixture.home, pluginFixture{
		name: "echo", manifest: echoManifest, files: map[string]string{"index.js": echoEntry},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	registered, ok := fixture.registry.Lookup("echo_run")
	if !ok {
		t.Fatalf("plugin tool is not in the registry; tools = %v", fixture.registry.ToolNames())
	}
	if registered.Policy.Source != tool.SourcePlugin {
		t.Errorf("Source = %q, want %q", registered.Policy.Source, tool.SourcePlugin)
	}

	// The tool must appear as an LLM tool definition, which is what makes it
	// selectable by the model.
	definitions := fixture.registry.Definitions()
	found := false
	for _, definition := range definitions {
		if definition.Name == "echo_run" {
			found = true
			if !strings.Contains(string(definition.Parameters), `"query"`) {
				t.Errorf("declared schema did not reach the tool definition: %s", definition.Parameters)
			}
		}
	}
	if !found {
		t.Fatalf("plugin tool is missing from Definitions(); got %d definitions", len(definitions))
	}
}

func TestPluginToolReceivesNestedJSONWithoutLoss(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	writeFixture(t, fixture.workspace, fixture.home, pluginFixture{
		name: "echo", manifest: echoManifest, files: map[string]string{"index.js": echoEntry},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := fixture.registry.ExecuteJSON(context.Background(), "echo_run",
		`{"query":"hello","options":{"recursive":true,"depth":3},"files":["a.go","b.go"],"nothing":null}`)

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result is not JSON: %v (%s)", err, out)
	}
	if got["query"] != "hello" {
		t.Errorf("query = %#v", got["query"])
	}
	if got["recursive"] != true {
		t.Errorf("nested boolean was lost: %#v", got["recursive"])
	}
	if got["depth"] != float64(3) {
		t.Errorf("nested number was lost: %#v", got["depth"])
	}
	files, ok := got["files"].([]any)
	if !ok || len(files) != 2 {
		t.Errorf("array was lost: %#v", got["files"])
	}
	if got["nothing"] != "was-null" {
		t.Errorf("null did not reach JavaScript: %#v", got["nothing"])
	}
}

func TestPluginToolReturnsStructuredError(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	writeFixture(t, fixture.workspace, fixture.home, pluginFixture{
		name: "boom",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "boom", "version": "1.0.0",
          "description": "Always fails", "entry": "index.js",
          "tools": [{"name": "boom_run", "description": "fail", "handler": "run"}]
        }`,
		files: map[string]string{"index.js": `export function run() { throw new Error("deliberate failure"); }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	outcome := fixture.registry.ExecuteResult(context.Background(), "boom_run", nil)
	if outcome.Status != tool.ToolCallFailed {
		t.Fatalf("status = %q, want failed", outcome.Status)
	}
	// The message must identify the plugin and the handler.
	for _, want := range []string{"boom", "run", "invoke", "deliberate failure"} {
		if !strings.Contains(outcome.Output, want) {
			t.Errorf("output %q is missing %q", outcome.Output, want)
		}
	}
	if !fixture.events.has(EventInvokeFailed) {
		t.Errorf("no failure event was published; events = %v", fixture.events.kinds())
	}
}

func TestPluginExceptionDoesNotCrashTheHost(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	writeFixture(t, fixture.workspace, fixture.home, pluginFixture{
		name: "boom",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "boom", "version": "1.0.0",
          "description": "Fails", "entry": "index.js",
          "tools": [{"name": "boom_run", "description": "fail", "handler": "run"}]
        }`,
		files: map[string]string{"index.js": `export function run() { throw new Error("boom"); }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Repeated failures must not degrade the host or the registry.
	for range 5 {
		outcome := fixture.registry.ExecuteResult(context.Background(), "boom_run", nil)
		if outcome.Status != tool.ToolCallFailed {
			t.Fatalf("status = %q", outcome.Status)
		}
	}
	if _, ok := fixture.registry.Lookup("boom_run"); !ok {
		t.Error("the tool disappeared after failures")
	}
}

func TestBrokenPluginDoesNotStopOthers(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	writeFixture(t, fixture.workspace, fixture.home, pluginFixture{
		name: "echo", manifest: echoManifest, files: map[string]string{"index.js": echoEntry},
	})
	// A plugin whose entry has a syntax error.
	writeFixture(t, fixture.workspace, fixture.home, pluginFixture{
		name: "broken",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "broken", "version": "1.0.0",
          "description": "Has a syntax error", "entry": "index.js",
          "tools": [{"name": "broken_run", "description": "x", "handler": "run"}]
        }`,
		files: map[string]string{"index.js": `export function run( {`},
	})
	// A plugin with an invalid manifest.
	writeFixture(t, fixture.workspace, fixture.home, pluginFixture{
		name:     "invalid",
		manifest: `{"apiVersion": "bruce.plugin/v1", "entry": "index.js"}`,
		files:    map[string]string{"index.js": `export function run() {}`},
	})

	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatalf("Load must not fail because one plugin is broken: %v", err)
	}
	if fixture.manager.Count() != 1 {
		t.Fatalf("loaded %d plugins, want 1", fixture.manager.Count())
	}
	if _, ok := fixture.registry.Lookup("echo_run"); !ok {
		t.Error("the healthy plugin was not loaded")
	}
	diagnostics := fixture.manager.Diagnostics()
	if len(diagnostics) < 2 {
		t.Fatalf("expected diagnostics for both broken plugins, got %d: %v", len(diagnostics), diagnostics)
	}
	if !fixture.events.has(EventLoadFailed) {
		t.Error("no load-failure event was published")
	}
}

func TestFailFastStopsStartup(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	writeFixture(t, workspace, home, pluginFixture{
		name:     "invalid",
		manifest: `{"apiVersion": "bruce.plugin/v1", "entry": "index.js"}`,
		files:    map[string]string{"index.js": `export function run() {}`},
	})
	manager, err := NewManager(ManagerOptions{
		Engine: moejs.Engine{}, Workspace: workspace, HomeDir: home,
		Policy: permissivePolicy(), Sandbox: openSandbox(), FailFast: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Load(context.Background()); err == nil {
		t.Fatal("FailFast must surface a broken manifest as an error")
	}
}

func TestToolApprovalFollowsDeclaredCapability(t *testing.T) {
	// A read-only tool must not ask for approval; a write tool must.
	fixture := newManagerFixture(t, permissivePolicy())
	writeFixture(t, fixture.workspace, fixture.home, pluginFixture{
		name: "io",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "io", "version": "1.0.0",
          "description": "Reads and writes", "entry": "index.js",
          "permissions": ["fs.read", "fs.write"],
          "tools": [
            {"name": "io_read", "description": "read", "handler": "read", "permissions": ["fs.read"]},
            {"name": "io_write", "description": "write", "handler": "write", "permissions": ["fs.write"]}
          ]
        }`,
		files: map[string]string{"index.js": `
export function read() { return "read"; }
export function write() { return "write"; }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	readTool, ok := fixture.registry.Lookup("io_read")
	if !ok {
		t.Fatal("io_read missing")
	}
	writeTool, _ := fixture.registry.Lookup("io_write")
	if readTool.Policy.NeedsApproval() {
		t.Error("a read-only tool must not require approval")
	}
	if !writeTool.Policy.NeedsApproval() {
		t.Error("a write tool must require approval")
	}
	if writeTool.Policy.MinimumMode != sandbox.ModeWorkspaceWrite {
		t.Errorf("write tool MinimumMode = %q", writeTool.Policy.MinimumMode)
	}
	if readTool.Policy.MinimumMode != sandbox.ModeReadOnly {
		t.Errorf("read tool MinimumMode = %q", readTool.Policy.MinimumMode)
	}
}

// TestToolGoesThroughHITL proves a plugin tool cannot bypass Bruce's approval
// path: with HITL enabled and rejecting, the handler must never run.
func TestToolGoesThroughHITL(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	writeFixture(t, fixture.workspace, fixture.home, pluginFixture{
		name: "io",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "io", "version": "1.0.0",
          "description": "Writes", "entry": "index.js",
          "permissions": ["fs.write"],
          "tools": [{"name": "io_write", "description": "write", "handler": "write"}]
        }`,
		files: map[string]string{"index.js": `export function write() { return "should-not-run"; }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.registry.WithHITL(approval.NewAutoHandler(true, approval.Reject("denied by the test")))
	outcome := fixture.registry.ExecuteResult(context.Background(), "io_write", nil)
	if outcome.Status != tool.ToolCallRejected {
		t.Fatalf("status = %q, want rejected", outcome.Status)
	}
	if strings.Contains(outcome.Output, "should-not-run") {
		t.Fatal("the handler ran despite the rejection")
	}
}

// TestToolGoesThroughSandboxPolicy proves the sandbox mode is enforced for a
// plugin tool exactly as it is for a built-in one.
func TestToolGoesThroughSandboxPolicy(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	manager, err := sandbox.New(context.Background(), sandbox.Options{
		Workspace: workspace, HomeDir: home, Mode: sandbox.ModeReadOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	registry := tool.EmptyRegistry(workspace)
	// The plugin is loaded while the sandbox can enforce a write, then the
	// sandbox is tightened before the call: the enforcement must happen at
	// call time against the live mode, not only at load time.
	pluginManager, err := NewManager(ManagerOptions{
		Engine: moejs.Engine{}, Workspace: workspace, HomeDir: home,
		Policy:  permissivePolicy(),
		Sandbox: openSandbox(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pluginManager.Close()
	pluginManager.WithRegistry(registry)
	registry.WithSandbox(manager)

	writeFixture(t, workspace, home, pluginFixture{
		name: "io",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "io", "version": "1.0.0",
          "description": "Writes", "entry": "index.js",
          "permissions": ["fs.write"],
          "tools": [{"name": "io_write", "description": "write", "handler": "write"}]
        }`,
		files: map[string]string{"index.js": `export function write() { return "ran"; }`},
	})
	if err := pluginManager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pluginManager.Count() != 1 {
		t.Fatalf("the plugin did not load: %v", pluginManager.Diagnostics())
	}
	if err := manager.SetMode(sandbox.ModeReadOnly); err != nil {
		t.Fatal(err)
	}
	// The plugin loaded, but read-only mode must refuse the write tool at call
	// time.
	outcome := registry.ExecuteResult(context.Background(), "io_write", nil)
	if outcome.Status == tool.ToolCallSuccess {
		t.Fatalf("read-only mode allowed a write tool: %+v", outcome)
	}
	if !strings.Contains(outcome.Output, "sandbox") {
		t.Errorf("output = %q, want a sandbox rejection", outcome.Output)
	}
}

// TestWriteCapabilityIsDeniedWhenSandboxCannotEnforce proves the fail-closed
// rule: a capability the host cannot enforce is refused at load time.
func TestWriteCapabilityIsDeniedWhenSandboxCannotEnforce(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	registry := tool.EmptyRegistry(workspace)
	manager, err := NewManager(ManagerOptions{
		Engine: moejs.Engine{}, Workspace: workspace, HomeDir: home,
		Policy: permissivePolicy(),
		Sandbox: func() SandboxStatus {
			return SandboxStatus{Mode: string(sandbox.ModeWorkspaceWrite), Available: false, Reason: "no backend"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.WithRegistry(registry)
	writeFixture(t, workspace, home, pluginFixture{
		name: "io",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "io", "version": "1.0.0",
          "description": "Writes", "entry": "index.js",
          "permissions": ["fs.write"],
          "tools": [{"name": "io_write", "description": "write", "handler": "write"}]
        }`,
		files: map[string]string{"index.js": `export function write() { return "ran"; }`},
	})
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.Count() != 0 {
		t.Fatal("a plugin whose capability cannot be enforced must not load")
	}
	if _, ok := registry.Lookup("io_write"); ok {
		t.Fatal("the tool was registered despite an unenforceable capability")
	}
	diagnostics := manager.Diagnostics()
	if len(diagnostics) == 0 || !strings.Contains(diagnostics[0].Message, "sandbox") {
		t.Fatalf("diagnostics = %v, want a sandbox reason", diagnostics)
	}
}

func TestReloadPicksUpChangedCodeWithoutDuplicates(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "echo", manifest: minimalEchoManifest,
		files: map[string]string{"index.js": `export function run() { return "first"; }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fixture.registry.Execute(context.Background(), "echo_run", nil); got != "first" {
		t.Fatalf("first result = %q", got)
	}

	fixture.write(t, pluginFixture{
		name: "echo", manifest: minimalEchoManifest,
		files: map[string]string{"index.js": `export function run() { return "second"; }`},
	})
	if err := fixture.manager.Reload(context.Background(), "echo"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := fixture.registry.Execute(context.Background(), "echo_run", nil); got != "second" {
		t.Fatalf("after reload result = %q, want \"second\"", got)
	}
	// No duplicate registration.
	count := 0
	for _, name := range fixture.registry.ToolNames() {
		if name == "echo_run" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("echo_run registered %d times", count)
	}
	if plugins := fixture.manager.Plugins(); len(plugins) != 1 || plugins[0].Generation != 2 {
		t.Fatalf("plugins = %+v, want one plugin at generation 2", plugins)
	}
}

func TestRepeatedReloadDoesNotAccumulateToolsOrRuntimes(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "echo", manifest: echoManifest,
		files: map[string]string{"index.js": echoEntry},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	baseline := len(fixture.registry.ToolNames())
	for i := range 10 {
		fixture.write(t, pluginFixture{
			name: "echo", manifest: minimalEchoManifest,
			files: map[string]string{"index.js": fmt.Sprintf(`export function run() { return %d; }`, i)},
		})
		if err := fixture.manager.Reload(context.Background(), "echo"); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	if got := len(fixture.registry.ToolNames()); got != baseline {
		t.Fatalf("tool count grew from %d to %d over ten reloads", baseline, got)
	}
	plugins := fixture.manager.Plugins()
	if len(plugins) != 1 {
		t.Fatalf("plugins = %d, want 1", len(plugins))
	}
	if plugins[0].Generation != 11 {
		t.Fatalf("generation = %d, want 11", plugins[0].Generation)
	}
	// The pool must hold at most one idle runtime after sequential reloads:
	// a leaked runtime from an old generation would show up here.
	if plugins[0].Runtimes.InFlight != 0 || plugins[0].Runtimes.Idle > 1 {
		t.Fatalf("runtime pool leaked: %+v", plugins[0].Runtimes)
	}
}

func TestReloadPicksUpManifestChanges(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "echo", manifest: echoManifest,
		files: map[string]string{"index.js": echoEntry},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Rename the tool and add a second one.
	fixture.write(t, pluginFixture{
		name: "echo",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "echo", "version": "2.0.0",
          "description": "Echoes structured input", "entry": "index.js",
          "tools": [
            {"name": "echo_renamed", "description": "Echo", "handler": "run"},
            {"name": "echo_extra", "description": "Echo again", "handler": "run"}
          ]
        }`,
		files: map[string]string{"index.js": echoEntry},
	})
	if err := fixture.manager.Reload(context.Background(), "echo"); err != nil {
		t.Fatal(err)
	}
	if _, ok := fixture.registry.Lookup("echo_run"); ok {
		t.Error("the removed tool is still registered")
	}
	for _, name := range []string{"echo_renamed", "echo_extra"} {
		if _, ok := fixture.registry.Lookup(name); !ok {
			t.Errorf("%s was not registered after the manifest change", name)
		}
	}
	if plugins := fixture.manager.Plugins(); plugins[0].Version != "2.0.0" {
		t.Errorf("version = %q", plugins[0].Version)
	}
}

func TestUnloadRemovesEverything(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "echo", manifest: echoManifest, files: map[string]string{"index.js": echoEntry},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.manager.Unload("echo")
	if _, ok := fixture.registry.Lookup("echo_run"); ok {
		t.Error("the tool survived unload")
	}
	if fixture.manager.Count() != 0 {
		t.Errorf("Count = %d", fixture.manager.Count())
	}
	if !fixture.events.has(EventUnloaded) {
		t.Error("no unload event")
	}
	// A deleted plugin must not come back on the next load.
	if err := os.RemoveAll(filepath.Join(fixture.workspace, ".bruce", "plugins", "echo")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.manager.Count() != 0 {
		t.Errorf("a deleted plugin was reloaded: %d", fixture.manager.Count())
	}
	if _, ok := fixture.registry.Lookup("echo_run"); ok {
		t.Error("the deleted plugin's tool is still registered")
	}
}

// TestPluginIsolation is docs/plugin.md section 31: one plugin's failure,
// reload or state must not affect another.
func TestPluginIsolation(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "alpha",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "alpha", "version": "1.0.0",
          "description": "Fails on demand", "entry": "index.js",
          "permissions": ["storage"],
          "tools": [
            {"name": "alpha_fail", "description": "fail", "handler": "fail"},
            {"name": "alpha_store", "description": "store", "handler": "store"},
            {"name": "alpha_load", "description": "load", "handler": "load"}
          ]
        }`,
		files: map[string]string{"index.js": `
import { set, get } from "bruce:storage";
export function fail() { throw new Error("alpha failed"); }
export function store(v) { set({ scope: "plugin", key: "shared", value: v }); return "stored"; }
export function load() { const r = get({ scope: "plugin", key: "shared" }); return r.found ? r.value : "absent"; }`},
	})
	fixture.write(t, pluginFixture{
		name: "beta",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "beta", "version": "1.0.0",
          "description": "Reads its own storage", "entry": "index.js",
          "permissions": ["storage"],
          "tools": [
            {"name": "beta_store", "description": "store", "handler": "store"},
            {"name": "beta_load", "description": "load", "handler": "load"}
          ]
        }`,
		files: map[string]string{"index.js": `
import { set, get } from "bruce:storage";
export function store(v) { set({ scope: "plugin", key: "shared", value: v }); return "stored"; }
export function load() { const r = get({ scope: "plugin", key: "shared" }); return r.found ? r.value : "absent"; }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A exception does not affect B.
	if out := fixture.registry.Execute(context.Background(), "alpha_fail", nil); !strings.Contains(out, "alpha failed") {
		t.Fatalf("alpha failure output = %q", out)
	}
	if got := fixture.registry.Execute(context.Background(), "beta_store", tool.Args{"v": "beta-value"}); got != "stored" {
		t.Fatalf("beta stopped working: %q", got)
	}

	// A's storage is not B's storage, even with the same key.
	fixture.registry.Execute(context.Background(), "alpha_store", tool.Args{"v": "alpha-value"})
	if got := fixture.registry.Execute(context.Background(), "beta_load", nil); strings.Contains(got, "alpha-value") {
		t.Fatalf("beta read alpha's storage: %q", got)
	}
	if got := fixture.registry.Execute(context.Background(), "alpha_load", nil); !strings.Contains(got, "alpha-value") {
		t.Fatalf("alpha lost its own value: %q", got)
	}

	// Reloading A does not disturb B.
	fixture.write(t, pluginFixture{
		name: "alpha",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "alpha", "version": "2.0.0",
          "description": "Reloaded", "entry": "index.js",
          "permissions": ["storage"],
          "tools": [{"name": "alpha_load", "description": "load", "handler": "load"}]
        }`,
		files: map[string]string{"index.js": `
import { get } from "bruce:storage";
export function load() { const r = get({ scope: "plugin", key: "shared" }); return r.found ? r.value : "absent"; }`},
	})
	if err := fixture.manager.Reload(context.Background(), "alpha"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := fixture.registry.Execute(context.Background(), "beta_load", nil); !strings.Contains(got, "beta-value") {
		t.Fatalf("beta broke after alpha reloaded: %q", got)
	}
	if _, ok := fixture.registry.Lookup("beta_store"); !ok {
		t.Error("beta's tool disappeared after alpha reloaded")
	}
}

// TestConcurrentInvocationsDoNotRace runs many calls at once through the tool
// registry, which is how the agent drives tools.
func TestConcurrentInvocationsDoNotRace(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "echo",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "echo", "version": "1.0.0",
          "description": "Echo", "entry": "index.js",
          "tools": [{"name": "echo_run", "description": "echo", "handler": "run"}]
        }`,
		files: map[string]string{"index.js": `
export function run(input) {
  let total = 0;
  for (let i = 0; i < input.n; i++) { total += i; }
  return { n: input.n, total };
}`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := range 40 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			out := fixture.registry.ExecuteJSON(context.Background(), "echo_run", fmt.Sprintf(`{"n":%d}`, n+1))
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				failures.Add(1)
				return
			}
			if got["n"] != float64(n+1) {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent invocations returned a wrong result", failures.Load())
	}
	plugins := fixture.manager.Plugins()
	if plugins[0].Runtimes.InFlight != 0 {
		t.Fatalf("runtimes still in flight after all calls returned: %+v", plugins[0].Runtimes)
	}
	if plugins[0].Runtimes.Idle > plugins[0].Runtimes.Capacity {
		t.Fatalf("pool over capacity: %+v", plugins[0].Runtimes)
	}
}

// TestCancellationStopsAPlugin proves ctx cancellation reaches JavaScript.
func TestCancellationStopsAPlugin(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "spin",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "spin", "version": "1.0.0",
          "description": "Loops forever", "entry": "index.js",
          "tools": [{"name": "spin_run", "description": "spin", "handler": "run"}]
        }`,
		files: map[string]string{"index.js": `export function run() { let i = 0; while (true) { i++; } }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan tool.ExecutionOutcome, 1)
	go func() {
		done <- fixture.registry.ExecuteResult(ctx, "spin_run", nil)
	}()
	select {
	case outcome := <-done:
		if outcome.Status != tool.ToolCallTimeout && outcome.Status != tool.ToolCallInterrupted {
			t.Fatalf("status = %q, want a cancellation status", outcome.Status)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancellation did not stop the plugin")
	}

	// The pool must still work afterwards.
	fixture.write(t, pluginFixture{
		name: "spin",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "spin", "version": "1.0.0",
          "description": "Loops forever", "entry": "index.js",
          "tools": [{"name": "spin_run", "description": "spin", "handler": "run"}]
        }`,
		files: map[string]string{"index.js": `export function run() { return "recovered"; }`},
	})
	if err := fixture.manager.Reload(context.Background(), "spin"); err != nil {
		t.Fatalf("Reload after cancellation: %v", err)
	}
	if got := fixture.registry.Execute(context.Background(), "spin_run", nil); got != "recovered" {
		t.Fatalf("after cancellation the pool returned %q", got)
	}
}

// TestCancelledInvocationDoesNotBlockLaterOnes proves a cancelled call returns
// its runtime to the pool.
func TestCancelledInvocationDoesNotBlockLaterOnes(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "mixed",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "mixed", "version": "1.0.0",
          "description": "Spins or returns", "entry": "index.js",
          "concurrency": {"maxRuntimes": 1},
          "tools": [
            {"name": "mixed_spin", "description": "spin", "handler": "spin"},
            {"name": "mixed_quick", "description": "quick", "handler": "quick"}
          ]
        }`,
		files: map[string]string{"index.js": `
export function spin() { let i = 0; while (true) { i++; } }
export function quick() { return "quick"; }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	outcome := fixture.registry.ExecuteResult(ctx, "mixed_spin", nil)
	cancel()
	if outcome.Status == tool.ToolCallSuccess {
		t.Fatalf("the spinning tool reported success: %+v", outcome)
	}
	// With a single runtime, a leaked runtime would make this hang.
	done := make(chan string, 1)
	go func() { done <- fixture.registry.Execute(context.Background(), "mixed_quick", nil) }()
	select {
	case got := <-done:
		if got != "quick" {
			t.Fatalf("result = %q", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the pool deadlocked after a cancelled invocation")
	}
}

// TestMissingHandlerInManifestFailsTheLoad turns a typo into a startup error.
func TestMissingHandlerInManifestFailsTheLoad(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "typo",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "typo", "version": "1.0.0",
          "description": "Names a handler that does not exist", "entry": "index.js",
          "tools": [{"name": "typo_run", "description": "x", "handler": "absent"}]
        }`,
		files: map[string]string{"index.js": `export function present() { return 1; }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.manager.Count() != 0 {
		t.Fatal("a plugin with a missing handler must not load")
	}
	if _, ok := fixture.registry.Lookup("typo_run"); ok {
		t.Fatal("the tool was registered despite the missing handler")
	}
	diagnostics := fixture.manager.Diagnostics()
	if len(diagnostics) == 0 || !strings.Contains(diagnostics[0].Message, "absent") {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
}

func TestWorkspacePluginOverridesUserPlugin(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "echo", manifest: minimalEchoManifest, source: SourceUser,
		files: map[string]string{"index.js": `export function run() { return "user"; }`},
	})
	fixture.write(t, pluginFixture{
		name: "echo", manifest: minimalEchoManifest, source: SourceWorkspace,
		files: map[string]string{"index.js": `export function run() { return "workspace"; }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fixture.registry.Execute(context.Background(), "echo_run", nil); got != "workspace" {
		t.Fatalf("result = %q, want the workspace plugin to win", got)
	}
	if overrides := fixture.manager.Overrides(); len(overrides) == 0 {
		t.Error("the override was not reported")
	}
}

func TestSchemaValidationRejectsMalformedArguments(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "echo", manifest: echoManifest,
		files: map[string]string{"index.js": `export function run(input) { return input.query; }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The declared schema requires query to be a string.
	outcome := fixture.registry.ExecuteResult(context.Background(), "echo_run", tool.Args{"query": 42})
	if outcome.Status == tool.ToolCallSuccess {
		t.Fatalf("a wrongly-typed argument was accepted: %+v", outcome)
	}
	if !strings.Contains(outcome.Output, "query") {
		t.Errorf("output = %q, want it to name the offending argument", outcome.Output)
	}
	// A missing required argument is rejected too.
	missing := fixture.registry.ExecuteResult(context.Background(), "echo_run", tool.Args{})
	if missing.Status == tool.ToolCallSuccess {
		t.Fatalf("a missing required argument was accepted: %+v", missing)
	}
	// And the valid call still works.
	if got := fixture.registry.Execute(context.Background(), "echo_run", tool.Args{"query": "ok"}); got != "ok" {
		t.Fatalf("valid call returned %q", got)
	}
}

func TestLoadIsIdempotent(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "echo", manifest: echoManifest, files: map[string]string{"index.js": echoEntry},
	})
	for range 3 {
		if err := fixture.manager.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.manager.Count() != 1 {
		t.Fatalf("Count = %d after three loads", fixture.manager.Count())
	}
	count := 0
	for _, name := range fixture.registry.ToolNames() {
		if name == "echo_run" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("echo_run registered %d times", count)
	}
}

func TestManagerCloseUnloadsEverything(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "echo", manifest: echoManifest, files: map[string]string{"index.js": echoEntry},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := fixture.registry.Lookup("echo_run"); ok {
		t.Error("Close left a tool registered")
	}
	// A closed manager refuses further loads rather than half-working.
	fixture.write(t, pluginFixture{
		name: "echo", manifest: echoManifest, files: map[string]string{"index.js": echoEntry},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatalf("Load after Close: %v", err)
	}
	if fixture.manager.Count() != 0 {
		t.Errorf("a closed manager loaded %d plugins", fixture.manager.Count())
	}
}

// TestLoadReportsObservabilityEvents covers the observability contract.
func TestLoadReportsObservabilityEvents(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "echo", manifest: echoManifest, files: map[string]string{"index.js": echoEntry},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.registry.Execute(context.Background(), "echo_run", tool.Args{"query": "x"})
	kinds := fixture.events.kinds()
	for _, want := range []string{EventLoaded, EventInvokeStart, EventInvokeDone} {
		found := false
		for _, kind := range kinds {
			if kind == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing event %q in %v", want, kinds)
		}
	}
}

func (f managerFixture) write(t *testing.T, fixture pluginFixture) {
	t.Helper()
	writeFixture(t, f.workspace, f.home, fixture)
}

var _ = errors.New
var _ jsengine.Engine = moejs.Engine{}

// TestDynamicCodeIsDisabledByDefault is a regression guard on a security
// default.
//
// The option was originally inverted, which silently left eval enabled for
// every caller that did not pass DisableDynamicCode. A host that forgets to set
// the option must still be safe, so the default is asserted directly rather
// than left to the caller's discipline.
func TestDynamicCodeIsDisabledByDefault(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "evaler",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "evaler", "version": "1.0.0",
          "description": "Tries to compile code at runtime", "entry": "index.js",
          "tools": [
            {"name": "evaler_eval", "description": "eval", "handler": "viaEval"},
            {"name": "evaler_ctor", "description": "Function", "handler": "viaFunction"}
          ]
        }`,
		files: map[string]string{"index.js": `
export function viaEval() { try { return "eval:" + eval("1+1"); } catch (e) { return "blocked:" + e.name; } }
export function viaFunction() { try { return "ctor:" + new Function("return 1")(); } catch (e) { return "blocked:" + e.name; } }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"evaler_eval", "evaler_ctor"} {
		out := fixture.registry.Execute(context.Background(), name, nil)
		if !strings.Contains(out, "blocked:EvalError") {
			t.Errorf("%s = %q, want dynamic code to be disabled by default", name, out)
		}
	}
}

// TestDynamicCodeCanBeEnabledExplicitly proves the option still works, so the
// default is a default and not a hard-coded refusal.
func TestDynamicCodeCanBeEnabledExplicitly(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	registry := tool.EmptyRegistry(workspace)
	allow := false
	manager, err := NewManager(ManagerOptions{
		Engine: moejs.Engine{}, Workspace: workspace, HomeDir: home,
		Policy: permissivePolicy(), Sandbox: openSandbox(),
		DisableDynamicCode: &allow,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.WithRegistry(registry)
	writeFixture(t, workspace, home, pluginFixture{
		name: "evaler",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "evaler", "version": "1.0.0",
          "description": "Compiles code", "entry": "index.js",
          "tools": [{"name": "evaler_eval", "description": "eval", "handler": "run"}]
        }`,
		files: map[string]string{"index.js": `
export function run() { return "eval:" + eval("1+1"); }`},
	})
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := registry.Execute(context.Background(), "evaler_eval", nil); got != "eval:2" {
		t.Fatalf("explicitly enabling dynamic code did not work: %q", got)
	}
}

// TestPluginHasNoHostGlobals is the documented capability boundary: a plugin
// runtime exposes no Node, no process, no filesystem and no network.
func TestPluginHasNoHostGlobals(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{
		name: "probe",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "probe", "version": "1.0.0",
          "description": "Probes the runtime globals", "entry": "index.js",
          "tools": [{"name": "probe_run", "description": "probe", "handler": "run"}]
        }`,
		files: map[string]string{"index.js": `
export function run() {
  return {
    require: typeof require, process: typeof process, os: typeof os,
    fetch: typeof fetch, Deno: typeof Deno, Buffer: typeof Buffer,
    XMLHttpRequest: typeof XMLHttpRequest, WebSocket: typeof WebSocket,
  };
}`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := fixture.registry.Execute(context.Background(), "probe_run", nil)
	for _, name := range []string{"require", "process", "os", "fetch", "Deno", "Buffer", "XMLHttpRequest", "WebSocket"} {
		if !strings.Contains(out, `"`+name+`": "undefined"`) {
			t.Errorf("the plugin runtime exposes %s: %s", name, out)
		}
	}
}

// TestStorageIsAbsentWithoutThePermission proves a host capability is not
// merely refused at call time: it is not installed at all.
func TestStorageIsAbsentWithoutThePermission(t *testing.T) {
	fixture := newManagerFixture(t, NewHostPolicy())
	fixture.write(t, pluginFixture{
		name: "nostorage",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "nostorage", "version": "1.0.0",
          "description": "Uses storage without asking", "entry": "index.js",
          "tools": [{"name": "nostorage_run", "description": "x", "handler": "run"}]
        }`,
		files: map[string]string{"index.js": `
export function run() {
  return typeof globalThis.bruce.storage;
}`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fixture.registry.Execute(context.Background(), "nostorage_run", nil); got != "undefined" {
		t.Fatalf("bruce.storage is present without the permission: %q", got)
	}
}

// TestRuntimeDiscardIsObservable proves a damaged runtime is reported as an
// event rather than silently replaced.
//
// A host function that panics is what corrupts a runtime in practice, so the
// fixture does exactly that and the test asserts the observation reaches the
// manager's event stream.
func TestRuntimeDiscardIsObservable(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy(), func(opts *ManagerOptions) {
		opts.ExtraGlobals = map[string]jsengine.GlobalObject{
			"bruce": {
				"panic": jsengine.HostFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
					panic("host bridge panic")
				}),
			},
		}
	})
	fixture.write(t, pluginFixture{
		name: "panicker",
		manifest: `{
          "apiVersion": "bruce.plugin/v1", "name": "panicker", "version": "1.0.0",
          "description": "Panics in a host function", "entry": "index.js",
          "tools": [
            {"name": "panicker_boom", "description": "panic", "handler": "boom"},
            {"name": "panicker_fine", "description": "works", "handler": "fine"}
          ]
        }`,
		files: map[string]string{"index.js": `
export function boom() { return bruce.panic(); }
export function fine() { return "fine"; }`},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	outcome := fixture.registry.ExecuteResult(context.Background(), "panicker_boom", nil)
	if outcome.Status == tool.ToolCallSuccess {
		t.Fatalf("a panicking host function reported success: %+v", outcome)
	}
	if !fixture.events.has(EventRuntimeRecycled) {
		t.Errorf("a discarded runtime was not reported; events = %v", fixture.events.kinds())
	}
	// The pool must have replaced the damaged runtime, so the next call works.
	if got := fixture.registry.Execute(context.Background(), "panicker_fine", nil); got != "fine" {
		t.Fatalf("the plugin did not recover after a corrupted runtime: %q", got)
	}
}
