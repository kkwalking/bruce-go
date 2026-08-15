package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bruce-go/internal/approval"
	"bruce-go/internal/llm"
	"bruce-go/internal/runtime"
	"bruce-go/internal/sandbox"
)

type Executor func(ctx context.Context, args map[string]string) (string, error)

type ToolCallStatus string

const (
	ToolCallSuccess     ToolCallStatus = "success"
	ToolCallFailed      ToolCallStatus = "failed"
	ToolCallTimeout     ToolCallStatus = "timeout"
	ToolCallInterrupted ToolCallStatus = "interrupted"
	ToolCallRejected    ToolCallStatus = "rejected"
	ToolCallSkipped     ToolCallStatus = "skipped"
)

type ExecutionOutcome struct {
	Output string
	Status ToolCallStatus
}

type Source string

const (
	SourceBuiltin Source = "builtin"
	SourceWeb     Source = "web"
	SourceSkill   Source = "skill"
	SourcePlan    Source = "plan"
	SourceMCP     Source = "mcp"
	SourceUnknown Source = "unknown"
)

type Policy struct {
	Source          Source
	MinimumMode     sandbox.Mode
	RequiresNetwork bool
	ParallelSafe    bool
}

type Tool struct {
	Name             string
	Description      string
	Parameters       json.RawMessage
	Exec             Executor
	PromptSnippet    string
	PromptGuidelines []string
	Policy           Policy
}

type Registry struct {
	mu            sync.RWMutex
	workspaceRoot string
	tools         map[string]Tool
	hitl          approval.Handler
	commandGuard  CommandGuard
	config        runtime.ConcurrencyConfig
	sandbox       *sandbox.Manager
}

func NewRegistry(workspaceRoot string) *Registry {
	root, _ := filepath.Abs(workspaceRoot)
	r := &Registry{
		workspaceRoot: filepath.Clean(root),
		tools:         map[string]Tool{},
		commandGuard:  CommandGuard{},
		config:        runtime.DefaultConcurrency(),
	}
	r.RegisterBuiltins()
	return r
}

func EmptyRegistry(workspaceRoot string) *Registry {
	root, _ := filepath.Abs(workspaceRoot)
	return &Registry{
		workspaceRoot: filepath.Clean(root),
		tools:         map[string]Tool{},
		commandGuard:  CommandGuard{},
		config:        runtime.DefaultConcurrency(),
	}
}

func (r *Registry) WithHITL(handler approval.Handler) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hitl = handler
	return r
}

func (r *Registry) WithConcurrency(config runtime.ConcurrencyConfig) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.config = config.Normalize()
	return r
}

func (r *Registry) WithSandbox(manager *sandbox.Manager) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sandbox = manager
	return r
}

func (r *Registry) WorkspaceRoot() string { return r.workspaceRoot }

func (r *Registry) Register(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name] = tool
}

func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

func (r *Registry) ToolNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	return names
}

func (r *Registry) Lookup(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Subset returns a registry containing only the named tools. The copy shares
// the parent's workspace root, HITL handler, sandbox manager, and concurrency
// configuration so restricted agents keep the same execution policies.
func (r *Registry) Subset(names ...string) *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	subset := &Registry{
		workspaceRoot: r.workspaceRoot,
		tools:         map[string]Tool{},
		hitl:          r.hitl,
		commandGuard:  r.commandGuard,
		config:        r.config,
		sandbox:       r.sandbox,
	}
	for _, name := range names {
		if candidate, ok := r.tools[name]; ok {
			subset.tools[name] = candidate
		}
	}
	return subset
}

func (r *Registry) concurrencyConfig() runtime.ConcurrencyConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.config.Normalize()
}

func (r *Registry) approvalHandler() approval.Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hitl
}

func (r *Registry) sandboxManager() *sandbox.Manager {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sandbox
}

func (r *Registry) Definitions() []llm.ToolDefinition {
	tools := r.availableTools(nil)
	defs := make([]llm.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, llm.ToolDefinition{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return defs
}

func (r *Registry) BuildPrompt() string {
	tools := r.availableTools(nil)
	var b strings.Builder
	b.WriteString("Available tools:\n")
	if len(tools) == 0 {
		b.WriteString("(none)\n")
	}
	for _, t := range tools {
		if strings.TrimSpace(t.PromptSnippet) != "" {
			b.WriteString("- " + t.Name + ": " + t.PromptSnippet + "\n")
		}
	}
	guidelines := defaultGuidelines(tools)
	if len(guidelines) > 0 {
		b.WriteString("\nGuidelines:\n")
		for _, guideline := range guidelines {
			b.WriteString("- " + guideline + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func (r *Registry) sortedToolsLocked() []Tool {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	tools := make([]Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, r.tools[name])
	}
	return tools
}

func (r *Registry) availableTools(modeOverride *sandbox.Mode) []Tool {
	r.mu.RLock()
	tools := append([]Tool(nil), r.sortedToolsLocked()...)
	r.mu.RUnlock()
	available := tools[:0]
	for _, candidate := range tools {
		if _, rejected := r.validateToolPolicy(candidate, modeOverride); rejected == "" {
			available = append(available, candidate)
		}
	}
	return available
}

func defaultGuidelines(tools []Tool) []string {
	seen := map[string]bool{}
	byName := make(map[string]Tool, len(tools))
	for _, candidate := range tools {
		byName[candidate.Name] = candidate
	}
	var guidelines []string
	add := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" || seen[text] {
			return
		}
		seen[text] = true
		guidelines = append(guidelines, text)
	}
	if _, ok := byName["execute_command"]; ok {
		add("For local directory exploration, file discovery, and full-text search, prefer execute_command with ls, rg --files, rg <pattern>, or find.")
		add("Use execute_command for builds, tests, Git operations, and scripts.")
	}
	if _, ok := byName["read_file"]; ok {
		add("Use read_file to read a single file at a known path. For large files, follow the returned guidance and continue reading with offset and limit.")
	}
	if _, ok := byName["edit_file"]; ok {
		add("Use edit_file for targeted changes to existing files; old_text must match exactly once.")
	}
	if _, ok := byName["write_file"]; ok {
		add("Use write_file to create a file or replace its entire contents; do not use it for targeted edits.")
	}
	for _, candidate := range tools {
		for _, guideline := range candidate.PromptGuidelines {
			add(guideline)
		}
	}
	for _, candidate := range tools {
		if strings.HasPrefix(candidate.Name, "mcp_") {
			add("Use mcp__* tools only when the user explicitly requests MCP, built-in tools cannot satisfy the request, or the task requires a capability specific to that MCP server.")
			break
		}
	}
	return guidelines
}

type preparedExecution struct {
	tool         Tool
	args         map[string]string
	modeOverride *sandbox.Mode
}

type executionStatusError struct {
	status ToolCallStatus
	cause  error
}

func (e *executionStatusError) Error() string { return e.cause.Error() }
func (e *executionStatusError) Unwrap() error { return e.cause }

func NewExecutionError(status ToolCallStatus, cause error) error {
	if cause == nil {
		cause = errors.New(string(status))
	}
	return &executionStatusError{status: status, cause: cause}
}

func (r *Registry) ExecuteJSON(ctx context.Context, name, argumentsJSON string) string {
	return r.executeJSONOutcome(ctx, name, argumentsJSON, nil).Output
}

func (r *Registry) Execute(ctx context.Context, name string, args map[string]string) string {
	return r.executeOutcome(ctx, name, args, nil).Output
}

func (r *Registry) ExecuteResult(ctx context.Context, name string, args map[string]string) ExecutionOutcome {
	return r.executeOutcome(ctx, name, args, nil)
}

func (r *Registry) ExecuteWithSandboxMode(ctx context.Context, name string, args map[string]string, mode sandbox.Mode) string {
	return r.executeOutcome(ctx, name, args, &mode).Output
}

func (r *Registry) ExecuteWithSandboxModeResult(ctx context.Context, name string, args map[string]string, mode sandbox.Mode) ExecutionOutcome {
	return r.executeOutcome(ctx, name, args, &mode)
}

func (r *Registry) executeJSONOutcome(ctx context.Context, name, argumentsJSON string, modeOverride *sandbox.Mode) ExecutionOutcome {
	args, err := ParseArguments(argumentsJSON)
	if err != nil {
		return ExecutionOutcome{Output: "Tool argument parsing failed: " + err.Error(), Status: ToolCallFailed}
	}
	return r.executeOutcome(ctx, name, args, modeOverride)
}

func (r *Registry) executeOutcome(ctx context.Context, name string, args map[string]string, modeOverride *sandbox.Mode) (outcome ExecutionOutcome) {
	defer func() {
		if recovered := recover(); recovered != nil {
			outcome = ExecutionOutcome{Output: fmt.Sprintf("Tool execution failed: panic: %v", recovered), Status: ToolCallFailed}
		}
	}()
	prepared, outcome, ok := r.prepare(ctx, name, args, modeOverride)
	if !ok {
		return outcome
	}
	return r.executePrepared(ctx, prepared)
}

func (r *Registry) prepare(ctx context.Context, name string, args map[string]string, modeOverride *sandbox.Mode) (preparedExecution, ExecutionOutcome, bool) {
	if err := ctx.Err(); err != nil {
		return preparedExecution{}, contextOutcome(name, err), false
	}
	r.mu.RLock()
	t, ok := r.tools[name]
	var names []string
	if !ok {
		names = make([]string, 0, len(r.tools))
		for toolName := range r.tools {
			names = append(names, toolName)
		}
	}
	r.mu.RUnlock()
	if !ok {
		sort.Strings(names)
		return preparedExecution{}, ExecutionOutcome{
			Output: "Unknown tool: " + name + ". Available tools: " + strings.Join(names, ", "),
			Status: ToolCallFailed,
		}, false
	}
	if _, rejected := r.validateToolRequest(t, args, modeOverride); rejected != "" {
		return preparedExecution{}, validationOutcome(rejected), false
	}
	hitl := r.approvalHandler()
	if hitl != nil && hitl.Enabled() && approval.RequiresApproval(name) {
		raw, _ := json.Marshal(args)
		result, err := hitl.Request(ctx, approval.NewRequest(name, string(raw), ""))
		if err != nil {
			return preparedExecution{}, contextOrFailureOutcome(name, "", err), false
		}
		if result.IsRejected() {
			if result.Reason == "" {
				result.Reason = "the user rejected this operation"
			}
			return preparedExecution{}, ExecutionOutcome{Output: "[HITL] Operation was rejected: " + result.Reason, Status: ToolCallRejected}, false
		}
		if result.IsSkipped() {
			return preparedExecution{}, ExecutionOutcome{Output: "[HITL] Operation was skipped", Status: ToolCallSkipped}, false
		}
		if result.Decision == approval.Modified {
			modified, err := ParseArguments(result.EffectiveArguments(string(raw)))
			if err != nil {
				return preparedExecution{}, ExecutionOutcome{Output: "Tool argument parsing failed: " + err.Error(), Status: ToolCallFailed}, false
			}
			args = modified
		} else if result.Decision != approval.Approved && result.Decision != approval.ApprovedAll {
			return preparedExecution{}, ExecutionOutcome{Output: "Tool execution failed: HITL returned an unknown decision", Status: ToolCallFailed}, false
		}
		// Approval may take arbitrarily long or modify arguments. Revalidate once
		// after it returns; execution performs a separate dynamic policy check.
		if _, rejected := r.validateToolRequest(t, args, modeOverride); rejected != "" {
			return preparedExecution{}, validationOutcome(rejected), false
		}
	}
	return preparedExecution{tool: t, args: args, modeOverride: modeOverride}, ExecutionOutcome{}, true
}

func (r *Registry) executePrepared(ctx context.Context, prepared preparedExecution) (outcome ExecutionOutcome) {
	name := prepared.tool.Name
	defer func() {
		if recovered := recover(); recovered != nil {
			outcome = ExecutionOutcome{Output: fmt.Sprintf("Tool execution failed: panic: %v", recovered), Status: ToolCallFailed}
		}
	}()
	if err := ctx.Err(); err != nil {
		return contextOutcome(name, err)
	}
	if _, rejected := r.validateToolRequest(prepared.tool, prepared.args, prepared.modeOverride); rejected != "" {
		return validationOutcome(rejected)
	}
	var out string
	var err error
	if name == "execute_command" && prepared.modeOverride != nil {
		out, err = r.executeCommandWithMode(ctx, prepared.args, prepared.modeOverride)
	} else {
		out, err = prepared.tool.Exec(ctx, prepared.args)
	}
	if contextErr := ctx.Err(); contextErr != nil {
		outcome := contextOutcome(name, contextErr)
		if out != "" {
			outcome.Output = out
		}
		return outcome
	}
	if err != nil {
		return contextOrFailureOutcome(name, out, err)
	}
	return ExecutionOutcome{Output: r.concurrencyConfig().Truncate(out), Status: ToolCallSuccess}
}

func validationOutcome(message string) ExecutionOutcome {
	status := ToolCallRejected
	if strings.HasPrefix(message, "Tool execution failed") || strings.HasPrefix(message, "Tool argument parsing failed") {
		status = ToolCallFailed
	}
	return ExecutionOutcome{Output: message, Status: status}
}

func contextOutcome(name string, err error) ExecutionOutcome {
	if errors.Is(err, context.DeadlineExceeded) {
		return ExecutionOutcome{Output: "Tool execution timed out and was canceled: " + name, Status: ToolCallTimeout}
	}
	return ExecutionOutcome{Output: "Tool batch execution was interrupted: " + name, Status: ToolCallInterrupted}
}

func contextOrFailureOutcome(name, output string, err error) ExecutionOutcome {
	var statusErr *executionStatusError
	if errors.As(err, &statusErr) {
		if output == "" {
			output = statusErr.Error()
		}
		return ExecutionOutcome{Output: output, Status: statusErr.status}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		outcome := contextOutcome(name, err)
		if output != "" {
			outcome.Output = output
		}
		return outcome
	}
	return ExecutionOutcome{Output: "Tool execution failed: " + err.Error(), Status: ToolCallFailed}
}

func (r *Registry) validateToolRequest(t Tool, args map[string]string, modeOverride *sandbox.Mode) (uint64, string) {
	generation, rejected := r.validateToolPolicy(t, modeOverride)
	if rejected != "" {
		return generation, rejected
	}
	name := t.Name
	if name == "execute_command" {
		if result := r.commandGuard.Check(args["command"]); !result.Allowed {
			return generation, "Command rejected by security policy: " + result.Reason
		}
		if manager := r.sandboxManager(); manager != nil {
			if err := manager.Preflight(modeOverride); err != nil {
				return generation, "Command rejected by sandbox: " + err.Error()
			}
		}
	}
	if name == "write_file" || name == "edit_file" {
		rel, err := r.writeTargetRelativePath(args["path"])
		if err != nil {
			if errors.Is(err, sandbox.ErrPolicy) {
				return generation, "Command rejected by sandbox: " + err.Error()
			}
			return generation, "Tool execution failed: " + err.Error()
		}
		if manager := r.sandboxManager(); manager != nil {
			if err := manager.CanWriteFile(rel); err != nil {
				return generation, "Command rejected by sandbox: " + err.Error()
			}
		}
	}
	return generation, ""
}

func (r *Registry) validateToolPolicy(t Tool, modeOverride *sandbox.Mode) (uint64, string) {
	manager := r.sandboxManager()
	if manager == nil {
		return 0, ""
	}
	status := manager.Status()
	mode := status.Mode
	network := status.NetworkAccess
	if modeOverride != nil {
		mode = *modeOverride
		network = mode == sandbox.ModeFullAccess || status.ConfiguredNetworkAccess
	}
	required := normalizedMinimumMode(t.Policy.MinimumMode)
	if !modeAllows(mode, required) {
		source := t.Policy.Source
		if source == "" {
			source = SourceUnknown
		}
		return status.Generation, fmt.Sprintf(
			"Command rejected by sandbox: %s mode does not permit tool %s (source=%s, required=%s)",
			mode, t.Name, source, required,
		)
	}
	if t.Policy.RequiresNetwork && !network {
		return status.Generation, fmt.Sprintf(
			"Command rejected by sandbox: sandbox network access is disabled, but tool %s requires network access",
			t.Name,
		)
	}
	return status.Generation, ""
}

func normalizedMinimumMode(mode sandbox.Mode) sandbox.Mode {
	switch mode {
	case sandbox.ModeReadOnly, sandbox.ModeWorkspaceWrite, sandbox.ModeFullAccess:
		return mode
	default:
		return sandbox.ModeFullAccess
	}
}

func modeAllows(current, required sandbox.Mode) bool {
	switch required {
	case sandbox.ModeReadOnly:
		return true
	case sandbox.ModeWorkspaceWrite:
		return current == sandbox.ModeWorkspaceWrite || current == sandbox.ModeFullAccess
	default:
		return current == sandbox.ModeFullAccess
	}
}

func (r *Registry) SandboxCanEnforce(mode sandbox.Mode) bool {
	manager := r.sandboxManager()
	return manager != nil && manager.Preflight(&mode) == nil
}

func ParseArguments(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return nil, err
	}
	args := map[string]string{}
	for k, v := range generic {
		switch value := v.(type) {
		case string:
			args[k] = value
		default:
			b, _ := json.Marshal(value)
			args[k] = string(b)
		}
	}
	return args, nil
}

func (r *Registry) RegisterBuiltins() {
	r.Register(Tool{
		Name:          "read_file",
		Description:   "Read file contents, with offset and limit for pagination by 1-based line number",
		Parameters:    params(param{"path", "string", "File path", true}, param{"offset", "integer", "Starting line number", false}, param{"limit", "integer", "Maximum number of lines to read", false}),
		Exec:          r.readFile,
		PromptSnippet: "Read known file contents with optional offset/limit",
		Policy:        Policy{Source: SourceBuiltin, MinimumMode: sandbox.ModeReadOnly, ParallelSafe: true},
	})
	r.Register(Tool{
		Name:          "write_file",
		Description:   "Create a file or replace its entire contents",
		Parameters:    params(param{"path", "string", "File path", true}, param{"content", "string", "File content", true}),
		Exec:          r.writeFile,
		PromptSnippet: "Create new files or completely overwrite existing files",
		Policy:        Policy{Source: SourceBuiltin, MinimumMode: sandbox.ModeWorkspaceWrite},
	})
	r.Register(Tool{
		Name:          "edit_file",
		Description:   "Precisely replace a text segment in an existing file; old_text must match exactly once",
		Parameters:    params(param{"path", "string", "File path", true}, param{"old_text", "string", "Exact text to replace", true}, param{"new_text", "string", "Replacement text", true}),
		Exec:          r.editFile,
		PromptSnippet: "Make precise small edits by replacing one unique exact text block",
		Policy:        Policy{Source: SourceBuiltin, MinimumMode: sandbox.ModeWorkspaceWrite},
	})
	r.Register(Tool{
		Name:          "execute_command",
		Description:   "Execute a shell command in the working directory for general local operations such as ls, rg, find, Git, builds, tests, and scripts",
		Parameters:    params(param{"command", "string", "Command to execute", true}),
		Exec:          r.executeCommand,
		PromptSnippet: "Execute shell commands for ls, rg, find, git, build, test, and scripts",
		Policy:        Policy{Source: SourceBuiltin, MinimumMode: sandbox.ModeReadOnly},
	})
}

func (r *Registry) readFile(ctx context.Context, args map[string]string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	rel, err := r.relativePath(args["path"])
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(r.workspaceRoot)
	if err != nil {
		return "", err
	}
	defer root.Close()
	data, err := root.ReadFile(rel)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	content := string(data)
	outputLimit := r.readFileOutputLimit()
	offset, err := optionalInt(args["offset"], "offset")
	if err != nil {
		return "", err
	}
	limit, err := optionalInt(args["limit"], "limit")
	if err != nil {
		return "", err
	}
	if offset != nil && *offset < 1 {
		return "offset must be at least 1", nil
	}
	if limit != nil && *limit < 1 {
		return "limit must be at least 1", nil
	}
	if offset == nil && limit == nil && len(content) <= outputLimit {
		return "File contents:\n" + content, nil
	}
	lines := splitLines(content)
	totalLines := len(lines)
	startLine := 1
	if offset != nil {
		startLine = *offset
	}
	if totalLines == 0 {
		if startLine > 1 {
			return fmt.Sprintf("offset is past the end of the file: offset=%d, total lines=0", startLine), nil
		}
		return "File contents (lines 0-0 of 0):\n", nil
	}
	if startLine > totalLines {
		return fmt.Sprintf("offset is past the end of the file: offset=%d, total lines=%d", startLine, totalLines), nil
	}

	startIndex := startLine - 1
	requestedEnd := totalLines
	if limit != nil && startIndex+*limit < requestedEnd {
		requestedEnd = startIndex + *limit
	}
	var selected strings.Builder
	endIndex := startIndex
	truncatedByChars := false
	partialLine := false
	for i := startIndex; i < requestedEnd; i++ {
		next := lines[i]
		if selected.Len() > 0 {
			next = "\n" + next
		}
		if selected.Len()+len(next) > outputLimit {
			if selected.Len() == 0 {
				selected.WriteString(next[:min(len(next), outputLimit)])
				endIndex = i + 1
				partialLine = true
			}
			truncatedByChars = true
			break
		}
		selected.WriteString(next)
		endIndex = i + 1
	}
	displayEndLine := max(startLine, endIndex)
	var result strings.Builder
	fmt.Fprintf(&result, "File contents (lines %d-%d of %d):\n%s", startLine, displayEndLine, totalLines, selected.String())
	if partialLine {
		fmt.Fprintf(&result, "\n\n[Line %d exceeds %d char limit. Use execute_command with sed/head/tail to inspect this long line.]", displayEndLine, outputLimit)
	}
	if endIndex < totalLines {
		fmt.Fprintf(&result, "\n\n[Showing lines %d-%d of %d", startLine, displayEndLine, totalLines)
		if truncatedByChars {
			fmt.Fprintf(&result, " (%d char limit)", outputLimit)
		}
		fmt.Fprintf(&result, ". Use offset=%d to continue.]", endIndex+1)
	}
	return result.String(), nil
}

func (r *Registry) writeFile(ctx context.Context, args map[string]string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	rel, err := r.writeTargetRelativePath(args["path"])
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(r.workspaceRoot)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return "", err
	}
	if err := root.WriteFile(rel, []byte(args["content"]), 0o644); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "File written: " + rel, nil
}

func (r *Registry) editFile(ctx context.Context, args map[string]string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	rel, err := r.writeTargetRelativePath(args["path"])
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(r.workspaceRoot)
	if err != nil {
		return "", err
	}
	defer root.Close()
	oldText := args["old_text"]
	if oldText == "" {
		return "", errors.New("edit_file failed: old_text must not be empty; the file was not modified")
	}
	data, err := root.ReadFile(rel)
	if err != nil {
		return "", err
	}
	content := string(data)
	count := strings.Count(content, oldText)
	if count == 0 {
		return "", errors.New("edit_file failed: old_text was not found; the file was not modified: " + rel)
	}
	if count > 1 {
		return "", fmt.Errorf("edit_file failed: old_text matched more than once (%d matches); provide more specific old_text; the file was not modified: %s", count, rel)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	updated := strings.Replace(content, oldText, args["new_text"], 1)
	if err := root.WriteFile(rel, []byte(updated), 0o644); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return fmt.Sprintf("File edited: %s (1 replacement, %d -> %d characters)", rel, len(oldText), len(args["new_text"])), nil
}

func (r *Registry) executeCommand(ctx context.Context, args map[string]string) (string, error) {
	return r.executeCommandWithMode(ctx, args, nil)
}

func (r *Registry) executeCommandWithMode(ctx context.Context, args map[string]string, modeOverride *sandbox.Mode) (string, error) {
	command := strings.TrimSpace(args["command"])
	if command == "" {
		return "", errors.New("command must not be empty")
	}
	config := r.concurrencyConfig()
	manager := r.sandboxManager()
	if manager != nil {
		result, err := manager.Run(ctx, command, config.CommandTimeout, config.MaxOutputChars, modeOverride)
		if err != nil {
			return "", err
		}
		if result.TimedOut {
			return "Command timed out and was terminated:\n" + result.Output, &executionStatusError{status: ToolCallTimeout, cause: context.DeadlineExceeded}
		}
		if result.Canceled {
			return "Command execution was canceled:\n" + result.Output, &executionStatusError{status: ToolCallInterrupted, cause: context.Canceled}
		}
		return fmt.Sprintf("Command completed (exit code: %d)\n%s", result.ExitCode, result.Output), nil
	}
	ctx, cancel := context.WithTimeout(ctx, config.CommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = r.workspaceRoot
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "Command timed out and was terminated:\n" + out.String(), &executionStatusError{status: ToolCallTimeout, cause: context.DeadlineExceeded}
	}
	if ctx.Err() == context.Canceled {
		return "Command execution was canceled:\n" + out.String(), &executionStatusError{status: ToolCallInterrupted, cause: context.Canceled}
	}
	exit := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exit = exitErr.ExitCode()
		} else {
			return "", err
		}
	}
	return fmt.Sprintf("Command completed (exit code: %d)\n%s", exit, out.String()), nil
}

func (r *Registry) relativePath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("path must not be empty")
	}
	var rel string
	if filepath.IsAbs(raw) {
		path := filepath.Clean(raw)
		var err error
		rel, err = filepath.Rel(r.workspaceRoot, path)
		if err != nil {
			return "", err
		}
	} else {
		rel = filepath.Clean(raw)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path is outside the working directory: %s", raw)
	}
	return rel, nil
}

func rejectGitMetadata(rel string) error {
	if sandbox.IsGitMetadataPath(rel) {
		return fmt.Errorf("%w: file tools must not modify .git directly", sandbox.ErrPolicy)
	}
	return nil
}

// writeTargetRelativePath 校验写目标：路径必须位于 workspace 内、不得直接命中 .git，
// 且经符号链接解析后仍不得落入 .git 或逃出 workspace。
func (r *Registry) writeTargetRelativePath(raw string) (string, error) {
	rel, err := r.relativePath(raw)
	if err != nil {
		return "", err
	}
	if err := rejectGitMetadata(rel); err != nil {
		return "", err
	}
	resolved, err := r.resolveWithinWorkspace(rel)
	if err != nil {
		return "", err
	}
	if err := rejectGitMetadata(resolved); err != nil {
		return "", err
	}
	return rel, nil
}

func (r *Registry) resolveWithinWorkspace(rel string) (string, error) {
	rootCanonical := sandbox.CanonicalizeAllowMissing(r.workspaceRoot)
	targetCanonical := sandbox.CanonicalizeAllowMissing(filepath.Join(rootCanonical, rel))
	resolved, err := filepath.Rel(rootCanonical, targetCanonical)
	if err != nil {
		return "", err
	}
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(os.PathSeparator)) || filepath.IsAbs(resolved) {
		return "", fmt.Errorf("path resolves through a symlink to a location outside the working directory: %s", rel)
	}
	return resolved, nil
}

type param struct {
	Name        string
	Type        string
	Description string
	Required    bool
}

func params(items ...param) json.RawMessage {
	props := map[string]any{}
	required := []string{}
	for _, item := range items {
		props[item.Name] = map[string]any{"type": item.Type, "description": item.Description}
		if item.Required {
			required = append(required, item.Name)
		}
	}
	data, _ := json.Marshal(map[string]any{"type": "object", "properties": props, "required": required})
	return data
}

const readFileOutputLimit = 12000

func (r *Registry) readFileOutputLimit() int {
	limit := readFileOutputLimit
	maxOutput := r.concurrencyConfig().MaxOutputChars
	if maxOutput > 700 && maxOutput-500 < limit {
		limit = maxOutput - 500
	}
	if limit < 200 {
		return 200
	}
	return limit
}

func optionalInt(raw, name string) (*int, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(strings.Trim(raw, `"`))
	if err != nil {
		return nil, fmt.Errorf("%s must be an integer: %s", name, raw)
	}
	return &n, nil
}

func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	return strings.Split(content, "\n")
}

type GuardResult struct {
	Allowed bool
	Reason  string
}

type CommandGuard struct{}

func (CommandGuard) Check(command string) GuardResult {
	if strings.TrimSpace(command) == "" {
		return GuardResult{Reason: "command must not be empty"}
	}
	lower := strings.ToLower(strings.Join(strings.Fields(command), " "))
	for _, pattern := range []string{"rm -rf /", "sudo ", "mkfs", "dd if=", ":(){", "chmod -r 777 /", "chown -r ", "diskutil erase"} {
		if strings.Contains(lower, pattern) {
			return GuardResult{Reason: "command matched the dangerous-command denylist: " + pattern}
		}
	}
	if isFullDiskScan(lower) {
		return GuardResult{Reason: "full-filesystem scans and traversals are prohibited"}
	}
	return GuardResult{Allowed: true}
}

func isFullDiskScan(lowerCommand string) bool {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\bfind\s+/($|\s.*)`),
		regexp.MustCompile(`\bgrep\s+-[a-z]*r[a-z]*\b.*\s/($|\s.*)`),
		regexp.MustCompile(`\bdu\s+[^;&|]*\s/($|\s.*)`),
		regexp.MustCompile(`\bls\s+-[a-z]*r[a-z]*\s+/($|\s.*)`),
	}
	for _, pattern := range patterns {
		if pattern.MatchString(lowerCommand) {
			return true
		}
	}
	return false
}

type ToolCallResult struct {
	ImageParts     []llm.ContentPart
	ToolCall       llm.ToolCall
	Result         string
	Status         ToolCallStatus
	DurationMillis int64
}

func RunToolCall(ctx context.Context, registry *Registry, call llm.ToolCall) (result ToolCallResult) {
	start := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			result = resultFromOutcome(call, start, ExecutionOutcome{
				Output: fmt.Sprintf("Tool execution failed: panic: %v", recovered),
				Status: ToolCallFailed,
			})
		}
	}()
	if registry == nil {
		return resultFromOutcome(call, start, ExecutionOutcome{Output: "Tool execution failed: Registry must not be nil", Status: ToolCallFailed})
	}
	args, err := ParseArguments(call.Function.Arguments)
	if err != nil {
		return resultFromOutcome(call, start, ExecutionOutcome{Output: "Tool argument parsing failed: " + err.Error(), Status: ToolCallFailed})
	}
	prepared, outcome, ok := registry.prepare(ctx, call.Function.Name, args, nil)
	if ok {
		outcome = registry.executePrepared(ctx, prepared)
	}
	return resultFromOutcome(call, start, outcome)
}

func resultFromOutcome(call llm.ToolCall, start time.Time, outcome ExecutionOutcome) ToolCallResult {
	status := outcome.Status
	if status == "" {
		status = ToolCallFailed
	}
	cleanText, imageParts := ParseToolResult(outcome.Output)
	return ToolCallResult{
		ToolCall:       call,
		Result:         cleanText,
		Status:         status,
		DurationMillis: time.Since(start).Milliseconds(),
		ImageParts:     imageParts,
	}
}

type ExecutionHooks struct {
	OnStarted   func(llm.ToolCall)
	OnCompleted func(ToolCallResult)
}

type hookNotifier struct {
	mu    sync.Mutex
	hooks ExecutionHooks
}

func (n *hookNotifier) started(ctx context.Context, call llm.ToolCall) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	if n.hooks.OnStarted != nil {
		n.hooks.OnStarted(call)
	}
	return true
}

func (n *hookNotifier) completed(result ToolCallResult) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.hooks.OnCompleted != nil {
		n.hooks.OnCompleted(result)
	}
}

type preparedBatchCall struct {
	index    int
	call     llm.ToolCall
	prepared preparedExecution
}

type indexedToolCallResult struct {
	index  int
	result ToolCallResult
}

type ParallelExecutor struct {
	Registry *Registry
	Config   runtime.ConcurrencyConfig
}

func (e ParallelExecutor) Execute(ctx context.Context, calls []llm.ToolCall, hooks ExecutionHooks) []ToolCallResult {
	if len(calls) == 0 {
		return nil
	}
	results := make([]ToolCallResult, len(calls))
	finished := make([]bool, len(calls))
	notifier := &hookNotifier{hooks: hooks}
	finish := func(index int, result ToolCallResult) {
		if finished[index] {
			return
		}
		results[index] = result
		finished[index] = true
		notifier.completed(result)
	}
	prepared := make([]preparedBatchCall, 0, len(calls))
	for index, call := range calls {
		start := time.Now()
		item, outcome, ok := prepareBatchCall(ctx, e.Registry, call)
		if !ok {
			finish(index, resultFromOutcome(call, start, outcome))
			continue
		}
		prepared = append(prepared, preparedBatchCall{index: index, call: call, prepared: item})
	}
	if len(prepared) == 0 {
		return results
	}

	config := e.Config.Normalize()
	batchCtx, cancel := context.WithTimeout(ctx, config.BatchTimeout)
	defer cancel()
	batchStart := time.Now()
	parallelism := config.ParallelismFor(len(prepared))
	for cursor := 0; cursor < len(prepared); {
		if err := batchCtx.Err(); err != nil {
			finishPending(prepared[cursor:], finish, batchStart, err)
			break
		}
		if parallelism == 1 || !prepared[cursor].prepared.tool.Policy.ParallelSafe {
			if !runExclusiveBatchCall(batchCtx, e.Registry, prepared[cursor], notifier, finish, batchStart) {
				finishPending(prepared[cursor+1:], finish, batchStart, batchCtx.Err())
				break
			}
			cursor++
			continue
		}
		end := cursor + 1
		for end < len(prepared) && prepared[end].prepared.tool.Policy.ParallelSafe {
			end++
		}
		if !runParallelBatchSegment(batchCtx, e.Registry, prepared[cursor:end], parallelism, notifier, finish, batchStart, finished) {
			finishPending(prepared[end:], finish, batchStart, batchCtx.Err())
			break
		}
		cursor = end
	}
	return results
}

func prepareBatchCall(ctx context.Context, registry *Registry, call llm.ToolCall) (prepared preparedExecution, outcome ExecutionOutcome, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			prepared = preparedExecution{}
			outcome = ExecutionOutcome{Output: fmt.Sprintf("Tool execution failed: panic: %v", recovered), Status: ToolCallFailed}
			ok = false
		}
	}()
	if registry == nil {
		return preparedExecution{}, ExecutionOutcome{Output: "Tool execution failed: Registry must not be nil", Status: ToolCallFailed}, false
	}
	args, err := ParseArguments(call.Function.Arguments)
	if err != nil {
		return preparedExecution{}, ExecutionOutcome{Output: "Tool argument parsing failed: " + err.Error(), Status: ToolCallFailed}, false
	}
	return registry.prepare(ctx, call.Function.Name, args, nil)
}

func runPreparedCall(ctx context.Context, registry *Registry, call preparedBatchCall) (result ToolCallResult) {
	start := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			result = resultFromOutcome(call.call, start, ExecutionOutcome{
				Output: fmt.Sprintf("Tool execution failed: panic: %v", recovered),
				Status: ToolCallFailed,
			})
		}
	}()
	outcome := registry.executePrepared(ctx, call.prepared)
	return resultFromOutcome(call.call, start, outcome)
}

func runExclusiveBatchCall(
	ctx context.Context,
	registry *Registry,
	call preparedBatchCall,
	notifier *hookNotifier,
	finish func(int, ToolCallResult),
	batchStart time.Time,
) bool {
	if !notifier.started(ctx, call.call) {
		finish(call.index, contextResult(call.call, batchStart, ctx.Err()))
		return false
	}
	resultCh := make(chan ToolCallResult, 1)
	go func() { resultCh <- runPreparedCall(ctx, registry, call) }()
	select {
	case result := <-resultCh:
		finish(call.index, result)
		return ctx.Err() == nil
	case <-ctx.Done():
		finish(call.index, contextResult(call.call, batchStart, ctx.Err()))
		return false
	}
}

func runParallelBatchSegment(
	ctx context.Context,
	registry *Registry,
	segment []preparedBatchCall,
	parallelism int,
	notifier *hookNotifier,
	finish func(int, ToolCallResult),
	batchStart time.Time,
	finished []bool,
) bool {
	workerCount := min(parallelism, len(segment))
	jobs := make(chan preparedBatchCall)
	out := make(chan indexedToolCallResult, len(segment))
	for range workerCount {
		go func() {
			for call := range jobs {
				if !notifier.started(ctx, call.call) {
					out <- indexedToolCallResult{index: call.index, result: contextResult(call.call, batchStart, ctx.Err())}
					continue
				}
				out <- indexedToolCallResult{index: call.index, result: runPreparedCall(ctx, registry, call)}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, call := range segment {
			select {
			case jobs <- call:
			case <-ctx.Done():
				return
			}
		}
	}()

	remaining := len(segment)
	for remaining > 0 {
		select {
		case completed := <-out:
			if !finished[completed.index] {
				finish(completed.index, completed.result)
				remaining--
			}
		case <-ctx.Done():
			for {
				select {
				case completed := <-out:
					if !finished[completed.index] {
						finish(completed.index, completed.result)
						remaining--
					}
				default:
					for _, call := range segment {
						if !finished[call.index] {
							finish(call.index, contextResult(call.call, batchStart, ctx.Err()))
						}
					}
					return false
				}
			}
		}
	}
	return true
}

func finishPending(calls []preparedBatchCall, finish func(int, ToolCallResult), start time.Time, err error) {
	if err == nil {
		err = context.Canceled
	}
	for _, call := range calls {
		finish(call.index, contextResult(call.call, start, err))
	}
}

func contextResult(call llm.ToolCall, start time.Time, err error) ToolCallResult {
	if err == nil {
		err = context.Canceled
	}
	return resultFromOutcome(call, start, contextOutcome(call.Function.Name, err))
}

// ParseToolResult extracts image content blocks from raw tool output.
// Mirrors Java ToolResultContent.parse(): each [bruce-image-content] block
// is replaced by a fallback text (not deleted), matching Java's behavior.
func ParseToolResult(raw string) (string, []llm.ContentPart) {
	if raw == "" {
		return raw, nil
	}
	const startTag = "[bruce-image-content mimeType="
	const endTag = "[/bruce-image-content]"
	var parts []llm.ContentPart
	var out strings.Builder
	cleaned := raw
	for {
		start := strings.Index(cleaned, startTag)
		if start < 0 {
			out.WriteString(cleaned)
			break
		}
		out.WriteString(cleaned[:start])

		headerEnd := strings.IndexByte(cleaned[start:], ']')
		if headerEnd < 0 {
			out.WriteString(cleaned[start:])
			break
		}
		headerEnd += start
		header := cleaned[start+len(startTag) : headerEnd]
		afterHeader := cleaned[headerEnd+1:]
		nl := strings.IndexByte(afterHeader, '\n')
		if nl < 0 {
			out.WriteString(cleaned[start:])
			break
		}
		bodyStart := headerEnd + 1 + nl + 1
		end := strings.Index(cleaned[bodyStart:], endTag)
		if end < 0 {
			out.WriteString(cleaned[start:])
			break
		}
		blockEnd := end + bodyStart + len(endTag)
		if blockEnd > len(cleaned) {
			out.WriteString(cleaned[start:])
			break
		}

		base64Data := strings.Join(strings.Fields(cleaned[bodyStart:end+bodyStart]), "")
		mimeType := ""
		source := "tool"
		for _, part := range strings.Split(header, " ") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) == 2 {
				switch strings.TrimSpace(kv[0]) {
				case "mimeType":
					mimeType = kv[1]
				case "source":
					source = kv[1]
				}
			}
		}

		imagePart, err := llm.FromBase64(base64Data, mimeType, source)
		if err == nil {
			parts = append(parts, imagePart)
			// Replace block with fallback text, mirroring Java fallbackText()
			out.WriteString("[Attached image: " + source + ", mimeType=" + mimeType + "]")
		} else {
			out.WriteString("[Failed to process image content: " + err.Error() + "]")
		}
		cleaned = cleaned[blockEnd:]
	}
	return strings.TrimSpace(out.String()), parts
}

func EncodeToolImage(mimeType, base64Data, source string) string {
	mime := mimeType
	if strings.TrimSpace(mime) == "" {
		mime = "image/png"
	}
	src := source
	if strings.TrimSpace(src) == "" {
		src = "tool"
	}
	return "\n[bruce-image-content mimeType=" + strings.TrimSpace(mime) +
		" source=" + src + "]\n" +
		strings.TrimSpace(base64Data) +
		"\n[/bruce-image-content]\n"
}
