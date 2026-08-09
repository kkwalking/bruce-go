package cli

import (
	"strings"
)

type Command struct {
	Name string
	Args []string
	Raw  string
}

type Result struct {
	Handled bool
	Exit    bool
	Output  string
	Err     error
}

type CommandInfo struct {
	Name        string
	Usage       string
	Description string
}

var Commands = []CommandInfo{
	{Name: "react", Usage: "/react", Description: "Switch to ReAct mode"},
	{Name: "plan", Usage: "/plan [task|approve|reject|cancel|continue]", Description: "Enter plan mode, revise a plan, or review a plan"},
	{Name: "model", Usage: "/model [provider/model | reasoning [off|low|medium|high|max]]", Description: "View or switch models and adjust reasoning effort"},
	{Name: "web", Usage: "/web on|off|status|search <query>|fetch <url>", Description: "Enable, disable, inspect, or manually use WebSearch and WebFetch"},
	{Name: "mcp", Usage: "/mcp [restart|logs|disable|enable <name>]", Description: "View or manage MCP servers"},
	{Name: "skill", Usage: "/skill list|show <name>|reload", Description: "List, inspect, or reload Skills"},
	{Name: "hitl", Usage: "/hitl on|off|status", Description: "Enable, disable, or inspect human approval"},
	{Name: "sandbox", Usage: "/sandbox [status|mode <mode>|network on|off]", Description: "View or change the command sandbox"},
	{Name: "parallel", Usage: "/parallel on|off|status", Description: "Enable, disable, or inspect parallel tool calls"},
	{Name: "status", Usage: "/status", Description: "View unified runtime status"},
	{Name: "session", Usage: "/session", Description: "View the current session"},
	{Name: "sessions", Usage: "/sessions", Description: "List sessions for the current working directory"},
	{Name: "new", Usage: "/new", Description: "Create a new session"},
	{Name: "resume", Usage: "/resume <id|path>", Description: "Resume a session"},
	{Name: "tree", Usage: "/tree [entryId]", Description: "View or select a session-tree node"},
	{Name: "compact", Usage: "/compact [instructions]", Description: "Compact earlier session history"},
	{Name: "clear", Usage: "/clear", Description: "Start a new session and clear current state"},
	{Name: "help", Usage: "/help", Description: "Show help"},
	{Name: "exit", Usage: "/exit", Description: "Exit the program"},
}

func Parse(input string) (Command, bool) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return Command{}, false
	}
	if raw == "exit" || raw == "quit" {
		return Command{Name: "exit", Raw: raw}, true
	}
	if !strings.HasPrefix(raw, "/") {
		return Command{}, false
	}
	parts := strings.Fields(raw)
	name := strings.TrimPrefix(parts[0], "/")
	var args []string
	if len(parts) > 1 {
		args = parts[1:]
	}
	return Command{Name: name, Args: args, Raw: raw}, true
}

func Help() string {
	var b strings.Builder
	b.WriteString("Available Bruce Go commands:\n\n")
	for _, cmd := range Commands {
		b.WriteString(cmd.Usage)
		if len(cmd.Usage) < 28 {
			b.WriteString(strings.Repeat(" ", 28-len(cmd.Usage)))
		} else {
			b.WriteString("  ")
		}
		b.WriteString(cmd.Description)
		b.WriteByte('\n')
	}
	b.WriteString("\nInput syntax:\n")
	b.WriteString("$<skill> <task>                 Explicitly load up to three Skills\n")
	b.WriteString("@image:<path>                   Attach an image file\n")
	b.WriteString("@image:<file:///path with space> Attach a file:// image\n")
	b.WriteString("@clipboard                      Attach an image from the macOS clipboard\n")
	return strings.TrimSpace(b.String())
}

func IsKnown(name string) bool {
	for _, cmd := range Commands {
		if cmd.Name == name {
			return true
		}
	}
	return false
}
