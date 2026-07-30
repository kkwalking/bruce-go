# 已知问题

## MCP initialize 错误未传播

- 状态：待处理
- 位置：`internal/mcp/mcp.go` 的 `initializeAndList`
- 现象：MCP `initialize` 请求的返回错误被忽略，随后仍会执行 `tools/list`。
- 影响：协议版本不兼容或服务端初始化失败时，最终错误可能表现为后续工具列表请求失败，降低诊断准确性。
- 后续修复：传播 `initialize` 错误并补充初始化失败测试；同时核对完整 MCP 初始化握手流程。
- 备注：该问题在本次并行工具执行器改造前已经存在，不纳入当前修复范围。
