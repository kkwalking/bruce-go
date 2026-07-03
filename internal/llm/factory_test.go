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
