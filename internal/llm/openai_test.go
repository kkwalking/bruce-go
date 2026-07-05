package llm

import (
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

