package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bruce-go/internal/config"
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

func (f *fakeTransport) Close() error   { return nil }
func (f *fakeTransport) Logs() []string { return []string{"fake log"} }

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
