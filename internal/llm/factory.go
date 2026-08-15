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
	DeepSeekModels = []string{"deepseek-v4-flash", "deepseek-v4-pro"}
	GLMModels      = []string{"glm-4.5-air", "glm-4.7", "glm-5-turbo", "glm-5.1", "glm-5.2", "glm-5v-turbo"}
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

func NewSwitchable(settings config.Settings, loader config.Loader) (*SwitchableClient, error) {
	if len(settings.LLM.Providers) == 0 {
		return nil, errors.New("error: llm.providers is not configured")
	}
	options := []ModelOption{}
	suppliers := map[string]func() ChatClient{}
	defaults := map[string]string{}
	for name, providerSettings := range settings.LLM.Providers {
		provider := NormalizeProvider(name)
		if strings.TrimSpace(providerSettings.APIKey) == "" {
			continue
		}
		if provider == "openai_compatiable" && strings.TrimSpace(providerSettings.BaseURL) == "" {
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
		return nil, errors.New("error: setting.json contains no usable LLM provider")
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
	case "openai-compatible", "openai_compatible", "openai", "compatible", "openai_compatiable":
		return "openai_compatiable"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

func NewProviderClient(provider, model string, settings config.ProviderSetting) ChatClient {
	var client *OpenAICompatibleClient
	switch provider {
	case "glm":
		client = NewGLMClient(settings.APIKey, model)
	case "deepseek":
		client = NewDeepSeekClient(settings.APIKey, model)
	case "openai_compatiable":
		client = NewOpenAICompatibleClient(provider, settings.APIKey, model, settings.BaseURL)
	default:
		client = NewOpenAICompatibleClient(provider, settings.APIKey, model, settings.BaseURL)
	}
	capability := settings.ModelCapabilities[model]
	client.SetModelCapability(capability.ContextWindow, capability.MaxOutputTokens)
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

func supportedModels(provider string, settings config.ProviderSetting) []string {
	switch provider {
	case "glm":
		return GLMModels
	case "deepseek":
		return DeepSeekModels
	case "openai_compatiable":
		return settings.Models
	default:
		return nil
	}
}

func defaultModel(provider string, models []string) string {
	switch provider {
	case "glm":
		return "glm-5.1"
	case "deepseek":
		return "deepseek-v4-flash"
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
