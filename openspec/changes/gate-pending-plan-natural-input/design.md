## Context

`align-plan-mode-with-claude-code` 已将 `/plan` 改为可审批的 markdown planning workflow：计划生成后处于 pending/presented 状态，用户需要通过 `/plan approve`、`/plan continue <反馈>`、`/plan reject` 或 `/plan cancel` 明确推进。

当前实现中，`Runtime.RunTask` 在 `PLAN` mode 下会把任意普通输入交给 Planning Agent。因此，当 pending plan 已经展示后，用户输入“开始实现”这类自然语言时，系统会再次调用 LLM。即使 prompt 要求不要执行，模型仍可能重复输出计划或产生冗长 reasoning。

## Goals / Non-Goals

**Goals:**

- pending/presented plan 存在时，普通自然语言输入直接返回简洁 slash command 提示。
- 短路路径不调用 LLM、不调用工具、不修改计划文件、不追加新的计划内容。
- `/plan continue <反馈>` 明确作为继续规划的唯一自然语言反馈入口。
- `/plan approve`、`/plan reject`、`/plan cancel` 保持现有行为。

**Non-Goals:**

- 不改变计划审批命令名称。
- 不新增 TUI review 面板。
- 不让模型自行判断用户意图；该行为由 runtime 确定性控制。

## Decisions

### Decision: 在 Runtime 层做 pending plan gate

在 `Runtime.RunTask` 进入 Planning Agent 前检查：

```text
mode == PLAN
AND currentPlanState.Pending()
AND 本次输入不是明确允许继续规划的内部调用
```

满足条件时直接返回固定提示，不进入 `r.planning.Run`。

替代方案是只改 Planning Agent prompt，让模型回答“请使用 /plan approve”。这无法保证不消耗 token，也无法防止模型重复输出计划。runtime gate 更确定。

### Decision: 为 `/plan continue` 使用内部 bypass

`/plan continue <反馈>` 当前会转入规划路径。实现时需要区分“普通用户输入”和“slash command 明确继续规划”。

建议引入内部 helper，例如：

```text
RunTask(...)                        // 普通输入，pending plan 时拦截
runTask(..., allowPendingPlanInput) // /plan continue 使用 allow=true
```

这样不会误拦截 `/plan continue`，也不需要把特殊标记暴露给 public CLI。

### Decision: 固定提示不包含完整计划正文

短路提示只说明当前有待审批计划，并列出可用命令：

```text
当前已有待审批计划。要开始实现，请输入 /plan approve。
如需调整计划，请使用 /plan continue <反馈>；如需放弃，请使用 /plan reject 或 /plan cancel。
```

不包含计划全文，避免截图中的重复计划体验。用户仍可通过计划展示、`/session`、`/tree`、计划文件路径或后续专用命令查看计划。

## Risks / Trade-offs

- [Risk] 用户用自然语言表达“继续修改计划”时也会被短路。→ Mitigation: 提示明确给出 `/plan continue <反馈>`。
- [Risk] 内部调用路径如果设计不清晰，可能误拦截 `/plan continue`。→ Mitigation: 增加测试断言 `/plan continue` 会调用 fake LLM。
- [Risk] 提示过短可能让用户不知道当前计划在哪里。→ Mitigation: 可在提示中包含 plan ID/revision/path，但仍不输出计划全文。

## Migration Plan

1. 增加 pending plan gate helper。
2. 修改 `/plan continue` 走允许 pending 输入的内部路径。
3. 增加集成测试覆盖普通自然语言短路和 slash command 例外。
4. 视需要微调提示文本和文档。

## Open Questions

- 是否需要增加 `/plan show` 专门查看当前计划全文？本 change 不处理。
