package learners

import (
	"context"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/message"
)

// 本文件集中定义包内最小注入接口（不 import internal/config、
// internal/llm 具体类型、internal/qq）。装配层用适配器接真实实现。

// ---------------------------------------------------------------------------
// LLM / Embedding / Prompt
// ---------------------------------------------------------------------------

// LLMMessage 是投喂给模型的单条消息。
type LLMMessage struct {
	Role    string
	Content string
}

// LLMRequest 是一次最小 LLM 调用。
type LLMRequest struct {
	Task         string
	RequestType  string
	SessionID    string
	SystemPrompt string
	Messages     []LLMMessage
	MaxTokens    int
	Temperature  float64
}

// LLMFunc 是包内统一的 LLM 边界：返回补全文本。
type LLMFunc func(ctx context.Context, request LLMRequest) (string, error)

// EmbedFunc 生成文本向量。
type EmbedFunc func(ctx context.Context, texts []string) ([][]float32, error)

// PromptFunc 渲染 prompts/zh-CN/<name>.prompt。
type PromptFunc func(name string, contextMap map[string]string) (string, error)

// ---------------------------------------------------------------------------
// 存储边界
// ---------------------------------------------------------------------------

// ExpressionRecord 是 expressions 表的一行视图（对齐 database.Expression）。
type ExpressionRecord struct {
	ID             int64
	Situation      string
	Style          string
	ContentList    string
	Count          int
	LastActiveTime int64 // Unix 秒
	CreateTime     int64
	SessionID      *string
	Checked        bool
	ModifiedBy     *string
}

// ExpressionRow 是候选加载结果的视图（对齐 _load_all_expression_candidates）。
type ExpressionRow struct {
	ID         int64
	Situation  string
	Style      string
	Count      int
	SessionID  *string
	Checked    bool
	ModifiedBy string
}

// ExpressionStore 是表达方式的持久化边界（对齐 learners/expression_learner.py
// 的 DB 操作 + maisaka_expression_selector 的候选加载）。
type ExpressionStore interface {
	// LoadCandidates mirrors _load_all_expression_candidates（作用域内全部候选）。
	LoadCandidates(sessionID string, checkedOnly bool) ([]ExpressionRow, error)
	// FindExact mirrors _find_similar_expression（完全一致的表达方式）。
	FindExact(sessionID string, situation string, style string) (*ExpressionRow, error)
	// Upsert 写入或合并（命中 (situation, style, session) 时 count+1）。
	Upsert(record ExpressionRecord) (ExpressionRecord, error)
	// UpdateChecked 更新人工审核标记（WebUI 审核路径）。
	UpdateChecked(id int64, checked bool, modifiedBy string) error
}

// JargonRecord 是 jargons 表的一行视图（对齐 database.Jargon）。
type JargonRecord struct {
	ID                 int64
	Content            string
	EvidenceMessages   string
	Meaning            string
	SessionIDDict      string
	Count              int
	IsJargon           bool
	IsComplete         bool
	IsGlobal           bool
	LastInferenceCount int
	CreatedBy          string
	CreatedTimestamp   int64
	UpdatedTimestamp   int64
}

// JargonStore 是黑话的持久化边界（对齐 jargon_miner.py 的 DB 操作）。
type JargonStore interface {
	// ListJargons 返回全部黑话（按 count desc, id desc）。
	ListJargons() ([]JargonRecord, error)
	// GetByContent 按内容精确查询（返回 nil 表示不存在）。
	GetByContent(content string) (*JargonRecord, error)
	// Insert 插入新黑话。
	Insert(record JargonRecord) (JargonRecord, error)
	// Update 覆盖写入（count/meaning/is_jargon/…）。
	Update(record JargonRecord) error
}

// ---------------------------------------------------------------------------
// 学习素材
// ---------------------------------------------------------------------------

// LearningMessage mirrors SessionMessage 的学习视图（避免本包 import
// internal/chat/manager）。
type LearningMessage struct {
	// MessageID / SessionID / Platform 对齐 SessionMessage 对应字段。
	MessageID string
	SessionID string
	Platform  string
	// UserID / UserName 是发言者信息。
	UserID   string
	UserName string
	// Content mirrors processed_plain_text。
	Content string
	// TimestampSeconds 是消息时间（Unix 秒）。
	TimestampSeconds float64
	// IsEmoji mirrors is_emoji（黑话学习素材过滤）。
	IsEmoji bool
	// IsNotify mirrors is_notify。
	IsNotify bool
	// IsSelf mirrors is_bot_self(platform, user_id)。
	IsSelf bool
}

// LearningMessageFromSession 把 SessionMessage 投影为学习素材。
func LearningMessageFromSession(msg *message.SessionMessage, isBotSelf IsBotSelfFunc) LearningMessage {
	if msg == nil {
		return LearningMessage{}
	}
	text := ""
	if msg.ProcessedPlainText != nil {
		text = *msg.ProcessedPlainText
	}
	userInfo := msg.MessageInfo.UserInfo
	self := false
	if isBotSelf != nil {
		self = isBotSelf(msg.Platform, userInfo.UserID)
	}
	return LearningMessage{
		MessageID:        msg.MessageID,
		SessionID:        msg.SessionID,
		Platform:         msg.Platform,
		UserID:           userInfo.UserID,
		UserName:         userInfo.DisplayName(),
		Content:          text,
		TimestampSeconds: float64(msg.Timestamp.UnixNano()) / 1e9,
		IsEmoji:          msg.IsEmoji,
		IsNotify:         msg.IsNotify,
		IsSelf:           self,
	}
}

// LearningSourceFromContext mirrors `_extract_session_messages_from_context`：
// 只保留 user / guided_reply / outbound_send 三类真实聊天消息（去重）。
func LearningSourceFromContext(
	contextMessages []chatcontext.LLMContextMessage,
	isBotSelf IsBotSelfFunc,
) []LearningMessage {
	out := make([]LearningMessage, 0, len(contextMessages))
	seenMessageIDs := map[string]bool{}
	for _, entry := range contextMessages {
		sessionBacked, ok := entry.(*chatcontext.SessionBackedMessage)
		if !ok {
			continue
		}
		switch sessionBacked.SourceKind {
		case chatcontext.SourceUser, chatcontext.SourceGuidedReply, chatcontext.SourceOutboundSend:
		default:
			continue
		}
		original := sessionBacked.OriginalMessage()
		if original == nil {
			continue
		}
		if messageID := original.MessageID; messageID != "" {
			if seenMessageIDs[messageID] {
				continue
			}
			seenMessageIDs[messageID] = true
		}
		out = append(out, LearningMessageFromSession(original, isBotSelf))
	}
	return out
}

// IsBotSelfFunc mirrors utils.is_bot_self。
type IsBotSelfFunc func(platform string, userID string) bool

// SessionExistsFunc mirrors chat_manager.get_existing_session_by_session_id != nil。
type SessionExistsFunc func(sessionID string) bool

// SessionDisplayNameFunc mirrors chat_manager.get_session_name。
type SessionDisplayNameFunc func(sessionID string) string

// ---------------------------------------------------------------------------
// 学习结果
// ---------------------------------------------------------------------------

// ExtractedExpression mirrors a (situation, style, source_id) candidate.
type ExtractedExpression struct {
	Situation string
	Style     string
	SourceID  string
}

// FilteredExpression mirrors a validated (situation, style) pair.
type FilteredExpression struct {
	Situation string
	Style     string
}

// JargonEntry mirrors JargonEntry（content + raw_content 片段 + 证据消息引用）。
type JargonEntry struct {
	Content          string
	RawContent       []string
	EvidenceMessages []JargonEvidenceRef
}

// JargonEvidenceRef mirrors JargonEvidenceMessageRef。
type JargonEvidenceRef struct {
	Platform  string
	MessageID string
}

// chatcontextAlias 是 chatcontext.LLMContextMessage 的包内别名（学习素材投影）。
type chatcontextAlias = chatcontext.LLMContextMessage

// chatcontextSessionBacked / chatcontextReference 是 chatcontext 类型的包内别名。
type chatcontextSessionBacked = chatcontext.SessionBackedMessage
type chatcontextReference = chatcontext.ReferenceMessage

// ReferencePayload 是参考注入文本的轻量载体（装配层转换为
// chatcontext.ReferenceMessage 或 Planner 注入文本）。
type ReferencePayload struct {
	Content string
	// Kind mirrors chatcontext.ReferenceMessageType（jargon/custom/...）。
	Kind string
	// DisplayPrefix mirrors ReferenceMessage.display_prefix。
	DisplayPrefix string
}
