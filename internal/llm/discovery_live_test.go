package llm

import (
	"context"
	"os"
	"slices"
	"testing"

	"bruce-go/internal/config"
)

// Live check of model discovery against the local three-protocol proxy, which
// accepts both authentication styles. Skipped unless BRUCE_LIVE_PROXY=1.
func TestLiveDiscoverModelsProxy(t *testing.T) {
	if os.Getenv("BRUCE_LIVE_PROXY") != "1" {
		t.Skip("set BRUCE_LIVE_PROXY=1 to run against the local proxy")
	}
	for _, tc := range []struct{ protocol, baseURL string }{
		{config.ProtocolOpenAIChat, "http://127.0.0.1:3425/v1"},
		{config.ProtocolOpenAIResponses, "http://127.0.0.1:3425/v1"},
		{config.ProtocolAnthropic, "http://127.0.0.1:3425"},
	} {
		models, err := DiscoverModels(context.Background(), tc.protocol, tc.baseURL, "magpie")
		if err != nil {
			t.Fatalf("%s: %v", tc.protocol, err)
		}
		if len(models) == 0 {
			t.Fatalf("%s: no models discovered", tc.protocol)
		}
		t.Logf("%s: %d models, first=%+v", tc.protocol, len(models), models[0])
		// The wizard's end-to-end verification uses this model, and its ID
		// contains a slash, which is the case most likely to break a selector.
		if !slices.Contains(ModelIDs(models), "group/flash") {
			t.Fatalf("%s: group/flash missing from the discovered list", tc.protocol)
		}
	}
}
