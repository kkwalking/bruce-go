package cli

import (
	"sort"
	"strings"
)

// Registry is the slash command registry.
//
// Built-in and plugin commands share one abstraction, so the CLI has a single
// dispatch path instead of a switch statement that would need a case per
// plugin. Built-in commands keep their existing handling in the runtime: the
// registry decides *what* a command is and who owns it, the runtime decides
// *how* a built-in runs.
type Registry struct {
	commands  map[string]CommandInfo
	order     []string
	conflicts []string
}

// NewRegistry creates a registry preloaded with the built-in commands.
func NewRegistry() *Registry {
	r := &Registry{commands: map[string]CommandInfo{}}
	r.RegisterBuiltins()
	return r
}

// RegisterBuiltins loads the built-in command table.
func (r *Registry) RegisterBuiltins() {
	for _, command := range Commands {
		r.register(command, true)
	}
}

// Register adds one command. A duplicate name is refused and recorded, so a
// plugin cannot silently replace a command the user already relies on.
//
// A name owned by a built-in command is reported as reserved rather than as a
// plain duplicate: that is the rule a plugin author needs to understand, and it
// is what stops a plugin from taking over a security-relevant command such as
// /sandbox or /hitl.
func (r *Registry) Register(command CommandInfo) error {
	if r.register(command, false) {
		return nil
	}
	name := normalizeName(command.Name)
	if existing, ok := r.commands[name]; ok {
		if existing.Builtin {
			return &ConflictError{
				Name: name, Existing: describe(existing), Incoming: describe(command),
				Reason: "the name is reserved by a built-in command and cannot be overridden",
			}
		}
		return &ConflictError{Name: name, Existing: describe(existing), Incoming: describe(command)}
	}
	return &ConflictError{Name: name, Incoming: describe(command), Reason: "the command name is empty"}
}

// ConflictError reports a rejected command registration.
type ConflictError struct {
	Name     string
	Existing string
	Incoming string
	Reason   string
}

func (e *ConflictError) Error() string {
	if e.Reason != "" {
		return "command " + e.Name + " was not registered: " + e.Reason
	}
	return "command " + e.Name + " is already provided by " + e.Existing + " and cannot be replaced by " + e.Incoming
}

// register performs the insertion. It reports whether the command was added.
func (r *Registry) register(command CommandInfo, builtin bool) bool {
	name := normalizeName(command.Name)
	if name == "" {
		return false
	}
	if _, exists := r.commands[name]; exists {
		return false
	}
	command.Name = name
	if command.Source == "" {
		if builtin {
			command.Source = "builtin"
		} else {
			command.Source = "plugin"
		}
	}
	if builtin {
		command.Builtin = true
	}
	r.commands[name] = command
	r.order = append(r.order, name)
	return true
}

// Unregister removes every command contributed by a plugin.
func (r *Registry) Unregister(plugin string) {
	if strings.TrimSpace(plugin) == "" {
		return
	}
	kept := r.order[:0]
	for _, name := range r.order {
		if r.commands[name].Plugin == plugin {
			delete(r.commands, name)
			continue
		}
		kept = append(kept, name)
	}
	r.order = kept
}

// UnregisterAll removes every non-built-in command.
func (r *Registry) UnregisterAll() {
	kept := r.order[:0]
	for _, name := range r.order {
		if r.commands[name].Builtin {
			kept = append(kept, name)
			continue
		}
		delete(r.commands, name)
	}
	r.order = kept
}

// Find resolves a command token case-insensitively.
func (r *Registry) Find(name string) (CommandInfo, bool) {
	command, ok := r.commands[normalizeName(name)]
	return command, ok
}

// All returns every registered command, sorted by name.
func (r *Registry) All() []CommandInfo {
	out := make([]CommandInfo, 0, len(r.commands))
	for _, command := range r.commands {
		out = append(out, command)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PluginCommands returns the commands contributed by plugins, sorted.
func (r *Registry) PluginCommands() []CommandInfo {
	out := make([]CommandInfo, 0)
	for _, command := range r.All() {
		if command.Plugin != "" {
			out = append(out, command)
		}
	}
	return out
}

// Conflicts returns the rejected registrations, sorted.
func (r *Registry) Conflicts() []string {
	out := append([]string(nil), r.conflicts...)
	sort.Strings(out)
	return out
}

func (r *Registry) recordConflict(err error) {
	if err == nil {
		return
	}
	r.conflicts = append(r.conflicts, err.Error())
}

// RegisterPlugin adds a plugin command, recording a conflict instead of
// replacing an existing command.
func (r *Registry) RegisterPlugin(command CommandInfo) error {
	err := r.Register(command)
	r.recordConflict(err)
	return err
}

func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "/")))
}

func describe(command CommandInfo) string {
	if command.Plugin != "" {
		return "plugin " + command.Plugin
	}
	if command.Source != "" {
		return command.Source
	}
	return "another source"
}

// Help renders the help text from a registry, so plugin commands are listed
// alongside built-ins.
func (r *Registry) Help() string {
	var b strings.Builder
	b.WriteString("Available Bruce Go commands:\n\n")
	for _, command := range r.All() {
		usage := command.Usage
		if usage == "" {
			usage = "/" + command.Name
		}
		b.WriteString(usage)
		if len(usage) < 28 {
			b.WriteString(strings.Repeat(" ", 28-len(usage)))
		} else {
			b.WriteString("  ")
		}
		b.WriteString(command.Description)
		if command.Plugin != "" {
			b.WriteString("  [plugin: " + command.Plugin + "]")
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nInput syntax:\n")
	b.WriteString("$<skill> <task>                 Explicitly load up to three Skills\n")
	b.WriteString("@image:<path>                   Attach an image file\n")
	b.WriteString("@image:<file:///path with space> Attach a file:// image\n")
	b.WriteString("@clipboard                      Attach an image from the macOS clipboard\n")
	return strings.TrimSpace(b.String())
}
