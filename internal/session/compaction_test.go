package session

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"bruce-go/internal/config"
	"bruce-go/internal/llm"
)

func messageEntry(id string, message llm.Message) Entry {
	return Entry{Type: TypeMessage, ID: id, Message: &message}
}

func TestPrepareCompactionKeepsToolProtocolIntact(t *testing.T) {
	toolCall := llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt","padding":"` + strings.Repeat("x", 400) + `"}`}}}}
	entries := []Entry{
		messageEntry("u-old", llm.User(strings.Repeat("旧", 200))),
		messageEntry("a-old", llm.Assistant("旧回复")),
		messageEntry("u-current", llm.User("读取文件")),
		messageEntry("a-call", toolCall),
		messageEntry("tool-result", llm.ToolMessage("call-1", "内容")),
		messageEntry("a-final", llm.Assistant("完成")),
	}
	preparation, ok := PrepareCompaction(entries, config.Compaction{ReserveTokens: 100, KeepRecentTokens: 50})
	if !ok {
		t.Fatal("expected compaction preparation")
	}
	if preparation.FirstKeptEntryID != "a-call" || !preparation.IsSplitTurn {
		t.Fatalf("unexpected boundary: %+v", preparation)
	}
	kept := entries[indexOfEntry(entries, preparation.FirstKeptEntryID):]
	if kept[0].Message == nil || kept[0].Message.Role == llm.RoleTool {
		t.Fatalf("context starts with orphan tool result: %+v", kept[0])
	}
	if len(kept) < 2 || kept[1].Message == nil || kept[1].Message.ToolCallID != "call-1" {
		t.Fatalf("tool call/result pair was broken: %+v", kept)
	}
}

func TestLengthOverflowEntryIsPersistedButExcludedFromRetryContext(t *testing.T) {
	overflow := llm.Message{Role: llm.RoleAssistant, InputTokens: 99, FinishReason: "length"}
	entries := []Entry{
		messageEntry("u-old", llm.User(strings.Repeat("旧", 40))),
		messageEntry("a-old", llm.Assistant(strings.Repeat("答", 40))),
		messageEntry("u-current", llm.User("当前请求")),
		messageEntry("a-overflow", overflow),
	}
	preparation, ok := PrepareCompaction(entries, config.Compaction{KeepRecentTokens: 1})
	if !ok || preparation.FirstKeptEntryID != "u-current" {
		t.Fatalf("preparation = %+v, ok=%v", preparation, ok)
	}
	messages := buildContextMessages(entries)
	for _, message := range messages {
		if message.FinishReason == "length" {
			t.Fatalf("overflow response leaked into retry context: %+v", messages)
		}
	}
	if entries[len(entries)-1].Message == nil || entries[len(entries)-1].Message.InputTokens != 99 {
		t.Fatal("raw session entry should retain overflow metadata")
	}
}

func TestPrepareCompactionNormalCustomSmallAndContinuous(t *testing.T) {
	entries := []Entry{
		messageEntry("u1", llm.User(strings.Repeat("旧", 40))),
		messageEntry("a1", llm.Assistant(strings.Repeat("答", 40))),
		messageEntry("u2", llm.User("新问")),
		messageEntry("a2", llm.Assistant("新答复")),
	}
	preparation, ok := PrepareCompaction(entries, config.Compaction{KeepRecentTokens: 2})
	if !ok || preparation.FirstKeptEntryID != "u2" || preparation.IsSplitTurn {
		t.Fatalf("normal turn boundary = %+v, ok=%v", preparation, ok)
	}
	if len(preparation.MessagesToSummarize) != 2 {
		t.Fatalf("summarized messages = %d", len(preparation.MessagesToSummarize))
	}

	custom := Entry{Type: TypeCustomMessage, ID: "custom", Content: "自定义上下文"}
	customEntries := append(entries[:2:2], custom, messageEntry("a3", llm.Assistant("答")))
	customPreparation, ok := PrepareCompaction(customEntries, config.Compaction{KeepRecentTokens: 3})
	if !ok || customPreparation.FirstKeptEntryID != "custom" {
		t.Fatalf("custom boundary = %+v, ok=%v", customPreparation, ok)
	}

	if _, ok := PrepareCompaction(entries[2:], config.Compaction{KeepRecentTokens: 100}); ok {
		t.Fatal("small session should not compact")
	}
	continuous := append(append([]Entry(nil), entries...), Entry{Type: TypeCompaction, ID: "compact", Summary: "摘要", FirstKeptEntryID: "u2"})
	if _, ok := PrepareCompaction(continuous, config.Compaction{KeepRecentTokens: 1}); ok {
		t.Fatal("consecutive compaction should be rejected")
	}
}

func TestPrepareCompactionRepeatedUsageAndFiles(t *testing.T) {
	readCall := llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "read", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"b.go"}`}}, {ID: "edit", Function: llm.FunctionCall{Name: "edit_file", Arguments: `{"path":"a.go"}`}}}}
	used := llm.Assistant("已处理")
	used.InputTokens, used.OutputTokens, used.FinishReason = 100, 20, "stop"
	entries := []Entry{
		messageEntry("u-old", llm.User("最旧")),
		messageEntry("a-old", llm.Assistant("旧答")),
		messageEntry("u-kept", llm.User("上次保留但现在淘汰")),
		messageEntry("a-kept", readCall),
		{Type: TypeCompaction, ID: "compact-1", Summary: "旧摘要", FirstKeptEntryID: "u-kept", Details: map[string]any{"readFiles": []string{"old.go"}, "modifiedFiles": []string{"z.go"}}},
		messageEntry("u-new", llm.User("新问题")),
		messageEntry("a-new", used),
		messageEntry("u-trailing", llm.User(strings.Repeat("尾", 40))),
	}
	preparation, ok := PrepareCompaction(entries, config.Compaction{KeepRecentTokens: 12})
	if !ok {
		t.Fatal("expected repeated compaction preparation")
	}
	if preparation.PreviousSummary != "旧摘要" {
		t.Fatalf("previous summary = %q", preparation.PreviousSummary)
	}
	serialized := SerializeConversation(preparation.MessagesToSummarize)
	if !strings.Contains(serialized, "上次保留但现在淘汰") {
		t.Fatalf("previously kept messages were not re-summarized: %s", serialized)
	}
	if preparation.TokensBefore != 130 {
		t.Fatalf("tokensBefore = %d, want usage 120 + trailing 10", preparation.TokensBefore)
	}
	if strings.Join(preparation.Details.ReadFiles, ",") != "b.go,old.go" || strings.Join(preparation.Details.ModifiedFiles, ",") != "a.go,z.go" {
		t.Fatalf("file details = %+v", preparation.Details)
	}
}

func TestEstimateAndSerializeCompactionContent(t *testing.T) {
	image := llm.UserParts([]llm.ContentPart{llm.TextPart("文字"), llm.ImagePart("data:image/png;base64,x", "image/png", "x.png")})
	if tokens := EstimateTokens(image); tokens < 1200 {
		t.Fatalf("image tokens = %d", tokens)
	}
	assistant := llm.Message{Role: llm.RoleAssistant, Content: "正文", ReasoningContent: "推理", ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: "edit_file", Arguments: `{"path":"main.go"}`}}}}
	toolText := strings.Repeat("界", 2005)
	serialized := SerializeConversation([]llm.Message{assistant, llm.ToolMessage("call", toolText)})
	for _, want := range []string{"[Assistant reasoning]: 推理", "[Assistant]: 正文", `edit_file({"path":"main.go"})`, "省略 5 个字符"} {
		if !strings.Contains(serialized, want) {
			t.Fatalf("serialized conversation missing %q: %s", want, serialized)
		}
	}
}

type summaryClient struct {
	mu        sync.Mutex
	responses []llm.ChatResponse
	prompts   []string
	limits    []int
	maxOutput int
}

func (c *summaryClient) Chat(_ context.Context, messages []llm.Message, _ []llm.ToolDefinition, opts llm.StreamOptions) (llm.ChatResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prompts = append(c.prompts, messages[len(messages)-1].Content)
	c.limits = append(c.limits, opts.MaxTokens)
	if len(c.responses) == 0 {
		return llm.ChatResponse{}, errors.New("no response")
	}
	response := c.responses[0]
	c.responses = c.responses[1:]
	return response, nil
}

func (*summaryClient) ProviderName() string        { return "summary" }
func (*summaryClient) ModelName() string           { return "summary-model" }
func (*summaryClient) MaxContextWindow() int       { return 1000 }
func (c *summaryClient) MaxOutputTokens() int      { return c.maxOutput }
func (*summaryClient) SupportsTools() bool         { return false }
func (*summaryClient) SupportsPromptCaching() bool { return false }
func (*summaryClient) SupportsImages() bool        { return false }

func TestCompactUsesChineseStructuredPromptLimitsAndFileTags(t *testing.T) {
	client := &summaryClient{maxOutput: 60, responses: []llm.ChatResponse{{Content: "## 目标\n完成压缩\n\n## 下一步\n1. 继续"}}}
	preparation := CompactionPreparation{
		FirstKeptEntryID:    "keep",
		MessagesToSummarize: []llm.Message{{Role: llm.RoleAssistant, ReasoningContent: "推理", ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}}, llm.ToolMessage("call", strings.Repeat("文", 2005))},
		PreviousSummary:     "旧摘要",
		TokensBefore:        500,
		Details:             Details{ReadFiles: []string{"a.go"}, ModifiedFiles: []string{"b.go"}},
		Settings:            config.Compaction{ReserveTokens: 100},
	}
	result, err := Compact(context.Background(), client, preparation, "重点保留 API")
	if err != nil {
		t.Fatal(err)
	}
	prompt := client.prompts[0]
	for _, want := range []string{"<previous-summary>", "旧摘要", "附加聚焦要求：重点保留 API", "## 目标", "## 约束与偏好", "## 进度", "## 关键决策", "## 下一步", "## 关键上下文", "read_file", "省略 5 个字符"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("summary prompt missing %q", want)
		}
	}
	if client.limits[0] != 60 {
		t.Fatalf("max tokens = %d", client.limits[0])
	}
	if !strings.Contains(result.Summary, "<read-files>\na.go") || !strings.Contains(result.Summary, "<modified-files>\nb.go") {
		t.Fatalf("summary file tags = %s", result.Summary)
	}
}

func TestCompactSplitTurnAndEmptyResponse(t *testing.T) {
	client := &summaryClient{maxOutput: 1000, responses: []llm.ChatResponse{{Content: "历史摘要"}, {Content: "回合前缀摘要"}}}
	preparation := CompactionPreparation{FirstKeptEntryID: "keep", MessagesToSummarize: []llm.Message{llm.User("历史")}, TurnPrefixMessages: []llm.Message{llm.User("超长回合")}, IsSplitTurn: true, Settings: config.Compaction{ReserveTokens: 100}}
	result, err := Compact(context.Background(), client, preparation, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Summary, "当前超长回合上下文") {
		t.Fatalf("split summary = %s", result.Summary)
	}
	sort.Ints(client.limits)
	if len(client.limits) != 2 || client.limits[0] != 50 || client.limits[1] != 80 {
		t.Fatalf("split limits = %v", client.limits)
	}

	empty := &summaryClient{responses: []llm.ChatResponse{{Content: "   "}}}
	preparation.IsSplitTurn = false
	preparation.TurnPrefixMessages = nil
	if _, err := Compact(context.Background(), empty, preparation, ""); err == nil {
		t.Fatal("empty summary should fail")
	}
}
