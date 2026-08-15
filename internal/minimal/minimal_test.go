package minimal

import (
	"strings"
	"testing"

	"bruce-go/internal/tool"
)

func TestNewToolRegistryContainsEveryDeclaredTool(t *testing.T) {
	registry := NewToolRegistry(tool.NewRegistry(t.TempDir()))
	var definitions []string
	for _, definition := range registry.Definitions() {
		definitions = append(definitions, definition.Name)
	}
	if strings.Join(definitions, ",") != "edit_file,execute_command,read_file,write_file" {
		t.Fatalf("minimal tool definitions = %v", definitions)
	}
	for _, name := range ToolNames() {
		if _, ok := registry.Lookup(name); !ok {
			t.Fatalf("minimal registry is missing declared tool %q", name)
		}
	}
}

func TestSystemPromptIsExact(t *testing.T) {
	if SystemPrompt != "You are a helpful software engineer assistant." {
		t.Fatalf("system prompt = %q", SystemPrompt)
	}
}
