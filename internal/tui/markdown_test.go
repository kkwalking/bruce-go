package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestMarkdownStylePalette(t *testing.T) {
	tests := []struct {
		name       string
		style      markdownStyle
		foreground lipgloss.Color
		bold       bool
		italic     bool
	}{
		{name: "body", style: markdownBody, foreground: lipgloss.Color("#2B2B2B")},
		{name: "bold", style: markdownBold, foreground: lipgloss.Color("#2B2B2B"), bold: true},
		{name: "italic", style: markdownItalic, foreground: lipgloss.Color("#2B2B2B"), italic: true},
		{name: "heading", style: markdownHeading, foreground: lipgloss.Color("#E58A00"), bold: true},
		{name: "inline code", style: markdownInlineCode, foreground: lipgloss.Color("#009A4C")},
		{name: "link", style: markdownLink, foreground: lipgloss.Color("#009A78")},
		{name: "quote", style: markdownQuote, foreground: lipgloss.Color("#666666")},
		{name: "rule", style: markdownRule, foreground: lipgloss.Color("#969696")},
		{name: "keyword", style: markdownCodeKeyword, foreground: lipgloss.Color("#D98200")},
		{name: "function", style: markdownCodeFunction, foreground: lipgloss.Color("#DF3045")},
		{name: "string", style: markdownCodeString, foreground: lipgloss.Color("#009A4C")},
		{name: "number", style: markdownCodeNumber, foreground: lipgloss.Color("#D98200")},
		{name: "type", style: markdownCodeType, foreground: lipgloss.Color("#008F75")},
		{name: "comment", style: markdownCodeComment, foreground: lipgloss.Color("#777777")},
		{name: "operator", style: markdownCodeOperator, foreground: lipgloss.Color("#555555")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			style := styleForMarkdown(test.style)
			if got := style.GetForeground(); got != test.foreground {
				t.Fatalf("foreground = %v, want %v", got, test.foreground)
			}
			if got := style.GetBold(); got != test.bold {
				t.Fatalf("bold = %v, want %v", got, test.bold)
			}
			if got := style.GetItalic(); got != test.italic {
				t.Fatalf("italic = %v, want %v", got, test.italic)
			}
			if got := style.GetBackground(); got != (lipgloss.NoColor{}) {
				t.Fatalf("background = %v, want none", got)
			}
		})
	}
}

func TestRenderMarkdownHeadingKeepsFlatSpacing(t *testing.T) {
	lines := renderMarkdown("# Plan\n\nParagraph one.\n\n## Details\n\nParagraph two.", 40)
	got := texts(lines)
	want := []string{"Plan", "", "Paragraph one.", "", "Details", "", "Paragraph two."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for _, index := range []int{0, 4} {
		if len(lines[index].spans) != 1 || lines[index].spans[0].style != markdownHeading {
			t.Fatalf("heading line %d = %+v", index, lines[index])
		}
	}
	for _, index := range []int{2, 6} {
		if len(lines[index].spans) == 0 || lines[index].spans[0].style != markdownBody {
			t.Fatalf("body line %d = %+v", index, lines[index])
		}
	}
}

func TestRenderMarkdownInlineTokensStayFlat(t *testing.T) {
	lines := renderMarkdown("Text with **bold**, *italic*, `code`, [link](https://example.com), and internal/tool/tool.go:339-343.", 120)
	if len(lines) != 1 {
		t.Fatalf("lines = %+v", lines)
	}
	spans := lines[0].spans

	if got := spanText(spans, markdownBold); !strings.Contains(got, "bold") {
		t.Fatalf("bold spans = %q", got)
	}
	if got := spanText(spans, markdownItalic); !strings.Contains(got, "italic") {
		t.Fatalf("italic spans = %q", got)
	}
	if got := spanText(spans, markdownInlineCode); !strings.Contains(got, "code") || !strings.Contains(got, "internal/tool/tool.go:339-343") {
		t.Fatalf("inline code / path spans = %q", got)
	}
	if got := spanText(spans, markdownLink); got != "link" {
		t.Fatalf("link spans = %q", got)
	}
	if strings.Contains(lines[0].text, "https://example.com") {
		t.Fatalf("link URL should stay hidden for a low-key TUI render: %+v", lines[0])
	}
	rendered := renderMarkdownSpans(spans)
	if lipgloss.Width(rendered) != 70 {
		t.Fatalf("rendered width = %d, want 70; %q", lipgloss.Width(rendered), rendered)
	}
}

func TestRenderMarkdownBoldKeepsBodyColor(t *testing.T) {
	lines := renderMarkdown("**bold only**", 40)
	if len(lines) != 1 || len(lines[0].spans) != 1 {
		t.Fatalf("lines = %+v", lines)
	}
	span := lines[0].spans[0]
	if span.style != markdownBold || span.text != "bold only" {
		t.Fatalf("span = %+v", span)
	}
	style := styleForMarkdown(span.style)
	if got := style.GetForeground(); got != lipgloss.Color("#2B2B2B") {
		t.Fatalf("bold foreground = %v, want body gray #2B2B2B", got)
	}
}

func TestRenderMarkdownRuleIsThinGrayLine(t *testing.T) {
	lines := renderMarkdown("before\n\n---\n\nafter", 24)
	if len(lines) != 5 {
		t.Fatalf("lines = %q", texts(lines))
	}
	rule := lines[2]
	if rule.text != strings.Repeat("─", 24) {
		t.Fatalf("rule text = %q", rule.text)
	}
	if len(rule.spans) != 1 || rule.spans[0].style != markdownRule {
		t.Fatalf("rule spans = %+v", rule.spans)
	}
}

func TestRenderMarkdownListMarkersAreCompact(t *testing.T) {
	lines := renderMarkdown("- first\n- second\n1. third\n2. fourth", 40)
	if got := texts(lines); strings.Join(got, "|") != "- first|- second|1. third|2. fourth" {
		t.Fatalf("lines = %q", got)
	}
	for _, index := range []int{0, 1, 2, 3} {
		styles := map[markdownStyle]bool{}
		for _, span := range lines[index].spans {
			styles[span.style] = true
		}
		if !styles[markdownListMarker] || !styles[markdownBody] {
			t.Fatalf("list line %d styles = %v; spans = %+v", index, styles, lines[index].spans)
		}
	}
}

func TestRenderMarkdownBlockquoteUsesThinLeftBorder(t *testing.T) {
	lines := renderMarkdown("> quoted text", 40)
	if len(lines) != 1 {
		t.Fatalf("lines = %+v", lines)
	}
	if !strings.HasPrefix(lines[0].text, "│ ") {
		t.Fatalf("quote text = %q", lines[0].text)
	}
	if len(lines[0].spans) < 2 ||
		lines[0].spans[0].style != markdownQuoteBorder ||
		lines[0].spans[0].text != "│ " {
		t.Fatalf("quote spans = %+v", lines[0].spans)
	}
	if got := spanText(lines[0].spans, markdownQuote); got != "quoted text" {
		t.Fatalf("quote content = %q", got)
	}
}

func TestRenderMarkdownCodeBlockHasNoFenceOrBackground(t *testing.T) {
	lines := renderMarkdown("```go\nfunc main() {\n\tfmt.Println(\"hi\") // note\n}\n```", 80)
	if got := texts(lines); strings.Join(got, "|") != "func main() {|\tfmt.Println(\"hi\") // note|}" {
		t.Fatalf("code lines = %q", got)
	}
	styles := map[markdownStyle]bool{}
	for _, line := range lines {
		for _, span := range line.spans {
			styles[span.style] = true
			if style := styleForMarkdown(span.style); style.GetBackground() != (lipgloss.NoColor{}) {
				t.Fatalf("code span %+v must not set a background", span)
			}
		}
	}
	for _, want := range []markdownStyle{markdownCodeKeyword, markdownCodeFunction, markdownCodeString, markdownCodeComment} {
		if !styles[want] {
			t.Fatalf("style %v missing in code spans: %v", want, styles)
		}
	}
	if got := spanText(lines[0].spans, markdownCodeFunction); got != "main" {
		t.Fatalf("function spans = %q", got)
	}
	if got := spanText(lines[1].spans, markdownCodeComment); got != "// note" {
		t.Fatalf("comment spans = %q", got)
	}
}

func TestRenderMarkdownCodeHighlightingNumbersTypesAndOperators(t *testing.T) {
	lines := renderMarkdown("```go\nconst limit = 42\nvar name string\n", 80)
	if len(lines) != 2 {
		t.Fatalf("lines = %+v", texts(lines))
	}
	if got := spanText(lines[0].spans, markdownCodeNumber); got != "42" {
		t.Fatalf("number spans = %q", got)
	}
	if got := spanText(lines[0].spans, markdownCodeOperator); !strings.Contains(got, "=") {
		t.Fatalf("operator spans = %q", got)
	}
	if got := spanText(lines[1].spans, markdownCodeType); got != "string" {
		t.Fatalf("type spans = %q", got)
	}
	if got := spanText(lines[1].spans, markdownCodeKeyword); got != "var" {
		t.Fatalf("keyword spans = %q", got)
	}
}

func TestWrappedMessageLinesUsesMarkdownForAssistantMessages(t *testing.T) {
	model := NewModel(t.Context(), testRuntime(t))
	model.messages = []tuiMessage{
		{kind: messageAssistant, text: "# Title\n\nBody with `code`."},
		{kind: messageSystem, text: "plain"},
	}
	lines := model.wrappedMessageLines(80)
	got := texts(lines)
	want := []string{"Title", "", "Body with code.", "", "plain"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if lines[0].spans == nil || lines[0].spans[0].style != markdownHeading {
		t.Fatalf("heading line = %+v", lines[0])
	}
	if lines[4].spans != nil {
		t.Fatalf("system line should remain unstyled: %+v", lines[4])
	}

	model.messages[0].text = "**new body**"
	lines = model.wrappedMessageLines(80)
	if len(lines) == 0 || len(lines[0].spans) == 0 || lines[0].spans[0].style != markdownBold {
		t.Fatalf("cache did not refresh after message update: %+v", lines)
	}
}

func TestInlineItalicSupportsUnderscoreWithoutEatingIdentifiers(t *testing.T) {
	spans := parseInline("Use _italic_ but keep snake_case and a*b*c intact.")
	if got := spanText(spans, markdownItalic); got != "italic" {
		t.Fatalf("italic spans = %q, spans = %+v", got, spans)
	}
	if got := spanText(spans, markdownBody); !strings.Contains(got, "snake_case") || !strings.Contains(got, "a*b*c") {
		t.Fatalf("identifiers must stay body text: %q, spans = %+v", got, spans)
	}
}

func TestMarkdownWrappingKeepsQuotePrefixAndListIndentWithinWidth(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "blockquote", text: "> " + strings.Repeat("a ", 40)},
		{name: "list", text: "- " + strings.Repeat("b ", 40)},
		{name: "code", text: "```go\n" + strings.Repeat("c ", 40) + "\n```"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lines := renderMarkdown(test.text, 14)
			if len(lines) < 2 {
				t.Fatalf("expected wrapped lines: %q", texts(lines))
			}
			for _, line := range lines {
				if line.spans == nil {
					continue
				}
				rendered := renderMarkdownSpans(line.spans)
				if width := lipgloss.Width(rendered); width > 14 {
					t.Fatalf("line width = %d, want <= 14: %q", width, rendered)
				}
			}
			if test.name == "blockquote" {
				for _, line := range lines {
					if !strings.HasPrefix(line.text, "│ ") {
						t.Fatalf("continuation line lost quote border: %q", line.text)
					}
				}
			}
			if test.name == "list" {
				if !strings.HasPrefix(lines[1].text, "  ") {
					t.Fatalf("list continuation line missing hanging indent: %q", lines[1].text)
				}
			}
		})
	}
}

func TestMarkdownPathHighlighterKeepsFileReferenceIntact(t *testing.T) {
	spans := parseInline("See internal/tool/tool.go:339-343 for the fix.")
	got := spanText(spans, markdownInlineCode)
	if got != "internal/tool/tool.go:339-343" {
		t.Fatalf("path spans = %q", got)
	}
}

func spanText(spans []markdownSpan, style markdownStyle) string {
	var out []string
	for _, span := range spans {
		if span.style == style {
			out = append(out, span.text)
		}
	}
	return strings.Join(out, "")
}
