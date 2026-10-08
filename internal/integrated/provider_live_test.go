package integrated

import (
	"os"
	"strings"
	"testing"

	"bruce-go/internal/config"
	"bruce-go/internal/llm"
)

// proxyBaseURL is the local three-protocol proxy used for live checks.
const proxyBaseURL = "http://127.0.0.1:3425"

// TestLiveProviderEndToEndConfiguresAndChats drives the whole path a user takes:
// start with no provider, discover the models from a real endpoint, save the
// provider, and then actually talk to it. Skipped unless BRUCE_LIVE_PROXY=1.
func TestLiveProviderEndToEndConfiguresAndChats(t *testing.T) {
	if os.Getenv("BRUCE_LIVE_PROXY") != "1" {
		t.Skip("set BRUCE_LIVE_PROXY=1 to run against the local proxy")
	}
	for _, tc := range []struct{ name, protocol, baseURL string }{
		{"live-chat", config.ProtocolOpenAIChat, proxyBaseURL + "/v1"},
		{"live-responses", config.ProtocolOpenAIResponses, proxyBaseURL + "/v1"},
		{"live-anthropic", config.ProtocolAnthropic, proxyBaseURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, _ := providerTestRuntime(t, `{"llm":{"providers":{}}}`)
			if !rt.NeedsProviderSetup() {
				t.Fatal("precondition: the runtime should start unconfigured")
			}

			// The same call the wizard makes when the user confirms.
			discovered, err := llm.DiscoverModels(t.Context(), tc.protocol, tc.baseURL, "magpie")
			if err != nil {
				t.Fatalf("discovery failed: %v", err)
			}
			if len(llm.ModelIDs(discovered)) == 0 {
				t.Fatal("discovery returned no models")
			}
			// Capabilities are recorded only for models that are also declared,
			// which is what the wizard does. The config layer refuses an entry
			// for a model that is not in the list, and it is right to.
			capability, ok := llm.ModelCapabilities(discovered)["group/flash"]
			if !ok {
				t.Fatal("the proxy published no capability for group/flash")
			}
			if err := rt.SaveProvider(tc.name, config.ProviderSetting{
				APIKey:            "magpie",
				BaseURL:           tc.baseURL,
				Protocol:          tc.protocol,
				Models:            []string{"group/flash"},
				ModelCapabilities: map[string]config.ModelCapability{"group/flash": capability},
			}, true); err != nil {
				t.Fatalf("saving the provider failed: %v", err)
			}
			if rt.NeedsProviderSetup() {
				t.Fatal("the runtime should be configured now")
			}
			if got := rt.CurrentModel(); got.Provider != tc.name || got.Model != "group/flash" {
				t.Fatalf("current model = %s, want %s/group/flash", got.Selector(), tc.name)
			}
			// The discovered capability must survive the save, otherwise the
			// provider cannot drive automatic compaction.
			if window := rt.Client.MaxContextWindow(); window <= 0 {
				t.Fatalf("context window = %d, want the discovered value", window)
			}

			// The real test: a message to the configured endpoint.
			out, err := rt.RunTask(t.Context(), "Reply with exactly: pong")
			if err != nil {
				t.Fatalf("task failed: %v", err)
			}
			if !strings.Contains(strings.ToLower(out), "pong") {
				t.Fatalf("reply = %q, want pong", out)
			}
		})
	}
}
