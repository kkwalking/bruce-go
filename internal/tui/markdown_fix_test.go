package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

// TestContentAfterClosingFenceRenders guards HIGH-1 root cause: a paragraph
// (or any text) immediately following a closing code fence, with no blank
// line in between, must not be silently dropped. LLM output commonly emits
// ```go ... ``` followed directly by an explanation.
func TestContentAfterClosingFenceRenders(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		expect string // substring that must appear in rendered output
	}{
		{
			name:   "plain text after closing fence",
			text:   "```go\ncode a\n```\nhere is an explanation\n",
			expect: "here is an explanation",
		},
		{
			name:   "multiple lines after closing fence",
			text:   "```go\ncode a\n```\nline one\nline two\n",
			expect: "line two",
		},
		{
			name:   "second block after closing fence",
			text:   "```go\nc1\n```\n```go\nc2\n```\n",
			expect: "c2",
		},
		{
			name:   "heading after closing fence",
			text:   "```go\ncode a\n```\n# Summary\n",
			expect: "Summary",
		},
		{
			name:   "text before and after fence both render",
			text:   "intro\n```go\ncode a\n```\noutro\n",
			expect: "outro",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out := renderMarkdownJoined(test.text, 60)
			if !strings.Contains(out, test.expect) {
				t.Fatalf("rendered output missing %q:\n%s", test.expect, out)
			}
		})
	}
}

// TestMarkdownOutputNeverExceedsColumns is a regression guard: every rendered
// markdown line must fit within the requested column width (re-verified: the
// current renderer already wraps within width; this locks it in).
func TestMarkdownOutputNeverExceedsColumns(t *testing.T) {
	cases := []struct {
		name, text string
		columns    int
	}{
		{"tabs-in-code", "```go\nfunc x() {\n\treturn 1\n}\n```", 10},
		{"cjk-bold", "**你好这是中文**", 6},
		{"long-inline-code", "`supercallssomefunctionnamethatislotsoflong`", 10},
		{"long-code-token", "```go\nverylongidentifierthatoverflowsthelines\n```", 12},
		{"wrapped-quote", "> this is a quote that should wrap nicely across lines okay", 12},
		{"text-after-fence", "```go\ncode a\n```\nhere is a trailing explanation that wraps\n", 15},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := renderMarkdown(c.text, c.columns)
			if len(lines) == 0 {
				t.Fatal("no lines rendered")
			}
			for _, l := range lines {
				if w := runewidth.StringWidth(l.text); w > c.columns {
					t.Errorf("line width %d > columns %d: %q", w, c.columns, l.text)
				}
			}
		})
	}
}

// renderMarkdownJoined renders markdown and joins line text with newlines,
// dropping ANSI style codes (style rendering only matters for display, not
// content-presence assertions).
func renderMarkdownJoined(text string, columns int) string {
	lines := renderMarkdown(text, columns)
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(l.text)
	}
	return b.String()
}