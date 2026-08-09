package approval

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/mattn/go-runewidth"
)

type Decision string

const (
	Approved    Decision = "approved"
	ApprovedAll Decision = "approved_all"
	Modified    Decision = "modified"
	Rejected    Decision = "rejected"
	Skipped     Decision = "skipped"
)

type Result struct {
	Decision  Decision
	Reason    string
	Arguments string
}

func Approve() Result    { return Result{Decision: Approved} }
func ApproveAll() Result { return Result{Decision: ApprovedAll} }
func Reject(reason string) Result {
	return Result{Decision: Rejected, Reason: reason}
}
func Skip() Result { return Result{Decision: Skipped} }

func (r Result) IsRejected() bool { return r.Decision == Rejected }
func (r Result) IsSkipped() bool  { return r.Decision == Skipped }

func (r Result) EffectiveArguments(original string) string {
	if r.Decision == Modified && strings.TrimSpace(r.Arguments) != "" {
		return r.Arguments
	}
	return original
}

type Handler interface {
	Enabled() bool
	SetEnabled(bool)
	Request(context.Context, Request) (Result, error)
	ClearApprovedAll()
}

type AutoHandler struct {
	mu      sync.RWMutex
	enabled bool
	result  Result
}

func NewAutoHandler(enabled bool, result Result) *AutoHandler {
	if result.Decision == "" {
		result = Approve()
	}
	return &AutoHandler{enabled: enabled, result: result}
}

func (h *AutoHandler) Enabled() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.enabled
}

func (h *AutoHandler) SetEnabled(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enabled = v
}

func (h *AutoHandler) ClearApprovedAll() {}
func (h *AutoHandler) Request(ctx context.Context, _ Request) (Result, error) {
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	default:
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.result, nil
}

type Request struct {
	ToolName        string
	Arguments       string
	DangerLevel     string
	RiskDescription string
	Suggestion      string
}

func NewRequest(toolName, args, suggestion string) Request {
	return Request{
		ToolName:        toolName,
		Arguments:       args,
		DangerLevel:     DangerLevel(toolName),
		RiskDescription: RiskDescription(toolName),
		Suggestion:      suggestion,
	}
}

func RequiresApproval(toolName string) bool {
	switch toolName {
	case "edit_file", "write_file", "execute_command", "create_project":
		return true
	default:
		return strings.HasPrefix(toolName, "mcp_") || strings.HasPrefix(toolName, "mcp__")
	}
}

func DangerLevel(toolName string) string {
	if strings.HasPrefix(toolName, "mcp_") || strings.HasPrefix(toolName, "mcp__") {
		return "medium risk"
	}
	switch toolName {
	case "execute_command":
		return "high risk"
	case "edit_file", "write_file", "create_project":
		return "medium risk"
	default:
		return "safe"
	}
}

func RiskDescription(toolName string) string {
	if strings.HasPrefix(toolName, "mcp_") || strings.HasPrefix(toolName, "mcp__") {
		return "Calls a third-party MCP tool that may access local or remote resources"
	}
	switch toolName {
	case "execute_command":
		return "Executes a shell command that may modify files, install software, or affect system state"
	case "edit_file":
		return "Modifies matching text in a file"
	case "write_file":
		return "Writes or overwrites file content"
	case "create_project":
		return "Creates new directories and files on disk"
	default:
		return "Safe read-only operation"
	}
}

func (r Request) DisplayText() string {
	args := formatArgs(r.Arguments)
	lines := []string{
		"Approval required",
		"Tool: " + r.ToolName,
		"Level: " + r.DangerLevel,
		"Risk: " + r.RiskDescription,
		"Arguments:",
	}
	lines = append(lines, args...)
	width := 58
	var out strings.Builder
	out.WriteString("+" + strings.Repeat("-", width) + "+\n")
	for _, line := range lines {
		for _, wrapped := range wrap(line, width) {
			out.WriteString("|" + padRight(wrapped, width) + "|\n")
		}
	}
	out.WriteString("+" + strings.Repeat("-", width) + "+")
	return out.String()
}

func formatArgs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{"  (empty)"}
	}
	var data map[string]any
	if json.Unmarshal([]byte(raw), &data) == nil {
		lines := make([]string, 0, len(data))
		for k, v := range data {
			b, _ := json.Marshal(v)
			lines = append(lines, "  "+k+": "+string(b))
		}
		return lines
	}
	return []string{"  " + raw}
}

func wrap(text string, width int) []string {
	if runewidth.StringWidth(text) <= width {
		return []string{text}
	}
	var lines []string
	var cur strings.Builder
	curWidth := 0
	for _, r := range text {
		w := runewidth.RuneWidth(r)
		if curWidth+w > width && cur.Len() > 0 {
			lines = append(lines, cur.String())
			cur.Reset()
			curWidth = 0
		}
		cur.WriteRune(r)
		curWidth += w
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}

func padRight(text string, width int) string {
	pad := width - runewidth.StringWidth(text)
	if pad <= 0 {
		return text
	}
	return text + strings.Repeat(" ", pad)
}
