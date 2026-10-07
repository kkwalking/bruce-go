package render

import (
	"fmt"
	"strings"

	"bruce-go/internal/mcp"
	"bruce-go/internal/plugin"
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
	if ctx.Task.ID != "" {
		out += fmt.Sprintf("\nTask: %s status=%s phase=%s\nObjective: %s", ctx.Task.ID, ctx.Task.Status, ctx.Task.Phase, ctx.Task.Objective)
	}
	return out
}

func Sessions(summaries []session.Summary) string {
	if len(summaries) == 0 {
		return "There are no resumable sessions in the current working directory."
	}
	var b strings.Builder
	for _, summary := range summaries {
		plan := ""
		if !summary.ActivePlan.Empty() {
			plan = fmt.Sprintf("  plan=%s/%s", summary.ActivePlan.ID, summary.ActivePlan.Action)
		}
		if summary.Task.ID != "" {
			plan += fmt.Sprintf("  task=%s", summary.Task.Status)
		}
		fmt.Fprintf(&b, "%s  %s  mode=%s  messages=%d%s\n", summary.ID, summary.UpdatedAt.Format("2006-01-02 15:04:05"), summary.Mode, summary.MessageCount, plan)
	}
	return strings.TrimSpace(b.String())
}

func Skills(skills []skill.Definition, diagnostics, overrides []string) string {
	var b strings.Builder
	if len(skills) == 0 {
		b.WriteString("No Skills found.")
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
		return "No MCP servers configured."
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
		fmt.Fprintf(&b, "- %s: %s, %s, transport=%s, enforcement=%s, generation=%d, tools=%d, blocked=%d",
			s.Name, enabled, ready, s.Transport, s.Enforcement, s.Generation, s.ToolCount, s.BlockedToolCount)
		if s.BlockedReason != "" {
			b.WriteString(", policy=" + s.BlockedReason)
		}
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

// Plugins renders the plugin list, diagnostics, overrides and command
// conflicts.
//
// The layout puts the security-relevant information first: what each plugin was
// granted, and what failed to load. A plugin's granted permission set is shown
// even when it is empty, because "this plugin can do nothing" is exactly what
// the user needs to know after installing it.
func Plugins(plugins []plugin.PluginStatus, diagnostics []plugin.Diagnostic, overrides, conflicts []string) string {
	var b strings.Builder
	if len(plugins) == 0 {
		b.WriteString("No JavaScript plugins are loaded.\n")
		b.WriteString("Plugin directories:\n")
		b.WriteString("- workspace: .bruce/plugins/<name>/plugin.json\n")
		b.WriteString("- user:      ~/.bruce/plugins/<name>/plugin.json\n")
	} else {
		for _, status := range plugins {
			fmt.Fprintf(&b, "- %s %s [%s]\n", status.Name, status.Version, status.Source)
			fmt.Fprintf(&b, "    %s\n", status.Description)
			fmt.Fprintf(&b, "    granted: %s\n", empty(status.Granted, "(no permissions)"))
			fmt.Fprintf(&b, "    generation: %d, runtimes: %d/%d idle\n",
				status.Generation, status.Runtimes.Idle, status.Runtimes.Capacity)
			if len(status.Tools) > 0 {
				fmt.Fprintf(&b, "    tools: %s\n", strings.Join(status.Tools, ", "))
			}
			if len(status.Hooks) > 0 {
				fmt.Fprintf(&b, "    hooks: %s\n", strings.Join(status.Hooks, ", "))
			}
			if len(status.Commands) > 0 {
				names := make([]string, 0, len(status.Commands))
				for _, name := range status.Commands {
					names = append(names, "/"+name)
				}
				fmt.Fprintf(&b, "    commands: %s\n", strings.Join(names, ", "))
			}
		}
	}
	if len(overrides) > 0 {
		b.WriteString("\nOverrides:\n")
		for _, item := range overrides {
			b.WriteString("- " + item + "\n")
		}
	}
	if len(conflicts) > 0 {
		b.WriteString("\nCommand conflicts (the existing command was kept):\n")
		for _, item := range conflicts {
			b.WriteString("- " + item + "\n")
		}
	}
	if len(diagnostics) > 0 {
		b.WriteString("\nDiagnostics:\n")
		for _, item := range diagnostics {
			b.WriteString("- " + item.String() + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// Plugin renders one plugin in detail.
func Plugin(status plugin.PluginStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plugin: %s %s\n", status.Name, status.Version)
	fmt.Fprintf(&b, "Source: %s\n", status.Source)
	fmt.Fprintf(&b, "Directory: %s\n", status.RootDir)
	fmt.Fprintf(&b, "Description: %s\n", status.Description)
	fmt.Fprintf(&b, "Generation: %d\n", status.Generation)
	fmt.Fprintf(&b, "Granted permissions: %s\n", empty(status.Granted, "(none)"))
	fmt.Fprintf(&b, "Runtimes: %d idle of %d\n", status.Runtimes.Idle, status.Runtimes.Capacity)
	if len(status.Tools) > 0 {
		fmt.Fprintf(&b, "Tools: %s\n", strings.Join(status.Tools, ", "))
	}
	if len(status.Hooks) > 0 {
		fmt.Fprintf(&b, "Hooks: %s\n", strings.Join(status.Hooks, ", "))
	}
	if len(status.Commands) > 0 {
		fmt.Fprintf(&b, "Commands: %s\n", strings.Join(status.Commands, ", "))
	}
	return strings.TrimSpace(b.String())
}

// PluginHooks renders the registered hooks in execution order.
func PluginHooks(bindings []plugin.HookBinding) string {
	if len(bindings) == 0 {
		return "No plugin hooks are registered."
	}
	var b strings.Builder
	b.WriteString("Plugin hooks, in execution order:\n")
	for _, binding := range bindings {
		kind := "interceptor"
		if binding.Observer {
			kind = "observer"
		}
		fmt.Fprintf(&b, "- %s: %s.%s (%s)\n", binding.Event, binding.Plugin, binding.Handler, kind)
	}
	return strings.TrimSpace(b.String())
}
