package integrated

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bruce-go/internal/approval"
	"bruce-go/internal/config"
	"bruce-go/internal/llm"
	"bruce-go/internal/runtime"
	"bruce-go/internal/tool"
)

// pluginWorkspace writes a workspace plugin and a settings file that grants it
// the permissions it asks for.
type pluginWorkspace struct {
	workspace string
	home      string
	settings  string
}

const acceptancePluginManifest = `{
  "apiVersion": "bruce.plugin/v1",
  "name": "acceptance",
  "version": "1.0.0",
  "description": "Acceptance-test plugin",
  "entry": "index.js",
  "permissions": ["fs.read"],
  "tools": [
    {
      "name": "acceptance_echo",
      "description": "Echo the arguments back, including nested structure",
      "handler": "run",
      "promptSnippet": "Echo structured input for the acceptance test",
      "schema": {
        "type": "object",
        "properties": {
          "query": {"type": "string"},
          "options": {"type": "object"},
          "files": {"type": "array"},
          "nothing": {"type": "null"},
          "count": {"type": "integer"},
          "flag": {"type": "boolean"}
        },
        "required": ["query"]
      }
    }
  ],
  "commands": [
    {"name": "acceptance-report", "description": "Print a report", "handler": "report"}
  ]
}`

const acceptancePluginEntry = `export function run(input) {
  return {
    query: input.query,
    recursive: input.options ? input.options.recursive : null,
    depth: input.options ? input.options.depth : null,
    files: input.files || [],
    nothing: input.nothing === null ? "was-null" : "not-null",
    count: input.count,
    flag: input.flag,
  };
}
export function report() { return "acceptance report"; }
`

func writeAcceptanceWorkspace(t *testing.T, manifest, entry string, allow []string) pluginWorkspace {
	t.Helper()
	workspace := t.TempDir()
	home := t.TempDir()
	dir := filepath.Join(workspace, ".bruce", "plugins", "acceptance")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(home, "setting.json")
	settings := config.DefaultSettings()
	settings.Plugins.Allow = allow
	settings.Sandbox.Mode = "full-access"
	if err := config.NewLoader(settingsPath).Save(settings); err != nil {
		t.Fatal(err)
	}
	return pluginWorkspace{workspace: workspace, home: home, settings: settingsPath}
}

func newAcceptanceRuntime(t *testing.T, environment pluginWorkspace, client llm.ChatClient) *Runtime {
	t.Helper()
	rt, err := New(context.Background(), Options{
		Workspace:    environment.workspace,
		HomeDir:      environment.home,
		SettingsPath: environment.settings,
		Client:       client,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// TestPluginAcceptanceMVP walks the seventeen acceptance criteria of
// docs/plugin.md section 35 through the real runtime.
func TestPluginAcceptanceMVP(t *testing.T) {
	environment := writeAcceptanceWorkspace(t, acceptancePluginManifest, acceptancePluginEntry, []string{"fs.read"})

	// 5. The model can select the tool: the fake client asks for it by name.
	client := &scriptedClient{responses: []llm.ChatResponse{
		{ToolCalls: []llm.ToolCall{{
			ID: "call-1",
			Function: llm.FunctionCall{
				Name: "acceptance_echo",
				// 6. Nested JSON arguments.
				Arguments: `{"query":"hello","options":{"recursive":true,"depth":3},"files":["a.go","b.go"],"nothing":null,"count":7,"flag":false}`,
			},
		}}},
		{Content: "done"},
	}}
	rt := newAcceptanceRuntime(t, environment, client)

	// 1. The plugin was discovered automatically.
	if rt.Plugins == nil {
		t.Fatal("criterion 1: the plugin manager was not created")
	}
	if rt.Plugins.Count() != 1 {
		t.Fatalf("criterion 1/2: loaded %d plugins, want 1 (diagnostics: %v)", rt.Plugins.Count(), rt.Plugins.Diagnostics())
	}

	// 2. The manifest was validated: a rejection would have produced a
	// diagnostic and no loaded plugin, so a loaded plugin with the declared
	// identity is the observable form of "the manifest passed validation".
	statuses := rt.Plugins.Plugins()
	if statuses[0].Name != "acceptance" || statuses[0].Version != "1.0.0" {
		t.Fatalf("criterion 2: status = %+v", statuses[0])
	}
	for _, diagnostic := range rt.Plugins.Diagnostics() {
		t.Errorf("criterion 2: manifest validation produced a diagnostic: %s", diagnostic.String())
	}
	// 3. The JS module loaded: the plugin would have been skipped otherwise,
	// and its handler would not resolve in a real runtime.
	if got := rt.Plugins.Count(); got != 1 {
		t.Fatalf("criterion 3: the module did not load (%d plugins)", got)
	}

	// 4. The tool is in Bruce's registry.
	if _, ok := rt.Tools.Lookup("acceptance_echo"); !ok {
		t.Fatalf("criterion 4: tool not registered; tools = %v", rt.Tools.ToolNames())
	}
	// ...and it is offered to the model as a tool definition.
	definitions := rt.Tools.Definitions()
	found := false
	for _, definition := range definitions {
		if definition.Name == "acceptance_echo" {
			found = true
		}
	}
	if !found {
		t.Fatal("criterion 5: the plugin tool is missing from the LLM tool definitions")
	}

	// Run a real agent turn so the tool is invoked through the agent.
	out, err := rt.RunTask(context.Background(), "echo something")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("agent output = %q", out)
	}
	if client.calls < 2 {
		t.Fatalf("criterion 5: the model was not given the tool result (calls=%d)", client.calls)
	}

	// 6/7/8: the tool received nested JSON, executed through the engine, and
	// its return value reached the agent.
	var toolResult string
	for _, message := range rt.react.History {
		if message.Role == llm.RoleTool {
			toolResult = message.Content
		}
	}
	if toolResult == "" {
		t.Fatal("criterion 8: no tool result reached the agent")
	}
	// 7. The tool executed through the JavaScript engine, not through a
	// Go-side shortcut. The marker below exists only because the plugin's own
	// JavaScript ran: the handler computes it from the arguments, so the host
	// could not have produced it.
	if !strings.Contains(toolResult, "was-null") {
		t.Fatalf("criterion 7: the plugin's JavaScript did not run; result = %q", toolResult)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(toolResult), &payload); err != nil {
		t.Fatalf("criterion 8: the tool result is not JSON: %v (%s)", err, toolResult)
	}
	if payload["query"] != "hello" {
		t.Errorf("criterion 6: query = %#v", payload["query"])
	}
	if payload["recursive"] != true {
		t.Errorf("criterion 6: nested boolean was lost: %#v", payload["recursive"])
	}
	if payload["depth"] != float64(3) {
		t.Errorf("criterion 6: nested number was lost: %#v", payload["depth"])
	}
	if payload["count"] != float64(7) {
		t.Errorf("criterion 6: integer was lost: %#v", payload["count"])
	}
	if payload["flag"] != false {
		t.Errorf("criterion 6: boolean was lost: %#v", payload["flag"])
	}
	if files, ok := payload["files"].([]any); !ok || len(files) != 2 {
		t.Errorf("criterion 6: array was lost: %#v", payload["files"])
	}
	if payload["nothing"] != "was-null" {
		t.Errorf("criterion 6: null did not reach JavaScript: %#v", payload["nothing"])
	}

	// 13. A plugin exception must not crash the host: force one and keep going.
	// The replacement entry is a valid module whose handler throws.
	failingEntry := `export function run() { throw new Error("acceptance failure"); }
export function report() { return "acceptance report"; }
`
	if err := os.WriteFile(filepath.Join(environment.workspace, ".bruce", "plugins", "acceptance", "index.js"),
		[]byte(failingEntry), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rt.Plugins.Reload(context.Background(), "acceptance"); err != nil {
		t.Fatalf("criterion 14: reload failed: %v", err)
	}
	// 14. The reload took effect: the new code throws, and the host reports it
	// as a tool failure instead of crashing.
	failed := rt.Tools.ExecuteResult(context.Background(), "acceptance_echo", tool.Args{"query": "after-reload"})
	if failed.Status != tool.ToolCallFailed {
		t.Fatalf("criterion 14: the reloaded code did not take effect: %+v", failed)
	}
	if !strings.Contains(failed.Output, "acceptance failure") {
		t.Fatalf("criterion 14: output = %q", failed.Output)
	}
	// 13. The host survived the plugin exception and still serves other calls.
	if _, ok := rt.Tools.Lookup("acceptance_echo"); !ok {
		t.Fatal("criterion 13: the tool disappeared after a plugin exception")
	}
	// Reload back to the working entry for the remaining checks.
	if err := os.WriteFile(filepath.Join(environment.workspace, ".bruce", "plugins", "acceptance", "index.js"),
		[]byte(acceptancePluginEntry), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rt.Plugins.Reload(context.Background(), "acceptance"); err != nil {
		t.Fatalf("criterion 14: second reload failed: %v", err)
	}
	if got := rt.Tools.Execute(context.Background(), "acceptance_echo", tool.Args{"query": "after-reload"}); !strings.Contains(got, "after-reload") {
		t.Fatalf("criterion 14: reloaded plugin returned %q", got)
	}
	// 15. No duplicate tool after the reload.
	count := 0
	for _, name := range rt.Tools.ToolNames() {
		if name == "acceptance_echo" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("criterion 15: acceptance_echo registered %d times", count)
	}

	// 16. Concurrent invocations without a runtime race.
	done := make(chan string, 8)
	for i := range 8 {
		go func(n int) {
			done <- rt.Tools.ExecuteJSON(context.Background(), "acceptance_echo", `{"query":"concurrent"}`)
		}(i)
	}
	for range 8 {
		select {
		case got := <-done:
			if !strings.Contains(got, "concurrent") {
				t.Fatalf("criterion 16: concurrent result = %q", got)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("criterion 16: a concurrent invocation never returned")
		}
	}
}

// TestPluginToolRequiresApprovalAndSandbox covers criteria 9, 10 and 11.
func TestPluginToolRequiresApprovalAndSandbox(t *testing.T) {
	manifest := `{
      "apiVersion": "bruce.plugin/v1", "name": "writer", "version": "1.0.0",
      "description": "A plugin that declares a write capability", "entry": "index.js",
      "permissions": ["fs.write"],
      "tools": [{"name": "writer_run", "description": "write", "handler": "run"}]
    }`
	environment := writeAcceptanceWorkspace(t, manifest, `export function run() { return "wrote"; }`, []string{"fs.write"})
	rt := newAcceptanceRuntime(t, environment, &scriptedClient{responses: []llm.ChatResponse{{Content: "ok"}}})

	if _, ok := rt.Tools.Lookup("writer_run"); !ok {
		t.Fatalf("criterion 9: the tool was not registered (diagnostics: %v)", rt.Plugins.Diagnostics())
	}
	registered, _ := rt.Tools.Lookup("writer_run")
	if !registered.Policy.NeedsApproval() {
		t.Fatal("criterion 11: a write-capable plugin tool must require approval")
	}
	if registered.Policy.Source != tool.SourcePlugin {
		t.Fatalf("criterion 9: tool source = %q, want plugin", registered.Policy.Source)
	}

	// 11. HITL: a rejecting handler must stop the plugin tool before it runs.
	// The runtime's default handler auto-approves, so a recording handler is
	// installed here to prove the plugin tool really goes through approval.
	recorder := &recordingApproval{result: approval.Reject("denied by the test")}
	rt.Tools.WithHITL(recorder)
	out := rt.Tools.Execute(context.Background(), "writer_run", nil)
	if recorder.requests == 0 {
		t.Fatal("criterion 11: the plugin tool never asked for approval")
	}
	if !strings.Contains(out, "[HITL] Operation was rejected") {
		t.Fatalf("criterion 11: HITL did not intercept the plugin tool: %q", out)
	}
	if strings.Contains(out, "wrote") {
		t.Fatal("criterion 11: the handler ran despite the rejection")
	}
	rt.Tools.WithHITL(rt.HITL)

	// 10. Sandbox: switching to read-only must refuse the write tool.
	if err := rt.Sandbox.SetMode("read-only"); err != nil {
		t.Fatal(err)
	}
	out = rt.Tools.Execute(context.Background(), "writer_run", nil)
	if strings.Contains(out, "wrote") {
		t.Fatalf("criterion 10: read-only mode allowed a write tool: %q", out)
	}
	if !strings.Contains(out, "sandbox") {
		t.Fatalf("criterion 10: output = %q, want a sandbox rejection", out)
	}
}

// TestPluginCancellationThroughRuntime covers criterion 12.
func TestPluginCancellationThroughRuntime(t *testing.T) {
	manifest := `{
      "apiVersion": "bruce.plugin/v1", "name": "spinner", "version": "1.0.0",
      "description": "Loops forever", "entry": "index.js",
      "tools": [{"name": "spinner_run", "description": "spin", "handler": "run"}]
    }`
	environment := writeAcceptanceWorkspace(t, manifest, `export function run() { let i = 0; while (true) { i++; } }`, nil)
	rt := newAcceptanceRuntime(t, environment, &scriptedClient{responses: []llm.ChatResponse{{Content: "ok"}}})
	if _, ok := rt.Tools.Lookup("spinner_run"); !ok {
		t.Fatalf("the tool was not registered: %v", rt.Plugins.Diagnostics())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan tool.ExecutionOutcome, 1)
	go func() { done <- rt.Tools.ExecuteResult(ctx, "spinner_run", nil) }()
	select {
	case outcome := <-done:
		// 12. ctx cancellation interrupts the plugin.
		if outcome.Status != tool.ToolCallTimeout && outcome.Status != tool.ToolCallInterrupted {
			t.Fatalf("criterion 12: status = %q, want a cancellation status", outcome.Status)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("criterion 12: ctx cancellation did not interrupt the plugin")
	}
}

// TestNoPluginsKeepsBruceUnchanged covers criterion 17 and docs/plugin.md
// section 22: with no plugin installed, behaviour must be identical.
func TestNoPluginsKeepsBruceUnchanged(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	settingsPath := filepath.Join(home, "setting.json")
	settings := config.DefaultSettings()
	settings.Sandbox.Mode = "full-access"
	if err := config.NewLoader(settingsPath).Save(settings); err != nil {
		t.Fatal(err)
	}
	rt := newAcceptanceRuntime(t, pluginWorkspace{workspace: workspace, home: home, settings: settingsPath},
		&scriptedClient{responses: []llm.ChatResponse{{Content: "ok"}}})

	if rt.Plugins.Count() != 0 {
		t.Fatalf("loaded %d plugins in an empty workspace", rt.Plugins.Count())
	}
	// The built-in tool set must be exactly the historical one.
	want := []string{"edit_file", "execute_command", "read_file", "web_fetch", "web_search", "write_file"}
	for _, name := range want {
		if _, ok := rt.Tools.Lookup(name); !ok {
			t.Errorf("built-in tool %q is missing", name)
		}
	}
	for _, name := range rt.Plugins.ToolNames() {
		t.Errorf("a plugin tool %q appeared with no plugin installed", name)
	}
	// The prompt must not have gained plugin content. The workspace path is
	// excluded because the temporary directory is named after this test.
	for _, line := range strings.Split(rt.react.SystemPrompt, "\n") {
		if strings.HasPrefix(line, "Current working directory:") {
			continue
		}
		if strings.Contains(strings.ToLower(line), "plugin") {
			t.Errorf("the system prompt mentions plugins with no plugin installed: %q", line)
		}
	}
	// Status reports zero plugins.
	status := rt.Status()
	if status.PluginCount != 0 || status.PluginTools != 0 || status.PluginHooks != 0 {
		t.Fatalf("status = plugins %d tools %d hooks %d", status.PluginCount, status.PluginTools, status.PluginHooks)
	}
}

// TestPluginDisabledKeepsBruceUnchanged proves the kill switch.
func TestPluginDisabledKeepsBruceUnchanged(t *testing.T) {
	environment := writeAcceptanceWorkspace(t, acceptancePluginManifest, acceptancePluginEntry, []string{"fs.read"})
	settings, err := config.NewLoader(environment.settings).Load()
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	settings.Plugins.Enabled = &disabled
	if err := config.NewLoader(environment.settings).Save(settings); err != nil {
		t.Fatal(err)
	}
	rt := newAcceptanceRuntime(t, environment, &scriptedClient{responses: []llm.ChatResponse{{Content: "ok"}}})
	if rt.Plugins != nil {
		t.Fatal("the plugin manager was created although plugins are disabled")
	}
	if _, ok := rt.Tools.Lookup("acceptance_echo"); ok {
		t.Fatal("a plugin tool was registered although plugins are disabled")
	}
}

// TestPluginDeniedPermissionDoesNotLoadTool proves the host policy is the final
// authority: a plugin that asks for a permission the host does not grant cannot
// get a tool that needs it.
func TestPluginDeniedPermissionDoesNotLoadTool(t *testing.T) {
	manifest := `{
      "apiVersion": "bruce.plugin/v1", "name": "writer", "version": "1.0.0",
      "description": "Asks for a write capability", "entry": "index.js",
      "permissions": ["fs.write"],
      "tools": [{"name": "writer_run", "description": "write", "handler": "run"}]
    }`
	// The settings file grants nothing at all.
	environment := writeAcceptanceWorkspace(t, manifest, `export function run() { return "wrote"; }`, nil)
	rt := newAcceptanceRuntime(t, environment, &scriptedClient{responses: []llm.ChatResponse{{Content: "ok"}}})

	if _, ok := rt.Tools.Lookup("writer_run"); ok {
		t.Fatal("a tool requiring an ungranted capability was registered")
	}
	diagnostics := rt.Plugins.Diagnostics()
	if len(diagnostics) == 0 {
		t.Fatal("the denial was not reported")
	}
	found := false
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.Message, "fs.write") || strings.Contains(diagnostic.Message, "permission") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %v, want a permission denial", diagnostics)
	}
}

// TestPluginCommandThroughRuntime proves a plugin slash command is reachable
// through the runtime's normal command handling.
func TestPluginCommandThroughRuntime(t *testing.T) {
	environment := writeAcceptanceWorkspace(t, acceptancePluginManifest, acceptancePluginEntry, []string{"fs.read"})
	rt := newAcceptanceRuntime(t, environment, &scriptedClient{responses: []llm.ChatResponse{{Content: "ok"}}})

	// The command is in the live registry, which is what the CLI completes and
	// dispatches against.
	if rt.Commands == nil {
		t.Fatal("the command registry was not created")
	}
	if _, ok := rt.Commands.Find("acceptance-report"); !ok {
		t.Fatalf("the plugin command was not registered: %v", rt.Commands.All())
	}
	result := rt.Handle(context.Background(), "/acceptance-report")
	if result.Err != nil {
		t.Fatalf("command failed: %v", result.Err)
	}
	if result.Output != "acceptance report" {
		t.Fatalf("output = %q", result.Output)
	}
	// A plugin command cannot shadow a built-in one.
	if command, ok := rt.Commands.Find("sandbox"); !ok || !command.Builtin {
		t.Fatalf("the built-in /sandbox command is not intact: %+v", command)
	}
}

// TestPluginStatusAndHelpIncludePlugins covers the observability surface.
func TestPluginStatusAndHelpIncludePlugins(t *testing.T) {
	environment := writeAcceptanceWorkspace(t, acceptancePluginManifest, acceptancePluginEntry, []string{"fs.read"})
	rt := newAcceptanceRuntime(t, environment, &scriptedClient{responses: []llm.ChatResponse{{Content: "ok"}}})

	status := rt.Status()
	if status.PluginCount != 1 || status.PluginTools != 1 {
		t.Fatalf("status = plugins %d tools %d", status.PluginCount, status.PluginTools)
	}
	if !strings.Contains(status.DisplayString(), "Plugins: 1") {
		t.Errorf("status text = %q", status.DisplayString())
	}

	listing := rt.Handle(context.Background(), "/plugin")
	if listing.Err != nil {
		t.Fatal(listing.Err)
	}
	for _, want := range []string{"acceptance", "1.0.0", "acceptance_echo", "acceptance-report", "granted"} {
		if !strings.Contains(listing.Output, want) {
			t.Errorf("/plugin output is missing %q:\n%s", want, listing.Output)
		}
	}

	help := rt.Handle(context.Background(), "/help")
	if !strings.Contains(help.Output, "/acceptance-report") {
		t.Errorf("/help does not list the plugin command:\n%s", help.Output)
	}
}

// scriptedClient returns prepared responses in order, so a test can drive a
// real agent turn deterministically.
type scriptedClient struct {
	responses []llm.ChatResponse
	calls     int
}

func (c *scriptedClient) Chat(_ context.Context, _ []llm.Message, _ []llm.ToolDefinition, _ llm.StreamOptions) (llm.ChatResponse, error) {
	if c.calls >= len(c.responses) {
		return llm.ChatResponse{Content: "exhausted"}, nil
	}
	response := c.responses[c.calls]
	c.calls++
	return response, nil
}

func (*scriptedClient) ProviderName() string        { return "scripted" }
func (*scriptedClient) ModelName() string           { return "scripted-model" }
func (*scriptedClient) MaxContextWindow() int       { return 200000 }
func (*scriptedClient) MaxOutputTokens() int        { return 0 }
func (*scriptedClient) SupportsTools() bool         { return true }
func (*scriptedClient) SupportsPromptCaching() bool { return false }
func (*scriptedClient) SupportsImages() bool        { return true }

// recordingApproval counts approval requests and returns a fixed decision.
type recordingApproval struct {
	requests int
	result   approval.Result
}

func (*recordingApproval) Enabled() bool     { return true }
func (*recordingApproval) SetEnabled(bool)   {}
func (*recordingApproval) ClearApprovedAll() {}
func (r *recordingApproval) Request(_ context.Context, _ approval.Request) (approval.Result, error) {
	r.requests++
	return r.result, nil
}

var _ = runtime.ModeReact
