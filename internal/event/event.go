package event

import (
	"sync"
	"time"

	"bruce-go/internal/llm"
	"bruce-go/internal/runtime"
	"bruce-go/internal/session"
	"bruce-go/internal/tool"
)

type Event interface {
	Type() string
}

type Basic struct {
	Kind      string
	RunID     string
	Timestamp time.Time
	Payload   any
}

func (e Basic) Type() string { return e.Kind }

type Listener func(Event)

type Bus struct {
	mu        sync.RWMutex
	listeners map[int]Listener
	nextID    int
}

func NewBus() *Bus {
	return &Bus{listeners: map[int]Listener{}}
}

func (b *Bus) Subscribe(listener Listener) func() {
	if listener == nil {
		return func() {}
	}
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.listeners[id] = listener
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.listeners, id)
		b.mu.Unlock()
	}
}

func (b *Bus) Emit(event Event) {
	if event == nil {
		return
	}
	b.mu.RLock()
	listeners := make([]Listener, 0, len(b.listeners))
	for _, listener := range b.listeners {
		listeners = append(listeners, listener)
	}
	b.mu.RUnlock()
	for _, listener := range listeners {
		func() {
			defer func() { _ = recover() }()
			listener(event)
		}()
	}
}

func NewRunID() string {
	return "r_" + time.Now().UTC().Format("20060102T150405.000000000")
}

type RunStarted struct {
	RunID     string
	Timestamp time.Time
	Mode      runtime.AgentMode
	Input     string
}

func NewRunStarted(runID string, mode runtime.AgentMode, input string) RunStarted {
	return RunStarted{RunID: runID, Timestamp: time.Now(), Mode: mode, Input: input}
}

func (e RunStarted) Type() string { return "run_started" }

type RunCompleted struct {
	RunID     string
	Timestamp time.Time
	Output    string
}

func NewRunCompleted(runID, output string) RunCompleted {
	return RunCompleted{RunID: runID, Timestamp: time.Now(), Output: output}
}

func (e RunCompleted) Type() string { return "run_completed" }

type RunFailed struct {
	RunID     string
	Timestamp time.Time
	Message   string
}

func NewRunFailed(runID, message string) RunFailed {
	return RunFailed{RunID: runID, Timestamp: time.Now(), Message: message}
}

func (e RunFailed) Type() string { return "run_failed" }

type MessageStarted struct {
	RunID     string
	Timestamp time.Time
	Role      string
}

func NewMessageStarted(runID, role string) MessageStarted {
	return MessageStarted{RunID: runID, Timestamp: time.Now(), Role: role}
}

func (e MessageStarted) Type() string { return "message_started" }

type MessageDelta struct {
	RunID     string
	Timestamp time.Time
	Role      string
	Channel   string
	Delta     string
}

func NewMessageDelta(runID, role, channel, delta string) MessageDelta {
	return MessageDelta{RunID: runID, Timestamp: time.Now(), Role: role, Channel: channel, Delta: delta}
}

func (e MessageDelta) Type() string { return "message_delta" }

type MessageCompleted struct {
	RunID     string
	Timestamp time.Time
	Message   llm.Message
	Durable   bool
}

func NewMessageCompleted(runID string, message llm.Message, durable bool) MessageCompleted {
	return MessageCompleted{RunID: runID, Timestamp: time.Now(), Message: message, Durable: durable}
}

func (e MessageCompleted) Type() string { return "message_completed" }

type ToolCallStarted struct {
	RunID     string
	Timestamp time.Time
	ToolCall  llm.ToolCall
}

func NewToolCallStarted(runID string, call llm.ToolCall) ToolCallStarted {
	return ToolCallStarted{RunID: runID, Timestamp: time.Now(), ToolCall: call}
}

func (e ToolCallStarted) Type() string { return "tool_call_started" }

type ToolCallCompleted struct {
	RunID     string
	Timestamp time.Time
	Result    tool.ToolCallResult
}

func NewToolCallCompleted(runID string, result tool.ToolCallResult) ToolCallCompleted {
	return ToolCallCompleted{RunID: runID, Timestamp: time.Now(), Result: result}
}

func (e ToolCallCompleted) Type() string { return "tool_call_completed" }

type ModeChanged struct {
	RunID     string
	Timestamp time.Time
	Mode      runtime.AgentMode
}

func NewModeChanged(mode runtime.AgentMode) ModeChanged {
	return ModeChanged{Timestamp: time.Now(), Mode: mode}
}

func (e ModeChanged) Type() string { return "mode_changed" }

type SessionChanged struct {
	RunID     string
	Timestamp time.Time
	Reason    string
	Context   session.Context
}

func NewSessionChanged(reason string, context session.Context) SessionChanged {
	return SessionChanged{Timestamp: time.Now(), Reason: reason, Context: context}
}

func (e SessionChanged) Type() string { return "session_changed" }

type Activity struct {
	RunID     string
	Timestamp time.Time
	Message   string
}

func NewActivity(runID, message string) Activity {
	return Activity{RunID: runID, Timestamp: time.Now(), Message: message}
}

func (e Activity) Type() string { return "activity" }
