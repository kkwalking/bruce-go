//go:build darwin || linux

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDiscoverLinkedWorktreeLayout(t *testing.T) {
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

	layout, err := discoverGitLayout(linked)
	if err != nil {
		t.Fatal(err)
	}
	if layout.GitDir == "" || layout.CommonDir == "" || layout.GitDir == layout.CommonDir {
		t.Fatalf("unexpected layout: %+v", layout)
	}
	if !pathContains(filepath.Join(layout.CommonDir, "worktrees"), layout.GitDir) {
		t.Fatalf("gitdir is not under common/worktrees: %+v", layout)
	}
	if !containsPath(layout.WriteRoots, layout.GitDir) || !containsPath(layout.WriteRoots, layout.CommonDir) {
		t.Fatalf("missing linked-worktree write roots: %+v", layout)
	}
	for _, protected := range []string{
		filepath.Join(layout.GitDir, "commondir"),
		filepath.Join(layout.GitDir, "gitdir"),
		filepath.Join(layout.CommonDir, "config"),
		filepath.Join(layout.CommonDir, "hooks"),
	} {
		if !containsPath(layout.ProtectedPaths, protected) {
			t.Fatalf("missing protected path %s: %+v", protected, layout)
		}
	}
}

func TestDiscoverGitLayoutRejectsArbitraryPointer(t *testing.T) {
	workspace := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte("gitdir: "+target+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverGitLayout(workspace); err == nil {
		t.Fatal("arbitrary gitdir pointer should be rejected")
	}
}

func runGit(t *testing.T, git string, args ...string) {
	t.Helper()
	cmd := exec.Command(git, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func containsPath(paths []string, target string) bool {
	target = filepath.Clean(target)
	for _, path := range paths {
		if filepath.Clean(path) == target {
			return true
		}
	}
	return false
}
