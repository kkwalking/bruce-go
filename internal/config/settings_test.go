package config

import (
	"os"
	"path/filepath"
	"strings"
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
        "env": {"TOKEN": "x"},
        "toolAccess": {
          " read_file ": " read-only ",
          "write_file": "workspace-write"
        }
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
	if got := settings.MCP.Servers["demo"].ToolAccess["read_file"]; got != "read-only" {
		t.Fatalf("mcp read_file access = %q", got)
	}
	if settings.Compaction.ReserveTokens != 1000 || settings.Compaction.KeepRecentTokens != 2000 {
		t.Fatalf("unexpected compaction: %+v", settings.Compaction)
	}
	if got := settings.Variables["demoToken"]; got != "replace_me" {
		t.Fatalf("variable = %q", got)
	}
}

func TestLoaderModelCapabilitiesCompatibilityAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setting.json")
	valid := `{"llm":{"providers":{"openai_compatiable":{"apiKey":"k","baseUrl":"http://localhost/v1","models":["local-model"],"modelCapabilities":{"local-model":{"contextWindow":128000,"maxOutputTokens":8192}}}}}}`
	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	settings, err := NewLoader(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	capability := settings.LLM.Providers["openai_compatiable"].ModelCapabilities["local-model"]
	if capability.ContextWindow != 128000 || capability.MaxOutputTokens != 8192 {
		t.Fatalf("capability = %+v", capability)
	}

	cases := []string{
		`{"llm":{"providers":{"p":{"models":["declared"],"modelCapabilities":{"other":{"contextWindow":1}}}}}}`,
		`{"llm":{"providers":{"p":{"models":["declared"],"modelCapabilities":{"declared":{"contextWindow":-1}}}}}}`,
		`{"llm":{"providers":{"p":{"models":["declared"],"modelCapabilities":{"declared":{}}}}}}`,
	}
	for _, data := range cases {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := NewLoader(path).Load(); err == nil {
			t.Fatalf("invalid capability should fail: %s", data)
		}
	}
}

func TestLoaderValidatesMCPToolAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setting.json")
	cases := []struct {
		name string
		data string
		want string
	}{
		{
			name: "invalid mode",
			data: `{"mcp":{"servers":{"demo":{"type":"stdio","toolAccess":{"write":"unsafe"}}}}}`,
			want: "mcp.servers.demo.toolAccess",
		},
		{
			name: "wildcard",
			data: `{"mcp":{"servers":{"demo":{"type":"stdio","toolAccess":{"read_*":"read-only"}}}}}`,
			want: "不支持通配符",
		},
		{
			name: "trim collision",
			data: `{"mcp":{"servers":{"demo":{"type":"stdio","toolAccess":{"read":"read-only"," read ":"read-only"}}}}}`,
			want: "重复",
		},
		{
			name: "http workspace write",
			data: `{"mcp":{"servers":{"remote":{"type":"streamable-http","toolAccess":{"write":"workspace-write"}}}}}`,
			want: "HTTP MCP 无法强制 workspace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.data), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := NewLoader(path).Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLegacyMCPConfigKeepsToolAccessEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setting.json")
	data := `{"mcp":{"servers":{"demo":{"type":"stdio","command":"demo"}}}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	settings, err := NewLoader(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.MCP.Servers["demo"].ToolAccess != nil {
		t.Fatalf("legacy toolAccess = %#v", settings.MCP.Servers["demo"].ToolAccess)
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
	if settings.Sandbox.Mode != "full-access" || settings.Sandbox.NetworkAccess {
		t.Fatalf("unexpected sandbox defaults: %+v", settings.Sandbox)
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

func TestLoaderValidatesSandboxSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setting.json")
	data := `{"sandbox":{"mode":"unsafe","allowedEnv":["OK","BAD=VALUE"]}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLoader(path).Load(); err == nil {
		t.Fatal("invalid sandbox settings should fail")
	}
	data = `{"sandbox":{"mode":"danger-full-access"}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLoader(path).Load(); err == nil {
		t.Fatal("legacy danger-full-access setting should fail")
	}

	data = `{"sandbox":{"mode":"read-only","networkAccess":true,"allowedEnv":[" EXTRA_TOKEN ","EXTRA_TOKEN"]}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	settings, err := NewLoader(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Sandbox.Mode != "read-only" || !settings.Sandbox.NetworkAccess {
		t.Fatalf("sandbox = %+v", settings.Sandbox)
	}
	if len(settings.Sandbox.AllowedEnv) != 1 || settings.Sandbox.AllowedEnv[0] != "EXTRA_TOKEN" {
		t.Fatalf("allowedEnv = %#v", settings.Sandbox.AllowedEnv)
	}

	data = `{"sandbox":{"mode":"read-only","commandTimeoutSeconds":-5}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLoader(path).Load(); err == nil {
		t.Fatal("negative commandTimeoutSeconds should fail")
	}
	data = `{"sandbox":{"mode":"read-only","commandTimeoutSeconds":120}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	settings, err = NewLoader(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Sandbox.CommandTimeoutSeconds != 120 {
		t.Fatalf("commandTimeoutSeconds = %d", settings.Sandbox.CommandTimeoutSeconds)
	}
}

func TestLoaderReasoningEffortField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.json")
	data := `{
  "llm": {
    "defaultProvider": "deepseek",
    "defaultModel": "deepseek-v4-flash",
    "reasoningEffort": "high",
    "providers": {
      "deepseek": {
        "apiKey": "key",
        "models": ["deepseek-v4-flash"]
      }
    }
  },
  "webSearch": {},
  "mcp": {"servers": {}},
  "compaction": {}
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	settings, err := NewLoader(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := settings.LLM.ReasoningEffort; got != "high" {
		t.Fatalf("reasoningEffort = %q, want high", got)
	}

	// Empty field (omitted from JSON) should not panic
	settings2 := DefaultSettings()
	if got := settings2.LLM.ReasoningEffort; got != "" {
		t.Fatalf("default reasoningEffort = %q, want empty", got)
	}
}
