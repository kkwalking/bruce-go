package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
		return RunResult{}, fmt.Errorf("start sandbox process: %w", err)
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
		result.Output += "\n... Output exceeded the limit and was truncated ..."
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

// closeOnceReader closes a pipe at most once, so a caller's own Close and the
// process watcher's cleanup cannot fight over the same descriptor.
type closeOnceReader struct {
	r    *os.File
	once sync.Once
}

func (c *closeOnceReader) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *closeOnceReader) Close() error {
	var err error
	c.once.Do(func() { err = c.r.Close() })
	return err
}

type managedProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	done    chan struct{}
	cleanup func()

	// The parent's end of the stdout/stderr pipes. The child holds the other
	// end, so these stay readable until the child exits. Closing them is what
	// releases a reader parked in Read.
	stdoutRead *os.File
	stderrRead *os.File

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
		return nil, errors.New("start long-running sandbox process: program must not be empty")
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

	// Cleanup for anything already created if a later step fails.
	var opened []io.Closer
	fail := func(err error) (*managedProcess, error) {
		for _, c := range opened {
			_ = c.Close()
		}
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(err)
	}
	opened = append(opened, stdin)

	// StdoutPipe/StderrPipe are deliberately not used: Cmd.Wait closes those
	// pipes once the child exits, and the watcher below is armed as soon as
	// StartProcess returns, so a Wait could close a pipe while the caller was
	// still reading it — "read |0: file already closed". Owning the pipes with
	// os.Pipe keeps Cmd.Wait out of their lifetime entirely.
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	opened = append(opened, stdoutRead, stdoutWrite)
	cmd.Stdout = stdoutWrite

	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	opened = append(opened, stderrRead, stderrWrite)
	cmd.Stderr = stderrWrite

	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("start long-running sandbox process: %w", err))
	}
	// The child holds its own copies now; the parent's write ends must be
	// closed or no reader would ever see EOF.
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	return &managedProcess{
		cmd:        cmd,
		stdin:      stdin,
		stdout:     &closeOnceReader{r: stdoutRead},
		stderr:     &closeOnceReader{r: stderrRead},
		stdoutRead: stdoutRead,
		stderrRead: stderrRead,
		done:       make(chan struct{}),
		cleanup:    cleanup,
	}, nil
}

func (p *managedProcess) startWatcher() {
	p.watchOnce.Do(func() {
		go func() {
			err := p.cmd.Wait()
			p.mu.Lock()
			p.waitErr = err
			p.mu.Unlock()
			// Wait does not touch the pipes any more (they are ours, not
			// StdoutPipe's), so release them here once the child is gone: a
			// reader parked in Read sees EOF, and the descriptors do not leak.
			// closeOnceReader makes this safe against the caller's own Close.
			if p.stdout != nil {
				_ = p.stdout.Close()
			}
			if p.stderr != nil {
				_ = p.stderr.Close()
			}
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
