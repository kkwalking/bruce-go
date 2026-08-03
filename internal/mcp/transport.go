package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"bruce-go/internal/config"
	"bruce-go/internal/sandbox"
	"bruce-go/internal/version"
)

const mcpProtocolVersion = "2024-11-05"

type Transport interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	Notify(ctx context.Context, method string, params any) error
	Close() error
	Logs() []string
}

type TransportFactory func(ctx context.Context, name string, cfg config.MCPServerSetting, workspace string) (Transport, error)

func DefaultTransportFactory(ctx context.Context, _ string, cfg config.MCPServerSetting, workspace string) (Transport, error) {
	return defaultTransportFactory(ctx, "", cfg, workspace, nil)
}

func defaultTransportFactory(ctx context.Context, _ string, cfg config.MCPServerSetting, workspace string, launcher *sandbox.Manager) (Transport, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "", "stdio":
		return newStdioTransport(ctx, cfg, workspace, launcher)
	case "http", "streamable_http", "streamable-http", "streamablehttp":
		return NewHTTPTransport(cfg), nil
	default:
		return nil, errors.New("不支持的 MCP transport: " + cfg.Type)
	}
}

func initializeAndList(ctx context.Context, transport Transport) ([]Tool, error) {
	if _, err := transport.Call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"clientInfo":      map[string]string{"name": "bruce-go", "version": version.Current},
		"capabilities":    map[string]any{},
	}); err != nil {
		return nil, fmt.Errorf("MCP initialize 失败: %w", err)
	}
	if err := transport.Notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return nil, fmt.Errorf("MCP notifications/initialized 失败: %w", err)
	}
	raw, err := transport.Call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("MCP tools/list 失败: %w", err)
	}
	var payload struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("MCP tools/list 响应解析失败: %w", err)
	}
	return payload.Tools, nil
}

func normalizedTransport(raw string) string {
	if isHTTPTransport(raw) {
		return "http"
	}
	return "stdio"
}

func isHTTPTransport(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "http", "streamable_http", "streamable-http", "streamablehttp":
		return true
	default:
		return false
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
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
