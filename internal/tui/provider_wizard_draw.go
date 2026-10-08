package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"bruce-go/internal/config"
)

// providerWizardTitle is the title drawn in the modal frame.
const providerWizardTitle = " LLM Provider "

// drawProviderWizard renders the configuration modal on top of the canvas.
// It follows the approval dialog's geometry: centred, inset from the edges, and
// skipped entirely when the terminal is too small to hold it.
func (m *Model) drawProviderWizard(canvas []string, columns, rows int) {
	wizard := m.wizard
	if wizard == nil {
		return
	}
	width := min(columns-4, 78)
	height := min(rows-4, 20)
	if width < 30 || height < 8 {
		return
	}
	contentWidth := width - 4
	left := max(0, (columns-width)/2)
	top := max(0, (rows-height)/2)

	title := providerWizardTitle + wizard.stepName() + " "
	setOverlayRow(canvas, top, columns, left, warnStyle.Render("┌"+title+strings.Repeat("─", max(0, width-2-runewidth.StringWidth(title)))+"┐"))
	lines := m.providerWizardLines(contentWidth)
	for i := 0; i < height-2; i++ {
		text := ""
		if i < len(lines) {
			text = lines[i]
		}
		setOverlayRow(canvas, top+i+1, columns, left, baseStyle.Render("│ "+padRight(text, contentWidth)+" │"))
	}
	setOverlayRow(canvas, top+height-1, columns, left, warnStyle.Render("└"+strings.Repeat("─", max(0, width-2))+"┘"))
}

// stepName labels the frame with the current step, so the user can tell how far
// into the flow they are.
func (w *providerWizard) stepName() string {
	total := len(w.steps)
	index := 1
	for i, step := range w.steps {
		if step == w.step {
			index = i + 1
			break
		}
	}
	switch w.step {
	case wizardName:
		return fmt.Sprintf("Name (%d/%d)", index, total)
	case wizardProtocol:
		return fmt.Sprintf("Protocol (%d/%d)", index, total)
	case wizardBaseURL:
		return fmt.Sprintf("Base URL (%d/%d)", index, total)
	case wizardAPIKey:
		return fmt.Sprintf("API Key (%d/%d)", index, total)
	case wizardProbe:
		return fmt.Sprintf("Connectivity (%d/%d)", index, total)
	case wizardModels:
		return fmt.Sprintf("Models (%d/%d)", index, total)
	default:
		return fmt.Sprintf("Confirm (%d/%d)", index, total)
	}
}

// providerWizardLines renders the body of the current step, already wrapped.
func (m *Model) providerWizardLines(width int) []string {
	wizard := m.wizard
	if wizard == nil {
		return nil
	}
	var lines []string
	switch wizard.step {
	case wizardName:
		lines = append(lines,
			"A short name for this provider. It is used in /model selectors",
			"and as the key in setting.json.",
			"",
			focusField("Name: ", wizard.name.display(), wizard.name.cursorCell()),
		)
	case wizardProtocol:
		lines = append(lines, "Which wire format does this endpoint speak? (←/→)", "")
		for i, choice := range protocolChoices() {
			marker := "  "
			if i == wizard.protocol {
				marker = "> "
			}
			lines = append(lines, marker+choice)
			for _, wrapped := range wrap("    "+protocolDescription(choice), width) {
				lines = append(lines, wrapped)
			}
		}
		// Inference is worth stating when it disagrees with the selection: the
		// wizard always saves an explicit protocol, so the hint tells the user
		// what an empty protocol field would have meant. When the two agree
		// there is nothing to say.
		if name := strings.ToLower(strings.TrimSpace(wizard.name.value())); name != "" {
			inferred := config.ResolveProtocol(name, config.ProviderSetting{})
			if protocolIndex(inferred) != wizard.protocol {
				lines = append(lines, "", "The name \""+name+"\" would infer "+inferred+" if the protocol were left unset.")
			}
		}
	case wizardBaseURL:
		lines = append(lines,
			"The endpoint root, for example https://api.example.com/v1.",
			"The path for the selected protocol is appended automatically.",
			"",
			focusField("Base URL: ", wizard.baseURL.display(), wizard.baseURL.cursorCell()),
		)
	case wizardAPIKey:
		lines = append(lines,
			"The key is stored in setting.json and sent only to the base URL above.",
			"",
			focusField("API Key: ", wizard.apiKey.display(), wizard.apiKey.cursorCell()),
		)
		if wizard.mode == wizardEdit {
			lines = append(lines, "", "Leave empty to keep the stored key.")
		}
	case wizardProbe:
		lines = append(lines, m.probeLines(width)...)
	case wizardModels:
		lines = append(lines, m.modelLines(width)...)
	case wizardConfirm:
		lines = append(lines, m.confirmLines()...)
	}
	if wizard.err != "" {
		lines = append(lines, "", warnStyle.Render("! "+wizard.err))
	}
	if wizard.status != "" {
		lines = append(lines, "", wizard.status)
	}

	var wrapped []string
	for _, line := range lines {
		wrapped = append(wrapped, wrap(line, width)...)
	}
	return wrapped
}

func (m *Model) probeLines(width int) []string {
	wizard := m.wizard
	lines := []string{
		fmt.Sprintf("Testing %s at %s", protocolChoices()[clamp(wizard.protocol, 0, len(protocolChoices())-1)], strings.TrimSpace(wizard.baseURL.value())),
		"",
	}
	switch {
	case wizard.probing:
		lines = append(lines, m.agentStatusText+" Querying the model list...")
	case wizard.probeErr != "":
		lines = append(lines,
			warnStyle.Render("The endpoint could not be reached:"),
			"",
			wizard.probeErr,
			"",
			"Enter to continue and type model names by hand, Esc to go back.",
		)
	default:
		lines = append(lines, "Enter to run the connectivity test and list models.")
	}
	return lines
}

// modelLines renders the selectable model list, windowed around the cursor so a
// long list stays usable in a small terminal.
func (m *Model) modelLines(width int) []string {
	wizard := m.wizard
	if len(wizard.models) == 0 {
		lines := []string{
			"No models were discovered. Type the model names to use,",
			"separated by commas.",
			"",
			focusField("Models: ", wizard.manualModels.display(), wizard.manualModels.cursorCell()),
		}
		return lines
	}
	lines := []string{
		"Space toggles a model, a toggles all. Enter to continue.",
		"",
	}
	// The list occupies whatever is left of the modal after the fixed lines.
	available := 14
	start := 0
	if wizard.modelCursor >= available {
		start = wizard.modelCursor - available + 1
	}
	end := min(len(wizard.models), start+available)
	for i := start; i < end; i++ {
		model := wizard.models[i]
		cursor := "  "
		if i == wizard.modelCursor {
			cursor = "> "
		}
		check := "[ ]"
		if model.Selected {
			check = "[x]"
		}
		line := cursor + check + " " + model.ID
		if model.Advertise.ContextWindow > 0 {
			line += fmt.Sprintf("  (%d ctx", model.Advertise.ContextWindow)
			if model.Advertise.MaxOutputTokens > 0 {
				line += fmt.Sprintf(", %d out", model.Advertise.MaxOutputTokens)
			}
			line += ")"
		}
		lines = append(lines, line)
	}
	if end < len(wizard.models) {
		lines = append(lines, fmt.Sprintf("  ... %d more (scroll with ↑/↓)", len(wizard.models)-end))
	}
	lines = append(lines,
		"",
		focusField("Also add: ", wizard.manualModels.display(), wizard.manualModels.cursorCell()),
	)
	return lines
}

func (m *Model) confirmLines() []string {
	wizard := m.wizard
	setting := wizard.setting()
	name := strings.ToLower(strings.TrimSpace(wizard.name.value()))
	lines := []string{
		"Save this provider?",
		"",
		"Name:     " + name,
		"Protocol: " + setting.Protocol,
		"Base URL: " + setting.BaseURL,
		"API Key:  " + maskKeyForDisplay(setting.APIKey),
		fmt.Sprintf("Models:   %d selected", len(setting.Models)),
	}
	models := setting.Models
	if len(models) > 5 {
		models = append(append([]string(nil), models[:5]...), fmt.Sprintf("... (%d more)", len(setting.Models)-5))
	}
	if len(models) > 0 {
		lines = append(lines, "          "+strings.Join(models, ", "))
	}
	lines = append(lines, "", "Enter to save. Esc to go back.")
	return lines
}

// maskKeyForDisplay hides a key in the confirmation screen. The wizard's own
// field already masks it; this keeps the summary consistent.
func maskKeyForDisplay(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "(unchanged)"
	}
	if len(key) <= 4 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}

// focusField renders a labelled input with a visible cursor cell, so the user
// can see where typing will land.
func focusField(label, value string, cursor int) string {
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("#C0C0C0"))
	runes := []rune(value)
	cursor = clamp(cursor, 0, len(runes))
	before := string(runes[:cursor])
	at := " "
	if cursor < len(runes) {
		at = string(runes[cursor])
	}
	after := ""
	if cursor < len(runes) {
		after = string(runes[cursor+1:])
	}
	return label + before + style.Render(at) + after
}

// providerSavedText is the confirmation shown in the transcript after a save.
func providerSavedText(msg providerSavedMsg) string {
	text := "Provider saved: " + msg.name + fmt.Sprintf(" (%d models)", len(msg.models))
	if msg.activate {
		text += "\nSwitched to it."
	}
	text += "\n\nUse /provider to see the configuration, /model to switch."
	return text
}
