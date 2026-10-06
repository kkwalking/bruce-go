package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"bruce-go/internal/cli"
	"bruce-go/internal/jsengine"
	"bruce-go/internal/sandbox"
	"bruce-go/internal/tool"
)

// DefaultInvocationTimeout bounds one plugin call when neither the manifest nor
// the tool declaration sets a shorter one. A plugin that hangs must not hang
// the agent, and the caller's context is still the outer bound.
const DefaultInvocationTimeout = 30 * time.Second

// MaxInvocationTimeout bounds what a manifest may request.
const MaxInvocationTimeout = 5 * time.Minute

// Observer is notified of plugin lifecycle and invocation events.
//
// It is the observability seam: the plugin system publishes into Bruce's
// existing event bus through this interface rather than depending on it, so the
// core stays testable and the bus stays optional.
type Observer interface {
	PluginEvent(kind string, fields map[string]any)
}

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(kind string, fields map[string]any)

func (f ObserverFunc) PluginEvent(kind string, fields map[string]any) { f(kind, fields) }

// Event kinds published through Observer.
const (
	EventLoaded          = "plugin.loaded"
	EventLoadFailed      = "plugin.load_failed"
	EventUnloaded        = "plugin.unloaded"
	EventReloaded        = "plugin.reloaded"
	EventInvokeStart     = "plugin.invoke_started"
	EventInvokeDone      = "plugin.invoke_completed"
	EventInvokeFailed    = "plugin.invoke_failed"
	EventHookInvoked     = "plugin.hook_invoked"
	EventTimeout         = "plugin.timeout"
	EventCancellation    = "plugin.cancellation"
	EventDenied          = "plugin.permission_denied"
	EventRuntimeRecycled = "plugin.runtime_discarded"
)

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	// Engine compiles and runs JavaScript. Required.
	Engine jsengine.Engine
	// Workspace and HomeDir locate the plugin search roots.
	Workspace string
	HomeDir   string
	// Policy is the final authority on plugin permissions.
	Policy HostPolicy
	// FailFast makes a broken manifest stop startup instead of producing a
	// diagnostic. Off by default: a third-party plugin must not stop Bruce.
	FailFast bool
	// Sandbox reports the current sandbox status, so capability checks can
	// fail closed when the backend cannot enforce them.
	Sandbox func() SandboxStatus
	// Storage backs bruce:storage.
	Storage *MemoryStorage
	// Observer receives lifecycle and invocation events.
	Observer Observer
	// DisableDynamicCode turns eval and the Function constructor off. Leaving
	// it nil means "off", so a host that does not think about it still gets the
	// safe behaviour; set it to a pointer to false only for a trusted-plugin
	// deployment, where a plugin compiling code at runtime is acceptable.
	DisableDynamicCode *bool
	// Now is the clock, for tests.
	Now func() time.Time
	// ExtraGlobals installs additional host objects into every plugin runtime.
	// It is how a host adds a capability beyond the built-in bruce namespace,
	// and how a test supplies a capability without a real filesystem.
	ExtraGlobals map[string]jsengine.GlobalObject
}

// SandboxStatus is the subset of sandbox.Status the plugin system needs.
//
// It is declared here rather than imported so the plugin core does not depend
// on the sandbox package's full surface, and so a test can describe a sandbox
// without constructing one.
type SandboxStatus struct {
	Mode          string
	NetworkAccess bool
	Available     bool
	Backend       string
	Reason        string
	Generation    uint64
}

// Manager owns the plugin lifecycle: discovery, load, invocation, reload and
// unload.
//
// One Manager owns every generation of every plugin. A reload never mutates a
// loaded plugin in place: it builds a new generation and swaps it in, so an
// invocation already in flight finishes on the code it started with while the
// next invocation uses the new code. That is the deterministic reload policy
// the plugin contract requires, and it is why generations exist at all.
type Manager struct {
	engine       jsengine.Engine
	workspace    string
	home         string
	policy       HostPolicy
	failFast     bool
	sandbox      func() SandboxStatus
	storage      *MemoryStorage
	observer     Observer
	dynamic      bool
	now          func() time.Time
	extraGlobals map[string]jsengine.GlobalObject

	mu      sync.RWMutex
	plugins map[string]*loadedPlugin
	// generations is a monotonic counter per plugin name. It outlives unload,
	// so a reload always produces a strictly newer generation than any
	// invocation that may still be running on the old code.
	generations map[string]uint64
	order       []string
	diags       []Diagnostic
	overrides   []string
	toolNames   map[string]string // tool name -> owning plugin
	cmdNames    map[string]string // command name -> owning plugin
	closed      bool

	// tools is the registry plugin tools are registered into. It is optional:
	// a Manager without a registry still loads plugins and runs hooks.
	tools *tool.Registry
	// hooks owns hook registration. It is created with the manager, not on
	// first use: a plugin loaded before the host asked for the hook manager
	// would otherwise have its hooks silently dropped.
	hooks *HookManager
	// commands is the slash command registry. Plugin commands are registered
	// into it so the CLI has one dispatch path instead of a switch that grows
	// a case per plugin.
	commands *cli.Registry
	// commandHandlers holds the Go side of each published plugin command.
	commandHandlers map[string]CommandHandler
	// commandConflicts records rejected command registrations.
	commandConflicts []string
}

// WithCommandRegistry attaches a command registry and publishes the commands
// of every already-loaded plugin into it.
func (m *Manager) WithCommandRegistry(registry *cli.Registry) *Manager {
	if registry == nil {
		return m
	}
	m.mu.Lock()
	m.commands = registry
	type pending struct {
		name     string
		manifest *Manifest
	}
	items := make([]pending, 0, len(m.order))
	for _, name := range m.order {
		if plugin, ok := m.plugins[name]; ok {
			items = append(items, pending{name: name, manifest: plugin.manifest})
		}
	}
	m.mu.Unlock()
	for _, item := range items {
		m.publishCommands(item.name, item.manifest)
	}
	return m
}

// Commands returns the attached command registry, or nil.
func (m *Manager) Commands() *cli.Registry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.commands
}

// publishCommands registers a plugin's commands and records the conflicts.
func (m *Manager) publishCommands(pluginName string, manifest *Manifest) {
	m.mu.RLock()
	registry := m.commands
	m.mu.RUnlock()
	if registry == nil {
		return
	}
	conflicts := m.PublishCommands(registry, pluginName, manifest)
	if len(conflicts) == 0 {
		return
	}
	m.mu.Lock()
	m.commandConflicts = append(m.commandConflicts, conflicts...)
	m.mu.Unlock()
}

// CommandConflicts returns the rejected command registrations, sorted.
func (m *Manager) CommandConflicts() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]string(nil), m.commandConflicts...)
	sort.Strings(out)
	return out
}

// Hooks returns the hook manager for this plugin manager.
//
// It shares the manager's lifecycle: loading a plugin registers its hooks, and
// unloading it removes them.
func (m *Manager) Hooks() *HookManager { return m.hooks }

// loadedPlugin is one plugin at one generation.
type loadedPlugin struct {
	manifest *Manifest
	// generation increases on every reload, so a stale invocation can be
	// recognised and a stale pool never serves a new call.
	generation uint64
	resolver   *Resolver
	pool       *jsengine.PooledModule
	grant      Grant
	handlers   map[string]jsengine.Handler
	loadedAt   time.Time
}

// NewManager creates a manager. Call Load to discover and load plugins.
func NewManager(opts ManagerOptions) (*Manager, error) {
	if opts.Engine == nil {
		return nil, errors.New("plugin: manager requires a JavaScript engine")
	}
	// Dynamic code is off unless the host explicitly turns it on.
	//
	// The safe default matters more than the convenience here: eval and the
	// Function constructor are the classic way out of a JavaScript sandbox, so
	// a host that forgets to set this option must still be safe. The previous
	// default was inverted, which silently left eval enabled for every caller
	// that did not pass the option.
	dynamic := false
	if opts.DisableDynamicCode != nil {
		dynamic = !*opts.DisableDynamicCode
	}
	storage := opts.Storage
	if storage == nil {
		storage = NewMemoryStorage()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	manager := &Manager{
		engine:          opts.Engine,
		workspace:       opts.Workspace,
		home:            opts.HomeDir,
		policy:          opts.Policy,
		failFast:        opts.FailFast,
		sandbox:         opts.Sandbox,
		storage:         storage,
		observer:        opts.Observer,
		dynamic:         dynamic,
		now:             now,
		extraGlobals:    opts.ExtraGlobals,
		plugins:         map[string]*loadedPlugin{},
		generations:     map[string]uint64{},
		toolNames:       map[string]string{},
		cmdNames:        map[string]string{},
		commandHandlers: map[string]CommandHandler{},
	}
	manager.hooks = NewHookManager(manager)
	return manager, nil
}

// WithRegistry attaches a tool registry. Plugin tools are registered into it,
// which is what makes a plugin tool indistinguishable from any other tool to
// the agent.
func (m *Manager) WithRegistry(registry *tool.Registry) *Manager {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = registry
	return m
}

// WorkspacePluginsDir returns the workspace-level plugin directory.
func (m *Manager) WorkspacePluginsDir() string {
	return filepath.Join(m.workspace, ".bruce", "plugins")
}

// UserPluginsDir returns the user-level plugin directory.
func (m *Manager) UserPluginsDir() string {
	return filepath.Join(m.home, ".bruce", "plugins")
}

// Load discovers and loads every plugin, replacing whatever was loaded.
//
// Loading is best effort: a plugin that fails to load produces a diagnostic and
// every other plugin still loads. Bruce never fails to start because of a
// plugin, unless FailFast was requested.
func (m *Manager) Load(ctx context.Context) error {
	result, err := Discovery{
		UserRoot:      m.UserPluginsDir(),
		WorkspaceRoot: m.WorkspacePluginsDir(),
		FailFast:      m.failFast,
	}.Load()
	if err != nil {
		return err
	}

	// Unload the previous generation before loading the new one, so a reload
	// cannot leave a duplicate tool, hook or command behind.
	m.unloadAll()

	diagnostics := append([]Diagnostic(nil), result.Diagnostics...)
	for _, manifest := range result.Manifests {
		if err := m.loadOne(ctx, manifest); err != nil {
			diagnostics = append(diagnostics, Diagnostic{
				Path:    manifest.File,
				Plugin:  manifest.Name,
				Message: Redact(err.Error()),
			})
			m.emit(EventLoadFailed, map[string]any{
				"plugin": manifest.Name, "path": manifest.File, "error": Redact(err.Error()),
			})
		}
	}

	m.mu.Lock()
	m.diags = diagnostics
	m.overrides = append([]string(nil), result.Overrides...)
	m.mu.Unlock()
	return nil
}

// loadOne compiles, links, validates and registers one plugin.
func (m *Manager) loadOne(ctx context.Context, manifest *Manifest) error {
	fail := func(stage Stage, category ErrorCategory, handler string, err error) error {
		return NewError(manifest.Name, manifest.File, handler, stage, category, err)
	}

	resolver, err := NewResolver(ResolverOptions{
		Engine:  m.engine,
		RootDir: manifest.RootDir,
		Virtual: m.virtualModules(manifest),
	})
	if err != nil {
		return fail(StageCompile, CategoryInternal, "", err)
	}

	entryPath := manifest.Entry
	if !filepath.IsAbs(entryPath) {
		entryPath = filepath.Join(manifest.RootDir, entryPath)
	}
	entry, err := resolver.Resolve("", "./"+filepath.ToSlash(relativeTo(manifest.RootDir, entryPath)))
	if err != nil {
		return fail(StageCompile, CategorySyntax, manifest.Entry, err)
	}
	linked, err := resolver.Link(entry)
	if err != nil {
		return fail(StageCompile, categoryForEngineError(err, CategoryLink), manifest.Entry, err)
	}

	// Every declared handler is resolved before the plugin is accepted, so a
	// typo in the manifest is a load failure reported once at startup rather
	// than a per-invocation surprise.
	handlers := map[string]jsengine.Handler{}
	for _, declaration := range allHandlerPaths(manifest) {
		handler, err := linked.Handler(declaration)
		if err != nil {
			return fail(StageCompile, CategoryMissingHandler, declaration, err)
		}
		handlers[declaration] = handler
	}

	grant := m.policy.GrantFor(manifest.Name, permissionStrings(manifest.Permissions))
	capacity := manifest.Concurrency.MaxRuntimes
	pool, err := jsengine.NewPooledModule(m.engine, linked, jsengine.RuntimeOptions{
		DisableDynamicCode: !m.dynamic,
		Globals:            m.globals(manifest, grant),
		Importer:           resolver.Resolve,
	}, capacity)
	if err != nil {
		return fail(StageLoad, categoryForEngineError(err, CategoryInternal), "", err)
	}
	// A discarded runtime is reported as an event: silently replacing a
	// damaged runtime would hide the plugin bug that damaged it.
	poolName := manifest.Name
	pool.OnDiscard(func(reason string) {
		m.emit(EventRuntimeRecycled, map[string]any{"plugin": poolName, "reason": reason})
	})

	// A warm runtime proves the module actually evaluates and that every
	// handler really is callable. It is discarded afterwards; the pool keeps
	// the runtimes that were validated.
	if err := m.validateLoad(ctx, manifest, pool, handlers); err != nil {
		_ = pool.Close()
		return err
	}

	generation := m.nextGeneration(manifest.Name)
	plugin := &loadedPlugin{
		manifest:   manifest,
		generation: generation,
		resolver:   resolver,
		pool:       pool,
		grant:      grant,
		handlers:   handlers,
		loadedAt:   m.now(),
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = pool.Close()
		return errors.New("plugin: manager is closed")
	}
	if _, exists := m.plugins[manifest.Name]; exists {
		m.mu.Unlock()
		_ = pool.Close()
		return fail(StageDiscover, CategoryConflict, "", errors.New("a plugin with this name is already loaded"))
	}
	m.plugins[manifest.Name] = plugin
	m.order = append(m.order, manifest.Name)
	m.mu.Unlock()

	if err := m.registerTools(plugin); err != nil {
		m.removePlugin(manifest.Name)
		_ = pool.Close()
		return err
	}
	m.hooks.Register(manifest.Name, manifest)
	m.publishCommands(manifest.Name, manifest)

	m.emit(EventLoaded, map[string]any{
		"plugin": manifest.Name, "version": manifest.Version, "source": string(manifest.Source),
		"tools": len(manifest.Tools), "hooks": len(manifest.Hooks), "commands": len(manifest.Commands),
		"permissions": grant.Summary(), "generation": generation,
	})
	return nil
}

// validateLoad evaluates the module once and checks every handler resolves in
// a real runtime.
func (m *Manager) validateLoad(ctx context.Context, manifest *Manifest, pool *jsengine.PooledModule, handlers map[string]jsengine.Handler) error {
	runtime, err := pool.Acquire(ctx)
	if err != nil {
		return NewError(manifest.Name, manifest.File, "", StageLoad, categoryForEngineError(err, CategoryInternal), err)
	}
	defer pool.Release(runtime, false)
	names := make([]string, 0, len(handlers))
	for name := range handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := runtime.ValidateHandler(ctx, handlers[name]); err != nil {
			return NewError(manifest.Name, manifest.File, name, StageLoad, CategoryMissingHandler, err)
		}
	}
	return nil
}

// registerTools publishes the plugin's tools into the attached registry.
func (m *Manager) registerTools(plugin *loadedPlugin) error {
	m.mu.Lock()
	registry := m.tools
	m.mu.Unlock()
	if registry == nil {
		return nil
	}
	for _, declaration := range plugin.manifest.Tools {
		capability := capabilityFor(plugin.manifest, declaration)
		if err := plugin.grant.CheckCapability(tool.Policy{Capability: capability}); err != nil {
			return NewError(plugin.manifest.Name, plugin.manifest.File, declaration.Name,
				StageLoad, CategoryPermission, err)
		}
		// A capability the sandbox cannot enforce is not granted at all: a
		// plugin must never believe it may read the filesystem when the host
		// cannot actually stop it from reading anything else.
		if allowed, reason := m.sandboxAllows(capability); !allowed {
			return NewError(plugin.manifest.Name, plugin.manifest.File, declaration.Name,
				StageLoad, CategoryPermission, errors.New(reason))
		}
		parallelSafe := plugin.manifest.Concurrency.ParallelSafe != nil && *plugin.manifest.Concurrency.ParallelSafe
		if declaration.ParallelSafe != nil {
			parallelSafe = *declaration.ParallelSafe
		}
		timeout := m.timeoutFor(declaration.TimeoutMS)
		policy := ToolPolicy(tool.SourcePlugin, capability, parallelSafe, riskFor(declaration.Risk), timeout, declaration.Description)

		name := declaration.Name
		handlerPath := declaration.Handler
		pluginName := plugin.manifest.Name
		schema := declaration.Schema
		registry.Register(tool.Tool{
			Name:              name,
			Description:       declaration.Description,
			Parameters:        schema,
			PromptSnippet:     promptSnippet(declaration),
			ValidateArguments: validateAgainstSchema(schema),
			Exec: func(ctx context.Context, args tool.Args) (string, error) {
				return m.invokeTool(ctx, pluginName, name, handlerPath, timeout, args)
			},
			Policy: policy,
		})
		m.mu.Lock()
		m.toolNames[name] = pluginName
		m.mu.Unlock()
	}
	return nil
}

// maxReloadRetries bounds how many times one invocation may be re-pointed at a
// newer generation. A reload that lands while a call is queued retries once; a
// host that reloads in a tight loop must not be able to keep a call spinning.
const maxReloadRetries = 3

// invokeTool runs one plugin tool handler.
//
// The plugin is looked up by name on every call rather than captured, so a
// reload swaps the code under a still-registered tool without leaving the old
// generation reachable.
//
// Reload policy, which docs/plugin.md section 19 requires to be deterministic:
// an invocation that already holds a runtime finishes on the code it started
// with, and an invocation that was still queued for a runtime is re-pointed at
// the new generation instead of failing. Closing the old pool is what makes the
// queued case visible -- its Acquire returns ErrPoolClosed -- so this loop is
// where the policy is actually implemented rather than merely documented.
func (m *Manager) invokeTool(ctx context.Context, pluginName, toolName, handlerPath string, timeout time.Duration, args tool.Args) (string, error) {
	payload, err := json.Marshal(args)
	if err != nil {
		return "", NewError(pluginName, "", handlerPath, StageInvoke, CategoryMalformedResult,
			errors.New("tool arguments are not JSON-serializable: "+err.Error()))
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", NewError(pluginName, "", handlerPath, StageInvoke, CategoryCancellation, err)
		}
		plugin, ok := m.plugin(pluginName)
		if !ok {
			return "", NewError(pluginName, "", handlerPath, StageInvoke, CategoryInternal,
				errors.New("plugin is not loaded"))
		}
		handler, ok := plugin.handlers[handlerPath]
		if !ok {
			return "", NewError(pluginName, plugin.manifest.File, handlerPath, StageInvoke, CategoryMissingHandler,
				errors.New("handler was not resolved at load time"))
		}
		generation := plugin.generation
		out, invokeErr := m.invoke(ctx, plugin, handlerPath, handler, timeout, payload)
		if invokeErr == nil {
			return resultText(out), nil
		}
		if !m.reloadSuperseded(pluginName, generation, invokeErr) || attempt >= maxReloadRetries {
			return "", invokeErr
		}
		// The plugin was reloaded while this call was waiting for a runtime, so
		// the old pool refused it. Re-point the call at the current generation.
		if !m.awaitReload(ctx, pluginName, generation) {
			return "", invokeErr
		}
		m.emit(EventReloaded, map[string]any{
			"plugin": pluginName, "handler": handlerPath, "generation": generation,
			"reason": "invocation was re-pointed at the new generation after a reload",
		})
	}
}

// reloadSuperseded reports whether an invocation failed only because a reload
// replaced the generation it was about to use.
//
// It is deliberately narrow: the error must be the closed old pool and the
// plugin's generation must have moved on. A closed pool with no reload (a
// manager shutting down) is a real failure and must not be retried.
//
// The check reads the monotonic generation counter rather than the loaded
// plugin, because a reload removes the plugin before loading its replacement.
// A call that was waiting for a runtime when the removal happened sees the
// plugin as absent for that instant; treating "absent" as "not superseded"
// would turn a routine reload into a failed tool call.
func (m *Manager) reloadSuperseded(pluginName string, generation uint64, err error) bool {
	if !errors.Is(err, jsengine.ErrPoolClosed) {
		return false
	}
	m.mu.RLock()
	current, tracking := m.generations[pluginName]
	_, loaded := m.plugins[pluginName]
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		// A closing manager is not a reload: the pool is closed for good.
		return false
	}
	if !tracking {
		return false
	}
	if current != generation {
		return true
	}
	// The counter has already moved past this generation but the plugin is
	// briefly unloaded mid-reload; the reload is still in progress.
	return !loaded
}

// awaitReload waits for an in-progress reload of a plugin to publish its new
// generation, so a superseded invocation can be re-pointed at it.
func (m *Manager) awaitReload(ctx context.Context, pluginName string, generation uint64) bool {
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, ok := m.plugin(pluginName); ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
	}
}

// invoke runs a handler with timeout, cancellation, observation and error
// classification, and returns the raw JSON result.
func (m *Manager) invoke(ctx context.Context, plugin *loadedPlugin, handlerName string, handler jsengine.Handler, timeout time.Duration, args []byte) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = DefaultInvocationTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := m.now()
	m.emit(EventInvokeStart, map[string]any{
		"plugin": plugin.manifest.Name, "handler": handlerName, "generation": plugin.generation,
	})
	out, err := plugin.pool.Invoke(callCtx, handler, args)
	duration := m.now().Sub(started)

	if err != nil {
		category := categoryForEngineError(err, CategoryInternal)
		kind := EventInvokeFailed
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			category = CategoryTimeout
			kind = EventTimeout
		case errors.Is(err, context.Canceled):
			category = CategoryCancellation
			kind = EventCancellation
		}
		pluginErr := NewError(plugin.manifest.Name, plugin.manifest.File, handlerName, StageInvoke, category, err)
		m.emit(kind, map[string]any{
			"plugin": plugin.manifest.Name, "handler": handlerName, "category": string(category),
			"duration_ms": duration.Milliseconds(), "error": Redact(pluginErr.Error()),
		})
		return nil, pluginErr
	}

	// A handler that returns something that is not JSON is a plugin bug, and
	// reporting it as a malformed result is more useful than passing invalid
	// JSON to the model.
	if len(out) > 0 && !json.Valid(out) {
		pluginErr := NewError(plugin.manifest.Name, plugin.manifest.File, handlerName, StageInvoke,
			CategoryMalformedResult, errors.New("handler returned a value that is not valid JSON"))
		m.emit(EventInvokeFailed, map[string]any{
			"plugin": plugin.manifest.Name, "handler": handlerName,
			"category": string(CategoryMalformedResult), "duration_ms": duration.Milliseconds(),
		})
		return nil, pluginErr
	}

	m.emit(EventInvokeDone, map[string]any{
		"plugin": plugin.manifest.Name, "handler": handlerName,
		"duration_ms": duration.Milliseconds(), "generation": plugin.generation,
	})
	return out, nil
}

// resultText renders a handler result for the agent.
//
// A string result is returned as its text, because that is what a tool result
// is; anything else keeps its JSON form so structure is not lost.
func resultText(out json.RawMessage) string {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(out, &asString); err == nil {
		return asString
	}
	var pretty any
	if err := json.Unmarshal(out, &pretty); err == nil {
		if encoded, err := json.MarshalIndent(pretty, "", "  "); err == nil {
			return string(encoded)
		}
	}
	return trimmed
}

// plugin returns the currently loaded generation of a plugin.
func (m *Manager) plugin(name string) (*loadedPlugin, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	plugin, ok := m.plugins[name]
	return plugin, ok
}

// nextGeneration allocates the next generation number for a plugin.
//
// The counter is per name and never resets, so generation N+1 always means
// "newer code than generation N" even across an unload.
func (m *Manager) nextGeneration(name string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generations[name]++
	return m.generations[name]
}

// Reload rebuilds one plugin from disk, or every plugin when name is empty.
//
// The old generation is unloaded and its pool closed, so no runtime, tool, hook
// or command from the previous generation survives. An invocation already in
// flight holds a runtime from the old pool and finishes on the code it started
// with; the pool's Close waits for it to be released rather than interrupting
// it.
func (m *Manager) Reload(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return m.Load(ctx)
	}
	plugin, ok := m.plugin(name)
	if !ok {
		return fmt.Errorf("plugin: unknown plugin %q", name)
	}
	manifest, err := m.reloadManifest(plugin.manifest)
	if err != nil {
		return NewError(name, plugin.manifest.File, "", StageReload, CategoryManifest, err)
	}
	if manifest.Name != plugin.manifest.Name {
		return NewError(name, manifest.File, "", StageReload, CategoryManifest,
			fmt.Errorf("reloaded manifest declares a different plugin name %q", manifest.Name))
	}

	m.removePlugin(name)
	if err := m.loadOne(ctx, manifest); err != nil {
		return err
	}
	m.emit(EventReloaded, map[string]any{"plugin": name, "path": manifest.File})
	return nil
}

// reloadManifest re-reads the manifest from the same path it was loaded from.
func (m *Manager) reloadManifest(previous *Manifest) (*Manifest, error) {
	manifest, err := LoadManifest(previous.File)
	if err != nil {
		return nil, err
	}
	manifest.Source = previous.Source
	return manifest, nil
}

// Unload removes one plugin, or every plugin when name is empty.
func (m *Manager) Unload(name string) {
	if strings.TrimSpace(name) == "" {
		m.unloadAll()
		return
	}
	m.removePlugin(name)
	m.emit(EventUnloaded, map[string]any{"plugin": name})
}

// unloadAll removes every loaded plugin.
func (m *Manager) unloadAll() {
	m.mu.Lock()
	names := append([]string(nil), m.order...)
	m.mu.Unlock()
	for _, name := range names {
		m.removePlugin(name)
	}
}

// removePlugin unregisters everything a plugin contributed and closes its pool.
//
// The order matters: the registry entry is removed before the pool is closed,
// so a tool call that starts during the removal cannot find a handler whose
// runtime is already gone.
func (m *Manager) removePlugin(name string) {
	m.mu.Lock()
	plugin, ok := m.plugins[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.plugins, name)
	for i, existing := range m.order {
		if existing == name {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	var contributed []string
	for toolName, owner := range m.toolNames {
		if owner == name {
			contributed = append(contributed, toolName)
			delete(m.toolNames, toolName)
		}
	}
	for commandName, owner := range m.cmdNames {
		if owner == name {
			delete(m.cmdNames, commandName)
		}
	}
	registry := m.tools
	commands := m.commands
	m.mu.Unlock()

	// Hooks and commands are removed first: a hook or command that is still
	// registered would try to run on a runtime that is about to be closed.
	m.hooks.Unregister(name)
	if commands != nil {
		commands.Unregister(name)
	}
	m.mu.Lock()
	for key := range m.commandHandlers {
		if strings.HasPrefix(key, name+"/") {
			delete(m.commandHandlers, key)
		}
	}
	m.mu.Unlock()
	if registry != nil {
		for _, toolName := range contributed {
			registry.Unregister(toolName)
		}
	}
	_ = plugin.pool.Close()
}

// Close unloads every plugin and refuses further loads.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	m.unloadAll()
	return nil
}

// Plugins returns a stable snapshot of the loaded plugins.
func (m *Manager) Plugins() []PluginStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]PluginStatus, 0, len(m.order))
	for _, name := range m.order {
		plugin, ok := m.plugins[name]
		if !ok {
			continue
		}
		stats := plugin.pool.Stats()
		tools := make([]string, 0, len(plugin.manifest.Tools))
		for _, declaration := range plugin.manifest.Tools {
			tools = append(tools, declaration.Name)
		}
		hooks := make([]string, 0, len(plugin.manifest.Hooks))
		for _, declaration := range plugin.manifest.Hooks {
			hooks = append(hooks, declaration.Event+":"+declaration.Handler)
		}
		commands := make([]string, 0, len(plugin.manifest.Commands))
		for _, declaration := range plugin.manifest.Commands {
			commands = append(commands, declaration.Name)
		}
		out = append(out, PluginStatus{
			Name:        name,
			Version:     plugin.manifest.Version,
			Description: plugin.manifest.Description,
			Source:      plugin.manifest.Source,
			RootDir:     plugin.manifest.RootDir,
			Generation:  plugin.generation,
			Granted:     plugin.grant.Summary(),
			Tools:       tools,
			Hooks:       hooks,
			Commands:    commands,
			Runtimes:    stats,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PluginStatus is one plugin as reported to the user.
type PluginStatus struct {
	Name        string
	Version     string
	Description string
	Source      Source
	RootDir     string
	Generation  uint64
	Granted     string
	Tools       []string
	Hooks       []string
	Commands    []string
	Runtimes    jsengine.Stats
}

// Diagnostics returns the load diagnostics, sorted and stable.
func (m *Manager) Diagnostics() []Diagnostic {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]Diagnostic(nil), m.diags...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Plugin != out[j].Plugin {
			return out[i].Plugin < out[j].Plugin
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Overrides returns the workspace-over-user precedence notes.
func (m *Manager) Overrides() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.overrides...)
}

// ToolNames returns the plugin-contributed tool names, sorted.
func (m *Manager) ToolNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.toolNames))
	for name := range m.toolNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CommandNames returns the plugin-contributed command names, sorted.
func (m *Manager) CommandNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.cmdNames))
	for name := range m.cmdNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Count returns the number of loaded plugins.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.plugins)
}

func (m *Manager) emit(kind string, fields map[string]any) {
	if m.observer == nil {
		return
	}
	// Observation must never break an invocation, so a panicking observer is
	// contained here.
	defer func() { _ = recover() }()
	m.observer.PluginEvent(kind, fields)
}

// timeoutFor converts a manifest millisecond timeout into a duration.
func (m *Manager) timeoutFor(milliseconds int) time.Duration {
	if milliseconds <= 0 {
		return 0
	}
	timeout := time.Duration(milliseconds) * time.Millisecond
	if timeout > MaxInvocationTimeout {
		return MaxInvocationTimeout
	}
	return timeout
}

// sandboxAllows checks a capability against the live sandbox status.
func (m *Manager) sandboxAllows(capability tool.Capability) (bool, string) {
	if capability.Empty() {
		return true, ""
	}
	if m.sandbox == nil {
		// No sandbox is configured, so nothing can be enforced. Fail closed.
		return false, "the host has no sandbox policy, so filesystem, network and shell capabilities cannot be granted"
	}
	status := m.sandbox()
	verdict, reason := SandboxAllows(sandboxStatusOf(status), capability)
	if !verdict {
		return false, reason
	}
	return true, ""
}

// sandboxStatusOf converts the manager's lightweight status into the sandbox
// package's status type.
func sandboxStatusOf(status SandboxStatus) sandbox.Status {
	return sandbox.Status{
		Mode:          sandbox.Mode(status.Mode),
		NetworkAccess: status.NetworkAccess,
		Capabilities: sandbox.Capabilities{
			Backend:   status.Backend,
			Available: status.Available,
			Reason:    status.Reason,
		},
		Generation: status.Generation,
	}
}

// virtualModules builds the bruce:* modules a plugin may import.
//
// A module is only offered when the plugin was granted the permission it
// stands for, so "can I import bruce:storage" is answered by the same policy
// that answers "can I read storage".
func (m *Manager) virtualModules(manifest *Manifest) map[string]string {
	modules := map[string]string{
		VirtualAPI: virtualAPISource(manifest),
	}
	grant := m.policy.GrantFor(manifest.Name, permissionStrings(manifest.Permissions))
	if grant.Allows(PermissionStorage) {
		modules[VirtualStorage] = virtualStorageSource()
	}
	if grant.Allows(PermissionEvents) {
		modules[VirtualEvents] = virtualEventsSource()
	}
	return modules
}

// globals builds the host capability object tree for one plugin.
func (m *Manager) globals(manifest *Manifest, grant Grant) map[string]jsengine.GlobalObject {
	bruce := jsengine.GlobalObject{}
	if grant.Allows(PermissionStorage) {
		bruce["storage"] = jsengine.GlobalObject{
			"get":    jsengine.HostFunc(m.storageGet(manifest.Name)),
			"set":    jsengine.HostFunc(m.storageSet(manifest.Name)),
			"delete": jsengine.HostFunc(m.storageDelete(manifest.Name)),
			"keys":   jsengine.HostFunc(m.storageKeys(manifest.Name)),
		}
	}
	bruce["events"] = jsengine.GlobalObject{
		"emit": jsengine.HostFunc(m.eventsEmit(manifest.Name, grant)),
	}
	globals := map[string]jsengine.GlobalObject{"bruce": bruce}
	for name, value := range m.extraGlobals {
		if name != "bruce" {
			globals[name] = value
			continue
		}
		// ExtraGlobals may extend the bruce namespace rather than replace it:
		// replacing would silently delete the storage and events capabilities
		// and make a plugin's own host functions invisible.
		for key, item := range value {
			bruce[key] = item
		}
	}
	return globals
}

// allHandlerPaths lists every handler the manifest declares.
func allHandlerPaths(manifest *Manifest) []string {
	seen := map[string]bool{}
	var out []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	for _, declaration := range manifest.Tools {
		add(declaration.Handler)
	}
	for _, declaration := range manifest.Hooks {
		add(declaration.Handler)
	}
	for _, declaration := range manifest.Commands {
		add(declaration.Handler)
	}
	sort.Strings(out)
	return out
}

func permissionStrings(permissions []Permission) []Permission {
	return append([]Permission(nil), permissions...)
}

// capabilityFor derives the capability of a tool from the permissions the
// plugin declared for it, falling back to the plugin-wide declarations.
func capabilityFor(manifest *Manifest, declaration ToolDeclaration) tool.Capability {
	permissions := declaration.Permissions
	if len(permissions) == 0 {
		permissions = manifest.Permissions
	}
	capability := tool.Capability{}
	for _, permission := range permissions {
		switch permission {
		case PermissionFilesystemRead:
			capability.FilesystemRead = true
		case PermissionFilesystemWrite:
			capability.FilesystemWrite = true
		case PermissionNetwork:
			capability.Network = true
		case PermissionShell:
			capability.Shell = true
			// A shell can read and write anywhere the sandbox allows.
			capability.FilesystemRead = true
			capability.FilesystemWrite = true
		}
	}
	if !capability.Empty() {
		capability.WorkspaceScope = tool.ScopeWorkspace
	}
	return capability
}

func riskFor(raw string) tool.Risk {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "safe":
		return tool.RiskSafe
	case "low":
		return tool.RiskLow
	case "high":
		return tool.RiskHigh
	case "medium":
		return tool.RiskMedium
	default:
		// An undeclared risk on third-party code is not "safe" by omission.
		return tool.RiskMedium
	}
}

func promptSnippet(declaration ToolDeclaration) string {
	if strings.TrimSpace(declaration.PromptSnippet) != "" {
		return declaration.PromptSnippet
	}
	return declaration.Description
}

// relativeTo returns target relative to base, falling back to the target.
func relativeTo(base, target string) string {
	relative, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return relative
}

func categoryForEngineError(err error, fallback ErrorCategory) ErrorCategory {
	if err == nil {
		return fallback
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CategoryTimeout
	}
	if errors.Is(err, context.Canceled) {
		return CategoryCancellation
	}
	switch kind, _ := jsengine.KindOf(err); kind {
	case jsengine.KindSyntax:
		return CategorySyntax
	case jsengine.KindLink:
		return CategoryLink
	case jsengine.KindMissingHandler:
		return CategoryMissingHandler
	case jsengine.KindException:
		return CategoryException
	case jsengine.KindInterrupted:
		return CategoryCancellation
	case jsengine.KindInternal:
		return CategoryPanic
	case jsengine.KindResource:
		return CategoryMalformedResult
	default:
		return fallback
	}
}
