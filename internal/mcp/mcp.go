package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bruce-go/internal/config"
	"bruce-go/internal/tool"
)

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type ServerStatus struct {
	Name      string
	Enabled   bool
	Ready     bool
	ToolCount int
	Error     string
}

type Transport interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	Close() error
	Logs() []string
}

type Manager struct {
	mu        sync.RWMutex
	servers   map[string]*Server
	workspace string
	factory   TransportFactory
}

type TransportFactory func(ctx context.Context, name string, cfg config.MCPServerSetting, workspace string) (Transport, error)

type Server struct {
	Name      string
	Config    config.MCPServerSetting
	Enabled   bool
	Ready     bool
	Tools     []Tool
	LastError string
	transport Transport
}

func NewManager(settings config.MCPSettings, workspace string) *Manager {
	m := &Manager{servers: map[string]*Server{}, workspace: workspace, factory: DefaultTransportFactory}
	for name, cfg := range settings.Servers {
		server := &Server{Name: name, Config: cfg, Enabled: !cfg.Disabled}
		m.servers[name] = server
	}
	return m
}

func (m *Manager) WithFactory(factory TransportFactory) *Manager {
	m.mu.Lock()
	defer m.mu.Unlock()
	if factory != nil {
		m.factory = factory
	}
	return m
}

func (m *Manager) StartEnabled(ctx context.Context) {
	names := m.Names()
	for _, name := range names {
		m.mu.RLock()
		enabled := m.servers[name] != nil && m.servers[name].Enabled
		m.mu.RUnlock()
		if !enabled {
			continue
		}
		_ = m.Enable(ctx, name)
	}
}

func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.servers))
	for name := range m.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *Manager) Status() []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	statuses := make([]ServerStatus, 0, len(m.servers))
	for _, server := range m.servers {
		statuses = append(statuses, ServerStatus{Name: server.Name, Enabled: server.Enabled, Ready: server.Ready, ToolCount: len(server.Tools), Error: server.LastError})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses
}

func (m *Manager) Enable(ctx context.Context, name string) error {
	m.mu.Lock()
	server := m.servers[name]
	if server == nil {
		m.mu.Unlock()
		return errors.New("未知 MCP server: " + name)
	}
	server.Enabled = true
	if server.Ready {
		m.mu.Unlock()
		return nil
	}
	cfg := server.Config
	factory := m.factory
	workspace := m.workspace
	m.mu.Unlock()

	transport, err := factory(ctx, name, cfg, workspace)
	if err != nil {
		m.recordError(name, err)
		return err
	}
	tools, err := initializeAndList(ctx, transport)
	if err != nil {
		_ = transport.Close()
		m.recordError(name, err)
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	server = m.servers[name]
	if server == nil {
		_ = transport.Close()
		return errors.New("未知 MCP server: " + name)
	}
	if server.transport != nil {
		_ = server.transport.Close()
	}
	server.transport = transport
	server.Tools = tools
	server.Ready = true
	server.Enabled = true
	server.LastError = ""
	return nil
}

func (m *Manager) Disable(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	server := m.servers[name]
	if server == nil {
		return errors.New("未知 MCP server: " + name)
	}
	if server.transport != nil {
		_ = server.transport.Close()
	}
	server.transport = nil
	server.Enabled = false
	server.Ready = false
	server.Tools = nil
	return nil
}

func (m *Manager) Restart(ctx context.Context, name string) error {
	if err := m.Disable(name); err != nil {
		return err
	}
	return m.Enable(ctx, name)
}

func (m *Manager) Logs(name string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	server := m.servers[name]
	if server == nil || server.transport == nil {
		return nil
	}
	return server.transport.Logs()
}

func (m *Manager) Tools() []RegisteredTool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []RegisteredTool
	for _, server := range m.servers {
		if !server.Enabled || !server.Ready {
			continue
		}
		for _, t := range server.Tools {
			out = append(out, RegisteredTool{Server: server.Name, Tool: t})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server == out[j].Server {
			return out[i].Tool.Name < out[j].Tool.Name
		}
		return out[i].Server < out[j].Server
	})
	return out
}

type RegisteredTool struct {
	Server string
	Tool   Tool
}

func (m *Manager) CallTool(ctx context.Context, serverName, toolName string, args map[string]string) (string, error) {
	m.mu.RLock()
	server := m.servers[serverName]
	if server == nil || !server.Enabled || !server.Ready || server.transport == nil {
		m.mu.RUnlock()
		return "", errors.New("MCP server 未就绪: " + serverName)
	}
	transport := server.transport
	m.mu.RUnlock()

	raw, err := transport.Call(ctx, "tools/call", map[string]any{"name": toolName, "arguments": args})
	if err != nil {
		return "", err
	}
	return formatToolResult(raw), nil
}

func RegisterTools(registry *tool.Registry, manager *Manager) {
	for _, item := range manager.Tools() {
		serverName, remoteName := item.Server, item.Tool.Name
		localName := "mcp_" + sanitize(serverName) + "_" + sanitize(remoteName)
		schema := SanitizeSchema(item.Tool.InputSchema)
		if len(schema) == 0 {
			schema = []byte(`{"type":"object","properties":{}}`)
		}
		registry.Register(tool.Tool{
			Name:        localName,
			Description: "[MCP " + serverName + "] " + item.Tool.Description,
			Parameters:  schema,
			Exec: func(ctx context.Context, args map[string]string) (string, error) {
				return manager.CallTool(ctx, serverName, remoteName, args)
			},
			PromptSnippet: "Call MCP tool " + serverName + "/" + remoteName,
		})
	}
}

func DefaultTransportFactory(ctx context.Context, _ string, cfg config.MCPServerSetting, workspace string) (Transport, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "", "stdio":
		return NewStdioTransport(ctx, cfg, workspace)
	case "http", "streamable_http", "streamable-http", "streamablehttp":
		return NewHTTPTransport(cfg), nil
	default:
		return nil, errors.New("不支持的 MCP transport: " + cfg.Type)
	}
}

func initializeAndList(ctx context.Context, transport Transport) ([]Tool, error) {
	_, _ = transport.Call(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]string{"name": "bruce-go", "version": "0.1.0"},
		"capabilities":    map[string]any{},
	})
	raw, err := transport.Call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload.Tools, nil
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type HTTPTransport struct {
	url     string
	headers map[string]string
	client  *http.Client
	nextID  atomic.Int64
}

func NewHTTPTransport(cfg config.MCPServerSetting) *HTTPTransport {
	return &HTTPTransport{
		url:     cfg.URL,
		headers: cfg.Headers,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (t *HTTPTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if strings.TrimSpace(t.url) == "" {
		return nil, errors.New("MCP HTTP URL 不能为空")
	}
	reqBody, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: t.nextID.Add(1), Method: method, Params: params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("MCP HTTP %d", resp.StatusCode)
	}
	return decodeRPCResponse(resp.Body)
}

func (*HTTPTransport) Close() error   { return nil }
func (*HTTPTransport) Logs() []string { return nil }

type StdioTransport struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	logs    *LogRingBuffer
	nextID  int64
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
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("MCP stdio command 不能为空")
	}
	home, _ := os.UserHomeDir()
	command := expandMCPVars(cfg.Command, workspace, home)
	args := make([]string, len(cfg.Args))
	for i, arg := range cfg.Args {
		args[i] = expandMCPVars(arg, workspace, home)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workspace
	cmd.Env = os.Environ()
	for key, value := range cfg.Env {
		cmd.Env = append(cmd.Env, key+"="+expandMCPVars(value, workspace, home))
	}
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
	logs := NewLogRingBuffer(200)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go readLines(stderr, logs)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &StdioTransport{cmd: cmd, stdin: stdin, scanner: scanner, logs: logs}, nil
}

func (t *StdioTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := atomic.AddInt64(&t.nextID, 1)
	req, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if _, err := t.stdin.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	type result struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
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
			if resp.ID != id {
				continue
			}
			if resp.Error != nil {
				ch <- result{err: errors.New(resp.Error.Message)}
			} else {
				ch <- result{raw: resp.Result}
			}
			return
		}
		if err := t.scanner.Err(); err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{err: io.EOF}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-ch:
		return out.raw, out.err
	}
}

func (t *StdioTransport) Close() error {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return nil
	}
	_ = t.stdin.Close()
	err := t.cmd.Process.Kill()
	_ = t.cmd.Wait()
	return err
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
		limit = 100
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

func decodeRPCResponse(r io.Reader) (json.RawMessage, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if bytes.HasPrefix(data, []byte("data:")) {
		lines := bytes.Split(data, []byte{'\n'})
		for _, line := range lines {
			line = bytes.TrimSpace(line)
			if bytes.HasPrefix(line, []byte("data:")) {
				data = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
				break
			}
		}
	}
	var resp rpcResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, errors.New(resp.Error.Message)
	}
	return resp.Result, nil
}

const schemaDescriptionLimit = 1000

// SanitizeSchema cleans a JSON Schema for safe tool registration.
func SanitizeSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{"type":"object","properties":{},"required":[]}`)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return json.RawMessage(`{"type":"object","properties":{},"required":[]}`)
	}
	if schema == nil {
		schema = map[string]any{}
	}
	ensureObjectSchema(schema)
	sanitizeObject(schema)
	if _, ok := schema["properties"]; !ok {
		schema["properties"] = map[string]any{}
	}
	if _, ok := schema["required"]; !ok {
		schema["required"] = []any{}
	}
	out, _ := json.Marshal(schema)
	return out
}

func ensureObjectSchema(schema map[string]any) {
	tp, _ := schema["type"].(string)
	if tp == "" {
		schema["type"] = "object"
		return
	}
	if tp != "object" {
		// Deep-copy the original content to avoid reference cycles
		inner := deepCopyMap(schema)
		for k := range schema {
			delete(schema, k)
		}
		schema["type"] = "object"
		schema["properties"] = map[string]any{"value": inner}
	}
}

func deepCopyMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		switch val := v.(type) {
		case map[string]any:
			dst[k] = deepCopyMap(val)
		case []any:
			arr := make([]any, len(val))
			for i, item := range val {
				if m, ok := item.(map[string]any); ok {
					arr[i] = deepCopyMap(m)
				} else {
					arr[i] = item
				}
			}
			dst[k] = arr
		default:
			dst[k] = val
		}
	}
	return dst
}

func sanitizeObject(schema map[string]any) {
	delete(schema, "$schema")
	delete(schema, "$id")
	delete(schema, "$defs")
	delete(schema, "definitions")
	delete(schema, "$ref")
	foldUnion(schema, "anyOf")
	foldUnion(schema, "oneOf")
	truncateSchemaDescription(schema)

	for _, v := range schema {
		switch val := v.(type) {
		case map[string]any:
			sanitizeObject(val)
		case []any:
			for _, item := range val {
				if obj, ok := item.(map[string]any); ok {
					sanitizeObject(obj)
				}
			}
		}
	}
}

func foldUnion(schema map[string]any, field string) {
	union, ok := schema[field].([]any)
	if !ok || len(union) == 0 {
		return
	}
	var desc strings.Builder
	if existing, _ := schema["description"].(string); existing != "" {
		desc.WriteString(existing)
			desc.WriteByte('\n')
	}
	desc.WriteString(field + " options: ")
	for _, option := range union {
		opt, _ := option.(map[string]any)
		if tp, _ := opt["type"].(string); tp != "" {
			desc.WriteString(tp)
		}
		if od, _ := opt["description"].(string); od != "" {
			desc.WriteString("(" + od + ")")
		}
		desc.WriteString("; ")
	}
	schema["description"] = desc.String()
	delete(schema, field)
}

func truncateSchemaDescription(schema map[string]any) {
	d, ok := schema["description"].(string)
	if !ok || len(d) <= schemaDescriptionLimit {
		return
	}
	schema["description"] = d[:schemaDescriptionLimit] + "..."
}

func formatToolResult(raw json.RawMessage) string {
	var payload struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && len(payload.Content) > 0 {
		var parts []string
		for _, item := range payload.Content {
			if item.Text != "" {
				parts = append(parts, item.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err == nil {
		pretty, _ := json.MarshalIndent(generic, "", "  ")
		return string(pretty)
	}
	return string(raw)
}

func readLines(r io.Reader, logs *LogRingBuffer) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		logs.Append(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		logs.Append("stderr read error: " + err.Error())
	}
}

func (m *Manager) recordError(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if server := m.servers[name]; server != nil {
		server.LastError = err.Error()
		server.Ready = false
	}
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_]+`)

func sanitize(value string) string {
	out := unsafeName.ReplaceAllString(value, "_")
	out = strings.Trim(out, "_")
	if out == "" {
		return "tool"
	}
	return strings.ToLower(out)
}
