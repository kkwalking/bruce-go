## Why

当前 plan mode 已能生成并展示待审批计划，但在 pending plan 已存在时，用户如果直接输入自然语言（例如“开始实现”），运行时仍会把这条输入交给 Planning Agent。这样会消耗 token、产生不必要 reasoning，并可能重复输出完整计划，体验上像截图中那样冗长。

待审批计划阶段应该是一个明确的 review gate：普通自然语言不能隐式继续规划或开始执行，必须提示用户使用 `/plan approve`、`/plan continue <反馈>`、`/plan reject` 或 `/plan cancel`。

## What Changes

- 在 `PLAN` mode 且存在 pending/presented plan 时，普通自然语言输入 MUST 被 runtime 确定性短路。
- 短路时不调用 LLM、不调用 Planning Agent、不追加新的计划内容、不重复输出完整计划。
- 短路输出只给出简洁操作提示：批准、继续调整、拒绝或取消。
- `/plan continue <反馈>` 仍然允许继续调用 Planning Agent 修改计划。
- `/plan approve` 仍然允许批准并交接到 ReAct 执行。
- 增加测试覆盖，确保 pending plan 下普通输入不会消耗 fake LLM response，也不会复述计划正文。

## Capabilities

### New Capabilities
- None.

### Modified Capabilities
- `plan-mode`: 增加 pending plan review gate，规定待审批计划下普通自然语言输入只返回 slash command 提示，不进入 Planning Agent。

## Impact

- 受影响运行时：`internal/integrated.Runtime.RunTask` 和 `/plan continue` 内部调用路径。
- 受影响测试：新增 pending plan 下普通输入短路、`/plan continue` 例外、`/plan approve` 例外的集成测试。
- 用户体验影响：pending plan 后输入“开始实现”等自然语言时，界面只显示短提示，不再重复计划正文。
