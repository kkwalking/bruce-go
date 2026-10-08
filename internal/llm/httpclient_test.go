package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A redirect that stays on the configured host is an ordinary gateway
// behaviour and must keep working, credentials included.
func TestProviderClientsFollowSameHostRedirectsWithCredentials(t *testing.T) {
	var seenAuth, seenAPIKey string
	var hits int
	// Each protocol parses a different response shape, so the redirect target
	// answers per path.
	body := func(path string) string {
		switch path {
		case "/responses":
			return `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"status":"completed"}`
		case "/v1/messages":
			return `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`
		default:
			return `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/moved"):
			hits++
			seenAuth, seenAPIKey = r.Header.Get("Authorization"), r.Header.Get("x-api-key")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body(strings.TrimPrefix(r.URL.Path, "/moved"))))
		default:
			http.Redirect(w, r, "/moved"+r.URL.Path, http.StatusTemporaryRedirect)
		}
	}))
	defer server.Close()

	clients := map[string]ChatClient{
		"anthropic": NewAnthropicClient("anthropic", "sk-secret", "claude-opus-5", server.URL),
		"chat":      NewOpenAICompatibleClient("custom", "sk-openai", "m", server.URL),
		"responses": NewOpenAIResponsesClient("custom", "sk-openai", "m", server.URL),
	}
	for name, client := range clients {
		if _, err := client.Chat(context.Background(), nil, nil, StreamOptions{}); err != nil {
			t.Fatalf("%s: same-host redirect should be followed: %v", name, err)
		}
	}
	if hits != len(clients) {
		t.Fatalf("the redirect target was reached %d times, want %d", hits, len(clients))
	}
	// The last client to run is whichever the map yielded; at minimum the
	// credential of some protocol survived the same-host hop.
	if seenAuth == "" && seenAPIKey == "" {
		t.Fatal("credentials were dropped on a same-host redirect")
	}
}

// A redirect that leaves the host must not replay the request with the API key:
// the key is only ever meant for the configured endpoint.
func TestProviderClientsRefuseCrossHostRedirects(t *testing.T) {
	var leaked []string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = append(leaked, r.Header.Get("x-api-key"), r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer attacker.Close()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer gateway.Close()

	paths := map[string]func() error{
		"anthropic chat": func() error {
			_, err := NewAnthropicClient("anthropic", "sk-secret", "claude-opus-5", gateway.URL).
				Chat(context.Background(), nil, nil, StreamOptions{})
			return err
		},
		"openai chat": func() error {
			_, err := NewOpenAICompatibleClient("custom", "sk-secret", "m", gateway.URL).
				Chat(context.Background(), nil, nil, StreamOptions{})
			return err
		},
		"responses chat": func() error {
			_, err := NewOpenAIResponsesClient("custom", "sk-secret", "m", gateway.URL).
				Chat(context.Background(), nil, nil, StreamOptions{})
			return err
		},
		"discovery": func() error {
			_, err := DiscoverModels(context.Background(), "anthropic", gateway.URL, "sk-secret")
			return err
		},
	}
	for name, run := range paths {
		if err := run(); err == nil {
			t.Errorf("%s: a cross-host redirect should fail rather than be followed", name)
		}
	}
	for _, value := range leaked {
		if value != "" {
			t.Fatalf("a credential reached the redirect target: %q", value)
		}
	}
}
