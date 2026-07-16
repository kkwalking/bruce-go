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
	{Name: "react", Usage: "/react", Description: "切换到 ReAct 模式"},
	{Name: "plan", Usage: "/plan [任务|approve|reject|cancel|continue]", Description: "进入计划模式、维护计划或审批计划"},
	{Name: "model", Usage: "/model [provider/model | reasoning [off|low|medium|high|max]]", Description: "查看或切换模型、调整推理级别"},
	{Name: "web", Usage: "/web on|off|status|search <query>|fetch <url>", Description: "开关、查看或手动使用 WebSearch/WebFetch"},
	{Name: "mcp", Usage: "/mcp [restart|logs|disable|enable <name>]", Description: "查看或管理 MCP server"},
	{Name: "skill", Usage: "/skill list|show <name>|reload", Description: "查看、展示或重新加载 Skill"},
	{Name: "hitl", Usage: "/hitl on|off|status", Description: "开关或查看人工审批"},
	{Name: "sandbox", Usage: "/sandbox [status|mode <mode>|network on|off]", Description: "查看或切换命令沙箱"},
	{Name: "parallel", Usage: "/parallel on|off|status", Description: "开关或查看并行工具调用"},
	{Name: "status", Usage: "/status", Description: "查看统一运行状态"},
	{Name: "session", Usage: "/session", Description: "查看当前 session"},
	{Name: "sessions", Usage: "/sessions", Description: "列出当前工作目录的 session"},
	{Name: "new", Usage: "/new", Description: "新建 session"},
	{Name: "resume", Usage: "/resume <id|path>", Description: "恢复指定 session"},
	{Name: "tree", Usage: "/tree [entryId]", Description: "查看或切换 session 树节点"},
	{Name: "compact", Usage: "/compact [instructions]", Description: "压缩较早 session 历史"},
	{Name: "clear", Usage: "/clear", Description: "开启新 session 并清空本轮状态"},
	{Name: "help", Usage: "/help", Description: "显示帮助"},
	{Name: "exit", Usage: "/exit", Description: "退出程序"},
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
	b.WriteString("Bruce Go 可用命令:\n\n")
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
	b.WriteString("\n输入语法:\n")
	b.WriteString("$<skill> <任务>                 显式加载 Skill，最多 3 个\n")
	b.WriteString("@image:<path>                   附加图片文件\n")
	b.WriteString("@image:<file:///path with space> 附加 file:// 图片\n")
	b.WriteString("@clipboard                      附加 macOS 剪贴板图片\n")
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
