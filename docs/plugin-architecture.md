# Bruce Go Plugin System — 架构改动总结与实现计划

> 本文对应 `docs/plugin.md` 第「四十、工作方式要求」：在修改代码前先描述现有架构、
> 影响面、实现计划、必须保持不变的行为与安全边界。实现完成后的用户文档见
> [plugins.md](plugins.md)。

## 1. 当前相关架构（以仓库实际代码为准）

| 关注点 | 现状 | 位置 |
|---|---|---|
| Tool 定义 | `Tool{Name,Description,Parameters,Exec,PromptSnippet,PromptGuidelines,Policy}` | `internal/tool/tool.go:61` |
| Tool 参数 | **扁平** `map[string]string`；`ParseArguments` 把非字符串值 `json.Marshal` 成字符串 | `internal/tool/tool.go:25,577` |
| Tool 注册表 | `Registry` 持有 `map[string]Tool`，`Register/Unregister/Lookup/Subset/Definitions/BuildPrompt` | `internal/tool/tool.go:71` |
| Tool 策略 | `Policy{Source, MinimumMode, RequiresNetwork, ParallelSafe}`；沙箱校验按 `MinimumMode` 比较 | `internal/tool/tool.go:54,520` |
| 审批 | `approval.RequiresApproval(toolName)` 按 **工具名硬编码 + `mcp_` 前缀** 判断 | `internal/approval/approval.go:107` |
| 执行 | `prepare`（策略校验→HITL→再校验）→ `executePrepared`（再校验→调用 `Exec`）→ 截断 | `internal/tool/tool.go:363,421` |
| 并行 | `ParallelExecutor`：按 `Policy.ParallelSafe` 分段，worker pool + 批次超时 | `internal/tool/tool.go:1093` |
| 沙箱 | `sandbox.Manager`：Mode（read-only/workspace-write/full-access）+ NetworkAccess + 原生后端探测 | `internal/sandbox/manager.go` |
| MCP | `mcp.RegisterTools` 把远端工具注册成 `mcp_<server>_<tool>`，带 `MinimumMode`/`RequiresNetwork` | `internal/mcp/tool_registry.go:96` |
| Skill | `skill.Catalog`：用户级 + 项目级目录，项目级覆盖用户级，`RegisterTools` 注册 `load_skill`/`read_skill_resource` | `internal/skill/skill.go:57,301` |
| 事件总线 | `event.Bus`：`Subscribe/Listen`, 监听器 panic 被吞掉 | `internal/event/event.go:28` |
| Agent | ReAct/Plan/Minimal 三个 `agent.Agent`，ReAct 走 `Executor.Execute` | `internal/agent/agent.go:163` |
| Runtime | `integrated.Runtime`：装配 sandbox/registry/web/skill/mcp/session/event，`HandleCommand` 大 switch | `internal/integrated/runtime.go:96,377` |
| CLI | `cli.Commands` 静态表 + `Parse`；补全在 `internal/tui/completion.go` | `internal/cli/cli.go:57` |
| 取消 | 全链路 `context.Context`；工具批次超时/取消已有明确状态 | `internal/tool/tool.go:1093` |

## 2. 影响面

改动集中在三处，其余是接入点：

1. **`internal/tool`（参数模型 + 策略元数据 + interceptor 钩子）** — 唯一有回归风险的
   改动。`map[string]string` → `map[string]any` 会触及 `web`、`skill`、`mcp`、`planning`、
   `plan` 六个注册方与 `approval` 的判定入口。
2. **新增 `internal/jsengine`（引擎抽象 + Pool）与 `internal/jsengine/moejs`（唯一 adapter）**
   — moejs 只出现在 adapter 包内。
3. **新增 `internal/plugin`（manifest/discovery/lifecycle/reload/hostapi/hooks/commands/storage/errors）**
   — Plugin Core 不依赖 moejs。
4. 接入点：`internal/integrated/runtime.go`（装配、`/plugin` 命令、status）、
   `internal/cli`（命令注册表化）、`internal/tui/completion.go`（插件命令补全）、
   `internal/render`（插件列表渲染）、`internal/config`（`plugins` 配置节）。

## 3. 必须保持不变的行为（回归基线）

- 未安装任何插件时：`Tool` 集合、LLM tool definition、系统提示、审批触发集合、
  沙箱判定结果与引入插件系统前**完全一致**。
- `go test ./...`、`go test -race ./...`、`make check` 全绿。
- 现有 tool 名与参数语义不变（`read_file`/`write_file`/`edit_file`/`execute_command`/
  `web_search`/`web_fetch`/`load_skill`/`read_skill_resource`/`mcp_*`/plan 工具）。
- HITL 的 `AutoHandler`、审批展示、`/hitl`、`/sandbox`、`/parallel` 行为不变。
- MCP 工具名、`MinimumMode`/`RequiresNetwork` 判定不变。
- 会话持久化、compaction、checkpoint 不变。

## 4. 安全边界

```
JavaScript Plugin
   │  只能用 Host API 暴露的对象（无 os/exec/net/env/Go 对象引用）
   ▼
Bruce Host Capability（internal/plugin/hostapi）
   │  每次调用先查 Plugin 声明的 capability × Bruce Policy
   ▼
Permission / Policy（tool.Policy + plugin.Policy + sandbox.Status）
   │  参数在 hook 修改后重新校验
   ▼
Sandbox / HITL（sandbox.Manager、approval.Handler）
   │
   ▼
Real Operation
```

- JS VM（moejs）**不是**安全边界，只是执行引擎；真正边界是 Host API + Policy + Sandbox + HITL。
- 默认拒绝：文件系统、网络、shell、子进程、环境变量、任意模块导入、动态代码。
- `DisableDynamicCode: true` 关闭 `eval` / `Function` / `EvalScript`。
- import resolver 只解析插件目录内的相对模块 + `bruce:*` virtual modules；
  解析符号链接后再判定根目录归属，阻止路径穿越。
- Hook 修改后的数据重新走 schema/权限/沙箱/HITL 校验。

## 5. 实现计划（按 `docs/plugin.md` 第三十九节顺序，Phase 1→4 全做）

Phase 1（MVP 必做）
1. `internal/tool`：参数模型升级为 `map[string]any`；新增 `Capability` 显式策略元数据
   （`Source` 增加 `plugin`、`RequiresApproval`、`FilesystemRead/Write`、`Network`、
   `Shell`、`WorkspaceScope`、`MinimumMode`、`ParallelSafe`、`Risk`）；
   `approval` 改由 Policy 驱动（保留旧名兜底，保证零回归）。
2. `internal/jsengine`：`Engine`/`Runtime`/`Module`/`Hook` 抽象 + `Pool`（容量、acquire、
   release、cancellation、corrupted discard、unload）。
3. `internal/jsengine/moejs`：唯一 adapter。
4. `internal/plugin`：manifest schema + 校验、discovery（workspace 覆盖 user）、
   Tool 注册、生命周期、reload（generation 模型）、错误类型分类。
5. Host API + import resolver + Storage + 可观测性事件。

Phase 2：HookManager（observer/interceptor）、ordering、failure policy、二次校验。
Phase 3：Command Registry，内置命令与插件命令统一抽象，冲突策略。
Phase 4：Storage scope、更丰富的 Host API。

## 6. 与后续 Phase 的边界（第一版明确不做）

Node.js/npm/node_modules/package.json、Bun、Pi/OpenCode 插件兼容、Node `fs`/`process`/
`child_process`、无限制 shell/网络/文件系统、TypeScript 运行时、HMR 框架、远端插件市场。
TypeScript 只走「预编译成 JS」路线。

## 7. 实现期间发现并修复的真实缺陷

实现与验证过程中发现了四个缺陷，都已修复并补了回归测试。记录在此，因为它们都是
"看起来没问题"的那类错误。

### 7.1 动态代码默认值是反的（安全）

`internal/plugin/manager.go` 的 `NewManager` 原来写的是：

```go
dynamic := true                      // 错：默认允许 eval
if opts.DisableDynamicCode != nil {
    dynamic = !*opts.DisableDynamicCode
}
```

`DisableDynamicCode` 的文档说"默认关闭动态代码"，但字段为 `nil`（未设置）时代码
反而打开了 `eval` 与 `Function`。也就是说，**任何没有显式传这个选项的调用方都会
静默拿到允许动态代码的运行时**——而 `integrated.Runtime` 恰好就是通过指针传值，
所以 CLI 路径侥幸正确，库调用方则不安全。

修复：默认改为 `false`（动态代码关闭），只有显式传入 `DisableDynamicCode: false`
才开启。回归测试 `TestDynamicCodeIsDisabledByDefault` 直接断言默认行为，
`TestDynamicCodeCanBeEnabledExplicitly` 断言开关仍然有效。

### 7.2 声明的 per-tool 超时被忽略

`timeoutMs` 已经写进 `tool.Policy.Timeout`，但 `invokeTool` → `invoke` 的调用链
没有把它传下去，`invoke` 永远用 `DefaultInvocationTimeout`（30s）。结果是插件
声明的 `"timeoutMs": 200` 完全不生效。

修复：把 `timeout` 沿调用链传下去，`invoke` 在收到 0 时才回落到默认值。
回归测试 `TestInvocationTimeoutAppliesWithoutCallerDeadline`。

### 7.3 永不 settle 的 promise 被静默当成 `{}` 成功返回

一个 `async` handler 返回 `new Promise(() => {})`（或 await 一个宿主永远不会
resolve 的东西）时，引擎把 promise 对象序列化成 `{}`，管理器把它当作**成功的
空结果**交给模型。模型看到的是"工具跑成功了，什么也没返回"，而实际工作从未发生。

修复：在 moejs adapter 里用 `PromiseResult` 读取 promise 状态——已 fulfilled 取
其值，已 rejected 报异常并带上拒绝原因，仍 pending 则报 `CategoryMalformedResult`
并说明原因。回归测试 `TestNeverSettlingPromiseIsReportedNotSilentlyEmpty`，
对照组 `TestAsyncHandlerResolves` 证明正常的 async handler 仍然工作。

### 7.4 ExtraGlobals 的 `bruce` 键会覆盖内建命名空间

`globals()` 用 `globals[name] = value` 合并额外全局对象，于是
`ExtraGlobals["bruce"]` 会**整体替换**内建的 `bruce` 命名空间，把 `storage` 和
`events` 一起删掉。这个 bug 只在宿主扩展 `bruce` 时触发，但触发时表现是"插件的
宿主函数不见了"，很难从现象反推。

修复：`bruce` 键改为逐项合并而不是替换。回归测试
`TestAsyncHandlerResolves`（它依赖 `bruce.double` 与内建命名空间共存）。

## 8. 与 docs/plugin.md 验收标准的对应

| 验收项 | 验证方式 |
|---|---|
| MVP 1–17 条 | `internal/integrated/plugin_acceptance_test.go` 逐条编号断言 |
| 安全验收（第三十六节） | `internal/plugin/security_test.go` + `resolver_test.go` + `hooks_test.go` |
| Manifest 测试（第二十五节） | `internal/plugin/manifest_test.go`、`discovery_test.go` |
| Tool 集成（第二十六节） | `manager_test.go`、`plugin_acceptance_test.go` |
| Runtime Pool（第二十八节） | `internal/jsengine/pool_test.go`（`-race`） |
| Cancellation（第二十九节） | `internal/plugin/cancellation_test.go` |
| Hook（第三十节） | `internal/plugin/hooks_test.go` |
| 隔离（第三十一节） | `manager_test.go` 的 `TestPluginIsolation`、`hooks_test.go` |
| Reload（第三十二节） | `manager_test.go`、`hooks_test.go`、`commands_test.go` |
| 回归（第三十三节） | 全仓库 `go test ./...`、`-race`、`make sandbox-test` |
| 性能（第三十四节） | `internal/plugin/bench_test.go` |
