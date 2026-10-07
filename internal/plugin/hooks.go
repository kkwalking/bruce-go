package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"bruce-go/internal/tool"
)

// Observer hook events: a hook that may only watch. The accepted set lives in
// manifest.go, which validates a declaration before anything runs; these
// constants exist so host code does not spell the strings by hand.
const (
	HookSessionStarted = "session.started"
	HookSessionEnded   = "session.ended"
	HookToolStarted    = "tool.started"
	HookToolCompleted  = "tool.completed"
	HookMessageCreated = "message.created"
)

// Interceptor hook events: a hook that may change or stop a flow.
const (
	HookChatBefore = "chat.before"
	HookToolBefore = "tool.before"
	HookToolAfter  = "tool.after"
)

// HookResult is what an interceptor returns.
type HookResult struct {
	// Args replaces the tool arguments when non-nil.
	Args tool.Args
	// Result replaces the tool outcome when non-nil.
	Result *tool.ExecutionOutcome
	// Block refuses the invocation.
	Block bool
	// Reason explains a block, and is shown to the model.
	Reason string
}

// HookContext carries the invocation a hook is observing.
type HookContext struct {
	Event  string
	Tool   string
	Args   tool.Args
	Result *tool.ExecutionOutcome
	// RunID, SessionID and Mode let an observer correlate events.
	RunID     string
	SessionID string
	Mode      string
}

// hookBinding is one registered hook handler.
type hookBinding struct {
	plugin   string
	event    string
	handler  string
	timeout  time.Duration
	observer bool
	// order is the registration sequence, so hook ordering is deterministic:
	// plugin name first, then declaration order.
	order int
}

// HookManager runs plugin hooks.
//
// Ordering is deterministic and documented: observer hooks run in plugin-name
// order, then declaration order. Interceptor hooks run in the same order, which
// makes the pipeline predictable: the second interceptor sees what the first
// one produced.
//
// Failure policy is explicit, because "a hook failed" has three different
// correct answers:
//
//   - An observer hook that fails is logged and skipped. Observation must
//     never be able to break a session.
//   - A before-interceptor that fails fails closed: the invocation is refused.
//     A hook that guards a security decision cannot be silently skipped,
//     because skipping it would be exactly the bypass it exists to prevent.
//   - An after-interceptor that fails leaves the produced result intact and
//     records the failure. The operation already happened; discarding its
//     result would lose real output.
//
// A failing plugin never disables another plugin: each binding runs in its own
// invocation, and one error does not stop the chain.
type HookManager struct {
	manager *Manager

	mu       sync.RWMutex
	bindings []hookBinding
	next     int
}

// NewHookManager creates a hook manager bound to a plugin manager.
func NewHookManager(manager *Manager) *HookManager {
	return &HookManager{manager: manager}
}

// Register adds every hook a loaded plugin declares.
func (h *HookManager) Register(pluginName string, manifest *Manifest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, declaration := range manifest.Hooks {
		event := strings.TrimSpace(declaration.Event)
		if !IsObserverHookEvent(event) && !IsInterceptorHookEvent(event) {
			continue
		}
		h.bindings = append(h.bindings, hookBinding{
			plugin:   pluginName,
			event:    event,
			handler:  declaration.Handler,
			timeout:  hookTimeout(declaration.TimeoutMS),
			observer: IsObserverHookEvent(event),
			order:    h.next,
		})
		h.next++
	}
	h.sortLocked()
}

// Unregister removes every hook a plugin registered.
//
// Reload calls this before re-registering, which is what keeps a reload from
// accumulating duplicate hooks.
func (h *HookManager) Unregister(pluginName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.bindings[:0]
	for _, binding := range h.bindings {
		if binding.plugin != pluginName {
			kept = append(kept, binding)
		}
	}
	h.bindings = kept
}

// UnregisterAll removes every hook.
func (h *HookManager) UnregisterAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bindings = nil
}

func (h *HookManager) sortLocked() {
	sort.SliceStable(h.bindings, func(i, j int) bool {
		if h.bindings[i].plugin != h.bindings[j].plugin {
			return h.bindings[i].plugin < h.bindings[j].plugin
		}
		return h.bindings[i].order < h.bindings[j].order
	})
}

// Bindings returns the registered hooks in execution order.
func (h *HookManager) Bindings() []HookBinding {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]HookBinding, 0, len(h.bindings))
	for _, binding := range h.bindings {
		out = append(out, HookBinding{
			Plugin: binding.plugin, Event: binding.event,
			Handler: binding.handler, Observer: binding.observer,
		})
	}
	return out
}

// HookBinding is one registered hook, as reported to the user.
type HookBinding struct {
	Plugin   string
	Event    string
	Handler  string
	Observer bool
}

// Count returns the number of registered hooks.
func (h *HookManager) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.bindings)
}

// Notify runs the observer hooks for an event.
//
// Every failure is contained: an observer hook cannot break the flow it
// observes, which is why this returns nothing.
func (h *HookManager) Notify(ctx context.Context, event string, call HookContext) {
	for _, binding := range h.bindingsFor(event, true) {
		call.Event = event
		if _, err := h.run(ctx, binding, call, nil); err != nil {
			h.recordFailure(binding, err)
		}
	}
}

// BeforeChat runs the chat.before interceptors.
//
// It returns the possibly rewritten message list and whether the turn should
// be refused. A failure in any before-interceptor refuses the turn: a
// security-relevant interceptor that cannot run must fail closed.
func (h *HookManager) BeforeChat(ctx context.Context, messages []json.RawMessage, call HookContext) ([]json.RawMessage, bool, string) {
	current := messages
	for _, binding := range h.bindingsFor(HookChatBefore, false) {
		call.Event = HookChatBefore
		payload := map[string]any{"messages": current}
		out, err := h.run(ctx, binding, call, payload)
		if err != nil {
			h.recordFailure(binding, err)
			return current, true, "plugin " + binding.plugin + " could not run its chat.before hook: " + Redact(err.Error())
		}
		var result struct {
			Messages []json.RawMessage `json:"messages"`
			Block    bool              `json:"block"`
			Reason   string            `json:"reason"`
		}
		if len(out) == 0 {
			continue
		}
		if err := json.Unmarshal(out, &result); err != nil {
			h.recordFailure(binding, err)
			return current, true, "plugin " + binding.plugin + " returned a malformed chat.before result: " + Redact(err.Error())
		}
		if result.Block {
			return current, true, "plugin " + binding.plugin + " refused the request: " + Redact(result.Reason)
		}
		if result.Messages != nil {
			current = result.Messages
		}
	}
	return current, false, ""
}

// BeforeTool runs the tool.before interceptors.
//
// The returned arguments are the ones every later check must use. This is the
// whole point of running interception inside the tool executor's prepare step:
// a hook that rewrites {"path":"src/a.go"} into {"path":"/etc/passwd"} is
// validated as /etc/passwd, not as the original.
func (h *HookManager) BeforeTool(ctx context.Context, call HookContext) (HookResult, error) {
	result := HookResult{Args: call.Args}
	for _, binding := range h.bindingsFor(HookToolBefore, false) {
		call.Event = HookToolBefore
		call.Args = result.Args
		payload := map[string]any{"tool": call.Tool, "args": result.Args, "runId": call.RunID, "mode": call.Mode}
		out, err := h.run(ctx, binding, call, payload)
		if err != nil {
			h.recordFailure(binding, err)
			return result, NewError(binding.plugin, "", binding.handler, StageInvoke, CategoryException,
				fmt.Errorf("the tool.before hook failed: %w", err))
		}
		if len(out) == 0 {
			continue
		}
		var hookResult struct {
			Args   tool.Args `json:"args"`
			Block  bool      `json:"block"`
			Reason string    `json:"reason"`
		}
		if err := json.Unmarshal(out, &hookResult); err != nil {
			h.recordFailure(binding, err)
			return result, NewError(binding.plugin, "", binding.handler, StageInvoke, CategoryMalformedResult,
				fmt.Errorf("the tool.before hook returned a malformed result: %w", err))
		}
		if hookResult.Block {
			result.Block = true
			result.Reason = "plugin " + binding.plugin + " refused the tool call: " + Redact(hookResult.Reason)
			return result, nil
		}
		if hookResult.Args != nil {
			result.Args = hookResult.Args
		}
	}
	return result, nil
}

// AfterTool runs the tool.after interceptors.
//
// A failure here keeps the produced outcome: the operation already ran, and
// throwing its result away because an observer could not run would lose real
// work.
func (h *HookManager) AfterTool(ctx context.Context, call HookContext) tool.ExecutionOutcome {
	outcome := tool.ExecutionOutcome{}
	if call.Result != nil {
		outcome = *call.Result
	}
	for _, binding := range h.bindingsFor(HookToolAfter, false) {
		call.Event = HookToolAfter
		call.Result = &outcome
		payload := map[string]any{
			"tool": call.Tool, "args": call.Args,
			"result": map[string]any{"output": outcome.Output, "status": string(outcome.Status)},
			"runId":  call.RunID, "mode": call.Mode,
		}
		out, err := h.run(ctx, binding, call, payload)
		if err != nil {
			h.recordFailure(binding, err)
			continue
		}
		if len(out) == 0 {
			continue
		}
		var hookResult struct {
			Result *struct {
				Output string `json:"output"`
				Status string `json:"status"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out, &hookResult); err != nil {
			h.recordFailure(binding, err)
			continue
		}
		if hookResult.Result == nil {
			continue
		}
		// An after hook may rewrite the text, but it must not be able to turn
		// a rejection, timeout or interruption into a success: that would let
		// a plugin erase a policy decision that already happened.
		if terminalStatus(outcome.Status) && strings.TrimSpace(hookResult.Result.Status) != "" {
			if tool.ToolCallStatus(hookResult.Result.Status) == tool.ToolCallSuccess {
				continue
			}
		}
		outcome.Output = hookResult.Result.Output
		if strings.TrimSpace(hookResult.Result.Status) != "" {
			outcome.Status = tool.ToolCallStatus(hookResult.Result.Status)
		}
	}
	return outcome
}

// bindingsFor returns the hooks for one event, in execution order.
func (h *HookManager) bindingsFor(event string, observer bool) []hookBinding {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []hookBinding
	for _, binding := range h.bindings {
		if binding.event == event && binding.observer == observer {
			out = append(out, binding)
		}
	}
	return out
}

// run executes one hook.
func (h *HookManager) run(ctx context.Context, binding hookBinding, call HookContext, payload map[string]any) (json.RawMessage, error) {
	plugin, ok := h.manager.plugin(binding.plugin)
	if !ok {
		return nil, NewError(binding.plugin, "", binding.handler, StageInvoke, CategoryInternal,
			errors.New("the plugin is no longer loaded"))
	}
	handler, ok := plugin.handlers[binding.handler]
	if !ok {
		return nil, NewError(binding.plugin, plugin.manifest.File, binding.handler, StageInvoke, CategoryMissingHandler,
			errors.New("the hook handler was not resolved at load time"))
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, NewError(binding.plugin, plugin.manifest.File, binding.handler, StageInvoke, CategoryMalformedResult, err)
	}
	hookCtx := ctx
	cancel := func() {}
	if binding.timeout > 0 {
		hookCtx, cancel = context.WithTimeout(ctx, binding.timeout)
	}
	defer cancel()

	started := h.manager.now()
	h.manager.emit(EventHookInvoked, map[string]any{
		"plugin": binding.plugin, "event": binding.event, "handler": binding.handler,
	})
	out, err := plugin.pool.Invoke(hookCtx, handler, encoded)
	if err != nil {
		category := categoryForEngineError(err, CategoryException)
		h.manager.emit(EventInvokeFailed, map[string]any{
			"plugin": binding.plugin, "handler": binding.handler, "event": binding.event,
			"category": string(category), "duration_ms": h.manager.now().Sub(started).Milliseconds(),
		})
		return nil, NewError(binding.plugin, plugin.manifest.File, binding.handler, StageInvoke, category, err)
	}
	h.manager.emit(EventInvokeDone, map[string]any{
		"plugin": binding.plugin, "handler": binding.handler, "event": binding.event,
		"duration_ms": h.manager.now().Sub(started).Milliseconds(),
	})
	return out, nil
}

// recordFailure reports a contained hook failure.
func (h *HookManager) recordFailure(binding hookBinding, err error) {
	h.manager.emit(EventInvokeFailed, map[string]any{
		"plugin": binding.plugin, "handler": binding.handler, "event": binding.event,
		"category": string(CategoryException), "error": Redact(err.Error()), "contained": true,
	})
}

func hookTimeout(milliseconds int) time.Duration {
	if milliseconds <= 0 {
		return DefaultInvocationTimeout
	}
	timeout := time.Duration(milliseconds) * time.Millisecond
	if timeout > MaxInvocationTimeout {
		return MaxInvocationTimeout
	}
	return timeout
}

// terminalStatus reports whether a status records a decision a hook must not
// be able to erase.
func terminalStatus(status tool.ToolCallStatus) bool {
	switch status {
	case tool.ToolCallRejected, tool.ToolCallInterrupted, tool.ToolCallTimeout:
		return true
	default:
		return false
	}
}

// ToolInterceptor adapts the hook manager to the tool registry's single
// before/after extension point.
//
// This is how plugin hooks reach tool execution without the plugin system
// creating a second execution path: the registry calls one interface, and the
// hook manager implements it.
type ToolInterceptor struct {
	Hooks *HookManager
	// Context supplies the run identity for a call.
	Context func(ctx context.Context) HookContext
}

var _ tool.Interceptor = ToolInterceptor{}

// BeforeTool implements tool.Interceptor.
func (i ToolInterceptor) BeforeTool(ctx context.Context, name string, args tool.Args) (tool.Args, error) {
	if i.Hooks == nil {
		return args, nil
	}
	call := HookContext{Tool: name, Args: args}
	if i.Context != nil {
		call = i.Context(ctx)
		call.Tool = name
		call.Args = args
	}
	result, err := i.Hooks.BeforeTool(ctx, call)
	if err != nil {
		return args, err
	}
	if result.Block {
		return args, tool.NewExecutionError(tool.ToolCallRejected, errors.New(result.Reason))
	}
	return result.Args, nil
}

// AfterTool implements tool.Interceptor.
func (i ToolInterceptor) AfterTool(ctx context.Context, name string, args tool.Args, outcome tool.ExecutionOutcome) (tool.ExecutionOutcome, error) {
	if i.Hooks == nil {
		return outcome, nil
	}
	call := HookContext{Tool: name, Args: args, Result: &outcome}
	if i.Context != nil {
		call = i.Context(ctx)
		call.Tool = name
		call.Args = args
		call.Result = &outcome
	}
	return i.Hooks.AfterTool(ctx, call), nil
}
