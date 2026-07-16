//go:build darwin

package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSeatbeltWorkspaceAndSensitivePaths(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	secretDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(secretDir, "id_test")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(context.Background(), Options{Workspace: workspace, HomeDir: home, Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if status := manager.Status(); !status.Capabilities.Available {
		t.Fatalf("Seatbelt unavailable: %+v", status)
	}

	result, err := manager.Run(context.Background(), "printf inside > inside.txt", 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("workspace write = %+v, %v", result, err)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "inside.txt")); err != nil || string(data) != "inside" {
		t.Fatalf("workspace content = %q, %v", data, err)
	}

	result, err = manager.Run(context.Background(), "printf outside > "+shellQuote(outside), 10*time.Second, 4000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("outside write unexpectedly succeeded: %+v", result)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside file exists: %v", err)
	}

	result, err = manager.Run(context.Background(), "cat "+shellQuote(secret), 10*time.Second, 4000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 || strings.Contains(result.Output, "secret") {
		t.Fatalf("sensitive read unexpectedly succeeded: %+v", result)
	}
}

func TestSeatbeltNetworkToggle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("network-ok")) }))
	defer server.Close()
	manager, err := New(context.Background(), Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	result, err := manager.Run(context.Background(), "/usr/bin/curl -fsS "+shellQuote(server.URL), 10*time.Second, 4000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("network should be denied: %+v", result)
	}
	manager.SetNetworkAccess(true)
	result, err = manager.Run(context.Background(), "/usr/bin/curl -fsS "+shellQuote(server.URL), 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Output, "network-ok") {
		t.Fatalf("network-enabled request = %+v, %v", result, err)
	}
}

func TestSeatbeltProtectsGitConfigButAllowsBranch(t *testing.T) {
	workspace := t.TempDir()
	cmd := exec.Command("/usr/bin/git", "init", workspace)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", workspace, "add", "README.md"},
		{"-C", workspace, "-c", "user.name=Bruce Test", "-c", "user.email=bruce@example.test", "commit", "-m", "init"},
	} {
		cmd = exec.Command("/usr/bin/git", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	manager, err := New(context.Background(), Options{Workspace: workspace, HomeDir: t.TempDir(), Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	result, err := manager.Run(context.Background(), "printf malicious > .git/config", 10*time.Second, 4000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("git config write unexpectedly succeeded: %+v", result)
	}
	result, err = manager.Run(context.Background(), "/usr/bin/git branch sandbox-test", 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("git branch = %+v, %v", result, err)
	}
}

func TestSeatbeltAllowsBranchInLinkedWorktree(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	repo := t.TempDir()
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, git, "init", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, "-C", repo, "add", "README.md")
	runGit(t, git, "-C", repo, "-c", "user.name=Bruce Test", "-c", "user.email=bruce@example.test", "commit", "-m", "init")
	runGit(t, git, "-C", repo, "worktree", "add", "--detach", linked)
	manager, err := New(context.Background(), Options{Workspace: linked, HomeDir: t.TempDir(), Mode: ModeWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	result, err := manager.Run(context.Background(), git+" switch -c sandbox-linked", 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("linked branch = %+v, %v", result, err)
	}
	if data, err := os.ReadFile(filepath.Join(manager.git.GitDir, "commondir")); err != nil || strings.TrimSpace(string(data)) == "" {
		t.Fatalf("commondir damaged: %q, %v", data, err)
	}
	result, err = manager.Run(context.Background(), git+" pack-refs --all", 10*time.Second, 4000, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("linked pack-refs = %+v, %v", result, err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
