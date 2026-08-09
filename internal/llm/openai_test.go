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
