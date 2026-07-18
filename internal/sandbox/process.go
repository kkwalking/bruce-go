package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

type cappedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(p)
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	if written > remaining {
		b.truncated = true
	}
	return written, nil
}

func (b *cappedBuffer) snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data), b.truncated
}

func runProcess(ctx context.Context, program string, args []string, spec CommandSpec) (RunResult, error) {
	if spec.Timeout <= 0 {
		spec.Timeout = 30 * time.Second
	}
	if spec.MaxOutputChars <= 0 {
		spec.MaxOutputChars = 24000
	}
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	cmd := exec.Command(program, args...)
	cmd.Dir = spec.Directory
	cmd.Env = spec.Environment
	cmd.WaitDelay = 2 * time.Second
	configureProcess(cmd)
	buffer := &cappedBuffer{limit: spec.MaxOutputChars}
	cmd.Stdout = buffer
	cmd.Stderr = buffer
	if err := cmd.Start(); err != nil {
		return RunResult{}, fmt.Errorf("启动 sandbox 进程: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var runErr error
	result := RunResult{}
	select {
	case runErr = <-done:
	case <-runCtx.Done():
		killProcessTree(cmd)
		select {
		case runErr = <-done:
		case <-time.After(2 * time.Second):
			runErr = runCtx.Err()
		}
		result.TimedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		result.Canceled = !result.TimedOut
	}
	result.Output, result.Truncated = buffer.snapshot()
	if result.Truncated {
		result.Output += "\n... 输出过长，已截断 ..."
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if runErr == nil || result.TimedOut || result.Canceled {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, runErr
}

type managedProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	done    chan struct{}
	cleanup func()

	watchOnce sync.Once
	closeOnce sync.Once
	mu        sync.Mutex
	waitErr   error
}

func startManagedProcess(ctx context.Context, prepared PreparedProcess, cleanup func()) (*managedProcess, error) {
	if prepared.Program == "" {
		if cleanup != nil {
			cleanup()
		}
		return nil, errors.New("启动 sandbox 长驻进程: program 不能为空")
	}
	select {
	case <-ctx.Done():
		if cleanup != nil {
			cleanup()
		}
		return nil, ctx.Err()
	default:
	}
	cmd := exec.Command(prepared.Program, prepared.Args...)
	cmd.Dir = prepared.Directory
	cmd.Env = prepared.Environment
	configureProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		if cleanup != nil {
			cleanup()
		}
		return nil, fmt.Errorf("启动 sandbox 长驻进程: %w", err)
	}
	return &managedProcess{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		done:    make(chan struct{}),
		cleanup: cleanup,
	}, nil
}

func (p *managedProcess) startWatcher() {
	p.watchOnce.Do(func() {
		go func() {
			err := p.cmd.Wait()
			p.mu.Lock()
			p.waitErr = err
			p.mu.Unlock()
			if p.cleanup != nil {
				p.cleanup()
			}
			close(p.done)
		}()
	})
}

func (p *managedProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *managedProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *managedProcess) Stderr() io.ReadCloser { return p.stderr }

func (p *managedProcess) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *managedProcess) Wait() error {
	if p == nil {
		return nil
	}
	p.startWatcher()
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func (p *managedProcess) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		_ = p.stdin.Close()
		p.startWatcher()
		select {
		case <-p.done:
			return
		default:
		}
		killProcessTree(p.cmd)
	})
	err := p.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return err
}
