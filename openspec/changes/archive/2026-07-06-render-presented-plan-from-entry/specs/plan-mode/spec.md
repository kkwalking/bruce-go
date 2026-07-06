## ADDED Requirements

### Requirement: Presented plan renders from session entry
系统 MUST 将 `plan_event(presented).content` 作为 TUI 展示完整 markdown 计划的权威来源。

#### Scenario: TUI renders presented plan from runtime event
- **WHEN** Planning Agent 已创建或更新计划
- **AND** runtime 成功追加 `plan_event(presented)`，且该 event 包含非空 `content`
- **THEN** TUI MUST 根据该 `plan_event(presented).content` 展示完整 markdown 计划
- **AND** TUI MUST NOT 依赖 assistant final message 或 command output 作为完整计划正文来源

#### Scenario: Presented plan render preserves content
- **WHEN** `plan_event(presented).content` 包含 markdown 标题、列表、代码块或表格文本
- **THEN** TUI MUST 展示该 content 的完整文本
- **AND** 展示内容 MUST NOT 被替换为 LLM 摘要或工具调用摘要

#### Scenario: Intermediate plan events do not render full plan
- **WHEN** session 追加 `plan_event(created)` 或 `plan_event(updated)`
- **AND** 尚未追加对应的 `plan_event(presented)`
- **THEN** TUI MUST NOT 将该中间事件渲染为用户审阅用的完整计划正文

### Requirement: Session replay restores presented plan display
系统 MUST 在 `/resume` 或 session replay 时从 active path 中的 `plan_event(presented)` 恢复 TUI 计划展示。

#### Scenario: Resume shows latest presented plan
- **WHEN** 用户恢复一个包含 pending presented plan 的 session
- **AND** active path 中最近一次 `plan_event(presented)` 包含非空 `content`
- **THEN** TUI MUST 在回放历史时展示该 presented plan 的完整 markdown 内容
- **AND** 恢复逻辑 MUST NOT 把该计划正文加入 LLM history messages

#### Scenario: Replay preserves timeline order
- **WHEN** active path 中同时包含普通 message entry 和 `plan_event(presented)` entry
- **THEN** TUI replay MUST 按 active path 顺序渲染普通消息和 presented plan
- **AND** presented plan MUST 出现在对应 session entry 的时间线位置

### Requirement: Planning Agent final response is concise after saving plan
系统 MUST 在 Plan Mode prompt 中要求 Planning Agent 保存计划后只输出简短状态，不复述计划正文。

#### Scenario: Prompt forbids plan summary after replace
- **WHEN** Planning Agent 通过 `replace_plan` 创建或替换计划
- **THEN** Plan Mode prompt MUST 指示模型最终回复只输出简短状态
- **AND** prompt MUST 指示模型不要重复、摘要、改写或节选 markdown 计划正文
- **AND** prompt MUST 告知模型完整计划会由系统根据 plan event 单独展示

#### Scenario: Prompt forbids plan summary after edit
- **WHEN** Planning Agent 通过 `edit_plan` 更新计划
- **THEN** Plan Mode prompt MUST 指示模型最终回复只输出简短状态
- **AND** prompt MUST 指示模型不要重复、摘要、改写或节选 markdown 计划正文

### Requirement: No heuristic folding of assistant final message
系统 MUST NOT 在本变更中增加基于文本内容猜测的 assistant final message 折叠逻辑。

#### Scenario: Assistant message remains ordinary assistant output
- **WHEN** assistant final message 非空
- **THEN** TUI MAY 按普通 assistant message 展示该内容
- **AND** TUI MUST NOT 通过启发式判断该内容是否像计划正文来自动折叠或删除它
- **AND** 完整计划块仍 MUST 只由 `plan_event(presented).content` 驱动
