package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"bruce-go/internal/sandbox"
	"bruce-go/internal/tool"
)

type ToolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

type Tool struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	InputSchema json.RawMessage  `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

type RegisteredTool struct {
	Server          string
	Tool            Tool
	MinimumMode     sandbox.Mode
	RequiresNetwork bool
}

func (m *Manager) Tools() []RegisteredTool {
	policy := m.sandboxStatus()
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []RegisteredTool
	for _, server := range m.servers {
		if !server.Enabled || !server.Ready {
			continue
		}
		for _, candidate := range server.Tools {
			if !toolAllowed(server.Config, candidate.Name, policy) {
				continue
			}
			out = append(out, RegisteredTool{
				Server:          server.Name,
				Tool:            candidate,
				MinimumMode:     configuredToolMode(server.Config, candidate.Name),
				RequiresNetwork: isHTTPTransport(server.Config.Type),
			})
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

func (m *Manager) CallTool(ctx context.Context, serverName, toolName string, args map[string]string) (string, error) {
	policy := m.sandboxStatus()
	m.mu.RLock()
	server := m.servers[serverName]
	if m.transitioning {
		m.mu.RUnlock()
		return "", errors.New("MCP policy transition is in progress")
	}
	if server == nil || !server.Enabled || !server.Ready || server.transport == nil {
		m.mu.RUnlock()
		return "", errors.New("MCP server is not ready: " + serverName)
	}
	if !toolAllowed(server.Config, toolName, policy) {
		required := configuredToolMode(server.Config, toolName)
		m.mu.RUnlock()
		return "", fmt.Errorf("MCP tool rejected by sandbox policy: %s/%s requires %s, current mode is %s", serverName, toolName, required, policy.Mode)
	}
	transport := server.transport
	m.calls.Add(1)
	m.mu.RUnlock()
	defer m.calls.Done()

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
			Description: "[MCP " + serverName + "] " + item.Tool.Description + annotationSummary(item.Tool.Annotations),
			Parameters:  schema,
			Exec: func(ctx context.Context, args map[string]string) (string, error) {
				return manager.CallTool(ctx, serverName, remoteName, args)
			},
			PromptSnippet: "Call MCP tool " + serverName + "/" + remoteName,
			Policy: tool.Policy{
				Source:          tool.SourceMCP,
				MinimumMode:     item.MinimumMode,
				RequiresNetwork: item.RequiresNetwork,
			},
		})
	}
}

func annotationSummary(annotations *ToolAnnotations) string {
	if annotations == nil {
		return ""
	}
	var hints []string
	if annotations.ReadOnlyHint != nil {
		hints = append(hints, fmt.Sprintf("readOnlyHint=%t", *annotations.ReadOnlyHint))
	}
	if annotations.DestructiveHint != nil {
		hints = append(hints, fmt.Sprintf("destructiveHint=%t", *annotations.DestructiveHint))
	}
	if annotations.OpenWorldHint != nil {
		hints = append(hints, fmt.Sprintf("openWorldHint=%t", *annotations.OpenWorldHint))
	}
	if len(hints) == 0 {
		return ""
	}
	return " [untrusted server hints: " + strings.Join(hints, ", ") + "]"
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

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_]+`)

func sanitize(value string) string {
	out := unsafeName.ReplaceAllString(value, "_")
	out = strings.Trim(out, "_")
	if out == "" {
		return "tool"
	}
	return strings.ToLower(out)
}
