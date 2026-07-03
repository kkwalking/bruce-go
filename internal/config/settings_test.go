package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoaderLoadsCompatibleSettingJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.json")
	data := `{
  "llm": {
    "defaultProvider": "openai_compatiable",
    "defaultModel": "local-model",
    "providers": {
      "openai_compatiable": {
        "apiKey": "key",
        "baseUrl": "http://localhost:9000/v1",
        "models": ["local-model"]
      }
    }
  },
  "webSearch": {
    "provider": "searxng",
    "searxng": {"url": "http://search.local"}
  },
  "mcp": {
    "servers": {
      "demo": {
        "type": "stdio",
        "command": "demo-mcp",
        "args": ["--json"],
        "env": {"TOKEN": "x"}
      }
    }
  },
  "compaction": {
    "enabled": true,
    "reserveTokens": 1000,
    "keepRecentTokens": 2000
  },
  "variables": {
    "demoToken": "replace_me"
  }
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	settings, err := NewLoader(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.LLM.DefaultProvider != "openai_compatiable" || settings.LLM.DefaultModel != "local-model" {
		t.Fatalf("unexpected default model: %+v", settings.LLM)
	}
	if got := settings.LLM.Providers["openai_compatiable"].BaseURL; got != "http://localhost:9000/v1" {
		t.Fatalf("baseUrl = %q", got)
	}
	if got := settings.WebSearch.Searxng.URL; got != "http://search.local" {
		t.Fatalf("searxng url = %q", got)
	}
	if got := settings.MCP.Servers["demo"].Args[0]; got != "--json" {
		t.Fatalf("mcp arg = %q", got)
	}
	if settings.Compaction.ReserveTokens != 1000 || settings.Compaction.KeepRecentTokens != 2000 {
		t.Fatalf("unexpected compaction: %+v", settings.Compaction)
	}
	if got := settings.Variables["demoToken"]; got != "replace_me" {
		t.Fatalf("variable = %q", got)
	}
}

func TestLoaderDefaultsAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".bruce", "setting.json")
	settings, err := NewLoader(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.LLM.Providers == nil || settings.MCP.Servers == nil || settings.Variables == nil {
		t.Fatalf("maps should be initialized: %+v", settings)
	}
	settings.LLM.DefaultProvider = "deepseek"
	settings.LLM.DefaultModel = "deepseek-v4-flash"
	settings.LLM.Providers["deepseek"] = ProviderSetting{APIKey: "k"}
	if err := NewLoader(path).Save(settings); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewLoader(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LLM.DefaultProvider != "deepseek" {
		t.Fatalf("save/load provider = %q", reloaded.LLM.DefaultProvider)
	}
}
