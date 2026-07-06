# Bruce Go Coding Agent

Bruce Go Coding Agent 是 Bruce Coding Agent 的 Go 版本移植，保持 Java 版 README 中除 RAG 之外的用户可见功能面：ReAct、Claude Code 风格 Plan mode、HITL、并行工具调用、session/resume/tree/compact、模型切换、WebSearch/WebFetch、MCP stdio/Streamable HTTP、Skill 加载、AGENTS 指令加载、图片输入引用解析和常用 slash 命令。

RAG、Embedding、SQLite 向量库、代码索引、RAG slash 命令和 RAG 测试按需求未移植。

## 环境要求

- Go 1.24.2
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
        "models": ["local-model"]
      }
    }
  },
  "webSearch": {
    "provider": "searxng",
    "searxng": {"url": "http://localhost:8080"}
  },
  "mcp": {
    "servers": {}
  },
  "compaction": {
    "enabled": true,
    "reserveTokens": 16384,
    "keepRecentTokens": 20000
  },
  "variables": {}
}
```

`llm.providers` 支持 `deepseek`、`glm` 和 `openai_compatiable`。测试使用 fake/mock，不依赖真实 API key。

## Slash 命令

可用命令：

- `/react`
- `/plan [任务|approve|reject|cancel|continue]`
- `/model [provider/model]`
- `/web on|off|status|search <query>|fetch <url>`
- `/mcp [restart|logs|disable|enable <name>]`
- `/skill list|show <name>|reload`
- `/hitl on|off|status`
- `/parallel on|off|status`
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

`/plan` 是只读 planning workflow：Planning Agent 可以读取和搜索项目、维护 `~/.bruce/plans/` 下的 markdown 计划，并把计划生命周期写入 session JSONL。只有执行 `/plan approve` 批准计划后，Bruce 才会切回 ReAct 并按批准计划执行；具体文件修改和命令仍受 HITL 设置约束。

## 输入语法

```text
$<skill> <任务>
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
