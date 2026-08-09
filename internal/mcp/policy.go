package mcp

import (
	"strings"

	"bruce-go/internal/config"
	"bruce-go/internal/sandbox"
)

func configuredToolMode(cfg config.MCPServerSetting, toolName string) sandbox.Mode {
	raw := strings.TrimSpace(cfg.ToolAccess[toolName])
	mode, err := sandbox.ParseMode(raw)
	if err != nil {
		return sandbox.ModeFullAccess
	}
	return mode
}

func toolAllowed(cfg config.MCPServerSetting, toolName string, status sandbox.Status) bool {
	if status.Mode == sandbox.ModeFullAccess {
		return true
	}
	required := configuredToolMode(cfg, toolName)
	if isHTTPTransport(cfg.Type) {
		return status.NetworkAccess && required == sandbox.ModeReadOnly
	}
	switch required {
	case sandbox.ModeReadOnly:
		return true
	case sandbox.ModeWorkspaceWrite:
		return status.Mode == sandbox.ModeWorkspaceWrite
	default:
		return false
	}
}

func serverStartBlock(cfg config.MCPServerSetting, status sandbox.Status) string {
	if status.Mode == sandbox.ModeFullAccess {
		return ""
	}
	if isHTTPTransport(cfg.Type) && !status.NetworkAccess {
		return "sandbox network access is disabled; HTTP MCP was not initialized"
	}
	for _, access := range cfg.ToolAccess {
		required, err := sandbox.ParseMode(strings.TrimSpace(access))
		if err != nil {
			continue
		}
		if isHTTPTransport(cfg.Type) {
			if required == sandbox.ModeReadOnly {
				return ""
			}
			continue
		}
		if required == sandbox.ModeReadOnly || (required == sandbox.ModeWorkspaceWrite && status.Mode == sandbox.ModeWorkspaceWrite) {
			return ""
		}
	}
	return "no MCP tools are authorized by toolAccess under the current sandbox policy"
}

func enforcementText(cfg config.MCPServerSetting, status sandbox.Status) string {
	if status.Mode == sandbox.ModeFullAccess {
		return "unrestricted"
	}
	if isHTTPTransport(cfg.Type) {
		return "trusted-remote"
	}
	backend := status.Capabilities.Backend
	if backend == "" {
		backend = "native"
	}
	return backend + "/" + string(status.Mode)
}
