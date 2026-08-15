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
	if leadingSlashStart(prefixText) >= 0 {
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
	start := wordStartRunes(runes, cursor)
	if slashStart := leadingSlashStart(prefix); slashStart >= 0 {
		prefixFromSlash := []rune(prefix)[slashStart:]
		if !containsSpace(prefixFromSlash) {
			// Replace the whole first slash token (preserving any leading
			// whitespace accepted by cli.Parse).
			start = slashStart
		}
	}
	next := string(runes[:start]) + item.Value + string(runes[cursor:])
	return next, len([]rune(string(runes[:start]) + item.Value))
}

type slashCompletionContext struct {
	command       cli.CommandInfo
	known         bool
	typingCommand bool
	args          []string
	prefix        string
	endsWithSpace bool
}

func parseSlashCompletion(input, word string) (slashCompletionContext, bool) {
	start := leadingSlashStart(input)
	if start < 0 {
		return slashCompletionContext{}, false
	}
	payload := string([]rune(input)[start+1:])
	fields := strings.Fields(payload)
	endsWithSpace := len(payload) > 0 && isSpace(lastRune(payload))

	// Only the first token exists so far (for example "/", "/sa", or
	// "/sandbox"): complete the command name itself.
	if len(fields) == 0 || (len(fields) == 1 && !endsWithSpace) {
		return slashCompletionContext{typingCommand: true}, true
	}

	command, known := cli.FindCommand(fields[0])
	ctx := slashCompletionContext{
		command:       command,
		known:         known,
		prefix:        word,
		endsWithSpace: endsWithSpace,
	}
	if len(fields) > 1 {
		ctx.args = fields[1:]
	}
	if endsWithSpace {
		ctx.prefix = ""
	}
	return ctx, true
}

func completeSlash(input, word string, rt *integrated.Runtime) []CompletionItem {
	ctx, ok := parseSlashCompletion(input, word)
	if !ok {
		return nil
	}
	if ctx.typingCommand {
		return completeTopLevelCommands(word)
	}
	if !ctx.known {
		return nil
	}
	if strings.EqualFold(ctx.command.Name, "model") {
		return completeModel(input, rt)
	}
	return completeCommandOptions(ctx.command.Options, ctx.args, ctx.prefix, ctx.endsWithSpace, rt)
}

func completeTopLevelCommands(prefix string) []CompletionItem {
	var out []CompletionItem
	for _, command := range cli.Commands {
		value := command.CompletionValue()
		if matches(value, prefix) {
			out = append(out, completion(value, command.Description, "Bruce command"))
		}
	}
	return out
}

func completeCommandOptions(options []cli.CommandOption, args []string, prefix string, endsWithSpace bool, rt *integrated.Runtime) []CompletionItem {
	if len(options) == 0 {
		return nil
	}
	if len(args) == 0 {
		return staticOptionCompletions(options, prefix)
	}
	if len(args) == 1 && !endsWithSpace {
		return staticOptionCompletions(options, prefix)
	}
	current := findCommandOption(options, args[0])
	if current == nil {
		return nil
	}
	if len(args) == 1 {
		return completeOptionArgument(*current, nil, false, rt)
	}
	return completeOptionArgument(*current, args[1:], endsWithSpace, rt)
}

func completeOptionArgument(option cli.CommandOption, remaining []string, endsWithSpace bool, rt *integrated.Runtime) []CompletionItem {
	if option.Kind != cli.CompletionStatic {
		if len(remaining) == 0 {
			return dynamicOptionCompletions(option.Kind, "", rt)
		}
		if len(remaining) == 1 && !endsWithSpace {
			return dynamicOptionCompletions(option.Kind, remaining[0], rt)
		}
		return nil
	}
	if len(option.Options) == 0 {
		return nil
	}
	if len(remaining) == 0 {
		return staticOptionCompletions(option.Options, "")
	}
	if len(remaining) == 1 && !endsWithSpace {
		return staticOptionCompletions(option.Options, remaining[0])
	}
	return nil
}

func staticOptionCompletions(options []cli.CommandOption, prefix string) []CompletionItem {
	var out []CompletionItem
	for _, option := range options {
		if !matches(option.Value, prefix) {
			continue
		}
		out = append(out, CompletionItem{
			Value:       option.Value,
			Display:     option.Value,
			Description: option.Description,
			Group:       option.Group,
			Complete:    true,
		})
	}
	return out
}

func findCommandOption(options []cli.CommandOption, token string) *cli.CommandOption {
	for i := range options {
		if strings.EqualFold(strings.TrimSpace(options[i].Value), token) {
			return &options[i]
		}
	}
	return nil
}

func dynamicOptionCompletions(kind cli.CompletionKind, prefix string, rt *integrated.Runtime) []CompletionItem {
	switch kind {
	case cli.CompletionMCPServer:
		return completeMCPServers(prefix, rt)
	case cli.CompletionSkillName:
		return completeSkillNames(prefix, rt, false)
	default:
		return nil
	}
}

func completeModel(input string, rt *integrated.Runtime) []CompletionItem {
	commandText := slashCommandText(input)
	rest := ""
	if startsWithFold(commandText, "/model ") {
		rest = strings.TrimSpace(commandText[len("/model "):])
	}
	parts := strings.Fields(rest)
	endsWithSpace := len(commandText) > 0 && isSpace(lastRune(commandText))

	// The reasoning subcommand has its own five-level completion list. Like
	// other argument-taking options, typing its prefix (for example
	// "/model rea") first completes "reasoning " and the levels appear only
	// after the following space.
	if len(parts) == 1 && strings.EqualFold(parts[0], "reasoning") {
		if endsWithSpace {
			return completeReasoningLevels("", rt.ReasoningEffort())
		}
		return []CompletionItem{reasoningCommandCompletion()}
	}
	if len(parts) > 1 && strings.EqualFold(parts[0], "reasoning") {
		if len(parts) == 2 && !endsWithSpace {
			return completeReasoningLevels(parts[1], rt.ReasoningEffort())
		}
		return nil
	}

	currentModel := rt.CurrentModel()
	prefix := rest
	var out []CompletionItem
	for _, option := range rt.ModelOptions() {
		selector := option.Selector()
		if !matches(option.Model, prefix) && !matches(selector, prefix) {
			continue
		}
		description := ""
		if strings.EqualFold(option.Provider, currentModel.Provider) && strings.EqualFold(option.Model, currentModel.Model) {
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
	if prefix != "" && matches("reasoning", prefix) {
		out = append(out, reasoningCommandCompletion())
	}
	return out
}

func reasoningCommandCompletion() CompletionItem {
	return CompletionItem{
		Value:       "reasoning ",
		Display:     "reasoning ",
		Description: "Adjust reasoning effort",
		Group:       "Reasoning command",
		Complete:    true,
	}
}

func completeReasoningLevels(prefix, current string) []CompletionItem {
	levels := []string{"off", "low", "medium", "high", "max"}
	var out []CompletionItem
	for _, level := range levels {
		if !matches(level, prefix) {
			continue
		}
		description := ""
		if strings.EqualFold(level, current) {
			description = "Current"
		}
		out = append(out, CompletionItem{
			Value:       level,
			Display:     level,
			Description: description,
			Group:       "Reasoning",
			Complete:    true,
		})
	}
	return out
}

func completeMCPServers(prefix string, rt *integrated.Runtime) []CompletionItem {
	var out []CompletionItem
	for _, name := range rt.MCPServerNames() {
		if matches(name, prefix) {
			out = append(out, completion(name, "Configured server", "MCP server"))
		}
	}
	return out
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

func leadingSlashStart(input string) int {
	runes := []rune(input)
	for i, r := range runes {
		if isSpace(r) {
			continue
		}
		if r == '/' {
			return i
		}
		return -1
	}
	return -1
}

func slashCommandText(input string) string {
	start := leadingSlashStart(input)
	if start < 0 {
		return input
	}
	return string([]rune(input)[start:])
}

func containsSpace(runes []rune) bool {
	for _, r := range runes {
		if isSpace(r) {
			return true
		}
	}
	return false
}

func lastRune(value string) rune {
	if value == "" {
		return 0
	}
	runes := []rune(value)
	return runes[len(runes)-1]
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
