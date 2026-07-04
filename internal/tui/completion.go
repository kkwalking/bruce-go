package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"bruce-go/internal/cli"
	"bruce-go/internal/integrated"
)

type CompletionItem struct {
	Value       string
	Display     string
	Description string
	Group       string
	Complete    bool
}

func completion(value, description, group string) CompletionItem {
	return CompletionItem{Value: value, Display: value, Description: description, Group: group, Complete: true}
}

func completionsFor(input string, cursor int, rt *integrated.Runtime) []CompletionItem {
	text := input
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len([]rune(text)) {
		cursor = len([]rune(text))
	}
	prefixText := string([]rune(text)[:cursor])
	word := currentWord(prefixText)
	var candidates []CompletionItem
	if strings.HasPrefix(word, "@image:") || strings.HasPrefix(word, "@img:") {
		return completeImagePath(word)
	}
	if strings.HasPrefix(word, "$") {
		return completeExplicitSkill(word, rt)
	}
	if strings.HasPrefix(prefixText, "/") {
		return completeSlash(prefixText, word, rt)
	}
	return candidates
}

func applyCompletion(input string, cursor int, item CompletionItem) (string, int) {
	runes := []rune(input)
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(runes) {
		cursor = len(runes)
	}
	prefix := string(runes[:cursor])
	if strings.EqualFold(prefix, "/model") && item.Group == "Model" {
		value := "/model " + item.Value + string(runes[cursor:])
		return value, len([]rune(value))
	}
	start := wordStartRunes(runes, cursor)
	if strings.HasPrefix(input, "/") && !strings.Contains(prefix, " ") {
		start = 0
	}
	next := string(runes[:start]) + item.Value + string(runes[cursor:])
	return next, len([]rune(string(runes[:start]) + item.Value))
}

func completeSlash(input, word string, rt *integrated.Runtime) []CompletionItem {
	if strings.EqualFold(input, "/model") || startsWithFold(input, "/model ") {
		return completeModel(input, rt)
	}
	if !strings.Contains(input, " ") && !strings.HasSuffix(input, " ") {
		var out []CompletionItem
		for _, command := range cli.Commands {
			value := topLevelCommandValue(command)
			if matches(value, input) {
				out = append(out, completion(value, command.Description, "bruce 命令"))
			}
		}
		return out
	}
	payload := strings.TrimPrefix(input, "/")
	parts := strings.Fields(payload)
	command := ""
	if len(parts) > 0 {
		command = strings.ToLower(parts[0])
	}
	prefix := word
	if strings.HasSuffix(input, " ") {
		prefix = ""
	}
	switch command {
	case "hitl", "parallel", "concurrency":
		return matchingOptions(prefix, "状态", []CompletionItem{
			completion("on", "开启", "状态"),
			completion("off", "关闭", "状态"),
			completion("status", "查看状态", "状态"),
		})
	case "web":
		if len(parts) <= 1 || (len(parts) == 2 && !strings.HasSuffix(input, " ")) {
			return matchingOptions(prefix, "Web", []CompletionItem{
				completion("on", "开启 Web 工具", "Web"),
				completion("off", "关闭 Web 工具", "Web"),
				completion("status", "查看状态", "Web"),
				completion("search ", "联网搜索", "Web"),
				completion("fetch ", "抓取网页正文", "Web"),
			})
		}
	case "mcp":
		return completeMCP(parts, prefix, strings.HasSuffix(input, " "), rt)
	case "skill":
		return completeSkill(parts, prefix, strings.HasSuffix(input, " "), rt)
	}
	return nil
}

func topLevelCommandValue(command cli.CommandInfo) string {
	value := "/" + command.Name
	switch command.Name {
	case "web", "mcp", "skill", "hitl", "parallel", "resume", "tree", "compact":
		return value + " "
	default:
		return value
	}
}

func completeModel(input string, rt *integrated.Runtime) []CompletionItem {
	prefix := ""
	if startsWithFold(input, "/model ") && len(input) > len("/model ") {
		prefix = strings.TrimSpace(input[len("/model "):])
	}
	current := rt.CurrentModel()
	var out []CompletionItem
	for _, option := range rt.ModelOptions() {
		selector := option.Selector()
		if !matches(option.Model, prefix) && !matches(selector, prefix) {
			continue
		}
		description := ""
		if option.Provider == current.Provider && option.Model == current.Model {
			description = "当前模型"
		}
		out = append(out, CompletionItem{
			Value:       selector,
			Display:     option.Display(),
			Description: description,
			Group:       "Model",
			Complete:    true,
		})
	}
	return out
}

func completeMCP(parts []string, prefix string, inputEndsWithSpace bool, rt *integrated.Runtime) []CompletionItem {
	if len(parts) <= 1 || (len(parts) == 2 && !inputEndsWithSpace) {
		return matchingOptions(prefix, "MCP", []CompletionItem{
			completion("status", "查看状态", "MCP"),
			completion("restart ", "重启 server", "MCP"),
			completion("logs ", "查看日志", "MCP"),
			completion("disable ", "禁用 server", "MCP"),
			completion("enable ", "启用 server", "MCP"),
		})
	}
	sub := strings.ToLower(parts[1])
	if sub != "restart" && sub != "logs" && sub != "disable" && sub != "enable" {
		return nil
	}
	var out []CompletionItem
	for _, name := range rt.MCPServerNames() {
		if matches(name, prefix) {
			out = append(out, completion(name, "已配置 server", "MCP server"))
		}
	}
	return out
}

func completeSkill(parts []string, prefix string, inputEndsWithSpace bool, rt *integrated.Runtime) []CompletionItem {
	if len(parts) <= 1 || (len(parts) == 2 && !inputEndsWithSpace) {
		return matchingOptions(prefix, "Skill", []CompletionItem{
			completion("list", "列出 Skill", "Skill"),
			completion("show ", "查看 Skill", "Skill"),
			completion("reload", "重新扫描", "Skill"),
		})
	}
	if strings.EqualFold(parts[1], "show") {
		return completeSkillNames(prefix, rt, false)
	}
	return nil
}

func completeExplicitSkill(word string, rt *integrated.Runtime) []CompletionItem {
	prefix := strings.TrimPrefix(word, "$")
	return completeSkillNames(prefix, rt, true)
}

func completeSkillNames(prefix string, rt *integrated.Runtime, explicit bool) []CompletionItem {
	var out []CompletionItem
	for _, skill := range rt.Skills.Skills() {
		name := skill.Name
		if !matches(name, prefix) {
			continue
		}
		value := name
		if explicit {
			value = "$" + name + " "
		}
		out = append(out, CompletionItem{Value: value, Display: value, Description: skill.Description, Group: "Skill", Complete: true})
	}
	return out
}

func completeImagePath(word string) []CompletionItem {
	prefix := strings.TrimPrefix(strings.TrimPrefix(word, "@image:"), "@img:")
	angle := strings.HasPrefix(prefix, "<")
	if angle {
		prefix = strings.TrimPrefix(prefix, "<")
	}
	raw := prefix
	if raw == "" {
		raw = "."
	}
	dir := raw
	namePrefix := ""
	if stat, err := os.Stat(raw); err == nil && stat.IsDir() {
		dir = raw
	} else {
		dir = filepath.Dir(raw)
		if dir == "." && !strings.Contains(raw, string(filepath.Separator)) {
			dir = "."
		}
		namePrefix = filepath.Base(raw)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out []CompletionItem
	for _, entry := range entries {
		if len(out) >= 80 {
			break
		}
		if !matches(entry.Name(), namePrefix) {
			continue
		}
		valuePath := entry.Name()
		if dir != "." {
			valuePath = filepath.Join(dir, entry.Name())
		}
		if entry.IsDir() {
			valuePath += string(filepath.Separator)
		}
		value := "@image:" + valuePath
		if angle {
			value = "@image:<" + valuePath
		}
		out = append(out, CompletionItem{Value: value, Display: value, Description: ternary(entry.IsDir(), "目录", "文件"), Group: "图片路径", Complete: !entry.IsDir()})
	}
	return out
}

func matchingOptions(prefix, group string, options []CompletionItem) []CompletionItem {
	var out []CompletionItem
	for _, option := range options {
		option.Group = group
		if matches(option.Value, prefix) {
			out = append(out, option)
		}
	}
	return out
}

func currentWord(input string) string {
	runes := []rune(input)
	start := wordStartRunes(runes, len(runes))
	return string(runes[start:])
}

func wordStartRunes(input []rune, cursor int) int {
	start := clamp(cursor, 0, len(input))
	for start > 0 && !isSpace(input[start-1]) {
		start--
	}
	return start
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func matches(value, prefix string) bool {
	return prefix == "" || strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix))
}

func startsWithFold(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}

func ternary[T any](ok bool, a, b T) T {
	if ok {
		return a
	}
	return b
}
