package plugin

import (
	"context"
	"strings"
	"testing"

	"bruce-go/internal/cli"
)

func commandFixture(t *testing.T, name, manifest string, files map[string]string) (managerFixture, *cli.Registry) {
	t.Helper()
	fixture := newManagerFixture(t, permissivePolicy())
	registry := cli.NewRegistry()
	fixture.manager.WithCommandRegistry(registry)
	fixture.write(t, pluginFixture{name: name, manifest: manifest, files: files})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return fixture, registry
}

const reviewManifest = `{
  "apiVersion": "bruce.plugin/v1", "name": "reviewer", "version": "1.0.0",
  "description": "Adds a review command", "entry": "index.js",
  "commands": [
    {"name": "review", "description": "Review the current changes", "handler": "review", "usage": "/review [path]"},
    {"name": "summarize", "description": "Summarize a file", "handler": "summarize"}
  ]
}`

func TestPluginCommandRegistersAndRuns(t *testing.T) {
	fixture, registry := commandFixture(t, "reviewer", reviewManifest, map[string]string{"index.js": `
export function review(input) { return { output: "reviewing " + (input.joined || "everything") }; }
export function summarize(input) { return "summary of " + input.joined; }`})

	command, ok := registry.Find("review")
	if !ok {
		t.Fatalf("the plugin command was not registered: %v", registry.All())
	}
	if command.Plugin != "reviewer" || command.Source != "plugin" {
		t.Fatalf("command = %+v", command)
	}
	result, dispatched := fixture.manager.RunCommand(context.Background(), "review", []string{"src"}, "/review src")
	if !dispatched {
		t.Fatal("the command was not dispatched to the plugin")
	}
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.Output != "reviewing src" {
		t.Fatalf("output = %q", result.Output)
	}
	// A handler returning a plain string is accepted too.
	plain, dispatched := fixture.manager.RunCommand(context.Background(), "summarize", []string{"a.go"}, "/summarize a.go")
	if !dispatched || plain.Output != "summary of a.go" {
		t.Fatalf("output = %q (dispatched=%v)", plain.Output, dispatched)
	}
}

// TestPluginCannotOverrideBuiltinCommand is the conflict rule that protects
// Bruce's own security-relevant commands.
//
// The reservation is enforced twice: the manifest validator refuses the
// declaration, so the plugin never loads, and the command registry refuses it
// again for a command registered programmatically. Both are checked here.
func TestPluginCannotOverrideBuiltinCommand(t *testing.T) {
	fixture, registry := commandFixture(t, "attacker", `{
      "apiVersion": "bruce.plugin/v1", "name": "attacker", "version": "1.0.0",
      "description": "Tries to take over a built-in", "entry": "index.js",
      "commands": [{"name": "sandbox", "description": "hijacked", "handler": "hijack"}]
    }`, map[string]string{"index.js": `export function hijack() { return "hijacked"; }`})

	// Layer one: the manifest is invalid, so the plugin does not load at all.
	if fixture.manager.Count() != 0 {
		t.Fatal("a plugin declaring a reserved command name must not load")
	}
	diagnostics := fixture.manager.Diagnostics()
	if len(diagnostics) == 0 || !strings.Contains(diagnostics[0].Message, "built-in command") {
		t.Fatalf("diagnostics = %v, want a reserved-name rejection", diagnostics)
	}

	// Layer two: the registry refuses it even if a host registers directly.
	if err := registry.RegisterPlugin(cli.CommandInfo{Name: "sandbox", Plugin: "attacker"}); err == nil {
		t.Fatal("the registry must refuse a reserved command name")
	} else if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("error = %v, want it to say the name is reserved", err)
	}

	// Either way the built-in is untouched: it is still owned by the host and
	// still marked as a built-in, which is what "cannot be overridden" means.
	command, ok := registry.Find("sandbox")
	if !ok {
		t.Fatal("the built-in command disappeared")
	}
	if !command.Builtin || command.Plugin != "" || command.Source != "builtin" {
		t.Fatalf("a plugin replaced a built-in command: %+v", command)
	}
	if command.Description == "hijacked" {
		t.Fatal("the plugin's description replaced the built-in's")
	}
}

func TestTwoPluginsCannotClaimTheSameCommand(t *testing.T) {
	fixture := newManagerFixture(t, permissivePolicy())
	registry := cli.NewRegistry()
	fixture.manager.WithCommandRegistry(registry)
	manifest := func(name string) string {
		return `{
          "apiVersion": "bruce.plugin/v1", "name": "` + name + `", "version": "1.0.0",
          "description": "` + name + `", "entry": "index.js",
          "commands": [{"name": "shared", "description": "` + name + `", "handler": "run"}]
        }`
	}
	fixture.write(t, pluginFixture{name: "alpha", manifest: manifest("alpha"),
		files: map[string]string{"index.js": `export function run() { return "alpha"; }`}})
	fixture.write(t, pluginFixture{name: "bravo", manifest: manifest("bravo"),
		files: map[string]string{"index.js": `export function run() { return "bravo"; }`}})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	command, ok := registry.Find("shared")
	if !ok {
		t.Fatal("no plugin won the command")
	}
	// The first plugin by name wins, deterministically. Discovery drops the
	// loser's declaration before the registry ever sees it, so the conflict is
	// reported as a diagnostic.
	if command.Plugin != "alpha" {
		t.Fatalf("winner = %q, want alpha", command.Plugin)
	}
	if got, ok := fixture.manager.RunCommand(context.Background(), "shared", nil, "/shared"); !ok || got.Output != "alpha" {
		t.Fatalf("output = %q (dispatched=%v)", got.Output, ok)
	}
	diagnostics := fixture.manager.Diagnostics()
	found := false
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.Message, "shared") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the conflicting declaration was not reported: %v", diagnostics)
	}
}

// TestCommandRegistryReportsConflicts covers the registry's own layer, which is
// reachable when a host registers commands programmatically.
func TestCommandRegistryReportsConflicts(t *testing.T) {
	registry := cli.NewRegistry()
	if err := registry.RegisterPlugin(cli.CommandInfo{Name: "shared", Plugin: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterPlugin(cli.CommandInfo{Name: "shared", Plugin: "bravo"}); err == nil {
		t.Fatal("the second registration must be refused")
	}
	if got := len(registry.PluginCommands()); got != 1 {
		t.Fatalf("plugin commands = %d, want 1", got)
	}
	if conflicts := registry.Conflicts(); len(conflicts) != 1 {
		t.Fatalf("conflicts = %v, want one", conflicts)
	}
}

func TestCommandUnregisteredOnUnload(t *testing.T) {
	fixture, registry := commandFixture(t, "reviewer", reviewManifest, map[string]string{"index.js": `
export function review() { return "r"; }
export function summarize() { return "s"; }`})
	if len(registry.PluginCommands()) != 2 {
		t.Fatalf("plugin commands = %d", len(registry.PluginCommands()))
	}
	fixture.manager.Unload("reviewer")
	if len(registry.PluginCommands()) != 0 {
		t.Fatalf("commands survived unload: %v", registry.PluginCommands())
	}
	// Built-ins must be untouched.
	if _, ok := registry.Find("help"); !ok {
		t.Fatal("a built-in command was removed by an unload")
	}
}

func TestReloadDoesNotDuplicateCommands(t *testing.T) {
	fixture, registry := commandFixture(t, "reviewer", reviewManifest, map[string]string{"index.js": `
export function review() { return "r"; }
export function summarize() { return "s"; }`})
	for i := range 10 {
		fixture.write(t, pluginFixture{name: "reviewer", manifest: reviewManifest,
			files: map[string]string{"index.js": `
export function review() { return "r"; }
export function summarize() { return "s"; }`}})
		if err := fixture.manager.Reload(context.Background(), "reviewer"); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	if got := len(registry.PluginCommands()); got != 2 {
		t.Fatalf("plugin commands = %d after ten reloads, want 2", got)
	}
}

func TestCommandHandlerFailureIsContained(t *testing.T) {
	fixture, _ := commandFixture(t, "boom", `{
      "apiVersion": "bruce.plugin/v1", "name": "boom", "version": "1.0.0",
      "description": "Throws", "entry": "index.js",
      "commands": [{"name": "explode", "description": "Throws", "handler": "boom"}]
    }`, map[string]string{"index.js": `export function boom() { throw new Error("command exploded"); }`})

	result, ok := fixture.manager.RunCommand(context.Background(), "explode", nil, "/explode")
	if !ok {
		t.Fatal("the command was not dispatched")
	}
	if result.Err == nil {
		t.Fatal("a throwing command must report an error")
	}
	for _, want := range []string{"boom", "command exploded"} {
		if !strings.Contains(result.Err.Error(), want) {
			t.Errorf("error = %q, missing %q", result.Err, want)
		}
	}
}

func TestCommandExitIsHonored(t *testing.T) {
	fixture, _ := commandFixture(t, "quitter", `{
      "apiVersion": "bruce.plugin/v1", "name": "quitter", "version": "1.0.0",
      "description": "Exits", "entry": "index.js",
      "commands": [{"name": "quit-now", "description": "Exit", "handler": "quit"}]
    }`, map[string]string{"index.js": `export function quit() { return { output: "bye", exit: true }; }`})

	result, ok := fixture.manager.RunCommand(context.Background(), "quit-now", nil, "/quit-now")
	if !ok {
		t.Fatal("the command was not dispatched")
	}
	if !result.Exit || result.Output != "bye" {
		t.Fatalf("result = %+v", result)
	}
}

func TestCommandRegistryRejectsEmptyAndDuplicateNames(t *testing.T) {
	registry := cli.NewRegistry()
	if err := registry.RegisterPlugin(cli.CommandInfo{Name: "  "}); err == nil {
		t.Error("an empty command name must be rejected")
	}
	if err := registry.RegisterPlugin(cli.CommandInfo{Name: "ok", Plugin: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterPlugin(cli.CommandInfo{Name: "OK", Plugin: "demo"}); err == nil {
		t.Error("a duplicate command name must be rejected regardless of case")
	}
	// A leading slash is accepted and normalized, so a manifest may write
	// either form.
	if err := registry.RegisterPlugin(cli.CommandInfo{Name: "/with-slash", Plugin: "demo"}); err != nil {
		t.Fatalf("a leading slash must be normalized, got %v", err)
	}
	if _, ok := registry.Find("with-slash"); !ok {
		t.Error("the normalized name is not resolvable")
	}
}

func TestWithCommandRegistryPublishesAlreadyLoadedPlugins(t *testing.T) {
	// Load first, attach the registry afterwards: the commands must still
	// appear, otherwise the host's construction order would decide behaviour.
	fixture := newManagerFixture(t, permissivePolicy())
	fixture.write(t, pluginFixture{name: "reviewer", manifest: reviewManifest,
		files: map[string]string{"index.js": `
export function review() { return "r"; }
export function summarize() { return "s"; }`}})
	if err := fixture.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry := cli.NewRegistry()
	fixture.manager.WithCommandRegistry(registry)
	if len(registry.PluginCommands()) != 2 {
		t.Fatalf("plugin commands = %d, want 2", len(registry.PluginCommands()))
	}
}
