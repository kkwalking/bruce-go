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
		messageEntry("u-old", llm.User(strings.Repeat("x", 200))),
		messageEntry("a-old", llm.Assistant("Old response")),
		messageEntry("u-current", llm.User("Read the file")),
		messageEntry("a-call", toolCall),
		messageEntry("tool-result", llm.ToolMessage("call-1", "Content")),
		messageEntry("a-final", llm.Assistant("Done")),
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
		messageEntry("u-old", llm.User(strings.Repeat("x", 40))),
		messageEntry("a-old", llm.Assistant(strings.Repeat("y", 40))),
		messageEntry("u-current", llm.User("Current request")),
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
		messageEntry("u1", llm.User(strings.Repeat("x", 40))),
		messageEntry("a1", llm.Assistant(strings.Repeat("y", 40))),
		messageEntry("u2", llm.User("Q?")),
		messageEntry("a2", llm.Assistant("New")),
	}
	preparation, ok := PrepareCompaction(entries, config.Compaction{KeepRecentTokens: 2})
	if !ok || preparation.FirstKeptEntryID != "u2" || preparation.IsSplitTurn {
		t.Fatalf("normal turn boundary = %+v, ok=%v", preparation, ok)
	}
	if len(preparation.MessagesToSummarize) != 2 {
		t.Fatalf("summarized messages = %d", len(preparation.MessagesToSummarize))
	}

	custom := Entry{Type: TypeCustomMessage, ID: "custom", Content: "Custom"}
	customEntries := append(entries[:2:2], custom, messageEntry("a3", llm.Assistant("A")))
	customPreparation, ok := PrepareCompaction(customEntries, config.Compaction{KeepRecentTokens: 3})
	if !ok || customPreparation.FirstKeptEntryID != "custom" {
		t.Fatalf("custom boundary = %+v, ok=%v", customPreparation, ok)
	}

	if _, ok := PrepareCompaction(entries[2:], config.Compaction{KeepRecentTokens: 100}); ok {
		t.Fatal("small session should not compact")
	}
	continuous := append(append([]Entry(nil), entries...), Entry{Type: TypeCompaction, ID: "compact", Summary: "Summary", FirstKeptEntryID: "u2"})
	if _, ok := PrepareCompaction(continuous, config.Compaction{KeepRecentTokens: 1}); ok {
		t.Fatal("consecutive compaction should be rejected")
	}
}

func TestPrepareCompactionRepeatedUsageAndFiles(t *testing.T) {
	readCall := llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "read", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"b.go"}`}}, {ID: "edit", Function: llm.FunctionCall{Name: "edit_file", Arguments: `{"path":"a.go"}`}}}}
	used := llm.Assistant("Processed")
	used.InputTokens, used.OutputTokens, used.FinishReason = 100, 20, "stop"
	entries := []Entry{
		messageEntry("u-old", llm.User("Oldest")),
		messageEntry("a-old", llm.Assistant("Old answer")),
		messageEntry("u-kept", llm.User("Previously retained but now evicted")),
		messageEntry("a-kept", readCall),
		{Type: TypeCompaction, ID: "compact-1", Summary: "Previous summary", FirstKeptEntryID: "u-kept", Details: map[string]any{"readFiles": []string{"old.go"}, "modifiedFiles": []string{"z.go"}}},
		messageEntry("u-new", llm.User("New question")),
		messageEntry("a-new", used),
		messageEntry("u-trailing", llm.User(strings.Repeat("z", 40))),
	}
	preparation, ok := PrepareCompaction(entries, config.Compaction{KeepRecentTokens: 12})
	if !ok {
		t.Fatal("expected repeated compaction preparation")
	}
	if preparation.PreviousSummary != "Previous summary" {
		t.Fatalf("previous summary = %q", preparation.PreviousSummary)
	}
	serialized := SerializeConversation(preparation.MessagesToSummarize)
	if !strings.Contains(serialized, "Previously retained but now evicted") {
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
	image := llm.UserParts([]llm.ContentPart{llm.TextPart("Text"), llm.ImagePart("data:image/png;base64,x", "image/png", "x.png")})
	if tokens := EstimateTokens(image); tokens < 1200 {
		t.Fatalf("image tokens = %d", tokens)
	}
	assistant := llm.Message{Role: llm.RoleAssistant, Content: "Body", ReasoningContent: "Reasoning", ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: "edit_file", Arguments: `{"path":"main.go"}`}}}}
	toolText := strings.Repeat("x", 2005)
	serialized := SerializeConversation([]llm.Message{assistant, llm.ToolMessage("call", toolText)})
	for _, want := range []string{"[Assistant reasoning]: Reasoning", "[Assistant]: Body", `edit_file({"path":"main.go"})`, "5 characters omitted"} {
		if !strings.Contains(serialized, want) {
			t.Fatalf("serialized conversation missing %q: %s", want, serialized)
		}
	}
}

func TestShouldCompactUsesContextWindowRatio(t *testing.T) {
	settings := config.Compaction{Enabled: true, ContextWindowRatio: 0.8, ReserveTokens: 10}
	if ShouldCompact(70, 100, settings) {
		t.Fatal("context at threshold should not compact")
	}
	if !ShouldCompact(71, 100, settings) {
		t.Fatal("context above threshold should compact")
	}
	settings.ReserveTokens = 80
	if ShouldCompact(100, 100, settings) {
		t.Fatal("invalid threshold should not compact")
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

func TestCompactUsesEnglishStructuredPromptLimitsAndFileTags(t *testing.T) {
	client := &summaryClient{maxOutput: 60, responses: []llm.ChatResponse{{Content: "## Objectives\nComplete compaction\n\n## Next Steps\n1. Continue"}}}
	preparation := CompactionPreparation{
		FirstKeptEntryID:    "keep",
		MessagesToSummarize: []llm.Message{{Role: llm.RoleAssistant, ReasoningContent: "Reasoning", ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}}, llm.ToolMessage("call", strings.Repeat("x", 2005))},
		PreviousSummary:     "Previous summary",
		TokensBefore:        500,
		Details:             Details{ReadFiles: []string{"a.go"}, ModifiedFiles: []string{"b.go"}},
		Settings:            config.Compaction{ReserveTokens: 100},
	}
	result, err := Compact(context.Background(), client, preparation, "Preserve API details")
	if err != nil {
		t.Fatal(err)
	}
	prompt := client.prompts[0]
	for _, want := range []string{"<previous-summary>", "Previous summary", "Additional focus requirements: Preserve API details", "## Objectives", "## Constraints and Preferences", "## Progress", "## Key Decisions", "## Next Steps", "## Critical Context", "read_file", "5 characters omitted"} {
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
	client := &summaryClient{maxOutput: 1000, responses: []llm.ChatResponse{{Content: "History summary"}, {Content: "Turn-prefix summary"}}}
	preparation := CompactionPreparation{FirstKeptEntryID: "keep", MessagesToSummarize: []llm.Message{llm.User("History")}, TurnPrefixMessages: []llm.Message{llm.User("Oversized turn")}, IsSplitTurn: true, Settings: config.Compaction{ReserveTokens: 100}}
	result, err := Compact(context.Background(), client, preparation, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Summary, "Current Oversized-Turn Context") {
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
