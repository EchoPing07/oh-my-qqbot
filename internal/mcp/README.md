# internal/mcp — MCP 客户端（M12）

对齐 `参考/MaiBot/src/mcp_module/`（10 文件 3247 行），基于
`github.com/modelcontextprotocol/go-sdk` v1.7.0 重写。进程级单例持有连接，
聊天 runtime 经 Provider 访问共享连接（不每会话起子进程）。

## 文件对照

| Python | Go | 说明 |
| --- | --- | --- |
| `config.py` | `config.go` | `MCPConfig` 及运行时配置转换；**导出类型供 `internal/config` 直接使用**（带 toml tag，字段名对齐 `official_configs.py`） |
| `models.py` | `models.go` | 内容块/注解/图标归一化（`build_tool_content_item` 系列） |
| `connection.py` | `connection.go` | 单服务器连接：三传输、能力发现、工具调用 |
| `manager.py` | `manager.go` | 多服务器管理器：工具/Prompt/资源路由表、状态快照 |
| `service.py` | `service.go` | 进程级单例 + 配置签名热切换 + lease 退休 |
| `provider.py` | `provider.go` | 统一工具层适配（`internal/chat/tools.ToolRegistry`） |
| `host_llm_bridge.py` | `sampling.go` | Host Sampling 桥（最小 LLM 接口注入） |
| `stdio_filter.py` | `stdio.go` | 容错 stdio（丢弃 stdout 非 JSON 噪声） |
| `hooks.py` | `hooks.go` | 宿主回调集合 |

## 传输

| transport | go-sdk | 说明 |
| --- | --- | --- |
| `stdio` | `tolerantStdioTransport`（内部用 `mcp.IOTransport`） | 真子进程；stdout 非 JSON 行只告警丢弃（对齐 stdio_filter） |
| `streamable_http` | `mcp.StreamableClientTransport` | `headers`（含 `authorization: bearer`）+ 读写超时走进 `http.Client` |
| `sse` | `mcp.SSEClientTransport` | 同上 |

超时语义照抄 `manager.connect_one`：非 stdio 为 `min(60s, max(5s, http_timeout+5s))`，
stdio 固定 30s；调用超时用 `read_timeout_seconds`。
会话生命周期与建连超时解耦（对应 Python 的独立 lifecycle task）：建连超时只
取消等待，连接由 `connection.close()` 结束，因此 SSE 之类长连接不会被建连
context 的取消破坏。

## 配置签名热切换

```go
svc := mcp.DefaultService()          // 进程级单例
svc.SetCallbacks(bridge.BuildCallbacks())
err := svc.EnsureInitialized(ctx, cfg) // 签名未变则直接返回，不重启连接
```

* `ConfigSignature(cfg)`：只对**有效运行时配置**（启用的 servers + 启用时的
  client 能力）做规范化 JSON + sha256，停用草稿变化不触发重启。
* 热切换时旧 manager 若仍有活跃 lease（在途工具调用）则进入 `retired`，
  引用归零后再关闭（`lease.Release()` 触发）。
* `StatusSnapshot()`：`initialized/server_count/tool_count/servers[]`
  （含每 server 的 `transport/connected/protocol_version/tool_count/error`），
  供 WebUI 的 mcp 路由使用。

## 工具接入（internal/chat/tools）

1. `provider.Register(builtinEntries)` 把 MCP 工具作为 **deferred** 条目并入
   内置目录，返回 `*tools.ToolRegistry`：

   ```go
   registry, _ := mcp.NewProvider(svc).Register(tools.BuiltinEntries(flags))
   ```

2. MCP 工具声明映射为 `tools.ToolSpec`：`ProviderType = "mcp"`、
   `Visibility = VisibilityDeferred`、`Stage = StageAction`，
   参数 schema 从 go-sdk 的 `inputSchema` 透传（剥离 `$schema`，空 schema
   补 `{"type":"object","properties":{}}`）。
3. Planner 先看到 `BuildDeferredToolsReminder()` 提醒 → 调用内置 `tool_search`
   → `DiscoverDeferredTools` 激活 → 后续轮次可用（与 `view_forward_message`
   同机制）。
4. 权限：`allow_users`（`qq:123456789`，平台前缀小写、user_id 原样）/
   `allow_chats`（session_id 或 stream_id）/ `permission`（管理权限列表）；
   三者全空 = 不限制。校验用 `SessionContextResolver` 注入的会话上下文
   （回退到 `ToolInvocation.Metadata`）。

## Sampling 桥

```go
bridge := mcp.NewHostLLMBridge(llm, cfg.Client.Sampling.TaskName) // llm 实现 SamplingLLM
svc.SetCallbacks(bridge.BuildCallbacks())
```

服务端 `sampling/createMessage` → 转发到任务模型（默认 `planner`）→
`CreateMessageWithToolsResult`（文本 + `tool_use`，`stopReason=toolUse/endTurn`）。
`toolChoice.mode == "required"` 且模型未返回工具调用时报协议错误（文案照抄）。
`include_context_support` 未实现，不向服务端声明虚假能力。
协议版本 ≥ 2026-07-28 时 go-sdk 走 SEP-2322 multi-round-trip，宿主只注册
`CreateMessageWithToolsHandler` 一个入口（基础 Sampling 请求由 SDK 下转）。

## 未实现（后续工作包）

* roots：配置与 `AddRoots` 已接，服务端 `list_roots` 在最新协议下走
  multi-round-trip（go-sdk 自动补全）。
* elicitation：`HostCallbacks.Elicitation` 已预留，配置校验已有，未接 UI。
* Prompt/Resource 主流程接入（`getPrompt`/`readResource` 已可用，但尚未暴露成
  工具/Prompt 目录）。
