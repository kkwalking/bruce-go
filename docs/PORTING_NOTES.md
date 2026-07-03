# Porting Notes

## 设计取舍

- 不是 Java 逐类翻译。Go 版按职责组织为 `internal/{agent,approval,cli,config,event,instructions,integrated,llm,mcp,plan,render,runtime,session,skill,tool,tui,web}`。
- 运行时入口集中在 `internal/integrated`，slash 命令解析在 `internal/cli`，避免把终端 UI、命令解析和业务状态耦合在一起。
- LLM、Web、MCP 都通过小接口隔离，测试使用 fake 或 `httptest`，不需要真实 API key、真实网络服务或真实 MCP server。
- 工具执行使用显式错误返回和 `context.Context`；并行工具调用和 Plan DAG 执行使用 bounded goroutine。
- Session 继续使用 JSONL，恢复上下文时保留 active leaf、tree 分支和 compaction 节点。
- TUI 使用 Bubble Tea 生态替代 Java Lanterna。

## 依赖清单

直接依赖：

- `github.com/charmbracelet/bubbletea`: 终端应用主循环。
- `github.com/charmbracelet/bubbles`: TUI 文本输入组件。
- `github.com/charmbracelet/lipgloss`: TUI 样式。
- `github.com/PuerkitoBio/goquery`: WebFetch HTML 正文提取。
- `github.com/mattn/go-runewidth`: HITL 审批文本宽度计算。
- `golang.org/x/image`: 图片缩放和压缩处理。

间接依赖均来自以上库的传递依赖。未新增 RAG、SQLite 向量索引、JavaParser 或 Embedding 检索相关依赖。

## 与 Java 版差异

- RAG 未移植：`src/main/java/com/brucecli/rag`、RAG slash 命令、Embedding、SQLite 向量库、代码索引和相关测试均按需求排除。
- Go 版 TUI 是轻量 Bubble Tea 输入循环，核心命令和运行时可独立测试。
- MCP Streamable HTTP 以 JSON-RPC HTTP POST 为主，并能解析简单 SSE `data:` 响应；复杂 server 特性后续可在 `internal/mcp` 扩展。
- `/compact` 当前提供确定性本地摘要节点，自动压缩判断和 LLM 摘要能力在 `internal/session` 保留可测试接口。
- HITL 默认使用 auto-approve handler，真实交互 handler 可在 TUI 层替换。

## 测试策略

- ReAct 使用 fake LLM 覆盖 tool calling 循环。
- Plan 使用 table-driven JSON/DAG 和本地工具执行覆盖。
- Web 使用 `httptest` 覆盖 fetch/search，不访问真实互联网。
- MCP 使用内存 fake transport 和 `httptest` 覆盖工具注册、调用和 HTTP JSON-RPC。
- 图片输入使用临时 PNG 和 fake clipboard reader。
- 配置、Skill、AGENTS、session 均使用临时目录。
