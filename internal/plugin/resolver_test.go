package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bruce-go/internal/jsengine"
	"bruce-go/internal/jsengine/moejs"
)

// writePlugin writes a plugin directory with a manifest and entry module and
// returns its root.
func writePlugin(t *testing.T, root, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for relative, content := range files {
		target := filepath.Join(dir, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newResolver(t *testing.T, root string, virtual map[string]string) *Resolver {
	t.Helper()
	resolver, err := NewResolver(ResolverOptions{Engine: moejs.Engine{}, RootDir: root, Virtual: virtual})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return resolver
}

func TestResolverAllowsRelativeModulesInsideThePlugin(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "demo", map[string]string{
		"entry.js":     `import { greet } from "./lib/greet.js"; export function run() { return greet(); }`,
		"lib/greet.js": `export function greet() { return "hi"; }`,
	})
	resolver := newResolver(t, filepath.Join(root, "demo"), nil)

	entry, err := resolver.Resolve("", "./entry.js")
	if err != nil {
		t.Fatalf("Resolve entry: %v", err)
	}
	linked, err := resolver.Link(entry)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if len(linked.Exports()) != 1 || linked.Exports()[0] != "run" {
		t.Fatalf("Exports = %v", linked.Exports())
	}
}

func TestResolverRefusesEscapeAttempts(t *testing.T) {
	root := t.TempDir()
	pluginDir := writePlugin(t, root, "demo", map[string]string{
		"entry.js": `export function run() { return 1; }`,
	})
	// A secret file outside the plugin directory, still inside the temp root.
	secret := filepath.Join(root, "secret.js")
	if err := os.WriteFile(secret, []byte(`export const stolen = true;`), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver := newResolver(t, pluginDir, nil)

	for _, specifier := range []string{
		"../secret.js",
		"../../secret.js",
		"../../../../../../etc/passwd",
		"./../../secret.js",
		"lib/../../../secret.js",
	} {
		if _, err := resolver.Resolve("", specifier); err == nil {
			t.Errorf("Resolve(%q) succeeded, want a refusal", specifier)
		}
	}
}

// TestResolverRefusesSymlinkEscape is the case a naive prefix check misses:
// the path looks contained, but it resolves outside the plugin directory.
func TestResolverRefusesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	pluginDir := writePlugin(t, root, "demo", map[string]string{
		"entry.js": `export function run() { return 1; }`,
	})
	outside := filepath.Join(root, "outside.js")
	if err := os.WriteFile(outside, []byte(`export const stolen = true;`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pluginDir, "escape.js")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(root, filepath.Join(pluginDir, "updir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolver := newResolver(t, pluginDir, nil)

	for _, specifier := range []string{"./escape.js", "./updir/outside.js", "./updir/../../secret.js"} {
		if _, err := resolver.Resolve("", specifier); err == nil {
			t.Errorf("Resolve(%q) followed a symlink out of the plugin directory", specifier)
		}
	}
}

// TestResolverRefusesBareSpecifiers blocks npm, node_modules, Node builtins
// and system paths with one rule.
func TestResolverRefusesBareSpecifiers(t *testing.T) {
	root := t.TempDir()
	pluginDir := writePlugin(t, root, "demo", map[string]string{
		"entry.js": `export function run() { return 1; }`,
	})
	// Even a node_modules directory that really exists must not be reachable.
	if err := os.MkdirAll(filepath.Join(pluginDir, "node_modules", "left-pad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "node_modules", "left-pad", "index.js"), []byte(`export const x = 1;`), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver := newResolver(t, pluginDir, nil)

	for _, specifier := range []string{
		"left-pad", "node_modules/left-pad/index.js", "fs", "node:fs",
		"child_process", "http", "process", "/etc/passwd", "file:///etc/passwd",
		"https://example.com/mod.js",
	} {
		if _, err := resolver.Resolve("", specifier); err == nil {
			t.Errorf("Resolve(%q) succeeded, want a refusal", specifier)
		}
	}
}

func TestResolverServesOnlyDeclaredVirtualModules(t *testing.T) {
	root := t.TempDir()
	pluginDir := writePlugin(t, root, "demo", map[string]string{
		"entry.js": `export function run() { return 1; }`,
	})
	resolver := newResolver(t, pluginDir, map[string]string{VirtualAPI: `export const version = "1";`})

	if _, err := resolver.Resolve("", VirtualAPI); err != nil {
		t.Fatalf("declared virtual module was refused: %v", err)
	}
	// A virtual module the host did not install for this plugin is refused,
	// so a plugin cannot reach a capability it was not granted.
	if _, err := resolver.Resolve("", VirtualStorage); err == nil {
		t.Error("an undeclared virtual module must be refused")
	}
	if _, err := resolver.Resolve("", "bruce:root"); err == nil {
		t.Error("an unknown bruce: module must be refused")
	}
}

// TestResolverCompilesEachModuleOnce proves the compile-once lifecycle that
// makes a warm invocation cheap.
func TestResolverCompilesEachModuleOnce(t *testing.T) {
	root := t.TempDir()
	pluginDir := writePlugin(t, root, "demo", map[string]string{
		"entry.js": `export function run() { return 1; }`,
	})
	resolver := newResolver(t, pluginDir, nil)
	for range 5 {
		if _, err := resolver.Resolve("", "./entry.js"); err != nil {
			t.Fatal(err)
		}
	}
	if cached := resolver.CachedModules(); len(cached) != 1 {
		t.Fatalf("CachedModules = %v, want one entry", cached)
	}
}

func TestResolverReportsMissingModule(t *testing.T) {
	root := t.TempDir()
	pluginDir := writePlugin(t, root, "demo", map[string]string{
		"entry.js": `export function run() { return 1; }`,
	})
	resolver := newResolver(t, pluginDir, nil)
	_, err := resolver.Resolve("", "./absent.js")
	if err == nil {
		t.Fatal("a missing module must be refused")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want a clear 'does not exist'", err)
	}
}

func TestResolverRefusesDirectories(t *testing.T) {
	root := t.TempDir()
	pluginDir := writePlugin(t, root, "demo", map[string]string{
		"entry.js":   `export function run() { return 1; }`,
		"lib/mod.js": `export const x = 1;`,
	})
	resolver := newResolver(t, pluginDir, nil)
	if _, err := resolver.Resolve("", "./lib"); err == nil {
		t.Error("importing a directory must be refused")
	}
}

func TestResolverEnforcesModuleSizeLimit(t *testing.T) {
	root := t.TempDir()
	pluginDir := writePlugin(t, root, "demo", map[string]string{
		"entry.js": `export function run() { return 1; }`,
		"big.js":   strings.Repeat("// padding\n", 500),
	})
	resolver, err := NewResolver(ResolverOptions{Engine: moejs.Engine{}, RootDir: pluginDir, MaxModuleBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve("", "./big.js"); err == nil {
		t.Error("a module over the size limit must be refused")
	}
}

// TestResolverNestedRelativeImportIsRelativeToTheImporter proves a module in a
// subdirectory importing "./sibling.js" means its own sibling, not the root's.
func TestResolverNestedRelativeImportIsRelativeToTheImporter(t *testing.T) {
	root := t.TempDir()
	pluginDir := writePlugin(t, root, "demo", map[string]string{
		"entry.js":       `import { value } from "./lib/mod.js"; export function run() { return value; }`,
		"lib/mod.js":     `import { helper } from "./sibling.js"; export const value = helper();`,
		"lib/sibling.js": `export function helper() { return "nested"; }`,
		"sibling.js":     `export function helper() { return "root"; }`,
	})
	resolver := newResolver(t, pluginDir, nil)
	entry, err := resolver.Resolve("", "./entry.js")
	if err != nil {
		t.Fatal(err)
	}
	linked, err := resolver.Link(entry)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := moejs.Engine{}.NewRuntime(jsengine.RuntimeOptions{DisableDynamicCode: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if err := rt.Load(context.Background(), linked); err != nil {
		t.Fatal(err)
	}
	h, err := linked.Handler("run")
	if err != nil {
		t.Fatal(err)
	}
	out, err := rt.Call(context.Background(), h, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"nested"` {
		t.Fatalf("result = %s, want \"nested\"", out)
	}
}

func TestResolverRequiresRootAndEngine(t *testing.T) {
	if _, err := NewResolver(ResolverOptions{Engine: moejs.Engine{}}); err == nil {
		t.Error("an empty root must be refused")
	}
	if _, err := NewResolver(ResolverOptions{RootDir: t.TempDir()}); err == nil {
		t.Error("a missing engine must be refused")
	}
	if _, err := NewResolver(ResolverOptions{Engine: moejs.Engine{}, RootDir: filepath.Join(t.TempDir(), "absent")}); err == nil {
		t.Error("a non-existent root must be refused")
	}
}

func TestVirtualModuleSourcesCompile(t *testing.T) {
	engine := moejs.Engine{}
	manifest := &Manifest{Name: "demo", Version: "1.2.3", Source: SourceWorkspace}
	sources := map[string]string{
		VirtualAPI:     virtualAPISource(manifest),
		VirtualStorage: virtualStorageSource(),
		VirtualEvents:  virtualEventsSource(),
	}
	for name, source := range sources {
		module, err := engine.Compile(name, source)
		if err != nil {
			t.Errorf("virtual module %s does not compile: %v", name, err)
			continue
		}
		if len(module.Exports()) == 0 {
			t.Errorf("virtual module %s exports nothing", name)
		}
	}
}

// TestVirtualAPIExposesPluginIdentity checks the rendered module really
// carries the plugin's identity, not a placeholder.
func TestVirtualAPIExposesPluginIdentity(t *testing.T) {
	engine := moejs.Engine{}
	manifest := &Manifest{Name: "demo", Version: "1.2.3", Source: SourceWorkspace}
	module, err := engine.Compile(VirtualAPI, virtualAPISource(manifest))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := engine.NewRuntime(jsengine.RuntimeOptions{DisableDynamicCode: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	h, err := module.Handler("info")
	if err != nil {
		t.Fatal(err)
	}
	out, err := rt.Call(context.Background(), h, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "demo" || got["version"] != "1.2.3" || got["apiVersion"] != APIVersion {
		t.Fatalf("bruce:api = %#v", got)
	}
	if got["source"] != string(SourceWorkspace) {
		t.Fatalf("source = %#v", got["source"])
	}
}

// TestStorageVirtualModuleCallsHostFunctions proves the storage virtual module
// is a thin wrapper over host capabilities, not a JavaScript-side store.
func TestStorageVirtualModuleCallsHostFunctions(t *testing.T) {
	engine := moejs.Engine{}
	module, err := engine.Compile(VirtualStorage, virtualStorageSource())
	if err != nil {
		t.Fatal(err)
	}
	type call struct {
		op    string
		scope string
		key   string
	}
	var calls []call
	globals := map[string]jsengine.GlobalObject{
		"bruce": {
			"storage": map[string]any{
				"get": jsengine.HostFunc(func(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
					var parsed struct {
						Scope string `json:"scope"`
						Key   string `json:"key"`
					}
					if err := json.Unmarshal(args, &parsed); err != nil {
						return nil, err
					}
					calls = append(calls, call{op: "get", scope: parsed.Scope, key: parsed.Key})
					return json.RawMessage(`{"found":true,"value":{"n":1}}`), nil
				}),
			},
		},
	}
	rt, err := engine.NewRuntime(jsengine.RuntimeOptions{DisableDynamicCode: true, Globals: globals})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if err := rt.Load(context.Background(), module); err != nil {
		t.Fatal(err)
	}
	h, err := module.Handler("get")
	if err != nil {
		t.Fatal(err)
	}
	out, err := rt.Call(context.Background(), h, []byte(`{"scope":"session","key":"k"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != (call{op: "get", scope: "session", key: "k"}) {
		t.Fatalf("host calls = %#v", calls)
	}
	if !strings.Contains(string(out), `"found":true`) {
		t.Fatalf("result = %s", out)
	}
	// An unknown scope must be rejected before it reaches the host.
	if _, err := rt.Call(context.Background(), h, []byte(`{"scope":"bogus","key":"k"}`)); err == nil {
		t.Error("an unknown storage scope must be rejected in JavaScript")
	}
}

var _ = errors.New
