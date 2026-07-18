## Why

当前 `read-only` 和 `workspace-write` 只约束 Shell 与内置文件工具，MCP 工具调用和 stdio MCP 进程仍以宿主权限运行，因此 Agent 可以绕过沙箱修改 workspace 或宿主文件。本期需要把 MCP 纳入同一安全边界，使界面展示的 sandbox mode 成为不可被工具来源绕过的权限上限。

## What Changes

- 为所有工具引入统一的沙箱访问策略，并在 HITL 之前和参数修改之后执行 fail-closed 校验，而不是继续按内置工具名特判。
- 在受限模式中隐藏并拒绝没有可信访问策略、无法被本地沙箱约束或声明能力超过当前模式的 MCP 工具；直接调用和旧工具引用同样不能绕过检查。
- 让 stdio MCP 进程使用与当前模式一致的 Seatbelt 或 Bubblewrap 策略运行，并在 sandbox mode 切换时安全停止、重启和刷新其工具。
- 对 Bruce 无法约束服务端文件访问的 Streamable HTTP MCP 默认采用保守策略，只允许用户明确配置为可信只读或符合当前写入边界的工具。
- MCP `readOnlyHint` 等服务端 annotations 只作为风险提示，不作为独立授权依据；缺失或不可信的声明按未知副作用处理。
- 扩展 sandbox/MCP 状态、错误信息、文档以及 macOS、Linux 和 fake transport 测试，明确哪些 MCP 工具当前可用、被拒绝或受原生沙箱保护。
- **BREAKING**：原先可在 `read-only` 或 `workspace-write` 中调用的未分类、未受约束 MCP 工具将默认不可用；`full-access` 保持现有兼容行为。

## Capabilities

### New Capabilities

- `mcp-sandboxing`: 定义 MCP 工具授权、stdio 进程原生隔离、HTTP MCP 保守策略、模式切换生命周期与可观察性要求。

### Modified Capabilities

无。

## Impact

- 主要影响 `internal/tool` 的统一工具策略、`internal/mcp` 的工具元数据与 transport 生命周期、`internal/sandbox` 的长驻进程启动能力，以及 `internal/integrated` 的模式切换协调。
- MCP server 配置将增加受限模式下的显式工具访问策略，并保持旧配置可加载但按安全默认值处理。
- README、sandbox 需求/设计文档和跨平台 sandbox CI 需要更新。
- 不新增外部运行时依赖，继续复用 macOS Seatbelt 与 Linux Bubblewrap；`full-access` 行为不变。
