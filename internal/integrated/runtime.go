package integrated

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"bruce-go/internal/agent"
	"bruce-go/internal/approval"
	"bruce-go/internal/cli"
	"bruce-go/internal/config"
	"bruce-go/internal/event"
	"bruce-go/internal/instructions"
	"bruce-go/internal/llm"
	"bruce-go/internal/mcp"
	"bruce-go/internal/planning"
	"bruce-go/internal/render"
	"bruce-go/internal/runtime"
	"bruce-go/internal/sandbox"
	"bruce-go/internal/session"
	"bruce-go/internal/skill"
	"bruce-go/internal/tool"
	"bruce-go/internal/web"
)

type Options struct {
	Workspace    string
	HomeDir      string
	SettingsPath string
	Client       llm.ChatClient
	StartMCP     bool
}

type Runtime struct {
	Workspace string
	HomeDir   string
	Settings  config.Settings
	Loader    config.Loader

	Client     llm.ChatClient
	switchable modelSwitcher
	Tools      *tool.Registry
	Web        *web.Manager
	MCP        *mcp.Manager
	Skills     *skill.Catalog
	Session    *session.Store
	Events     *event.Bus
	HITL       approval.Handler
	Mode       runtime.AgentMode
	Parallel   bool
	Concurrent runtime.ConcurrencyConfig
	StartMCP   bool
	Sandbox    *sandbox.Manager

	react      *agent.Agent
	planning   *agent.Agent
	planStore  *planning.Store
	startMu    sync.Mutex
	mcpStarted bool

	mcpToolNames []string
}

type modelSwitcher interface {
	llm.ChatClient
	Options() []llm.ModelOption
	Current() llm.ModelOption
	Switch(selector string) (llm.ModelOption, error)
	ReasoningEffort() string
	SetReasoningEffort(level string) error
}

var errCompactionRequired = errors.New("context compaction required")

func validateCompactionWindow(settings config.Compaction, client llm.ChatClient) error {
	if !settings.Enabled || client.MaxContextWindow() <= 0 {
		return nil
	}
	if _, err := settings.Threshold(client.MaxContextWindow()); err != nil {
		return fmt.Errorf("模型 %s/%s 的自动压缩配置无效: %w", client.ProviderName(), client.ModelName(), err)
	}
	return nil
}

func New(ctx context.Context, opts Options) (*Runtime, error) {
	workspace := abs(opts.Workspace)
	if canonical, err := sandbox.CanonicalAbsolute(workspace); err == nil {
		workspace = canonical
	}
	home := opts.HomeDir
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil || home == "" {
			home = "."
		}
	}
	home = abs(home)
	loader := config.NewLoader(opts.SettingsPath)
	settings, err := loader.Load()
	if err != nil {
		return nil, err
	}

	var client llm.ChatClient
	var switcher modelSwitcher
	if opts.Client != nil {
		client = opts.Client
		if s, ok := opts.Client.(modelSwitcher); ok {
			switcher = s
		}
	} else {
		s, err := llm.NewSwitchable(settings, loader)
		if err != nil {
			return nil, err
		}
		client = s
		switcher = s
	}
	if err := validateCompactionWindow(settings.Compaction, client); err != nil {
		return nil, err
	}

	hitl := approval.NewAutoHandler(false, approval.Approve())
	concurrency := runtime.DefaultConcurrency()
	if settings.Sandbox.CommandTimeoutSeconds > 0 {
		concurrency.CommandTimeout = time.Duration(settings.Sandbox.CommandTimeoutSeconds) * time.Second
	}
	sandboxMode, err := sandbox.ParseMode(settings.Sandbox.Mode)
	if err != nil {
		return nil, err
	}
	sandboxManager, err := sandbox.New(ctx, sandbox.Options{
		Workspace:     workspace,
		HomeDir:       home,
		Mode:          sandboxMode,
		NetworkAccess: settings.Sandbox.NetworkAccess,
		AllowedEnv:    settings.Sandbox.AllowedEnv,
	})
	if err != nil {
		return nil, err
	}
	registry := tool.NewRegistry(workspace).WithHITL(hitl).WithConcurrency(concurrency).WithSandbox(sandboxManager)
	webManager := web.NewManager(settings.WebSearch, nil)
	web.RegisterTools(registry, webManager)
	skills := skill.NewCatalog(home, workspace)
	skill.RegisterTools(registry, skills)
	mcpManager := mcp.NewManager(settings.MCP, workspace).WithSandbox(sandboxManager)

	store, err := session.CreateNew(home, workspace, runtime.ModeReact)
	if err != nil {
		_ = sandboxManager.Close()
		return nil, err
	}
	bus := event.NewBus()
	r := &Runtime{
		Workspace:  workspace,
		HomeDir:    home,
		Settings:   settings,
		Loader:     loader,
		Client:     client,
		switchable: switcher,
		Tools:      registry,
		Web:        webManager,
		MCP:        mcpManager,
		Skills:     skills,
		Session:    store,
		Events:     bus,
		HITL:       hitl,
		Mode:       runtime.ModeReact,
		Parallel:   true,
		Concurrent: concurrency,
		StartMCP:   opts.StartMCP,
		Sandbox:    sandboxManager,
	}
	r.planStore = planning.NewStore(home, store)
	r.subscribeSessionRecorder()
	r.refreshMCPTools()
	r.rebuildAgents()
	return r, nil
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	var errs []error
	if r.MCP != nil {
		errs = append(errs, r.MCP.Close())
	}
	if r.Sandbox != nil {
		errs = append(errs, r.Sandbox.Close())
	}
	return errors.Join(errs...)
}

func (r *Runtime) Start(ctx context.Context) {
	if r == nil || !r.StartMCP {
		return
	}
	r.startMu.Lock()
	if r.mcpStarted {
		r.startMu.Unlock()
		return
	}
	r.mcpStarted = true
	r.startMu.Unlock()

	statuses := r.MCP.Status()
	enabled := 0
	for _, status := range statuses {
		if status.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		r.emit(event.NewActivity("", "未配置需要启动的 MCP server。"))
		return
	}
	r.emit(event.NewActivity("", fmt.Sprintf("启动 MCP server (%d 个)...", enabled)))
	startCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	r.MCP.StartEnabled(startCtx)
	r.refreshMCPTools()
	for _, status := range r.MCP.Status() {
		if status.Error != "" {
			r.emit(event.NewActivity("", "MCP 初始化失败: "+status.Name+": "+status.Error))
		}
	}
	r.emit(event.NewActivity("", "MCP server 启动完成。"))
}

func (r *Runtime) Handle(ctx context.Context, input string) cli.Result {
	if command, ok := cli.Parse(input); ok {
		return r.HandleCommand(ctx, command)
	}
	out, err := r.RunTask(ctx, input)
	return cli.Result{Handled: true, Output: out, Err: err}
}

func (r *Runtime) RunTask(ctx context.Context, input string) (string, error) {
	return r.runTask(ctx, input, false)
}

func (r *Runtime) runTask(ctx context.Context, input string, allowPendingPlanInput bool) (string, error) {
	r.Skills.BeginTask()
	defer r.Skills.EndTask()
	runID := event.NewRunID()
	invocation, err := skill.ParseInvocation(input)
	if err != nil {
		r.emit(event.NewRunFailed(runID, err.Error()))
		return "", err
	}
	task := invocation.Task
	if task == "" {
		task = strings.TrimSpace(input)
	}
	r.emit(event.NewRunStarted(runID, r.Mode, task))
	if r.Mode == runtime.ModePlan && !allowPendingPlanInput {
		if prompt, ok := r.pendingPlanInputPrompt(); ok {
			user := llm.User(task)
			r.emit(event.NewMessageCompleted(runID, user, true))
			assistant := llm.Assistant(prompt)
			r.emit(event.NewMessageCompleted(runID, assistant, true))
			r.emit(event.NewRunCompleted(runID, prompt))
			return prompt, nil
		}
	}
	for _, name := range invocation.Names {
		if _, err := r.Skills.LoadSkill(name); err != nil {
			r.emit(event.NewRunFailed(runID, err.Error()))
			return "", err
		}
	}
	taskContext := r.taskContext()
	prepared, err := llm.ParseImageReferences(ctx, task, r.Workspace, nil)
	if err != nil {
		r.emit(event.NewRunFailed(runID, err.Error()))
		return "", err
	}
	if r.Mode == runtime.ModePlan {
		out, err := r.runAgentWithCompaction(ctx, r.planning, prepared, r.taskContextWithPlan(taskContext), runID)
		if err != nil {
			r.emit(event.NewRunFailed(runID, err.Error()))
			return "", err
		}
		display, err := r.presentPlan(runID, out)
		if err != nil {
			r.emit(event.NewRunFailed(runID, err.Error()))
			return "", err
		}
		r.emit(event.NewRunCompleted(runID, display))
		return display, nil
	}
	out, err := r.runAgentWithCompaction(ctx, r.react, prepared, r.taskContextWithPlan(taskContext), runID)
	if err != nil {
		r.emit(event.NewRunFailed(runID, err.Error()))
		return "", err
	}
	r.emit(event.NewRunCompleted(runID, out))
	return out, nil
}

func (r *Runtime) HandleCommand(ctx context.Context, command cli.Command) cli.Result {
	result := cli.Result{Handled: true}
	switch command.Name {
	case "help":
		result.Output = cli.Help()
	case "exit":
		result.Exit = true
		result.Output = "bye"
	case "react":
		result.Err = r.setMode(runtime.ModeReact)
		result.Output = "已切换到 ReAct 模式"
	case "plan":
		result.Output, result.Err = r.handlePlan(ctx, command.Args, command.Raw)
	case "model":
		result.Output, result.Err = r.handleModel(command.Args)
	case "web":
		result.Output, result.Err = r.handleWeb(ctx, command.Args)
	case "mcp":
		result.Output, result.Err = r.handleMCP(ctx, command.Args)
	case "skill":
		result.Output, result.Err = r.handleSkill(command.Args)
	case "hitl":
		result.Output, result.Err = r.handleToggle(command.Args, "HITL", r.HITL.Enabled(), func(v bool) { r.HITL.SetEnabled(v) })
	case "sandbox":
		result.Output, result.Err = r.handleSandbox(ctx, command.Args)
	case "parallel":
		result.Output, result.Err = r.handleParallel(command.Args)
	case "status":
		result.Output = render.Status(r.Status())
	case "session":
		result.Output = render.Session(r.Session.Context(r.Mode))
	case "sessions":
		summaries, err := r.Session.List(r.Mode)
		result.Output, result.Err = render.Sessions(summaries), err
	case "new", "clear":
		result.Err = r.newSession()
		if command.Name == "clear" {
			r.HITL.ClearApprovedAll()
			result.Output = "已清空当前对话并开启新 session"
		} else {
			result.Output = "已开启新 session"
		}
		if result.Err == nil {
			r.emit(event.NewSessionChanged(command.Name, r.Session.Context(r.Mode)))
		}
	case "resume":
		ref := strings.Join(command.Args, " ")
		result.Err = r.Session.Resume(ref)
		if result.Err == nil {
			r.Mode = r.Session.Context(r.Mode).Mode
			result.Output = "已恢复 session: " + r.Session.Context(r.Mode).SessionID
			r.rebuildAgents()
			r.emit(event.NewSessionChanged("resume", r.Session.Context(r.Mode)))
		}
	case "tree":
		if len(command.Args) == 0 {
			result.Output = r.Session.RenderTree(r.Mode)
		} else {
			result.Err = r.Session.SelectLeaf(command.Args[0])
			if result.Err == nil {
				result.Output = r.Session.RenderTree(r.Mode)
			}
		}
	case "compact":
		result.Output, result.Err = r.compact(ctx, strings.Join(command.Args, " "))
		if result.Err == nil {
			r.emit(event.NewSessionChanged("compact", r.Session.Context(r.Mode)))
		}
	default:
		result.Err = errors.New("未知命令: /" + command.Name)
	}
	if result.Err != nil && result.Output == "" {
		result.Output = result.Err.Error()
	}
	return result
}

func (r *Runtime) Status() runtime.Status {
	toolNames := r.Tools.ToolNames()
	sort.Strings(toolNames)
	mcpStatuses := r.MCP.Status()
	mcpSummary := render.MCP(mcpStatuses)
	sandboxStatus := r.Sandbox.Status()
	return runtime.Status{
		Mode:              r.Mode,
		Model:             r.Client.ModelName(),
		Provider:          r.Client.ProviderName(),
		WorkspaceRoot:     r.Workspace,
		RAGEnabled:        false,
		WebEnabled:        r.Web != nil && r.Web.Enabled,
		WebSearchProvider: strings.TrimSpace(r.Settings.WebSearch.Provider),
		MCPSummary:        mcpSummary,
		HITLEnabled:       r.HITL.Enabled(),
		SandboxMode:       string(sandboxStatus.Mode),
		SandboxBackend:    sandboxStatus.Capabilities.Backend,
		SandboxNetwork:    sandboxStatus.NetworkAccess,
		SandboxAvailable:  sandboxStatus.Capabilities.Available || sandboxStatus.Mode == sandbox.ModeFullAccess,
		SandboxReason:     sandboxStatus.Capabilities.Reason,
		ParallelEnabled:   r.Parallel,
		MaxParallelism:    r.Concurrent.MaxParallelism,
		BatchTimeout:      r.Concurrent.BatchTimeout,
		RAGIndexed:        false,
		SkillCount:        len(r.Skills.Skills()),
		ToolNames:         toolNames,
		ActivePlan:        r.currentPlanState(),
	}
}

func (r *Runtime) handleSandbox(ctx context.Context, args []string) (string, error) {
	if r.Sandbox == nil {
		return "", errors.New("sandbox 未初始化")
	}
	statusText := func() string {
		status := r.Sandbox.Status()
		available := status.Capabilities.Available || status.Mode == sandbox.ModeFullAccess
		network := onOff(status.NetworkAccess)
		if status.NetworkAccess != status.ConfiguredNetworkAccess {
			network += fmt.Sprintf(" (配置=%s)", onOff(status.ConfiguredNetworkAccess))
		}
		text := fmt.Sprintf("Sandbox: mode=%s, backend=%s, network=%s, available=%s",
			status.Mode, status.Capabilities.Backend, network, onOff(available))
		if !available && strings.TrimSpace(status.Capabilities.Reason) != "" {
			text += "\nReason: " + status.Capabilities.Reason
		}
		if r.MCP != nil {
			text += "\nMCP:\n" + render.MCP(r.MCP.Status())
		}
		return text
	}
	if len(args) == 0 || args[0] == "status" {
		return statusText(), nil
	}
	switch args[0] {
	case "mode":
		if len(args) != 2 {
			return "", errors.New("用法: /sandbox mode read-only|workspace-write|full-access")
		}
		mode, err := sandbox.ParseMode(args[1])
		if err != nil {
			return "", err
		}
		if err := r.Sandbox.ValidateMode(mode); err != nil {
			return "", err
		}
		if err := r.MCP.Reconfigure(ctx, func() error {
			return r.Sandbox.SetMode(mode)
		}); err != nil {
			return "", err
		}
		r.refreshMCPTools()
		r.rebuildAgents()
		return statusText(), nil
	case "network":
		if len(args) != 2 || (args[1] != "on" && args[1] != "off") {
			return "", errors.New("用法: /sandbox network on|off")
		}
		enabled := args[1] == "on"
		if err := r.MCP.Reconfigure(ctx, func() error {
			r.Sandbox.SetNetworkAccess(enabled)
			return nil
		}); err != nil {
			return "", err
		}
		r.refreshMCPTools()
		r.rebuildAgents()
		text := statusText()
		if !enabled && r.Sandbox.Mode() == sandbox.ModeFullAccess {
			text += "\n注意: full-access 模式下网络始终开启，该设置将在切换到受限模式后生效"
		}
		return text, nil
	default:
		return "", errors.New("用法: /sandbox [status|mode <mode>|network on|off]")
	}
}

func (r *Runtime) handlePlan(ctx context.Context, args []string, raw string) (string, error) {
	if len(args) == 0 {
		if err := r.setMode(runtime.ModePlan); err != nil {
			return "", err
		}
		return "已切换到 Plan 模式。输入任务开始规划，或使用 /plan <任务> 直接开始。", nil
	}
	sub := strings.ToLower(args[0])
	switch sub {
	case "approve":
		return r.approvePlan(ctx, raw)
	case "reject":
		return r.rejectPlan(runtime.PlanActionRejected, strings.TrimSpace(strings.Join(args[1:], " ")))
	case "cancel":
		return r.rejectPlan(runtime.PlanActionCanceled, strings.TrimSpace(strings.Join(args[1:], " ")))
	case "continue":
		if err := r.setMode(runtime.ModePlan); err != nil {
			return "", err
		}
		feedback := strings.TrimSpace(strings.Join(args[1:], " "))
		if feedback == "" {
			feedback = "请继续完善当前计划。"
		}
		return r.runTask(ctx, feedback, true)
	default:
		if err := r.setMode(runtime.ModePlan); err != nil {
			return "", err
		}
		return r.RunTask(ctx, strings.Join(args, " "))
	}
}

func (r *Runtime) handleModel(args []string) (string, error) {
	if len(args) > 0 && strings.EqualFold(args[0], "reasoning") {
		return r.handleModelReasoning(args[1:])
	}
	if r.switchable == nil {
		return fmt.Sprintf("当前模型: %s/%s", r.Client.ProviderName(), r.Client.ModelName()), nil
	}
	if len(args) == 0 {
		current := r.switchable.Current()
		var b strings.Builder
		b.WriteString("当前模型: " + current.Selector() + "\n可用模型:\n")
		for _, opt := range r.switchable.Options() {
			prefix := "  "
			if opt.Provider == current.Provider && opt.Model == current.Model {
				prefix = "* "
			}
			b.WriteString(prefix + opt.Selector() + "\n")
		}
		b.WriteString("\n当前推理级别: " + r.ReasoningEffort() + "（/model reasoning <级别> 调整）")
		return strings.TrimSpace(b.String()), nil
	}
	next, err := r.switchable.Switch(strings.Join(args, " "))
	if err != nil {
		return "", err
	}
	r.Client = r.switchable
	r.rebuildAgents()
	return "已切换模型: " + next.Selector() + " " + r.ReasoningEffort(), nil
}

func (r *Runtime) handleModelReasoning(args []string) (string, error) {
	current := r.ReasoningEffort()
	if len(args) == 0 {
		return fmt.Sprintf("当前推理级别: %s\n可选级别: off / low / medium / high / max", current), nil
	}
	if err := r.SetReasoningEffort(args[0]); err != nil {
		return "", err
	}
	return "已切换推理级别: " + r.ReasoningEffort(), nil
}

func (r *Runtime) handleWeb(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 || args[0] == "status" {
		return r.Web.Status(), nil
	}
	switch args[0] {
	case "on":
		r.Web.SetEnabled(true)
		return "Web 已开启", nil
	case "off":
		r.Web.SetEnabled(false)
		return "Web 已关闭", nil
	case "search":
		if len(args) < 2 {
			return "", errors.New("用法: /web search <query>")
		}
		results, err := r.Web.Search(ctx, strings.Join(args[1:], " "), 5)
		if err != nil {
			return "", err
		}
		return formatSearch(results), nil
	case "fetch":
		if len(args) != 2 {
			return "", errors.New("用法: /web fetch <url>")
		}
		page, err := r.Web.Fetch(ctx, args[1])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s\n\n%s", page.Title, page.Text), nil
	default:
		return "", errors.New("用法: /web on|off|status|search <query>|fetch <url>")
	}
}

func (r *Runtime) handleMCP(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return render.MCP(r.MCP.Status()), nil
	}
	if len(args) < 2 {
		return "", errors.New("用法: /mcp restart|logs|disable|enable <name>")
	}
	name := args[1]
	switch args[0] {
	case "restart":
		if err := r.MCP.Restart(ctx, name); err != nil {
			return "", err
		}
		r.refreshMCPTools()
		return "已重启 MCP server: " + name, nil
	case "logs":
		return render.Lines(r.MCP.Logs(name)), nil
	case "disable":
		if err := r.MCP.Disable(name); err != nil {
			return "", err
		}
		r.refreshMCPTools()
		return "已禁用 MCP server: " + name, nil
	case "enable":
		if err := r.MCP.Enable(ctx, name); err != nil {
			return "", err
		}
		r.refreshMCPTools()
		return "已启用 MCP server: " + name, nil
	default:
		return "", errors.New("用法: /mcp restart|logs|disable|enable <name>")
	}
}

func (r *Runtime) handleSkill(args []string) (string, error) {
	if len(args) == 0 || args[0] == "list" {
		return render.Skills(r.Skills.Skills(), r.Skills.Diagnostics(), r.Skills.Overrides()), nil
	}
	switch args[0] {
	case "show":
		if len(args) != 2 {
			return "", errors.New("用法: /skill show <name>")
		}
		def, ok := r.Skills.Find(args[1])
		if !ok {
			return "", errors.New("未知 Skill: " + args[1])
		}
		return render.Skill(def), nil
	case "reload":
		result := r.Skills.Reload()
		r.rebuildAgents()
		return render.Skills(result.Skills, result.Diagnostics, result.Overrides), nil
	default:
		return "", errors.New("用法: /skill list|show <name>|reload")
	}
}

func (r *Runtime) handleToggle(args []string, label string, current bool, set func(bool)) (string, error) {
	if len(args) == 0 || args[0] == "status" {
		return label + ": " + onOff(current), nil
	}
	switch args[0] {
	case "on":
		set(true)
		return label + " 已开启", nil
	case "off":
		set(false)
		return label + " 已关闭", nil
	default:
		return "", errors.New("用法: /" + strings.ToLower(label) + " on|off|status")
	}
}

func (r *Runtime) handleParallel(args []string) (string, error) {
	return r.handleToggle(args, "Parallel", r.Parallel, func(v bool) {
		r.Parallel = v
		if v {
			timeout := r.Concurrent.CommandTimeout
			r.Concurrent = runtime.DefaultConcurrency()
			if timeout > 0 {
				r.Concurrent.CommandTimeout = timeout
			}
		} else {
			r.Concurrent.MaxParallelism = 1
		}
		r.Tools.WithConcurrency(r.Concurrent)
		r.rebuildAgents()
	})
}

func (r *Runtime) setMode(mode runtime.AgentMode) error {
	r.Mode = mode
	r.emit(event.NewModeChanged(mode))
	return nil
}

func (r *Runtime) newSession() error {
	if err := r.Session.CreateNew(r.Mode); err != nil {
		return err
	}
	r.rebuildAgents()
	return nil
}

func (r *Runtime) runAgentWithCompaction(ctx context.Context, currentAgent *agent.Agent, input llm.PreparedInput, taskContext, runID string) (string, error) {
	currentAgent.RestoreHistory(r.Session.Context(r.Mode).Messages)
	out, err := currentAgent.Run(ctx, input, taskContext, runID)
	overflowRetries := 0
	thresholdRetries := 0
	for err != nil {
		switch {
		case errors.Is(err, errCompactionRequired):
			if thresholdRetries >= 3 {
				return "", errors.New("上下文在自动压缩后仍持续超过阈值")
			}
			thresholdRetries++
			if _, compactErr := r.autoCompact(ctx, runID, "阈值"); compactErr != nil {
				r.emit(event.NewActivity(runID, "自动压缩失败，将尝试继续当前任务: "+compactErr.Error()))
				currentAgent.SkipNextBeforeChat()
			} else {
				currentAgent.RestoreHistory(r.Session.Context(r.Mode).Messages)
			}
			out, err = currentAgent.Continue(ctx, taskContext, runID)
		case llm.IsContextOverflowError(err):
			if !r.Settings.Compaction.Enabled {
				return "", fmt.Errorf("模型上下文已溢出，且自动压缩已禁用: %w", err)
			}
			if overflowRetries >= 1 {
				return "", fmt.Errorf("自动压缩后模型上下文仍然溢出，已停止重试: %w", err)
			}
			overflowRetries++
			if _, compactErr := r.autoCompact(ctx, runID, "上下文溢出恢复"); compactErr != nil {
				return "", fmt.Errorf("上下文溢出后的自动压缩失败: %w", compactErr)
			}
			currentAgent.RestoreHistory(r.Session.Context(r.Mode).Messages)
			currentAgent.DiscardTrailingOverflowResponse()
			out, err = currentAgent.Continue(ctx, taskContext, runID)
		default:
			return "", err
		}
	}
	r.compactAfterSuccessfulTurn(ctx, runID)
	return out, nil
}

func (r *Runtime) compactAfterSuccessfulTurn(ctx context.Context, runID string) {
	response, ok := r.latestAssistantResponseAfterCompaction()
	if ok {
		overflow := llm.DetectContextOverflowResponse(response, r.Client.MaxContextWindow())
		if overflow.Overflow {
			if !r.Settings.Compaction.Enabled {
				return
			}
			if _, err := r.autoCompact(ctx, runID, "成功响应 usage 超出上下文窗口"); err != nil {
				r.emit(event.NewActivity(runID, "自动压缩失败，但当前回答已完成: "+err.Error()))
			}
			return
		}
	}
	needed, _, _, thresholdErr := r.compactionThreshold()
	if thresholdErr != nil {
		r.emit(event.NewActivity(runID, "自动压缩阈值配置无效，但当前回答已完成: "+thresholdErr.Error()))
		return
	}
	if !needed {
		return
	}
	if _, err := r.autoCompact(ctx, runID, "回合结束阈值"); err != nil {
		r.emit(event.NewActivity(runID, "自动压缩失败，但当前回答已完成: "+err.Error()))
	}
}

func (r *Runtime) compactionThreshold() (needed bool, tokens int, threshold int, err error) {
	return r.compactionThresholdFor(r.Session.Context(r.Mode).Messages)
}

func (r *Runtime) compactionThresholdFor(messages []llm.Message) (needed bool, tokens int, threshold int, err error) {
	if !r.Settings.Compaction.Enabled || r.Client.MaxContextWindow() <= 0 {
		return false, 0, 0, nil
	}
	threshold, err = r.Settings.Compaction.Threshold(r.Client.MaxContextWindow())
	if err != nil {
		return false, 0, 0, err
	}
	entries := r.Session.ActiveEntries()
	lastCompaction := -1
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == session.TypeCompaction {
			lastCompaction = i
			break
		}
	}
	hasFreshUsage := false
	for i := len(entries) - 1; i > lastCompaction; i-- {
		entry := entries[i]
		if entry.Type == session.TypeMessage && entry.Message != nil && entry.Message.Role == llm.RoleAssistant && entry.Message.TotalUsageTokens() > 0 &&
			!strings.EqualFold(entry.Message.FinishReason, "aborted") && !strings.EqualFold(entry.Message.FinishReason, "error") {
			hasFreshUsage = true
			break
		}
	}
	if !hasFreshUsage {
		return false, 0, 0, nil
	}
	tokens = session.EstimateContextTokens(messages).Tokens
	return session.ShouldCompact(tokens, r.Client.MaxContextWindow(), r.Settings.Compaction), tokens, threshold, nil
}

func (r *Runtime) latestAssistantResponseAfterCompaction() (llm.ChatResponse, bool) {
	entries := r.Session.ActiveEntries()
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.Type == session.TypeCompaction {
			break
		}
		if entry.Type != session.TypeMessage || entry.Message == nil || entry.Message.Role != llm.RoleAssistant {
			continue
		}
		message := entry.Message
		return llm.ChatResponse{
			Content:           message.Content,
			ReasoningContent:  message.ReasoningContent,
			ToolCalls:         message.ToolCalls,
			InputTokens:       message.InputTokens,
			OutputTokens:      message.OutputTokens,
			CachedInputTokens: message.CachedInputTokens,
			Provider:          message.Provider,
			Model:             message.Model,
			FinishReason:      message.FinishReason,
		}, true
	}
	return llm.ChatResponse{}, false
}

func (r *Runtime) performCompaction(ctx context.Context, instructions string) (session.CompactionResult, error) {
	preparation, ok := session.PrepareCompaction(r.Session.ActiveEntries(), r.Settings.Compaction)
	if !ok {
		return session.CompactionResult{}, errors.New("当前会话没有可安全压缩的历史，或最新节点已经是 compaction")
	}
	result, err := session.Compact(ctx, r.Client, *preparation, instructions)
	if err != nil {
		return session.CompactionResult{}, err
	}
	if err := r.Session.AppendCompaction(result.Summary, result.FirstKeptEntryID, result.TokensBefore, result.Details); err != nil {
		return session.CompactionResult{}, err
	}
	return result, nil
}

func (r *Runtime) autoCompact(ctx context.Context, runID, reason string) (session.CompactionResult, error) {
	result, err := r.performCompaction(ctx, "")
	if err != nil {
		return session.CompactionResult{}, err
	}
	r.emit(event.NewActivity(runID, fmt.Sprintf("已执行自动压缩（%s，压缩前约 %d tokens）。", reason, result.TokensBefore)))
	r.emit(event.NewSessionChanged("compact", r.Session.Context(r.Mode)))
	return result, nil
}

func (r *Runtime) compact(ctx context.Context, extra string) (string, error) {
	entries := r.Session.ActiveEntries()
	if len(entries) == 0 {
		return "当前 session 没有可压缩的历史。", nil
	}
	if entries[len(entries)-1].Type == session.TypeCompaction {
		return "", errors.New("最新节点已经是 compaction，不能连续压缩")
	}
	preparation, ok := session.PrepareCompaction(entries, r.Settings.Compaction)
	if !ok {
		return "当前 session 没有可安全淘汰的历史，无需 compaction。", nil
	}
	result, err := session.Compact(ctx, r.Client, *preparation, extra)
	if err != nil {
		return "", err
	}
	if err := r.Session.AppendCompaction(result.Summary, result.FirstKeptEntryID, result.TokensBefore, result.Details); err != nil {
		return "", err
	}
	r.rebuildAgents()
	return fmt.Sprintf("已压缩会话上下文（压缩前约 %d tokens，保留边界 %s）。", result.TokensBefore, result.FirstKeptEntryID), nil
}

func (r *Runtime) refreshMCPTools() {
	for _, name := range r.mcpToolNames {
		r.Tools.Unregister(name)
	}
	r.mcpToolNames = nil
	before := map[string]bool{}
	for _, name := range r.Tools.ToolNames() {
		before[name] = true
	}
	mcp.RegisterTools(r.Tools, r.MCP)
	for _, name := range r.Tools.ToolNames() {
		if !before[name] && strings.HasPrefix(name, "mcp_") {
			r.mcpToolNames = append(r.mcpToolNames, name)
		}
	}
	sort.Strings(r.mcpToolNames)
}

func (r *Runtime) rebuildAgents() {
	additional := "当前工作目录: " + r.Workspace
	if catalog := strings.TrimSpace(r.Skills.CatalogPrompt()); catalog != "" {
		additional += "\n\n" + catalog
	}
	r.react = agent.New(r.Client, r.Tools, additional, r.Concurrent, r.Events)
	planRegistry := planning.NewToolRegistry(r.Tools, r.planStore, func() runtime.PlanState {
		return r.currentPlanState()
	})
	r.planning = agent.New(r.Client, planRegistry, planning.Prompt(additional), r.Concurrent, r.Events)
	beforeChat := func(messages []llm.Message) error {
		needed, tokens, threshold, err := r.compactionThresholdFor(messages)
		if err != nil {
			return fmt.Errorf("自动压缩阈值配置无效: %w", err)
		}
		if !needed {
			return nil
		}
		return fmt.Errorf("%w: 当前约 %d tokens，阈值 %d", errCompactionRequired, tokens, threshold)
	}
	r.react.BeforeChat = beforeChat
	r.planning.BeforeChat = beforeChat
}

func (r *Runtime) ModelOptions() []llm.ModelOption {
	if r.switchable == nil {
		return nil
	}
	return r.switchable.Options()
}

func (r *Runtime) CurrentModel() llm.ModelOption {
	if r.switchable != nil {
		return r.switchable.Current()
	}
	return llm.ModelOption{Provider: r.Client.ProviderName(), Model: r.Client.ModelName()}
}

func (r *Runtime) ReasoningEffort() string {
	if r.switchable != nil {
		return r.switchable.ReasoningEffort()
	}
	return ""
}

func (r *Runtime) SetReasoningEffort(level string) error {
	if r.switchable == nil {
		return errors.New("当前运行时不支持调整推理级别")
	}
	return r.switchable.SetReasoningEffort(level)
}

func (r *Runtime) MCPServerNames() []string {
	if r.MCP == nil {
		return nil
	}
	return r.MCP.Names()
}

func (r *Runtime) emit(evt event.Event) {
	if r != nil && r.Events != nil {
		r.Events.Emit(evt)
	}
}

func (r *Runtime) subscribeSessionRecorder() {
	if r == nil || r.Events == nil || r.Session == nil {
		return
	}
	r.Events.Subscribe(func(evt event.Event) {
		switch e := evt.(type) {
		case event.MessageCompleted:
			if e.Durable {
				if err := r.Session.AppendMessage(e.Message); err != nil {
					r.emit(event.NewActivity(e.RunID, "Session 写入失败: "+err.Error()))
				}
			}
		case event.ModeChanged:
			if err := r.Session.AppendModeChange(e.Mode); err != nil {
				r.emit(event.NewActivity(e.RunID, "Session 写入失败: "+err.Error()))
			}
		}
	})
}

func (r *Runtime) taskContext() string {
	var sections []string
	if loaded := strings.TrimSpace(instructions.Load(r.HomeDir, r.Workspace).Prompt); loaded != "" {
		sections = append(sections, "AGENTS 指令:\n"+loaded)
	}
	if active := strings.TrimSpace(r.Skills.ActiveInstructions()); active != "" {
		sections = append(sections, active)
	}
	return strings.Join(sections, "\n\n")
}

func (r *Runtime) taskContextWithPlan(base string) string {
	sections := []string{}
	if strings.TrimSpace(base) != "" {
		sections = append(sections, strings.TrimSpace(base))
	}
	state := r.currentPlanState()
	if state.Empty() {
		return strings.Join(sections, "\n\n")
	}
	content, _ := r.readPlanContent(state)
	switch {
	case r.Mode == runtime.ModePlan && state.Pending():
		sections = append(sections, fmt.Sprintf("当前待审批计划:\nPlan ID: %s\nRevision: %d\nPath: %s\n\n%s", state.ID, state.Revision, state.Path, content))
	case r.Mode == runtime.ModeReact && state.Approved():
		sections = append(sections, fmt.Sprintf("已批准计划执行上下文:\n用户输入 `/plan approve` 表示用户已经批准下方计划，并要求你立即按该计划执行项目修改。遇到该输入时，不要把它当作普通 slash command，也不要重新要求用户审批计划。\nPlan ID: %s\nRevision: %d\nPath: %s\n\n%s", state.ID, state.Revision, state.Path, content))
	}
	return strings.Join(sections, "\n\n")
}

func (r *Runtime) currentPlanState() runtime.PlanState {
	if r == nil || r.Session == nil {
		return runtime.PlanState{}
	}
	state := r.Session.Context(r.Mode).ActivePlan
	if r.planStore != nil {
		state = r.planStore.Recover(state)
	}
	return state
}

func (r *Runtime) presentPlan(runID, out string) (string, error) {
	content := strings.TrimSpace(out)
	state := r.currentPlanState()
	if !state.Empty() {
		if current, err := r.readPlanContent(state); err == nil && strings.TrimSpace(current) != "" {
			content = strings.TrimSpace(current)
		}
	}
	if content == "" {
		return out, nil
	}
	if state.Empty() {
		next, err := r.planStore.Replace(state, content, "Planning Agent 输出计划")
		if err != nil {
			return "", err
		}
		state = next
	}
	presented, err := r.planStore.Record(runtime.PlanActionPresented, state, content, "计划已展示")
	if err != nil {
		return "", err
	}
	r.emit(event.NewPlanEventRecorded(runID, planEventFromState(presented)))
	return content, nil
}

func planEventFromState(state runtime.PlanState) runtime.PlanEvent {
	return runtime.PlanEvent{
		ID:       state.ID,
		Path:     state.Path,
		Action:   state.Action,
		Revision: state.Revision,
		SHA256:   state.SHA256,
		Summary:  state.Summary,
		Content:  state.Content,
	}
}

func (r *Runtime) approvePlan(ctx context.Context, raw string) (string, error) {
	state := r.currentPlanState()
	if state.Empty() || !state.Pending() {
		return "", errors.New("当前没有待批准计划")
	}
	content, err := r.readPlanContent(state)
	if err != nil {
		return "", err
	}
	approved, err := r.planStore.Record(runtime.PlanActionApproved, state, content, "用户批准计划")
	if err != nil {
		return "", err
	}
	if err := r.setMode(runtime.ModeReact); err != nil {
		return "", err
	}
	if _, err := r.planStore.Record(runtime.PlanActionHandoff, approved, content, "交接给 ReAct 执行"); err != nil {
		return "", err
	}
	display := strings.TrimSpace(raw)
	if display == "" {
		display = "/plan approve"
	}
	return r.runTask(ctx, display, false)
}

func (r *Runtime) rejectPlan(action runtime.PlanAction, reason string) (string, error) {
	state := r.currentPlanState()
	if state.Empty() {
		return "", errors.New("当前没有 active plan")
	}
	content, _ := r.readPlanContent(state)
	summary := "用户取消计划"
	if action == runtime.PlanActionRejected {
		summary = "用户拒绝计划"
	}
	if strings.TrimSpace(reason) != "" {
		summary += ": " + strings.TrimSpace(reason)
	}
	if _, err := r.planStore.Record(action, state, content, summary); err != nil {
		return "", err
	}
	if err := r.setMode(runtime.ModeReact); err != nil {
		return "", err
	}
	if action == runtime.PlanActionRejected {
		return "已拒绝计划，未执行项目修改。", nil
	}
	return "已取消计划，未执行项目修改。", nil
}

func (r *Runtime) readPlanContent(state runtime.PlanState) (string, error) {
	if r.planStore == nil {
		if strings.TrimSpace(state.Content) == "" {
			return "", errors.New("plan store 未初始化")
		}
		return state.Content, nil
	}
	content, err := r.planStore.Read(state)
	if err != nil {
		if strings.TrimSpace(state.Content) != "" {
			return state.Content, nil
		}
		return "", err
	}
	return content, nil
}

func (r *Runtime) pendingPlanInputPrompt() (string, bool) {
	if r == nil || r.Session == nil {
		return "", false
	}
	state := r.Session.Context(r.Mode).ActivePlan
	if !state.Pending() {
		return "", false
	}
	var b strings.Builder
	b.WriteString("当前已有待审批计划。要开始实现，请输入 `/plan approve`。")
	if state.ID != "" {
		fmt.Fprintf(&b, "\nPlan: %s", state.ID)
		if state.Revision > 0 {
			fmt.Fprintf(&b, " rev=%d", state.Revision)
		}
		if strings.TrimSpace(state.Path) != "" {
			fmt.Fprintf(&b, " path=%s", state.Path)
		}
	}
	b.WriteString("\n如需调整计划，请使用 `/plan continue <反馈>`；如需放弃，请使用 `/plan reject` 或 `/plan cancel`。")
	return b.String(), true
}

func formatSearch(results []web.Result) string {
	if len(results) == 0 {
		return "没有搜索结果"
	}
	var b strings.Builder
	for i, result := range results {
		fmt.Fprintf(&b, "%d. %s\n%s\n%s\n\n", i+1, result.Title, result.URL, result.Snippet)
	}
	return strings.TrimSpace(b.String())
}

func onOff(v bool) string {
	if v {
		return "开启"
	}
	return "关闭"
}

func abs(path string) string {
	if path == "" {
		path = "."
	}
	out, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(out)
}
