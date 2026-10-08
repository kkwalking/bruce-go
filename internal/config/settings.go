package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Settings struct {
	LLM        LLMSettings       `json:"llm"`
	WebSearch  WebSearchSettings `json:"webSearch"`
	Embedding  EmbeddingSettings `json:"embedding,omitempty"`
	MCP        MCPSettings       `json:"mcp"`
	Compaction Compaction        `json:"compaction"`
	Sandbox    SandboxSettings   `json:"sandbox"`
	Plugins    PluginsSettings   `json:"plugins"`
	Variables  map[string]string `json:"variables"`
}

// PluginsSettings configures the JavaScript plugin system.
//
// Everything here is opt-in. A default settings file grants no plugin
// permission at all, so installing a plugin does not by itself give it
// filesystem, network or shell access.
type PluginsSettings struct {
	// Enabled turns plugin discovery on. Discovery is on by default; setting
	// it to false makes Bruce behave exactly as it did before plugins existed.
	Enabled *bool `json:"enabled,omitempty"`
	// Allow lists the permissions granted to every plugin. A plugin still has
	// to declare a permission in its manifest to receive it.
	Allow []string `json:"allow,omitempty"`
	// PerPlugin grants or denies permissions for one plugin by name. A denial
	// always wins.
	PerPlugin map[string]PluginPolicySetting `json:"perPlugin,omitempty"`
	// Deny removes permissions from every plugin, including ones Allow listed.
	Deny []string `json:"deny,omitempty"`
	// FailFast makes a broken manifest stop startup instead of producing a
	// diagnostic.
	FailFast bool `json:"failFast,omitempty"`
	// AllowDynamicCode turns eval and the Function constructor back on. It
	// exists for a trusted deployment and is off by default: runtime code
	// generation is the classic way out of a sandbox.
	AllowDynamicCode bool `json:"allowDynamicCode,omitempty"`
}

// PluginPolicySetting is the per-plugin permission override.
type PluginPolicySetting struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

type SandboxSettings struct {
	Mode                  string   `json:"mode"`
	NetworkAccess         bool     `json:"networkAccess"`
	AllowedEnv            []string `json:"allowedEnv,omitempty"`
	CommandTimeoutSeconds int      `json:"commandTimeoutSeconds,omitempty"`
}

type LLMSettings struct {
	DefaultProvider string                     `json:"defaultProvider"`
	DefaultModel    string                     `json:"defaultModel"`
	Providers       map[string]ProviderSetting `json:"providers"`
	ReasoningEffort string                     `json:"reasoningEffort,omitempty"`
}

type ProviderSetting struct {
	APIKey  string   `json:"apiKey"`
	BaseURL string   `json:"baseUrl"`
	Models  []string `json:"models"`
	// Protocol selects the wire format used to talk to this provider. It is
	// optional: an empty value is inferred from the provider name by
	// ResolveProtocol. It is never left unset internally, because it decides
	// which endpoint receives the API key.
	Protocol          string                     `json:"protocol,omitempty"`
	ModelCapabilities map[string]ModelCapability `json:"modelCapabilities,omitempty"`
}

// Wire protocols a provider can speak. These are the values accepted by the
// "protocol" field of a provider entry in setting.json.
const (
	// ProtocolOpenAIChat is the OpenAI Chat Completions shape: POST
	// {base}/chat/completions with a "messages" array. This is what the
	// built-in providers and every generic OpenAI-compatible gateway speak.
	ProtocolOpenAIChat = "openai_chat"
	// ProtocolOpenAIResponses is the OpenAI Responses shape: POST
	// {base}/responses with "input" items and "instructions".
	ProtocolOpenAIResponses = "openai_responses"
	// ProtocolAnthropic is the Anthropic Messages shape: POST {base}/v1/messages
	// with a top-level "system" field and content blocks.
	ProtocolAnthropic = "anthropic"
)

// protocolAliases maps the spellings a settings file may use onto the canonical
// protocol constants. Aliases exist because the field is hand-written and the
// obvious spellings differ from the canonical ones.
var protocolAliases = map[string]string{
	"openai":             ProtocolOpenAIChat,
	"openai_chat":        ProtocolOpenAIChat,
	"openai_compatible":  ProtocolOpenAIChat,
	"openai_compatiable": ProtocolOpenAIChat,
	"chat_completions":   ProtocolOpenAIChat,
	"responses":          ProtocolOpenAIResponses,
	"openai_responses":   ProtocolOpenAIResponses,
	"anthropic":          ProtocolAnthropic,
	"anthropic_messages": ProtocolAnthropic,
	"claude":             ProtocolAnthropic,
}

// Protocols returns the canonical protocol values, in the order they should be
// offered to a user choosing one.
func Protocols() []string {
	return []string{ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolAnthropic}
}

// NormalizeProtocol resolves a protocol value or alias to its canonical form.
// It reports whether the value was recognized; an empty value is not, so
// callers can distinguish "not set" from "invalid".
func NormalizeProtocol(value string) (string, bool) {
	canonical, ok := protocolAliases[strings.ToLower(strings.TrimSpace(value))]
	return canonical, ok
}

// ResolveProtocol returns the wire protocol for a provider, inferring one from
// the provider name when the entry does not state it.
//
// Inference exists so an existing setting.json keeps working after the field is
// introduced: built-in providers and generic gateways speak Chat Completions,
// while a provider named after Anthropic or Claude is far more likely to be
// spoken to in the Messages format. The inference is deliberately conservative
// — a name that mentions neither keeps the historical Chat Completions
// behaviour rather than guessing.
func ResolveProtocol(providerName string, setting ProviderSetting) string {
	if canonical, ok := NormalizeProtocol(setting.Protocol); ok {
		return canonical
	}
	name := strings.ToLower(strings.TrimSpace(providerName))
	if strings.Contains(name, "anthropic") || strings.Contains(name, "claude") {
		return ProtocolAnthropic
	}
	if strings.Contains(name, "responses") {
		return ProtocolOpenAIResponses
	}
	return ProtocolOpenAIChat
}

// ProviderNamePattern is the accepted shape of a provider name. The name is
// used as a /model selector component and as a settings-file object key, so it
// is restricted to the characters that survive both.
var ProviderNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

type ModelCapability struct {
	ContextWindow   int `json:"contextWindow,omitempty"`
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

// Environment variables that each enable a built-in provider on their own, with
// no entry in setting.json. A provider configured explicitly in setting.json is
// always preserved, including its API key.
const (
	DeepSeekAPIKeyEnv = "DEEPSEEK_API_KEY"
	GLMAPIKeyEnv      = "GLM_API_KEY"
	KimiAPIKeyEnv     = "MOONSHOT_API_KEY"
)

// Default models of the built-in providers. Each is the model selected when its
// provider is enabled by an environment variable alone.
const (
	DeepSeekDefaultModel = "deepseek-v4.1-flash"
	GLMDefaultModel      = "glm-5.1"
	KimiDefaultModel     = "kimi-k3"
)

// BuiltInProvider describes a provider that an environment variable can enable
// without any entry in setting.json.
type BuiltInProvider struct {
	// Name is the canonical provider name. It must stay identical to the value
	// llm.NormalizeProvider returns for this provider, because that is what the
	// model factory switches on.
	Name string
	// Env is the environment variable holding the API key.
	Env string
	// DefaultModel is the model used when this provider supplies the default.
	DefaultModel string
}

// builtInProviders lists the built-in providers in priority order. The first
// entry whose environment variable is set becomes the default provider when
// setting.json configures no default of its own; the order is fixed so that
// startup never depends on map iteration order.
var builtInProviders = []BuiltInProvider{
	{Name: "deepseek", Env: DeepSeekAPIKeyEnv, DefaultModel: DeepSeekDefaultModel},
	{Name: "glm", Env: GLMAPIKeyEnv, DefaultModel: GLMDefaultModel},
	{Name: "kimi", Env: KimiAPIKeyEnv, DefaultModel: KimiDefaultModel},
}

// BuiltInProviders returns the built-in providers in priority order.
func BuiltInProviders() []BuiltInProvider {
	return append([]BuiltInProvider(nil), builtInProviders...)
}

type WebSearchSettings struct {
	Provider string        `json:"provider"`
	Zhipu    ZhipuSearch   `json:"zhipu"`
	SerpAPI  SerpAPISearch `json:"serpapi"`
	Searxng  SearxngSearch `json:"searxng"`
}

type ZhipuSearch struct {
	APIKey       string `json:"apiKey"`
	SearchEngine string `json:"searchEngine"`
	ContentSize  string `json:"contentSize"`
	Endpoint     string `json:"endpoint"`
}

type SerpAPISearch struct {
	APIKey string `json:"apiKey"`
}

type SearxngSearch struct {
	URL string `json:"url"`
}

type EmbeddingSettings struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"baseUrl"`
	APIKey   string `json:"apiKey"`
}

type MCPSettings struct {
	Servers map[string]MCPServerSetting `json:"servers"`
}

type MCPServerSetting struct {
	Type       string            `json:"type"`
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers"`
	Disabled   bool              `json:"disabled"`
	ToolAccess map[string]string `json:"toolAccess,omitempty"`
}

type Compaction struct {
	Enabled            bool    `json:"enabled"`
	ContextWindowRatio float64 `json:"contextWindowRatio"`
	ReserveTokens      int     `json:"reserveTokens"`
	KeepRecentTokens   int     `json:"keepRecentTokens"`
}

func (c Compaction) Validate() error {
	if c.ContextWindowRatio <= 0 || c.ContextWindowRatio > 1 || math.IsNaN(c.ContextWindowRatio) {
		return errors.New("compaction.contextWindowRatio must be greater than 0 and at most 1")
	}
	if c.ReserveTokens < 0 {
		return errors.New("compaction.reserveTokens must not be negative")
	}
	return nil
}

func (c Compaction) Threshold(contextWindow int) (int, error) {
	if err := c.Validate(); err != nil {
		return 0, err
	}
	if contextWindow <= 0 {
		return 0, nil
	}
	usableWindow := int(math.Floor(float64(contextWindow) * c.ContextWindowRatio))
	threshold := usableWindow - c.ReserveTokens
	if threshold <= 0 {
		return 0, fmt.Errorf(
			"invalid automatic-compaction threshold: contextWindow(%d) * contextWindowRatio(%g) = %d; it must be greater than reserveTokens(%d)",
			contextWindow,
			c.ContextWindowRatio,
			usableWindow,
			c.ReserveTokens,
		)
	}
	return threshold, nil
}

func DefaultSettings() Settings {
	return Settings{
		LLM: LLMSettings{
			Providers: map[string]ProviderSetting{},
		},
		WebSearch: WebSearchSettings{
			Provider: "zhipu",
			Zhipu: ZhipuSearch{
				SearchEngine: "search_std",
				ContentSize:  "medium",
				Endpoint:     "https://open.bigmodel.cn/api/paas/v4/web_search",
			},
		},
		Embedding: EmbeddingSettings{
			Provider: "ollama",
			Model:    "nomic-embed-text:latest",
			BaseURL:  "http://localhost:11434",
		},
		MCP: MCPSettings{
			Servers: map[string]MCPServerSetting{},
		},
		Compaction: Compaction{
			Enabled:            true,
			ContextWindowRatio: 0.8,
			ReserveTokens:      16384,
			KeepRecentTokens:   20000,
		},
		Sandbox:   SandboxSettings{Mode: "full-access"},
		Plugins:   PluginsSettings{},
		Variables: map[string]string{},
	}
}

type Loader struct {
	Path string
}

func DefaultSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, ".bruce", "setting.json")
}

func NewLoader(path string) Loader {
	if path == "" {
		path = DefaultSettingsPath()
	}
	return Loader{Path: filepath.Clean(path)}
}

func (l Loader) Load() (Settings, error) {
	settings := DefaultSettings()
	if l.Path == "" {
		l.Path = DefaultSettingsPath()
	}
	data, err := os.ReadFile(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		applyEnvironmentDefaults(&settings)
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if len(data) == 0 {
		applyEnvironmentDefaults(&settings)
		return settings, nil
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return settings, err
	}
	normalize(&settings)
	applyEnvironmentDefaults(&settings)
	if err := settings.Compaction.Validate(); err != nil {
		return settings, err
	}
	if err := validateLLM(settings.LLM); err != nil {
		return settings, err
	}
	if err := validateSandbox(settings.Sandbox); err != nil {
		return settings, err
	}
	if err := validateAndNormalizeMCP(&settings.MCP); err != nil {
		return settings, err
	}
	if err := validatePlugins(&settings.Plugins); err != nil {
		return settings, err
	}
	return settings, nil
}

func (l Loader) Save(settings Settings) error {
	if l.Path == "" {
		l.Path = DefaultSettingsPath()
	}
	normalize(&settings)
	if err := settings.Compaction.Validate(); err != nil {
		return err
	}
	if err := validateLLM(settings.LLM); err != nil {
		return err
	}
	if err := validateSandbox(settings.Sandbox); err != nil {
		return err
	}
	if err := validateAndNormalizeMCP(&settings.MCP); err != nil {
		return err
	}
	if err := validatePlugins(&settings.Plugins); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(l.Path, append(data, '\n'), 0o644)
}
func validateLLM(settings LLMSettings) error {
	for providerName, provider := range settings.Providers {
		if !ProviderNamePattern.MatchString(providerName) {
			return fmt.Errorf(
				"invalid provider name %q in llm.providers: names must match %s",
				providerName, ProviderNamePattern.String(),
			)
		}
		// The protocol decides which endpoint receives the API key, so a value
		// that is not recognized is refused rather than silently replaced by
		// the inferred default.
		if strings.TrimSpace(provider.Protocol) != "" {
			if _, ok := NormalizeProtocol(provider.Protocol); !ok {
				return fmt.Errorf(
					"llm.providers.%s.protocol %q is not a known protocol (allowed values: %s)",
					providerName, provider.Protocol, strings.Join(Protocols(), ", "),
				)
			}
		}
		declared := make(map[string]bool, len(provider.Models))
		for _, model := range provider.Models {
			model = strings.TrimSpace(model)
			if model != "" {
				declared[model] = true
			}
		}
		for model, capability := range provider.ModelCapabilities {
			model = strings.TrimSpace(model)
			if model == "" {
				return fmt.Errorf("llm.providers.%s.modelCapabilities contains an empty model name", providerName)
			}
			if !declared[model] {
				return fmt.Errorf("llm.providers.%s.modelCapabilities[%q] is not declared in models", providerName, model)
			}
			if capability.ContextWindow < 0 {
				return fmt.Errorf("llm.providers.%s.modelCapabilities[%q].contextWindow must be positive", providerName, model)
			}
			if capability.MaxOutputTokens < 0 {
				return fmt.Errorf("llm.providers.%s.modelCapabilities[%q].maxOutputTokens must be positive", providerName, model)
			}
			if capability.ContextWindow == 0 && capability.MaxOutputTokens == 0 {
				return fmt.Errorf("llm.providers.%s.modelCapabilities[%q] must configure at least one model capability", providerName, model)
			}
		}
	}
	return nil
}

// normalizeProviders lower-cases provider names, canonicalizes protocol values
// and trims the declared model lists. It is applied on both load and save, so a
// hand-written settings file and one written by Bruce converge on the same
// shape.
//
// An entry whose name is empty is dropped: the JSON object key is legal but
// nothing can address it, and validateLLM would reject it as an invalid name.
func normalizeProviders(settings *Settings) {
	if settings.LLM.Providers == nil {
		settings.LLM.Providers = map[string]ProviderSetting{}
		return
	}
	normalized := make(map[string]ProviderSetting, len(settings.LLM.Providers))
	for name, provider := range settings.LLM.Providers {
		trimmed := strings.ToLower(strings.TrimSpace(name))
		if trimmed == "" {
			continue
		}
		if canonical, ok := NormalizeProtocol(provider.Protocol); ok {
			provider.Protocol = canonical
		} else {
			// Leave an unrecognized value untouched so validateLLM can report
			// it instead of the loader silently accepting a typo.
			provider.Protocol = strings.TrimSpace(provider.Protocol)
		}
		provider.Models = normalizeModelList(provider.Models)
		normalized[trimmed] = provider
	}
	settings.LLM.Providers = normalized
}

func normalizeModelList(models []string) []string {
	seen := make(map[string]bool, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		out = append(out, model)
	}
	return out
}

func ResolveUserPath(value string) string {
	if value == "" {
		value = "."
	}
	home, _ := os.UserHomeDir()
	if value == "~" {
		return filepath.Clean(home)
	}
	if len(value) >= 2 && value[:2] == "~/" {
		return filepath.Join(home, value[2:])
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return filepath.Clean(value)
	}
	return abs
}

func normalize(settings *Settings) {
	normalizeProviders(settings)
	if settings.WebSearch.Provider == "" {
		settings.WebSearch.Provider = "zhipu"
	}
	if settings.WebSearch.Zhipu.SearchEngine == "" {
		settings.WebSearch.Zhipu.SearchEngine = "search_std"
	}
	if settings.WebSearch.Zhipu.ContentSize == "" {
		settings.WebSearch.Zhipu.ContentSize = "medium"
	}
	if settings.WebSearch.Zhipu.Endpoint == "" {
		settings.WebSearch.Zhipu.Endpoint = "https://open.bigmodel.cn/api/paas/v4/web_search"
	}
	if settings.MCP.Servers == nil {
		settings.MCP.Servers = map[string]MCPServerSetting{}
	}
	if settings.Compaction.ReserveTokens == 0 {
		settings.Compaction.ReserveTokens = 16384
	}
	if settings.Compaction.KeepRecentTokens == 0 {
		settings.Compaction.KeepRecentTokens = 20000
	}
	if settings.Variables == nil {
		settings.Variables = map[string]string{}
	}
	if strings.TrimSpace(settings.Sandbox.Mode) == "" {
		settings.Sandbox.Mode = "full-access"
	}
	if settings.Plugins.PerPlugin == nil {
		settings.Plugins.PerPlugin = map[string]PluginPolicySetting{}
	}
	settings.Sandbox.AllowedEnv = normalizeAllowedEnv(settings.Sandbox.AllowedEnv)
}

// applyEnvironmentDefaults registers a built-in provider for every built-in
// provider whose environment variable holds an API key. A provider that is
// already configured explicitly in setting.json is never touched, including
// when its apiKey is empty — an explicit entry always wins over the
// environment.
//
// Registering is not the same as selecting: the environment only supplies the
// default provider when setting.json names no default of its own. When a
// default is configured, the environment-provided providers are still added as
// candidates, so they can be reached with /model, but they never displace the
// configured default.
func applyEnvironmentDefaults(settings *Settings) {
	hasExplicitDefault := strings.TrimSpace(settings.LLM.DefaultProvider) != ""
	for _, provider := range builtInProviders {
		apiKey := strings.TrimSpace(os.Getenv(provider.Env))
		if apiKey == "" {
			continue
		}
		if _, exists := settings.LLM.Providers[provider.Name]; exists {
			continue
		}
		settings.LLM.Providers[provider.Name] = ProviderSetting{
			APIKey: apiKey,
			Models: []string{provider.DefaultModel},
		}
		if hasExplicitDefault {
			continue
		}
		// First built-in provider with a key becomes the default. hasExplicitDefault
		// is updated so a lower-priority provider cannot overwrite it.
		settings.LLM.DefaultProvider = provider.Name
		settings.LLM.DefaultModel = provider.DefaultModel
		hasExplicitDefault = true
	}
}

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateSandbox(settings SandboxSettings) error {
	switch settings.Mode {
	case "read-only", "workspace-write", "full-access":
	default:
		return fmt.Errorf("invalid sandbox.mode: %q (allowed values: read-only, workspace-write, full-access)", settings.Mode)
	}
	for _, name := range settings.AllowedEnv {
		if !environmentNamePattern.MatchString(name) {
			return fmt.Errorf("sandbox.allowedEnv contains an invalid environment variable name: %q", name)
		}
	}
	if settings.CommandTimeoutSeconds < 0 {
		return fmt.Errorf("sandbox.commandTimeoutSeconds must not be negative: %d", settings.CommandTimeoutSeconds)
	}
	return nil
}

func normalizeAllowedEnv(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func validateAndNormalizeMCP(settings *MCPSettings) error {
	if settings == nil {
		return nil
	}
	for serverName, server := range settings.Servers {
		normalized := make(map[string]string, len(server.ToolAccess))
		for rawName, rawAccess := range server.ToolAccess {
			name := strings.TrimSpace(rawName)
			access := strings.TrimSpace(rawAccess)
			if name == "" {
				return fmt.Errorf("mcp.servers.%s.toolAccess contains an empty tool name", serverName)
			}
			if strings.ContainsAny(name, "*?[]") {
				return fmt.Errorf("mcp.servers.%s.toolAccess does not support wildcard tool names: %q", serverName, rawName)
			}
			if _, exists := normalized[name]; exists {
				return fmt.Errorf("mcp.servers.%s.toolAccess contains a duplicate tool name after trimming whitespace: %q", serverName, name)
			}
			switch access {
			case "read-only", "workspace-write", "full-access":
			default:
				return fmt.Errorf("invalid mcp.servers.%s.toolAccess[%q]: %q (allowed values: read-only, workspace-write, full-access)", serverName, name, rawAccess)
			}
			if isHTTPMCPType(server.Type) && access == "workspace-write" {
				return fmt.Errorf("mcp.servers.%s.toolAccess[%q] cannot be workspace-write: HTTP MCP cannot enforce workspace filesystem boundaries", serverName, name)
			}
			normalized[name] = access
		}
		if len(normalized) == 0 {
			server.ToolAccess = nil
		} else {
			server.ToolAccess = normalized
		}
		settings.Servers[serverName] = server
	}
	return nil
}

// PluginPermissions lists the permission tokens a settings file may use. It is
// duplicated from internal/plugin on purpose: config must not import the
// plugin package, which would make the settings loader depend on the plugin
// runtime.
func PluginPermissions() []string {
	return []string{"fs.read", "fs.write", "net", "shell", "storage", "events"}
}

func validatePlugins(settings *PluginsSettings) error {
	if settings == nil {
		return nil
	}
	valid := map[string]bool{}
	for _, permission := range PluginPermissions() {
		valid[permission] = true
	}
	check := func(scope string, values []string) error {
		for _, value := range values {
			if !valid[strings.TrimSpace(value)] {
				return fmt.Errorf("%s contains an invalid plugin permission: %q (allowed values: %s)",
					scope, value, strings.Join(PluginPermissions(), ", "))
			}
		}
		return nil
	}
	if err := check("plugins.allow", settings.Allow); err != nil {
		return err
	}
	if err := check("plugins.deny", settings.Deny); err != nil {
		return err
	}
	for name, policy := range settings.PerPlugin {
		if strings.TrimSpace(name) == "" {
			return errors.New("plugins.perPlugin contains an empty plugin name")
		}
		if err := check("plugins.perPlugin."+name+".allow", policy.Allow); err != nil {
			return err
		}
		if err := check("plugins.perPlugin."+name+".deny", policy.Deny); err != nil {
			return err
		}
	}
	return nil
}

func isHTTPMCPType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "http", "streamable_http", "streamable-http", "streamablehttp":
		return true
	default:
		return false
	}
}
