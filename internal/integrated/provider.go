package integrated

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"bruce-go/internal/config"
	"bruce-go/internal/llm"
)

// ProviderSummary describes one configured provider for display and for the TUI
// editor. The API key is masked: a summary is rendered on screen and into
// session transcripts, and neither is a place for a credential.
type ProviderSummary struct {
	Name string
	// Protocol is the resolved protocol, not the raw field: an entry that
	// relies on inference or an alias reports what will actually be used.
	Protocol string
	BaseURL  string
	// APIKey is a masked rendering, never the stored value.
	APIKey string
	// Models is the effective model list: the declared one, or the built-in
	// table when the entry declares none.
	Models []string
	// Active marks the provider of the current model.
	Active bool
}

// ListProviders returns every configured provider, sorted by name so the output
// is stable across runs, in map order otherwise.
func (r *Runtime) ListProviders() ([]ProviderSummary, error) {
	settings, err := r.Loader.Load()
	if err != nil {
		return nil, err
	}
	current := r.CurrentModel()
	names := make([]string, 0, len(settings.LLM.Providers))
	for name := range settings.LLM.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	catalog := llm.BuildCatalog(settings)
	summaries := make([]ProviderSummary, 0, len(names))
	for _, name := range names {
		provider := settings.LLM.Providers[name]
		summaries = append(summaries, ProviderSummary{
			Name:     name,
			Protocol: config.ResolveProtocol(name, provider),
			BaseURL:  strings.TrimSpace(provider.BaseURL),
			APIKey:   maskAPIKey(provider.APIKey),
			Models:   catalog.Models[llm.NormalizeProvider(name)],
			Active:   strings.EqualFold(name, current.Provider),
		})
	}
	return summaries, nil
}

// maskAPIKey renders a key so it can be shown as "set" without being usable.
// A short key has no safe prefix to display and is masked entirely.
func maskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "********"
	}
	return "****" + key[len(key)-4:]
}

// SaveProvider adds or replaces a provider and rebuilds the running client.
//
// The order matters and is enforced here: load from disk, mutate the copy,
// resolve the default model, build the candidate in memory, persist, re-read,
// swap. Building before persisting means a rejected configuration never reaches
// the disk, and re-reading after persisting means the process only adopts a
// file that provably parses back.
//
// When activate is true the new provider becomes the current model. Otherwise
// the current selection is kept if it still exists.
func (r *Runtime) SaveProvider(name string, setting config.ProviderSetting, activate bool) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return errors.New("a provider name is required")
	}
	if !config.ProviderNamePattern.MatchString(name) {
		return fmt.Errorf("invalid provider name %q: names must match %s", name, config.ProviderNamePattern.String())
	}

	// r.Settings is a startup snapshot. The switchable client keeps its own
	// copy and writes model switches into it, so reading from disk is the only
	// way to avoid resurrecting a stale default.
	fresh, err := r.Loader.Load()
	if err != nil {
		return err
	}
	if fresh.LLM.Providers == nil {
		fresh.LLM.Providers = map[string]config.ProviderSetting{}
	}
	fresh.LLM.Providers[name] = setting
	return r.replaceProviders(fresh, activate, name)
}

// RemoveProvider deletes a provider. Removing the last usable one is a legal
// outcome: the runtime lands back in the unconfigured state rather than
// refusing the operation.
func (r *Runtime) RemoveProvider(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	fresh, err := r.Loader.Load()
	if err != nil {
		return err
	}
	if _, ok := fresh.LLM.Providers[name]; !ok {
		return errors.New("unknown provider: " + name)
	}
	delete(fresh.LLM.Providers, name)
	// The removed provider may have owned the default entry. Clearing it lets
	// the catalog pick the replacement deterministically instead of keeping a
	// dangling name that only happens to resolve today.
	if strings.EqualFold(fresh.LLM.DefaultProvider, name) {
		fresh.LLM.DefaultProvider = ""
		fresh.LLM.DefaultModel = ""
	}
	return r.replaceProviders(fresh, false, "")
}

// replaceProviders applies an edited provider set: it resolves the new default,
// builds the candidate, persists it, and only then swaps the running client.
// activate names the provider to switch to, if any.
func (r *Runtime) replaceProviders(fresh config.Settings, activate bool, activeName string) error {
	// Resolve the default model before building anything: the client starts on
	// whatever the settings name, so it has to be the resolved value or the
	// running client would disagree with the file that was just written.
	catalog := llm.BuildCatalog(fresh)
	next := catalog.Initial(fresh.LLM)
	switch {
	case activate && catalog.Has(activeName):
		normalized := llm.NormalizeProvider(activeName)
		next = llm.ModelOption{Provider: normalized, Model: catalog.Defaults[normalized]}
	case !activate:
		next = catalog.ResolveDefault(r.CurrentModel())
	}
	// The default is written explicitly rather than left empty: the providers
	// are a map, so an empty default would resolve through map iteration order.
	if len(catalog.Options) > 0 {
		fresh.LLM.DefaultProvider = next.Provider
		fresh.LLM.DefaultModel = next.Model
	} else {
		fresh.LLM.DefaultProvider = ""
		fresh.LLM.DefaultModel = ""
	}

	candidate, err := llm.NewSwitchable(fresh, r.Loader)
	if err != nil && !errors.Is(err, llm.ErrNoProvider) {
		// An invalid compaction window, say: a real misconfiguration that must
		// be reported before anything is written.
		return err
	}

	previous := r.Settings
	if err := r.Loader.Save(fresh); err != nil {
		// Nothing was swapped, so the process still runs the old client and
		// the file was not successfully written.
		return err
	}
	// Only adopt a file that reads back. A save that produced something
	// unparseable must not become the running configuration.
	loaded, err := r.Loader.Load()
	if err != nil {
		if restoreErr := r.Loader.Save(previous); restoreErr != nil {
			return fmt.Errorf("the new provider configuration could not be read back (%v) and the previous one could not be restored (%v)", err, restoreErr)
		}
		return fmt.Errorf("the new provider configuration could not be read back: %w", err)
	}

	r.Settings = loaded
	if candidate == nil {
		// Every provider is gone; fall back to the stand-in client and leave
		// the switchable client nil so the existing nil branches apply.
		r.Client = llm.NewUnconfiguredClient()
		r.switchable = nil
	} else {
		r.switchable = candidate
		r.Client = candidate
	}
	r.rebuildAgents()
	return nil
}

// SwitchModel switches the running client by selector, using the same syntax as
// /model: "provider", "provider/model", or a bare model name that is unique.
func (r *Runtime) SwitchModel(selector string) (llm.ModelOption, error) {
	if r.switchable == nil {
		return llm.ModelOption{}, errors.New("no LLM provider is configured; add one with /provider add")
	}
	if _, err := r.switchable.Switch(selector); err != nil {
		return llm.ModelOption{}, err
	}
	r.Client = r.switchable
	r.rebuildAgents()
	return r.switchable.Current(), nil
}

// handleProvider implements /provider. The TUI intercepts the interactive
// subcommands (add and edit) before they reach this point; everything else has
// a text implementation so the command works without a terminal UI too.
func (r *Runtime) handleProvider(_ context.Context, args []string) (string, error) {
	if len(args) == 0 || strings.EqualFold(args[0], "list") {
		return r.renderProviders()
	}
	switch strings.ToLower(args[0]) {
	case "remove", "rm":
		if len(args) < 2 {
			return "", errors.New("usage: /provider remove <name>")
		}
		if err := r.RemoveProvider(strings.Join(args[1:], " ")); err != nil {
			return "", err
		}
		return fmt.Sprintf("Removed provider: %s\n\n%s", args[1], r.renderProvidersOrEmpty()), nil
	case "add", "edit":
		// Without the wizard there is nowhere to enter a name, URL and key, so
		// this is a pointer rather than a partial implementation that would
		// persist half a provider.
		return "", errors.New("/provider " + args[0] + " must be run in the TUI, where the configuration wizard opens")
	default:
		return "", errors.New("usage: /provider [add|edit <name>|remove <name>|list]")
	}
}

func (r *Runtime) renderProvidersOrEmpty() string {
	out, err := r.renderProviders()
	if err != nil {
		return ""
	}
	return out
}

func (r *Runtime) renderProviders() (string, error) {
	summaries, err := r.ListProviders()
	if err != nil {
		return "", err
	}
	return RenderProviders(summaries), nil
}

// RenderProviders formats the provider list. It lives here rather than in the
// render package because its input type is defined here, and render already
// imports runtime.
func RenderProviders(summaries []ProviderSummary) string {
	if len(summaries) == 0 {
		return "No LLM provider is configured. Run /provider add to set one up."
	}
	var b strings.Builder
	b.WriteString("Providers:\n")
	for _, summary := range summaries {
		marker := "  "
		if summary.Active {
			marker = "* "
		}
		key := "(no API key)"
		if summary.APIKey != "" {
			key = "key=" + summary.APIKey
		}
		base := summary.BaseURL
		if base == "" {
			base = "(built-in endpoint)"
		}
		fmt.Fprintf(&b, "%s%s  protocol=%s  %s  baseUrl=%s\n", marker, summary.Name, summary.Protocol, key, base)
		if len(summary.Models) > 0 {
			models := summary.Models
			// A long list is summarized: the editor shows the whole thing, and
			// a wall of names buries the next provider's line.
			if len(models) > 6 {
				models = append(append([]string(nil), models[:6]...), fmt.Sprintf("… (%d more)", len(summary.Models)-6))
			}
			fmt.Fprintf(&b, "    models: %s\n", strings.Join(models, ", "))
		} else {
			b.WriteString("    models: (none)\n")
		}
	}
	b.WriteString("\nUse /provider add, /provider edit <name>, or /provider remove <name>.")
	return strings.TrimSpace(b.String())
}
