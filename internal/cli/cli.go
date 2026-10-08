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
	CompletionProvider  CompletionKind = "provider"
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
	// Source records who contributed the command: "builtin" or "plugin".
	Source string
	// Plugin is the owning plugin name, empty for a built-in command.
	Plugin string
	// Builtin marks a command the host itself owns. A plugin may never
	// replace one.
	Builtin bool
}

var statusOptions = []CommandOption{
	{Value: "on", Description: "Enable", Group: "Status"},
	{Value: "off", Description: "Disable", Group: "Status"},
	{Value: "status", Description: "View status", Group: "Status"},
}

var Commands = []CommandInfo{
	{Name: "react", Usage: "/react", Description: "Switch to ReAct mode"},
	{Name: "minimal", Usage: "/minimal", Description: "Switch to Minimal mode with a fresh session: fixed prompt and basic shell and file tools"},
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
		Name: "provider", Usage: "/provider [add|edit <name>|remove <name>|list]", Description: "View or manage LLM providers (add and edit open the configuration wizard)",
		Complete: "/provider ",
		Options: []CommandOption{
			{Value: "add", Description: "Configure a new provider", Group: "Provider"},
			{Value: "edit ", Description: "Edit an existing provider", Group: "Provider", Kind: CompletionProvider},
			{Value: "remove ", Description: "Delete a provider", Group: "Provider", Kind: CompletionProvider},
			{Value: "list", Description: "List configured providers", Group: "Provider"},
		},
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
	{Name: "resume", Usage: "/resume [id|path] [--continue] [--accept-changes]", Description: "Restore a session or continue its unfinished task", Complete: "/resume ", Options: []CommandOption{
		{Value: "--continue ", Description: "Continue the unfinished task", Group: "Resume", Options: []CommandOption{{Value: "--accept-changes", Description: "Acknowledge workspace changes", Group: "Resume"}}},
	}},
	{Name: "checkpoint", Usage: "/checkpoint", Description: "Inspect task progress and workspace changes"},
	{Name: "tree", Usage: "/tree [entryId]", Description: "View or select a session-tree node", Complete: "/tree "},
	{Name: "compact", Usage: "/compact [instructions]", Description: "Compact earlier session history", Complete: "/compact "},
	{Name: "plugin", Usage: "/plugin [list|info <name>|reload [name]|unload <name>|hooks]", Description: "List, inspect, reload, or unload JavaScript plugins", Complete: "/plugin ", Options: []CommandOption{
		{Value: "list", Description: "List loaded plugins", Group: "Plugin"},
		{Value: "info ", Description: "Inspect a plugin", Group: "Plugin"},
		{Value: "reload", Description: "Reload every plugin", Group: "Plugin"},
		{Value: "reload ", Description: "Reload one plugin", Group: "Plugin"},
		{Value: "unload ", Description: "Unload a plugin", Group: "Plugin"},
		{Value: "hooks", Description: "List registered hooks", Group: "Plugin"},
	}},
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

// defaultRegistry holds the built-in commands. Callers that need plugin
// commands use a Registry instead; this exists so existing call sites keep
// working unchanged.
var defaultRegistry = NewRegistry()

// FindCommand resolves a command token case-insensitively against the built-in
// command table.
func FindCommand(name string) (CommandInfo, bool) {
	return defaultRegistry.Find(name)
}

// FindCommandIn resolves a command token against a registry that may also hold
// plugin commands.
func FindCommandIn(registry *Registry, name string) (CommandInfo, bool) {
	if registry == nil {
		return FindCommand(name)
	}
	return registry.Find(name)
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

// ParseInvocation splits a slash command into its name, arguments and the raw
// input, without checking whether the command exists.
func ParseInvocation(input string) (Command, bool) {
	return Parse(input)
}
