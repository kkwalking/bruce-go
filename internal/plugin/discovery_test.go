package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedPlugin writes <root>/<name>/plugin.json plus the index.js it declares,
// so the manifest is valid and only the behaviour under test can fail it.
func seedPlugin(t *testing.T, root, name, body string) {
	t.Helper()
	writePluginDir(t, root, name, body)
	writeTestFile(t, filepath.Join(root, name, "index.js"), "export function run() {}\n")
}

// seedPluginWithTool writes a valid plugin declaring one tool.
func seedPluginWithTool(t *testing.T, root, name, tool string) {
	t.Helper()
	seedPlugin(t, root, name, fmt.Sprintf(`{
	  "apiVersion": %q,
	  "name": %q,
	  "version": "1.0.0",
	  "description": "plugin %s",
	  "entry": "index.js",
	  "tools": [{"name": %q, "handler": "run"}]
	}`, APIVersion, name, name, tool))
}

// seedPluginWithCommand writes a valid plugin declaring one slash command.
func seedPluginWithCommand(t *testing.T, root, name, command string) {
	t.Helper()
	seedPlugin(t, root, name, fmt.Sprintf(`{
	  "apiVersion": %q,
	  "name": %q,
	  "version": "1.0.0",
	  "description": "plugin %s",
	  "entry": "index.js",
	  "commands": [{"name": %q, "handler": "run"}]
	}`, APIVersion, name, name, command))
}

func pluginNames(manifests []*Manifest) []string {
	names := make([]string, 0, len(manifests))
	for _, manifest := range manifests {
		names = append(names, manifest.Name)
	}
	return names
}

func manifestNamed(t *testing.T, result LoadResult, name string) *Manifest {
	t.Helper()
	for _, manifest := range result.Manifests {
		if manifest.Name == name {
			return manifest
		}
	}
	t.Fatalf("plugin %q is missing from %v", name, pluginNames(result.Manifests))
	return nil
}

func diagnosticText(result LoadResult) string {
	parts := make([]string, 0, len(result.Diagnostics))
	for _, diagnostic := range result.Diagnostics {
		parts = append(parts, diagnostic.Path+"|"+diagnostic.Plugin+"|"+diagnostic.Message)
	}
	return strings.Join(parts, "\n")
}

func TestDiscoveryLoadsBothRootsSortedByName(t *testing.T) {
	userRoot := filepath.Join(t.TempDir(), "user-plugins")
	workspaceRoot := filepath.Join(t.TempDir(), "workspace-plugins")
	seedPlugin(t, userRoot, "zulu", minimalManifest("zulu"))
	seedPlugin(t, workspaceRoot, "alpha", minimalManifest("alpha"))
	seedPlugin(t, workspaceRoot, "mike", minimalManifest("mike"))

	result, err := Discovery{UserRoot: userRoot, WorkspaceRoot: workspaceRoot}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := pluginNames(result.Manifests); strings.Join(got, ",") != "alpha,mike,zulu" {
		t.Fatalf("manifests = %v, want [alpha mike zulu]", got)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %s", diagnosticText(result))
	}
	if len(result.Overrides) != 0 {
		t.Fatalf("unexpected overrides: %v", result.Overrides)
	}
	if manifestNamed(t, result, "zulu").Source != SourceUser {
		t.Error("a user root plugin must be recorded as SourceUser")
	}
	if manifestNamed(t, result, "alpha").Source != SourceWorkspace {
		t.Error("a workspace root plugin must be recorded as SourceWorkspace")
	}
	if manifestNamed(t, result, "alpha").File != filepath.Join(workspaceRoot, "alpha", "plugin.json") {
		t.Error("File must point at the manifest that was loaded")
	}
}

func TestDiscoveryMissingRootsIsNotAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	result, err := Discovery{UserRoot: missing, WorkspaceRoot: filepath.Join(missing, "also-nope")}.Load()
	if err != nil {
		t.Fatalf("a missing plugin root must not be an error: %v", err)
	}
	if len(result.Manifests) != 0 || len(result.Diagnostics) != 0 {
		t.Fatalf("empty roots must load nothing: %+v", result)
	}
	if result.Manifests == nil {
		t.Error("Manifests must be an empty slice, not nil")
	}
}

func TestDiscoveryEmptyConfig(t *testing.T) {
	result, err := Discovery{}.Load()
	if err != nil {
		t.Fatalf("an unconfigured discovery must not fail: %v", err)
	}
	if len(result.Manifests) != 0 {
		t.Fatalf("manifests = %v", pluginNames(result.Manifests))
	}
}

func TestDiscoveryWorkspaceOverridesUser(t *testing.T) {
	userRoot := filepath.Join(t.TempDir(), "user-plugins")
	workspaceRoot := filepath.Join(t.TempDir(), "workspace-plugins")
	seedPlugin(t, userRoot, "demo", fmt.Sprintf(`{
	  "apiVersion": %q, "name": "demo", "version": "1.0.0",
	  "description": "user copy", "entry": "index.js"
	}`, APIVersion))
	seedPlugin(t, workspaceRoot, "demo", fmt.Sprintf(`{
	  "apiVersion": %q, "name": "demo", "version": "2.0.0",
	  "description": "workspace copy", "entry": "index.js"
	}`, APIVersion))

	result, err := Discovery{UserRoot: userRoot, WorkspaceRoot: workspaceRoot}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(result.Manifests) != 1 {
		t.Fatalf("manifests = %v, want exactly one", pluginNames(result.Manifests))
	}
	winner := result.Manifests[0]
	if winner.Version != "2.0.0" || winner.Description != "workspace copy" || winner.Source != SourceWorkspace {
		t.Fatalf("workspace plugin must win: %+v", winner)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("an override is not a diagnostic: %s", diagnosticText(result))
	}
	if len(result.Overrides) != 1 {
		t.Fatalf("overrides = %v, want one entry", result.Overrides)
	}
	want := "demo: workspace overrides user"
	if result.Overrides[0] != want {
		t.Errorf("overrides[0] = %q, want %q", result.Overrides[0], want)
	}
}

func TestDiscoveryDuplicateNameWithinOneRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	// Two different directories claiming the same plugin name. Neither may win
	// silently: which one loads would depend on scan order.
	seedPlugin(t, root, "demo-a", minimalManifest("demo"))
	seedPlugin(t, root, "demo-b", minimalManifest("demo"))

	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(result.Manifests) != 1 {
		t.Fatalf("manifests = %v, want exactly one", pluginNames(result.Manifests))
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("expected one diagnostic, got: %s", diagnosticText(result))
	}
	diagnostic := result.Diagnostics[0]
	if !strings.Contains(diagnostic.Message, "already declared") {
		t.Errorf("diagnostic = %q", diagnostic.Message)
	}
	if diagnostic.Path != filepath.Join(root, "demo-b", "plugin.json") {
		t.Errorf("diagnostic.Path = %q, want the ignored manifest", diagnostic.Path)
	}
	if len(result.Overrides) != 0 {
		t.Errorf("a same-root duplicate is not an override: %v", result.Overrides)
	}
}

func TestDiscoveryBrokenPluginDoesNotStopTheOthers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	seedPlugin(t, root, "good", minimalManifest("good"))
	writePluginDir(t, root, "broken", `{"apiVersion":"bruce.plugin/v1","name":"broken"}`)
	writeTestFile(t, filepath.Join(root, "broken", "index.js"), "")

	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("a broken plugin must not fail the load: %v", err)
	}
	if got := pluginNames(result.Manifests); strings.Join(got, ",") != "good" {
		t.Fatalf("manifests = %v, want [good]", got)
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("expected one diagnostic, got: %s", diagnosticText(result))
	}
	diagnostic := result.Diagnostics[0]
	if diagnostic.Plugin != "broken" {
		t.Errorf("diagnostic.Plugin = %q, want broken", diagnostic.Plugin)
	}
	if !strings.Contains(diagnostic.Message, "version") {
		t.Errorf("diagnostic must name the failing field: %q", diagnostic.Message)
	}
}

func TestDiscoveryDiagnosticsAreSorted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	// Created in reverse order on purpose: the report must not depend on the
	// order the filesystem happens to enumerate.
	for _, name := range []string{"zulu", "mike", "alpha"} {
		writePluginDir(t, root, name, `{"apiVersion":"bruce.plugin/v1","name":"`+name+`"}`)
	}
	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(result.Diagnostics) != 3 {
		t.Fatalf("expected three diagnostics, got: %s", diagnosticText(result))
	}
	paths := make([]string, 0, len(result.Diagnostics))
	for _, diagnostic := range result.Diagnostics {
		paths = append(paths, diagnostic.Path)
	}
	for i := 1; i < len(paths); i++ {
		if paths[i-1] >= paths[i] {
			t.Fatalf("diagnostics are not sorted: %v", paths)
		}
	}
}

func TestDiscoveryFailFastReturnsTheFirstError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	seedPlugin(t, root, "good", minimalManifest("good"))
	writePluginDir(t, root, "broken", `{"apiVersion":"bruce.plugin/v1","name":"broken"}`)
	writeTestFile(t, filepath.Join(root, "broken", "index.js"), "")

	_, err := Discovery{UserRoot: root, FailFast: true}.Load()
	manifestErr := requireManifestError(t, err, "version")
	if manifestErr.Plugin != "broken" {
		t.Errorf("FailFast error must name the broken plugin, got %q", manifestErr.Plugin)
	}

	// Without FailFast the same tree loads the healthy plugin.
	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(result.Manifests) != 1 || len(result.Diagnostics) != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestDiscoveryFailFastOnSameRootDuplicate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	seedPlugin(t, root, "demo-a", minimalManifest("demo"))
	seedPlugin(t, root, "demo-b", minimalManifest("demo"))

	_, err := Discovery{UserRoot: root, FailFast: true}.Load()
	requireManifestError(t, err, "name")
}

func TestDiscoverySupportsBothLayouts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	seedPlugin(t, root, "directory-layout", minimalManifest("directory-layout"))
	// <root>/<name>.json: entry resolves against the root itself.
	writeTestFile(t, filepath.Join(root, "single.json"), minimalManifest("single"))
	writeTestFile(t, filepath.Join(root, "index.js"), "export function run() {}\n")

	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := pluginNames(result.Manifests); strings.Join(got, ",") != "directory-layout,single" {
		t.Fatalf("manifests = %v", got)
	}
	single := manifestNamed(t, result, "single")
	if single.RootDir != root {
		t.Errorf("single-file RootDir = %q, want %q", single.RootDir, root)
	}
	if single.File != filepath.Join(root, "single.json") {
		t.Errorf("single-file File = %q", single.File)
	}
	if single.Entry != "index.js" {
		t.Errorf("single-file Entry = %q", single.Entry)
	}
	if directory := manifestNamed(t, result, "directory-layout"); directory.RootDir != filepath.Join(root, "directory-layout") {
		t.Errorf("directory RootDir = %q", directory.RootDir)
	}
}

func TestDiscoveryIgnoresUnrelatedEntries(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	seedPlugin(t, root, "real", minimalManifest("real"))
	writeTestFile(t, filepath.Join(root, "README.md"), "not a plugin")
	writeTestFile(t, filepath.Join(root, "notes.txt"), "not a plugin")
	writeTestFile(t, filepath.Join(root, "manifest.yaml"), "not: a plugin")
	writeTestFile(t, filepath.Join(root, "broken.json"), `{"apiVersion":"bruce.plugin/v1"}`)
	if err := os.MkdirAll(filepath.Join(root, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := pluginNames(result.Manifests); strings.Join(got, ",") != "real" {
		t.Fatalf("manifests = %v, want [real]", got)
	}
	// broken.json is a manifest-shaped file, so it is reported; the other
	// entries are not manifests at all and stay silent.
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Path != filepath.Join(root, "broken.json") {
		t.Fatalf("diagnostics = %s", diagnosticText(result))
	}
}

func TestDiscoveryCrossPluginToolConflict(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	seedPluginWithTool(t, root, "alpha", "shared_tool")
	seedPluginWithTool(t, root, "beta", "shared_tool")
	seedPluginWithTool(t, root, "gamma", "gamma_only")

	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	alpha := manifestNamed(t, result, "alpha")
	if len(alpha.Tools) != 1 || alpha.Tools[0].Name != "shared_tool" {
		t.Fatalf("the first plugin in name order must keep the tool: %+v", alpha.Tools)
	}
	beta := manifestNamed(t, result, "beta")
	if len(beta.Tools) != 0 {
		t.Fatalf("the conflicting declaration must be dropped, not overwritten: %+v", beta.Tools)
	}
	if gamma := manifestNamed(t, result, "gamma"); len(gamma.Tools) != 1 {
		t.Fatalf("an unrelated tool must survive: %+v", gamma.Tools)
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("expected one conflict diagnostic, got: %s", diagnosticText(result))
	}
	diagnostic := result.Diagnostics[0]
	if diagnostic.Plugin != "beta" || diagnostic.Path != filepath.Join(root, "beta", "plugin.json") {
		t.Errorf("diagnostic identity = %+v", diagnostic)
	}
	for _, want := range []string{"shared_tool", "alpha"} {
		if !strings.Contains(diagnostic.Message, want) {
			t.Errorf("diagnostic %q must mention %q", diagnostic.Message, want)
		}
	}
}

func TestDiscoveryCrossPluginCommandConflict(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	seedPluginWithCommand(t, root, "alpha", "review")
	seedPluginWithCommand(t, root, "beta", "review")

	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if commands := manifestNamed(t, result, "alpha").Commands; len(commands) != 1 {
		t.Fatalf("alpha commands = %+v", commands)
	}
	if commands := manifestNamed(t, result, "beta").Commands; len(commands) != 0 {
		t.Fatalf("beta commands must be dropped: %+v", commands)
	}
	if len(result.Diagnostics) != 1 || !strings.Contains(result.Diagnostics[0].Message, "review") {
		t.Fatalf("diagnostics = %s", diagnosticText(result))
	}
}

func TestDiscoveryToolAndCommandNamespacesAreSeparate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	seedPluginWithTool(t, root, "alpha", "review")
	seedPluginWithCommand(t, root, "beta", "review")

	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("a tool and a command may share a name: %s", diagnosticText(result))
	}
}

func TestDiscoveryOverrideAlsoReleasesTheOverriddenDeclarations(t *testing.T) {
	userRoot := filepath.Join(t.TempDir(), "user-plugins")
	workspaceRoot := filepath.Join(t.TempDir(), "workspace-plugins")
	seedPluginWithTool(t, userRoot, "demo", "demo_tool")
	seedPluginWithTool(t, workspaceRoot, "demo", "demo_tool")
	seedPluginWithTool(t, workspaceRoot, "other", "demo_tool")

	result, err := Discovery{UserRoot: userRoot, WorkspaceRoot: workspaceRoot}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(result.Manifests) != 2 {
		t.Fatalf("manifests = %v", pluginNames(result.Manifests))
	}
	// The user copy never loads, so it must not reserve the tool name and turn
	// the workspace copy's own declaration into a conflict.
	demo := manifestNamed(t, result, "demo")
	if len(demo.Tools) != 1 || demo.Source != SourceWorkspace {
		t.Fatalf("workspace demo = %+v", demo)
	}
	if other := manifestNamed(t, result, "other"); len(other.Tools) != 0 {
		t.Fatalf("other must lose the conflict to demo: %+v", other.Tools)
	}
	if len(result.Overrides) != 1 {
		t.Fatalf("overrides = %v", result.Overrides)
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %s", diagnosticText(result))
	}
}

func TestDiscoveryIsDeterministic(t *testing.T) {
	userRoot := filepath.Join(t.TempDir(), "user-plugins")
	workspaceRoot := filepath.Join(t.TempDir(), "workspace-plugins")
	seedPlugin(t, userRoot, "shared", minimalManifest("shared"))
	seedPlugin(t, workspaceRoot, "shared", minimalManifest("shared"))
	seedPluginWithTool(t, workspaceRoot, "alpha", "dup_tool")
	seedPluginWithTool(t, workspaceRoot, "beta", "dup_tool")
	seedPluginWithCommand(t, workspaceRoot, "gamma", "dup-cmd")
	seedPluginWithCommand(t, workspaceRoot, "delta", "dup-cmd")
	writePluginDir(t, workspaceRoot, "broken-one", `{"apiVersion":"bruce.plugin/v1","name":"broken-one"}`)
	writePluginDir(t, workspaceRoot, "broken-two", `not json at all`)

	discovery := Discovery{UserRoot: userRoot, WorkspaceRoot: workspaceRoot}
	first, err := discovery.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	reference := fmt.Sprintf("names=%v\ndiagnostics=%s\noverrides=%v",
		pluginNames(first.Manifests), diagnosticText(first), first.Overrides)
	if len(first.Diagnostics) != 4 {
		t.Fatalf("expected four diagnostics, got: %s", diagnosticText(first))
	}
	for attempt := 0; attempt < 5; attempt++ {
		next, err := discovery.Load()
		if err != nil {
			t.Fatalf("Load failed on attempt %d: %v", attempt, err)
		}
		got := fmt.Sprintf("names=%v\ndiagnostics=%s\noverrides=%v",
			pluginNames(next.Manifests), diagnosticText(next), next.Overrides)
		if got != reference {
			t.Fatalf("attempt %d is not deterministic:\n%s\nwant:\n%s", attempt, got, reference)
		}
	}
}

func TestDiscoveryUnreadableRootBecomesDiagnostic(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are not enforced")
	}
	root := filepath.Join(t.TempDir(), "plugins")
	seedPlugin(t, root, "good", minimalManifest("good"))
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("an unreadable root must not abort the load: %v", err)
	}
	if len(result.Manifests) != 0 {
		t.Fatalf("manifests = %v", pluginNames(result.Manifests))
	}
	if len(result.Diagnostics) != 1 || !strings.Contains(result.Diagnostics[0].Message, "failed to scan plugin directory") {
		t.Fatalf("diagnostics = %s", diagnosticText(result))
	}

	if _, err := (Discovery{UserRoot: root, FailFast: true}).Load(); err == nil {
		t.Fatal("FailFast must turn an unreadable root into an error")
	}
}

func TestDiscoveryFollowsSymlinkedPluginDirectory(t *testing.T) {
	// Installing a plugin by linking a checkout into the plugin directory is a
	// normal workflow; skipping it silently would look like "the plugin did not
	// load".
	real := filepath.Join(t.TempDir(), "checkout")
	seedPlugin(t, real, "linked", minimalManifest("linked"))

	root := filepath.Join(t.TempDir(), "plugins")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "linked"), filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := pluginNames(result.Manifests); strings.Join(got, ",") != "linked" {
		t.Fatalf("manifests = %v, want [linked] (diagnostics: %s)", got, diagnosticText(result))
	}
}

func TestDiscoveryReportsUnusableManifestPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	seedPlugin(t, root, "good", minimalManifest("good"))
	// A directory whose plugin.json is itself a directory: the plugin is
	// present but unusable, so it must be reported, not skipped.
	if err := os.MkdirAll(filepath.Join(root, "weird", "plugin.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := pluginNames(result.Manifests); strings.Join(got, ",") != "good" {
		t.Fatalf("manifests = %v, want [good]", got)
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %s", diagnosticText(result))
	}
	if !strings.Contains(result.Diagnostics[0].Message, "regular file") {
		t.Errorf("diagnostic = %q", result.Diagnostics[0].Message)
	}
}

func TestDiscoverySkipsBrokenSymlinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	seedPlugin(t, root, "good", minimalManifest("good"))
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), filepath.Join(root, "dangling")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	result, err := Discovery{UserRoot: root}.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := pluginNames(result.Manifests); strings.Join(got, ",") != "good" {
		t.Fatalf("manifests = %v, want [good]", got)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("a dangling symlink is not a plugin: %s", diagnosticText(result))
	}
}

func TestDiagnosticStringIsRedacted(t *testing.T) {
	diagnostic := Diagnostic{Path: "/tmp/demo/plugin.json", Plugin: "demo", Message: "request failed: apiKey=supersecretvalue"}
	text := diagnostic.String()
	if strings.Contains(text, "supersecretvalue") {
		t.Errorf("diagnostic leaked a secret: %q", text)
	}
	for _, want := range []string{"demo", "/tmp/demo/plugin.json"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic %q must contain %q", text, want)
		}
	}
}
