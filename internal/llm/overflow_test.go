package llm

import (
	"errors"
	"testing"
)

func TestContextOverflowErrorPatternsAndExclusions(t *testing.T) {
	overflowMessages := []string{
		"prompt is too long: 200001 tokens",
		"Your input exceeds the context window of this model",
		"Input length (265330) exceeds model's maximum context length (262144)",
		"Please reduce the length of the messages or completion",
		"model_context_window_exceeded",
		"token limit exceeded",
	}
	for _, message := range overflowMessages {
		if !IsContextOverflowError(errors.New(message)) {
			t.Errorf("overflow not detected: %q", message)
		}
	}
	for _, message := range []string{"rate limit: too many tokens", "too many requests", "Throttling error: too many tokens"} {
		if IsContextOverflowError(errors.New(message)) {
			t.Errorf("non-overflow misdetected: %q", message)
		}
	}
	if !IsContextOverflowError(&APIError{StatusCode: 413, Status: "413 Request Entity Too Large"}) {
		t.Fatal("empty HTTP 413 should be detected")
	}
}

func TestDetectContextOverflowResponse(t *testing.T) {
	tests := []struct {
		name     string
		response ChatResponse
		window   int
		want     OverflowResponse
	}{
		{name: "silent", response: ChatResponse{FinishReason: "stop", InputTokens: 101}, window: 100, want: OverflowResponse{Overflow: true}},
		{name: "cached silent", response: ChatResponse{FinishReason: "stop", InputTokens: 80, CachedInputTokens: 21}, window: 100, want: OverflowResponse{Overflow: true}},
		{name: "length retry", response: ChatResponse{FinishReason: "length", InputTokens: 99}, window: 100, want: OverflowResponse{Overflow: true, Retry: true}},
		{name: "normal length", response: ChatResponse{FinishReason: "length", InputTokens: 99, OutputTokens: 1}, window: 100},
		{name: "unknown window", response: ChatResponse{FinishReason: "stop", InputTokens: 1000}, window: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DetectContextOverflowResponse(test.response, test.window); got != test.want {
				t.Fatalf("got %+v, want %+v", got, test.want)
			}
		})
	}
}
