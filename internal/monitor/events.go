// emit 辅助函数 — 对齐 参考MaiBot/src/maisaka/monitor/events.py 的 emit_* 系列
// （planner.progress / session.start / stage.status / message.ingested /
// message.sent / llm.retry / llm.error），事件载荷字段名与 Python 版逐一对齐。
package monitor

import (
	"time"

	contracts "oh-my-qqbot/internal"
)

// MONITOR_DOMAIN / MONITOR_TOPIC 对齐 events.py 常量（WebUI WS 订阅用）。
const (
	MonitorDomain = "maisaka_monitor"
	MonitorTopic  = "main"
)

// newEvent 构造带时间戳的事件（timestamp 字段对齐 Python time.time()）。
func newEvent(kind contracts.EventKind, sessionID string, payload map[string]any) contracts.Event {
	return contracts.Event{
		Kind:      kind,
		SessionID: sessionID,
		Time:      time.Now(),
		Payload:   payload,
	}
}

// put 只在值非零时写入载荷（保持 JSON 载荷与 Python 版的省略语义一致）。
func put(payload map[string]any, key string, value any) {
	payload[key] = value
}

// EmitSessionStart 对齐 emit_session_start。
func EmitSessionStart(b contracts.EventBus, sessionID, sessionName string, isGroupChat bool, groupID, userID, platform string) {
	if b == nil {
		return
	}
	payload := map[string]any{
		"session_id":    sessionID,
		"session_name":  sessionName,
		"is_group_chat": isGroupChat,
		"group_id":      groupID,
		"user_id":       userID,
		"platform":      platform,
		"timestamp":     float64(time.Now().UnixNano()) / 1e9,
	}
	b.Emit(newEvent(contracts.EventSessionStart, sessionID, payload))
}

// StageStatusParams 是 emit_stage_status 的参数集合。
type StageStatusParams struct {
	SessionID      string
	SessionName    string
	Stage          string
	Detail         string
	RoundText      string
	AgentState     string
	StageStartedAt float64 // unix 秒
	UpdatedAt      float64 // unix 秒
	Timestamp      float64 // unix 秒
}

// EmitStageStatus 对齐 emit_stage_status。
func EmitStageStatus(b contracts.EventBus, p StageStatusParams) {
	if b == nil {
		return
	}
	payload := map[string]any{
		"session_id":       p.SessionID,
		"session_name":     p.SessionName,
		"stage":            p.Stage,
		"detail":           p.Detail,
		"round_text":       p.RoundText,
		"agent_state":      p.AgentState,
		"stage_started_at": p.StageStartedAt,
		"updated_at":       p.UpdatedAt,
		"timestamp":        p.Timestamp,
	}
	b.Emit(newEvent(contracts.EventStageStatus, p.SessionID, payload))
}

// EmitStageRemoved 对齐 emit_stage_removed（Python 事件名 stage.removed；
// contracts 枚举未单列，复用 stage.status 事件种类并以 stage_removed 标记，
// WebUI 侧按载荷区分）。
func EmitStageRemoved(b contracts.EventBus, sessionID, sessionName string) {
	if b == nil {
		return
	}
	payload := map[string]any{
		"session_id":   sessionID,
		"session_name": sessionName,
		"stage":        "removed",
		"timestamp":    float64(time.Now().UnixNano()) / 1e9,
	}
	b.Emit(newEvent(contracts.EventStageStatus, sessionID, payload))
}

// LLMRetryParams 是 emit_llm_retry 的参数集合。
type LLMRetryParams struct {
	SessionID     string
	TaskName      string
	RequestType   string
	ModelName     string
	Attempt       int
	MaxAttempts   int
	Reason        string
	RetryInterval float64 // 秒
}

// EmitLLMRetry 对齐 emit_llm_retry。
func EmitLLMRetry(b contracts.EventBus, p LLMRetryParams) {
	if b == nil {
		return
	}
	payload := map[string]any{
		"session_id":     p.SessionID,
		"task_name":      p.TaskName,
		"request_type":   p.RequestType,
		"model_name":     p.ModelName,
		"attempt":        p.Attempt,
		"max_attempts":   p.MaxAttempts,
		"reason":         p.Reason,
		"retry_interval": p.RetryInterval,
		"timestamp":      float64(time.Now().UnixNano()) / 1e9,
	}
	b.Emit(newEvent(contracts.EventLLMRetry, p.SessionID, payload))
}

// LLMErrorParams 是 emit_llm_error 的参数集合。
type LLMErrorParams struct {
	SessionID   string
	TaskName    string
	RequestType string
	ModelName   string
	Message     string
}

// EmitLLMError 对齐 emit_llm_error。
func EmitLLMError(b contracts.EventBus, p LLMErrorParams) {
	if b == nil {
		return
	}
	payload := map[string]any{
		"session_id":   p.SessionID,
		"task_name":    p.TaskName,
		"request_type": p.RequestType,
		"model_name":   p.ModelName,
		"message":      p.Message,
		"timestamp":    float64(time.Now().UnixNano()) / 1e9,
	}
	b.Emit(newEvent(contracts.EventLLMError, p.SessionID, payload))
}

// MessageParams 是 message.ingested / message.sent 共用的参数集合。
type MessageParams struct {
	SessionID   string
	SpeakerName string
	Content     string
	MessageID   string
	Timestamp   float64 // unix 秒
	SourceKind  string  // message.sent 专用
	Platform    string
	UserID      string
	GroupID     string
	ReplyTo     map[string]any
	Media       []map[string]any
}

func messagePayload(p MessageParams, includeSourceKind bool) map[string]any {
	payload := map[string]any{
		"session_id":   p.SessionID,
		"speaker_name": p.SpeakerName,
		"content":      p.Content,
		"message_id":   p.MessageID,
		"platform":     p.Platform,
		"user_id":      p.UserID,
		"group_id":     p.GroupID,
		"reply_to":     p.ReplyTo,
		"media":        p.Media,
		"timestamp":    p.Timestamp,
	}
	if includeSourceKind {
		put(payload, "source_kind", p.SourceKind)
	}
	return payload
}

// EmitMessageIngested 对齐 emit_message_ingested。
func EmitMessageIngested(b contracts.EventBus, p MessageParams) {
	if b == nil {
		return
	}
	b.Emit(newEvent(contracts.EventMessageIngested, p.SessionID, messagePayload(p, false)))
}

// EmitMessageSent 对齐 emit_message_sent。
func EmitMessageSent(b contracts.EventBus, p MessageParams) {
	if b == nil {
		return
	}
	b.Emit(newEvent(contracts.EventMessageSent, p.SessionID, messagePayload(p, true)))
}

// PlannerProgressParams 是 planner.progress 快照的参数集合（对齐
// emit_planner_snapshot(event_type="planner.progress") 的核心面；prompt HTML
// 记录等 Python 特有面在 Go 版裁剪）。
type PlannerProgressParams struct {
	SessionID          string
	CycleID            int
	RunID              string
	SelectedHistoryCnt int
	ToolCount          int
	RequestMessages    []map[string]any
	Content            string
	ToolCalls          []map[string]any
	PromptTokens       int
	CompletionTokens   int
	TotalTokens        int
	DurationMS         float64
	ModelName          string
	AgentState         string
	EndReason          string
	EndDetail          string
	Interrupted        bool
	ActiveToolCallID   string
}

// EmitPlannerProgress 对齐 emit_planner_snapshot 的 planner.progress 形态。
func EmitPlannerProgress(b contracts.EventBus, p PlannerProgressParams) {
	if b == nil {
		return
	}
	request := map[string]any{
		"messages":               p.RequestMessages,
		"selected_history_count": p.SelectedHistoryCnt,
		"tool_count":             p.ToolCount,
	}
	planner := map[string]any{
		"content":           p.Content,
		"model_name":        p.ModelName,
		"tool_calls":        p.ToolCalls,
		"native_tool_calls": []any{},
		"prompt_tokens":     p.PromptTokens,
		"completion_tokens": p.CompletionTokens,
		"total_tokens":      p.TotalTokens,
		"duration_ms":       p.DurationMS,
		"prompt_html_uri":   "",
	}
	payload := map[string]any{
		"session_id":          p.SessionID,
		"cycle_id":            p.CycleID,
		"run_id":              p.RunID,
		"timestamp":           float64(time.Now().UnixNano()) / 1e9,
		"request":             request,
		"planner":             planner,
		"tools":               []any{},
		"agent_state":         p.AgentState,
		"planner_interrupted": p.Interrupted,
		"end_reason":          p.EndReason,
		"end_detail":          p.EndDetail,
		"active_tool_call_id": p.ActiveToolCallID,
	}
	b.Emit(newEvent(contracts.EventPlannerProgress, p.SessionID, payload))
}
