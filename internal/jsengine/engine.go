// Package jsengine is the JavaScript engine abstraction the plugin system is
// built on.
//
// Bruce's plugin core depends only on this package. The concrete engine lives
// behind an adapter (internal/jsengine/moejs), so a change to the engine's API
// stops at that adapter and swapping engines does not touch the plugin core,
// the tool registry, the CLI or the sandbox.
//
// The boundary type is JSON: modules go in as source text, calls go in and out
// as json.RawMessage. That is deliberate. The standard JSON data model is
// exactly what a tool argument object is, so nested objects, arrays, booleans,
// numbers and null cross the boundary without stringification or flattening,
// and no engine value can be smuggled across runtimes.
package jsengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrorKind classifies a failure reported by an engine, so callers can map it
// onto their own error categories without inspecting engine-specific types.
type ErrorKind int

const (
	KindUnknown ErrorKind = iota
	// KindSyntax is a parse or early error in module source.
	KindSyntax
	// KindLink is a failure to resolve or link an import.
	KindLink
	// KindMissingHandler is a handler path that names nothing callable.
	KindMissingHandler
	// KindException is a JavaScript throw.
	KindException
	// KindInterrupted is a call stopped by Interrupt.
	KindInterrupted
	// KindInternal is a Go panic that escaped the engine, or an engine bug.
	// The runtime's call state is restored but it must not be reused.
	KindInternal
	// KindResource is a limit violation such as excessive nesting or an
	// oversized dynamic source.
	KindResource
)

func (k ErrorKind) String() string {
	switch k {
	case KindSyntax:
		return "syntax"
	case KindLink:
		return "link"
	case KindMissingHandler:
		return "missing_handler"
	case KindException:
		return "exception"
	case KindInterrupted:
		return "interrupted"
	case KindInternal:
		return "internal"
	case KindResource:
		return "resource"
	default:
		return "unknown"
	}
}

// EngineError is the engine-neutral error returned by every Engine and Runtime
// method. An adapter translates its engine's errors into this type once.
type EngineError struct {
	Kind    ErrorKind
	Message string
	// File, Line and Column locate a syntax or link error. Line and Column are
	// 1-based and zero when unknown.
	File   string
	Line   int
	Column int
	// Stack is the engine's stack trace for a thrown JavaScript error.
	Stack string
	// Cause is the engine's original error, for logging only. Callers must not
	// depend on its type.
	Cause error
}

func (e *EngineError) Error() string {
	location := ""
	if e.File != "" {
		location = e.File
		if e.Line > 0 {
			location += fmt.Sprintf(":%d", e.Line)
			if e.Column > 0 {
				location += fmt.Sprintf(":%d", e.Column)
			}
		}
		location += ": "
	}
	return location + e.Kind.String() + " error: " + e.Message
}

func (e *EngineError) Unwrap() error { return e.Cause }

// KindOf returns the ErrorKind of the first *EngineError in err's chain.
func KindOf(err error) (ErrorKind, bool) {
	var target *EngineError
	if errors.As(err, &target) {
		return target.Kind, true
	}
	return KindUnknown, false
}

// IsInterrupted reports whether err is an engine interrupt.
func IsInterrupted(err error) bool {
	kind, ok := KindOf(err)
	return ok && kind == KindInterrupted
}

// IsCorrupted reports whether err leaves the runtime unsafe to reuse.
//
// A Go panic that unwound through the engine restores the call bookkeeping but
// may have left the runtime's internal state inconsistent, so the pool must
// discard the runtime instead of returning it to the idle list.
func IsCorrupted(err error) bool {
	kind, ok := KindOf(err)
	return ok && kind == KindInternal
}

// Handler is a resolved path to a function exported by a module. It is
// resolved once, at compile time, which is what makes "missing handler" a load
// failure rather than a per-call surprise.
type Handler interface {
	// Name returns the dotted path, for example "tools.read".
	Name() string
}

// Module is a compiled JavaScript module.
//
// A Module is immutable: any number of runtimes may load it, concurrently, and
// compiling a module once and sharing it is the intended lifecycle.
type Module interface {
	// Name returns the name the module was compiled with.
	Name() string
	// Exports returns the module's export names, sorted.
	Exports() []string
	// Requests returns the static import specifiers, in source order.
	Requests() []string
	// Handler resolves a dotted export path such as "run" or "tools.read".
	Handler(path string) (Handler, error)
}

// Resolver returns the module a specifier names, as seen from referrer.
//
// The engine never reads files or the network: the host decides what a
// specifier means. Returning an error is how an import is refused.
type Resolver func(referrer, specifier string) (Module, error)

// HostFunc is a host function a plugin may call from JavaScript.
//
// Arguments and the result are JSON. That is the entire host capability
// surface: a plugin never receives a Go value, a Go object reference, an
// os.File, an http.Client or anything else it could use to step outside the
// host's policy. Everything a plugin can do is a function the host chose to
// install.
type HostFunc func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)

// GlobalObject is a JSON-shaped tree installed as a JavaScript global. Leaves
// may be HostFunc values, which become callable host functions; everything
// else follows the JSON data model.
type GlobalObject = map[string]any

// RuntimeOptions configures a new runtime.
type RuntimeOptions struct {
	// DisableDynamicCode makes eval, the Function constructors and any
	// equivalent engine API throw instead of compiling. The plugin system
	// turns this on by default: a plugin has no reason to generate code, and
	// generated code is the classic way out of a sandbox.
	DisableDynamicCode bool
	// Importer resolves dynamic import() and import.meta. Without it, import()
	// rejects.
	Importer Resolver
	// MaxDynamicSource caps the length in bytes of source that dynamic code
	// may compile. Zero means the engine default; negative removes the cap.
	MaxDynamicSource int
	// Globals are host objects installed into every runtime before the module
	// is loaded, for example {"bruce": {"storage": {...}, "log": ...}}.
	Globals map[string]GlobalObject
}

// Runtime is one JavaScript global environment with at most one loaded module.
//
// A Runtime must be used by one goroutine at a time. Interrupt and
// ClearInterrupt are the only methods safe to call concurrently, which is what
// lets a context cancellation stop a running plugin.
type Runtime interface {
	// Load evaluates the module's top level in this runtime.
	Load(ctx context.Context, m Module) error
	// SetGlobal installs one host object as a JavaScript global. Leaves that
	// are HostFunc become callable; the rest follow the JSON data model.
	SetGlobal(name string, value GlobalObject) error
	// ValidateHandler reports whether a handler names a callable in this
	// runtime, without calling it.
	//
	// A manifest declares handlers by name, and a name that leads nowhere is a
	// plugin author's typo, not a runtime condition. Checking it right after
	// Load turns "missing handler" into a load failure that is reported once,
	// at startup, instead of a per-invocation surprise.
	ValidateHandler(ctx context.Context, h Handler) error
	// Call invokes a handler with JSON arguments and returns the JSON result.
	Call(ctx context.Context, h Handler, args json.RawMessage) (json.RawMessage, error)
	// ReleaseCallData drops the runtime's references to the finished call's
	// host values, so a pooled runtime does not keep the last request alive.
	ReleaseCallData()
	// Interrupt stops running code. Any goroutine may call it.
	Interrupt(reason any)
	// ClearInterrupt drops a pending interrupt. It must be called before a
	// runtime returns to the pool: an interrupt that arrived while nothing ran
	// would otherwise stop the next call on that runtime.
	ClearInterrupt()
	// Close releases the runtime.
	Close() error
}

// Engine compiles and links modules and creates runtimes.
type Engine interface {
	// Name identifies the engine, for status output and diagnostics.
	Name() string
	// Compile parses and compiles one module. Compilation is not affected by
	// RuntimeOptions: the host, not a plugin, decides what source to compile.
	Compile(name, source string) (Module, error)
	// Link resolves the entry's imports, directly or indirectly, and returns
	// the linked module. A module with no imports is returned unchanged.
	Link(entry Module, resolve Resolver) (Module, error)
	// NewRuntime creates a runtime.
	NewRuntime(opts RuntimeOptions) (Runtime, error)
}

// CallWithContext invokes a handler and bridges context cancellation into the
// engine's interrupt mechanism.
//
// This is the whole of Bruce's cancellation contract for JavaScript: a
// cancelled context, a tool timeout, a session cancel, an agent cancellation
// and process shutdown all arrive here as ctx.Done() and stop the running
// script. The watcher goroutine always terminates, so a call cannot leak one.
//
// A successful call that raced with cancellation stays successful: the caller
// (the tool executor) performs its own post-execution context check, and
// rewriting a completed result into a failure would lose real output.
func CallWithContext(ctx context.Context, rt Runtime, h Handler, args json.RawMessage) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finished := make(chan struct{})
	release := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			rt.Interrupt(ctx.Err())
		case <-release:
		}
	}()
	out, err := rt.Call(ctx, h, args)
	close(release)
	// Wait for the watcher before clearing: an interrupt that lands after
	// ClearInterrupt would poison the runtime's next call.
	<-finished
	rt.ClearInterrupt()
	if err != nil && IsInterrupted(err) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	return out, err
}
