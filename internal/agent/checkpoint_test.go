package agent

import (
	"context"
	"errors"
	"testing"

	"bruce-go/internal/llm"
	"bruce-go/internal/runtime"
	"bruce-go/internal/tool"
)

func TestCheckpointPersistenceFailurePreventsToolExecution(t *testing.T) {
	registry := tool.EmptyRegistry(t.TempDir())
	executed := 0
	registry.Register(tool.Tool{Name: "write", Exec: func(context.Context, map[string]string) (string, error) { executed++; return "written", nil }})
	client := &FakeClient{Responses: []llm.ChatResponse{{ToolCalls: []llm.ToolCall{{ID: "call", Function: llm.FunctionCall{Name: "write", Arguments: `{}`}}}}}}
	a := New(client, registry, "", runtime.DefaultConcurrency(), nil)
	failure := errors.New("disk full")
	a.PersistMessage = func(msg llm.Message) error {
		if msg.Role == llm.RoleAssistant {
			return failure
		}
		return nil
	}
	if _, err := a.Run(context.Background(), llm.PreparedInput{Message: llm.User("write")}, "", "run"); !errors.Is(err, failure) {
		t.Fatalf("error: %v", err)
	}
	if executed != 0 {
		t.Fatal("tool executed without a durable checkpoint")
	}
}

func TestCheckpointJournalFailureStopsFurtherTools(t *testing.T) {
	registry := tool.EmptyRegistry(t.TempDir())
	executed := 0
	registry.Register(tool.Tool{Name: "write", Exec: func(context.Context, map[string]string) (string, error) { executed++; return "written", nil }})
	client := &FakeClient{Responses: []llm.ChatResponse{{ToolCalls: []llm.ToolCall{
		{ID: "one", Function: llm.FunctionCall{Name: "write", Arguments: `{}`}},
		{ID: "two", Function: llm.FunctionCall{Name: "write", Arguments: `{}`}},
	}}}}
	a := New(client, registry, "", runtime.DefaultConcurrency(), nil)
	failure := errors.New("journal unavailable")
	a.PersistToolResult = func(llm.Message) error { return failure }
	if _, err := a.Run(context.Background(), llm.PreparedInput{Message: llm.User("write")}, "", "run"); !errors.Is(err, failure) {
		t.Fatalf("error: %v", err)
	}
	if executed != 1 || client.Calls != 1 {
		t.Fatalf("continued after journal failure: executions=%d model=%d", executed, client.Calls)
	}
}
