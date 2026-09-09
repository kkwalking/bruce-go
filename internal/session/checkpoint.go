package session

import (
	"errors"
	"fmt"

	"bruce-go/internal/checkpoint"
	"bruce-go/internal/llm"
)

func taskFromPath(path []Entry) checkpoint.Task {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].Checkpoint != nil {
			return *path[i].Checkpoint
		}
	}
	return checkpoint.Task{}
}

// AppendCheckpoint optionally commits the initial user message in the same
// record, so recovery cannot lose the objective between two writes.
func (s *Store) AppendCheckpoint(task checkpoint.Task, input *llm.Message) error {
	if task.ID == "" {
		return errors.New("checkpoint requires a task ID")
	}
	task.UpdatedAt = now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := Entry{Type: TypeCheckpoint, ID: newEntryID(), ParentID: s.ActiveLeaf, Timestamp: now(), Checkpoint: &task}
	if input != nil {
		message := input.WithoutImages()
		entry.Type, entry.Message = TypeMessage, &message
	}
	return s.appendBranchLocked(entry)
}

// JournalToolResult records completion immediately, independently of the
// ordered model transcript. It is never sent to the model twice.
func (s *Store) JournalToolResult(message llm.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendBranchLocked(Entry{Type: TypeToolResult, ID: newEntryID(), ParentID: s.ActiveLeaf, Timestamp: now(), Message: &message})
}

// RepairPendingTools fills gaps using durable completion records. Calls with
// no result are marked unknown, never executed again by the recovery layer.
func (s *Store) RepairPendingTools() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.activePathLocked()
	var calls []llm.ToolCall
	results := map[string]llm.Message{}
	recorded := map[string]bool{}
	for _, entry := range path {
		if entry.Message == nil {
			continue
		}
		msg := *entry.Message
		if entry.Type == TypeMessage && msg.Role == llm.RoleAssistant && len(msg.ToolCalls) > 0 {
			calls = msg.ToolCalls
			results, recorded = map[string]llm.Message{}, map[string]bool{}
		}
		if entry.Type == TypeToolResult {
			results[msg.ToolCallID] = msg
		}
		if entry.Type == TypeMessage && msg.Role == llm.RoleTool {
			recorded[msg.ToolCallID] = true
		}
	}
	var unknown []string
	for _, call := range calls {
		if recorded[call.ID] {
			continue
		}
		msg, ok := results[call.ID]
		if !ok {
			unknown = append(unknown, call.Function.Name+" ("+call.ID+")")
			msg = llm.ToolMessage(call.ID, fmt.Sprintf("[Interrupted tool call: outcome unknown] %s may have executed before interruption. Do not blindly repeat it. Inspect files, process state, or the external service first; ask the user if its outcome cannot be verified.", call.Function.Name))
		}
		if err := s.appendBranchLocked(Entry{Type: TypeMessage, ID: newEntryID(), ParentID: s.ActiveLeaf, Timestamp: now(), Message: &msg}); err != nil {
			return unknown, err
		}
	}
	return unknown, nil
}
