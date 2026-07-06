## Context

Plan Mode 当前已经形成双轨存储：计划正文写入 `~/.bruce/plans/<plan-id>.md`，计划生命周期写入 session JSONL 的 `plan_event`。`presented` 事件会携带完整 markdown 内容快照，足以作为 TUI 展示的权威来源。

当前 TUI 的实时展示主要来自 `MessageDelta` / `MessageCompleted`，`/plan <description>` 的命令 output 还会被 `commandOutputAlreadyRendered` 抑制。结果是完整计划是否出现在屏幕上，仍取决于 Planning Agent 最后一条 assistant 消息怎么写。`/resume` 时 TUI 只回放 `Context.Messages`，而这些 messages 故意不包含 `plan_event`，所以恢复会话时也无法从 session entry 展示完整计划。

## Goals / Non-Goals

**Goals:**

- 让 TUI 的完整计划展示由 `plan_event(presented).content` 驱动。
- 保持 plan file 和 session JSONL 现有格式不变。
- 保持 LLM history 干净，不把计划正文复制成重复 assistant message。
- 让 `/resume` 和 session replay 能恢复并展示 active path 上最近 presented plan。
- 调整 Plan Mode prompt，要求最终回复只输出简短状态，不复述计划正文。

**Non-Goals:**

- 不实现 markdown 富文本解析器；初版可以按现有 TUI 文本换行能力展示完整 markdown。
- 不增加启发式逻辑识别并折叠 assistant final message 中的长计划文本。
- 不改变 `/plan approve`、`/plan continue`、`/plan reject`、`/plan cancel` 语义。
- 不改变 `plan_event` 的 JSONL 字段结构。

## Decisions

### Decision: 新增 typed runtime event 表达 plan_event

在 `internal/event` 增加类似 `PlanEventRecorded` 的 typed event，字段包含 `RunID`、时间戳和 `runtime.PlanEvent`。Runtime 在成功记录 `plan_event(presented)` 后发出该 event。TUI 只对 `Action == presented` 且 `Content` 非空的 event 渲染完整计划。

替代方案是让 TUI 读取 `RunCompleted.Output` 或命令 result output。这个方案仍然把计划当普通字符串结果，无法表达它来自 session entry，也无法统一 resume replay。

### Decision: 实时展示只在 presented 阶段渲染完整计划

`replace_plan` / `edit_plan` 会记录 `created` / `updated`，但这些是 Planning Agent 内部维护 artifact 的中间步骤。TUI 的完整计划展示应在 runtime `presentPlan()` 记录 `presented` 后发生，表示“此 revision 已经准备给用户审阅”。

替代方案是每次 `created` / `updated` 都实时渲染。这样会在工具调用过程中展示中间版本，容易和最终 presented 版本重复。

### Decision: Session replay 使用 active path entries

`session.Context` 或等价 UI replay 数据需要包含 active path 上的 entries，至少要让 TUI 能看到 `message` 和 `plan_event` 的相对顺序。`Context.Messages` 继续服务 LLM history，不应该为了 UI replay 而混入 `plan_event`。

TUI 的 replay 方法应从 entries 重建屏幕消息：

- `message` entry 渲染为用户/assistant/reasoning 消息。
- `plan_event(presented)` 且 `content` 非空时渲染为计划消息。
- 其它 plan lifecycle event 可先不渲染全文，仍由 `/tree`、`/session`、`/status` 表达状态。

替代方案是只把 `ActivePlan.Content` 加到 context 里。这能展示最后计划，但会丢失它在会话时间线中的位置，也无法自然支持后续多次 presented 事件。

### Decision: TUI 增加计划消息类型

TUI 可以新增 `messagePlan` 或等价 message kind。初版不需要引入 markdown renderer，只需用不同样式或标题把计划正文作为完整 markdown 文本展示，保证正文完整、可滚动、可换行。

这让用户能区分“模型状态回复”和“系统从 session entry 渲染出的计划正文”。

### Decision: Plan Mode prompt 不再要求模型展示计划关键内容

`internal/planning/prompt.go` 当前要求“回复用户时展示当前计划的关键内容”。这应改为：当已经通过 `replace_plan` 或 `edit_plan` 保存计划后，最终回复只输出简短状态，例如“计划创建完成，请审阅。”或“计划已更新，请审阅。”，不要重复、摘要、改写或节选 markdown 计划正文，并说明完整计划会由系统单独展示。

这不是安全边界，只是降低重复和困惑；权威展示仍由 `plan_event(presented)` 驱动。

## Risks / Trade-offs

- [Risk] 如果 Planning Agent 无视 prompt 并在 assistant final message 中复述计划，用户仍可能看到重复文本。→ Mitigation: 本 change 先不做启发式折叠，依赖 prompt 降低概率，并通过 typed plan message 清楚标明权威计划块。
- [Risk] TUI replay 改用 entries 可能改变 resume 后消息顺序。→ Mitigation: 只按 active path 顺序重建，保留现有 `message` 渲染逻辑，并增加 resume/replay 测试。
- [Risk] Runtime event 和 session append 可能不一致。→ Mitigation: 只有在 `planStore.Record(presented, ...)` 成功后才 emit event，event payload 使用同一份 plan event 数据。
- [Risk] 计划正文很长会占用屏幕。→ Mitigation: 复用现有滚动和换行机制，后续可再增加折叠/展开交互。

## Migration Plan

1. 增加 runtime plan event 类型和 TUI handler。
2. 在 `presentPlan()` 成功记录 `presented` 后 emit typed event。
3. 扩展 session context/replay 数据，让 TUI resume 时能看到 active path entries。
4. 增加 TUI 计划消息类型和渲染逻辑。
5. 更新 Plan Mode prompt，移除“展示当前计划关键内容”的要求。
6. 增加 runtime/TUI/session replay/prompt 测试。

回滚策略：如果 TUI entry 渲染出现问题，可以临时只禁用 typed event handler，session JSONL 和 plan file 格式不需要回滚。

## Open Questions

- 后续是否需要为 plan message 增加折叠/展开或独立 review 面板？
- 后续是否需要 `/plan show` 作为非 TUI 环境下查看当前完整计划的显式命令？
