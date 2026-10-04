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
	result := runMarkedDescendant(t, manager, "child.pid", 200*time.Millisecond)
	if !result.TimedOut {
		t.Fatalf("command should time out: %+v", result)
	}
	assertMarkedDescendantsGone(t)
}

func TestSandboxCancellationKillsDescendants(t *testing.T) {
	workspace := t.TempDir()
	manager := newAvailableTestManager(t, workspace)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	command := "exec -a " + descendantMarker + " sleep 60 & echo $! > canceled-child.pid; wait"
	result, err := manager.Run(ctx, command, 10*time.Second, 4000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Canceled || result.TimedOut {
		t.Fatalf("command should be canceled: %+v", result)
	}
	assertMarkedDescendantsGone(t)
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

// descendantMarker is put in a descendant's argv[0] so the host can find it
// after the process is gone, without knowing its sandbox-internal pid.
const descendantMarker = "bruce-descendant-probe"

// runMarkedDescendant runs a shell that starts a long-lived child carrying
// descendantMarker in its argv[0], records the child's sandbox pid, and then
// waits to be killed. Callers assert the marked child is gone afterwards.
//
// bubblewrap runs with --unshare-pid, so the pid the command reports ($!) is
// the pid *inside* the sandbox: the host sees the same process under a
// different number. Signalling the sandbox pid on the host therefore proves
// nothing — with no host process at that number kill(2) returns ESRCH and the
// old check passed for the wrong reason, and when an unrelated host process
// held the number it returned EPERM, which the old code reported as
// "check descendant process 3: operation not permitted" on CI's ubuntu-latest.
// Finding the descendant by its argv[0] works in both backends.
func runMarkedDescendant(t *testing.T, manager *Manager, pidFile string, timeout time.Duration) RunResult {
	t.Helper()
	// exec -a sets argv[0]; bash runs it so the marker survives into the child.
	command := "exec -a " + descendantMarker + " sleep 60 & echo $! > " + pidFile + "; wait"
	result, err := manager.Run(context.Background(), command, timeout, 4000, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// markedDescendants returns the pids of processes still carrying
// descendantMarker in their argv[0], as the host sees them.
func markedDescendants(t *testing.T) []int {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", descendantMarker).Output()
	if err != nil {
		// pgrep exits 1 when nothing matches, which is the expected case.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil
		}
		if errors.Is(err, exec.ErrNotFound) {
			t.Skip("pgrep is not available to look for the descendant")
		}
		t.Fatalf("pgrep: %v", err)
	}
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		pid, convErr := strconv.Atoi(field)
		if convErr != nil {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// assertMarkedDescendantsGone waits for every process carrying
// descendantMarker to disappear, killing any that outlive the deadline so one
// failure cannot leak a stray `sleep` into later tests.
func assertMarkedDescendantsGone(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		pids := markedDescendants(t)
		if len(pids) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, pid := range pids {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			t.Fatalf("descendant %v survived the sandbox teardown", pids)
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
