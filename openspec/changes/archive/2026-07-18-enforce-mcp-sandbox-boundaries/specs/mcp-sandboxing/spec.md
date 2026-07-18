## ADDED Requirements

### Requirement: Sandbox mode is an MCP authorization ceiling
系统 MUST 将当前 sandbox mode 作为所有 MCP 工具调用不可绕过的权限上限，并按照用户配置的精确 `toolAccess` 判断工具所需的最低模式。缺失、空白或未知的 MCP 工具策略 MUST 按 `full-access` 处理。

#### Scenario: Read-only rejects a workspace-writing MCP tool
- **WHEN** 当前模式为 `read-only`，模型或调用方尝试执行 `toolAccess=workspace-write` 的 MCP 工具
- **THEN** 系统 MUST 在进入 HITL 和 MCP transport 之前拒绝调用
- **AND** 系统 MUST NOT 修改 workspace 或宿主文件

#### Scenario: Workspace-write rejects a full-access MCP tool
- **WHEN** 当前模式为 `workspace-write`，调用方尝试执行未分类或 `toolAccess=full-access` 的 MCP 工具
- **THEN** 系统 MUST 拒绝调用并说明工具所需权限超过当前模式
- **AND** MCP transport MUST NOT 收到该次 `tools/call`

#### Scenario: Full-access preserves existing MCP availability
- **WHEN** 当前模式为 `full-access` 且 MCP server 已启用并就绪
- **THEN** 系统 MUST 继续暴露和执行该 server 的全部工具，无需旧配置补充 `toolAccess`

### Requirement: Tool policy is enforced independently of model and HITL behavior
系统 MUST 在统一工具执行入口执行 MCP sandbox 策略，并 MUST 在 HITL 修改参数后及真正执行前重新校验当前 policy generation。提示词、工具名、服务端描述和 HITL 批准 MUST NOT 提升 sandbox 权限。

#### Scenario: Direct execution cannot bypass the registry guard
- **WHEN** 调用方绕过模型提示并使用工具名直接请求一个当前模式不允许的 MCP 工具
- **THEN** 统一工具执行入口 MUST 返回 sandbox policy 拒绝
- **AND** MCP executor MUST NOT 被调用

#### Scenario: HITL approval cannot override read-only
- **WHEN** 当前模式为 `read-only` 且 HITL 批准一个 workspace 写入 MCP 调用
- **THEN** 系统 MUST 在请求用户审批之前拒绝该调用
- **AND** 批准结果 MUST NOT 使调用继续执行

#### Scenario: Policy changes while approval is pending
- **WHEN** MCP 工具等待审批期间 sandbox mode 或有效网络策略发生变化
- **THEN** 系统 MUST 在执行前使用新的 policy generation 重新授权
- **AND** 新策略不允许时 MUST 拒绝调用

### Requirement: Tool exposure matches executable permissions
系统 MUST 从发送给 LLM 的 tool definitions 和工具提示中移除当前策略不可执行的 MCP 工具，同时保留执行时的权威校验。

#### Scenario: Mutating MCP tool is hidden in read-only
- **WHEN** 当前模式为 `read-only` 且 MCP server 同时提供只读与写入工具
- **THEN** LLM MUST 只看到符合 `read-only` 策略的工具
- **AND** 对被隐藏写入工具的陈旧调用仍 MUST 被执行入口拒绝

#### Scenario: Mode change refreshes tool visibility
- **WHEN** 用户成功切换 sandbox mode
- **THEN** 下一次 LLM 请求的 tool definitions MUST 与新模式一致
- **AND** agent prompt 与 Registry 中的 MCP 可用状态 MUST 被刷新

### Requirement: Stdio MCP servers run inside the native sandbox
系统 MUST 在 `read-only` 和 `workspace-write` 中通过当前平台原生 sandbox 启动 stdio MCP server，并将启动、初始化、后台任务和工具调用限制在同一个不可变策略 snapshot 内。

#### Scenario: Read-only server cannot write during initialization
- **WHEN** 系统在 `read-only` 中启动一个会在初始化阶段尝试写 workspace 的 stdio MCP server
- **THEN** Seatbelt 或 Bubblewrap MUST 拒绝该文件写入
- **AND** 系统 MUST NOT 以宿主权限重试 server

#### Scenario: Read-only stdio tool can inspect but not modify workspace
- **WHEN** `toolAccess=read-only` 的 stdio MCP 工具在 `read-only` 中读取 workspace
- **THEN** 系统 MUST 允许读取非敏感 workspace 内容
- **AND** 该 MCP 进程对 workspace、Git 元数据和宿主其他位置的写入 MUST 被原生 sandbox 拒绝

#### Scenario: Workspace-write stdio tool remains inside allowed roots
- **WHEN** `toolAccess=workspace-write` 的 stdio MCP 工具在 `workspace-write` 中执行
- **THEN** 该进程 MUST 只能写 workspace、允许的当前 Git 元数据和 server 临时根
- **AND** 受保护 Git 项、敏感路径、其他 worktree 与宿主其他位置 MUST 保持不可写

#### Scenario: Native backend is unavailable
- **WHEN** 当前为安全模式且 stdio MCP 所需的原生 sandbox backend 不可用或策略构造失败
- **THEN** server MUST 保持 blocked/error
- **AND** 系统 MUST NOT 注册其工具或退回无沙箱启动

### Requirement: Stdio MCP inherits safe environment and network policy
安全模式中的 stdio MCP server MUST 使用 sandbox Manager 构造的安全环境、专用可写临时根和当前有效网络策略，不得默认继承完整宿主环境。

#### Scenario: Network-disabled stdio server cannot reach the network
- **WHEN** 当前为安全模式且有效网络设置为关闭
- **THEN** stdio MCP 进程 MUST 无法建立 TCP、UDP 或本机网络连接
- **AND** 已知容器及认证 Agent Socket MUST 保持不可访问

#### Scenario: Allowed environment variables are preserved
- **WHEN** 安全模式启动 stdio MCP server
- **THEN** 环境 MUST 仅包含安全内置变量、`sandbox.allowedEnv` 允许的精确宿主变量、隔离临时目录变量和该 server 显式配置的 `env`
- **AND** MCP server 专属 `env` MUST 按现有规则完成变量展开且 MUST NOT 导致其他宿主环境变量被继承

### Requirement: HTTP MCP uses an explicit remote trust policy
由于 Bruce 无法隔离 HTTP MCP 服务端进程，安全模式 MUST 只允许用户在 `toolAccess` 中精确声明为 `read-only` 的 HTTP 工具。HTTP 工具 MUST NOT 在安全模式声明或获得 `workspace-write` 权限。

#### Scenario: Unclassified HTTP tool is denied
- **WHEN** 当前为 `read-only` 或 `workspace-write`，HTTP MCP 工具没有精确的 `toolAccess=read-only` 配置
- **THEN** 系统 MUST 隐藏并拒绝该工具
- **AND** 系统 MUST NOT 向 HTTP endpoint 发送 `tools/call`

#### Scenario: Trusted read-only HTTP tool is allowed with network
- **WHEN** 用户精确配置 HTTP MCP 工具为 `read-only` 且安全模式有效网络为开启
- **THEN** 系统 MUST 允许调用该工具
- **AND** 状态 MUST 标明其安全性依赖用户对远端服务的显式信任，而非本地进程隔离

#### Scenario: HTTP workspace-write policy is invalid
- **WHEN** 配置把 HTTP MCP 工具声明为 `workspace-write`
- **THEN** 配置加载 MUST 失败并给出 transport 无法强制 workspace 边界的原因

#### Scenario: Server annotations do not grant access
- **WHEN** 未配置的 MCP 工具只通过服务端 annotation 声明 `readOnlyHint=true`
- **THEN** 系统 MUST NOT 因该 annotation 在安全模式授权工具
- **AND** annotation MAY 仅用于风险和状态展示

### Requirement: MCP network access follows sandbox policy
安全模式中的 MCP 网络行为 MUST 遵循当前有效网络设置；`full-access` 仍按既有语义强制网络开启。

#### Scenario: HTTP MCP is not initialized while network is disabled
- **WHEN** 当前为安全模式且有效网络设置为关闭
- **THEN** 系统 MUST NOT 初始化或调用 HTTP MCP server
- **AND** server 状态 MUST 显示因 sandbox network policy 被阻止

#### Scenario: Enabling network refreshes eligible MCP servers
- **WHEN** 用户在安全模式成功开启网络
- **THEN** 系统 MUST 重新配置 MCP transport
- **AND** 只有满足当前 mode 与 `toolAccess` 的 HTTP 工具 MUST 被注册

### Requirement: Sandbox transitions revoke stale MCP permissions
Runtime MUST 将 sandbox mode 和有效网络变化作为 MCP policy transition 处理，在报告切换成功前停止接受新调用、关闭旧 transport、终止旧 stdio 进程并按新 snapshot 刷新 server 与工具。

#### Scenario: Full-access server is replaced before entering read-only
- **WHEN** 用户从 `full-access` 切换到 `read-only`
- **THEN** 原 full-access stdio MCP 进程 MUST 在切换完成前退出
- **AND** 系统 MUST 仅以 read-only 原生策略重新启动符合条件的 server

#### Scenario: Restart failure remains fail closed
- **WHEN** sandbox mode 已切换为更严格模式，但某个 MCP server 无法按新策略启动
- **THEN** 系统 MUST 保持新 sandbox mode
- **AND** 失败 server MUST 保持 blocked/error，不得恢复旧 transport 或旧权限

#### Scenario: Calls are blocked during transition
- **WHEN** MCP policy transition 正在进行
- **THEN** 新 MCP 调用 MUST 被拒绝或等待至新 generation 生效
- **AND** 调用 MUST NOT 使用切换前的 transport

### Requirement: MCP sandbox state is observable
系统 MUST 在 MCP、sandbox 和统一状态输出中区分原生隔离、可信远端、full-access 未隔离以及 policy blocked 状态，并提供可操作但不泄密的原因。

#### Scenario: Blocked tools are reported
- **WHEN** server 已就绪但部分工具因当前 mode、网络或缺少 `toolAccess` 被阻止
- **THEN** 状态 MUST 显示可用和被阻止工具数量
- **AND** 日志或详情 MUST 给出稳定的阻止原因

#### Scenario: Full-access status is not presented as sandboxed
- **WHEN** stdio MCP server 在 `full-access` 运行
- **THEN** 状态 MUST 明确显示其未受原生 sandbox 约束
- **AND** UI MUST NOT 将 backend 可用误报为该进程当前已隔离

### Requirement: MCP tool access configuration is validated
系统 MUST 对每个 server 的 `toolAccess` 做确定性校验、去除工具名首尾空白并拒绝空名称、通配符、重复冲突和非法模式。旧配置缺少该字段时 MUST 保持可加载。

#### Scenario: Legacy configuration loads safely
- **WHEN** 旧 MCP server 配置不包含 `toolAccess`
- **THEN** 配置 MUST 正常加载
- **AND** 其工具在安全模式 MUST 按 `full-access` 要求处理，在 `full-access` MUST 保持可用

#### Scenario: Invalid access mode is rejected
- **WHEN** `toolAccess` 包含不是 `read-only`、`workspace-write` 或 `full-access` 的值
- **THEN** 配置加载 MUST 失败
- **AND** 错误 MUST 指明 server、工具名和允许的模式
