package planning

import (
	"os"
	"path/filepath"
	"testing"

	"bruce-go/internal/runtime"
)

type recordingStore struct {
	events []runtime.PlanEvent
}

func (r *recordingStore) AppendPlanEvent(event runtime.PlanEvent) error {
	r.events = append(r.events, event)
	return nil
}

func TestStoreReplaceReadAndRecover(t *testing.T) {
	recorder := &recordingStore{}
	store := NewStore(t.TempDir(), recorder)

	state, err := store.Replace(runtime.PlanState{}, "# Plan\n\n- Step", "create")
	if err != nil {
		t.Fatal(err)
	}
	if state.ID == "" || state.Revision != 1 || state.Action != runtime.PlanActionCreated || state.SHA256 == "" {
		t.Fatalf("state = %+v", state)
	}
	if len(recorder.events) != 1 || recorder.events[0].Action != runtime.PlanActionCreated {
		t.Fatalf("events = %+v", recorder.events)
	}
	content, err := store.Read(state)
	if err != nil {
		t.Fatal(err)
	}
	if content != "# Plan\n\n- Step" {
		t.Fatalf("content = %q", content)
	}

	state, err = store.Replace(state, "# Plan\n\n- Step\n- Verify", "update")
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 2 || state.Action != runtime.PlanActionUpdated {
		t.Fatalf("updated state = %+v", state)
	}
	if len(recorder.events) != 2 || recorder.events[1].Action != runtime.PlanActionUpdated {
		t.Fatalf("events = %+v", recorder.events)
	}

	if err := os.Remove(state.Path); err != nil {
		t.Fatal(err)
	}
	recovered := store.Recover(state)
	if recovered.MissingFile || !recovered.RecoveredFromSnapshot {
		t.Fatalf("recovered = %+v", recovered)
	}
	data, err := os.ReadFile(state.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != state.Content {
		t.Fatalf("recovered content = %q", data)
	}
}

func TestStoreRejectsPlanSymlink(t *testing.T) {
	home := t.TempDir()
	store := NewStore(home, nil)
	if err := os.MkdirAll(store.Directory(), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.md")
	link := filepath.Join(store.Directory(), "plan_link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := store.Replace(runtime.PlanState{ID: "plan_link", Path: link}, "content", "")
	if err == nil {
		t.Fatal("expected symlink write to be rejected")
	}
}
