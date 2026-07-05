package llm

import (
	"encoding/json"
	"net/http"
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

data: {"choices":[{"delta":{"content":"你好"}}]}

data: {"choices":[{"delta":{"content":"！"}}]}

data: {"choices":[{"delta":{"content":""}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}

data: [DONE]

`), StreamOptions{OnContent: func(delta string) { content.WriteString(delta) }})
	if err != nil {
		t.Fatal(err)
	}
	if content.String() != "你好！" || resp.Content != "你好！" {
		t.Fatalf("content delta=%q response=%q", content.String(), resp.Content)
	}
	if resp.InputTokens != 5 || resp.OutputTokens != 2 {
		t.Fatalf("usage = %d/%d", resp.InputTokens, resp.OutputTokens)
	}
}


func TestOpenAICompatibleClientRequestBodyHonorsReasoningEffort(t *testing.T) {
	body, err := func() ([]byte, error) {
		c := NewOpenAICompatibleClient("deepseek", "key", "model", "https://api.example.com/v1")
		c.SetReasoningEffort("high")
		return c.requestBody(nil, nil, false)
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
	body2, err := c2.requestBody(nil, nil, false)
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
	body3, err := c3.requestBody(nil, nil, false)
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
