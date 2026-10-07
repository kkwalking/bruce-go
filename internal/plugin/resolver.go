package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"bruce-go/internal/jsengine"
)

// Virtual module specifiers Bruce provides to plugins.
//
// Only these, plus relative modules inside the plugin's own directory, may be
// imported. There is deliberately no npm resolution, no node_modules, no Node
// builtin and no network loading: an import is a capability, and the host
// decides which ones exist.
const (
	// VirtualAPI exposes the plugin's own identity and the declared API
	// version, so a plugin can assert the host it is running against.
	VirtualAPI = "bruce:api"
	// VirtualStorage is the explicit state abstraction. A plugin must not keep
	// long-lived state in a module global, because several runtimes serve one
	// plugin and their globals diverge.
	VirtualStorage = "bruce:storage"
	// VirtualEvents lets a plugin emit observation events into Bruce's bus.
	VirtualEvents = "bruce:events"
)

// VirtualModules lists the virtual module specifiers the host serves.
func VirtualModules() []string {
	return []string{VirtualAPI, VirtualEvents, VirtualStorage}
}

// ResolverOptions configures one plugin's import resolution.
type ResolverOptions struct {
	// Engine compiles the resolved modules. It is injected rather than
	// imported so this package never names a concrete engine.
	Engine jsengine.Engine
	// RootDir is the plugin's own directory. Nothing outside it may be
	// imported, directly or through a symlink.
	RootDir string
	// Virtual supplies the virtual modules this plugin may import. A specifier
	// that is not in this map and is not a relative module inside RootDir is
	// refused.
	Virtual map[string]string
	// MaxModuleBytes bounds one source file, so a plugin cannot make the host
	// read an enormous file into memory.
	MaxModuleBytes int64
}

// DefaultMaxModuleBytes bounds a single plugin module.
const DefaultMaxModuleBytes = 1 << 20

// Resolver compiles and caches the modules one plugin imports.
//
// Modules are compiled once and shared by every runtime of that plugin, which
// is the lifecycle the engine expects: a Module is immutable, and compiling it
// per call would make a warm invocation pay the parse cost again.
type Resolver struct {
	engine  jsengine.Engine
	root    string
	virtual map[string]string
	maxSize int64

	mu      sync.Mutex
	cache   map[string]jsengine.Module
	loading map[string]bool
}

// NewResolver validates the plugin root and returns a resolver for it.
func NewResolver(opts ResolverOptions) (*Resolver, error) {
	if opts.Engine == nil {
		return nil, errors.New("plugin: resolver requires a JavaScript engine")
	}
	if strings.TrimSpace(opts.RootDir) == "" {
		return nil, errors.New("plugin: resolver root directory must not be empty")
	}
	// The root is resolved once, through symlinks. Every candidate path is
	// resolved the same way before it is compared against it, so a symlink
	// inside the plugin directory cannot point outside it.
	root, err := filepath.EvalSymlinks(opts.RootDir)
	if err != nil {
		return nil, fmt.Errorf("plugin: resolve plugin directory: %w", err)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("plugin: resolve plugin directory: %w", err)
	}
	maxSize := opts.MaxModuleBytes
	if maxSize <= 0 {
		maxSize = DefaultMaxModuleBytes
	}
	virtual := make(map[string]string, len(opts.Virtual))
	for name, source := range opts.Virtual {
		virtual[name] = source
	}
	return &Resolver{
		engine:  opts.Engine,
		root:    filepath.Clean(absolute),
		virtual: virtual,
		maxSize: maxSize,
		cache:   map[string]jsengine.Module{},
		loading: map[string]bool{},
	}, nil
}

// Root returns the resolved plugin root directory.
func (r *Resolver) Root() string { return r.root }

// Resolve implements jsengine.Resolver.
//
// It is called by the engine while linking, for every static import and for
// every dynamic import(). The referrer is the importing module's name; a
// relative specifier is resolved against the importing module's directory, so
// a nested module importing "./sibling.js" means its own sibling and not the
// plugin root's.
func (r *Resolver) Resolve(referrer, specifier string) (jsengine.Module, error) {
	specifier = strings.TrimSpace(specifier)
	if specifier == "" {
		return nil, fmt.Errorf("plugin %s: empty import specifier", r.root)
	}
	if source, ok := r.virtual[specifier]; ok {
		return r.compile(specifier, source)
	}
	if !strings.HasPrefix(specifier, "./") && !strings.HasPrefix(specifier, "../") {
		// A bare specifier is never resolved from disk. This is what blocks
		// npm, node_modules, Node builtins and system paths in one rule.
		return nil, fmt.Errorf("import %q is not allowed: only relative modules inside the plugin directory and the host modules %s may be imported",
			specifier, strings.Join(VirtualModules(), ", "))
	}
	target, err := r.resolveFile(referrer, specifier)
	if err != nil {
		return nil, err
	}
	source, err := r.readSource(target)
	if err != nil {
		return nil, err
	}
	return r.compile(target, source)
}

// resolveFile turns a relative specifier into an absolute path that is proven
// to live inside the plugin root.
func (r *Resolver) resolveFile(referrer, specifier string) (string, error) {
	base := r.root
	if referrer != "" && !strings.HasPrefix(referrer, "bruce:") {
		referrerPath := referrer
		if !filepath.IsAbs(referrerPath) {
			referrerPath = filepath.Join(r.root, referrerPath)
		}
		base = filepath.Dir(referrerPath)
	}
	candidate := filepath.Join(base, filepath.FromSlash(specifier))

	// EvalSymlinks both proves existence and resolves every symlink on the
	// way, so a link pointing outside the plugin directory is caught by the
	// containment check below rather than trusted.
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("import %q does not exist in plugin %s", specifier, r.root)
		}
		return "", fmt.Errorf("import %q cannot be resolved: %w", specifier, err)
	}
	if err := r.contains(resolved); err != nil {
		return "", fmt.Errorf("import %q escapes the plugin directory: %w", specifier, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("import %q names a directory", specifier)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("import %q does not name a regular file", specifier)
	}
	return resolved, nil
}

// contains reports whether an already-resolved path is inside the plugin root.
func (r *Resolver) contains(path string) error {
	relative, err := filepath.Rel(r.root, path)
	if err != nil {
		return err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("%s is outside %s", path, r.root)
	}
	return nil
}

func (r *Resolver) readSource(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > r.maxSize {
		return "", fmt.Errorf("plugin module %s is %d bytes, which exceeds the %d byte limit", path, info.Size(), r.maxSize)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// compile compiles a module once and caches it by resolved path.
//
// The loading guard turns an import cycle into a clear error instead of
// unbounded recursion: the engine links a graph itself, but a dynamic
// import() resolved while a module is still compiling would otherwise
// re-enter.
func (r *Resolver) compile(name, source string) (jsengine.Module, error) {
	r.mu.Lock()
	if module, ok := r.cache[name]; ok {
		r.mu.Unlock()
		return module, nil
	}
	if r.loading[name] {
		r.mu.Unlock()
		return nil, fmt.Errorf("circular import of %q", name)
	}
	r.loading[name] = true
	r.mu.Unlock()

	module, err := r.engine.Compile(name, source)

	r.mu.Lock()
	delete(r.loading, name)
	if err == nil {
		r.cache[name] = module
	}
	r.mu.Unlock()
	return module, err
}

// Link resolves the entry's static imports and returns the linked module.
//
// A module that imports nothing is returned unchanged, so a single-file plugin
// pays nothing for the linker.
func (r *Resolver) Link(entry jsengine.Module) (jsengine.Module, error) {
	if entry == nil {
		return nil, errors.New("plugin: entry module must not be nil")
	}
	if len(entry.Requests()) == 0 {
		return entry, nil
	}
	return r.engine.Link(entry, r.Resolve)
}

// CachedModules returns the resolved paths of every module compiled for this
// plugin, sorted. It exists for diagnostics and tests.
func (r *Resolver) CachedModules() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.cache))
	for name := range r.cache {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// virtualAPISource renders bruce:api for one plugin.
func virtualAPISource(manifest *Manifest) string {
	return fmt.Sprintf(`const apiVersion = %q;
const name = %q;
const version = %q;
const source = %q;
// info() exists so a plugin can assert at runtime which host it is running
// against, and so the identity is reachable as a callable handler rather than
// only as data.
const info = () => ({ apiVersion, name, version, source });
export { apiVersion, name, version, source, info };
export default { apiVersion, name, version, source, info };
`, APIVersion, manifest.Name, manifest.Version, string(manifest.Source))
}

// virtualEventsSource renders bruce:events. Emitting is a host capability: the
// call is checked against the plugin's grant before anything is published.
func virtualEventsSource() string {
	return `const emit = (name, payload) => globalThis.bruce.events.emit(name, payload);
export { emit };
export default { emit };
`
}

// virtualStorageSource renders bruce:storage on top of the host functions.
//
// Every scope is explicit. There is no implicit "module global is persistent"
// shortcut, because with a runtime pool the globals of one runtime are not the
// globals of the next.
//
// Arguments are a single object rather than positional parameters, matching
// how a plugin handler itself is called: one JSON value in, one JSON value
// out. That keeps the host boundary uniform and lets a scope or a key be
// omitted without changing the arity.
func virtualStorageSource() string {
	return `const scopes = ["invocation", "session", "plugin", "workspace", "global"];
const check = (request) => {
  if (!request || typeof request !== "object") throw new Error("storage requires an object argument");
  if (!scopes.includes(request.scope)) throw new Error("unknown storage scope: " + request.scope);
  if (typeof request.key !== "string" || request.key === "") throw new Error("storage requires a non-empty string key");
};
const get = (request) => { check(request); return globalThis.bruce.storage.get(request); };
const set = (request) => { check(request); return globalThis.bruce.storage.set(request); };
const remove = (request) => { check(request); return globalThis.bruce.storage.delete(request); };
const keys = (request) => {
  if (!request || typeof request !== "object") throw new Error("storage requires an object argument");
  if (!scopes.includes(request.scope)) throw new Error("unknown storage scope: " + request.scope);
  return globalThis.bruce.storage.keys(request);
};
export { scopes, get, set, remove, keys };
export default { scopes, get, set, remove, keys };
`
}
