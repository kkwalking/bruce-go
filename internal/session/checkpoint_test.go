package session

import (
	"os"
	"strings"
	"testing"

	"bruce-go/internal/checkpoint"
	"bruce-go/internal/llm"
	"bruce-go/internal/runtime"
)

func TestCheckpointAtomicObjectiveAndBranchRecovery(t *testing.T) {
	s, err := CreateNew(t.TempDir(), t.TempDir(), runtime.ModeReact)
	if err != nil {
		t.Fatal(err)
	}
	input := llm.User("fix the bug")
	first := checkpoint.Task{ID: "first", Objective: input.Content, Status: checkpoint.Running}
	if err := s.AppendCheckpoint(first, &input); err != nil {
		t.Fatal(err)
	}
	leaf := s.ActiveLeaf
	second := checkpoint.Task{ID: "second", Objective: "other task", Status: checkpoint.Completed}
	if err := s.AppendCheckpoint(second, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SelectLeaf(leaf); err != nil {
		t.Fatal(err)
	}
	reopened := NewStore(s.HomeDir, s.Workspace)
	if err := reopened.Resume(s.File); err != nil {
		t.Fatal(err)
	}
	state := reopened.Context(runtime.ModeReact)
	if state.Task.ID != "first" || len(state.Messages) != 1 || state.Messages[0].Content != input.Content {
		t.Fatalf("restored wrong branch: %+v", state)
	}
}

func TestCheckpointRepairsPartialBatchExactlyOnce(t *testing.T) {
	s, err := CreateNew(t.TempDir(), t.TempDir(), runtime.ModeReact)
	if err != nil {
		t.Fatal(err)
	}
	assistant := llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: "one", Function: llm.FunctionCall{Name: "write_file"}},
		{ID: "two", Function: llm.FunctionCall{Name: "execute_command"}},
		{ID: "three", Function: llm.FunctionCall{Name: "read_file"}},
	}}
	if err := s.AppendMessage(assistant); err != nil {
		t.Fatal(err)
	}
	// Completion order differs from protocol order, and one result never landed.
	for _, msg := range []llm.Message{llm.ToolMessage("three", "test evidence"), llm.ToolMessage("one", "wrote file")} {
		if err := s.JournalToolResult(msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendMessage(llm.ToolMessage("one", "wrote file")); err != nil {
		t.Fatal(err)
	}
	reopened := NewStore(s.HomeDir, s.Workspace)
	if err := reopened.Resume(s.File); err != nil {
		t.Fatal(err)
	}
	unknown, err := reopened.RepairPendingTools()
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 1 || !strings.Contains(unknown[0], "execute_command") {
		t.Fatalf("unknown calls: %v", unknown)
	}
	state := reopened.Context(runtime.ModeReact)
	if len(state.Messages) != 4 || state.Messages[2].ToolCallID != "two" || !strings.Contains(state.Messages[2].Content, "outcome unknown") || state.Messages[3].Content != "test evidence" {
		t.Fatalf("invalid tool protocol: %+v", state.Messages)
	}
	if _, err := reopened.RepairPendingTools(); err != nil {
		t.Fatal(err)
	}
	if len(reopened.Context(runtime.ModeReact).Messages) != 4 {
		t.Fatal("repair duplicated results")
	}
}

func TestCheckpointTornTailRecoveryAndLargeRecord(t *testing.T) {
	s, err := CreateNew(t.TempDir(), t.TempDir(), runtime.ModeReact)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(llm.User(strings.Repeat("large", 20000))); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.File, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","id":"torn`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	reopened := NewStore(s.HomeDir, s.Workspace)
	if err := reopened.Resume(s.File); err != nil {
		t.Fatal(err)
	}
	if reopened.Context(runtime.ModeReact).MessageCount != 1 {
		t.Fatal("lost valid history")
	}
	if err := reopened.AppendMessage(llm.Assistant("continued")); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(s.File); err != nil {
		t.Fatal(err)
	}
	if s.Context(runtime.ModeReact).MessageCount != 2 {
		t.Fatal("tail repair failed")
	}
	f, err = os.OpenFile(s.File, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{malformed}\n")
	_ = f.Close()
	if err := reopened.Resume(s.File); err == nil {
		t.Fatal("complete corrupt records must not be silently dropped")
	}
}
