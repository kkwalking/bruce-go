package tui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bruce-go/internal/config"
	"bruce-go/internal/integrated"
	"bruce-go/internal/llm"
)

// wizardRuntime builds a runtime backed by a settings file the test can read
// back, with no ambient provider environment.
func wizardRuntime(t *testing.T, providers string) (*Model, string) {
	t.Helper()
	for _, name := range []string{config.DeepSeekAPIKeyEnv, config.GLMAPIKeyEnv, config.KimiAPIKeyEnv} {
		t.Setenv(name, "")
	}
	home := t.TempDir()
	settingsPath := filepath.Join(home, "setting.json")
	if err := os.WriteFile(settingsPath, []byte(providers), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := newRuntimeWithSettings(t, settingsPath)
	model := NewModel(context.Background(), rt)
	t.Cleanup(func() { _ = rt.Close() })
	return model, settingsPath
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func typeText(m *Model, text string) {
	for _, r := range text {
		m.handleProviderWizardKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// stepThroughToProbe walks the wizard from the first step to the probe, typing
// the supplied values. It stops before starting the probe.
func stepThroughToProbe(t *testing.T, m *Model, name, baseURL, apiKey string) {
	t.Helper()
	m.openProviderWizard()
	if m.wizard == nil {
		t.Fatal("the wizard should be open")
	}
	typeText(m, name)
	m.handleProviderWizardKey(keyMsg("enter")) // -> protocol
	m.handleProviderWizardKey(keyMsg("enter")) // protocol default -> base URL
	typeText(m, baseURL)
	m.handleProviderWizardKey(keyMsg("enter")) // -> api key
	typeText(m, apiKey)
	m.handleProviderWizardKey(keyMsg("enter")) // -> probe (and starts it)
}

func TestProviderWizardAppearsWhenNoProviderIsConfigured(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	if m.wizard == nil {
		t.Fatal("the wizard should open automatically on first run")
	}
	if m.wizard.step != wizardName {
		t.Fatalf("wizard step = %v, want the name step", m.wizard.step)
	}
}

func TestProviderWizardDoesNotOpenWhenConfigured(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{"alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`)
	if m.wizard != nil {
		t.Fatal("the wizard should not open when a provider is configured")
	}
}

// The API key must never appear in rendered output, including the confirmation
// screen.
func TestProviderWizardMasksTheAPIKeyOnScreen(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	stepThroughToProbe(t, m, "mygateway", "http://127.0.0.1:3425/v1", "sk-super-secret-key")

	// Walk to the confirmation step, skipping the probe result.
	m.wizard.probing = false
	m.wizard.models = []discoveredModel{{ID: "group/flash", Selected: true}}
	m.wizard.next() // probe -> models
	m.wizard.next() // models -> confirm
	if m.wizard.step != wizardConfirm {
		t.Fatalf("step = %v, want confirm", m.wizard.step)
	}
	rendered := strings.Join(m.providerWizardLines(70), "\n")
	if strings.Contains(rendered, "sk-super-secret-key") || strings.Contains(rendered, "super-secret") {
		t.Fatalf("the confirmation screen leaked the key:\n%s", rendered)
	}
	// The last four characters are the deliberate hint that a key is set.
	if !strings.Contains(rendered, "****-key") {
		t.Fatalf("the confirmation should show a masked key:\n%s", rendered)
	}
}

// Typing into the key field shows only the tail.
func TestProviderWizardKeyFieldIsMaskedWhileTyping(t *testing.T) {
	field := newTextField()
	field.masked = true
	field.set("sk-abcdefgh")
	if got := field.display(); strings.Contains(got, "abcdefgh") {
		t.Fatalf("display = %q, the key is visible", got)
	} else if !strings.HasSuffix(got, "efgh") {
		t.Fatalf("display = %q, want the last four characters", got)
	}
	// A short key has no safe prefix and is fully masked.
	field.set("abc")
	if got := field.display(); got != "***" {
		t.Fatalf("display = %q, want a full mask", got)
	}
}

// The wizard must refuse to advance with a missing or malformed value, and must
// say why.
func TestProviderWizardValidatesEachStep(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	wizard := m.wizard

	// An empty name cannot be left.
	m.handleProviderWizardKey(keyMsg("enter"))
	if wizard.step != wizardName || wizard.err == "" {
		t.Fatalf("an empty name should be refused: step=%v err=%q", wizard.step, wizard.err)
	}
	// An invalid name cannot be left either.
	typeText(m, "Bad Name")
	m.handleProviderWizardKey(keyMsg("enter"))
	if wizard.step != wizardName || wizard.err == "" {
		t.Fatalf("an invalid name should be refused: step=%v err=%q", wizard.step, wizard.err)
	}
	// Correcting it advances.
	for range "Bad Name" {
		m.handleProviderWizardKey(keyMsg("backspace"))
	}
	typeText(m, "mygateway")
	m.handleProviderWizardKey(keyMsg("enter"))
	if wizard.step != wizardProtocol {
		t.Fatalf("step = %v, want the protocol step after a valid name", wizard.step)
	}
	// A base URL without a scheme is refused.
	m.handleProviderWizardKey(keyMsg("enter"))
	typeText(m, "127.0.0.1:3425")
	m.handleProviderWizardKey(keyMsg("enter"))
	if wizard.step != wizardBaseURL || wizard.err == "" {
		t.Fatalf("a scheme-less base URL should be refused: step=%v err=%q", wizard.step, wizard.err)
	}
	for range "127.0.0.1:3425" {
		m.handleProviderWizardKey(keyMsg("backspace"))
	}
	typeText(m, "http://127.0.0.1:3425/v1")
	m.handleProviderWizardKey(keyMsg("enter"))
	if wizard.step != wizardAPIKey {
		t.Fatalf("step = %v, want the API key step", wizard.step)
	}
	// An empty key is refused.
	m.handleProviderWizardKey(keyMsg("enter"))
	if wizard.step != wizardAPIKey || wizard.err == "" {
		t.Fatalf("an empty API key should be refused: step=%v err=%q", wizard.step, wizard.err)
	}
}

func TestProviderWizardProtocolSelection(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	wizard := m.wizard
	typeText(m, "mygateway")
	m.handleProviderWizardKey(keyMsg("enter"))
	if wizard.step != wizardProtocol {
		t.Fatalf("step = %v", wizard.step)
	}
	if got := protocolChoices()[wizard.protocol]; got != config.ProtocolOpenAIChat {
		t.Fatalf("default protocol = %q, want openai_chat", got)
	}
	m.handleProviderWizardKey(keyMsg("right"))
	if got := protocolChoices()[wizard.protocol]; got != config.ProtocolOpenAIResponses {
		t.Fatalf("protocol after right = %q", got)
	}
	m.handleProviderWizardKey(keyMsg("right"))
	if got := protocolChoices()[wizard.protocol]; got != config.ProtocolAnthropic {
		t.Fatalf("protocol after right = %q", got)
	}
	// The selection is bounded at both ends.
	m.handleProviderWizardKey(keyMsg("right"))
	if got := protocolChoices()[wizard.protocol]; got != config.ProtocolAnthropic {
		t.Fatalf("protocol should not run past the end: %q", got)
	}
	m.handleProviderWizardKey(keyMsg("left"))
	m.handleProviderWizardKey(keyMsg("left"))
	m.handleProviderWizardKey(keyMsg("left"))
	if got := protocolChoices()[wizard.protocol]; got != config.ProtocolOpenAIChat {
		t.Fatalf("protocol should not run past the start: %q", got)
	}
}

// A name that implies a different protocol is called out, so the user knows
// what leaving the protocol unset would have meant. The selection itself is not
// changed: the wizard always saves an explicit value.
func TestProviderWizardShowsInferredProtocol(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	typeText(m, "myclaude")
	m.handleProviderWizardKey(keyMsg("enter"))
	rendered := strings.Join(m.providerWizardLines(70), "\n")
	if !strings.Contains(rendered, "would infer anthropic") {
		t.Fatalf("the inference hint should appear:\n%s", rendered)
	}
	if got := protocolChoices()[m.wizard.protocol]; got != config.ProtocolOpenAIChat {
		t.Fatalf("protocol = %q, want the explicit default until the user chooses", got)
	}

	// A name that agrees with the selection has nothing to call out.
	m2, _ := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	typeText(m2, "mygateway")
	m2.handleProviderWizardKey(keyMsg("enter"))
	if rendered := strings.Join(m2.providerWizardLines(70), "\n"); strings.Contains(rendered, "would infer") {
		t.Fatalf("no hint is needed when the inference agrees:\n%s", rendered)
	}
}

// A late probe result from a canceled probe must not be applied.
func TestProviderWizardDiscardsStaleProbeResults(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	stepThroughToProbe(t, m, "mygateway", "http://127.0.0.1:3425/v1", "magpie")
	wizard := m.wizard
	staleSeq := wizard.probeSeq - 1

	handled, _ := m.handleProviderWizardMsg(providerProbeMsg{
		seq:    staleSeq,
		models: []llm.DiscoveredModel{{ID: "stale/model"}},
	})
	if !handled {
		t.Fatal("the message should be recognized as a wizard message")
	}
	if len(wizard.models) != 0 {
		t.Fatalf("a stale result was applied: %#v", wizard.models)
	}

	// The current sequence is applied.
	handled, _ = m.handleProviderWizardMsg(providerProbeMsg{
		seq:    wizard.probeSeq,
		models: []llm.DiscoveredModel{{ID: "group/flash", ContextWindow: 128000, MaxOutputTokens: 8192}},
	})
	if !handled || len(wizard.models) != 1 {
		t.Fatalf("the current result should be applied: %#v", wizard.models)
	}
	if wizard.step != wizardModels {
		t.Fatalf("step = %v, want the model list after a successful probe", wizard.step)
	}
	if !wizard.models[0].Selected {
		t.Fatal("discovered models should be selected by default")
	}
}

// Esc closes the wizard and cancels the in-flight probe.
func TestProviderWizardEscapeClosesAndCancels(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	stepThroughToProbe(t, m, "mygateway", "http://127.0.0.1:3425/v1", "magpie")
	wizard := m.wizard
	seq := wizard.probeSeq
	if wizard.cancel == nil {
		t.Fatal("starting the probe should install a cancel function")
	}
	m.handleProviderWizardKey(keyMsg("esc"))
	if m.wizard != nil {
		t.Fatal("Esc at the probe step should close the wizard")
	}
	if wizard.probeSeq == seq {
		t.Fatal("closing should invalidate the in-flight probe")
	}
}

// Esc steps back rather than closing once the wizard is past the first step.
func TestProviderWizardEscapeStepsBack(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	wizard := m.wizard
	typeText(m, "mygateway")
	m.handleProviderWizardKey(keyMsg("enter"))
	m.handleProviderWizardKey(keyMsg("enter"))
	if wizard.step != wizardBaseURL {
		t.Fatalf("step = %v", wizard.step)
	}
	m.handleProviderWizardKey(keyMsg("esc"))
	if wizard.step != wizardProtocol {
		t.Fatalf("Esc should go back a step, got %v", wizard.step)
	}
	if m.wizard == nil {
		t.Fatal("Esc should not close before the first step")
	}
}

// A failed probe keeps the wizard open and offers the manual path.
func TestProviderWizardProbeFailureFallsBackToManualEntry(t *testing.T) {
	m, settingsPath := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	stepThroughToProbe(t, m, "mygateway", "http://127.0.0.1:65000/v1", "magpie")
	wizard := m.wizard
	handled, _ := m.handleProviderWizardMsg(providerProbeMsg{seq: wizard.probeSeq, err: errors.New("connection refused")})
	if !handled {
		t.Fatal("the failure should be handled by the wizard")
	}
	if wizard.step != wizardProbe {
		t.Fatalf("a failed probe should stay on the probe step, got %v", wizard.step)
	}
	if wizard.probeErr == "" {
		t.Fatal("the failure should be recorded")
	}
	rendered := strings.Join(m.providerWizardLines(70), "\n")
	if !strings.Contains(rendered, "connection refused") {
		t.Fatalf("the failure should be shown:\n%s", rendered)
	}

	// Continue to the model step and type names by hand.
	m.handleProviderWizardKey(keyMsg("enter"))
	if wizard.step != wizardModels {
		t.Fatalf("step = %v, want the model step", wizard.step)
	}
	typeText(m, "group/flash, group/think")
	if got := wizard.typedModels(); len(got) != 2 || got[0] != "group/flash" || got[1] != "group/think" {
		t.Fatalf("typed models = %#v", got)
	}
	m.handleProviderWizardKey(keyMsg("enter")) // -> confirm
	if wizard.step != wizardConfirm {
		t.Fatalf("step = %v, want confirm", wizard.step)
	}
	// Saving writes what was typed.
	cmd := m.handleProviderWizardKey(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("saving should return a command")
	}
	msg := cmd()
	if _, ok := msg.(providerSavedMsg); !ok {
		t.Fatalf("save returned %#v, want providerSavedMsg", msg)
	}
	settings, err := config.NewLoader(settingsPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	saved := settings.LLM.Providers["mygateway"]
	if len(saved.Models) != 2 {
		t.Fatalf("saved models = %#v, want the two typed names", saved.Models)
	}
	if saved.APIKey != "magpie" {
		t.Fatalf("saved API key = %q", saved.APIKey)
	}
}

// A full happy path: the wizard saves a provider, the runtime adopts it, and
// the discovered capabilities are persisted so compaction can use them.
func TestProviderWizardSavesDiscoveredProvider(t *testing.T) {
	m, settingsPath := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	stepThroughToProbe(t, m, "mygateway", "http://127.0.0.1:3425/v1", "magpie")
	wizard := m.wizard
	m.handleProviderWizardMsg(providerProbeMsg{seq: wizard.probeSeq, models: []llm.DiscoveredModel{
		{ID: "group/flash", ContextWindow: 128000, MaxOutputTokens: 8192},
		{ID: "group/think", ContextWindow: 200000, MaxOutputTokens: 64000},
	}})
	if wizard.step != wizardModels {
		t.Fatalf("step = %v", wizard.step)
	}
	// Deselect the second model.
	m.handleProviderWizardKey(keyMsg("down"))
	m.handleProviderWizardKey(keyMsg(" "))
	m.handleProviderWizardKey(keyMsg("enter"))
	if wizard.step != wizardConfirm {
		t.Fatalf("step = %v, want confirm", wizard.step)
	}
	cmd := m.handleProviderWizardKey(keyMsg("enter"))
	msg := cmd()
	saved, ok := msg.(providerSavedMsg)
	if !ok {
		t.Fatalf("save returned %#v", msg)
	}
	if saved.name != "mygateway" || len(saved.models) != 1 || saved.models[0] != "group/flash" {
		t.Fatalf("saved = %+v, want only the selected model", saved)
	}

	settings, err := config.NewLoader(settingsPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	provider := settings.LLM.Providers["mygateway"]
	if len(provider.Models) != 1 || provider.Models[0] != "group/flash" {
		t.Fatalf("persisted models = %#v", provider.Models)
	}
	if provider.Protocol != config.ProtocolOpenAIChat {
		t.Fatalf("persisted protocol = %q", provider.Protocol)
	}
	capability := provider.ModelCapabilities["group/flash"]
	if capability.ContextWindow != 128000 {
		t.Fatalf("capability = %+v, want the discovered context window", capability)
	}
	if settings.LLM.DefaultProvider != "mygateway" || settings.LLM.DefaultModel != "group/flash" {
		t.Fatalf("defaults = %s/%s", settings.LLM.DefaultProvider, settings.LLM.DefaultModel)
	}
	// The runtime adopted it without a restart.
	if m.runtime.NeedsProviderSetup() {
		t.Fatal("the runtime should be configured now")
	}
	if got := m.runtime.CurrentModel(); got.Provider != "mygateway" {
		t.Fatalf("current model = %s", got.Selector())
	}
}

// Editing keeps the stored key when the field is left empty: the wizard cannot
// show the existing key, so requiring a re-type would make editing a URL a
// credential-rotation event.
func TestProviderWizardEditKeepsStoredKey(t *testing.T) {
	providers := `{"llm":{"providers":{"alpha":{"apiKey":"original-key","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`
	m, settingsPath := wizardRuntime(t, providers)
	if err := m.openProviderEditor("alpha"); err != nil {
		t.Fatal(err)
	}
	wizard := m.wizard
	if wizard == nil || wizard.mode != wizardEdit {
		t.Fatalf("wizard = %+v, want an edit wizard", wizard)
	}
	if wizard.step != wizardProtocol {
		t.Fatalf("an edit should start at the protocol step, got %v", wizard.step)
	}
	if wizard.baseURL.value() != "http://127.0.0.1:1/v1" {
		t.Fatalf("base URL = %q, want the stored value prefilled", wizard.baseURL.value())
	}
	// Change the base URL, leave the key blank, and finish.
	wizard.baseURL.set("http://127.0.0.1:9/v1")
	wizard.step = wizardModels
	wizard.manualModels.set("a, b")
	wizard.step = wizardConfirm
	cmd := m.handleProviderWizardKey(keyMsg("enter"))
	msg := cmd()
	if _, ok := msg.(providerSavedMsg); !ok {
		t.Fatalf("save returned %#v", msg)
	}
	settings, err := config.NewLoader(settingsPath).Load()
	if err != nil {
		t.Fatal(err)
	}
	provider := settings.LLM.Providers["alpha"]
	if provider.APIKey != "original-key" {
		t.Fatalf("API key = %q, want the stored key preserved", provider.APIKey)
	}
	if provider.BaseURL != "http://127.0.0.1:9/v1" {
		t.Fatalf("base URL = %q", provider.BaseURL)
	}
	if len(provider.Models) != 2 {
		t.Fatalf("models = %#v", provider.Models)
	}
}

// /provider add opens the wizard instead of being sent to the runtime, which
// has no way to prompt.
func TestProviderAddCommandOpensTheWizard(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{"alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`)
	m.replaceInput("/provider add")
	m.submitInput()
	if m.wizard == nil {
		t.Fatal("/provider add should open the wizard")
	}
	if m.wizard.mode != wizardAdd || m.wizard.step != wizardName {
		t.Fatalf("wizard = mode %v step %v", m.wizard.mode, m.wizard.step)
	}
}

func TestProviderEditCommandOpensTheEditor(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{"alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`)
	m.replaceInput("/provider edit alpha")
	m.submitInput()
	if m.wizard == nil {
		t.Fatal("/provider edit should open the wizard")
	}
	if m.wizard.mode != wizardEdit || m.wizard.existing != "alpha" {
		t.Fatalf("wizard = mode %v existing %q", m.wizard.mode, m.wizard.existing)
	}
}

func TestProviderEditUnknownProviderReportsIt(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{"alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`)
	m.replaceInput("/provider edit nope")
	m.submitInput()
	if m.wizard != nil {
		t.Fatal("an unknown provider should not open a wizard")
	}
	if last := m.messages[len(m.messages)-1].text; !strings.Contains(last, "unknown provider") {
		t.Fatalf("the transcript should say why: %q", last)
	}
}

// The other subcommands still go to the runtime.
func TestProviderListStillReachesTheRuntime(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{"alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`)
	if m.handleProviderCommand("/provider") {
		t.Fatal("a plain /provider should not be intercepted by the TUI")
	}
	if m.handleProviderCommand("/provider remove alpha") {
		t.Fatal("/provider remove should not be intercepted by the TUI")
	}
}

// While the wizard is open, keys must not reach the main input line.
func TestProviderWizardOwnsTheKeyboard(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	if m.wizard == nil {
		t.Fatal("the wizard should be open")
	}
	m.Update(keyMsg("x"))
	if m.inputText() != "" {
		t.Fatalf("input = %q, the wizard should have consumed the keystroke", m.inputText())
	}
	if m.wizard.name.value() != "x" {
		t.Fatalf("wizard name = %q, want the keystroke to land in the field", m.wizard.name.value())
	}
	// A click must not toggle anything underneath either.
	before := len(m.messages)
	m.Update(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 5, Y: 5})
	if len(m.messages) != before {
		t.Fatal("a click should not reach the view underneath the wizard")
	}
}

// The rendering must not leak the key and must survive a very small terminal.
func TestProviderWizardRendersAtSmallSizes(t *testing.T) {
	m, _ := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	typeText(m, "mygateway")
	m.handleProviderWizardKey(keyMsg("enter"))
	m.handleProviderWizardKey(keyMsg("enter"))
	typeText(m, "http://127.0.0.1:3425/v1")
	m.handleProviderWizardKey(keyMsg("enter"))
	typeText(m, "sk-secret-value")
	for _, size := range []struct{ columns, rows int }{{80, 24}, {40, 12}, {30, 8}, {200, 60}, {20, 6}} {
		m.width, m.height = size.columns, size.rows
		view := m.View()
		if strings.Contains(view, "sk-secret-value") {
			t.Fatalf("the view leaked the key at %dx%d", size.columns, size.rows)
		}
	}
}

// newRuntimeWithSettings builds a runtime around one settings file. It is a
// helper for the wizard tests, which have to observe what a save wrote.
func newRuntimeWithSettings(t *testing.T, settingsPath string) *integrated.Runtime {
	t.Helper()
	rt, err := integrated.New(context.Background(), integrated.Options{
		Workspace:    t.TempDir(),
		HomeDir:      filepath.Dir(settingsPath),
		SettingsPath: settingsPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// The probe runs on the program's loop, so this test drives the real Model
// through a headless tea.Program and lets the whole flow finish. The evidence
// is the file the flow writes: the model IDs in it can only have come from the
// HTTP endpoint, which proves the asynchronous round trip really completed.
func TestProviderWizardProbeRoundTripThroughProgram(t *testing.T) {
	body := []byte(`{"data":[{"id":"group/flash","context_window":128000,"max_output_tokens":8192}]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer magpie" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	m, settingsPath := wizardRuntime(t, `{"llm":{"providers":{}}}`)
	program := tea.NewProgram(m, tea.WithoutRenderer(), tea.WithInput(nil), tea.WithOutput(io.Discard))
	done := make(chan error, 1)
	go func() {
		_, err := program.Run()
		done <- err
	}()
	t.Cleanup(func() {
		program.Quit()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})

	send := func(text string) {
		for _, r := range text {
			program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}
	send("mygateway")
	program.Send(tea.KeyMsg{Type: tea.KeyEnter}) // protocol
	program.Send(tea.KeyMsg{Type: tea.KeyEnter}) // base URL
	send(server.URL + "/v1")
	program.Send(tea.KeyMsg{Type: tea.KeyEnter}) // API key
	send("magpie")
	program.Send(tea.KeyMsg{Type: tea.KeyEnter}) // starts the probe

	// Enter is a no-op while the probe is in flight, so this nudges the flow
	// along without having to guess when the round trip lands.
	loader := config.NewLoader(settingsPath)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		program.Send(tea.KeyMsg{Type: tea.KeyEnter})
		time.Sleep(25 * time.Millisecond)
		settings, err := loader.Load()
		if err != nil {
			t.Fatal(err)
		}
		if provider, ok := settings.LLM.Providers["mygateway"]; ok {
			if len(provider.Models) != 1 || provider.Models[0] != "group/flash" {
				t.Fatalf("saved models = %#v, want the discovered one", provider.Models)
			}
			if provider.APIKey != "magpie" || provider.BaseURL != server.URL+"/v1" {
				t.Fatalf("saved provider = %+v", provider)
			}
			if provider.ModelCapabilities["group/flash"].ContextWindow != 128000 {
				t.Fatalf("capabilities = %#v, want the discovered window", provider.ModelCapabilities)
			}
			return
		}
	}
	t.Fatalf("the probe never completed; setting.json holds: %s", readFileOrEmpty(settingsPath))
}

func readFileOrEmpty(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "<unreadable: " + err.Error() + ">"
	}
	return string(data)
}
