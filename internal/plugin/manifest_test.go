package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestFile writes a file, creating parent directories.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writePluginDir creates <root>/<name>/plugin.json and returns the manifest
// path. It does not create the entry file, so tests that check entry
// validation stay in control of what exists.
func writePluginDir(t *testing.T, root, name, body string) string {
	t.Helper()
	path := filepath.Join(root, name, "plugin.json")
	writeTestFile(t, path, body)
	return path
}

// minimalManifest renders a valid manifest whose entry is index.js.
func minimalManifest(name string) string {
	return fmt.Sprintf(`{
  "apiVersion": %q,
  "name": %q,
  "version": "1.0.0",
  "description": "a test plugin",
  "entry": "index.js"
}`, APIVersion, name)
}

// parsePlugin writes body to <root>/<name>/plugin.json together with the
// index.js entry it declares, then parses it.
func parsePlugin(t *testing.T, root, name, body string) (*Manifest, error) {
	t.Helper()
	path := writePluginDir(t, root, name, body)
	writeTestFile(t, filepath.Join(root, name, "index.js"), "export function run() {}\n")
	return ParseManifest([]byte(body), path)
}

// mustParsePlugin parses a manifest that is expected to be valid.
func mustParsePlugin(t *testing.T, root, name, body string) *Manifest {
	t.Helper()
	manifest, err := parsePlugin(t, root, name, body)
	if err != nil {
		t.Fatalf("ParseManifest(%s) failed: %v", name, err)
	}
	return manifest
}

// requireManifestError asserts that err is a *ManifestError naming field.
func requireManifestError(t *testing.T, err error, field string) *ManifestError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a manifest error for field %q, got nil", field)
	}
	var manifestErr *ManifestError
	if !errors.As(err, &manifestErr) {
		t.Fatalf("error %v (%T) is not a *ManifestError", err, err)
	}
	if field != "" && manifestErr.Field != field {
		t.Fatalf("error field = %q, want %q (error: %v)", manifestErr.Field, field, err)
	}
	return manifestErr
}

func TestParseManifestValid(t *testing.T) {
	root := t.TempDir()
	body := fmt.Sprintf(`{
  "apiVersion": %q,
  "name": "demo-plugin",
  "version": "1.2.3",
  "description": "does demo things",
  "entry": "index.js",
  "tools": [{
    "name": "demo_run",
    "description": "run the demo",
    "handler": "run",
    "schema": {"type": "object", "properties": {"path": {"type": "string"}}},
    "permissions": ["fs.read"],
    "parallelSafe": true,
    "timeoutMs": 1500,
    "risk": "low",
    "promptSnippet": "Run the demo"
  }],
  "hooks": [
    {"event": "session.started", "handler": "onStart", "timeoutMs": 100},
    {"event": "tool.before", "handler": "beforeTool"}
  ],
  "commands": [{
    "name": "demo-review",
    "description": "review something",
    "handler": "review",
    "usage": "/demo-review <path>",
    "permissions": ["fs.read", "storage"],
    "timeoutMs": 200
  }],
  "permissions": ["fs.read", "events"],
  "concurrency": {"maxRuntimes": 4, "parallelSafe": false},
  "metadata": {"author": "bruce"}
}`, APIVersion)
	manifest := mustParsePlugin(t, root, "demo-plugin", body)

	if manifest.APIVersion != APIVersion || manifest.Name != "demo-plugin" ||
		manifest.Version != "1.2.3" || manifest.Description != "does demo things" ||
		manifest.Entry != "index.js" {
		t.Fatalf("manifest identity fields are wrong: %+v", manifest)
	}
	if manifest.RootDir != filepath.Join(root, "demo-plugin") {
		t.Errorf("RootDir = %q, want %q", manifest.RootDir, filepath.Join(root, "demo-plugin"))
	}
	if manifest.File != filepath.Join(root, "demo-plugin", "plugin.json") {
		t.Errorf("File = %q", manifest.File)
	}
	if manifest.Source != "" {
		t.Errorf("ParseManifest must not invent a source, got %q", manifest.Source)
	}
	if len(manifest.Tools) != 1 || manifest.Tools[0].Name != "demo_run" ||
		manifest.Tools[0].Handler != "run" || manifest.Tools[0].TimeoutMS != 1500 ||
		manifest.Tools[0].Risk != "low" || manifest.Tools[0].PromptSnippet != "Run the demo" {
		t.Fatalf("tool declaration is wrong: %+v", manifest.Tools)
	}
	if manifest.Tools[0].ParallelSafe == nil || !*manifest.Tools[0].ParallelSafe {
		t.Error("tool parallelSafe must round-trip")
	}
	if got := string(manifest.Tools[0].Permissions[0]); got != "fs.read" {
		t.Errorf("tool permissions = %v", manifest.Tools[0].Permissions)
	}
	if len(manifest.Hooks) != 2 || manifest.Hooks[0].Event != "session.started" ||
		manifest.Hooks[1].Event != "tool.before" || manifest.Hooks[1].TimeoutMS != 0 {
		t.Fatalf("hook declarations are wrong: %+v", manifest.Hooks)
	}
	if len(manifest.Commands) != 1 || manifest.Commands[0].Name != "demo-review" ||
		manifest.Commands[0].Usage != "/demo-review <path>" || manifest.Commands[0].TimeoutMS != 200 {
		t.Fatalf("command declarations are wrong: %+v", manifest.Commands)
	}
	if len(manifest.Permissions) != 2 || manifest.Permissions[1] != PermissionEvents {
		t.Fatalf("permissions = %v", manifest.Permissions)
	}
	if manifest.Concurrency.MaxRuntimes != 4 || manifest.Concurrency.ParallelSafe == nil ||
		*manifest.Concurrency.ParallelSafe {
		t.Fatalf("concurrency = %+v", manifest.Concurrency)
	}
	if manifest.Metadata["author"] != "bruce" {
		t.Fatalf("metadata = %v", manifest.Metadata)
	}
}

func TestParseManifestAppliesDefaults(t *testing.T) {
	root := t.TempDir()
	body := fmt.Sprintf(`{
  "apiVersion": %q,
  "name": "demo",
  "version": "0.1.0",
  "description": "defaults",
  "entry": "index.js",
  "tools": [{"name": "demo_ping", "handler": "ping"}]
}`, APIVersion)
	manifest := mustParsePlugin(t, root, "demo", body)

	tool := manifest.Tools[0]
	if string(tool.Schema) != string(defaultToolSchema) {
		t.Errorf("missing schema must default to %s, got %s", defaultToolSchema, tool.Schema)
	}
	if tool.TimeoutMS != 0 || tool.Risk != "" || tool.ParallelSafe != nil {
		t.Errorf("unset optional tool fields must stay zero: %+v", tool)
	}
	if manifest.Concurrency.MaxRuntimes != 0 || manifest.Concurrency.ParallelSafe != nil {
		t.Errorf("unset concurrency must stay zero: %+v", manifest.Concurrency)
	}
	if manifest.Hooks != nil || manifest.Commands != nil || manifest.Permissions != nil {
		t.Errorf("absent lists must stay nil: %+v", manifest)
	}

	// The default schema must not be shared between manifests.
	other := mustParsePlugin(t, root, "other", strings.Replace(body, `"name": "demo"`, `"name": "other"`, 1))
	if &manifest.Tools[0].Schema[0] == &other.Tools[0].Schema[0] {
		t.Error("default schema must be a fresh copy per manifest")
	}
}

func TestParseManifestMissingName(t *testing.T) {
	root := t.TempDir()
	body := fmt.Sprintf(`{
  "apiVersion": %q,
  "version": "1.0.0",
  "description": "no name",
  "entry": "index.js"
}`, APIVersion)
	path := writePluginDir(t, root, "demo", body)
	writeTestFile(t, filepath.Join(root, "demo", "index.js"), "")

	_, err := ParseManifest([]byte(body), path)
	manifestErr := requireManifestError(t, err, "name")
	if !strings.Contains(manifestErr.Error(), "<unknown>") {
		t.Errorf("a nameless manifest must be reported as <unknown>: %v", err)
	}
	if !strings.Contains(manifestErr.Error(), path) {
		t.Errorf("error must carry the manifest path: %v", err)
	}
}

func TestParseManifestInvalidAPIVersion(t *testing.T) {
	root := t.TempDir()

	missing := fmt.Sprintf(`{"name":"demo","version":"1.0.0","description":"x","entry":"index.js"}`)
	if _, err := parsePlugin(t, root, "demo", missing); requireManifestError(t, err, "apiVersion") == nil {
		t.Fatal("unreachable")
	}

	wrong := fmt.Sprintf(`{"apiVersion":"bruce.plugin/v2","name":"demo","version":"1.0.0","description":"x","entry":"index.js"}`)
	_, err := parsePlugin(t, root, "demo", wrong)
	manifestErr := requireManifestError(t, err, "apiVersion")
	if !strings.Contains(manifestErr.Message, APIVersion) || !strings.Contains(manifestErr.Message, "bruce.plugin/v2") {
		t.Errorf("apiVersion error must name both versions: %v", err)
	}
}

func TestParseManifestInvalidIdentityFields(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"uppercase name", `{"apiVersion":"bruce.plugin/v1","name":"Demo","version":"1.0.0","description":"x","entry":"index.js"}`, "name"},
		{"name with slash", `{"apiVersion":"bruce.plugin/v1","name":"demo/plugin","version":"1.0.0","description":"x","entry":"index.js"}`, "name"},
		{"name trailing dash", `{"apiVersion":"bruce.plugin/v1","name":"demo-","version":"1.0.0","description":"x","entry":"index.js"}`, "name"},
		{"name too long", fmt.Sprintf(`{"apiVersion":"bruce.plugin/v1","name":%q,"version":"1.0.0","description":"x","entry":"index.js"}`, strings.Repeat("a", 65)), "name"},
		{"missing version", `{"apiVersion":"bruce.plugin/v1","name":"demo","description":"x","entry":"index.js"}`, "version"},
		{"version not semver", `{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0","description":"x","entry":"index.js"}`, "version"},
		{"version prefixed", `{"apiVersion":"bruce.plugin/v1","name":"demo","version":"v1.0.0","description":"x","entry":"index.js"}`, "version"},
		{"missing description", `{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","entry":"index.js"}`, "description"},
		{"blank description", `{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"   ","entry":"index.js"}`, "description"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parsePlugin(t, root, "demo", testCase.body)
			requireManifestError(t, err, testCase.field)
		})
	}
}

func TestParseManifestAcceptsPrereleaseVersion(t *testing.T) {
	root := t.TempDir()
	for _, version := range []string{"1.0.0", "0.0.1", "10.20.30", "1.0.0-rc.1", "1.0.0+build.7"} {
		body := fmt.Sprintf(`{"apiVersion":"bruce.plugin/v1","name":"demo","version":%q,"description":"x","entry":"index.js"}`, version)
		if _, err := parsePlugin(t, root, "demo", body); err != nil {
			t.Errorf("version %q must be accepted: %v", version, err)
		}
	}
}

func TestParseManifestEntryValidation(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name  string
		entry string
		setup func(dir string)
	}{
		{name: "missing entry", entry: "", setup: nil},
		{name: "absolute entry", entry: "/etc/passwd", setup: nil},
		{name: "parent escape", entry: "../evil.js", setup: nil},
		{name: "nested parent escape", entry: "lib/../../evil.js", setup: nil},
		{name: "does not exist", entry: "missing.js", setup: nil},
		{name: "is a directory", entry: "lib", setup: func(dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dir := filepath.Join(root, "demo")
			body := fmt.Sprintf(`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":%q}`, testCase.entry)
			path := writePluginDir(t, root, "demo", body)
			if testCase.setup != nil {
				testCase.setup(dir)
			}
			_, err := ParseManifest([]byte(body), path)
			manifestErr := requireManifestError(t, err, "entry")
			if manifestErr.Plugin != "demo" {
				t.Errorf("entry error must name the plugin, got %q", manifestErr.Plugin)
			}
		})
	}
}

func TestParseManifestAcceptsNestedEntry(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	body := `{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"lib/index.js"}`
	path := writePluginDir(t, root, "demo", body)
	writeTestFile(t, filepath.Join(dir, "lib", "index.js"), "export function run() {}")

	manifest, err := ParseManifest([]byte(body), path)
	if err != nil {
		t.Fatalf("nested entry must be accepted: %v", err)
	}
	if manifest.Entry != "lib/index.js" {
		t.Errorf("Entry = %q", manifest.Entry)
	}
}

func TestParseManifestToolValidation(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name  string
		tools string
		field string
	}{
		{"missing tool name", `[{"handler":"run"}]`, "tools[0].name"},
		{"invalid tool name", `[{"name":"Demo-Tool","handler":"run"}]`, "tools[0].name"},
		{"tool name too long", fmt.Sprintf(`[{"name":%q,"handler":"run"}]`, strings.Repeat("a", 65)), "tools[0].name"},
		{"missing handler", `[{"name":"demo_run"}]`, "tools[0].handler"},
		{"duplicate tool", `[{"name":"demo_run","handler":"a"},{"name":"demo_run","handler":"b"}]`, "tools[1].name"},
		{"builtin tool conflict", `[{"name":"read_file","handler":"run"}]`, "tools[0].name"},
		{"invalid risk", `[{"name":"demo_run","handler":"run","risk":"catastrophic"}]`, "tools[0].risk"},
		{"negative timeout", `[{"name":"demo_run","handler":"run","timeoutMs":-1}]`, "tools[0].timeoutMs"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":%s}`, testCase.tools)
			_, err := parsePlugin(t, root, "demo", body)
			requireManifestError(t, err, testCase.field)
		})
	}
}

func TestParseManifestSchemaValidation(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name   string
		schema string
		field  string
	}{
		{"schema is not an object", `"object"`, "tools[0].schema"},
		{"schema is an array", `[{"type":"object"}]`, "tools[0].schema"},
		{"schema has no type", `{"properties":{}}`, "tools[0].schema"},
		{"schema type is not object", `{"type":"string"}`, "tools[0].schema"},
		{"schema type is an array", `{"type":["object","null"]}`, "tools[0].schema"},
		{"schema properties is not an object", `{"type":"object","properties":[]}`, "tools[0].schema"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":[{"name":"demo_run","handler":"run","schema":%s}]}`, testCase.schema)
			_, err := parsePlugin(t, root, "demo", body)
			requireManifestError(t, err, testCase.field)
		})
	}

	body := `{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":[{"name":"demo_run","handler":"run","schema":null}]}`
	manifest := mustParsePlugin(t, root, "demo", body)
	if string(manifest.Tools[0].Schema) != string(defaultToolSchema) {
		t.Errorf("null schema must default, got %s", manifest.Tools[0].Schema)
	}
}

func TestParseManifestSchemaWithBrokenJSON(t *testing.T) {
	// A schema that is not valid JSON cannot be isolated from the manifest: the
	// manifest as a whole is malformed, and that is what must be reported.
	root := t.TempDir()
	body := `{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":[{"name":"demo_run","handler":"run","schema":{"type":}}]}`
	path := writePluginDir(t, root, "demo", body)
	_, err := ParseManifest([]byte(body), path)
	manifestErr := requireManifestError(t, err, "")
	if !strings.Contains(manifestErr.Message, "malformed JSON") {
		t.Errorf("message = %q, want a malformed JSON diagnosis", manifestErr.Message)
	}
	if manifestErr.Plugin != "demo" {
		t.Errorf("Plugin = %q, want demo", manifestErr.Plugin)
	}
}

func TestParseManifestPermissionValidation(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name        string
		permissions string
		field       string
	}{
		{"unknown permission", `["root"]`, "permissions[0]"},
		{"duplicate permission", `["fs.read","fs.read"]`, "permissions[1]"},
		{"empty permission", `[""]`, "permissions[0]"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","permissions":%s}`, testCase.permissions)
			_, err := parsePlugin(t, root, "demo", body)
			requireManifestError(t, err, testCase.field)
		})
	}

	toolBody := `{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":[{"name":"demo_run","handler":"run","permissions":["nope"]}]}`
	_, err := parsePlugin(t, root, "demo", toolBody)
	requireManifestError(t, err, "tools[0].permissions[0]")
}

func TestParseManifestHookValidation(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name  string
		hooks string
		field string
	}{
		{"missing event", `[{"handler":"onStart"}]`, "hooks[0].event"},
		{"unknown event", `[{"event":"session.paused","handler":"onPause"}]`, "hooks[0].event"},
		{"missing handler", `[{"event":"session.started"}]`, "hooks[0].handler"},
		{"duplicate hook", `[{"event":"tool.before","handler":"h"},{"event":"tool.before","handler":"h"}]`, "hooks[1]"},
		{"negative timeout", `[{"event":"tool.before","handler":"h","timeoutMs":-5}]`, "hooks[0].timeoutMs"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","hooks":%s}`, testCase.hooks)
			_, err := parsePlugin(t, root, "demo", body)
			requireManifestError(t, err, testCase.field)
		})
	}

	// Every documented observer and interceptor event must be accepted, and the
	// same event with two different handlers is not a duplicate.
	var hooks []string
	for _, event := range ObserverHookEvents() {
		hooks = append(hooks, fmt.Sprintf(`{"event":%q,"handler":"observer_%s"}`, event, strings.ReplaceAll(event, ".", "_")))
	}
	for _, event := range InterceptorHookEvents() {
		hooks = append(hooks, fmt.Sprintf(`{"event":%q,"handler":"interceptor_%s"}`, event, strings.ReplaceAll(event, ".", "_")))
	}
	body := fmt.Sprintf(`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","hooks":[%s]}`, strings.Join(hooks, ","))
	manifest := mustParsePlugin(t, root, "demo", body)
	if len(manifest.Hooks) != len(observerHookEvents)+len(interceptorHookEvents) {
		t.Fatalf("hooks = %d, want %d", len(manifest.Hooks), len(observerHookEvents)+len(interceptorHookEvents))
	}

	if IsHookEvent("nope") || !IsObserverHookEvent("tool.started") || !IsInterceptorHookEvent("tool.before") {
		t.Error("hook event classification is wrong")
	}
}

func TestParseManifestCommandValidation(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name     string
		commands string
		field    string
	}{
		{"missing name", `[{"handler":"review"}]`, "commands[0].name"},
		{"uppercase name", `[{"name":"Review","handler":"review"}]`, "commands[0].name"},
		{"leading dash", `[{"name":"-review","handler":"review"}]`, "commands[0].name"},
		{"underscore name", `[{"name":"my_review","handler":"review"}]`, "commands[0].name"},
		{"name too long", fmt.Sprintf(`[{"name":%q,"handler":"review"}]`, strings.Repeat("a", 33)), "commands[0].name"},
		{"missing handler", `[{"name":"review"}]`, "commands[0].handler"},
		{"duplicate command", `[{"name":"review","handler":"a"},{"name":"review","handler":"b"}]`, "commands[1].name"},
		{"builtin command conflict", `[{"name":"help","handler":"review"}]`, "commands[0].name"},
		{"unknown permission", `[{"name":"review","handler":"review","permissions":["admin"]}]`, "commands[0].permissions[0]"},
		{"negative timeout", `[{"name":"review","handler":"review","timeoutMs":-1}]`, "commands[0].timeoutMs"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","commands":%s}`, testCase.commands)
			_, err := parsePlugin(t, root, "demo", body)
			requireManifestError(t, err, testCase.field)
		})
	}

	body := fmt.Sprintf(`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","commands":[{"name":%q,"handler":"review"}]}`, strings.Repeat("a", 32))
	if _, err := parsePlugin(t, root, "demo", body); err != nil {
		t.Errorf("a 32 character command name must be accepted: %v", err)
	}
}

func TestParseManifestConcurrencyRange(t *testing.T) {
	root := t.TempDir()
	for _, maxRuntimes := range []int{-1, 65, 1000} {
		body := fmt.Sprintf(`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","concurrency":{"maxRuntimes":%d}}`, maxRuntimes)
		_, err := parsePlugin(t, root, "demo", body)
		requireManifestError(t, err, "concurrency.maxRuntimes")
	}
	for _, maxRuntimes := range []int{0, 1, 64} {
		body := fmt.Sprintf(`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","concurrency":{"maxRuntimes":%d}}`, maxRuntimes)
		manifest := mustParsePlugin(t, root, "demo", body)
		if manifest.Concurrency.MaxRuntimes != maxRuntimes {
			t.Errorf("MaxRuntimes = %d, want %d", manifest.Concurrency.MaxRuntimes, maxRuntimes)
		}
	}
}

func TestParseManifestRejectsUnknownFields(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{
			"misspelled top-level field",
			`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","entryPoint":"index.js"}`,
			"entryPoint",
		},
		{
			"misspelled tool field",
			`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":[{"name":"demo_run","handler":"run","timeoutMS":10}]}`,
			"tools[0].timeoutMS",
		},
		{
			"host bookkeeping field",
			`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","rootDir":"/tmp"}`,
			"rootDir",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parsePlugin(t, root, "demo", testCase.body)
			manifestErr := requireManifestError(t, err, testCase.field)
			if !strings.Contains(manifestErr.Message, "unknown field") {
				t.Errorf("message must explain the unknown field: %v", err)
			}
		})
	}
}

func TestParseManifestRejectsCaseOnlyFieldTypos(t *testing.T) {
	// encoding/json matches struct tags case-insensitively, so DisallowUnknownFields
	// alone accepts "timeoutMS" and silently applies the zero value. These cases
	// pin the extra guard that turns such a typo into a diagnostic.
	root := t.TempDir()
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{
			"top-level casing",
			`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","ApiVersion":"bruce.plugin/v1"}`,
			"ApiVersion",
		},
		{
			"tool timeout casing",
			`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":[{"name":"demo_run","handler":"run","timeoutMS":10}]}`,
			"tools[0].timeoutMS",
		},
		{
			"tool parallel safe casing",
			`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":[{"name":"demo_run","handler":"run","parallelsafe":true}]}`,
			"tools[0].parallelsafe",
		},
		{
			"concurrency casing",
			`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","concurrency":{"maxruntimes":2}}`,
			"concurrency.maxruntimes",
		},
		{
			"command timeout casing in second element",
			`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","commands":[{"name":"a","handler":"h"},{"name":"b","handler":"h","TimeOutMs":5}]}`,
			"commands[1].TimeOutMs",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parsePlugin(t, root, "demo", testCase.body)
			manifestErr := requireManifestError(t, err, testCase.field)
			if !strings.Contains(manifestErr.Message, "did you mean") {
				t.Errorf("message must suggest the canonical spelling: %q", manifestErr.Message)
			}
		})
	}
}

func TestParseManifestMetadataKeysAreFreeForm(t *testing.T) {
	root := t.TempDir()
	body := `{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","metadata":{"AnyKey":"v","another_key":"w"}}`
	manifest := mustParsePlugin(t, root, "demo", body)
	if manifest.Metadata["AnyKey"] != "v" || manifest.Metadata["another_key"] != "w" {
		t.Fatalf("metadata = %v", manifest.Metadata)
	}
}

func TestParseManifestWrongFieldTypeNamesTheField(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{
			"tools is an object",
			`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":{}}`,
			"tools",
		},
		{
			"version is a number",
			`{"apiVersion":"bruce.plugin/v1","name":"demo","version":1,"description":"x","entry":"index.js"}`,
			"version",
		},
		{
			"concurrency is a string",
			`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","concurrency":"many"}`,
			"concurrency",
		},
		{
			"tool timeoutMs is a string",
			`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":[{"name":"demo_run","handler":"run","timeoutMs":"soon"}]}`,
			"tools.timeoutMs",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parsePlugin(t, root, "demo", testCase.body)
			manifestErr := requireManifestError(t, err, testCase.field)
			if manifestErr.Plugin != "demo" {
				t.Errorf("Plugin = %q, want demo", manifestErr.Plugin)
			}
			if !strings.Contains(manifestErr.Message, "must be") {
				t.Errorf("message must state the expected type: %q", manifestErr.Message)
			}
		})
	}
}

func TestAttributePluginNameSurvivesBrokenTail(t *testing.T) {
	// The name is often declared after a large tools array. A syntax error in
	// that array must not cost the diagnostic its plugin identity, so the
	// recovery scan has to skip nested values rather than give up.
	cases := map[string]string{
		`{"name":"demo","tools":[{"name":"t","handler":"h"}],"version":}`:        "demo",
		`{"tools":{"nested":{"deep":[1,2,{"x":"y"}]}},"name":"demo","version":}`: "demo",
		`{"name":"demo","metadata":{"a":{"b":"c"}},"version":}`:                  "demo",
		`{"tools":[{"name":"t"}],"name":"demo"`:                                  "demo",
		`{"name":123}`:                                                           "",
		`{"tools":[]}`:                                                           "",
		`[]`:                                                                     "",
		``:                                                                       "",
		`not json`:                                                               "",
	}
	for body, want := range cases {
		if got := attributePluginName([]byte(body)); got != want {
			t.Errorf("attributePluginName(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestSortDiagnosticsIsFullyOrdered(t *testing.T) {
	diagnostics := []Diagnostic{
		{Path: "/b", Plugin: "zeta", Message: "m"},
		{Path: "/a", Plugin: "beta", Message: "z"},
		{Path: "/a", Plugin: "beta", Message: "a"},
		{Path: "/a", Plugin: "alpha", Message: "m"},
	}
	sortDiagnostics(diagnostics)
	got := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		got = append(got, diagnostic.Path+"/"+diagnostic.Plugin+"/"+diagnostic.Message)
	}
	want := []string{"/a/alpha/m", "/a/beta/a", "/a/beta/z", "/b/zeta/m"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sorted = %v, want %v", got, want)
	}
}

func TestParseManifestMalformedJSON(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"whitespace", "   \n\t "},
		{"truncated", `{"apiVersion":"bruce.plugin/v1","name":"demo"`},
		{"trailing comma", `{"apiVersion":"bruce.plugin/v1","name":"demo",}`},
		{"top-level array", `[{"name":"demo"}]`},
		{"bare string", `"demo"`},
		{"two objects", `{"name":"demo"}{"name":"other"}`},
		{"wrong type for tools", `{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":{}}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := writePluginDir(t, root, "demo", testCase.body)
			_, err := ParseManifest([]byte(testCase.body), path)
			requireManifestError(t, err, "")
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error must carry the manifest path: %v", err)
			}
		})
	}
}

func TestParseManifestMalformedJSONStillNamesPlugin(t *testing.T) {
	root := t.TempDir()
	body := `{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":[{"name":"demo_run","handler":"run","bogus":1}]}`
	path := writePluginDir(t, root, "demo", body)
	_, err := ParseManifest([]byte(body), path)
	manifestErr := requireManifestError(t, err, "bogus")
	if manifestErr.Plugin != "demo" {
		t.Errorf("a partially decoded manifest must still name the plugin, got %q", manifestErr.Plugin)
	}
}

func TestParseManifestEveryFailureIsManifestError(t *testing.T) {
	root := t.TempDir()
	bodies := []string{
		"",
		`[]`,
		`{"apiVersion":"bruce.plugin/v1"}`,
		`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"x","description":"x","entry":"index.js"}`,
		`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"/abs.js"}`,
		`{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","unknown":1}`,
	}
	for index, body := range bodies {
		path := writePluginDir(t, root, "demo", body)
		_, err := ParseManifest([]byte(body), path)
		if err == nil {
			t.Fatalf("case %d must fail", index)
		}
		var manifestErr *ManifestError
		if !errors.As(err, &manifestErr) {
			t.Fatalf("case %d: error %v (%T) is not a *ManifestError", index, err, err)
		}
	}
}

func TestLoadManifestReadsFromDisk(t *testing.T) {
	root := t.TempDir()
	path := writePluginDir(t, root, "demo", minimalManifest("demo"))
	writeTestFile(t, filepath.Join(root, "demo", "index.js"), "export function run() {}")

	manifest, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest failed: %v", err)
	}
	if manifest.Name != "demo" || manifest.File != path {
		t.Fatalf("manifest = %+v", manifest)
	}

	_, err = LoadManifest(filepath.Join(root, "demo", "missing.json"))
	manifestErr := requireManifestError(t, err, "")
	if !strings.Contains(manifestErr.Message, "failed to read manifest") {
		t.Errorf("a missing file must be reported as a read failure: %v", err)
	}
}

func TestParseManifestSingleFileLayoutResolvesEntryAgainstRoot(t *testing.T) {
	root := t.TempDir()
	body := `{"apiVersion":"bruce.plugin/v1","name":"solo","version":"1.0.0","description":"x","entry":"lib/index.js"}`
	path := filepath.Join(root, "solo.json")
	writeTestFile(t, path, body)
	writeTestFile(t, filepath.Join(root, "lib", "index.js"), "export function run() {}")

	manifest, err := ParseManifest([]byte(body), path)
	if err != nil {
		t.Fatalf("single-file manifest must resolve its entry against the root: %v", err)
	}
	if manifest.RootDir != root {
		t.Errorf("RootDir = %q, want %q", manifest.RootDir, root)
	}
	if manifest.File != path {
		t.Errorf("File = %q", manifest.File)
	}
}

func TestParseManifestRedactsSecretsInDiagnostics(t *testing.T) {
	root := t.TempDir()
	body := `{"apiVersion":"bruce.plugin/v1","name":"demo","version":"1.0.0","description":"x","entry":"index.js","tools":[{"name":"demo_run","handler":"run","schema":"apiKey=supersecretvalue"}]}`
	path := writePluginDir(t, root, "demo", body)
	_, err := ParseManifest([]byte(body), path)
	if err == nil {
		t.Fatal("expected a schema error")
	}
	if strings.Contains(err.Error(), "supersecretvalue") {
		t.Errorf("manifest diagnostics leaked a secret: %v", err)
	}
}

func TestManifestJSONShapeIsCamelCase(t *testing.T) {
	// The wire names are part of the documented manifest schema; a rename here
	// silently breaks every published plugin.
	body := fmt.Sprintf(`{
	  "apiVersion": %q,
	  "name": "demo",
	  "version": "1.0.0",
	  "description": "x",
	  "entry": "index.js",
	  "tools": [{"name":"demo_run","handler":"run","parallelSafe":true,"timeoutMs":10,"promptSnippet":"p"}],
	  "hooks": [{"event":"tool.before","handler":"h","timeoutMs":10}],
	  "commands": [{"name":"demo-cmd","handler":"c","usage":"u","timeoutMs":10}],
	  "concurrency": {"maxRuntimes": 2, "parallelSafe": true},
	  "metadata": {"k": "v"}
	}`, APIVersion)
	root := t.TempDir()
	manifest := mustParsePlugin(t, root, "demo", body)

	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(encoded)
	for _, want := range []string{`"apiVersion"`, `"promptSnippet"`, `"timeoutMs"`, `"parallelSafe"`, `"maxRuntimes"`, `"metadata"`} {
		if !strings.Contains(text, want) {
			t.Errorf("marshalled manifest is missing %s: %s", want, text)
		}
	}
}
