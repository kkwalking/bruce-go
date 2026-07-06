## ADDED Requirements

### Requirement: Pending plan natural language gate
系统 MUST 在 `PLAN` mode 且存在 pending/presented plan 时，将普通自然语言输入短路为审批命令提示，而不是再次调用 Planning Agent。

#### Scenario: Natural language is gated while plan is pending
- **WHEN** 当前 mode 为 `PLAN`
- **AND** 当前 session 存在 pending/presented plan
- **AND** 用户输入普通自然语言而不是 slash command
- **THEN** 系统 MUST NOT 调用 Planning Agent
- **AND** 系统 MUST NOT 调用 LLM
- **AND** 系统 MUST 返回简洁提示，要求用户使用 `/plan approve`、`/plan continue <反馈>`、`/plan reject` 或 `/plan cancel`

#### Scenario: Gated response does not repeat plan
- **WHEN** pending/presented plan 已经展示过
- **AND** 用户输入普通自然语言
- **THEN** 系统返回内容 MUST NOT 包含完整计划正文
- **AND** 系统 MUST NOT 追加新的 `plan_event(presented)` 或 `plan_event(updated)`

#### Scenario: Continue command bypasses gate
- **WHEN** 当前 mode 为 `PLAN`
- **AND** 当前 session 存在 pending/presented plan
- **AND** 用户执行 `/plan continue <反馈>`
- **THEN** 系统 MUST 将反馈交给 Planning Agent
- **AND** Planning Agent MAY 更新或重新展示当前 plan

#### Scenario: Approval command bypasses gate
- **WHEN** 当前 mode 为 `PLAN`
- **AND** 当前 session 存在 pending/presented plan
- **AND** 用户执行 `/plan approve`
- **THEN** 系统 MUST 允许批准计划并交接到 ReAct 执行
