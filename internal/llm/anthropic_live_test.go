package llm

import (
	"context"
	"os"
	"testing"
)

// Live check against the local three-protocol proxy. Skipped unless
// BRUCE_LIVE_PROXY=1, so CI never depends on it.
func TestLiveAnthropicProxy(t *testing.T) {
	if os.Getenv("BRUCE_LIVE_PROXY") != "1" {
		t.Skip("set BRUCE_LIVE_PROXY=1 to run against the local proxy")
	}
	c := NewAnthropicClient("anthropic", "magpie", "group/flash", "http://127.0.0.1:3425")

	var streamed string
	resp, err := c.Chat(context.Background(), []Message{
		System("You are a terse assistant."),
		User("Reply with exactly: pong"),
	}, nil, StreamOptions{MaxTokens: 100, OnContent: func(d string) { streamed += d }})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("content=%q streamed=%q finish=%q usage=%d/%d cached=%d",
		resp.Content, streamed, resp.FinishReason, resp.InputTokens, resp.OutputTokens, resp.CachedInputTokens)
	if resp.Content == "" {
		t.Fatal("empty content")
	}
	if resp.InputTokens == 0 {
		t.Fatal("input tokens should be non-zero")
	}

	// Tool use, streaming: exercises input_json_delta accumulation.
	resp2, err := c.Chat(context.Background(), []Message{
		User("What is the weather in Paris? Use the tool."),
	}, []ToolDefinition{{
		Name:        "get_weather",
		Description: "Get weather",
		Parameters:  []byte(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}}, StreamOptions{MaxTokens: 200})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("toolcalls=%d finish=%q", len(resp2.ToolCalls), resp2.FinishReason)
	for _, call := range resp2.ToolCalls {
		t.Logf("  %s(%s) id=%s", call.Function.Name, call.Function.Arguments, call.ID)
	}
	if len(resp2.ToolCalls) == 0 {
		t.Fatal("expected a tool call")
	}
	if resp2.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", resp2.FinishReason)
	}
}
