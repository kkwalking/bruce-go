package tui

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bruce-go/internal/config"
	"bruce-go/internal/integrated"
	"bruce-go/internal/llm"
)

// providerProbeMsg carries the result of an asynchronous connectivity probe
// back into the update loop.
type providerProbeMsg struct {
	seq    int
	models []llm.DiscoveredModel
	err    error
}

// wizardSpinnerMsg advances the probe spinner.
type wizardSpinnerMsg struct{ seq int }

// openProviderWizard starts a fresh add wizard.
func (m *Model) openProviderWizard() {
	wizard := newProviderWizard(wizardAdd)
	// If the runtime has nothing usable, prefill the protocol with whatever
	// the settings suggest so the common case is one keypress away.
	m.wizard = wizard
}

// openProviderEditor loads a configured provider into the wizard.
func (m *Model) openProviderEditor(name string) error {
	summaries, err := m.runtime.ListProviders()
	if err != nil {
		return err
	}
	var summary *integrated.ProviderSummary
	for i, candidate := range summaries {
		if strings.EqualFold(candidate.Name, name) {
			summary = &summaries[i]
			break
		}
	}
	if summary == nil {
		return errors.New("unknown provider: " + name)
	}
	wizard := newProviderWizard(wizardEdit)
	wizard.existing = summary.Name
	wizard.protocol = protocolIndex(summary.Protocol)
	wizard.baseURL.set(summary.BaseURL)
	// The API key is not recoverable from the summary (it is masked), so the
	// field starts empty and an empty value means "keep the stored key".
	wizard.name.set(summary.Name)
	m.wizard = wizard
	return nil
}

// probeProviderCmd runs discovery in the background and reports back with the
// sequence number that was current when it started.
func probeProviderCmd(ctx context.Context, seq int, protocol, baseURL, apiKey string) tea.Cmd {
	return func() tea.Msg {
		models, err := llm.DiscoverModels(ctx, protocol, baseURL, apiKey)
		return providerProbeMsg{seq: seq, models: models, err: err}
	}
}

// spinnerCmd schedules one spinner frame, tagged with the probe it belongs to.
func spinnerCmd(seq int) tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg {
		return wizardSpinnerMsg{seq: seq}
	})
}

// startProbe begins discovery for the current wizard state. The sequence number
// is incremented first, so any in-flight result is discarded on arrival.
func (m *Model) startProbe() tea.Cmd {
	wizard := m.wizard
	if wizard == nil {
		return nil
	}
	if wizard.cancel != nil {
		wizard.cancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	wizard.cancel = cancel
	wizard.probeSeq++
	wizard.probing = true
	wizard.probeErr = ""
	wizard.models = nil
	wizard.modelCursor = 0
	seq := wizard.probeSeq
	protocol := protocolChoices()[clamp(wizard.protocol, 0, len(protocolChoices())-1)]
	baseURL := strings.TrimSpace(wizard.baseURL.value())
	apiKey := strings.TrimSpace(wizard.apiKey.value())
	return tea.Batch(
		probeProviderCmd(ctx, seq, protocol, baseURL, apiKey),
		spinnerCmd(seq),
	)
}

// cancelProbe stops an in-flight probe. The sequence bump makes any result that
// is already on its way a no-op.
func (m *Model) cancelProbe() {
	wizard := m.wizard
	if wizard == nil {
		return
	}
	if wizard.cancel != nil {
		wizard.cancel()
		wizard.cancel = nil
	}
	wizard.probeSeq++
	wizard.probing = false
}

// closeProviderWizard dismisses the modal and cancels any in-flight probe.
func (m *Model) closeProviderWizard() {
	m.cancelProbe()
	m.wizard = nil
}

// handleProviderWizardMsg applies an asynchronous probe result. It reports
// whether the message belonged to the wizard, so Update can leave other
// messages alone.
func (m *Model) handleProviderWizardMsg(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case providerProbeMsg:
		wizard := m.wizard
		if wizard == nil || msg.seq != wizard.probeSeq {
			// A late result from a canceled or superseded probe.
			return true, nil
		}
		wizard.probing = false
		if wizard.cancel != nil {
			wizard.cancel()
			wizard.cancel = nil
		}
		if msg.err != nil {
			wizard.probeErr = msg.err.Error()
			return true, nil
		}
		wizard.models = make([]discoveredModel, 0, len(msg.models))
		for _, model := range msg.models {
			wizard.models = append(wizard.models, discoveredModel{ID: model.ID, Selected: true, Advertise: model})
		}
		// The probe succeeded, so there is nothing left to correct; move on to
		// choosing which models to keep.
		if len(wizard.models) > 0 && wizard.step == wizardProbe {
			wizard.next()
		}
		return true, nil
	case wizardSpinnerMsg:
		wizard := m.wizard
		if wizard == nil || !wizard.probing || msg.seq != wizard.probeSeq {
			return true, nil
		}
		return true, spinnerCmd(msg.seq)
	}
	return false, nil
}

// handleProviderWizardKey owns the keyboard while the wizard is open.
func (m *Model) handleProviderWizardKey(msg tea.KeyMsg) tea.Cmd {
	wizard := m.wizard
	if wizard == nil {
		return nil
	}
	switch msg.String() {
	case "ctrl+c":
		m.closeProviderWizard()
		return nil
	case "esc":
		// Esc steps back; at the first step it closes the wizard. A running
		// probe is canceled on the way out either way.
		m.cancelProbe()
		if wizard.step == wizardProbe || !wizard.back() {
			m.closeProviderWizard()
		}
		return nil
	case "tab":
		return m.advanceProviderWizard()
	case "shift+tab":
		wizard.back()
		return nil
	case "up":
		if wizard.step == wizardModels && wizard.modelCursor > 0 {
			wizard.modelCursor--
		}
		return nil
	case "down":
		if wizard.step == wizardModels && wizard.modelCursor < len(wizard.models)-1 {
			wizard.modelCursor++
		}
		return nil
	case "left":
		if wizard.step == wizardProtocol {
			wizard.protocol = max(0, wizard.protocol-1)
			return nil
		}
		if field := wizard.currentField(); field != nil {
			field.moveLeft()
		}
		return nil
	case "right":
		if wizard.step == wizardProtocol {
			wizard.protocol = min(len(protocolChoices())-1, wizard.protocol+1)
			return nil
		}
		if field := wizard.currentField(); field != nil {
			field.moveRight()
		}
		return nil
	case "backspace":
		if field := wizard.currentField(); field != nil {
			field.backspace()
		}
		return nil
	case "delete":
		if field := wizard.currentField(); field != nil {
			field.deleteAtCursor()
		}
		return nil
	case "enter":
		return m.advanceProviderWizard()
	}
	if len(msg.Runes) == 0 {
		return nil
	}
	// A space toggles a model rather than typing into the manual list: the
	// selection list is the common case, and the manual field accepts the
	// other separators.
	if wizard.step == wizardModels && len(wizard.models) > 0 {
		switch msg.String() {
		case " ":
			wizard.models[wizard.modelCursor].Selected = !wizard.models[wizard.modelCursor].Selected
			return nil
		case "a":
			allSelected := true
			for _, model := range wizard.models {
				if !model.Selected {
					allSelected = false
					break
				}
			}
			for i := range wizard.models {
				wizard.models[i].Selected = !allSelected
			}
			return nil
		}
	}
	if field := wizard.currentField(); field != nil {
		field.insert(msg.Runes)
	}
	return nil
}

// advanceProviderWizard handles Enter/Tab: it validates the current step and
// moves on, starting the probe when that step is reached and saving at the end.
func (m *Model) advanceProviderWizard() tea.Cmd {
	wizard := m.wizard
	if wizard == nil {
		return nil
	}
	if message := wizard.validationError(); message != "" {
		wizard.err = message
		return nil
	}
	// A probe in flight has no result to act on yet; Enter would otherwise
	// advance to an empty model list while the answer is still coming.
	if wizard.step == wizardProbe && wizard.probing {
		return nil
	}
	// Entering the probe step is what triggers the probe, so that stepping back
	// and forward again re-runs it rather than reusing a stale result.
	if wizard.step == wizardAPIKey {
		wizard.next()
		return m.startProbe()
	}
	if wizard.step == wizardConfirm {
		return m.saveProviderWizard()
	}
	wizard.next()
	return nil
}

// saveProviderWizard persists the configured provider and closes the modal.
func (m *Model) saveProviderWizard() tea.Cmd {
	wizard := m.wizard
	if wizard == nil {
		return nil
	}
	name := strings.ToLower(strings.TrimSpace(wizard.name.value()))
	setting := wizard.setting()
	setting.ModelCapabilities = wizard.capabilityMap()
	// An edit that leaves the key blank keeps the stored one: the wizard cannot
	// show the existing key, so requiring a re-type would make editing a
	// base URL force a new credential.
	if wizard.mode == wizardEdit && setting.APIKey == "" {
		if existing, err := m.existingSetting(name); err == nil {
			setting.APIKey = existing.APIKey
		}
	}
	activate := wizard.mode == wizardAdd
	rt := m.runtime
	m.closeProviderWizard()
	return func() tea.Msg {
		if err := rt.SaveProvider(name, setting, activate); err != nil {
			return providerSaveFailedMsg{name: name, err: err}
		}
		return providerSavedMsg{name: name, models: setting.Models, activate: activate}
	}
}

// existingSetting reads one provider entry from disk, used to carry the stored
// API key across an edit.
func (m *Model) existingSetting(name string) (config.ProviderSetting, error) {
	settings, err := m.runtime.Loader.Load()
	if err != nil {
		return config.ProviderSetting{}, err
	}
	setting, ok := settings.LLM.Providers[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return config.ProviderSetting{}, errors.New("unknown provider: " + name)
	}
	return setting, nil
}

// providerSavedMsg reports a successful save back on the update loop.
type providerSavedMsg struct {
	name     string
	models   []string
	activate bool
}

// providerSaveFailedMsg reports a failed save. The wizard is already closed, so
// the failure is reported as a message rather than as a form error.
type providerSaveFailedMsg struct {
	name string
	err  error
}

// sortedModelIDs returns the discovered IDs in a stable order for display.
