package tui

import "regexp"

type inputStyle int

const (
	styleNormal inputStyle = iota
	styleCommand
	styleMention
	styleImage
	styleDanger
	styleSecret
)

type styledSpan struct {
	Text  string
	Style inputStyle
}

var (
	slashCommandPattern = regexp.MustCompile(`^[ \t]*/\S*`)
	imagePattern        = regexp.MustCompile(`@(image|img):(<[^>]*>|\S*)|@clipboard(?:\b|$)`)
	atReferencePattern  = regexp.MustCompile(`@[[:alnum:]_./:~${}<>-]+`)
	dangerPattern       = regexp.MustCompile(`(?i)\b(sudo|mkfs|shutdown|reboot|halt|poweroff)\b|\brm\s+-[a-z]*r[a-z]*f[a-z]*\s+(/|~|\$home)|\bcurl\b[^|\n]*\|\s*(sh|bash|zsh|fish|ksh)\b|\bwget\b[^|\n]*\|\s*(sh|bash|zsh|fish|ksh)\b|\bdd\b[^\n]*\bof=/dev/`)
	secretPattern       = regexp.MustCompile(`(?i)\b(api[_-]?key|token|password|secret|authorization|bearer)\b`)
)

func highlightInput(input string) []styledSpan {
	runes := []rune(input)
	if len(runes) == 0 {
		return nil
	}
	styles := make([]inputStyle, len(runes))
	applyStyle(styles, input, atReferencePattern, styleMention)
	applyStyle(styles, input, imagePattern, styleImage)
	applyStyle(styles, input, secretPattern, styleSecret)
	applyStyle(styles, input, dangerPattern, styleDanger)
	applyStyle(styles, input, slashCommandPattern, styleCommand)

	var out []styledSpan
	current := styles[0]
	buf := []rune{runes[0]}
	for i := 1; i < len(runes); i++ {
		if styles[i] != current {
			out = append(out, styledSpan{Text: string(buf), Style: current})
			buf = buf[:0]
			current = styles[i]
		}
		buf = append(buf, runes[i])
	}
	out = append(out, styledSpan{Text: string(buf), Style: current})
	return out
}

func applyStyle(styles []inputStyle, input string, pattern *regexp.Regexp, style inputStyle) {
	byteToRune := make([]int, len(input)+1)
	runeIndex := 0
	lastByte := 0
	for byteIndex, r := range input {
		for i := lastByte; i <= byteIndex; i++ {
			byteToRune[i] = runeIndex
		}
		lastByte = byteIndex + len(string(r))
		runeIndex++
	}
	for i := lastByte; i <= len(input); i++ {
		byteToRune[i] = runeIndex
	}
	byteToRune[len(input)] = runeIndex
	for _, match := range pattern.FindAllStringIndex(input, -1) {
		start := byteToRune[match[0]]
		end := byteToRune[match[1]]
		if start < 0 {
			start = 0
		}
		if end > len(styles) {
			end = len(styles)
		}
		for i := start; i < end; i++ {
			styles[i] = style
		}
	}
}
