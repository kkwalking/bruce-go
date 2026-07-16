package sandbox

import (
	"context"
	"errors"
	"fmt"
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
