package llm

import (
	"strings"

	"bruce-go/internal/config"
)

// Catalog is the resolved model offering: which provider/model pairs can
// actually be used, what each provider's default model is, and which option the
// settings point at.
//
// It exists because the same resolution is needed twice — once to construct the
// switchable client, and once by the provider editor, which has to know the
// resulting default before it can persist a change. Building it is pure: no
// network access, no file access.
type Catalog struct {
	// Options is every usable provider/model pair, in map order.
	Options []ModelOption
	// Defaults is the default model per provider.
	Defaults map[string]string
	// Models is the usable model list per provider, in the order considered.
	Models map[string][]string
}

// BuildCatalog resolves the configured providers into usable options.
//
// A provider is skipped when it has no API key, when it has no endpoint and no
// compiled-in one, or when no model can be determined for it. Skipping is
// deliberate: an entry that cannot be reached would otherwise fail at the first
// message rather than at configuration time.
func BuildCatalog(settings config.Settings) Catalog {
	catalog := Catalog{
		Options:  []ModelOption{},
		Defaults: map[string]string{},
		Models:   map[string][]string{},
	}
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
		catalog.Defaults[provider] = defaultModel(provider, models)
		catalog.Models[provider] = append([]string(nil), models...)
		for _, model := range models {
			catalog.Options = append(catalog.Options, ModelOption{Provider: provider, Model: model})
		}
	}
	return catalog
}

// modelsOf returns the usable models for a provider.
func (c Catalog) modelsOf(provider string) []string {
	return c.Models[NormalizeProvider(provider)]
}

// Has reports whether a provider contributes any usable option.
func (c Catalog) Has(provider string) bool {
	return len(c.modelsOf(provider)) > 0
}

// Initial returns the option the settings point at: the configured
// provider/model when it is usable, that provider's default when only the
// provider is known, and the first option otherwise.
//
// The last fallback is why this is a method rather than a bare lookup: the
// providers are stored in a map, so "first" has to come from the ordered option
// list to be deterministic.
func (c Catalog) Initial(settings config.LLMSettings) ModelOption {
	if len(c.Options) == 0 {
		return ModelOption{}
	}
	provider := NormalizeProvider(settings.DefaultProvider)
	if provider != "" && settings.DefaultModel != "" {
		for _, opt := range c.Options {
			if strings.EqualFold(opt.Provider, provider) && strings.EqualFold(opt.Model, settings.DefaultModel) {
				return opt
			}
		}
	}
	if model, ok := c.Defaults[provider]; ok {
		for _, opt := range c.Options {
			if strings.EqualFold(opt.Provider, provider) && strings.EqualFold(opt.Model, model) {
				return opt
			}
		}
	}
	return OrderedModelOptions(c.Options, ModelOption{})[0]
}

// ResolveDefault returns the provider/model pair to persist as the default once
// a set of providers has changed, keeping the current selection when it still
// exists and otherwise falling back deterministically.
func (c Catalog) ResolveDefault(current ModelOption) ModelOption {
	if len(c.Options) == 0 {
		return ModelOption{}
	}
	if current.Provider != "" && current.Model != "" {
		for _, opt := range c.Options {
			if strings.EqualFold(opt.Provider, current.Provider) && strings.EqualFold(opt.Model, current.Model) {
				return opt
			}
		}
	}
	// The current provider may still be usable with a different model — after
	// its list was edited, say.
	if c.Has(current.Provider) {
		return ModelOption{Provider: NormalizeProvider(current.Provider), Model: c.Defaults[NormalizeProvider(current.Provider)]}
	}
	return OrderedModelOptions(c.Options, ModelOption{})[0]
}
