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

const baseSystemPrompt = `You are Bruce Coding Agent, an intelligent coding assistant that helps users complete a wide range of software-engineering tasks.

Use tool calls whenever you need to inspect files, modify code, run commands, or create project artifacts.
After each tool call, use its result to determine the next action.
Once the task is complete, provide the final response immediately and stop calling tools.
Keep responses concise and clearly identify relevant file paths.

Respond in the language most appropriate for the user. Normally, use the language of the user's latest request. If the user explicitly requests a different language, use the requested language. For mixed-language requests, use the dominant language unless the context clearly indicates otherwise.`

const maxRetries = 2

type Agent struct {
	Client         llm.ChatClient
	Tools          *tool.Registry
	Executor       tool.ParallelExecutor
	SystemPrompt   string
	History        []llm.Message
	MaxIterations  int
	Events         *event.Bus
	BeforeChat     func([]llm.Message) error
	skipBeforeChat bool
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
		return "Please enter a task.", nil
	}
	if input.Message.HasImages() && !a.Client.SupportsImages() {
		msg := "The current model, " + a.Client.ModelName() + " [" + a.Client.ProviderName() + "], does not support images. Switch to a vision-capable model, such as glm-5v-turbo."
		a.emit(event.NewMessageCompleted(runID, llm.Assistant(msg), false))
		return msg, nil
	}
	if runID == "" {
		runID = event.NewRunID()
	}
	a.appendDurable(runID, input.Message)
	return a.run(ctx, taskContext, runID)
}

// Continue resumes an interrupted model turn from the existing history without
// appending the user's message a second time.
func (a *Agent) Continue(ctx context.Context, taskContext string, runID string) (string, error) {
	if runID == "" {
		runID = event.NewRunID()
	}
	return a.run(ctx, taskContext, runID)
}

func (a *Agent) run(ctx context.Context, taskContext string, runID string) (string, error) {
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
	retryCount := 0
	for i := 0; i < a.MaxIterations; i++ {
		select {
		case <-ctx.Done():
			return "The task was interrupted by the user.", nil
		default:
		}
		if a.skipBeforeChat {
			a.skipBeforeChat = false
		} else if a.BeforeChat != nil {
			if err := a.BeforeChat(a.History); err != nil {
				return "", err
			}
		}
		pruneImages(a.History)
		a.emit(event.NewMessageStarted(runID, llm.RoleAssistant))
		resp, err := a.Client.Chat(ctx, a.History, a.Tools.Definitions(), streamToEvents(a.Events, runID))
		if err != nil {
			if errors.Is(err, context.Canceled) {
				a.emit(event.NewMessageCompleted(runID, llm.Assistant(""), false))
				return "The task was interrupted by the user.", nil
			}
			if llm.IsContextOverflowError(err) {
				a.emit(event.NewMessageCompleted(runID, llm.Assistant(""), false))
				return "", &llm.ContextOverflowError{Cause: err}
			}
			if retryCount < maxRetries && isRetryable(err) {
				retryCount++
				continue
			}
			out := "Network error: " + err.Error()
			a.emit(event.NewMessageCompleted(runID, llm.Assistant(out), false))
			return out, nil
		}
		retryCount = 0
		assistant := a.assistantMessage(resp)
		if resp.HasToolCalls() {
			a.append(assistant)
			a.emit(event.NewMessageCompleted(runID, assistant, true))
			results := a.Executor.Execute(ctx, resp.ToolCalls, tool.ExecutionHooks{
				OnStarted: func(call llm.ToolCall) {
					a.emit(event.NewToolCallStarted(runID, call))
				},
				OnCompleted: func(result tool.ToolCallResult) {
					a.emit(event.NewToolCallCompleted(runID, result))
				},
			})
			for _, result := range results {
				toolMessage := llm.ToolMessage(result.ToolCall.ID, result.Result)
				a.append(toolMessage)
				a.emit(event.NewMessageCompleted(runID, a.durableToolMessage(result.ToolCall, toolMessage), true))
			}
			a.appendImageToolMessages(runID, results)
			continue
		}
		a.append(assistant)
		a.emit(event.NewMessageCompleted(runID, assistant, true))
		if overflow := llm.DetectContextOverflowResponse(resp, a.Client.MaxContextWindow()); overflow.Retry {
			return "", &llm.ContextOverflowError{Cause: errors.New("the model produced no output because its context window was exhausted")}
		}
		return resp.Content, nil
	}
	stopped := "Maximum iteration limit reached."
	a.appendDurable(runID, llm.Assistant(stopped))
	return stopped, nil
}

// SkipNextBeforeChat lets the runtime make one best-effort model call after a
// threshold compaction warning. Later tool-loop calls are checked normally.
func (a *Agent) SkipNextBeforeChat() {
	a.skipBeforeChat = true
}

// DiscardTrailingOverflowResponse removes a persisted empty length response
// from the in-memory retry context. The session entry remains available for
// diagnostics and compaction bookkeeping.
func (a *Agent) DiscardTrailingOverflowResponse() {
	if len(a.History) == 0 {
		return
	}
	last := a.History[len(a.History)-1]
	if last.Role == llm.RoleAssistant && strings.EqualFold(last.FinishReason, "length") &&
		strings.TrimSpace(last.Content) == "" && len(last.ToolCalls) == 0 {
		a.History = a.History[:len(a.History)-1]
	}
}

func (a *Agent) assistantMessage(resp llm.ChatResponse) llm.Message {
	provider := resp.Provider
	if provider == "" {
		provider = a.Client.ProviderName()
	}
	model := resp.Model
	if model == "" {
		model = a.Client.ModelName()
	}
	return llm.Message{
		Role:              llm.RoleAssistant,
		Content:           resp.Content,
		ReasoningContent:  resp.ReasoningContent,
		ToolCalls:         resp.ToolCalls,
		InputTokens:       resp.InputTokens,
		OutputTokens:      resp.OutputTokens,
		CachedInputTokens: resp.CachedInputTokens,
		Provider:          provider,
		Model:             model,
		FinishReason:      resp.FinishReason,
	}
}

func (a *Agent) appendImageToolMessages(runID string, results []tool.ToolCallResult) {
	for _, result := range results {
		if len(result.ImageParts) == 0 {
			continue
		}
		parts := make([]llm.ContentPart, 0, len(result.ImageParts)+1)
		parts = append(parts, llm.TextPart(
			"Tool "+result.ToolCall.Function.Name+" returned image content. Analyze it together with the tool's text result above.",
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
		return llm.ToolMessage(msg.ToolCallID, "[Skill content was valid only for the original task and has been removed from history]")
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
			a.History[i] = llm.ToolMessage(msg.ToolCallID, "[Skill content was valid only for the original task and has been removed from history]")
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
func (*FakeClient) MaxOutputTokens() int        { return 0 }
func (*FakeClient) SupportsTools() bool         { return true }
func (*FakeClient) SupportsPromptCaching() bool { return false }
func (*FakeClient) SupportsImages() bool        { return true }
