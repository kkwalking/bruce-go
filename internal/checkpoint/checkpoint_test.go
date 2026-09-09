package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureDetectsContentChangesWithoutSizeOrTimestampChange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	before, err := Capture(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := Capture(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if changes := before.Changes(after); len(changes) != 1 || changes[0] != "modified: file" {
		t.Fatalf("changes: %v", changes)
	}
}

func TestCaptureGitIgnoredFilesAndUntrackedChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	git("init", "-q")
	for name, content := range map[string]string{".gitignore": "ignored\n", "tracked": "a", "ignored": "cache"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".gitignore", "tracked")
	before, err := Capture(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Git {
		t.Fatal("git workspace not detected")
	}
	if _, ok := before.Files["ignored"]; ok {
		t.Fatal("ignored file was captured")
	}
	if err := os.Remove(filepath.Join(root, "tracked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new file"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := Capture(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	changes := strings.Join(before.Changes(after), "\n")
	if !strings.Contains(changes, "deleted: tracked") || !strings.Contains(changes, "added: new file") {
		t.Fatalf("changes: %s", changes)
	}
}

func TestCaptureSymlinkTargetAndCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("missing-a", filepath.Join(root, "link")); err != nil {
		t.Skip(err)
	}
	before, err := Capture(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(root, "link"))
	_ = os.Symlink("missing-b", filepath.Join(root, "link"))
	after, err := Capture(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Changes(after)) != 1 {
		t.Fatal("symlink target change was not detected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Capture(ctx, root); err == nil {
		t.Fatal("capture ignored cancellation")
	}
}
