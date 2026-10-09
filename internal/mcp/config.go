// Package mcp implements the MCP (Model Context Protocol) client of
// oh-my-qqbot, mirroring 参考/MaiBot/src/mcp_module/ (10 file, 3247 行) on top of
// github.com/modelcontextprotocol/go-sdk v1.7.0.
//
// 结构对照:
//
//	Python                          Go
//	mcp_module/config.py            config.go      —— MCPConfig 及运行时配置转换
//	mcp_module/models.py            models.go      —— 内容块/注解/图标归一化
//	mcp_module/connection.py        connection.go  —— 单服务器连接(三传输)
//	mcp_module/manager.py           manager.go     —— 多服务器管理器 + 工具注册
//	mcp_module/service.py           service.go     —— 进程级单例 + 签名热切换 + lease
//	mcp_module/provider.py          provider.go    —— 统一工具层适配(ToolRegistry)
//	mcp_module/host_llm_bridge.py   sampling.go    —— Host Sampling 桥
//	mcp_module/stdio_filter.py      stdio.go       —— 容错 stdio(丢弃非 JSON 噪声)
//
// 依赖边界:本包只 import internal/chat/tools 与标准库 + go-sdk;
// 不 import internal/config(配置类型定义在本包内,供 internal/config 直接使用)、
// internal/qq、internal/llm 具体类型(Sampling LLM 走最小接口注入)。
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// This file mirrors 参考/MaiBot/src/config/official_configs.py 的 MCPConfig 及其
// 子配置(MCPClientConfig / MCPServerItemConfig / MCPSamplingConfig /
// MCPRootsConfig / MCPRootItemConfig / MCPElicitationConfig /
// MCPAuthorizationConfig),字段名与默认值照抄,以便 internal/config 直接使用。

// ---------------------------------------------------------------------------
// 配置类型(与 official_configs.MCPConfig 同构)
// ---------------------------------------------------------------------------

// MCPAuthorizationConfig 对齐 MCPAuthorizationConfig: MCP HTTP 认证配置。
type MCPAuthorizationConfig struct {
	// Mode 对齐 mode: none | bearer。
	Mode string `toml:"mode"`
	// BearerToken 对齐 bearer_token: 只在 mode 为 bearer 时使用。
	BearerToken string `toml:"bearer_token"`
}

// MCPRootItemConfig 对齐 MCPRootItemConfig: 单个 MCP Root 配置。
type MCPRootItemConfig struct {
	Enabled bool   `toml:"enabled"`
	URI     string `toml:"uri"`
	Name    string `toml:"name"`
}

// MCPRootsConfig 对齐 MCPRootsConfig: MCP Roots 能力配置。
type MCPRootsConfig struct {
	Enable bool                `toml:"enable"`
	Items  []MCPRootItemConfig `toml:"items"`
}

// MCPSamplingConfig 对齐 MCPSamplingConfig: MCP Sampling 能力配置。
type MCPSamplingConfig struct {
	Enable bool `toml:"enable"`
	// TaskName 对齐 task_name: Sampling 调用模型时使用的任务名(默认 planner)。
	TaskName string `toml:"task_name"`
	// IncludeContextSupport 对齐 include_context_support(宿主桥当前不实现)。
	IncludeContextSupport bool `toml:"include_context_support"`
	// ToolSupport 对齐 tool_support: Sampling 过程中是否允许继续使用工具。
	ToolSupport bool `toml:"tool_support"`
}

// MCPElicitationConfig 对齐 MCPElicitationConfig: MCP Elicitation 能力配置。
type MCPElicitationConfig struct {
	Enable    bool `toml:"enable"`
	AllowForm bool `toml:"allow_form"`
	AllowURL  bool `toml:"allow_url"`
}

// MCPClientConfig 对齐 MCPClientConfig: MCP 客户端宿主能力配置。
type MCPClientConfig struct {
	ClientName    string               `toml:"client_name"`
	ClientVersion string               `toml:"client_version"`
	Roots         MCPRootsConfig       `toml:"roots"`
	Sampling      MCPSamplingConfig    `toml:"sampling"`
	Elicitation   MCPElicitationConfig `toml:"elicitation"`
}

// MCPServerItemConfig 对齐 MCPServerItemConfig: 单个 MCP 服务器配置。
type MCPServerItemConfig struct {
	Name               string                 `toml:"name"`
	Enabled            bool                   `toml:"enabled"`
	Transport          string                 `toml:"transport"` // stdio|streamable_http|sse
	Command            string                 `toml:"command"`
	Args               []string               `toml:"args"`
	Env                map[string]string      `toml:"env"`
	URL                string                 `toml:"url"`
	Headers            map[string]string      `toml:"headers"`
	HTTPTimeoutSeconds float64                `toml:"http_timeout_seconds"`
	ReadTimeoutSeconds float64                `toml:"read_timeout_seconds"`
	Authorization      MCPAuthorizationConfig `toml:"authorization"`

	// AllowUsers / AllowChats / Permission 收录 §6.18 与 §8 的 MCP 权限字段
	// (「MCP 配置 schema ... authorization/allow_*」)。Python 版 MCPConfig 未直接
	// 携带这三项,语义照抄同文件的 CommandPermissionConfig + PluginConfig.permission:
	//   - allow_users: 允许调用该服务器工具的用户,格式 qq:123456789
	//   - allow_chats: 允许调用该服务器工具的真实聊天流 ID
	//   - permission:  拥有管理权限的用户列表;非空时该服务器的工具对这一批用户开放
	// 三者全空表示不限制。
	AllowUsers []string `toml:"allow_users"`
	AllowChats []string `toml:"allow_chats"`
	Permission []string `toml:"permission"`
}

// MCPConfig 对齐 MCPConfig: MCP 总配置。
type MCPConfig struct {
	// Enable 对齐 enable: 是否启用 MCP 工具接入能力(默认 true)。
	Enable bool `toml:"enable"`
	// Client 对齐 client: 麦麦作为 MCP 客户端时声明的能力。
	Client MCPClientConfig `toml:"client"`
	// Servers 对齐 servers: 要连接的 MCP 服务器列表。
	Servers []MCPServerItemConfig `toml:"servers"`
}

// DefaultMCPConfig mirrors config.defaultMCPConfig (official_configs.MCPConfig
// 的默认值)。
func DefaultMCPConfig() MCPConfig {
	return MCPConfig{
		Enable: true,
		Client: MCPClientConfig{
			ClientName:    "MaiBot",
			ClientVersion: "1.0.0",
			Roots:         MCPRootsConfig{Enable: false, Items: []MCPRootItemConfig{}},
			Sampling:      MCPSamplingConfig{Enable: false, TaskName: "planner", IncludeContextSupport: false, ToolSupport: false},
			Elicitation:   MCPElicitationConfig{Enable: false, AllowForm: true, AllowURL: false},
		},
		Servers: []MCPServerItemConfig{},
	}
}

// Validate mirrors MCPServerItemConfig.model_post_init + MCPConfig.model_post_init
// 的校验语义(启用项必填、服务名唯一、bearer 必须有 token)。
func (c MCPConfig) Validate() error {
	seen := map[string]bool{}
	for _, server := range c.Servers {
		if !server.Enabled {
			continue
		}
		name := strings.TrimSpace(server.Name)
		if name == "" {
			return fmt.Errorf("MCPServerItemConfig.name 不能为空")
		}
		if seen[name] {
			return fmt.Errorf("MCP 配置中的服务器名称不能重复: %s", name)
		}
		seen[name] = true

		switch NormalizeTransport(server.Transport) {
		case TransportStdio:
			if strings.TrimSpace(server.Command) == "" {
				return fmt.Errorf("MCP 服务器 %s 使用 stdio 时必须填写 command", name)
			}
		case TransportStreamableHTTP, TransportSSE:
			if strings.TrimSpace(server.URL) == "" {
				return fmt.Errorf("MCP 服务器 %s 使用 %s 时必须填写 url", name, NormalizeTransport(server.Transport))
			}
		default:
			return fmt.Errorf("MCP 服务器 %s 使用了未知传输类型: %s", name, server.Transport)
		}

		if strings.EqualFold(strings.TrimSpace(server.Authorization.Mode), "bearer") &&
			strings.TrimSpace(server.Authorization.BearerToken) == "" {
			return fmt.Errorf("MCP 服务器 %s 使用 bearer 认证时必须填写 bearer_token", name)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 传输类型
// ---------------------------------------------------------------------------

// Transport 是 MCP 服务器传输类型(config.transport)。
type Transport string

// Transport values（对齐 MCPServerRuntimeConfig.transport_type）。
const (
	TransportStdio          Transport = "stdio"
	TransportStreamableHTTP Transport = "streamable_http"
	TransportSSE            Transport = "sse"
	TransportUnknown        Transport = "unknown"
)

// NormalizeTransport mirrors MCPServerItemConfig.transport 的 Literal 取值。
func NormalizeTransport(raw string) Transport {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(TransportStdio):
		return TransportStdio
	case string(TransportStreamableHTTP):
		return TransportStreamableHTTP
	case string(TransportSSE):
		return TransportSSE
	default:
		return TransportUnknown
	}
}

// ---------------------------------------------------------------------------
// 运行时配置（config.py 的 dataclass 形态）
// ---------------------------------------------------------------------------

// AuthorizationRuntimeConfig 对齐 MCPAuthorizationRuntimeConfig。
type AuthorizationRuntimeConfig struct {
	Mode        string `json:"mode"`
	BearerToken string `json:"bearer_token"`
}

// RootRuntimeConfig 对齐 MCPRootRuntimeConfig。
type RootRuntimeConfig struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// ClientRuntimeConfig 对齐 MCPClientRuntimeConfig。
type ClientRuntimeConfig struct {
	ClientName                    string              `json:"client_name"`
	ClientVersion                 string              `json:"client_version"`
	EnableRoots                   bool                `json:"enable_roots"`
	Roots                         []RootRuntimeConfig `json:"roots"`
	EnableSampling                bool                `json:"enable_sampling"`
	SamplingTaskName              string              `json:"sampling_task_name"`
	SamplingIncludeContextSupport bool                `json:"sampling_include_context_support"`
	SamplingToolSupport           bool                `json:"sampling_tool_support"`
	EnableElicitation             bool                `json:"enable_elicitation"`
	ElicitationAllowForm          bool                `json:"elicitation_allow_form"`
	ElicitationAllowURL           bool                `json:"elicitation_allow_url"`
}

// ServerRuntimeConfig 对齐 MCPServerRuntimeConfig。
type ServerRuntimeConfig struct {
	Name               string                     `json:"name"`
	Transport          Transport                  `json:"transport"`
	Command            string                     `json:"command"`
	Args               []string                   `json:"args"`
	Env                map[string]string          `json:"env"`
	URL                string                     `json:"url"`
	Headers            map[string]string          `json:"headers"`
	HTTPTimeoutSeconds float64                    `json:"http_timeout_seconds"`
	ReadTimeoutSeconds float64                    `json:"read_timeout_seconds"`
	Authorization      AuthorizationRuntimeConfig `json:"authorization"`

	AllowUsers []string `json:"allow_users"`
	AllowChats []string `json:"allow_chats"`
	Permission []string `json:"permission"`
}

// TransportType mirrors MCPServerRuntimeConfig.transport_type: 配置完整时返回
// 实际可用的传输类型,否则 unknown。
func (c ServerRuntimeConfig) TransportType() Transport {
	switch c.Transport {
	case TransportStdio:
		if c.Command != "" {
			return TransportStdio
		}
	case TransportStreamableHTTP:
		if c.URL != "" {
			return TransportStreamableHTTP
		}
	case TransportSSE:
		if c.URL != "" {
			return TransportSSE
		}
	}
	return TransportUnknown
}

// BuildHTTPHeaders mirrors MCPServerRuntimeConfig.build_http_headers:
// 归一化 headers,并在 bearer 模式下追加 Authorization。
func (c ServerRuntimeConfig) BuildHTTPHeaders() map[string]string {
	headers := make(map[string]string, len(c.Headers)+1)
	for key, value := range c.Headers {
		headers[key] = value
	}
	if strings.EqualFold(strings.TrimSpace(c.Authorization.Mode), "bearer") {
		if token := strings.TrimSpace(c.Authorization.BearerToken); token != "" {
			headers["Authorization"] = "Bearer " + token
		}
	}
	return headers
}

// BuildClientRuntimeConfig mirrors build_mcp_client_runtime_config。
func BuildClientRuntimeConfig(config MCPConfig) ClientRuntimeConfig {
	client := config.Client

	roots := make([]RootRuntimeConfig, 0, len(client.Roots.Items))
	for _, root := range client.Roots.Items {
		uri := strings.TrimSpace(root.URI)
		if !root.Enabled || uri == "" {
			continue
		}
		roots = append(roots, RootRuntimeConfig{URI: uri, Name: strings.TrimSpace(root.Name)})
	}

	clientName := strings.TrimSpace(client.ClientName)
	if clientName == "" {
		clientName = "MaiBot"
	}
	clientVersion := strings.TrimSpace(client.ClientVersion)
	if clientVersion == "" {
		clientVersion = "1.0.0"
	}
	taskName := strings.TrimSpace(client.Sampling.TaskName)
	if taskName == "" {
		taskName = "planner"
	}

	return ClientRuntimeConfig{
		ClientName:       clientName,
		ClientVersion:    clientVersion,
		EnableRoots:      client.Roots.Enable && len(roots) > 0,
		Roots:            roots,
		EnableSampling:   client.Sampling.Enable,
		SamplingTaskName: taskName,
		// 宿主桥接层尚未实现 includeContext 的上下文收集,不能向服务端声明虚假能力
		// （对齐 build_mcp_client_runtime_config 的注释）。
		SamplingIncludeContextSupport: false,
		SamplingToolSupport:           client.Sampling.ToolSupport,
		EnableElicitation:             client.Elicitation.Enable,
		ElicitationAllowForm:          client.Elicitation.AllowForm,
		ElicitationAllowURL:           client.Elicitation.AllowURL,
	}
}

// BuildServerRuntimeConfigs mirrors build_mcp_server_runtime_configs:
// 返回启用且配置完整的 MCP 服务器列表(关闭 MCP 或未启用时为空)。
func BuildServerRuntimeConfigs(config MCPConfig) []ServerRuntimeConfig {
	if !config.Enable {
		return nil
	}
	configs := make([]ServerRuntimeConfig, 0, len(config.Servers))
	for _, server := range config.Servers {
		if !server.Enabled {
			continue
		}
		configs = append(configs, BuildServerRuntimeConfig(server))
	}
	return configs
}

// BuildServerRuntimeConfig mirrors build_mcp_server_runtime_config。
func BuildServerRuntimeConfig(server MCPServerItemConfig) ServerRuntimeConfig {
	return ServerRuntimeConfig{
		Name:               strings.TrimSpace(server.Name),
		Transport:          NormalizeTransport(server.Transport),
		Command:            strings.TrimSpace(server.Command),
		Args:               normalizeArgs(server.Args),
		Env:                normalizeStringMap(server.Env),
		URL:                strings.TrimSpace(server.URL),
		Headers:            normalizeStringMap(server.Headers),
		HTTPTimeoutSeconds: normalizeTimeout(server.HTTPTimeoutSeconds, DefaultHTTPTimeoutSeconds),
		ReadTimeoutSeconds: normalizeTimeout(server.ReadTimeoutSeconds, DefaultReadTimeoutSeconds),
		Authorization: AuthorizationRuntimeConfig{
			Mode:        strings.ToLower(strings.TrimSpace(server.Authorization.Mode)),
			BearerToken: strings.TrimSpace(server.Authorization.BearerToken),
		},
		AllowUsers: normalizeList(server.AllowUsers),
		AllowChats: normalizeList(server.AllowChats),
		Permission: normalizeList(server.Permission),
	}
}

// 默认超时（对齐 official_configs 的 Field 默认值）。
const (
	// DefaultHTTPTimeoutSeconds 对齐 http_timeout_seconds 默认 30.0。
	DefaultHTTPTimeoutSeconds = 30.0
	// DefaultReadTimeoutSeconds 对齐 read_timeout_seconds 默认 300.0。
	DefaultReadTimeoutSeconds = 300.0
)

func normalizeTimeout(value float64, fallback float64) float64 {
	if value > 0 {
		return value
	}
	return fallback
}

func normalizeArgs(args []string) []string {
	normalized := make([]string, 0, len(args))
	for _, argument := range args {
		normalized = append(normalized, argument)
	}
	return normalized
}

func normalizeStringMap(values map[string]string) map[string]string {
	normalized := make(map[string]string, len(values))
	for key, value := range values {
		normalized[key] = value
	}
	return normalized
}

func normalizeList(values []string) []string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		normalized = append(normalized, strings.TrimSpace(value))
	}
	return normalized
}

// ---------------------------------------------------------------------------
// 配置签名
// ---------------------------------------------------------------------------

// ConfigSignature mirrors MCPService._build_config_signature: 仅按有效运行时
// 配置生成签名(停用草稿变化不会重启连接)。返回 sha256 十六进制串。
func ConfigSignature(config MCPConfig) string {
	serverConfigs := BuildServerRuntimeConfigs(config)
	clientConfig := BuildClientRuntimeConfig(config)

	payload := map[string]any{
		"servers": serverConfigs,
		"client":  nil,
	}
	if len(serverConfigs) > 0 {
		client := map[string]any{
			"client_name":    clientConfig.ClientName,
			"client_version": clientConfig.ClientVersion,
			"roots":          []any{},
			"sampling":       nil,
		}
		if clientConfig.EnableRoots {
			client["roots"] = clientConfig.Roots
		}
		if clientConfig.EnableSampling {
			client["sampling"] = map[string]any{
				"task_name":    clientConfig.SamplingTaskName,
				"tool_support": clientConfig.SamplingToolSupport,
			}
		}
		payload["client"] = client
	}

	// json.dumps(..., sort_keys=True, separators=(",", ":")) 的 Go 等价物:
	// encoding/json 对 map 键排序且不插入多余空白,因此字节级稳定。
	encoded, err := json.Marshal(payload)
	if err != nil {
		// 结构体全部可序列化;兜底返回错误文本签名避免静默错误。
		encoded = []byte(fmt.Sprintf("signature-error:%v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
