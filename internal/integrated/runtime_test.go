package integrated

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"bruce-go/internal/agent"
	"bruce-go/internal/config"
	"bruce-go/internal/event"
	"bruce-go/internal/llm"
	"bruce-go/internal/mcp"
	bruntime "bruce-go/internal/runtime"
	"bruce-go/internal/sandbox"
	"bruce-go/internal/session"
	"bruce-go/internal/web"
)

func TestRuntimeHandlesTaskAndSlashCommands(t *testing.T) {
	client := &agent.FakeClient{Responses: []llm.ChatResponse{{Content: "done"}}}
	rt, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: client})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)
	if rt.HITL.Enabled() {
		t.Fatal("HITL should be disabled by default")
	}
	result := rt.Handle(context.Background(), "hello")
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.Output != "done" {
		t.Fatalf("task output = %q", result.Output)
	}
	if rt.Session.Context(rt.Mode).MessageCount != 2 {
		t.Fatalf("message count = %d", rt.Session.Context(rt.Mode).MessageCount)
	}
	status := rt.Handle(context.Background(), "/status")
	if !strings.Contains(status.Output, "RAG: 关闭") || !strings.Contains(status.Output, "fake-model") || !strings.Contains(status.Output, "Sandbox: full-access") {
		t.Fatalf("status = %s", status.Output)
	}
	sandboxStatus := rt.Handle(context.Background(), "/sandbox status")
	if sandboxStatus.Err != nil || !strings.Contains(sandboxStatus.Output, "mode=full-access") || !strings.Contains(sandboxStatus.Output, "network=开启") {
		t.Fatalf("sandbox status = %+v", sandboxStatus)
	}
	network := rt.Handle(context.Background(), "/sandbox network on")
	if network.Err != nil || !strings.Contains(network.Output, "network=开启") {
		t.Fatalf("sandbox network = %+v", network)
	}
	readOnly := rt.Handle(context.Background(), "/sandbox mode read-only")
	if readOnly.Err != nil || !strings.Contains(readOnly.Output, "mode=read-only") {
		t.Fatalf("sandbox mode = %+v", readOnly)
	}
	networkOff := rt.Handle(context.Background(), "/sandbox network off")
	if networkOff.Err != nil || !strings.Contains(networkOff.Output, "network=关闭") {
		t.Fatalf("sandbox network off = %+v", networkOff)
	}
	fullAccess := rt.Handle(context.Background(), "/sandbox mode full-access")
	if fullAccess.Err != nil || !strings.Contains(fullAccess.Output, "mode=full-access") || !strings.Contains(fullAccess.Output, "network=开启") {
		t.Fatalf("sandbox full-access mode = %+v", fullAccess)
	}
	networkOff = rt.Handle(context.Background(), "/sandbox network off")
	if networkOff.Err != nil || !strings.Contains(networkOff.Output, "网络始终开启") || !strings.Contains(networkOff.Output, "network=开启 (配置=关闭)") {
		t.Fatalf("full-access network off should persist configured value = %+v", networkOff)
	}
	workspaceWrite := rt.Handle(context.Background(), "/sandbox mode workspace-write")
	if workspaceWrite.Err != nil || !strings.Contains(workspaceWrite.Output, "network=关闭") {
		t.Fatalf("sandbox should restore safe-mode network setting = %+v", workspaceWrite)
	}
	legacyMode := rt.Handle(context.Background(), "/sandbox mode danger-full-access")
	if legacyMode.Err == nil {
		t.Fatalf("legacy sandbox mode should fail: %+v", legacyMode)
	}
	parallel := rt.Handle(context.Background(), "/parallel off")
	if parallel.Err != nil || !strings.Contains(parallel.Output, "已关闭") {
		t.Fatalf("parallel result = %+v", parallel)
	}
	compact := rt.Handle(context.Background(), "/compact focus")
	if compact.Err != nil || !strings.Contains(compact.Output, "compaction") {
		t.Fatalf("compact result = %+v", compact)
	}
	newSession := rt.Handle(context.Background(), "/new")
	if newSession.Err != nil {
		t.Fatal(newSession.Err)
	}
	if rt.Session.Context(rt.Mode).MessageCount != 0 {
		t.Fatalf("new session message count = %d", rt.Session.Context(rt.Mode).MessageCount)
	}
}

func TestRuntimeAgentPromptsIncludeCanonicalWorkspace(t *testing.T) {
	workspace := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Skipf("创建 workspace 符号链接失败: %v", err)
	}
	expected, err := sandbox.CanonicalAbsolute(workspace)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(context.Background(), Options{
		Workspace: link,
		HomeDir:   t.TempDir(),
		Client:    &agent.FakeClient{},
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)

	if rt.Workspace != expected {
		t.Fatalf("workspace = %q, want canonical path %q", rt.Workspace, expected)
	}
	want := "当前工作目录: " + rt.Workspace
	for name, prompt := range map[string]string{
		"react": rt.react.SystemPrompt,
		"plan":  rt.planning.SystemPrompt,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("%s system prompt missing %q:\n%s", name, want, prompt)
		}
		if link != rt.Workspace && strings.Contains(prompt, "当前工作目录: "+link) {
			t.Errorf("%s system prompt contains uncanonical workspace %q", name, link)
		}
	}
}

func TestRuntimeSandboxTransitionRefreshesMCPToolsAndSafeStatus(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	settingsPath := filepath.Join(t.TempDir(), "setting.json")
	settings := config.DefaultSettings()
	settings.MCP.Servers["demo"] = config.MCPServerSetting{
		Type:    "stdio",
		Command: "fake",
		Env:     map[string]string{"MCP_SECRET": "should-not-render"},
		Headers: map[string]string{"Authorization": "Bearer should-not-render"},
		ToolAccess: map[string]string{
			"read":  "read-only",
			"write": "workspace-write",
		},
	}
	if err := config.NewLoader(settingsPath).Save(settings); err != nil {
		t.Fatal(err)
	}
	rt, err := New(context.Background(), Options{
		Workspace: workspace, HomeDir: home, SettingsPath: settingsPath, Client: &agent.FakeClient{},
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)
	if err := rt.Sandbox.ValidateMode(sandbox.ModeReadOnly); err != nil {
		t.Skipf("sandbox backend unavailable: %v", err)
	}
	rt.MCP.WithFactory(func(context.Context, string, config.MCPServerSetting, string) (mcp.Transport, error) {
		return runtimePolicyTransport{}, nil
	})
	if err := rt.MCP.Enable(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	rt.refreshMCPTools()
	if !containsTool(rt.Tools.ToolNames(), "mcp_demo_write") || !containsTool(rt.Tools.ToolNames(), "mcp_demo_hinted") {
		t.Fatalf("full-access MCP tools = %v", rt.Tools.ToolNames())
	}

	result := rt.Handle(context.Background(), "/sandbox mode read-only")
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	names := rt.Tools.ToolNames()
	if !containsTool(names, "mcp_demo_read") || containsTool(names, "mcp_demo_write") || containsTool(names, "mcp_demo_hinted") {
		t.Fatalf("read-only MCP tools = %v", names)
	}
	for _, output := range []string{
		result.Output,
		rt.Handle(context.Background(), "/mcp").Output,
		rt.Handle(context.Background(), "/status").Output,
	} {
		if !strings.Contains(output, "tools=1") || !strings.Contains(output, "blocked=2") || !strings.Contains(output, "generation=") {
			t.Fatalf("MCP policy diagnostics missing:\n%s", output)
		}
		if strings.Contains(output, "should-not-render") || strings.Contains(output, "Authorization") || strings.Contains(output, "MCP_SECRET") {
			t.Fatalf("MCP status leaked configuration secret:\n%s", output)
		}
	}
}

type runtimePolicyTransport struct{}

func (runtimePolicyTransport) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	switch method {
	case "initialize":
		return json.RawMessage(`{}`), nil
	case "tools/list":
		return json.RawMessage(`{"tools":[
			{"name":"read","description":"read","inputSchema":{"type":"object"}},
			{"name":"write","description":"write","inputSchema":{"type":"object"}},
			{"name":"hinted","description":"hint only","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}}
		]}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func (runtimePolicyTransport) Notify(context.Context, string, any) error { return nil }

func (runtimePolicyTransport) Close() error   { return nil }
func (runtimePolicyTransport) Logs() []string { return nil }

func containsTool(names []string, expected string) bool {
	for _, name := range names {
		if name == expected {
			return true
		}
	}
	return false
}

func TestRuntimeWebCommandUsesFakeSearcher(t *testing.T) {
	rt, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: &agent.FakeClient{}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)
	rt.Web.Searcher = fakeSearcher{}
	result := rt.Handle(context.Background(), "/web search golang")
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if !strings.Contains(result.Output, "Go") || !strings.Contains(result.Output, "https://go.dev") {
		t.Fatalf("web output = %s", result.Output)
	}
}

func TestRuntimePersistsReactToolTranscriptInSession(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	client := &agent.FakeClient{Responses: []llm.ChatResponse{
		{ToolCalls: []llm.ToolCall{{ID: "call_a", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`}}}},
		{Content: "done"},
	}}
	rt, err := New(context.Background(), Options{Workspace: workspace, HomeDir: t.TempDir(), Client: client})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)

	out, err := rt.RunTask(context.Background(), "读取 a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if out != "done" {
		t.Fatalf("out = %q", out)
	}
	ctx := rt.Session.Context(rt.Mode)
	if ctx.MessageCount != 4 || len(ctx.Messages) != 4 {
		t.Fatalf("context = %+v", ctx)
	}
	if ctx.Messages[0].Role != llm.RoleUser || ctx.Messages[1].Role != llm.RoleAssistant || ctx.Messages[2].Role != llm.RoleTool || ctx.Messages[3].Role != llm.RoleAssistant {
		t.Fatalf("messages = %+v", ctx.Messages)
	}
	if len(ctx.Messages[1].ToolCalls) != 1 || ctx.Messages[1].ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("assistant tool calls = %+v", ctx.Messages[1].ToolCalls)
	}
	if ctx.Messages[2].ToolCallID != "call_a" || !strings.Contains(ctx.Messages[2].Content, "alpha") {
		t.Fatalf("tool message = %+v", ctx.Messages[2])
	}
	raw, err := os.ReadFile(ctx.File)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"toolCalls"`) || !strings.Contains(string(raw), `"toolCallId":"call_a"`) {
		t.Fatalf("raw session missing tool transcript:\n%s", raw)
	}
}

func TestPlanModePersistsOnlyTopLevelMessagesAndPlanEvents(t *testing.T) {
	client := &agent.FakeClient{Responses: []llm.ChatResponse{{Content: `{"goal":"分析","tasks":[{"id":"t1","description":"分析目标","type":"ANALYSIS","dependencies":[]}]}`}}}
	rt, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: client})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)
	if result := rt.Handle(context.Background(), "/plan"); result.Err != nil {
		t.Fatal(result.Err)
	}

	if _, err := rt.RunTask(context.Background(), "分析项目"); err != nil {
		t.Fatal(err)
	}
	ctx := rt.Session.Context(rt.Mode)
	if ctx.MessageCount != 2 || len(ctx.Messages) != 2 {
		t.Fatalf("context = %+v", ctx)
	}
	if ctx.Messages[0].Role != llm.RoleUser || ctx.Messages[1].Role != llm.RoleAssistant {
		t.Fatalf("messages = %+v", ctx.Messages)
	}
	if ctx.ActivePlan.ID == "" || !ctx.ActivePlan.Pending() {
		t.Fatalf("active plan = %+v", ctx.ActivePlan)
	}
	raw, err := os.ReadFile(ctx.File)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"type":"plan_event"`) || !strings.Contains(string(raw), `"action":"presented"`) {
		t.Fatalf("raw session missing plan events:\n%s", raw)
	}
}

func TestPlanModeEmitsPresentedPlanEvent(t *testing.T) {
	client := &recordingChatClient{responses: []llm.ChatResponse{
		{ToolCalls: []llm.ToolCall{{ID: "call_plan", Function: llm.FunctionCall{Name: "replace_plan", Arguments: `{"content":"# Presented Plan\n\n- Step","summary":"create"}`}}}},
		{Content: "计划创建完成，请审阅。"},
	}}
	rt, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: client})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)
	var planEvents []event.PlanEventRecorded
	unsubscribe := rt.Events.Subscribe(func(evt event.Event) {
		if recorded, ok := evt.(event.PlanEventRecorded); ok {
			planEvents = append(planEvents, recorded)
		}
	})
	defer unsubscribe()

	planned := rt.Handle(context.Background(), "/plan 生成计划")
	if planned.Err != nil {
		t.Fatal(planned.Err)
	}
	if len(planEvents) != 1 {
		t.Fatalf("plan events = %+v", planEvents)
	}
	got := planEvents[0].Plan
	if got.Action != bruntime.PlanActionPresented || !strings.Contains(got.Content, "# Presented Plan") || got.Revision != 1 {
		t.Fatalf("presented event = %+v", got)
	}
}

func TestPlanDescriptionApproveAndRejectLifecycle(t *testing.T) {
	client := &recordingChatClient{responses: []llm.ChatResponse{
		{ToolCalls: []llm.ToolCall{{ID: "call_plan", Function: llm.FunctionCall{Name: "replace_plan", Arguments: `{"content":"# Plan\n\n- Inspect\n- Edit","summary":"create"}`}}}},
		{Content: "计划已准备"},
		{Content: "执行完成"},
	}}
	workspace := t.TempDir()
	home := t.TempDir()
	rt, err := New(context.Background(), Options{Workspace: workspace, HomeDir: home, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)
	var assistantCompletions []string
	unsubscribe := rt.Events.Subscribe(func(evt event.Event) {
		completed, ok := evt.(event.MessageCompleted)
		if ok && completed.Message.Role == llm.RoleAssistant {
			assistantCompletions = append(assistantCompletions, completed.Message.Content)
		}
	})
	defer unsubscribe()

	planned := rt.Handle(context.Background(), "/plan 实现新能力")
	if planned.Err != nil {
		t.Fatal(planned.Err)
	}
	if !strings.Contains(planned.Output, "# Plan") || rt.Mode != bruntime.ModePlan {
		t.Fatalf("planned = %+v mode=%s", planned, rt.Mode)
	}
	ctx := rt.Session.Context(rt.Mode)
	if !ctx.ActivePlan.Pending() || ctx.ActivePlan.Revision != 1 {
		t.Fatalf("active plan = %+v", ctx.ActivePlan)
	}
	if messagesContainText(ctx.Messages, "# Plan\n\n- Inspect\n- Edit") {
		t.Fatalf("plan file content should not be appended as a duplicate assistant message: %+v", ctx.Messages)
	}
	if messagesContainRawText(assistantCompletions, "# Plan\n\n- Inspect\n- Edit") {
		t.Fatalf("plan file content should not be emitted as a duplicate assistant completion: %+v", assistantCompletions)
	}

	approved := rt.Handle(context.Background(), "/plan approve")
	if approved.Err != nil {
		t.Fatal(approved.Err)
	}
	if approved.Output != "执行完成" || rt.Mode != bruntime.ModeReact {
		t.Fatalf("approved = %+v mode=%s", approved, rt.Mode)
	}
	raw, err := os.ReadFile(rt.Session.Context(rt.Mode).File)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{`"action":"created"`, `"action":"presented"`, `"action":"approved"`, `"action":"handoff"`, `"content":"/plan approve"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("session missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "请按已批准计划开始执行") {
		t.Fatalf("session should keep the user command, not the internal handoff prompt:\n%s", text)
	}
	if len(client.calls) != 3 {
		t.Fatalf("chat calls = %d", len(client.calls))
	}
	approveMessages := client.calls[2]
	if !messagesContainUser(approveMessages, "/plan approve") {
		t.Fatalf("approve call should use the slash command as model input: %+v", approveMessages)
	}
	if messagesContainText(approveMessages, "请按已批准计划开始执行") {
		t.Fatalf("approve call should not use the internal handoff prompt: %+v", approveMessages)
	}
	if !messagesContainText(approveMessages, "用户输入 `/plan approve` 表示用户已经批准下方计划") {
		t.Fatalf("approve call missing approved-plan system context: %+v", approveMessages)
	}
	resumed, err := New(context.Background(), Options{Workspace: workspace, HomeDir: home, Client: &agent.FakeClient{}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, resumed)
	if err := resumed.Session.Resume(rt.Session.Context(rt.Mode).File); err != nil {
		t.Fatal(err)
	}
	resumed.Mode = resumed.Session.Context(resumed.Mode).Mode
	var sawApprove, sawInternal bool
	for _, msg := range resumed.Session.Context(resumed.Mode).Messages {
		if msg.Role != llm.RoleUser {
			continue
		}
		if msg.Content == "/plan approve" {
			sawApprove = true
		}
		if strings.Contains(msg.Content, "请按已批准计划开始执行") {
			sawInternal = true
		}
	}
	if !sawApprove || sawInternal {
		t.Fatalf("resumed messages sawApprove=%v sawInternal=%v messages=%+v", sawApprove, sawInternal, resumed.Session.Context(resumed.Mode).Messages)
	}
	tree := resumed.Session.RenderTree(resumed.Mode)
	if !strings.Contains(tree, "user /plan approve") || strings.Contains(tree, "请按已批准计划开始执行") {
		t.Fatalf("resumed tree should show the slash command only:\n%s", tree)
	}

	client2 := &agent.FakeClient{Responses: []llm.ChatResponse{
		{Content: "# Reject Plan"},
	}}
	rt2, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: client2})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt2)
	if result := rt2.Handle(context.Background(), "/plan 规划但不执行"); result.Err != nil {
		t.Fatal(result.Err)
	}
	rejected := rt2.Handle(context.Background(), "/plan reject 不采用")
	if rejected.Err != nil {
		t.Fatal(rejected.Err)
	}
	if rt2.Mode != bruntime.ModeReact || !strings.Contains(rejected.Output, "已拒绝计划") {
		t.Fatalf("rejected = %+v mode=%s", rejected, rt2.Mode)
	}
	if action := rt2.Session.Context(rt2.Mode).ActivePlan.Action; action != "rejected" {
		t.Fatalf("plan action = %s", action)
	}
}

func TestPendingPlanNaturalLanguageInputIsGated(t *testing.T) {
	client := &agent.FakeClient{Responses: []llm.ChatResponse{
		{Content: "# Full Plan\n\n- should-not-repeat"},
		{Content: "执行完成"},
	}}
	rt, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: client})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)

	planned := rt.Handle(context.Background(), "/plan 生成计划")
	if planned.Err != nil {
		t.Fatal(planned.Err)
	}
	if client.Calls != 1 {
		t.Fatalf("planning calls = %d", client.Calls)
	}
	beforeRaw, err := os.ReadFile(rt.Session.Context(rt.Mode).File)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents := strings.Count(string(beforeRaw), `"type":"plan_event"`)

	gated := rt.Handle(context.Background(), "开始实现")
	if gated.Err != nil {
		t.Fatal(gated.Err)
	}
	for _, want := range []string{"/plan approve", "/plan continue", "/plan reject", "/plan cancel"} {
		if !strings.Contains(gated.Output, want) {
			t.Fatalf("gated output missing %q:\n%s", want, gated.Output)
		}
	}
	if strings.Contains(gated.Output, "should-not-repeat") {
		t.Fatalf("gated output repeated plan body:\n%s", gated.Output)
	}
	if client.Calls != 1 {
		t.Fatalf("gated input should not consume LLM response, calls=%d", client.Calls)
	}
	afterRaw, err := os.ReadFile(rt.Session.Context(rt.Mode).File)
	if err != nil {
		t.Fatal(err)
	}
	afterEvents := strings.Count(string(afterRaw), `"type":"plan_event"`)
	if afterEvents != beforeEvents {
		t.Fatalf("plan_event count changed: before=%d after=%d\n%s", beforeEvents, afterEvents, afterRaw)
	}

	approved := rt.Handle(context.Background(), "/plan approve")
	if approved.Err != nil {
		t.Fatal(approved.Err)
	}
	if approved.Output != "执行完成" || client.Calls != 2 {
		t.Fatalf("approve output=%q calls=%d", approved.Output, client.Calls)
	}
}

func TestPlanContinueAndResumeRecovery(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	client := &agent.FakeClient{Responses: []llm.ChatResponse{
		{Content: "# Initial Plan"},
		{ToolCalls: []llm.ToolCall{{ID: "call_edit", Function: llm.FunctionCall{Name: "edit_plan", Arguments: `{"old_text":"Initial","new_text":"Updated","summary":"feedback"}`}}}},
		{Content: "已更新"},
	}}
	rt, err := New(context.Background(), Options{Workspace: workspace, HomeDir: home, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)
	if result := rt.Handle(context.Background(), "/plan 初始规划"); result.Err != nil {
		t.Fatal(result.Err)
	}
	if result := rt.Handle(context.Background(), "/plan continue 补充反馈"); result.Err != nil {
		t.Fatal(result.Err)
	}
	if client.Calls != 3 {
		t.Fatalf("continue should reach Planning Agent, calls=%d", client.Calls)
	}
	ctx := rt.Session.Context(rt.Mode)
	if rt.Mode != bruntime.ModePlan || !ctx.ActivePlan.Pending() || ctx.ActivePlan.Revision != 2 {
		t.Fatalf("ctx = %+v mode=%s", ctx, rt.Mode)
	}
	if err := os.Remove(ctx.ActivePlan.Path); err != nil {
		t.Fatal(err)
	}

	resumed, err := New(context.Background(), Options{Workspace: workspace, HomeDir: home, Client: &agent.FakeClient{}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, resumed)
	if result := resumed.Handle(context.Background(), "/resume "+rt.Session.Context(rt.Mode).SessionID); result.Err != nil {
		t.Fatal(result.Err)
	}
	status := resumed.Status()
	if status.Mode != bruntime.ModePlan || !status.ActivePlan.Pending() || !status.ActivePlan.RecoveredFromSnapshot {
		t.Fatalf("status = %+v", status)
	}
	if _, err := os.Stat(status.ActivePlan.Path); err != nil {
		t.Fatalf("plan file not recovered: %v", err)
	}
	if err := os.WriteFile(status.ActivePlan.Path, []byte("# Diverged"), 0o644); err != nil {
		t.Fatal(err)
	}
	status = resumed.Status()
	if !status.ActivePlan.HashMismatch {
		t.Fatalf("expected hash mismatch, status = %+v", status.ActivePlan)
	}
}

func TestModeChangePersistsOnce(t *testing.T) {
	rt, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: &agent.FakeClient{}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)
	if result := rt.Handle(context.Background(), "/plan"); result.Err != nil {
		t.Fatal(result.Err)
	}
	raw, err := os.ReadFile(rt.Session.Context(rt.Mode).File)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(raw), `"type":"mode_change"`); count != 1 {
		t.Fatalf("mode_change count = %d\n%s", count, raw)
	}
}

type fakeSearcher struct{}

func (fakeSearcher) Search(context.Context, string, int) ([]web.Result, error) {
	return []web.Result{{Title: "Go", URL: "https://go.dev", Snippet: "build simple"}}, nil
}

func TestHandleModelReasoningSubcommand(t *testing.T) {
	// Without switchable: shows current level or error
	rt, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: &agent.FakeClient{}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)

	// Query current level (no switchable → empty)
	result := rt.Handle(context.Background(), "/model reasoning")
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if !strings.Contains(result.Output, "可选级别") {
		t.Fatalf("expected level list, got: %q", result.Output)
	}

	// Set level without switchable → error
	result2 := rt.Handle(context.Background(), "/model reasoning high")
	if result2.Err == nil {
		t.Fatal("expected error when setting effort without switchable")
	}
}

type recordingChatClient struct {
	responses []llm.ChatResponse
	calls     [][]llm.Message
}

type compactionChatStep struct {
	response llm.ChatResponse
	err      error
}

type compactionChatClient struct {
	mu        sync.Mutex
	steps     []compactionChatStep
	calls     [][]llm.Message
	options   []llm.StreamOptions
	window    int
	maxOutput int
}

func (c *compactionChatClient) Chat(_ context.Context, messages []llm.Message, _ []llm.ToolDefinition, opts llm.StreamOptions) (llm.ChatResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	copied := append([]llm.Message(nil), messages...)
	c.calls = append(c.calls, copied)
	c.options = append(c.options, opts)
	if len(c.steps) == 0 {
		return llm.ChatResponse{}, errors.New("compaction client exhausted")
	}
	step := c.steps[0]
	c.steps = c.steps[1:]
	return step.response, step.err
}

func (*compactionChatClient) ProviderName() string        { return "fake" }
func (*compactionChatClient) ModelName() string           { return "fake-model" }
func (c *compactionChatClient) MaxContextWindow() int     { return c.window }
func (c *compactionChatClient) MaxOutputTokens() int      { return c.maxOutput }
func (*compactionChatClient) SupportsTools() bool         { return true }
func (*compactionChatClient) SupportsPromptCaching() bool { return false }
func (*compactionChatClient) SupportsImages() bool        { return true }

func newCompactionRuntime(t *testing.T, client *compactionChatClient) *Runtime {
	t.Helper()
	homeDir := t.TempDir()
	settingsPath := filepath.Join(homeDir, "setting.json")
	settings := config.DefaultSettings()
	settings.Compaction = config.Compaction{Enabled: true, ContextWindowRatio: 0.8, ReserveTokens: 10, KeepRecentTokens: 1}
	if err := config.NewLoader(settingsPath).Save(settings); err != nil {
		t.Fatal(err)
	}
	rt, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: homeDir, SettingsPath: settingsPath, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)
	if err := rt.Session.AppendMessage(llm.User(strings.Repeat("旧问题", 40))); err != nil {
		t.Fatal(err)
	}
	if err := rt.Session.AppendMessage(llm.Assistant(strings.Repeat("旧回答", 40))); err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestRuntimeOverflowCompactsAndContinuesWithoutDuplicateUser(t *testing.T) {
	client := &compactionChatClient{window: 100, maxOutput: 50, steps: []compactionChatStep{
		{err: errors.New("input exceeds the context window")},
		{response: llm.ChatResponse{Content: "## 目标\n恢复任务"}},
		{response: llm.ChatResponse{Content: "恢复完成", FinishReason: "stop"}},
	}}
	rt := newCompactionRuntime(t, client)
	out, err := rt.RunTask(context.Background(), "唯一的新问题")
	if err != nil || out != "恢复完成" {
		t.Fatalf("result = %q, %v", out, err)
	}
	if len(client.calls) != 3 {
		t.Fatalf("calls = %d", len(client.calls))
	}
	if !messagesContainText(client.calls[1], "上下文摘要助手") && !messagesContainText(client.calls[1], "超长回合") {
		t.Fatalf("second call is not summarization: %+v", client.calls[1])
	}
	users := 0
	for _, entry := range rt.Session.ActiveEntries() {
		if entry.Message != nil && entry.Message.Role == llm.RoleUser && entry.Message.Content == "唯一的新问题" {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("persisted user count = %d", users)
	}
	if countExactUser(client.calls[2], "唯一的新问题") != 1 {
		t.Fatalf("retry context duplicated user: %+v", client.calls[2])
	}
}

func TestManualCompactUsesLLMSummaryWhenAutomaticCompactionDisabled(t *testing.T) {
	client := &compactionChatClient{window: 100, maxOutput: 20, steps: []compactionChatStep{{response: llm.ChatResponse{Content: "## 目标\n手动摘要"}}}}
	rt := newCompactionRuntime(t, client)
	rt.Settings.Compaction.Enabled = false
	rt.Settings.Compaction.ReserveTokens = 100
	rt.Settings.Compaction.KeepRecentTokens = 2
	if err := rt.Session.AppendMessage(llm.User("x")); err != nil {
		t.Fatal(err)
	}
	if err := rt.Session.AppendMessage(llm.Assistant("y")); err != nil {
		t.Fatal(err)
	}
	result := rt.Handle(context.Background(), "/compact 重点保留接口")
	if result.Err != nil || !strings.Contains(result.Output, "已压缩") {
		t.Fatalf("result = %+v", result)
	}
	if len(client.calls) != 1 || !messagesContainText(client.calls[0], "重点保留接口") || client.options[0].MaxTokens != 20 {
		t.Fatalf("summary call = %+v opts=%+v", client.calls, client.options)
	}
	entries := rt.Session.ActiveEntries()
	last := entries[len(entries)-1]
	if last.Type != session.TypeCompaction || last.TokensBefore <= 0 || !strings.Contains(last.Summary, "手动摘要") {
		t.Fatalf("compaction entry = %+v", last)
	}
}

func TestRuntimeLengthOverflowKeepsUserContextForRetry(t *testing.T) {
	client := &compactionChatClient{window: 100, steps: []compactionChatStep{
		{response: llm.ChatResponse{InputTokens: 99, FinishReason: "length"}},
		{response: llm.ChatResponse{Content: "摘要"}},
		{response: llm.ChatResponse{Content: "重试成功", FinishReason: "stop"}},
	}}
	rt := newCompactionRuntime(t, client)
	out, err := rt.RunTask(context.Background(), "必须保留的请求")
	if err != nil || out != "重试成功" {
		t.Fatalf("result=%q err=%v", out, err)
	}
	if len(client.calls) != 3 || countExactUser(client.calls[2], "必须保留的请求") != 1 {
		t.Fatalf("retry context = %+v", client.calls)
	}
	foundPersistedOverflow := false
	for _, entry := range rt.Session.ActiveEntries() {
		if entry.Message != nil && entry.Message.FinishReason == "length" && entry.Message.InputTokens == 99 {
			foundPersistedOverflow = true
		}
	}
	if !foundPersistedOverflow {
		t.Fatal("length overflow response metadata was not persisted")
	}
	current := rt.Session.Context(rt.Mode)
	resumed := session.NewStore(rt.HomeDir, rt.Workspace)
	if err := resumed.Resume(current.File); err != nil {
		t.Fatal(err)
	}
	restored := resumed.Context(rt.Mode)
	if len(restored.Messages) != len(current.Messages) {
		t.Fatalf("restored messages=%+v current=%+v", restored.Messages, current.Messages)
	}
	for i := range current.Messages {
		if restored.Messages[i].Role != current.Messages[i].Role || restored.Messages[i].Content != current.Messages[i].Content {
			t.Fatalf("restored message %d=%+v current=%+v", i, restored.Messages[i], current.Messages[i])
		}
	}
}

func TestRuntimeOverflowRetriesOnlyOnce(t *testing.T) {
	client := &compactionChatClient{window: 100, steps: []compactionChatStep{
		{err: errors.New("prompt is too long")},
		{response: llm.ChatResponse{Content: "摘要"}},
		{err: errors.New("prompt is too long")},
	}}
	rt := newCompactionRuntime(t, client)
	_, err := rt.RunTask(context.Background(), "new")
	if err == nil || !strings.Contains(err.Error(), "仍然溢出") {
		t.Fatalf("error = %v", err)
	}
	if len(client.calls) != 3 {
		t.Fatalf("calls = %d, expected agent + summary + one retry", len(client.calls))
	}
}

func TestRuntimeSuccessfulSilentOverflowCompactsWithoutRetry(t *testing.T) {
	client := &compactionChatClient{window: 100, steps: []compactionChatStep{
		{response: llm.ChatResponse{Content: "答案", InputTokens: 101, FinishReason: "stop"}},
		{response: llm.ChatResponse{Content: "摘要"}},
	}}
	rt := newCompactionRuntime(t, client)
	rt.Settings.Compaction.KeepRecentTokens = 2
	out, err := rt.RunTask(context.Background(), "new")
	if err != nil || out != "答案" {
		t.Fatalf("result = %q, %v", out, err)
	}
	if len(client.calls) != 2 {
		t.Fatalf("silent overflow should summarize but not retry, calls=%d", len(client.calls))
	}
	if rt.Session.ActiveEntries()[len(rt.Session.ActiveEntries())-1].Type != "compaction" {
		t.Fatalf("last entry is not compaction: %+v", rt.Session.ActiveEntries())
	}
}

func TestRuntimeThresholdBeforeAndAfterModelCalls(t *testing.T) {
	t.Run("before", func(t *testing.T) {
		client := &compactionChatClient{window: 100, steps: []compactionChatStep{{response: llm.ChatResponse{Content: "摘要"}}, {response: llm.ChatResponse{Content: "答案", FinishReason: "stop"}}}}
		rt := newCompactionRuntime(t, client)
		entries := rt.Session.ActiveEntries()
		used := *entries[len(entries)-1].Message
		used.InputTokens, used.OutputTokens, used.FinishReason = 92, 1, "stop"
		if err := rt.Session.AppendMessage(used); err != nil {
			t.Fatal(err)
		}
		out, err := rt.RunTask(context.Background(), "new")
		if err != nil || out != "答案" || len(client.calls) != 2 {
			t.Fatalf("result=%q err=%v calls=%d", out, err, len(client.calls))
		}
		if !messagesContainText(client.calls[0], "conversation") {
			t.Fatalf("first call should be compaction: %+v", client.calls[0])
		}
	})

	t.Run("after", func(t *testing.T) {
		client := &compactionChatClient{window: 100, steps: []compactionChatStep{{response: llm.ChatResponse{Content: "答案", InputTokens: 92, OutputTokens: 1, FinishReason: "stop"}}, {response: llm.ChatResponse{Content: "摘要"}}}}
		rt := newCompactionRuntime(t, client)
		rt.Settings.Compaction.KeepRecentTokens = 2
		out, err := rt.RunTask(context.Background(), "new")
		if err != nil || out != "答案" || len(client.calls) != 2 {
			t.Fatalf("result=%q err=%v calls=%d", out, err, len(client.calls))
		}
	})
}

func TestRuntimeCompactionThresholdUsesContextWindowRatio(t *testing.T) {
	client := &compactionChatClient{window: 100}
	rt := newCompactionRuntime(t, client)

	atThreshold := llm.Assistant("usage")
	atThreshold.InputTokens, atThreshold.FinishReason = 70, "stop"
	if err := rt.Session.AppendMessage(atThreshold); err != nil {
		t.Fatal(err)
	}
	needed, tokens, threshold, err := rt.compactionThreshold()
	if err != nil || needed || tokens != 70 || threshold != 70 {
		t.Fatalf("at threshold: needed=%v tokens=%d threshold=%d err=%v", needed, tokens, threshold, err)
	}

	aboveThreshold := llm.Assistant("usage")
	aboveThreshold.InputTokens, aboveThreshold.FinishReason = 71, "stop"
	if err := rt.Session.AppendMessage(aboveThreshold); err != nil {
		t.Fatal(err)
	}
	needed, tokens, threshold, err = rt.compactionThreshold()
	if err != nil || !needed || tokens != 71 || threshold != 70 {
		t.Fatalf("above threshold: needed=%v tokens=%d threshold=%d err=%v", needed, tokens, threshold, err)
	}
}

func TestRuntimeRejectsInvalidCompactionWindowAtStartup(t *testing.T) {
	homeDir := t.TempDir()
	settingsPath := filepath.Join(homeDir, "setting.json")
	settings := config.DefaultSettings()
	settings.Compaction.ContextWindowRatio = 0.8
	settings.Compaction.ReserveTokens = 80
	if err := config.NewLoader(settingsPath).Save(settings); err != nil {
		t.Fatal(err)
	}
	client := &compactionChatClient{window: 100}
	if _, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: homeDir, SettingsPath: settingsPath, Client: client}); err == nil || !strings.Contains(err.Error(), "自动压缩配置无效") {
		t.Fatalf("startup error = %v", err)
	}
}

func TestRuntimeThresholdDisabledUnknownWindowAndOldCompactionUsage(t *testing.T) {
	tests := []struct {
		name       string
		window     int
		enabled    bool
		oldCompact bool
	}{
		{name: "disabled", window: 100, enabled: false},
		{name: "unknown window", window: 0, enabled: true},
		{name: "old compaction usage", window: 100, enabled: true, oldCompact: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &compactionChatClient{window: test.window, steps: []compactionChatStep{{response: llm.ChatResponse{Content: "答案", FinishReason: "stop"}}}}
			rt := newCompactionRuntime(t, client)
			rt.Settings.Compaction.Enabled = test.enabled
			entries := rt.Session.ActiveEntries()
			used := llm.Assistant("usage")
			used.InputTokens, used.FinishReason = 95, "stop"
			if err := rt.Session.AppendMessage(used); err != nil {
				t.Fatal(err)
			}
			if test.oldCompact {
				entries = rt.Session.ActiveEntries()
				if err := rt.Session.AppendCompaction("旧摘要", entries[0].ID, 95, nil); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := rt.RunTask(context.Background(), "new"); err != nil || out != "答案" {
				t.Fatalf("result=%q err=%v", out, err)
			}
			if len(client.calls) != 1 {
				t.Fatalf("unexpected compaction, calls=%d", len(client.calls))
			}
		})
	}
}

func TestRuntimePostTurnCompactionFailurePreservesAnswer(t *testing.T) {
	client := &compactionChatClient{window: 100, steps: []compactionChatStep{{response: llm.ChatResponse{Content: "已完成答案", InputTokens: 92, FinishReason: "stop"}}, {err: errors.New("summary unavailable")}}}
	rt := newCompactionRuntime(t, client)
	rt.Settings.Compaction.KeepRecentTokens = 2
	var activities []string
	rt.Events.Subscribe(func(evt event.Event) {
		if activity, ok := evt.(event.Activity); ok {
			activities = append(activities, activity.Message)
		}
	})
	out, err := rt.RunTask(context.Background(), "new")
	if err != nil || out != "已完成答案" {
		t.Fatalf("result=%q err=%v", out, err)
	}
	if !messagesContainRawText(activities, "当前回答已完成") {
		t.Fatalf("activities = %v", activities)
	}
}

func countExactUser(messages []llm.Message, content string) int {
	count := 0
	for _, message := range messages {
		if message.Role == llm.RoleUser && message.Content == content {
			count++
		}
	}
	return count
}

func (c *recordingChatClient) Chat(_ context.Context, messages []llm.Message, _ []llm.ToolDefinition, opts llm.StreamOptions) (llm.ChatResponse, error) {
	copied := append([]llm.Message(nil), messages...)
	c.calls = append(c.calls, copied)
	if len(c.calls) > len(c.responses) {
		return llm.ChatResponse{}, errors.New("recording client response exhausted")
	}
	resp := c.responses[len(c.calls)-1]
	if resp.ReasoningContent != "" && opts.OnReasoning != nil {
		opts.OnReasoning(resp.ReasoningContent)
	}
	if resp.Content != "" && opts.OnContent != nil {
		opts.OnContent(resp.Content)
	}
	return resp, nil
}

func (*recordingChatClient) ProviderName() string        { return "fake" }
func (*recordingChatClient) ModelName() string           { return "fake-model" }
func (*recordingChatClient) MaxContextWindow() int       { return 200000 }
func (*recordingChatClient) MaxOutputTokens() int        { return 0 }
func (*recordingChatClient) SupportsTools() bool         { return true }
func (*recordingChatClient) SupportsPromptCaching() bool { return false }
func (*recordingChatClient) SupportsImages() bool        { return true }

func messagesContainUser(messages []llm.Message, content string) bool {
	for _, msg := range messages {
		if msg.Role == llm.RoleUser && msg.Content == content {
			return true
		}
	}
	return false
}

func messagesContainText(messages []llm.Message, text string) bool {
	for _, msg := range messages {
		if strings.Contains(msg.Content, text) {
			return true
		}
	}
	return false
}

func messagesContainRawText(messages []string, text string) bool {
	for _, msg := range messages {
		if strings.Contains(msg, text) {
			return true
		}
	}
	return false
}

func cleanupRuntime(t *testing.T, runtime *Runtime) {
	t.Helper()
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("close runtime: %v", err)
		}
	})
}
