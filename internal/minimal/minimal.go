// Package minimal defines the minimal agent mode inspired by DeepSeek
// Harness: a fixed one-line system prompt and a small built-in tool set.
package minimal

import "bruce-go/internal/tool"

// SystemPrompt is the complete system prompt for the minimal mode. It mirrors
// apps/cli/config/agent-presets/minimal/agent.cordis.yml in deepseek-harness.
const SystemPrompt = "You are a helpful software engineer assistant."

// ToolNames lists the model-visible tools for the minimal mode. Bruce has no
// str_replace_editor, so the file operations are provided by the built-in
// read_file, write_file, and edit_file tools.
func ToolNames() []string {
	return []string{"execute_command", "read_file", "write_file", "edit_file"}
}

// NewToolRegistry returns a copy of base limited to the minimal tool set.
func NewToolRegistry(base *tool.Registry) *tool.Registry {
	return base.Subset(ToolNames()...)
}
