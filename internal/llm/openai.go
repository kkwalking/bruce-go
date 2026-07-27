package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

type OpenAICompatibleClient struct {
	Provider        string
	APIKey          string
	Model           string
	APIURL          string
	HTTPClient      *http.Client
	reasoningEffort string
	contextWindow   int
	maxOutputTokens int
}

func NewOpenAICompatibleClient(provider, apiKey, model, baseURL string) *OpenAICompatibleClient {
	return &OpenAICompatibleClient{
		Provider:   provider,
		APIKey:     apiKey,
		Model:      model,
		APIURL:     chatCompletionsURL(baseURL),
		HTTPClient: &http.Client{Timeout: 120 * time.Second},
	}
}

func NewDeepSeekClient(apiKey, model string) *OpenAICompatibleClient {
	if strings.TrimSpace(model) == "" {
		model = "deepseek-v4-flash"
	}
	c := NewOpenAICompatibleClient("deepseek", apiKey, model, "https://api.deepseek.com")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	c.HTTPClient = &http.Client{Timeout: 120 * time.Second, Transport: transport}
	return c
}

func NewGLMClient(apiKey, model string) *OpenAICompatibleClient {
	if strings.TrimSpace(model) == "" {
		model = "glm-5.1"
	}
	base := "https://open.bigmodel.cn/api/coding/paas/v4"
	if strings.HasPrefix(strings.ToLower(model), "glm-5v") {
		base = "https://open.bigmodel.cn/api/paas/v4"
	}
	return NewOpenAICompatibleClient("glm", apiKey, model, base)
}

func (c *OpenAICompatibleClient) ProviderName() string { return c.Provider }
func (c *OpenAICompatibleClient) ModelName() string    { return c.Model }
func (c *OpenAICompatibleClient) SupportsTools() bool  { return true }
func (c *OpenAICompatibleClient) SupportsPromptCaching() bool {
	return c.Provider == "deepseek" || c.Provider == "glm"
}

func (c *OpenAICompatibleClient) SetReasoningEffort(effort string) { c.reasoningEffort = effort }
func (c *OpenAICompatibleClient) ReasoningEffort() string          { return c.reasoningEffort }

func (c *OpenAICompatibleClient) SupportsImages() bool {
	switch c.Provider {
	case "glm":
		return strings.HasPrefix(strings.ToLower(c.Model), "glm-5v")
	case "deepseek":
		return false
	default:
		return strings.Contains(strings.ToLower(c.Model), "vision") ||
			strings.Contains(strings.ToLower(c.Model), "vl")
	}
}
func (c *OpenAICompatibleClient) MaxContextWindow() int {
	if c.contextWindow > 0 {
		return c.contextWindow
	}
	contextWindow, _ := builtInModelCapability(c.Provider, c.Model)
	return contextWindow
}

func (c *OpenAICompatibleClient) MaxOutputTokens() int {
	if c.maxOutputTokens > 0 {
		return c.maxOutputTokens
	}
	_, maxOutputTokens := builtInModelCapability(c.Provider, c.Model)
	return maxOutputTokens
}

func (c *OpenAICompatibleClient) SetModelCapability(contextWindow, maxOutputTokens int) {
	c.contextWindow = contextWindow
	c.maxOutputTokens = maxOutputTokens
}

func builtInModelCapability(provider, model string) (contextWindow, maxOutputTokens int) {
	switch provider + "/" + model {
	case "deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-pro":
		return 1000000, 384000
	case "glm/glm-4.5-air":
		return 131072, 98304
	case "glm/glm-4.7":
		return 204800, 131072
	case "glm/glm-5-turbo", "glm/glm-5.1", "glm/glm-5v-turbo":
		return 200000, 131072
	case "glm/glm-5.2":
		return 1000000, 131072
	default:
		return 0, 0
	}
}

func (c *OpenAICompatibleClient) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, opts StreamOptions) (ChatResponse, error) {
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
		result, err := ParseChatStream(resp.Body, opts)
		result.Provider, result.Model = c.Provider, c.Model
		return result, err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, err
	}
	result, err := ParseChatResponse(data)
	result.Provider, result.Model = c.Provider, c.Model
	return result, err
}

func (c *OpenAICompatibleClient) requestBody(messages []Message, tools []ToolDefinition, stream bool, opts StreamOptions) ([]byte, error) {
	payload := map[string]any{
		"model":  c.Model,
		"stream": stream,
		"messages": func() []any {
			out := make([]any, 0, len(messages))
			for _, msg := range messages {
				out = append(out, serializeMessage(msg, c.Provider, c.Model))
			}
			return out
		}(),
	}
	if stream {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	if opts.MaxTokens > 0 {
		payload["max_tokens"] = opts.MaxTokens
	}
	if len(tools) > 0 {
		serialized := make([]any, 0, len(tools))
		for _, tool := range tools {
			serialized = append(serialized, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tool.Name,
					"description": tool.Description,
					"parameters":  tool.Parameters,
				},
			})
		}
		payload["tools"] = serialized
		payload["tool_choice"] = "auto"
	}
	switch c.reasoningEffort {
	case "", "off":
	default:
		payload["reasoning_effort"] = c.reasoningEffort
		if c.Provider == "deepseek" {
			payload["thinking"] = map[string]any{"type": "enabled"}
		}
	}
	return json.Marshal(payload)
}

func ParseChatResponse(data []byte) (ChatResponse, error) {
	var root struct {
		Choices []struct {
			Message      responseMessage `json:"message"`
			FinishReason string          `json:"finish_reason"`
		} `json:"choices"`
		Usage usage `json:"usage"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return ChatResponse{}, err
	}
	if len(root.Choices) == 0 {
		return ChatResponse{}, errors.New("API response has no choices")
	}
	msg := root.Choices[0].Message
	return ChatResponse{
		Role:              emptyString(msg.Role, RoleAssistant),
		Content:           msg.Content,
		ReasoningContent:  firstNonEmpty(msg.ReasoningContent, msg.Reasoning, msg.ReasoningCamel),
		ToolCalls:         msg.toToolCalls(),
		InputTokens:       root.Usage.inputTokens(),
		OutputTokens:      root.Usage.CompletionTokens,
		CachedInputTokens: root.Usage.cachedTokens(),
		FinishReason:      root.Choices[0].FinishReason,
	}, nil
}

func ParseChatStream(r io.Reader, opts StreamOptions) (ChatResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data strings.Builder
	acc := streamAccumulator{role: RoleAssistant, calls: map[int]*toolCallDelta{}}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			done, err := parseStreamData(data.String(), &acc, opts)
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
		_, err := parseStreamData(data.String(), &acc, opts)
		if err != nil {
			return ChatResponse{}, err
		}
	}
	return acc.response(), scanner.Err()
}

func parseStreamData(data string, acc *streamAccumulator, opts StreamOptions) (bool, error) {
	payload := strings.TrimSpace(data)
	if payload == "" {
		return false, nil
	}
	if payload == "[DONE]" {
		return true, nil
	}
	var root struct {
		Choices []struct {
			Delta        responseMessage `json:"delta"`
			Message      responseMessage `json:"message"`
			FinishReason string          `json:"finish_reason"`
		} `json:"choices"`
		Usage usage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return false, err
	}
	if root.Usage.PromptTokens != 0 || root.Usage.CompletionTokens != 0 {
		acc.usage = root.Usage
	}
	if len(root.Choices) == 0 {
		return false, nil
	}
	if root.Choices[0].FinishReason != "" {
		acc.finishReason = root.Choices[0].FinishReason
	}
	delta := root.Choices[0].Delta
	if delta.IsZero() {
		delta = root.Choices[0].Message
	}
	if delta.Role != "" {
		acc.role = delta.Role
	}
	reasoning := firstNonEmpty(delta.ReasoningContent, delta.Reasoning, delta.ReasoningCamel)
	if reasoning != "" {
		acc.reasoning.WriteString(reasoning)
		if opts.OnReasoning != nil {
			opts.OnReasoning(reasoning)
		}
	}
	if delta.Content != "" {
		acc.content.WriteString(delta.Content)
		if opts.OnContent != nil {
			opts.OnContent(delta.Content)
		}
	}
	for _, call := range delta.ToolCalls {
		d := acc.calls[call.Index]
		if d == nil {
			d = &toolCallDelta{index: call.Index}
			acc.calls[call.Index] = d
		}
		if call.ID != "" {
			d.id = call.ID
		}
		d.name.WriteString(call.Function.Name)
		d.arguments.WriteString(call.Function.Arguments)
	}
	return false, nil
}

type usage struct {
	PromptTokens         int `json:"prompt_tokens"`
	CompletionTokens     int `json:"completion_tokens"`
	CachedInputTokens    int `json:"cached_input_tokens"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	PromptTokenDetails   struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u usage) cachedTokens() int {
	if u.CachedInputTokens > 0 {
		return u.CachedInputTokens
	}
	if u.PromptCacheHitTokens > 0 {
		return u.PromptCacheHitTokens
	}
	return u.PromptTokenDetails.CachedTokens
}

func (u usage) inputTokens() int {
	input := u.PromptTokens - u.cachedTokens()
	if input < 0 {
		return 0
	}
	return input
}

type responseMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
	Reasoning        string `json:"reasoning"`
	ReasoningCamel   string `json:"reasoningContent"`
	ToolCalls        []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func (m responseMessage) IsZero() bool {
	return m.Role == "" &&
		m.Content == "" &&
		m.ReasoningContent == "" &&
		m.Reasoning == "" &&
		m.ReasoningCamel == "" &&
		len(m.ToolCalls) == 0
}

func (m responseMessage) toToolCalls() []ToolCall {
	calls := make([]ToolCall, 0, len(m.ToolCalls))
	for _, call := range m.ToolCalls {
		calls = append(calls, ToolCall{ID: call.ID, Function: FunctionCall{Name: call.Function.Name, Arguments: call.Function.Arguments}})
	}
	return calls
}

type streamAccumulator struct {
	role         string
	content      strings.Builder
	reasoning    strings.Builder
	calls        map[int]*toolCallDelta
	usage        usage
	finishReason string
}

type toolCallDelta struct {
	index     int
	id        string
	name      strings.Builder
	arguments strings.Builder
}

func (a streamAccumulator) response() ChatResponse {
	calls := make([]ToolCall, 0, len(a.calls))
	for i := 0; i < len(a.calls); i++ {
		if d := a.calls[i]; d != nil {
			calls = append(calls, ToolCall{ID: d.id, Function: FunctionCall{Name: d.name.String(), Arguments: d.arguments.String()}})
		}
	}
	return ChatResponse{
		Role:              emptyString(a.role, RoleAssistant),
		Content:           a.content.String(),
		ReasoningContent:  a.reasoning.String(),
		ToolCalls:         calls,
		InputTokens:       a.usage.inputTokens(),
		OutputTokens:      a.usage.CompletionTokens,
		CachedInputTokens: a.usage.cachedTokens(),
		FinishReason:      a.finishReason,
	}
}

func serializeMessage(message Message, provider, model string) map[string]any {
	out := map[string]any{"role": message.Role}
	if len(message.ContentParts) == 0 {
		if message.Content == "" {
			out["content"] = nil
		} else {
			out["content"] = message.Content
		}
	} else {
		parts := make([]any, 0, len(message.ContentParts))
		for _, part := range message.ContentParts {
			if part.Type == ContentText {
				parts = append(parts, map[string]any{"type": "text", "text": part.Text})
			} else {
				url := part.ImageURL
				if provider == "glm" && strings.HasPrefix(strings.ToLower(model), "glm-5v") {
					if comma := strings.Index(url, ","); strings.HasPrefix(url, "data:") && comma >= 0 {
						url = url[comma+1:]
					}
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		}
		out["content"] = parts
	}
	if len(message.ToolCalls) > 0 {
		calls := make([]any, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			calls = append(calls, map[string]any{
				"id":   call.ID,
				"type": "function",
				"function": map[string]any{
					"name":      call.Function.Name,
					"arguments": call.Function.Arguments,
				},
			})
		}
		out["tool_calls"] = calls
	}
	if message.ToolCallID != "" {
		out["tool_call_id"] = message.ToolCallID
	}
	return out
}

func chatCompletionsURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

func (c *OpenAICompatibleClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 120 * time.Second}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func emptyString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
