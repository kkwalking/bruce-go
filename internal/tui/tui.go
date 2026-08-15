package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

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

const (
	historySize       = 2000
	historyFileSize   = 10000
	scrollLines       = 2
	maxCompletionRows = 6
	inputPrompt       = "❯ "
)

type messageKind int

const (
	messageUser messageKind = iota
	messageAssistant
	messageSystem
	messageActivity
	messageReasoning
	messagePlan
)

type tuiMessage struct {
	kind messageKind
	text string
}

type renderLine struct {
	kind messageKind
	text string
}

type inputRenderCell struct {
	text   string
	style  inputStyle
	cursor bool
	width  int
}

type inputRenderLine struct {
	cells  []inputRenderCell
	width  int
	cursor bool
}

type commandFinishedMsg struct {
	command        bool
	result         cli.Result
	err            error
	elapsedMillis  int64
	suppressOutput bool
}

type runtimeStartedMsg struct{}

type runtimeEventMsg struct {
	event event.Event
}

type approvalMode int

const (
	approvalChoose approvalMode = iota
	approvalRejectReason
	approvalModifyArgs
)

type approvalDialog struct {
	id      uint64
	request approval.Request
	reply   chan approval.Result
	mode    approvalMode
	text    []rune
}

type Model struct {
	ctx     context.Context
	runtime *integrated.Runtime

	input  []rune
	cursor int

	messages                []tuiMessage
	streamingAssistant      string
	streamingAssistantIndex int
	streamingReasoning      string
	streamingReasoningIndex int
	toolActivityIndexes     map[string]int
	agentStatusText         string

	width                  int
	height                 int
	quit                   bool
	busy                   bool
	statusPhase            string
	elapsedMillis          int64
	cancel                 context.CancelFunc
	lastEscTime            time.Time
	scrollOffset           int
	selectedCompletion     int
	modelSelectorOpen      bool
	pendingReasoningEffort string // popup 内 ←/→ 调整的待定强度，回车确认才落盘
	approval               *approvalDialog

	history      []string
	historyIndex int
	historyFile  string
}

func NewModel(ctx context.Context, rt *integrated.Runtime) *Model {
	homeDir := ""
	if rt != nil {
		homeDir = rt.HomeDir
	}
	m := &Model{
		ctx:                     ctx,
		runtime:                 rt,
		width:                   80,
		height:                  24,
		statusPhase:             "idle",
		busy:                    rt != nil && rt.StartMCP,
		streamingAssistantIndex: -1,
		streamingReasoningIndex: -1,
		toolActivityIndexes:     map[string]int{},
		historyFile:             resolveHistoryFile(homeDir),
	}
	if m.busy {
		m.statusPhase = "starting"
	}
	m.messages = append(m.messages, tuiMessage{kind: messageSystem, text: strings.Join(welcomeLines(rt), "\n")})
	m.loadHistory()
	m.historyIndex = len(m.history)
	return m
}

func (m *Model) Init() tea.Cmd {
	return func() tea.Msg {
		if m.runtime != nil {
			m.runtime.Start(m.ctx)
		}
		return runtimeStartedMsg{}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.scrollOffset = m.clampScrollOffset(m.scrollOffset)
	case runtimeStartedMsg:
		m.busy = false
		m.statusPhase = "idle"
	case runtimeEventMsg:
		m.handleRuntimeEvent(msg.event)
	case approvalRequestMsg:
		m.approval = &approvalDialog{id: msg.ID, request: msg.Request, reply: msg.Reply}
		m.appendActivity("HITL awaiting approval: " + msg.Request.ToolName)
	case approvalCanceledMsg:
		if m.approval != nil && m.approval.id == msg.ID {
			m.appendActivity("HITL approval canceled: " + m.approval.request.ToolName)
			m.approval = nil
		}
	case commandFinishedMsg:
		return m, m.handleCommandFinished(msg)
	case tea.MouseMsg:
		m.handleMouse(msg)
	case tea.KeyMsg:
		if m.approval != nil {
			return m, m.handleApprovalKey(msg)
		}
		return m, m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) View() string {
	if m.quit {
		return ""
	}
	columns := max(20, m.width)
	rows := max(8, m.height)
	canvas := make([]string, rows)
	for i := range canvas {
		canvas[i] = strings.Repeat(" ", columns)
	}
	layout := m.layoutFor(columns, rows)
	m.drawMessages(canvas, columns, layout.messageRows)
	m.drawCompletions(canvas, columns, layout.indexStatusRow)
	m.drawInput(canvas, columns, layout)
	m.drawStatus(canvas, columns, layout.statusRow)
	m.drawApproval(canvas, columns, rows)
	return strings.Join(canvas, "\n")
}

func Run(ctx context.Context, rt *integrated.Runtime) error {
	handler := newTUIApprovalHandler()
	if rt != nil {
		rt.HITL = handler
		rt.Tools.WithHITL(handler)
	}
	model := NewModel(ctx, rt)
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	handler.SetProgram(program)
	var unsubscribe func()
	if rt != nil && rt.Events != nil {
		unsubscribe = rt.Events.Subscribe(func(evt event.Event) {
			program.Send(runtimeEventMsg{event: evt})
		})
	}
	if unsubscribe != nil {
		defer unsubscribe()
	}
	finalModel, err := program.Run()
	if final, ok := finalModel.(*Model); ok {
		final.saveHistory()
	}
	return err
}

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "ctrl+c":
		if len(m.input) == 0 {
			m.quit = true
			return tea.Quit
		}
		m.clearInput()
		m.appendSystemMessage("Input cleared.")
		return nil
	case "ctrl+d":
		if len(m.input) == 0 {
			m.quit = true
			return tea.Quit
		}
		m.deleteAtCursor()
		return nil
	case "enter":
		if handled, cmd := m.handleModelSelectorEnter(); handled {
			return cmd
		}
		return m.submitInput()
	case "backspace":
		m.backspace()
	case "delete":
		m.deleteAtCursor()
	case "left":
		if m.modelSelectorOpen {
			m.adjustReasoningEffort(-1)
			return nil
		}
		m.cursor = max(0, m.cursor-1)
	case "right":
		if m.modelSelectorOpen {
			m.adjustReasoningEffort(1)
			return nil
		}
		m.cursor = min(len(m.input), m.cursor+1)
	case "home":
		m.cursor = 0
	case "end":
		m.cursor = len(m.input)
	case "up":
		completions := m.completions()
		if len(completions) > 0 {
			m.selectedCompletion = max(0, m.selectedCompletion-1)
		} else {
			m.previousHistory()
		}
	case "down":
		completions := m.completions()
		if len(completions) > 0 {
			m.selectedCompletion = min(len(completions)-1, m.selectedCompletion+1)
		} else {
			m.nextHistory()
		}
	case "pgup", "pageup":
		m.scrollBy(scrollLines)
	case "pgdown", "pagedown":
		m.scrollBy(-scrollLines)
	case "tab":
		m.applyCompletion()
	case "esc":
		if m.busy && m.cancel != nil {
			if time.Since(m.lastEscTime) < 500*time.Millisecond {
				m.cancel()
				m.cancel = nil
				m.appendSystemMessage("⏹ Task interrupted by the user.")
				return nil
			}
			m.lastEscTime = time.Now()
			return nil
		}
		m.selectedCompletion = 0
		m.modelSelectorOpen = false
	default:
		if len(msg.Runes) > 0 {
			m.insertRunes(msg.Runes)
		}
	}
	return nil
}

func (m *Model) handleMouse(msg tea.MouseMsg) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.scrollBy(scrollLines)
	case tea.MouseButtonWheelDown:
		m.scrollBy(-scrollLines)
	}
}

func (m *Model) handleCommandFinished(msg commandFinishedMsg) tea.Cmd {
	m.busy = false
	m.cancel = nil
	m.agentStatusText = ""
	m.statusPhase = "idle"
	m.elapsedMillis = msg.elapsedMillis
	if msg.command {
		if msg.result.Output != "" && !msg.suppressOutput {
			m.appendSystemMessage(msg.result.Output)
		} else if msg.result.Err != nil {
			m.appendSystemMessage("Execution failed: " + msg.result.Err.Error())
		}
		if msg.result.Exit {
			m.quit = true
			return tea.Quit
		}
		return nil
	}
	if msg.err != nil {
		m.appendSystemMessage("Execution failed: " + msg.err.Error())
	}
	return nil
}

func (m *Model) handleRuntimeEvent(evt event.Event) {
	switch e := evt.(type) {
	case event.RunStarted:
		m.toolActivityIndexes = map[string]int{}
		m.agentStatusText = "💭 Thinking..."
	case event.MessageStarted:
		if e.Role == llm.RoleAssistant {
			m.beginStreamingAssistantMessage()
		}
	case event.MessageDelta:
		if e.Role == llm.RoleAssistant {
			switch e.Channel {
			case "reasoning":
				m.appendStreamingReasoningDelta(e.Delta)
				m.agentStatusText = "🧠 Reasoning..."
			case "content":
				m.appendStreamingAssistantDelta(e.Delta)
				m.agentStatusText = "📝 Generating response..."
			}
		}
	case event.MessageCompleted:
		if e.Message.Role == llm.RoleAssistant {
			m.finishStreamingReasoningMessage(e.Message.ReasoningContent)
			m.finishStreamingAssistantMessage(e.Message.Content)
		}
	case event.ToolCallStarted:
		m.agentStatusText = "🔧 Calling tool..."
		index := m.appendActivityAndReturnIndex(toolActivityText(e.ToolCall, "in progress"))
		if index >= 0 {
			m.toolActivityIndexes[toolActivityKey(e.RunID, e.ToolCall)] = index
		}
	case event.ToolCallCompleted:
		status := toolCompletionStatus(e.Result.Status)
		text := toolActivityText(e.Result.ToolCall, status)
		key := toolActivityKey(e.RunID, e.Result.ToolCall)
		if index, ok := m.toolActivityIndexes[key]; ok && m.replaceActivity(index, text) {
			delete(m.toolActivityIndexes, key)
		} else {
			m.appendActivity(text)
		}
	case event.Activity:
		m.appendActivity(e.Message)
	case event.PlanEventRecorded:
		m.appendPresentedPlan(e.Plan)
	case event.SessionChanged:
		if e.Reason == "resume" || e.Reason == "compact" {
			m.replaySessionEntries(e.Context.Entries, e.Context.Messages)
			m.agentStatusText = ""
		}
	case event.RunFailed:
		m.agentStatusText = "❌ Failed"
		if e.Message != "" {
			m.appendSystemMessage("Execution failed: " + e.Message)
		}
	case event.RunCompleted:
		m.agentStatusText = ""
	case event.Basic:
		m.handleBasicEvent(e)
	}
	m.scrollOffset = m.clampScrollOffset(m.scrollOffset)
}

func toolCompletionStatus(status tool.ToolCallStatus) string {
	switch status {
	case tool.ToolCallSuccess:
		return "completed"
	case tool.ToolCallTimeout:
		return "timed out"
	case tool.ToolCallInterrupted:
		return "canceled"
	case tool.ToolCallRejected:
		return "rejected"
	case tool.ToolCallSkipped:
		return "skipped"
	default:
		return "failed"
	}
}

func (m *Model) handleBasicEvent(e event.Basic) {
	switch e.Kind {
	case "activity":
		if text, ok := e.Payload.(string); ok {
			m.appendActivity(text)
		}
	}
}

func (m *Model) submitInput() tea.Cmd {
	if m.busy {
		m.appendSystemMessage("The current task is still running.")
		return nil
	}
	submitted := strings.TrimSpace(m.inputText())
	m.clearInput()
	if submitted == "" {
		return nil
	}
	m.addHistory(submitted)
	m.scrollOffset = 0
	m.appendUserMessage(submitted)
	m.busy = true
	m.statusPhase = "running"
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	return runInputCmd(ctx, m.runtime, submitted)
}

func runInputCmd(ctx context.Context, rt *integrated.Runtime, submitted string) tea.Cmd {
	return func() tea.Msg {
		started := time.Now()
		if command, ok := cli.Parse(submitted); ok {
			result := rt.HandleCommand(ctx, command)
			return commandFinishedMsg{command: true, result: result, elapsedMillis: time.Since(started).Milliseconds(), suppressOutput: commandOutputAlreadyRendered(command, result)}
		}
		_, err := rt.RunTask(ctx, submitted)
		return commandFinishedMsg{command: false, err: err, elapsedMillis: time.Since(started).Milliseconds()}
	}
}

func commandOutputAlreadyRendered(command cli.Command, result cli.Result) bool {
	if result.Err != nil || command.Name != "plan" || len(command.Args) == 0 {
		return false
	}
	switch strings.ToLower(command.Args[0]) {
	case "reject", "cancel":
		return false
	default:
		return true
	}
}

func (m *Model) handleApprovalKey(msg tea.KeyMsg) tea.Cmd {
	dialog := m.approval
	if dialog == nil {
		return nil
	}
	switch msg.String() {
	case "esc":
		m.completeApproval(approval.Reject("the user canceled approval"))
	case "backspace":
		if len(dialog.text) > 0 {
			dialog.text = dialog.text[:len(dialog.text)-1]
		}
	case "enter":
		switch dialog.mode {
		case approvalRejectReason:
			reason := strings.TrimSpace(string(dialog.text))
			if reason == "" {
				reason = "the user rejected this operation"
			}
			m.completeApproval(approval.Reject(reason))
		case approvalModifyArgs:
			modified := strings.TrimSpace(string(dialog.text))
			if modified != "" {
				m.completeApproval(approval.Result{Decision: approval.Modified, Arguments: modified})
			}
		default:
			m.completeApproval(approval.Approve())
		}
	default:
		if len(msg.Runes) == 0 {
			return nil
		}
		if dialog.mode == approvalRejectReason || dialog.mode == approvalModifyArgs {
			dialog.text = append(dialog.text, msg.Runes...)
			return nil
		}
		switch strings.ToLower(string(msg.Runes)) {
		case "y":
			m.completeApproval(approval.Approve())
		case "a":
			m.completeApproval(approval.ApproveAll())
		case "n":
			dialog.mode = approvalRejectReason
			dialog.text = nil
		case "s":
			m.completeApproval(approval.Skip())
		case "m":
			dialog.mode = approvalModifyArgs
			dialog.text = nil
		}
	}
	return nil
}

func (m *Model) completeApproval(result approval.Result) {
	if m.approval == nil {
		return
	}
	reply := m.approval.reply
	m.approval = nil
	select {
	case reply <- result:
	default:
	}
}

func (m *Model) inputText() string {
	return string(m.input)
}

func (m *Model) replaceInput(value string) {
	m.input = []rune(value)
	m.cursor = len(m.input)
	m.selectedCompletion = 0
	m.modelSelectorOpen = false
}

func (m *Model) clearInput() {
	m.input = nil
	m.cursor = 0
	m.selectedCompletion = 0
	m.modelSelectorOpen = false
	m.historyIndex = len(m.history)
}

func (m *Model) insertRunes(runes []rune) {
	if len(runes) == 0 {
		return
	}
	m.input = append(m.input[:m.cursor], append(runes, m.input[m.cursor:]...)...)
	m.cursor += len(runes)
	m.selectedCompletion = 0
	m.updateModelSelectorStateAfterEdit()
}

func (m *Model) backspace() {
	if m.cursor <= 0 {
		return
	}
	m.input = append(m.input[:m.cursor-1], m.input[m.cursor:]...)
	m.cursor--
	m.selectedCompletion = 0
	m.updateModelSelectorStateAfterEdit()
}

func (m *Model) deleteAtCursor() {
	if m.cursor >= len(m.input) {
		return
	}
	m.input = append(m.input[:m.cursor], m.input[m.cursor+1:]...)
	m.selectedCompletion = 0
	m.updateModelSelectorStateAfterEdit()
}

func (m *Model) completions() []CompletionItem {
	value := m.inputText()
	items := completionsFor(value, m.cursor, m.runtime)
	if m.selectedCompletion >= len(items) {
		m.selectedCompletion = max(0, len(items)-1)
	}
	return items
}

func (m *Model) applyCompletion() {
	items := m.completions()
	if len(items) == 0 {
		return
	}
	item := items[clamp(m.selectedCompletion, 0, len(items)-1)]
	next, cursor := applyCompletion(m.inputText(), m.cursor, item)
	m.input = []rune(next)
	m.cursor = cursor
	m.selectedCompletion = 0
	if item.Group == "Model" && !m.modelSelectorOpen {
		// Selecting a concrete model through Tab should preserve the original
		// popup behavior: ←/→ now adjusts the pending reasoning effort and
		// Enter confirms it together with the model.
		m.modelSelectorOpen = true
		m.syncPendingReasoningEffort()
	}
}

func (m *Model) handleModelSelectorEnter() (bool, tea.Cmd) {
	value := m.inputText()
	if isExactModelCommand(value) && !m.modelSelectorOpen {
		m.openModelSelector()
		return true, nil
	}
	items := filterCompletionGroup(m.completions(), "Model")
	if (m.modelSelectorOpen || startsWithModelCommand(value)) && len(items) > 0 {
		m.confirmReasoningEffort() // 落盘 pendingReasoningEffort（若有变化）
		item := items[clamp(m.selectedCompletion, 0, len(items)-1)]
		m.replaceInput("/model " + item.Value)
		return true, m.submitInput()
	}
	return false, nil
}
func (m *Model) openModelSelector() {
	m.replaceInput("/model ")
	m.modelSelectorOpen = true
	m.syncPendingReasoningEffort()
	items := filterCompletionGroup(m.completions(), "Model")
	m.selectedCompletion = 0
	for i, item := range items {
		if item.Description == "Current model" {
			m.selectedCompletion = i
			return
		}
	}
}

func (m *Model) syncPendingReasoningEffort() {
	if m.runtime != nil {
		m.pendingReasoningEffort = m.runtime.ReasoningEffort()
	} else {
		m.pendingReasoningEffort = ""
	}
}

var reasoningEffortLevels = []string{"off", "low", "medium", "high", "max"}

func (m *Model) adjustReasoningEffort(delta int) {
	current := m.pendingReasoningEffort
	idx := -1
	for i, lvl := range reasoningEffortLevels {
		if strings.EqualFold(lvl, current) {
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = 4 // 未识别值（含空串）回退到 max 档位（与 NewSwitchable 默认 max 一致）
	}
	n := len(reasoningEffortLevels)
	m.pendingReasoningEffort = reasoningEffortLevels[((idx+delta)%n+n)%n]
}

func (m *Model) confirmReasoningEffort() {
	if m.runtime == nil || m.pendingReasoningEffort == "" {
		return
	}
	if m.pendingReasoningEffort == m.runtime.ReasoningEffort() {
		return // 无变化，跳过写文件
	}
	_ = m.runtime.SetReasoningEffort(m.pendingReasoningEffort) // 失败静默：无 switchable 时忽略
}

func (m *Model) updateModelSelectorStateAfterEdit() {
	value := m.inputText()
	if !isExactModelCommand(value) && !startsWithModelCommand(value) {
		m.modelSelectorOpen = false
	}
}

func filterCompletionGroup(items []CompletionItem, group string) []CompletionItem {
	var out []CompletionItem
	for _, item := range items {
		if item.Group == group {
			out = append(out, item)
		}
	}
	return out
}

func isExactModelCommand(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "/model")
}

func startsWithModelCommand(value string) bool {
	return len(value) >= len("/model ") && strings.EqualFold(value[:len("/model ")], "/model ")
}

func (m *Model) previousHistory() {
	if len(m.history) == 0 {
		return
	}
	m.historyIndex = max(0, m.historyIndex-1)
	m.replaceInput(m.history[m.historyIndex])
}

func (m *Model) nextHistory() {
	if len(m.history) == 0 {
		return
	}
	m.historyIndex = min(len(m.history), m.historyIndex+1)
	if m.historyIndex >= len(m.history) {
		m.replaceInput("")
		return
	}
	m.replaceInput(m.history[m.historyIndex])
}

func (m *Model) addHistory(value string) {
	normalized := strings.Join(strings.Fields(value), " ")
	if normalized == "" {
		return
	}
	if len(m.history) > 0 && m.history[len(m.history)-1] == normalized {
		return
	}
	m.history = append(m.history, normalized)
	if len(m.history) > historySize {
		m.history = append([]string(nil), m.history[len(m.history)-historySize:]...)
	}
	m.historyIndex = len(m.history)
}

func (m *Model) loadHistory() {
	data, err := os.ReadFile(m.historyFile)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	start := max(0, len(lines)-historySize)
	for _, line := range lines[start:] {
		if strings.TrimSpace(line) != "" {
			m.history = append(m.history, strings.TrimSpace(line))
		}
	}
}

func (m *Model) saveHistory() {
	if len(m.history) == 0 || m.historyFile == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.historyFile), 0o755); err != nil {
		return
	}
	lines := m.history
	if len(lines) > historyFileSize {
		lines = lines[len(lines)-historyFileSize:]
	}
	_ = os.WriteFile(m.historyFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func resolveHistoryFile(homeDir string) string {
	if homeDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			homeDir = home
		}
	}
	if homeDir == "" {
		homeDir = "."
	}
	return filepath.Clean(filepath.Join(homeDir, ".bruce", "history"))
}

func (m *Model) appendUserMessage(text string) {
	m.appendMessage(messageUser, "❯ "+text)
}

func (m *Model) appendSystemMessage(text string) {
	m.appendMessage(messageSystem, "• "+text)
}

func (m *Model) appendActivity(text string) {
	m.appendMessage(messageActivity, "* "+text)
}

func (m *Model) appendActivityAndReturnIndex(text string) int {
	return m.appendMessageAndReturnIndex(messageActivity, "* "+text)
}

func (m *Model) appendMessage(kind messageKind, text string) {
	_ = m.appendMessageAndReturnIndex(kind, text)
}

func (m *Model) appendPresentedPlan(plan bruntime.PlanEvent) {
	if plan.Action != bruntime.PlanActionPresented || strings.TrimSpace(plan.Content) == "" {
		return
	}
	m.appendMessage(messagePlan, presentedPlanText(plan))
}

func (m *Model) appendMessageAndReturnIndex(kind messageKind, text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return -1
	}
	m.messages = append(m.messages, tuiMessage{kind: kind, text: text})
	return len(m.messages) - 1
}

func (m *Model) replaceActivity(index int, text string) bool {
	if index < 0 || index >= len(m.messages) || m.messages[index].kind != messageActivity {
		return false
	}
	m.messages[index] = tuiMessage{kind: messageActivity, text: "* " + text}
	return true
}

func (m *Model) beginStreamingAssistantMessage() {
	if m.streamingAssistantIndex >= 0 {
		return
	}
	m.streamingAssistant = ""
	m.messages = append(m.messages, tuiMessage{kind: messageAssistant})
	m.streamingAssistantIndex = len(m.messages) - 1
}

func (m *Model) appendStreamingAssistantDelta(delta string) {
	if delta == "" {
		return
	}
	if m.streamingAssistantIndex < 0 {
		m.beginStreamingAssistantMessage()
	}
	m.streamingAssistant += delta
	m.messages[m.streamingAssistantIndex] = tuiMessage{kind: messageAssistant, text: m.streamingAssistant}
}

func (m *Model) finishStreamingAssistantMessage(finalText string) {
	text := strings.TrimSpace(finalText)
	if m.streamingAssistantIndex < 0 {
		if text != "" {
			m.messages = append(m.messages, tuiMessage{kind: messageAssistant, text: text})
		}
		return
	}
	if text == "" {
		text = strings.TrimSpace(m.streamingAssistant)
	}
	if text == "" {
		m.messages = append(m.messages[:m.streamingAssistantIndex], m.messages[m.streamingAssistantIndex+1:]...)
	} else {
		m.messages[m.streamingAssistantIndex] = tuiMessage{kind: messageAssistant, text: text}
	}
	m.streamingAssistantIndex = -1
	m.streamingAssistant = ""
}

func (m *Model) beginStreamingReasoningMessage() {
	if m.streamingReasoningIndex >= 0 {
		return
	}
	m.streamingReasoning = ""
	if m.streamingAssistantIndex >= 0 {
		pos := m.streamingAssistantIndex
		m.messages = append(m.messages, tuiMessage{})
		copy(m.messages[pos+1:], m.messages[pos:])
		m.messages[pos] = tuiMessage{kind: messageReasoning}
		m.streamingReasoningIndex = pos
		m.streamingAssistantIndex++
	} else {
		m.messages = append(m.messages, tuiMessage{kind: messageReasoning})
		m.streamingReasoningIndex = len(m.messages) - 1
	}
}

func (m *Model) appendStreamingReasoningDelta(delta string) {
	if delta == "" {
		return
	}
	if m.streamingReasoningIndex < 0 {
		m.beginStreamingReasoningMessage()
	}
	m.streamingReasoning += delta
	m.messages[m.streamingReasoningIndex] = tuiMessage{kind: messageReasoning, text: m.streamingReasoning}
}

func (m *Model) finishStreamingReasoningMessage(text string) {
	text = strings.TrimSpace(text)
	if m.streamingReasoningIndex < 0 {
		if text != "" {
			m.messages = append(m.messages, tuiMessage{kind: messageReasoning, text: text})
		}
		return
	}
	if text == "" {
		text = strings.TrimSpace(m.streamingReasoning)
	}
	if text == "" {
		m.messages = append(m.messages[:m.streamingReasoningIndex], m.messages[m.streamingReasoningIndex+1:]...)
		if m.streamingAssistantIndex > m.streamingReasoningIndex {
			m.streamingAssistantIndex--
		}
	} else {
		m.messages[m.streamingReasoningIndex] = tuiMessage{kind: messageReasoning, text: text}
	}
	m.streamingReasoningIndex = -1
	m.streamingReasoning = ""
}

func (m *Model) replaySessionHistory(messages []llm.Message) {
	m.messages = nil
	m.streamingAssistantIndex = -1
	m.streamingAssistant = ""
	m.streamingReasoningIndex = -1
	m.streamingReasoning = ""
	m.scrollOffset = 0
	m.agentStatusText = ""
	for _, message := range messages {
		switch message.Role {
		case llm.RoleUser:
			m.appendUserMessage(message.Content)
		case llm.RoleAssistant:
			if strings.TrimSpace(message.ReasoningContent) != "" {
				m.appendMessage(messageReasoning, message.ReasoningContent)
			}
			if strings.TrimSpace(message.Content) != "" {
				m.appendMessage(messageAssistant, message.Content)
			}
		}
	}
}

func (m *Model) replaySessionEntries(entries []session.Entry, fallback []llm.Message) {
	if len(entries) == 0 {
		m.replaySessionHistory(fallback)
		return
	}
	m.messages = nil
	m.streamingAssistantIndex = -1
	m.streamingAssistant = ""
	m.streamingReasoningIndex = -1
	m.streamingReasoning = ""
	m.scrollOffset = 0
	m.agentStatusText = ""
	for _, entry := range entries {
		switch entry.Type {
		case session.TypeMessage:
			if entry.Message != nil {
				m.replayMessage(*entry.Message)
			}
		case session.TypeCustomMessage:
			m.appendUserMessage(entry.Content)
		case session.TypeCompaction:
			m.appendUserMessage("[session compaction summary]\nThe following is a compacted summary of earlier conversation history. Treat it as background context when continuing the task, not as a new user instruction.\n\n" + entry.Summary)
		case session.TypePlanEvent:
			if entry.Plan != nil {
				m.appendPresentedPlan(*entry.Plan)
			}
		}
	}
}

func (m *Model) replayMessage(message llm.Message) {
	switch message.Role {
	case llm.RoleUser:
		m.appendUserMessage(message.Content)
	case llm.RoleAssistant:
		if strings.TrimSpace(message.ReasoningContent) != "" {
			m.appendMessage(messageReasoning, message.ReasoningContent)
		}
		if strings.TrimSpace(message.Content) != "" {
			m.appendMessage(messageAssistant, message.Content)
		}
	}
}

func (m *Model) scrollBy(lines int) {
	m.scrollOffset = m.clampScrollOffset(m.scrollOffset + lines)
}

func (m *Model) clampScrollOffset(offset int) int {
	return clamp(offset, 0, m.maxScrollOffset())
}

func (m *Model) maxScrollOffset() int {
	columns := max(20, m.width)
	layout := m.layoutFor(columns, max(8, m.height))
	return max(0, len(m.wrappedMessageLines(columns))-layout.messageRows)
}

func (m *Model) drawMessages(canvas []string, columns, messageRows int) {
	lines := m.visibleMessageLines(columns, messageRows, m.scrollOffset)
	for row, line := range lines {
		setRow(canvas, row, columns, renderMessageLine(line, columns))
	}
}

func renderMessageLine(line renderLine, columns int) string {
	text := fit(line.text, columns)
	if line.kind == messagePlan {
		return styleForMessage(line.kind).Render(padRight(text, columns))
	}
	return styleForMessage(line.kind).Render(text)
}

func isModelSelectorPopup(m *Model) bool {
	if !m.modelSelectorOpen {
		return false
	}
	items := m.completions()
	if len(items) == 0 {
		return false
	}
	// reasoning 子命令的 5 档列表 Group=="Reasoning"，不显示 reasoning 行
	return items[0].Group == "Model"
}

func (m *Model) drawCompletions(canvas []string, columns, indexStatusRow int) {
	items := m.completions()
	if len(items) == 0 || indexStatusRow <= 1 {
		return
	}
	modelRows := min(maxCompletionRows, len(items))
	popupRows := modelRows
	if isModelSelectorPopup(m) {
		popupRows = modelRows + 2 // reasoning 行 + 底框
	}
	top := max(0, indexStatusRow-popupRows-1)
	visible := min(modelRows, max(0, indexStatusRow-top-1))
	if visible <= 0 {
		return
	}
	width := min(columns, 72)
	selected := clamp(m.selectedCompletion, 0, len(items)-1)
	first := firstVisibleCompletionIndex(selected, len(items), visible)
	setRow(canvas, top, columns, dimStyle.Render("┌"+strings.Repeat("─", max(0, width-2))+"┐"))
	for i := range visible {
		itemIndex := first + i
		item := items[itemIndex]
		body := " " + item.Display + "  " + item.Description
		line := "│" + padRight(body, max(0, width-2)) + "│"
		style := infoStyle
		if itemIndex == selected {
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("14")).Bold(true)
		}
		setRow(canvas, top+i+1, columns, style.Render(fit(line, width)))
	}
	if isModelSelectorPopup(m) {
		body := " Reasoning effort: " + m.pendingReasoningEffort + "  ←/→ to adjust"
		line := "│" + padRight(body, max(0, width-2)) + "│"
		setRow(canvas, top+visible+1, columns, dimStyle.Render(line))
		setRow(canvas, top+visible+2, columns, dimStyle.Render("└"+strings.Repeat("─", max(0, width-2))+"┘"))
	}
}

func (m *Model) drawInput(canvas []string, columns int, layout tuiLayout) {
	if m.agentStatusText != "" {
		leftFrame := 3
		content := dimStyle.Render(" " + m.agentStatusText + " ")
		contentW := runewidth.StringWidth(m.agentStatusText) + 2
		restW := max(0, columns-contentW-leftFrame)
		setRow(canvas, layout.inputTop, columns, dimStyle.Render(strings.Repeat("━", leftFrame))+content+dimStyle.Render(strings.Repeat("━", restW)))
	} else {
		setRow(canvas, layout.inputTop, columns, dimStyle.Render(inputFrameLine(columns)))
	}
	setRow(canvas, layout.inputBottom, columns, dimStyle.Render(inputFrameLine(columns)))
	promptStyle := userStyle
	if m.busy {
		promptStyle = warnStyle
	}
	promptWidth := runewidth.StringWidth(inputPrompt)
	contentWidth := max(1, columns-promptWidth)
	lines := visibleInputLines(m.wrappedInputLines(contentWidth), layout.inputRows)
	for i := 0; i < layout.inputRows; i++ {
		prefix := strings.Repeat(" ", promptWidth)
		if i == 0 {
			prefix = promptStyle.Render(inputPrompt)
		}
		input := strings.Repeat(" ", contentWidth)
		if i < len(lines) {
			input = renderInputLine(lines[i], contentWidth)
		}
		setRow(canvas, layout.inputLine+i, columns, prefix+input)
	}
}

func (m *Model) drawStatus(canvas []string, columns, row int) {
	if m.runtime == nil {
		setRow(canvas, row, columns, dimStyle.Render(" bruce"))
		return
	}
	status := m.runtime.Status()
	permission := " HITL on "
	permissionStyle := okStyle
	if !status.HITLEnabled {
		permission = " HITL off "
		permissionStyle = warnStyle
	}
	details := statusDetails(status, m.runtime.HomeDir, m.elapsedMillis)
	left := permissionStyle.Render(permission)
	remaining := max(0, columns-runewidth.StringWidth(permission))
	setRow(canvas, row, columns, left+dimStyle.Render(fit(details, remaining)))
}

func statusDetails(status bruntime.Status, homeDir string, elapsedMillis int64) string {
	model := empty(status.Model, "auto")
	if effort := strings.TrimSpace(status.ReasoningEffort); effort != "" {
		model += " (" + effort + ")"
	}
	details := fmt.Sprintf(" bruce · %s · mode %s · %s",
		model,
		strings.ToLower(string(status.Mode)),
		compactPath(status.WorkspaceRoot, homeDir),
	)
	details += " · sandbox " + empty(status.SandboxMode, "unknown")
	if elapsedMillis > 0 {
		details += fmt.Sprintf(" · %dms", elapsedMillis)
	}
	return details
}

func (m *Model) drawApproval(canvas []string, columns, rows int) {
	if m.approval == nil {
		return
	}
	width := min(columns-4, 76)
	height := min(rows-4, 16)
	if width < 20 || height < 6 {
		return
	}
	left := max(0, (columns-width)/2)
	top := max(0, (rows-height)/2)
	title := " HITL Approval "
	setOverlayRow(canvas, top, columns, left, warnStyle.Render("┌"+title+strings.Repeat("─", max(0, width-2-runewidth.StringWidth(title)))+"┐"))
	lines := m.approvalLines(width - 4)
	for i := 0; i < height-2; i++ {
		text := ""
		if i < len(lines) {
			text = lines[i]
		}
		setOverlayRow(canvas, top+i+1, columns, left, baseStyle.Render("│ "+padRight(text, width-4)+" │"))
	}
	setOverlayRow(canvas, top+height-1, columns, left, warnStyle.Render("└"+strings.Repeat("─", max(0, width-2))+"┘"))
}

func (m *Model) approvalLines(width int) []string {
	dialog := m.approval
	if dialog == nil {
		return nil
	}
	request := dialog.request
	lines := []string{
		"Tool: " + request.ToolName,
		"Level: " + request.DangerLevel,
		"Risk: " + request.RiskDescription,
	}
	if strings.TrimSpace(request.Suggestion) != "" {
		lines = append(lines, "Suggestion: "+strings.TrimSpace(request.Suggestion))
	}
	lines = append(lines, "Arguments: "+request.Arguments, "")
	switch dialog.mode {
	case approvalRejectReason:
		lines = append(lines, "Rejection reason: "+string(dialog.text))
	case approvalModifyArgs:
		lines = append(lines, "Modified arguments JSON: "+string(dialog.text))
	default:
		lines = append(lines, "[Enter/y] Approve  [a] Allow all  [n] Reject  [s] Skip  [m] Modify")
	}
	var wrapped []string
	for _, line := range lines {
		wrapped = append(wrapped, wrap(line, width)...)
	}
	return wrapped
}

func (m *Model) wrappedInputLines(width int) []inputRenderLine {
	width = max(1, width)
	styles := inputRuneStyles(m.inputText(), len(m.input))
	lines := []inputRenderLine{{}}
	appendCell := func(cell inputRenderCell) {
		current := &lines[len(lines)-1]
		if len(current.cells) > 0 && current.width+cell.width > width {
			lines = append(lines, inputRenderLine{})
			current = &lines[len(lines)-1]
		}
		current.cells = append(current.cells, cell)
		current.width += cell.width
		if cell.cursor {
			current.cursor = true
		}
	}
	for i, r := range m.input {
		appendCell(inputRenderCell{
			text:   string(r),
			style:  styles[i],
			cursor: i == m.cursor,
			width:  max(0, runewidth.RuneWidth(r)),
		})
	}
	if m.cursor == len(m.input) {
		appendCell(inputRenderCell{text: " ", cursor: true, width: 1})
	}
	return lines
}

func renderInputLine(line inputRenderLine, width int) string {
	var out strings.Builder
	for _, cell := range line.cells {
		if cell.cursor {
			out.WriteString(cursorCellStyle.Render(cell.text))
			continue
		}
		out.WriteString(styleForInput(cell.style).Render(cell.text))
	}
	if line.width < width {
		out.WriteString(strings.Repeat(" ", width-line.width))
	}
	return out.String()
}

func visibleInputLines(lines []inputRenderLine, rows int) []inputRenderLine {
	if len(lines) == 0 {
		return []inputRenderLine{{cells: []inputRenderCell{{text: " ", cursor: true, width: 1}}, width: 1, cursor: true}}
	}
	rows = max(1, rows)
	if len(lines) <= rows {
		return append([]inputRenderLine(nil), lines...)
	}
	cursorLine := len(lines) - 1
	for i, line := range lines {
		if line.cursor {
			cursorLine = i
			break
		}
	}
	start := clamp(cursorLine-rows+1, 0, len(lines)-rows)
	return append([]inputRenderLine(nil), lines[start:start+rows]...)
}

func inputRuneStyles(input string, length int) []inputStyle {
	styles := make([]inputStyle, length)
	offset := 0
	for _, span := range highlightInput(input) {
		for range []rune(span.Text) {
			if offset < len(styles) {
				styles[offset] = span.Style
			}
			offset++
		}
	}
	return styles
}

func (m *Model) wrappedMessageLines(columns int) []renderLine {
	var out []renderLine
	for _, message := range m.messages {
		for _, raw := range strings.Split(message.text, "\n") {
			for _, line := range wrap(raw, columns) {
				out = append(out, renderLine{kind: message.kind, text: line})
			}
		}
		out = append(out, renderLine{kind: messageAssistant, text: ""})
	}
	if len(out) > 0 && strings.TrimSpace(out[len(out)-1].text) == "" {
		out = out[:len(out)-1]
	}
	return out
}

func (m *Model) visibleMessageLines(columns, messageRows, scrollOffset int) []renderLine {
	lines := m.wrappedMessageLines(columns)
	maxOffset := max(0, len(lines)-max(1, messageRows))
	offset := clamp(scrollOffset, 0, maxOffset)
	start := max(0, len(lines)-messageRows-offset)
	end := min(len(lines), start+max(0, messageRows))
	return append([]renderLine(nil), lines[start:end]...)
}

type tuiLayout struct {
	messageRows    int
	indexStatusRow int
	inputTop       int
	inputLine      int
	inputBottom    int
	inputRows      int
	statusRow      int
}

func (m *Model) layoutFor(columns, rows int) tuiLayout {
	promptWidth := runewidth.StringWidth(inputPrompt)
	inputRows := len(m.wrappedInputLines(max(1, columns-promptWidth)))
	return layoutFor(rows, inputRows)
}

func layoutFor(rows, inputRows int) tuiLayout {
	rows = max(8, rows)
	inputRows = clamp(inputRows, 1, max(1, rows-5))
	statusRow := rows - 1
	inputBottom := statusRow - 1
	inputTop := inputBottom - inputRows - 1
	indexStatusRow := max(1, inputTop-1)
	return tuiLayout{
		messageRows:    max(1, indexStatusRow),
		indexStatusRow: indexStatusRow,
		inputTop:       inputTop,
		inputLine:      inputTop + 1,
		inputBottom:    inputBottom,
		inputRows:      inputRows,
		statusRow:      statusRow,
	}
}

func inputFrameLine(columns int) string {
	return strings.Repeat("━", max(1, columns))
}

func firstVisibleCompletionIndex(selected, count, visibleRows int) int {
	if count <= 0 || visibleRows <= 0 {
		return 0
	}
	visible := min(visibleRows, count)
	selected = clamp(selected, 0, count-1)
	maxFirst := max(0, count-visible)
	return min(maxFirst, max(0, selected-visible+1))
}

func welcomeLines(rt *integrated.Runtime) []string {
	model := "auto"
	workspace := ""
	if rt != nil {
		model = empty(rt.Client.ModelName(), "auto")
		workspace = compactPath(rt.Workspace, rt.HomeDir)
	}
	// title is ASCII-only so runewidth.StringWidth matches terminal cell width;
	// box-drawing runes like ─ are mis-measured by runewidth (counted as 2,
	// rendered as 1) in EastAsianWidth locales, which would skew the border math.
	title := " Bruce Coding Agent " + version.Current + " "
	rows := []string{
		"  Welcome back!",
		"  model: " + model,
		"  workspace: " + workspace,
	}
	inner := runewidth.StringWidth(title)
	for _, r := range rows {
		if w := runewidth.StringWidth(r); w > inner {
			inner = w
		}
	}
	inner += 5 // right inner padding
	out := []string{
		"┌─" + title + strings.Repeat("─", max(0, inner-1-runewidth.StringWidth(title))) + "┐",
	}
	for _, r := range rows {
		out = append(out, "│"+padRight(r, inner)+"│")
	}
	out = append(out, "└"+strings.Repeat("─", inner)+"┘")
	return out
}

func presentedPlanText(plan bruntime.PlanEvent) string {
	title := "Plan"
	if strings.TrimSpace(plan.ID) != "" {
		title += ": " + strings.TrimSpace(plan.ID)
		if plan.Revision > 0 {
			title += fmt.Sprintf(" rev=%d", plan.Revision)
		}
	}
	content := strings.TrimSpace(plan.Content)
	if content == "" {
		return ""
	}
	return title + "\n\n" + content
}

func toolActivityText(call llm.ToolCall, status string) string {
	return "Tool call: " + toolName(call) + " (" + status + ")"
}

func toolActivityKey(runID string, call llm.ToolCall) string {
	id := call.ID
	if id == "" {
		id = toolName(call)
	}
	return runID + ":" + id
}

func toolName(call llm.ToolCall) string {
	name := call.Function.Name
	if name == "" {
		return "unknown"
	}
	if summary := builtinToolSummary(name, call.Function.Arguments); summary != "" {
		return name + "[" + summary + "]"
	}
	return name
}

func builtinToolSummary(name, rawArgs string) string {
	argName := ""
	switch name {
	case "read_file", "write_file", "edit_file":
		argName = "path"
	case "execute_command":
		argName = "command"
	default:
		return ""
	}
	var args map[string]any
	if json.Unmarshal([]byte(rawArgs), &args) != nil {
		return ""
	}
	value, ok := args[argName].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func compactPath(path, homeDir string) string {
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if homeDir == "" {
		return path
	}
	homeDir = filepath.Clean(homeDir)
	relative, err := filepath.Rel(homeDir, path)
	if err != nil || filepath.IsAbs(relative) {
		return path
	}
	if relative == "." {
		return "~"
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return path
	}
	return "~/" + filepath.ToSlash(relative)
}

func wrap(text string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	if runewidth.StringWidth(text) <= width {
		return []string{text}
	}
	var lines []string
	var line strings.Builder
	cols := 0
	for _, r := range text {
		w := max(0, runewidth.RuneWidth(r))
		if line.Len() > 0 && cols+w > width {
			lines = append(lines, line.String())
			line.Reset()
			cols = 0
		}
		line.WriteRune(r)
		cols += w
	}
	if line.Len() > 0 {
		lines = append(lines, line.String())
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

func fit(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(value) <= width {
		return value
	}
	ellipsis := "…"
	target := max(0, width-runewidth.StringWidth(ellipsis))
	var out strings.Builder
	cols := 0
	for _, r := range value {
		w := runewidth.RuneWidth(r)
		if cols+w > target {
			break
		}
		out.WriteRune(r)
		cols += w
	}
	out.WriteString(ellipsis)
	return out.String()
}

func padRight(value string, width int) string {
	value = fit(value, width)
	pad := width - runewidth.StringWidth(value)
	if pad <= 0 {
		return value
	}
	return value + strings.Repeat(" ", pad)
}

func setRow(canvas []string, row, width int, content string) {
	if row < 0 || row >= len(canvas) {
		return
	}
	visible := lipgloss.Width(content)
	if visible < width {
		content += strings.Repeat(" ", width-visible)
	}
	canvas[row] = content
}

func setOverlayRow(canvas []string, row, columns, left int, content string) {
	if row < 0 || row >= len(canvas) {
		return
	}
	prefix := strings.Repeat(" ", max(0, left))
	visible := left + lipgloss.Width(content)
	if visible < columns {
		content += strings.Repeat(" ", columns-visible)
	}
	canvas[row] = prefix + content
}

func styleForMessage(kind messageKind) lipgloss.Style {
	switch kind {
	case messageUser:
		return userStyle
	case messageSystem:
		return infoStyle
	case messageReasoning:
		return reasoningStyle
	case messagePlan:
		return planStyle
	default:
		return baseStyle
	}
}

func styleForInput(style inputStyle) lipgloss.Style {
	switch style {
	case styleCommand:
		return infoStyle.Bold(true)
	case styleMention:
		return mentionStyle
	case styleImage:
		return imageStyle
	case styleDanger:
		return warnStyle
	case styleSecret:
		return brandStyle
	default:
		return baseStyle
	}
}

var (
	baseStyle       = lipgloss.NewStyle()
	dimStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	brandStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	okStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	warnStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	infoStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	userStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	mentionStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	imageStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	cursorCellStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("#C0C0C0")).Bold(true)
	reasoningStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#A0A0A0"))
	planStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("#E6F4EA"))
)

func empty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func clamp(value, low, high int) int {
	if high < low {
		return low
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
