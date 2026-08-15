package llm

import (
	"path/filepath"
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
	for i := 2; i < len(options); i++ {
		prev := options[i-1].Selector()
		curr := options[i].Selector()
		if prev > curr {
			t.Fatalf("remaining options are not sorted: %q before %q", prev, curr)
		}
	}

	if _, err := client.Switch("deepseek/deepseek-v4-pro"); err != nil {
		t.Fatal(err)
	}
	options = client.Options()
	if got := options[0].Selector(); got != "deepseek/deepseek-v4-pro" {
		t.Fatalf("first option after switch = %q, want deepseek/deepseek-v4-pro", got)
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
