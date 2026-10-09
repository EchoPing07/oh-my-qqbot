package memory

import (
	"context"
	"time"

	contracts "oh-my-qqbot/internal"
	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/message"
)

// 本文件集中定义包内的最小注入接口（不 import internal/config、
// internal/llm 的具体客户端、internal/qq）。装配层用适配器把真实实现接进来。

// ---------------------------------------------------------------------------
// LLM / Embedding / Prompt
// ---------------------------------------------------------------------------

// LLMMessage 是投喂给模型的单条消息（角色 + 文本）。
type LLMMessage struct {
	Role    string
	Content string
}

// LLMRequest 是一次最小 LLM 调用（task/request_type 对齐 Python 的
// LLMServiceClient(task_name=..., request_type=...)）。
type LLMRequest struct {
	Task         string
	RequestType  string
	SessionID    string
	SystemPrompt string
	Messages     []LLMMessage
	// MaxTokens / Temperature 为 0 表示沿用任务配置默认值。
	MaxTokens   int
	Temperature float64
}

// LLMFunc 是包内统一的 LLM 边界：返回补全文本。
type LLMFunc func(ctx context.Context, request LLMRequest) (string, error)

// EmbedFunc 生成文本向量（对齐 embedding_service.embed_text(s)）。
type EmbedFunc func(ctx context.Context, texts []string) ([][]float32, error)

// PromptFunc 渲染 prompts/zh-CN/<name>.prompt（装配层用 internal/prompt 实现）。
type PromptFunc func(name string, context map[string]string) (string, error)

// ---------------------------------------------------------------------------
// 内核与存储边界
// ---------------------------------------------------------------------------

// SummaryIngester 是聊天摘要写入内核的最小面（MemoryKernel 子集）。
type SummaryIngester interface {
	IngestSummary(ctx context.Context, chatID string, summary string, ts time.Time) error
}

// ImageMemoryIngester 是图片记忆写入内核的最小面。
type ImageMemoryIngester interface {
	IngestImageMemory(ctx context.Context, chatID string, description string, hash string, ts time.Time) error
}

// PersonFactWriter 是人物事实写回内核的最小面。
type PersonFactWriter interface {
	UpsertPersonFact(ctx context.Context, personID string, name string, fact contracts.ProfileFact) error
}

// ProfileReader 是人物画像读取的最小面。
type ProfileReader interface {
	GetPersonProfile(ctx context.Context, personID string) (*contracts.PersonProfile, error)
}

// MemorySearcher 是记忆检索的最小面（mode: search/time/hybrid/episode/aggregate）。
type MemorySearcher interface {
	SearchMemory(ctx context.Context, mode string, query string, chatID string, topK int) ([]contracts.MemoryHit, error)
}

// MemorySearchHit 是给工具层/注入层的检索结果视图（对齐 services/memory_service.py
// 的 MemoryHit / to_dict 形态）。
type MemorySearchHit struct {
	Content   string
	Score     float64
	Type      string
	Source    string
	Hash      string
	Metadata  map[string]any
	EpisodeID string
	Title     string
}

// MemorySearchResult mirrors memory_service.MemorySearchResult。
type MemorySearchResult struct {
	Summary  string
	Hits     []MemorySearchHit
	Filtered bool
	Success  bool
	Error    string
}

// MemoryWriteResult mirrors memory_service.MemoryWriteResult。
type MemoryWriteResult struct {
	Success    bool
	StoredIDs  []string
	SkippedIDs []string
	Detail     string
}

// PersonProfileResult mirrors memory_service.PersonProfileResult。
type PersonProfileResult struct {
	Summary  string
	Traits   []string
	Evidence []map[string]any
}

// ---------------------------------------------------------------------------
// 消息侧读取边界（对齐 common/message_repository 的 find_messages/count_messages）
// ---------------------------------------------------------------------------

// MessageQuery mirrors find_messages 的调用面。
type MessageQuery struct {
	SessionID string
	MessageID string
	// BeforeTime 为 nil 表示不过滤时间上界（对齐 before_time=None）。
	BeforeTime *time.Time
	Limit      int
	// LimitMode mirrors limit_mode（"latest" 取最新）。
	LimitMode string
	// FilterBot mirrors filter_bot=True（排除机器人自己的消息）。
	FilterBot bool
}

// MessageQueryFunc 是消息仓储读取边界（默认实现走 internal/database 适配器）。
type MessageQueryFunc func(ctx context.Context, query MessageQuery) ([]*message.SessionMessage, error)

// MessageCountFunc mirrors count_messages。
type MessageCountFunc func(ctx context.Context, sessionID string, endTime *time.Time) (int, error)

// MessageByIDFunc mirrors find_messages(message_id=...) 的单条查询语义
// （返回条数用于人物事实校验：!=1 即失败）。
type MessageByIDFunc func(ctx context.Context, sessionID string, messageID string) ([]*message.SessionMessage, error)

// ---------------------------------------------------------------------------
// 聊天流身份（对齐 ChatManager.get_session_name / BotChatSession）
// ---------------------------------------------------------------------------

// SessionIdentity 是聊天流的只读身份视图。
type SessionIdentity struct {
	SessionID string
	Platform  string
	UserID    string
	GroupID   string
	// IsGroup mirrors session.is_group_session。
	IsGroup bool
	// DisplayName mirrors chat_manager.get_session_name()。
	DisplayName string
}

// SessionLookupFunc 按 session_id 查已注册聊天流（nil 表示未注册）。
type SessionLookupFunc func(sessionID string) (*SessionIdentity, bool)

// PersonIDFunc mirrors get_person_id(platform, user_id)。
type PersonIDFunc func(platform string, userID string) string

// ResolvePersonIDByNameFunc mirrors resolve_person_id_for_memory(
// person_name=..., platform=..., user_id=...)。
type ResolvePersonIDByNameFunc func(platform string, userID string, personName string) string

// IsBotSelfFunc mirrors utils.is_bot_self。
type IsBotSelfFunc func(platform string, userID string) bool

// ---------------------------------------------------------------------------
// 内核门面的组合视图
// ---------------------------------------------------------------------------

// KernelSurface 汇总 MemoryService 门面需要的全部内核能力。
// 任意字段可为 nil：对应能力以「不可用」语义降级。
type KernelSurface struct {
	Summaries  SummaryIngester
	Images     ImageMemoryIngester
	Facts      PersonFactWriter
	Profiles   ProfileReader
	Searcher   MemorySearcher
	TextWriter interface {
		IngestText(ctx context.Context, chatID string, text string, ts time.Time) error
	}
}

// ToMemorySearchHits 把内核契约命中转换为门面视图。
func ToMemorySearchHits(hits []contracts.MemoryHit) []MemorySearchHit {
	out := make([]MemorySearchHit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, MemorySearchHit{
			Content:   hit.Text,
			Score:     hit.Score,
			Type:      hit.Kind,
			Source:    hit.ChatID,
			Hash:      hit.ID,
			EpisodeID: "",
			Title:     "",
			Metadata: map[string]any{
				"chat_id":    hit.ChatID,
				"similarity": hit.Similarity,
				"time":       hit.Time,
			},
		})
	}
	return out
}

// ReferencePayload 是参考注入文本的轻量载体（装配层转换为
// chatcontext.ReferenceMessage / Planner 注入消息）。
type ReferencePayload struct {
	Content string
	// Kind mirrors chatcontext.ReferenceMessageType（memory/jargon/custom...）。
	Kind string
	// DisplayPrefix mirrors ReferenceMessage.display_prefix。
	DisplayPrefix string
}

// referenceMessageType 让包内文件共享 ChatContext 参考消息类型的只读引用。
type referenceMessageType = chatcontext.ReferenceMessageType
