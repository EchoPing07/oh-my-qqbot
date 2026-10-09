package mcp

import "strings"

// This file 收敛本包对外的统一工具层数据结构,对照
// 参考/MaiBot/src/core/tooling.py 的 ToolSpec / ToolInvocation /
// ToolExecutionResult / ToolExecutionContext 与
// 参考/MaiBot/src/mcp_module/manager.py 的工具注册输出。

// Provider 标识（对齐 MCPToolProvider.provider_name / provider_type）。
const (
	// ProviderName 对齐 provider_name = "mcp"。
	ProviderName = "mcp"
	// ProviderType 对齐 provider_type = "mcp"。
	ProviderType = "mcp"
)

// ToolSpec mirrors core.tooling.ToolSpec（MCP 工具形态）:
// schema 从 go-sdk 的 inputSchema / outputSchema 透传。
type ToolSpec struct {
	Name        string
	Title       string
	Description string
	// ParametersSchema 是 inputSchema 归一化后的 JSON Schema（$schema 已剥离）。
	ParametersSchema map[string]any
	// OutputSchema 是 outputSchema 归一化后的 JSON Schema（可空）。
	OutputSchema map[string]any
	// ProviderName / ProviderType 固定为 "mcp"。
	ProviderName string
	ProviderType string
	Enabled      bool
	Icons        []ToolIcon
	Annotation   *ToolAnnotation
	Metadata     map[string]any
	// ServerName 是工具所属的 MCP 服务器（对齐 metadata["server_name"]）。
	ServerName string
	// Permission 是该服务器的 allow_users/allow_chats/permission 规则。
	Permission PermissionRule
}

// LLMDescription mirrors ToolSpec.build_llm_description。
func (s ToolSpec) LLMDescription() string {
	return strings.TrimSpace(s.Description)
}

// ToolInvocation mirrors core.tooling.ToolInvocation。
type ToolInvocation struct {
	ToolName      string
	Arguments     map[string]any
	CallID        string
	SessionID     string
	StreamID      string
	Reasoning     string
	LogicalTurnID string
	Metadata      map[string]any
}

// ToolResult mirrors core.tooling.ToolExecutionResult。
type ToolResult struct {
	ToolName          string
	Success           bool
	Content           string
	ErrorMessage      string
	StructuredContent any
	ContentItems      []ToolContentItem
	Metadata          map[string]any
}

// HistoryContent mirrors ToolExecutionResult.get_history_content。
func (r ToolResult) HistoryContent() string {
	return HistoryText(r.Content, r.ContentItems, r.StructuredContent, r.ErrorMessage)
}

// StatusSnapshot mirrors MCPService.get_status_snapshot 的结构:
// initialized / server_count / tool_count / servers[]。
type StatusSnapshot struct {
	Initialized bool         `json:"initialized"`
	ServerCount int          `json:"server_count"`
	ToolCount   int          `json:"tool_count"`
	Servers     []ServerInfo `json:"servers"`
	// ConfigSignature 是本快照对应的配置签名（WebUI 用于判断是否需要热切换）。
	ConfigSignature string `json:"config_signature,omitempty"`
	// Loaded 表示 MCP 总开关是否启用（未启用时 servers 为空）。
	Loaded bool `json:"loaded"`
}

// EmptyStatusSnapshot mirrors `_update_status_snapshot(None, initialized=False)`。
func EmptyStatusSnapshot(initialized bool) StatusSnapshot {
	return StatusSnapshot{
		Initialized: initialized,
		ServerCount: 0,
		ToolCount:   0,
		Servers:     []ServerInfo{},
	}
}

// ---------------------------------------------------------------------------
// Prompt / Resource 结果模型（对齐 models.py 的 dataclass）
// ---------------------------------------------------------------------------

// PromptArgument mirrors MCPPromptArgumentSpec。
type PromptArgument struct {
	Name        string
	Description string
	Required    bool
}

// PromptResult mirrors MCPPromptResult。
type PromptResult struct {
	PromptName  string
	ServerName  string
	Description string
	Messages    []PromptMessage
	Metadata    map[string]any
}

// PromptMessage mirrors MCPPromptMessage。
type PromptMessage struct {
	Role    string
	Content ToolContentItem
}

// ResourceReadResult mirrors MCPResourceReadResult。
type ResourceReadResult struct {
	URI        string
	ServerName string
	Contents   []ToolContentItem
	Metadata   map[string]any
}

// ---------------------------------------------------------------------------
// 会话上下文与权限（对齐 core/tooling.py 的 ToolExecutionContext 与
// core/local_operator.py 的 has_command_permission 语义）
// ---------------------------------------------------------------------------

// SessionContext mirrors ToolExecutionContext / ToolAvailabilityContext 中
// 权限判定需要的字段。
type SessionContext struct {
	SessionID   string
	StreamID    string
	IsGroupChat *bool
	GroupID     string
	UserID      string
	Platform    string
}

// ScopedUserID mirrors local_operator.build_scoped_user_id：仅平台前缀转小写，
// user_id 保持原样（openid 等标识符大小写敏感）。
func (c SessionContext) ScopedUserID() string {
	platform := strings.ToLower(strings.TrimSpace(c.Platform))
	userID := strings.TrimSpace(c.UserID)
	if platform == "" || userID == "" {
		return ""
	}
	return platform + ":" + userID
}

// PermissionRule mirrors CommandPermissionConfig + PluginConfig.permission:
// 单个 MCP 服务器的调用权限规则。三者全空表示不限制。
type PermissionRule struct {
	AllowUsers []string
	AllowChats []string
	Permission []string
}

// IsUnrestricted 报告该规则是否不做任何限制。
func (r PermissionRule) IsUnrestricted() bool {
	return len(trimmedList(r.AllowUsers)) == 0 &&
		len(trimmedList(r.AllowChats)) == 0 &&
		len(trimmedList(r.Permission)) == 0
}

// Allows mirrors has_command_permission：管理权限或 allow_users / allow_chats
// 命中即放行（对照 normalize_operator_permissions 的规范化规则）。
func (r PermissionRule) Allows(session SessionContext) bool {
	if r.IsUnrestricted() {
		return true
	}

	scoped := session.ScopedUserID()
	if scoped != "" {
		for _, allowed := range normalizePermissionList(r.Permission) {
			if allowed == scoped {
				return true
			}
		}
		for _, allowed := range normalizePermissionList(r.AllowUsers) {
			if allowed == scoped {
				return true
			}
		}
	}

	sessionID := strings.TrimSpace(session.SessionID)
	if sessionID != "" {
		for _, chatID := range trimmedList(r.AllowChats) {
			if chatID == sessionID {
				return true
			}
		}
	}
	if streamID := strings.TrimSpace(session.StreamID); streamID != "" {
		for _, chatID := range trimmedList(r.AllowChats) {
			if chatID == streamID {
				return true
			}
		}
	}

	return false
}

// DenyMessage 是权限不通过时的回执文案（对齐工具失败回执风格）。
func (r PermissionRule) DenyMessage(toolName string) string {
	return "MCP 工具 '" + strings.TrimSpace(toolName) + "' 在当前会话没有调用权限。"
}

// normalizePermissionList mirrors normalize_operator_permissions。
func normalizePermissionList(values []string) []string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		platform, userID, found := strings.Cut(trimmed, ":")
		if !found {
			normalized = append(normalized, strings.ToLower(platform))
			continue
		}
		normalized = append(normalized, strings.ToLower(strings.TrimSpace(platform))+":"+strings.TrimSpace(userID))
	}
	return normalized
}

func trimmedList(values []string) []string {
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		if normalized := strings.TrimSpace(value); normalized != "" {
			trimmed = append(trimmed, normalized)
		}
	}
	return trimmed
}
