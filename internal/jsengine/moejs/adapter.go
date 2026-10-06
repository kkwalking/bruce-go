// Package moejs adapts the moejs JavaScript engine to the engine-neutral
// jsengine interfaces.
//
// This package is the only place in Bruce that imports moejs. Nothing outside
// it names a moejs type, so an API change stops here and replacing the engine
// means writing one more adapter rather than rewriting the plugin system.
package moejs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	engine "github.com/Calcium-Ion/moejs"

	"bruce-go/internal/jsengine"
)

// EngineName is the value reported by Engine.Name().
const EngineName = "moejs"

// Engine implements jsengine.Engine on top of moejs.
type Engine struct{}

var _ jsengine.Engine = Engine{}

// Name implements jsengine.Engine.
func (Engine) Name() string { return EngineName }

// Compile implements jsengine.Engine.
func (Engine) Compile(name, source string) (jsengine.Module, error) {
	module, err := engine.Compile(name, source)
	if err != nil {
		return nil, translate(err, nil)
	}
	return &moduleAdapter{module: module}, nil
}

// Link implements jsengine.Engine.
func (Engine) Link(entry jsengine.Module, resolve jsengine.Resolver) (jsengine.Module, error) {
	adapter, ok := entry.(*moduleAdapter)
	if !ok {
		return nil, &jsengine.EngineError{Kind: jsengine.KindLink, Message: "module was not produced by the moejs adapter"}
	}
	linked, err := engine.Link(adapter.module, func(referrer engine.Referrer, specifier string) (*engine.Module, error) {
		dependency, resolveErr := resolve(referrer.Name(), specifier)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if dependency == nil {
			return nil, fmt.Errorf("module %q is not available", specifier)
		}
		dependencyAdapter, ok := dependency.(*moduleAdapter)
		if !ok {
			return nil, fmt.Errorf("module %q was not produced by the moejs adapter", specifier)
		}
		return dependencyAdapter.module, nil
	})
	if err != nil {
		return nil, translate(err, nil)
	}
	return &moduleAdapter{module: linked, linked: true}, nil
}

// NewRuntime implements jsengine.Engine.
func (Engine) NewRuntime(opts jsengine.RuntimeOptions) (jsengine.Runtime, error) {
	options := engine.Options{
		DisableDynamicCode: opts.DisableDynamicCode,
		MaxDynamicSource:   opts.MaxDynamicSource,
	}
	if opts.Importer != nil {
		importer := opts.Importer
		options.Importer = &engine.Importer{Resolve: func(referrer engine.Referrer, specifier string) (*engine.Module, error) {
			name := ""
			if referrer != nil {
				name = referrer.Name()
			}
			dependency, err := importer(name, specifier)
			if err != nil {
				return nil, err
			}
			if dependency == nil {
				return nil, fmt.Errorf("module %q is not available", specifier)
			}
			adapter, ok := dependency.(*moduleAdapter)
			if !ok {
				return nil, fmt.Errorf("module %q was not produced by the moejs adapter", specifier)
			}
			return adapter.module, nil
		}}
	}
	return &runtimeAdapter{runtime: engine.NewRuntime(options), globals: opts.Globals}, nil
}

// moduleAdapter implements jsengine.Module.
type moduleAdapter struct {
	module *engine.Module
	// linked records that Link produced this module. The engine refuses to
	// load a module that has imports and was never linked, and reporting that
	// as a link failure (rather than passing the engine's bare error through)
	// keeps the classification engine-independent.
	linked bool
}

func (m *moduleAdapter) Name() string      { return m.module.Name() }
func (m *moduleAdapter) Exports() []string { return m.module.Exports() }
func (m *moduleAdapter) Requests() []string {
	return m.module.Requests()
}

func (m *moduleAdapter) Handler(path string) (jsengine.Handler, error) {
	export, members := splitHandlerPath(path)
	if export == "" {
		return nil, &jsengine.EngineError{Kind: jsengine.KindMissingHandler, Message: "handler path must not be empty"}
	}
	hook, err := m.module.Hook(export, members...)
	if err != nil {
		return nil, &jsengine.EngineError{
			Kind:    jsengine.KindMissingHandler,
			Message: fmt.Sprintf("module %q has no exported function %q", m.module.Name(), path),
			Cause:   err,
		}
	}
	return handlerAdapter{hook: hook}, nil
}

// splitHandlerPath turns "tools.read" into ("tools", ["read"]).
func splitHandlerPath(path string) (string, []string) {
	parts := strings.Split(strings.TrimSpace(path), ".")
	if len(parts) == 0 {
		return "", nil
	}
	return parts[0], parts[1:]
}

// handlerAdapter implements jsengine.Handler.
type handlerAdapter struct {
	hook engine.Hook
}

func (h handlerAdapter) Name() string { return h.hook.Name() }

// runtimeAdapter implements jsengine.Runtime.
type runtimeAdapter struct {
	runtime *engine.Runtime
	closed  bool
	// ctx is the context in effect while a host function runs. Host functions
	// are synchronous calls out of JavaScript, so the context of the call that
	// entered JavaScript is the right one to propagate: it is what carries a
	// user's Ctrl-C or a tool timeout into a host capability.
	ctx context.Context
	// globals are installed before the module's top level runs.
	globals map[string]jsengine.GlobalObject
}

// hostFunction adapts a jsengine.HostFunc to a moejs native function.
//
// Errors cross the boundary as thrown JavaScript Errors, so a plugin can
// catch a permission denial; the Go error stays attached, so the host can
// still classify it after the exception unwinds.
func (r *runtimeAdapter) hostFunction(name string, fn jsengine.HostFunc) engine.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		ctx := r.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		var raw json.RawMessage
		if len(args) > 0 {
			encoded, err := r.runtime.AppendJSON(nil, args[0])
			if err != nil {
				return engine.Undefined(), err
			}
			raw = json.RawMessage(encoded)
		}
		if len(bytesTrimSpace(raw)) == 0 {
			raw = json.RawMessage("{}")
		}
		out, err := fn(ctx, raw)
		if err != nil {
			return engine.Undefined(), fmt.Errorf("%s: %w", name, err)
		}
		if len(bytesTrimSpace(out)) == 0 {
			return engine.Undefined(), nil
		}
		value, err := r.runtime.ParseJSON(out)
		if err != nil {
			return engine.Undefined(), err
		}
		return value, nil
	}
}

func bytesTrimSpace(raw json.RawMessage) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}

// convertGlobal turns a JSON-shaped host object into a value moejs accepts,
// replacing HostFunc leaves with native functions.
func (r *runtimeAdapter) convertGlobal(value any) (any, error) {
	switch typed := value.(type) {
	case jsengine.HostFunc:
		return engine.NativeFunc(r.hostFunction("host", typed)), nil
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			converted, err := r.convertGlobal(item)
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			converted, err := r.convertGlobal(item)
			if err != nil {
				return nil, err
			}
			out[i] = converted
		}
		return out, nil
	case nil, bool, string, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		return typed, nil
	default:
		return nil, fmt.Errorf("host global value of type %T is not JSON-shaped", value)
	}
}

// SetGlobal implements jsengine.Runtime.
func (r *runtimeAdapter) SetGlobal(name string, value jsengine.GlobalObject) error {
	converted, err := r.convertGlobal(map[string]any(value))
	if err != nil {
		return &jsengine.EngineError{Kind: jsengine.KindInternal, Message: err.Error(), Cause: err}
	}
	if err := r.runtime.SetGlobal(name, converted); err != nil {
		return translate(err, r.runtime)
	}
	return nil
}

func (r *runtimeAdapter) Load(ctx context.Context, m jsengine.Module) error {
	adapter, ok := m.(*moduleAdapter)
	if !ok {
		return &jsengine.EngineError{Kind: jsengine.KindLink, Message: "module was not produced by the moejs adapter"}
	}
	if !adapter.linked {
		if requests := adapter.module.Requests(); len(requests) > 0 {
			return &jsengine.EngineError{
				Kind:    jsengine.KindLink,
				Message: fmt.Sprintf("module %q imports %q but was never linked", adapter.module.Name(), requests[0]),
			}
		}
	}
	// Loading is compilation of the module graph plus evaluation of its top
	// level, and the top level can loop forever. The context must be able to
	// stop it, exactly as it stops a call.
	return r.run(ctx, func() error {
		for name, value := range r.globals {
			if err := r.SetGlobal(name, value); err != nil {
				return err
			}
		}
		return r.runtime.Load(adapter.module)
	})
}

func (r *runtimeAdapter) Call(ctx context.Context, h jsengine.Handler, args json.RawMessage) (json.RawMessage, error) {
	adapter, ok := h.(handlerAdapter)
	if !ok {
		return nil, &jsengine.EngineError{Kind: jsengine.KindMissingHandler, Message: "handler was not produced by the moejs adapter"}
	}
	var out json.RawMessage
	err := r.run(ctx, func() error {
		// ParseJSON rather than FromGo: the arguments already are JSON, and
		// going through JSON.parse keeps the standard data model intact
		// (nested objects, arrays, booleans, numbers, null) with no
		// intermediate Go representation to lose type information.
		argument, err := r.runtime.ParseJSON(normalizeJSONObject(args))
		if err != nil {
			return err
		}
		result, err := r.runtime.Call(adapter.hook, argument)
		if err != nil {
			return err
		}
		// An async handler returns a promise. The engine runs every job the
		// call queued before returning, so a promise that can settle already
		// has; reading its state distinguishes a real result from a promise
		// that never settles. Serializing a pending promise directly would
		// produce "{}", which the model would read as a successful empty
		// result for work that never happened.
		if state, settled, ok := engine.PromiseResult(result); ok {
			switch state {
			case engine.PromiseFulfilled:
				result = settled
			case engine.PromiseRejected:
				reason := ""
				if encoded, encodeErr := r.runtime.AppendJSON(nil, settled); encodeErr == nil {
					reason = string(encoded)
				}
				return &jsengine.EngineError{
					Kind:    jsengine.KindException,
					Message: "the handler's promise was rejected: " + reason,
				}
			default:
				return &jsengine.EngineError{
					Kind: jsengine.KindResource,
					Message: "the handler returned a promise that never settled; " +
						"an async handler must resolve, and a promise the host cannot settle " +
						"(such as one awaiting a timer) is not supported",
				}
			}
		}
		encoded, err := r.runtime.AppendJSON(nil, result)
		if err != nil {
			return err
		}
		out = json.RawMessage(encoded)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// normalizeJSONObject turns absent or empty arguments into an empty object, so
// a handler always receives an object rather than undefined.
func normalizeJSONObject(args json.RawMessage) []byte {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" || trimmed == "null" {
		return []byte("{}")
	}
	return []byte(trimmed)
}

// run executes fn while bridging ctx cancellation into engine.Interrupt.
//
// The watcher goroutine is always joined before returning, and the pending
// interrupt is always cleared, so a runtime is never returned to the pool
// carrying an interrupt that would stop an unrelated later call.
func (r *runtimeAdapter) run(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	previous := r.ctx
	r.ctx = ctx
	defer func() { r.ctx = previous }()
	finished := make(chan struct{})
	release := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			r.runtime.Interrupt(ctx.Err())
		case <-release:
		}
	}()
	err := fn()
	close(release)
	<-finished
	r.runtime.ClearInterrupt()
	if err != nil && isInterrupted(err) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	return translate(err, r.runtime)
}

// ValidateHandler implements jsengine.Runtime.
func (r *runtimeAdapter) ValidateHandler(ctx context.Context, h jsengine.Handler) error {
	adapter, ok := h.(handlerAdapter)
	if !ok {
		return &jsengine.EngineError{Kind: jsengine.KindMissingHandler, Message: "handler was not produced by the moejs adapter"}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ok, err := r.runtime.Has(adapter.hook)
	if err != nil {
		return translate(err, r.runtime)
	}
	if !ok {
		return &jsengine.EngineError{
			Kind:    jsengine.KindMissingHandler,
			Message: fmt.Sprintf("handler %q does not name a function in this runtime", adapter.hook.Name()),
		}
	}
	return nil
}

func (r *runtimeAdapter) ReleaseCallData() { r.runtime.ReleaseCallData() }
func (r *runtimeAdapter) Interrupt(reason any) {
	r.runtime.Interrupt(reason)
}
func (r *runtimeAdapter) ClearInterrupt() { r.runtime.ClearInterrupt() }

func (r *runtimeAdapter) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return nil
}

// translate converts an engine error into the engine-neutral form.
//
// It runs in exactly one place so that no other package needs to know the
// engine's error taxonomy.
func translate(err error, runtime *engine.Runtime) error {
	if err == nil {
		return nil
	}
	var already *jsengine.EngineError
	if errors.As(err, &already) {
		return err
	}
	// A context error surfaced by the interrupt bridge is not an engine error.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var syntax *engine.SyntaxError
	if errors.As(err, &syntax) {
		return &jsengine.EngineError{
			Kind: jsengine.KindSyntax, Message: syntax.Message,
			File: syntax.File, Line: syntax.Line, Column: syntax.Column, Cause: err,
		}
	}
	var resolve *engine.ResolveError
	if errors.As(err, &resolve) {
		return &jsengine.EngineError{
			Kind:    jsengine.KindLink,
			Message: fmt.Sprintf("cannot resolve module %q: %s", resolve.Specifier, resolve.Err),
			File:    resolve.File, Line: resolve.Line, Column: resolve.Column, Cause: err,
		}
	}
	var internal *engine.InternalError
	if errors.As(err, &internal) {
		return &jsengine.EngineError{Kind: jsengine.KindInternal, Message: internal.Error(), Cause: err}
	}
	var interrupted *engine.InterruptedError
	if errors.As(err, &interrupted) {
		return &jsengine.EngineError{Kind: jsengine.KindInterrupted, Message: interrupted.Error(), Cause: err}
	}
	if errors.Is(err, engine.ErrHookNotFound) || errors.Is(err, engine.ErrNotCallable) {
		return &jsengine.EngineError{Kind: jsengine.KindMissingHandler, Message: err.Error(), Cause: err}
	}
	var exception *engine.Exception
	if errors.As(err, &exception) {
		stack := ""
		if runtime != nil {
			stack = runtime.StackTrace(exception)
		}
		message := strings.TrimSpace(exception.Name() + ": " + exception.Message())
		return &jsengine.EngineError{Kind: jsengine.KindException, Message: message, Stack: stack, Cause: err}
	}
	// A RangeError for excessive nesting or an oversized dynamic source is a
	// resource limit, not a plugin bug.
	message := err.Error()
	if strings.Contains(message, "RangeError") || strings.Contains(message, "maximum") && strings.Contains(message, "length") {
		return &jsengine.EngineError{Kind: jsengine.KindResource, Message: message, Cause: err}
	}
	return &jsengine.EngineError{Kind: jsengine.KindUnknown, Message: message, Cause: err}
}

func isInterrupted(err error) bool {
	var interrupted *engine.InterruptedError
	if errors.As(err, &interrupted) {
		return true
	}
	kind, ok := jsengine.KindOf(err)
	return ok && kind == jsengine.KindInterrupted
}
