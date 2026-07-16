package runtime

import (
	"strings"
	"time"
)

type AgentMode string

const (
	ModeReact AgentMode = "REACT"
	ModePlan  AgentMode = "PLAN"
)

type PlanAction string

const (
	PlanActionCreated   PlanAction = "created"
	PlanActionUpdated   PlanAction = "updated"
	PlanActionPresented PlanAction = "presented"
	PlanActionApproved  PlanAction = "approved"
	PlanActionRejected  PlanAction = "rejected"
	PlanActionCanceled  PlanAction = "canceled"
	PlanActionHandoff   PlanAction = "handoff"
)

type PlanEvent struct {
	ID       string     `json:"id"`
	Path     string     `json:"path,omitempty"`
	Action   PlanAction `json:"action"`
	Revision int        `json:"revision,omitempty"`
	SHA256   string     `json:"sha256,omitempty"`
	Summary  string     `json:"summary,omitempty"`
	Content  string     `json:"content,omitempty"`
}

type PlanState struct {
	ID                    string     `json:"id,omitempty"`
	Path                  string     `json:"path,omitempty"`
	Action                PlanAction `json:"action,omitempty"`
	Revision              int        `json:"revision,omitempty"`
	SHA256                string     `json:"sha256,omitempty"`
	Summary               string     `json:"summary,omitempty"`
	Content               string     `json:"content,omitempty"`
	MissingFile           bool       `json:"missingFile,omitempty"`
	HashMismatch          bool       `json:"hashMismatch,omitempty"`
	RecoveredFromSnapshot bool       `json:"recoveredFromSnapshot,omitempty"`
}

func (s PlanState) Empty() bool {
	return strings.TrimSpace(s.ID) == ""
}

func (s PlanState) Pending() bool {
	switch s.Action {
	case PlanActionCreated, PlanActionUpdated, PlanActionPresented:
		return strings.TrimSpace(s.ID) != ""
	default:
		return false
	}
}

func (s PlanState) Approved() bool {
	return strings.TrimSpace(s.ID) != "" && (s.Action == PlanActionApproved || s.Action == PlanActionHandoff)
}

func (s PlanState) Terminal() bool {
	switch s.Action {
	case PlanActionApproved, PlanActionRejected, PlanActionCanceled, PlanActionHandoff:
		return strings.TrimSpace(s.ID) != ""
	default:
		return false
	}
}

type ConcurrencyConfig struct {
	MaxParallelism int
	BatchTimeout   time.Duration
	MaxOutputChars int
}

func DefaultConcurrency() ConcurrencyConfig {
	return ConcurrencyConfig{MaxParallelism: 4, BatchTimeout: 30 * time.Second, MaxOutputChars: 8000}
}

func (c ConcurrencyConfig) Normalize() ConcurrencyConfig {
	if c.MaxParallelism <= 0 {
		c.MaxParallelism = DefaultConcurrency().MaxParallelism
	}
	if c.BatchTimeout <= 0 {
		c.BatchTimeout = DefaultConcurrency().BatchTimeout
	}
	if c.MaxOutputChars <= 0 {
		c.MaxOutputChars = DefaultConcurrency().MaxOutputChars
	}
	return c
}

func (c ConcurrencyConfig) ParallelismFor(n int) int {
	c = c.Normalize()
	if n <= 1 {
		return 1
	}
	if n < c.MaxParallelism {
		return n
	}
	return c.MaxParallelism
}

func (c ConcurrencyConfig) Truncate(text string) string {
	c = c.Normalize()
	if len(text) <= c.MaxOutputChars {
		return text
	}
	return text[:c.MaxOutputChars] + "\n... 输出过长，已截断 ..."
}

type Status struct {
	Mode              AgentMode
	Model             string
	Provider          string
	WorkspaceRoot     string
	RAGEnabled        bool
	WebEnabled        bool
	WebSearchProvider string
	MCPSummary        string
	HITLEnabled       bool
	SandboxMode       string
	SandboxBackend    string
	SandboxNetwork    bool
	SandboxAvailable  bool
	SandboxReason     string
	ParallelEnabled   bool
	MaxParallelism    int
	BatchTimeout      time.Duration
	RAGIndexed        bool
	SkillCount        int
	ToolNames         []string
	ActivePlan        PlanState
}

func (s Status) DisplayString() string {
	tools := make([]string, 0, len(s.ToolNames))
	for _, name := range s.ToolNames {
		if strings.HasPrefix(name, "mcp__") || name == "load_skill" || name == "read_skill_resource" {
			continue
		}
		tools = append(tools, name)
	}
	status := strings.TrimSpace(
		"当前模式: " + string(s.Mode) + "\n" +
			"当前模型: " + empty(s.Model, "unknown") + " [" + empty(s.Provider, "unknown") + "]\n" +
			"工作目录: " + s.WorkspaceRoot + "\n" +
			"RAG: 关闭\n" +
			"RAG 索引: 未建立\n" +
			"Web: " + onOff(s.WebEnabled) + " (provider=" + empty(s.WebSearchProvider, "unknown") + ")\n" +
			"MCP: " + empty(s.MCPSummary, "未配置") + "\n" +
			"HITL: " + onOff(s.HITLEnabled) + "\n" +
			"Sandbox: " + empty(s.SandboxMode, "unknown") + " (backend=" + empty(s.SandboxBackend, "unknown") + ", network=" + onOff(s.SandboxNetwork) + ", available=" + onOff(s.SandboxAvailable) + ")\n" +
			"Parallel: " + onOff(s.ParallelEnabled) + "\n" +
			"Skills: " + itoa(s.SkillCount) + " 个\n" +
			"Tools: " + strings.Join(tools, ", "),
	)
	if !s.SandboxAvailable && strings.TrimSpace(s.SandboxReason) != "" {
		status += "\nSandbox reason: " + strings.TrimSpace(s.SandboxReason)
	}
	if s.ActivePlan.Pending() {
		status += "\nPending Plan: " + s.ActivePlan.ID + " rev=" + itoa(s.ActivePlan.Revision) + " path=" + s.ActivePlan.Path
	}
	return status
}

func onOff(v bool) string {
	if v {
		return "开启"
	}
	return "关闭"
}

func empty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.TrimSpace(v)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
