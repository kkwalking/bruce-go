package llm

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"bruce-go/internal/config"
)

func TestSwitchableClientListsAndSwitchesModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setting.json")
	settings := config.DefaultSettings()
	settings.LLM.DefaultProvider = "openai_compatiable"
	settings.LLM.DefaultModel = "local-a"
	settings.LLM.Providers["openai_compatiable"] = config.ProviderSetting{
		APIKey:  "key",
		BaseURL: "http://localhost:9000/v1",
		Models:  []string{"local-a", "local-b"},
	}
	loader := config.NewLoader(path)
	if err := loader.Save(settings); err != nil {
		t.Fatal(err)
	}

	client, err := NewSwitchable(settings, loader)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.Current().Selector(); got != "openai_compatiable/local-a" {
		t.Fatalf("current = %q", got)
	}
	next, err := client.Switch("openai_compatiable/local-b")
	if err != nil {
		t.Fatal(err)
	}
	if next.Model != "local-b" || client.ModelName() != "local-b" {
		t.Fatalf("next=%+v model=%q", next, client.ModelName())
	}
	reloaded, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LLM.DefaultModel != "local-b" {
		t.Fatalf("saved default model = %q", reloaded.LLM.DefaultModel)
	}
}

func TestSwitchableClientOrdersOptionsWithCurrentFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setting.json")
	settings := config.DefaultSettings()
	settings.LLM.DefaultProvider = "glm"
	settings.LLM.DefaultModel = "glm-5.2"
	settings.LLM.Providers["deepseek"] = config.ProviderSetting{APIKey: "deepseek-key"}
	settings.LLM.Providers["glm"] = config.ProviderSetting{APIKey: "glm-key"}
	settings.LLM.Providers["openai_compatiable"] = config.ProviderSetting{
		APIKey:  "local-key",
		BaseURL: "http://localhost:9000/v1",
		Models:  []string{"local-b", "local-a"},
	}
	loader := config.NewLoader(path)
	if err := loader.Save(settings); err != nil {
		t.Fatal(err)
	}

	client, err := NewSwitchable(settings, loader)
	if err != nil {
		t.Fatal(err)
	}
	options := client.Options()
	if got := options[0].Selector(); got != "glm/glm-5.2" {
		t.Fatalf("first option = %q, want current model glm/glm-5.2", got)
	}
	rest := options[1:]
	for i := 1; i < len(rest); i++ {
		prev := rest[i-1].Selector()
		curr := rest[i].Selector()
		if prev > curr {
			t.Fatalf("remaining options are not sorted: %q before %q", prev, curr)
		}
	}

	if _, err := client.Switch("deepseek/deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	options = client.Options()
	if got := options[0].Selector(); got != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("first option after switch = %q, want deepseek/deepseek-v4.1-flash", got)
	}
}

func TestSwitchableClientRejectsInvalidCompactionWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setting.json")
	settings := config.DefaultSettings()
	settings.Compaction.ContextWindowRatio = 0.8
	settings.Compaction.ReserveTokens = 80
	settings.LLM.DefaultProvider = "openai_compatiable"
	settings.LLM.DefaultModel = "local-a"
	settings.LLM.Providers["openai_compatiable"] = config.ProviderSetting{
		APIKey:  "key",
		BaseURL: "http://localhost:9000/v1",
		Models:  []string{"local-a", "local-b"},
		ModelCapabilities: map[string]config.ModelCapability{
			"local-a": {ContextWindow: 200},
			"local-b": {ContextWindow: 100},
		},
	}
	loader := config.NewLoader(path)
	if err := loader.Save(settings); err != nil {
		t.Fatal(err)
	}

	client, err := NewSwitchable(settings, loader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Switch("openai_compatiable/local-b"); err == nil {
		t.Fatal("switch to model with invalid compaction window should fail")
	}
	if got := client.Current().Model; got != "local-a" {
		t.Fatalf("current model changed to %q", got)
	}
	reloaded, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.LLM.DefaultModel; got != "local-a" {
		t.Fatalf("persisted default model changed to %q", got)
	}

	settings.LLM.DefaultModel = "local-b"
	if _, err := NewSwitchable(settings, loader); err == nil {
		t.Fatal("initial model with invalid compaction window should fail")
	}
}

func TestSwitchableClientReasoningEffort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setting.json")
	settings := config.DefaultSettings()
	settings.LLM.DefaultProvider = "openai_compatiable"
	settings.LLM.DefaultModel = "local-a"
	settings.LLM.Providers["openai_compatiable"] = config.ProviderSetting{
		APIKey:  "key",
		BaseURL: "http://localhost:9000/v1",
		Models:  []string{"local-a", "local-b"},
	}
	loader := config.NewLoader(path)
	if err := loader.Save(settings); err != nil {
		t.Fatal(err)
	}

	client, err := NewSwitchable(settings, loader)
	if err != nil {
		t.Fatal(err)
	}

	// Default should be "max"
	if got := client.ReasoningEffort(); got != "max" {
		t.Fatalf("default reasoning effort = %q, want max", got)
	}

	// Set to "low" and verify
	if err := client.SetReasoningEffort("low"); err != nil {
		t.Fatal(err)
	}
	if got := client.ReasoningEffort(); got != "low" {
		t.Fatalf("reasoning effort after set = %q, want low", got)
	}

	// Verify persistence
	reloaded, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.LLM.ReasoningEffort; got != "low" {
		t.Fatalf("persisted reasoningEffort = %q, want low", got)
	}

	// Invalid value returns error
	if err := client.SetReasoningEffort("invalid"); err == nil {
		t.Fatal("expected error for invalid reasoning effort")
	}
}

func TestNormalizeProviderAliases(t *testing.T) {
	cases := map[string]string{
		"kimi":       "kimi",
		"Kimi":       "kimi",
		"moonshot":   "kimi",
		"MoonshotAI": "kimi",
		"zai":        "glm",
		"zhipu":      "glm",
		"bigmodel":   "glm",
		"glm":        "glm",
		"deepseek":   "deepseek",
		"openai":     "openai_compatiable",
		"compatible": "openai_compatiable",
		"  Kimi  ":   "kimi",
		"unknown-x":  "unknown-x",
	}
	for input, want := range cases {
		if got := NormalizeProvider(input); got != want {
			t.Fatalf("NormalizeProvider(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNewProviderClientBuildsKimiClient(t *testing.T) {
	client := NewProviderClient("kimi", "", config.ProviderSetting{APIKey: "kimi-key"})
	if got := client.ProviderName(); got != "kimi" {
		t.Fatalf("provider = %q, want kimi", got)
	}
	if got := client.ModelName(); got != config.KimiDefaultModel {
		t.Fatalf("model = %q, want %s", got, config.KimiDefaultModel)
	}
	if client.MaxContextWindow() != 1048576 {
		t.Fatalf("context window = %d, want 1048576", client.MaxContextWindow())
	}
	if !client.SupportsTools() {
		t.Fatal("kimi should support tools")
	}
	if !client.SupportsPromptCaching() {
		t.Fatal("kimi should report prompt caching")
	}
	if !client.SupportsImages() {
		t.Fatal("kimi models accept image input")
	}
}

// A provider name is an address, not a protocol switch. Any name a user picks
// must become a usable provider, as long as the entry carries what the chosen
// protocol needs.
func TestNewSwitchableAcceptsCustomProviderName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setting.json")
	settings := config.DefaultSettings()
	settings.LLM.DefaultProvider = "mygateway"
	settings.LLM.DefaultModel = "custom-large"
	settings.LLM.Providers["mygateway"] = config.ProviderSetting{
		APIKey:  "key",
		BaseURL: "http://localhost:9000/v1",
		Models:  []string{"custom-large", "custom-small"},
	}
	loader := config.NewLoader(path)
	if err := loader.Save(settings); err != nil {
		t.Fatal(err)
	}

	client, err := NewSwitchable(settings, loader)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.Current().Selector(); got != "mygateway/custom-large" {
		t.Fatalf("current = %q, want mygateway/custom-large", got)
	}
	if _, err := client.Switch("mygateway/custom-small"); err != nil {
		t.Fatalf("switching within a custom provider failed: %v", err)
	}
	if got := client.ModelName(); got != "custom-small" {
		t.Fatalf("model = %q, want custom-small", got)
	}
}

// The declared model list is the provider's own list. For a built-in provider
// it must override the compiled-in table rather than be ignored, otherwise a
// user cannot narrow a provider down to the models their key can reach.
func TestSupportedModelsPrefersConfiguredList(t *testing.T) {
	declared := []string{"glm-5.2", "glm-5.1"}
	if got := supportedModels("glm", config.ProviderSetting{Models: declared}); !slices.Equal(got, declared) {
		t.Fatalf("glm models = %#v, want the declared list %#v", got, declared)
	}
	// With nothing declared the compiled-in table still applies.
	if got := supportedModels("glm", config.ProviderSetting{}); !slices.Equal(got, GLMModels) {
		t.Fatalf("glm models without a declared list = %#v, want %#v", got, GLMModels)
	}
	// A custom name has no compiled-in table, so the declared list is all there is.
	custom := []string{"a", "b"}
	if got := supportedModels("mygateway", config.ProviderSetting{Models: custom}); !slices.Equal(got, custom) {
		t.Fatalf("custom models = %#v, want %#v", got, custom)
	}
	if got := supportedModels("mygateway", config.ProviderSetting{}); len(got) != 0 {
		t.Fatalf("custom models without a declared list = %#v, want none", got)
	}
}

// An entry without a key cannot authenticate, so it is skipped instead of
// producing a provider that fails on the first request.
func TestNewSwitchableSkipsProviderWithBlankKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setting.json")
	settings := config.DefaultSettings()
	settings.LLM.DefaultProvider = "withkey"
	settings.LLM.DefaultModel = "m"
	settings.LLM.Providers["withkey"] = config.ProviderSetting{APIKey: "k", BaseURL: "http://x/v1", Models: []string{"m"}}
	settings.LLM.Providers["nokey"] = config.ProviderSetting{BaseURL: "http://y/v1", Models: []string{"m"}}
	loader := config.NewLoader(path)
	if err := loader.Save(settings); err != nil {
		t.Fatal(err)
	}

	client, err := NewSwitchable(settings, loader)
	if err != nil {
		t.Fatal(err)
	}
	for _, opt := range client.Options() {
		if opt.Provider == "nokey" {
			t.Fatalf("provider without an API key must not become an option: %#v", opt)
		}
	}
}

// Each protocol needs an endpoint of its own shape. A base URL is only
// mandatory where nothing else supplies one, which is everywhere except the
// built-in providers.
func TestNewSwitchableRequiresBaseURLOnlyForCustomEndpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setting.json")
	settings := config.DefaultSettings()
	settings.LLM.DefaultProvider = "deepseek"
	settings.LLM.DefaultModel = config.DeepSeekDefaultModel
	// deepseek has a compiled-in endpoint, so no baseUrl is needed.
	settings.LLM.Providers["deepseek"] = config.ProviderSetting{APIKey: "k"}
	// An anthropic-named provider has no compiled-in endpoint.
	settings.LLM.Providers["anthropic"] = config.ProviderSetting{APIKey: "k", Models: []string{"claude-opus-5"}}
	loader := config.NewLoader(path)
	if err := loader.Save(settings); err != nil {
		t.Fatal(err)
	}

	client, err := NewSwitchable(settings, loader)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.Current().Selector(); got != "deepseek/"+config.DeepSeekDefaultModel {
		t.Fatalf("current = %q", got)
	}
	for _, opt := range client.Options() {
		if opt.Provider == "anthropic" {
			t.Fatalf("a provider without a base URL must not become an option: %#v", opt)
		}
	}
}

// A provider that names a protocol must be built with that protocol's client,
// because the wire formats are not interchangeable.
func TestNewProviderClientSelectsProtocol(t *testing.T) {
	setting := config.ProviderSetting{APIKey: "k", BaseURL: "http://localhost:9000", Models: []string{"m"}}
	for _, tc := range []struct {
		protocol string
		check    func(client ChatClient) bool
	}{
		{config.ProtocolOpenAIChat, func(c ChatClient) bool { _, ok := c.(*OpenAICompatibleClient); return ok }},
		{config.ProtocolOpenAIResponses, func(c ChatClient) bool { _, ok := c.(*OpenAIResponsesClient); return ok }},
		{config.ProtocolAnthropic, func(c ChatClient) bool { _, ok := c.(*AnthropicClient); return ok }},
	} {
		setting.Protocol = tc.protocol
		client := NewProviderClient("custom", "m", setting)
		if !tc.check(client) {
			t.Fatalf("protocol %q produced %T", tc.protocol, client)
		}
	}
	// An anthropic-named provider with no explicit protocol still gets the
	// Anthropic client, and a generic name still gets Chat Completions.
	if _, ok := NewProviderClient("my-claude", "m", config.ProviderSetting{APIKey: "k", BaseURL: "http://x"}).(*AnthropicClient); !ok {
		t.Fatal("an anthropic-named provider should use the Anthropic client")
	}
	if _, ok := NewProviderClient("mygateway", "m", config.ProviderSetting{APIKey: "k", BaseURL: "http://x"}).(*OpenAICompatibleClient); !ok {
		t.Fatal("a generic provider should use the Chat Completions client")
	}
}

// Environment-provided providers must be selectable through the switchable
// client, which is the point of registering them as candidates.
func TestSwitchableClientIncludesEnvironmentProviders(t *testing.T) {
	for _, provider := range config.BuiltInProviders() {
		t.Setenv(provider.Env, "")
	}
	t.Setenv(config.GLMAPIKeyEnv, "glm-env-key")
	t.Setenv(config.KimiAPIKeyEnv, "kimi-env-key")

	loader := config.NewLoader(filepath.Join(t.TempDir(), "setting.json"))
	settings, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewSwitchable(settings, loader)
	if err != nil {
		t.Fatal(err)
	}

	options := map[string]bool{}
	for _, opt := range client.Options() {
		options[opt.Selector()] = true
	}
	if !options["glm/"+config.GLMDefaultModel] {
		t.Fatalf("glm/%s missing from options: %#v", config.GLMDefaultModel, options)
	}
	if !options["kimi/"+config.KimiDefaultModel] {
		t.Fatalf("kimi/%s missing from options: %#v", config.KimiDefaultModel, options)
	}

	next, err := client.Switch("kimi")
	if err != nil {
		t.Fatal(err)
	}
	if next.Provider != "kimi" || next.Model != config.KimiDefaultModel {
		t.Fatalf("switch result = %+v", next)
	}
	if got := client.ProviderName(); got != "kimi" {
		t.Fatalf("current provider = %q, want kimi", got)
	}
}

// Two entries whose names normalize to the same provider cannot both be
// honoured: NormalizeProvider collapses "kimi" and "moonshot" onto one
// provider, so at most one endpoint and one credential can win, and which one
// would depend on map iteration order. Before this was rejected, the model list
// (built by BuildCatalog) and the supplier table (built by a second loop) could
// disagree and NewSwitchable called a nil supplier.
func TestNewSwitchableRejectsEntriesThatNameTheSameProvider(t *testing.T) {
	settings := config.DefaultSettings()
	settings.LLM.DefaultProvider = "kimi"
	settings.LLM.DefaultModel = "shared"
	settings.LLM.Providers["kimi"] = config.ProviderSetting{
		APIKey: "KEY-ALICE", BaseURL: "https://alice.example/v1", Models: []string{"shared"},
	}
	settings.LLM.Providers["moonshot"] = config.ProviderSetting{
		APIKey: "KEY-BOB", BaseURL: "https://bob.example/v1", Models: []string{"shared"},
	}
	client, err := NewSwitchable(settings, config.NewLoader(filepath.Join(t.TempDir(), "setting.json")))
	if err == nil {
		t.Fatalf("expected the duplicate provider names to be refused, got client with %s", client.Current().Selector())
	}
	for _, want := range []string{"kimi", "moonshot"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}
