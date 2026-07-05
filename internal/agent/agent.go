package agent

import (
	"context"
	"errors"
	"strings"

	"bruce-go/internal/event"
	"bruce-go/internal/llm"
	"bruce-go/internal/runtime"
	"bruce-go/internal/skill"
	"bruce-go/internal/tool"
)

const baseSystemPrompt = `你是 Bruce Coding Agent，一个智能编程助手，可以帮助用户完成各种任务。

当需要观察文件、修改代码、执行命令或创建项目时，请使用工具调用。
使用工具后，根据工具返回的结果继续思考下一步行动。
如果任务已经完成，请直接给出最终答复，不要继续调用工具。
回答用户时保持简洁，并清晰标注相关文件路径。

请用中文回复用户。`

const maxRetries = 2


type Agent struct {
	Client        llm.ChatClient
	Tools         *tool.Registry
	Executor      tool.ParallelExecutor
	SystemPrompt  string
	History       []llm.Message
	MaxIterations int
	Events        *event.Bus
}

func New(client llm.ChatClient, registry *tool.Registry, additional string, config runtime.ConcurrencyConfig, events *event.Bus) *Agent {
	a := &Agent{
		Client:        client,
		Tools:         registry,
		SystemPrompt:  buildPrompt(registry, additional),
		MaxIterations: 40,
		Events:        events,
	}
	a.Executor = tool.ParallelExecutor{Registry: registry, Config: config.Normalize()}
	a.ClearHistory()
	return a
}

func (a *Agent) ClearHistory() {
	a.History = []llm.Message{llm.System(a.SystemPrompt)}
}

func (a *Agent) RestoreHistory(messages []llm.Message) {
	a.ClearHistory()
	for _, msg := range messages {
		if msg.Role == llm.RoleSystem {
			continue
		}
		a.History = append(a.History, msg)
	}
}

func (a *Agent) Run(ctx context.Context, input llm.PreparedInput, taskContext string, runID string) (string, error) {
	if strings.TrimSpace(input.Message.Content) == "" && len(input.Message.ContentParts) == 0 {
		return "请输入任务内容", nil
	}
	if input.Message.HasImages() && !a.Client.SupportsImages() {
		msg := "当前模型 " + a.Client.ModelName() + " [" + a.Client.ProviderName() + "] 不支持图片，请切换到支持视觉的模型（如 glm-5v-turbo）"
		a.emit(event.NewMessageCompleted(runID, llm.Assistant(msg), false))
		return msg, nil
	}
	if runID == "" {
		runID = event.NewRunID()
	}
	if taskContext != "" {
		a.History = append(a.History, llm.System(taskContext))
		defer func() {
			for i := len(a.History) - 1; i >= 0; i-- {
				if a.History[i].Role == llm.RoleSystem && a.History[i].Content == taskContext {
					a.History = append(a.History[:i], a.History[i+1:]...)
					break
				}
			}
		}()
	}
	defer a.redactSkillToolResults()
	a.appendDurable(runID, input.Message)
	retryCount := 0
	for i := 0; i < a.MaxIterations; i++ {
		select {
		case <-ctx.Done():
			return "任务已被用户中断", nil
		default:
		}
		pruneImages(a.History)
		a.emit(event.NewMessageStarted(runID, llm.RoleAssistant))
		resp, err := a.Client.Chat(ctx, a.History, a.Tools.Definitions(), streamToEvents(a.Events, runID))
		if err != nil {
			if errors.Is(err, context.Canceled) {
				a.emit(event.NewMessageCompleted(runID, llm.Assistant(""), false))
				return "任务已被用户中断", nil
			}
			if retryCount < maxRetries && isRetryable(err) {
				retryCount++
				continue
			}
			out := "网络错误: " + err.Error()
			a.emit(event.NewMessageCompleted(runID, llm.Assistant(out), false))
			return out, nil
		}
		retryCount = 0
		if resp.HasToolCalls() {
			assistant := llm.Message{
				Role:             llm.RoleAssistant,
				Content:          resp.Content,
				ReasoningContent: resp.ReasoningContent,
				ToolCalls:        resp.ToolCalls,
			}
			a.append(assistant)
			a.emit(event.NewMessageCompleted(runID, assistant, true))
			for _, call := range resp.ToolCalls {
				a.emit(event.NewToolCallStarted(runID, call))
			}
			results := a.Executor.Execute(ctx, resp.ToolCalls)
			a.appendImageToolMessages(runID, results)
			for _, result := range results {
				a.emit(event.NewToolCallCompleted(runID, result))
				toolMessage := llm.ToolMessage(result.ToolCall.ID, result.Result)
				a.append(toolMessage)
				a.emit(event.NewMessageCompleted(runID, a.durableToolMessage(result.ToolCall, toolMessage), true))
			}
			continue
		}
		msg := llm.Message{
			Role:              llm.RoleAssistant,
			Content:           resp.Content,
			ReasoningContent:  resp.ReasoningContent,
			InputTokens:       resp.InputTokens,
			OutputTokens:      resp.OutputTokens,
			CachedInputTokens: resp.CachedInputTokens,
		}
		a.append(msg)
		a.emit(event.NewMessageCompleted(runID, msg, true))
		return resp.Content, nil
	}
	stopped := "达到最大迭代次数限制"
	a.appendDurable(runID, llm.Assistant(stopped))
	return stopped, nil
}

func (a *Agent) appendImageToolMessages(runID string, results []tool.ToolCallResult) {
	for _, result := range results {
		if len(result.ImageParts) == 0 {
			continue
		}
		parts := make([]llm.ContentPart, 0, len(result.ImageParts)+1)
		parts = append(parts, llm.TextPart(
			"工具 "+result.ToolCall.Function.Name+" 返回了图片内容，请结合上面的工具文本结果分析。",
		))
		parts = append(parts, result.ImageParts...)
		msg := llm.Message{
			Role:         llm.RoleUser,
			Content:      llm.PlainText(parts),
			ContentParts: parts,
		}
		a.append(msg)
		a.emit(event.NewMessageCompleted(runID, msg, true))
	}
}

// isRetryable returns true when an error from Chat() is a transient network failure.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "HTTP 5") || strings.Contains(msg, "network") ||
		strings.Contains(msg, "timeout") || strings.Contains(msg, "temporary") ||
		strings.Contains(msg, "connection") || strings.Contains(msg, "deadline")
}



func (a *Agent) append(msg llm.Message) {
	a.History = append(a.History, msg)
}

func (a *Agent) appendDurable(runID string, msg llm.Message) {
	a.append(msg)
	a.emit(event.NewMessageCompleted(runID, msg, true))
}

func (a *Agent) durableToolMessage(call llm.ToolCall, msg llm.Message) llm.Message {
	if skill.IsSkillTool(call.Function.Name) {
		return llm.ToolMessage(msg.ToolCallID, "[Skill 内容仅对原任务有效，已从历史中移除]")
	}
	return msg
}

func (a *Agent) redactSkillToolResults() {
	skillCallIDs := map[string]bool{}
	for _, msg := range a.History {
		for _, call := range msg.ToolCalls {
			if skill.IsSkillTool(call.Function.Name) {
				skillCallIDs[call.ID] = true
			}
		}
	}
	if len(skillCallIDs) == 0 {
		return
	}
	for i, msg := range a.History {
		if msg.Role == llm.RoleTool && skillCallIDs[msg.ToolCallID] {
			a.History[i] = llm.ToolMessage(msg.ToolCallID, "[Skill 内容仅对原任务有效，已从历史中移除]")
		}
	}
}

func (a *Agent) emit(evt event.Event) {
	if a.Events != nil {
		a.Events.Emit(evt)
	}
}

func buildPrompt(registry *tool.Registry, additional string) string {
	prompt := baseSystemPrompt
	if registry != nil {
		prompt += "\n\n" + registry.BuildPrompt()
	}
	if strings.TrimSpace(additional) != "" {
		prompt += "\n\n" + strings.TrimSpace(additional)
	}
	return prompt
}

func streamToEvents(bus *event.Bus, runID string) llm.StreamOptions {
	if bus == nil {
		return llm.StreamOptions{}
	}
	return llm.StreamOptions{
		OnContent:   func(delta string) { bus.Emit(event.NewMessageDelta(runID, llm.RoleAssistant, "content", delta)) },
		OnReasoning: func(delta string) { bus.Emit(event.NewMessageDelta(runID, llm.RoleAssistant, "reasoning", delta)) },
	}
}


func pruneImages(messages []llm.Message) {
	latest := -1
	for i, msg := range messages {
		if msg.HasImages() {
			latest = i
		}
	}
	for i := range messages {
		if i != latest && messages[i].HasImages() {
			messages[i] = messages[i].WithoutImages()
		}
	}
}

type FakeClient struct {
	Provider  string
	Model     string
	Responses []llm.ChatResponse
	Calls     int
	Err       error
}

func (f *FakeClient) Chat(_ context.Context, _ []llm.Message, _ []llm.ToolDefinition, opts llm.StreamOptions) (llm.ChatResponse, error) {
	if f.Err != nil {
		return llm.ChatResponse{}, f.Err
	}
	if f.Calls >= len(f.Responses) {
		return llm.ChatResponse{}, errors.New("fake client response exhausted")
	}
	resp := f.Responses[f.Calls]
	f.Calls++
	if resp.ReasoningContent != "" && opts.OnReasoning != nil {
		opts.OnReasoning(resp.ReasoningContent)
	}
	if resp.Content != "" && opts.OnContent != nil {
		opts.OnContent(resp.Content)
	}
	return resp, nil
}

func (f *FakeClient) ProviderName() string {
	if f.Provider == "" {
		return "fake"
	}
	return f.Provider
}

func (f *FakeClient) ModelName() string {
	if f.Model == "" {
		return "fake-model"
	}
	return f.Model
}

func (*FakeClient) MaxContextWindow() int       { return 200000 }
func (*FakeClient) SupportsTools() bool         { return true }
func (*FakeClient) SupportsPromptCaching() bool { return false }
func (*FakeClient) SupportsImages() bool         { return true }
