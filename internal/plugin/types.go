package plugin

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// APIVersion is the manifest apiVersion this build accepts.
const APIVersion = "bruce.plugin/v1"

// Permission is a capability a plugin may request.
//
// A manifest declaration is a request, never a grant: the host policy decides
// what a plugin actually receives, and a plugin can never grant itself a
// permission.
type Permission string

const (
	PermissionFilesystemRead  Permission = "fs.read"
	PermissionFilesystemWrite Permission = "fs.write"
	PermissionNetwork         Permission = "net"
	PermissionShell           Permission = "shell"
	PermissionStorage         Permission = "storage"
	PermissionEvents          Permission = "events"
)

// Permissions lists every permission in a stable order.
func Permissions() []Permission {
	return []Permission{
		PermissionFilesystemRead,
		PermissionFilesystemWrite,
		PermissionNetwork,
		PermissionShell,
		PermissionStorage,
		PermissionEvents,
	}
}

// ParsePermission validates one permission token.
func ParsePermission(raw string) (Permission, error) {
	trimmed := strings.TrimSpace(raw)
	for _, candidate := range Permissions() {
		if string(candidate) == trimmed {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("unknown permission %q", raw)
}

// Source records which search root produced a plugin.
type Source string

const (
	SourceWorkspace Source = "workspace"
	SourceUser      Source = "user"
)

// Stage names the phase in which a plugin failed. It is part of every
// diagnostic because "the plugin broke" is not actionable while "the plugin
// failed to link" is.
type Stage string

const (
	StageDiscover Stage = "discover"
	StageManifest Stage = "manifest"
	StageCompile  Stage = "compile"
	StageLoad     Stage = "load"
	StageInvoke   Stage = "invoke"
	StageReload   Stage = "reload"
	StageUnload   Stage = "unload"
)

// ErrorCategory classifies a plugin failure so that callers can react without
// parsing message text.
type ErrorCategory string

const (
	CategoryManifest        ErrorCategory = "manifest"
	CategorySyntax          ErrorCategory = "syntax"
	CategoryLink            ErrorCategory = "link"
	CategoryMissingHandler  ErrorCategory = "missing_handler"
	CategoryException       ErrorCategory = "exception"
	CategoryMalformedResult ErrorCategory = "malformed_result"
	CategoryTimeout         ErrorCategory = "timeout"
	CategoryCancellation    ErrorCategory = "cancellation"
	CategoryPanic           ErrorCategory = "panic"
	CategoryPermission      ErrorCategory = "permission"
	CategoryStorage         ErrorCategory = "storage"
	CategoryConflict        ErrorCategory = "conflict"
	CategoryInternal        ErrorCategory = "internal"
)

// ManifestError is a validation failure of a plugin manifest.
type ManifestError struct {
	Plugin  string
	Path    string
	Field   string
	Message string
}

func (e *ManifestError) Error() string {
	plugin := strings.TrimSpace(e.Plugin)
	if plugin == "" {
		plugin = "<unknown>"
	}
	field := strings.TrimSpace(e.Field)
	if field == "" {
		return fmt.Sprintf("plugin %q (%s): %s", plugin, e.Path, Redact(e.Message))
	}
	return fmt.Sprintf("plugin %q (%s): field %s: %s", plugin, e.Path, field, Redact(e.Message))
}

// PluginError is the single error type that crosses the plugin boundary.
//
// Every failure carries the plugin identity, the plugin path, the handler that
// failed and the stage it failed in, so a diagnostic is actionable without
// guessing which plugin produced it. Message text is redacted: a plugin
// failure must never leak an API key or another credential into a log.
type PluginError struct {
	Plugin   string
	Path     string
	Handler  string
	Stage    Stage
	Category ErrorCategory
	Err      error
}

func (e *PluginError) Error() string {
	var b strings.Builder
	b.WriteString("plugin ")
	if strings.TrimSpace(e.Plugin) == "" {
		b.WriteString("<unknown>")
	} else {
		b.WriteString(fmt.Sprintf("%q", e.Plugin))
	}
	if e.Path != "" {
		b.WriteString(" (" + e.Path + ")")
	}
	b.WriteString(": stage=" + string(e.Stage))
	if e.Category != "" {
		b.WriteString(" category=" + string(e.Category))
	}
	if e.Handler != "" {
		b.WriteString(" handler=" + e.Handler)
	}
	if e.Err != nil {
		b.WriteString(": " + Redact(e.Err.Error()))
	}
	return b.String()
}

func (e *PluginError) Unwrap() error { return e.Err }

// NewError builds a PluginError.
func NewError(pluginName, path, handler string, stage Stage, category ErrorCategory, err error) *PluginError {
	return &PluginError{Plugin: pluginName, Path: path, Handler: handler, Stage: stage, Category: category, Err: err}
}

// IsPluginError reports whether err is (or wraps) a *PluginError.
func IsPluginError(err error) bool {
	var target *PluginError
	return errors.As(err, &target)
}

// CategoryOf returns the category of the first *PluginError in err's chain.
func CategoryOf(err error) (ErrorCategory, bool) {
	var target *PluginError
	if errors.As(err, &target) {
		return target.Category, true
	}
	return "", false
}

// IsCancellation reports whether err records a cancellation or timeout, so
// callers can map it onto Bruce's existing interruption statuses instead of
// reporting a generic failure.
func IsCancellation(err error) bool {
	category, ok := CategoryOf(err)
	if !ok {
		return false
	}
	return category == CategoryCancellation || category == CategoryTimeout
}

var redactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(sk|pk|rk)-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)\b(api[_-]?key|apikey|access[_-]?token|auth[_-]?token|token|secret|password|passwd|credential)s?\b"?\s*[:=]\s*"?[^\s",;]{6,}"?`),
	regexp.MustCompile(`(?i)\b[A-Za-z0-9_]*(API_?KEY|TOKEN|SECRET|PASSWORD)[A-Za-z0-9_]*\s*[:=]\s*"?[^\s",;]{6,}"?`),
	regexp.MustCompile(`(?i)-----BEGIN[^-]*PRIVATE KEY-----[\s\S]*?-----END[^-]*PRIVATE KEY-----`),
}

// Redact removes credential-shaped substrings from a message.
//
// Plugin errors are surfaced to the user, written to logs and shown in the TUI.
// A JavaScript exception that quotes an environment value, a URL with a token,
// or a header dump must not become a credential leak.
func Redact(message string) string {
	if message == "" {
		return message
	}
	out := message
	for _, pattern := range redactPatterns {
		out = pattern.ReplaceAllStringFunc(out, func(match string) string {
			// Keep the key name when the match has one, so the diagnostic
			// still says what was hidden.
			if idx := strings.IndexAny(match, ":="); idx > 0 {
				return match[:idx+1] + "[redacted]"
			}
			if fields := strings.Fields(match); len(fields) > 1 {
				return fields[0] + " [redacted]"
			}
			return "[redacted]"
		})
	}
	return out
}

// builtinToolNames are the tools Bruce itself registers. A plugin tool may not
// shadow one of them: doing so would let a plugin silently take over a
// security-relevant built-in.
var builtinToolNames = []string{
	"edit_file", "edit_plan", "execute_command", "load_skill",
	"read_file", "read_plan", "read_skill_resource", "replace_plan",
	"web_fetch", "web_search", "write_file",
}

// builtinCommandNames are the slash commands Bruce itself owns. A plugin
// command may not shadow one of them.
var builtinCommandNames = []string{
	"checkpoint", "clear", "compact", "exit", "help", "hitl", "mcp",
	"minimal", "model", "new", "parallel", "plan", "plugin", "react",
	"resume", "sandbox", "session", "sessions", "skill", "status", "tree",
	"web",
}

// BuiltinToolNames returns the reserved built-in tool names, sorted.
func BuiltinToolNames() []string {
	return append([]string(nil), builtinToolNames...)
}

// BuiltinCommandNames returns the reserved built-in command names, sorted.
func BuiltinCommandNames() []string {
	return append([]string(nil), builtinCommandNames...)
}

// IsBuiltinToolName reports whether name is reserved by a built-in tool.
func IsBuiltinToolName(name string) bool {
	return sortedContains(builtinToolNames, name)
}

// IsBuiltinCommandName reports whether name is reserved by a built-in command.
func IsBuiltinCommandName(name string) bool {
	return sortedContains(builtinCommandNames, name)
}

func sortedContains(values []string, name string) bool {
	index := sort.SearchStrings(values, name)
	return index < len(values) && values[index] == name
}
