package cli

import (
	"strings"
	"testing"
)

func TestParseSlashAndExitAliases(t *testing.T) {
	tests := []struct {
		input string
		name  string
		args  []string
		ok    bool
	}{
		{input: "/web search bruce go", name: "web", args: []string{"search", "bruce", "go"}, ok: true},
		{input: "exit", name: "exit", ok: true},
		{input: "quit", name: "exit", ok: true},
		{input: "normal task", ok: false},
	}
	for _, tt := range tests {
		cmd, ok := Parse(tt.input)
		if ok != tt.ok {
			t.Fatalf("Parse(%q) ok=%v", tt.input, ok)
		}
		if !ok {
			continue
		}
		if cmd.Name != tt.name {
			t.Fatalf("Parse(%q) name=%q", tt.input, cmd.Name)
		}
		if strings.Join(cmd.Args, ",") != strings.Join(tt.args, ",") {
			t.Fatalf("Parse(%q) args=%v", tt.input, cmd.Args)
		}
	}
}

func TestHelpContainsNonRAGCommandsOnly(t *testing.T) {
	help := Help()
	for _, want := range []string{"/react", "/plan", "/model", "/web", "/mcp", "/skill", "/status", "/compact", "@image:", "@clipboard"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
	for _, forbidden := range []string{"/rag", "/index", "/graph"} {
		if strings.Contains(help, forbidden) {
			t.Fatalf("help contains forbidden RAG entry %q:\n%s", forbidden, help)
		}
	}
}
