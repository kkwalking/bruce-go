## Why

当前 Plan Mode 已经会把完整 markdown 计划写入 `plan_event(presented).content`，但 TUI 仍主要依赖 LLM 最后一条 assistant 消息或命令 output 来呈现计划。这样计划展示会受模型自由回复影响，也容易出现“LLM 摘要”和“完整计划正文”重复并存，让用户不确定哪个内容才是权威计划。

本变更让结构化 session entry 成为计划正文展示的唯一权威来源，并用 prompt 约束模型最终回复只给简短状态。

## What Changes

- TUI 在实时运行中 MUST 根据 `plan_event(presented).content` 渲染完整 markdown 计划。
- `/resume` 或 session replay 后，TUI MUST 能从 active path 中的 `plan_event(presented)` 恢复并展示最近一次完整计划。
- Runtime/event 层需要把已记录的 `plan_event` 作为 typed UI event 发送给 TUI，避免 TUI 依赖普通 assistant 文本推断计划内容。
- Planning Agent prompt MUST 要求模型在 `replace_plan` 或 `edit_plan` 后只输出简短状态，例如“计划创建完成，请审阅。”或“计划已更新，请审阅。”，不要复述、摘要、改写或节选计划正文。
- 暂不增加启发式逻辑去识别并折叠 assistant final message 中的长计划文本。

## Capabilities

### New Capabilities

- `plan-mode`: 补充 Plan Mode 中 presented plan 的 TUI 渲染来源和 Planning Agent 最终回复约束。

### Modified Capabilities

- None.

## Impact

- 受影响代码：`internal/event`、`internal/session`、`internal/integrated`、`internal/tui`、`internal/planning`。
- 受影响行为：Plan Mode 计划展示、session resume/replay 的计划展示、Planning Agent prompt。
- 受影响测试：新增/更新 runtime event、TUI 渲染、resume replay、prompt 内容相关测试。
- 不新增外部依赖，不改变 plan file/session JSONL 的既有持久化格式。
