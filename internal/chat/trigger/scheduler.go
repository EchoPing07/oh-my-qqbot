package trigger

import (
	"fmt"
	"strconv"
	"sync"

	"oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/message"
)

// This file mirrors 参考/MaiBot/src/maisaka/turn_trigger/scheduler.py and the
// runtime surface it drives (`_get_effective_reply_frequency`,
// `_is_reply_frequency_silent`, `_has_forced_turn_trigger`,
// `_get_message_trigger_threshold`, `_enqueue_message_turn`,
// `_defer_message_turn_check`, `_enter_running_state`, `_enter_stop_state`).

// Agent state values mirroring
// MaisakaHeartFlowChatting._STATE_RUNNING / _STATE_WAIT / _STATE_STOP.
const (
	StateRunning = "running"
	StateWait    = "wait"
	StateStop    = "stop"
)

// SchedulerRuntime is the runtime surface MessageTurnScheduler drives. The Go
// port keeps it as an interface so the runtime package (P4a) implements it while
// this package stays free of import cycles.
type SchedulerRuntime interface {
	// SessionID mirrors `runtime.session_id`.
	SessionID() string
	// IsGroupSession mirrors `runtime.chat_stream.is_group_session`.
	IsGroupSession() bool
	// LogPrefix mirrors `runtime.log_prefix`.
	LogPrefix() string

	// AgentState mirrors `runtime._agent_state`.
	AgentState() string
	// MessageTurnScheduled mirrors `runtime._message_turn_scheduled`.
	MessageTurnScheduled() bool
	// EnterRunningState mirrors `_enter_running_state`.
	EnterRunningState()
	// EnterStopState mirrors `_enter_stop_state`.
	EnterStopState()

	// PendingMessageCount mirrors `_get_pending_message_count`.
	PendingMessageCount() int
	// PendingMessages mirrors `runtime.message_cache[runtime._last_processed_index:]`.
	PendingMessages() []*message.SessionMessage
	// EffectiveReplyFrequency mirrors `_get_effective_reply_frequency`.
	EffectiveReplyFrequency() float64
	// IsReplyFrequencySilent mirrors `_is_reply_frequency_silent`.
	IsReplyFrequencySilent() bool
	// MessageTriggerThreshold mirrors `_get_message_trigger_threshold`.
	MessageTriggerThreshold() int
	// HasForcedTurnTrigger mirrors `_has_forced_turn_trigger`.
	HasForcedTurnTrigger() bool

	// EnqueueMessageTurn mirrors `_enqueue_message_turn`.
	EnqueueMessageTurn()
	// DeferMessageTurnCheck mirrors `_defer_message_turn_check`.
	DeferMessageTurnCheck(delaySeconds float64)

	// IdleBackoff mirrors `runtime._idle_backoff`.
	IdleBackoff() *IdleBackoffController

	// ChatHistory mirrors `runtime._chat_history` (used by the dynamic gate).
	ChatHistory() []context.LLMContextMessage
	// RecentAverageExternalMessageInterval mirrors
	// `_get_recent_average_external_message_interval`.
	RecentAverageExternalMessageInterval() (float64, bool)
	// LastExternalMessageReceivedAt mirrors
	// `_last_external_message_received_at` (ok=false when still nil).
	LastExternalMessageReceivedAt() (float64, bool)
	// LastMessageReceivedAt mirrors `_last_message_received_at`.
	LastMessageReceivedAt() float64
}

// FocusGate decides whether the session is currently allowed to run the Maisaka
// decision loop (mirrors `focus_mode_manager.can_decide`). nil means "always".
type FocusGate func(sessionID string, isGroupChat bool) bool

// MessageTurnScheduler mirrors scheduler.MessageTurnScheduler: it decides when
// external messages enter the Maisaka internal loop.
//
// T1 修复：ScheduleMessageTurn（渠道读循环 goroutine 触发）与 RecordReply
// （引擎 goroutine 经 tools/reply → RecordPlannerReply 触发）并发进入，
// Python 靠 asyncio 单线程天然串行，Go 必须显式加锁串行化
// 「门控评估 → 投递/延迟」的完整决策序列。
type MessageTurnScheduler struct {
	mu                     sync.Mutex
	runtime                SchedulerRuntime
	dynamicReplyGate       *DynamicReplyTurnGate
	frequencyThresholdGate *FrequencyThresholdTurnGate
	focusGate              FocusGate
	dynamicEnabled         bool
	log                    func(format string, args ...any)
}

// SchedulerOptions carries the injected configuration / collaborators of the
// scheduler (the config package is not imported here by design).
type SchedulerOptions struct {
	// DynamicReplyTriggerEnabled mirrors
	// `is_dynamic_reply_trigger_enabled()` (reply_trigger_mode == "dynamic").
	DynamicReplyTriggerEnabled bool
	// FocusGate mirrors `focus_mode_manager.can_decide` (nil → always allowed).
	FocusGate FocusGate
	// Now mirrors `time.time` for the gate bookkeeping.
	Now func() float64
	// IsBotSelf mirrors `is_bot_self`.
	IsBotSelf func(platform string, userID string) bool
	// Log mirrors the logger used by scheduler.py.
	Log func(format string, args ...any)
}

// NewMessageTurnScheduler mirrors MessageTurnScheduler.__init__.
func NewMessageTurnScheduler(runtime SchedulerRuntime, options SchedulerOptions) *MessageTurnScheduler {
	dynamicRuntime := &schedulerDynamicRuntime{runtime: runtime}
	frequencyRuntime := &schedulerFrequencyRuntime{runtime: runtime}
	return &MessageTurnScheduler{
		runtime:                runtime,
		dynamicReplyGate:       NewDynamicReplyTurnGate(dynamicRuntime, options.Now, options.IsBotSelf),
		frequencyThresholdGate: NewFrequencyThresholdTurnGate(frequencyRuntime, options.Now),
		focusGate:              options.FocusGate,
		dynamicEnabled:         options.DynamicReplyTriggerEnabled,
		log:                    options.Log,
	}
}

// RecordReply mirrors MessageTurnScheduler.record_reply: called by the runtime
// after a successful reply so the dynamic gate can count actual replies.
func (s *MessageTurnScheduler) RecordReply() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dynamicReplyGate.RecordReply()
}

// DynamicGate exposes the inner gate (tests / diagnostics).
func (s *MessageTurnScheduler) DynamicGate() *DynamicReplyGate { return s.dynamicReplyGate.gate }

// ScheduleMessageTurn mirrors MessageTurnScheduler.schedule_message_turn.
func (s *MessageTurnScheduler) ScheduleMessageTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()

	runtime := s.runtime
	if s.focusGate != nil && !s.focusGate(runtime.SessionID(), runtime.IsGroupSession()) {
		s.logf("%s 当前不在 focus 状态，跳过 Maisaka 决策调度", runtime.LogPrefix())
		return
	}

	if runtime.AgentState() == StateWait {
		if !runtime.IsReplyFrequencySilent() {
			if runtime.IsGroupSession() {
				return
			}
			s.logf("%s 私聊 wait 期间收到新消息，结束等待并进入 Planner", runtime.LogPrefix())
			runtime.EnterRunningState()
		} else {
			runtime.EnterStopState()
		}
	}

	if runtime.MessageTurnScheduled() {
		return
	}

	pendingCount := runtime.PendingMessageCount()
	if pendingCount <= 0 {
		return
	}

	effectiveFrequency := runtime.EffectiveReplyFrequency()
	formattedFrequency := formatFrequency(effectiveFrequency)
	if runtime.IsReplyFrequencySilent() {
		s.logf("%s 回复频率调度: 频率=%s pending=%d 判定=静默消费",
			runtime.LogPrefix(), formattedFrequency, pendingCount)
		runtime.EnqueueMessageTurn()
		return
	}

	if runtime.HasForcedTurnTrigger() {
		// @ 强制触发的回复同样占用动态门控的回复额度
		s.dynamicReplyGate.RecordForcedTurn(runtime.PendingMessages())
		s.logf("%s 回复频率调度: 频率=%s pending=%d 判定=强制触发",
			runtime.LogPrefix(), formattedFrequency, pendingCount)
		runtime.EnqueueMessageTurn()
		return
	}

	if backoff := runtime.IdleBackoff(); backoff != nil {
		if backoff.ShouldDelay(pendingCount) {
			return
		}
	}

	triggerThreshold := runtime.MessageTriggerThreshold()
	scheduleDetail := fmt.Sprintf("[频率: %s][%d/%d 消息]", formattedFrequency, pendingCount, triggerThreshold)
	if s.dynamicEnabled {
		dynamicResult := s.dynamicReplyGate.Evaluate(runtime.PendingMessages(), effectiveFrequency)
		s.logf("%s 回复频率调度: [频率: %s][%d 消息] %s",
			runtime.LogPrefix(), formattedFrequency, pendingCount, dynamicResult.Detail)
		if dynamicResult.ShouldTrigger() {
			runtime.EnqueueMessageTurn()
		}
		return
	}

	s.logf("%s 回复频率调度: %s", runtime.LogPrefix(), scheduleDetail)
	frequencyResult := s.frequencyThresholdGate.Evaluate(pendingCount, triggerThreshold)
	s.logf("%s 回复频率调度: %s", runtime.LogPrefix(), frequencyResult.Detail)
	if frequencyResult.ShouldTrigger() {
		runtime.EnqueueMessageTurn()
		return
	}
	if frequencyResult.Decision == TurnGateDelay && frequencyResult.DelaySeconds != nil {
		runtime.DeferMessageTurnCheck(*frequencyResult.DelaySeconds)
	}
}

func (s *MessageTurnScheduler) logf(format string, args ...any) {
	if s.log != nil {
		s.log(format, args...)
	}
}

// ---------------------------------------------------------------------------
// runtime adapters
// ---------------------------------------------------------------------------

// schedulerDynamicRuntime adapts SchedulerRuntime to DynamicGateRuntime.
type schedulerDynamicRuntime struct {
	runtime SchedulerRuntime
}

func (a *schedulerDynamicRuntime) IsGroupSession() bool { return a.runtime.IsGroupSession() }

func (a *schedulerDynamicRuntime) ChatHistory() []context.LLMContextMessage {
	return a.runtime.ChatHistory()
}

// schedulerFrequencyRuntime adapts SchedulerRuntime to FrequencyGateRuntime.
type schedulerFrequencyRuntime struct {
	runtime SchedulerRuntime
}

func (a *schedulerFrequencyRuntime) RecentAverageExternalMessageInterval() (float64, bool) {
	return a.runtime.RecentAverageExternalMessageInterval()
}

func (a *schedulerFrequencyRuntime) LastExternalMessageReceivedAt() (float64, bool) {
	return a.runtime.LastExternalMessageReceivedAt()
}

func (a *schedulerFrequencyRuntime) LastMessageReceivedAt() float64 {
	return a.runtime.LastMessageReceivedAt()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// formatFrequency mirrors `f"{effective_frequency:.3f}"`.
func formatFrequency(frequency float64) string {
	return fmt.Sprintf("%.3f", maxFloat(0.0, frequency))
}

func itoa(value int) string { return strconv.Itoa(value) }
