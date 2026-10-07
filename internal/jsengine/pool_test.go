package jsengine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRuntime records how it is used so the pool's lifecycle rules can be
// asserted directly rather than inferred from timing.
type fakeRuntime struct {
	id         int
	mu         sync.Mutex
	inUse      bool
	released   bool
	closed     bool
	loads      int
	interrupts atomic.Int32
	clears     atomic.Int32
	// callBlock blocks Call until released, to hold a runtime inside a call.
	callBlock chan struct{}
	// onCall runs at the start of every Call.
	onCall func()
	// callErr is returned by Call.
	callErr error
	// corrupt marks the runtime as unusable after a call.
	corrupt bool
}

func (f *fakeRuntime) Load(context.Context, Module) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	return nil
}

func (f *fakeRuntime) Call(ctx context.Context, _ Handler, args json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	if f.inUse {
		f.mu.Unlock()
		return nil, errors.New("runtime used concurrently")
	}
	f.inUse = true
	block := f.callBlock
	err := f.callErr
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inUse = false
		f.mu.Unlock()
	}()
	if hook := f.onCall; hook != nil {
		hook()
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), args...), nil
}

func (f *fakeRuntime) SetGlobal(string, GlobalObject) error { return nil }

func (f *fakeRuntime) ValidateHandler(context.Context, Handler) error { return nil }

func (f *fakeRuntime) ReleaseCallData() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = true
}
func (f *fakeRuntime) Interrupt(any)   { f.interrupts.Add(1) }
func (f *fakeRuntime) ClearInterrupt() { f.clears.Add(1) }
func (f *fakeRuntime) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

type fakeEngine struct {
	mu        sync.Mutex
	created   int
	runtimes  []*fakeRuntime
	createErr error
	// onCall runs inside every fakeRuntime.Call, to hold a call open.
	onCall func()
}

func (e *fakeEngine) Name() string { return "fake" }

func (e *fakeEngine) Compile(name, source string) (Module, error) {
	return &fakeModule{name: name}, nil
}

func (e *fakeEngine) Link(entry Module, _ Resolver) (Module, error) { return entry, nil }

func (e *fakeEngine) NewRuntime(RuntimeOptions) (Runtime, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.createErr != nil {
		return nil, e.createErr
	}
	rt := &fakeRuntime{id: e.created, onCall: e.onCall}
	e.created++
	e.runtimes = append(e.runtimes, rt)
	return rt, nil
}

func (e *fakeEngine) snapshot() (int, []*fakeRuntime) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.created, append([]*fakeRuntime(nil), e.runtimes...)
}

type fakeModule struct{ name string }

func (m *fakeModule) Name() string                    { return m.name }
func (m *fakeModule) Exports() []string               { return nil }
func (m *fakeModule) Requests() []string              { return nil }
func (m *fakeModule) Handler(string) (Handler, error) { return fakeHandler{}, nil }

type fakeHandler struct{}

func (fakeHandler) Name() string { return "run" }

func newTestPool(t *testing.T, engine Engine, capacity int) *PooledModule {
	t.Helper()
	module, err := engine.Compile("test.js", "")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPooledModule(engine, module, RuntimeOptions{}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

// TestOneRuntimeIsNeverUsedConcurrently is the core pool invariant.
func TestOneRuntimeIsNeverUsedConcurrently(t *testing.T) {
	engine := &fakeEngine{}
	pool := newTestPool(t, engine, 4)

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pool.Invoke(context.Background(), fakeHandler{}, []byte(`{"a":1}`)); err != nil {
				t.Errorf("Invoke: %v", err)
			}
		}()
	}
	wg.Wait()

	created, runtimes := engine.snapshot()
	if created > 4 {
		t.Fatalf("pool created %d runtimes, capacity is 4", created)
	}
	for _, rt := range runtimes {
		if rt.inUse {
			t.Error("runtime left marked in use")
		}
	}
	// After every call returned, all runtimes must be idle again.
	if stats := pool.Stats(); stats.Idle != created {
		t.Errorf("stats = %+v, want Idle == %d", stats, created)
	}
}

// TestConcurrentInvocationsRunInParallel proves the pool does not serialise
// plugin calls behind one global runtime: two runtimes must be inside a call
// at the same time.
func TestConcurrentInvocationsRunInParallel(t *testing.T) {
	engine := &fakeEngine{}
	pool := newTestPool(t, engine, 4)

	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	engine.onCall = func() {
		entered <- struct{}{}
		<-release
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pool.Invoke(context.Background(), fakeHandler{}, []byte(`{}`)); err != nil {
				t.Errorf("Invoke: %v", err)
			}
		}()
	}

	// Two runtimes must reach the barrier before either is allowed to finish.
	// With a global lock only one would ever get there and this times out.
	deadline := time.After(3 * time.Second)
	for seen := 0; seen < 2; {
		select {
		case <-entered:
			seen++
		case <-deadline:
			close(release)
			t.Fatalf("only %d runtime(s) ran concurrently; plugin calls are serialised", seen)
		}
	}
	close(release)
	wg.Wait()
}

func TestPoolDiscardsCorruptedRuntime(t *testing.T) {
	engine := &fakeEngine{}
	pool := newTestPool(t, engine, 2)

	rt, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats := pool.Stats(); stats.InFlight != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	pool.Release(rt, true)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pool.Stats().InFlight == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if stats := pool.Stats(); stats.InFlight != 0 {
		t.Fatalf("corrupted runtime was not discarded: %+v", stats)
	}
	// The pool still works afterwards.
	if _, err := pool.Invoke(context.Background(), fakeHandler{}, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("Invoke after discard: %v", err)
	}
}

// TestPoolCancellationDoesNotDeadlock holds the pool at capacity and proves
// that waiting for a runtime ends on context cancellation, and that the pool
// keeps working afterwards.
func TestPoolCancellationDoesNotDeadlock(t *testing.T) {
	engine := &fakeEngine{}
	pool := newTestPool(t, engine, 1)

	// Occupy the only runtime through Invoke, so the permit accounting stays
	// exactly as production uses it.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	engine.onCall = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	held := make(chan error, 1)
	go func() {
		_, err := pool.Invoke(context.Background(), fakeHandler{}, []byte(`{}`))
		held <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the first invocation never reached the runtime")
	}

	// The pool is at capacity: waiting must end when the context does.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := pool.Invoke(ctx, fakeHandler{}, []byte(`{}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for a busy pool returned %v, want DeadlineExceeded", err)
	}

	close(release)
	if err := <-held; err != nil {
		t.Fatalf("first invocation failed: %v", err)
	}
	// The permit must have come back: a cancelled waiter must not consume one.
	engine.onCall = nil
	if _, err := pool.Invoke(context.Background(), fakeHandler{}, []byte(`{}`)); err != nil {
		t.Fatalf("Invoke after cancellation: %v", err)
	}
	if stats := pool.Stats(); stats.InFlight != 0 || stats.Idle != 1 {
		t.Fatalf("stats = %+v, want one idle runtime and nothing in flight", stats)
	}
}

// TestAcquireReturnsPermitWhenCreationFails makes sure a runtime that never
// came into existence does not permanently shrink the pool.
func TestAcquireReturnsPermitWhenCreationFails(t *testing.T) {
	engine := &fakeEngine{createErr: errors.New("cannot create runtime")}
	pool := newTestPool(t, engine, 2)

	for range 3 {
		if _, err := pool.Acquire(context.Background()); err == nil {
			t.Fatal("Acquire must fail when the engine cannot create a runtime")
		}
	}
	if stats := pool.Stats(); stats.InFlight != 0 {
		t.Fatalf("stats = %+v, want no runtimes in flight", stats)
	}
	engine.mu.Lock()
	engine.createErr = nil
	engine.mu.Unlock()
	if _, err := pool.Invoke(context.Background(), fakeHandler{}, []byte(`{}`)); err != nil {
		t.Fatalf("pool did not recover after a creation failure: %v", err)
	}
}

func TestPoolCloseReleasesAndRefuses(t *testing.T) {
	engine := &fakeEngine{}
	pool := newTestPool(t, engine, 2)
	if _, err := pool.Invoke(context.Background(), fakeHandler{}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	created, runtimes := engine.snapshot()
	if created != 1 {
		t.Fatalf("created = %d", created)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	for _, rt := range runtimes {
		if !rt.closed {
			t.Error("Close left a runtime open")
		}
	}
	if _, err := pool.Invoke(context.Background(), fakeHandler{}, []byte(`{}`)); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("Invoke after Close = %v, want ErrPoolClosed", err)
	}
	if stats := pool.Stats(); stats.InFlight != 0 || !stats.Closed {
		t.Fatalf("stats after Close = %+v", stats)
	}
}

func TestPoolReleaseClearsPendingInterrupt(t *testing.T) {
	engine := &fakeEngine{}
	pool := newTestPool(t, engine, 1)
	rt, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pool.Release(rt, false)
	if got := rt.(*fakeRuntime).clears.Load(); got == 0 {
		t.Error("Release must clear a pending interrupt before reusing the runtime")
	}
	if got := rt.(*fakeRuntime).released; !got {
		t.Error("Release must drop call data")
	}
}

func TestPoolWaitsForReleaseRatherThanGrowingPastCapacity(t *testing.T) {
	engine := &fakeEngine{}
	pool := newTestPool(t, engine, 1)
	rt, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := pool.Invoke(context.Background(), fakeHandler{}, []byte(`{}`))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("second invocation returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	pool.Release(rt, false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second invocation failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second invocation never ran after release")
	}
	created, _ := engine.snapshot()
	if created != 1 {
		t.Fatalf("pool created %d runtimes with capacity 1", created)
	}
}

func TestCallWithContextInterruptsRunningCall(t *testing.T) {
	engine := &fakeEngine{}
	pool := newTestPool(t, engine, 1)
	rt, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Release(rt, false)

	block := make(chan struct{})
	fake := rt.(*fakeRuntime)
	fake.callBlock = block
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err = CallWithContext(ctx, rt, fakeHandler{}, []byte(`{}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CallWithContext = %v, want context.Canceled", err)
	}
	if fake.interrupts.Load() == 0 {
		t.Error("cancellation must reach the engine as an interrupt")
	}
	close(block)
}

func TestPoolCapacityIsBounded(t *testing.T) {
	engine := &fakeEngine{}
	pool := newTestPool(t, engine, MaxPoolCapacity*4)
	if pool.Capacity() != MaxPoolCapacity {
		t.Fatalf("Capacity = %d, want %d", pool.Capacity(), MaxPoolCapacity)
	}
	zero := newTestPool(t, &fakeEngine{}, 0)
	if zero.Capacity() != DefaultPoolCapacity {
		t.Fatalf("Capacity = %d, want %d", zero.Capacity(), DefaultPoolCapacity)
	}
}

// TestPoolReportsDiscardedRuntimes proves a discard is observable.
//
// Replacing a damaged runtime silently would hide the plugin bug that damaged
// it, so the pool reports the reason through a callback.
func TestPoolReportsDiscardedRuntimes(t *testing.T) {
	engine := &fakeEngine{}
	pool := newTestPool(t, engine, 2)

	var mu sync.Mutex
	var reasons []string
	pool.OnDiscard(func(reason string) {
		mu.Lock()
		defer mu.Unlock()
		reasons = append(reasons, reason)
	})

	rt, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pool.Release(rt, true)

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		count := len(reasons)
		mu.Unlock()
		if count > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a corrupted runtime was discarded without reporting it")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if reasons[0] != "corrupted" {
		t.Fatalf("reason = %q, want %q", reasons[0], "corrupted")
	}
}

// TestPoolReportsRuntimesDroppedByClose covers the other discard path: a
// runtime released after the pool closed.
func TestPoolReportsRuntimesDroppedByClose(t *testing.T) {
	engine := &fakeEngine{}
	pool := newTestPool(t, engine, 2)

	var mu sync.Mutex
	var reasons []string
	pool.OnDiscard(func(reason string) {
		mu.Lock()
		defer mu.Unlock()
		reasons = append(reasons, reason)
	})

	rt, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	// Releasing after Close must close the runtime and say why.
	pool.Release(rt, false)

	mu.Lock()
	defer mu.Unlock()
	if len(reasons) != 1 || reasons[0] != "pool closed" {
		rt.(*fakeRuntime).mu.Lock()
		closed := rt.(*fakeRuntime).closed
		rt.(*fakeRuntime).mu.Unlock()
		t.Fatalf("reasons = %v (runtime closed = %v), want one %q", reasons, closed, "pool closed")
	}
}
