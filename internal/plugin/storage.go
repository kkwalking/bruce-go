package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// StorageScope names the lifetime of a stored value.
//
// The scope is part of the identity of a value: the same key under two scopes
// is two distinct entries, and a plugin must say which lifetime it means.
type StorageScope string

const (
	ScopeInvocation StorageScope = "invocation"
	ScopeSession    StorageScope = "session"
	ScopePlugin     StorageScope = "plugin"
	ScopeWorkspace  StorageScope = "workspace"
	ScopeGlobal     StorageScope = "global"
)

// Storage is the explicit state abstraction handed to plugins. A plugin must
// never keep long-lived state in a JavaScript module global: several runtimes
// serve one plugin, so module globals diverge.
//
// Every scope is namespaced by plugin. Two plugins never see each other's
// values, not even under ScopeWorkspace or ScopeGlobal: those scopes widen the
// lifetime of a value, never its visibility.
type Storage interface {
	Get(ctx context.Context, scope StorageScope, key string) (json.RawMessage, bool, error)
	Set(ctx context.Context, scope StorageScope, key string, value json.RawMessage) error
	Delete(ctx context.Context, scope StorageScope, key string) error
	Keys(ctx context.Context, scope StorageScope) ([]string, error)
}

// Compile-time proof that both handles satisfy the interface plugins are
// handed. A signature drift here must fail the build, not a caller.
var (
	_ Storage = (*pluginStorage)(nil)
	_ Storage = (*ScopedStorage)(nil)
)

// Storage sentinels. They are errors.Is-able so a caller can classify a
// failure without matching message text, and every storage failure is wrapped
// in a *PluginError carrying CategoryStorage so the plugin boundary sees one
// error shape.
var (
	storageErrClosed    = errors.New("plugin storage is closed")
	storageErrInvalid   = errors.New("invalid plugin storage key")
	storageErrScope     = errors.New("invalid plugin storage scope")
	storageErrNamespace = errors.New("invalid plugin storage namespace")
)

// MemoryStorage is the first-version implementation. It persists nothing
// across process restarts for the non-durable scopes, and it is safe for
// concurrent use.
//
// The zero value is usable and grants nothing.
type MemoryStorage struct {
	mu     sync.RWMutex
	values map[storageNamespace]map[string]json.RawMessage
}

// storageNamespace is the fully resolved identity of one value space.
//
// extra carries the session id for ScopeSession and the session/invocation
// pair for ScopeInvocation, so that two invocations of one plugin never share
// a bucket even when their keys are identical.
type storageNamespace struct {
	scope  StorageScope
	plugin string
	extra  string
}

// NewMemoryStorage returns an empty in-memory store.
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{values: map[storageNamespace]map[string]json.RawMessage{}}
}

// ForPlugin returns the storage handle for one plugin. It is namespaced:
// plugin A can never read or overwrite plugin B's entries, even with the same
// key, and it must be impossible to escape the namespace by using a key that
// contains "/", "..", or a NUL byte.
//
// ScopeInvocation is not reachable from this handle: an invocation-scoped
// value needs an invocation identity, which only ForInvocation carries.
// Rejecting it here is deliberate, because a fallback namespace would let one
// invocation's value leak into the next.
//
// An invalid plugin name yields a handle that fails closed: every operation
// reports the problem instead of silently writing into a shared namespace.
func (s *MemoryStorage) ForPlugin(plugin string) Storage {
	handle := &pluginStorage{store: s, plugin: plugin}
	if err := validateStorageName("plugin name", plugin); err != nil {
		handle.err = err
	}
	return handle
}

// ForInvocation returns the storage handle for one invocation of one plugin.
//
// Closing it releases the invocation-scoped values and makes every later
// operation on the handle fail. The handle refuses to open at all when the
// plugin name or the invocation id is missing or malformed, because an empty
// invocation id would silently address the namespace of another invocation.
//
// One handle belongs to one invocation: create it when the invocation starts
// and close it when the invocation ends.
func (s *MemoryStorage) ForInvocation(plugin, sessionID, invocationID string) *ScopedStorage {
	scoped := &ScopedStorage{
		store:      s,
		plugin:     plugin,
		sessionID:  sessionID,
		invocation: invocationID,
		namespace: storageNamespace{
			scope:  ScopeInvocation,
			plugin: plugin,
			extra:  sessionID + "\x00" + invocationID,
		},
	}
	for _, field := range []struct{ kind, value string }{
		{"plugin name", plugin},
		{"invocation id", invocationID},
	} {
		if err := validateStorageName(field.kind, field.value); err != nil {
			scoped.err = fmt.Errorf("%w: %v", storageErrNamespace, err)
			break
		}
	}
	if scoped.err == nil && sessionID != "" {
		if err := validateStorageName("session id", sessionID); err != nil {
			scoped.err = fmt.Errorf("%w: %v", storageErrNamespace, err)
		}
	}
	if scoped.err == nil {
		scoped.base = s.ForPlugin(plugin)
	}
	return scoped
}

// pluginStorage is the per-plugin view returned by ForPlugin.
type pluginStorage struct {
	store  *MemoryStorage
	plugin string
	// err, when set, makes every operation fail. It is how a handle built from
	// an invalid name fails closed without a constructor error return.
	err error
}

func (p *pluginStorage) Get(ctx context.Context, scope StorageScope, key string) (json.RawMessage, bool, error) {
	namespace, err := p.resolve(scope, key, false)
	if err != nil {
		return nil, false, err
	}
	if err := ctxErr(ctx); err != nil {
		return nil, false, p.wrap(err)
	}
	value, found := p.store.lookup(namespace, key)
	if !found {
		return nil, false, nil
	}
	return value, true, nil
}

func (p *pluginStorage) Set(ctx context.Context, scope StorageScope, key string, value json.RawMessage) error {
	namespace, err := p.resolve(scope, key, false)
	if err != nil {
		return err
	}
	if err := ctxErr(ctx); err != nil {
		return p.wrap(err)
	}
	p.store.put(namespace, key, value)
	return nil
}

func (p *pluginStorage) Delete(ctx context.Context, scope StorageScope, key string) error {
	namespace, err := p.resolve(scope, key, false)
	if err != nil {
		return err
	}
	if err := ctxErr(ctx); err != nil {
		return p.wrap(err)
	}
	p.store.remove(namespace, key)
	return nil
}

func (p *pluginStorage) Keys(ctx context.Context, scope StorageScope) ([]string, error) {
	namespace, err := p.resolve(scope, "", true)
	if err != nil {
		return nil, err
	}
	if err := ctxErr(ctx); err != nil {
		return nil, p.wrap(err)
	}
	return p.store.list(namespace), nil
}

// resolve validates the handle, the scope and the key, and returns the
// namespace the operation addresses.
//
// Only Keys may omit the key, because it addresses a whole namespace: an empty
// key on Get, Set or Delete is a caller bug, and treating it as "some default
// entry" would make one plugin's value reachable by two different names.
func (p *pluginStorage) resolve(scope StorageScope, key string, wholeNamespace bool) (storageNamespace, error) {
	if p.err != nil {
		return storageNamespace{}, p.wrap(p.err)
	}
	if scope == ScopeInvocation {
		return storageNamespace{}, p.wrap(fmt.Errorf(
			"%w: scope %q requires an invocation-scoped handle; use ForInvocation",
			storageErrScope, string(ScopeInvocation),
		))
	}
	if err := validateStorageScope(scope); err != nil {
		return storageNamespace{}, p.wrap(err)
	}
	if !wholeNamespace {
		if err := validateStorageKey(key); err != nil {
			return storageNamespace{}, p.wrap(err)
		}
	}
	return storageNamespace{scope: scope, plugin: p.plugin}, nil
}

// wrap turns a storage failure into a *PluginError so the plugin boundary sees
// one error shape, while errors.Is still reaches the sentinel underneath.
func (p *pluginStorage) wrap(err error) error { return storageFailure(p.plugin, err) }

// ScopedStorage narrows a Storage to one invocation: after Close it rejects
// every operation, so a value written for one invocation cannot leak into the
// next.
type ScopedStorage struct {
	store      *MemoryStorage
	base       Storage
	plugin     string
	sessionID  string
	invocation string
	namespace  storageNamespace

	// mu guards closed and err, and it is held for the duration of every
	// operation. Close takes it exclusively, so a write that already passed
	// the open check either lands before Close drops the namespace or is
	// refused — it can never land after.
	mu     sync.RWMutex
	closed bool
	err    error
}

func (s *ScopedStorage) Get(ctx context.Context, scope StorageScope, key string) (json.RawMessage, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.open(); err != nil {
		return nil, false, err
	}
	if scope != ScopeInvocation {
		return s.base.Get(ctx, scope, key)
	}
	if err := validateStorageKey(key); err != nil {
		return nil, false, s.wrap(err)
	}
	if err := ctxErr(ctx); err != nil {
		return nil, false, s.wrap(err)
	}
	value, found := s.store.lookup(s.namespace, key)
	if !found {
		return nil, false, nil
	}
	return value, true, nil
}

func (s *ScopedStorage) Set(ctx context.Context, scope StorageScope, key string, value json.RawMessage) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.open(); err != nil {
		return err
	}
	if scope != ScopeInvocation {
		return s.base.Set(ctx, scope, key, value)
	}
	if err := validateStorageKey(key); err != nil {
		return s.wrap(err)
	}
	if err := ctxErr(ctx); err != nil {
		return s.wrap(err)
	}
	s.store.put(s.namespace, key, value)
	return nil
}

func (s *ScopedStorage) Delete(ctx context.Context, scope StorageScope, key string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.open(); err != nil {
		return err
	}
	if scope != ScopeInvocation {
		return s.base.Delete(ctx, scope, key)
	}
	if err := validateStorageKey(key); err != nil {
		return s.wrap(err)
	}
	if err := ctxErr(ctx); err != nil {
		return s.wrap(err)
	}
	s.store.remove(s.namespace, key)
	return nil
}

func (s *ScopedStorage) Keys(ctx context.Context, scope StorageScope) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.open(); err != nil {
		return nil, err
	}
	if scope != ScopeInvocation {
		return s.base.Keys(ctx, scope)
	}
	if err := ctxErr(ctx); err != nil {
		return nil, s.wrap(err)
	}
	return s.store.list(s.namespace), nil
}

// Close ends the invocation: the invocation-scoped values are dropped and
// every later operation on this handle fails.
//
// Close is idempotent. It waits for operations already in flight, so a write
// cannot land after the namespace was dropped.
func (s *ScopedStorage) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.err == nil {
		s.store.drop(s.namespace)
	}
}

// open reports whether the handle is still usable. The caller must hold mu.
func (s *ScopedStorage) open() error {
	if s == nil {
		return storageFailure("", fmt.Errorf("%w: nil invocation scope", storageErrClosed))
	}
	if s.closed {
		return storageFailure(s.plugin, fmt.Errorf(
			"%w: invocation %q of plugin %q has ended", storageErrClosed, s.invocation, s.plugin,
		))
	}
	if s.err != nil {
		return storageFailure(s.plugin, s.err)
	}
	return nil
}

func (s *ScopedStorage) wrap(err error) error { return storageFailure(s.plugin, err) }

// storageFailure wraps a storage failure as a *PluginError in the invoke stage
// with CategoryStorage.
func storageFailure(plugin string, err error) error {
	if err == nil {
		return nil
	}
	return NewError(plugin, "", "", StageInvoke, CategoryStorage, err)
}

func (s *MemoryStorage) lookup(namespace storageNamespace, key string) (json.RawMessage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bucket, ok := s.values[namespace]
	if !ok {
		return nil, false
	}
	value, ok := bucket[key]
	if !ok {
		return nil, false
	}
	// Hand back a copy: a caller that mutates the returned bytes must not be
	// able to reach into the store.
	return append(json.RawMessage(nil), value...), true
}

func (s *MemoryStorage) put(namespace storageNamespace, key string, value json.RawMessage) {
	stored := append(json.RawMessage(nil), value...)
	if stored == nil {
		stored = json.RawMessage{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[storageNamespace]map[string]json.RawMessage{}
	}
	bucket, ok := s.values[namespace]
	if !ok {
		bucket = map[string]json.RawMessage{}
		s.values[namespace] = bucket
	}
	bucket[key] = stored
}

func (s *MemoryStorage) remove(namespace storageNamespace, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.values[namespace]
	if !ok {
		return
	}
	delete(bucket, key)
	if len(bucket) == 0 {
		delete(s.values, namespace)
	}
}

func (s *MemoryStorage) list(namespace storageNamespace) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bucket := s.values[namespace]
	if len(bucket) == 0 {
		return nil
	}
	keys := make([]string, 0, len(bucket))
	for key := range bucket {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// drop releases every value of one namespace.
func (s *MemoryStorage) drop(namespace storageNamespace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, namespace)
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// validateStorageScope accepts only the five declared scopes.
func validateStorageScope(scope StorageScope) error {
	switch scope {
	case ScopeInvocation, ScopeSession, ScopePlugin, ScopeWorkspace, ScopeGlobal:
		return nil
	default:
		return fmt.Errorf("%w: unknown scope %q", storageErrScope, string(scope))
	}
}

// validateStorageKey rejects any key that could be read as a path.
//
// Rejecting is the point: silently normalizing "../b" into "b" would let one
// namespace address another, and a storage backend that later writes to disk
// would inherit that escape.
func validateStorageKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: the key must not be empty", storageErrInvalid)
	}
	if !utf8.ValidString(key) {
		return fmt.Errorf("%w: the key is not valid UTF-8", storageErrInvalid)
	}
	if strings.ContainsRune(key, 0) {
		return fmt.Errorf("%w: the key must not contain a NUL byte", storageErrInvalid)
	}
	if strings.ContainsAny(key, `/\`) {
		return fmt.Errorf("%w: the key must not contain a path separator: %q", storageErrInvalid, key)
	}
	if strings.Contains(key, "..") {
		return fmt.Errorf("%w: the key must not contain %q: %q", storageErrInvalid, "..", key)
	}
	return nil
}

// validateStorageName rejects namespace components that are empty or that
// could be mistaken for a path or a traversal.
func validateStorageName(kind, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: the %s must not be empty", storageErrNamespace, kind)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: the %s is not valid UTF-8", storageErrNamespace, kind)
	}
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("%w: the %s must not contain a NUL byte", storageErrNamespace, kind)
	}
	if strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("%w: the %s must not contain a path separator", storageErrNamespace, kind)
	}
	if strings.Contains(value, "..") {
		return fmt.Errorf("%w: the %s must not contain %q", storageErrNamespace, kind, "..")
	}
	return nil
}
