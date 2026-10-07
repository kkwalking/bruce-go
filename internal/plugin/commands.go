package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"bruce-go/internal/cli"
)

// CommandResult is what a slash command returns.
type CommandResult struct {
	Output string
	Err    error
	// Exit asks the host to end the session.
	Exit bool
}

// CommandHandler runs one command.
type CommandHandler func(ctx context.Context, args []string, raw string) CommandResult

// commandExecutor is the host's slash-command dispatch. A plugin command is
// published into Bruce's one command registry (internal/cli) rather than into a
// parallel one, so built-in and plugin commands share the same abstraction and
// the same conflict policy.
type commandExecutor interface {
	RegisterPlugin(cli.CommandInfo) error
}

// PublishCommands registers every command a loaded plugin declares into the
// host's command registry.
//
// A conflict is reported and skipped rather than replacing the existing
// command, so a plugin can neither take over a built-in command nor another
// plugin's.
func (m *Manager) PublishCommands(registry *cli.Registry, pluginName string, manifest *Manifest) []string {
	if registry == nil {
		return nil
	}
	var conflicts []string
	for _, declaration := range manifest.Commands {
		name := declaration.Name
		handlerPath := declaration.Handler
		timeout := m.timeoutFor(declaration.TimeoutMS)
		command := cli.CommandInfo{
			Name:        name,
			Usage:       commandUsage(declaration),
			Description: declaration.Description,
			Complete:    "/" + name + " ",
			Source:      "plugin",
			Plugin:      pluginName,
		}
		// The handler is stored alongside the declaration so the runtime can
		// dispatch it without a second lookup table.
		m.registerCommandHandler(pluginName, name, func(ctx context.Context, args []string, raw string) CommandResult {
			return m.runCommand(ctx, pluginName, name, handlerPath, timeout, args, raw)
		})
		if err := registry.RegisterPlugin(command); err != nil {
			conflicts = append(conflicts, Redact(err.Error()))
			m.emit(EventLoadFailed, map[string]any{
				"plugin": pluginName, "command": name, "error": Redact(err.Error()),
			})
		}
	}
	return conflicts
}

// registerCommandHandler records the Go side of a plugin command.
func (m *Manager) registerCommandHandler(pluginName, commandName string, handler CommandHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commandHandlers[commandKey(pluginName, commandName)] = handler
}

func commandKey(pluginName, commandName string) string {
	return pluginName + "/" + commandName
}

// RunCommand dispatches a plugin command.
//
// It returns false when the command is not a plugin command, so the host can
// fall through to its built-in handling.
func (m *Manager) RunCommand(ctx context.Context, name string, args []string, raw string) (CommandResult, bool) {
	normalized := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "/")))
	m.mu.RLock()
	var handler CommandHandler
	for key, candidate := range m.commandHandlers {
		if strings.HasSuffix(key, "/"+normalized) {
			handler = candidate
			break
		}
	}
	m.mu.RUnlock()
	if handler == nil {
		return CommandResult{}, false
	}
	return handler(ctx, args, raw), true
}

// runCommand invokes a plugin command handler.
func (m *Manager) runCommand(ctx context.Context, pluginName, commandName, handlerPath string, timeout time.Duration, args []string, raw string) CommandResult {
	plugin, ok := m.plugin(pluginName)
	if !ok {
		return CommandResult{Err: NewError(pluginName, "", handlerPath, StageInvoke, CategoryInternal,
			errors.New("plugin is not loaded"))}
	}
	handler, ok := plugin.handlers[handlerPath]
	if !ok {
		return CommandResult{Err: NewError(pluginName, plugin.manifest.File, handlerPath, StageInvoke, CategoryMissingHandler,
			errors.New("handler was not resolved at load time"))}
	}
	payload, err := json.Marshal(map[string]any{
		"command": commandName,
		"args":    args,
		"raw":     raw,
		"joined":  strings.Join(args, " "),
	})
	if err != nil {
		return CommandResult{Err: NewError(pluginName, plugin.manifest.File, handlerPath, StageInvoke, CategoryMalformedResult, err)}
	}
	callCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	out, err := m.invoke(callCtx, plugin, handlerPath, handler, timeout, payload)
	if err != nil {
		return CommandResult{Err: err}
	}
	var result struct {
		Output string `json:"output"`
		Text   string `json:"text"`
		Exit   bool   `json:"exit"`
	}
	if len(out) > 0 {
		if err := json.Unmarshal(out, &result); err != nil {
			// A handler may return a plain string, which is the simplest
			// useful shape for a command.
			var text string
			if stringErr := json.Unmarshal(out, &text); stringErr == nil {
				return CommandResult{Output: text}
			}
			return CommandResult{Err: NewError(pluginName, plugin.manifest.File, handlerPath, StageInvoke,
				CategoryMalformedResult, errors.New("command handler returned a value that is neither a string nor {output}"))}
		}
	}
	output := result.Output
	if output == "" {
		output = result.Text
	}
	return CommandResult{Output: output, Exit: result.Exit}
}

func commandUsage(declaration CommandDeclaration) string {
	if strings.TrimSpace(declaration.Usage) != "" {
		return declaration.Usage
	}
	return "/" + declaration.Name
}
