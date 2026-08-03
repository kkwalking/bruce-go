package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"bruce-go/internal/config"
	"bruce-go/internal/sandbox"
)

const (
	stdioLogLimit              = 200
	defaultLogRingBufferLimit  = 100
	stdioInitialScanBufferSize = 64 * 1024
	stdioMaxMessageSize        = 4 * 1024 * 1024
	stdioMaxLogLineSize        = 1024 * 1024
)

type StdioTransport struct {
	writeMu   sync.Mutex
	stateMu   sync.Mutex
	closeOnce sync.Once
	cmd       *exec.Cmd
	process   sandbox.LongRunningProcess
	stdin     io.WriteCloser
	scanner   *bufio.Scanner
	logs      *LogRingBuffer
	nextID    atomic.Int64
	pending   map[int64]chan stdioResult
	closed    bool
	closeErr  error
}

var _ Transport = (*StdioTransport)(nil)

type stdioResult struct {
	raw json.RawMessage
	err error
}

var mcpVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_.]*)\}`)

func expandMCPVars(s string, workspace, home string) string {
	return mcpVarRe.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1]
		switch name {
		case "PROJECT_DIR":
			return workspace
		case "HOME":
			return home
		default:
			return match
		}
	})
}

func NewStdioTransport(ctx context.Context, cfg config.MCPServerSetting, workspace string) (*StdioTransport, error) {
	return newStdioTransport(ctx, cfg, workspace, nil)
}

func newStdioTransport(ctx context.Context, cfg config.MCPServerSetting, workspace string, launcher *sandbox.Manager) (*StdioTransport, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("MCP stdio command 不能为空")
	}
	home, _ := os.UserHomeDir()
	command := expandMCPVars(cfg.Command, workspace, home)
	args := make([]string, len(cfg.Args))
	for i, arg := range cfg.Args {
		args[i] = expandMCPVars(arg, workspace, home)
	}
	extraEnv := make([]string, 0, len(cfg.Env))
	for key, value := range cfg.Env {
		extraEnv = append(extraEnv, key+"="+expandMCPVars(value, workspace, home))
	}
	sort.Strings(extraEnv)
	if launcher != nil {
		process, err := launcher.StartProcess(ctx, sandbox.ProcessSpec{
			Program:     command,
			Args:        args,
			Directory:   workspace,
			Environment: extraEnv,
		}, nil)
		if err != nil {
			return nil, err
		}
		logs := NewLogRingBuffer(stdioLogLimit)
		go readLines(process.Stderr(), logs)
		scanner := bufio.NewScanner(process.Stdout())
		scanner.Buffer(make([]byte, 0, stdioInitialScanBufferSize), stdioMaxMessageSize)
		transport := &StdioTransport{
			process: process,
			stdin:   process.Stdin(),
			scanner: scanner,
			logs:    logs,
			pending: map[int64]chan stdioResult{},
		}
		go transport.readLoop()
		return transport, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	cmd := exec.Command(command, args...)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), extraEnv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	logs := NewLogRingBuffer(stdioLogLimit)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go readLines(stderr, logs)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, stdioInitialScanBufferSize), stdioMaxMessageSize)
	transport := &StdioTransport{cmd: cmd, stdin: stdin, scanner: scanner, logs: logs, pending: map[int64]chan stdioResult{}}
	go transport.readLoop()
	return transport, nil
}

func (t *StdioTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := t.nextID.Add(1)
	req, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	ch := make(chan stdioResult, 1)
	t.stateMu.Lock()
	if t.closed {
		t.stateMu.Unlock()
		return nil, io.ErrClosedPipe
	}
	t.pending[id] = ch
	t.stateMu.Unlock()

	t.writeMu.Lock()
	t.stateMu.Lock()
	closed := t.closed
	t.stateMu.Unlock()
	if closed {
		t.writeMu.Unlock()
		t.removePending(id)
		return nil, io.ErrClosedPipe
	}
	_, err = t.stdin.Write(append(req, '\n'))
	t.writeMu.Unlock()
	if err != nil {
		t.removePending(id)
		return nil, err
	}
	select {
	case <-ctx.Done():
		t.removePending(id)
		return nil, ctx.Err()
	case out := <-ch:
		return out.raw, out.err
	}
}

func (t *StdioTransport) Notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	notification, err := json.Marshal(rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.stateMu.Lock()
	closed := t.closed
	t.stateMu.Unlock()
	if closed {
		return io.ErrClosedPipe
	}
	_, err = t.stdin.Write(append(notification, '\n'))
	return err
}

func (t *StdioTransport) readLoop() {
	for t.scanner.Scan() {
		line := bytes.TrimSpace(t.scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			t.logs.Append("stdout parse error: " + err.Error())
			continue
		}
		result := stdioResult{raw: resp.Result}
		if resp.Error != nil {
			result = stdioResult{err: errors.New(resp.Error.Message)}
		}
		t.stateMu.Lock()
		ch := t.pending[resp.ID]
		delete(t.pending, resp.ID)
		t.stateMu.Unlock()
		if ch == nil {
			t.logs.Append(fmt.Sprintf("unmatched response id: %d", resp.ID))
			continue
		}
		ch <- result
	}
	err := t.scanner.Err()
	if err == nil {
		err = io.EOF
	}
	t.failPending(err)
}

func (t *StdioTransport) removePending(id int64) {
	t.stateMu.Lock()
	delete(t.pending, id)
	t.stateMu.Unlock()
}

func (t *StdioTransport) failPending(err error) {
	t.stateMu.Lock()
	if !t.closed {
		t.closed = true
	}
	pending := t.pending
	t.pending = map[int64]chan stdioResult{}
	t.stateMu.Unlock()
	for _, ch := range pending {
		ch <- stdioResult{err: err}
	}
}

func (t *StdioTransport) Close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		t.failPending(io.ErrClosedPipe)
		t.writeMu.Lock()
		defer t.writeMu.Unlock()
		if t.process != nil {
			t.closeErr = t.process.Close()
			return
		}
		if t.cmd == nil || t.cmd.Process == nil {
			return
		}
		_ = t.stdin.Close()
		t.closeErr = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
	})
	return t.closeErr
}

func (t *StdioTransport) Logs() []string {
	if t == nil || t.logs == nil {
		return nil
	}
	return t.logs.Lines()
}

type LogRingBuffer struct {
	mu    sync.Mutex
	limit int
	lines []string
}

func NewLogRingBuffer(limit int) *LogRingBuffer {
	if limit <= 0 {
		limit = defaultLogRingBufferLimit
	}
	return &LogRingBuffer{limit: limit}
}

func (b *LogRingBuffer) Append(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	if len(b.lines) > b.limit {
		b.lines = append([]string(nil), b.lines[len(b.lines)-b.limit:]...)
	}
}

func (b *LogRingBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lines...)
}

func readLines(r io.Reader, logs *LogRingBuffer) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, stdioInitialScanBufferSize), stdioMaxLogLineSize)
	for scanner.Scan() {
		logs.Append(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		logs.Append("stderr read error: " + err.Error())
	}
}
