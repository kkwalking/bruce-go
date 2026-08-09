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
				out = append(out, completion(value, command.Description, "Bruce command"))
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
	case "plan":
		if len(parts) <= 1 || (len(parts) == 2 && !strings.HasSuffix(input, " ")) {
			return matchingOptions(prefix, "Plan", []CompletionItem{
				completion("approve", "Approve the current plan and begin execution", "Plan"),
				completion("continue ", "Continue planning with feedback", "Plan"),
				completion("reject ", "Reject the current plan", "Plan"),
				completion("cancel", "Cancel the current plan", "Plan"),
			})
		}
	case "hitl", "parallel", "concurrency":
		return matchingOptions(prefix, "Status", []CompletionItem{
			completion("on", "Enable", "Status"),
			completion("off", "Disable", "Status"),
			completion("status", "View status", "Status"),
		})
	case "web":
		if len(parts) <= 1 || (len(parts) == 2 && !strings.HasSuffix(input, " ")) {
			return matchingOptions(prefix, "Web", []CompletionItem{
				completion("on", "Enable Web tools", "Web"),
				completion("off", "Disable Web tools", "Web"),
				completion("status", "View status", "Web"),
				completion("search ", "Search the web", "Web"),
				completion("fetch ", "Fetch web-page content", "Web"),
			})
		}
	case "mcp":
		return completeMCP(parts, prefix, strings.HasSuffix(input, " "), rt)
	case "skill":
		return completeSkill(parts, prefix, strings.HasSuffix(input, " "), rt)
	case "sandbox":
		return completeSandbox(parts, prefix, strings.HasSuffix(input, " "))
	}
	return nil
}

func topLevelCommandValue(command cli.CommandInfo) string {
	value := "/" + command.Name
	switch command.Name {
	case "web", "mcp", "skill", "hitl", "parallel", "sandbox", "resume", "tree", "compact", "plan":
		return value + " "
	default:
		return value
	}
}

func completeSandbox(parts []string, prefix string, inputEndsWithSpace bool) []CompletionItem {
	if len(parts) <= 1 || (len(parts) == 2 && !inputEndsWithSpace) {
		return matchingOptions(prefix, "Sandbox", []CompletionItem{
			completion("status", "View sandbox status", "Sandbox"),
			completion("mode ", "Change filesystem permission mode", "Sandbox"),
			completion("network ", "Change command network access", "Sandbox"),
		})
	}
	switch strings.ToLower(parts[1]) {
	case "mode":
		if len(parts) == 2 || (len(parts) == 3 && !inputEndsWithSpace) {
			return matchingOptions(prefix, "Sandbox mode", []CompletionItem{
				completion("read-only", "Read-only workspace", "Sandbox mode"),
				completion("workspace-write", "Allow writes only within the workspace", "Sandbox mode"),
				completion("full-access", "Disable native shell sandboxing", "Sandbox mode"),
			})
		}
	case "network":
		if len(parts) == 2 || (len(parts) == 3 && !inputEndsWithSpace) {
			return matchingOptions(prefix, "Sandbox network", []CompletionItem{
				completion("on", "Allow commands to access the network", "Sandbox network"),
				completion("off", "Prevent commands from accessing the network", "Sandbox network"),
			})
		}
	}
	return nil
}

func completeModel(input string, rt *integrated.Runtime) []CompletionItem {
	// Extract the part after "/model "
	rest := ""
	if startsWithFold(input, "/model ") && len(input) > len("/model ") {
		rest = strings.TrimSpace(input[len("/model "):])
	}
	parts := strings.Fields(rest)

	// If first word is "reasoning", show reasoning level completions
	if len(parts) > 0 && strings.EqualFold(parts[0], "reasoning") {
		// Check what comes after "reasoning"
		levelPrefix := ""
		if len(parts) > 1 {
			levelPrefix = parts[1]
		}
		// If input ends with space after "reasoning", prefix should be empty
		if strings.HasSuffix(input, " ") && len(parts) == 1 {
			levelPrefix = ""
		}
		current := rt.ReasoningEffort()
		levels := []string{"off", "low", "medium", "high", "max"}
		var out []CompletionItem
		for _, level := range levels {
			if !matches(level, levelPrefix) {
				continue
			}
			desc := ""
			if strings.EqualFold(level, current) {
				desc = "Current"
			}
			out = append(out, CompletionItem{
				Value:       level,
				Display:     level,
				Description: desc,
				Group:       "Reasoning",
				Complete:    true,
			})
		}
		return out
	}

	// Model list
	currentModel := rt.CurrentModel()
	prefix := rest
	var out []CompletionItem

	for _, option := range rt.ModelOptions() {
		selector := option.Selector()
		if !matches(option.Model, prefix) && !matches(selector, prefix) {
			continue
		}
		description := ""
		if option.Provider == currentModel.Provider && option.Model == currentModel.Model {
			description = "Current model"
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
			completion("status", "View status", "MCP"),
			completion("restart ", "Restart a server", "MCP"),
			completion("logs ", "View logs", "MCP"),
			completion("disable ", "Disable a server", "MCP"),
			completion("enable ", "Enable a server", "MCP"),
		})
	}
	sub := strings.ToLower(parts[1])
	if sub != "restart" && sub != "logs" && sub != "disable" && sub != "enable" {
		return nil
	}
	var out []CompletionItem
	for _, name := range rt.MCPServerNames() {
		if matches(name, prefix) {
			out = append(out, completion(name, "Configured server", "MCP server"))
		}
	}
	return out
}

func completeSkill(parts []string, prefix string, inputEndsWithSpace bool, rt *integrated.Runtime) []CompletionItem {
	if len(parts) <= 1 || (len(parts) == 2 && !inputEndsWithSpace) {
		return matchingOptions(prefix, "Skill", []CompletionItem{
			completion("list", "List Skills", "Skill"),
			completion("show ", "Inspect a Skill", "Skill"),
			completion("reload", "Rescan Skills", "Skill"),
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
		out = append(out, CompletionItem{Value: value, Display: value, Description: ternary(entry.IsDir(), "Directory", "File"), Group: "Image path", Complete: !entry.IsDir()})
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
