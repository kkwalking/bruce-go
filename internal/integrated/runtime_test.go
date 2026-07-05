package integrated

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bruce-go/internal/agent"
	"bruce-go/internal/llm"
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

func TestPlanModePersistsOnlyTopLevelMessages(t *testing.T) {
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
