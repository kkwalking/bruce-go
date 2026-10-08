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

// anthropicVersion is the API version sent on every request. Anthropic requires
// it; the value is the stable release of the Messages API.
const anthropicVersion = "2023-06-01"

// anthropicDefaultMaxTokens is sent when the caller does not set a limit.
// max_tokens is a required field of the Messages API, and unlike the OpenAI
// shape there is no server-side default to fall back on.
const anthropicDefaultMaxTokens = 8192

// AnthropicClient talks to the Anthropic Messages API.
//
// The Messages format differs from Chat Completions in ways that cannot be
// papered over by renaming fields: the system prompt is a top-level parameter
// rather than a message, tool results are content blocks inside a user turn
// rather than a message role, and tool arguments travel as JSON objects rather
// than as strings. This client performs that translation in both directions.
type AnthropicClient struct {
	Provider        string
	APIKey          string
	Model           string
	APIURL          string
	HTTPClient      *http.Client
	reasoningEffort string
	contextWindow   int
	maxOutputTokens int
}

func NewAnthropicClient(provider, apiKey, model, baseURL string) *AnthropicClient {
	return &AnthropicClient{
		Provider:   provider,
		APIKey:     apiKey,
		Model:      model,
		APIURL:     anthropicMessagesURL(baseURL),
		HTTPClient: &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *AnthropicClient) ProviderName() string { return c.Provider }
func (c *AnthropicClient) ModelName() string    { return c.Model }
func (c *AnthropicClient) SupportsTools() bool  { return true }

// SupportsPromptCaching reports true because requestBody places explicit
// cache_control breakpoints on the tool and system prefixes.
func (c *AnthropicClient) SupportsPromptCaching() bool { return true }

// SupportsImages reports true for every model: the Messages API accepts image
// blocks for all current Claude models, and a gateway fronting a non-vision
// model rejects the request with a clear error of its own.
func (c *AnthropicClient) SupportsImages() bool { return true }

func (c *AnthropicClient) SetReasoningEffort(effort string) { c.reasoningEffort = effort }
func (c *AnthropicClient) ReasoningEffort() string          { return c.reasoningEffort }

func (c *AnthropicClient) MaxContextWindow() int {
	if c.contextWindow > 0 {
		return c.contextWindow
	}
	contextWindow, _ := builtInModelCapability(c.Provider, c.Model)
	return contextWindow
}

func (c *AnthropicClient) MaxOutputTokens() int {
	if c.maxOutputTokens > 0 {
		return c.maxOutputTokens
	}
	_, maxOutputTokens := builtInModelCapability(c.Provider, c.Model)
	return maxOutputTokens
}

func (c *AnthropicClient) SetModelCapability(contextWindow, maxOutputTokens int) {
	c.contextWindow = contextWindow
	c.maxOutputTokens = maxOutputTokens
}

func (c *AnthropicClient) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, opts StreamOptions) (ChatResponse, error) {
	body, err := c.requestBody(messages, tools, true, opts)
	if err != nil {
		return ChatResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIURL, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
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
		result, err := ParseAnthropicStream(resp.Body, opts)
		result.Provider, result.Model = c.Provider, c.Model
		return result, err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, err
	}
	result, err := ParseAnthropicResponse(data)
	result.Provider, result.Model = c.Provider, c.Model
	return result, err
}

func (c *AnthropicClient) requestBody(messages []Message, tools []ToolDefinition, stream bool, opts StreamOptions) ([]byte, error) {
	system, turns, err := anthropicMessages(messages)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"model":      c.Model,
		"max_tokens": c.maxTokens(opts),
		"stream":     stream,
		"messages":   turns,
	}
	if system != "" {
		// Caching the system prompt also caches the tool definitions rendered
		// before it, because the cache prefix is ordered tools -> system ->
		// messages. One breakpoint here therefore covers both.
		payload["system"] = []any{anthropicCacheableTextBlock(system)}
	}
	if len(tools) > 0 {
		payload["tools"] = anthropicTools(tools)
	}
	return json.Marshal(payload)
}

// maxTokens returns the output limit to send. The Messages API requires the
// field, so an unset caller limit falls back to the model's declared capability
// and finally to a fixed default.
func (c *AnthropicClient) maxTokens(opts StreamOptions) int {
	if opts.MaxTokens > 0 {
		return opts.MaxTokens
	}
	if limit := c.MaxOutputTokens(); limit > 0 {
		return limit
	}
	return anthropicDefaultMaxTokens
}

// anthropicCacheableTextBlock renders a text block carrying a cache
// breakpoint, so the prefix up to and including it is cached.
func anthropicCacheableTextBlock(text string) map[string]any {
	return map[string]any{
		"type":          "text",
		"text":          text,
		"cache_control": map[string]any{"type": "ephemeral"},
	}
}

// anthropicTools converts tool definitions and places a cache breakpoint on the
// last one, which caches the whole tool block.
func anthropicTools(tools []ToolDefinition) []any {
	out := make([]any, 0, len(tools))
	for i, tool := range tools {
		schema := any(map[string]any{"type": "object", "properties": map[string]any{}})
		if len(tool.Parameters) > 0 {
			schema = json.RawMessage(tool.Parameters)
		}
		entry := map[string]any{
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": schema,
		}
		if i == len(tools)-1 {
			entry["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		out = append(out, entry)
	}
	return out
}

// anthropicMessages splits the conversation into the top-level system prompt and
// the user/assistant turns.
//
// The Messages API has no "system" or "tool" role: system text is hoisted out of
// the array, and a tool result becomes a tool_result content block inside a user
// turn. Consecutive turns with the same role are merged, which the API requires
// and which also keeps a tool result adjacent to the assistant turn that asked
// for it.
func anthropicMessages(messages []Message) (string, []any, error) {
	var systemParts []string
	turns := make([]any, 0, len(messages))
	for _, message := range messages {
		if message.Role == RoleSystem {
			if text := strings.TrimSpace(message.Content); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		blocks, err := anthropicBlocks(message)
		if err != nil {
			return "", nil, err
		}
		if len(blocks) == 0 {
			continue
		}
		role := RoleUser
		if message.Role == RoleAssistant {
			role = RoleAssistant
		}
		// A tool result arrives as its own message but belongs in a user turn.
		if len(turns) > 0 {
			last := turns[len(turns)-1].(map[string]any)
			if last["role"] == role {
				last["content"] = append(last["content"].([]any), blocks...)
				continue
			}
		}
		turns = append(turns, map[string]any{"role": role, "content": blocks})
	}
	return strings.Join(systemParts, "\n\n"), turns, nil
}

// anthropicBlocks renders one message as Anthropic content blocks.
func anthropicBlocks(message Message) ([]any, error) {
	blocks := make([]any, 0, len(message.ContentParts)+len(message.ToolCalls)+1)
	if message.Role == RoleTool {
		blocks = append(blocks, map[string]any{
			"type":        "tool_result",
			"tool_use_id": message.ToolCallID,
			"content":     message.Content,
		})
		return blocks, nil
	}
	if len(message.ContentParts) > 0 {
		for _, part := range message.ContentParts {
			if part.Type == ContentImageURL {
				block, err := anthropicImageBlock(part)
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, block)
				continue
			}
			if part.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
			}
		}
	} else if message.Content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": message.Content})
	}
	for _, call := range message.ToolCalls {
		input := any(map[string]any{})
		if strings.TrimSpace(call.Function.Arguments) != "" {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(call.Function.Arguments), &parsed); err != nil {
				return nil, errors.New("tool call arguments for " + call.Function.Name + " are not a JSON object: " + err.Error())
			}
			input = parsed
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Function.Name,
			"input": input,
		})
	}
	return blocks, nil
}

// anthropicImageBlock converts a data-URL image part into an image block. The
// Messages API takes base64 inline data and rejects remote URLs, which is
// already the form ProcessImageBytes produces.
func anthropicImageBlock(part ContentPart) (map[string]any, error) {
	mimeType, data, err := splitDataURL(part.ImageURL)
	if err != nil {
		return nil, err
	}
	if mimeType == "" {
		mimeType = part.MIMEType
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mimeType,
			"data":       data,
		},
	}, nil
}

// splitDataURL separates a data: URL into its media type and payload.
func splitDataURL(value string) (mimeType, data string, err error) {
	if !strings.HasPrefix(value, "data:") {
		return "", "", errors.New("image content must be a base64 data URL; remote image URLs are not supported by this protocol")
	}
	comma := strings.Index(value, ",")
	if comma < 0 {
		return "", "", errors.New("image data URL is missing its payload separator")
	}
	prefix := value[len("data:"):comma]
	if semi := strings.Index(prefix, ";"); semi >= 0 {
		mimeType = prefix[:semi]
	} else {
		mimeType = prefix
	}
	return mimeType, value[comma+1:], nil
}

func ParseAnthropicResponse(data []byte) (ChatResponse, error) {
	var root struct {
		Content    []anthropicContentBlock `json:"content"`
		StopReason string                  `json:"stop_reason"`
		Usage      anthropicUsage          `json:"usage"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return ChatResponse{}, err
	}
	if len(root.Content) == 0 {
		return ChatResponse{}, errors.New("API response has no content blocks")
	}
	var content strings.Builder
	var calls []ToolCall
	for _, block := range root.Content {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "tool_use":
			arguments := "{}"
			if len(block.Input) > 0 {
				arguments = string(block.Input)
			}
			calls = append(calls, ToolCall{
				ID:       block.ID,
				Function: FunctionCall{Name: block.Name, Arguments: arguments},
			})
		}
	}
	return root.Usage.response(RoleAssistant, root.StopReason, content.String(), calls), nil
}

// ParseAnthropicStream reads the Messages API SSE stream. The event name is
// carried inside the data payload as "type", so only data lines need to be
// tracked; the event: lines are ignored.
func ParseAnthropicStream(r io.Reader, opts StreamOptions) (ChatResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data strings.Builder
	acc := &anthropicStreamAccumulator{role: RoleAssistant, blocks: map[int]*anthropicBlockDelta{}}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			done, err := parseAnthropicStreamData(data.String(), acc, opts)
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
		if _, err := parseAnthropicStreamData(data.String(), acc, opts); err != nil {
			return ChatResponse{}, err
		}
	}
	return acc.response(), scanner.Err()
}

func parseAnthropicStreamData(data string, acc *anthropicStreamAccumulator, opts StreamOptions) (bool, error) {
	payload := strings.TrimSpace(data)
	if payload == "" {
		return false, nil
	}
	var event struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		// message_start nests the initial usage inside the message object,
		// while message_delta reports it at the top level.
		Message struct {
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return false, err
	}
	switch event.Type {
	case "message_start":
		acc.usage.merge(event.Message.Usage)
	case "content_block_start":
		if event.ContentBlock.Type == "tool_use" {
			acc.blocks[event.Index] = &anthropicBlockDelta{
				kind: "tool_use",
				id:   event.ContentBlock.ID,
				name: event.ContentBlock.Name,
			}
		} else {
			acc.blocks[event.Index] = &anthropicBlockDelta{kind: "text"}
		}
	case "content_block_delta":
		block := acc.blocks[event.Index]
		if block == nil {
			block = &anthropicBlockDelta{kind: "text"}
			acc.blocks[event.Index] = block
		}
		switch event.Delta.Type {
		case "input_json_delta":
			block.arguments.WriteString(event.Delta.PartialJSON)
		default:
			if event.Delta.Text != "" {
				acc.content.WriteString(event.Delta.Text)
				if opts.OnContent != nil {
					opts.OnContent(event.Delta.Text)
				}
			}
		}
	case "message_delta":
		if event.Delta.StopReason != "" {
			acc.stopReason = event.Delta.StopReason
		}
		acc.usage.merge(event.Usage)
	case "message_stop":
		return true, nil
	case "error":
		return false, errors.New("Anthropic stream error: " + payload)
	}
	return false, nil
}

// anthropicBlockDelta accumulates one content block. Tool arguments arrive as
// fragments of a JSON string and are only parseable once the block closes.
type anthropicBlockDelta struct {
	kind      string
	id        string
	name      string
	arguments strings.Builder
}

type anthropicStreamAccumulator struct {
	role       string
	content    strings.Builder
	blocks     map[int]*anthropicBlockDelta
	usage      anthropicUsage
	stopReason string
}

func (a *anthropicStreamAccumulator) response() ChatResponse {
	resp := ChatResponse{
		Role:              a.role,
		Content:           a.content.String(),
		InputTokens:       a.usage.InputTokens,
		OutputTokens:      a.usage.OutputTokens,
		CachedInputTokens: a.usage.CacheReadInputTokens,
		FinishReason:      anthropicFinishReason(a.stopReason),
	}
	// Blocks are indexed, and tool calls must keep their original order.
	for i := 0; i < len(a.blocks); i++ {
		block := a.blocks[i]
		if block == nil || block.kind != "tool_use" {
			continue
		}
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			ID:       block.id,
			Function: FunctionCall{Name: block.name, Arguments: block.arguments.String()},
		})
	}
	return resp
}

// anthropicContentBlock is one entry of a non-streaming response's content.
type anthropicContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// anthropicUsage is the usage object of both the non-streaming response and the
// message_start / message_delta stream events. Fields are cumulative, so a
// later event supersedes an earlier one only when it reports a value.
type anthropicUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

func (u *anthropicUsage) merge(next anthropicUsage) {
	if next.InputTokens > 0 {
		u.InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		u.OutputTokens = next.OutputTokens
	}
	if next.CacheReadInputTokens > 0 {
		u.CacheReadInputTokens = next.CacheReadInputTokens
	}
}

func (u anthropicUsage) response(role, stopReason string, content string, calls []ToolCall) ChatResponse {
	return ChatResponse{
		Role:              role,
		Content:           content,
		ToolCalls:         calls,
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
		FinishReason:      anthropicFinishReason(stopReason),
	}
}

// anthropicFinishReason maps the Messages API stop reasons onto the
// Chat Completions vocabulary the rest of the codebase reasons about, so
// overflow detection and retry logic keep working unchanged.
func anthropicFinishReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return reason
	}
}

func (c *AnthropicClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 120 * time.Second}
}

// anthropicMessagesURL joins a base URL with the Messages API path. A base that
// already ends in /v1/messages is used as-is, so the field accepts either the
// host root or the full endpoint.
func anthropicMessagesURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	switch {
	case strings.HasSuffix(base, "/v1/messages"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/messages"
	default:
		return base + "/v1/messages"
	}
}
