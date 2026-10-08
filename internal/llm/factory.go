package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"bruce-go/internal/config"
)

var (
	DeepSeekModels = []string{config.DeepSeekDefaultModel}
	GLMModels      = []string{"glm-4.5-air", "glm-4.7", "glm-5-turbo", "glm-5.1", "glm-5.2", "glm-5v-turbo"}
	KimiModels     = []string{config.KimiDefaultModel, "kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k2.6"}
)

func validReasoningEffort(s string) bool {
	switch s {
	case "off", "low", "medium", "high", "max":
		return true
	}
	return false
}

type SwitchableClient struct {
	mu              sync.RWMutex
	settings        *config.Settings
	loader          config.Loader
	options         []ModelOption
	suppliers       map[string]func() ChatClient
	defaultModels   map[string]string
	current         ModelOption
	client          ChatClient
	reasoningEffort string
}

// ErrNoProvider means there is nothing to talk to: no provider entry at all, or
// none of them usable. It is distinct from a misconfiguration that must be
// fixed before Bruce can run, such as an invalid compaction window.
var ErrNoProvider = errors.New("error: llm.providers is not configured")

func NewSwitchable(settings config.Settings, loader config.Loader) (*SwitchableClient, error) {
	if len(settings.LLM.Providers) == 0 {
		return nil, ErrNoProvider
	}
	options := []ModelOption{}
	suppliers := map[string]func() ChatClient{}
	defaults := map[string]string{}
	for name, providerSettings := range settings.LLM.Providers {
		provider := NormalizeProvider(name)
		if strings.TrimSpace(providerSettings.APIKey) == "" {
			continue
		}
		// A provider with no compiled-in endpoint needs an explicit one; a
		// built-in provider may still override its endpoint via baseUrl.
		explicitProtocol := strings.TrimSpace(providerSettings.Protocol) != ""
		if (explicitProtocol || !providerHasEndpoint(provider)) && strings.TrimSpace(providerSettings.BaseURL) == "" {
			continue
		}
		models := supportedModels(provider, providerSettings)
		if len(models) == 0 {
			continue
		}
		defaults[provider] = defaultModel(provider, models)
		for _, model := range models {
			opt := ModelOption{Provider: provider, Model: model}
			options = append(options, opt)
			ps := providerSettings
			suppliers[key(opt)] = func() ChatClient {
				return NewProviderClient(provider, model, ps)
			}
		}
	}
	if len(options) == 0 {
		return nil, fmt.Errorf("%w: no provider has both an API key and a model list", ErrNoProvider)
	}
	initial := initialModel(settings.LLM, options, defaults)
	c := &SwitchableClient{
		settings:      &settings,
		loader:        loader,
		options:       options,
		suppliers:     suppliers,
		defaultModels: defaults,
		current:       initial,
	}
	c.client = c.suppliers[key(initial)]()
	if err := validateCompactionWindow(settings.Compaction, initial, c.client); err != nil {
		return nil, err
	}
	effort := strings.TrimSpace(settings.LLM.ReasoningEffort)
	if effort == "" || !validReasoningEffort(effort) {
		effort = "max"
	}
	c.reasoningEffort = effort
	c.applyEffortToClient()
	return c, nil
}

func NormalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "zai", "zhipu", "bigmodel", "zhipuai":
		return "glm"
	case "kimi", "moonshot", "moonshotai":
		return "kimi"
	case "openai-compatible", "openai_compatible", "openai", "compatible", "openai_compatiable":
		return "openai_compatiable"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

// NewProviderClient builds the client for one provider/model pair.
//
// The protocol decides the wire format, and it is resolved per provider rather
// than per name: the built-in names keep their compiled-in endpoints and
// behaviour, an explicit "protocol" wins everywhere, and a name that says
// anthropic or claude is spoken to in the Messages format.
func NewProviderClient(provider, model string, settings config.ProviderSetting) ChatClient {
	protocol := config.ResolveProtocol(provider, settings)
	// A built-in provider with a compiled-in endpoint keeps its own client, so
	// its request quirks (HTTP/2, endpoint selection, per-model fields) stay
	// intact. An explicit protocol or an explicit baseUrl overrides that: the
	// user asked for a different endpoint, and the quirks travel with the
	// provider name rather than with the constructor.
	explicit := strings.TrimSpace(settings.Protocol) != "" || strings.TrimSpace(settings.BaseURL) != ""
	if !explicit {
		switch provider {
		case "glm":
			return withCapability(NewGLMClient(settings.APIKey, model), settings, model)
		case "deepseek":
			return withCapability(NewDeepSeekClient(settings.APIKey, model), settings, model)
		case "kimi":
			return withCapability(NewKimiClient(settings.APIKey, model), settings, model)
		}
	}
	switch protocol {
	case config.ProtocolAnthropic:
		return withCapability(NewAnthropicClient(provider, settings.APIKey, model, settings.BaseURL), settings, model)
	case config.ProtocolOpenAIResponses:
		return withCapability(NewOpenAIResponsesClient(provider, settings.APIKey, model, settings.BaseURL), settings, model)
	default:
		return withCapability(NewOpenAICompatibleClient(provider, settings.APIKey, model, settings.BaseURL), settings, model)
	}
}

// withCapability applies the declared context window and output limit to a
// client that supports them.
func withCapability(client ChatClient, settings config.ProviderSetting, model string) ChatClient {
	capability := settings.ModelCapabilities[model]
	if capability.ContextWindow == 0 && capability.MaxOutputTokens == 0 {
		return client
	}
	if configurable, ok := client.(interface {
		SetModelCapability(contextWindow, maxOutputTokens int)
	}); ok {
		configurable.SetModelCapability(capability.ContextWindow, capability.MaxOutputTokens)
	}
	return client
}

func (c *SwitchableClient) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, opts StreamOptions) (ChatResponse, error) {
	c.mu.RLock()
	client := c.client
	c.mu.RUnlock()
	return client.Chat(ctx, messages, tools, opts)
}

func (c *SwitchableClient) ProviderName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current.Provider
}

func (c *SwitchableClient) ModelName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current.Model
}

func (c *SwitchableClient) MaxContextWindow() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client.MaxContextWindow()
}

func (c *SwitchableClient) MaxOutputTokens() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client.MaxOutputTokens()
}

func (c *SwitchableClient) SupportsTools() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client.SupportsTools()
}

func (c *SwitchableClient) SupportsPromptCaching() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client.SupportsPromptCaching()
}

func (c *SwitchableClient) SupportsImages() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client.SupportsImages()
}

// Options returns the model list sorted deterministically with the current
// model first. This is the single place where ordering is applied.
func (c *SwitchableClient) Options() []ModelOption {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return OrderedModelOptions(c.options, c.current)
}

func (c *SwitchableClient) Current() ModelOption {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}

func (c *SwitchableClient) applyEffortToClient() {
	if rc, ok := c.client.(interface {
		SetReasoningEffort(string)
	}); ok {
		rc.SetReasoningEffort(c.reasoningEffort)
	}
}

func (c *SwitchableClient) Switch(selector string) (ModelOption, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next, err := c.resolve(selector)
	if err != nil {
		return ModelOption{}, err
	}
	supplier := c.suppliers[key(next)]
	if supplier == nil {
		return ModelOption{}, errors.New("unknown model: " + next.Display())
	}
	nextClient := supplier()
	if err := validateCompactionWindow(c.settings.Compaction, next, nextClient); err != nil {
		return ModelOption{}, err
	}
	oldProvider, oldModel := c.settings.LLM.DefaultProvider, c.settings.LLM.DefaultModel
	c.settings.LLM.DefaultProvider = strings.ToLower(next.Provider)
	c.settings.LLM.DefaultModel = next.Model
	if c.loader.Path != "" {
		if err := c.loader.Save(*c.settings); err != nil {
			c.settings.LLM.DefaultProvider, c.settings.LLM.DefaultModel = oldProvider, oldModel
			return ModelOption{}, err
		}
	}
	c.current = next
	c.client = nextClient
	c.applyEffortToClient()
	return next, nil
}

func validateCompactionWindow(settings config.Compaction, model ModelOption, client ChatClient) error {
	if !settings.Enabled || client.MaxContextWindow() <= 0 {
		return nil
	}
	if _, err := settings.Threshold(client.MaxContextWindow()); err != nil {
		return fmt.Errorf("invalid automatic-compaction configuration for model %s: %w", model.Selector(), err)
	}
	return nil
}

func (c *SwitchableClient) ReasoningEffort() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reasoningEffort
}

func (c *SwitchableClient) SetReasoningEffort(level string) error {
	level = strings.TrimSpace(strings.ToLower(level))
	if !validReasoningEffort(level) {
		return fmt.Errorf("invalid reasoning effort: %s (allowed values: off, low, medium, high, max)", level)
	}
	c.mu.Lock()
	old := c.settings.LLM.ReasoningEffort
	c.settings.LLM.ReasoningEffort = level
	if c.loader.Path != "" {
		if err := c.loader.Save(*c.settings); err != nil {
			c.settings.LLM.ReasoningEffort = old
			c.mu.Unlock()
			return err
		}
	}
	c.reasoningEffort = level
	c.applyEffortToClient()
	c.mu.Unlock()
	return nil
}

func (c *SwitchableClient) resolve(selector string) (ModelOption, error) {
	value := strings.TrimSpace(selector)
	if value == "" {
		return c.current, nil
	}
	if strings.Contains(value, "/") {
		parts := strings.SplitN(value, "/", 2)
		return c.find(NormalizeProvider(parts[0]), parts[1])
	}
	provider := NormalizeProvider(value)
	if model, ok := c.defaultModels[provider]; ok {
		return c.find(provider, model)
	}
	var matches []ModelOption
	for _, opt := range c.options {
		if strings.EqualFold(opt.Model, value) {
			matches = append(matches, opt)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return ModelOption{}, errors.New("model name is ambiguous: " + value + "; use /model provider/model")
	}
	return ModelOption{}, errors.New("unknown model: " + value)
}

func (c *SwitchableClient) find(provider, model string) (ModelOption, error) {
	for _, opt := range c.options {
		if strings.EqualFold(opt.Provider, provider) && strings.EqualFold(opt.Model, model) {
			return opt, nil
		}
	}
	return ModelOption{}, errors.New("unknown model: " + provider + "/" + model)
}

// supportedModels returns the models to offer for a provider.
//
// An explicitly declared list always wins, for built-in providers too: it is the
// user's statement of what their key can reach, and the compiled-in table is
// only a fallback for entries that do not say.
func supportedModels(provider string, settings config.ProviderSetting) []string {
	if len(settings.Models) > 0 {
		return settings.Models
	}
	switch provider {
	case "glm":
		return GLMModels
	case "deepseek":
		return DeepSeekModels
	case "kimi":
		return KimiModels
	default:
		return nil
	}
}

// providerHasEndpoint reports whether a provider can be reached without an
// explicit baseUrl. The built-in providers carry compiled-in endpoints; every
// other provider must name one, because guessing an endpoint would send the API
// key to a host the user never chose.
func providerHasEndpoint(provider string) bool {
	switch provider {
	case "glm", "deepseek", "kimi":
		return true
	default:
		return false
	}
}

func defaultModel(provider string, models []string) string {
	switch provider {
	case "glm":
		return "glm-5.1"
	case "deepseek":
		return config.DeepSeekDefaultModel
	case "kimi":
		return config.KimiDefaultModel
	default:
		if len(models) > 0 {
			return models[0]
		}
		return ""
	}
}

func initialModel(settings config.LLMSettings, options []ModelOption, defaults map[string]string) ModelOption {
	provider := NormalizeProvider(settings.DefaultProvider)
	if provider != "" && settings.DefaultModel != "" {
		for _, opt := range options {
			if strings.EqualFold(opt.Provider, provider) && strings.EqualFold(opt.Model, settings.DefaultModel) {
				return opt
			}
		}
	}
	if model, ok := defaults[provider]; ok {
		for _, opt := range options {
			if strings.EqualFold(opt.Provider, provider) && strings.EqualFold(opt.Model, model) {
				return opt
			}
		}
	}
	return options[0]
}

func key(opt ModelOption) string {
	return strings.ToLower(opt.Provider + "/" + opt.Model)
}
