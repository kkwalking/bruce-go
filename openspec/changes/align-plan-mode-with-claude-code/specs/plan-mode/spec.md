## ADDED Requirements

### Requirement: Claude Code-style plan mode
系统 MUST 将 `/plan` 作为只读 planning workflow，而不是自动执行 workflow。

#### Scenario: Enter plan mode
- **WHEN** 用户执行 `/plan`
- **THEN** 系统 MUST 追加 `mode_change` session entry，当前 mode MUST 变为 `PLAN`
- **AND** 后续普通任务 MUST 由 Planning Agent 处理

#### Scenario: Enter plan mode with description
- **WHEN** 用户执行 `/plan <description>`
- **THEN** 系统 MUST 切换到 `PLAN`
- **AND** 系统 MUST 将 `<description>` 作为第一条规划任务交给 Planning Agent

#### Scenario: Plan mode does not auto-execute workspace changes
- **WHEN** 用户在 plan mode 中请求代码改动
- **THEN** 系统 MUST 产出或更新待审批计划
- **AND** 系统 MUST NOT 修改项目源文件
- **AND** 系统 MUST NOT 运行会修改 workspace 的命令

### Requirement: Planning Agent read-only exploration
系统 MUST 在 plan mode 中只向 Planning Agent 暴露只读探索工具和 plan 专用工具。

#### Scenario: Read-only file inspection is allowed
- **WHEN** Planning Agent 需要检查已有项目文件
- **THEN** 系统 MUST 允许使用只读文件读取和搜索工具

#### Scenario: Source file edits are blocked
- **WHEN** Planning Agent 尝试调用 `write_file`、`edit_file` 或等价项目写入工具
- **THEN** 系统 MUST 拒绝该工具调用
- **AND** 拒绝结果 MUST 说明 plan mode 不允许修改项目文件

#### Scenario: Mutating commands are blocked
- **WHEN** Planning Agent 尝试执行不在 plan-mode 只读 allowlist 中的 shell 命令
- **THEN** 系统 MUST 拒绝该命令
- **AND** 拒绝结果 MUST 引导模型改用只读探索方式

### Requirement: Markdown plan persistence
系统 MUST 将 Planning Agent 维护的计划保存为 `~/.bruce/plans/` 下的 markdown 文件。

#### Scenario: Create plan file
- **WHEN** Planning Agent 首次为当前任务创建计划
- **THEN** 系统 MUST 分配稳定 plan ID
- **AND** 系统 MUST 在 `~/.bruce/plans/` 下创建对应 markdown 文件
- **AND** 系统 MUST 记录 revision 和 sha256

#### Scenario: Update plan file
- **WHEN** Planning Agent 修改当前计划
- **THEN** 系统 MUST 只允许修改当前 active plan 文件
- **AND** 系统 MUST 原子写入新内容
- **AND** 系统 MUST 增加 revision 并重新计算 sha256

### Requirement: Plan-specific tools
系统 MUST 提供只允许操作当前 active plan 的 plan 专用工具。

#### Scenario: Edit active plan
- **WHEN** Planning Agent 调用 plan 编辑工具
- **THEN** 工具 MUST 验证目标是当前 active plan
- **AND** 工具 MUST 验证目标路径位于 `~/.bruce/plans/` 内
- **AND** 工具 MUST NOT 跟随 symlink 跳出计划目录

#### Scenario: Record plan tool update
- **WHEN** plan 专用工具成功写入计划
- **THEN** 系统 MUST 追加 `plan_event` session entry
- **AND** entry MUST 包含 plan ID、action、revision、sha256 和计划路径

### Requirement: Plan lifecycle session events
系统 MUST 将计划生命周期事件作为一等 session entry 保存到 session JSONL。

#### Scenario: Record plan lifecycle
- **WHEN** 计划被创建、更新、展示、批准、拒绝、取消或交接执行
- **THEN** 系统 MUST 追加对应 `plan_event`
- **AND** 该 entry MUST 位于当前 session active path 上

#### Scenario: Preserve key snapshots
- **WHEN** 系统记录 created、presented、approved、rejected 或 canceled 事件
- **THEN** `plan_event` MUST 包含对应 markdown 内容快照或可恢复等价内容

### Requirement: Resume restores plan state
系统 MUST 在 `/resume` 时从 session active path 恢复当前 mode 和 active plan state。

#### Scenario: Resume pending plan
- **WHEN** session active path 的最后计划状态是 `presented` 且没有后续 `approved`、`rejected` 或 `canceled`
- **THEN** `/resume` 后系统 MUST 保留 pending plan
- **AND** 如果当前 mode 为 `PLAN`，系统 MUST 允许用户继续批准、修改、拒绝或继续规划

#### Scenario: Resume approved plan
- **WHEN** session active path 中已有 `plan_event(approved)`
- **THEN** `/resume` 后系统 MUST 能展示最近批准的计划状态
- **AND** 系统 MUST NOT 将该计划错误恢复为 pending

#### Scenario: Mode is restored independently
- **WHEN** `/resume` 重放 session active path
- **THEN** 当前 mode MUST 由 `mode_change` entry 决定
- **AND** 系统 MUST NOT 只依赖 `plan_event` 推断当前 mode

### Requirement: Explicit plan approval handoff
系统 MUST 只有在用户明确批准计划后才执行计划对应的项目修改。

#### Scenario: Continue planning with feedback
- **WHEN** 用户执行 `/plan continue <feedback>`
- **THEN** 系统 MUST 保持 `PLAN` mode
- **AND** Planning Agent MUST 使用 feedback 更新或重新展示当前 plan

#### Scenario: Approve plan
- **WHEN** 用户执行 `/plan approve` 批准当前 presented plan
- **THEN** 系统 MUST 追加 `plan_event(approved)`
- **AND** 系统 MUST 将 mode 切换到 `REACT`
- **AND** 系统 MUST 将批准的 markdown 计划注入执行上下文

#### Scenario: Reject or cancel plan
- **WHEN** 用户执行 `/plan reject [reason]` 或 `/plan cancel`
- **THEN** 系统 MUST 追加 `plan_event(rejected)` 或 `plan_event(canceled)`
- **AND** 系统 MUST NOT 执行计划中的项目修改

### Requirement: User-visible session trace
系统 MUST 在 session/tree/status 等用户可见输出中呈现计划状态。

#### Scenario: Tree shows plan events
- **WHEN** 用户执行 `/tree`
- **THEN** 输出 MUST 显示 plan lifecycle 节点的简短标签，包括 action、plan ID 和 revision

#### Scenario: Status shows pending plan
- **WHEN** 当前 session 存在 pending plan
- **THEN** `/status` 或等价状态渲染 MUST 显示 pending plan ID、revision 和路径
