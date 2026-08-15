package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

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
	"bruce-go/internal/version"
)

func TestWelcomeLinesUseCurrentVersion(t *testing.T) {
	welcome := strings.Join(welcomeLines(nil), "\n")
	want := " Bruce Coding Agent " + version.Current + " "
	if !strings.Contains(welcome, want) {
		t.Fatalf("welcome = %q, want title to contain %q", welcome, want)
	}
}

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
	if !strings.Contains(view, "Available Bruce Go commands") || !strings.Contains(view, "/status") {
		t.Fatalf("view = %s", view)
	}
}

func TestPlanAgentCommandDoesNotAppendResultOutput(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	command := cli.Command{Name: "plan", Args: []string{"Implement a new capability"}}
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
	result := cli.Result{Handled: true, Output: "Switched to Plan mode"}
	suppress := commandOutputAlreadyRendered(command, result)
	if suppress {
		t.Fatal("plain /plan should still render its command output")
	}
	model.handleCommandFinished(commandFinishedMsg{command: true, result: result, suppressOutput: suppress})
	assertMessageContains(t, model.messages, "Switched to Plan mode")
}

func TestPlanEventRecordedRendersPresentedPlanContent(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	runID := "run-plan"
	model.handleRuntimeEvent(event.NewMessageCompleted(runID, llm.Assistant("The plan is ready for review."), false))
	model.handleRuntimeEvent(event.NewPlanEventRecorded(runID, bruntime.PlanEvent{
		ID:       "plan_1",
		Action:   bruntime.PlanActionPresented,
		Revision: 2,
		Content:  "# Plan\n\n| File | Description |\n|------|-------------|\n| a.go | Implement |",
	}))

	if len(model.messages) != 2 {
		t.Fatalf("messages = %+v", model.messages)
	}
	if model.messages[0].kind != messageAssistant || !strings.Contains(model.messages[0].text, "plan is ready") {
		t.Fatalf("assistant message should remain ordinary output: %+v", model.messages)
	}
	if model.messages[1].kind != messagePlan || !strings.Contains(model.messages[1].text, "# Plan") || !strings.Contains(model.messages[1].text, "| a.go | Implement |") {
		t.Fatalf("plan message missing full content: %+v", model.messages)
	}
}

func TestSessionReplayRendersPresentedPlanEntriesInOrder(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	model.replaySessionEntries([]session.Entry{
		{Type: session.TypeMessage, Message: messagePtr(llm.User("Create a plan"))},
		{Type: session.TypePlanEvent, Plan: &bruntime.PlanEvent{ID: "plan_1", Action: bruntime.PlanActionCreated, Revision: 1, Content: "# Draft"}},
		{Type: session.TypePlanEvent, Plan: &bruntime.PlanEvent{ID: "plan_1", Action: bruntime.PlanActionPresented, Revision: 1, Content: "# Final Plan\n\n- Step"}},
		{Type: session.TypeMessage, Message: messagePtr(llm.Assistant("The plan is ready for review."))},
	}, nil)

	if len(model.messages) != 3 {
		t.Fatalf("messages = %+v", model.messages)
	}
	if model.messages[0].kind != messageUser || !strings.Contains(model.messages[0].text, "Create a plan") {
		t.Fatalf("user message order broken: %+v", model.messages)
	}
	if model.messages[1].kind != messagePlan || !strings.Contains(model.messages[1].text, "# Final Plan") || strings.Contains(model.messages[1].text, "# Draft") {
		t.Fatalf("plan replay message = %+v", model.messages[1])
	}
	if model.messages[2].kind != messageAssistant || !strings.Contains(model.messages[2].text, "plan is ready") {
		t.Fatalf("assistant message order broken: %+v", model.messages)
	}
}

func TestLayoutKeepsInputAndStatusDockedAtBottom(t *testing.T) {
	layout := layoutFor(24, 1)
	if layout.messageRows != 19 || layout.indexStatusRow != 19 || layout.inputTop != 20 || layout.inputLine != 21 || layout.inputBottom != 22 || layout.inputRows != 1 || layout.statusRow != 23 {
		t.Fatalf("layout = %+v", layout)
	}
}

func TestStatusDetailsShowsCompactSandboxWithoutMCP(t *testing.T) {
	details := statusDetails(bruntime.Status{
		Mode:            bruntime.ModeReact,
		Model:           "test-model",
		ReasoningEffort: "high",
		WorkspaceRoot:   "/home/test/code/bruce-cli",
		SandboxBackend:  "seatbelt",
		SandboxMode:     "full-access",
		SandboxNetwork:  true,
	}, "/home/test", 12)
	for _, expected := range []string{
		"test-model (high)",
		"~/code/bruce-cli",
		"sandbox full-access",
		"12ms",
	} {
		if !strings.Contains(details, expected) {
			t.Fatalf("status details missing %q: %s", expected, details)
		}
	}
	for _, unexpected := range []string{"seatbelt", "net", "mcp"} {
		if strings.Contains(details, unexpected) {
			t.Fatalf("status details unexpectedly contains %q: %s", unexpected, details)
		}
	}
}

func TestStatusDetailsOmitsEmptyReasoningEffort(t *testing.T) {
	details := statusDetails(bruntime.Status{Model: "test-model"}, "", 0)
	if !strings.Contains(details, "bruce · test-model · mode") {
		t.Fatalf("status details changed model formatting without reasoning effort: %s", details)
	}
}

func TestStatusDetailsShowsEachSandboxMode(t *testing.T) {
	for _, mode := range []string{"read-only", "workspace-write", "full-access"} {
		details := statusDetails(bruntime.Status{SandboxMode: mode}, "", 0)
		if !strings.Contains(details, "sandbox "+mode) {
			t.Fatalf("status details missing sandbox mode %q: %s", mode, details)
		}
		if strings.Contains(details, "net") {
			t.Fatalf("status details unexpectedly contains network state for %q: %s", mode, details)
		}
	}
}

func TestCompactPathRelativeToHome(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		homeDir string
		want    string
	}{
		{name: "empty", homeDir: "/home/test", want: ""},
		{name: "home", path: "/home/test", homeDir: "/home/test", want: "~"},
		{name: "nested", path: "/home/test/code/bruce-cli", homeDir: "/home/test", want: "~/code/bruce-cli"},
		{name: "outside home", path: "/opt/bruce-cli", homeDir: "/home/test", want: "/opt/bruce-cli"},
		{name: "similar prefix", path: "/home/test-other/bruce-cli", homeDir: "/home/test", want: "/home/test-other/bruce-cli"},
		{name: "missing home", path: "/home/test/code/bruce-cli", want: "/home/test/code/bruce-cli"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compactPath(tt.path, tt.homeDir); got != tt.want {
				t.Fatalf("compactPath(%q, %q) = %q, want %q", tt.path, tt.homeDir, got, tt.want)
			}
		})
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
	model.replaceInput(strings.Repeat("🧩content", 3))

	expanded := model.layoutFor(20, 24)
	if expanded.inputRows <= 1 {
		t.Fatalf("expected wrapped input rows, layout = %+v", expanded)
	}
	if expanded.messageRows >= 19 || expanded.inputBottom != 22 || expanded.statusRow != 23 {
		t.Fatalf("expanded layout = %+v", expanded)
	}

	model.replaceInput("x")
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
	model.replaceInput("🧩🧩🧩🧩")

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
	model.appendStreamingAssistantDelta("Hel")
	model.appendStreamingAssistantDelta("lo")
	model.finishStreamingAssistantMessage("Hello")
	if len(model.messages) != 1 || model.messages[0].text != "Hello" {
		t.Fatalf("messages = %+v", model.messages)
	}
}

func TestBlankStreamingAssistantIsRemovedAroundToolActivity(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	model.beginStreamingAssistantMessage()
	model.appendActivity("Tool call: write_file (in progress)")
	model.finishStreamingAssistantMessage("")
	if len(model.messages) != 1 || !strings.Contains(model.messages[0].text, "write_file") {
		t.Fatalf("messages = %+v", model.messages)
	}
}

func TestStreamingReasoningDeltasProduceSingleFinalMessage(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	model.messages = nil
	model.appendStreamingReasoningDelta("Think")
	model.appendStreamingReasoningDelta(" this")
	model.appendStreamingReasoningDelta(" through")
	model.finishStreamingReasoningMessage("Think this through")
	if len(model.messages) != 1 || model.messages[0].kind != messageReasoning || model.messages[0].text != "Think this through" {
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
	model.handleRuntimeEvent(event.NewMessageDelta(runID, llm.RoleAssistant, "reasoning", "Reasoning step 1"))
	model.handleRuntimeEvent(event.NewMessageDelta(runID, llm.RoleAssistant, "reasoning", "Reasoning step 2"))
	model.handleRuntimeEvent(event.NewMessageCompleted(runID, llm.Message{
		Role:             llm.RoleAssistant,
		ReasoningContent: "Reasoning step 1Reasoning step 2",
		Content:          "Final answer",
	}, false))
	hasReasoning := false
	hasAnswer := false
	for _, msg := range model.messages {
		if msg.kind == messageReasoning && strings.Contains(msg.text, "Reasoning step") {
			hasReasoning = true
		}
		if msg.kind == messageAssistant && strings.Contains(msg.text, "Final answer") {
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

func TestApprovalCancellationClearsMatchingDialogOnly(t *testing.T) {
	model := NewModel(context.Background(), testRuntime(t))
	firstReply := make(chan approval.Result, 1)
	model.Update(approvalRequestMsg{ID: 1, Request: approval.NewRequest("write_file", `{"path":"a.txt"}`, ""), Reply: firstReply})
	model.Update(approvalCanceledMsg{ID: 2})
	if model.approval == nil || model.approval.id != 1 {
		t.Fatalf("unrelated cancellation cleared approval: %+v", model.approval)
	}
	model.Update(approvalCanceledMsg{ID: 1})
	if model.approval != nil {
		t.Fatalf("matching cancellation did not clear approval: %+v", model.approval)
	}
}

func TestToolCompletionStatusLabels(t *testing.T) {
	tests := map[tool.ToolCallStatus]string{
		tool.ToolCallSuccess:     "completed",
		tool.ToolCallFailed:      "failed",
		tool.ToolCallTimeout:     "timed out",
		tool.ToolCallInterrupted: "canceled",
		tool.ToolCallRejected:    "rejected",
		tool.ToolCallSkipped:     "skipped",
	}
	for status, want := range tests {
		if got := toolCompletionStatus(status); got != want {
			t.Fatalf("status %q = %q, want %q", status, got, want)
		}
	}
}

type approvalCaptureModel struct {
	messages chan tea.Msg
}

type approvalRequestResult struct {
	result approval.Result
	err    error
}

func (*approvalCaptureModel) Init() tea.Cmd { return nil }
func (m *approvalCaptureModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case approvalRequestMsg, approvalCanceledMsg:
		m.messages <- msg
	}
	return m, nil
}
func (*approvalCaptureModel) View() string { return "" }

func startApprovalCaptureProgram(t *testing.T) (*approvalCaptureModel, *tea.Program) {
	t.Helper()
	model := &approvalCaptureModel{messages: make(chan tea.Msg, 4)}
	program := tea.NewProgram(model, tea.WithoutRenderer(), tea.WithInput(nil), tea.WithOutput(io.Discard))
	runDone := make(chan error, 1)
	go func() {
		_, err := program.Run()
		runDone <- err
	}()
	t.Cleanup(func() {
		program.Quit()
		select {
		case <-runDone:
		case <-time.After(time.Second):
		}
	})
	return model, program
}

func TestTUIApprovalHandlerSerializesRequestsAndApproveAll(t *testing.T) {
	model, program := startApprovalCaptureProgram(t)
	handler := newTUIApprovalHandler()
	handler.SetProgram(program)
	handler.SetEnabled(true)
	results := make(chan approvalRequestResult, 2)
	for _, name := range []string{"write_file", "execute_command"} {
		name := name
		go func() {
			result, err := handler.Request(context.Background(), approval.NewRequest(name, `{}`, ""))
			results <- approvalRequestResult{result: result, err: err}
		}()
	}
	var request approvalRequestMsg
	select {
	case msg := <-model.messages:
		request = msg.(approvalRequestMsg)
	case <-time.After(time.Second):
		t.Fatal("approval request was not displayed")
	}
	select {
	case second := <-model.messages:
		t.Fatalf("second approval bypassed serialization: %#v", second)
	case <-time.After(30 * time.Millisecond):
	}
	request.Reply <- approval.ApproveAll()
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil || (result.result.Decision != approval.ApprovedAll && result.result.Decision != approval.Approved) {
				t.Fatalf("approval result = %+v", result)
			}
		case <-time.After(time.Second):
			t.Fatal("approval request did not finish")
		}
	}
	select {
	case second := <-model.messages:
		t.Fatalf("approve-all still displayed another request: %#v", second)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestTUIApprovalHandlerCancellationReleasesGate(t *testing.T) {
	model, program := startApprovalCaptureProgram(t)
	handler := newTUIApprovalHandler()
	handler.SetProgram(program)
	handler.SetEnabled(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := handler.Request(ctx, approval.NewRequest("write_file", `{}`, ""))
		done <- err
	}()
	select {
	case msg := <-model.messages:
		if _, ok := msg.(approvalRequestMsg); !ok {
			t.Fatalf("message = %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("approval request was not displayed")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("request error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled approval did not return")
	}
	select {
	case msg := <-model.messages:
		if _, ok := msg.(approvalCanceledMsg); !ok {
			t.Fatalf("message = %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("approval cancellation was not delivered")
	}
	secondDone := make(chan approvalRequestResult, 1)
	go func() {
		result, err := handler.Request(context.Background(), approval.NewRequest("write_file", `{}`, ""))
		secondDone <- approvalRequestResult{result: result, err: err}
	}()
	var secondRequest approvalRequestMsg
	select {
	case msg := <-model.messages:
		secondRequest = msg.(approvalRequestMsg)
	case <-time.After(time.Second):
		t.Fatal("gate was not released for the next approval")
	}
	secondRequest.Reply <- approval.Approve()
	second := <-secondDone
	if second.err != nil || second.result.Decision != approval.Approved {
		t.Fatalf("result = %+v", second)
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
			if !strings.Contains(message.text, "write_file[a.txt]") || !strings.Contains(message.text, "completed") {
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
			want: "Tool call: read_file[pom.xml] (completed)",
		},
		{
			name: "write path",
			call: llm.ToolCall{Function: llm.FunctionCall{Name: "write_file", Arguments: `{"path":"internal/tui/tui.go","content":"..."}`}},
			want: "Tool call: write_file[internal/tui/tui.go] (completed)",
		},
		{
			name: "edit path",
			call: llm.ToolCall{Function: llm.FunctionCall{Name: "edit_file", Arguments: `{"path":"README.md","old_text":"a","new_text":"b"}`}},
			want: "Tool call: edit_file[README.md] (completed)",
		},
		{
			name: "execute command",
			call: llm.ToolCall{Function: llm.FunctionCall{Name: "execute_command", Arguments: `{"command":"find src -name \"*.java\" | sort | head -100"}`}},
			want: `Tool call: execute_command[find src -name "*.java" | sort | head -100] (completed)`,
		},
		{
			name: "mcp unchanged",
			call: llm.ToolCall{Function: llm.FunctionCall{Name: "mcp__filesystem__read_file", Arguments: `{"path":"pom.xml"}`}},
			want: "Tool call: mcp__filesystem__read_file (completed)",
		},
		{
			name: "skill unchanged",
			call: llm.ToolCall{Function: llm.FunctionCall{Name: "load_skill", Arguments: `{"name":"review"}`}},
			want: "Tool call: load_skill (completed)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolActivityText(tt.call, "completed"); got != tt.want {
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
	if !strings.Contains(model.agentStatusText, "Thinking") {
		t.Fatalf("agentStatusText = %q, want Thinking", model.agentStatusText)
	}

	model.handleRuntimeEvent(event.NewMessageDelta(runID, llm.RoleAssistant, "reasoning", "Think first"))
	if !strings.Contains(model.agentStatusText, "Reasoning") {
		t.Fatalf("agentStatusText = %q, want Reasoning", model.agentStatusText)
	}

	model.handleRuntimeEvent(event.NewMessageDelta(runID, llm.RoleAssistant, "content", "Answer"))
	if !strings.Contains(model.agentStatusText, "Generating response") {
		t.Fatalf("agentStatusText = %q, want Generating response", model.agentStatusText)
	}
	assertMessageContains(t, model.messages, "Answer")

	model.handleRuntimeEvent(event.NewRunCompleted(runID, "Answer"))
	if model.agentStatusText != "" {
		t.Fatalf("agentStatusText = %q, want empty after completed", model.agentStatusText)
	}

	// also check reasoning text is recorded as a message
	model2 := NewModel(context.Background(), testRuntime(t))
	model2.messages = nil
	reasoningRunID := "run-2"
	model2.handleRuntimeEvent(event.NewMessageDelta(reasoningRunID, llm.RoleAssistant, "reasoning", "Thinking"))
	model2.handleRuntimeEvent(event.NewMessageCompleted(reasoningRunID, llm.Message{
		Role:             llm.RoleAssistant,
		ReasoningContent: "Thinking",
		Content:          "Answer",
	}, false))
	hasReasoning := false
	for _, msg := range model2.messages {
		if msg.kind == messageReasoning && strings.Contains(msg.text, "Thinking") {
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
	for _, want := range []string{"/help", "/status", "/session", "/model "} {
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

func TestEveryRegisteredSlashCommandCompletesWithTab(t *testing.T) {
	rt := testRuntime(t)
	for _, command := range cli.Commands {
		input := "/" + command.Name
		cursor := len([]rune(input))
		expected := command.CompletionValue()
		item := completionItemByValue(t, completionsFor(input, cursor, rt), expected)
		got, gotCursor := applyCompletion(input, cursor, item)
		if got != expected {
			t.Fatalf("Tab completion for %q = %q, want %q", input, got, expected)
		}
		if gotCursor != len([]rune(expected)) {
			t.Fatalf("cursor after completing %q = %d, want %d", input, gotCursor, len([]rune(expected)))
		}
	}
}

func TestSlashCompletionDoesNotSuggestUnregisteredCommands(t *testing.T) {
	rt := testRuntime(t)
	for _, input := range []string{"/concurrency", "/concurrency "} {
		if items := completionsFor(input, len([]rune(input)), rt); len(items) != 0 {
			t.Fatalf("completions for %q = %v, want none", input, completionValues(items))
		}
	}
}

func TestSlashCompletionMatchesParseWithLeadingWhitespace(t *testing.T) {
	rt := testRuntime(t)
	input := "  /sand"
	cursor := len([]rune(input))

	item := completionItemByValue(t, completionsFor(input, cursor, rt), "/sandbox ")
	got, gotCursor := applyCompletion(input, cursor, item)
	if got != "  /sandbox " {
		t.Fatalf("Tab completion with leading whitespace = %q", got)
	}
	if gotCursor != len([]rune("  /sandbox ")) {
		t.Fatalf("cursor after leading-whitespace completion = %d", gotCursor)
	}
	firstLevel := completionValues(completionsFor(got, gotCursor, rt))
	for _, want := range []string{"status", "mode ", "network "} {
		if !contains(firstLevel, want) {
			t.Fatalf("sandbox completion missing %q: %v", want, firstLevel)
		}
	}
}

func TestEverySlashOptionLevelAppliesWithTab(t *testing.T) {
	rt := testRuntime(t)
	for _, command := range cli.Commands {
		if len(command.Options) == 0 {
			continue
		}
		input := command.CompletionValue()
		cursor := len([]rune(input))
		items := completionsFor(input, cursor, rt)
		for _, option := range command.Options {
			item := completionItemByValue(t, items, option.Value)
			got, gotCursor := applyCompletion(input, cursor, item)
			want := input + option.Value
			if got != want {
				t.Fatalf("Tab completion for %s %q = %q, want %q", command.Name, option.Value, got, want)
			}
			if gotCursor != len([]rune(want)) {
				t.Fatalf("cursor after completing %s %q = %d, want %d", command.Name, option.Value, gotCursor, len([]rune(want)))
			}
		}
	}
}

func TestModelCommandUsesTheSameTabFlow(t *testing.T) {
	rt := testSwitchableRuntime(t)

	input := "/model"
	cursor := len([]rune(input))
	item := completionItemByValue(t, completionsFor(input, cursor, rt), "/model ")
	input, cursor = applyCompletion(input, cursor, item)
	if input != "/model " {
		t.Fatalf("first Tab for /model = %q, want /model ", input)
	}

	models := completionsFor(input, cursor, rt)
	if len(models) < 2 || models[0].Value != "acme/alpha" || models[1].Value != "acme/beta" {
		t.Fatalf("model completion order = %v, want current model first", completionValues(models))
	}
	modelItem := completionItemByValue(t, models, "acme/alpha")
	input, cursor = applyCompletion(input, cursor, modelItem)
	if input != "/model acme/alpha" {
		t.Fatalf("second Tab for model selection = %q", input)
	}
	if cursor != len([]rune(input)) {
		t.Fatalf("cursor after model completion = %d, want %d", cursor, len([]rune(input)))
	}

	// The reasoning subcommand follows the same hierarchical Tab flow as
	// other argument-taking options.
	reasoningInput := "/model rea"
	cursor = len([]rune(reasoningInput))
	reasoning := completionItemByValue(t, completionsFor(reasoningInput, cursor, rt), "reasoning ")
	reasoningInput, cursor = applyCompletion(reasoningInput, cursor, reasoning)
	if reasoningInput != "/model reasoning " {
		t.Fatalf("reasoning token completion = %q", reasoningInput)
	}
	levels := completionValues(completionsFor(reasoningInput, cursor, rt))
	for _, want := range []string{"off", "low", "medium", "high", "max"} {
		if !contains(levels, want) {
			t.Fatalf("reasoning level completion missing %q: %v", want, levels)
		}
	}
	level := completionItemByValue(t, completionsFor(reasoningInput, cursor, rt), "high")
	reasoningInput, cursor = applyCompletion(reasoningInput, cursor, level)
	if reasoningInput != "/model reasoning high" {
		t.Fatalf("reasoning level completion = %q", reasoningInput)
	}

	// The Enter-driven model selector popup must remain available.
	model := NewModel(context.Background(), rt)
	model.replaceInput("/model")
	model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !model.modelSelectorOpen || model.inputText() != "/model " {
		t.Fatalf("Enter should open the model selector, got input=%q open=%v", model.inputText(), model.modelSelectorOpen)
	}
}

func TestTabSelectedModelKeepsReasoningEffortArrows(t *testing.T) {
	rt := testSwitchableRuntime(t)
	model := NewModel(context.Background(), rt)
	model.replaceInput("/model ")

	items := model.completions()
	selected := -1
	for i, item := range items {
		if item.Value == "acme/alpha" {
			selected = i
			break
		}
	}
	if selected < 0 {
		t.Fatalf("model completion missing acme/alpha: %v", completionValues(items))
	}
	model.selectedCompletion = selected
	model.applyCompletion()

	if model.inputText() != "/model acme/alpha" {
		t.Fatalf("Tab-selected model input = %q", model.inputText())
	}
	if !model.modelSelectorOpen {
		t.Fatal("Tab-selecting a model should keep the reasoning-effort popup available")
	}
	if !isModelSelectorPopup(model) {
		t.Fatal("Tab-selecting a model should render the reasoning-effort popup")
	}
	if model.pendingReasoningEffort != "medium" {
		t.Fatalf("pending reasoning effort = %q, want current effort medium", model.pendingReasoningEffort)
	}

	cursorBefore := model.cursor
	model.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
	if model.pendingReasoningEffort != "low" {
		t.Fatalf("left arrow reasoning effort = %q, want low", model.pendingReasoningEffort)
	}
	if model.cursor != cursorBefore {
		t.Fatalf("left arrow moved cursor while model selector is active")
	}

	model.handleKey(tea.KeyMsg{Type: tea.KeyRight})
	if model.pendingReasoningEffort != "medium" {
		t.Fatalf("right arrow reasoning effort = %q, want medium", model.pendingReasoningEffort)
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
	prefixed := completionValues(completionsFor("/plan a", len("/plan a"), rt))
	if len(prefixed) != 1 || prefixed[0] != "approve" {
		t.Fatalf("plan prefix completions = %v", prefixed)
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
	if leading := highlightInput("  /status"); len(leading) == 0 || leading[0].Style != styleCommand {
		t.Fatalf("leading-whitespace slash command should be styled as command: %+v", leading)
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

type fakeSwitchableClient struct {
	*agent.FakeClient
	options []llm.ModelOption
	current llm.ModelOption
	effort  string
}

func (f *fakeSwitchableClient) Options() []llm.ModelOption {
	return llm.OrderedModelOptions(f.options, f.current)
}

func (f *fakeSwitchableClient) Current() llm.ModelOption { return f.current }

func (f *fakeSwitchableClient) Switch(selector string) (llm.ModelOption, error) {
	selector = strings.TrimSpace(selector)
	for _, option := range f.options {
		if strings.EqualFold(option.Selector(), selector) || strings.EqualFold(option.Model, selector) {
			return option, nil
		}
	}
	return llm.ModelOption{}, errors.New("unknown model: " + selector)
}

func (f *fakeSwitchableClient) ReasoningEffort() string { return f.effort }

func (f *fakeSwitchableClient) SetReasoningEffort(level string) error {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "off", "low", "medium", "high", "max":
		f.effort = strings.ToLower(strings.TrimSpace(level))
		return nil
	default:
		return errors.New("invalid reasoning effort: " + level)
	}
}

func testSwitchableRuntime(t *testing.T) *integrated.Runtime {
	t.Helper()
	options := []llm.ModelOption{
		{Provider: "acme", Model: "beta"},
		{Provider: "acme", Model: "alpha"},
	}
	client := &fakeSwitchableClient{
		FakeClient: &agent.FakeClient{},
		options:    options,
		current:    options[1],
		effort:     "medium",
	}
	rt, err := integrated.New(context.Background(), integrated.Options{Workspace: t.TempDir(), HomeDir: t.TempDir(), Client: client})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func toolResult(call llm.ToolCall, status tool.ToolCallStatus) tool.ToolCallResult {
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

	// /model rea → hierarchical guidance to first complete "reasoning ".
	values2 := completionValues(completeModel("/model rea", rt))
	if !contains(values2, "reasoning ") {
		t.Fatalf("reasoning prefix completion missing: %v", values2)
	}

	// The same guidance is available even when the user already typed a
	// trailing space after a partial reasoning token.
	values2Trailing := completionValues(completeModel("/model rea ", rt))
	if !contains(values2Trailing, "reasoning ") {
		t.Fatalf("reasoning prefix completion missing with trailing space: %v", values2Trailing)
	}

	// /model (empty first-argument prefix) still lists models only.
	allModelValues := completionValues(completeModel("/model ", rt))
	if contains(allModelValues, "reasoning") || contains(allModelValues, "reasoning ") {
		t.Fatalf("unexpected reasoning item in model list: %v", allModelValues)
	}

	// /model reasoning without trailing space completes the reasoning token
	// instead of offering levels that would overwrite it.
	exactReasoning := completionValues(completeModel("/model reasoning", rt))
	if len(exactReasoning) != 1 || exactReasoning[0] != "reasoning " {
		t.Fatalf("exact reasoning completion = %v, want [reasoning ]", exactReasoning)
	}

	// /model reasoning hi → filtered to "high"
	values3 := completionValues(completeModel("/model reasoning hi", rt))
	if !contains(values3, "high") || contains(values3, "low") {
		t.Fatalf("expected only high, got: %v", values3)
	}
}
