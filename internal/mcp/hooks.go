package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file mirrors 参考/MaiBot/src/mcp_module/hooks.py: MCP 宿主回调声明。
// 用于向 connection 注入宿主侧可选能力(Sampling / Elicitation)。

// SamplingHost 是 Sampling 桥的最小接口:connection 只依赖「能处理一次
// sampling/createMessage 请求」这一能力,不依赖具体 LLM 类型
// (对齐 host_llm_bridge.MCPHostLLMBridge 的 handle_sampling_request)。
type SamplingHost interface {
	// HandleWithTools mirrors MCPHostLLMBridge.handle_sampling_request。
	// 返回 (nil, err) 时 go-sdk 会把 err 作为 JSON-RPC 错误回给服务端。
	HandleWithTools(ctx context.Context, request *mcp.CreateMessageWithToolsRequest) (*mcp.CreateMessageWithToolsResult, error)
}

// ElicitationHandler mirrors host_callbacks.elicitation_callback。
type ElicitationHandler func(ctx context.Context, request *mcp.ElicitRequest) (*mcp.ElicitResult, error)

// LoggingHandler mirrors host_callbacks.logging_callback：消费服务端
// notifications/message 日志（go-sdk 的 LoggingMessageHandler）。
type LoggingHandler func(ctx context.Context, request *mcp.LoggingMessageRequest)

// ToolListChangedHandler mirrors 「服务端工具列表变化」的通知回调；MCP 工具
// 因此可以在不断连的情况下重新发现。
type ToolListChangedHandler func(ctx context.Context, request *mcp.ToolListChangedRequest)

// HostCallbacks mirrors MCPHostCallbacks: MCP 宿主回调集合。
type HostCallbacks struct {
	// Sampling mirrors sampling_callback。
	Sampling SamplingHost
	// Elicitation mirrors elicitation_callback。
	Elicitation ElicitationHandler
	// Logging mirrors logging_callback。
	Logging LoggingHandler
	// ToolListChanged 处理 tools/list_changed 通知（宿主可据此刷新工具目录）。
	ToolListChanged ToolListChangedHandler
}
