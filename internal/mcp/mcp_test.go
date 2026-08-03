package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bruce-go/internal/config"
	"bruce-go/internal/sandbox"
	"bruce-go/internal/tool"
)

func TestManagerEnableRegisterAndCallTool(t *testing.T) {
	settings := config.MCPSettings{Servers: map[string]config.MCPServerSetting{
		"demo": {Type: "stdio", Command: "fake"},
	}}
	manager := NewManager(settings, t.TempDir()).WithFactory(func(context.Context, string, config.MCPServerSetting, string) (Transport, error) {
		return &fakeTransport{}, nil
	})
	if err := manager.Enable(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if len(status) != 1 || !status[0].Ready || status[0].ToolCount != 1 {
		t.Fatalf("status = %+v", status)
	}
	registry := tool.EmptyRegistry(t.TempDir())
	RegisterTools(registry, manager)
	out := registry.Execute(context.Background(), "mcp_demo_echo", map[string]string{"text": "hello"})
	if out != "mcp:hello" {
		t.Fatalf("tool output = %q", out)
	}
	if err := manager.Disable("demo"); err != nil {
		t.Fatal(err)
	}
	if manager.Status()[0].Ready {
		t.Fatalf("server should be disabled: %+v", manager.Status())
	}
}

func TestManagerEnablePropagatesInitializeError(t *testing.T) {
	initializeErr := errors.New("unsupported protocol version")
	transport := &handshakeTransport{initializeErr: initializeErr}
	manager := NewManager(config.MCPSettings{Servers: map[string]config.MCPServerSetting{
		"demo": {Type: "stdio", Command: "fake"},
	}}, t.TempDir()).WithFactory(func(context.Context, string, config.MCPServerSetting, string) (Transport, error) {
		return transport, nil
	})

	err := manager.Enable(context.Background(), "demo")
	if !errors.Is(err, initializeErr) {
		t.Fatalf("enable error = %v", err)
	}
	if got := strings.Join(transport.calls, ","); got != "initialize" {
		t.Fatalf("calls = %q", got)
	}
	if got := transport.closed.Load(); got != 1 {
		t.Fatalf("close count = %d", got)
	}
	status := manager.Status()[0]
	if status.Ready || !strings.Contains(status.Error, "MCP initialize 失败") || !strings.Contains(status.Error, initializeErr.Error()) {
		t.Fatalf("status = %+v", status)
	}
}

func TestInitializeAndListStopsAfterInitializedNotificationError(t *testing.T) {
	notifyErr := errors.New("notification rejected")
	transport := &handshakeTransport{notifyErr: notifyErr}

	_, err := initializeAndList(context.Background(), transport)
	if !errors.Is(err, notifyErr) || !strings.Contains(err.Error(), "MCP notifications/initialized 失败") {
		t.Fatalf("initializeAndList error = %v", err)
	}
	if got := strings.Join(transport.calls, ","); got != "initialize,notifications/initialized" {
		t.Fatalf("calls = %q", got)
	}
}

func TestInitializeAndListCompletesHandshakeBeforeListingTools(t *testing.T) {
	transport := &handshakeTransport{}

	tools, err := initializeAndList(context.Background(), transport)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	if got := strings.Join(transport.calls, ","); got != "initialize,notifications/initialized,tools/list" {
		t.Fatalf("calls = %q", got)
	}
}

func TestInitializeAndListAddsToolsListErrorContext(t *testing.T) {
	listErr := errors.New("list unavailable")
	transport := &handshakeTransport{listErr: listErr}

	_, err := initializeAndList(context.Background(), transport)
	if !errors.Is(err, listErr) || !strings.Contains(err.Error(), "MCP tools/list 失败") {
		t.Fatalf("initializeAndList error = %v", err)
	}
}

func TestMCPToolAccessFiltersDefinitionsAndRejectsStaleCalls(t *testing.T) {
	workspace := t.TempDir()
	sandboxManager, err := sandbox.New(context.Background(), sandbox.Options{
		Workspace: workspace, HomeDir: t.TempDir(), Mode: sandbox.ModeWorkspaceWrite,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandboxManager.Close() })
	transport := &policyTransport{}
	settings := config.MCPSettings{Servers: map[string]config.MCPServerSetting{
		"filesystem": {
			Type:    "stdio",
			Command: "fake",
			ToolAccess: map[string]string{
				"read":  "read-only",
				"write": "workspace-write",
			},
		},
	}}
	manager := NewManager(settings, workspace).WithSandbox(sandboxManager).WithFactory(
		func(context.Context, string, config.MCPServerSetting, string) (Transport, error) {
			return transport, nil
		},
	)
	if err := manager.Enable(context.Background(), "filesystem"); err != nil {
		t.Fatal(err)
	}
	registry := tool.EmptyRegistry(workspace).WithSandbox(sandboxManager)
	RegisterTools(registry, manager)
	if got := len(registry.Definitions()); got != 2 {
		t.Fatalf("workspace-write definitions = %d", got)
	}
	if err := sandboxManager.SetMode(sandbox.ModeReadOnly); err != nil {
		t.Fatal(err)
	}
	defs := registry.Definitions()
	if len(defs) != 1 || defs[0].Name != "mcp_filesystem_read" {
		t.Fatalf("read-only definitions = %+v", defs)
	}
	out := registry.Execute(context.Background(), "mcp_filesystem_write", map[string]string{"path": "blocked"})
	if !strings.Contains(out, "需要=workspace-write") {
		t.Fatalf("write policy output = %q", out)
	}
	if got := transport.toolCalls.Load(); got != 0 {
		t.Fatalf("transport calls after blocked write = %d", got)
	}
	out = registry.Execute(context.Background(), "mcp_filesystem_read", nil)
	if !strings.Contains(out, "mcp:read") {
		t.Fatalf("read output = %q", out)
	}
	if got := transport.toolCalls.Load(); got != 1 {
		t.Fatalf("transport calls = %d", got)
	}
}

func TestMCPAnnotationsDoNotAuthorizeUnclassifiedTool(t *testing.T) {
	workspace := t.TempDir()
	sandboxManager, err := sandbox.New(context.Background(), sandbox.Options{
		Workspace: workspace, HomeDir: t.TempDir(), Mode: sandbox.ModeReadOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandboxManager.Close() })
	transport := &policyTransport{}
	settings := config.MCPSettings{Servers: map[string]config.MCPServerSetting{
		"demo": {
			Type:       "stdio",
			Command:    "fake",
			ToolAccess: map[string]string{"read": "read-only"},
		},
	}}
	manager := NewManager(settings, workspace).WithSandbox(sandboxManager).WithFactory(
		func(context.Context, string, config.MCPServerSetting, string) (Transport, error) {
			return transport, nil
		},
	)
	if err := manager.Enable(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	tools := manager.Tools()
	if len(tools) != 1 || tools[0].Tool.Name != "read" {
		t.Fatalf("authorized tools = %+v", tools)
	}
	if _, err := manager.CallTool(context.Background(), "demo", "hinted", nil); err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("hint-only tool error = %v", err)
	}
	status := manager.Status()[0]
	if status.BlockedToolCount != 2 || !strings.Contains(status.BlockedReason, "toolAccess") {
		t.Fatalf("blocked tools = %+v", status)
	}
	if got := transport.toolCalls.Load(); got != 0 {
		t.Fatalf("transport calls = %d", got)
	}
}

func TestHTTPMCPFollowsNetworkPolicyAndReconfigures(t *testing.T) {
	workspace := t.TempDir()
	sandboxManager, err := sandbox.New(context.Background(), sandbox.Options{
		Workspace: workspace, HomeDir: t.TempDir(), Mode: sandbox.ModeReadOnly, NetworkAccess: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandboxManager.Close() })
	var starts atomic.Int32
	settings := config.MCPSettings{Servers: map[string]config.MCPServerSetting{
		"remote": {
			Type:       "http",
			URL:        "https://example.test/mcp",
			ToolAccess: map[string]string{"read": "read-only"},
		},
	}}
	manager := NewManager(settings, workspace).WithSandbox(sandboxManager).WithFactory(
		func(context.Context, string, config.MCPServerSetting, string) (Transport, error) {
			starts.Add(1)
			return &policyTransport{}, nil
		},
	)
	if err := manager.Enable(context.Background(), "remote"); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 0 || !strings.Contains(manager.Status()[0].BlockedReason, "network") {
		t.Fatalf("network-off status = %+v, starts=%d", manager.Status(), starts.Load())
	}
	if err := manager.Reconfigure(context.Background(), func() error {
		sandboxManager.SetNetworkAccess(true)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || !manager.Status()[0].Ready {
		t.Fatalf("network-on status = %+v, starts=%d", manager.Status(), starts.Load())
	}
}

func TestReconfigureClosesOldTransportAndKeepsStricterModeOnRestartFailure(t *testing.T) {
	workspace := t.TempDir()
	sandboxManager, err := sandbox.New(context.Background(), sandbox.Options{
		Workspace: workspace, HomeDir: t.TempDir(), Mode: sandbox.ModeFullAccess,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandboxManager.Close() })

	first := &policyTransport{}
	var starts atomic.Int32
	manager := NewManager(config.MCPSettings{Servers: map[string]config.MCPServerSetting{
		"demo": {
			Type:       "stdio",
			Command:    "fake",
			ToolAccess: map[string]string{"read": "read-only"},
		},
	}}, workspace).WithSandbox(sandboxManager).WithFactory(
		func(context.Context, string, config.MCPServerSetting, string) (Transport, error) {
			if starts.Add(1) == 1 {
				return first, nil
			}
			return nil, errors.New("restart denied")
		},
	)
	if err := manager.Enable(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	before := manager.Status()[0].Generation
	if err := manager.Reconfigure(context.Background(), func() error {
		return sandboxManager.SetMode(sandbox.ModeReadOnly)
	}); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()[0]
	if sandboxManager.Mode() != sandbox.ModeReadOnly {
		t.Fatalf("sandbox mode = %s", sandboxManager.Mode())
	}
	if first.closed.Load() != 1 {
		t.Fatalf("old transport close count = %d", first.closed.Load())
	}
	if status.Ready || !strings.Contains(status.Error, "restart denied") || status.Generation <= before {
		t.Fatalf("failed restart status = %+v", status)
	}
	if starts.Load() != 2 {
		t.Fatalf("transport starts = %d", starts.Load())
	}
}

func TestReconfigureGatesNewCallsAndWaitsForActiveTransport(t *testing.T) {
	workspace := t.TempDir()
	sandboxManager, err := sandbox.New(context.Background(), sandbox.Options{
		Workspace: workspace, HomeDir: t.TempDir(), Mode: sandbox.ModeFullAccess,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandboxManager.Close() })

	blocking := newBlockingTransport()
	var starts atomic.Int32
	manager := NewManager(config.MCPSettings{Servers: map[string]config.MCPServerSetting{
		"demo": {
			Type:       "stdio",
			Command:    "fake",
			ToolAccess: map[string]string{"read": "read-only"},
		},
	}}, workspace).WithSandbox(sandboxManager).WithFactory(
		func(context.Context, string, config.MCPServerSetting, string) (Transport, error) {
			if starts.Add(1) == 1 {
				return blocking, nil
			}
			return &policyTransport{}, nil
		},
	)
	if err := manager.Enable(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}

	activeDone := make(chan error, 1)
	go func() {
		_, err := manager.CallTool(context.Background(), "demo", "read", nil)
		activeDone <- err
	}()
	select {
	case <-blocking.callStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("active MCP call did not start")
	}

	reconfigureDone := make(chan error, 1)
	go func() {
		reconfigureDone <- manager.Reconfigure(context.Background(), func() error {
			return sandboxManager.SetMode(sandbox.ModeReadOnly)
		})
	}()
	select {
	case <-blocking.closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("MCP transition did not start closing the old transport")
	}
	if _, err := manager.CallTool(context.Background(), "demo", "read", nil); err == nil || !strings.Contains(err.Error(), "transition") {
		t.Fatalf("new call during transition error = %v", err)
	}
	close(blocking.allowClose)
	select {
	case err := <-activeDone:
		if err != nil {
			t.Fatalf("active call error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active MCP call was not released by transport close")
	}
	select {
	case err := <-reconfigureDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MCP reconfigure did not finish")
	}
	if status := manager.Status()[0]; !status.Ready || status.Generation < 3 {
		t.Fatalf("reconfigured status = %+v", status)
	}
}

type policyTransport struct {
	toolCalls atomic.Int32
	closed    atomic.Int32
}

func (f *policyTransport) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	switch method {
	case "initialize":
		return json.RawMessage(`{}`), nil
	case "tools/list":
		return json.RawMessage(`{"tools":[
			{"name":"read","description":"read","inputSchema":{"type":"object"}},
			{"name":"write","description":"write","inputSchema":{"type":"object"}},
			{"name":"hinted","description":"hint only","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}}
		]}`), nil
	case "tools/call":
		f.toolCalls.Add(1)
		payload := params.(map[string]any)
		return json.RawMessage(`{"content":[{"type":"text","text":"mcp:` + payload["name"].(string) + `"}]}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func (*policyTransport) Notify(context.Context, string, any) error { return nil }

func (f *policyTransport) Close() error {
	f.closed.Add(1)
	return nil
}

func (*policyTransport) Logs() []string { return nil }

type blockingTransport struct {
	callStarted  chan struct{}
	closeStarted chan struct{}
	allowClose   chan struct{}
	releaseCall  chan struct{}
	callOnce     sync.Once
	closeOnce    sync.Once
}

func newBlockingTransport() *blockingTransport {
	return &blockingTransport{
		callStarted:  make(chan struct{}),
		closeStarted: make(chan struct{}),
		allowClose:   make(chan struct{}),
		releaseCall:  make(chan struct{}),
	}
}

func (t *blockingTransport) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	switch method {
	case "initialize":
		return json.RawMessage(`{}`), nil
	case "tools/list":
		return json.RawMessage(`{"tools":[{"name":"read","description":"read","inputSchema":{"type":"object"}}]}`), nil
	case "tools/call":
		t.callOnce.Do(func() { close(t.callStarted) })
		<-t.releaseCall
		return json.RawMessage(`{"content":[{"type":"text","text":"released"}]}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func (*blockingTransport) Notify(context.Context, string, any) error { return nil }

func (t *blockingTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closeStarted)
		<-t.allowClose
		close(t.releaseCall)
	})
	return nil
}

func (*blockingTransport) Logs() []string { return nil }

func TestHTTPTransportCallsJSONRPC(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Method != "ping" {
			http.Error(w, "unexpected method: "+req.Method, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"ok":true}`)})
	}))
	defer server.Close()

	transport := NewHTTPTransport(config.MCPServerSetting{URL: server.URL})
	raw, err := transport.Call(context.Background(), "ping", map[string]string{"x": "y"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("raw = %s", raw)
	}
}

func TestHTTPTransportSendsNotificationWithoutID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Header") != "notification" {
			http.Error(w, "missing configured header", http.StatusBadRequest)
			return
		}
		var message map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, ok := message["id"]; ok {
			http.Error(w, "notification must not contain id", http.StatusBadRequest)
			return
		}
		var method string
		if err := json.Unmarshal(message["method"], &method); err != nil || method != "notifications/initialized" {
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	transport := NewHTTPTransport(config.MCPServerSetting{
		URL:     server.URL,
		Headers: map[string]string{"X-Test-Header": "notification"},
	})
	if err := transport.Notify(context.Background(), "notifications/initialized", map[string]any{}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPTransportNotificationReturnsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rejected", http.StatusBadRequest)
	}))
	defer server.Close()

	transport := NewHTTPTransport(config.MCPServerSetting{URL: server.URL})
	err := transport.Notify(context.Background(), "notifications/initialized", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "MCP HTTP 400") {
		t.Fatalf("notify error = %v", err)
	}
}

func TestStdioTransportSendsNotificationWithoutPendingResponse(t *testing.T) {
	transport, requests, _ := newPipeStdioTransport(t)
	done := make(chan error, 1)
	go func() {
		done <- transport.Notify(context.Background(), "notifications/initialized", map[string]any{})
	}()

	if !requests.Scan() {
		t.Fatal("missing stdio notification")
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(requests.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	if _, ok := message["id"]; ok {
		t.Fatalf("notification contains id: %s", requests.Bytes())
	}
	var method string
	if err := json.Unmarshal(message["method"], &method); err != nil || method != "notifications/initialized" {
		t.Fatalf("notification method = %q, err = %v", method, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("notification waited for a response")
	}
	transport.stateMu.Lock()
	pending := len(transport.pending)
	transport.stateMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending responses = %d", pending)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := transport.Notify(ctx, "notifications/initialized", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled notification error = %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	if err := transport.Notify(context.Background(), "notifications/initialized", nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("closed notification error = %v", err)
	}
}

func TestStdioTransportDispatchesConcurrentResponsesByID(t *testing.T) {
	transport, requests, responses := newPipeStdioTransport(t)
	type callResult struct {
		method string
		raw    json.RawMessage
		err    error
	}
	results := make(chan callResult, 2)
	for _, method := range []string{"first", "second"} {
		method := method
		go func() {
			raw, err := transport.Call(context.Background(), method, nil)
			results <- callResult{method: method, raw: raw, err: err}
		}()
	}
	requestByMethod := map[string]rpcRequest{}
	for range 2 {
		if !requests.Scan() {
			t.Fatal("missing stdio request")
		}
		var request rpcRequest
		if err := json.Unmarshal(requests.Bytes(), &request); err != nil {
			t.Fatal(err)
		}
		requestByMethod[request.Method] = request
	}
	for _, method := range []string{"second", "first"} {
		request := requestByMethod[method]
		response, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{"method":"` + method + `"}`)})
		if _, err := responses.Write(append(response, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		result := <-results
		if result.err != nil || !strings.Contains(string(result.raw), result.method) {
			t.Fatalf("result = %+v", result)
		}
	}
}

func TestStdioTransportIgnoresLateCanceledResponse(t *testing.T) {
	transport, requests, responses := newPipeStdioTransport(t)
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := transport.Call(ctx, "first", nil)
		firstDone <- err
	}()
	if !requests.Scan() {
		t.Fatal("missing first request")
	}
	var first rpcRequest
	if err := json.Unmarshal(requests.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first call error = %v", err)
	}
	late, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: first.ID, Result: json.RawMessage(`{"late":true}`)})
	if _, err := responses.Write(append(late, '\n')); err != nil {
		t.Fatal(err)
	}

	secondDone := make(chan struct {
		raw json.RawMessage
		err error
	}, 1)
	go func() {
		raw, err := transport.Call(context.Background(), "second", nil)
		secondDone <- struct {
			raw json.RawMessage
			err error
		}{raw: raw, err: err}
	}()
	if !requests.Scan() {
		t.Fatal("missing second request")
	}
	var second rpcRequest
	if err := json.Unmarshal(requests.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	response, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: second.ID, Result: json.RawMessage(`{"ok":true}`)})
	if _, err := responses.Write(append(response, '\n')); err != nil {
		t.Fatal(err)
	}
	result := <-secondDone
	if result.err != nil || !strings.Contains(string(result.raw), `"ok":true`) {
		t.Fatalf("second result = %+v", result)
	}
}

func TestStdioTransportCloseReleasesPendingCalls(t *testing.T) {
	transport, requests, _ := newPipeStdioTransport(t)
	done := make(chan error, 1)
	go func() {
		_, err := transport.Call(context.Background(), "blocked", nil)
		done <- err
	}()
	if !requests.Scan() {
		t.Fatal("missing request")
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("pending call error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending call was not released")
	}
}

func newPipeStdioTransport(t *testing.T) (*StdioTransport, *bufio.Scanner, io.Writer) {
	t.Helper()
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	responseScanner := bufio.NewScanner(responseReader)
	transport := &StdioTransport{
		stdin:   requestWriter,
		scanner: responseScanner,
		logs:    NewLogRingBuffer(32),
		pending: map[int64]chan stdioResult{},
	}
	go transport.readLoop()
	t.Cleanup(func() {
		_ = transport.Close()
		_ = requestReader.Close()
		_ = requestWriter.Close()
		_ = responseWriter.Close()
		_ = responseReader.Close()
	})
	return transport, bufio.NewScanner(requestReader), responseWriter
}

type fakeTransport struct{}

func (f *fakeTransport) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	switch method {
	case "initialize":
		return json.RawMessage(`{}`), nil
	case "tools/list":
		return json.RawMessage(`{"tools":[{"name":"echo","description":"echo text","inputSchema":{"type":"object","properties":{"text":{"type":"string"}}}}]}`), nil
	case "tools/call":
		payload := params.(map[string]any)
		args := payload["arguments"].(map[string]string)
		return json.RawMessage(`{"content":[{"type":"text","text":"mcp:` + args["text"] + `"}]}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func (*fakeTransport) Notify(context.Context, string, any) error { return nil }

func (f *fakeTransport) Close() error   { return nil }
func (f *fakeTransport) Logs() []string { return []string{"fake log"} }

type handshakeTransport struct {
	calls         []string
	initializeErr error
	notifyErr     error
	listErr       error
	closed        atomic.Int32
}

func (t *handshakeTransport) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	t.calls = append(t.calls, method)
	switch method {
	case "initialize":
		return json.RawMessage(`{}`), t.initializeErr
	case "tools/list":
		if t.listErr != nil {
			return nil, t.listErr
		}
		return json.RawMessage(`{"tools":[{"name":"echo","description":"echo","inputSchema":{"type":"object"}}]}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func (t *handshakeTransport) Notify(_ context.Context, method string, _ any) error {
	t.calls = append(t.calls, method)
	return t.notifyErr
}

func (t *handshakeTransport) Close() error {
	t.closed.Add(1)
	return nil
}

func (*handshakeTransport) Logs() []string { return nil }

func TestSanitizeSchemaEnsuresObject(t *testing.T) {
	// Non-object schema should be wrapped
	raw := json.RawMessage(`{"type":"string"}`)
	out := SanitizeSchema(raw)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "object" {
		t.Fatalf("expected type=object, got %v", m["type"])
	}
	props, _ := m["properties"].(map[string]any)
	if props == nil || props["value"] == nil {
		t.Fatalf("expected properties.value, got %v", m["properties"])
	}
}

func TestSanitizeSchemaRemovesExtensionKeys(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","$schema":"http://x","$ref":"#/defs/Foo","properties":{"x":{"type":"string"}}}`)
	out := SanitizeSchema(raw)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["$schema"]; ok {
		t.Fatal("expected $schema to be removed")
	}
	if _, ok := m["$ref"]; ok {
		t.Fatal("expected $ref to be removed")
	}
}

func TestSanitizeSchemaFoldsUnion(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","anyOf":[{"type":"string","description":"a string"},{"type":"number"}],"description":"check"}`)
	out := SanitizeSchema(raw)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["anyOf"]; ok {
		t.Fatal("expected anyOf to be removed")
	}
	desc, _ := m["description"].(string)
	if !strings.Contains(desc, "anyOf options") {
		t.Fatalf("expected description to contain union options, got %q", desc)
	}
}

func TestSanitizeSchemaEnsuresRequired(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`)
	out := SanitizeSchema(raw)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	req, _ := m["required"].([]any)
	if req == nil {
		t.Fatal("expected required to be an array")
	}
}

func TestExpandMCPVarsReplacesKnownVariables(t *testing.T) {
	cases := []struct {
		input     string
		workspace string
		home      string
		want      string
	}{
		{"/some/path", "/ws", "/home/user", "/some/path"},
		{"${PROJECT_DIR}/src", "/ws", "/home/user", "/ws/src"},
		{"${HOME}/.bruce", "/ws", "/home/user", "/home/user/.bruce"},
		{"${PROJECT_DIR},${HOME}", "/ws", "/home", "/ws,/home"},
		{"${UNKNOWN}", "/ws", "/home", "${UNKNOWN}"},
		{"", "/ws", "/home", ""},
		{"npx -y @scope/pkg ${PROJECT_DIR}", "/app", "/h", "npx -y @scope/pkg /app"},
	}
	for _, tc := range cases {
		got := expandMCPVars(tc.input, tc.workspace, tc.home)
		if got != tc.want {
			t.Errorf("expandMCPVars(%q, %q, %q) = %q, want %q", tc.input, tc.workspace, tc.home, got, tc.want)
		}
	}
}
