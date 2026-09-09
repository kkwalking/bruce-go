//go:build darwin || linux

package session

import (
	"testing"

	"bruce-go/internal/checkpoint"
	"bruce-go/internal/runtime"
)

func TestCheckpointLeaseExcludesConcurrentExecutionAndRefreshesState(t *testing.T) {
	s, err := CreateNew(t.TempDir(), t.TempDir(), runtime.ModeReact)
	if err != nil {
		t.Fatal(err)
	}
	other := NewStore(s.HomeDir, s.Workspace)
	if err := other.Resume(s.File); err != nil {
		t.Fatal(err)
	}
	release, err := s.AcquireTask()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if unlock, err := other.AcquireTask(); err == nil {
		unlock()
		t.Fatal("second executor acquired the session")
	}
	if err := s.AppendCheckpoint(checkpoint.Task{ID: "task", Status: checkpoint.Completed}, nil); err != nil {
		t.Fatal(err)
	}
	release()
	unlocked, err := other.AcquireTask()
	if err != nil {
		t.Fatal(err)
	}
	defer unlocked()
	if other.Context(runtime.ModeReact).Task.Status != checkpoint.Completed {
		t.Fatal("lease did not refresh stale session state")
	}
}
