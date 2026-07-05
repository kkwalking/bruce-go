package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"bruce-go/internal/llm"
	"bruce-go/internal/runtime"
	"bruce-go/internal/tool"
)

type TaskType string

const (
	TaskPlanning     TaskType = "PLANNING"
	TaskFileRead     TaskType = "FILE_READ"
	TaskFileWrite    TaskType = "FILE_WRITE"
	TaskCommand      TaskType = "COMMAND"
	TaskAnalysis     TaskType = "ANALYSIS"
	TaskVerification TaskType = "VERIFICATION"
)

type TaskStatus string

const (
	TaskPending   TaskStatus = "PENDING"
	TaskRunning   TaskStatus = "RUNNING"
	TaskCompleted TaskStatus = "COMPLETED"
	TaskFailed    TaskStatus = "FAILED"
	TaskSkipped   TaskStatus = "SKIPPED"
)

type PlanStatus string

const (
	PlanCreated   PlanStatus = "CREATED"
	PlanRunning   PlanStatus = "RUNNING"
	PlanCompleted PlanStatus = "COMPLETED"
	PlanFailed    PlanStatus = "FAILED"
)

type Task struct {
	ID           string     `json:"id"`
	Description  string     `json:"description"`
	Type         TaskType   `json:"type"`
	Dependencies []string   `json:"dependencies"`
	Path         string     `json:"path,omitempty"`
	Content      string     `json:"content,omitempty"`
	Command      string     `json:"command,omitempty"`
	Status       TaskStatus `json:"status,omitempty"`
	Result       string     `json:"result,omitempty"`
	Error        string     `json:"error,omitempty"`
}

type ExecutionPlan struct {
	ID             string
	Goal           string
	Summary        string
	Tasks          map[string]*Task
	ExecutionOrder []string
	Status         PlanStatus
}

func NewExecutionPlan(goal string) *ExecutionPlan {
	return &ExecutionPlan{ID: "plan_" + time.Now().UTC().Format("150405.000000000"), Goal: goal, Tasks: map[string]*Task{}, Status: PlanCreated}
}

func (p *ExecutionPlan) AddTask(task *Task) error {
	if task == nil || task.ID == "" {
		return errors.New("任务 id 不能为空")
	}
	if _, exists := p.Tasks[task.ID]; exists {
		return errors.New("重复任务 ID: " + task.ID)
	}
	if task.Status == "" {
		task.Status = TaskPending
	}
	p.Tasks[task.ID] = task
	return nil
}

func (p *ExecutionPlan) AddDependency(taskID, depID string) error {
	task := p.Tasks[taskID]
	if task == nil || p.Tasks[depID] == nil {
		return errors.New("依赖不存在")
	}
	for _, dep := range task.Dependencies {
		if dep == depID {
			return nil
		}
	}
	task.Dependencies = append(task.Dependencies, depID)
	return nil
}

func (p *ExecutionPlan) ValidateDAG() error {
	_, err := p.TopologicalOrder()
	return err
}

func (p *ExecutionPlan) TopologicalOrder() ([]*Task, error) {
	state := map[string]int{}
	var ordered []*Task
	var visit func(*Task) error
	visit = func(task *Task) error {
		switch state[task.ID] {
		case 1:
			return errors.New("DAG 中存在环，任务参与环路: " + task.ID)
		case 2:
			return nil
		}
		state[task.ID] = 1
		for _, depID := range task.Dependencies {
			dep := p.Tasks[depID]
			if dep == nil {
				return fmt.Errorf("任务 %s 依赖不存在的任务: %s", task.ID, depID)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[task.ID] = 2
		ordered = append(ordered, task)
		return nil
	}
	for _, task := range p.Tasks {
		if err := visit(task); err != nil {
			return nil, err
		}
	}
	p.ExecutionOrder = make([]string, 0, len(ordered))
	for _, task := range ordered {
		p.ExecutionOrder = append(p.ExecutionOrder, task.ID)
	}
	return ordered, nil
}

func (p *ExecutionPlan) ExecutableTasks() []*Task {
	var out []*Task
	for _, task := range p.Tasks {
		if task.Status != TaskPending {
			continue
		}
		ready := true
		for _, dep := range task.Dependencies {
			if p.Tasks[dep] == nil || p.Tasks[dep].Status != TaskCompleted {
				ready = false
				break
			}
		}
		if ready {
			out = append(out, task)
		}
	}
	return out
}

func (p *ExecutionPlan) SkipBlocked() {
	changed := true
	for changed {
		changed = false
		for _, task := range p.Tasks {
			if task.Status != TaskPending {
				continue
			}
			for _, dep := range task.Dependencies {
				if p.Tasks[dep] == nil || p.Tasks[dep].Status == TaskFailed || p.Tasks[dep].Status == TaskSkipped {
					task.Status = TaskSkipped
					task.Error = "依赖任务失败或被跳过"
					changed = true
					break
				}
			}
		}
	}
}

type Parser struct{}

func (Parser) Parse(raw, fallbackGoal string) (*ExecutionPlan, error) {
	text := extractJSONObject(raw)
	var root struct {
		Goal    string `json:"goal"`
		Summary string `json:"summary"`
		Tasks   []Task `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(text), &root); err != nil {
		return nil, err
	}
	if root.Goal == "" {
		root.Goal = fallbackGoal
	}
	if len(root.Tasks) == 0 {
		return nil, errors.New("计划 JSON 中必须包含非空 tasks 数组")
	}
	p := NewExecutionPlan(root.Goal)
	p.Summary = root.Summary
	idMap := map[string]string{}
	for i := range root.Tasks {
		task := root.Tasks[i]
		oldID := task.ID
		if oldID == "" {
			return nil, errors.New("任务缺少必填字段: id")
		}
		if !strings.HasPrefix(oldID, "task_") {
			task.ID = fmt.Sprintf("task_%d", i+1)
		}
		idMap[oldID] = task.ID
		task.Status = TaskPending
		task.Dependencies = nil
		if err := p.AddTask(&task); err != nil {
			return nil, err
		}
	}
	for _, task := range root.Tasks {
		for _, dep := range task.Dependencies {
			mapped := idMap[dep]
			if mapped == "" {
				return nil, errors.New("依赖不存在: " + dep)
			}
			if err := p.AddDependency(idMap[task.ID], mapped); err != nil {
				return nil, err
			}
		}
	}
	return p, p.ValidateDAG()
}

func extractJSONObject(raw string) string {
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "```") {
		if idx := strings.IndexByte(text, '\n'); idx >= 0 {
			text = text[idx+1:]
		}
		if idx := strings.LastIndex(text, "```"); idx >= 0 {
			text = text[:idx]
		}
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		return text[start : end+1]
	}
	return text
}

type Planner interface {
	Plan(ctx context.Context, goal, supplemental, taskContext string) (*ExecutionPlan, error)
	Replan(ctx context.Context, failed *ExecutionPlan, reason, supplemental, taskContext string) (*ExecutionPlan, error)
}

type LLMPlanner struct {
	Client llm.ChatClient
	Tools  *tool.Registry
	Parser Parser
}

func (p LLMPlanner) Plan(ctx context.Context, goal, supplemental, taskContext string) (*ExecutionPlan, error) {
	user := goal
	if supplemental != "" {
		user = "用户原始目标:\n" + goal + "\n\n可用补充上下文:\n" + supplemental
	}
	messages := []llm.Message{llm.System(planPrompt)}
	if taskContext != "" {
		messages = append(messages, llm.System(taskContext))
	}
	messages = append(messages, llm.User(user))
	var defs []llm.ToolDefinition
	if p.Tools != nil {
		defs = p.Tools.Definitions()
	}
	for i := 0; i < 5; i++ {
		resp, err := p.Client.Chat(ctx, messages, defs, llm.StreamOptions{})
		if err != nil {
			return nil, err
		}
		if !resp.HasToolCalls() {
			return p.Parser.Parse(resp.Content, goal)
		}
		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})
		for _, call := range resp.ToolCalls {
			result := "规划阶段不允许调用工具"
			if p.Tools != nil {
				result = p.Tools.ExecuteJSON(ctx, call.Function.Name, call.Function.Arguments)
			}
			messages = append(messages, llm.ToolMessage(call.ID, result))
		}
	}
	return nil, errors.New("规划器读取资源次数超过限制")
}

func (p LLMPlanner) Replan(ctx context.Context, failed *ExecutionPlan, reason, supplemental, taskContext string) (*ExecutionPlan, error) {
	return p.Plan(ctx, "请基于失败上下文重新规划任务。\n原目标: "+failed.Goal+"\n失败原因: "+reason, supplemental, taskContext)
}

type Executor struct {
	Tools  *tool.Registry
	Config runtime.ConcurrencyConfig
}

type Report struct {
	Plan      *ExecutionPlan
	StartedAt time.Time
	EndedAt   time.Time
}

func (r Report) Success() bool {
	return r.Plan != nil && r.Plan.Status == PlanCompleted
}

func (r Report) SuccessRate() float64 {
	if r.Plan == nil || len(r.Plan.Tasks) == 0 {
		return 0
	}
	done := 0
	for _, task := range r.Plan.Tasks {
		if task.Status == TaskCompleted {
			done++
		}
	}
	return float64(done) / float64(len(r.Plan.Tasks))
}

func (e Executor) Execute(ctx context.Context, p *ExecutionPlan) Report {
	start := time.Now()
	p.Status = PlanRunning
	_, _ = p.TopologicalOrder()
	for hasPending(p) {
		p.SkipBlocked()
		batch := p.ExecutableTasks()
		if len(batch) == 0 {
			for _, task := range p.Tasks {
				if task.Status == TaskPending {
					task.Status = TaskSkipped
					task.Error = "没有可执行依赖路径"
				}
			}
			break
		}
		e.executeBatch(ctx, batch)
	}
	p.Status = PlanCompleted
	for _, task := range p.Tasks {
		if task.Status != TaskCompleted {
			p.Status = PlanFailed
			break
		}
	}
	return Report{Plan: p, StartedAt: start, EndedAt: time.Now()}
}

func (e Executor) executeBatch(ctx context.Context, batch []*Task) {
	if len(batch) == 1 {
		e.executeOne(ctx, batch[0])
		return
	}
	config := e.Config.Normalize()
	sem := make(chan struct{}, config.ParallelismFor(len(batch)))
	var wg sync.WaitGroup
	for _, task := range batch {
		task := task
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			e.executeOne(ctx, task)
		}()
	}
	wg.Wait()
}

func (e Executor) executeOne(ctx context.Context, task *Task) {
	task.Status = TaskRunning
	var result string
	switch task.Type {
	case TaskPlanning, TaskAnalysis:
		result = "已完成: " + task.Description
	case TaskFileRead:
		result = e.Tools.Execute(ctx, "read_file", map[string]string{"path": task.Path})
	case TaskFileWrite:
		result = e.Tools.Execute(ctx, "write_file", map[string]string{"path": task.Path, "content": task.Content})
	case TaskCommand, TaskVerification:
		result = e.Tools.Execute(ctx, "execute_command", map[string]string{"command": task.Command})
	default:
		result = "工具执行失败: 未知任务类型"
	}
	task.Result = result
	if strings.HasPrefix(result, "[HITL] 操作已被跳过") {
		task.Status = TaskSkipped
		task.Error = result
	} else if strings.HasPrefix(result, "[HITL] 操作已被拒绝") || strings.HasPrefix(result, "工具执行失败") || strings.HasPrefix(result, "工具参数解析失败") || strings.HasPrefix(result, "命令被安全策略拒绝") {
		task.Status = TaskFailed
		task.Error = result
	} else {
		task.Status = TaskCompleted
	}
}

type Agent struct {
	Planner  Planner
	Executor Executor
}

func (a Agent) Run(ctx context.Context, goal, supplemental, taskContext string) (Report, error) {
	p, err := a.Planner.Plan(ctx, goal, supplemental, taskContext)
	if err != nil {
		return Report{}, err
	}
	report := a.Executor.Execute(ctx, p)
	if !report.Success() && report.SuccessRate() < 0.5 {
		replanned, err := a.Planner.Replan(ctx, report.Plan, "执行成功率低于 50%", supplemental, taskContext)
		if err != nil {
			return report, nil
		}
		return a.Executor.Execute(ctx, replanned), nil
	}
	return report, nil
}

func hasPending(p *ExecutionPlan) bool {
	for _, task := range p.Tasks {
		if task.Status == TaskPending {
			return true
		}
	}
	return false
}

const planPrompt = `你是 BruceCLI 的 Plan-and-Execute 规划器。
你的任务是把用户目标拆解为可执行 DAG 任务，只返回 JSON，不要返回 Markdown。
JSON 格式必须包含 goal、summary、tasks；任务 type 可为 PLANNING、FILE_READ、FILE_WRITE、COMMAND、ANALYSIS、VERIFICATION。`
