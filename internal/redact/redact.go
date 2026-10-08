// Package redact removes credential-shaped substrings from text that is about
// to be shown to a person.
//
// It exists as its own package because both llm and plugin need it and their
// dependency runs one way (plugin imports llm), so neither can host a shared
// implementation. The text it handles is upstream error output: a gateway that
// rejects a key commonly quotes it back, and that body is rendered in the TUI
// and written to the transcript.
package redact

import (
	"regexp"
	"strings"
)

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(sk|pk|rk)-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)\b(api[_-]?key|apikey|access[_-]?token|auth[_-]?token|token|secret|password|passwd|credential)s?\b"?\s*[:=]\s*"?[^\s",;]{6,}"?`),
	regexp.MustCompile(`(?i)\b[A-Za-z0-9_]*(API_?KEY|TOKEN|SECRET|PASSWORD)[A-Za-z0-9_]*\s*[:=]\s*"?[^\s",;]{6,}"?`),
	regexp.MustCompile(`(?i)-----BEGIN[^-]*PRIVATE KEY-----[\s\S]*?-----END[^-]*PRIVATE KEY-----`),
}

// Text replaces credential-shaped substrings with "[redacted]".
//
// The key name is kept when the match has one, so a diagnostic still says what
// was hidden ("invalid api key: [redacted]" rather than a bare marker).
func Text(message string) string {
	if message == "" {
		return message
	}
	out := message
	for _, pattern := range patterns {
		out = pattern.ReplaceAllStringFunc(out, func(match string) string {
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
