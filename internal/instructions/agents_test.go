package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAgentsInstructionsOrderAndLimit(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	nested := filepath.Join(root, "pkg", "feature")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".bruce", "AGENTS.md"), "home")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root")
	writeFile(t, filepath.Join(root, "pkg", "AGENTS.md"), "pkg")
	writeFile(t, filepath.Join(nested, "AGENTS.md"), "nested\n@/tmp/not-expanded.md")

	result := Load(home, nested)
	if result.Prompt != "home\n\nroot\n\npkg\n\nnested\n@/tmp/not-expanded.md" {
		t.Fatalf("prompt order = %q", result.Prompt)
	}
	if strings.Contains(result.Prompt, "not expanded content") {
		t.Fatal("@ references should not be expanded")
	}
}

func writeFile(t *testing.T, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
