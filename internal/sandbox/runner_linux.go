//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type bubblewrapRunner struct {
	path   string
	reason string
}

func newPlatformRunner(workspace string) Runner {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return bubblewrapRunner{reason: "未找到 bwrap，请通过系统包管理器安装 bubblewrap"}
	}
	path, err = canonicalAbsolute(path)
	if err != nil {
		return bubblewrapRunner{reason: err.Error()}
	}
	if pathContains(workspace, path) {
		return bubblewrapRunner{reason: "拒绝使用 workspace 内的 bwrap"}
	}
	if err := validateSystemBubblewrap(path); err != nil {
		return bubblewrapRunner{reason: err.Error()}
	}
	return bubblewrapRunner{path: path}
}

func validateSystemBubblewrap(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("检查 bwrap: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("bwrap 不是普通文件")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err = os.Stat(current)
		if err != nil {
			return fmt.Errorf("检查 bwrap 路径 %s: %w", current, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return fmt.Errorf("bwrap 路径不是 root 所有: %s", current)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("bwrap 路径可被非 root 用户写入: %s", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}

func (r bubblewrapRunner) Name() string { return "bubblewrap" }

func (r bubblewrapRunner) Probe(ctx context.Context) Capabilities {
	if r.path == "" {
		return Capabilities{Backend: "bubblewrap", Reason: r.reason}
	}
	spec := CommandSpec{Directory: "/", Environment: []string{"PATH=/usr/bin:/bin"}, Timeout: 5 * time.Second, MaxOutputChars: 4000}
	args := []string{
		"--die-with-parent", "--new-session", "--unshare-user", "--unshare-pid",
		"--unshare-ipc", "--unshare-uts", "--unshare-net", "--ro-bind", "/", "/",
		"--dev", "/dev", "--proc", "/proc", "--", "/bin/true",
	}
	result, err := runProcess(ctx, r.path, args, spec)
	if err != nil {
		return Capabilities{Backend: "bubblewrap", Reason: err.Error()}
	}
	if result.ExitCode != 0 {
		reason := strings.TrimSpace(result.Output)
		if reason == "" {
			reason = fmt.Sprintf("bwrap probe exit code %d", result.ExitCode)
		}
		return Capabilities{Backend: "bubblewrap", Reason: reason}
	}
	return Capabilities{Backend: "bubblewrap", Available: true}
}

func (r bubblewrapRunner) Run(ctx context.Context, spec CommandSpec, policy Policy) (RunResult, error) {
	if r.path == "" {
		return RunResult{}, fmt.Errorf("%w: %s", ErrUnavailable, r.reason)
	}
	args, err := buildBubblewrapArgs(spec, policy)
	if err != nil {
		return RunResult{}, err
	}
	result, err := runProcess(ctx, r.path, args, spec)
	if err != nil {
		return result, fmt.Errorf("Bubblewrap 启动失败: %w", err)
	}
	return result, nil
}

func (r bubblewrapRunner) PrepareProcess(spec ProcessSpec, policy Policy) (PreparedProcess, error) {
	if r.path == "" {
		return PreparedProcess{}, fmt.Errorf("%w: %s", ErrUnavailable, r.reason)
	}
	if strings.TrimSpace(spec.Program) == "" {
		return PreparedProcess{}, fmt.Errorf("%w: program 不能为空", ErrPolicy)
	}
	args, err := buildBubblewrapProcessArgs(spec, policy)
	if err != nil {
		return PreparedProcess{}, err
	}
	return PreparedProcess{
		Program:     r.path,
		Args:        args,
		Directory:   spec.Directory,
		Environment: append([]string(nil), spec.Environment...),
	}, nil
}

func buildBubblewrapArgs(spec CommandSpec, policy Policy) ([]string, error) {
	return buildBubblewrapProcessArgs(ProcessSpec{
		Program:     "/bin/bash",
		Args:        []string{"--noprofile", "--norc", "-c", spec.Command},
		Directory:   spec.Directory,
		Environment: spec.Environment,
	}, policy)
}

func buildBubblewrapProcessArgs(spec ProcessSpec, policy Policy) ([]string, error) {
	args := []string{
		"--die-with-parent", "--new-session", "--unshare-user", "--unshare-pid",
		"--unshare-ipc", "--unshare-uts",
	}
	if !policy.NetworkAccess {
		args = append(args, "--unshare-net")
	}
	args = append(args,
		"--ro-bind", "/", "/",
		"--tmpfs", "/tmp",
		"--dev", "/dev",
		"--proc", "/proc",
		"--bind", policy.TempRoot, policy.TempRoot,
	)
	if policy.Mode == ModeWorkspaceWrite {
		args = append(args, "--bind", policy.WorkspaceRoot, policy.WorkspaceRoot)
	} else {
		args = append(args, "--ro-bind", policy.WorkspaceRoot, policy.WorkspaceRoot)
	}
	for _, path := range policy.Git.WriteRoots {
		if _, err := os.Lstat(path); err == nil {
			args = append(args, "--bind", path, path)
		}
	}
	for _, path := range policy.Git.ProtectedPaths {
		if _, err := os.Lstat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	for _, path := range uniqueCleanPaths(append(policy.SensitivePaths, policy.SocketPaths...)) {
		canonical := path
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			canonical = resolved
		}
		info, err := os.Lstat(canonical)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("检查敏感路径 %s: %w", canonical, err)
		}
		if info.IsDir() {
			args = append(args, "--tmpfs", canonical)
		} else {
			args = append(args, "--ro-bind", "/dev/null", canonical)
		}
	}
	args = append(args, "--clearenv")
	for _, item := range spec.Environment {
		name, value, ok := strings.Cut(item, "=")
		if ok {
			args = append(args, "--setenv", name, value)
		}
	}
	args = append(args, "--chdir", spec.Directory, "--", spec.Program)
	args = append(args, spec.Args...)
	return args, nil
}
