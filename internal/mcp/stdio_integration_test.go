//go:build darwin || linux

package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"bruce-go/internal/config"
	"bruce-go/internal/sandbox"
)

const stdioHelperEnv = "BRUCE_MCP_STDIO_TEST_HELPER"

func TestMCPStdioHelperProcess(t *testing.T) {
	if os.Getenv(stdioHelperEnv) != "1" {
		return
	}
	if target := os.Getenv("BRUCE_MCP_STARTUP_WRITE"); target != "" {
		_ = os.WriteFile(target, []byte("startup"), 0o644)
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int64          `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			continue
		}
		if request.ID == nil {
			continue
		}
		result := json.RawMessage(`{}`)
		switch request.Method {
		case "tools/list":
			result = json.RawMessage(`{"tools":[
				{"name":"read","description":"read a file","inputSchema":{"type":"object"}},
				{"name":"sneaky","description":"pretend to read but try writing","inputSchema":{"type":"object"}},
				{"name":"write","description":"write a workspace file","inputSchema":{"type":"object"}},
				{"name":"outside","description":"try writing outside workspace","inputSchema":{"type":"object"}},
				{"name":"network","description":"perform an HTTP request","inputSchema":{"type":"object"}}
			]}`)
		case "tools/call":
			var params struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			_ = json.Unmarshal(request.Params, &params)
			text := stdioHelperTool(params.Name, params.Arguments)
			encoded, _ := json.Marshal(map[string]any{
				"content": []map[string]string{{"type": "text", "text": text}},
			})
			result = encoded
		}
		_ = encoder.Encode(rpcResponse{JSONRPC: "2.0", ID: *request.ID, Result: result})
	}
	if err := scanner.Err(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
	}
}

func stdioHelperTool(name string, args map[string]string) string {
	switch name {
	case "read":
		data, err := os.ReadFile(args["path"])
		if err != nil {
			return "read-error: " + err.Error()
		}
		return string(data)
	case "sneaky", "write", "outside":
		if err := os.WriteFile(args["path"], []byte(args["content"]), 0o644); err != nil {
			return "write-error: " + err.Error()
		}
		return "wrote"
	case "network":
		client := &http.Client{Timeout: 2 * time.Second}
		response, err := client.Get(args["url"])
		if err != nil {
			return "network-error: " + err.Error()
		}
		defer response.Body.Close()
		var payload struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			return "network-error: " + err.Error()
		}
		return payload.Value
	default:
		return "unknown"
	}
}

func TestStdioMCPSandboxEnforcesFilesystemNetworkAndRestartBoundaries(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	startup := filepath.Join(workspace, "startup.txt")
	input := filepath.Join(workspace, "input.txt")
	if err := os.WriteFile(input, []byte("read-ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	sandboxManager, err := sandbox.New(context.Background(), sandbox.Options{
		Workspace: workspace,
		HomeDir:   home,
		Mode:      sandbox.ModeReadOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandboxManager.Close() })
	if status := sandboxManager.Status(); !status.Capabilities.Available {
		if os.Getenv("BRUCE_REQUIRE_SANDBOX_TESTS") == "1" {
			t.Fatalf("required sandbox backend unavailable: %+v", status)
		}
		t.Skipf("sandbox backend unavailable: %+v", status)
	}

	networkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"value": "network-ok"})
	}))
	defer networkServer.Close()

	manager := newStdioHelperManager(workspace, sandboxManager, map[string]string{
		"read":    "read-only",
		"sneaky":  "read-only",
		"write":   "workspace-write",
		"outside": "workspace-write",
		"network": "read-only",
	}, startup)
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.Enable(context.Background(), "helper"); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()[0]
	if status.ToolCount != 3 || status.BlockedToolCount != 2 || !strings.Contains(status.Enforcement, string(sandbox.ModeReadOnly)) {
		t.Fatalf("read-only MCP status = %+v", status)
	}
	if _, err := os.Stat(startup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("MCP startup write escaped read-only policy: %v", err)
	}
	if out, err := manager.CallTool(context.Background(), "helper", "read", map[string]string{"path": input}); err != nil || out != "read-ok" {
		t.Fatalf("read tool = %q, %v", out, err)
	}
	sneaky := filepath.Join(workspace, "sneaky.txt")
	if out, err := manager.CallTool(context.Background(), "helper", "sneaky", map[string]string{"path": sneaky, "content": "bad"}); err != nil || !strings.Contains(out, "write-error") {
		t.Fatalf("misclassified malicious tool = %q, %v", out, err)
	}
	if _, err := os.Stat(sneaky); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("misclassified tool wrote in read-only mode: %v", err)
	}
	if out, err := manager.CallTool(context.Background(), "helper", "network", map[string]string{"url": networkServer.URL}); err != nil || !strings.Contains(out, "network-error") {
		t.Fatalf("network-off stdio tool = %q, %v", out, err)
	}

	oldPID := stdioServerPID(t, manager, "helper")
	if err := manager.Reconfigure(context.Background(), func() error {
		sandboxManager.SetNetworkAccess(true)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	newPID := stdioServerPID(t, manager, "helper")
	if oldPID == newPID {
		t.Fatalf("MCP process was not restarted: pid=%d", oldPID)
	}
	waitForProcessExit(t, oldPID)
	if out, err := manager.CallTool(context.Background(), "helper", "network", map[string]string{"url": networkServer.URL}); err != nil || out != "network-ok" {
		t.Fatalf("network-on stdio tool = %q, %v", out, err)
	}

	if err := manager.Reconfigure(context.Background(), func() error {
		return sandboxManager.SetMode(sandbox.ModeWorkspaceWrite)
	}); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(workspace, "inside.txt")
	if out, err := manager.CallTool(context.Background(), "helper", "write", map[string]string{"path": inside, "content": "inside"}); err != nil || out != "wrote" {
		t.Fatalf("workspace write = %q, %v", out, err)
	}
	if data, err := os.ReadFile(inside); err != nil || string(data) != "inside" {
		t.Fatalf("workspace file = %q, %v", data, err)
	}
	if out, err := manager.CallTool(context.Background(), "helper", "outside", map[string]string{"path": outside, "content": "outside"}); err != nil || !strings.Contains(out, "write-error") {
		t.Fatalf("outside write = %q, %v", out, err)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside file exists: %v", err)
	}
}

func TestStdioMCPLegacyFullAccessRemainsUnrestricted(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "legacy.txt")
	sandboxManager, err := sandbox.New(context.Background(), sandbox.Options{
		Workspace: workspace,
		HomeDir:   t.TempDir(),
		Mode:      sandbox.ModeFullAccess,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandboxManager.Close() })
	manager := newStdioHelperManager(workspace, sandboxManager, nil, "")
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.Enable(context.Background(), "helper"); err != nil {
		t.Fatal(err)
	}
	if out, err := manager.CallTool(context.Background(), "helper", "outside", map[string]string{"path": outside, "content": "legacy"}); err != nil || out != "wrote" {
		t.Fatalf("legacy full-access write = %q, %v", out, err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "legacy" {
		t.Fatalf("legacy outside file = %q, %v", data, err)
	}
}

func newStdioHelperManager(workspace string, sandboxManager *sandbox.Manager, access map[string]string, startupWrite string) *Manager {
	env := map[string]string{stdioHelperEnv: "1"}
	if startupWrite != "" {
		env["BRUCE_MCP_STARTUP_WRITE"] = startupWrite
	}
	return NewManager(config.MCPSettings{Servers: map[string]config.MCPServerSetting{
		"helper": {
			Type:       "stdio",
			Command:    os.Args[0],
			Args:       []string{"-test.run=^TestMCPStdioHelperProcess$"},
			Env:        env,
			ToolAccess: access,
		},
	}}, workspace).WithSandbox(sandboxManager)
}

func stdioServerPID(t *testing.T, manager *Manager, serverName string) int {
	t.Helper()
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	server := manager.servers[serverName]
	if server == nil || server.transport == nil {
		t.Fatalf("MCP server %q has no transport", serverName)
	}
	transport, ok := server.transport.(*StdioTransport)
	if !ok || transport.process == nil {
		t.Fatalf("MCP server %q transport = %T", serverName, server.transport)
	}
	return transport.process.PID()
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("old MCP process %d is still alive: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
