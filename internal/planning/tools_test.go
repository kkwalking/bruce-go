package planning

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bruce-go/internal/runtime"
	"bruce-go/internal/tool"
)

func TestPlanToolsOperateOnlyOnActivePlan(t *testing.T) {
	recorder := &recordingStore{}
	store := NewStore(t.TempDir(), recorder)
	var active runtime.PlanState
	registry := tool.EmptyRegistry(t.TempDir())
	RegisterPlanTools(registry, store, func() runtime.PlanState { return active })

	out := registry.Execute(context.Background(), "replace_plan", map[string]string{"content": "# Plan", "summary": "create"})
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

	out = registry.Execute(context.Background(), "edit_plan", map[string]string{"old_text": "# Plan", "new_text": "# Better Plan", "summary": "edit"})
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
	out := registry.Execute(context.Background(), "execute_command", map[string]string{"command": "rg alpha ."})
	if !strings.Contains(out, "alpha") {
		t.Fatalf("read-only command output = %s", out)
	}
	out = registry.Execute(context.Background(), "execute_command", map[string]string{"command": "touch x.txt"})
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
