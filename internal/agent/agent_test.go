package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"bruce-go/internal/event"
	"bruce-go/internal/llm"
	"bruce-go/internal/runtime"
	"bruce-go/internal/tool"
)

func TestReActRunsToolCallsAndReturnsFinalAnswer(t *testing.T) {
	registry := tool.EmptyRegistry(t.TempDir())
	registry.Register(tool.Tool{
		Name:        "echo",
		Description: "echo tool",
		Parameters:  []byte(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Exec: func(_ context.Context, args map[string]string) (string, error) {
			return "tool:" + args["text"], nil
		},
		PromptSnippet: "echo input",
	})
	client := &FakeClient{Responses: []llm.ChatResponse{
		{ToolCalls: []llm.ToolCall{{ID: "call_1", Function: llm.FunctionCall{Name: "echo", Arguments: `{"text":"hello"}`}}}},
		{Content: "完成"},
	}}
	a := New(client, registry, "", runtime.DefaultConcurrency(), nil)

	out, err := a.Run(context.Background(), llm.PreparedInput{Text: "run", Message: llm.User("run")}, "", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if out != "完成" {
		t.Fatalf("out = %q", out)
	}
	if client.Calls != 2 {
		t.Fatalf("calls = %d", client.Calls)
	}
	foundToolResult := false
	for _, msg := range a.History {
		if msg.Role == llm.RoleTool && strings.Contains(msg.Content, "tool:hello") {
			foundToolResult = true
		}
	}
	if !foundToolResult {
		t.Fatalf("history missing tool result: %+v", a.History)
	}
}

func TestReActEmitsDurableToolTranscriptInProtocolOrder(t *testing.T) {
	registry := tool.EmptyRegistry(t.TempDir())
	registry.Register(tool.Tool{
		Name:        "echo",
		Description: "echo tool",
		Parameters:  []byte(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Exec: func(_ context.Context, args map[string]string) (string, error) {
			return "tool:" + args["text"], nil
		},
	})
	bus := event.NewBus()
	var events []event.Event
	bus.Subscribe(func(evt event.Event) { events = append(events, evt) })
	client := &FakeClient{Responses: []llm.ChatResponse{
		{ToolCalls: []llm.ToolCall{{ID: "call_1", Function: llm.FunctionCall{Name: "echo", Arguments: `{"text":"hello"}`}}}},
		{Content: "完成"},
	}}
	a := New(client, registry, "", runtime.DefaultConcurrency(), bus)

	if _, err := a.Run(context.Background(), llm.PreparedInput{Text: "run", Message: llm.User("run")}, "", "run_1"); err != nil {
		t.Fatal(err)
	}

	got := eventLabels(events)
	want := []string{
		"message:user",
		"message_started:assistant",
		"message:assistant",
		"tool_started:echo",
		"tool_completed:echo",
		"message:tool",
		"message_started:assistant",
		"message:assistant",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", got)
	}
	completed := completedMessages(events)
	if len(completed) != 4 || !completed[1].HasToolCalls() || completed[2].ToolCallID != "call_1" {
		t.Fatalf("completed messages = %+v", completed)
	}
}

func TestNetworkErrorAssistantIsNonDurable(t *testing.T) {
	bus := event.NewBus()
	var completed []event.MessageCompleted
	bus.Subscribe(func(evt event.Event) {
		if msg, ok := evt.(event.MessageCompleted); ok {
			completed = append(completed, msg)
		}
	})
	a := New(&FakeClient{Err: errors.New("boom")}, tool.EmptyRegistry(t.TempDir()), "", runtime.DefaultConcurrency(), bus)

	out, err := a.Run(context.Background(), llm.PreparedInput{Text: "run", Message: llm.User("run")}, "", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "网络错误") {
		t.Fatalf("out = %q", out)
	}
	if len(completed) != 2 || !completed[0].Durable || completed[0].Message.Role != llm.RoleUser || completed[1].Durable || completed[1].Message.Role != llm.RoleAssistant {
		t.Fatalf("completed = %+v", completed)
	}
}

func TestSkillToolResultIsRedactedAfterTask(t *testing.T) {
	registry := tool.EmptyRegistry(t.TempDir())
	registry.Register(tool.Tool{
		Name:        "load_skill",
		Description: "load skill",
		Parameters:  []byte(`{"type":"object","properties":{"name":{"type":"string"}}}`),
		Exec: func(context.Context, map[string]string) (string, error) {
			return "SECRET_SKILL_INSTRUCTIONS", nil
		},
	})
	client := &recordingClient{responses: []llm.ChatResponse{
		{ToolCalls: []llm.ToolCall{{ID: "load_1", Function: llm.FunctionCall{Name: "load_skill", Arguments: `{"name":"review"}`}}}},
		{Content: "完成"},
	}}
	a := New(client, registry, "", runtime.DefaultConcurrency(), nil)

	if _, err := a.Run(context.Background(), llm.PreparedInput{Text: "run", Message: llm.User("run")}, "", "run_1"); err != nil {
		t.Fatal(err)
	}
	if len(client.calls) < 2 || !messagesContain(client.calls[1], "SECRET_SKILL_INSTRUCTIONS") {
		t.Fatalf("same-task model call did not receive raw skill result: %+v", client.calls)
	}
	if messagesContain(a.History, "SECRET_SKILL_INSTRUCTIONS") || !messagesContain(a.History, "已从历史中移除") {
		t.Fatalf("history was not redacted: %+v", a.History)
	}
}

func TestReActPrunesOlderImages(t *testing.T) {
	a := New(&FakeClient{Responses: []llm.ChatResponse{{Content: "ok"}}}, tool.EmptyRegistry(t.TempDir()), "", runtime.DefaultConcurrency(), nil)
	a.RestoreHistory([]llm.Message{
		llm.UserParts([]llm.ContentPart{llm.ImagePart("data:image/png;base64,a", "image/png", "old")}),
	})
	_, err := a.Run(context.Background(), llm.PreparedInput{
		Text:    "new",
		Message: llm.UserParts([]llm.ContentPart{llm.TextPart("new"), llm.ImagePart("data:image/png;base64,b", "image/png", "new")}),
	}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	imageMessages := 0
	for _, msg := range a.History {
		if msg.HasImages() {
			imageMessages++
		}
	}
	if imageMessages != 1 {
		t.Fatalf("expected only latest image to remain, got %d", imageMessages)
	}
}

func eventLabels(events []event.Event) []string {
	var labels []string
	for _, evt := range events {
		switch e := evt.(type) {
		case event.MessageCompleted:
			labels = append(labels, "message:"+e.Message.Role)
		case event.MessageStarted:
			labels = append(labels, "message_started:"+e.Role)
		case event.ToolCallStarted:
			labels = append(labels, "tool_started:"+e.ToolCall.Function.Name)
		case event.ToolCallCompleted:
			labels = append(labels, "tool_completed:"+e.Result.ToolCall.Function.Name)
		}
	}
	return labels
}

func completedMessages(events []event.Event) []llm.Message {
	var messages []llm.Message
	for _, evt := range events {
		if completed, ok := evt.(event.MessageCompleted); ok && completed.Durable {
			messages = append(messages, completed.Message)
		}
	}
	return messages
}

func messagesContain(messages []llm.Message, text string) bool {
	for _, msg := range messages {
		if strings.Contains(msg.Content, text) {
			return true
		}
	}
	return false
}

type recordingClient struct {
	responses []llm.ChatResponse
	calls     [][]llm.Message
}

func (c *recordingClient) Chat(_ context.Context, messages []llm.Message, _ []llm.ToolDefinition, opts llm.StreamOptions) (llm.ChatResponse, error) {
	copied := append([]llm.Message(nil), messages...)
	c.calls = append(c.calls, copied)
	if len(c.calls) > len(c.responses) {
		return llm.ChatResponse{}, errors.New("recording client response exhausted")
	}
	resp := c.responses[len(c.calls)-1]
	if opts.OnContent != nil && resp.Content != "" {
		opts.OnContent(resp.Content)
	}
	return resp, nil
}

func (*recordingClient) ProviderName() string        { return "fake" }
func (*recordingClient) ModelName() string           { return "fake-model" }
func (*recordingClient) MaxContextWindow() int       { return 200000 }
func (*recordingClient) SupportsTools() bool         { return true }
func (*recordingClient) SupportsPromptCaching() bool { return false }
func (*recordingClient) SupportsImages() bool         { return false }

func TestAgentRetriesOnNetworkError(t *testing.T) {
	bus := event.NewBus()
	client := &FakeClient{
		Responses: []llm.ChatResponse{{Content: "ok"}},
		Err:       errors.New("HTTP 502 Bad Gateway"),
	}
	a := New(client, tool.EmptyRegistry(t.TempDir()), "", runtime.DefaultConcurrency(), bus)
	out, err := a.Run(context.Background(), llm.PreparedInput{Text: "run", Message: llm.User("run")}, "", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "网络错误") {
		t.Fatalf("expected network error, got %q", out)
	}
}

func TestAgentRetryExhaustsAndReturnsError(t *testing.T) {
	bus := event.NewBus()
	client := &FakeClient{
		Responses: []llm.ChatResponse{{Content: "ok"}},
		Err:       errors.New("HTTP 503 Service Unavailable"),
	}
	a := New(client, tool.EmptyRegistry(t.TempDir()), "", runtime.DefaultConcurrency(), bus)
	out, err := a.Run(context.Background(), llm.PreparedInput{Text: "run", Message: llm.User("run")}, "", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "网络错误") {
		t.Fatalf("expected network error in output, got %q", out)
	}
}
