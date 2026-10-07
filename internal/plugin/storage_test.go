package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func mustSet(t *testing.T, storage Storage, scope StorageScope, key, value string) {
	t.Helper()
	if err := storage.Set(context.Background(), scope, key, json.RawMessage(value)); err != nil {
		t.Fatalf("Set(%q, %q) = %v", scope, key, err)
	}
}

func mustGet(t *testing.T, storage Storage, scope StorageScope, key string) (json.RawMessage, bool) {
	t.Helper()
	value, found, err := storage.Get(context.Background(), scope, key)
	if err != nil {
		t.Fatalf("Get(%q, %q) = %v", scope, key, err)
	}
	return value, found
}

func TestMemoryStorageSetGetDeleteRoundTrip(t *testing.T) {
	storage := NewMemoryStorage().ForPlugin("demo")
	ctx := context.Background()

	if _, found := mustGet(t, storage, ScopePlugin, "missing"); found {
		t.Error("an unwritten key must not be found")
	}

	mustSet(t, storage, ScopePlugin, "answer", `{"value":42}`)
	value, found := mustGet(t, storage, ScopePlugin, "answer")
	if !found {
		t.Fatal("a written key must be found")
	}
	if string(value) != `{"value":42}` {
		t.Errorf("Get() = %s, want the stored JSON", value)
	}

	keys, err := storage.Keys(ctx, ScopePlugin)
	if err != nil || len(keys) != 1 || keys[0] != "answer" {
		t.Fatalf("Keys() = %v, %v", keys, err)
	}

	if err := storage.Delete(ctx, ScopePlugin, "answer"); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if _, found := mustGet(t, storage, ScopePlugin, "answer"); found {
		t.Error("a deleted key must not be found")
	}
	// Deleting a missing key is not an error: delete is idempotent.
	if err := storage.Delete(ctx, ScopePlugin, "answer"); err != nil {
		t.Errorf("Delete() of a missing key = %v, want nil", err)
	}
	if keys, err := storage.Keys(ctx, ScopePlugin); err != nil || len(keys) != 0 {
		t.Errorf("Keys() = %v, %v, want empty", keys, err)
	}
}

func TestMemoryStorageScopesAreDistinct(t *testing.T) {
	storage := NewMemoryStorage().ForPlugin("demo")
	for _, scope := range []StorageScope{ScopeSession, ScopePlugin, ScopeWorkspace, ScopeGlobal} {
		mustSet(t, storage, scope, "shared-key", string(scope))
	}
	for _, scope := range []StorageScope{ScopeSession, ScopePlugin, ScopeWorkspace, ScopeGlobal} {
		value, found := mustGet(t, storage, scope, "shared-key")
		if !found || string(value) != string(scope) {
			t.Errorf("scope %q returned %q (found=%v), want its own value", scope, value, found)
		}
	}
	if err := storage.Delete(context.Background(), ScopePlugin, "shared-key"); err != nil {
		t.Fatal(err)
	}
	if _, found := mustGet(t, storage, ScopeGlobal, "shared-key"); !found {
		t.Error("deleting in one scope must not touch another scope")
	}
}

func TestStorageIsolatesPlugins(t *testing.T) {
	store := NewMemoryStorage()
	pluginA := store.ForPlugin("plugin-a")
	pluginB := store.ForPlugin("plugin-b")
	ctx := context.Background()

	for _, scope := range []StorageScope{ScopePlugin, ScopeWorkspace, ScopeGlobal} {
		mustSet(t, pluginA, scope, "same-key", `"from-a"`)
		mustSet(t, pluginB, scope, "same-key", `"from-b"`)

		valueA, foundA := mustGet(t, pluginA, scope, "same-key")
		valueB, foundB := mustGet(t, pluginB, scope, "same-key")
		if !foundA || !foundB {
			t.Fatalf("scope %q: both plugins must see their own value (a=%v b=%v)", scope, foundA, foundB)
		}
		if string(valueA) != `"from-a"` || string(valueB) != `"from-b"` {
			t.Fatalf("scope %q: plugins shared a namespace: a=%s b=%s", scope, valueA, valueB)
		}
	}

	// A must not be able to delete B's value, and B must not appear in A's keys.
	if err := pluginA.Delete(ctx, ScopePlugin, "same-key"); err != nil {
		t.Fatal(err)
	}
	if _, found := mustGet(t, pluginB, ScopePlugin, "same-key"); !found {
		t.Error("plugin A's delete removed plugin B's value")
	}
	keys, err := pluginA.Keys(ctx, ScopePlugin)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("plugin A sees %v, want only its own (now empty) namespace", keys)
	}

	// Plugin B's other scopes are untouched by A's writes.
	if keys, _ := pluginB.Keys(ctx, ScopeGlobal); len(keys) != 1 {
		t.Errorf("plugin B global keys = %v, want one", keys)
	}
}

func TestStorageRejectsInvalidKeys(t *testing.T) {
	store := NewMemoryStorage()
	pluginA := store.ForPlugin("plugin-a")
	pluginB := store.ForPlugin("plugin-b")
	ctx := context.Background()

	// Seed B so that an escape would be observable.
	mustSet(t, pluginB, ScopePlugin, "secret", `"b-private"`)

	invalid := []string{
		"",
		"../secret",
		"..",
		"a/../../secret",
		"a/b",
		"a\\b",
		"..\\secret",
		"nested/..",
		"with\x00nul",
		"\x00",
	}
	for _, key := range invalid {
		if _, _, err := pluginA.Get(ctx, ScopePlugin, key); err == nil {
			t.Errorf("Get(%q) succeeded, want an error", key)
		}
		if err := pluginA.Set(ctx, ScopePlugin, key, json.RawMessage(`"x"`)); err == nil {
			t.Errorf("Set(%q) succeeded, want an error", key)
		}
		if err := pluginA.Delete(ctx, ScopePlugin, key); err == nil {
			t.Errorf("Delete(%q) succeeded, want an error", key)
		}
	}

	// Nothing was normalized into a valid key along the way.
	keys, err := pluginA.Keys(ctx, ScopePlugin)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("plugin A keys = %v, want none: an invalid key must not be normalized", keys)
	}
	if _, found := mustGet(t, pluginB, ScopePlugin, "secret"); !found {
		t.Error("an invalid key reached another plugin's namespace")
	}
}

func TestStorageRejectsUnknownScopeAndInvocationScopeOnPluginHandle(t *testing.T) {
	storage := NewMemoryStorage().ForPlugin("demo")
	ctx := context.Background()

	if _, _, err := storage.Get(ctx, StorageScope("forever"), "k"); err == nil {
		t.Error("an unknown scope must be rejected")
	}
	if err := storage.Set(ctx, StorageScope("forever"), "k", json.RawMessage(`1`)); err == nil {
		t.Error("an unknown scope must be rejected on Set")
	}
	if _, err := storage.Keys(ctx, StorageScope("forever")); err == nil {
		t.Error("an unknown scope must be rejected on Keys")
	}
	// An invocation-scoped value needs an invocation identity, which a
	// per-plugin handle does not carry.
	if err := storage.Set(ctx, ScopeInvocation, "k", json.RawMessage(`1`)); err == nil {
		t.Error("ScopeInvocation on a ForPlugin handle must be rejected")
	}
	if _, _, err := storage.Get(ctx, ScopeInvocation, "k"); err == nil {
		t.Error("ScopeInvocation on a ForPlugin handle must be rejected on Get")
	}
}

func TestStorageErrorsArePluginErrors(t *testing.T) {
	storage := NewMemoryStorage().ForPlugin("demo")
	err := storage.Set(context.Background(), ScopePlugin, "bad/key", json.RawMessage(`1`))
	if err == nil {
		t.Fatal("an invalid key must fail")
	}
	if !IsPluginError(err) {
		t.Errorf("error %v is not a *PluginError", err)
	}
	if category, ok := CategoryOf(err); !ok || category != CategoryStorage {
		t.Errorf("CategoryOf = %q, %v, want %q", category, ok, CategoryStorage)
	}
	if !errors.Is(err, storageErrInvalid) {
		t.Errorf("error %v must wrap the invalid-key sentinel", err)
	}
}

func TestMemoryStorageCopiesValues(t *testing.T) {
	storage := NewMemoryStorage().ForPlugin("demo")
	ctx := context.Background()

	value := json.RawMessage(`{"a":1}`)
	if err := storage.Set(ctx, ScopePlugin, "k", value); err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's slice must not change what is stored.
	value[0] = '['
	stored, found := mustGet(t, storage, ScopePlugin, "k")
	if !found || string(stored) != `{"a":1}` {
		t.Errorf("stored value = %s, want the original bytes", stored)
	}
	// Mutating the returned slice must not change what is stored either.
	stored[0] = '['
	again, _ := mustGet(t, storage, ScopePlugin, "k")
	if string(again) != `{"a":1}` {
		t.Errorf("stored value = %s, want the original bytes", again)
	}
}

func TestStorageContextCancellation(t *testing.T) {
	storage := NewMemoryStorage().ForPlugin("demo")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := storage.Set(ctx, ScopePlugin, "k", json.RawMessage(`1`)); !errors.Is(err, context.Canceled) {
		t.Errorf("Set() = %v, want context.Canceled", err)
	}
	if _, _, err := storage.Get(ctx, ScopePlugin, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get() = %v, want context.Canceled", err)
	}
	if _, err := storage.Keys(ctx, ScopePlugin); !errors.Is(err, context.Canceled) {
		t.Errorf("Keys() = %v, want context.Canceled", err)
	}
	if err := storage.Delete(ctx, ScopePlugin, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete() = %v, want context.Canceled", err)
	}
}

func TestStorageConcurrentReadWrite(t *testing.T) {
	store := NewMemoryStorage()
	ctx := context.Background()
	const (
		writers = 8
		readers = 8
		rounds  = 200
	)

	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			storage := store.ForPlugin(fmt.Sprintf("plugin-%d", writer%3))
			for round := 0; round < rounds; round++ {
				key := fmt.Sprintf("key-%d", round%16)
				if err := storage.Set(ctx, ScopePlugin, key, json.RawMessage(fmt.Sprintf(`{"w":%d,"r":%d}`, writer, round))); err != nil {
					t.Errorf("Set() = %v", err)
					return
				}
				if _, _, err := storage.Get(ctx, ScopePlugin, key); err != nil {
					t.Errorf("Get() = %v", err)
					return
				}
				if _, err := storage.Keys(ctx, ScopePlugin); err != nil {
					t.Errorf("Keys() = %v", err)
					return
				}
				if round%4 == 0 {
					if err := storage.Delete(ctx, ScopePlugin, key); err != nil {
						t.Errorf("Delete() = %v", err)
						return
					}
				}
			}
		}(writer)
	}
	for reader := 0; reader < readers; reader++ {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			storage := store.ForPlugin(fmt.Sprintf("plugin-%d", reader%3))
			for round := 0; round < rounds; round++ {
				key := fmt.Sprintf("key-%d", round%16)
				if _, _, err := storage.Get(ctx, ScopePlugin, key); err != nil {
					t.Errorf("Get() = %v", err)
					return
				}
				if _, err := storage.Keys(ctx, ScopeGlobal); err != nil {
					t.Errorf("Keys() = %v", err)
					return
				}
			}
		}(reader)
	}
	wg.Wait()
}

func TestScopedStorageInvocationIsolation(t *testing.T) {
	store := NewMemoryStorage()
	ctx := context.Background()

	first := store.ForInvocation("demo", "session-1", "invocation-1")
	mustSet(t, first, ScopeInvocation, "scratch", `"first"`)
	// Session scope spans invocations: it is the documented way to carry state
	// from one invocation to the next, so it must be visible to both.
	mustSet(t, first, ScopeSession, "handoff", `"from-first"`)

	// A second invocation must not see the first one's value.
	second := store.ForInvocation("demo", "session-1", "invocation-2")
	if _, found := mustGet(t, second, ScopeInvocation, "scratch"); found {
		t.Error("invocation 2 must not see invocation 1's invocation-scoped value")
	}
	if value, found := mustGet(t, second, ScopeSession, "handoff"); !found || string(value) != `"from-first"` {
		t.Errorf("session-scoped value = %s (found=%v), want it visible across invocations", value, found)
	}
	if keys, err := second.Keys(ctx, ScopeSession); err != nil || len(keys) != 1 {
		t.Errorf("Keys(session) = %v, %v, want the session value", keys, err)
	}

	// A different plugin with the same invocation id must not see it either.
	otherPlugin := store.ForInvocation("other", "session-1", "invocation-1")
	if _, found := mustGet(t, otherPlugin, ScopeInvocation, "scratch"); found {
		t.Error("another plugin must not see this invocation's value")
	}

	first.Close()
	second.Close()
	otherPlugin.Close()
}

func TestScopedStorageCloseReleasesAndRejects(t *testing.T) {
	store := NewMemoryStorage()
	ctx := context.Background()

	scoped := store.ForInvocation("demo", "session-1", "invocation-1")
	mustSet(t, scoped, ScopeInvocation, "scratch", `"value"`)
	mustSet(t, scoped, ScopePlugin, "durable", `"kept"`)

	scoped.Close()
	// Close is idempotent.
	scoped.Close()

	if _, _, err := scoped.Get(ctx, ScopeInvocation, "scratch"); !errors.Is(err, storageErrClosed) {
		t.Errorf("Get() after Close = %v, want a closed error", err)
	}
	if err := scoped.Set(ctx, ScopeInvocation, "scratch", json.RawMessage(`"x"`)); !errors.Is(err, storageErrClosed) {
		t.Errorf("Set() after Close = %v, want a closed error", err)
	}
	if err := scoped.Delete(ctx, ScopeInvocation, "scratch"); !errors.Is(err, storageErrClosed) {
		t.Errorf("Delete() after Close = %v, want a closed error", err)
	}
	if _, err := scoped.Keys(ctx, ScopeInvocation); !errors.Is(err, storageErrClosed) {
		t.Errorf("Keys() after Close = %v, want a closed error", err)
	}
	// Every scope is rejected, not only the invocation scope.
	if err := scoped.Set(ctx, ScopePlugin, "durable", json.RawMessage(`"x"`)); !errors.Is(err, storageErrClosed) {
		t.Errorf("Set(plugin scope) after Close = %v, want a closed error", err)
	}

	// The invocation-scoped value is gone, not merely hidden behind the
	// closed handle: reusing the invocation id must not resurrect it.
	reopened := store.ForInvocation("demo", "session-1", "invocation-1")
	if _, found := mustGet(t, reopened, ScopeInvocation, "scratch"); found {
		t.Error("Close must drop the invocation-scoped values")
	}
	// Plugin-scoped state survives an invocation ending.
	if value, found := mustGet(t, reopened, ScopePlugin, "durable"); !found || string(value) != `"kept"` {
		t.Errorf("plugin-scoped value = %s (found=%v), want it to survive Close", value, found)
	}
	reopened.Close()
}

func TestScopedStorageConcurrentUseAndClose(t *testing.T) {
	store := NewMemoryStorage()
	ctx := context.Background()
	scoped := store.ForInvocation("demo", "session-1", "invocation-1")

	const goroutines = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < goroutines; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for round := 0; round < 100; round++ {
				key := fmt.Sprintf("key-%d", worker)
				// Writes may succeed or fail once Close lands; what must never
				// happen is a panic, a data race, or a wrong-namespace write.
				if err := scoped.Set(ctx, ScopeInvocation, key, json.RawMessage(`"v"`)); err != nil && !errors.Is(err, storageErrClosed) {
					t.Errorf("Set() = %v, want nil or a closed error", err)
					return
				}
				if _, _, err := scoped.Get(ctx, ScopeInvocation, key); err != nil && !errors.Is(err, storageErrClosed) {
					t.Errorf("Get() = %v, want nil or a closed error", err)
					return
				}
				if _, err := scoped.Keys(ctx, ScopeInvocation); err != nil && !errors.Is(err, storageErrClosed) {
					t.Errorf("Keys() = %v, want nil or a closed error", err)
					return
				}
			}
		}(worker)
	}
	closer := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-closer
		scoped.Close()
	}()
	close(start)
	close(closer)
	wg.Wait()

	// After every writer finished and Close ran, the handle is closed and the
	// namespace is empty.
	if _, _, err := scoped.Get(ctx, ScopeInvocation, "key-0"); !errors.Is(err, storageErrClosed) {
		t.Errorf("Get() after concurrent Close = %v, want a closed error", err)
	}
	if _, found := mustGet(t, store.ForInvocation("demo", "session-1", "invocation-1"), ScopeInvocation, "key-0"); found {
		t.Error("a write landed after Close dropped the namespace")
	}
}

func TestScopedStorageRejectsInvalidIdentity(t *testing.T) {
	store := NewMemoryStorage()
	ctx := context.Background()

	cases := []struct {
		name       string
		plugin     string
		sessionID  string
		invocation string
	}{
		{"empty plugin", "", "session-1", "invocation-1"},
		{"empty invocation", "demo", "session-1", ""},
		{"plugin with separator", "de/mo", "session-1", "invocation-1"},
		{"invocation with separator", "demo", "session-1", "in/vocation"},
		{"invocation with traversal", "demo", "session-1", ".."},
		{"invocation with NUL", "demo", "session-1", "inv\x00ocation"},
		{"session with separator", "demo", "ses/sion", "invocation-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scoped := store.ForInvocation(tc.plugin, tc.sessionID, tc.invocation)
			defer scoped.Close()
			if err := scoped.Set(ctx, ScopeInvocation, "k", json.RawMessage(`1`)); err == nil {
				t.Fatal("Set() succeeded on an invalid invocation identity")
			}
			if _, _, err := scoped.Get(ctx, ScopeInvocation, "k"); err == nil {
				t.Fatal("Get() succeeded on an invalid invocation identity")
			}
			if _, err := scoped.Keys(ctx, ScopeInvocation); err == nil {
				t.Fatal("Keys() succeeded on an invalid invocation identity")
			}
		})
	}
	// An empty session id is allowed: an invocation may be session-less.
	scoped := store.ForInvocation("demo", "", "invocation-1")
	defer scoped.Close()
	mustSet(t, scoped, ScopeInvocation, "k", `1`)
	if _, found := mustGet(t, scoped, ScopeInvocation, "k"); !found {
		t.Error("a session-less invocation must still store values")
	}
}

func TestForPluginRejectsInvalidName(t *testing.T) {
	store := NewMemoryStorage()
	ctx := context.Background()
	for _, name := range []string{"", "   ", "a/b", "a\\b", "with\x00nul"} {
		storage := store.ForPlugin(name)
		if err := storage.Set(ctx, ScopePlugin, "k", json.RawMessage(`1`)); err == nil {
			t.Errorf("ForPlugin(%q).Set succeeded, want an error", name)
		}
		if _, _, err := storage.Get(ctx, ScopePlugin, "k"); err == nil {
			t.Errorf("ForPlugin(%q).Get succeeded, want an error", name)
		}
		if _, err := storage.Keys(ctx, ScopePlugin); err == nil {
			t.Errorf("ForPlugin(%q).Keys succeeded, want an error", name)
		}
	}
	// A plugin name with a space is odd but not a namespace escape.
	if err := store.ForPlugin("my plugin").Set(ctx, ScopePlugin, "k", json.RawMessage(`1`)); err != nil {
		t.Errorf("a plugin name with a space must be usable: %v", err)
	}
}

func TestStorageKeysAreSortedAndScoped(t *testing.T) {
	storage := NewMemoryStorage().ForPlugin("demo")
	for _, key := range []string{"zebra", "alpha", "middle"} {
		mustSet(t, storage, ScopeWorkspace, key, `1`)
	}
	keys, err := storage.Keys(context.Background(), ScopeWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "alpha,middle,zebra" {
		t.Errorf("Keys() = %v, want sorted order", keys)
	}
	// The returned slice must not alias internal state.
	keys[0] = "mutated"
	again, _ := storage.Keys(context.Background(), ScopeWorkspace)
	if again[0] != "alpha" {
		t.Error("Keys() returned a shared slice")
	}
}
