package moejs_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"bruce-go/internal/jsengine"
	"bruce-go/internal/jsengine/moejs"
)

func compile(t *testing.T, name, source string) jsengine.Module {
	t.Helper()
	module, err := moejs.Engine{}.Compile(name, source)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return module
}

func handler(t *testing.T, module jsengine.Module, path string) jsengine.Handler {
	t.Helper()
	h, err := module.Handler(path)
	if err != nil {
		t.Fatalf("Handler(%q): %v", path, err)
	}
	return h
}

func newRuntime(t *testing.T, opts jsengine.RuntimeOptions) jsengine.Runtime {
	t.Helper()
	rt, err := moejs.Engine{}.NewRuntime(opts)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func TestEngineName(t *testing.T) {
	if got := (moejs.Engine{}).Name(); got != moejs.EngineName {
		t.Fatalf("Name = %q", got)
	}
}

// TestNestedJSONRoundTripsWithoutLoss is the type-fidelity contract: a tool
// argument object must reach JavaScript with its structure and types intact.
func TestNestedJSONRoundTripsWithoutLoss(t *testing.T) {
	module := compile(t, "plugin.js", `
export function echo(input) {
  return {
    query: input.query,
    recursive: input.options.recursive,
    depth: input.options.depth,
    ratio: input.options.ratio,
    files: input.files,
    nested: { deep: { deeper: [1, 2, { three: true }] } },
    nothing: input.nothing,
    empty: input.empty,
    big: input.big,
  };
}
`)
	rt := newRuntime(t, jsengine.RuntimeOptions{DisableDynamicCode: true})
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatalf("Load: %v", err)
	}
	args := []byte(`{
      "query": "hello",
      "options": {"recursive": true, "depth": 3, "ratio": 0.5},
      "files": ["a.go", "b.go"],
      "nested": {"deep": {"deeper": [1, 2, {"three": true}]}},
      "nothing": null,
      "empty": {},
      "big": 9007199254740993
    }`)
	out, err := rt.Call(context.Background(), handler(t, module, "echo"), args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v (%s)", err, out)
	}
	if got["query"] != "hello" {
		t.Errorf("query = %#v", got["query"])
	}
	if got["recursive"] != true {
		t.Errorf("recursive = %#v, want true (boolean must not become a string)", got["recursive"])
	}
	if got["depth"] != float64(3) {
		t.Errorf("depth = %#v, want 3 (number must not become a string)", got["depth"])
	}
	if got["ratio"] != 0.5 {
		t.Errorf("ratio = %#v", got["ratio"])
	}
	if got["nothing"] != nil {
		t.Errorf("nothing = %#v, want nil (null must survive)", got["nothing"])
	}
	files, ok := got["files"].([]any)
	if !ok || len(files) != 2 || files[0] != "a.go" {
		t.Errorf("files = %#v, want a two-element array", got["files"])
	}
	empty, ok := got["empty"].(map[string]any)
	if !ok || len(empty) != 0 {
		t.Errorf("empty = %#v, want an empty object", got["empty"])
	}
	// 2^53+1 is not representable as float64; JSON.parse keeps it exact as a
	// double only if the engine honours the full JSON number syntax. The
	// assertion is that the value is numeric and not stringified.
	if _, ok := got["big"].(float64); !ok {
		t.Errorf("big = %#v, want a JSON number", got["big"])
	}
}

// TestDeepNestingSurvives proves the engine-neutral boundary does not flatten
// structures at depth.
func TestDeepNestingSurvives(t *testing.T) {
	module := compile(t, "plugin.js", `export function deep(input) { return input; }`)
	rt := newRuntime(t, jsengine.RuntimeOptions{DisableDynamicCode: true})
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	args := []byte(`{"a":{"b":{"c":{"d":[{"e":[1,[2,[3]]]}]}}}}`)
	out, err := rt.Call(context.Background(), handler(t, module, "deep"), args)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	level := got["a"].(map[string]any)["b"].(map[string]any)["c"].(map[string]any)
	list := level["d"].([]any)[0].(map[string]any)["e"].([]any)
	if len(list) != 2 || list[0] != float64(1) {
		t.Fatalf("deep structure was altered: %#v", list)
	}
}

func TestMissingArgumentsBecomeEmptyObject(t *testing.T) {
	module := compile(t, "plugin.js", `export function keys(input) { return Object.keys(input); }`)
	rt := newRuntime(t, jsengine.RuntimeOptions{})
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]byte{nil, []byte(``), []byte(`null`)} {
		out, err := rt.Call(context.Background(), handler(t, module, "keys"), args)
		if err != nil {
			t.Fatalf("Call(%q): %v", args, err)
		}
		if string(out) != `[]` {
			t.Fatalf("Call(%q) = %s, want []", args, out)
		}
	}
}

func TestInterruptStopsInfiniteLoopAndRuntimeStaysUsable(t *testing.T) {
	module := compile(t, "plugin.js", `
export function spin() { let i = 0; while (true) { i++; } }
export function quick() { return "ok"; }
`)
	rt := newRuntime(t, jsengine.RuntimeOptions{})
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := jsengine.CallWithContext(ctx, rt, handler(t, module, "spin"), []byte(`{}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CallWithContext = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("interrupt took %s", elapsed)
	}
	// The runtime must be reusable after an interrupt.
	out, err := rt.Call(context.Background(), handler(t, module, "quick"), []byte(`{}`))
	if err != nil {
		t.Fatalf("runtime unusable after interrupt: %v", err)
	}
	if string(out) != `"ok"` {
		t.Fatalf("result = %s", out)
	}
	rt.ReleaseCallData()
}

// TestLoadInterruptStopsTopLevelLoop covers the module that loops while it is
// being evaluated: cancellation must reach Load too, not only Call.
func TestLoadInterruptStopsTopLevelLoop(t *testing.T) {
	module := compile(t, "plugin.js", `let i = 0; while (true) { i++; } export function noop() {}`)
	rt := newRuntime(t, jsengine.RuntimeOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rt.Load(ctx, module) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Load of an infinite top-level loop must not succeed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Load ignored context cancellation")
	}
}

func TestExceptionIsClassifiedWithStack(t *testing.T) {
	module := compile(t, "plugin.js", `export function boom() { throw new Error("kaboom"); }`)
	rt := newRuntime(t, jsengine.RuntimeOptions{})
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	_, err := rt.Call(context.Background(), handler(t, module, "boom"), []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	kind, ok := jsengine.KindOf(err)
	if !ok || kind != jsengine.KindException {
		t.Fatalf("KindOf = %v, %v (err = %v)", kind, ok, err)
	}
	var engineErr *jsengine.EngineError
	if !errors.As(err, &engineErr) {
		t.Fatal("error is not an EngineError")
	}
	if !strings.Contains(engineErr.Message, "kaboom") {
		t.Errorf("message = %q", engineErr.Message)
	}
	if !strings.Contains(engineErr.Stack, "boom") {
		t.Errorf("stack = %q, want the throwing frame", engineErr.Stack)
	}
}

func TestSyntaxErrorCarriesLocation(t *testing.T) {
	_, err := moejs.Engine{}.Compile("bad.js", "export function (")
	if err == nil {
		t.Fatal("expected a syntax error")
	}
	var engineErr *jsengine.EngineError
	if !errors.As(err, &engineErr) {
		t.Fatalf("error is not an EngineError: %v", err)
	}
	if engineErr.Kind != jsengine.KindSyntax {
		t.Errorf("Kind = %v", engineErr.Kind)
	}
	if engineErr.File != "bad.js" || engineErr.Line != 1 || engineErr.Column == 0 {
		t.Errorf("location = %s:%d:%d", engineErr.File, engineErr.Line, engineErr.Column)
	}
}

func TestMissingHandlerIsClassified(t *testing.T) {
	module := compile(t, "plugin.js", `export function present() { return 1; }`)
	if _, err := module.Handler("absent"); err == nil {
		t.Fatal("resolving a missing export must fail")
	} else if kind, _ := jsengine.KindOf(err); kind != jsengine.KindMissingHandler {
		t.Fatalf("Kind = %v", kind)
	}
	// A non-function export resolves as a handler path but must be rejected
	// once a runtime checks it, which is what Load-time validation relies on.
	notAFunction := compile(t, "plugin.js", `export const value = 42;`)
	valueHandler, err := notAFunction.Handler("value")
	if err != nil {
		t.Fatalf("Handler(value): %v", err)
	}
	rt := newRuntime(t, jsengine.RuntimeOptions{})
	if err := rt.Load(context.Background(), notAFunction); err != nil {
		t.Fatal(err)
	}
	err = rt.ValidateHandler(context.Background(), valueHandler)
	if err == nil {
		t.Fatal("a non-function export must not validate as a handler")
	}
	if kind, _ := jsengine.KindOf(err); kind != jsengine.KindMissingHandler {
		t.Fatalf("Kind = %v (err = %v)", kind, err)
	}
}

// TestValidateHandlerAcceptsRealHandler is the other half of the contract: a
// handler that does exist must validate, so the check cannot be a blanket
// rejection.
func TestValidateHandlerAcceptsRealHandler(t *testing.T) {
	module := compile(t, "plugin.js", `export function present() { return 1; }`)
	rt := newRuntime(t, jsengine.RuntimeOptions{})
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	if err := rt.ValidateHandler(context.Background(), handler(t, module, "present")); err != nil {
		t.Fatalf("ValidateHandler: %v", err)
	}
}

// TestDynamicCodeIsDisabledByDefault proves eval and the Function constructor
// cannot be used to escape the sandbox.
func TestDynamicCodeIsDisabledByDefault(t *testing.T) {
	module := compile(t, "plugin.js", `
export function viaEval() { try { return String(eval("1+1")); } catch (e) { return "blocked:" + e.name; } }
export function viaFunction() { try { return String(new Function("return 1")()); } catch (e) { return "blocked:" + e.name; } }
`)
	rt := newRuntime(t, jsengine.RuntimeOptions{DisableDynamicCode: true})
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"viaEval", "viaFunction"} {
		out, err := rt.Call(context.Background(), handler(t, module, name), []byte(`{}`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(out), "blocked:EvalError") {
			t.Errorf("%s = %s, want EvalError", name, out)
		}
		rt.ReleaseCallData()
	}
}

// TestBuiltinsAreFrozen proves a plugin cannot patch shared prototypes, which
// would otherwise let one plugin alter another plugin's behaviour.
func TestBuiltinsAreFrozen(t *testing.T) {
	module := compile(t, "plugin.js", `
export function patch() {
  try { Array.prototype.push = function () { return "hijacked"; }; return "patched"; }
  catch (e) { return "blocked:" + e.name; }
}
`)
	rt := newRuntime(t, jsengine.RuntimeOptions{})
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	out, err := rt.Call(context.Background(), handler(t, module, "patch"), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "blocked:TypeError") {
		t.Fatalf("patching a builtin was not blocked: %s", out)
	}
}

func TestStaticImportsResolveThroughHostResolver(t *testing.T) {
	engine := moejs.Engine{}
	entry := compile(t, "entry.js", `
import { greet } from "./lib/greet.js";
import { bruceVersion } from "bruce:api";
export function run() { return greet() + "|" + bruceVersion; }
`)
	modules := map[string]jsengine.Module{
		"./lib/greet.js": compile(t, "lib/greet.js", `export function greet() { return "hi"; }`),
		"bruce:api":      compile(t, "bruce:api", `export const bruceVersion = "0.1";`),
	}
	resolve := func(_, specifier string) (jsengine.Module, error) {
		module, ok := modules[specifier]
		if !ok {
			return nil, errors.New("import not allowed: " + specifier)
		}
		return module, nil
	}
	linked, err := engine.Link(entry, resolve)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	rt := newRuntime(t, jsengine.RuntimeOptions{DisableDynamicCode: true})
	if err := rt.Load(context.Background(), linked); err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := rt.Call(context.Background(), handler(t, linked, "run"), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"hi|0.1"` {
		t.Fatalf("result = %s", out)
	}
}

// TestRefusedImportIsALinkError is the security-relevant half of module
// resolution: a specifier the host refuses must fail the link, not silently
// resolve to something else.
func TestRefusedImportIsALinkError(t *testing.T) {
	engine := moejs.Engine{}
	entry := compile(t, "entry.js", `import { x } from "/etc/passwd"; export function run() { return x; }`)
	_, err := engine.Link(entry, func(_, specifier string) (jsengine.Module, error) {
		return nil, errors.New("import not allowed: " + specifier)
	})
	if err == nil {
		t.Fatal("linking a refused import must fail")
	}
	if kind, _ := jsengine.KindOf(err); kind != jsengine.KindLink {
		t.Fatalf("Kind = %v (err = %v)", kind, err)
	}
	if !strings.Contains(err.Error(), "import not allowed") {
		t.Errorf("error = %v, want the resolver's reason", err)
	}
}

func TestDynamicImportUsesHostImporter(t *testing.T) {
	entry := compile(t, "entry.js", `export function load() { return import("bruce:api"); }`)
	api := compile(t, "bruce:api", `export const bruceVersion = "0.1";`)
	importer := func(_, specifier string) (jsengine.Module, error) {
		if specifier == "bruce:api" {
			return api, nil
		}
		return nil, errors.New("import not allowed: " + specifier)
	}
	rt := newRuntime(t, jsengine.RuntimeOptions{Importer: importer})
	if err := rt.Load(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Call(context.Background(), handler(t, entry, "load"), []byte(`{}`)); err != nil {
		t.Fatalf("dynamic import failed: %v", err)
	}
}

func TestLinkRequiresImportingModuleToBeLinked(t *testing.T) {
	module := compile(t, "entry.js", `import { x } from "./x.js"; export function run() { return x; }`)
	rt := newRuntime(t, jsengine.RuntimeOptions{})
	err := rt.Load(context.Background(), module)
	if err == nil {
		t.Fatal("loading an unlinked module with imports must fail")
	}
	if kind, _ := jsengine.KindOf(err); kind != jsengine.KindLink {
		t.Fatalf("Kind = %v (err = %v)", kind, err)
	}
}

// TestRuntimesAreIsolated proves state does not leak between runtimes of the
// same module, which is what makes pooling safe.
func TestRuntimesAreIsolated(t *testing.T) {
	module := compile(t, "plugin.js", `
let counter = 0;
export function bump() { counter++; return counter; }
`)
	first := newRuntime(t, jsengine.RuntimeOptions{})
	second := newRuntime(t, jsengine.RuntimeOptions{})
	if err := first.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	if err := second.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		out, err := first.Call(context.Background(), handler(t, module, "bump"), []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != string(rune('0'+i)) {
			t.Fatalf("first runtime counter = %s, want %d", out, i)
		}
		first.ReleaseCallData()
	}
	out, err := second.Call(context.Background(), handler(t, module, "bump"), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "1" {
		t.Fatalf("second runtime saw the first runtime's state: %s", out)
	}
}

func TestModuleExportsAndRequestsAreStable(t *testing.T) {
	module := compile(t, "entry.js", `
import { a } from "./a.js";
export function zeta() { return a; }
export const alpha = 1;
`)
	exports := module.Exports()
	if len(exports) != 2 || exports[0] != "alpha" || exports[1] != "zeta" {
		t.Fatalf("Exports = %v, want sorted [alpha zeta]", exports)
	}
	requests := module.Requests()
	if len(requests) != 1 || requests[0] != "./a.js" {
		t.Fatalf("Requests = %v", requests)
	}
	if module.Name() != "entry.js" {
		t.Fatalf("Name = %q", module.Name())
	}
}

func TestNestedHandlerPath(t *testing.T) {
	module := compile(t, "plugin.js", `
export const tools = {
  read: (input) => "read:" + input.path,
};
`)
	h := handler(t, module, "tools.read")
	if h.Name() != "tools.read" {
		t.Fatalf("Name = %q", h.Name())
	}
	rt := newRuntime(t, jsengine.RuntimeOptions{})
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	out, err := rt.Call(context.Background(), h, []byte(`{"path":"a.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"read:a.go"` {
		t.Fatalf("result = %s", out)
	}
}

// TestHostFunctionsAreCallableFromJavaScript proves the host capability bridge:
// a plugin can call a Go function and receive structured data back.
func TestHostFunctionsAreCallableFromJavaScript(t *testing.T) {
	module := compile(t, "plugin.js", `
export function useHost(input) {
  const result = bruce.echo(input);
  return { echoed: result, logged: bruce.log("hello") };
}
`)
	var gotArgs string
	globals := map[string]jsengine.GlobalObject{
		"bruce": {
			"echo": jsengine.HostFunc(func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
				gotArgs = string(args)
				var parsed map[string]any
				if err := json.Unmarshal(args, &parsed); err != nil {
					return nil, err
				}
				parsed["added"] = true
				return json.Marshal(parsed)
			}),
			"log": jsengine.HostFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"ok":true}`), nil
			}),
		},
	}
	rt := newRuntime(t, jsengine.RuntimeOptions{DisableDynamicCode: true, Globals: globals})
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	out, err := rt.Call(context.Background(), handler(t, module, "useHost"), []byte(`{"nested":{"deep":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	echoed := got["echoed"].(map[string]any)
	if echoed["added"] != true {
		t.Errorf("host function result did not reach JavaScript: %#v", echoed)
	}
	if _, ok := echoed["nested"].(map[string]any); !ok {
		t.Errorf("nested object was lost across the host boundary: %#v", echoed)
	}
	if !strings.Contains(gotArgs, `"nested"`) {
		t.Errorf("host function received %q", gotArgs)
	}
}

// TestHostFunctionErrorBecomesAJavaScriptException proves a denied capability
// is catchable and classifiable, which is what the permission model needs.
func TestHostFunctionErrorBecomesAJavaScriptException(t *testing.T) {
	module := compile(t, "plugin.js", `
export function denied() {
  try { return bruce.forbidden(); } catch (e) { return "caught:" + e.message; }
}
`)
	globals := map[string]jsengine.GlobalObject{
		"bruce": {
			"forbidden": jsengine.HostFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return nil, errors.New("permission denied: fs.read")
			}),
		},
	}
	rt := newRuntime(t, jsengine.RuntimeOptions{DisableDynamicCode: true, Globals: globals})
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	out, err := rt.Call(context.Background(), handler(t, module, "denied"), []byte(`{}`))
	if err != nil {
		t.Fatalf("the exception should be catchable in JavaScript: %v", err)
	}
	if !strings.Contains(string(out), "caught:") || !strings.Contains(string(out), "permission denied") {
		t.Fatalf("result = %s", out)
	}
}

// TestHostFunctionReceivesCallContext proves cancellation reaches a host
// capability, so a plugin cannot use a host call to outlive its context.
func TestHostFunctionReceivesCallContext(t *testing.T) {
	module := compile(t, "plugin.js", `export function call() { return bruce.slow(); }`)
	var sawCancel bool
	globals := map[string]jsengine.GlobalObject{
		"bruce": {
			"slow": jsengine.HostFunc(func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
				<-ctx.Done()
				sawCancel = true
				return nil, ctx.Err()
			}),
		},
	}
	rt := newRuntime(t, jsengine.RuntimeOptions{DisableDynamicCode: true, Globals: globals})
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := jsengine.CallWithContext(ctx, rt, handler(t, module, "call"), []byte(`{}`))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the call must fail when its context ends")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not reach the host function")
	}
	if !sawCancel {
		t.Error("the host function never observed the cancellation")
	}
}
