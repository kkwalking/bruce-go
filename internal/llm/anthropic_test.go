package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bruce-go/internal/config"
)

func TestAnthropicRequestHoistsSystemPrompt(t *testing.T) {
	client := NewAnthropicClient("anthropic", "key", "claude-opus-5", "https://api.anthropic.com")
	body, err := client.requestBody([]Message{
		System("You are Bruce."),
		System("Be concise."),
		User("hello"),
	}, nil, true, StreamOptions{MaxTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	payload := decodePayload(t, body)

	// The Messages API rejects a "system" role inside messages, so the prompt
	// has to be lifted into the top-level field.
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v, want one user turn", payload["messages"])
	}
	turn := messages[0].(map[string]any)
	if turn["role"] != "user" {
		t.Fatalf("first turn role = %v, want user", turn["role"])
	}
	system, ok := payload["system"].([]any)
	if !ok || len(system) != 1 {
		t.Fatalf("system = %#v, want one text block", payload["system"])
	}
	block := system[0].(map[string]any)
	if block["text"] != "You are Bruce.\n\nBe concise." {
		t.Fatalf("system text = %q", block["text"])
	}
	// The system block carries the cache breakpoint, which also covers the
	// tool definitions rendered before it.
	if _, ok := block["cache_control"].(map[string]any); !ok {
		t.Fatalf("system block should carry a cache breakpoint: %#v", block)
	}
}

// max_tokens is required by the Messages API, so a caller that sets no limit
// must still get one.
func TestAnthropicRequestAlwaysSendsMaxTokens(t *testing.T) {
	client := NewAnthropicClient("anthropic", "key", "claude-opus-5", "https://api.anthropic.com")
	body, err := client.requestBody([]Message{User("hi")}, nil, true, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	payload := decodePayload(t, body)
	if got, ok := payload["max_tokens"].(float64); !ok || got <= 0 {
		t.Fatalf("max_tokens = %#v, want a positive default", payload["max_tokens"])
	}

	// A declared model capability is a better default than the fixed fallback.
	client.SetModelCapability(0, 4096)
	body, err = client.requestBody([]Message{User("hi")}, nil, true, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := decodePayload(t, body)["max_tokens"]; got != float64(4096) {
		t.Fatalf("max_tokens = %#v, want the declared capability 4096", got)
	}
}

// Tool results are content blocks inside a user turn, not a message role, and
// consecutive same-role turns have to be merged because the API rejects them.
func TestAnthropicRequestMergesToolResultIntoUserTurn(t *testing.T) {
	client := NewAnthropicClient("anthropic", "key", "claude-opus-5", "https://api.anthropic.com")
	body, err := client.requestBody([]Message{
		User("what is the weather"),
		{
			Role: RoleAssistant,
			ToolCalls: []ToolCall{
				{ID: "toolu_1", Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}},
			},
		},
		ToolMessage("toolu_1", `{"temp":18}`),
	}, nil, true, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	messages := decodePayload(t, body)["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3 turns (user, assistant, user): %#v", len(messages), messages)
	}
	assistant := messages[1].(map[string]any)
	blocks := assistant["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("assistant content = %#v, want a single tool_use block", blocks)
	}
	call := blocks[0].(map[string]any)
	if call["id"] != "toolu_1" || call["name"] != "get_weather" {
		t.Fatalf("tool_use block = %#v", call)
	}
	// Arguments travel as a JSON object, not as a string.
	input, ok := call["input"].(map[string]any)
	if !ok || input["city"] != "Paris" {
		t.Fatalf("tool_use input = %#v, want a parsed object", call["input"])
	}

	last := messages[2].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("tool result turn role = %v, want user", last["role"])
	}
	result := last["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "toolu_1" {
		t.Fatalf("tool_result block = %#v", result)
	}
}

// Tool arguments that are not a JSON object cannot be represented in the
// Messages format, so they are reported instead of being sent malformed.
func TestAnthropicRequestRejectsNonObjectToolArguments(t *testing.T) {
	client := NewAnthropicClient("anthropic", "key", "claude-opus-5", "https://api.anthropic.com")
	_, err := client.requestBody([]Message{{
		Role: RoleAssistant,
		ToolCalls: []ToolCall{
			{ID: "toolu_1", Function: FunctionCall{Name: "get_weather", Arguments: `"not an object"`}},
		},
	}}, nil, true, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "get_weather") {
		t.Fatalf("non-object arguments should be refused, err=%v", err)
	}
}

func TestAnthropicRequestPlacesToolCacheBreakpointOnLastTool(t *testing.T) {
	client := NewAnthropicClient("anthropic", "key", "claude-opus-5", "https://api.anthropic.com")
	body, err := client.requestBody([]Message{User("hi")}, []ToolDefinition{
		{Name: "a", Description: "first", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "b", Description: "second", Parameters: json.RawMessage(`{"type":"object"}`)},
	}, true, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tools := decodePayload(t, body)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(tools))
	}
	first := tools[0].(map[string]any)
	last := tools[1].(map[string]any)
	if _, ok := first["cache_control"]; ok {
		t.Fatalf("only the last tool should carry the breakpoint: %#v", first)
	}
	if _, ok := last["cache_control"].(map[string]any); !ok {
		t.Fatalf("last tool should carry a cache breakpoint: %#v", last)
	}
	// The schema is passed through verbatim under the name this API uses.
	if _, ok := last["input_schema"]; !ok {
		t.Fatalf("tool should carry input_schema: %#v", last)
	}
	if _, ok := last["parameters"]; ok {
		t.Fatalf("tool should not carry the Chat Completions field name: %#v", last)
	}
}

func TestAnthropicRequestSendsImageAsBase64Source(t *testing.T) {
	client := NewAnthropicClient("anthropic", "key", "claude-opus-5", "https://api.anthropic.com")
	body, err := client.requestBody([]Message{
		UserParts([]ContentPart{
			TextPart("what is this"),
			ImagePart("data:image/png;base64,AAAA", "image/png", "test.png"),
		}),
	}, nil, true, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	messages := decodePayload(t, body)["messages"].([]any)
	blocks := messages[0].(map[string]any)["content"].([]any)
	image := blocks[1].(map[string]any)
	if image["type"] != "image" {
		t.Fatalf("image block = %#v", image)
	}
	source := image["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != "AAAA" {
		t.Fatalf("image source = %#v", source)
	}
}

// A remote URL cannot be sent to this API, and silently dropping the image
// would lose content the user attached.
func TestAnthropicRequestRejectsRemoteImageURL(t *testing.T) {
	client := NewAnthropicClient("anthropic", "key", "claude-opus-5", "https://api.anthropic.com")
	_, err := client.requestBody([]Message{
		UserParts([]ContentPart{ImagePart("https://example.com/a.png", "image/png", "a.png")}),
	}, nil, true, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "data URL") {
		t.Fatalf("a remote image URL should be refused, err=%v", err)
	}
}

func TestParseAnthropicResponseReadsTextAndToolUse(t *testing.T) {
	resp, err := ParseAnthropicResponse([]byte(`{
	  "content": [
	    {"type": "text", "text": "Let me check."},
	    {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Paris"}}
	  ],
	  "stop_reason": "tool_use",
	  "usage": {"input_tokens": 10, "output_tokens": 20, "cache_read_input_tokens": 5}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Let me check." {
		t.Fatalf("content = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool calls = %#v", resp.ToolCalls)
	}
	// Arguments are re-serialized so the agent loop can pass them along
	// unchanged, exactly as the Chat Completions client does. Compared as a
	// parsed object: the encoding of an equivalent object is not specified.
	var arguments map[string]any
	if err := json.Unmarshal([]byte(resp.ToolCalls[0].Function.Arguments), &arguments); err != nil {
		t.Fatalf("arguments %q are not valid JSON: %v", resp.ToolCalls[0].Function.Arguments, err)
	}
	if arguments["city"] != "Paris" {
		t.Fatalf("arguments = %#v, want city=Paris", arguments)
	}
	// The stop reason is translated into the vocabulary the rest of the
	// codebase reasons about.
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", resp.FinishReason)
	}
	if resp.InputTokens != 10 || resp.OutputTokens != 20 || resp.CachedInputTokens != 5 {
		t.Fatalf("usage = %+v", resp)
	}
}

// Cached tokens are reported separately by this API, so they must not be
// subtracted from the input count the way the Chat Completions client does.
func TestParseAnthropicResponseKeepsInputTokensWhole(t *testing.T) {
	resp, err := ParseAnthropicResponse([]byte(`{
	  "content": [{"type": "text", "text": "hi"}],
	  "stop_reason": "end_turn",
	  "usage": {"input_tokens": 100, "output_tokens": 5, "cache_read_input_tokens": 80}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.InputTokens != 100 {
		t.Fatalf("input tokens = %d, want 100 (cached tokens are already reported separately)", resp.InputTokens)
	}
	if resp.CachedInputTokens != 80 {
		t.Fatalf("cached tokens = %d, want 80", resp.CachedInputTokens)
	}
}

func TestParseAnthropicStreamAccumulatesTextAndToolUse(t *testing.T) {
	var content strings.Builder
	resp, err := ParseAnthropicStream(strings.NewReader(`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":0,"cache_read_input_tokens":3}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" check."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}

event: message_stop
data: {"type":"message_stop"}

`), StreamOptions{OnContent: func(delta string) { content.WriteString(delta) }})
	if err != nil {
		t.Fatal(err)
	}
	if content.String() != "Let me check." || resp.Content != "Let me check." {
		t.Fatalf("streamed content = %q / %q", content.String(), resp.Content)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v, want one", resp.ToolCalls)
	}
	call := resp.ToolCalls[0]
	if call.ID != "toolu_1" || call.Function.Name != "get_weather" {
		t.Fatalf("tool call = %+v", call)
	}
	// The argument fragments are only meaningful once concatenated.
	if call.Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("arguments = %q, want the concatenated fragments", call.Function.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q", resp.FinishReason)
	}
	// Usage arrives split across message_start and message_delta.
	if resp.InputTokens != 7 || resp.OutputTokens != 12 || resp.CachedInputTokens != 3 {
		t.Fatalf("usage = %+v", resp)
	}
}

// A ping event carries no content and must not disturb the accumulation; a
// proxy may send them at any point.
func TestParseAnthropicStreamIgnoresPingEvents(t *testing.T) {
	resp, err := ParseAnthropicStream(strings.NewReader(`event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_stop
data: {"type":"message_stop"}

`), StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hi" {
		t.Fatalf("content = %q, want hi", resp.Content)
	}
}

func TestAnthropicClientReturnsTypedAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer server.Close()

	client := NewAnthropicClient("anthropic", "key", "claude-opus-5", server.URL)
	_, err := client.Chat(context.Background(), []Message{User("hi")}, nil, StreamOptions{})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want a typed APIError with status 429", err)
	}
}

// The request must carry the headers this API authenticates with; the Chat
// Completions bearer token is not accepted.
func TestAnthropicClientSendsRequiredHeaders(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	client := NewAnthropicClient("anthropic", "secret-key", "claude-opus-5", server.URL)
	if _, err := client.Chat(context.Background(), []Message{User("hi")}, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if got.Get("x-api-key") != "secret-key" {
		t.Fatalf("x-api-key = %q", got.Get("x-api-key"))
	}
	if got.Get("anthropic-version") != anthropicVersion {
		t.Fatalf("anthropic-version = %q, want %q", got.Get("anthropic-version"), anthropicVersion)
	}
	if got.Get("Authorization") != "" {
		t.Fatalf("Authorization = %q, this API does not use a bearer token", got.Get("Authorization"))
	}
}

func TestAnthropicMessagesURL(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"https://api.anthropic.com", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/v1", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/v1/messages", "https://api.anthropic.com/v1/messages"},
		{"http://127.0.0.1:3425", "http://127.0.0.1:3425/v1/messages"},
	} {
		if got := anthropicMessagesURL(tc.base); got != tc.want {
			t.Fatalf("anthropicMessagesURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestAnthropicClientUsesDeclaredCapability(t *testing.T) {
	client := NewAnthropicClient("anthropic", "key", "claude-opus-5", "https://api.anthropic.com")
	if got := client.MaxContextWindow(); got != 0 {
		t.Fatalf("context window = %d, want 0 for an unknown model", got)
	}
	client.SetModelCapability(200000, 64000)
	if got := client.MaxContextWindow(); got != 200000 {
		t.Fatalf("context window = %d, want the declared 200000", got)
	}
	if got := client.MaxOutputTokens(); got != 64000 {
		t.Fatalf("max output tokens = %d, want the declared 64000", got)
	}
}

// The client is built through the factory with the declared capability applied,
// so a configured context window reaches the client the runtime measures.
func TestNewProviderClientAppliesAnthropicCapability(t *testing.T) {
	client := NewProviderClient("myclaude", "m", config.ProviderSetting{
		APIKey:            "k",
		BaseURL:           "http://localhost:9000",
		Protocol:          config.ProtocolAnthropic,
		ModelCapabilities: map[string]config.ModelCapability{"m": {ContextWindow: 12345}},
	})
	if got := client.MaxContextWindow(); got != 12345 {
		t.Fatalf("context window = %d, want 12345", got)
	}
}

func decodePayload(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}
