package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"oh-my-qqbot/internal/chat/tools"
)

// 本文件覆盖 MCP 工具接入统一工具层(internal/chat/tools)的语义:
// schema 透传、provider_type="mcp"、默认 deferred 注册、tool_search 激活、
// allow_users/allow_chats/permission 权限校验。

func TestProviderSchemaPassthrough(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	specs := service.ListTools()
	var echo *ToolSpec
	for index := range specs {
		if specs[index].Name == "echo" {
			echo = &specs[index]
		}
	}
	if echo == nil {
		t.Fatalf("未找到 echo 工具: %+v", specs)
	}

	// provider 标识（对齐 MCPToolProvider.provider_name/provider_type）。
	if echo.ProviderName != "mcp" || echo.ProviderType != "mcp" {
		t.Fatalf("provider 标识异常: %s/%s", echo.ProviderName, echo.ProviderType)
	}
	// schema 透传:inputSchema 的 properties / required / description 原样保留。
	properties, ok := echo.ParametersSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("inputSchema.properties 未透传: %#v", echo.ParametersSchema)
	}
	text, ok := properties["text"].(map[string]any)
	if !ok || text["type"] != "string" || text["description"] != "要回显的文本" {
		t.Fatalf("inputSchema property 未透传: %#v", properties["text"])
	}
	if _, ok := properties["count"].(map[string]any); !ok {
		t.Fatalf("inputSchema 第二个 property 丢失: %#v", properties)
	}
	if required, ok := echo.ParametersSchema["required"].([]any); !ok || len(required) != 1 || required[0] != "text" {
		t.Fatalf("inputSchema.required 未透传: %#v", echo.ParametersSchema["required"])
	}
	if _, exists := echo.ParametersSchema["$schema"]; exists {
		t.Fatalf("$schema 应被剥离")
	}
	// 标题/描述/元数据。
	if echo.Title != "回显工具" || echo.Description != "回显输入文本" {
		t.Fatalf("标题/描述异常: %+v", echo)
	}
	if echo.ServerName != "alpha" || echo.Metadata["server_name"] != "alpha" {
		t.Fatalf("server_name 元数据缺失: %+v", echo.Metadata)
	}
}

func TestToRegistrySpecMarksDeferredMCP(t *testing.T) {
	spec := ToolSpec{
		Name:             "echo",
		Description:      "回显输入文本",
		ParametersSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		ProviderName:     ProviderName,
		ProviderType:     ProviderType,
		Enabled:          true,
	}
	registrySpec := ToRegistrySpec(spec)
	if registrySpec.ProviderType != "mcp" || registrySpec.ProviderName != "mcp" {
		t.Fatalf("provider 标识异常: %+v", registrySpec)
	}
	if registrySpec.Visibility != tools.VisibilityDeferred {
		t.Fatalf("MCP 工具默认必须进 deferred 列表, got %s", registrySpec.Visibility)
	}
	if registrySpec.Stage != tools.StageAction {
		t.Fatalf("MCP 工具应为 action 阶段, got %s", registrySpec.Stage)
	}
	if !registrySpec.Enabled {
		t.Fatalf("工具应标记启用")
	}
	if registrySpec.Description != "回显输入文本" {
		t.Fatalf("描述异常: %q", registrySpec.Description)
	}

	// 空 schema 补齐为合法对象 schema（对齐 manager._build_tool_parameters_schema 的兜底）。
	empty := ToRegistrySpec(ToolSpec{Name: "x", Enabled: true})
	if empty.ParametersSchema["type"] != "object" {
		t.Fatalf("空 schema 应补齐: %#v", empty.ParametersSchema)
	}
}

func TestProviderDeferredRegistrationAndToolSearchActivation(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	provider := NewProvider(service)
	registry, entries := provider.Register(tools.BuiltinEntries(tools.FeatureFlags{BotNickname: "麦麦"}))

	// 内置 + MCP 条目一起注册。
	if len(entries) != len(tools.BuiltinEntries(tools.FeatureFlags{BotNickname: "麦麦"}))+2 {
		t.Fatalf("条目数异常: %d", len(entries))
	}

	// MCP 工具不在可见列表里（deferred）。
	for _, spec := range registry.VisibleToolSpecs() {
		if spec.Name == "echo" {
			t.Fatalf("deferred 的 MCP 工具不应出现在可见列表")
		}
	}

	// deferred 提醒中应当包含 MCP 工具（tool_search 的真实用武之地）。
	deferred := registry.DeferredToolSpecs()
	names := map[string]bool{}
	for _, spec := range deferred {
		names[spec.Name] = true
	}
	if !names["echo"] {
		t.Fatalf("MCP 工具应进入 deferred 列表: %+v", names)
	}
	reminder := registry.BuildDeferredToolsReminder()
	if !strings.Contains(reminder, "echo") || !strings.Contains(reminder, "tool_search") {
		t.Fatalf("deferred 提醒异常:\n%s", reminder)
	}

	// tool_search 搜索 → 激活。
	searchResult := tools.HandleToolSearchTool(registry, tools.ToolInvocation{
		ToolName:  "tool_search",
		Arguments: map[string]any{"query": "echo"},
	})
	if !searchResult.Success {
		t.Fatalf("tool_search 失败: %+v", searchResult)
	}
	if !strings.Contains(searchResult.Content, "echo") {
		t.Fatalf("tool_search 未命中: %q", searchResult.Content)
	}

	discovered := registry.DiscoveredToolSpecs()
	found := false
	for _, spec := range discovered {
		if spec.Name == "echo" && spec.ProviderType == "mcp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("激活后 echo 应出现在 discovered 列表: %+v", discovered)
	}

	// 激活后 MCP 工具可经统一注册表调用。
	result := registry.Invoke(context.Background(), &tools.RuntimeContext{SessionID: "s1"}, tools.ToolInvocation{
		ToolName:      "echo",
		Arguments:     map[string]any{"text": "hi"},
		SessionID:     "s1",
		LogicalTurnID: "turn-1",
	})
	if !result.Success {
		t.Fatalf("调用 MCP 工具失败: %+v", result)
	}
	if !strings.Contains(result.Content, "echo:hi") {
		t.Fatalf("结果内容异常: %q", result.Content)
	}
	// 图片内容块应保留在结构化内容之外（Go ToolResult 无 content_items 字段，
	// 图片摘要以文本形式进入 content）。
	if result.Metadata["provider_type"] != "mcp" {
		t.Fatalf("元数据缺少 provider_type: %+v", result.Metadata)
	}
	if result.Metadata["server_name"] != "alpha" {
		t.Fatalf("元数据缺少 server_name: %+v", result.Metadata)
	}
}

func TestProviderEntriesReenableAfterToolDisappears(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	provider := NewProvider(service)
	entries := provider.Entries()
	byName := map[string]tools.BuiltinToolEntry{}
	for _, entry := range entries {
		byName[entry.Name] = entry
	}
	echoEntry, ok := byName["echo"]
	if !ok {
		t.Fatalf("未生成 echo 条目: %+v", byName)
	}
	if !echoEntry.EnabledByConfig() {
		t.Fatalf("连接正常时 MCP 工具应可用")
	}

	// 关闭服务 → 工具下线 → EnabledByConfig 返回 false（对应 provider.list_tools
	// 的实时语义：共享服务关闭后不再暴露工具）。
	if err := service.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if echoEntry.EnabledByConfig() {
		t.Fatalf("服务关闭后 MCP 工具应不可用")
	}
}

func TestProviderPermissionRules(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()

	config := stdioTestConfig("alpha")
	config.Servers[0].AllowUsers = []string{"qq:123456789"}
	config.Servers[0].AllowChats = []string{"session-group-1"}
	config.Servers[0].Permission = []string{"qq:999"}
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	provider := NewProvider(service)
	registry, _ := provider.Register(tools.BuiltinEntries(tools.FeatureFlags{}))

	invoke := func(session SessionContext) tools.ToolResult {
		t.Helper()
		resolver := func(
			ctx context.Context,
			runtime *tools.RuntimeContext,
			invocation tools.ToolInvocation,
		) SessionContext {
			return session
		}
		provider.SetSessionContextResolver(resolver)
		return registry.Invoke(context.Background(), &tools.RuntimeContext{SessionID: session.SessionID}, tools.ToolInvocation{
			ToolName:  "echo",
			Arguments: map[string]any{"text": "perm"},
		})
	}

	// allow_users 命中（大小写：平台小写、user_id 原样）。
	allowed := invoke(SessionContext{SessionID: "other", UserID: "123456789", Platform: "QQ"})
	if !allowed.Success {
		t.Fatalf("allow_users 命中应放行: %+v", allowed)
	}
	// allow_chats 命中（session_id）。
	allowed = invoke(SessionContext{SessionID: "session-group-1", UserID: "1", Platform: "qq"})
	if !allowed.Success {
		t.Fatalf("allow_chats 命中应放行: %+v", allowed)
	}
	// permission 命中。
	allowed = invoke(SessionContext{SessionID: "other", UserID: "999", Platform: "qq"})
	if !allowed.Success {
		t.Fatalf("permission 命中应放行: %+v", allowed)
	}
	// 全部未命中 → 拒绝。
	denied := invoke(SessionContext{SessionID: "other", UserID: "5", Platform: "qq"})
	if denied.Success {
		t.Fatalf("未授权不应放行: %+v", denied)
	}
	if !strings.Contains(denied.ErrorMessage, "没有调用权限") {
		t.Fatalf("回执文案异常: %q", denied.ErrorMessage)
	}
	if denied.Metadata["permission"] != "denied" {
		t.Fatalf("拒绝元数据缺失: %+v", denied.Metadata)
	}
	// 不同平台同 user_id 不匹配（scoped user id 语义）。
	denied = invoke(SessionContext{SessionID: "other", UserID: "123456789", Platform: "discord"})
	if denied.Success {
		t.Fatalf("平台不匹配不应放行: %+v", denied)
	}
}

func TestPermissionRuleUnrestrictedAndScopedID(t *testing.T) {
	// 空规则 = 不限制。
	if !(PermissionRule{}).IsUnrestricted() {
		t.Fatalf("空规则应视为不限制")
	}
	if !(PermissionRule{}).Allows(SessionContext{}) {
		t.Fatalf("空规则应放行任意会话")
	}
	// 只有空白项也视为不限制。
	if !(PermissionRule{AllowUsers: []string{"  ", ""}}).IsUnrestricted() {
		t.Fatalf("空白项应被忽略")
	}

	// build_scoped_user_id / normalize_operator_permissions 语义。
	if got := (SessionContext{Platform: " QQ ", UserID: " AbC "}).ScopedUserID(); got != "qq:AbC" {
		t.Fatalf("scoped user id 异常: %q", got)
	}
	if got := (SessionContext{Platform: "", UserID: "x"}).ScopedUserID(); got != "" {
		t.Fatalf("缺平台时应返回空: %q", got)
	}
	rule := PermissionRule{AllowUsers: []string{"QQ:AbC"}}
	if !rule.Allows(SessionContext{Platform: "qq", UserID: "AbC"}) {
		t.Fatalf("权限列表规范化后应命中")
	}
	if rule.Allows(SessionContext{Platform: "qq", UserID: "abc"}) {
		t.Fatalf("user_id 大小写敏感（openid 语义）")
	}
	// stream_id 也可作为 allow_chats 的候选。
	if !(PermissionRule{AllowChats: []string{"stream-9"}}).Allows(SessionContext{StreamID: "stream-9"}) {
		t.Fatalf("allow_chats 应匹配 stream_id")
	}
}

func TestProviderInvokeWithoutService(t *testing.T) {
	provider := NewProvider(nil) // 回落到 DefaultService（未初始化）
	result := provider.Invoke(context.Background(), nil, tools.ToolInvocation{ToolName: "echo"})
	if result.Success {
		t.Fatalf("未初始化服务不应成功: %+v", result)
	}
	if !strings.Contains(result.ErrorMessage, "尚未连接") {
		t.Fatalf("错误文案异常: %q", result.ErrorMessage)
	}
	if err := provider.Close(); err != nil {
		t.Fatalf("Close 应为空实现: %v", err)
	}
	if provider.ProviderName() != "mcp" || provider.ProviderType() != "mcp" {
		t.Fatalf("provider 标识异常")
	}
}

func TestProviderResolvesSessionFromMetadata(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	config := stdioTestConfig("alpha")
	config.Servers[0].AllowUsers = []string{"qq:42"}
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	// 不注入 resolver:回退到 invocation.Metadata（装配层的无侵入路径）。
	provider := NewProvider(service)
	result := provider.Invoke(context.Background(), &tools.RuntimeContext{SessionID: "fallback"},
		tools.ToolInvocation{
			ToolName:  "echo",
			Arguments: map[string]any{"text": "meta"},
			Metadata:  map[string]any{"platform": "qq", "user_id": "42", "stream_id": "stream-x"},
		})
	if !result.Success {
		t.Fatalf("metadata 解析应放行: %+v", result)
	}
	if result.Metadata["chat_session_id"] != "fallback" {
		t.Fatalf("会话审计元数据缺失: %+v", result.Metadata)
	}
	if result.Metadata["chat_stream_id"] != "stream-x" {
		t.Fatalf("stream 审计元数据缺失: %+v", result.Metadata)
	}
}

func TestProviderServerErrorPropagation(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	provider := NewProvider(service)
	registry, _ := provider.Register(nil)

	result := registry.Invoke(context.Background(), &tools.RuntimeContext{SessionID: "s"}, tools.ToolInvocation{
		ToolName: "boom",
	})
	if result.Success {
		t.Fatalf("isError 的工具结果应映射为失败: %+v", result)
	}
	if !strings.Contains(result.ErrorMessage, "出错了") {
		t.Fatalf("错误信息未透传: %q", result.ErrorMessage)
	}

	// 未知工具（不是 MCP 工具名）→ 注册表级错误。
	result = registry.Invoke(context.Background(), &tools.RuntimeContext{SessionID: "s"}, tools.ToolInvocation{
		ToolName: "not_registered",
	})
	if result.Success {
		t.Fatalf("未知工具不应成功")
	}
}

func TestServiceLeaseProtectsInFlightCallDuringReload(t *testing.T) {
	// 并发热切换 + 在途调用：旧连接必须活到调用返回（lease 语义的核心保证）。
	release := make(chan struct{})
	entered := make(chan struct{})
	restore := installInMemoryTransport(t, testServerOptions{
		onCall: func(name string, args map[string]any) {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-release
		},
	})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	results := make(chan ToolResult, 1)
	go func() {
		results <- service.CallTool(context.Background(), ToolInvocation{
			ToolName:  "echo",
			Arguments: map[string]any{"text": "inflight"},
		}, SessionContext{SessionID: "inflight"})
	}()

	<-entered // 调用已进入远程处理

	// 热切换（签名变化）不得中断在途调用。
	changed := stdioTestConfig("alpha")
	changed.Servers[0].Args = []string{"--changed"}
	if err := service.Reload(context.Background(), changed); err != nil {
		t.Fatalf("热切换失败: %v", err)
	}

	close(release)
	result := <-results
	if !result.Success {
		t.Fatalf("在途调用应成功: %+v", result)
	}
	if !strings.Contains(result.Content, "echo:inflight") {
		t.Fatalf("在途调用结果异常: %q", result.Content)
	}
	waitFor(t, 5*time.Second, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return len(service.retiredManagers) == 0
	}, "旧管理器应被回收")
}

func TestRefreshServerToolsRebuildsRouting(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	if !service.IsTool("echo") {
		t.Fatalf("初始应注册 echo")
	}

	// 模拟服务端下线工具：直接清空连接的工具缓存后刷新。
	service.mu.Lock()
	connection := service.manager.connections["alpha"]
	service.mu.Unlock()
	connection.mu.Lock()
	connection.tools = nil
	connection.mu.Unlock()

	service.mu.Lock()
	manager := service.manager
	service.mu.Unlock()
	manager.refreshServerTools("alpha")

	if service.IsTool("echo") {
		t.Fatalf("刷新后 echo 应被移除: %+v", service.ListTools())
	}
	// 未知服务器刷新不得 panic。
	manager.refreshServerTools("ghost")
}

func TestDefaultServiceIsSingleton(t *testing.T) {
	if DefaultService() != DefaultService() {
		t.Fatalf("DefaultService 必须是进程级单例")
	}
}

func TestProviderSetDeferredMakesToolsVisible(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	provider := NewProvider(service)
	provider.SetDeferred(false)
	registry, _ := provider.Register(tools.BuiltinEntries(tools.FeatureFlags{}))
	found := false
	for _, spec := range registry.VisibleToolSpecs() {
		if spec.Name == "echo" && spec.ProviderType == "mcp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("SetDeferred(false) 后 MCP 工具应进入可见列表: %+v", registry.VisibleToolSpecs())
	}
	if len(registry.DeferredToolSpecs()) == 0 {
		// 内置的 view_forward_message 仍在 deferred 中，MCP 工具不应出现。
		t.Fatalf("内置 deferred 工具不应消失")
	}
	for _, spec := range registry.DeferredToolSpecs() {
		if spec.ProviderType == "mcp" {
			t.Fatalf("SetDeferred(false) 后 MCP 工具不应在 deferred 列表: %+v", spec)
		}
	}
}

func TestProviderEntriesReflectSpecRefresh(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	provider := NewProvider(service)
	var echoEntry tools.BuiltinToolEntry
	for _, entry := range provider.Entries() {
		if entry.Name == "echo" {
			echoEntry = entry
		}
	}
	spec := echoEntry.GetSpec()
	if spec.Description != "回显输入文本" {
		t.Fatalf("描述异常: %q", spec.Description)
	}
	if spec.ParametersSchema["type"] != "object" {
		t.Fatalf("schema 异常: %+v", spec.ParametersSchema)
	}
	// GetSpec 动态解析：服务关闭后回退到注册时快照（不 panic）。
	_ = service.Close()
	if fallback := echoEntry.GetSpec(); fallback.Name != "echo" {
		t.Fatalf("回退快照异常: %+v", fallback)
	}
}
