package mcp

import (
	"context"
	"strings"

	"oh-my-qqbot/internal/chat/tools"
)

// This file mirrors 参考/MaiBot/src/mcp_module/provider.py: 把 MCP 工具适配进统一
// 工具层(internal/chat/tools 的 ToolRegistry)。
//
// 对接说明:
//   - Provider 本身对应 Python 的 `MCPToolProvider`(provider_name/provider_type
//     = "mcp"),提供 ListTools / Invoke / Close 三个动作。
//   - MCP 工具声明被转换成 tools.ToolSpec(schema 透传:go-sdk 的 inputSchema →
//     ParametersSchema / outputSchema → 无对应字段,保留在本包的 ToolSpec 中)。
//   - MCP 工具默认注册为 deferred(Visibility: VisibilityDeferred),与
//     view_forward_message 同机制:Planner 先看到 deferred 提醒,调用内置
//     tool_search 后 `DiscoverDeferredTools` 会把命中工具加入后续轮次的可用列表。
//   - 因为 tools.ToolRegistry 的 deferred 名册在 NewToolRegistry 时固定,装配层
//     需要:MCP 连接就绪 → provider.Entries() → NewToolRegistry(append(builtin, mcp...))。
//     工具条目内的 GetSpec/HandleTool 是闭包,始终读取服务当前状态。

// SessionContextResolver 由装配层注入:从工具执行上下文/调用元数据解析出会话
// 信息(权限判定需要 platform/user_id/session_id,而 tools.ToolInvocation 只携带
// SessionID 与 Metadata)。
type SessionContextResolver func(
	ctx context.Context,
	runtime *tools.RuntimeContext,
	invocation tools.ToolInvocation,
) SessionContext

// Provider mirrors MCPToolProvider。
type Provider struct {
	service *Service
	// resolver 是会话上下文解析器(权限判定),为空时使用 metadata 回退。
	resolver SessionContextResolver
	// deferred 控制 Entries() 产出的可见性(默认 deferred,对齐 §6.18)。
	deferred bool
}

// NewProvider mirrors MCPToolProvider.__init__。
func NewProvider(service *Service) *Provider {
	if service == nil {
		service = DefaultService()
	}
	return &Provider{service: service, deferred: true}
}

// SetSessionContextResolver 注入会话上下文解析器(装配层在运行时构建时调用)。
func (p *Provider) SetSessionContextResolver(resolver SessionContextResolver) {
	if p == nil {
		return
	}
	p.resolver = resolver
}

// SetDeferred 控制 MCP 工具的注册可见性(默认 true = 进 deferred 列表)。
// 设为 false 时 MCP 工具直接进入可见列表(不做 tool_search 发现)。
func (p *Provider) SetDeferred(deferred bool) {
	if p == nil {
		return
	}
	p.deferred = deferred
}

// Service 暴露被包装的共享服务。
func (p *Provider) Service() *Service {
	if p == nil {
		return nil
	}
	return p.service
}

// ProviderName mirrors provider_name = "mcp"。
func (p *Provider) ProviderName() string { return ProviderName }

// ProviderType mirrors provider_type = "mcp"。
func (p *Provider) ProviderType() string { return ProviderType }

// ---------------------------------------------------------------------------
// 声明转换与注册
// ---------------------------------------------------------------------------

// ToRegistrySpec 把本包的工具声明转换为 tools.ToolSpec(deferred 可见性 +
// provider_type="mcp")。
func ToRegistrySpec(spec ToolSpec) tools.ToolSpec {
	parameters := spec.ParametersSchema
	if len(parameters) == 0 {
		parameters = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return tools.ToolSpec{
		Name:             spec.Name,
		Description:      spec.LLMDescription(),
		ParametersSchema: parameters,
		ProviderName:     ProviderName,
		ProviderType:     ProviderType,
		Enabled:          spec.Enabled,
		Stage:            tools.StageAction,
		Visibility:       tools.VisibilityDeferred,
	}
}

// ListToolSpecs mirrors MCPToolProvider.list_tools：返回本包形态的全部 MCP 工具。
func (p *Provider) ListToolSpecs() []ToolSpec {
	if p == nil || p.service == nil {
		return nil
	}
	return p.service.ListTools()
}

// ListTools mirrors MCPToolProvider.list_tools 的统一工具层形态。
func (p *Provider) ListTools() []tools.ToolSpec {
	specs := p.ListToolSpecs()
	converted := make([]tools.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		converted = append(converted, ToRegistrySpec(spec))
	}
	return converted
}

// Entries mirrors MCPToolProvider 在统一注册表中的条目形态:每个 MCP 工具一个
// deferred BuiltinToolEntry,HandleTool 直接落到共享连接。
//
// 注意:tools.ToolRegistry 的 deferred 名册在 NewToolRegistry 时固定，因此
// MCP 连接发生热切换后，装配层应再次调用 Entries()/Register() 重建注册表
// （否则 deferred 名册会保留旧工具集）。
func (p *Provider) Entries() []tools.BuiltinToolEntry {
	specs := p.ListToolSpecs()
	entries := make([]tools.BuiltinToolEntry, 0, len(specs))
	for _, spec := range specs {
		spec := spec
		visibility := tools.VisibilityDeferred
		if !p.deferred {
			visibility = tools.VisibilityVisible
		}
		entries = append(entries, tools.BuiltinToolEntry{
			Name: spec.Name,
			GetSpec: func() tools.ToolSpec {
				// GetSpec 实时解析（工具可能在热重载后更新描述/schema；
				// 对不存在时才回退到注册时快照）。
				return ToRegistrySpec(p.currentSpec(spec))
			},
			HandleTool: p.handlerFor(spec),
			Stage:      tools.StageAction,
			Visibility: visibility,
			// 服务器断开/工具下线后不再暴露(对齐 provider.list_tools 的实时语义)。
			EnabledByConfig: func() bool { return p.service != nil && p.service.IsTool(spec.Name) },
		})
	}
	return entries
}

// Register 把 MCP 条目并入内置工具目录并构建注册表;MCP 工具默认 deferred,
// 经内置 tool_search 发现后激活。
//
// 热切换后请再次调用（注册表的 deferred 名册不会自动跟进）。
func (p *Provider) Register(builtin []tools.BuiltinToolEntry) (*tools.ToolRegistry, []tools.BuiltinToolEntry) {
	entries := append(append([]tools.BuiltinToolEntry{}, builtin...), p.Entries()...)
	return tools.NewToolRegistry(entries), entries
}

// currentSpec 返回共享服务里该工具的最新声明（不存在时回退到快照）。
func (p *Provider) currentSpec(fallback ToolSpec) ToolSpec {
	if p == nil || p.service == nil {
		return fallback
	}
	for _, spec := range p.service.ListTools() {
		if spec.Name == fallback.Name {
			return spec
		}
	}
	return fallback
}

// handlerFor 构造单个 MCP 工具的处理器:权限校验 → 远程调用 → 统一结果。
// 工具名由注册时的 spec 固定（获取 spec 走 currentSpec 实时刷新）。
func (p *Provider) handlerFor(spec ToolSpec) tools.Handler {
	toolName := spec.Name
	return func(ctx context.Context, runtime *tools.RuntimeContext, invocation tools.ToolInvocation) tools.ToolResult {
		if strings.TrimSpace(invocation.ToolName) == "" {
			invocation.ToolName = toolName
		}
		return p.Invoke(ctx, runtime, invocation)
	}
}

// ---------------------------------------------------------------------------
// 调用与权限
// ---------------------------------------------------------------------------

// Invoke 执行一次 MCP 工具调用（对齐 MCPToolProvider.invoke →
// MCPService.call_tool_invocation 的链路）。
func (p *Provider) Invoke(
	ctx context.Context,
	runtime *tools.RuntimeContext,
	invocation tools.ToolInvocation,
) tools.ToolResult {
	toolName := strings.TrimSpace(invocation.ToolName)
	if p == nil || p.service == nil {
		return tools.BuildFailureResult(toolName, "MCP 共享服务不可用", nil, nil)
	}

	session := p.resolveSession(ctx, runtime, invocation)
	rule := p.service.PermissionFor(toolName)
	if !rule.Allows(session) {
		return tools.BuildFailureResult(toolName, rule.DenyMessage(toolName), nil,
			map[string]any{
				"provider_type": ProviderType,
				"server_name":   serverNameOf(p.service, toolName),
				"permission":    "denied",
			})
	}

	result := p.service.CallTool(ctx, ToolInvocation{
		ToolName:      toolName,
		Arguments:     invocation.Arguments,
		CallID:        invocation.CallID,
		SessionID:     firstNonEmpty(invocation.SessionID, session.SessionID),
		StreamID:      session.StreamID,
		Reasoning:     invocation.Reasoning,
		LogicalTurnID: invocation.LogicalTurnID,
		Metadata:      invocation.Metadata,
	}, session)

	content := result.HistoryContent()
	metadata := map[string]any{"provider_type": ProviderType}
	for key, value := range result.Metadata {
		metadata[key] = value
	}
	structured := structuredMap(result.StructuredContent)
	if !result.Success {
		return tools.BuildFailureResult(toolName, result.ErrorMessage, structured, metadata)
	}
	return tools.BuildSuccessResult(toolName, content, structured, metadata, nil)
}

// structuredMap 把 MCP 的 structuredContent 归一化为 map(非对象时返回 nil,
// 内容仍完整保留在文本结果里)。
func structuredMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return nil
}

// resolveSession 取会话上下文:优先注入的解析器,其次调用元数据。
func (p *Provider) resolveSession(
	ctx context.Context,
	runtime *tools.RuntimeContext,
	invocation tools.ToolInvocation,
) SessionContext {
	if p != nil && p.resolver != nil {
		return p.resolver(ctx, runtime, invocation)
	}
	session := SessionContext{}
	if runtime != nil {
		session.SessionID = strings.TrimSpace(runtime.SessionID)
		if runtime.IsGroupSession {
			isGroup := true
			session.IsGroupChat = &isGroup
		}
	}
	if session.SessionID == "" {
		session.SessionID = strings.TrimSpace(invocation.SessionID)
	}
	// 回退:装配层可把平台/用户信息塞进 invocation.Metadata。
	if invocation.Metadata != nil {
		session.Platform = metadataString(invocation.Metadata, "platform")
		session.UserID = metadataString(invocation.Metadata, "user_id")
		session.GroupID = metadataString(invocation.Metadata, "group_id")
		if streamID := metadataString(invocation.Metadata, "stream_id"); streamID != "" {
			session.StreamID = streamID
		}
		if sessionID := metadataString(invocation.Metadata, "session_id"); sessionID != "" {
			session.SessionID = sessionID
		}
	}
	return session
}

// Close mirrors MCPToolProvider.close：Runtime 不拥有共享连接，因此关闭 Provider
// 时无需释放服务。
func (p *Provider) Close() error { return nil }

func metadataString(metadata map[string]any, key string) string {
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func serverNameOf(service *Service, toolName string) string {
	if service == nil {
		return ""
	}
	lease := service.Lease()
	defer lease.Release()
	manager := lease.Manager()
	if manager == nil {
		return ""
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.toolToServer[toolName]
}
