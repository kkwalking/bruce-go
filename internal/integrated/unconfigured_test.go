package integrated

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bruce-go/internal/config"
)

// writeUnconfiguredSettings writes a settings file with no provider entries and
// clears the environment variables the loader would otherwise turn into
// providers, so the test observes the state a first-run user is in.
func writeUnconfiguredSettings(t *testing.T, providers string) string {
	t.Helper()
	for _, name := range []string{config.DeepSeekAPIKeyEnv, config.GLMAPIKeyEnv, config.KimiAPIKeyEnv} {
		t.Setenv(name, "")
	}
	home := t.TempDir()
	settingsPath := filepath.Join(home, "setting.json")
	if err := os.WriteFile(settingsPath, []byte(providers), 0o600); err != nil {
		t.Fatal(err)
	}
	return settingsPath
}

// An empty settings file used to make New fail outright, which left a first-run
// user with an error instead of a way to configure a provider. The runtime must
// now come up and say so.
func TestNewStartsWithoutAnyProvider(t *testing.T) {
	settingsPath := writeUnconfiguredSettings(t, `{"llm":{"providers":{}}}`)
	rt, err := New(context.Background(), Options{
		Workspace:    t.TempDir(),
		HomeDir:      filepath.Dir(settingsPath),
		SettingsPath: settingsPath,
	})
	if err != nil {
		t.Fatalf("New should come up without a provider, got %v", err)
	}
	cleanupRuntime(t, rt)

	if !rt.NeedsProviderSetup() {
		t.Fatal("the runtime should report that a provider has to be set up")
	}
	// Client must never be nil: the status line, welcome text and context
	// accounting all dereference it.
	if rt.Client == nil {
		t.Fatal("Client must not be nil")
	}
	// The switchable client stays nil so the existing nil branches keep working.
	if rt.switchable != nil {
		t.Fatal("switchable should stay nil while nothing is configured")
	}
	status := rt.Handle(context.Background(), "/status")
	if status.Err != nil {
		t.Fatalf("/status should still work: %v", status.Err)
	}
	if !strings.Contains(status.Output, "unconfigured") {
		t.Fatalf("status should name the unconfigured state: %s", status.Output)
	}
	// A provider without a usable entry cannot be switched to.
	if _, err := rt.Handle(context.Background(), "/model list").Output, rt.Handle(context.Background(), "/model list").Err; err != nil {
		t.Fatalf("/model list should not fail without providers: %v", err)
	}
}

// Running a task must explain what is missing and how to fix it, not fail with
// a transport error.
func TestTaskWithoutProviderExplainsWhatToDo(t *testing.T) {
	settingsPath := writeUnconfiguredSettings(t, `{"llm":{"providers":{}}}`)
	rt, err := New(context.Background(), Options{
		Workspace:    t.TempDir(),
		HomeDir:      filepath.Dir(settingsPath),
		SettingsPath: settingsPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)

	result := rt.Handle(context.Background(), "hello")
	text := result.Output
	if result.Err != nil {
		text = result.Err.Error()
	}
	if !strings.Contains(text, "/provider add") {
		t.Fatalf("the failure should point at /provider add, got %q", text)
	}
}

// A settings file whose providers are all unusable is the same situation as an
// empty one, and must not be reported as a configuration error either.
func TestNewStartsWhenEveryProviderIsUnusable(t *testing.T) {
	body := `{"llm":{"providers":{"mygateway":{"apiKey":"","baseUrl":"http://127.0.0.1:1/v1"}}}}`
	settingsPath := writeUnconfiguredSettings(t, body)
	rt, err := New(context.Background(), Options{
		Workspace:    t.TempDir(),
		HomeDir:      filepath.Dir(settingsPath),
		SettingsPath: settingsPath,
	})
	if err != nil {
		t.Fatalf("New should come up with no usable provider, got %v", err)
	}
	cleanupRuntime(t, rt)
	if !rt.NeedsProviderSetup() {
		t.Fatal("the runtime should report that a provider has to be set up")
	}
}

// A configured runtime must not claim setup is needed.
func TestConfiguredRuntimeDoesNotNeedSetup(t *testing.T) {
	body := `{"llm":{"providers":{"mygateway":{"apiKey":"k","baseUrl":"http://127.0.0.1:1/v1","models":["m"]}}}}`
	settingsPath := writeUnconfiguredSettings(t, body)
	rt, err := New(context.Background(), Options{
		Workspace:    t.TempDir(),
		HomeDir:      filepath.Dir(settingsPath),
		SettingsPath: settingsPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupRuntime(t, rt)
	if rt.NeedsProviderSetup() {
		t.Fatal("a configured runtime should not ask for setup")
	}
	if rt.switchable == nil {
		t.Fatal("a configured runtime should have a switchable client")
	}
}
