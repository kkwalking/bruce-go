package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeepSeekClientDisablesHTTP2ForStreaming(t *testing.T) {
	client := NewDeepSeekClient("key", "")
	if client.HTTPClient == nil {
		t.Fatal("expected deepseek client to have a custom HTTP client")
	}
	if client.HTTPClient.Timeout != 120*time.Second {
		t.Fatalf("timeout = %s", client.HTTPClient.Timeout)
	}
	transport, ok := client.HTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", client.HTTPClient.Transport)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("deepseek transport should not force HTTP/2")
	}
	if transport.TLSNextProto == nil || len(transport.TLSNextProto) != 0 {
		t.Fatalf("TLSNextProto should be an empty map to disable HTTP/2, got %#v", transport.TLSNextProto)
	}
}

func TestParseChatStreamParsesDeepSeekSSEChunks(t *testing.T) {
	var content strings.Builder
	resp, err := ParseChatStream(strings.NewReader(`data: {"choices":[{"delta":{"role":"assistant","content":""}}]}

data: {"choices":[{"delta":{"content":"café"}}]}

data: {"choices":[{"delta":{"content":"!"}}]}

data: {"choices":[{"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}

data: [DONE]

`), StreamOptions{OnContent: func(delta string) { content.WriteString(delta) }})
	if err != nil {
		t.Fatal(err)
	}
	if content.String() != "café!" || resp.Content != "café!" {
		t.Fatalf("content delta=%q response=%q", content.String(), resp.Content)
	}
	if resp.InputTokens != 5 || resp.OutputTokens != 2 {
		t.Fatalf("usage = %d/%d", resp.InputTokens, resp.OutputTokens)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish reason = %q", resp.FinishReason)
	}
}

func TestParseChatResponseFinishReasonAndCachedUsage(t *testing.T) {
	resp, err := ParseChatResponse([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "length" || resp.InputTokens != 6 || resp.CachedInputTokens != 4 || resp.OutputTokens != 2 {
		t.Fatalf("response = %+v", resp)
	}
}

func TestOpenAICompatibleClientRequestBodyHonorsReasoningEffort(t *testing.T) {
	body, err := func() ([]byte, error) {
		c := NewOpenAICompatibleClient("deepseek", "key", "model", "https://api.example.com/v1")
		c.SetReasoningEffort("high")
		return c.requestBody(nil, nil, false, StreamOptions{})
	}()
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %v, want high", got)
	}
	if _, ok := payload["thinking"]; !ok {
		t.Fatal("deepseek should include thinking.enabled")
	}

	// off: no reasoning fields at all
	c2 := NewOpenAICompatibleClient("deepseek", "key", "model", "https://api.example.com/v1")
	c2.SetReasoningEffort("off")
	body2, err := c2.requestBody(nil, nil, false, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var payload2 map[string]any
	if err := json.Unmarshal(body2, &payload2); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload2["reasoning_effort"]; ok {
		t.Fatal("off should not include reasoning_effort")
	}
	if _, ok := payload2["thinking"]; ok {
		t.Fatal("off should not include thinking")
	}

	// glm: reasoning_effort sent, but no thinking field
	c3 := NewOpenAICompatibleClient("glm", "key", "model", "https://api.example.com/v1")
	c3.SetReasoningEffort("medium")
	body3, err := c3.requestBody(nil, nil, false, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var payload3 map[string]any
	if err := json.Unmarshal(body3, &payload3); err != nil {
		t.Fatal(err)
	}
	if got := payload3["reasoning_effort"]; got != "medium" {
		t.Fatalf("reasoning_effort = %v, want medium", got)
	}
	if _, ok := payload3["thinking"]; ok {
		t.Fatal("glm should not include thinking")
	}
}

func TestOpenAICompatibleRequestMaxTokensAndCapabilities(t *testing.T) {
	c := NewGLMClient("key", "glm-4.7")
	if c.MaxContextWindow() != 204800 || c.MaxOutputTokens() != 131072 {
		t.Fatalf("built-in capability = %d/%d", c.MaxContextWindow(), c.MaxOutputTokens())
	}
	c.SetModelCapability(128000, 8192)
	if c.MaxContextWindow() != 128000 || c.MaxOutputTokens() != 8192 {
		t.Fatalf("overridden capability = %d/%d", c.MaxContextWindow(), c.MaxOutputTokens())
	}
	body, err := c.requestBody(nil, nil, false, StreamOptions{MaxTokens: 321})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["max_tokens"] != float64(321) {
		t.Fatalf("max_tokens = %#v", payload["max_tokens"])
	}
}

func TestOpenAICompatibleClientReturnsTypedAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":"request_too_large"}`))
	}))
	defer server.Close()
	c := NewOpenAICompatibleClient("test", "key", "model", server.URL)
	_, err := c.Chat(context.Background(), nil, nil, StreamOptions{})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(apiError.Body, "request_too_large") {
		t.Fatalf("error = %#v", err)
	}
}

func TestKimiRequestUsesOpenAICompatibleEndpointAndTokenField(t *testing.T) {
	c := NewKimiClient("kimi-key", "")
	if c.APIURL != "https://api.moonshot.cn/v1/chat/completions" {
		t.Fatalf("APIURL = %q", c.APIURL)
	}
	if c.Model != "kimi-k3" {
		t.Fatalf("default model = %q, want kimi-k3", c.Model)
	}
	body, err := c.requestBody(nil, nil, false, StreamOptions{MaxTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	// Moonshot deprecates max_tokens, so the current field must be present too.
	if payload["max_completion_tokens"] != float64(4096) {
		t.Fatalf("max_completion_tokens = %#v", payload["max_completion_tokens"])
	}
}

// kimi-k3 accepts reasoning_effort; the K2.x models reject it, so sending it
// there would turn every request into a 400.
func TestKimiReasoningEffortOnlySentToK3(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{model: "kimi-k3", want: true},
		{model: "kimi-k2.7-code", want: false},
		{model: "kimi-k2.7-code-highspeed", want: false},
		{model: "kimi-k2.6", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			c := NewKimiClient("key", tc.model)
			c.SetReasoningEffort("high")
			body, err := c.requestBody(nil, nil, false, StreamOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			_, present := payload["reasoning_effort"]
			if present != tc.want {
				t.Fatalf("reasoning_effort present = %v, want %v (payload %v)", present, tc.want, payload)
			}
			if _, ok := payload["thinking"]; ok {
				t.Fatal("kimi must not receive the deepseek thinking field")
			}
		})
	}
}

// Non-Kimi providers must keep sending reasoning_effort unchanged.
func TestNonKimiProvidersStillSendReasoningEffort(t *testing.T) {
	for _, provider := range []string{"glm", "deepseek"} {
		c := NewOpenAICompatibleClient(provider, "key", "some-model", "https://example.com/v1")
		c.SetReasoningEffort("high")
		body, err := c.requestBody(nil, nil, false, StreamOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["reasoning_effort"] != "high" {
			t.Fatalf("%s reasoning_effort = %#v, want high", provider, payload["reasoning_effort"])
		}
	}
}

func TestKimiBuiltInModelCapabilities(t *testing.T) {
	cases := []struct {
		model           string
		contextWindow   int
		maxOutputTokens int
	}{
		{model: "kimi-k3", contextWindow: 1048576, maxOutputTokens: 131072},
		{model: "kimi-k2.7-code", contextWindow: 262144, maxOutputTokens: 32768},
		{model: "kimi-k2.7-code-highspeed", contextWindow: 262144, maxOutputTokens: 32768},
		{model: "kimi-k2.6", contextWindow: 262144, maxOutputTokens: 32768},
	}
	for _, tc := range cases {
		c := NewKimiClient("key", tc.model)
		if got := c.MaxContextWindow(); got != tc.contextWindow {
			t.Fatalf("%s context window = %d, want %d", tc.model, got, tc.contextWindow)
		}
		if got := c.MaxOutputTokens(); got != tc.maxOutputTokens {
			t.Fatalf("%s max output = %d, want %d", tc.model, got, tc.maxOutputTokens)
		}
	}
}

// Kimi K3 accepts only low/high/max and cannot stop thinking, so the project's
// off/medium levels must be mapped onto a supported value instead of being sent
// verbatim (which the API rejects).
func TestKimiK3MapsUnsupportedReasoningEfforts(t *testing.T) {
	cases := map[string]string{
		"off":    "low",
		"low":    "low",
		"medium": "high",
		"high":   "high",
		"max":    "max",
	}
	for effort, want := range cases {
		c := NewKimiClient("key", "kimi-k3")
		c.SetReasoningEffort(effort)
		body, err := c.requestBody(nil, nil, false, StreamOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if got := payload["reasoning_effort"]; got != want {
			t.Fatalf("effort %q -> %#v, want %q", effort, got, want)
		}
	}
}

// Turning thinking off must not add reasoning_effort or a thinking flag for
// providers that can genuinely disable it.
func TestDeepSeekOffSendsNoReasoningFields(t *testing.T) {
	c := NewOpenAICompatibleClient("deepseek", "key", "model", "https://api.example.com/v1")
	c.SetReasoningEffort("off")
	body, err := c.requestBody(nil, nil, false, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["reasoning_effort"]; ok {
		t.Fatalf("off must not send reasoning_effort: %v", payload)
	}
	if _, ok := payload["thinking"]; ok {
		t.Fatalf("off must not send thinking: %v", payload)
	}
}
