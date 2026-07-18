## Context

现有 sandbox 的原生隔离只存在于 `sandbox.Manager.Run` 启动的一次性 Shell 进程中，内置 `write_file`、`edit_file` 则由 `Tool.Registry` 按工具名调用 `CanWriteFile`。MCP 工具注册后只是普通 `Tool.Exec`，会直接进入 `Manager.CallTool`；stdio transport 还会通过 `exec.CommandContext` 继承完整宿主环境并长期运行。因此模型提示、HITL 或内置工具检查都不能阻止 MCP 绕过 `read-only` 和 `workspace-write`。

本变更涉及工具注册与调度、MCP transport、原生沙箱长驻进程、动态模式切换及配置兼容。现有约束包括：

- `full-access` 必须保持旧配置和旧 MCP 行为。
- 安全模式必须 fail closed，Seatbelt/Bubblewrap 不可用时不得退回宿主执行。
- `/sandbox mode` 与 `/sandbox network` 可在运行时切换，MCP 进程是长驻资源，不能继续使用切换前的权限。
- Plan mode 当前不复制 MCP 工具，本变更不得削弱其既有只读语义。
- 不新增容器或第三方沙箱依赖，继续复用 Seatbelt 与 Bubblewrap。

## Goals / Non-Goals

**Goals:**

- 使 sandbox mode 成为所有 MCP 调用不可绕过的本地文件系统权限上限。
- 在工具进入 HITL、transport 或直接执行之前完成统一授权，并让模型只看到当前可用工具。
- 让 stdio MCP server 在安全环境、文件系统和网络策略与当前 sandbox snapshot 下运行。
- 对无法由 Bruce 隔离服务端进程的 HTTP MCP 采用显式信任和默认拒绝策略。
- 在 mode/network 切换时撤销旧 MCP 权限并刷新 transport、工具定义和状态。
- 保持 `full-access` 下旧 MCP 配置无需迁移即可继续工作。

**Non-Goals:**

- 不保证远程 MCP 服务端内部实现诚实，也不撤销已经由远端系统完成的副作用。
- 不把 WebSearch、WebFetch 或 LLM 请求纳入本期 MCP 网络策略。
- 不增加域名/端口 allowlist、seccomp、cgroup、资源配额或 Windows 原生隔离。
- 不根据工具名称、描述或模型判断自动推断写权限。
- 不改变 Plan mode 计划文件持久化这一显式授权的元数据写入。

## Decisions

### 1. 工具注册携带显式访问策略，零值按最高权限处理

`tool.Tool` 增加结构化策略，至少记录工具来源、所需 sandbox mode 和执行约束。所需模式采用 `read-only`、`workspace-write`、`full-access` 三档；未声明或无效策略按 `full-access` 处理，而不是默认只读。

所有内置、Web、Skill、Plan 和 MCP 工具在注册时显式赋值。MCP server 配置新增按远端工具原名精确匹配的 `toolAccess`：

```json
{
  "mcp": {
    "servers": {
      "filesystem": {
        "type": "stdio",
        "command": "mcp-server-filesystem",
        "args": ["${PROJECT_DIR}"],
        "toolAccess": {
          "read_file": "read-only",
          "list_directory": "read-only",
          "write_file": "workspace-write"
        }
      }
    }
  }
}
```

没有出现在 `toolAccess` 中的 MCP 工具仅能在 `full-access` 使用。配置只接受精确工具名和三个合法值，不支持通配符；加载时去除空白、拒绝空名称和非法值。

选择显式策略而不是工具名黑名单，是因为 MCP 工具可任意命名且一个工具可能组合多个动作。MCP `annotations.readOnlyHint`、`destructiveHint` 等字段可以展示为风险提示，但服务端声明不能提升 `toolAccess` 授予的权限。

### 2. Registry 是统一授权点，transport 仍执行纵深防御

`Registry.execute` 在找到 `Tool` 后先使用当前 sandbox snapshot 校验访问策略，再进入 HITL；HITL 修改参数后以及真正调用 `Exec` 前再次校验 policy generation。若模式或网络在审批期间发生变化，必须按新 snapshot 重新判定。

`Definitions()` 与 `BuildPrompt()` 使用相同策略过滤不可用工具，减少模型产生注定失败的调用；执行时校验仍是权威边界，以覆盖直接调用、旧 agent prompt、并发切换和陈旧闭包。

拒绝结果统一使用 sandbox policy 错误，包含当前 mode、工具来源和所需权限，但不泄露环境变量或凭据。HITL 只能在沙箱允许范围内决定是否执行，不能把拒绝操作升级为允许。

### 3. stdio MCP 使用新的长驻沙箱进程接口

`sandbox.Manager` 增加面向 argv 的长驻进程启动能力，而不是让 MCP 拼接 Shell 字符串。接口接收 program、args、工作目录和附加环境，返回受管理的 stdin/stdout/stderr、Wait、Kill/Close 与 cleanup。

Manager 在启动时创建 server 生命周期专用临时根，并复用现有 snapshot、`safeEnvironment`、敏感路径、Socket、GitLayout 和有效网络设置。安全环境仅从宿主继承内置允许项与 `sandbox.allowedEnv`，再叠加用户在该 MCP server `env` 中显式配置并完成变量展开的键值；不得继承其他宿主变量。Runner 分别把原始 argv 包装为：

- macOS：`/usr/bin/sandbox-exec -p <profile> ... -- <program> <args...>`
- Linux：`bwrap <mount/network/env args> -- <program> <args...>`
- `full-access`：直接启动原始 program/args 并继承宿主环境。

安全模式不经过 `/bin/bash -c`，避免 argv 再解释。stdio server 的启动阶段、后台任务及所有工具调用均继承同一不可变策略；进程退出或 transport 关闭时清理专用临时根和进程树。

只做 Registry 拒绝不足以防止恶意 server 在初始化阶段写文件；只做进程沙箱又会让模型反复调用明显不允许的工具，因此两层都保留。

### 4. stdio 与 HTTP MCP 使用不同的信任模型

stdio server 的进程由 Bruce 启动，因此：

- `read-only` 只注册 `toolAccess=read-only` 的工具，进程物理上不能写 workspace 或宿主。
- `workspace-write` 注册 `read-only` 和 `workspace-write` 工具，进程只能写 workspace、允许的 Git 元数据和临时根。
- `full-access` 注册全部服务端工具，保持现有行为。

HTTP server 不在 Bruce 的进程边界内，因此 `workspace-write` 声明无法证明服务端只写当前 workspace。为避免虚假保证：

- 安全模式只允许显式配置为 `read-only` 的 HTTP 工具。
- HTTP 工具配置为 `workspace-write` 时配置校验失败，并提示该 transport 只能在安全模式声明只读工具。
- 未配置工具以及所有写工具只在 `full-access` 可用。
- 显式 `read-only` 是用户建立的信任边界，状态和文档必须说明 Bruce 无法验证远端实现。

备选方案“安全模式禁用全部 MCP”虽然简单可靠，但会不必要地移除受原生沙箱保护的 stdio 读取能力；它保留为后端不可用、策略未知或切换失败时的降级行为。

### 5. mode/network 切换使用 fail-closed 生命周期

MCP Manager 维护递增的 policy generation 和 `transitioning` 状态。Runtime 切换 sandbox mode 或有效网络设置时：

1. 预检目标 sandbox policy；失败则保持原状态和原 transport。
2. MCP Manager 进入 transitioning，拒绝新的 MCP 调用。
3. 取消或等待当前调用并关闭旧 transport；stdio 进程必须退出。
4. 提交新的 mode/network snapshot。
5. 按新 snapshot 启动已启用 server，重新发现并过滤工具。
6. 刷新 Registry、重建 agent prompt，最后退出 transitioning。

进入更严格模式后，单个 MCP server 启动失败不得回滚 sandbox mode，也不得使用旧的高权限进程；该 server 保持 blocked/error，其他 server 可继续启动。开始于切换前且远端已经接受的 HTTP 副作用无法撤销，状态只在旧调用结束或取消后报告切换完成。

网络策略同时应用于 MCP：

- 安全模式下 stdio server 继承 Seatbelt/Bubblewrap 的有效网络设置。
- 有效网络关闭时不初始化或调用 HTTP MCP。
- 网络开关变化会使相关 MCP transport 重新配置；`full-access` 仍强制网络开启。

### 6. 状态明确展示“可用、受约束、被策略阻止”

MCP server/tool 状态增加 transport、sandbox enforcement、policy generation、可用/阻止工具数和阻止原因。`/mcp`、`/sandbox status` 与统一状态输出必须区分：

- stdio server 正在 `read-only`/`workspace-write` 原生沙箱中运行；
- HTTP server 因网络关闭或缺少可信只读策略而 blocked；
- `full-access` 下 server 未受原生沙箱约束。

工具执行错误使用稳定文本，便于模型停止尝试同类写操作，也便于测试确认 transport 未被调用。

## Risks / Trade-offs

- [旧配置在安全模式下看不到 MCP 工具] → `full-access` 保持兼容，并提供 `toolAccess` 迁移示例和 blocked 原因；不以不安全默认值换取兼容。
- [部分 MCP server 依赖宿主完整环境、缓存或启动时下载] → 安全模式只提供 `safeEnvironment` 和可写临时缓存，启动失败时给出日志；用户可预安装 server 或显式使用 `full-access`。
- [HTTP read-only 配置所信任的服务端可能撒谎] → 必须由用户精确配置，界面标注“trusted remote”，annotations 不自动授权；高保证场景使用 stdio 原生隔离。
- [模式切换需要重启长驻进程，增加延迟并中断调用] → 切换期间明确显示 transitioning，并按 server 并行重启；安全优先于无缝复用。
- [Runner 增加长驻进程接口后生命周期更复杂] → 统一进程树回收、Wait/Close 幂等和临时根清理，并增加取消与 race 测试。
- [工具策略遗漏导致功能不可用] → 零值 fail closed、启动状态列出未分类工具，测试审计所有内置注册点。

## Migration Plan

1. 先增加配置解析、Tool policy 与执行时 fail-closed gate；此时安全模式下未分类 MCP 不可用，立即关闭绕过窗口。
2. 增加跨平台长驻进程接口和 stdio transport 接入，通过集成测试后恢复已配置的安全模式 stdio 工具。
3. 接入 HTTP 网络/只读策略、模式切换 generation 和状态刷新。
4. 更新 README、sandbox requirements/design 与配置示例，并运行单元测试、race、vet 及 macOS/Linux sandbox CI。

回滚代码不需要数据迁移；用户可临时使用 `--no-mcp`。切回 `full-access` 只能作为明确接受风险的兼容手段，不能被程序自动执行。

## Open Questions

无。本期采用精确 `toolAccess`、stdio 原生隔离和 HTTP 只读显式信任的保守模型；更细粒度的远端授权或域名策略留待后续变更。
