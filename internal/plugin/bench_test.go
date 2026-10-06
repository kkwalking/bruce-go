package plugin

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"bruce-go/internal/jsengine"
	"bruce-go/internal/jsengine/moejs"
	"bruce-go/internal/tool"
)

// Benchmarks for the JavaScript plugin system, covering docs/plugin.md
// section 34 ("Performance Expectations"): plugin load, warm invocation,
// concurrent invocation, runtime pool acquire/release and Go <-> JS nested
// JSON conversion.
//
// No benchmark asserts a performance threshold. A wall-clock budget would fail
// on a slow or loaded CI machine while saying nothing about this code; what the
// numbers have to show is the *shape* of the cost, and that is read off the
// report rather than enforced here. The correctness of the path being measured
// is asserted once, before the timer starts, so a benchmark can never quietly
// time a call that fails.
//
// Run them with:
//
//	go test -run '^$' -bench . -benchtime 200x ./internal/plugin/

// benchT returns the *testing.T the fixtures in manager_test.go require.
//
// newManagerFixture, writeFixture and managerFixture.write take a *testing.T,
// and a benchmark cannot construct a real one. A zero-value testing.T supports
// exactly what those helpers use — Helper, TempDir, Cleanup and Fatal — so the
// fixtures are reused verbatim here instead of being copied into this file,
// which is what keeps the benchmark honest: it measures the same wiring the
// tests exercise. Its cleanup never runs, so every benchmark below registers
// its own teardown on the *testing.B.
func benchT() *testing.T { return &testing.T{} }

// benchFixture builds a manager fixture for a benchmark and closes the manager
// when the benchmark (or sub-benchmark) ends.
func benchFixture(b *testing.B, policy HostPolicy) managerFixture {
	b.Helper()
	fixture := newManagerFixture(benchT(), policy)
	b.Cleanup(func() { _ = fixture.manager.Close() })
	return fixture
}

// benchOptions mirrors the wiring newManagerFixture uses, for the benchmarks
// that need a fresh Manager per iteration (a cold Load) rather than the single
// fixture manager.
//
// The observer is a no-op rather than the fixture's eventRecorder: production
// installs one (internal/integrated/runtime.go), so leaving it nil would
// benchmark a wiring that does not exist, while the recorder would accumulate
// every event of every iteration and grow the heap for reasons that have
// nothing to do with loading.
func benchOptions(fixture managerFixture, policy HostPolicy) ManagerOptions {
	return ManagerOptions{
		Engine:    moejs.Engine{},
		Workspace: fixture.workspace,
		HomeDir:   fixture.home,
		Policy:    policy,
		Sandbox:   openSandbox(),
		Observer:  ObserverFunc(func(string, map[string]any) {}),
	}
}

// benchEchoTool is the tool name echoManifest and warmManifestTemplate declare.
const benchEchoTool = "echo_run"

// benchWarmArgs is a nested argument object that satisfies echoManifest's
// schema: a required string plus a nested object, an array and a null.
const benchWarmArgs = `{"query":"hello","options":{"recursive":true,"depth":3},"files":["a.go","b.go"],"nothing":null}`

// warmManifestTemplate is echoManifest plus an explicit runtime pool capacity,
// so a concurrency benchmark can tell the pool's bound apart from a global
// JavaScript lock. A capacity of 0 means the host default.
const warmManifestTemplate = `{
  "apiVersion": "bruce.plugin/v1",
  "name": "%s",
  "version": "1.0.0",
  "description": "Echoes structured input back",
  "entry": "index.js",
  "concurrency": {"maxRuntimes": %d},
  "tools": [
    {
      "name": "%s_run",
      "description": "Echo the arguments back",
      "handler": "run",
      "promptSnippet": "Echo structured input",
      "schema": {
        "type": "object",
        "properties": {
          "query": {"type": "string"},
          "options": {"type": "object"},
          "files": {"type": "array"},
          "nothing": {"type": "null"}
        },
        "required": ["query"]
      }
    }
  ]
}`

func warmManifest(name string, maxRuntimes int) string {
	return fmt.Sprintf(warmManifestTemplate, name, maxRuntimes, name)
}

// benchEchoModule is the smallest module that exercises the whole JSON
// boundary: arguments in, the same value out.
const benchEchoModule = `export function echo(input) { return input; }`

// benchFlatArgs and benchNestedArgs carry the same ten scalar leaves — a
// string, a boolean, a number, a float, a two-element array, two integers, a
// boolean and a null. The only difference is shape: flat spreads them over nine
// top-level keys, nested buries six of them behind three levels of object and
// array. That isolates the cost of nesting from the cost of the data.
const (
	benchFlatArgs   = `{"query":"hello","recursive":true,"depth":3,"ratio":0.5,"tags":["a","b"],"one":1,"two":2,"three":true,"four":null}`
	benchNestedArgs = `{"query":"hello","options":{"recursive":true,"depth":3,"ratio":0.5,"tags":["a","b"],"inner":{"deep":{"deeper":[1,2,{"three":true,"four":null}]}}}}`
)

// benchMediumModule stands in for a real plugin: five exports, closures,
// destructuring, spread, a template literal, a nullish default and a loop. It
// is the "one medium sized module" BenchmarkModuleCompile measures, so the
// compile cost can be compared against the warm invocation cost.
const benchMediumModule = `
const LEVELS = { debug: 0, info: 1, warn: 2, error: 3 };

export function summarize(entries) {
  const byLevel = {};
  let total = 0;
  for (const entry of entries) {
    const level = entry.level || "info";
    byLevel[level] = (byLevel[level] || 0) + 1;
    total += entry.size || 0;
  }
  return { total, byLevel, count: entries.length };
}

export function filter(entries, minimum) {
  const floor = LEVELS[minimum] ?? 0;
  return entries.filter((entry) => (LEVELS[entry.level] ?? 0) >= floor);
}

export function format(entries) {
  return entries
    .map(({ level, message, size }) => level + ": " + message + " (" + (size ?? 0) + "B)")
    .join("\n");
}

export function chunk(values, size) {
  const out = [];
  for (let i = 0; i < values.length; i += size) {
    out.push(values.slice(i, i + size));
  }
  return out;
}

export function merge(base, extra) {
  const merged = { ...base };
  for (const [key, value] of Object.entries(extra || {})) {
    merged[key] = value;
  }
  return merged;
}
`

// compileForBench, handlerForBench and runtimeForBench are the benchmark-side
// equivalents of the helpers in internal/jsengine/moejs/adapter_test.go. Those
// live in package moejs_test and cannot be imported from here, so the small
// equivalents are repeated rather than the other package's file being changed.
func compileForBench(b *testing.B, name, source string) jsengine.Module {
	b.Helper()
	module, err := moejs.Engine{}.Compile(name, source)
	if err != nil {
		b.Fatalf("Compile(%s): %v", name, err)
	}
	return module
}

func handlerForBench(b *testing.B, module jsengine.Module, path string) jsengine.Handler {
	b.Helper()
	handler, err := module.Handler(path)
	if err != nil {
		b.Fatalf("Handler(%q): %v", path, err)
	}
	return handler
}

func runtimeForBench(b *testing.B, module jsengine.Module) jsengine.Runtime {
	b.Helper()
	rt, err := moejs.Engine{}.NewRuntime(jsengine.RuntimeOptions{DisableDynamicCode: true})
	if err != nil {
		b.Fatalf("NewRuntime: %v", err)
	}
	b.Cleanup(func() { _ = rt.Close() })
	if err := rt.Load(context.Background(), module); err != nil {
		b.Fatalf("Load: %v", err)
	}
	return rt
}

// BenchmarkPluginLoad measures one complete Manager.Load(): discovery, manifest
// validation, compile, link, runtime pool creation, the warm validation pass
// and tool registration.
//
// A fresh Manager is built per iteration, so every measured Load is a cold
// load rather than a reload that also has to tear the previous generation
// down. Manager construction and Close are inside the loop and are part of the
// number; they are microseconds against a load that compiles a module and
// creates a runtime.
func BenchmarkPluginLoad(b *testing.B) {
	// The fixture supplies the workspace, the home directory and the policy;
	// the plugin is written with the same helper the lifecycle tests use.
	host := benchFixture(b, permissivePolicy())
	host.write(benchT(), pluginFixture{
		name: "echo", manifest: echoManifest,
		files: map[string]string{"index.js": echoEntry},
	})
	opts := benchOptions(host, permissivePolicy())
	// One registry is reused across iterations: Load registers into it and
	// Close unregisters, so each iteration starts from an empty registry.
	registry := tool.EmptyRegistry(host.workspace)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		manager, err := NewManager(opts)
		if err != nil {
			b.Fatalf("NewManager: %v", err)
		}
		manager.WithRegistry(registry)
		if err := manager.Load(ctx); err != nil {
			_ = manager.Close()
			b.Fatalf("Load: %v", err)
		}
		// Registration is part of what a load is for, so a load that silently
		// registered nothing must not be timed as a success.
		if _, ok := registry.Lookup(benchEchoTool); !ok {
			_ = manager.Close()
			b.Fatalf("Load did not register %s", benchEchoTool)
		}
		if err := manager.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

// BenchmarkWarmInvocation measures one already-loaded plugin tool call through
// the tool registry — the path the agent actually drives.
//
// This is the number that proves the warm path does not recompile: it goes
// through ExecuteJSON (argument parsing, schema validation, policy checks), the
// manager's lookup, the runtime pool and one JavaScript call, and it must land
// orders of magnitude below BenchmarkPluginLoad and BenchmarkModuleCompile.
func BenchmarkWarmInvocation(b *testing.B) {
	fixture := benchFixture(b, permissivePolicy())
	fixture.write(benchT(), pluginFixture{
		name: "echo", manifest: echoManifest,
		files: map[string]string{"index.js": echoEntry},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		b.Fatalf("Load: %v", err)
	}

	// Asserted once, before the timer: the loop must time a call that works.
	if out := fixture.registry.ExecuteJSON(context.Background(), benchEchoTool, benchWarmArgs); !strings.Contains(out, `"depth": 3`) {
		b.Fatalf("warm invocation did not round-trip the nested argument: %s", out)
	}

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	// The result is checked, not just discarded: a benchmark that silently
	// times failing calls reports a wonderful number for code that never ran.
	for i := 0; i < b.N; i++ {
		if !strings.Contains(fixture.registry.ExecuteJSON(ctx, benchEchoTool, benchWarmArgs), `"depth": 3`) {
			b.Fatal("warm invocation did not round-trip the nested argument")
		}
	}
}

// benchParallelRuntimes is the pool capacity of the "wide" variant below. It
// is above the host default (jsengine.DefaultPoolCapacity, 4) and above
// GOMAXPROCS on a normal machine, so the pool cannot be what bounds the
// scaling being measured.
const benchParallelRuntimes = 16

// BenchmarkWarmInvocationParallel measures the same call under b.RunParallel.
//
// Two capacities are measured because they answer different questions:
//
//   - "default" uses the production pool (4 runtimes). If a global JavaScript
//     lock serialised calls, this would scale like the single-threaded number
//     up to the pool bound; scaling towards 4x instead shows the pool is the
//     only ceiling.
//   - "wide" raises the pool above GOMAXPROCS, so the remaining ceiling is the
//     engine itself rather than the pool.
//
// The ns/op of a parallel benchmark is per-operation wall time, so comparing it
// with BenchmarkWarmInvocation gives the speedup directly.
func BenchmarkWarmInvocationParallel(b *testing.B) {
	b.Run("default", func(b *testing.B) { benchWarmParallel(b, 0) })
	b.Run("wide", func(b *testing.B) { benchWarmParallel(b, benchParallelRuntimes) })
}

func benchWarmParallel(b *testing.B, maxRuntimes int) {
	fixture := benchFixture(b, permissivePolicy())
	fixture.write(benchT(), pluginFixture{
		name: "echo", manifest: warmManifest("echo", maxRuntimes),
		files: map[string]string{"index.js": echoEntry},
	})
	if err := fixture.manager.Load(context.Background()); err != nil {
		b.Fatalf("Load: %v", err)
	}
	if out := fixture.registry.ExecuteJSON(context.Background(), benchEchoTool, benchWarmArgs); !strings.Contains(out, `"depth": 3`) {
		b.Fatalf("warm invocation did not round-trip the nested argument: %s", out)
	}

	// Counted rather than failed inline: b.Fatal must not be called from a
	// RunParallel goroutine, and an empty result is cheap to spot.
	var failures atomic.Int64
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if !strings.Contains(fixture.registry.ExecuteJSON(ctx, benchEchoTool, benchWarmArgs), `"depth": 3`) {
				failures.Add(1)
			}
		}
	})
	b.StopTimer()
	if count := failures.Load(); count != 0 {
		b.Errorf("%d concurrent invocations did not round-trip the nested argument", count)
	}
}

// BenchmarkRuntimePoolAcquireRelease measures the pool on its own, with no
// JavaScript call in the loop.
//
// The two variants separate the two costs the pool can have:
//
//   - "reuse" keeps one runtime idle, so the loop measures the acquire/release
//     bookkeeping a warm plugin pays per call (channel permit, idle slice,
//     ClearInterrupt, ReleaseCallData).
//   - "discard" releases the runtime as corrupted, which closes it, so every
//     acquire has to create and evaluate a fresh runtime. That is the cost the
//     pool exists to avoid paying per call.
func BenchmarkRuntimePoolAcquireRelease(b *testing.B) {
	module := compileForBench(b, "bench.js", benchEchoModule)
	engine := moejs.Engine{}

	newPool := func(b *testing.B, capacity int) *jsengine.PooledModule {
		b.Helper()
		pool, err := jsengine.NewPooledModule(engine, module, jsengine.RuntimeOptions{DisableDynamicCode: true}, capacity)
		if err != nil {
			b.Fatalf("NewPooledModule: %v", err)
		}
		b.Cleanup(func() { _ = pool.Close() })
		return pool
	}

	b.Run("reuse", func(b *testing.B) {
		// Capacity 1: a single runtime, created once before the timer, so the
		// loop always takes the idle path.
		pool := newPool(b, 1)
		ctx := context.Background()
		warm, err := pool.Acquire(ctx)
		if err != nil {
			b.Fatalf("Acquire: %v", err)
		}
		pool.Release(warm, false)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rt, err := pool.Acquire(ctx)
			if err != nil {
				b.Fatalf("Acquire: %v", err)
			}
			pool.Release(rt, false)
		}
		b.StopTimer()
		if stats := pool.Stats(); stats.InFlight != 0 || stats.Idle != 1 {
			b.Fatalf("pool leaked a runtime: %+v", stats)
		}
	})

	b.Run("discard", func(b *testing.B) {
		// corrupted=true is the documented "this runtime must not be reused"
		// path, so each iteration discards the runtime and the next Acquire
		// creates one. That is runtime creation plus module evaluation.
		pool := newPool(b, 1)
		ctx := context.Background()

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rt, err := pool.Acquire(ctx)
			if err != nil {
				b.Fatalf("Acquire: %v", err)
			}
			pool.Release(rt, true)
		}
		b.StopTimer()
		if stats := pool.Stats(); stats.InFlight != 0 || stats.Idle != 0 {
			b.Fatalf("discarded runtimes were kept: %+v", stats)
		}
	})
}

// BenchmarkNestedJSONConversion measures only the Go <-> JS JSON boundary, with
// no plugin manager, no policy and no pool in the way: one Runtime, one handler
// that returns its argument unchanged, and Runtime.Call around it. That is the
// conversion itself — CallWithContext only adds a watcher goroutine, and the
// pool benchmark already covers the acquire/release around a call.
//
// "flat" and "nested" carry the same ten scalar leaves (see benchFlatArgs and
// benchNestedArgs); the delta between the two is the cost of depth alone —
// three extra object/array levels that both directions have to walk.
func BenchmarkNestedJSONConversion(b *testing.B) {
	module := compileForBench(b, "bench.js", benchEchoModule)
	rt := runtimeForBench(b, module)
	handler := handlerForBench(b, module, "echo")
	ctx := context.Background()

	run := func(b *testing.B, args string) {
		b.Helper()
		out, err := rt.Call(ctx, handler, []byte(args))
		if err != nil {
			b.Fatalf("Call: %v", err)
		}
		// Both directions are exercised: the argument goes in as bytes and the
		// result comes back as bytes, so a conversion that dropped the nesting
		// would show up here rather than as a suspiciously fast number.
		if !strings.Contains(string(out), `"hello"`) {
			b.Fatalf("Call did not return the argument: %s", out)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := rt.Call(ctx, handler, []byte(args)); err != nil {
				b.Fatalf("Call: %v", err)
			}
		}
	}

	b.Run("flat", func(b *testing.B) { run(b, benchFlatArgs) })
	b.Run("nested", func(b *testing.B) { run(b, benchNestedArgs) })
}

// BenchmarkModuleCompile measures moejs.Engine.Compile alone on a medium sized
// module — parse and bytecode compile, no link, no runtime, no call.
//
// It is the control for the "compile happens once" claim: the warm invocation
// number must be orders of magnitude below this one, which is only possible if
// the compiled module is kept and reused instead of being rebuilt per call.
func BenchmarkModuleCompile(b *testing.B) {
	engine := moejs.Engine{}
	if _, err := engine.Compile("bench.js", benchMediumModule); err != nil {
		b.Fatalf("Compile: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := engine.Compile("bench.js", benchMediumModule); err != nil {
			b.Fatalf("Compile: %v", err)
		}
	}
}
