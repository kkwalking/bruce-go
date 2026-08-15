# Bruce Go Coding Agent

Bruce Go Coding Agent 是 Bruce Coding Agent 的 Go 版本移植，保持 Java 版 README 中除 RAG 之外的用户可见功能面：ReAct、Claude Code 风格 Plan mode、HITL、并行工具调用、session/resume/tree/compact、模型切换、WebSearch/WebFetch、MCP stdio/Streamable HTTP、Skill 加载、AGENTS 指令加载、图片输入引用解析和常用 slash 命令。

RAG、Embedding、SQLite 向量库、代码索引、RAG slash 命令和 RAG 测试按需求未移植。

## 环境要求

- Go 1.26.5
- macOS：系统自带 `/usr/bin/sandbox-exec`（Seatbelt）
- Linux：系统安装的 Bubblewrap（`bwrap`）以及可用的 user/PID/mount namespace
- 可选：`~/.bruce/setting.json` 配置 LLM、WebSearch、MCP
- 可选：MCP server 命令或 Streamable HTTP endpoint

## 构建与运行

```bash
go build ./...
go run ./cmd/bruce --help
go run ./cmd/bruce
```

启动时默认读取 `~/.bruce/setting.json`。也可以指定配置路径：

```bash
go run ./cmd/bruce --settings ./setting.json
```

如需启动时暂不连接 MCP：

```bash
go run ./cmd/bruce --no-mcp
```

## 配置

核心结构兼容 Java 版：

```json
{
  "llm": {
    "defaultProvider": "openai_compatiable",
    "defaultModel": "local-model",
    "providers": {
      "openai_compatiable": {
        "apiKey": "your_key",
        "baseUrl": "http://localhost:9000/v1",
        "models": ["local-model"],
        "modelCapabilities": {
          "local-model": {
            "contextWindow": 128000,
            "maxOutputTokens": 8192
          }
        }
      }
    }
  },
  "webSearch": {
    "provider": "searxng",
    "searxng": {"url": "http://localhost:8080"}
  },
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
  },
  "compaction": {
    "enabled": true,
    "contextWindowRatio": 0.8,
    "reserveTokens": 16384,
    "keepRecentTokens": 20000
  },
  "sandbox": {
    "mode": "full-access",
    "networkAccess": false,
    "allowedEnv": []
  },
  "variables": {}
}
```

`llm.providers` 支持 `deepseek`、`glm` 和 `openai_compatiable`。测试使用 fake/mock，不依赖真实 API key。

自定义模型可通过 `modelCapabilities` 声明上下文窗口和最大输出 token；键必须同时出现在 `models` 中，配置值会覆盖内置模型能力。未配置 `contextWindow` 的自定义模型不会触发阈值自动压缩，但 API 返回的显式上下文溢出仍会被识别。

`/compact [instructions]` 使用当前模型生成英文结构化摘要，保留安全的 tool call/result 边界并累计已读/已修改文件。自动压缩在模型调用前和成功回合后按 `floor(contextWindow * contextWindowRatio) - reserveTokens` 检查，`contextWindowRatio` 默认是 `0.8`、合法范围为 `(0, 1]`，启用自动压缩时比例窗口必须大于 `reserveTokens`；上下文溢出时最多压缩并续跑一次，不会重复写入用户消息。`compaction.enabled=false` 仅关闭自动压缩，手动 `/compact` 仍可使用。Session JSONL 格式版本保持不变，旧 session 和缺少新增 assistant 元数据的记录仍可恢复。

`sandbox.mode` 仅允许 `read-only`、`workspace-write`、`full-access`。旧配置没有 `sandbox` 字段时会自动采用 `full-access`，该模式下网络始终开启。`networkAccess` 保存安全模式的网络偏好，因此默认仍为 `false`，切换到 `read-only` 或 `workspace-write` 后恢复禁网。非法 mode 或非法环境变量名会在启动时报错。`allowedEnv` 只接受精确环境变量名，列出的变量会追加到安全环境允许列表中。`commandTimeoutSeconds` 可选，设置 execute_command 的单命令超时（默认 30 秒，不能为负数）。

MCP server 的 `toolAccess` 使用远端工具原名精确声明最低权限，只接受 `read-only`、`workspace-write`、`full-access`，不支持通配符。未声明的工具在安全模式默认拒绝，但旧配置在 `full-access` 下仍保持全部工具可用。stdio MCP 在安全模式中与 Shell 一样由 Seatbelt/Bubblewrap 隔离；HTTP MCP 无法隔离远端进程，因此安全模式只允许用户显式信任为 `read-only` 的工具，配置 `workspace-write` 会在启动时报错。服务端 `readOnlyHint` 等 annotation 仅作提示，不会自动授权。

Linux 不捆绑 Bubblewrap，也不会自动回退到 Docker。可按发行版安装：

```bash
# Debian / Ubuntu
sudo apt-get install bubblewrap

# Fedora
sudo dnf install bubblewrap

# Arch Linux
sudo pacman -S bubblewrap
```

## Slash 命令

可用命令：

- `/react`
- `/minimal`
- `/plan [task|approve|reject|cancel|continue]`
- `/model [provider/model]`
- `/web on|off|status|search <query>|fetch <url>`
- `/mcp [restart|logs|disable|enable <name>]`
- `/skill list|show <name>|reload`
- `/hitl on|off|status`
- `/parallel on|off|status`
- `/sandbox status`
- `/sandbox mode read-only|workspace-write|full-access`
- `/sandbox network on|off`
- `/status`
- `/session`
- `/sessions`
- `/new`
- `/resume <id|path>`
- `/tree [entryId]`
- `/compact [instructions]`
- `/clear`
- `/help`
- `/exit`

不提供 `/rag`、`/index`、`/graph` 等 RAG 入口。

`/minimal` 切换到借鉴 DeepSeek Harness 极简 preset 的精简模式：切换时会**新建一个空会话**，不携带切换前的完整模式历史；系统提示词固定为 `You are a helpful software engineer assistant.`，不注入 AGENTS 指令、Skill 目录或工作目录等额外上下文，模型只能使用 `execute_command`、`read_file`、`write_file`、`edit_file` 四个基础工具。极简模式下显式 `$skill` 调用会被拒绝，`/react` 可切回完整模式。`/status` 仍展示进程级 MCP、Skill、沙箱等运行时信息，但其中的工具列表只包含极简模式的四个工具。

`/plan` 是只读 planning workflow：Planning Agent 可以读取和搜索项目、维护 `~/.bruce/plans/` 下的 markdown 计划，并把计划生命周期写入 session JSONL。只有执行 `/plan approve` 批准计划后，Bruce 才会切回 ReAct 并按批准计划执行；具体文件修改和命令仍受 HITL 设置约束。

## 原生沙箱

默认的 `full-access` 使用完整宿主环境、宿主网络和兼容 Shell 行为，网络在该模式下始终开启，但仍保留命令黑名单、超时、输出限制和进程树回收。需要原生隔离时可切换到 `workspace-write`，它允许 Shell 和已授权 stdio MCP 读取宿主工具链、写入 workspace 和受控 Git 元数据，但禁止写入宿主其他位置；`read-only` 连 workspace 也不可写。切回安全模式后恢复此前保存的网络开关，默认禁网。Slash 命令的切换只影响当前进程，不会回写 `setting.json`；一次性命令继续使用启动时快照，MCP transport 会在切换完成前关闭旧进程并按新策略重启。

安全模式使用受限环境变量和不加载用户启动脚本的 `/bin/bash --noprofile --norc -c`。`HOME` 可读以兼容工具配置但不可写，临时目录、Go/npm cache 和 XDG runtime/cache 会重定向到每条命令独立的 Bruce 临时目录。SSH/GPG、云厂商、Kubernetes、Docker、Git/npm/PyPI/GitHub CLI 凭据、macOS Keychains 以及 Docker/Podman/Agent Socket 默认不可读或不可访问。Git 的 refs、objects、index、日志和 linked worktree 正常流程可写，但配置、hooks、alternates 和其他已有 worktree 元数据受保护；文件工具在任何模式都不能直接修改 `.git`。

`/sandbox network on` 整体放开安全模式中 `execute_command` 和 stdio MCP 的 TCP/UDP 网络；Docker、Podman、SSH/GPG Agent 等 Unix Socket 仍被屏蔽。安全模式禁网时 HTTP MCP 不会初始化。WebSearch、WebFetch、LLM 请求和 Bruce 自身网络不受该开关控制；HTTP MCP 的远端进程也不在本机原生沙箱内。workspace 自身被视为任务输入，因此其中的 `.env` 不会自动隐藏。

后端缺失、namespace 被系统禁用或策略构造失败时，默认的 `full-access` 仍可执行 Shell；一旦切换到 `read-only` 或 `workspace-write`，Shell 会 fail closed，且不会自动无沙箱重试。此时可查看 `/sandbox status` 获取失败原因，并显式切回 `/sandbox mode full-access`。`workspace-write` 会拒绝文件系统根、用户 HOME 及其祖先作为 workspace，避免产生过宽写权限。Plan mode 的 Shell 在原生后端可用时强制 `read-only`（即使当前运行时是 `full-access`）；后端不可用且运行时为 `full-access` 时退回纯文本只读白名单校验，保持 plan mode 可用。

故障排查先运行 `/sandbox status` 查看 backend、availability、MCP enforcement、可用/阻止工具数和失败原因。Linux 常见原因是未安装 `bwrap`、内核禁用 unprivileged user namespace，或运行环境禁止创建 PID/mount namespace；Bruce 不会把这些错误降级成不安全执行。

## 输入语法

```text
$<skill> <task>
@image:<path>
@image:<file:///path with spaces.png>
@clipboard
```

Skill 扫描路径：

```text
~/.bruce/skills/<skill-name>/SKILL.md
<项目目录>/.bruce/skills/<skill-name>/SKILL.md
```

AGENTS 指令读取：

```text
~/.bruce/AGENTS.md
<Git root>/AGENTS.md
<Git root 到当前工作目录之间的子目录>/AGENTS.md
<当前工作目录>/AGENTS.md
```

## 测试

```bash
go test ./...
go test -race ./...
go vet ./...
```

网络、LLM 和 MCP 能力均可通过 fake 或 `httptest` 覆盖，测试不依赖真实外部服务。

沙箱集成测试默认在本机后端不可用时跳过；CI 设置 `BRUCE_REQUIRE_SANDBOX_TESTS=1`，要求 macOS Seatbelt 和 Ubuntu Bubblewrap 探测及隔离矩阵真实通过。
