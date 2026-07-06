## Context

当前 `/plan` 在 `internal/integrated.Runtime.RunTask` 中直接调用 `internal/plan.Agent.Run`。该路径会让 LLM 生成 JSON DAG，并由 executor 自动执行 `read_file`、`write_file`、`execute_command` 等任务。它更像批处理执行器，不像 Claude Code 的 plan mode。

Bruce Go 已有几块可以复用的基础：

- `runtime.AgentMode` 已有 `REACT` 和 `PLAN`。
- `session.Store` 已有 JSONL 分支树、`mode_change`、`custom`、`custom_message`、`compaction` 和 `/resume` active path 重建能力。
- `approval.Handler` 和 TUI 审批层已能表达 approve/reject/skip/modify/approve all。
- `tool.Registry` 已集中管理工具定义、执行、HITL 和命令 guard。

这次设计的关键是把 plan mode 做成一个可恢复的只读 planning workflow：计划正文存为 markdown 文件，计划生命周期存为 session entry，mode 仍由 `mode_change` 独立决定。

## Goals / Non-Goals

**Goals:**

- `/plan` 进入 Claude Code 风格计划模式，不自动修改项目源文件。
- Planning Agent 能探索代码库并维护一份 markdown 计划。
- `~/.bruce/plans/` 存储计划文件，session JSONL 记录每次计划生命周期事件。
- `/resume` 能恢复 mode、pending plan、计划 revision 和审批状态。
- 用户批准计划后才进入执行，执行使用普通 ReAct 路径和现有审批/工具机制。
- 计划工具只允许操作当前 active plan 文件，不能作为任意文件写入后门。

**Non-Goals:**

- 不实现多用户协作或远程同步计划。
- 不保留 `/plan` 的自动 DAG 执行语义作为用户可见行为。
- 不要求计划 markdown 支持复杂结构化 schema；普通 markdown 即可。
- 不把所有 shell 命令做成完美静态安全分析；先提供保守 allowlist/blocklist。

## Decisions

### Decision: mode 和 plan lifecycle 分离

`mode_change` 继续作为当前运行模式的唯一来源；新增 `plan_event` 只描述计划生命周期。`/resume` 重放 active path 时分别计算当前 mode 和 active plan state。

替代方案是从 `plan_event` 推断 mode。这个方案较脆弱：用户可能退出 plan 但保留未批准计划，也可能批准计划后还未开始执行。分离后 session 时间线更清晰。

### Decision: 新增 plan_event session entry

在 `session.Entry` 中增加 plan 相关数据，或增加等价的 typed entry。建议字段：

```json
{
  "type": "plan_event",
  "id": "e_xxx",
  "parentId": "e_prev",
  "timestamp": "...",
  "plan": {
    "id": "plan_xxx",
    "path": "~/.bruce/plans/plan_xxx.md",
    "action": "created|updated|presented|approved|rejected|canceled|handoff",
    "revision": 3,
    "sha256": "...",
    "summary": "用户要求补充测试策略",
    "content": "可选 markdown 快照"
  }
}
```

关键事件建议保存完整 markdown 快照，至少包括 `created`、`presented`、`approved`、`rejected`、`canceled`。这样即使计划文件丢失，session 仍能恢复最后状态。

### Decision: markdown 计划文件是可编辑 artifact，session 是事件账本

计划正文持久化到 `~/.bruce/plans/<plan-id>.md`，文件内容便于用户查看、diff 和手动审阅。session JSONL 不只存文件路径，还存 action、revision、hash 和必要快照，用于追溯和 resume。

如果只存文件，会丢失审批过程；如果只存 session，不方便用户直接打开计划文件。双轨存储更符合现有 session 体系。

### Decision: 给 Planning Agent 窄口径 plan tools

新增 plan 专用工具，例如：

- `read_plan`: 读取当前 active plan。
- `replace_plan`: 原子替换当前 active plan 内容。
- `edit_plan`: 基于唯一匹配文本修改当前 active plan。
- 可选 `append_plan_section`: 追加计划小节。

这些工具只在 plan mode 暴露，只能操作当前 active plan ID 绑定的路径，路径必须解析到 `~/.bruce/plans/` 下，且不能跟随 symlink 跳出目录。每次写入后更新 revision、sha256，并追加 `plan_event(updated)`。

替代方案是允许 Planning Agent 使用 `write_file` 写计划文件，但这会扩大权限面，也容易绕过只读模式。

### Decision: 计划模式使用受限工具视图

Planning Agent 获取的工具定义应是普通 registry 的受限视图：

- 允许 `read_file`、只读搜索/列目录命令、web search/fetch、只读 MCP 工具、plan tools。
- 禁止 `write_file`、`edit_file`、创建项目、会写 workspace 的 MCP 工具。
- 对 `execute_command` 使用 plan-mode guard，只允许明显只读命令，例如 `ls`、`pwd`、`find`、`rg`、`grep`、`sed -n`、`head`、`tail`、`cat`、`git status/log/show/diff`。

命令 guard 保守拒绝不确定命令。被拒绝时返回清晰信息，让模型改用只读方式探索。

### Decision: 批准后交给 ReAct 执行

用户批准计划时追加 `plan_event(approved)`，随后追加 `mode_change(REACT)`，并把批准的 markdown 计划作为系统/上下文注入普通 ReAct 执行路径。执行时使用现有 HITL 和工具机制，而不是复用旧 DAG executor。

这让实际代码修改仍走已有成熟路径，避免维护两套执行引擎。

### Decision: 初版使用 slash commands 驱动计划审批

初版在 CLI/TUI 统一支持 `/plan approve`、`/plan reject [reason]`、`/plan cancel`、`/plan continue [feedback]`。`/plan <description>` 切换到 plan mode，并把 description 作为第一条规划任务立即交给 Planning Agent；纯 `/plan` 只切换模式。

替代方案是先做专用 TUI plan review 面板，但这会把核心运行时和 UI 复杂度绑在一起。slash commands 能先验证 session/resume/权限模型，TUI 可以在此基础上提供快捷键和更好的渲染。

### Decision: 执行审批复用现有 HITL

初版不区分 `approved/current` 和 `approved/manual`。计划批准只表示“可以进入执行路径”，实际文件修改和命令执行仍按当前 HITL 配置处理。

这样用户心智更简单：plan approval 审批的是方向和步骤，HITL 审批的是具体危险操作。

## Risks / Trade-offs

- 命令只读判断可能误杀合法探查命令 -> 采用保守策略并给出可恢复错误，后续可逐步扩展 allowlist。
- 双轨存储可能出现计划文件和 session hash 不一致 -> `/resume` 检测 hash，不一致时优先使用 session 快照并提示活动信息。
- 每次 plan update 存完整快照会增加 session 体积 -> 初期可接受；计划通常远小于工具 transcript，必要时只对关键事件存快照。
- 新 plan_event 需要 TUI/tree/session 渲染适配 -> 先提供简洁 label，保证可追溯，再迭代 UI 细节。
- 旧 `internal/plan` 删除可能影响迁移文档和测试 -> 先把用户可见路径切走，再清理或标记遗留实现。

## Migration Plan

1. 添加 plan state、plan_event、plan store 和 plan tools。
2. 将 `/plan` runtime 路径切到 Planning Agent，不再调用旧 DAG executor。
3. 实现 approve/reject/cancel/continue planning 的命令或 TUI action。
4. 更新 `/resume`、`/session`、`/tree`、`/status` 读取和渲染 plan state。
5. 更新 README/docs/tests。
6. 确认无用户可见引用后删除或隔离旧 `internal/plan`。

回滚策略：保留旧 `runtime.ModePlan` 枚举和旧代码直到新路径测试稳定；如出现阻塞，可临时把 `/plan` 命令指回旧路径，但不应长期保留双重语义。

## Open Questions

- 是否需要在后续版本增加专用 TUI plan review 面板和快捷键？
- 是否需要为计划文件增加用户可配置保留策略或清理命令？
