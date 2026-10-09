package heartflow

import (
	"sync"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	chatruntime "oh-my-qqbot/internal/chat/runtime"
	"oh-my-qqbot/internal/message"
)

// This file mirrors the register_message message-ingestion path of
// 参考/MaiBot/src/maisaka/runtime.py (MaisakaHeartFlowChatting.register_message),
// which P4a's runtime.RegisterMessage implements. This file documents the
// alignment and provides the assistant-facing wrapper used by the message
// receiver (P3 chat pipeline) plus the monitoring mirror.

// RegisterMessageOptions carries the per-message ingestion hooks of
// register_message（message_cache 200 上限、连发过滤 5s、@ 强制触发判定）。
type RegisterMessageOptions struct {
	// MaxRetainedMessageCacheSize mirrors MAX_RETAINED_MESSAGE_CACHE_SIZE.
	MaxRetainedMessageCacheSize int
	// BurstIntervalSeconds mirrors EXTERNAL_MESSAGE_BURST_INTERVAL_SECONDS
	// （连发过滤：同一阵内的短间隔不代表真实发言节奏）。
	BurstIntervalSeconds float64
	// OnMessageIngested is the monitor hook (emit_message_ingested 的等价面).
	OnMessageIngested func(sessionID string, message *message.SessionMessage)
}

// DefaultRegisterMessageOptions mirrors the Python constants.
func DefaultRegisterMessageOptions() RegisterMessageOptions {
	return RegisterMessageOptions{
		MaxRetainedMessageCacheSize: chatruntime.MaxRetainedMessageCacheSize,         // 200
		BurstIntervalSeconds:        chatruntime.ExternalMessageBurstIntervalSeconds, // 5s
	}
}

// IngestionStats mirrors the observable side effects of register_message for
// monitoring and tests.
type IngestionStats struct {
	MessageID        string  `json:"message_id"`
	IsAt             bool    `json:"is_at"`
	IsMentioned      bool    `json:"is_mentioned"`
	ForcedTurnArmed  bool    `json:"forced_turn_armed"`
	BurstSkipped     bool    `json:"burst_skipped"`
	MessageCacheSize int     `json:"message_cache_size"`
	ReceivedAt       float64 `json:"received_at"`
}

// MessageIngestor wraps one runtime with the ingestion bookkeeping that the
// Python runtime does inline (message cache, burst filtering, forced-turn).
// It composes with P4a's runtime.RegisterMessage which performs the same
// operations internally — this wrapper adds the monitor emission.
type MessageIngestor struct {
	runtime *chatruntime.Runtime
	options RegisterMessageOptions
	mu      sync.Mutex
}

// NewMessageIngestor builds the ingestion wrapper.
func NewMessageIngestor(runtime *chatruntime.Runtime, options RegisterMessageOptions) *MessageIngestor {
	if options.MaxRetainedMessageCacheSize <= 0 {
		options.MaxRetainedMessageCacheSize = 200
	}
	if options.BurstIntervalSeconds <= 0 {
		options.BurstIntervalSeconds = 5.0
	}
	return &MessageIngestor{runtime: runtime, options: options}
}

// RegisterMessage mirrors register_message's observable behaviour:
//
//  1. 消息入缓存（message_cache 200 上限，runtime 内部维护）；
//  2. 记录外部消息间隔（连发过滤 5s、空窗补偿统计）；
//  3. @ 强制触发判定（is_at / mentioned → forced turn）。
//
// The runtime performs the state mutation; this method returns the observation
// for monitoring and guarantees the monitor hook runs after ingestion.
func (i *MessageIngestor) RegisterMessage(sessionMessage *message.SessionMessage) IngestionStats {
	if sessionMessage == nil {
		return IngestionStats{}
	}
	receivedAt := float64(time.Now().UnixNano()) / 1e9

	// P4a runtime 完成缓存/间隔统计/强制触发/调度。
	i.runtime.RegisterMessage(sessionMessage)

	stats := IngestionStats{
		MessageID:        sessionMessage.MessageID,
		IsAt:             sessionMessage.IsAt,
		IsMentioned:      sessionMessage.IsMentioned,
		ForcedTurnArmed:  sessionMessage.IsAt || sessionMessage.IsMentioned,
		MessageCacheSize: i.runtime.PendingMessageCount() + len(i.runtime.PendingMessages()),
		ReceivedAt:       receivedAt,
	}
	if hook := i.options.OnMessageIngested; hook != nil {
		hook(i.runtime.SessionID(), sessionMessage)
	}
	return stats
}

// Runtime 暴露内部 runtime。
func (i *MessageIngestor) Runtime() *chatruntime.Runtime { return i.runtime }

// AppendIngestedToHistory 是监控/测试辅助：把一条 SessionMessage 折叠为
// planner 可见历史条目（BuildSessionBackedTextMessage 的转发面）。
func AppendIngestedToHistory(runtime *chatruntime.Runtime, sessionMessage *message.SessionMessage) {
	if sessionMessage == nil {
		return
	}
	prefix := chatcontext.BuildPlannerUserPrefixFromSessionMessage(sessionMessage, chatcontext.PlannerPrefixOptions{})
	visibleText := chatcontext.BuildSessionMessageVisibleText(sessionMessage, sessionMessage.RawMessage, false)
	runtime.AppendChatHistory(chatcontext.NewSessionBackedMessage(
		sessionMessage.RawMessage,
		prefix+visibleText,
		sessionMessage.Timestamp,
		sessionMessage.MessageID,
		sessionMessage,
		chatcontext.SourceUser,
	))
}
