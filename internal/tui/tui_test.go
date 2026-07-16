package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"bruce-go/internal/agent"
	"bruce-go/internal/approval"
	"bruce-go/internal/cli"
	"bruce-go/internal/event"
	"bruce-go/internal/integrated"
	"bruce-go/internal/llm"
	bruntime "bruce-go/internal/runtime"
	"bruce-go/internal/session"
	"bruce-go/internal/tool"
)

func TestModelRunsSlashCommandOnEnter(t *testing.T) {
	rt, err := integrated.New(context.Background(), integrated.Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: &agent.FakeClient{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	model := NewModel(context.Background(), rt)
	model.height = 80
	model.replaceInput("/help")
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next := updated.(*Model)
	if cmd == nil {
		t.Fatal("expected command")
	}
	updated, _ = next.Update(cmd())
	next = updated.(*Model)
	view := next.View()
	if !strings.Contains(view, "Bruce Go 可用命令") || !strings.Contains(view, "/status") {
		t.Fatalf("view = %s", view)
	}
}

func TestPlanAgentCommandDoesNotAppendResultOutput(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	command := cli.Command{Name: "plan", Args: []string{"实现新能力"}}
	result := cli.Result{Handled: true, Output: "# Full Plan\n\n- Step"}
	suppress := commandOutputAlreadyRendered(command, result)
	if !suppress {
		t.Fatal("expected plan task output to be marked as already rendered")
	}
	model.handleCommandFinished(commandFinishedMsg{command: true, result: result, suppressOutput: suppress})
	if len(model.messages) != 0 {
		t.Fatalf("messages = %+v", model.messages)
	}
}

func TestPlainPlanCommandStillAppendsResultOutput(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	command := cli.Command{Name: "plan"}
	result := cli.Result{Handled: true, Output: "已切换到 Plan 模式"}
	suppress := commandOutputAlreadyRendered(command, result)
	if suppress {
		t.Fatal("plain /plan should still render its command output")
	}
	model.handleCommandFinished(commandFinishedMsg{command: true, result: result, suppressOutput: suppress})
	assertMessageContains(t, model.messages, "已切换到 Plan 模式")
}

func TestPlanEventRecordedRendersPresentedPlanContent(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	runID := "run-plan"
	model.handleRuntimeEvent(event.NewMessageCompleted(runID, llm.Assistant("计划创建完成，请审阅。"), false))
	model.handleRuntimeEvent(event.NewPlanEventRecorded(runID, bruntime.PlanEvent{
		ID:       "plan_1",
		Action:   bruntime.PlanActionPresented,
		Revision: 2,
		Content:  "# Plan\n\n| 文件 | 说明 |\n|------|------|\n| a.go | 实现 |",
	}))

	if len(model.messages) != 2 {
		t.Fatalf("messages = %+v", model.messages)
	}
	if model.messages[0].kind != messageAssistant || !strings.Contains(model.messages[0].text, "计划创建完成") {
		t.Fatalf("assistant message should remain ordinary output: %+v", model.messages)
	}
	if model.messages[1].kind != messagePlan || !strings.Contains(model.messages[1].text, "# Plan") || !strings.Contains(model.messages[1].text, "| a.go | 实现 |") {
		t.Fatalf("plan message missing full content: %+v", model.messages)
	}
}

func TestSessionReplayRendersPresentedPlanEntriesInOrder(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	model.replaySessionEntries([]session.Entry{
		{Type: session.TypeMessage, Message: messagePtr(llm.User("请创建计划"))},
		{Type: session.TypePlanEvent, Plan: &bruntime.PlanEvent{ID: "plan_1", Action: bruntime.PlanActionCreated, Revision: 1, Content: "# Draft"}},
		{Type: session.TypePlanEvent, Plan: &bruntime.PlanEvent{ID: "plan_1", Action: bruntime.PlanActionPresented, Revision: 1, Content: "# Final Plan\n\n- Step"}},
		{Type: session.TypeMessage, Message: messagePtr(llm.Assistant("计划创建完成，请审阅。"))},
	}, nil)

	if len(model.messages) != 3 {
		t.Fatalf("messages = %+v", model.messages)
	}
	if model.messages[0].kind != messageUser || !strings.Contains(model.messages[0].text, "请创建计划") {
		t.Fatalf("user message order broken: %+v", model.messages)
	}
	if model.messages[1].kind != messagePlan || !strings.Contains(model.messages[1].text, "# Final Plan") || strings.Contains(model.messages[1].text, "# Draft") {
		t.Fatalf("plan replay message = %+v", model.messages[1])
	}
	if model.messages[2].kind != messageAssistant || !strings.Contains(model.messages[2].text, "计划创建完成") {
		t.Fatalf("assistant message order broken: %+v", model.messages)
	}
}

func TestLayoutKeepsInputAndStatusDockedAtBottom(t *testing.T) {
	layout := layoutFor(24, 1)
	if layout.messageRows != 19 || layout.indexStatusRow != 19 || layout.inputTop != 20 || layout.inputLine != 21 || layout.inputBottom != 22 || layout.inputRows != 1 || layout.statusRow != 23 {
		t.Fatalf("layout = %+v", layout)
	}
}

func TestLayoutExpandsInputRowsUpward(t *testing.T) {
	layout := layoutFor(24, 3)
	if layout.messageRows != 17 || layout.indexStatusRow != 17 || layout.inputTop != 18 || layout.inputLine != 19 || layout.inputBottom != 22 || layout.inputRows != 3 || layout.statusRow != 23 {
		t.Fatalf("layout = %+v", layout)
	}
}

func TestLongInputWrapsAndShrinkRestoresSingleRow(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.replaceInput(strings.Repeat("当前的内容", 3))

	expanded := model.layoutFor(20, 24)
	if expanded.inputRows <= 1 {
		t.Fatalf("expected wrapped input rows, layout = %+v", expanded)
	}
	if expanded.messageRows >= 19 || expanded.inputBottom != 22 || expanded.statusRow != 23 {
		t.Fatalf("expanded layout = %+v", expanded)
	}

	model.replaceInput("短")
	shrunk := model.layoutFor(20, 24)
	if shrunk.inputRows != 1 || shrunk.messageRows != 19 || shrunk.inputTop != 20 {
		t.Fatalf("shrunk layout = %+v", shrunk)
	}
}

func TestWrappedInputCursorMovesToWrappedLine(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.replaceInput("abcdefghi")
	model.cursor = 8

	lines := model.wrappedInputLines(8)
	if len(lines) != 2 {
		t.Fatalf("lines = %+v", lines)
	}
	if got := cursorLineIndex(lines); got != 1 {
		t.Fatalf("cursor line = %d, want 1; lines = %+v", got, lines)
	}

	visible := visibleInputLines(lines, 1)
	if got := cursorLineIndex(visible); got != 0 {
		t.Fatalf("visible cursor line = %d, want 0; lines = %+v", got, visible)
	}
}

func TestWrappedInputWideRunesStayWithinWidth(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.replaceInput("当前内容当前内容")

	lines := model.wrappedInputLines(5)
	if len(lines) < 2 {
		t.Fatalf("expected wrapped wide input, lines = %+v", lines)
	}
	for _, line := range lines {
		if line.width > 5 {
			t.Fatalf("line exceeds width: %+v", line)
		}
	}
}

func TestInputFrameLineUsesSolidRule(t *testing.T) {
	line := inputFrameLine(40)
	if len([]rune(line)) != 40 || strings.Trim(line, "━") != "" {
		t.Fatalf("line = %q", line)
	}
}

func TestCursorStyleUsesGrayBackground(t *testing.T) {
	if got := cursorCellStyle.GetBackground(); got != lipgloss.Color("#C0C0C0") {
		t.Fatalf("cursor background = %v, want gray", got)
	}
	if got := cursorCellStyle.GetForeground(); got != lipgloss.Color("0") {
		t.Fatalf("cursor foreground = %v, want black", got)
	}
}

func TestPlanStyleUsesReadableLightBackground(t *testing.T) {
	if got := planStyle.GetForeground(); got != lipgloss.Color("0") {
		t.Fatalf("plan foreground = %v, want black", got)
	}
	if got := planStyle.GetBackground(); got != lipgloss.Color("#E6F4EA") {
		t.Fatalf("plan background = %v, want light green", got)
	}
	rendered := renderMessageLine(renderLine{kind: messagePlan, text: "# Plan"}, 20)
	if lipgloss.Width(rendered) != 20 {
		t.Fatalf("rendered plan width = %d, want 20", lipgloss.Width(rendered))
	}
}

func TestTUIApprovalHandlerDefaultsDisabled(t *testing.T) {
	if newTUIApprovalHandler().Enabled() {
		t.Fatal("TUI HITL should be disabled by default")
	}
}

func TestStreamingAssistantDeltasProduceSingleFinalMessage(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	model.beginStreamingAssistantMessage()
	model.appendStreamingAssistantDelta("你")
	model.appendStreamingAssistantDelta("好")
	model.finishStreamingAssistantMessage("你好")
	if len(model.messages) != 1 || model.messages[0].text != "你好" {
		t.Fatalf("messages = %+v", model.messages)
	}
}

func TestBlankStreamingAssistantIsRemovedAroundToolActivity(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	model.beginStreamingAssistantMessage()
	model.appendActivity("工具调用: write_file (处理中)")
	model.finishStreamingAssistantMessage("")
	if len(model.messages) != 1 || !strings.Contains(model.messages[0].text, "write_file") {
		t.Fatalf("messages = %+v", model.messages)
	}
}

func TestStreamingReasoningDeltasProduceSingleFinalMessage(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	model.appendStreamingReasoningDelta("先")
	model.appendStreamingReasoningDelta("想")
	model.appendStreamingReasoningDelta("一")
	model.appendStreamingReasoningDelta("下")
	model.finishStreamingReasoningMessage("先想一下")
	if len(model.messages) != 1 || model.messages[0].kind != messageReasoning || model.messages[0].text != "先想一下" {
		t.Fatalf("messages = %+v", model.messages)
	}
}

func TestBlankStreamingReasoningIsRemoved(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	model.beginStreamingReasoningMessage()
	model.finishStreamingReasoningMessage("")
	if len(model.messages) != 0 {
		t.Fatalf("messages = %+v", model.messages)
	}
}

func TestReasoningDeltaViaRuntimeEventCreatesReasoningMessage(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	runID := "run-r1"
	model.handleRuntimeEvent(event.NewMessageDelta(runID, llm.RoleAssistant, "reasoning", "思考步骤1"))
	model.handleRuntimeEvent(event.NewMessageDelta(runID, llm.RoleAssistant, "reasoning", "思考步骤2"))
	model.handleRuntimeEvent(event.NewMessageCompleted(runID, llm.Message{
		Role:             llm.RoleAssistant,
		ReasoningContent: "思考步骤1思考步骤2",
		Content:          "最终答案",
	}, false))
	hasReasoning := false
	hasAnswer := false
	for _, msg := range model.messages {
		if msg.kind == messageReasoning && strings.Contains(msg.text, "思考步骤") {
			hasReasoning = true
		}
		if msg.kind == messageAssistant && strings.Contains(msg.text, "最终答案") {
			hasAnswer = true
		}
	}
	if !hasReasoning {
		t.Fatal("reasoning message not found in messages")
	}
	if !hasAnswer {
		t.Fatal("assistant message not found in messages")
	}
}

func TestMessageWindowUsesScrollOffsetAndClamps(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = []tuiMessage{{kind: messageAssistant, text: strings.Join([]string{
		"line-1", "line-2", "line-3", "line-4", "line-5", "line-6", "line-7", "line-8",
	}, "\n")}}
	lines := model.visibleMessageLines(80, 3, 0)
	if got := texts(lines); strings.Join(got, ",") != "line-6,line-7,line-8" {
		t.Fatalf("bottom lines = %v", got)
	}
	lines = model.visibleMessageLines(80, 3, 99)
	if got := texts(lines); strings.Join(got, ",") != "line-1,line-2,line-3" {
		t.Fatalf("top lines = %v", got)
	}
}

func TestApprovalDialogCompletesFromKeyInput(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	reply := make(chan approval.Result, 1)
	model.Update(approvalRequestMsg{Request: approval.NewRequest("write_file", `{"path":"a.txt"}`, ""), Reply: reply})
	if model.approval == nil {
		t.Fatal("approval dialog not opened")
	}
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	result := <-reply
	if result.Decision != approval.ApprovedAll {
		t.Fatalf("result = %+v", result)
	}
}

func TestRuntimeEventsUpdateSingleToolActivityLine(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	call := llm.ToolCall{ID: "call-1", Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"a.txt"}`}}
	model.handleRuntimeEvent(event.NewToolCallStarted("run-1", call))
	model.handleRuntimeEvent(event.NewToolCallCompleted("run-1", toolResult(call, "success")))
	count := 0
	for _, message := range model.messages {
		if strings.Contains(message.text, "write_file") {
			count++
			if !strings.Contains(message.text, "write_file[a.txt]") || !strings.Contains(message.text, "完成") {
				t.Fatalf("message = %s", message.text)
			}
		}
	}
	if count != 1 {
		t.Fatalf("messages = %+v", model.messages)
	}
}

func TestToolActivityTextDisplaysBuiltinArguments(t *testing.T) {
	tests := []struct {
		name string
		call llm.ToolCall
		want string
	}{
		{
			name: "read path",
			call: llm.ToolCall{Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"pom.xml"}`}},
			want: "工具调用: read_file[pom.xml] (完成)",
		},
		{
			name: "write path",
			call: llm.ToolCall{Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"internal/tui/tui.go","content":"..."}`}},
			want: "工具调用: write_file[internal/tui/tui.go] (完成)",
		},
		{
			name: "edit path",
			call: llm.ToolCall{Function: llm.FunctionCall{Name: "edit_file", Arguments: `{"path":"README.md","old_text":"a","new_text":"b"}`}},
			want: "工具调用: edit_file[README.md] (完成)",
		},
		{
			name: "execute command",
			call: llm.ToolCall{Function: llm.FunctionCall{Name: "execute_command", Arguments: `{"command":"find src -name \"*.java\" | sort | head -100"}`}},
			want: `工具调用: execute_command[find src -name "*.java" | sort | head -100] (完成)`,
		},
		{
			name: "mcp unchanged",
			call: llm.ToolCall{Function: llm.FunctionCall{Name: "mcp__filesystem__read_file", Arguments: `{"path":"pom.xml"}`}},
			want: "工具调用: mcp__filesystem__read_file (完成)",
		},
		{
			name: "skill unchanged",
			call: llm.ToolCall{Function: llm.FunctionCall{Name: "load_skill", Arguments: `{"name":"review"}`}},
			want: "工具调用: load_skill (完成)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolActivityText(tt.call, "完成"); got != tt.want {
				t.Fatalf("toolActivityText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunActivityTracksReasoningContentAndCompletion(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	runID := "run-1"

	model.handleRuntimeEvent(event.NewRunStarted(runID, model.runtime.Mode, "hello"))
	if !strings.Contains(model.agentStatusText, "思考中") {
		t.Fatalf("agentStatusText = %q, want 思考中", model.agentStatusText)
	}

	model.handleRuntimeEvent(event.NewMessageDelta(runID, llm.RoleAssistant, "reasoning", "先想"))
	if !strings.Contains(model.agentStatusText, "推理中") {
		t.Fatalf("agentStatusText = %q, want 推理中", model.agentStatusText)
	}

	model.handleRuntimeEvent(event.NewMessageDelta(runID, llm.RoleAssistant, "content", "答案"))
	if !strings.Contains(model.agentStatusText, "生成回答") {
		t.Fatalf("agentStatusText = %q, want 生成回答", model.agentStatusText)
	}
	assertMessageContains(t, model.messages, "答案")

	model.handleRuntimeEvent(event.NewRunCompleted(runID, "答案"))
	if model.agentStatusText != "" {
		t.Fatalf("agentStatusText = %q, want empty after completed", model.agentStatusText)
	}

	// also check reasoning text is recorded as a message
	model2 := NewModel(context.Background(), testRuntime(t))
	model2.messages = nil
	reasoningRunID := "run-2"
	model2.handleRuntimeEvent(event.NewMessageDelta(reasoningRunID, llm.RoleAssistant, "reasoning", "思考中"))
	model2.handleRuntimeEvent(event.NewMessageCompleted(reasoningRunID, llm.Message{
		Role:             llm.RoleAssistant,
		ReasoningContent: "思考中",
		Content:          "答案",
	}, false))
	hasReasoning := false
	for _, msg := range model2.messages {
		if msg.kind == messageReasoning && strings.Contains(msg.text, "思考中") {
			hasReasoning = true
		}
	}
	if !hasReasoning {
		t.Fatalf("reasoning message not found: %+v", model2.messages)
	}
}

func TestCompletesTopLevelSlashCommandsWithoutRAG(t *testing.T) {
	rt := testRuntime(t)
	values := completionValues(completionsFor("/", 1, rt))
	for _, want := range []string{"/help", "/status", "/session", "/model"} {
		if !contains(values, want) {
			t.Fatalf("completion missing %q: %v", want, values)
		}
	}
	for _, forbidden := range []string{"/rag ", "/index ", "/graph "} {
		if contains(values, forbidden) {
			t.Fatalf("completion contains forbidden %q: %v", forbidden, values)
		}
	}
}

func TestCompletesPlanSubcommands(t *testing.T) {
	rt := testRuntime(t)
	values := completionValues(completionsFor("/plan ", len("/plan "), rt))
	for _, want := range []string{"approve", "continue ", "reject ", "cancel"} {
		if !contains(values, want) {
			t.Fatalf("plan completion missing %q: %v", want, values)
		}
	}
}

func TestCompletesNestedSandboxCommandsWithTab(t *testing.T) {
	rt := testRuntime(t)
	input := "/sandbox"
	cursor := len([]rune(input))

	item := completionItemByValue(t, completionsFor(input, cursor, rt), "/sandbox ")
	input, cursor = applyCompletion(input, cursor, item)
	if input != "/sandbox " {
		t.Fatalf("top-level completion = %q", input)
	}

	firstLevel := completionValues(completionsFor(input, cursor, rt))
	for _, want := range []string{"status", "mode ", "network "} {
		if !contains(firstLevel, want) {
			t.Fatalf("sandbox completion missing %q: %v", want, firstLevel)
		}
	}
	item = completionItemByValue(t, completionsFor(input, cursor, rt), "mode ")
	input, cursor = applyCompletion(input, cursor, item)
	if input != "/sandbox mode " {
		t.Fatalf("mode completion = %q", input)
	}

	modes := completionValues(completionsFor(input, cursor, rt))
	for _, want := range []string{"read-only", "workspace-write", "full-access"} {
		if !contains(modes, want) {
			t.Fatalf("sandbox mode completion missing %q: %v", want, modes)
		}
	}

	network := completionValues(completionsFor("/sandbox network ", len("/sandbox network "), rt))
	if !contains(network, "on") || !contains(network, "off") {
		t.Fatalf("sandbox network completions = %v", network)
	}
	prefixed := completionValues(completionsFor("/sandbox mode fu", len("/sandbox mode fu"), rt))
	if len(prefixed) != 1 || prefixed[0] != "full-access" {
		t.Fatalf("sandbox mode prefix completions = %v", prefixed)
	}
}

func TestHighlightsCoreInputPatterns(t *testing.T) {
	spans := highlightInput("/web search @image:<shot.png> @clipboard rm -rf / api_key")
	styles := map[inputStyle]bool{}
	for _, span := range spans {
		styles[span.Style] = true
	}
	for _, want := range []inputStyle{styleCommand, styleImage, styleDanger, styleSecret} {
		if !styles[want] {
			t.Fatalf("style %v missing in spans %+v", want, spans)
		}
	}
}

func texts(lines []renderLine) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, line.text)
	}
	return out
}

func cursorLineIndex(lines []inputRenderLine) int {
	for i, line := range lines {
		if line.cursor {
			return i
		}
	}
	return -1
}

func assertMessageContains(t *testing.T, messages []tuiMessage, want string) {
	t.Helper()
	for _, message := range messages {
		if strings.Contains(message.text, want) {
			return
		}
	}
	t.Fatalf("message containing %q not found in %+v", want, messages)
}

func completionValues(items []CompletionItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Value)
	}
	return out
}

func completionItemByValue(t *testing.T, items []CompletionItem, value string) CompletionItem {
	t.Helper()
	for _, item := range items {
		if item.Value == value {
			return item
		}
	}
	t.Fatalf("completion %q not found in %v", value, completionValues(items))
	return CompletionItem{}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func testRuntime(t *testing.T) *integrated.Runtime {
	t.Helper()
	rt, err := integrated.New(context.Background(), integrated.Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: &agent.FakeClient{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func toolResult(call llm.ToolCall, status string) tool.ToolCallResult {
	return tool.ToolCallResult{ToolCall: call, Status: status}
}

func messagePtr(message llm.Message) *llm.Message {
	return &message
}

func TestCompletesModelReasoningSubcommand(t *testing.T) {
	rt := testRuntime(t)

	// /model reasoning → 5 level items
	values := completionValues(completeModel("/model reasoning ", rt))
	if len(values) != 5 {
		t.Fatalf("reasoning completions = %d items, want 5: %v", len(values), values)
	}
	expectedLevels := []string{"off", "low", "medium", "high", "max"}
	for _, level := range expectedLevels {
		if !contains(values, level) {
			t.Fatalf("missing level %q in %v", level, values)
		}
	}

	// /model rea → no reasoning item (removed; use ←/→ in popup instead)
	values2 := completionValues(completeModel("/model rea", rt))
	if contains(values2, "reasoning") {
		t.Fatalf("unexpected reasoning item for 'rea' prefix: %v", values2)
	}

	// /model → completions must not include reasoning entry
	allModelValues := completionValues(completeModel("/model ", rt))
	if contains(allModelValues, "reasoning") {
		t.Fatalf("unexpected reasoning item in model list: %v", allModelValues)
	}

	// /model reasoning hi → filtered to "high"
	values3 := completionValues(completeModel("/model reasoning hi", rt))
	if !contains(values3, "high") || contains(values3, "low") {
		t.Fatalf("expected only high, got: %v", values3)
	}
}
