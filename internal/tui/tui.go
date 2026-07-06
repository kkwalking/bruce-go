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
)

const (
	historySize       = 2000
	historyFileSize   = 10000
	scrollLines       = 2
	maxCompletionRows = 6
)

type messageKind int

const (
	messageUser messageKind = iota
	messageAssistant
	messageSystem
	messageActivity
	messageReasoning
)

type tuiMessage struct {
	kind messageKind
	text string
}

type renderLine struct {
	kind messageKind
	text string
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
		m.approval = &approvalDialog{request: msg.Request, reply: msg.Reply}
		m.appendActivity("HITL 等待审批: " + msg.Request.ToolName)
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
	layout := layoutFor(rows)
	m.drawMessages(canvas, columns, layout.messageRows)
	m.drawCompletions(canvas, columns, layout.indexStatusRow)
	m.drawInput(canvas, columns, layout.inputTop, layout.inputLine, layout.inputBottom)
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
		m.appendSystemMessage("输入已清空。")
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
				m.appendSystemMessage("⏹ 任务已被用户中断。")
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
			m.appendSystemMessage("执行失败: " + msg.result.Err.Error())
		}
		if msg.result.Exit {
			m.quit = true
			return tea.Quit
		}
		return nil
	}
	if msg.err != nil {
		m.appendSystemMessage("执行失败: " + msg.err.Error())
	}
	return nil
}

func (m *Model) handleRuntimeEvent(evt event.Event) {
	switch e := evt.(type) {
	case event.RunStarted:
		m.toolActivityIndexes = map[string]int{}
		m.agentStatusText = "💭 思考中..."
	case event.MessageStarted:
		if e.Role == llm.RoleAssistant {
			m.beginStreamingAssistantMessage()
		}
	case event.MessageDelta:
		if e.Role == llm.RoleAssistant {
			switch e.Channel {
			case "reasoning":
				m.appendStreamingReasoningDelta(e.Delta)
				m.agentStatusText = "🧠 推理中..."
			case "content":
				m.appendStreamingAssistantDelta(e.Delta)
				m.agentStatusText = "📝 生成回答..."
			}
		}
	case event.MessageCompleted:
		if e.Message.Role == llm.RoleAssistant {
			m.finishStreamingReasoningMessage(e.Message.ReasoningContent)
			m.finishStreamingAssistantMessage(e.Message.Content)
		}
	case event.ToolCallStarted:
		m.agentStatusText = "🔧 调用工具..."
		index := m.appendActivityAndReturnIndex(toolActivityText(e.ToolCall, "处理中"))
		if index >= 0 {
			m.toolActivityIndexes[toolActivityKey(e.RunID, e.ToolCall)] = index
		}
	case event.ToolCallCompleted:
		status := "完成"
		if e.Result.Status != "success" {
			status = "失败"
		}
		text := toolActivityText(e.Result.ToolCall, status)
		key := toolActivityKey(e.RunID, e.Result.ToolCall)
		if index, ok := m.toolActivityIndexes[key]; ok && m.replaceActivity(index, text) {
			delete(m.toolActivityIndexes, key)
		} else {
			m.appendActivity(text)
		}
	case event.Activity:
		m.appendActivity(e.Message)
	case event.SessionChanged:
		if e.Reason == "resume" || e.Reason == "compact" {
			m.replaySessionHistory(e.Context.Messages)
			m.agentStatusText = ""
		}
	case event.RunFailed:
		m.agentStatusText = "❌ 失败"
		if e.Message != "" {
			m.appendSystemMessage("执行失败: " + e.Message)
		}
	case event.RunCompleted:
		m.agentStatusText = ""
	case event.Basic:
		m.handleBasicEvent(e)
	}
	m.scrollOffset = m.clampScrollOffset(m.scrollOffset)
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
		m.appendSystemMessage("当前任务仍在执行中。")
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
		m.completeApproval(approval.Reject("用户取消审批"))
	case "backspace":
		if len(dialog.text) > 0 {
			dialog.text = dialog.text[:len(dialog.text)-1]
		}
	case "enter":
		switch dialog.mode {
		case approvalRejectReason:
			reason := strings.TrimSpace(string(dialog.text))
			if reason == "" {
				reason = "用户拒绝了此操作"
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
	reply <- result
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
	if !m.modelSelectorOpen && isExactModelCommand(value) {
		return nil
	}
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
	if m.runtime != nil {
		m.pendingReasoningEffort = m.runtime.ReasoningEffort()
	} else {
		m.pendingReasoningEffort = ""
	}
	items := filterCompletionGroup(m.completions(), "Model")
	m.selectedCompletion = 0
	for i, item := range items {
		if item.Description == "当前模型" {
			m.selectedCompletion = i
			return
		}
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

func (m *Model) scrollBy(lines int) {
	m.scrollOffset = m.clampScrollOffset(m.scrollOffset + lines)
}

func (m *Model) clampScrollOffset(offset int) int {
	return clamp(offset, 0, m.maxScrollOffset())
}

func (m *Model) maxScrollOffset() int {
	layout := layoutFor(max(8, m.height))
	return max(0, len(m.wrappedMessageLines(max(20, m.width)))-layout.messageRows)
}

func (m *Model) drawMessages(canvas []string, columns, messageRows int) {
	lines := m.visibleMessageLines(columns, messageRows, m.scrollOffset)
	for row, line := range lines {
		setRow(canvas, row, columns, styleForMessage(line.kind).Render(fit(line.text, columns)))
	}
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
		body := " 推理强度: " + m.pendingReasoningEffort + "  ←/→ to adjust"
		line := "│" + padRight(body, max(0, width-2)) + "│"
		setRow(canvas, top+visible+1, columns, dimStyle.Render(line))
		setRow(canvas, top+visible+2, columns, dimStyle.Render("└"+strings.Repeat("─", max(0, width-2))+"┘"))
	}
}

func (m *Model) drawInput(canvas []string, columns, inputTop, inputLine, inputBottom int) {
	if m.agentStatusText != "" {
		leftFrame := 3
		content := dimStyle.Render(" " + m.agentStatusText + " ")
		contentW := runewidth.StringWidth(m.agentStatusText) + 2
		restW := max(0, columns-contentW-leftFrame)
		setRow(canvas, inputTop, columns, dimStyle.Render(strings.Repeat("━", leftFrame))+content+dimStyle.Render(strings.Repeat("━", restW)))
	} else {
		setRow(canvas, inputTop, columns, dimStyle.Render(inputFrameLine(columns)))
	}
	setRow(canvas, inputBottom, columns, dimStyle.Render(inputFrameLine(columns)))
	prompt := "❯ "
	promptStyle := userStyle
	if m.busy {
		promptStyle = warnStyle
	}
	promptWidth := runewidth.StringWidth(prompt)
	input := m.renderHighlightedInput(max(0, columns-promptWidth), promptStyle)
	setRow(canvas, inputLine, columns, promptStyle.Render(prompt)+input)
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
	details := fmt.Sprintf(" bruce · %s · mode %s · %s",
		empty(status.Model, "auto"),
		strings.ToLower(string(status.Mode)),
		compactPath(status.WorkspaceRoot),
	)
	if m.elapsedMillis > 0 {
		details += fmt.Sprintf(" · %dms", m.elapsedMillis)
	}
	left := permissionStyle.Render(permission)
	remaining := max(0, columns-runewidth.StringWidth(permission))
	setRow(canvas, row, columns, left+dimStyle.Render(fit(details, remaining)))
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
	setOverlayRow(canvas, top, columns, left, warnStyle.Render("┌ HITL 审批 "+strings.Repeat("─", max(0, width-12))+"┐"))
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
		"工具: " + request.ToolName,
		"等级: " + request.DangerLevel,
		"风险: " + request.RiskDescription,
	}
	if strings.TrimSpace(request.Suggestion) != "" {
		lines = append(lines, "建议: "+strings.TrimSpace(request.Suggestion))
	}
	lines = append(lines, "参数: "+request.Arguments, "")
	switch dialog.mode {
	case approvalRejectReason:
		lines = append(lines, "拒绝原因: "+string(dialog.text))
	case approvalModifyArgs:
		lines = append(lines, "修改参数 JSON: "+string(dialog.text))
	default:
		lines = append(lines, "[Enter/y] 批准  [a] 全部放行  [n] 拒绝  [s] 跳过  [m] 修改")
	}
	var wrapped []string
	for _, line := range lines {
		wrapped = append(wrapped, wrap(line, width)...)
	}
	return wrapped
}

func (m *Model) renderHighlightedInput(width int, cursorStyle lipgloss.Style) string {
	if width <= 0 {
		return ""
	}
	styles := inputRuneStyles(m.inputText(), len(m.input))
	var out strings.Builder
	used := 0
	for i, r := range m.input {
		if i == m.cursor {
			if used+runewidth.RuneWidth(r) > width {
				break
			}
			out.WriteString(cursorCellStyle.Render(string(r)))
			used += runewidth.RuneWidth(r)
			continue
		}
		w := runewidth.RuneWidth(r)
		if used+w > width {
			break
		}
		out.WriteString(styleForInput(styles[i]).Render(string(r)))
		used += w
	}
	if m.cursor == len(m.input) && used < width {
		out.WriteString(cursorCellStyle.Render(" "))
		used++
	}
	if used < width {
		out.WriteString(strings.Repeat(" ", width-used))
	}
	_ = cursorStyle
	return out.String()
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
	statusRow      int
}

func layoutFor(rows int) tuiLayout {
	rows = max(8, rows)
	return tuiLayout{
		messageRows:    max(1, rows-5),
		indexStatusRow: rows - 5,
		inputTop:       rows - 4,
		inputLine:      rows - 3,
		inputBottom:    rows - 2,
		statusRow:      rows - 1,
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
		workspace = compactPath(rt.Workspace)
	}
	// title is ASCII-only so runewidth.StringWidth matches terminal cell width;
	// box-drawing runes like ─ are mis-measured by runewidth (counted as 2,
	// rendered as 1) in EastAsianWidth locales, which would skew the border math.
	title := " Bruce Coding Agent "
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

func toolActivityText(call llm.ToolCall, status string) string {
	return "工具调用: " + toolName(call) + " (" + status + ")"
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

func compactPath(path string) string {
	if path == "" {
		return ""
	}
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) {
		return path
	}
	return "~/" + base
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
