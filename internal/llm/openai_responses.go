package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAIResponsesClient talks to the OpenAI Responses API.
//
// It is a different wire format from Chat Completions, not a dialect of it: the
// conversation is sent as a flat list of "input" items rather than as messages,
// the system prompt is a top-level "instructions" field, tool calls are their
// own items correlated by call_id, and streaming uses typed semantic events
// instead of uniform delta chunks.
type OpenAIResponsesClient struct {
	Provider        string
	APIKey          string
	Model           string
	APIURL          string
	HTTPClient      *http.Client
	reasoningEffort string
	contextWindow   int
	maxOutputTokens int
}

func NewOpenAIResponsesClient(provider, apiKey, model, baseURL string) *OpenAIResponsesClient {
	return &OpenAIResponsesClient{
		Provider:   provider,
		APIKey:     apiKey,
		Model:      model,
		APIURL:     responsesURL(baseURL),
		HTTPClient: &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *OpenAIResponsesClient) ProviderName() string { return c.Provider }
func (c *OpenAIResponsesClient) ModelName() string    { return c.Model }
func (c *OpenAIResponsesClient) SupportsTools() bool  { return true }
func (c *OpenAIResponsesClient) SupportsPromptCaching() bool {
	// Caching is automatic server-side in this API; there is no request-side
	// breakpoint to place.
	return true
}
func (c *OpenAIResponsesClient) SupportsImages() bool {
	return strings.Contains(strings.ToLower(c.Model), "vision") ||
		strings.Contains(strings.ToLower(c.Model), "vl") ||
		strings.HasPrefix(strings.ToLower(c.Model), "gpt-") ||
		strings.HasPrefix(strings.ToLower(c.Model), "o")
}

func (c *OpenAIResponsesClient) SetReasoningEffort(effort string) { c.reasoningEffort = effort }
func (c *OpenAIResponsesClient) ReasoningEffort() string          { return c.reasoningEffort }

func (c *OpenAIResponsesClient) MaxContextWindow() int {
	if c.contextWindow > 0 {
		return c.contextWindow
	}
	contextWindow, _ := builtInModelCapability(c.Provider, c.Model)
	return contextWindow
}

func (c *OpenAIResponsesClient) MaxOutputTokens() int {
	if c.maxOutputTokens > 0 {
		return c.maxOutputTokens
	}
	_, maxOutputTokens := builtInModelCapability(c.Provider, c.Model)
	return maxOutputTokens
}

func (c *OpenAIResponsesClient) SetModelCapability(contextWindow, maxOutputTokens int) {
	c.contextWindow = contextWindow
	c.maxOutputTokens = maxOutputTokens
}

func (c *OpenAIResponsesClient) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, opts StreamOptions) (ChatResponse, error) {
	body, err := c.requestBody(messages, tools, true, opts)
	if err != nil {
		return ChatResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIURL, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return ChatResponse{}, &APIError{Provider: c.Provider, StatusCode: resp.StatusCode, Status: resp.Status, Body: string(data)}
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		result, err := ParseResponsesStream(resp.Body, opts)
		result.Provider, result.Model = c.Provider, c.Model
		return result, err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, err
	}
	result, err := ParseResponsesResponse(data)
	result.Provider, result.Model = c.Provider, c.Model
	return result, err
}

func (c *OpenAIResponsesClient) requestBody(messages []Message, tools []ToolDefinition, stream bool, opts StreamOptions) ([]byte, error) {
	instructions, input, err := responsesInput(messages)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"model":  c.Model,
		"input":  input,
		"stream": stream,
	}
	if instructions != "" {
		payload["instructions"] = instructions
	}
	if opts.MaxTokens > 0 {
		payload["max_output_tokens"] = opts.MaxTokens
	}
	if len(tools) > 0 {
		serialized := make([]any, 0, len(tools))
		for _, tool := range tools {
			parameters := any(map[string]any{"type": "object", "properties": map[string]any{}})
			if len(tool.Parameters) > 0 {
				parameters = json.RawMessage(tool.Parameters)
			}
			serialized = append(serialized, map[string]any{
				"type":        "function",
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  parameters,
			})
		}
		payload["tools"] = serialized
		payload["tool_choice"] = "auto"
	}
	return json.Marshal(payload)
}

// responsesInput splits the conversation into the top-level instructions and the
// flat input item list.
//
// The input list is not a message array: an assistant's tool call and the
// result that answers it are separate items linked by call_id, and images are
// content parts of an "input_text"/"input_image" message rather than of a
// Chat Completions content array.
func responsesInput(messages []Message) (string, []any, error) {
	var instructions []string
	items := make([]any, 0, len(messages))
	for _, message := range messages {
		if message.Role == RoleSystem {
			if text := strings.TrimSpace(message.Content); text != "" {
				instructions = append(instructions, text)
			}
			continue
		}
		if message.Role == RoleTool {
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": message.ToolCallID,
				"output":  message.Content,
			})
			continue
		}
		content, err := responsesContent(message)
		if err != nil {
			return "", nil, err
		}
		if len(content) > 0 {
			items = append(items, map[string]any{
				"type":    "message",
				"role":    message.Role,
				"content": content,
			})
		}
		// A function call is an item of its own, and must precede the output
		// item that answers it.
		for _, call := range message.ToolCalls {
			items = append(items, map[string]any{
				"type":      "function_call",
				"call_id":   call.ID,
				"name":      call.Function.Name,
				"arguments": call.Function.Arguments,
			})
		}
	}
	return strings.Join(instructions, "\n\n"), items, nil
}

// responsesContent renders a message's content as Responses API parts.
func responsesContent(message Message) ([]any, error) {
	parts := make([]any, 0, len(message.ContentParts)+1)
	if len(message.ContentParts) > 0 {
		for _, part := range message.ContentParts {
			if part.Type == ContentImageURL {
				parts = append(parts, map[string]any{
					"type":      "input_image",
					"image_url": part.ImageURL,
				})
				continue
			}
			if part.Text != "" {
				parts = append(parts, map[string]any{"type": "input_text", "text": part.Text})
			}
		}
		return parts, nil
	}
	if message.Content != "" {
		parts = append(parts, map[string]any{"type": "input_text", "text": message.Content})
	}
	return parts, nil
}

func ParseResponsesResponse(data []byte) (ChatResponse, error) {
	var root responsesPayload
	if err := json.Unmarshal(data, &root); err != nil {
		return ChatResponse{}, err
	}
	if root.Error != nil {
		return ChatResponse{}, errors.New("Responses API error: " + root.Error.Message)
	}
	if len(root.Output) == 0 {
		return ChatResponse{}, errors.New("API response has no output items")
	}
	return root.chatResponse(), nil
}

// ParseResponsesStream reads the Responses API SSE stream. Every event carries
// its own "type"; the terminal events are response.completed,
// response.incomplete and response.failed.
func ParseResponsesStream(r io.Reader, opts StreamOptions) (ChatResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data strings.Builder
	acc := &responsesStreamAccumulator{
		role:    RoleAssistant,
		calls:   map[string]*responsesCallDelta{},
		itemIDs: map[string]string{},
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			done, err := parseResponsesStreamData(data.String(), acc, opts)
			data.Reset()
			if err != nil || done {
				return acc.response(), err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if data.Len() > 0 {
		if _, err := parseResponsesStreamData(data.String(), acc, opts); err != nil {
			return ChatResponse{}, err
		}
	}
	return acc.response(), scanner.Err()
}

func parseResponsesStreamData(data string, acc *responsesStreamAccumulator, opts StreamOptions) (bool, error) {
	payload := strings.TrimSpace(data)
	if payload == "" || payload == "[DONE]" {
		return payload == "[DONE]", nil
	}
	var event struct {
		Type        string           `json:"type"`
		Delta       string           `json:"delta"`
		OutputIndex int              `json:"output_index"`
		ItemID      string           `json:"item_id"`
		Item        responsesItem    `json:"item"`
		Response    responsesPayload `json:"response"`
		Error       *responsesError  `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return false, err
	}
	switch event.Type {
	case "response.output_text.delta":
		if event.Delta != "" {
			acc.content.WriteString(event.Delta)
			if opts.OnContent != nil {
				opts.OnContent(event.Delta)
			}
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if event.Delta != "" {
			acc.reasoning.WriteString(event.Delta)
			if opts.OnReasoning != nil {
				opts.OnReasoning(event.Delta)
			}
		}
	case "response.output_item.added":
		if event.Item.Type == "function_call" {
			acc.addCall(event.Item)
		}
	case "response.function_call_arguments.delta":
		// The delta events identify the item by item_id, not by call_id; the
		// call_id is only known from the output_item.added event that opened it.
		if call := acc.calls[acc.callIDForItem(event.ItemID)]; call != nil {
			call.arguments.WriteString(event.Delta)
		}
	case "response.completed", "response.incomplete":
		acc.finish(event.Response)
		return true, nil
	case "response.failed":
		if event.Response.Error != nil {
			return false, errors.New("Responses API error: " + event.Response.Error.Message)
		}
		return false, errors.New("Responses API reported a failed response")
	case "error":
		if event.Error != nil {
			return false, errors.New("Responses API error: " + event.Error.Message)
		}
		return false, errors.New("Responses API error: " + payload)
	}
	return false, nil
}

type responsesCallDelta struct {
	name      string
	arguments strings.Builder
}

type responsesStreamAccumulator struct {
	role      string
	content   strings.Builder
	reasoning strings.Builder
	calls     map[string]*responsesCallDelta
	// itemIDs maps an output item's id onto its call_id. The argument deltas
	// identify the item only by item_id, while tool results are correlated by
	// call_id, so the two have to be bridged.
	itemIDs map[string]string
	order   []string
	usage   responsesUsage
	status  string
}

func (a *responsesStreamAccumulator) addCall(item responsesItem) {
	if a.calls[item.CallID] == nil {
		a.calls[item.CallID] = &responsesCallDelta{name: item.Name}
		a.order = append(a.order, item.CallID)
	}
	if item.ID != "" {
		a.itemIDs[item.ID] = item.CallID
	}
}

func (a *responsesStreamAccumulator) callIDForItem(itemID string) string {
	if itemID == "" {
		return ""
	}
	return a.itemIDs[itemID]
}

// finish records the terminal response object, which carries the usage that the
// individual deltas do not.
func (a *responsesStreamAccumulator) finish(response responsesPayload) {
	if response.Usage != nil {
		a.usage = *response.Usage
	}
	a.status = response.Status
}

func (a *responsesStreamAccumulator) response() ChatResponse {
	resp := ChatResponse{
		Role:              a.role,
		Content:           a.content.String(),
		ReasoningContent:  a.reasoning.String(),
		InputTokens:       a.usage.inputTokens(),
		OutputTokens:      a.usage.OutputTokens,
		CachedInputTokens: a.usage.cachedTokens(),
		FinishReason:      responsesFinishReason(a.status, len(a.order)),
	}
	for _, callID := range a.order {
		call := a.calls[callID]
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			ID:       callID,
			Function: FunctionCall{Name: call.name, Arguments: call.arguments.String()},
		})
	}
	return resp
}

// responsesPayload is the response object returned whole, or nested inside the
// response.completed event.
type responsesPayload struct {
	Status string          `json:"status"`
	Output []responsesItem `json:"output"`
	Usage  *responsesUsage `json:"usage"`
	Error  *responsesError `json:"error"`
}

type responsesItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Arguments string `json:"arguments"`
}

type responsesError struct {
	Message string `json:"message"`
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	InputDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (u responsesUsage) cachedTokens() int { return u.InputDetails.CachedTokens }

// inputTokens excludes cached tokens, matching the accounting the Chat
// Completions client reports: DetectContextOverflowResponse adds the cached
// count back before comparing against the context window.
func (u responsesUsage) inputTokens() int {
	input := u.InputTokens - u.cachedTokens()
	if input < 0 {
		return 0
	}
	return input
}

// chatResponse assembles a ChatResponse from a complete response object.
func (p responsesPayload) chatResponse() ChatResponse {
	var content, reasoning strings.Builder
	var calls []ToolCall
	for _, item := range p.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					content.WriteString(part.Text)
				}
			}
		case "reasoning":
			for _, part := range item.Content {
				if part.Text != "" {
					reasoning.WriteString(part.Text)
				}
			}
		case "function_call":
			arguments := item.Arguments
			if strings.TrimSpace(arguments) == "" {
				arguments = "{}"
			}
			calls = append(calls, ToolCall{
				ID:       item.CallID,
				Function: FunctionCall{Name: item.Name, Arguments: arguments},
			})
		}
	}
	resp := ChatResponse{
		Role:             RoleAssistant,
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		ToolCalls:        calls,
		FinishReason:     responsesFinishReason(p.Status, len(calls)),
	}
	if p.Usage != nil {
		resp.InputTokens = p.Usage.inputTokens()
		resp.OutputTokens = p.Usage.OutputTokens
		resp.CachedInputTokens = p.Usage.cachedTokens()
	}
	return resp
}

// responsesFinishReason maps the Responses API status onto the Chat Completions
// vocabulary used elsewhere in the codebase.
func responsesFinishReason(status string, toolCalls int) string {
	switch status {
	case "incomplete":
		return "length"
	case "failed":
		return "error"
	}
	if toolCalls > 0 {
		return "tool_calls"
	}
	return "stop"
}

func (c *OpenAIResponsesClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 120 * time.Second}
}

// responsesURL joins a base URL with the Responses API path. A base that already
// names the endpoint is used as-is, so the field accepts either the host root,
// a /v1 root, or the full endpoint.
func responsesURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	switch {
	case strings.HasSuffix(base, "/responses"):
		return base
	default:
		return base + "/responses"
	}
}
