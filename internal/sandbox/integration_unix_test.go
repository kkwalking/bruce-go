//go:build darwin || linux

package sandbox

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSandboxReadOnlyOverride(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readable.txt"), []byte("readable"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := newAvailableTestManager(t, workspace)
	if err := manager.SetMode(ModeFullAccess); err != nil {
		t.Fatal(err)
	}
	mode := ModeReadOnly
	result, err := manager.Run(context.Background(), "cat readable.txt", 10*time.Second, 4000, &mode)
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Output, "readable") {
		t.Fatalf("read-only override should allow reads: %+v, %v", result, err)
	}
	result, err = manager.Run(context.Background(), "printf blocked > blocked.txt", 10*time.Second, 4000, &mode)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("read-only override unexpectedly wrote: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(workspace, "blocked.txt")); !os.IsNotExist(err) {
		t.Fatalf("blocked file exists: %v", err)
	}
}

func TestSandboxWorkspaceWriteRejectsOutsideWrite(t *testing.T) {
	manager := newAvailableTestManager(t, t.TempDir())
	outside := filepath.Join(t.TempDir(), "outside.txt")
	result, err := manager.Run(context.Background(), "printf outside > "+posixShellQuote(outside), 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode == 0 {
		t.Fatalf("outside write should fail: %+v, %v", result, err)
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("outside file exists: %v", statErr)
	}
}

func TestSandboxTimeoutKillsDescendants(t *testing.T) {
	workspace := t.TempDir()
	manager := newAvailableTestManager(t, workspace)
	result, err := manager.Run(context.Background(), "sleep 60 & echo $! > child.pid; wait", 200*time.Millisecond, 4000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut {
		t.Fatalf("command should time out: %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	assertProcessGone(t, pid)
}

func TestSandboxCancellationKillsDescendants(t *testing.T) {
	workspace := t.TempDir()
	manager := newAvailableTestManager(t, workspace)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	result, err := manager.Run(ctx, "sleep 60 & echo $! > canceled-child.pid; wait", 10*time.Second, 4000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Canceled || result.TimedOut {
		t.Fatalf("command should be canceled: %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "canceled-child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	assertProcessGone(t, pid)
}

func TestSandboxHandlesQuotedUnicodeWorkspace(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "space ' quote café")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	manager := newAvailableTestManager(t, workspace)
	result, err := manager.Run(context.Background(), "printf ok > 'result file.txt'", 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("special workspace path = %+v, %v", result, err)
	}
	if data, readErr := os.ReadFile(filepath.Join(workspace, "result file.txt")); readErr != nil || string(data) != "ok" {
		t.Fatalf("result = %q, %v", data, readErr)
	}
}

func TestSandboxMasksAgentSocketWhenNetworkEnabled(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "bruce-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "agent.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("SSH_AUTH_SOCK", socketPath)
	manager := newAvailableTestManager(t, t.TempDir())
	manager.SetNetworkAccess(true)
	result, err := manager.Run(context.Background(), "test ! -S "+posixShellQuote(socketPath), 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("agent socket remained visible: %+v, %v", result, err)
	}
}

func TestSandboxBoundsOutputDuringExecution(t *testing.T) {
	manager := newAvailableTestManager(t, t.TempDir())
	result, err := manager.Run(context.Background(), "i=0; while [ $i -lt 10000 ]; do printf x; i=$((i+1)); done", 10*time.Second, 128, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len(result.Output) > 200 {
		t.Fatalf("output was not bounded: len=%d truncated=%v", len(result.Output), result.Truncated)
	}
}

func TestSandboxGitWorkflowAndProtectedMetadata(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	workspace := t.TempDir()
	runGit(t, git, "init", workspace)
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, "-C", workspace, "add", "tracked.txt")
	runGit(t, git, "-C", workspace, "-c", "user.name=Bruce Test", "-c", "user.email=bruce@example.test", "commit", "-m", "initial")
	manager := newAvailableTestManager(t, workspace)

	result, err := manager.Run(context.Background(), "printf malicious > .git/config", 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode == 0 {
		t.Fatalf("git config protection = %+v, %v", result, err)
	}
	for _, protected := range []string{".git/hooks/pre-commit", ".git/objects/info/alternates"} {
		result, err = manager.Run(context.Background(), "printf malicious > "+posixShellQuote(protected), 10*time.Second, 4000, nil)
		if err != nil || result.ExitCode == 0 {
			t.Fatalf("git protected path %s = %+v, %v", protected, result, err)
		}
	}
	result, err = manager.Run(context.Background(), "printf changed > tracked.txt && git add tracked.txt && git -c user.name='Bruce Test' -c user.email=bruce@example.test commit -m changed && git branch sandbox-branch", 10*time.Second, 8000, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("git workflow = %+v, %v", result, err)
	}
}

func TestSandboxLinkedWorktreeProtectsOtherWorktrees(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	repo := t.TempDir()
	linkedRoot := t.TempDir()
	current := filepath.Join(linkedRoot, "current")
	other := filepath.Join(linkedRoot, "other")
	runGit(t, git, "init", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, "-C", repo, "add", "README.md")
	runGit(t, git, "-C", repo, "-c", "user.name=Bruce Test", "-c", "user.email=bruce@example.test", "commit", "-m", "initial")
	runGit(t, git, "-C", repo, "worktree", "add", "--detach", current)
	runGit(t, git, "-C", repo, "worktree", "add", "--detach", other)
	otherLayout, err := discoverGitLayout(other)
	if err != nil {
		t.Fatal(err)
	}
	manager := newAvailableTestManager(t, current)

	quotedGit := posixShellQuote(git)
	result, err := manager.Run(context.Background(), quotedGit+" switch -c sandbox-linked && "+quotedGit+" pack-refs --all", 10*time.Second, 8000, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("linked worktree workflow = %+v, %v", result, err)
	}
	result, err = manager.Run(context.Background(), "printf malicious > "+posixShellQuote(filepath.Join(otherLayout.GitDir, "HEAD")), 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode == 0 {
		t.Fatalf("other worktree protection = %+v, %v", result, err)
	}
}

func posixShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatalf("check descendant process %d: %v", pid, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant process %d still exists", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newAvailableTestManager(t *testing.T, workspace string) *Manager {
	t.Helper()
	manager, err := New(context.Background(), Options{Workspace: workspace, HomeDir: t.TempDir(), Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	status := manager.Status()
	if !status.Capabilities.Available {
		if os.Getenv("BRUCE_REQUIRE_SANDBOX_TESTS") == "1" {
			t.Fatalf("required sandbox unavailable: %+v", status)
		}
		t.Skipf("sandbox unavailable: %+v", status)
	}
	return manager
}
