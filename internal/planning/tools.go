package planning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"bruce-go/internal/runtime"
	"bruce-go/internal/sandbox"
	"bruce-go/internal/tool"
)

type ActivePlanFunc func() runtime.PlanState

func NewToolRegistry(base *tool.Registry, store *Store, active ActivePlanFunc) *tool.Registry {
	registry := tool.EmptyRegistry(base.WorkspaceRoot())
	for _, name := range []string{"read_file", "web_search", "web_fetch"} {
		if copied, ok := base.Lookup(name); ok {
			registry.Register(copied)
		}
	}
	registry.Register(tool.Tool{
		Name:          "execute_command",
		Description:   "Run a read-only shell command for exploration in plan mode. Only a conservative allowlist is permitted, including ls, pwd, find, rg, grep, sed -n, head, tail, cat, and git status/log/show/diff",
		Parameters:    rawSchema(param{"command", "string", "Read-only command to execute", true}),
		Exec:          planExecuteCommand(base),
		PromptSnippet: "Execute read-only shell commands for inspecting the workspace",
		PromptGuidelines: []string{
			"Plan mode permits only read-only exploration commands. Do not run builds, tests, installers, formatters, file-writing commands, or any command that modifies Git or the workspace.",
		},
		Policy: tool.Policy{Source: tool.SourcePlan, MinimumMode: sandbox.ModeReadOnly},
	})
	RegisterPlanTools(registry, store, active)
	return registry
}

func RegisterPlanTools(registry *tool.Registry, store *Store, active ActivePlanFunc) {
	registry.Register(tool.Tool{
		Name:          "read_plan",
		Description:   "Read the current active Markdown plan",
		Parameters:    rawSchema(),
		Exec:          readPlan(store, active),
		PromptSnippet: "Read the current markdown plan",
		Policy:        tool.Policy{Source: tool.SourcePlan, MinimumMode: sandbox.ModeReadOnly, ParallelSafe: true},
	})
	registry.Register(tool.Tool{
		Name:          "replace_plan",
		Description:   "Create the active Markdown plan or replace its entire content",
		Parameters:    rawSchema(param{"content", "string", "Complete Markdown plan content", true}, param{"summary", "string", "Summary of this change", false}),
		Exec:          replacePlan(store, active),
		PromptSnippet: "Create or replace the current markdown plan",
		Policy:        tool.Policy{Source: tool.SourcePlan, MinimumMode: sandbox.ModeReadOnly},
	})
	registry.Register(tool.Tool{
		Name:          "edit_plan",
		Description:   "Precisely replace a text segment in the active Markdown plan; old_text must match exactly once",
		Parameters:    rawSchema(param{"old_text", "string", "Exact text to replace", true}, param{"new_text", "string", "Replacement text", true}, param{"summary", "string", "Summary of this change", false}),
		Exec:          editPlan(store, active),
		PromptSnippet: "Make one exact edit to the current markdown plan",
		Policy:        tool.Policy{Source: tool.SourcePlan, MinimumMode: sandbox.ModeReadOnly},
	})
}

func readPlan(store *Store, active ActivePlanFunc) tool.Executor {
	return func(_ context.Context, _ map[string]string) (string, error) {
		return store.Read(active())
	}
}

func replacePlan(store *Store, active ActivePlanFunc) tool.Executor {
	return func(_ context.Context, args map[string]string) (string, error) {
		state, err := store.Replace(active(), args["content"], args["summary"])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Plan %s: %s rev=%d\n%s", actionText(state.Action), state.ID, state.Revision, state.Path), nil
	}
}

func editPlan(store *Store, active ActivePlanFunc) tool.Executor {
	return func(_ context.Context, args map[string]string) (string, error) {
		state := active()
		if state.Empty() {
			return "", errors.New("there is no active plan; create one with replace_plan first")
		}
		oldText := args["old_text"]
		if oldText == "" {
			return "", errors.New("old_text must not be empty")
		}
		content, err := store.Read(state)
		if err != nil {
			return "", err
		}
		count := strings.Count(content, oldText)
		if count == 0 {
			return "", errors.New("old_text was not found in the current plan; the plan was not modified")
		}
		if count > 1 {
			return "", fmt.Errorf("old_text matched more than once (%d matches); provide more specific text; the plan was not modified", count)
		}
		updated := strings.Replace(content, oldText, args["new_text"], 1)
		next, err := store.Replace(state, updated, args["summary"])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Plan edited: %s rev=%d", next.ID, next.Revision), nil
	}
}

func planExecuteCommand(base *tool.Registry) tool.Executor {
	return func(ctx context.Context, args map[string]string) (string, error) {
		command := args["command"]
		if result := CheckReadOnlyCommand(command); !result.Allowed {
			message := "Command rejected by the plan-mode security policy: " + result.Reason
			return message, tool.NewExecutionError(tool.ToolCallRejected, errors.New(message))
		}
		var outcome tool.ExecutionOutcome
		if base.SandboxCanEnforce(sandbox.ModeReadOnly) {
			outcome = base.ExecuteWithSandboxModeResult(ctx, "execute_command", map[string]string{"command": command}, sandbox.ModeReadOnly)
		} else {
			outcome = base.ExecuteResult(ctx, "execute_command", map[string]string{"command": command})
		}
		if outcome.Status != tool.ToolCallSuccess {
			return outcome.Output, tool.NewExecutionError(outcome.Status, errors.New(outcome.Output))
		}
		return outcome.Output, nil
	}
}

func CheckReadOnlyCommand(command string) tool.GuardResult {
	normalized := strings.TrimSpace(command)
	if normalized == "" {
		return tool.GuardResult{Reason: "command must not be empty"}
	}
	if result := (tool.CommandGuard{}).Check(normalized); !result.Allowed {
		return result
	}
	lower := strings.ToLower(strings.Join(strings.Fields(normalized), " "))
	for _, banned := range []string{
		">", ">>", "<<", ";", "&&", "||", "`", "$(",
		" rm ", " rm -", "rm ", " mv ", "mv ", " cp ", "cp ", " touch ", "touch ",
		" mkdir ", "mkdir ", " rmdir ", "rmdir ", " chmod ", "chmod ", " chown ", "chown ",
		" tee ", "tee ", " sed -i", " perl -i", " go test", "go test", " go build", "go build",
		" npm ", "npm ", " pnpm ", "pnpm ", " yarn ", "yarn ", " cargo ", "cargo ",
	} {
		if strings.Contains(" "+lower+" ", banned) {
			return tool.GuardResult{Reason: "plan mode permits only read-only exploration commands; rejected segment: " + strings.TrimSpace(banned)}
		}
	}
	segments := strings.Split(normalized, "|")
	for _, segment := range segments {
		if !readOnlySegmentAllowed(strings.TrimSpace(segment)) {
			return tool.GuardResult{Reason: "command is not on the plan-mode read-only allowlist: " + strings.TrimSpace(segment)}
		}
	}
	return tool.GuardResult{Allowed: true}
}

func readOnlySegmentAllowed(segment string) bool {
	fields := strings.Fields(segment)
	if len(fields) == 0 {
		return false
	}
	cmd := strings.TrimPrefix(filepathBase(fields[0]), "./")
	switch cmd {
	case "pwd", "ls", "find", "rg", "grep", "head", "tail", "cat", "wc", "sort", "uniq":
		return true
	case "sed":
		for _, field := range fields[1:] {
			if field == "-n" || strings.Contains(field, "p") {
				return true
			}
		}
		return false
	case "git":
		if len(fields) < 2 {
			return false
		}
		switch fields[1] {
		case "status", "log", "show", "diff", "rev-parse", "branch", "ls-files", "grep":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func filepathBase(value string) string {
	if idx := strings.LastIndexAny(value, `/\`); idx >= 0 {
		return value[idx+1:]
	}
	return value
}

func actionText(action runtime.PlanAction) string {
	if action == runtime.PlanActionCreated {
		return "created"
	}
	return "updated"
}

type param struct {
	Name        string
	Type        string
	Description string
	Required    bool
}

func rawSchema(items ...param) json.RawMessage {
	props := map[string]any{}
	required := []string{}
	for _, item := range items {
		props[item.Name] = map[string]any{"type": item.Type, "description": item.Description}
		if item.Required {
			required = append(required, item.Name)
		}
	}
	data, _ := json.Marshal(map[string]any{"type": "object", "properties": props, "required": required})
	return data
}
