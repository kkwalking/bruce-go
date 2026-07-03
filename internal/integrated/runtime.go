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
	"bruce-go/internal/plan"
	"bruce-go/internal/render"
	"bruce-go/internal/runtime"
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

	react      *agent.Agent
	plan       plan.Agent
	startMu    sync.Mutex
	mcpStarted bool

	mcpToolNames []string
}

type modelSwitcher interface {
	llm.ChatClient
	Options() []llm.ModelOption
	Current() llm.ModelOption
	Switch(selector string) (llm.ModelOption, error)
}

func New(ctx context.Context, opts Options) (*Runtime, error) {
	workspace := abs(opts.Workspace)
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

	hitl := approval.NewAutoHandler(false, approval.Approve())
	concurrency := runtime.DefaultConcurrency()
	registry := tool.NewRegistry(workspace).WithHITL(hitl).WithConcurrency(concurrency)
	webManager := web.NewManager(settings.WebSearch, nil)
	web.RegisterTools(registry, webManager)
	skills := skill.NewCatalog(home, workspace)
	skill.RegisterTools(registry, skills)
	mcpManager := mcp.NewManager(settings.MCP, workspace)

	store, err := session.CreateNew(home, workspace, runtime.ModeReact)
	if err != nil {
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
	}
	r.subscribeSessionRecorder()
	r.refreshMCPTools()
	r.rebuildAgents()
	return r, nil
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
	current := r.Session.Context(r.Mode)
	if r.Mode == runtime.ModePlan {
		r.emit(event.NewMessageCompleted(runID, prepared.Message, true))
		report, err := r.plan.Run(ctx, prepared.Text, "", taskContext)
		if err != nil {
			r.emit(event.NewRunFailed(runID, err.Error()))
			return "", err
		}
		out := formatPlanReport(report)
		r.emit(event.NewMessageCompleted(runID, llm.Assistant(out), true))
		r.emit(event.NewRunCompleted(runID, out))
		return out, nil
	}
	r.react.RestoreHistory(current.Messages)
	out, err := r.react.Run(ctx, prepared, taskContext, runID)
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
		result.Err = r.setMode(runtime.ModePlan)
		result.Output = "已切换到 Plan-and-Execute 模式"
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
		result.Output, result.Err = r.compact(strings.Join(command.Args, " "))
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
	mcpSummary := render.MCP(r.MCP.Status())
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
		ParallelEnabled:   r.Parallel,
		MaxParallelism:    r.Concurrent.MaxParallelism,
		BatchTimeout:      r.Concurrent.BatchTimeout,
		RAGIndexed:        false,
		SkillCount:        len(r.Skills.Skills()),
		ToolNames:         toolNames,
	}
}

func (r *Runtime) handleModel(args []string) (string, error) {
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
		return strings.TrimSpace(b.String()), nil
	}
	next, err := r.switchable.Switch(strings.Join(args, " "))
	if err != nil {
		return "", err
	}
	r.Client = r.switchable
	r.rebuildAgents()
	return "已切换模型: " + next.Selector(), nil
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
			r.Concurrent = runtime.DefaultConcurrency()
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

func (r *Runtime) compact(extra string) (string, error) {
	entries := r.Session.ActiveEntries()
	if len(entries) == 0 {
		return "当前 session 没有可压缩的历史。", nil
	}
	firstKept := entries[len(entries)-1].ID
	if len(entries) >= 2 {
		firstKept = entries[len(entries)-2].ID
	}
	summary := summarizeEntries(entries, extra)
	if err := r.Session.AppendCompaction(summary, firstKept, len(summary), map[string]string{"instructions": extra}); err != nil {
		return "", err
	}
	return "已追加 compaction 摘要节点。", nil
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
	additional := strings.TrimSpace(r.Skills.CatalogPrompt())
	r.react = agent.New(r.Client, r.Tools, additional, r.Concurrent, r.Events)
	planner := plan.LLMPlanner{Client: r.Client, Tools: r.Tools, Parser: plan.Parser{}}
	r.plan = plan.Agent{Planner: planner, Executor: plan.Executor{Tools: r.Tools, Config: r.Concurrent}}
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

func formatPlanReport(report plan.Report) string {
	if report.Plan == nil {
		return "计划执行失败"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Plan: %s\nStatus: %s\n", report.Plan.ID, report.Plan.Status)
	ordered, _ := report.Plan.TopologicalOrder()
	for _, task := range ordered {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", task.Status, task.ID, task.Description)
		if task.Error != "" {
			b.WriteString("  " + task.Error + "\n")
		}
	}
	return strings.TrimSpace(b.String())
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

func summarizeEntries(entries []session.Entry, extra string) string {
	var b strings.Builder
	b.WriteString("会话历史摘要")
	if strings.TrimSpace(extra) != "" {
		b.WriteString("（聚焦: " + strings.TrimSpace(extra) + "）")
	}
	b.WriteString(":\n")
	for i, entry := range entries {
		if i >= 12 {
			b.WriteString("- ... 更早历史省略 ...\n")
			break
		}
		if entry.Message == nil {
			continue
		}
		content := strings.TrimSpace(entry.Message.Content)
		if len(content) > 240 {
			content = content[:240] + "..."
		}
		fmt.Fprintf(&b, "- %s: %s\n", entry.Message.Role, content)
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
