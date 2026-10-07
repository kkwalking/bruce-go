你正在为 Go 项目 **bruce-go** 设计并实现一套 JavaScript 插件机制。

项目仓库：

- bruce-go: https://github.com/kkwalking/bruce-go
- moejs: https://github.com/Calcium-Ion/moejs

开始工作前，请完整阅读 bruce-go 当前架构，尤其关注：

- Agent / ReAct / Plan 执行流程
- Tool Registry 与 Tool Executor
- MCP 工具注册机制
- Skill 机制
- Event Bus
- HITL / Approval
- Sandbox 与 Network Policy
- Integrated Runtime
- CLI / Slash Command
- Context cancellation
- 并行 tool execution

同时阅读 moejs 的：

- Runtime 生命周期
- Module compile/link
- Runtime Pool
- Module Hook / named export
- Go ↔ JavaScript value conversion
- async/await
- import resolver
- Interrupt
- DisableDynamicCode
- frozen builtins
- Runtime 并发限制
- Runtime value 生命周期限制

你的任务不是简单地“在 Go 中运行 JS”，而是为 Bruce 构建一套**可长期演进、安全、轻量、单 binary、适合 Coding Agent 的 JavaScript Plugin System**。

---

# 一、总体目标

为 bruce-go 增加 JavaScript 插件能力，使第三方插件可以扩展 Bruce 的行为，同时继续复用 Bruce 已有的 Agent、Tool、Sandbox、HITL、MCP、Skill 和事件体系。

插件系统目标参考 Pi Coding Agent / OpenCode 的 extension/plugin 体验，但：

**不要求兼容 Pi 或 OpenCode 的现有插件。**

目标应定义为：

> Bruce Plugin API inspired by Pi/OpenCode，而不是 Pi/OpenCode compatibility layer。

moejs 只作为：

> JavaScript VM / Plugin Execution Engine

不能让 moejs 自身承担 Bruce 的权限、安全、Tool Registry、Sandbox 或 Agent Runtime 职责。

Bruce 必须始终是最终宿主和安全边界。

---

# 二、核心架构原则

实现必须遵守以下原则。

## 2.1 插件不能形成第二套 Agent Runtime

JavaScript 插件只能作为 Bruce 现有架构的扩展。

插件定义的 Tool 必须最终进入 Bruce 原有 Tool Registry，并继续经过 Bruce 原有：

- Tool 调度
- Agent 调用
- 并发控制
- Sandbox
- HITL / Approval
- Network Policy
- Cancellation
- Logging / Events

不能建立一套与现有 Tool 系统平行的 JS Tool Executor。

---

## 2.2 moejs 必须被隔离在单独的 VM Adapter 层

Bruce 的 Plugin Core 不得直接依赖 moejs 的具体 API。

必须存在明确的 JavaScript Engine abstraction。

目标是：

- moejs API 变化时，不影响 Plugin Core。
- 将来如果替换 JS Engine，不需要重写整个插件系统。
- Agent、Tool、CLI、Sandbox 等模块不得大量出现 moejs 类型。

moejs 只能存在于清晰、有限的 adapter 层。

---

## 2.3 插件能力必须经过 Bruce Host API

JavaScript 插件不得直接获得：

- os.File
- os.Open
- exec.Command
- unrestricted HTTP client
- Go filesystem API
- unrestricted environment variables
- process execution
- raw network socket
- Bruce 内部对象引用

插件访问文件、网络、Shell、Git 等能力时，必须通过 Bruce 提供的受控 Host Capability API。

调用链必须保持：

JavaScript Plugin
→ Bruce Host Capability
→ Permission / Policy
→ Sandbox / HITL
→ Real Operation

插件不得绕过 Bruce 安全模型。

---

# 三、第一阶段必须实现的 Plugin MVP

第一版至少必须支持：

- Plugin discovery
- Plugin manifest
- JavaScript entry module
- JS Tool registration
- Plugin lifecycle
- Runtime pooling
- Context cancellation
- Plugin reload
- Plugin listing
- Plugin validation
- Plugin permissions declaration
- Plugin execution error isolation

建议同时支持：

- 用户级插件目录
- Workspace 级插件目录

Workspace 插件与用户级插件冲突时，必须定义明确、一致、可测试的优先级策略。

---

# 四、Plugin Manifest

每个插件必须拥有静态 manifest。

Manifest 至少描述：

- API version
- Plugin name
- Plugin version
- Plugin description
- JavaScript entry
- Tool declarations
- Hook declarations
- Command declarations
- Requested permissions
- Parallel safety / concurrency constraints
- 可选 metadata

Manifest 必须在插件代码执行前完成完整校验。

错误 manifest 必须：

- 不加载插件
- 输出明确错误
- 不影响其他插件
- 不导致 Bruce 启动失败，除非配置明确要求 fail-fast

---

# 五、Tool 插件机制

JavaScript 插件必须能够向 Bruce 注册 Tool。

Tool 至少具备：

- name
- description
- JSON Schema input
- handler
- permission requirements
- concurrency / parallel safety metadata

Tool schema 必须继续作为 LLM Tool Definition 的来源。

Plugin Tool 对 Agent 来说应该和：

- Builtin Tool
- MCP Tool
- 其他 Bruce Tool

拥有统一调用体验。

Agent 不应该需要知道一个 Tool 是否来自 JS Plugin。

---

# 六、Tool 参数模型升级

当前如果 Bruce Tool Executor 只能表达类似：

map[string]string

的扁平参数模型，需要升级。

插件系统必须完整支持标准 JSON 数据模型：

- string
- number
- boolean
- null
- object
- array
- nested object
- nested array

例如这样的输入必须可以无损传给插件：

{
  "query": "hello",
  "options": {
    "recursive": true,
    "depth": 3
  },
  "files": ["a.go", "b.go"]
}

不能通过字符串化或扁平化破坏结构或类型。

同时必须保证现有 Builtin Tool 不因参数模型升级而产生行为回归。

---

# 七、Tool Policy 与权限模型升级

不要继续依赖：

- Tool 名称
- Tool name prefix
- MCP name prefix

推断 Tool 的风险和权限。

应建立显式 Tool Policy / Capability Metadata。

至少需要表达：

- Tool source
- requires approval
- filesystem read
- filesystem write
- network
- shell/process
- workspace scope
- minimum sandbox mode
- parallel safety
- risk classification

应新增 Plugin 作为 Tool Source。

Manifest 声明的 permission 仅表示：

> Plugin requested permissions

不是插件自动获得权限。

最终权限必须由：

> Bruce Host Policy

决定。

---

# 八、Sandbox 与安全模型

插件必须默认运行在最小权限原则下。

第一版建议默认禁止：

- 任意 filesystem access
- 任意 network access
- shell execution
- subprocess
- arbitrary environment access
- raw Go object access
- arbitrary module imports
- arbitrary dynamic code generation

只有明确声明并被 Bruce Policy 允许的 capability 才能使用。

JS Plugin 即使运行在 moejs sandbox 中，也不能把 JavaScript VM 本身当作完整安全边界。

安全模型必须建立在：

- Host API
- Capability
- Sandbox
- HITL
- Policy

之上。

---

# 九、Dynamic Code

第一版建议默认关闭：

- eval
- Function constructor
- 其他等价动态代码能力

如果 moejs 支持 DisableDynamicCode，应利用该能力。

除非后续明确引入可信插件模式，否则不要默认开启。

---

# 十、Module Import

JavaScript Plugin 必须拥有受控 import resolver。

第一版只允许：

- 插件自身目录内的合法相对模块
- Bruce 提供的 virtual modules

例如未来可能存在：

bruce:api
bruce:storage
bruce:events

不得默认支持：

- npm resolution
- node_modules
- arbitrary system paths
- Node builtins
- Internet module loading

必须阻止路径穿越，例如：

../../../../etc/...

符号链接也不能成为越过插件根目录的简单绕过方式。

---

# 十一、Runtime 生命周期

必须针对 moejs 的 Runtime 特性设计正确生命周期。

基本原则：

- Module 尽可能 compile/link 一次。
- 同一 Module 可以供多个 Runtime 使用。
- Runtime 不允许被多个并发调用同时使用。
- 并发执行通过 Runtime Pool 或等价机制实现。
- JS Value / Function 不得错误地跨 Runtime 保存和复用。
- 调用结束后正确释放 call-local data。
- 污染或异常 Runtime 不应继续进入正常池。

Runtime Pool 必须拥有：

- 最大容量策略
- acquire
- release
- cancellation
- corrupted runtime discard
- plugin unload cleanup

---

# 十二、Plugin Handler 模型

建议采用：

> Static metadata + Named module exports

而不是依赖持久 JS closure。

Tool / Hook / Command 的 handler 应可通过 manifest 指向 JS module 的 named export。

原因：

- 适配 Runtime Pool。
- 避免跨 Runtime 保存函数。
- 让 Plugin Metadata 在执行 JS 前即可发现。
- 易于 validation。
- 易于 reload。
- 易于测试。

即使未来提供类似 registerTool() 的开发体验，底层也应保持可稳定映射到静态 metadata + handler export 的模型。

---

# 十三、Context Cancellation

Bruce 的 context.Context 必须与 JavaScript invocation cancellation 打通。

至少支持：

- 用户 Ctrl-C
- Tool timeout
- Session cancel
- Agent cancellation
- Bruce shutdown

这些事件必须能够终止正在运行的 JS。

如果 moejs 提供 Interrupt，应正确使用。

必须测试：

- JS 死循环
- 长时间计算
- await 状态
- cancellation race
- cancellation 后 Runtime 是否仍安全

取消后不能导致：

- Goroutine 泄漏
- Runtime 永久卡死
- Pool deadlock
- Bruce session 卡死

---

# 十四、Hook System

第一版 Tool MVP 完成后，需要实现 Plugin Hook 机制。

Hook 必须区分：

## Observer Hook

只能观察事件。

例如：

- session.started
- session.ended
- tool.started
- tool.completed
- message.created

这类 hook 可以建立在现有 Event Bus 之上。

## Interceptor Hook

可以修改或阻止流程。

例如：

- chat.before
- tool.before
- tool.after

Interceptor 必须同步、有序、可预测。

---

# 十五、Hook 安全原则

任何 Plugin Hook 对数据进行修改后，必须重新经过 Bruce 的：

- schema validation
- permission validation
- sandbox validation
- HITL validation

例如：

Tool 原始参数：

{
  "path": "src/a.go"
}

插件 hook 修改成：

{
  "path": "/etc/passwd"
}

不能因为原始参数已通过检查，就继续执行。

所有安全检查必须针对：

> 最终实际执行数据

而不是原始数据。

插件不得：

- bypass approval
- grant itself permissions
- disable sandbox
- disable network policy
- change Bruce security mode

---

# 十六、Hook Failure Policy

必须明确不同 Hook 失败时的行为。

至少考虑：

- observer hook error
- before hook error
- after hook error
- timeout
- panic / JS exception
- plugin cancellation

要求行为明确且可测试。

原则：

- Observer 的非关键错误通常不应让 Agent session 崩溃。
- Security-sensitive interceptor failure 应优先 fail closed。
- 一个 Plugin 崩溃不应导致所有 Plugin 失效。
- 错误必须包含 plugin identity 与 hook identity。

---

# 十七、Plugin Command

后续需要允许插件注册 Slash Command。

例如：

/review
/foo
/my-plugin-command

不要直接将插件命令硬编码进 CLI switch。

应该逐步形成统一的 Command Registry，使：

- Builtin Command
- Plugin Command

共享统一抽象。

插件 Command 至少需要：

- name
- description
- handler
- argument representation
- permission requirements

命令冲突必须有明确策略。

Plugin Command 不允许覆盖 Bruce 的安全关键内建命令，除非未来显式设计 override policy。

---

# 十八、Plugin Storage

不要鼓励插件将长期状态保存在 JS module global 中。

因为多个 Runtime 会导致状态不一致。

必须提供显式 Storage abstraction。

未来至少考虑 scope：

- invocation
- session
- plugin
- workspace
- global

第一版可以仅实现必要 scope，但 API 设计不能默认 JS Runtime global 就是持久状态。

不同 Plugin 必须 namespace 隔离。

Plugin A 默认不得读取 Plugin B storage。

---

# 十九、Plugin Reload

支持 Plugin Reload。

Reload 必须至少处理：

- manifest reload
- JS module recompile
- Tool unregister
- Tool re-register
- Runtime pool replacement
- stale Runtime cleanup
- stale Hook cleanup
- stale Command cleanup

Reload 过程中不能：

- 留下重复 Tool
- 重复 Hook
- 重复 Command
- 泄漏旧 Runtime
- 错误复用旧 Module

如果存在正在执行中的 invocation，应定义 deterministic policy，例如：

- 旧 invocation 使用旧 generation 执行完成
- 新 invocation 使用新 generation

或其他等价安全模型。

---

# 二十、错误隔离

插件错误不得导致 Bruce 主进程异常退出。

需要隔离：

- JS syntax error
- module link error
- manifest error
- missing handler
- handler exception
- malformed return value
- timeout
- cancellation
- panic from host bridge
- permission denial

错误信息必须能明确显示：

- Plugin name
- Plugin path
- Handler / Tool / Hook
- Failure stage

但不得泄漏：

- secret
- API key
- sensitive environment
- internal credentials

---

# 二十一、Observability

Plugin invocation 必须进入 Bruce 已有观测体系。

至少能够观察：

- Plugin loaded
- Plugin load failed
- Plugin unloaded
- Plugin reloaded
- Tool invocation start
- Tool invocation complete
- Tool invocation failed
- Hook invocation
- Timeout
- Cancellation
- Permission denial

建议记录：

- plugin identity
- handler
- duration
- outcome
- error category

不要默认记录敏感 Tool 参数全文。

---

# 二十二、兼容性目标

插件系统不能破坏现有：

- ReAct
- Plan
- Builtin tools
- MCP
- Skill
- Sandbox
- HITL
- CLI
- Session
- Parallel tool calls

没有安装任何 Plugin 时：

> Bruce 的行为必须和引入 Plugin System 前保持一致。

这是强制性回归要求。

---

# 二十三、明确的非目标

第一阶段不要实现以下内容：

- Node.js compatibility
- npm runtime
- node_modules
- package.json dependency resolution
- Bun compatibility
- Pi plugin compatibility
- OpenCode plugin compatibility
- Node fs
- Node process
- Node child_process
- unrestricted shell
- unrestricted network
- unrestricted filesystem
- TypeScript runtime
- Hot module replacement framework
- remote plugin marketplace

如果未来增加 TypeScript，应优先考虑：

> precompile / transpile to JavaScript

而不是让 JavaScript VM 承担 TypeScript Runtime。

---

# 二十四、测试总体要求

所有新增能力必须具有自动化测试。

至少需要以下测试层级：

1. Unit Tests
2. Integration Tests
3. Security Tests
4. Concurrency Tests
5. Cancellation Tests
6. Reload Tests
7. Regression Tests

测试不能只验证 happy path。

---

# 二十五、Manifest Tests

必须测试：

- valid manifest
- missing name
- duplicate plugin name
- invalid API version
- invalid entry
- missing handler
- invalid schema
- invalid permission
- malformed JSON
- unsupported fields policy
- invalid command
- invalid hook
- conflicting tool name

错误必须稳定且可诊断。

---

# 二十六、Tool Integration Tests

必须验证：

Plugin Tool 可以：

- 被成功加载
- 出现在 Tool Registry
- 被转成 LLM tool definition
- 被 Agent 调用
- 正确接收 JSON input
- 正确返回结果
- 正确返回 structured error
- 正确经过 Sandbox
- 正确经过 HITL

还必须测试 nested JSON：

- nested object
- array
- boolean
- numeric value
- null

确保类型无损。

---

# 二十七、Security Tests

必须至少覆盖：

- Plugin 默认无 filesystem permission
- Plugin 默认无 network permission
- Plugin 默认无 shell permission
- import 路径穿越失败
- unauthorized Host API 调用失败
- Hook 修改参数后重新进行安全校验
- Plugin 无法绕过 HITL
- Plugin 无法修改 Sandbox mode
- Plugin 无法访问其他 Plugin private storage
- Plugin 无法直接访问宿主 Go 内部对象

建议增加 adversarial plugin fixtures。

---

# 二十八、Runtime Pool Tests

必须验证：

- 同一 Runtime 不被并发使用
- 多 invocation 可以并行
- Runtime 正确回池
- Runtime 异常后可丢弃
- cancellation 后池仍可继续工作
- reload 后旧 pool 不被新 invocation 使用
- unload 后没有 Runtime 泄漏

使用 Go race detector 运行相关测试。

---

# 二十九、Cancellation Tests

至少创建：

1. 无限循环插件
2. 长时间计算插件
3. async 插件
4. canceled Tool invocation

必须验证：

- ctx cancel 后 JS 能退出
- 返回明确 cancellation error
- Bruce 不挂死
- Pool 不 deadlock
- 之后的 invocation 仍能执行

---

# 三十、Hook Tests

至少测试：

- observer 正常调用
- before hook 修改参数
- before hook 拒绝 invocation
- after hook 修改结果
- hook throw exception
- hook timeout
- 多插件 hook ordering
- hook unregister
- reload 后无重复 hook
- Hook 修改出非法/越权参数后，被 Bruce 安全层阻止

---

# 三十一、Plugin Isolation Tests

准备两个 Plugin：

Plugin A
Plugin B

验证：

- A exception 不影响 B
- A reload 不影响 B
- A storage 默认不可访问 B
- A Runtime 不共享 B Runtime state
- A permissions 不自动传递给 B

---

# 三十二、Reload Tests

必须测试：

- 修改 JS 后 reload 生效
- 修改 manifest 后 reload 生效
- 删除 Tool 后 Tool Registry 同步删除
- 新增 Tool 后同步注册
- 删除 Plugin 后完全卸载
- reload 10 次不产生重复 Tool
- reload 10 次不产生重复 Hook
- reload 10 次不产生明显 Runtime/resource leak

---

# 三十三、Regression Tests

所有 Bruce 现有测试必须继续通过。

重点回归：

- Builtin Tool invocation
- MCP Tool invocation
- Skill loading
- Approval
- Sandbox
- Network restriction
- ReAct
- Plan
- parallel Tool
- cancellation
- CLI behavior

无 Plugin 时不应增加明显行为差异。

---

# 三十四、Performance Expectations

Plugin System 第一目标不是 benchmark 极限，而是：

- Runtime creation/reuse 成本可控
- Plugin invocation 不产生明显 goroutine leak
- Module 不重复无意义 compile
- 并行 Tool invocation 不被全局 JS lock 串行化
- Plugin 数量增加时内存增长可解释

建议增加 benchmark，至少衡量：

- Plugin load
- Warm invocation
- Runtime pool acquire/release
- Concurrent invocation
- Go ↔ JS nested JSON conversion

但性能优化不能牺牲安全和正确性。

---

# 三十五、验收标准：MVP

第一阶段完成后，必须满足以下 Demo。

Workspace 中存在一个 JavaScript Plugin。

该 Plugin 定义一个 Tool。

启动 Bruce 后：

1. Plugin 被自动发现。
2. Manifest 被校验。
3. JS Module 被成功加载。
4. Tool 出现在 Bruce Tool Registry。
5. LLM 可以正常选择该 Tool。
6. Tool receives nested JSON arguments correctly。
7. Tool executes through moejs。
8. Tool return value correctly returns to Agent。
9. Tool 必须经过 Bruce Policy。
10. Tool 必须遵守 Sandbox。
11. 需要 Approval 时必须出现 HITL。
12. ctx cancellation 可以中断插件。
13. Plugin exception 不导致 Bruce 崩溃。
14. Plugin reload 后新代码生效。
15. Reload 后没有重复 Tool。
16. 多个并发调用不存在 Runtime race。
17. 所有原有 Bruce tests 继续通过。

满足以上全部条件，才能视为 MVP 验收通过。

---

# 三十六、验收标准：安全

以下任意情况成功发生，都视为验收失败：

- Plugin 绕过 Sandbox。
- Plugin 绕过 Approval。
- Plugin 未经允许读取 workspace 外文件。
- Plugin 未经允许访问 network。
- Plugin 未经允许执行 shell。
- Hook 修改参数后绕过二次验证。
- 一个 Runtime 被并发使用。
- Context cancellation 无法终止死循环插件。
- Plugin crash 导致 Bruce process crash。
- Plugin reload 留下旧 Tool / Hook。
- Plugin 权限可以自行提升。

---

# 三十七、验收标准：工程质量

实现必须做到：

- Plugin Core 与 moejs adapter 解耦。
- Plugin 代码不大规模污染 Agent Core。
- 不复制一套 Tool execution architecture。
- 不创建第二套 Sandbox。
- 不创建第二套 Approval。
- 不通过 Tool name prefix 实现新权限模型。
- 有清晰 lifecycle ownership。
- 有明确 error types / categories。
- 有足够测试覆盖关键安全路径。
- `go test ./...` 通过。
- race-sensitive tests 使用 `go test -race` 验证。
- 必要文档完整。

---

# 三十八、文档要求

实现结束后更新项目文档。

至少说明：

- Plugin 是什么
- Plugin directory
- Manifest format
- JavaScript entry
- Tool declaration
- Permission model
- Sandbox behavior
- Host API
- Import restrictions
- Cancellation
- Reload
- Debugging
- Known limitations
- Security model

必须明确告诉 Plugin Author：

JavaScript VM sandbox ≠ OS security boundary。

Bruce Host Policy 才是最终权限边界。

---

# 三十九、推荐开发顺序

严格控制 scope，按以下顺序推进：

Phase 1
- Plugin discovery
- Manifest
- JS Engine abstraction
- moejs adapter
- Plugin Tool
- Runtime lifecycle
- cancellation
- permissions
- reload

Phase 2
- HookManager
- observer hooks
- interceptor hooks
- hook ordering
- hook failure policy

Phase 3
- Command Registry
- Plugin slash commands

Phase 4
- Plugin Storage
- richer Host API
- developer experience

Phase 5
- optional TypeScript precompile
- richer ecosystem capability

在 Phase 1 未稳定、未完成安全验收之前，不要扩大 scope。

---

# 四十、工作方式要求

在修改代码前：

1. 阅读现有实现。
2. 描述当前相关架构。
3. 指出会影响哪些模块。
4. 给出实现计划。
5. 明确哪些现有行为必须保持不变。
6. 明确安全边界。
7. 然后才开始修改。

不要因为本需求给出了建议架构，就假定 Bruce 当前实现与你的理解完全一致。

应以仓库实际代码为准。

如果发现需求与现有架构冲突：

- 优先保持上述安全原则和产品目标。
- 选择与 Bruce 当前架构最自然的实现。
- 不要为了机械满足文字要求而创建不必要的复杂度。

---

# 最终交付要求

最终输出必须包含：

1. 架构改动总结
2. Plugin 生命周期说明
3. 安全模型说明
4. Tool 集成说明
5. Runtime Pool / concurrency 说明
6. Cancellation 说明
7. Reload 说明
8. 新增测试清单
9. 实际执行的测试及结果
10. 尚未解决的限制
11. 与后续 Phase 的边界

不要只声明“实现完成”。

必须通过自动化测试和上述验收标准证明实现确实满足要求。