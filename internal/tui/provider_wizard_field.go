package tui

import (
	"strings"
)

// textField is a single-line editable string with a cursor, used by the
// provider wizard. It is deliberately small: the wizard needs masked input for
// API keys and plain input elsewhere, and neither belongs in the main input
// line's editing path.
type textField struct {
	runes  []rune
	cursor int
	// masked hides all but the trailing characters when rendered.
	masked bool
}

func newTextField() textField { return textField{} }

func (f *textField) set(value string) {
	f.runes = []rune(value)
	f.cursor = len(f.runes)
}

func (f *textField) value() string { return string(f.runes) }

func (f *textField) insert(runes []rune) {
	if len(runes) == 0 {
		return
	}
	f.cursor = clamp(f.cursor, 0, len(f.runes))
	next := make([]rune, 0, len(f.runes)+len(runes))
	next = append(next, f.runes[:f.cursor]...)
	next = append(next, runes...)
	next = append(next, f.runes[f.cursor:]...)
	f.runes = next
	f.cursor += len(runes)
}

func (f *textField) backspace() {
	if f.cursor <= 0 || len(f.runes) == 0 {
		return
	}
	f.cursor = clamp(f.cursor, 0, len(f.runes))
	f.runes = append(f.runes[:f.cursor-1], f.runes[f.cursor:]...)
	f.cursor--
}

func (f *textField) deleteAtCursor() {
	if f.cursor < 0 || f.cursor >= len(f.runes) {
		return
	}
	f.runes = append(f.runes[:f.cursor], f.runes[f.cursor+1:]...)
}

func (f *textField) moveLeft() { f.cursor = max(0, f.cursor-1) }
func (f *textField) moveRight() {
	f.cursor = min(len(f.runes), f.cursor+1)
}

// display renders the field for a single-line prompt, masking the value when
// masked is set. Only the trailing four characters are shown: enough to confirm
// which key was pasted, never enough to be used.
func (f textField) display() string {
	if !f.masked {
		return string(f.runes)
	}
	if len(f.runes) == 0 {
		return ""
	}
	if len(f.runes) <= 4 {
		return strings.Repeat("*", len(f.runes))
	}
	return strings.Repeat("*", len(f.runes)-4) + string(f.runes[len(f.runes)-4:])
}

// cursorCell returns the index of the cursor within display(), so the caller can
// place a cursor cell without knowing about masking.
func (f textField) cursorCell() int {
	if !f.masked {
		return f.cursor
	}
	if len(f.runes) == 0 {
		return 0
	}
	// The mask preserves the character count, so the cursor lands in the same
	// place it would unmasked.
	return clamp(f.cursor, 0, len(f.runes))
}
