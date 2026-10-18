package planning

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bruce-go/internal/approval"
	"bruce-go/internal/runtime"
	"bruce-go/internal/tool"
)

func TestPlanToolsOperateOnlyOnActivePlan(t *testing.T) {
	recorder := &recordingStore{}
	store := NewStore(t.TempDir(), recorder)
	var active runtime.PlanState
	registry := tool.EmptyRegistry(t.TempDir())
	RegisterPlanTools(registry, store, func() runtime.PlanState { return active })

	out := registry.Execute(context.Background(), "replace_plan", tool.Args{"content": "# Plan", "summary": "create"})
	if !strings.Contains(out, "Plan created") {
		t.Fatalf("replace_plan output = %s", out)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("events = %+v", recorder.events)
	}
	active = runtime.PlanState{
		ID:       recorder.events[0].ID,
		Path:     recorder.events[0].Path,
		Action:   recorder.events[0].Action,
		Revision: recorder.events[0].Revision,
		SHA256:   recorder.events[0].SHA256,
		Content:  recorder.events[0].Content,
	}

	out = registry.Execute(context.Background(), "edit_plan", tool.Args{"old_text": "# Plan", "new_text": "# Better Plan", "summary": "edit"})
	if !strings.Contains(out, "Plan edited") {
		t.Fatalf("edit_plan output = %s", out)
	}
	if len(recorder.events) != 2 || recorder.events[1].Action != runtime.PlanActionUpdated {
		t.Fatalf("events = %+v", recorder.events)
	}
}

func TestPlanToolRegistryBlocksWorkspaceWritesAndMutatingCommands(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := tool.NewRegistry(workspace)
	store := NewStore(t.TempDir(), nil)
	registry := NewToolRegistry(base, store, func() runtime.PlanState { return runtime.PlanState{} })

	if _, ok := registry.Lookup("write_file"); ok {
		t.Fatal("write_file should not be available in plan registry")
	}
	if _, ok := registry.Lookup("edit_file"); ok {
		t.Fatal("edit_file should not be available in plan registry")
	}
	// cat, not rg: the command must exist on every machine that runs the
	// suite. rg is only *suggested* to the model (tool.go's guidelines), it
	// is not a dependency, and CI runners have no ripgrep — this test failed
	// there with "bash: rg: command not found" (exit 127).
	out := registry.Execute(context.Background(), "execute_command", tool.Args{"command": "cat a.txt"})
	if !strings.Contains(out, "alpha") {
		t.Fatalf("read-only command output = %s", out)
	}
	out = registry.Execute(context.Background(), "execute_command", tool.Args{"command": "touch x.txt"})
	if !strings.Contains(out, "plan-mode security policy") {
		t.Fatalf("mutating command output = %s", out)
	}
	if _, err := os.Stat(filepath.Join(workspace, "x.txt")); !os.IsNotExist(err) {
		t.Fatalf("mutating command created file, err=%v", err)
	}
}

func TestCheckReadOnlyCommand(t *testing.T) {
	for _, command := range []string{"ls", "pwd", "find . -name '*.go'", "rg foo .", "git status --short", "git diff", "sed -n '1,20p' file.go", "rg foo . | head"} {
		if got := CheckReadOnlyCommand(command); !got.Allowed {
			t.Fatalf("command should be allowed %q: %+v", command, got)
		}
	}
	for _, command := range []string{"go test ./...", "git checkout main", "sed -i s/a/b/g file", "echo x > file", "touch file", "npm install"} {
		if got := CheckReadOnlyCommand(command); got.Allowed {
			t.Fatalf("command should be rejected %q", command)
		}
	}
}

// countingApproval is a HITL handler that refuses every request and records
// how often it was consulted.
type countingApproval struct {
	requests int
}

func (*countingApproval) Enabled() bool     { return true }
func (*countingApproval) SetEnabled(bool)   {}
func (*countingApproval) ClearApprovedAll() {}
func (h *countingApproval) Request(context.Context, approval.Request) (approval.Result, error) {
	h.requests++
	return approval.Reject("denied by the plan-mode test"), nil
}

// TestPlanRegistryInheritsHITLForCopiedTools is the regression for a HITL
// bypass: NewToolRegistry used to build an EmptyRegistry and copy the web
// tools into it, which dropped the parent's approval handler. web_search and
// web_fetch declare a network capability, so NeedsApproval is true outside
// plan mode, but inside plan mode they ran without ever asking.
func TestPlanRegistryInheritsHITLForCopiedTools(t *testing.T) {
	workspace := t.TempDir()
	base := tool.EmptyRegistry(workspace)
	executed := 0
	base.Register(tool.Tool{
		Name:       "web_fetch",
		Parameters: rawSchema(param{"url", "string", "URL", true}),
		Policy: tool.Policy{
			Source:     tool.SourceWeb,
			Capability: tool.Capability{Network: true},
			Risk:       tool.RiskLow, ApprovalReason: "Performs a network request",
		},
		Exec: func(context.Context, tool.Args) (string, error) {
			executed++
			return "fetched", nil
		},
	})
	handler := &countingApproval{}
	base.WithHITL(handler)

	registry := NewToolRegistry(base, NewStore(t.TempDir(), nil), func() runtime.PlanState {
		return runtime.PlanState{}
	})
	out := registry.Execute(context.Background(), "web_fetch", tool.Args{"url": "https://example.com"})
	if !strings.Contains(out, "[HITL] Operation was rejected") {
		t.Fatalf("plan-mode web_fetch did not go through HITL: %q", out)
	}
	if handler.requests != 1 {
		t.Fatalf("HITL requests = %d, want 1", handler.requests)
	}
	if executed != 0 {
		t.Fatalf("web_fetch ran despite rejection, calls = %d", executed)
	}
}
