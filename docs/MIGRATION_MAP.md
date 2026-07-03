# Migration Map

Source: `/Users/zhouzekun/code/bruce-cli/src/main/java/com/brucecli`

Target: `/Users/zhouzekun/code/bruce-go`

Counts verified during port:

- Non-RAG Java main sources: 133
- RAG Java main sources intentionally excluded: 14
- RAG Java tests intentionally excluded: 5

## Non-RAG Main Sources

| Java source | Go target | Behavior mapping |
| --- | --- | --- |
| `agent/Agent.java` | `internal/agent/agent.go` | ReAct loop, tool calling, streaming callbacks, image pruning |
| `approval/ApprovalPolicy.java` | `internal/approval/approval.go` | Tool risk and approval policy |
| `approval/ApprovalRequest.java` | `internal/approval/approval.go` | Approval request model and display text |
| `approval/ApprovalResult.java` | `internal/approval/approval.go` | Approval decisions and modified arguments |
| `approval/HitlHandler.java` | `internal/approval/approval.go` | HITL handler interface and auto handler |
| `config/BruceSettings.java` | `internal/config/settings.go` | `setting.json` structure |
| `config/BruceSettingsLoader.java` | `internal/config/settings.go` | Load/save defaults and compatibility |
| `event/BruceEvent.java` | `internal/event/event.go` | Event interface |
| `event/BruceEventBus.java` | `internal/event/event.go` | Pub/sub event bus |
| `event/BruceEventListener.java` | `internal/event/event.go` | Listener callback |
| `event/BruceEventSink.java` | `internal/event/event.go` | Event emission abstraction |
| `event/BruceEvents.java` | `internal/event/event.go` | Basic event and run id helpers |
| `event/package-info.java` | `internal/event/event.go` | Package documentation folded into Go package |
| `instructions/AgentInstructionsLoadResult.java` | `internal/instructions/agents.go` | AGENTS load result |
| `instructions/AgentInstructionsLoader.java` | `internal/instructions/agents.go` | AGENTS search order and size limit |
| `integrated/cli/CommandResult.java` | `internal/cli/cli.go`, `internal/integrated/runtime.go` | Slash command result |
| `integrated/cli/IntegratedCommandProcessor.java` | `internal/integrated/runtime.go` | Slash command execution |
| `integrated/cli/IntegratedMain.java` | `cmd/bruce/main.go`, `internal/tui/tui.go` | CLI entry and TUI startup |
| `integrated/runtime/AgentMode.java` | `internal/runtime/runtime.go` | ReAct/Plan mode enum |
| `integrated/runtime/IntegratedRuntime.java` | `internal/integrated/runtime.go` | Runtime wiring for LLM, tools, web, MCP, skills, session |
| `integrated/runtime/RuntimeStatus.java` | `internal/runtime/runtime.go`, `internal/render/render.go` | Status model and rendering |
| `llm/ChatClient.java` | `internal/llm/types.go` | Chat client interface |
| `llm/ChatClientFactory.java` | `internal/llm/factory.go` | Provider normalization and client construction |
| `llm/ChatResponse.java` | `internal/llm/types.go`, `internal/llm/openai.go` | Response model and parser |
| `llm/ContentPart.java` | `internal/llm/types.go` | Text/image content parts |
| `llm/DeepSeekClient.java` | `internal/llm/openai.go` | DeepSeek OpenAI-compatible client |
| `llm/FunctionCall.java` | `internal/llm/types.go` | Tool function call model |
| `llm/GlmClient.java` | `internal/llm/openai.go` | GLM OpenAI-compatible client |
| `llm/ImageProcessor.java` | `internal/llm/image.go` | Image loading, resizing, compression |
| `llm/ImageReferenceParser.java` | `internal/llm/image.go` | `@image:` and `@clipboard` parsing |
| `llm/Message.java` | `internal/llm/types.go` | Chat message model |
| `llm/MessageHistoryPruner.java` | `internal/agent/agent.go`, `internal/llm/types.go` | Older image pruning |
| `llm/ModelOption.java` | `internal/llm/types.go` | Model selector/display model |
| `llm/ModelSelectionService.java` | `internal/llm/factory.go`, `internal/integrated/runtime.go` | `/model` list/switch/write-back |
| `llm/OpenAiCompatiableClient.java` | `internal/llm/openai.go` | Compatible provider alias support |
| `llm/OpenAiCompatibleChatClient.java` | `internal/llm/openai.go` | HTTP/SSE OpenAI-compatible chat |
| `llm/PreparedUserInput.java` | `internal/llm/image.go` | Prepared input with content parts |
| `llm/SwitchableChatClient.java` | `internal/llm/factory.go` | Switchable model wrapper |
| `llm/ToolCall.java` | `internal/llm/types.go` | Tool call model |
| `llm/ToolDefinition.java` | `internal/llm/types.go` | Tool schema model |
| `mcp/config/McpConfig.java` | `internal/config/settings.go`, `internal/mcp/mcp.go` | MCP settings |
| `mcp/config/McpConfigLoader.java` | `internal/config/settings.go` | MCP config read from `setting.json` |
| `mcp/config/McpServerConfig.java` | `internal/config/settings.go` | MCP server setting |
| `mcp/config/McpTransportType.java` | `internal/mcp/mcp.go` | stdio/http transport selection |
| `mcp/protocol/McpClient.java` | `internal/mcp/mcp.go` | JSON-RPC MCP calls |
| `mcp/protocol/McpException.java` | `internal/mcp/mcp.go` | Explicit error returns |
| `mcp/protocol/McpProtocol.java` | `internal/mcp/mcp.go` | initialize, tools/list, tools/call |
| `mcp/protocol/McpSchemaSanitizer.java` | `internal/mcp/mcp.go`, `internal/tool/tool.go` | Tool schema pass-through and fallback schema |
| `mcp/protocol/McpTool.java` | `internal/mcp/mcp.go` | MCP tool descriptor |
| `mcp/runtime/LogRingBuffer.java` | `internal/mcp/mcp.go` | Server stderr ring buffer |
| `mcp/runtime/McpServerManager.java` | `internal/mcp/mcp.go` | Enable/disable/restart/status |
| `mcp/runtime/McpServerRuntime.java` | `internal/mcp/mcp.go` | Per-server runtime state |
| `mcp/runtime/McpServerState.java` | `internal/mcp/mcp.go` | Ready/enabled/error flags |
| `mcp/runtime/McpServerStatus.java` | `internal/mcp/mcp.go`, `internal/render/render.go` | Status rendering |
| `mcp/runtime/McpToolDescriptor.java` | `internal/mcp/mcp.go` | Registered MCP tool mapping |
| `mcp/transport/McpTransport.java` | `internal/mcp/mcp.go` | Transport interface |
| `mcp/transport/McpTransportFactory.java` | `internal/mcp/mcp.go` | Transport factory |
| `mcp/transport/StdioMcpTransport.java` | `internal/mcp/mcp.go` | stdio JSON-RPC transport |
| `mcp/transport/StreamableHttpMcpTransport.java` | `internal/mcp/mcp.go` | HTTP JSON-RPC and simple SSE response parsing |
| `plan/agent/PlanAndExecuteAgent.java` | `internal/plan/plan.go` | Plan-and-Execute agent |
| `plan/executor/ExecutionPlanExecutor.java` | `internal/plan/plan.go` | Plan executor |
| `plan/executor/ParallelPlanExecutor.java` | `internal/plan/plan.go` | Parallel DAG batch execution |
| `plan/executor/PlanExecutionReport.java` | `internal/plan/plan.go` | Plan report |
| `plan/executor/PlanExecutor.java` | `internal/plan/plan.go` | Executor interface folded into struct |
| `plan/executor/TaskExecutionResult.java` | `internal/plan/plan.go` | Task result fields on `Task` |
| `plan/model/ExecutionPlan.java` | `internal/plan/plan.go` | Plan model |
| `plan/model/PlanStatus.java` | `internal/plan/plan.go` | Plan status enum |
| `plan/model/Task.java` | `internal/plan/plan.go` | Task model |
| `plan/model/TaskStatus.java` | `internal/plan/plan.go` | Task status enum |
| `plan/model/TaskType.java` | `internal/plan/plan.go` | Task type enum |
| `plan/planner/DeepSeekPlanner.java` | `internal/plan/plan.go` | LLM planner with compatible chat client |
| `plan/planner/PlanJsonParser.java` | `internal/plan/plan.go` | Plan JSON parser |
| `plan/planner/Planner.java` | `internal/plan/plan.go` | Planner interface |
| `plan/util/DagValidator.java` | `internal/plan/plan.go` | Topological validation |
| `plan/util/PlanValidator.java` | `internal/plan/plan.go` | Plan validation |
| `render/BruceRenderer.java` | `internal/render/render.go`, `internal/tui/tui.go` | Text rendering helpers and TUI view |
| `render/BruceStatusInfo.java` | `internal/runtime/runtime.go`, `internal/render/render.go` | Status info model |
| `runtime/ConcurrencyConfig.java` | `internal/runtime/runtime.go` | Parallelism/timeouts/output truncation |
| `runtime/DaemonThreadFactory.java` | `internal/plan/plan.go`, `internal/tool/tool.go` | Goroutine-based parallel execution |
| `session/SessionContext.java` | `internal/session/session.go` | Session context |
| `session/SessionEntry.java` | `internal/session/session.go` | JSONL entry model |
| `session/SessionEventRecorder.java` | `internal/session/session.go`, `internal/integrated/runtime.go` | Session append operations |
| `session/SessionHeader.java` | `internal/session/session.go` | JSONL header |
| `session/SessionManager.java` | `internal/session/session.go` | Create/list/resume/tree/select/append |
| `session/SessionSummary.java` | `internal/session/session.go` | Session list summary |
| `session/compaction/CompactionDetails.java` | `internal/session/session.go` | Compaction details |
| `session/compaction/CompactionPreparation.java` | `internal/session/session.go` | Compaction preparation |
| `session/compaction/CompactionResult.java` | `internal/session/session.go` | Compaction result |
| `session/compaction/SessionCompactor.java` | `internal/session/session.go`, `internal/integrated/runtime.go` | Manual and helper compaction |
| `skill/SkillDefinition.java` | `internal/skill/skill.go` | Skill definition |
| `skill/SkillInvocation.java` | `internal/skill/skill.go` | Explicit skill invocation |
| `skill/SkillInvocationParser.java` | `internal/skill/skill.go` | `$skill` parser |
| `skill/SkillLoadResult.java` | `internal/skill/skill.go` | Load result |
| `skill/SkillLoader.java` | `internal/skill/skill.go` | Skill directory loader |
| `skill/SkillManager.java` | `internal/skill/skill.go` | Catalog and active skill tracking |
| `skill/SkillSource.java` | `internal/skill/skill.go` | User/project source enum |
| `skill/SkillToolRegistrar.java` | `internal/skill/skill.go` | `load_skill` and `read_skill_resource` tools |
| `tool/CommandGuard.java` | `internal/tool/tool.go` | Dangerous command guard |
| `tool/GuardedHitlToolRegistry.java` | `internal/tool/tool.go`, `internal/approval/approval.go` | HITL-protected registry |
| `tool/ParallelToolCallExecutor.java` | `internal/tool/tool.go` | Parallel tool executor |
| `tool/Param.java` | `internal/tool/tool.go` | JSON schema param helper |
| `tool/Tool.java` | `internal/tool/tool.go` | Tool definition |
| `tool/ToolCallExecutor.java` | `internal/tool/tool.go` | Tool call execution |
| `tool/ToolCallResult.java` | `internal/tool/tool.go` | Tool call result |
| `tool/ToolCallRunner.java` | `internal/tool/tool.go` | Single call runner |
| `tool/ToolExecutor.java` | `internal/tool/tool.go` | Executor function type |
| `tool/ToolRegistry.java` | `internal/tool/tool.go` | Registry and builtins |
| `tool/ToolResultContent.java` | `internal/tool/tool.go` | String result formatting |
| `tui/BruceCompletionEngine.java` | `internal/cli/cli.go`, `internal/tui/tui.go` | Command hints via help/command list |
| `tui/BruceSlashCommandHints.java` | `internal/cli/cli.go` | Slash command metadata |
| `tui/BruceSyntaxHighlighter.java` | `internal/tui/tui.go` | Simplified terminal rendering |
| `tui/BruceTuiApp.java` | `internal/tui/tui.go` | Bubble Tea app |
| `tui/CompletionItem.java` | `internal/cli/cli.go` | Command metadata row |
| `tui/LanternaBruceRenderer.java` | `internal/tui/tui.go`, `internal/render/render.go` | Bubble Tea/lipgloss replacement |
| `tui/LanternaHitlHandler.java` | `internal/approval/approval.go`, `internal/tui/tui.go` | HITL handler interface and TUI extension point |
| `tui/StyledSpan.java` | `internal/tui/tui.go` | Simplified text styling |
| `tui/TuiCommandHandler.java` | `internal/integrated/runtime.go` | TUI command dispatch |
| `tui/TuiCommandResult.java` | `internal/cli/cli.go` | Command result |
| `web/fetch/ExtractedContent.java` | `internal/web/web.go` | Extracted title/text |
| `web/fetch/FetchedPage.java` | `internal/web/web.go` | Fetched page model |
| `web/fetch/HtmlExtractor.java` | `internal/web/web.go` | goquery HTML extractor |
| `web/fetch/NetworkPolicy.java` | `internal/web/web.go` | URL/scheme/private network policy |
| `web/fetch/WebFetchFormatter.java` | `internal/web/web.go`, `internal/integrated/runtime.go` | Fetch output formatting |
| `web/fetch/WebFetcher.java` | `internal/web/web.go` | HTTP fetcher |
| `web/search/SearchProvider.java` | `internal/web/web.go` | Searcher interface |
| `web/search/SearchProviderFactory.java` | `internal/web/web.go` | Search provider factory |
| `web/search/SearxngSearchProvider.java` | `internal/web/web.go` | Searxng provider |
| `web/search/SerpApiSearchProvider.java` | `internal/web/web.go` | SerpAPI provider |
| `web/search/WebSearchConfig.java` | `internal/config/settings.go`, `internal/web/web.go` | Web search config |
| `web/search/WebSearchFormatter.java` | `internal/web/web.go`, `internal/integrated/runtime.go` | Search output formatting |
| `web/search/WebSearchResult.java` | `internal/web/web.go` | Search result model |
| `web/search/ZhipuSearchProvider.java` | `internal/web/web.go` | Zhipu/GLM provider |
| `web/tool/WebToolRegistrar.java` | `internal/web/web.go` | `web_search` and `web_fetch` tools |

## Intentionally Excluded RAG Main Sources

| Java source | Status | Reason |
| --- | --- | --- |
| `rag/chunk/CodeChunker.java` | Intentionally excluded | RAG/code indexing out of scope |
| `rag/embedding/EmbeddingClient.java` | Intentionally excluded | Embedding out of scope |
| `rag/graph/CodeAnalyzer.java` | Intentionally excluded | JavaParser/code graph out of scope |
| `rag/index/CodeIndex.java` | Intentionally excluded | RAG index out of scope |
| `rag/model/CodeChunk.java` | Intentionally excluded | RAG model out of scope |
| `rag/model/CodeRelation.java` | Intentionally excluded | RAG model out of scope |
| `rag/model/IndexProgress.java` | Intentionally excluded | RAG indexing out of scope |
| `rag/model/IndexProgressListener.java` | Intentionally excluded | RAG indexing out of scope |
| `rag/model/IndexStats.java` | Intentionally excluded | RAG indexing out of scope |
| `rag/search/CodeRetriever.java` | Intentionally excluded | RAG retrieval out of scope |
| `rag/search/RagQueryTokenizer.java` | Intentionally excluded | RAG retrieval out of scope |
| `rag/search/SearchResultFormatter.java` | Intentionally excluded | RAG retrieval out of scope |
| `rag/store/VectorStore.java` | Intentionally excluded | SQLite/vector store out of scope |
| `rag/tool/RagToolRegistrar.java` | Intentionally excluded | RAG tools and slash entries out of scope |

## Intentionally Excluded RAG Tests

| Java test | Status | Reason |
| --- | --- | --- |
| `rag/chunk/CodeChunkerTest.java` | Intentionally excluded | RAG/code indexing out of scope |
| `rag/embedding/EmbeddingClientTest.java` | Intentionally excluded | Embedding out of scope |
| `rag/graph/CodeAnalyzerTest.java` | Intentionally excluded | JavaParser/code graph out of scope |
| `rag/search/CodeIndexRetrieverTest.java` | Intentionally excluded | RAG retrieval out of scope |
| `rag/search/RagQueryTokenizerTest.java` | Intentionally excluded | RAG retrieval out of scope |
