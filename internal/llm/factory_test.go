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
