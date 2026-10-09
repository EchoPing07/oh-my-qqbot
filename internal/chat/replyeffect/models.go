// Package replyeffect 实现 MaiBot「回复效果观察」实验系统：
//
//	参考/MaiBot/src/maisaka/reply_effect/{models,tracker,judge,scoring,storage}.py
//
// 语义：reply 工具成功发送后登记一条观察记录（effect_id / session_id / status /
// created_at）；观察窗口内该会话的后续用户消息被记入记录；窗口结束（或后续消息
// 达到上限）后，用 LLM（utils 任务）对「后续消息是否承接/评价了 Bot 回复」做严格
// 语义归因评审，再由确定性评分规则算出回应度/聊天推动度，最终落库
// maisaka_reply_effects 表。
//
// 依赖边界（REWRITE_PLAN §6.1 / P11 约束）：
//   - 本包不 import internal/qq、internal/llm 的具体类型、internal/config；
//     LLM 与配置值全部经构造参数注入（JudgeRunner / TrackerConfig）。
//   - 存储经 Store 接口注入；DatabaseStore 适配 internal/database（同 person/emoji
//     包的既有模式）。
package replyeffect

import (
	"time"
)

// 版本与窗口常数 — 照抄 models.py / tracker.py。
const (
	// SchemaVersion mirrors SCHEMA_VERSION。
	SchemaVersion = 3
	// EvaluationVersion mirrors EVALUATION_VERSION。
	EvaluationVersion = 6
	// SessionFollowupLimit mirrors SESSION_FOLLOWUP_LIMIT。
	SessionFollowupLimit = 15
	// ObservationWindow mirrors OBSERVATION_WINDOW_SECONDS（30 分钟）。
	ObservationWindow = 30 * time.Minute
	// EvaluationConcurrency mirrors EVALUATION_CONCURRENCY。
	EvaluationConcurrency = 2
	// EvaluationTotalTimeout mirrors EVALUATION_TOTAL_TIMEOUT_SECONDS（含一次校验重试）。
	EvaluationTotalTimeout = 60 * time.Second
	// PreActivityWindow mirrors _count_pre_activity 的 2 分钟统计窗口。
	PreActivityWindow = 2 * time.Minute
	// DefaultMaxRecordsPerSession mirrors ReplyEffectStorage._DEFAULT_MAX_RECORDS_PER_CHAT。
	DefaultMaxRecordsPerSession = 256
)

// Status mirrors ReplyEffectStatus。
type Status string

// 状态取值（照抄 ReplyEffectStatus）。
const (
	StatusPending          Status = "pending"
	StatusEvaluating       Status = "evaluating"
	StatusFinalized        Status = "finalized"
	StatusIncomplete       Status = "incomplete"
	StatusEvaluationFailed Status = "evaluation_failed"
)

// CompleteObservationReasons mirrors COMPLETE_OBSERVATION_REASONS：
// 只有这两个结束原因才进入评审；其余（runtime_stop 等）记为 incomplete。
var CompleteObservationReasons = map[string]bool{
	"window_timeout":          true,
	"session_followups_limit": true,
}

// 结束原因取值（对齐 tracker.py 的 ready/finalize 文案）。
const (
	ReasonWindowTimeout   = "window_timeout"
	ReasonFollowupsLimit  = "session_followups_limit"
	ReasonRuntimeStop     = "runtime_stop"
	ReasonRuntimeRecovery = "runtime_recovery"
)

// StanceTargets mirrors STANCE_TARGETS（评价目标轴）。
var StanceTargets = []string{"topic_or_third_party", "bot_content", "bot_persona"}

// Stances mirrors STANCES（情绪/态度轴，顺序对齐 RECEPTION_CATEGORY_ORDER）。
var Stances = []string{
	"appreciation", "playful", "neutral", "confusion",
	"factual_correction", "rejection", "bot_attack",
}

// Contributions mirrors CONTRIBUTIONS（讨论贡献轴）。
var Contributions = []string{"advance", "maintain", "acknowledge", "close", "wrong_push"}

// Strategies mirrors STRATEGIES（Bot 回复策略）。
var Strategies = []string{
	"answer", "opinion", "empathy", "humor",
	"question", "topic_start", "acknowledgement", "other",
}

// BotIdentity mirrors global_config.bot 的 evaluator 可见部分。
type BotIdentity struct {
	// Nickname mirrors global_config.bot.nickname。
	Nickname string
	// AliasNames mirrors global_config.bot.alias_names。
	AliasNames []string
}

// Names 返回去重后的 Bot 名称列表（对齐 build_judge_prompt 的 dict.fromkeys 去重）。
func (b BotIdentity) Names() []string {
	seen := map[string]bool{}
	names := make([]string, 0, len(b.AliasNames)+1)
	for _, raw := range append([]string{b.Nickname}, b.AliasNames...) {
		name := trimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// SessionSnapshot mirrors SessionSnapshot。
type SessionSnapshot struct {
	SessionID      string `json:"session_id"`
	PlatformTypeID string `json:"platform_type_id"`
	Platform       string `json:"platform"`
	ChatType       string `json:"chat_type"`
	GroupID        string `json:"group_id"`
	UserID         string `json:"user_id"`
	SessionName    string `json:"session_name"`
}

// UserSnapshot mirrors UserSnapshot。
type UserSnapshot struct {
	UserID   string `json:"user_id"`
	Nickname string `json:"nickname"`
	Cardname string `json:"cardname"`
}

// DisplayName mirrors `cardname or nickname or 用户`。
func (u UserSnapshot) DisplayName() string {
	if u.Cardname != "" {
		return u.Cardname
	}
	if u.Nickname != "" {
		return u.Nickname
	}
	return "用户"
}

// ReplySnapshot mirrors ReplySnapshot。
type ReplySnapshot struct {
	ToolCallID         string           `json:"tool_call_id"`
	TargetMessageID    string           `json:"target_message_id"`
	SetQuote           bool             `json:"set_quote"`
	ReplyText          string           `json:"reply_text"`
	ReplySegments      []string         `json:"reply_segments"`
	PlannerReasoning   string           `json:"planner_reasoning"`
	SentMessageIDs     []string         `json:"sent_message_ids"`
	ModelName          string           `json:"model_name"`
	RequestFingerprint string           `json:"request_fingerprint"`
	PromptFingerprint  string           `json:"prompt_fingerprint"`
	StrategyPrimary    string           `json:"strategy_primary"`
	StrategySecondary  []string         `json:"strategy_secondary"`
	StrategyConfidence float64          `json:"strategy_confidence"`
	ToolContext        map[string]any   `json:"tool_context"`
	SendResults        []map[string]any `json:"send_results"`
	ReplyMetadata      map[string]any   `json:"reply_metadata"`
}

// ReplyAssociation mirrors ReplyAssociation（一条「后续消息 ↔ Bot 回复」关联边）。
type ReplyAssociation struct {
	EffectID              string   `json:"effect_id"`
	AttributionType       string   `json:"attribution_type"`
	AttributionConfidence float64  `json:"attribution_confidence"`
	StanceTarget          string   `json:"stance_target"`
	Stance                string   `json:"stance"`
	Contribution          string   `json:"contribution"`
	Reason                string   `json:"reason"`
	EvidenceSpans         []string `json:"evidence_spans"`
	EvaluatorConfidence   float64  `json:"evaluator_confidence"`
}

// 归因类型：显式引用（quote/reply_to 命中已发出消息）与 LLM 语义归因。
const (
	AttributionExplicitQuote = "explicit_quote"
	AttributionSemantic      = "semantic"
)

// VisualAttachment mirrors image_utils.extract_visual_attachments_from_sequence 的条目。
type VisualAttachment struct {
	Kind     string `json:"kind"`
	Index    int    `json:"index"`
	Hash     string `json:"hash"`
	Content  string `json:"content"`
	Path     string `json:"path"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	DataURL  string `json:"data_url,omitempty"`
}

// FollowupSnapshot mirrors FollowupMessageSnapshot。
type FollowupSnapshot struct {
	MessageID          string             `json:"message_id"`
	Timestamp          string             `json:"timestamp"`
	UserID             string             `json:"user_id"`
	Nickname           string             `json:"nickname"`
	Cardname           string             `json:"cardname"`
	VisibleText        string             `json:"visible_text"`
	PlainText          string             `json:"plain_text"`
	LatencySeconds     float64            `json:"latency_seconds"`
	IsTargetUser       bool               `json:"is_target_user"`
	ReplyTo            string             `json:"reply_to"`
	QuoteTargetIDs     []string           `json:"quote_target_ids"`
	CandidateEffectIDs []string           `json:"candidate_effect_ids"`
	Associations       []ReplyAssociation `json:"associations"`
	Attachments        []VisualAttachment `json:"attachments"`
}

// DisplayName mirrors `cardname or nickname or 用户`。
func (f FollowupSnapshot) DisplayName() string {
	if f.Cardname != "" {
		return f.Cardname
	}
	if f.Nickname != "" {
		return f.Nickname
	}
	return "用户"
}

// ContextItem mirrors runtime._build_reply_effect_context_snapshot 的单条快照。
type ContextItem struct {
	MessageID      string             `json:"message_id"`
	Source         string             `json:"source"`
	Role           string             `json:"role"`
	Timestamp      string             `json:"timestamp"`
	Text           string             `json:"text"`
	RawText        string             `json:"raw_text,omitempty"`
	QuoteTargetIDs []string           `json:"quote_target_ids"`
	Attachments    []VisualAttachment `json:"attachments"`
	Sender         *ContextSender     `json:"sender,omitempty"`
}

// ContextSender mirrors 快照里的 sender 子对象。
type ContextSender struct {
	UserID      string `json:"user_id"`
	Nickname    string `json:"nickname"`
	Cardname    string `json:"cardname"`
	DisplayName string `json:"display_name"`
}

// Scores mirrors ReplyEffectScores（确定性评分结果）。
type Scores struct {
	ResponseScore                  float64        `json:"response_score"`
	ReceptionCategories            []string       `json:"reception_categories"`
	ReceptionCounts                map[string]int `json:"reception_counts"`
	ConversationScore              float64        `json:"conversation_score"`
	Confidence                     *float64       `json:"confidence"`
	ResponseEvidenceConfidence     float64        `json:"response_evidence_confidence"`
	ReceptionEvidenceConfidence    float64        `json:"reception_evidence_confidence"`
	ConversationEvidenceConfidence float64        `json:"conversation_evidence_confidence"`
}

// FollowupSummary mirrors tracker._build_followup_summary。
type FollowupSummary struct {
	TotalCount       int `json:"total_count"`
	AssociatedCount  int `json:"associated_count"`
	ParticipantCount int `json:"participant_count"`
}

// Record mirrors ReplyEffectRecord（to_json_dict 的 Go 形态，字段名与 Python 一致）。
type Record struct {
	EffectID          string             `json:"effect_id"`
	Status            Status             `json:"status"`
	CreatedAt         string             `json:"created_at"`
	UpdatedAt         string             `json:"updated_at"`
	Session           SessionSnapshot    `json:"session"`
	Reply             ReplySnapshot      `json:"reply"`
	TargetUser        UserSnapshot       `json:"target_user"`
	PreActivityCount  int                `json:"pre_activity_count"`
	PreActivityBucket string             `json:"pre_activity_bucket"`
	ContextSnapshot   []ContextItem      `json:"context_snapshot"`
	FollowupMessages  []FollowupSnapshot `json:"followup_messages"`
	Scores            *Scores            `json:"scores"`
	FinalizedAt       string             `json:"finalized_at"`
	FinalizeReason    string             `json:"finalize_reason"`
	ConfidenceNote    string             `json:"confidence_note"`
	FollowupSummary   FollowupSummary    `json:"followup_summary"`
	EvaluationError   string             `json:"evaluation_error"`
	EvaluationVersion int                `json:"evaluation_version"`
	SchemaVersion     int                `json:"schema_version"`
}

// Clone 深拷贝一条记录（测试与评测路径使用，避免共享切片）。
func (r *Record) Clone() *Record {
	if r == nil {
		return nil
	}
	cloned := *r
	cloned.Reply.ReplySegments = append([]string(nil), r.Reply.ReplySegments...)
	cloned.Reply.SentMessageIDs = append([]string(nil), r.Reply.SentMessageIDs...)
	cloned.Reply.StrategySecondary = append([]string(nil), r.Reply.StrategySecondary...)
	cloned.ContextSnapshot = append([]ContextItem(nil), r.ContextSnapshot...)
	cloned.FollowupMessages = make([]FollowupSnapshot, 0, len(r.FollowupMessages))
	for _, followup := range r.FollowupMessages {
		item := followup
		item.QuoteTargetIDs = append([]string(nil), followup.QuoteTargetIDs...)
		item.CandidateEffectIDs = append([]string(nil), followup.CandidateEffectIDs...)
		item.Attachments = append([]VisualAttachment(nil), followup.Attachments...)
		item.Associations = append([]ReplyAssociation(nil), followup.Associations...)
		cloned.FollowupMessages = append(cloned.FollowupMessages, item)
	}
	if r.Scores != nil {
		scores := *r.Scores
		scores.ReceptionCategories = append([]string(nil), r.Scores.ReceptionCategories...)
		counts := make(map[string]int, len(r.Scores.ReceptionCounts))
		for key, value := range r.Scores.ReceptionCounts {
			counts[key] = value
		}
		scores.ReceptionCounts = counts
		cloned.Scores = &scores
	}
	return &cloned
}

// FormatTimestamp mirrors models.now_iso：本地时区、秒精度 ISO 8601。
func FormatTimestamp(t time.Time) string {
	return t.Format("2006-01-02T15:04:05-07:00")
}

// ParseTimestamp 解析 FormatTimestamp / Python isoformat 产物。
func ParseTimestamp(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	return time.Parse("2006-01-02T15:04:05.999999-07:00", value)
}
