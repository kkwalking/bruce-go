package render

import (
	"fmt"
	"strings"

	"bruce-go/internal/mcp"
	"bruce-go/internal/runtime"
	"bruce-go/internal/session"
	"bruce-go/internal/skill"
)

func Status(status runtime.Status) string {
	return status.DisplayString()
}

func Session(ctx session.Context) string {
	out := strings.TrimSpace(fmt.Sprintf(`Session: %s
File: %s
Mode: %s
Active leaf: %s
Messages: %d`, ctx.SessionID, ctx.File, ctx.Mode, empty(ctx.ActiveLeaf, "(none)"), ctx.MessageCount))
	if !ctx.ActivePlan.Empty() {
		out += fmt.Sprintf("\nPlan: %s action=%s rev=%d path=%s", ctx.ActivePlan.ID, ctx.ActivePlan.Action, ctx.ActivePlan.Revision, ctx.ActivePlan.Path)
	}
	return out
}

func Sessions(summaries []session.Summary) string {
	if len(summaries) == 0 {
		return "当前工作目录没有可恢复的 session。"
	}
	var b strings.Builder
	for _, summary := range summaries {
		plan := ""
		if !summary.ActivePlan.Empty() {
			plan = fmt.Sprintf("  plan=%s/%s", summary.ActivePlan.ID, summary.ActivePlan.Action)
		}
		fmt.Fprintf(&b, "%s  %s  mode=%s  messages=%d%s\n", summary.ID, summary.UpdatedAt.Format("2006-01-02 15:04:05"), summary.Mode, summary.MessageCount, plan)
	}
	return strings.TrimSpace(b.String())
}

func Skills(skills []skill.Definition, diagnostics, overrides []string) string {
	var b strings.Builder
	if len(skills) == 0 {
		b.WriteString("未发现 Skill。")
	} else {
		for _, def := range skills {
			fmt.Fprintf(&b, "- %s [%s]: %s\n", def.Name, def.Source, def.Description)
		}
	}
	if len(overrides) > 0 {
		b.WriteString("\nOverrides:\n")
		for _, item := range overrides {
			b.WriteString("- " + item + "\n")
		}
	}
	if len(diagnostics) > 0 {
		b.WriteString("\nDiagnostics:\n")
		for _, item := range diagnostics {
			b.WriteString("- " + item + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func Skill(def skill.Definition) string {
	return strings.TrimSpace(fmt.Sprintf(`# %s

Source: %s
File: %s
Description: %s

%s`, def.Name, def.Source, def.File, def.Description, def.Instructions))
}

func MCP(statuses []mcp.ServerStatus) string {
	if len(statuses) == 0 {
		return "未配置 MCP server。"
	}
	var b strings.Builder
	for _, s := range statuses {
		ready := "not-ready"
		if s.Ready {
			ready = "ready"
		}
		enabled := "disabled"
		if s.Enabled {
			enabled = "enabled"
		}
		fmt.Fprintf(&b, "- %s: %s, %s, tools=%d", s.Name, enabled, ready, s.ToolCount)
		if s.Error != "" {
			b.WriteString(", error=" + s.Error)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func Lines(lines []string) string {
	if len(lines) == 0 {
		return "(empty)"
	}
	return strings.Join(lines, "\n")
}

func empty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
