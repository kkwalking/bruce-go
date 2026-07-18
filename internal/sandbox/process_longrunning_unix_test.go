//go:build darwin || linux

package sandbox

import (
	"bufio"
	"context"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestManagedProcessCloseKillsProcessTree(t *testing.T) {
	manager, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Mode: ModeFullAccess})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	process, err := manager.StartProcess(context.Background(), ProcessSpec{
		Program: "/bin/sh",
		Args:    []string{"-c", "sleep 60 & child=$!; echo $child; wait"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(process.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("child PID %q: %v", line, err)
	}
	start := time.Now()
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Close took %s", elapsed)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d still exists: %v", childPID, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
