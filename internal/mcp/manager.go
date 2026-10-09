package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file mirrors 参考/MaiBot/src/mcp_module/manager.py: 管理所有 MCP 服务器
// 连接，提供统一的工具 / Prompt / Resource 访问入口。

// builtinToolNames mirrors manager.BUILTIN_TOOL_NAMES: MCP 工具不允许与这些
// 内置工具名冲突（Go 版按 tools 包现有内置目录补充同类冲突名）。
var builtinToolNames = map[string]bool{
	"reply":     true,
	"no_action": true,
	"stop":      true,
	// Go 版内置工具目录（internal/chat/tools/catalog.go）。
	"wait":                 true,
	"view_forward_message": true,
	"query_memory":         true,
	"send_emoji":           true,
	"show_emoji_list":      true,
	"tool_search":          true,
	"fetch_history":        true,
}

// manager 对齐 MCPManager：持有全部服务器连接并维护工具/Prompt/资源路由表。
type manager struct {
	clientConfig ClientRuntimeConfig
	callbacks    *HostCallbacks

	mu               sync.RWMutex
	connections      map[string]*connection
	connectionError  map[string]string
	serverConfigs    map[string]ServerRuntimeConfig
	toolToServer     map[string]string
	promptToServer   map[string]string
	resourceToServer map[string]string
	templateToServer map[string]string
	statusCallbacks  []func()
}

// newManager 构造空管理器（连接由 connectAll 建立）。
func newManager(clientConfig ClientRuntimeConfig, callbacks *HostCallbacks) *manager {
	if callbacks == nil {
		callbacks = &HostCallbacks{}
	}
	return &manager{
		clientConfig:     clientConfig,
		callbacks:        callbacks,
		connections:      map[string]*connection{},
		connectionError:  map[string]string{},
		serverConfigs:    map[string]ServerRuntimeConfig{},
		toolToServer:     map[string]string{},
		promptToServer:   map[string]string{},
		resourceToServer: map[string]string{},
		templateToServer: map[string]string{},
	}
}

// managerFromConfig mirrors MCPManager.from_app_config：按配置建立管理器。
// 全部连接失败时仍返回包含诊断信息的管理器（对应 allow_empty=true 语义，
// M12 热切换需要保留失败服务器状态供 WebUI 展示）。
func managerFromConfig(
	ctx context.Context,
	config MCPConfig,
	callbacks *HostCallbacks,
) *manager {
	configs := BuildServerRuntimeConfigs(config)
	if len(configs) == 0 {
		return nil
	}
	manager := newManager(BuildClientRuntimeConfig(config), callbacks)
	manager.connectAll(ctx, configs)
	return manager
}

// connectAll mirrors _connect_all：并行建连，但注册顺序稳定（按配置顺序）。
func (m *manager) connectAll(ctx context.Context, configs []ServerRuntimeConfig) {
	serverConfigs := make(map[string]ServerRuntimeConfig, len(configs))
	connections := make([]*connection, len(configs))
	for index, config := range configs {
		serverConfigs[config.Name] = config
		connections[index] = newConnection(config, m.clientConfig, m.callbacks)
		// tools/list_changed → 重建该服务器的工具路由表并通知状态订阅者。
		connections[index].onToolsChanged = func() { m.refreshServerTools(config.Name) }
	}

	m.mu.Lock()
	m.serverConfigs = serverConfigs
	m.mu.Unlock()

	var wg sync.WaitGroup
	success := make([]bool, len(configs))
	for index, conn := range connections {
		wg.Add(1)
		go func(index int, conn *connection) {
			defer wg.Done()
			success[index] = conn.connect(ctx)
		}(index, conn)
	}
	wg.Wait()

	for index, config := range configs {
		if !success[index] {
			m.mu.Lock()
			m.connectionError[config.Name] = connections[index].lastErrorText()
			m.mu.Unlock()
			_ = connections[index].close()
			continue
		}
		m.mu.Lock()
		m.connections[config.Name] = connections[index]
		m.mu.Unlock()
		m.registerTools(config.Name, connections[index])
		m.registerPrompts(config.Name, connections[index])
		m.registerResources(config.Name, connections[index])
		m.registerResourceTemplates(config.Name, connections[index])
	}
}

// refreshServerTools 在服务端宣告工具列表变化后重建路由表（保持先注册优先）。
func (m *manager) refreshServerTools(serverName string) {
	m.mu.Lock()
	connection := m.connections[serverName]
	m.mu.Unlock()
	if connection == nil {
		return
	}

	// 先移除该服务器的旧条目，再按新列表重新注册（内置名/重名规则不变）。
	m.mu.Lock()
	for toolName, owner := range m.toolToServer {
		if owner == serverName {
			delete(m.toolToServer, toolName)
		}
	}
	m.mu.Unlock()
	m.registerTools(serverName, connection)
	m.notifyStatusChange()
}

// registerTools mirrors _register_tools：内置名冲突 / 跨服务器重名时跳过。
func (m *manager) registerTools(serverName string, conn *connection) int {
	registered := 0
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tool := range conn.toolList() {
		if tool == nil {
			continue
		}
		toolName := strings.TrimSpace(tool.Name)
		if toolName == "" {
			continue
		}
		if builtinToolNames[toolName] {
			loggerOrDefault(nil).Warn("MCP 工具与内置工具冲突，已跳过",
				"tool", toolName, "server", serverName)
			continue
		}
		if existing, ok := m.toolToServer[toolName]; ok {
			loggerOrDefault(nil).Warn("MCP 工具与其他服务器冲突，已跳过",
				"tool", toolName, "server", serverName, "existing_server", existing)
			continue
		}
		m.toolToServer[toolName] = serverName
		registered++
	}
	return registered
}

func (m *manager) registerPrompts(serverName string, conn *connection) int {
	conn.mu.RLock()
	prompts := append([]*mcp.Prompt{}, conn.prompts...)
	conn.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	registered := 0
	for _, prompt := range prompts {
		if prompt == nil {
			continue
		}
		name := strings.TrimSpace(prompt.Name)
		if name == "" {
			continue
		}
		if _, exists := m.promptToServer[name]; exists {
			continue
		}
		m.promptToServer[name] = serverName
		registered++
	}
	return registered
}

func (m *manager) registerResources(serverName string, conn *connection) int {
	conn.mu.RLock()
	resources := append([]*mcp.Resource{}, conn.resources...)
	conn.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	registered := 0
	for _, resource := range resources {
		if resource == nil {
			continue
		}
		uri := strings.TrimSpace(resource.URI)
		if uri == "" {
			continue
		}
		if _, exists := m.resourceToServer[uri]; exists {
			continue
		}
		m.resourceToServer[uri] = serverName
		registered++
	}
	return registered
}

func (m *manager) registerResourceTemplates(serverName string, conn *connection) int {
	conn.mu.RLock()
	templates := append([]*mcp.ResourceTemplate{}, conn.resourceTmpl...)
	conn.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	registered := 0
	for _, template := range templates {
		if template == nil {
			continue
		}
		uriTemplate := strings.TrimSpace(template.URITemplate)
		if uriTemplate == "" {
			continue
		}
		if _, exists := m.templateToServer[uriTemplate]; exists {
			continue
		}
		m.templateToServer[uriTemplate] = serverName
		registered++
	}
	return registered
}

// toolParametersSchema mirrors MCPManager._build_tool_parameters_schema。
func toolParametersSchema(tool *mcp.Tool) map[string]any {
	if tool == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	schema := schemaMap(tool.InputSchema)
	if len(schema) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	delete(schema, "$schema")
	return schema
}

// toolOutputSchema mirrors MCPManager._build_tool_output_schema。
func toolOutputSchema(tool *mcp.Tool) map[string]any {
	if tool == nil || tool.OutputSchema == nil {
		return nil
	}
	schema := schemaMap(tool.OutputSchema)
	if len(schema) == 0 {
		return nil
	}
	delete(schema, "$schema")
	return schema
}

// getToolSpecs mirrors MCPManager.get_tool_specs：返回全部已注册 MCP 工具的
// 统一声明（provider_name/provider_type = "mcp"，schema 透传）。
func (m *manager) getToolSpecs() []ToolSpec {
	m.mu.RLock()
	connections := make(map[string]*connection, len(m.connections))
	order := make([]string, 0, len(m.serverConfigs))
	for name := range m.serverConfigs {
		order = append(order, name)
	}
	for name, conn := range m.connections {
		connections[name] = conn
	}
	serverConfigs := make(map[string]ServerRuntimeConfig, len(m.serverConfigs))
	for name, config := range m.serverConfigs {
		serverConfigs[name] = config
	}
	m.mu.RUnlock()

	sort.Strings(order)
	specs := make([]ToolSpec, 0)
	for _, serverName := range order {
		conn, ok := connections[serverName]
		if !ok {
			continue
		}
		config := serverConfigs[serverName]
		for _, tool := range conn.toolList() {
			if tool == nil {
				continue
			}
			name := strings.TrimSpace(tool.Name)
			if name == "" {
				continue
			}
			m.mu.RLock()
			owner, registered := m.toolToServer[name]
			m.mu.RUnlock()
			if !registered || owner != serverName {
				continue
			}

			description := strings.TrimSpace(tool.Description)
			if description == "" {
				description = "来自 " + serverName + " 的 MCP 工具"
			}
			metadata := map[string]any{"server_name": serverName}
			for key, value := range metaToMap(tool.Meta) {
				metadata[key] = value
			}
			specs = append(specs, ToolSpec{
				Name:             name,
				Title:            strings.TrimSpace(tool.Title),
				Description:      description,
				ParametersSchema: toolParametersSchema(tool),
				OutputSchema:     toolOutputSchema(tool),
				ProviderName:     ProviderName,
				ProviderType:     ProviderType,
				Enabled:          true,
				Icons:            BuildToolIcons(tool.Icons),
				Annotation:       BuildToolAnnotation(tool.Annotations),
				Metadata:         metadata,
				ServerName:       serverName,
				Permission:       permissionOf(config),
			})
		}
	}
	return specs
}

// getStatusSnapshot mirrors MCPManager.get_status_snapshot。
func (m *manager) getStatusSnapshot() StatusSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	serverNames := make([]string, 0, len(m.serverConfigs))
	for name := range m.serverConfigs {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)

	servers := make([]ServerInfo, 0, len(serverNames))
	for _, name := range serverNames {
		config := m.serverConfigs[name]
		conn := m.connections[name]
		info := ServerInfo{
			Name:      name,
			Transport: string(config.Transport),
			Connected: conn != nil && conn.isConnected(),
			Error:     m.connectionError[name],
		}
		if conn != nil {
			info.ProtocolVersion = conn.protocolVersionValue()
			info.ToolCount = len(conn.toolList())
			if lastError := conn.lastErrorText(); lastError != "" {
				info.Error = lastError
			}
		}
		servers = append(servers, info)
	}

	return StatusSnapshot{
		Initialized: true,
		ServerCount: len(m.connections),
		ToolCount:   len(m.toolToServer),
		Servers:     servers,
	}
}

// addStatusChangeCallback mirrors MCPManager.add_status_change_callback。
// Go SDK 在连接断开时不会主动回调，这里记录订阅者并由 service 在状态刷新时
// 统一触发（保持「连接持有任务结束 → 刷新状态」的语义位置）。
func (m *manager) addStatusChangeCallback(callback func()) {
	if callback == nil {
		return
	}
	m.mu.Lock()
	m.statusCallbacks = append(m.statusCallbacks, callback)
	m.mu.Unlock()
}

func (m *manager) notifyStatusChange() {
	m.mu.RLock()
	callbacks := append([]func(){}, m.statusCallbacks...)
	m.mu.RUnlock()
	for _, callback := range callbacks {
		callback()
	}
}

// callTool mirrors MCPManager.call_tool_invocation。
func (m *manager) callTool(ctx context.Context, invocation ToolInvocation) ToolResult {
	toolName := strings.TrimSpace(invocation.ToolName)

	m.mu.RLock()
	serverName := m.toolToServer[toolName]
	conn := m.connections[serverName]
	m.mu.RUnlock()

	if serverName == "" || conn == nil {
		return ToolResult{
			ToolName:     toolName,
			Success:      false,
			ErrorMessage: fmt.Sprintf("MCP 工具 '%s' 未找到", toolName),
		}
	}
	return conn.callTool(ctx, toolName, invocation.Arguments)
}

// isTool 判断给定名称是否为已注册 MCP 工具。
func (m *manager) isTool(toolName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.toolToServer[toolName]
	return ok
}

// isPrompt 判断给定名称是否为已注册 MCP Prompt。
func (m *manager) isPrompt(promptName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.promptToServer[promptName]
	return ok
}

// isResource 判断给定 URI 是否为已注册 MCP Resource。
func (m *manager) isResource(uri string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.resourceToServer[uri]
	return ok
}

// getPrompt mirrors MCPManager.get_prompt。
func (m *manager) getPrompt(ctx context.Context, promptName string, arguments map[string]string) (*PromptResult, error) {
	m.mu.RLock()
	serverName := m.promptToServer[promptName]
	conn := m.connections[serverName]
	m.mu.RUnlock()
	if serverName == "" || conn == nil {
		return nil, fmt.Errorf("MCP Prompt '%s' 未找到", promptName)
	}
	return conn.getPrompt(ctx, promptName, arguments)
}

// readResource mirrors MCPManager.read_resource。
func (m *manager) readResource(ctx context.Context, uri string) (*ResourceReadResult, error) {
	m.mu.RLock()
	serverName := m.resourceToServer[uri]
	conn := m.connections[serverName]
	m.mu.RUnlock()
	if serverName == "" || conn == nil {
		return nil, fmt.Errorf("MCP Resource '%s' 未找到", uri)
	}
	return conn.readResource(ctx, uri)
}

// getToolSummary mirrors MCPManager.get_tool_summary。
func (m *manager) getToolSummary() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	serverNames := make([]string, 0, len(m.connections))
	for name := range m.connections {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)

	parts := make([]string, 0, len(serverNames))
	for _, serverName := range serverNames {
		conn := m.connections[serverName]
		toolNames := make([]string, 0)
		for _, tool := range conn.toolList() {
			if m.toolToServer[strings.TrimSpace(tool.Name)] == serverName {
				toolNames = append(toolNames, strings.TrimSpace(tool.Name))
			}
		}
		if len(toolNames) > 0 {
			parts = append(parts, "  • "+serverName+": "+strings.Join(toolNames, ", "))
		}
	}
	return strings.Join(parts, "\n")
}

// getFeatureSummary mirrors MCPManager.get_feature_summary。
func (m *manager) getFeatureSummary() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	serverNames := make([]string, 0, len(m.connections))
	for name := range m.connections {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)

	parts := make([]string, 0, len(serverNames))
	for _, serverName := range serverNames {
		conn := m.connections[serverName]
		toolCount := 0
		for _, tool := range conn.toolList() {
			if m.toolToServer[strings.TrimSpace(tool.Name)] == serverName {
				toolCount++
			}
		}
		promptCount := 0
		conn.mu.RLock()
		for _, prompt := range conn.prompts {
			if m.promptToServer[strings.TrimSpace(prompt.Name)] == serverName {
				promptCount++
			}
		}
		resourceCount := 0
		for _, resource := range conn.resources {
			if m.resourceToServer[strings.TrimSpace(resource.URI)] == serverName {
				resourceCount++
			}
		}
		templateCount := 0
		for _, template := range conn.resourceTmpl {
			if m.templateToServer[strings.TrimSpace(template.URITemplate)] == serverName {
				templateCount++
			}
		}
		conn.mu.RUnlock()
		parts = append(parts, fmt.Sprintf("  • %s: 工具 %d / Prompt %d / 资源 %d / 模板 %d",
			serverName, toolCount, promptCount, resourceCount, templateCount))
	}
	return strings.Join(parts, "\n")
}

// serverCount mirrors MCPManager.server_count。
func (m *manager) serverCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.connections)
}

// toolCount mirrors MCPManager.tool_count。
func (m *manager) toolCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.toolToServer)
}

// close mirrors MCPManager.close: 关闭全部连接并清空路由表。
func (m *manager) close() error {
	m.mu.Lock()
	connections := make([]*connection, 0, len(m.connections))
	for _, conn := range m.connections {
		connections = append(connections, conn)
	}
	m.connections = map[string]*connection{}
	m.connectionError = map[string]string{}
	m.serverConfigs = map[string]ServerRuntimeConfig{}
	m.toolToServer = map[string]string{}
	m.promptToServer = map[string]string{}
	m.resourceToServer = map[string]string{}
	m.templateToServer = map[string]string{}
	m.mu.Unlock()

	var firstError error
	for _, conn := range connections {
		if err := conn.close(); err != nil && firstError == nil {
			firstError = err
		}
	}
	return firstError
}

// permissionOf 收集单个服务器的权限列表（配置为空表示不限制）。
func permissionOf(config ServerRuntimeConfig) PermissionRule {
	return PermissionRule{
		AllowUsers: normalizeList(config.AllowUsers),
		AllowChats: normalizeList(config.AllowChats),
		Permission: normalizeList(config.Permission),
	}
}

// newLogicalTurnID mirrors ContextItemMeta.create 的 logical_turn_id 生成。
func newLogicalTurnID() string {
	return uuid.NewString()
}
