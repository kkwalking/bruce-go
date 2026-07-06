package session

import (
	"strings"
	"testing"

	"bruce-go/internal/llm"
	"bruce-go/internal/runtime"
)

func TestSessionJSONLResumeTreeAndCompact(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	store, err := CreateNew(home, workspace, runtime.ModeReact)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(llm.User("hello")); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(llm.Assistant("world")); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendModeChange(runtime.ModePlan); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendPlanEvent(runtime.PlanEvent{ID: "plan_1", Path: "/tmp/plan_1.md", Action: runtime.PlanActionPresented, Revision: 1, SHA256: "abc", Content: "# Plan"}); err != nil {
		t.Fatal(err)
	}
	entries := store.ActiveEntries()
	if len(entries) < 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	if err := store.AppendCompaction("summary", entries[0].ID, 123, nil); err != nil {
		t.Fatal(err)
	}
	tree := store.RenderTree(runtime.ModeReact)
	if !strings.Contains(tree, "Session:") || !strings.Contains(tree, "compact") {
		t.Fatalf("tree = %s", tree)
	}
	ctx := store.Context(runtime.ModeReact)
	if ctx.Mode != runtime.ModePlan {
		t.Fatalf("mode = %s", ctx.Mode)
	}
	if ctx.ActivePlan.ID != "plan_1" || !ctx.ActivePlan.Pending() {
		t.Fatalf("active plan = %+v", ctx.ActivePlan)
	}
	if len(ctx.Messages) == 0 || !strings.Contains(ctx.Messages[0].Content, "summary") {
		t.Fatalf("messages after compaction = %+v", ctx.Messages)
	}

	resumed := NewStore(home, workspace)
	if err := resumed.Resume(store.Header.ID[:8]); err != nil {
		t.Fatal(err)
	}
	if resumed.Context(runtime.ModeReact).SessionID != store.Header.ID {
		t.Fatalf("resumed wrong session")
	}
	summaries, err := resumed.List(runtime.ModeReact)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].MessageCount != 2 {
		t.Fatalf("summaries = %+v", summaries)
	}
	if err := resumed.SelectLeaf(entries[0].ID); err != nil {
		t.Fatal(err)
	}
	if resumed.Context(runtime.ModeReact).ActiveLeaf != entries[0].ID {
		t.Fatalf("active leaf = %q", resumed.Context(runtime.ModeReact).ActiveLeaf)
	}
}

func TestPlanEventDoesNotDetermineMode(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	store, err := CreateNew(home, workspace, runtime.ModeReact)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendPlanEvent(runtime.PlanEvent{ID: "plan_1", Action: runtime.PlanActionPresented, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	ctx := store.Context(runtime.ModeReact)
	if ctx.Mode != runtime.ModeReact {
		t.Fatalf("mode should come from mode_change, got %s", ctx.Mode)
	}
	if ctx.ActivePlan.ID != "plan_1" || !ctx.ActivePlan.Pending() {
		t.Fatalf("active plan = %+v", ctx.ActivePlan)
	}
}
