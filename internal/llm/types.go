package llm

import (
	"context"
	"encoding/json"
)

const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

type StreamListener interface {
	OnReasoningDelta(delta string)
	OnContentDelta(delta string)
}

type NoopStreamListener struct{}

func (NoopStreamListener) OnReasoningDelta(string) {}
func (NoopStreamListener) OnContentDelta(string)   {}

type ChatClient interface {
	Chat(ctx context.Context, messages []Message, tools []ToolDefinition, listener StreamListener) (ChatResponse, error)
	ProviderName() string
	ModelName() string
	MaxContextWindow() int
	SupportsTools() bool
	SupportsPromptCaching() bool
	SupportsImages() bool
}

type Message struct {
	Role              string        `json:"role"`
	Content           string        `json:"content,omitempty"`
	ReasoningContent  string        `json:"reasoningContent,omitempty"`
	ToolCalls         []ToolCall    `json:"toolCalls,omitempty"`
	ToolCallID        string        `json:"toolCallId,omitempty"`
	ContentParts      []ContentPart `json:"contentParts,omitempty"`
	InputTokens       int           `json:"inputTokens,omitempty"`
	OutputTokens      int           `json:"outputTokens,omitempty"`
	CachedInputTokens int           `json:"cachedInputTokens,omitempty"`
}

func System(content string) Message    { return Message{Role: RoleSystem, Content: content} }
func User(content string) Message      { return Message{Role: RoleUser, Content: content} }
func Assistant(content string) Message { return Message{Role: RoleAssistant, Content: content} }
func ToolMessage(toolCallID, content string) Message {
	return Message{Role: RoleTool, ToolCallID: toolCallID, Content: content}
}

func UserParts(parts []ContentPart) Message {
	return Message{Role: RoleUser, Content: PlainText(parts), ContentParts: append([]ContentPart(nil), parts...)}
}

func (m Message) HasToolCalls() bool { return len(m.ToolCalls) > 0 }
func (m Message) HasImages() bool {
	for _, part := range m.ContentParts {
		if part.Type == ContentImageURL {
			return true
		}
	}
	return false
}

func (m Message) WithoutImages() Message {
	if !m.HasImages() {
		return m
	}
	m.ContentParts = []ContentPart{TextPart("[历史图片内容已移除，仅保留文字占位]")}
	if m.Content == "" {
		m.Content = "[历史图片内容已移除，仅保留文字占位]"
	} else {
		m.Content += "\n[历史图片内容已移除，仅保留文字占位]"
	}
	return m
}

func (m Message) TotalUsageTokens() int {
	return m.InputTokens + m.OutputTokens
}

type ContentPartType string

const (
	ContentText     ContentPartType = "text"
	ContentImageURL ContentPartType = "image_url"
)

type ContentPart struct {
	Type     ContentPartType `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL string          `json:"imageUrl,omitempty"`
	MIMEType string          `json:"mimeType,omitempty"`
	Source   string          `json:"source,omitempty"`
}

func TextPart(text string) ContentPart {
	return ContentPart{Type: ContentText, Text: text}
}

func ImagePart(dataURL, mimeType, source string) ContentPart {
	return ContentPart{Type: ContentImageURL, ImageURL: dataURL, MIMEType: mimeType, Source: source}
}

func (p ContentPart) FallbackText() string {
	if p.Type == ContentText {
		return p.Text
	}
	if p.Source == "" {
		return "[image]"
	}
	return "[image: " + p.Source + "]"
}

func PlainText(parts []ContentPart) string {
	out := ""
	for _, part := range parts {
		text := part.FallbackText()
		if text == "" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += text
	}
	return out
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Function FunctionCall `json:"function"`
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ChatResponse struct {
	Role              string
	Content           string
	ReasoningContent  string
	ToolCalls         []ToolCall
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int
}

func (r ChatResponse) HasToolCalls() bool { return len(r.ToolCalls) > 0 }

type ModelOption struct {
	Provider string
	Model    string
}

func (m ModelOption) Display() string {
	if m.Provider == "" {
		return m.Model
	}
	return m.Model + " [" + m.Provider + "]"
}

func (m ModelOption) Selector() string {
	if m.Provider == "" {
		return m.Model
	}
	return m.Provider + "/" + m.Model
}
