package integrated

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bruce-go/internal/agent"
	"bruce-go/internal/event"
	"bruce-go/internal/llm"
	bruntime "bruce-go/internal/runtime"
	"bruce-go/internal/web"
)

func TestRuntimeHandlesTaskAndSlashCommands(t *testing.T) {
	client := &agent.FakeClient{Responses: []llm.ChatResponse{{Content: "done"}}}
	rt, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: client})
	if err != nil {
		t.Fatal(err)
	}
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
	if !strings.Contains(status.Output, "RAG: 关闭") || !strings.Contains(status.Output, "fake-model") {
		t.Fatalf("status = %s", status.Output)
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

func TestRuntimeWebCommandUsesFakeSearcher(t *testing.T) {
	rt, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: &agent.FakeClient{}})
	if err != nil {
		t.Fatal(err)
	}
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
