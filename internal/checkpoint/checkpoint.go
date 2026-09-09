// Package checkpoint captures task progress and detects workspace drift.
package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	Running     = "running"
	Completed   = "completed"
	Interrupted = "interrupted"
	Failed      = "failed"
	Limited     = "iteration_limit"
)

type Task struct {
	ID        string   `json:"id"`
	Objective string   `json:"objective"`
	Mode      string   `json:"mode"`
	Skills    []string `json:"skills,omitempty"`
	Status    string   `json:"status"`
	Phase     string   `json:"phase"`
	Reason    string   `json:"reason,omitempty"`
	UpdatedAt string   `json:"updatedAt"`
	Workspace Snapshot `json:"workspace"`
}

func (t Task) Resumable() bool { return t.ID != "" && t.Status != Completed }

type Snapshot struct {
	Root   string            `json:"root"`
	Git    bool              `json:"git,omitempty"`
	Head   string            `json:"head,omitempty"`
	Branch string            `json:"branch,omitempty"`
	Files  map[string]string `json:"files"`
}

// Capture hashes regular files and symlink targets, never following symlinks.
// Git workspaces include tracked and non-ignored untracked files. Other
// workspaces include all files except Git metadata and Bruce runtime storage.
func Capture(ctx context.Context, root string) (Snapshot, error) {
	s := Snapshot{Root: filepath.Clean(root), Files: map[string]string{}}
	git := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
		return cmd.Output()
	}
	var paths []string
	if out, err := git("rev-parse", "--is-inside-work-tree"); err == nil && strings.TrimSpace(string(out)) == "true" {
		s.Git = true
		out, err = git("ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", ".")
		if err != nil {
			return s, fmt.Errorf("list checkpoint files: %w", err)
		}
		paths = strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		head, _ := git("rev-parse", "--verify", "HEAD")
		branch, _ := git("symbolic-ref", "--quiet", "HEAD")
		s.Head, s.Branch = strings.TrimSpace(string(head)), strings.TrimSpace(string(branch))
	} else {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if excluded(rel) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.IsDir() {
				paths = append(paths, rel)
			}
			return nil
		})
		if err != nil {
			return s, err
		}
	}
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		if rel == "" || excluded(rel) {
			continue
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return s, fmt.Errorf("invalid checkpoint path %q", rel)
		}
		// A tracked directory may have been replaced by a symlink since the
		// index was written. Capture that link rather than reading through it.
		parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
		for i := 1; i < len(parts); i++ {
			parent := filepath.Join(parts[:i]...)
			info, err := os.Lstat(filepath.Join(root, parent))
			if err == nil && info.Mode()&os.ModeSymlink != 0 {
				rel = parent
				break
			}
		}
		path := filepath.Join(root, rel)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return s, err
		}
		if info.IsDir() {
			continue
		}
		h := sha256.New()
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return s, err
			}
			_, _ = io.WriteString(h, target)
		} else if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return s, err
			}
			_, copyErr := io.Copy(h, &contextReader{ctx: ctx, reader: f})
			closeErr := f.Close()
			if copyErr != nil {
				return s, copyErr
			}
			if closeErr != nil {
				return s, closeErr
			}
		} else {
			continue
		}
		s.Files[filepath.ToSlash(rel)] = fmt.Sprintf("%s:%s", info.Mode().String(), hex.EncodeToString(h.Sum(nil)))
	}
	return s, ctx.Err()
}

func excluded(path string) bool {
	path = filepath.ToSlash(path)
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == ".git" {
			return true
		}
		if part == ".bruce" && i+1 < len(parts) {
			switch parts[i+1] {
			case "sessions", "plans", "history", "history.txt":
				return true
			}
		}
	}
	return false
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s Snapshot) Changes(next Snapshot) []string {
	var changes []string
	if s.Root != next.Root {
		changes = append(changes, "workspace root changed")
	}
	if s.Git != next.Git || s.Head != next.Head || s.Branch != next.Branch {
		changes = append(changes, "Git HEAD or branch changed")
	}
	for path, hash := range s.Files {
		if value, ok := next.Files[path]; !ok {
			changes = append(changes, "deleted: "+path)
		} else if value != hash {
			changes = append(changes, "modified: "+path)
		}
	}
	for path := range next.Files {
		if _, ok := s.Files[path]; !ok {
			changes = append(changes, "added: "+path)
		}
	}
	sort.Strings(changes)
	return changes
}
