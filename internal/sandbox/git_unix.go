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
			return GitLayout{}, errors.New("无效 .git gitdir 指针")
		}
		value = strings.TrimSpace(value)
		if !filepath.IsAbs(value) {
			value = filepath.Join(workspace, value)
		}
		gitDir, err = canonicalAbsolute(value)
	} else {
		return GitLayout{}, errors.New(".git 既不是目录也不是普通文件")
	}
	if err != nil {
		return GitLayout{}, err
	}
	if err := ownedDirectory(gitDir); err != nil {
		return GitLayout{}, fmt.Errorf("gitdir 不可信: %w", err)
	}

	commonDir := gitDir
	if data, readErr := os.ReadFile(filepath.Join(gitDir, "commondir")); readErr == nil {
		value := strings.TrimSpace(string(data))
		if value == "" {
			return GitLayout{}, errors.New("空 commondir")
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
		return GitLayout{}, fmt.Errorf("git common dir 不可信: %w", err)
	}
	for _, required := range []string{"HEAD", "objects", "refs"} {
		if _, statErr := os.Stat(filepath.Join(commonDir, required)); statErr != nil {
			return GitLayout{}, fmt.Errorf("git common dir 缺少 %s", required)
		}
	}

	linked := gitDir != commonDir
	if linked {
		worktreesRoot := filepath.Join(commonDir, "worktrees")
		if !pathContains(worktreesRoot, gitDir) {
			return GitLayout{}, errors.New("linked worktree gitdir 不在 common/worktrees 下")
		}
		backPointer, readErr := os.ReadFile(filepath.Join(gitDir, "gitdir"))
		if readErr != nil {
			return GitLayout{}, errors.New("linked worktree 缺少 gitdir 回指")
		}
		if filepath.Clean(strings.TrimSpace(string(backPointer))) != filepath.Clean(marker) {
			return GitLayout{}, errors.New("linked worktree gitdir 回指与 workspace 不匹配")
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
		return errors.New("不是目录")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("无法读取目录所有者")
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("目录 uid=%d，当前 uid=%d", stat.Uid, os.Geteuid())
	}
	return nil
}
