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
		Description:   "在 plan mode 中执行只读 Shell 探索命令，仅允许 ls、pwd、find、rg、grep、sed -n、head、tail、cat、git status/log/show/diff 等保守 allowlist",
		Parameters:    rawSchema(param{"command", "string", "要执行的只读命令", true}),
		Exec:          planExecuteCommand(base),
		PromptSnippet: "Execute read-only shell commands for inspecting the workspace",
		PromptGuidelines: []string{
			"plan mode 中只能执行只读探索命令；不要运行构建、测试、安装、格式化、写文件或会修改 Git/workspace 的命令。",
		},
	})
	RegisterPlanTools(registry, store, active)
	return registry
}

func RegisterPlanTools(registry *tool.Registry, store *Store, active ActivePlanFunc) {
	registry.Register(tool.Tool{
		Name:          "read_plan",
		Description:   "读取当前 active markdown 计划",
		Parameters:    rawSchema(),
		Exec:          readPlan(store, active),
		PromptSnippet: "Read the current markdown plan",
	})
	registry.Register(tool.Tool{
		Name:          "replace_plan",
		Description:   "创建或完整替换当前 active markdown 计划内容",
		Parameters:    rawSchema(param{"content", "string", "完整 markdown 计划内容", true}, param{"summary", "string", "本次修改摘要", false}),
		Exec:          replacePlan(store, active),
		PromptSnippet: "Create or replace the current markdown plan",
	})
	registry.Register(tool.Tool{
		Name:          "edit_plan",
		Description:   "精确修改当前 active markdown 计划中的一段文本，old_text 必须唯一匹配",
		Parameters:    rawSchema(param{"old_text", "string", "要替换的原文", true}, param{"new_text", "string", "替换后的文本", true}, param{"summary", "string", "本次修改摘要", false}),
		Exec:          editPlan(store, active),
		PromptSnippet: "Make one exact edit to the current markdown plan",
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
		return fmt.Sprintf("计划已%s: %s rev=%d\n%s", actionText(state.Action), state.ID, state.Revision, state.Path), nil
	}
}

func editPlan(store *Store, active ActivePlanFunc) tool.Executor {
	return func(_ context.Context, args map[string]string) (string, error) {
		state := active()
		if state.Empty() {
			return "", errors.New("当前没有 active plan，请先使用 replace_plan 创建计划")
		}
		oldText := args["old_text"]
		if oldText == "" {
			return "", errors.New("old_text 不能为空")
		}
		content, err := store.Read(state)
		if err != nil {
			return "", err
		}
		count := strings.Count(content, oldText)
		if count == 0 {
			return "", errors.New("old_text 未在当前计划中找到，计划未修改")
		}
		if count > 1 {
			return "", fmt.Errorf("old_text 匹配多处 (%d)，请提供更精确的文本，计划未修改", count)
		}
		updated := strings.Replace(content, oldText, args["new_text"], 1)
		next, err := store.Replace(state, updated, args["summary"])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("计划已编辑: %s rev=%d", next.ID, next.Revision), nil
	}
}

func planExecuteCommand(base *tool.Registry) tool.Executor {
	return func(ctx context.Context, args map[string]string) (string, error) {
		command := args["command"]
		if result := CheckReadOnlyCommand(command); !result.Allowed {
			return "命令被 plan mode 安全策略拒绝: " + result.Reason, nil
		}
		return base.ExecuteWithSandboxMode(ctx, "execute_command", map[string]string{"command": command}, sandbox.ModeReadOnly), nil
	}
}

func CheckReadOnlyCommand(command string) tool.GuardResult {
	normalized := strings.TrimSpace(command)
	if normalized == "" {
		return tool.GuardResult{Reason: "命令不能为空"}
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
			return tool.GuardResult{Reason: "plan mode 仅允许只读探索命令，拒绝片段: " + strings.TrimSpace(banned)}
		}
	}
	segments := strings.Split(normalized, "|")
	for _, segment := range segments {
		if !readOnlySegmentAllowed(strings.TrimSpace(segment)) {
			return tool.GuardResult{Reason: "命令不在 plan mode 只读 allowlist 中: " + strings.TrimSpace(segment)}
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
		return "创建"
	}
	return "更新"
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
