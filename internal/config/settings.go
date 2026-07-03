package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type Settings struct {
	LLM        LLMSettings       `json:"llm"`
	WebSearch  WebSearchSettings `json:"webSearch"`
	Embedding  EmbeddingSettings `json:"embedding,omitempty"`
	MCP        MCPSettings       `json:"mcp"`
	Compaction Compaction        `json:"compaction"`
	Variables  map[string]string `json:"variables"`
}

type LLMSettings struct {
	DefaultProvider string                     `json:"defaultProvider"`
	DefaultModel    string                     `json:"defaultModel"`
	Providers       map[string]ProviderSetting `json:"providers"`
}

type ProviderSetting struct {
	APIKey  string   `json:"apiKey"`
	BaseURL string   `json:"baseUrl"`
	Models  []string `json:"models"`
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
	Type     string            `json:"type"`
	Command  string            `json:"command"`
	Args     []string          `json:"args"`
	Env      map[string]string `json:"env"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	Disabled bool              `json:"disabled"`
}

type Compaction struct {
	Enabled          bool `json:"enabled"`
	ReserveTokens    int  `json:"reserveTokens"`
	KeepRecentTokens int  `json:"keepRecentTokens"`
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
			Enabled:          true,
			ReserveTokens:    16384,
			KeepRecentTokens: 20000,
		},
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
	return settings, nil
}

func (l Loader) Save(settings Settings) error {
	if l.Path == "" {
		l.Path = DefaultSettingsPath()
	}
	normalize(&settings)
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(l.Path, append(data, '\n'), 0o644)
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
}
