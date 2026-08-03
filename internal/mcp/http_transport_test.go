package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bruce-go/internal/config"
)

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
