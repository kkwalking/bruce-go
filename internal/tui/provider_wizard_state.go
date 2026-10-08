package tui

import (
	"context"
	"net"
	"net/url"
	"strings"

	"bruce-go/internal/config"
	"bruce-go/internal/llm"
)

// isLoopbackURL reports whether a base URL names the local machine, where
// plain http is acceptable because the traffic never reaches the network.
func isLoopbackURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	name := strings.ToLower(parsed.Hostname())
	if name == "localhost" {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// wizardStep is one screen of the provider configuration wizard.
type wizardStep int

const (
	// wizardName asks for the provider name. It is first because the name
	// decides a lot that follows, including the protocol inference.
	wizardName wizardStep = iota
	// wizardProtocol picks the wire protocol.
	wizardProtocol
	// wizardBaseURL asks for the endpoint.
	wizardBaseURL
	// wizardAPIKey asks for the credential.
	wizardAPIKey
	// wizardProbe runs the connectivity test and model discovery.
	wizardProbe
	// wizardModels picks which discovered models to keep.
	wizardModels
	// wizardConfirm saves.
	wizardConfirm
)

// wizardMode distinguishes adding a provider from editing an existing one.
type wizardMode int

const (
	wizardAdd wizardMode = iota
	wizardEdit
)

// discoveredModel is one selectable row of the model list.
type discoveredModel struct {
	ID        string
	Selected  bool
	Advertise llm.DiscoveredModel
}

// providerWizard is the interactive provider editor.
//
// It is a modal in the same sense as the approval dialog: it owns the keyboard
// while open, and it is drawn last so it covers everything else. Unlike the
// approval dialog it spans several steps, so it also owns a request sequence
// number and a cancel function for the asynchronous probe.
type providerWizard struct {
	mode  wizardMode
	step  wizardStep
	steps []wizardStep

	name     textField
	baseURL  textField
	apiKey   textField
	protocol int // index into llm/config protocol list

	// probe state. The sequence number is compared before a result is
	// accepted, so a result that arrives after the user backed out or started
	// another probe is discarded rather than applied.
	probing  bool
	probeSeq int
	cancel   context.CancelFunc
	probeErr string

	models       []discoveredModel
	modelCursor  int
	manualModels textField

	err      string
	status   string
	existing string // provider name being edited, empty when adding
}

// protocolChoices is the protocol list offered by the wizard. Chat Completions
// is first because it is what the overwhelming majority of gateways speak.
func protocolChoices() []string {
	return []string{
		config.ProtocolOpenAIChat,
		config.ProtocolOpenAIResponses,
		config.ProtocolAnthropic,
	}
}

func protocolDescription(protocol string) string {
	switch protocol {
	case config.ProtocolOpenAIResponses:
		return "OpenAI Responses API (/responses with input items)"
	case config.ProtocolAnthropic:
		return "Anthropic Messages API (/v1/messages with content blocks)"
	default:
		return "OpenAI Chat Completions (/chat/completions), also used by most compatible gateways"
	}
}

func protocolIndex(protocol string) int {
	for i, candidate := range protocolChoices() {
		if candidate == protocol {
			return i
		}
	}
	return 0
}

func newProviderWizard(mode wizardMode) *providerWizard {
	apiKey := newTextField()
	apiKey.masked = true
	steps := []wizardStep{wizardName, wizardProtocol, wizardBaseURL, wizardAPIKey, wizardProbe, wizardModels, wizardConfirm}
	if mode == wizardEdit {
		// An existing provider already has a name; renaming it would be a
		// delete plus an add, which is what /provider remove is for.
		steps = steps[1:]
	}
	return &providerWizard{
		mode:     mode,
		step:     steps[0],
		steps:    steps,
		name:     newTextField(),
		baseURL:  newTextField(),
		apiKey:   apiKey,
		protocol: protocolIndex(config.ProtocolOpenAIChat),
	}
}

// selectedModels returns the model IDs the user kept, in list order.
func (w *providerWizard) selectedModels() []string {
	var out []string
	for _, model := range w.models {
		if model.Selected {
			out = append(out, model.ID)
		}
	}
	return out
}

// typedModels parses the hand-entered model list, used when discovery failed or
// found nothing the user wanted.
func (w *providerWizard) typedModels() []string {
	fields := strings.FieldsFunc(w.manualModels.value(), func(r rune) bool {
		return r == ',' || r == '\n' || r == ' '
	})
	var out []string
	seen := map[string]bool{}
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
	}
	return out
}

// chosenModels is the model list to save: the ticked discoveries plus anything
// typed by hand, so a partially successful probe is still usable.
func (w *providerWizard) chosenModels() []string {
	out := w.selectedModels()
	seen := make(map[string]bool, len(out))
	for _, model := range out {
		seen[model] = true
	}
	for _, model := range w.typedModels() {
		if !seen[model] {
			seen[model] = true
			out = append(out, model)
		}
	}
	return out
}

// setting builds the configuration entry for the current wizard state.
func (w *providerWizard) setting() config.ProviderSetting {
	setting := config.ProviderSetting{
		APIKey:   strings.TrimSpace(w.apiKey.value()),
		BaseURL:  strings.TrimSpace(w.baseURL.value()),
		Protocol: protocolChoices()[clamp(w.protocol, 0, len(protocolChoices())-1)],
		Models:   w.chosenModels(),
	}
	return setting
}

// capabilityMap keeps the capability hints discovered for the chosen models, so
// an automatically discovered provider can drive compaction without the user
// typing a context window by hand.
func (w *providerWizard) capabilityMap() map[string]config.ModelCapability {
	capabilities := map[string]config.ModelCapability{}
	for _, model := range w.models {
		if !model.Selected {
			continue
		}
		if model.Advertise.ContextWindow == 0 && model.Advertise.MaxOutputTokens == 0 {
			continue
		}
		capabilities[model.ID] = config.ModelCapability{
			ContextWindow:   model.Advertise.ContextWindow,
			MaxOutputTokens: model.Advertise.MaxOutputTokens,
		}
	}
	if len(capabilities) == 0 {
		return nil
	}
	return capabilities
}

// needsBaseURL reports whether the current protocol requires an explicit
// endpoint. All three do when the provider name is not built in, so the wizard
// asks for one whenever the protocol is set explicitly.
func (w *providerWizard) needsBaseURL() bool { return true }

// currentField returns the field the current step edits, or nil when the step
// takes no text.
func (w *providerWizard) currentField() *textField {
	switch w.step {
	case wizardName:
		return &w.name
	case wizardBaseURL:
		return &w.baseURL
	case wizardAPIKey:
		return &w.apiKey
	case wizardModels:
		return &w.manualModels
	default:
		return nil
	}
}

// next advances to the following step, returning false at the end.
func (w *providerWizard) next() bool {
	for i, step := range w.steps {
		if step != w.step {
			continue
		}
		if i+1 >= len(w.steps) {
			return false
		}
		w.step = w.steps[i+1]
		w.err = ""
		return true
	}
	return false
}

// back moves to the previous step, returning false at the start.
func (w *providerWizard) back() bool {
	for i, step := range w.steps {
		if step != w.step {
			continue
		}
		if i == 0 {
			return false
		}
		w.step = w.steps[i-1]
		w.err = ""
		return true
	}
	return false
}

// validationError returns the reason the current step cannot be left, or an
// empty string when it can.
func (w *providerWizard) validationError() string {
	switch w.step {
	case wizardName:
		name := strings.ToLower(strings.TrimSpace(w.name.value()))
		if name == "" {
			return "a provider name is required"
		}
		if !config.ProviderNamePattern.MatchString(name) {
			return "names must match " + config.ProviderNamePattern.String() + " (lower-case letters, digits, dot, dash, underscore)"
		}
	case wizardBaseURL:
		value := strings.TrimSpace(w.baseURL.value())
		if w.needsBaseURL() && value == "" {
			return "a base URL is required"
		}
		if value != "" && !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
			return "the base URL must start with http:// or https://"
		}
		// The API key travels to this endpoint on every request. Over plain
		// http to a remote host it is readable by anything on the path, so
		// that combination is refused; loopback is exempt because the traffic
		// never leaves the machine.
		if strings.HasPrefix(value, "http://") && !isLoopbackURL(value) {
			return "plain http would send the API key in the clear; use https:// for a remote host (http:// is allowed for localhost)"
		}
	case wizardAPIKey:
		if strings.TrimSpace(w.apiKey.value()) == "" {
			return "an API key is required"
		}
	case wizardModels:
		if len(w.chosenModels()) == 0 {
			return "select or type at least one model"
		}
	}
	return ""
}
