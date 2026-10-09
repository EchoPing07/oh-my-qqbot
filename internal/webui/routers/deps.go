// Package routers 实现 WebUI REST 路由（对齐 参考/MaiBot/src/webui/routers/ 的
// 路径与响应字段，非核心字段按 REWRITE_PLAN §6.16 裁剪）。
//
// 依赖边界：
//   - 本包不 import internal/webui（避免环）；对宿主能力的依赖全部以本包内
//     定义的小接口表达（AuthAPI/Broadcaster/MemoryAPI/StatusProvider/
//     ChatInjector/SnapshotSource），由 internal/webui 装配层注入；
//   - 配置走 internal/config.Manager，数据库走 internal/database.DB；
//   - 不 import internal/mcp（并行开发中，MCP 状态经 StatusProvider 接口）；
//   - 不 import internal/qq。
package routers

import (
	"context"
	"net/http"
	"time"

	contracts "oh-my-qqbot/internal"
	config "oh-my-qqbot/internal/config"
	database "oh-my-qqbot/internal/database"
)

// WebUIChatPlatform 对齐 WEBUI_CHAT_PLATFORM：WebUI 内置聊天虚拟平台名。
const WebUIChatPlatform = "webui"

// AuthAPI 是 WebUI 鉴权边界（由 internal/webui.TokenManager 实现，方法集对齐
// 参考/MaiBot/src/webui/core/security.py 的 TokenManager）。
type AuthAPI interface {
	// Token 返回当前访问令牌。
	Token() string
	// VerifyToken 常数时间校验令牌。
	VerifyToken(token string) bool
	// UpdateToken 更新为用户自定义令牌（校验失败返回 (false, 原因)）。
	UpdateToken(newToken string) (bool, string)
	// RegenerateToken 重新生成令牌并返回新值。
	RegenerateToken() string
	// TokenSource 返回令牌来源：temporary | configured。
	TokenSource() string
	// IsFirstSetup 返回首次配置向导是否未完成。
	IsFirstSetup() bool
	// MarkSetupCompleted 标记首次配置完成。
	MarkSetupCompleted() bool
	// ResetSetup 重置首次配置状态。
	ResetSetup() bool
	// GenerateWSToken 为 WS 握手签发一次性临时令牌（60s 有效期）。
	GenerateWSToken(sessionToken string) string
	// VerifyWSToken 校验并消费（一次性）WS 握手临时令牌。
	VerifyWSToken(token string) bool
}

// Broadcaster 是 WS 统一通道广播边界（由 internal/webui.WSHub 实现）。
type Broadcaster interface {
	// Broadcast 向订阅了 domain:topic 的全部连接广播 event 事件。
	Broadcast(domain, topic, event string, data any)
}

// MemoryAPI 是记忆内核检索/画像边界（对齐 internal.MemoryKernel 的检索/画像面；
// 由 internal/memorix.Kernel 等实现）。
type MemoryAPI interface {
	SearchMemory(ctx context.Context, mode string, query string, chatID string, topK int) ([]contracts.MemoryHit, error)
	GetPersonProfile(ctx context.Context, personID string) (*contracts.PersonProfile, error)
}

// StatusProvider 是 MCP 服务状态快照边界（internal/mcp 并行开发中，
// 这里只依赖最小接口，装配层适配其快照函数）。
type StatusProvider interface {
	StatusSnapshot() map[string]any
}

// ChatInjector 把 WebUI 虚拟平台（WebUIChatPlatform）发言注入聊天主链路。
// 由装配层实现：构造 SessionMessage → ChatManager.RegisterMessage →
// StoreMessageToDB → 心流 runtime.RegisterMessage。
type ChatInjector interface {
	InjectWebUIMessage(msg WebUIChatMessage) error
}

// WebUIChatMessage 是 WebUI 聊天页发送的一条用户发言。
type WebUIChatMessage struct {
	SessionID string `json:"session_id,omitempty"` // 指定已有会话时直接投递
	UserID    string `json:"user_id,omitempty"`
	UserName  string `json:"user_name"`
	GroupID   string `json:"group_id,omitempty"`
	GroupName string `json:"group_name,omitempty"`
	Content   string `json:"content"`
}

// SnapshotSource 是 Planner 快照历史来源（由 internal/webui.MonitorBridge 实现，
// 缓存事件 bus 最近 N 条 planner.progress 事件）。
type SnapshotSource interface {
	RecentPlannerSnapshots(sessionID string, limit int) []ReasoningSnapshot
}

// MemoryHitView 是返回给前端的记忆命中视图（contracts.MemoryHit 的 JSON 形态）。
type MemoryHitView = contracts.MemoryHit

// ReasoningSnapshot 是一条 Planner 快照缓存记录（对齐
// maisaka/monitor/events.py 的 planner.progress 载荷外壳）。
type ReasoningSnapshot struct {
	Seq       int64          `json:"seq"`
	Kind      string         `json:"kind"`
	SessionID string         `json:"session_id"`
	Time      time.Time      `json:"time"`
	Payload   map[string]any `json:"payload"`
}

// Deps 汇总全部路由依赖；零值成员对应的能力路由返回 503/501。
type Deps struct {
	// Auth 鉴权 API（必填，见 AuthAPI）。
	Auth AuthAPI
	// Hub WS 广播（nil 时日志/监控只进缓存不推送）。
	Hub Broadcaster
	// Logs 日志环形缓冲 + slog handler + 级别控制（nil 时日志路由 503）。
	Logs *LogService
	// Config 配置管理器（nil 时配置/模型路由 503）。
	Config *config.Manager
	// DB 数据库（nil 时会话/统计/资源路由 503）。
	DB *database.DB
	// Memory 记忆内核（nil 时记忆路由 503）。
	Memory MemoryAPI
	// MCP MCP 状态提供者（nil 时 /mcp/status 返回 disabled 快照）。
	MCP StatusProvider
	// Chat WebUI 虚拟平台注入器（nil 时 /chat/send 501）。
	Chat ChatInjector
	// Snapshots Planner 快照缓存（nil 时快照路由返回空列表）。
	Snapshots SnapshotSource

	// Version 主程序版本号。
	Version string
	// StartedAt 进程启动时间（/system/status 的 uptime 起点）。
	StartedAt time.Time
	// DataDir 数据目录（webui.json / 用户表情包根目录）。
	DataDir string
	// NewsURL 首页资讯上游 URL（空值取环境变量或默认源）。
	NewsURL string
	// HTTPClient 资讯代理等出站请求客户端（nil 时用默认 8s 超时客户端）。
	HTTPClient *http.Client
}
