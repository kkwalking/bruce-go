//go:build darwin || linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func discoverGitLayout(workspace string) (GitLayout, error) {
	var err error
	workspace, err = canonicalAbsolute(workspace)
	if err != nil {
		return GitLayout{}, err
	}
	marker := filepath.Join(workspace, ".git")
	info, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return GitLayout{}, nil
	}
	if err != nil {
		return GitLayout{}, err
	}

	var gitDir string
	if info.IsDir() {
		gitDir, err = canonicalAbsolute(marker)
	} else if info.Mode().IsRegular() {
		data, readErr := os.ReadFile(marker)
		if readErr != nil {
			return GitLayout{}, readErr
		}
		line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
		value, ok := strings.CutPrefix(line, "gitdir:")
		if !ok || strings.TrimSpace(value) == "" {
			return GitLayout{}, errors.New("invalid .git gitdir pointer")
		}
		value = strings.TrimSpace(value)
		if !filepath.IsAbs(value) {
			value = filepath.Join(workspace, value)
		}
		gitDir, err = canonicalAbsolute(value)
	} else {
		return GitLayout{}, errors.New(".git is neither a directory nor a regular file")
	}
	if err != nil {
		return GitLayout{}, err
	}
	if err := ownedDirectory(gitDir); err != nil {
		return GitLayout{}, fmt.Errorf("untrusted gitdir: %w", err)
	}

	commonDir := gitDir
	if data, readErr := os.ReadFile(filepath.Join(gitDir, "commondir")); readErr == nil {
		value := strings.TrimSpace(string(data))
		if value == "" {
			return GitLayout{}, errors.New("commondir is empty")
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(gitDir, value)
		}
		commonDir, err = canonicalAbsolute(value)
		if err != nil {
			return GitLayout{}, err
		}
	}
	if err := ownedDirectory(commonDir); err != nil {
		return GitLayout{}, fmt.Errorf("untrusted Git common directory: %w", err)
	}
	for _, required := range []string{"HEAD", "objects", "refs"} {
		if _, statErr := os.Stat(filepath.Join(commonDir, required)); statErr != nil {
			return GitLayout{}, fmt.Errorf("Git common directory is missing %s", required)
		}
	}

	linked := gitDir != commonDir
	if linked {
		worktreesRoot := filepath.Join(commonDir, "worktrees")
		if !pathContains(worktreesRoot, gitDir) {
			return GitLayout{}, errors.New("linked-worktree gitdir is not under common/worktrees")
		}
		backPointer, readErr := os.ReadFile(filepath.Join(gitDir, "gitdir"))
		if readErr != nil {
			return GitLayout{}, errors.New("linked worktree is missing its gitdir backlink")
		}
		if filepath.Clean(strings.TrimSpace(string(backPointer))) != filepath.Clean(marker) {
			return GitLayout{}, errors.New("linked-worktree gitdir backlink does not match the workspace")
		}
	}

	layout := GitLayout{MarkerPath: marker, GitDir: gitDir, CommonDir: commonDir}
	if !linked {
		layout.WriteRoots = []string{commonDir}
		layout.ProtectedPaths = []string{
			filepath.Join(commonDir, "config"),
			filepath.Join(commonDir, "hooks"),
			filepath.Join(commonDir, "info"),
			filepath.Join(commonDir, "objects", "info"),
			filepath.Join(commonDir, "worktrees"),
		}
	} else {
		layout.WriteRoots = []string{
			gitDir,
			commonDir,
		}
		layout.ProtectedPaths = []string{
			marker,
			filepath.Join(gitDir, "commondir"),
			filepath.Join(gitDir, "gitdir"),
			filepath.Join(gitDir, "config.worktree"),
			filepath.Join(commonDir, "config"),
			filepath.Join(commonDir, "hooks"),
			filepath.Join(commonDir, "info"),
			filepath.Join(commonDir, "objects", "info"),
		}
		worktreesRoot := filepath.Join(commonDir, "worktrees")
		if entries, readErr := os.ReadDir(worktreesRoot); readErr == nil {
			for _, entry := range entries {
				other := filepath.Join(worktreesRoot, entry.Name())
				if filepath.Clean(other) != filepath.Clean(gitDir) {
					layout.ProtectedPaths = append(layout.ProtectedPaths, other)
				}
			}
		}
	}
	layout.WriteRoots = uniqueCleanPaths(layout.WriteRoots)
	layout.ProtectedPaths = uniqueCleanPaths(layout.ProtectedPaths)
	return layout, nil
}

func ownedDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine directory owner")
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("directory uid=%d, current uid=%d", stat.Uid, os.Geteuid())
	}
	return nil
}
