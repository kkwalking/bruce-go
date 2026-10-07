package jsengine

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// DefaultPoolCapacity is the number of runtimes one pooled module keeps.
//
// A plugin tool call is short and CPU-bound, so a handful of runtimes is
// enough to keep several concurrent tool calls off each other's critical path
// without multiplying memory per plugin. Runtimes are created on demand, so an
// idle plugin costs one runtime, not the full capacity.
const DefaultPoolCapacity = 4

// MaxPoolCapacity bounds what a manifest may ask for. A plugin that requested
// thousands of runtimes would be a memory-exhaustion vector.
const MaxPoolCapacity = 64

var (
	// ErrPoolClosed is returned once a pool has been closed.
	ErrPoolClosed = errors.New("jsengine: runtime pool is closed")
)

// PooledModule is a module with a pool of runtimes that serve its calls.
//
// The lifecycle rule this type exists to enforce: one runtime serves one call
// at a time. Runtimes are never shared between concurrent calls, and a runtime
// that failed in a way that left it unsafe is discarded rather than reused.
//
// Capacity is a buffered channel of permits rather than a condition variable.
// A permit is taken to run a call and given back when the call ends, so the
// bound is enforced by the channel itself: waiting is a plain select, which
// composes with context cancellation and cannot lose a wakeup.
type PooledModule struct {
	engine   Engine
	module   Module
	opts     RuntimeOptions
	capacity int

	permits chan struct{}

	mu     sync.Mutex
	idle   []Runtime
	closed bool

	closeOnce sync.Once
	closedCh  chan struct{}

	// onDiscard, when set, is told why a runtime was thrown away. Discarding a
	// runtime is the pool's most consequential decision, so it must be
	// observable rather than silent.
	onDiscard func(reason string)
}

// OnDiscard registers a callback invoked whenever a runtime is discarded
// instead of being returned to the idle list. It is called at most once per
// discarded runtime and must not block.
func (p *PooledModule) OnDiscard(fn func(reason string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onDiscard = fn
}

func (p *PooledModule) notifyDiscard(reason string) {
	p.mu.Lock()
	callback := p.onDiscard
	p.mu.Unlock()
	if callback != nil {
		callback(reason)
	}
}

// NewPooledModule creates a pool for one compiled module.
func NewPooledModule(engine Engine, module Module, opts RuntimeOptions, capacity int) (*PooledModule, error) {
	if engine == nil {
		return nil, errors.New("jsengine: engine must not be nil")
	}
	if module == nil {
		return nil, errors.New("jsengine: module must not be nil")
	}
	if capacity <= 0 {
		capacity = DefaultPoolCapacity
	}
	if capacity > MaxPoolCapacity {
		capacity = MaxPoolCapacity
	}
	pool := &PooledModule{
		engine:   engine,
		module:   module,
		opts:     opts,
		capacity: capacity,
		permits:  make(chan struct{}, capacity),
		closedCh: make(chan struct{}),
	}
	for range capacity {
		pool.permits <- struct{}{}
	}
	return pool, nil
}

// Module returns the pooled module.
func (p *PooledModule) Module() Module { return p.module }

// Capacity returns the maximum number of runtimes.
func (p *PooledModule) Capacity() int { return p.capacity }

// Stats reports pool occupancy, for status output and tests.
type Stats struct {
	// Idle is the number of runtimes waiting in the pool.
	Idle int
	// InFlight is the number of permits currently held, that is the number of
	// calls running right now.
	InFlight int
	// Capacity is the maximum number of concurrent calls.
	Capacity int
	Closed   bool
}

func (p *PooledModule) Stats() Stats {
	p.mu.Lock()
	idle := len(p.idle)
	closed := p.closed
	p.mu.Unlock()
	return Stats{Idle: idle, InFlight: p.capacity - len(p.permits), Capacity: p.capacity, Closed: closed}
}

// Acquire returns a runtime that no other caller is using.
//
// The runtime is already loaded with the module. When every runtime is busy,
// Acquire waits for one to be released, the context to end, or the pool to
// close.
func (p *PooledModule) Acquire(ctx context.Context) (Runtime, error) {
	select {
	case <-p.closedCh:
		return nil, ErrPoolClosed
	case <-p.permits:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// A permit is held from here on; every path below returns it exactly once.
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.returnPermit()
		return nil, ErrPoolClosed
	}
	if count := len(p.idle); count > 0 {
		rt := p.idle[count-1]
		p.idle = p.idle[:count-1]
		p.mu.Unlock()
		return rt, nil
	}
	p.mu.Unlock()

	rt, err := p.create()
	if err != nil {
		p.returnPermit()
		return nil, err
	}
	return rt, nil
}

// create builds a runtime and loads the module into it.
//
// Loading evaluates the module's top level, which can run arbitrary code, so
// it is bounded by the pool's own lifetime rather than by a caller context: a
// module that fails to load must fail for every caller, not only for the one
// whose context happened to expire.
func (p *PooledModule) create() (Runtime, error) {
	rt, err := p.engine.NewRuntime(p.opts)
	if err != nil {
		return nil, err
	}
	if err := rt.Load(context.Background(), p.module); err != nil {
		_ = rt.Close()
		return nil, err
	}
	return rt, nil
}

func (p *PooledModule) returnPermit() {
	select {
	case p.permits <- struct{}{}:
	default:
		// Unreachable while Release and Acquire stay balanced. Dropping a
		// permit here would permanently shrink the pool, so report it loudly
		// rather than failing silently.
		panic("jsengine: runtime pool permit accounting is unbalanced")
	}
}

// Release returns a runtime to the pool.
//
// Every exit path of Invoke goes through Release, so the pool cannot leak a
// runtime and the "one runtime, one caller" rule is enforced by construction.
// Pass corrupted=true when the call failed in a way that left the runtime
// unsafe: the runtime is closed and its permit is returned, so the next caller
// gets a fresh runtime instead of a damaged one.
func (p *PooledModule) Release(rt Runtime, corrupted bool) {
	if rt == nil {
		return
	}
	if corrupted {
		_ = rt.Close()
		p.returnPermit()
		p.notifyDiscard("corrupted")
		return
	}
	// A pending interrupt must never reach the next caller: an interrupt that
	// arrived after a call finished would stop an unrelated later call.
	rt.ClearInterrupt()
	rt.ReleaseCallData()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = rt.Close()
		p.returnPermit()
		p.notifyDiscard("pool closed")
		return
	}
	p.idle = append(p.idle, rt)
	p.mu.Unlock()
	p.returnPermit()
}

// Close discards every idle runtime and refuses further acquisitions.
//
// A runtime currently serving a call is closed when it is released, so Close
// never interrupts work in flight and never leaks.
func (p *PooledModule) Close() error {
	var idle []Runtime
	p.closeOnce.Do(func() {
		close(p.closedCh)
		p.mu.Lock()
		p.closed = true
		idle = p.idle
		p.idle = nil
		p.mu.Unlock()
	})
	var errs []error
	for _, rt := range idle {
		if err := rt.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Invoke runs one handler call on a pooled runtime.
//
// It is the single entry point the plugin manager uses, and it guarantees the
// lifecycle: acquire, call with cancellation bridging, release (or discard),
// on every path including a panic.
func (p *PooledModule) Invoke(ctx context.Context, h Handler, args []byte) (out []byte, err error) {
	rt, err := p.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	corrupted := false
	defer func() {
		if recovered := recover(); recovered != nil {
			corrupted = true
			err = fmt.Errorf("jsengine: panic during plugin call: %v", recovered)
		}
		p.Release(rt, corrupted)
	}()
	out, err = CallWithContext(ctx, rt, h, args)
	if err != nil {
		corrupted = IsCorrupted(err)
	}
	return out, err
}
