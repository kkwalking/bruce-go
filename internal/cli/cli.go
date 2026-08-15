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

// CompletionKind identifies dynamic slash-completion providers. Options with
// no Kind are static and may carry nested Options.
type CompletionKind string

const (
	CompletionStatic    CompletionKind = ""
	CompletionMCPServer CompletionKind = "mcp-server"
	CompletionSkillName CompletionKind = "skill-name"
)

// CommandOption is one static or dynamic candidate in a slash command's
// completion tree. A trailing space in Value means the candidate accepts
// another argument and Tab should leave the cursor ready for the next level.
type CommandOption struct {
	Value       string
	Description string
	Group       string
	Kind        CompletionKind
	Options     []CommandOption
}

type CommandInfo struct {
	Name        string
	Usage       string
	Description string
	// Complete is the value inserted by Tab while the command token is being
	// typed. It defaults to "/" + Name.
	Complete string
	Options  []CommandOption
}

var statusOptions = []CommandOption{
	{Value: "on", Description: "Enable", Group: "Status"},
	{Value: "off", Description: "Disable", Group: "Status"},
	{Value: "status", Description: "View status", Group: "Status"},
}

var Commands = []CommandInfo{
	{Name: "react", Usage: "/react", Description: "Switch to ReAct mode"},
	{
		Name: "plan", Usage: "/plan [task|approve|reject|cancel|continue]", Description: "Enter plan mode, revise a plan, or review a plan",
		Complete: "/plan ",
		Options: []CommandOption{
			{Value: "approve", Description: "Approve the current plan and begin execution", Group: "Plan"},
			{Value: "continue ", Description: "Continue planning with feedback", Group: "Plan"},
			{Value: "reject ", Description: "Reject the current plan", Group: "Plan"},
			{Value: "cancel", Description: "Cancel the current plan", Group: "Plan"},
		},
	},
	{
		Name: "model", Usage: "/model [provider/model | reasoning [off|low|medium|high|max]]", Description: "View or switch models and adjust reasoning effort",
		Complete: "/model ",
	},
	{
		Name: "web", Usage: "/web on|off|status|search <query>|fetch <url>", Description: "Enable, disable, inspect, or manually use WebSearch and WebFetch",
		Complete: "/web ",
		Options: []CommandOption{
			{Value: "on", Description: "Enable Web tools", Group: "Web"},
			{Value: "off", Description: "Disable Web tools", Group: "Web"},
			{Value: "status", Description: "View status", Group: "Web"},
			{Value: "search ", Description: "Search the web", Group: "Web"},
			{Value: "fetch ", Description: "Fetch web-page content", Group: "Web"},
		},
	},
	{
		Name: "mcp", Usage: "/mcp [restart|logs|disable|enable <name>]", Description: "View or manage MCP servers",
		Complete: "/mcp ",
		Options: []CommandOption{
			{Value: "status", Description: "View status", Group: "MCP"},
			{Value: "restart ", Description: "Restart a server", Group: "MCP", Kind: CompletionMCPServer},
			{Value: "logs ", Description: "View logs", Group: "MCP", Kind: CompletionMCPServer},
			{Value: "disable ", Description: "Disable a server", Group: "MCP", Kind: CompletionMCPServer},
			{Value: "enable ", Description: "Enable a server", Group: "MCP", Kind: CompletionMCPServer},
		},
	},
	{
		Name: "skill", Usage: "/skill list|show <name>|reload", Description: "List, inspect, or reload Skills",
		Complete: "/skill ",
		Options: []CommandOption{
			{Value: "list", Description: "List Skills", Group: "Skill"},
			{Value: "show ", Description: "Inspect a Skill", Group: "Skill", Kind: CompletionSkillName},
			{Value: "reload", Description: "Rescan Skills", Group: "Skill"},
		},
	},
	{Name: "hitl", Usage: "/hitl on|off|status", Description: "Enable, disable, or inspect human approval", Complete: "/hitl ", Options: statusOptions},
	{
		Name: "sandbox", Usage: "/sandbox [status|mode <mode>|network on|off]", Description: "View or change the command sandbox",
		Complete: "/sandbox ",
		Options: []CommandOption{
			{Value: "status", Description: "View sandbox status", Group: "Sandbox"},
			{
				Value: "mode ", Description: "Change filesystem permission mode", Group: "Sandbox",
				Options: []CommandOption{
					{Value: "read-only", Description: "Read-only workspace", Group: "Sandbox mode"},
					{Value: "workspace-write", Description: "Allow writes only within the workspace", Group: "Sandbox mode"},
					{Value: "full-access", Description: "Disable native shell sandboxing", Group: "Sandbox mode"},
				},
			},
			{
				Value: "network ", Description: "Change command network access", Group: "Sandbox",
				Options: []CommandOption{
					{Value: "on", Description: "Allow commands to access the network", Group: "Sandbox network"},
					{Value: "off", Description: "Prevent commands from accessing the network", Group: "Sandbox network"},
				},
			},
		},
	},
	{Name: "parallel", Usage: "/parallel on|off|status", Description: "Enable, disable, or inspect parallel tool calls", Complete: "/parallel ", Options: statusOptions},
	{Name: "status", Usage: "/status", Description: "View unified runtime status"},
	{Name: "session", Usage: "/session", Description: "View the current session"},
	{Name: "sessions", Usage: "/sessions", Description: "List sessions for the current working directory"},
	{Name: "new", Usage: "/new", Description: "Create a new session"},
	{Name: "resume", Usage: "/resume <id|path>", Description: "Resume a session", Complete: "/resume "},
	{Name: "tree", Usage: "/tree [entryId]", Description: "View or select a session-tree node", Complete: "/tree "},
	{Name: "compact", Usage: "/compact [instructions]", Description: "Compact earlier session history", Complete: "/compact "},
	{Name: "clear", Usage: "/clear", Description: "Start a new session and clear current state"},
	{Name: "help", Usage: "/help", Description: "Show help"},
	{Name: "exit", Usage: "/exit", Description: "Exit the program"},
}

// CompletionValue returns the value inserted by Tab while the command token is
// still being typed.
func (c CommandInfo) CompletionValue() string {
	if c.Complete != "" {
		return c.Complete
	}
	return "/" + c.Name
}

// FindCommand resolves a command token case-insensitively.
func FindCommand(name string) (CommandInfo, bool) {
	for _, command := range Commands {
		if strings.EqualFold(command.Name, name) {
			return command, true
		}
	}
	return CommandInfo{}, false
}

func Parse(input string) (Command, bool) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return Command{}, false
	}
	if strings.EqualFold(raw, "exit") || strings.EqualFold(raw, "quit") {
		return Command{Name: "exit", Raw: raw}, true
	}
	if !strings.HasPrefix(raw, "/") {
		return Command{}, false
	}
	parts := strings.Fields(raw)
	name := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
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
	_, ok := FindCommand(strings.ToLower(name))
	return ok
}
