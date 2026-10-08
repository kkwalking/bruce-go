package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bruce-go/internal/config"
)

func TestDiscoverModelsUsesBearerForOpenAIProtocols(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"group/flash","context_window":128000,"max_output_tokens":8192}]}`))
	}))
	defer server.Close()

	models, err := DiscoverModels(context.Background(), config.ProtocolOpenAIChat, server.URL+"/v1", "magpie")
	if err != nil {
		t.Fatal(err)
	}
	// The configured base already names the version prefix, so it must not be
	// repeated.
	if gotPath != "/v1/models" {
		t.Fatalf("path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer magpie" {
		t.Fatalf("Authorization = %q, want a bearer token", gotAuth)
	}
	if len(models) != 1 || models[0].ID != "group/flash" {
		t.Fatalf("models = %#v", models)
	}
	if models[0].ContextWindow != 128000 || models[0].MaxOutputTokens != 8192 {
		t.Fatalf("capabilities = %+v, want the published hints", models[0])
	}
}

// The Anthropic API authenticates with its own header and is rooted at /v1,
// which the configured base URL does not have to include.
func TestDiscoverModelsUsesAPIKeyHeaderForAnthropic(t *testing.T) {
	var gotPath, gotKey, gotVersion, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5","max_input_tokens":200000,"max_tokens":64000}]}`))
	}))
	defer server.Close()

	models, err := DiscoverModels(context.Background(), config.ProtocolAnthropic, server.URL, "magpie")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("path = %q, want the version prefix to be added", gotPath)
	}
	if gotKey != "magpie" || gotVersion != anthropicVersion {
		t.Fatalf("headers = x-api-key %q, anthropic-version %q", gotKey, gotVersion)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, this protocol does not use a bearer token", gotAuth)
	}
	// The Anthropic spelling of the capability hints is read too.
	if models[0].ContextWindow != 200000 || models[0].MaxOutputTokens != 64000 {
		t.Fatalf("capabilities = %+v", models[0])
	}
}

// A gateway may publish either spelling of the capability hints, or none.
func TestDiscoverModelsReadsBothCapabilitySpellings(t *testing.T) {
	models, err := parseModelList([]byte(`{"data":[
	  {"id":"a","context_window":100,"max_output_tokens":10},
	  {"id":"b","context_length":200},
	  {"id":"c","max_input_tokens":300,"max_tokens":30},
	  {"id":"d"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]DiscoveredModel{}
	for _, model := range models {
		byID[model.ID] = model
	}
	if got := byID["a"]; got.ContextWindow != 100 || got.MaxOutputTokens != 10 {
		t.Fatalf("a = %+v", got)
	}
	if got := byID["b"]; got.ContextWindow != 200 || got.MaxOutputTokens != 0 {
		t.Fatalf("b = %+v", got)
	}
	if got := byID["c"]; got.ContextWindow != 300 || got.MaxOutputTokens != 30 {
		t.Fatalf("c = %+v", got)
	}
	// A model with no published hint is still usable, it just cannot trigger
	// automatic compaction on its own.
	if got := byID["d"]; got.ContextWindow != 0 || got.MaxOutputTokens != 0 {
		t.Fatalf("d = %+v, want zero capabilities", got)
	}
}

func TestDiscoverModelsSortsAndDeduplicates(t *testing.T) {
	models, err := parseModelList([]byte(`{"data":[
	  {"id":"zeta"},{"id":"alpha"},{"id":"zeta"},{"id":"  "},{"id":" beta "}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := ModelIDs(models)
	if strings.Join(got, ",") != "alpha,beta,zeta" {
		t.Fatalf("ids = %v, want a sorted, deduplicated list", got)
	}
}

func TestDiscoverModelsReportsHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer server.Close()

	_, err := DiscoverModels(context.Background(), config.ProtocolOpenAIChat, server.URL+"/v1", "wrong")
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want a typed APIError with status 401", err)
	}
}

// A gateway that answers with a login page or an empty list must be reported,
// so the wizard can fall back to entering model names by hand.
func TestDiscoverModelsRejectsUnreadableResponses(t *testing.T) {
	for _, body := range []string{"<html>login</html>", `{"data":[]}`, `{"data":[{"id":""}]}`} {
		if _, err := parseModelList([]byte(body)); err == nil {
			t.Fatalf("body %q should be rejected", body)
		}
	}
}

// Without a base URL there is nowhere to send the API key, so no request is
// attempted at all.
func TestDiscoverModelsRequiresBaseURL(t *testing.T) {
	if _, err := DiscoverModels(context.Background(), config.ProtocolOpenAIChat, "  ", "key"); err == nil {
		t.Fatal("a blank base URL should be refused")
	}
}

func TestModelsURL(t *testing.T) {
	for _, tc := range []struct{ base, versionPrefix, want string }{
		{"https://api.openai.com/v1", "", "https://api.openai.com/v1/models"},
		{"https://api.openai.com/v1/", "", "https://api.openai.com/v1/models"},
		{"https://api.openai.com/v1/models", "", "https://api.openai.com/v1/models"},
		{"http://127.0.0.1:3425/v1", "", "http://127.0.0.1:3425/v1/models"},
		{"https://api.anthropic.com", "/v1", "https://api.anthropic.com/v1/models"},
		{"https://api.anthropic.com/v1", "/v1", "https://api.anthropic.com/v1/models"},
		{"https://api.anthropic.com/v1/models", "/v1", "https://api.anthropic.com/v1/models"},
	} {
		if got := modelsURL(strings.TrimRight(tc.base, "/"), tc.versionPrefix); got != tc.want {
			t.Fatalf("modelsURL(%q, %q) = %q, want %q", tc.base, tc.versionPrefix, got, tc.want)
		}
	}
}

func TestModelCapabilitiesOmitsModelsWithoutHints(t *testing.T) {
	capabilities := ModelCapabilities([]DiscoveredModel{
		{ID: "known", ContextWindow: 100, MaxOutputTokens: 10},
		{ID: "unknown"},
		{ID: "partial", ContextWindow: 200},
	})
	if _, ok := capabilities["unknown"]; ok {
		t.Fatalf("a model with no hint should not be recorded: %#v", capabilities)
	}
	if got := capabilities["known"]; got.ContextWindow != 100 || got.MaxOutputTokens != 10 {
		t.Fatalf("known = %+v", got)
	}
	if got := capabilities["partial"]; got.ContextWindow != 200 {
		t.Fatalf("partial = %+v", got)
	}
}
