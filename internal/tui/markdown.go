package tui

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// OpenCode Light Theme inspired markdown rendering.
//
// The renderer deliberately stays flat and minimal: no card/panel
// backgrounds, no borders around headings or code blocks, and color is used
// only as a light emphasis layer on top of a dark-gray body.

type markdownStyle int

const (
	markdownBody markdownStyle = iota
	markdownBold
	markdownItalic
	markdownBoldItalic
	markdownHeading
	markdownInlineCode
	markdownLink
	markdownListMarker
	markdownQuoteBorder
	markdownQuote
	markdownQuoteBold
	markdownQuoteItalic
	markdownQuoteBoldItalic
	markdownRule
	markdownCode
	markdownCodeKeyword
	markdownCodeFunction
	markdownCodeString
	markdownCodeNumber
	markdownCodeType
	markdownCodeComment
	markdownCodeOperator
)

type markdownSpan struct {
	text  string
	style markdownStyle
}

type markdownBlockKind int

const (
	markdownParagraph markdownBlockKind = iota
	markdownHeadingBlock
	markdownRuleBlock
	markdownQuoteBlock
	markdownListBlock
	markdownCodeBlock
)

type markdownBlock struct {
	kind     markdownBlockKind
	lines    []string
	language string
}

var (
	markdownHeadingPattern = regexp.MustCompile(`^[ \t]{0,3}(#{1,6})[ \t]+(.+)$`)
	markdownQuotePattern   = regexp.MustCompile(`^[ \t]{0,3}>[ \t]?(.*)$`)
	markdownListPattern    = regexp.MustCompile(`^([ \t]*)(?:([-+*])|(\d+)([.)]))[ \t]+(.*)$`)
	markdownPathPattern    = regexp.MustCompile(`(?:^|[\s"'(\[<])((?:\.{0,2}/|~/)?(?:[A-Za-z0-9_.@-]+/)*[A-Za-z0-9_.@-]+\.(?:go|mod|sum|md|markdown|ya?ml|json|toml|txt|py|js|ts|jsx|tsx|java|rb|sh|sql|rs|c|h|cpp|hpp|css|html|xml|env|ini|conf)(?::\d+(?:-\d+)?)?)(?:[\s"')\].>,;:]|$)`)
)

var (
	markdownBodyStyle            = lipgloss.NewStyle().Foreground(lipgloss.Color("#2B2B2B"))
	markdownBoldStyle            = lipgloss.NewStyle().Foreground(lipgloss.Color("#2B2B2B")).Bold(true)
	markdownItalicStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("#2B2B2B")).Italic(true)
	markdownBoldItalicStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#2B2B2B")).Bold(true).Italic(true)
	markdownHeadingStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("#E58A00")).Bold(true)
	markdownInlineCodeStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#009A4C"))
	markdownLinkStyle            = lipgloss.NewStyle().Foreground(lipgloss.Color("#009A78"))
	markdownListMarkerStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#E58A00"))
	markdownQuoteBorderStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#969696"))
	markdownQuoteStyle           = lipgloss.NewStyle().Foreground(lipgloss.Color("#666666"))
	markdownQuoteBoldStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("#666666")).Bold(true)
	markdownQuoteItalicStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#666666")).Italic(true)
	markdownQuoteBoldItalicStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#666666")).Bold(true).Italic(true)
	markdownRuleStyle            = lipgloss.NewStyle().Foreground(lipgloss.Color("#969696"))
	markdownCodeStyle            = lipgloss.NewStyle().Foreground(lipgloss.Color("#2B2B2B"))
	markdownCodeKeywordStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#D98200"))
	markdownCodeFunctionStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#DF3045"))
	markdownCodeStringStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#009A4C"))
	markdownCodeNumberStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#D98200"))
	markdownCodeTypeStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#008F75"))
	markdownCodeCommentStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#777777"))
	markdownCodeOperatorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#555555"))
)

func styleForMarkdown(style markdownStyle) lipgloss.Style {
	switch style {
	case markdownBold:
		return markdownBoldStyle
	case markdownItalic:
		return markdownItalicStyle
	case markdownBoldItalic:
		return markdownBoldItalicStyle
	case markdownHeading:
		return markdownHeadingStyle
	case markdownInlineCode:
		return markdownInlineCodeStyle
	case markdownLink:
		return markdownLinkStyle
	case markdownListMarker:
		return markdownListMarkerStyle
	case markdownQuoteBorder:
		return markdownQuoteBorderStyle
	case markdownQuote:
		return markdownQuoteStyle
	case markdownQuoteBold:
		return markdownQuoteBoldStyle
	case markdownQuoteItalic:
		return markdownQuoteItalicStyle
	case markdownQuoteBoldItalic:
		return markdownQuoteBoldItalicStyle
	case markdownRule:
		return markdownRuleStyle
	case markdownCode:
		return markdownCodeStyle
	case markdownCodeKeyword:
		return markdownCodeKeywordStyle
	case markdownCodeFunction:
		return markdownCodeFunctionStyle
	case markdownCodeString:
		return markdownCodeStringStyle
	case markdownCodeNumber:
		return markdownCodeNumberStyle
	case markdownCodeType:
		return markdownCodeTypeStyle
	case markdownCodeComment:
		return markdownCodeCommentStyle
	case markdownCodeOperator:
		return markdownCodeOperatorStyle
	default:
		return markdownBodyStyle
	}
}

func renderMarkdown(text string, columns int) []renderLine {
	blocks := parseMarkdownBlocks(text)
	var out []renderLine
	for i, block := range blocks {
		lines := renderMarkdownBlock(block, columns)
		if i > 0 && len(lines) > 0 && !lineIsEmpty(out[len(out)-1]) && !lineIsEmpty(lines[0]) {
			out = append(out, markdownLine(nil))
		}
		out = append(out, lines...)
	}
	for len(out) > 0 && lineIsEmpty(out[len(out)-1]) {
		out = out[:len(out)-1]
	}
	return out
}

func parseMarkdownBlocks(text string) []markdownBlock {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	var blocks []markdownBlock
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if language, ok := fenceOpening(line); ok {
			i++
			var code []string
			for i < len(lines) && !fenceClosing(lines[i]) {
				code = append(code, lines[i])
				i++
			}
			if i < len(lines) {
				i++
			}
			blocks = append(blocks, markdownBlock{kind: markdownCodeBlock, lines: code, language: language})
			continue
		}
		if heading, ok := parseMarkdownHeading(line); ok {
			blocks = append(blocks, markdownBlock{kind: markdownHeadingBlock, lines: []string{heading}})
			continue
		}
		if isMarkdownRule(line) {
			blocks = append(blocks, markdownBlock{kind: markdownRuleBlock})
			continue
		}
		if _, matched, _ := markdownQuoteLine(line); matched {
			var quote []string
			for i < len(lines) {
				content, matched, _ := markdownQuoteLine(lines[i])
				if !matched {
					break
				}
				quote = append(quote, content)
				i++
			}
			i--
			blocks = append(blocks, markdownBlock{kind: markdownQuoteBlock, lines: quote})
			continue
		}
		if _, _, _, ok := markdownListItem(line); ok {
			var items []string
			for i < len(lines) {
				itemIndent, itemMarker, itemContent, ok := markdownListItem(lines[i])
				if !ok {
					break
				}
				items = append(items, itemIndent+"\x00"+itemMarker+"\x00"+itemContent)
				i++
			}
			i--
			blocks = append(blocks, markdownBlock{kind: markdownListBlock, lines: items})
			continue
		}
		// Paragraph: absorb consecutive ordinary lines as one block.
		var paragraph []string
		for i < len(lines) {
			candidate := lines[i]
			if strings.TrimSpace(candidate) == "" || isMarkdownBlockStart(candidate) {
				break
			}
			paragraph = append(paragraph, candidate)
			i++
		}
		i--
		blocks = append(blocks, markdownBlock{kind: markdownParagraph, lines: paragraph})
	}
	return blocks
}

func renderMarkdownBlock(block markdownBlock, columns int) []renderLine {
	switch block.kind {
	case markdownHeadingBlock:
		return wrapMarkdownSpans([]markdownSpan{{text: strings.TrimSpace(block.lines[0]), style: markdownHeading}}, columns, nil, nil)
	case markdownRuleBlock:
		width := max(1, columns)
		return []renderLine{markdownLine([]markdownSpan{{text: strings.Repeat("─", width), style: markdownRule}})}
	case markdownQuoteBlock:
		var out []renderLine
		border := []markdownSpan{{text: "│ ", style: markdownQuoteBorder}}
		for _, quote := range block.lines {
			spans := quoteStyleSpans(parseInline(quote))
			out = append(out, wrapMarkdownSpans(spans, columns, border, border)...)
		}
		return out
	case markdownListBlock:
		var out []renderLine
		for _, item := range block.lines {
			parts := strings.SplitN(item, "\x00", 3)
			if len(parts) != 3 {
				continue
			}
			indent, marker, content := parts[0], parts[1], parts[2]
			first := []markdownSpan{
				{text: indent, style: markdownBody},
				{text: marker, style: markdownListMarker},
				{text: " ", style: markdownBody},
			}
			continuation := []markdownSpan{{text: indent + strings.Repeat(" ", runewidth.StringWidth(marker)+1), style: markdownBody}}
			out = append(out, wrapMarkdownSpans(parseInline(content), columns, first, continuation)...)
		}
		return out
	case markdownCodeBlock:
		var out []renderLine
		for _, code := range block.lines {
			out = append(out, wrapMarkdownSpans(highlightCodeLine(code, block.language), columns, nil, nil)...)
		}
		return out
	default:
		var out []renderLine
		for _, paragraphLine := range block.lines {
			out = append(out, wrapMarkdownSpans(parseInline(paragraphLine), columns, nil, nil)...)
		}
		return out
	}
}

func isMarkdownBlockStart(line string) bool {
	if _, ok := fenceOpening(line); ok {
		return true
	}
	if _, ok := parseMarkdownHeading(line); ok {
		return true
	}
	if isMarkdownRule(line) {
		return true
	}
	if _, matched, _ := markdownQuoteLine(line); matched {
		return true
	}
	if _, _, _, ok := markdownListItem(line); ok {
		return true
	}
	return false
}

func parseMarkdownHeading(line string) (string, bool) {
	match := markdownHeadingPattern.FindStringSubmatch(line)
	if match == nil {
		return "", false
	}
	return strings.TrimSpace(match[2]), true
}

func markdownQuoteLine(line string) (content string, matched bool, blank bool) {
	match := markdownQuotePattern.FindStringSubmatch(line)
	if match == nil {
		return "", false, false
	}
	return match[1], true, strings.TrimSpace(match[1]) == ""
}

func markdownListItem(line string) (indent, marker, content string, ok bool) {
	match := markdownListPattern.FindStringSubmatch(line)
	if match == nil {
		return "", "", "", false
	}
	indent = match[1]
	if match[2] != "" {
		marker = match[2]
	} else {
		marker = match[3] + match[4]
	}
	return indent, marker, match[5], true
}

func isMarkdownRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	var marker rune
	count := 0
	for _, r := range trimmed {
		switch r {
		case ' ', '\t':
			continue
		case '-', '*', '_':
			if marker == 0 {
				marker = r
			}
			if r != marker {
				return false
			}
			count++
		default:
			return false
		}
	}
	return count >= 3
}

func fenceOpening(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "```") && !strings.HasPrefix(trimmed, "~~~") {
		return "", false
	}
	marker := "```"
	if strings.HasPrefix(trimmed, "~~~") {
		marker = "~~~"
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
	if rest == "" {
		return "", true
	}
	return strings.Fields(rest)[0], true
}

func fenceClosing(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
		return true
	}
	return false
}

// parseInline handles the small inline vocabulary OpenCode uses most:
// `code`, *italic*, _italic_, **bold**, __bold__, [text](url), escaped
// punctuation, and file-path references.
func parseInline(input string) []markdownSpan {
	spans := parseInlineDepth(input)
	return mergeMarkdownSpans(spans)
}

func parseInlineDepth(input string) []markdownSpan {
	var spans []markdownSpan
	plainStart := 0
	flushPlain := func(end int) {
		if end <= plainStart {
			return
		}
		spans = append(spans, filePathSpans(input[plainStart:end])...)
		plainStart = end
	}

	for i := 0; i < len(input); {
		if input[i] == '\\' && i+1 < len(input) && isMarkdownEscape(input[i+1]) {
			flushPlain(i)
			spans = append(spans, markdownSpan{text: input[i+1 : i+2], style: markdownBody})
			i += 2
			plainStart = i
			continue
		}

		switch input[i] {
		case '`':
			if end := strings.IndexByte(input[i+1:], '`'); end >= 0 {
				flushPlain(i)
				spans = append(spans, markdownSpan{text: input[i+1 : i+1+end], style: markdownInlineCode})
				i += end + 2
				plainStart = i
				continue
			}
		case '[':
			if textEnd, urlEnd, ok := markdownLinkEnds(input, i); ok {
				flushPlain(i)
				spans = append(spans, markdownSpan{text: input[i+1 : textEnd], style: markdownLink})
				i = urlEnd + 1
				plainStart = i
				continue
			}
		case '*', '_':
			double := i+1 < len(input) && input[i+1] == input[i]
			if double {
				if end, ok := markdownDelimiterEnd(input, i+2, input[i]); ok {
					flushPlain(i)
					spans = append(spans, boldSpans(parseInlineDepth(input[i+2:end]))...)
					i = end + 2
					plainStart = i
					continue
				}
			}
			if end, ok := markdownItalicEnd(input, i); ok {
				flushPlain(i)
				spans = append(spans, italicSpans(parseInlineDepth(input[i+1:end]))...)
				i = end + 1
				plainStart = i
				continue
			}
		}
		i++
	}
	flushPlain(len(input))
	return mergeMarkdownSpans(spans)
}

func markdownLinkEnds(input string, start int) (textEnd, urlEnd int, ok bool) {
	textEndRelative := strings.Index(input[start+1:], "](")
	if textEndRelative < 0 {
		return 0, 0, false
	}
	textEnd = start + 1 + textEndRelative
	urlStart := textEnd + 2
	if urlStart >= len(input) {
		return 0, 0, false
	}
	closeRelative := strings.IndexByte(input[urlStart:], ')')
	if closeRelative < 0 {
		return 0, 0, false
	}
	return textEnd, urlStart + closeRelative, true
}

func markdownDelimiterEnd(input string, start int, delimiter byte) (int, bool) {
	for i := start; i+1 < len(input); {
		if input[i] == '\\' {
			i += 2
			continue
		}
		if input[i] == delimiter && input[i+1] == delimiter {
			return i, true
		}
		i++
	}
	return 0, false
}

func markdownItalicEnd(input string, start int) (int, bool) {
	delimiter := input[start]
	contentStart := start + 1
	first, _ := utf8.DecodeRuneInString(input[contentStart:])
	if start > 0 && isWordRune(previousRune(input, start)) && isWordRune(first) {
		return 0, false
	}

	for i := contentStart; i < len(input); i++ {
		if input[i] != delimiter {
			continue
		}
		if i > 0 && input[i-1] == '\\' {
			continue
		}
		content := input[contentStart:i]
		first, _ := utf8.DecodeRuneInString(content)
		last, _ := utf8.DecodeLastRuneInString(content)
		if content == "" || unicode.IsSpace(first) || unicode.IsSpace(last) {
			continue
		}
		if delimiter == '_' && i+1 < len(input) && isWordRune(rune(input[i+1])) {
			continue
		}
		return i, true
	}
	return 0, false
}

func previousRune(input string, before int) rune {
	r, _ := utf8.DecodeLastRuneInString(input[:before])
	return r
}

func boldSpans(spans []markdownSpan) []markdownSpan {
	out := make([]markdownSpan, 0, len(spans))
	for _, span := range spans {
		switch span.style {
		case markdownBody:
			out = append(out, markdownSpan{text: span.text, style: markdownBold})
		case markdownItalic:
			out = append(out, markdownSpan{text: span.text, style: markdownBoldItalic})
		default:
			out = append(out, span)
		}
	}
	return out
}

func italicSpans(spans []markdownSpan) []markdownSpan {
	out := make([]markdownSpan, 0, len(spans))
	for _, span := range spans {
		switch span.style {
		case markdownBody:
			out = append(out, markdownSpan{text: span.text, style: markdownItalic})
		case markdownBold:
			out = append(out, markdownSpan{text: span.text, style: markdownBoldItalic})
		default:
			out = append(out, span)
		}
	}
	return out
}

func quoteStyleSpans(spans []markdownSpan) []markdownSpan {
	out := make([]markdownSpan, 0, len(spans))
	for _, span := range spans {
		switch span.style {
		case markdownBody:
			out = append(out, markdownSpan{text: span.text, style: markdownQuote})
		case markdownBold:
			out = append(out, markdownSpan{text: span.text, style: markdownQuoteBold})
		case markdownItalic:
			out = append(out, markdownSpan{text: span.text, style: markdownQuoteItalic})
		case markdownBoldItalic:
			out = append(out, markdownSpan{text: span.text, style: markdownQuoteBoldItalic})
		default:
			out = append(out, span)
		}
	}
	return out
}

func filePathSpans(text string) []markdownSpan {
	matches := markdownPathPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return []markdownSpan{{text: text, style: markdownBody}}
	}
	var out []markdownSpan
	last := 0
	for _, match := range matches {
		// match[2:4] is the path capture (with optional :line[-line]).
		pathStart, pathEnd := match[2], match[3]
		if pathStart < last || pathStart >= pathEnd {
			continue
		}
		if pathStart > last {
			out = append(out, markdownSpan{text: text[last:pathStart], style: markdownBody})
		}
		out = append(out, markdownSpan{text: text[pathStart:pathEnd], style: markdownInlineCode})
		last = pathEnd
	}
	if last < len(text) {
		out = append(out, markdownSpan{text: text[last:], style: markdownBody})
	}
	return mergeMarkdownSpans(out)
}

func mergeMarkdownSpans(spans []markdownSpan) []markdownSpan {
	if len(spans) < 2 {
		return spans
	}
	out := spans[:1]
	for _, span := range spans[1:] {
		last := &out[len(out)-1]
		if last.style == span.style {
			last.text += span.text
			continue
		}
		out = append(out, span)
	}
	return out
}

func isMarkdownEscape(r byte) bool {
	return strings.ContainsRune("\\`*_[]()", rune(r))
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// wrapMarkdownSpans wraps styled spans at display width. firstPrefix is
// emitted once at the top of the logical line; restPrefix is emitted at the
// beginning of every continuation line (used for blockquote borders and list
// hanging indents).
func wrapMarkdownSpans(spans []markdownSpan, columns int, firstPrefix, restPrefix []markdownSpan) []renderLine {
	width := max(1, columns)
	if len(spans) == 0 {
		return []renderLine{markdownLine(append([]markdownSpan(nil), firstPrefix...))}
	}

	firstPrefixWidth := markdownSpansWidth(firstPrefix)
	restPrefixWidth := markdownSpansWidth(restPrefix)

	var out []renderLine
	current := append([]markdownSpan(nil), firstPrefix...)
	currentPrefixWidth := firstPrefixWidth
	currentContentWidth := 0

	flush := func() {
		out = append(out, markdownLine(current))
		current = append([]markdownSpan(nil), restPrefix...)
		currentPrefixWidth = restPrefixWidth
		currentContentWidth = 0
	}

	for _, span := range spans {
		for len(span.text) > 0 {
			r, size := utf8.DecodeRuneInString(span.text)
			span.text = span.text[size:]
			runeWidth := max(0, runewidth.RuneWidth(r))
			available := width - currentPrefixWidth
			if currentContentWidth > 0 && currentContentWidth+runeWidth > available {
				flush()
			}
			appendMarkdownSpan(&current, string(r), span.style)
			currentContentWidth += runeWidth
		}
	}
	if len(current) > 0 || len(out) == 0 {
		out = append(out, markdownLine(current))
	}
	return out
}

func appendMarkdownSpan(spans *[]markdownSpan, text string, style markdownStyle) {
	if text == "" {
		return
	}
	if len(*spans) > 0 {
		last := &(*spans)[len(*spans)-1]
		if last.style == style {
			last.text += text
			return
		}
	}
	*spans = append(*spans, markdownSpan{text: text, style: style})
}

func markdownSpansWidth(spans []markdownSpan) int {
	width := 0
	for _, span := range spans {
		width += runewidth.StringWidth(span.text)
	}
	return width
}

func markdownLine(spans []markdownSpan) renderLine {
	var text strings.Builder
	for _, span := range spans {
		text.WriteString(span.text)
	}
	return renderLine{kind: messageAssistant, text: text.String(), spans: spans}
}

func lineIsEmpty(line renderLine) bool {
	return strings.TrimSpace(line.text) == ""
}

func renderMarkdownSpans(spans []markdownSpan) string {
	var out strings.Builder
	for _, span := range spans {
		if span.text != "" {
			out.WriteString(styleForMarkdown(span.style).Render(span.text))
		}
	}
	return out.String()
}

// Code syntax highlighting.
//
// Intentionally small: a line scanner with no external lexer dependency. It
// understands the constructs that dominate agent output (Go first, plus the
// usual C-like family), and keeps everything else deep gray.

var codeKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "class": true, "const": true,
	"continue": true, "default": true, "defer": true, "def": true, "else": true,
	"elif": true, "enum": true, "fallthrough": true, "for": true, "from": true,
	"func": true, "function": true, "go": true, "goto": true, "if": true,
	"import": true, "interface": true, "iota": true, "let": true, "map": true,
	"nil": true, "package": true, "range": true, "return": true, "select": true,
	"struct": true, "switch": true, "true": true, "type": true, "false": true,
	"var": true, "while": true,
}

var codeBuiltinTypes = map[string]bool{
	"any": true, "bool": true, "byte": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true, "int": true, "int8": true,
	"int16": true, "int32": true, "int64": true, "rune": true, "string": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true,
}

func highlightCodeLine(line, language string) []markdownSpan {
	if line == "" {
		return nil
	}
	language = strings.ToLower(strings.TrimSpace(language))
	var spans []markdownSpan
	appendSpan := func(text string, style markdownStyle) {
		if text != "" {
			spans = append(spans, markdownSpan{text: text, style: style})
		}
	}

	prevSignificant := ""
	for i := 0; i < len(line); {
		r, size := utf8.DecodeRuneInString(line[i:])
		if unicode.IsSpace(r) {
			start := i
			for i < len(line) {
				r, size = utf8.DecodeRuneInString(line[i:])
				if !unicode.IsSpace(r) {
					break
				}
				i += size
			}
			appendSpan(line[start:i], markdownCode)
			continue
		}

		if strings.HasPrefix(line[i:], "//") || (hashCommentLanguage(language) && r == '#') {
			appendSpan(line[i:], markdownCodeComment)
			break
		}
		if strings.HasPrefix(line[i:], "/*") {
			end := strings.Index(line[i+2:], "*/")
			if end < 0 {
				appendSpan(line[i:], markdownCodeComment)
				break
			}
			appendSpan(line[i:i+2+end+2], markdownCodeComment)
			i += 2 + end + 2
			continue
		}
		if end, ok := codeStringEnd(line, i, language); ok {
			appendSpan(line[i:end], markdownCodeString)
			i = end
			continue
		}
		if r >= '0' && r <= '9' {
			end := codeNumberEnd(line, i)
			appendSpan(line[i:end], markdownCodeNumber)
			i = end
			continue
		}
		if isCodeIdentStart(r) {
			start := i
			i += size
			for i < len(line) {
				r, size = utf8.DecodeRuneInString(line[i:])
				if !isCodeIdentPart(r) {
					break
				}
				i += size
			}
			word := line[start:i]
			style := markdownCode
			switch {
			case codeKeywords[word]:
				style = markdownCodeKeyword
			case codeBuiltinTypes[word]:
				style = markdownCodeType
			case codeTypeContext(prevSignificant, language) && startsUppercase(word):
				style = markdownCodeType
			case codeFunctionCall(line, i, word):
				style = markdownCodeFunction
			}
			appendSpan(word, style)
			prevSignificant = word
			continue
		}
		if end := codeOperatorEnd(line, i); end > i {
			appendSpan(line[i:end], markdownCodeOperator)
			prevSignificant = line[i:end]
			i = end
			continue
		}
		appendSpan(string(r), markdownCode)
		prevSignificant = string(r)
		i += size
	}
	return mergeMarkdownSpans(spans)
}

func hashCommentLanguage(language string) bool {
	switch language {
	case "python", "py", "sh", "bash", "zsh", "yaml", "yml", "toml", "ruby", "rb", "conf", "ini":
		return true
	default:
		return false
	}
}

func codeStringEnd(line string, start int, language string) (int, bool) {
	quote := line[start]
	if quote == '`' {
		if end := strings.IndexByte(line[start+1:], '`'); end >= 0 {
			return start + 1 + end + 1, true
		}
		return len(line), true
	}
	if quote != '"' && quote != '\'' {
		return 0, false
	}
	if quote == '\'' && !singleQuoteStringLanguage(language) {
		return 0, false
	}
	for i := start + 1; i < len(line); i++ {
		if line[i] == '\\' {
			i++
			continue
		}
		if line[i] == quote {
			return i + 1, true
		}
	}
	return len(line), true
}

func singleQuoteStringLanguage(language string) bool {
	switch language {
	case "go", "python", "py", "js", "javascript", "ts", "typescript", "sh", "bash", "zsh", "ruby", "rb", "yaml", "yml":
		return true
	default:
		return false
	}
}

func codeNumberEnd(line string, start int) int {
	i := start
	for i < len(line) {
		r, size := utf8.DecodeRuneInString(line[i:])
		if !isCodeNumberPart(r) {
			break
		}
		i += size
	}
	return i
}

func isCodeNumberPart(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') ||
		r == 'x' || r == 'X' || r == 'o' || r == 'O' || r == 'b' || r == 'B' || r == '.' || r == '_'
}

func isCodeIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isCodeIdentPart(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func codeTypeContext(prev, language string) bool {
	if language != "" && language != "go" && language != "golang" {
		return false
	}
	switch prev {
	case "type", "*", "&":
		return true
	default:
		return false
	}
}

func startsUppercase(word string) bool {
	r, _ := utf8.DecodeRuneInString(word)
	return r >= 'A' && r <= 'Z'
}

func codeFunctionCall(line string, end int, word string) bool {
	if codeKeywords[word] || codeBuiltinTypes[word] {
		return false
	}
	for i := end; i < len(line); i++ {
		switch line[i] {
		case ' ', '\t':
			continue
		case '(':
			return true
		default:
			return false
		}
	}
	return false
}

func codeOperatorEnd(line string, start int) int {
	operators := "=+-*/%<>!&|^~:"
	if start+1 < len(line) && strings.ContainsRune(operators, rune(line[start])) &&
		strings.ContainsRune(operators, rune(line[start+1])) {
		return start + 2
	}
	if strings.ContainsRune(operators, rune(line[start])) {
		return start + 1
	}
	return start
}
