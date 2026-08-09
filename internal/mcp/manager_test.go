package mcp

import (
	"context"
	"encoding/json"
	"errors"
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
	if status.Ready || !strings.Contains(status.Error, "MCP initialize failed") || !strings.Contains(status.Error, initializeErr.Error()) {
		t.Fatalf("status = %+v", status)
	}
}

func TestInitializeAndListStopsAfterInitializedNotificationError(t *testing.T) {
	notifyErr := errors.New("notification rejected")
	transport := &handshakeTransport{notifyErr: notifyErr}

	_, err := initializeAndList(context.Background(), transport)
	if !errors.Is(err, notifyErr) || !strings.Contains(err.Error(), "MCP notifications/initialized failed") {
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
	if !errors.Is(err, listErr) || !strings.Contains(err.Error(), "MCP tools/list failed") {
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
	if !strings.Contains(out, "required=workspace-write") {
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
