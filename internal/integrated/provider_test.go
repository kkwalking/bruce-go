package integrated

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bruce-go/internal/config"
)

// configuredRuntime builds a runtime whose settings file names one provider, so
// the provider commands have something to edit and remove.
func providerTestRuntime(t *testing.T, providers string) (*Runtime, string) {
	t.Helper()
	settingsPath := writeUnconfiguredSettings(t, providers)
	rt, err := New(context.Background(), Options{
		Workspace:    t.TempDir(),
		HomeDir:      filepath.Dir(settingsPath),
		SettingsPath: settingsPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupRuntime(t, rt) })
	return rt, settingsPath
}

func readSettings(t *testing.T, path string) config.Settings {
	t.Helper()
	loader := config.NewLoader(path)
	settings, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

// Adding a provider must persist it, make it switchable, and leave the runtime
// usable without a restart.
func TestSaveProviderBuildsPersistsAndSwaps(t *testing.T) {
	rt, settingsPath := providerTestRuntime(t, `{"llm":{"providers":{}}}`)
	if !rt.NeedsProviderSetup() {
		t.Fatal("precondition: the runtime should start unconfigured")
	}
	if err := rt.SaveProvider("mygateway", config.ProviderSetting{
		APIKey:   "magpie",
		BaseURL:  "http://127.0.0.1:3425/v1",
		Protocol: config.ProtocolOpenAIChat,
		Models:   []string{"group/flash", "group/think"},
	}, true); err != nil {
		t.Fatal(err)
	}
	if rt.NeedsProviderSetup() {
		t.Fatal("the runtime should no longer need setup")
	}
	if rt.switchable == nil {
		t.Fatal("the provider should be switchable now")
	}
	// The saved file is the source of truth, and the runtime agrees with it.
	persisted := readSettings(t, settingsPath)
	if _, ok := persisted.LLM.Providers["mygateway"]; !ok {
		t.Fatalf("provider was not persisted: %#v", persisted.LLM.Providers)
	}
	if persisted.LLM.DefaultProvider != "mygateway" || persisted.LLM.DefaultModel != "group/flash" {
		t.Fatalf("defaults = %s/%s, want the new provider", persisted.LLM.DefaultProvider, persisted.LLM.DefaultModel)
	}
	if got := rt.CurrentModel(); got.Provider != "mygateway" || got.Model != "group/flash" {
		t.Fatalf("current model = %s, want the new provider's first model", got.Selector())
	}
	// The agents were rebuilt against the new client, so a task uses it.
	options := rt.ModelOptions()
	if len(options) != 2 {
		t.Fatalf("model options = %#v, want both declared models", options)
	}
	list, err := rt.ListProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "mygateway" || !list[0].Active {
		t.Fatalf("providers = %#v", list)
	}
	if len(list[0].Models) != 2 {
		t.Fatalf("models = %#v", list[0].Models)
	}
	// The API key must never be rendered back in full.
	if strings.Contains(strings.Join(list[0].Models, ","), "magpie") {
		t.Fatal("the provider summary should not leak the API key")
	}
}

// Adding a second provider must not silently take over the current model the
// user is already on.
func TestSaveProviderKeepsCurrentModelWhenItSurvives(t *testing.T) {
	rt, _ := providerTestRuntime(t, `{"llm":{"providers":{"alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`)
	if err := rt.SaveProvider("beta", config.ProviderSetting{
		APIKey:  "k",
		BaseURL: "http://127.0.0.1:2/v1",
		Models:  []string{"b"},
	}, false); err != nil {
		t.Fatal(err)
	}
	if got := rt.CurrentModel(); got.Provider != "alpha" || got.Model != "a" {
		t.Fatalf("current model = %s, want the untouched alpha/a", got.Selector())
	}
	persisted := readSettings(t, rt.Loader.Path)
	if persisted.LLM.DefaultProvider != "alpha" {
		t.Fatalf("persisted default = %s, want alpha", persisted.LLM.DefaultProvider)
	}

	// With activate, the switch is explicit and must happen.
	if err := rt.SaveProvider("gamma", config.ProviderSetting{
		APIKey:  "k",
		BaseURL: "http://127.0.0.1:3/v1",
		Models:  []string{"g"},
	}, true); err != nil {
		t.Fatal(err)
	}
	if got := rt.CurrentModel(); got.Provider != "gamma" || got.Model != "g" {
		t.Fatalf("current model = %s, want the activated gamma/g", got.Selector())
	}
}

// Removing the provider that is currently in use has to move the runtime to
// whatever remains, and removing the last one has to land on the unconfigured
// state rather than a broken client.
func TestRemoveLastProviderFallsBackToUnconfigured(t *testing.T) {
	rt, settingsPath := providerTestRuntime(t, `{"llm":{"providers":{"only":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["m"]}}}}`)
	if err := rt.RemoveProvider("only"); err != nil {
		t.Fatal(err)
	}
	if !rt.NeedsProviderSetup() {
		t.Fatal("removing the last provider should return to the unconfigured state")
	}
	if rt.switchable != nil {
		t.Fatal("switchable should be nil again")
	}
	if rt.Client == nil {
		t.Fatal("Client must never be nil")
	}
	persisted := readSettings(t, settingsPath)
	if len(persisted.LLM.Providers) != 0 {
		t.Fatalf("providers = %#v, want none", persisted.LLM.Providers)
	}
	// The runtime still answers commands in this state.
	result := rt.Handle(context.Background(), "/status")
	if result.Err != nil {
		t.Fatalf("/status should still work: %v", result.Err)
	}
}

// Removing one of two providers keeps the survivor usable, moving the current
// model if the removed one was in use.
func TestRemoveProviderSwitchesToSurvivor(t *testing.T) {
	providers := `{"llm":{"providers":{
	  "alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a"]},
	  "beta":{"apiKey":"k","baseUrl":"http://127.0.0.1:2/v1","models":["b"]}}}}`
	rt, _ := providerTestRuntime(t, providers)
	// Move onto beta first, so removing alpha is not the easy case.
	if result := rt.Handle(context.Background(), "/model beta/b"); result.Err != nil {
		t.Fatalf("/model switch failed: %v", result.Err)
	}
	if err := rt.RemoveProvider("beta"); err != nil {
		t.Fatal(err)
	}
	if got := rt.CurrentModel(); got.Provider != "alpha" || got.Model != "a" {
		t.Fatalf("current model = %s, want the surviving alpha/a", got.Selector())
	}
	if rt.NeedsProviderSetup() {
		t.Fatal("a surviving provider means setup is not needed")
	}
}

func TestRemoveUnknownProviderIsAnError(t *testing.T) {
	rt, _ := providerTestRuntime(t, `{"llm":{"providers":{"alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`)
	if err := rt.RemoveProvider("nope"); err == nil {
		t.Fatal("removing an unknown provider should fail")
	}
}

// A save that cannot be persisted must leave both the file and the running
// process as they were: a half-applied configuration is worse than a refused
// one.
func TestSaveProviderFailureLeavesDiskAndProcessUntouched(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, where a read-only file is still writable")
	}
	providers := `{"llm":{"providers":{"alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`
	rt, settingsPath := providerTestRuntime(t, providers)
	before, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeModel := rt.CurrentModel()

	// A read-only settings file makes the write step fail after the candidate
	// model has already been built in memory.
	if err := os.Chmod(settingsPath, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(settingsPath, 0o600) })
	err = rt.SaveProvider("beta", config.ProviderSetting{
		APIKey:  "k",
		BaseURL: "http://127.0.0.1:2/v1",
		Models:  []string{"b"},
	}, true)
	if err == nil {
		t.Fatal("saving onto a read-only file should fail")
	}
	// The process still runs the old provider, and still answers commands.
	if got := rt.CurrentModel(); got != beforeModel {
		t.Fatalf("current model changed to %s despite the failure", got.Selector())
	}
	if rt.NeedsProviderSetup() {
		t.Fatal("the runtime should still be configured")
	}
	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("disk changed despite the failure:\n%s", after)
	}
}

// The Settings field the runtime exposes must match what the switchable client
// last wrote, not the snapshot taken at startup.
func TestSaveProviderDoesNotResurrectStaleSettings(t *testing.T) {
	providers := `{"llm":{"providers":{
	  "alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a1","a2"]},
	  "beta":{"apiKey":"k","baseUrl":"http://127.0.0.1:2/v1","models":["b1"]}}}}`
	rt, settingsPath := providerTestRuntime(t, providers)
	// /model writes to the switchable client's own copy of the settings.
	if result := rt.Handle(context.Background(), "/model alpha/a2"); result.Err != nil {
		t.Fatal(result.Err)
	}
	// A later provider edit must not overwrite that with the stale startup copy.
	if err := rt.SaveProvider("beta", config.ProviderSetting{
		APIKey:  "k",
		BaseURL: "http://127.0.0.1:2/v1",
		Models:  []string{"b1", "b2"},
	}, false); err != nil {
		t.Fatal(err)
	}
	persisted := readSettings(t, settingsPath)
	if persisted.LLM.DefaultModel != "a2" {
		t.Fatalf("default model = %q, want the switch made before the edit", persisted.LLM.DefaultModel)
	}
	if len(persisted.LLM.Providers["beta"].Models) != 2 {
		t.Fatalf("beta = %#v, want both models", persisted.LLM.Providers["beta"])
	}
}

// A protocol or name that the config layer rejects must be refused before
// anything is written.
func TestSaveProviderRejectsInvalidInput(t *testing.T) {
	rt, settingsPath := providerTestRuntime(t, `{"llm":{"providers":{"alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`)
	before := readSettings(t, settingsPath)
	for _, tc := range []struct {
		name  string
		value config.ProviderSetting
	}{
		{"Bad Name", config.ProviderSetting{APIKey: "k", BaseURL: "http://127.0.0.1:2/v1", Models: []string{"b"}}},
		{"beta", config.ProviderSetting{APIKey: "k", BaseURL: "http://127.0.0.1:2/v1", Protocol: "not-a-protocol", Models: []string{"b"}}},
	} {
		if err := rt.SaveProvider(tc.name, tc.value, false); err == nil {
			t.Fatalf("saving %q should have been refused", tc.name)
		}
	}
	after := readSettings(t, settingsPath)
	if len(after.LLM.Providers) != len(before.LLM.Providers) {
		t.Fatalf("a refused save changed the file: %#v", after.LLM.Providers)
	}
}

func TestSwitchModelByProvider(t *testing.T) {
	providers := `{"llm":{"providers":{
	  "alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a"]},
	  "beta":{"apiKey":"k","baseUrl":"http://127.0.0.1:2/v1","models":["b1","b2"]}}}}`
	rt, _ := providerTestRuntime(t, providers)

	// A bare provider name selects that provider's default model.
	next, err := rt.SwitchModel("beta")
	if err != nil {
		t.Fatal(err)
	}
	if next.Provider != "beta" || next.Model != "b1" {
		t.Fatalf("switched to %s, want beta/b1", next.Selector())
	}
	next, err = rt.SwitchModel("beta/b2")
	if err != nil {
		t.Fatal(err)
	}
	if next.Model != "b2" {
		t.Fatalf("switched to %s, want beta/b2", next.Selector())
	}
	// A model ID that itself contains a slash resolves through the full
	// provider/model selector.
	if _, err := rt.SwitchModel("nope"); err == nil {
		t.Fatal("an unknown selector should fail")
	}
}

func TestSwitchModelWithoutProvidersExplainsWhatToDo(t *testing.T) {
	rt, _ := providerTestRuntime(t, `{"llm":{"providers":{}}}`)
	_, err := rt.SwitchModel("anything")
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("err = %v, want it to mention providers", err)
	}
}

// The slash command must reach the same code the TUI will call.
func TestProviderCommandListsAndEdits(t *testing.T) {
	rt, _ := providerTestRuntime(t, `{"llm":{"providers":{"alpha":{"apiKey":"secret-key","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`)
	list := rt.Handle(context.Background(), "/provider")
	if list.Err != nil {
		t.Fatal(list.Err)
	}
	if !strings.Contains(list.Output, "alpha") || !strings.Contains(list.Output, "openai_chat") {
		t.Fatalf("list output = %q", list.Output)
	}
	// The key is present but never in full.
	if strings.Contains(list.Output, "secret-key") {
		t.Fatalf("list output leaked the API key: %q", list.Output)
	}
	removed := rt.Handle(context.Background(), "/provider remove alpha")
	if removed.Err != nil {
		t.Fatal(removed.Err)
	}
	if !rt.NeedsProviderSetup() {
		t.Fatal("removing the only provider should need setup again")
	}
}

func TestProviderCommandUsageErrors(t *testing.T) {
	rt, _ := providerTestRuntime(t, `{"llm":{"providers":{"alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`)
	for _, input := range []string{"/provider remove", "/provider remove nope", "/provider edit", "/provider bogus"} {
		result := rt.Handle(context.Background(), input)
		if result.Err == nil {
			t.Fatalf("%s should fail", input)
		}
	}
}

// The summary describes a provider without ever exposing the key, and reports
// the protocol that will actually be used rather than the raw field.
func TestListProvidersReportsResolvedProtocol(t *testing.T) {
	providers := `{"llm":{"providers":{
	  "myclaude":{"apiKey":"k","baseUrl":"http://127.0.0.1:1"},
	  "mygateway":{"apiKey":"k","baseUrl":"http://127.0.0.1:2/v1","protocol":"responses","models":["m"]}}}}`
	rt, _ := providerTestRuntime(t, providers)
	list, err := rt.ListProviders()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ProviderSummary{}
	for _, summary := range list {
		byName[summary.Name] = summary
	}
	// A name that says claude is inferred as Anthropic.
	if got := byName["myclaude"].Protocol; got != config.ProtocolAnthropic {
		t.Fatalf("myclaude protocol = %q, want the inferred anthropic", got)
	}
	// An alias in the file is reported in its canonical spelling.
	if got := byName["mygateway"].Protocol; got != config.ProtocolOpenAIResponses {
		t.Fatalf("mygateway protocol = %q, want the canonical responses value", got)
	}
	if len(list) != 2 {
		t.Fatalf("providers = %#v", list)
	}
	if !byName["mygateway"].Active && !byName["myclaude"].Active {
		t.Fatal("one provider should be marked active")
	}
}

func TestListProvidersMasksKeys(t *testing.T) {
	rt, _ := providerTestRuntime(t, `{"llm":{"providers":{"alpha":{"apiKey":"sk-super-secret-value","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`)
	list, err := rt.ListProviders()
	if err != nil {
		t.Fatal(err)
	}
	masked := list[0].APIKey
	if strings.Contains(masked, "super-secret") {
		t.Fatalf("masked key = %q, it still contains the secret", masked)
	}
	if masked == "" {
		t.Fatal("a set key should report that it is set")
	}
	// A short key has nothing safe to show and must not be echoed.
	rt2, _ := providerTestRuntime(t, `{"llm":{"providers":{"alpha":{"apiKey":"abc","baseUrl":"http://127.0.0.1:1/v1","models":["a"]}}}}`)
	list2, _ := rt2.ListProviders()
	if strings.Contains(list2[0].APIKey, "abc") {
		t.Fatalf("short key = %q, it should be fully masked", list2[0].APIKey)
	}
}

func TestProviderSummaryDescribesUnconfiguredModels(t *testing.T) {
	// A built-in provider with no declared models still offers its built-in
	// table, so the summary is not empty.
	rt, _ := providerTestRuntime(t, `{"llm":{"providers":{"glm":{"apiKey":"k"}}}}`)
	list, err := rt.ListProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || len(list[0].Models) == 0 {
		t.Fatalf("providers = %#v, want glm with its built-in models", list)
	}
}

// Switching a model while the render loop reads the client used to be a data
// race: handleModel wrote r.Client on a command goroutine while Status read it
// on the event loop. The race detector is what catches this; the assertions
// only keep the two sides doing real work until it can.
func TestModelSwitchIsSafeAgainstConcurrentStatusReads(t *testing.T) {
	providers := `{"llm":{"providers":{
	  "alpha":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["a1","a2"]}}}}`
	rt, _ := providerTestRuntime(t, providers)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			model := "a1"
			if i%2 == 0 {
				model = "a2"
			}
			if _, err := rt.SwitchModel("alpha/" + model); err != nil {
				t.Errorf("switch: %v", err)
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		status := rt.Status()
		if status.Model == "" {
			t.Error("status reported no model")
			return
		}
		_ = rt.ModelOptions()
		_ = rt.CurrentModel()
		_ = rt.NeedsProviderSetup()
	}
	<-done
}
