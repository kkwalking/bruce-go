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
	ParallelEnabled   bool
	MaxParallelism    int
	BatchTimeout      time.Duration
	RAGIndexed        bool
	SkillCount        int
	ToolNames         []string
}

func (s Status) DisplayString() string {
	tools := make([]string, 0, len(s.ToolNames))
	for _, name := range s.ToolNames {
		if strings.HasPrefix(name, "mcp__") || name == "load_skill" || name == "read_skill_resource" {
			continue
		}
		tools = append(tools, name)
	}
	return strings.TrimSpace(
		"当前模式: " + string(s.Mode) + "\n" +
			"当前模型: " + empty(s.Model, "unknown") + " [" + empty(s.Provider, "unknown") + "]\n" +
			"工作目录: " + s.WorkspaceRoot + "\n" +
			"RAG: 关闭\n" +
			"RAG 索引: 未建立\n" +
			"Web: " + onOff(s.WebEnabled) + " (provider=" + empty(s.WebSearchProvider, "unknown") + ")\n" +
			"MCP: " + empty(s.MCPSummary, "未配置") + "\n" +
			"HITL: " + onOff(s.HITLEnabled) + "\n" +
			"Parallel: " + onOff(s.ParallelEnabled) + "\n" +
			"Skills: " + itoa(s.SkillCount) + " 个\n" +
			"Tools: " + strings.Join(tools, ", "),
	)
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
