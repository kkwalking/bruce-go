## 1. 配置与工具策略模型

- [x] 1.1 为 MCP server 配置增加精确 `toolAccess` 映射，完成 trim、合法模式、通配符和 HTTP `workspace-write` 校验，并覆盖旧配置兼容及错误信息测试。
- [x] 1.2 为 `tool.Tool` 增加来源、最低 sandbox mode 和执行约束元数据，使零值/未知策略按 `full-access` fail closed。
- [x] 1.3 审计并显式标注内置、Web、Skill、Plan 和 MCP 的全部工具注册点，添加测试保证安全模式不会因遗漏策略意外授权。
- [x] 1.4 扩展 MCP tool schema 解析可选 annotations，用于风险展示但不改变 `toolAccess` 授权，并添加不可信 annotation 测试。

## 2. Registry 统一授权

- [x] 2.1 实现基于 sandbox snapshot 的工具策略判定，并让 `Definitions()`、`BuildPrompt()` 只暴露当前可执行工具。
- [x] 2.2 在 Registry 执行入口于 HITL 前、HITL 参数修改后和最终执行前校验策略与 generation，统一返回可诊断的 sandbox policy 错误。
- [x] 2.3 添加直接调用、陈旧工具引用、HITL 批准/改参、模式变化和 transport 零调用次数测试，证明 MCP 不能绕过统一授权。

## 3. 长驻原生沙箱进程

- [x] 3.1 为 `sandbox.Manager` 和 Runner 增加 argv 型长驻进程接口、server 生命周期临时根、安全环境合并及幂等 Wait/Kill/Close/cleanup。
- [x] 3.2 在 macOS Runner 中复用 Seatbelt profile 启动原始 program/args，并测试 read-only、workspace-write、网络和特殊 argv/profile 参数。
- [x] 3.3 在 Linux Runner 中复用 Bubblewrap 挂载、环境和网络策略启动原始 program/args，并测试 workspace/Git/敏感路径及禁网行为。
- [x] 3.4 完成 host/unsupported Runner 行为、取消与进程树回收测试，确保安全 backend 不可用时不回退宿主启动。

## 4. MCP Transport 与策略接入

- [x] 4.1 将 stdio transport 改为使用 sandbox 长驻进程接口，合并安全基础环境与 server 显式 `env`，并保证初始化、后台任务和工具调用共用同一 snapshot。
- [x] 4.2 在 MCP 工具注册和调用中应用 `toolAccess`：stdio 按当前 mode 过滤，HTTP 安全模式仅允许显式 `read-only`，`full-access` 保持全部工具可用。
- [x] 4.3 让 stdio MCP 继承有效网络策略，并在安全模式禁网时阻止 HTTP MCP 初始化和调用。
- [x] 4.4 添加 fake transport 与 HTTP 测试，覆盖未分类默认拒绝、可信只读、HTTP 写策略非法、annotation 不授权和 full-access 兼容。

## 5. 模式切换与运行时协调

- [x] 5.1 为 MCP Manager 增加 policy generation、transitioning gate、调用取消/等待和 server 重启状态，阻止调用使用旧 transport。
- [x] 5.2 将 `/sandbox mode` 和 `/sandbox network` 接入 fail-closed MCP transition：预检目标、关闭旧进程、提交策略、重启 server、刷新 Registry 并重建 agent prompt。
- [x] 5.3 处理部分 server 重启失败和更严格模式切换，保证保持新 sandbox mode、失败 server blocked 且绝不恢复旧高权限进程。
- [x] 5.4 更新 MCP enable/disable/restart 和 Runtime 启动路径，使所有入口都使用当前 sandbox snapshot 并与并发模式切换互斥。

## 6. 状态与用户反馈

- [x] 6.1 扩展 MCP server/tool 状态，记录 transport、enforcement、policy generation、可用/阻止工具数及稳定阻止原因。
- [x] 6.2 更新 `/mcp`、`/sandbox status`、统一 `/status` 和 TUI 渲染，区分原生隔离、trusted remote、full-access 未隔离与 policy blocked。
- [x] 6.3 添加状态、错误文本、动态工具刷新和 TUI 展示测试，确认不输出环境变量值、Header 或凭据。

## 7. 安全回归与文档

- [x] 7.1 编写可测试的 stdio MCP helper，覆盖 server 初始化写入、read-only 工具读取、workspace 写入、宿主越界写入及旧进程退出。
- [ ] 7.2 在 macOS Seatbelt 和 Linux Bubblewrap 集成矩阵验证 read-only、workspace-write、full-access、网络开关、backend fail-closed 和模式切换。
- [ ] 7.3 添加并发工具调用与 mode/network transition 的 race 测试，并运行 `go test ./...`、`go test -race ./...` 和 `go vet ./...`。
- [x] 7.4 更新 README、sandbox requirements/design、MCP 配置示例和迁移说明，删除“MCP 不在沙箱范围内”的旧表述并说明 HTTP 显式信任边界。
- [ ] 7.5 运行 OpenSpec 严格校验并核对所有需求场景均有对应测试。
