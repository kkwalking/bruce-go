## Why

Bruce Go 当前的 `/plan` 模式是早期 Plan-and-Execute 移植：它让模型生成 JSON DAG，然后立即执行文件写入和命令。这与 Claude Code CLI 的 plan mode 预期不一致；后者是一个只读研究流程，先产出可审阅计划，只有用户明确批准后才进入执行。

现有行为把规划、执行、失败重规划混在一条不透明路径里，真实编码任务中可控性弱，也难以配合 Bruce Go 已有的 session/tree/resume 体系做恢复和追溯。

## What Changes

- **BREAKING**: 将 `/plan` 从 Plan-and-Execute 自动执行语义改为 Claude Code 风格的计划语义。
- 新增 Planning Agent 路径：允许用只读工具探索 workspace 并维护 markdown 计划，但不能修改项目源文件或运行会修改 workspace 的命令。
- 将 markdown 计划持久化到 `~/.bruce/plans/`，包含稳定的 plan ID、revision 和内容 hash。
- 新增 plan 生命周期 session event，让 `session/*.jsonl` 记录计划创建、更新、展示、批准、拒绝、取消和执行交接。
- `/resume` 从 session active path 同时恢复当前 mode 和 pending plan 状态，让恢复后的会话像从未中断。
- 新增 plan 专用工具，只允许读取和修改当前 active markdown 计划文件。
- 更新 CLI help、status/session/tree 渲染、README/docs 和测试，描述新的 plan mode 行为。
- 旧 DAG Plan-and-Execute 不再作为用户可见 `/plan` 的主路径；若无引用可删除，或仅保留为内部实现。

## Capabilities

### New Capabilities
- `plan-mode`: 定义 Claude Code 风格 plan mode、markdown 计划持久化、计划审批生命周期事件和 resume 恢复行为。

### Modified Capabilities
- None.

## Impact

- 受影响运行时：`internal/integrated`、`internal/runtime`、`internal/agent`、`internal/tool`、`internal/session`、`internal/event`、`internal/tui`、`internal/cli`、`internal/render`。
- 受影响持久化数据：session JSONL 新增 plan 生命周期事件，`~/.bruce/plans/*.md` 存储计划 artifact。
- 受影响用户行为：`/plan`、`/react`、`/resume`、`/session`、`/tree`、`/status`、审批提示、CLI help 和文档。
- 测试影响：新增 plan 工具、plan event 持久化、resume 重建、命令/工具限制、审批交接的单元和集成测试。
