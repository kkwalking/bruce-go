package llm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"bruce-go/internal/config"
)

func TestUnconfiguredClientFailsChatWithTheSetupHint(t *testing.T) {
	client := NewUnconfiguredClient()
	_, err := client.Chat(context.Background(), []Message{User("hi")}, nil, StreamOptions{})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if !IsNotConfigured(err) {
		t.Fatal("IsNotConfigured should recognize its own sentinel")
	}
	if !strings.Contains(err.Error(), "/provider add") {
		t.Fatalf("err = %q, want it to name the fixing command", err)
	}
	if IsNotConfigured(errors.New("some other failure")) {
		t.Fatal("IsNotConfigured should not match unrelated errors")
	}
}

// Every capability reports zero, which is the value the runtime already treats
// as "unknown" and skips compaction and overflow handling for.
func TestUnconfiguredClientReportsNoCapabilities(t *testing.T) {
	client := NewUnconfiguredClient()
	if client.MaxContextWindow() != 0 || client.MaxOutputTokens() != 0 {
		t.Fatalf("unconfigured client should report no window: %d/%d", client.MaxContextWindow(), client.MaxOutputTokens())
	}
	if client.SupportsTools() || client.SupportsImages() || client.SupportsPromptCaching() {
		t.Fatal("unconfigured client should not claim any capability")
	}
	// Names are non-empty so a status line never renders as a bare slash.
	if client.ProviderName() == "" || client.ModelName() == "" {
		t.Fatal("unconfigured client should report placeholder names")
	}
}

// An empty provider map and a map with no usable entry are the same condition.
func TestNewSwitchableReturnsSentinelForNoUsableProvider(t *testing.T) {
	none := config.DefaultSettings()
	if _, err := NewSwitchable(none, config.Loader{}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("empty providers: err = %v, want ErrNoProvider", err)
	}
	unusable := config.DefaultSettings()
	unusable.LLM.Providers["mygateway"] = config.ProviderSetting{BaseURL: "http://127.0.0.1:1/v1"}
	if _, err := NewSwitchable(unusable, config.Loader{}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("unusable providers: err = %v, want ErrNoProvider", err)
	}
}
