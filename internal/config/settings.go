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
	Variables  map[string]string `json:"variables"`
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
	APIKey            string                     `json:"apiKey"`
	BaseURL           string                     `json:"baseUrl"`
	Models            []string                   `json:"models"`
	ModelCapabilities map[string]ModelCapability `json:"modelCapabilities,omitempty"`
}

type ModelCapability struct {
	ContextWindow   int `json:"contextWindow,omitempty"`
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
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
		return errors.New("compaction.contextWindowRatio 必须大于 0 且不超过 1")
	}
	if c.ReserveTokens < 0 {
		return errors.New("compaction.reserveTokens 不能为负数")
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
			"compaction 自动压缩阈值无效: contextWindow(%d) × contextWindowRatio(%g) = %d，必须大于 reserveTokens(%d)",
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
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if len(data) == 0 {
		return settings, nil
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return settings, err
	}
	normalize(&settings)
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
				return fmt.Errorf("llm.providers.%s.modelCapabilities 包含空模型名", providerName)
			}
			if !declared[model] {
				return fmt.Errorf("llm.providers.%s.modelCapabilities[%q] 未在 models 中声明", providerName, model)
			}
			if capability.ContextWindow < 0 {
				return fmt.Errorf("llm.providers.%s.modelCapabilities[%q].contextWindow 必须为正数", providerName, model)
			}
			if capability.MaxOutputTokens < 0 {
				return fmt.Errorf("llm.providers.%s.modelCapabilities[%q].maxOutputTokens 必须为正数", providerName, model)
			}
			if capability.ContextWindow == 0 && capability.MaxOutputTokens == 0 {
				return fmt.Errorf("llm.providers.%s.modelCapabilities[%q] 至少配置一个模型能力", providerName, model)
			}
		}
	}
	return nil
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
	if settings.LLM.Providers == nil {
		settings.LLM.Providers = map[string]ProviderSetting{}
	}
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
	settings.Sandbox.AllowedEnv = normalizeAllowedEnv(settings.Sandbox.AllowedEnv)
}

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateSandbox(settings SandboxSettings) error {
	switch settings.Mode {
	case "read-only", "workspace-write", "full-access":
	default:
		return fmt.Errorf("sandbox.mode 无效: %q（允许 read-only、workspace-write、full-access）", settings.Mode)
	}
	for _, name := range settings.AllowedEnv {
		if !environmentNamePattern.MatchString(name) {
			return fmt.Errorf("sandbox.allowedEnv 包含非法环境变量名: %q", name)
		}
	}
	if settings.CommandTimeoutSeconds < 0 {
		return fmt.Errorf("sandbox.commandTimeoutSeconds 不能为负数: %d", settings.CommandTimeoutSeconds)
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
				return fmt.Errorf("mcp.servers.%s.toolAccess 包含空工具名", serverName)
			}
			if strings.ContainsAny(name, "*?[]") {
				return fmt.Errorf("mcp.servers.%s.toolAccess 不支持通配符工具名: %q", serverName, rawName)
			}
			if _, exists := normalized[name]; exists {
				return fmt.Errorf("mcp.servers.%s.toolAccess 工具名去除空白后重复: %q", serverName, name)
			}
			switch access {
			case "read-only", "workspace-write", "full-access":
			default:
				return fmt.Errorf("mcp.servers.%s.toolAccess[%q] 无效: %q（允许 read-only、workspace-write、full-access）", serverName, name, rawAccess)
			}
			if isHTTPMCPType(server.Type) && access == "workspace-write" {
				return fmt.Errorf("mcp.servers.%s.toolAccess[%q] 不能设为 workspace-write：HTTP MCP 无法强制 workspace 文件边界", serverName, name)
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

func isHTTPMCPType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "http", "streamable_http", "streamable-http", "streamablehttp":
		return true
	default:
		return false
	}
}
