# Bruce 原生沙箱需求文档

## 文档信息

- 文档状态：当前实现基线
- 更新日期：2026-07-17
- 实现分支：`zzk/enforce-mcp-sandbox-boundaries`
- 适用平台：macOS、Linux
- 配套设计：[`sandbox-design.md`](sandbox-design.md)

## 1. 背景

Bruce 的 Shell、内置文件工具和 MCP 工具能够直接或间接读写本机文件。仅依靠命令黑名单、模型提示、HITL 和路径字符串校验，无法阻止子进程、Shell 重定向、软链接、MCP 初始化副作用或工具链间接访问宿主资源，因此需要统一授权和操作系统原生沙箱边界。

本功能在 macOS 使用 Seatbelt，在 Linux 使用 Bubblewrap，并保留 `full-access` 兼容模式。当前产品默认选择 `full-access`，以保持现有使用体验；用户需要主动切换到 `read-only` 或 `workspace-write` 才会获得原生文件系统与网络隔离。

> `full-access` 不是安全沙箱。该模式允许 Shell 使用完整宿主文件系统、环境变量和网络，只保留 Bruce 原有的命令防护、超时、输出限制与进程回收机制。

## 2. 目标

1. 为 `execute_command` 提供操作系统级文件系统、网络和敏感资源隔离。
2. 为 `read_file`、`write_file`、`edit_file` 提供抗目录穿越和抗软链接逃逸的 workspace 边界。
3. 支持只读、workspace 可写和完全访问三种运行模式，并允许在运行时切换。
4. 在安全模式下保护凭据、Agent Socket、Docker/Podman Socket 和 Git 执行配置。
5. 在原生后端不可用时保持 Bruce 可启动、状态可诊断，并确保安全模式拒绝执行。
6. 保留 Git 常规开发工作流、命令超时、输出限制、命令黑名单和进程树回收能力。

## 3. 术语

| 术语 | 含义 |
| --- | --- |
| workspace | Bruce 启动时确定的工作目录，也是内置文件工具允许访问的根目录 |
| 安全模式 | `read-only` 或 `workspace-write`，Shell 必须经过原生沙箱后端执行 |
| 原生后端 | macOS Seatbelt 或 Linux Bubblewrap |
| 配置网络值 | `sandbox.networkAccess` 或 `/sandbox network` 保存的运行时偏好 |
| 有效网络值 | 当前模式实际采用的网络状态；`full-access` 下始终为开启 |
| 受保护 Git 元数据 | `config`、hooks、alternates 以及可能影响 Git 执行行为或其他 worktree 的元数据 |
| `toolAccess` | 按 MCP 远端工具原名精确声明的最低 sandbox mode；缺失时按 `full-access` 处理 |

## 4. 功能范围

### 4.1 本期包含

- `execute_command`。
- 内置 `read_file`、`write_file`、`edit_file`。
- MCP stdio server 的启动、初始化、后台任务、工具调用、文件系统和网络访问。
- HTTP MCP 的显式只读信任策略、网络门禁与工具过滤。
- MCP policy generation、模式切换重启和统一工具执行授权。
- macOS Seatbelt 后端。
- Linux Bubblewrap 后端。
- Sandbox 配置、Slash 命令、TUI 状态与补全。
- Git 普通仓库和 linked worktree 的受控写入。
- 敏感路径和本地 Socket 隔离。
- 超时、取消、输出上限和进程树回收。

### 4.2 本期不包含

- Windows 原生沙箱。
- WebSearch、WebFetch、LLM 请求和 Bruce 自身网络访问。
- HTTP MCP 远端服务进程的文件系统或内部行为隔离。
- 域名级网络白名单、逐命令网络审批或网络代理。
- seccomp、cgroup、CPU、内存、磁盘和进程数配额。
- 自动安装或捆绑 Bubblewrap。
- Bubblewrap 不可用时自动回退 Docker。

## 5. 默认配置

用户配置结构如下：

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
          "write_file": "workspace-write"
        }
      }
    }
  },
  "sandbox": {
    "mode": "full-access",
    "networkAccess": false,
    "allowedEnv": []
  }
}
```

- 旧配置缺少 `sandbox` 或 `sandbox.mode` 时，必须补齐为 `full-access`。
- `networkAccess` 默认值为 `false`，它是切换到安全模式后使用的网络偏好；`full-access` 的有效网络仍始终开启。
- `allowedEnv` 默认为空，只在安全模式中追加允许透传的环境变量。
- 旧 MCP 配置缺少 `toolAccess` 时必须正常加载；其工具在安全模式按 `full-access` 处理，在 `full-access` 保持兼容。
- 配置文件只提供启动值；Slash 命令修改的状态不得回写配置文件。

## 6. 模式需求

| 模式 | Shell 文件系统 | workspace | Shell 网络 | 环境变量 | 原生后端要求 |
| --- | --- | --- | --- | --- | --- |
| `read-only` | 宿主只读，敏感资源遮蔽 | 只读 | 使用配置网络值 | 安全允许列表 | 必须可用 |
| `workspace-write` | 宿主只读，敏感资源遮蔽 | 可写，但 Git 受保护项不可写 | 使用配置网络值 | 安全允许列表 | 必须可用 |
| `full-access` | 完整宿主访问 | 可写 | 始终开启 | 完整宿主环境 | 不要求 |

### FR-001 模式取值

- `sandbox.mode` 只允许 `read-only`、`workspace-write`、`full-access`。
- 已废弃的 `danger-full-access` 和其他非法值必须在启动配置校验阶段报错。
- 一次性命令继续使用启动时的不可变策略快照；MCP transport 在模式切换完成前必须关闭旧进程并按新策略重启。

### FR-002 `read-only`

- Shell 可以读取 workspace 和宿主的非敏感文件。
- Shell 不得写入 workspace、Git 元数据或宿主其他位置。
- `write_file` 和 `edit_file` 必须在进入 HITL 前拒绝。
- 原生后端不可用、策略无效或启动失败时，命令必须失败，不得无沙箱重试。

### FR-003 `workspace-write`

- Shell 可以写入 workspace 和当前命令的隔离临时目录。
- 宿主其他位置保持只读，只有经验证的当前 Git 元数据目录可以获得受控写权限。
- Shell 不得修改受保护 Git 元数据、敏感路径和被遮蔽的 Socket。
- workspace 不得为文件系统根、用户 HOME 或 HOME 的祖先，也不得位于已知敏感目录中。
- 原生后端不可用、策略无效或启动失败时，命令必须失败，不得无沙箱重试。

### FR-004 `full-access`

- Shell 必须直接使用宿主执行器，不经过 Seatbelt 或 Bubblewrap。
- Shell 必须保留完整宿主环境、登录 Shell 兼容行为、文件系统访问和网络访问。
- 有效网络状态必须始终为开启。
- `/sandbox network off` 必须返回明确错误，不得展示为已关闭。
- 命令黑名单、HITL、超时、输出上限和进程树回收仍然有效。
- 内置文件工具仍受 workspace 边界和 `.git` 禁写约束，不能因 `full-access` 而放宽。

## 7. 网络需求

### FR-005 网络开关

- `/sandbox network on|off` 修改当前运行时的配置网络值。
- 安全模式网络关闭时，沙箱中的命令不得访问主机网络，包括本机监听端口。
- 安全模式网络开启时可以使用 TCP、UDP、DNS 和 TLS，但 Docker、Podman、D-Bus、SSH Agent、GPG Agent 等本地 Socket 仍必须不可访问。
- `full-access` 下网络强制开启；切换回安全模式后恢复使用此前保存的配置网络值。
- 网络开关控制安全模式中的 `execute_command`、stdio MCP 和 HTTP MCP 初始化/调用，不控制 Bruce 自身、LLM 或 Web 工具。

## 8. 配置与环境需求

### FR-006 `allowedEnv`

- 只接受精确环境变量名，不支持通配符、前缀或正则表达式。
- 合法名称必须匹配 `[A-Za-z_][A-Za-z0-9_]*`。
- 配置加载时必须去除首尾空白并去重；非法名称必须导致配置错误。
- 错误信息不得打印环境变量值。

### FR-007 安全环境

- 安全模式内置保留必要的 `PATH`、语言区域、终端变量、用户标识和常见工具链根路径。
- `LC_*` 可以按名称保留。
- `HOME` 保持真实路径以允许读取工具配置，但沙箱策略不得允许写入。
- `TMPDIR`、`TMP`、`TEMP`、`XDG_CACHE_HOME`、`XDG_RUNTIME_DIR`、`GOCACHE` 和 npm cache 必须指向当前命令的隔离临时目录。
- `full-access` 必须保留完整 `os.Environ()`。

### FR-022 MCP `toolAccess`

- `toolAccess` 必须使用远端工具原名精确匹配，只接受 `read-only`、`workspace-write`、`full-access`。
- 配置加载必须 trim 工具名和权限值，并拒绝空名称、通配符、trim 后重复和非法权限。
- 缺失、空白、未知或未匹配的策略必须按 `full-access` fail closed。
- MCP annotation 只能作为不可信风险提示，不得授予访问权限。
- 统一 Tool Registry 必须在 HITL 前、HITL 修改参数后和最终执行前按当前 generation 校验权限。
- LLM tool definitions 和 prompt 必须只包含当前策略可执行的工具，陈旧或直接调用仍必须在执行入口拒绝。

### FR-023 stdio MCP 原生隔离

- 安全模式必须通过 Sandbox Manager 的 argv 型长驻进程接口启动 stdio server，不得使用宿主权限重试。
- server 启动、初始化、后台任务和工具调用必须共用同一策略快照、专用临时根和安全环境。
- `read-only` 只暴露显式只读工具，且原生后端必须阻止进程写 workspace 和宿主。
- `workspace-write` 可暴露只读和工作区写工具，但进程只能写 workspace、允许的 Git 元数据和专用临时根。
- server 显式 `env` 可在安全环境基础上追加并完成变量展开，但不得导致其他宿主变量被继承。

### FR-024 HTTP MCP 信任边界

- 安全模式只允许用户在 `toolAccess` 中精确声明为 `read-only` 的 HTTP 工具。
- HTTP 工具配置为 `workspace-write` 必须在配置加载时报错，因为 Bruce 无法强制远端 workspace 边界。
- annotation 声明 `readOnlyHint=true` 不得替代用户授权。
- 安全模式有效网络关闭时不得初始化或调用 HTTP MCP。
- 状态必须标记 `trusted-remote`，不得把 HTTP 工具展示为受本机原生沙箱隔离。

### FR-025 MCP policy transition

- mode 或有效网络变化时，MCP Manager 必须进入 transitioning，拒绝新调用并关闭旧 transport。
- 旧 stdio 进程和进程树必须在切换完成前退出；正在执行的调用必须被取消或等待结束。
- 新策略提交后按新 snapshot 重启已启用 server、刷新 Registry，并重建 agent prompt。
- 更严格模式下部分 server 重启失败时必须保留新模式，失败 server 保持 blocked/error，不得恢复旧 transport。
- MCP 状态必须展示 transport、enforcement、policy generation、可用/阻止工具数和稳定原因，不得输出 env、Header 或凭据值。

## 9. 命令与交互需求

### FR-008 Slash 命令

必须支持：

```text
/sandbox
/sandbox status
/sandbox mode read-only
/sandbox mode workspace-write
/sandbox mode full-access
/sandbox network on
/sandbox network off
```

- `/sandbox` 等价于 `/sandbox status`。
- 缺失参数、非法子命令或非法值必须返回用法提示。
- `/sandbox mode` 和 `/sandbox network` 只修改当前进程状态，不写入 `setting.json`。

### FR-009 补全

- 输入 `/sandbox` 后按 Tab，必须补全或展开 `status`、`mode`、`network`。
- 输入 `/sandbox mode` 后按 Tab，必须展开三个模式。
- 输入 `/sandbox network` 后按 Tab，必须展开 `on`、`off`。

### FR-010 状态展示

`/sandbox status`、`/status` 和 TUI 状态栏必须展示：

- 当前模式。
- 探测到的平台后端。
- 有效网络状态。
- 后端是否可用。
- 后端不可用或探测失败的原因。
- MCP transport、原生隔离/可信远端/未隔离状态、policy generation 和 blocked 摘要。

`full-access` 下展示的平台后端仅代表探测结果，不代表当前命令经过该后端。

## 10. 后端与执行需求

### FR-011 后端探测与降级

- macOS 必须固定使用 `/usr/bin/sandbox-exec`。
- Linux 只能使用 workspace 外、解析后可信的系统 `bwrap`。
- 启动时必须探测后端能力；探测失败不得阻止 Bruce TUI 启动。
- 安全模式必须 fail closed；`full-access` 可以在后端不可用时继续执行。
- 不支持的平台必须提供明确的不可用原因。

### FR-012 Shell

- 安全模式必须使用可信绝对路径 `/bin/bash --noprofile --norc -c`，不得加载用户 profile 或 rc 文件。
- `full-access` 保持原有 `/bin/bash -lc` 兼容行为。
- Seatbelt profile 和 Bubblewrap 参数必须通过 argv 传递，不得拼接为二次 Shell 命令。

### FR-013 进程管理

- 命令与 stdio MCP 长驻进程必须支持 context 取消或显式关闭。
- Unix 进程必须使用独立进程组，超时、取消或 transport 关闭后回收子进程和孙进程。
- Bubblewrap 必须启用 `--die-with-parent`。
- 命令输出必须在执行期间有界，不得先无限缓存再截断。
- 结果必须区分正常退出、非零退出、超时、取消、输出截断和沙箱启动失败。

## 11. 平台隔离需求

### FR-014 macOS Seatbelt

- 动态路径必须使用严格的 SBPL 参数转义，并以 `-D` 参数传入 profile。
- 安全策略默认拒绝写操作，只为策略允许的目录放开写权限。
- 网络关闭时不得建立网络连接；网络开启时只放开网络协议所需能力。
- Apple Events、LaunchServices、未允许的 Mach Service 和 Unix Socket 默认不得使用。
- 敏感文件、敏感目录和本地 Agent/容器 Socket 必须显式拒绝。
- `Probe` 必须使用最小 profile 执行 `/usr/bin/true` 并保留可诊断错误。

### FR-015 Linux Bubblewrap

- `bwrap` 解析路径及其父目录必须满足系统所有权和不可被普通用户篡改的要求。
- 探测必须验证 user、PID、IPC、UTS、network namespace 和 mount 能力。
- 安全命令必须使用只读宿主根、独立 `/proc`、最小 `/dev` 和隔离 `/tmp`。
- 必须启用 user、PID、IPC、UTS namespace、`--new-session` 和 `--die-with-parent`。
- 网络关闭时使用独立 network namespace；网络开启时共享宿主网络。
- 敏感目录使用空挂载遮蔽，敏感文件和 Socket 使用不可用文件遮蔽；不存在的路径可忽略。
- 所有参与授权或遮蔽的路径必须先规范化并校验软链接。

## 12. Git 需求

### FR-016 仓库识别

- 初始化时必须识别普通仓库和 linked worktree 的 worktree gitdir 与 common dir。
- Git 元数据路径必须存在、归当前用户所有，并与当前 workspace 仓库关系一致。
- 不可信 `.git` 指针、错误 backpointer 或越界 worktree 布局必须导致安全模式 fail closed。
- 非 Git workspace 不得增加任何 workspace 外的 Git 写权限。

### FR-017 Git 可写边界

- `workspace-write` 应支持创建分支、暂存、生成对象、更新 refs、写日志、提交和 pack refs 等常规流程。
- common dir 必须保护 `config`、`hooks/`、`info/`、`objects/info/` 和其他 worktree 的元数据。
- 当前 worktree gitdir 必须保护 `commondir`、`gitdir`、`config.worktree` 等执行配置或指针文件。
- 必须阻止直接覆盖、rename、unlink 或通过父目录重建受保护路径。

## 13. 内置文件工具需求

### FR-018 workspace 根访问

- `read_file`、`write_file`、`edit_file` 必须使用 Go `os.Root` 作为文件访问根。
- 绝对路径只有在位于 workspace 内时才可转换为相对路径。
- 必须拒绝 `..` 越界、绝对路径越界、中间目录软链接、最终组件软链接和尾部 `/` 造成的越界。
- 所有模式都必须禁止文件工具直接修改 `.git`。
- HITL 修改工具参数后，必须重新执行 CommandGuard 和沙箱路径校验。

## 14. 敏感信息需求

### FR-019 默认遮蔽范围

安全模式默认遮蔽：

- SSH、GPG、AWS、Azure、GCP 和 Kubernetes 凭据。
- Docker/Podman 配置与登录凭据。
- Git credentials、`.netrc`、npm、PyPI 和 GitHub CLI 凭据。
- macOS Keychains。
- Docker、Podman、D-Bus、SSH Agent 和 GPG Agent Socket。

workspace 内的内容视为任务输入，不默认遮蔽其中的 `.env` 或其他凭据文件。用户必须确保传入 workspace 的内容适合被 Agent 读取。

## 15. 生命周期与并发需求

### FR-020 Manager 生命周期

- Runtime 初始化时创建唯一的 Sandbox Manager。
- Manager 必须线程安全地保存当前策略。
- 每条命令必须获得不可变策略快照和独立临时目录。
- 每个 stdio MCP 进程必须获得不可变策略快照和生命周期专用临时目录。
- Manager 关闭必须幂等，并清理运行时沙箱临时根目录。
- 主程序和测试必须显式调用 Runtime/Manager 的关闭逻辑。

### FR-021 Plan mode

- Plan mode 的 `execute_command` 必须强制覆盖为 `read-only`。
- 即使当前运行时为 `workspace-write` 或 `full-access`，Plan mode 也不得写入文件。
- Plan mode 仍使用当前安全网络偏好，不自动扩大网络权限。

## 16. 非功能需求

### NFR-001 安全性

- 安全模式不得因探测、策略构造或命令启动失败而自动回退无沙箱执行。
- 所有路径授权必须以规范化绝对路径为基础，并验证所有权、仓库关系和软链接。
- 错误和状态信息不得泄露环境变量值或凭据内容。

### NFR-002 兼容性

- Go 构建基线为 `1.26.5`。
- macOS 和 Ubuntu 必须具有独立 CI 验证。
- Ubuntu CI 必须显式安装 Bubblewrap，并要求 namespace 探测成功，不得用 skip 掩盖失败。

### NFR-003 可诊断性

- 用户必须能区分后端不可用、策略构造失败、沙箱启动失败、命令退出、超时和取消。
- 状态输出必须包含后端失败原因，但不得包含秘密值。

## 17. 验收标准

1. 三种模式的配置默认值、非法值、运行时切换和 Tab 补全均有单元测试。
2. 安全模式可以读取 workspace；`read-only` 禁止写，`workspace-write` 仅允许规定范围写。
3. `full-access` 始终展示网络开启，`network off` 返回明确错误。
4. 安全模式无法读取测试用敏感凭据，网络开启时仍无法连接受保护 Socket。
5. 安全模式网络关闭时无法访问本地测试服务，开启后可以访问。
6. 普通仓库和 linked worktree 可以完成常规 Git 操作，但不能修改配置、hooks、alternates 或其他 worktree 元数据。
7. 目录穿越和软链接逃逸测试全部失败关闭。
8. 子进程与孙进程继承限制，并在超时或取消后退出。
9. 后端探测失败时安全模式拒绝执行，切换到 `full-access` 后允许执行。
10. Plan mode 在所有运行时模式下都不能写入。
11. `go test ./...`、`go test -race ./...`、`go vet ./...` 以及 macOS/Linux 集成测试通过。
12. `read-only` 下 stdio MCP 初始化写和伪装只读工具写入均被原生后端拒绝。
13. `workspace-write` 下 stdio MCP 可写 workspace，但不能写宿主外部路径。
14. HTTP MCP 未分类工具默认拒绝，禁网时不初始化，显式只读且联网时可用。
15. mode/network 切换会终止旧 MCP 进程、刷新工具可见性，并在重启失败时保持新策略。

## 18. 当前实现约束

以下约束记录当前实现边界，后续增强时需要重新评估：

- Git 仓库拓扑在 Sandbox Manager 初始化时解析；运行过程中新增仓库、执行 `git init` 或改变 worktree 布局不会自动刷新策略。
- Linux 对不存在的受保护目标不会预创建遮蔽挂载；运行过程中创建的新 Git 特殊路径依赖上层 Git 边界与已有父目录策略。
- `full-access` 下 Shell 可以读取宿主凭据并访问本地 Socket，这是该兼容模式的预期行为，不属于原生沙箱保障范围。
- `full-access` 下 MCP 保持旧配置兼容并可执行全部工具，同样不受原生沙箱保障。
- HTTP MCP 的 `read-only` 是用户对远端实现的显式信任，不是 Bruce 能验证的本地隔离保证。

`full-access` 放宽 Shell 和 MCP 兼容行为，但不放宽内置文件工具。因此即使 Shell 或 MCP 能直接修改任意文件，`write_file` 和 `edit_file` 仍被限制在 workspace 且不能直接修改 `.git`。
