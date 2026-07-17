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

type Tool struct {
	Name             string
	Description      string
	Parameters       json.RawMessage
	Exec             Executor
	PromptSnippet    string
	PromptGuidelines []string
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
	r.hitl = handler
	return r
}

func (r *Registry) WithConcurrency(config runtime.ConcurrencyConfig) *Registry {
	r.config = config.Normalize()
	return r
}

func (r *Registry) WithSandbox(manager *sandbox.Manager) *Registry {
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

func (r *Registry) Definitions() []llm.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]llm.ToolDefinition, 0, len(r.tools))
	for _, t := range r.sortedToolsLocked() {
		defs = append(defs, llm.ToolDefinition{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return defs
}

func (r *Registry) BuildPrompt() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var b strings.Builder
	b.WriteString("Available tools:\n")
	if len(r.tools) == 0 {
		b.WriteString("(none)\n")
	}
	for _, t := range r.sortedToolsLocked() {
		if strings.TrimSpace(t.PromptSnippet) != "" {
			b.WriteString("- " + t.Name + ": " + t.PromptSnippet + "\n")
		}
	}
	guidelines := r.defaultGuidelinesLocked()
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

func (r *Registry) defaultGuidelinesLocked() []string {
	seen := map[string]bool{}
	var guidelines []string
	add := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" || seen[text] {
			return
		}
		seen[text] = true
		guidelines = append(guidelines, text)
	}
	if _, ok := r.tools["execute_command"]; ok {
		add("本地目录浏览、文件发现和全文搜索优先使用 execute_command 运行 ls、rg --files、rg <pattern> 或 find。")
		add("构建、测试、Git 操作和脚本执行使用 execute_command。")
	}
	if _, ok := r.tools["read_file"]; ok {
		add("读取已知路径的单个文件用 read_file；大文件按返回提示继续使用 offset/limit 读取。")
	}
	if _, ok := r.tools["edit_file"]; ok {
		add("小范围修改已有文件用 edit_file，old_text 必须精确且唯一匹配。")
	}
	if _, ok := r.tools["write_file"]; ok {
		add("新建文件或完整覆盖文件用 write_file，不要用它做小范围修改。")
	}
	for _, tool := range r.tools {
		for _, guideline := range tool.PromptGuidelines {
			add(guideline)
		}
	}
	for name := range r.tools {
		if strings.HasPrefix(name, "mcp__filesystem__") {
			add("mcp__* 只在用户明确要求 MCP、内置工具无法满足，或需要该 MCP server 特有能力时使用。")
			break
		}
	}
	return guidelines
}

func (r *Registry) ExecuteJSON(ctx context.Context, name, argumentsJSON string) string {
	args, err := ParseArguments(argumentsJSON)
	if err != nil {
		return "工具参数解析失败: " + err.Error()
	}
	return r.Execute(ctx, name, args)
}

func (r *Registry) Execute(ctx context.Context, name string, args map[string]string) string {
	return r.execute(ctx, name, args, nil)
}

func (r *Registry) ExecuteWithSandboxMode(ctx context.Context, name string, args map[string]string, mode sandbox.Mode) string {
	return r.execute(ctx, name, args, &mode)
}

func (r *Registry) execute(ctx context.Context, name string, args map[string]string, modeOverride *sandbox.Mode) string {
	r.mu.RLock()
	t, ok := r.tools[name]
	names := make([]string, 0, len(r.tools))
	for toolName := range r.tools {
		names = append(names, toolName)
	}
	r.mu.RUnlock()
	if !ok {
		sort.Strings(names)
		return "未知工具: " + name + "，可用工具: " + strings.Join(names, ", ")
	}
	if rejected := r.validateToolRequest(name, args, modeOverride); rejected != "" {
		return rejected
	}
	if r.hitl != nil && r.hitl.Enabled() && approval.RequiresApproval(name) {
		raw, _ := json.Marshal(args)
		result := r.hitl.Request(approval.NewRequest(name, string(raw), ""))
		if result.IsRejected() {
			if result.Reason == "" {
				result.Reason = "用户拒绝了此操作"
			}
			return "[HITL] 操作已被拒绝：" + result.Reason
		}
		if result.IsSkipped() {
			return "[HITL] 操作已被跳过"
		}
		if result.Decision == approval.Modified {
			modified, err := ParseArguments(result.EffectiveArguments(string(raw)))
			if err != nil {
				return "工具参数解析失败: " + err.Error()
			}
			args = modified
			if rejected := r.validateToolRequest(name, args, modeOverride); rejected != "" {
				return rejected
			}
		}
	}
	var out string
	var err error
	if name == "execute_command" && modeOverride != nil {
		out, err = r.executeCommandWithMode(ctx, args, modeOverride)
	} else {
		out, err = t.Exec(ctx, args)
	}
	if err != nil {
		return "工具执行失败: " + err.Error()
	}
	return r.config.Truncate(out)
}

func (r *Registry) validateToolRequest(name string, args map[string]string, modeOverride *sandbox.Mode) string {
	if name == "execute_command" {
		if result := r.commandGuard.Check(args["command"]); !result.Allowed {
			return "命令被安全策略拒绝: " + result.Reason
		}
		if r.sandbox != nil {
			if err := r.sandbox.Preflight(modeOverride); err != nil {
				return "命令被 sandbox 拒绝: " + err.Error()
			}
		}
	}
	if name == "write_file" || name == "edit_file" {
		rel, err := r.writeTargetRelativePath(args["path"])
		if err != nil {
			if errors.Is(err, sandbox.ErrPolicy) {
				return "命令被 sandbox 拒绝: " + err.Error()
			}
			return "工具执行失败: " + err.Error()
		}
		if r.sandbox != nil {
			if err := r.sandbox.CanWriteFile(rel); err != nil {
				return "命令被 sandbox 拒绝: " + err.Error()
			}
		}
	}
	return ""
}

func (r *Registry) SandboxCanEnforce(mode sandbox.Mode) bool {
	return r.sandbox != nil && r.sandbox.Preflight(&mode) == nil
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
		Description:   "读取文件内容，支持 offset/limit 按 1-based 行号分段读取",
		Parameters:    params(param{"path", "string", "文件路径", true}, param{"offset", "integer", "起始行号", false}, param{"limit", "integer", "最多读取行数", false}),
		Exec:          r.readFile,
		PromptSnippet: "Read known file contents with optional offset/limit",
	})
	r.Register(Tool{
		Name:          "write_file",
		Description:   "新建文件或完整覆盖文件内容",
		Parameters:    params(param{"path", "string", "文件路径", true}, param{"content", "string", "文件内容", true}),
		Exec:          r.writeFile,
		PromptSnippet: "Create new files or completely overwrite existing files",
	})
	r.Register(Tool{
		Name:          "edit_file",
		Description:   "精确修改已有文件中的一段文本，old_text 必须唯一匹配",
		Parameters:    params(param{"path", "string", "文件路径", true}, param{"old_text", "string", "要替换的原文", true}, param{"new_text", "string", "替换后的文本", true}),
		Exec:          r.editFile,
		PromptSnippet: "Make precise small edits by replacing one unique exact text block",
	})
	r.Register(Tool{
		Name:          "execute_command",
		Description:   "在工作目录内执行 Shell 命令，用于 ls、rg、find、git、build、test、脚本运行等通用本地操作",
		Parameters:    params(param{"command", "string", "要执行的命令", true}),
		Exec:          r.executeCommand,
		PromptSnippet: "Execute shell commands for ls, rg, find, git, build, test, and scripts",
	})
}

func (r *Registry) readFile(_ context.Context, args map[string]string) (string, error) {
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
		return "offset 必须大于等于 1", nil
	}
	if limit != nil && *limit < 1 {
		return "limit 必须大于等于 1", nil
	}
	if offset == nil && limit == nil && len(content) <= outputLimit {
		return "文件内容:\n" + content, nil
	}
	lines := splitLines(content)
	totalLines := len(lines)
	startLine := 1
	if offset != nil {
		startLine = *offset
	}
	if totalLines == 0 {
		if startLine > 1 {
			return fmt.Sprintf("offset 超出文件末尾: offset=%d, 文件总行数=0", startLine), nil
		}
		return "文件内容 (lines 0-0 of 0):\n", nil
	}
	if startLine > totalLines {
		return fmt.Sprintf("offset 超出文件末尾: offset=%d, 文件总行数=%d", startLine, totalLines), nil
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
	fmt.Fprintf(&result, "文件内容 (lines %d-%d of %d):\n%s", startLine, displayEndLine, totalLines, selected.String())
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

func (r *Registry) writeFile(_ context.Context, args map[string]string) (string, error) {
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
	return "文件已写入: " + rel, nil
}

func (r *Registry) editFile(_ context.Context, args map[string]string) (string, error) {
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
		return "edit_file 失败: old_text 不能为空，文件未修改", nil
	}
	data, err := root.ReadFile(rel)
	if err != nil {
		return "", err
	}
	content := string(data)
	count := strings.Count(content, oldText)
	if count == 0 {
		return "edit_file 失败: old_text 未在文件中找到，文件未修改: " + rel, nil
	}
	if count > 1 {
		return fmt.Sprintf("edit_file 失败: old_text 匹配多处 (%d)，请提供更精确的 old_text，文件未修改: %s", count, rel), nil
	}
	updated := strings.Replace(content, oldText, args["new_text"], 1)
	if err := root.WriteFile(rel, []byte(updated), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("文件已编辑: %s (替换 1 处，%d -> %d 字符)", rel, len(oldText), len(args["new_text"])), nil
}

func (r *Registry) executeCommand(ctx context.Context, args map[string]string) (string, error) {
	return r.executeCommandWithMode(ctx, args, nil)
}

func (r *Registry) executeCommandWithMode(ctx context.Context, args map[string]string, modeOverride *sandbox.Mode) (string, error) {
	command := strings.TrimSpace(args["command"])
	if command == "" {
		return "命令不能为空", nil
	}
	config := r.config.Normalize()
	if r.sandbox != nil {
		result, err := r.sandbox.Run(ctx, command, config.CommandTimeout, config.MaxOutputChars, modeOverride)
		if err != nil {
			return "", err
		}
		if result.TimedOut {
			return "命令执行超时，已终止:\n" + result.Output, nil
		}
		if result.Canceled {
			return "命令执行已取消:\n" + result.Output, nil
		}
		return fmt.Sprintf("命令执行完成 (exit code: %d)\n%s", result.ExitCode, result.Output), nil
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
		return "命令执行超时，已终止:\n" + out.String(), nil
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
	return fmt.Sprintf("命令执行完成 (exit code: %d)\n%s", exit, out.String()), nil
}

func (r *Registry) relativePath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("路径不能为空")
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
		return "", fmt.Errorf("路径超出工作目录: %s", raw)
	}
	return rel, nil
}

func rejectGitMetadata(rel string) error {
	if sandbox.IsGitMetadataPath(rel) {
		return fmt.Errorf("%w: 文件工具禁止直接修改 .git", sandbox.ErrPolicy)
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
		return "", fmt.Errorf("路径经符号链接解析后超出工作目录: %s", rel)
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
	maxOutput := r.config.Normalize().MaxOutputChars
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
		return nil, fmt.Errorf("%s 必须是整数: %s", name, raw)
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
		return GuardResult{Reason: "命令不能为空"}
	}
	lower := strings.ToLower(strings.Join(strings.Fields(command), " "))
	for _, pattern := range []string{"rm -rf /", "sudo ", "mkfs", "dd if=", ":(){", "chmod -r 777 /", "chown -r ", "diskutil erase"} {
		if strings.Contains(lower, pattern) {
			return GuardResult{Reason: "命中危险命令黑名单: " + pattern}
		}
	}
	if isFullDiskScan(lower) {
		return GuardResult{Reason: "禁止全盘扫描或全盘遍历"}
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
	Status         string
	DurationMillis int64
}

func RunToolCall(ctx context.Context, registry *Registry, call llm.ToolCall) ToolCallResult {
	start := time.Now()
	result := registry.ExecuteJSON(ctx, call.Function.Name, call.Function.Arguments)
	status := "success"
	if strings.HasPrefix(result, "工具执行失败") || strings.HasPrefix(result, "工具参数解析失败") {
		status = "failed"
	}
	cleanText, imageParts := ParseToolResult(result)
	return ToolCallResult{
		ToolCall:       call,
		Result:         cleanText,
		Status:         status,
		DurationMillis: time.Since(start).Milliseconds(),
		ImageParts:     imageParts,
	}
}

type ParallelExecutor struct {
	Registry *Registry
	Config   runtime.ConcurrencyConfig
}

func (e ParallelExecutor) Execute(ctx context.Context, calls []llm.ToolCall) []ToolCallResult {
	if len(calls) == 0 {
		return nil
	}
	if len(calls) == 1 {
		return []ToolCallResult{RunToolCall(ctx, e.Registry, calls[0])}
	}
	config := e.Config.Normalize()
	ctx, cancel := context.WithTimeout(ctx, config.BatchTimeout)
	defer cancel()
	results := make([]ToolCallResult, len(calls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, config.ParallelismFor(len(calls)))
	for i, call := range calls {
		i, call := i, call
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = RunToolCall(ctx, e.Registry, call)
		}()
	}
	wg.Wait()
	return results
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
			out.WriteString("[已附加图片: " + source + ", mimeType=" + mimeType + "]")
		} else {
			out.WriteString("[图片内容处理失败: " + err.Error() + "]")
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
