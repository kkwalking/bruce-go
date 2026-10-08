package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesRequestSplitsInstructionsFromInput(t *testing.T) {
	client := NewOpenAIResponsesClient("openai-responses", "key", "gpt-5.5", "https://api.openai.com/v1")
	body, err := client.requestBody([]Message{
		System("You are Bruce."),
		User("hello"),
	}, nil, true, StreamOptions{MaxTokens: 128})
	if err != nil {
		t.Fatal(err)
	}
	payload := decodePayload(t, body)

	// There is no messages array in this format, and a system message is not
	// a valid input item.
	if _, ok := payload["messages"]; ok {
		t.Fatalf("payload should not carry a messages array: %#v", payload)
	}
	if payload["instructions"] != "You are Bruce." {
		t.Fatalf("instructions = %#v", payload["instructions"])
	}
	input := payload["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %#v, want one message item", input)
	}
	item := input[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "user" {
		t.Fatalf("input item = %#v", item)
	}
	// Content parts use this API's type names.
	part := item["content"].([]any)[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "hello" {
		t.Fatalf("content part = %#v", part)
	}
	if payload["max_output_tokens"] != float64(128) {
		t.Fatalf("max_output_tokens = %#v", payload["max_output_tokens"])
	}
}

// A tool call and its result are separate items correlated by call_id, not
// message roles.
func TestResponsesRequestEmitsFunctionCallItems(t *testing.T) {
	client := NewOpenAIResponsesClient("openai-responses", "key", "gpt-5.5", "https://api.openai.com/v1")
	body, err := client.requestBody([]Message{
		User("what is the weather"),
		{
			Role: RoleAssistant,
			ToolCalls: []ToolCall{
				{ID: "call_1", Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}},
			},
		},
		ToolMessage("call_1", `{"temp":18}`),
	}, nil, true, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	input := decodePayload(t, body)["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %d items, want 3: %#v", len(input), input)
	}
	call := input[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "get_weather" {
		t.Fatalf("function_call item = %#v", call)
	}
	// Arguments stay a JSON string here, unlike the Anthropic format.
	if call["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("arguments = %#v", call["arguments"])
	}
	result := input[2].(map[string]any)
	if result["type"] != "function_call_output" || result["call_id"] != "call_1" {
		t.Fatalf("function_call_output item = %#v", result)
	}
}

func TestResponsesRequestSendsToolsAtTopLevel(t *testing.T) {
	client := NewOpenAIResponsesClient("openai-responses", "key", "gpt-5.5", "https://api.openai.com/v1")
	body, err := client.requestBody([]Message{User("hi")}, []ToolDefinition{{
		Name:        "get_weather",
		Description: "Get weather",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
	}}, true, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tools := decodePayload(t, body)["tools"].([]any)
	tool := tools[0].(map[string]any)
	// Function tools are flattened here: no nested "function" object.
	if tool["type"] != "function" || tool["name"] != "get_weather" {
		t.Fatalf("tool = %#v", tool)
	}
	if _, ok := tool["function"]; ok {
		t.Fatalf("tool should not nest a function object: %#v", tool)
	}
	if _, ok := tool["parameters"].(map[string]any); !ok {
		t.Fatalf("parameters = %#v, want the schema object", tool["parameters"])
	}
}

func TestParseResponsesResponseReadsOutputItems(t *testing.T) {
	resp, err := ParseResponsesResponse([]byte(`{
	  "status": "completed",
	  "output": [
	    {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Let me check."}]},
	    {"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}
	  ],
	  "usage": {"input_tokens": 100, "output_tokens": 20, "input_tokens_details": {"cached_tokens": 40}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Let me check." {
		t.Fatalf("content = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool calls = %#v", resp.ToolCalls)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", resp.FinishReason)
	}
	// Cached tokens are excluded from the fresh input count, matching the
	// accounting the Chat Completions client reports.
	if resp.InputTokens != 60 || resp.CachedInputTokens != 40 {
		t.Fatalf("usage = %+v, want input=60 cached=40", resp)
	}
}

func TestParseResponsesStreamAccumulatesTextAndFunctionCalls(t *testing.T) {
	var content strings.Builder
	resp, err := ParseResponsesStream(strings.NewReader(`event: response.created
data: {"type":"response.created","response":{"status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"delta":"Let me"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"delta":" check."}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"city\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"\"Paris\"}"}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":50,"output_tokens":9,"input_tokens_details":{"cached_tokens":10}}}}

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
	// The call_id is only known from the opening item; the deltas reference
	// the item id, so the two have to be bridged.
	if call.ID != "call_1" {
		t.Fatalf("tool call id = %q, want call_1 (not the item id)", call.ID)
	}
	if call.Function.Name != "get_weather" {
		t.Fatalf("tool call name = %q", call.Function.Name)
	}
	if call.Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("arguments = %q, want the concatenated fragments", call.Function.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q", resp.FinishReason)
	}
	if resp.InputTokens != 40 || resp.OutputTokens != 9 || resp.CachedInputTokens != 10 {
		t.Fatalf("usage = %+v, want input=40 output=9 cached=10", resp)
	}
}

func TestParseResponsesStreamSurfacesFailure(t *testing.T) {
	_, err := ParseResponsesStream(strings.NewReader(`event: response.failed
data: {"type":"response.failed","response":{"status":"failed","error":{"message":"model overloaded"}}}

`), StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "model overloaded") {
		t.Fatalf("err = %v, want the reported failure", err)
	}
}

func TestResponsesClientReturnsTypedAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer server.Close()

	client := NewOpenAIResponsesClient("openai-responses", "key", "gpt-5.5", server.URL)
	_, err := client.Chat(context.Background(), []Message{User("hi")}, nil, StreamOptions{})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want a typed APIError with status 401", err)
	}
}

func TestResponsesURL(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"https://api.openai.com/v1", "https://api.openai.com/v1/responses"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1/responses"},
		{"https://api.openai.com/v1/responses", "https://api.openai.com/v1/responses"},
		{"http://127.0.0.1:3425/v1", "http://127.0.0.1:3425/v1/responses"},
	} {
		if got := responsesURL(tc.base); got != tc.want {
			t.Fatalf("responsesURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}
