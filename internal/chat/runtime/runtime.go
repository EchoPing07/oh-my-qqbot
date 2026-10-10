// Package runtime implements the Maisaka heart-flow runtime: the per-session
// state machine (RUNNING / WAIT / STOP), the internal round loop of the
// reasoning engine, the planner interrupt controller and the runtime surfaces
// consumed by internal/chat/trigger.
//
// Aligned with 参考/MaiBot/src/maisaka/runtime.py and
// 参考/MaiBot/src/maisaka/reasoning_engine.py (behaviour is authoritative
// there). The planner prompt assembly (chat_loop_service.py) is deliberately
// abstracted behind the PlannerLLM interface implemented by P4b.
package runtime

import (
	"context"
	"fmt"
	"math"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/chat/trigger"
	"oh-my-qqbot/internal/message"
)

// Constants copied verbatim from runtime.py.
const (
	// MaxInternalRounds mirrors MAX_INTERNAL_ROUNDS.
	MaxInternalRounds = 10
	// MaxRetainedMessageCacheSize mirrors MAX_RETAINED_MESSAGE_CACHE_SIZE.
	MaxRetainedMessageCacheSize = 200
	// ContextRestoreFillRatio mirrors CONTEXT_RESTORE_FILL_RATIO.
	ContextRestoreFillRatio = 0.5
	// ExternalMessageIntervalSampleWindowSeconds mirrors
	// EXTERNAL_MESSAGE_INTERVAL_SAMPLE_WINDOW_SECONDS.
	ExternalMessageIntervalSampleWindowSeconds = 1800.0
	// ExternalMessageBurstIntervalSeconds mirrors
	// EXTERNAL_MESSAGE_BURST_INTERVAL_SECONDS: adjacent external messages closer
	// than this are treated as one burst and skipped when sampling intervals.
	ExternalMessageBurstIntervalSeconds = 5.0
	// IdleCompensationMinAverageIntervalSeconds mirrors
	// IDLE_COMPENSATION_MIN_AVERAGE_INTERVAL_SECONDS.
	IdleCompensationMinAverageIntervalSeconds = 30.0
)

// ---------------------------------------------------------------------------
// runtime options
// ---------------------------------------------------------------------------

// Options carries the configuration values resolved by the caller (this package
// never imports internal/config).
type Options struct {
	// SessionID mirrors the runtime session id.
	SessionID string
	// IsGroupSession mirrors `chat_stream.is_group_session`.
	IsGroupSession bool
	// MaxContextSize mirrors `_max_context_size` (group: chat.max_context_size,
	// private: chat.max_private_context_size).
	MaxContextSize int
	// PlannerInterruptMaxConsecutiveCount mirrors
	// `_planner_interrupt_max_consecutive_count` (0 disables interrupting).
	PlannerInterruptMaxConsecutiveCount int
	// MaxConsecutiveWaitCount mirrors `max_consecutive_wait_count` (default 3,
	// minimum 1 as enforced by `_try_enter_wait_state`).
	MaxConsecutiveWaitCount int
	// DynamicReplyTriggerEnabled mirrors
	// `is_dynamic_reply_trigger_enabled()` (reply_trigger_mode == "dynamic").
	DynamicReplyTriggerEnabled bool
	// BaseReplyFrequency mirrors `_get_base_reply_frequency` (group:
	// chat.reply_timing.talk_value, private: private_talk_value).
	BaseReplyFrequency float64
	// TalkValueProvider mirrors `ChatConfigUtils.get_talk_value` when the
	// dynamic talk-value rules are enabled (`enable_talk_value_rules`). When set
	// it overrides BaseReplyFrequency on every evaluation; nil means the static
	// base value is used.
	TalkValueProvider func() float64
	// TalkFrequencyAdjust is the per-session multiplier updated by
	// `adjust_talk_frequency` (default 1.0).
	TalkFrequencyAdjust float64
	// MessageDebounceSeconds mirrors `_message_debounce_seconds` (default 1.0).
	MessageDebounceSeconds float64
	// MaxInternalRounds overrides MAX_INTERNAL_ROUNDS when > 0.
	MaxInternalRounds int
	// IdleBackoffConfig mirrors the `chat.reply_timing` backoff fields.
	IdleBackoffConfig trigger.ReplyTimingBackoffConfig
	// InevitableAtReply mirrors `chat.reply_timing.inevitable_at_reply`
	//（runtime.py:1294，Python 默认 true）；nil 时按默认 true 处理。
	InevitableAtReply *bool
	// MentionedBotReply mirrors `chat.reply_timing.mentioned_bot_reply`
	//（runtime.py:1295，Python 默认 false）；nil 时按默认 false 处理。
	MentionedBotReply *bool
	// FocusModeActive mirrors the focus-mode probe; nil means focus mode is off
	// (the MaiBot default `experimental.focus_mode = false`).
	FocusModeActive func() bool
	// FocusEntryGate mirrors `focus_mode_manager.try_enter_focus`: it reports
	// whether this chat may enter the focus slot. nil means "no focus gating"
	// (the default while focus mode is off).
	FocusEntryGate func(sessionID string, isAt bool) bool
	// FocusDecideGate mirrors `focus_mode_manager.can_decide`: it reports whether
	// the session may run a decision loop right now. nil means "always allowed"
	// (the default while focus mode is off).
	FocusDecideGate func(sessionID string) bool
	// EnableContextOptimization mirrors `chat.enable_context_optimization`.
	EnableContextOptimization bool
	// MidTermMemory mirrors `chat.mid_term_memory`.
	MidTermMemory bool
	// MidTermMemoryLength mirrors `chat.mid_term_memory_lenth` (kept spelling).
	MidTermMemoryLength int
	// Summarizer mirrors the P6 chat-recall compression; nil disables the fold.
	Summarizer MidTermMemorySummarizer
	// IsBotSelf mirrors `is_bot_self`.
	IsBotSelf func(platform string, userID string) bool
	// Log mirrors `get_logger("maisaka_runtime")`.
	Log func(format string, args ...any)
}

// MaxContextSizeOrDefault mirrors `max(1, int(configured))`.
func (o Options) maxContextSize() int {
	if o.MaxContextSize < 1 {
		return 1
	}
	return o.MaxContextSize
}

func (o Options) maxConsecutiveWaitCount() int {
	if o.MaxConsecutiveWaitCount < 1 {
		return 1
	}
	return o.MaxConsecutiveWaitCount
}

func (o Options) maxInternalRounds() int {
	if o.MaxInternalRounds > 0 {
		return o.MaxInternalRounds
	}
	return MaxInternalRounds
}

// inevitableAtReply mirrors `global_config.chat.reply_timing.inevitable_at_reply`
// 的默认值语义（official_configs.py:545 默认 True；nil → true）。
func (o Options) inevitableAtReply() bool {
	return o.InevitableAtReply == nil || *o.InevitableAtReply
}

// mentionedBotReply mirrors `global_config.chat.reply_timing.mentioned_bot_reply`
// 的默认值语义（official_configs.py:531 默认 False；nil → false）。
func (o Options) mentionedBotReply() bool {
	return o.MentionedBotReply != nil && *o.MentionedBotReply
}

// ---------------------------------------------------------------------------
// runtime
// ---------------------------------------------------------------------------

// Runtime mirrors MaisakaHeartFlowChatting: one instance per chat session.
type Runtime struct {
	options Options
	log     func(format string, args ...any)

	mu sync.Mutex

	chatHistory []chatcontext.LLMContextMessage

	// messageCache mirrors `message_cache` (all original messages, for batching
	// and later learning); lastProcessedIndex mirrors `_last_processed_index`.
	messageCache       []*message.SessionMessage
	lastProcessedIndex int

	// internalTurnQueue mirrors `_internal_turn_queue` (message/timeout/proactive).
	internalTurnQueue []string
	// turnNotify wakes the engine loop when a trigger is queued (the Go stand-in
	// for `asyncio.Queue.get()` blocking).
	turnNotify chan struct{}

	agentState           string
	consecutiveWaitCount int
	consecutiveIdleCount int

	pendingWaitToolCallID    *string
	pendingWaitLogicalTurnID *string
	pendingWaitStartedAt     *float64
	pendingWaitSeconds       *float64

	messageTurnScheduled bool
	deferredTurnDelay    *float64
	// deferredTurnTimer 是 deferredTurnDelay 的定时触发器（P1-3 修复：对齐
	// Python `_defer_message_turn_check` 的 asyncio.create_task——延迟睡够后
	// 重跑 schedule_message_turn。Python 靠事件循环调度，Go 用 time.AfterFunc，
	// 回调在独立 goroutine 执行，全部字段访问必须持 r.mu）。
	deferredTurnTimer       *time.Timer
	messageDebounceRequired bool

	running                        bool
	lastMessageReceivedAt          float64
	lastExternalMessageReceivedAt  *float64
	recentExternalMessageIntervals []timedInterval

	talkFrequencyAdjust float64

	forcedTurnEnabled   bool
	forcedTurnMessageID string
	forcedTurnReason    string
	summarizer          MidTermMemorySummarizer

	plannerContinuationActive bool

	interrupt *PlannerInterruptController

	scheduler *trigger.MessageTurnScheduler
	backoff   *trigger.IdleBackoffController
	engine    *Engine

	// 引擎循环守护（panic 自愈；字段均由 r.mu 保护）。
	loopStop       <-chan struct{} // Start 时记录的 stop 通道（自愈重建用）
	loopSupervised bool            // 引擎循环 goroutine 是否已有守护在跑

	// hooks injected by the runtime owner (P4b wiring).
	planner                 PlannerLLM
	roundObserver           func(RoundInfo)
	proactiveTriggerMessage *message.SessionMessage
	proactiveLogicalTurnID  string
}

type timedInterval struct {
	at       float64
	interval float64
}

// RoundInfo describes one internal round for monitoring (mirrors the state the
// reasoning engine reports on `planner.progress`).
type RoundInfo struct {
	RoundIndex     int
	MaxRounds      int
	CycleID        int
	PlannerContent string
	ToolCalls      []string
	Interrupted    bool
	EndReason      string
	EndDetail      string
}

// NewRuntime mirrors MaisakaHeartFlowChatting.__init__ (trimmed to the surfaces
// this work package owns).
func NewRuntime(options Options) *Runtime {
	if options.maxConsecutiveWaitCount() != options.MaxConsecutiveWaitCount && options.MaxConsecutiveWaitCount == 0 {
		options.MaxConsecutiveWaitCount = 3
	}
	if options.TalkFrequencyAdjust <= 0 {
		options.TalkFrequencyAdjust = 1.0
	}
	if options.MessageDebounceSeconds <= 0 {
		options.MessageDebounceSeconds = 1.0
	}
	if options.Log == nil {
		options.Log = func(string, ...any) {}
	}
	if options.IdleBackoffConfig == (trigger.ReplyTimingBackoffConfig{}) {
		options.IdleBackoffConfig = trigger.DefaultReplyTimingBackoffConfig()
	}

	instance := &Runtime{
		options:             options,
		log:                 options.Log,
		agentState:          trigger.StateStop,
		interrupt:           NewPlannerInterruptController(),
		talkFrequencyAdjust: options.TalkFrequencyAdjust,
		summarizer:          options.Summarizer,
		turnNotify:          make(chan struct{}, 1),
	}
	instance.backoff = trigger.NewIdleBackoffController(instance, options.IdleBackoffConfig, nil, options.Log)
	instance.scheduler = trigger.NewMessageTurnScheduler(instance, trigger.SchedulerOptions{
		DynamicReplyTriggerEnabled: options.DynamicReplyTriggerEnabled,
		FocusGate:                  nil,
		IsBotSelf:                  options.IsBotSelf,
		Log:                        options.Log,
	})
	instance.engine = NewEngine(instance)
	return instance
}

// SetPlanner injects the planner implementation (P4b).
func (r *Runtime) SetPlanner(planner PlannerLLM) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.planner = planner
}

// Planner returns the planner implementation.
func (r *Runtime) Planner() PlannerLLM {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.planner
}

// SetMidTermMemorySummarizer injects the chat-recall compression implementation
// (P6). Passing nil disables the fold while the trimming still runs.
func (r *Runtime) SetMidTermMemorySummarizer(summarizer MidTermMemorySummarizer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.summarizer = summarizer
}

// MidTermMemorySummarizer returns the injected summarizer (nil when unset).
func (r *Runtime) MidTermMemorySummarizer() MidTermMemorySummarizer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.summarizer
}

// PostProcessChatHistoryAfterCycle mirrors
// `_post_process_chat_history_after_cycle`: it runs the context trimming, folds
// the removed prefix into a single chat-recall entry (mid_term_memory) when
// enabled and calls the planner's optional history hook.
//
// enableMidTermMemory is false for the silent branch (`_handle_silent_turn`
// passes `enable_mid_term_memory=False`).
func (r *Runtime) PostProcessChatHistoryAfterCycle(
	ctx context.Context,
	cycle CycleInfo,
	enableMidTermMemory bool,
) (chatcontext.HistoryPostProcessResult, error) {
	history := r.ChatHistory()
	result, err := chatcontext.ProcessChatHistoryAfterCycle(
		history,
		r.options.maxContextSize(),
		r.options.EnableContextOptimization,
		r.GetPendingWaitToolCallIDs(),
	)
	if err != nil {
		return chatcontext.HistoryPostProcessResult{}, err
	}
	if result.ChangedCount <= 0 {
		return result, nil
	}

	finalHistory := result.History
	if len(result.RemovedMessages) > 0 && enableMidTermMemory && r.options.MidTermMemory {
		summarizer := r.MidTermMemorySummarizer()
		if summarizer == nil {
			r.log("%s 聊天回想未配置摘要实现，跳过生成", r.LogPrefix())
		} else {
			r.log("%s 开始生成聊天回想: 裁切上下文消息数量=%d 保留上限=%d",
				r.LogPrefix(), len(result.RemovedMessages), r.options.MidTermMemoryLength)
			payload, ok, summarizeErr := summarizer.SummarizeMidTermMemory(ctx, result.RemovedMessages)
			if summarizeErr != nil {
				r.log("%s 生成聊天回想失败，已跳过本次插入: %v", r.LogPrefix(), summarizeErr)
			} else if ok && payload != nil {
				summaryMessage := chatcontext.BuildMidTermMemoryComplexMessage(*payload, result.RemovedMessages)
				finalHistory = chatcontext.InsertMidTermMemoryMessage(
					finalHistory, summaryMessage, maxInt(0, r.options.MidTermMemoryLength))
				r.log("%s 已生成聊天回想: msg_id=%s", r.LogPrefix(), summaryMessage.MsgID)
			} else {
				r.log("%s 聊天回想未产生可插入内容，已跳过", r.LogPrefix())
			}
		}
	} else if len(result.RemovedMessages) > 0 {
		r.log("%s 聊天回想未启用，跳过生成", r.LogPrefix())
	}

	r.SetChatHistory(finalHistory)
	result.History = finalHistory
	cycle.HistoryPostProcess = &result
	if hook, ok := r.Planner().(HistoryPostProcessor); ok {
		hook.AfterHistoryPostProcessed(ctx, result, enableMidTermMemory)
	}
	if result.RemovedCount > 0 {
		r.log("%s 已裁切聊天历史: 移除=%d 剩余上下文=%d",
			r.LogPrefix(), result.RemovedCount, result.RemainingContextCount)
	}
	return result, nil
}

// Engine exposes the reasoning engine driving the internal rounds.
func (r *Runtime) Engine() *Engine { return r.engine }

// Scheduler exposes the turn scheduler.
func (r *Runtime) Scheduler() *trigger.MessageTurnScheduler { return r.scheduler }

// Options returns the resolved options.
func (r *Runtime) Options() Options { return r.options }

// Logf writes through the injected logger.
func (r *Runtime) Logf(format string, args ...any) { r.log(format, args...) }

// ---------------------------------------------------------------------------
// trigger.SchedulerRuntime implementation
// ---------------------------------------------------------------------------

// SessionID mirrors `runtime.session_id`.
func (r *Runtime) SessionID() string { return r.options.SessionID }

// IsGroupSession mirrors `runtime.chat_stream.is_group_session`.
func (r *Runtime) IsGroupSession() bool { return r.options.IsGroupSession }

// LogPrefix mirrors `runtime.log_prefix`.
func (r *Runtime) LogPrefix() string { return fmt.Sprintf("[%s]", r.options.SessionID) }

// AgentState mirrors `runtime._agent_state`.
func (r *Runtime) AgentState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agentState
}

// EnterRunningState mirrors `_enter_running_state` (does not clear the wait
// suspension info).
func (r *Runtime) EnterRunningState() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agentState = trigger.StateRunning
}

// EnterStopState mirrors `_enter_stop_state`.
func (r *Runtime) EnterStopState() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agentState = trigger.StateStop
	r.resetConsecutiveWaitCountLocked("stop_state")
	r.clearWaitStateLocked()
}

// PendingMessageCount mirrors `_get_pending_message_count`.
func (r *Runtime) PendingMessageCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.messageCache) - r.lastProcessedIndex
}

// MessageCacheSize 返回消息缓存总条数（监控用；区别于 PendingMessageCount
// 的「未消费条数」口径）。
func (r *Runtime) MessageCacheSize() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.messageCache)
}

// PendingMessages mirrors `runtime.message_cache[runtime._last_processed_index:]`.
func (r *Runtime) PendingMessages() []*message.SessionMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastProcessedIndex >= len(r.messageCache) {
		return nil
	}
	return append([]*message.SessionMessage{}, r.messageCache[r.lastProcessedIndex:]...)
}

// HasPendingMessages mirrors `_has_pending_messages`.
func (r *Runtime) HasPendingMessages() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastProcessedIndex < len(r.messageCache)
}

// MessageTurnScheduled mirrors `runtime._message_turn_scheduled`.
func (r *Runtime) MessageTurnScheduled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.messageTurnScheduled
}

// EffectiveReplyFrequency mirrors `_get_effective_reply_frequency`.
//
// `ChatConfigUtils.get_talk_value` (the dynamic talk-value rules) is injected
// through Options.TalkValueProvider; without it the static base value is used.
func (r *Runtime) EffectiveReplyFrequency() float64 {
	baseTalkValue := r.options.BaseReplyFrequency
	if r.options.TalkValueProvider != nil {
		baseTalkValue = r.options.TalkValueProvider()
	}
	r.mu.Lock()
	talkFrequencyAdjust := r.talkFrequencyAdjust
	r.mu.Unlock()
	if baseTalkValue <= 0 || talkFrequencyAdjust <= 0 {
		return 0.0
	}
	return math.Max(0.0, baseTalkValue*talkFrequencyAdjust)
}

// IsReplyFrequencySilent mirrors `_is_reply_frequency_silent`.
func (r *Runtime) IsReplyFrequencySilent() bool { return r.EffectiveReplyFrequency() <= 0.0 }

// AdjustTalkFrequency mirrors `adjust_talk_frequency`.
func (r *Runtime) AdjustTalkFrequency(frequency float64) {
	r.mu.Lock()
	r.talkFrequencyAdjust = math.Max(0.0, frequency)
	r.mu.Unlock()
	r.ScheduleMessageTurn()
}

// MessageTriggerThreshold mirrors `_get_message_trigger_threshold`:
// `max(1, ceil(1 / frequency))`, 0 when the frequency is silent.
func (r *Runtime) MessageTriggerThreshold() int {
	effectiveFrequency := math.Min(1.0, r.EffectiveReplyFrequency())
	if effectiveFrequency <= 0 {
		return 0
	}
	return maxInt(1, int(math.Ceil(1.0/effectiveFrequency)))
}

// HasForcedTurnTrigger mirrors `_has_forced_turn_trigger`.
func (r *Runtime) HasForcedTurnTrigger() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forcedTurnEnabled
}

// enqueueTriggerLocked appends a trigger and wakes the loop (the caller must
// hold r.mu).
func (r *Runtime) enqueueTriggerLocked(triggerKind string) {
	r.internalTurnQueue = append(r.internalTurnQueue, triggerKind)
	select {
	case r.turnNotify <- struct{}{}:
	default:
	}
}

// EnqueueMessageTurn mirrors `_enqueue_message_turn`（含
// `_cancel_deferred_message_turn_task`：投递消息触发时取消待执行的延迟复查，
// 并清空挂起延迟的观察值）。
func (r *Runtime) EnqueueMessageTurn() {
	r.mu.Lock()
	r.cancelDeferredTurnTimerLocked()
	r.deferredTurnDelay = nil
	r.messageTurnScheduled = true
	r.enqueueTriggerLocked(TriggerMessage)
	r.mu.Unlock()
}

// DeferMessageTurnCheck mirrors `_defer_message_turn_check`: 记录延迟并起定时
// 任务，睡够后重跑 schedule_message_turn（P1-3 修复，对齐 runtime.py:1249-1254）。
func (r *Runtime) DeferMessageTurnCheck(delaySeconds float64) {
	r.mu.Lock()
	delay := delaySeconds
	r.deferredTurnDelay = &delay
	// 先取消旧定时任务再建新任务（对齐 `_defer_message_turn_check` 的
	// cancel + create_task 语义，重复调度只保留最后一次）。
	r.cancelDeferredTurnTimerLocked()
	delaySecondsValue := delaySeconds
	r.deferredTurnTimer = time.AfterFunc(time.Duration(delaySecondsValue*float64(time.Second)), r.FireDeferredTurnCheck)
	r.mu.Unlock()
}

// cancelDeferredTurnTimerLocked 停掉待执行的延迟复查定时器（调用方须持 r.mu）。
func (r *Runtime) cancelDeferredTurnTimerLocked() {
	if r.deferredTurnTimer != nil {
		r.deferredTurnTimer.Stop()
		r.deferredTurnTimer = nil
	}
}

// DeferredTurnDelay returns the delay of the pending deferred check (nil when
// none is scheduled).
func (r *Runtime) DeferredTurnDelay() *float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deferredTurnDelay == nil {
		return nil
	}
	value := *r.deferredTurnDelay
	return &value
}

// FireDeferredTurnCheck mirrors `_schedule_deferred_message_turn`（runtime.py
// :1269-1281）：延迟到期后清理挂起状态、检查运行态并重跑调度。
func (r *Runtime) FireDeferredTurnCheck() {
	r.mu.Lock()
	r.cancelDeferredTurnTimerLocked()
	r.deferredTurnDelay = nil
	running := r.running
	r.mu.Unlock()
	if !running {
		// 对齐 Python `if not self._running: return`。
		return
	}
	r.ScheduleMessageTurn()
}

// MarkMessageTurnUnscheduled mirrors `_mark_message_turn_unscheduled`.
func (r *Runtime) MarkMessageTurnUnscheduled() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messageTurnScheduled = false
}

// MarkMessageDebounceRequired mirrors `_mark_message_debounce_required`.
func (r *Runtime) MarkMessageDebounceRequired() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messageDebounceRequired = true
}

// ClearMessageDebounceRequired mirrors `_clear_message_debounce_required`.
func (r *Runtime) ClearMessageDebounceRequired() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messageDebounceRequired = false
}

// IdleBackoff mirrors `runtime._idle_backoff`.
func (r *Runtime) IdleBackoff() *trigger.IdleBackoffController { return r.backoff }

// ChatHistory mirrors `runtime._chat_history`. T3 修复：返回副本——此前直接
// 返回底层切片，调用方持引用迭代期间引擎并发 AppendChatHistory 会触发
// reslice/扩容，读到半新半旧的数据；历史条目量级 ≤ 上下文上限，拷贝开销可接受。
func (r *Runtime) ChatHistory() []chatcontext.LLMContextMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]chatcontext.LLMContextMessage{}, r.chatHistory...)
}

// SetChatHistory replaces the internal history (used by context restore and
// tests).
func (r *Runtime) SetChatHistory(history []chatcontext.LLMContextMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chatHistory = history
}

// AppendChatHistory appends one entry to the internal history.
func (r *Runtime) AppendChatHistory(entries ...chatcontext.LLMContextMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chatHistory = append(r.chatHistory, entries...)
}

// FocusModeActive mirrors `_is_focus_mode_active_for_current_chat`: focus mode
// is an experimental switch that defaults to OFF (`experimental.focus_mode =
// false`), and when it is off `can_decide` always returns true.
func (r *Runtime) FocusModeActive() bool {
	if r.options.FocusModeActive == nil {
		return false
	}
	return r.options.FocusModeActive()
}

// FocusModeActiveForCurrentChat implements trigger.IdleBackoffRuntime.
func (r *Runtime) FocusModeActiveForCurrentChat() bool { return r.FocusModeActive() }

// CanDecide mirrors `focus_mode_manager.can_decide` for the engine loop.
func (r *Runtime) CanDecide() bool {
	if !r.FocusModeActive() || r.options.FocusDecideGate == nil {
		return true
	}
	return r.options.FocusDecideGate(r.options.SessionID)
}

// ScheduleMessageTurn mirrors `_schedule_message_turn`.
func (r *Runtime) ScheduleMessageTurn() { r.scheduler.ScheduleMessageTurn() }

// ---------------------------------------------------------------------------
// message ingestion
// ---------------------------------------------------------------------------

// RegisterMessage mirrors `register_message`: cache the message, update trigger
// state, refresh the idle-backoff interrupt and schedule a turn.
func (r *Runtime) RegisterMessage(sessionMessage *message.SessionMessage) {
	if sessionMessage == nil {
		return
	}
	if r.IsRunning() {
		r.EnsureBackgroundTasksRunning()
	}
	receivedAt := nowSeconds()
	r.mu.Lock()
	r.lastMessageReceivedAt = receivedAt
	r.mu.Unlock()
	r.recordExternalMessageInterval(sessionMessage, receivedAt)
	r.updateMessageTriggerState(sessionMessage)

	r.mu.Lock()
	r.messageCache = append(r.messageCache, sessionMessage)
	r.mu.Unlock()
	r.pruneProcessedMessageCache()

	if !r.shouldContinueAfterFocusGate(sessionMessage) {
		return
	}
	if r.AgentState() == trigger.StateRunning {
		r.MarkMessageDebounceRequired()
	}
	r.RequestPlannerInterruptForMessage(sessionMessage)
	if r.IsRunning() {
		r.ScheduleMessageTurn()
	}
}

// shouldContinueAfterFocusGate mirrors `_should_continue_after_focus_gate`: when
// focus mode is active and the chat does not hold the focus slot, the message is
// only cached and no Maisaka decision is scheduled.
func (r *Runtime) shouldContinueAfterFocusGate(sessionMessage *message.SessionMessage) bool {
	if !r.FocusModeActive() {
		return true
	}
	if r.options.FocusEntryGate == nil {
		return true
	}
	if r.options.FocusEntryGate(r.options.SessionID, sessionMessage.IsAt) {
		return true
	}
	r.log("%s focus_mode 已启用且当前会话未获得关注槽，仅缓存消息不进入 Maisaka 决策；消息编号=%s",
		r.LogPrefix(), sessionMessage.MessageID)
	return false
}

// UpdateMessageTriggerState mirrors `_update_message_trigger_state`（runtime.py
// :1283-1296）: the Go port receives the @/mention flags already resolved by the
// channel layer, and only maps them onto the forced-turn state. T4 修复：按配置
// 门控——IsAt 受 inevitable_at_reply（默认 true）、IsMentioned 受
// mentioned_bot_reply（默认 false），此前一律武装强制触发、忽略配置。
func (r *Runtime) updateMessageTriggerState(sessionMessage *message.SessionMessage) {
	if !sessionMessage.IsAt && !sessionMessage.IsMentioned {
		return
	}
	shouldForceReply := (sessionMessage.IsAt && r.options.inevitableAtReply()) ||
		(sessionMessage.IsMentioned && r.options.mentionedBotReply())
	if !shouldForceReply {
		return
	}
	r.armForcedTurn(sessionMessage, sessionMessage.IsAt, sessionMessage.IsMentioned)
}

func (r *Runtime) armForcedTurn(sessionMessage *message.SessionMessage, isAt bool, isMentioned bool) {
	triggerReason := "触发消息"
	switch {
	case isAt:
		triggerReason = "@消息"
	case isMentioned:
		triggerReason = "提及消息"
	}
	wasArmed := r.armForcedTurnState(sessionMessage.MessageID, triggerReason)
	r.backoff.Reset()
	if wasArmed {
		r.log("%s 检测到新的%s，刷新强制 continue 状态；消息编号=%s", r.LogPrefix(), triggerReason, sessionMessage.MessageID)
		return
	}
	r.log("%s 检测到%s，下一轮 Planner 将强制触发；消息编号=%s", r.LogPrefix(), triggerReason, sessionMessage.MessageID)
}

func (r *Runtime) armForcedTurnState(messageID string, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	wasArmed := r.forcedTurnEnabled
	r.forcedTurnEnabled = true
	r.forcedTurnMessageID = strings.TrimSpace(messageID)
	r.forcedTurnReason = strings.TrimSpace(reason)
	return wasArmed
}

// ConsumeForcedTurnReason mirrors `_consume_forced_turn_reason`; ok=false means
// no forced turn was armed.
func (r *Runtime) ConsumeForcedTurnReason() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.forcedTurnEnabled {
		return "", false
	}
	triggerReason := r.forcedTurnReason
	if triggerReason == "" {
		triggerReason = "@/提及消息"
	}
	triggerMessageID := r.forcedTurnMessageID
	if triggerMessageID == "" {
		triggerMessageID = "unknown"
	}
	reason := fmt.Sprintf("检测到新的%s（消息编号=%s），本轮直接进入 Planner。", triggerReason, triggerMessageID)
	r.log("%s 已结束本次强制触发状态；触发原因=%s 触发消息编号=%s", r.LogPrefix(), triggerReason, triggerMessageID)
	r.clearForcedTurnStateLocked()
	return reason, true
}

// ClearForcedTurnState mirrors `_clear_forced_turn_state`.
func (r *Runtime) ClearForcedTurnState() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearForcedTurnStateLocked()
}

func (r *Runtime) clearForcedTurnStateLocked() {
	r.forcedTurnEnabled = false
	r.forcedTurnMessageID = ""
	r.forcedTurnReason = ""
}

// StartPlannerContinuation mirrors `_start_planner_continuation`.
func (r *Runtime) StartPlannerContinuation() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plannerContinuationActive = true
}

// EndPlannerContinuation mirrors `_end_planner_continuation`.
func (r *Runtime) EndPlannerContinuation() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plannerContinuationActive = false
}

// IsPlannerContinuationActive mirrors `_is_planner_continuation_active`.
func (r *Runtime) IsPlannerContinuationActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.plannerContinuationActive
}

// RecordPlannerReply mirrors `record_planner_reply`.
func (r *Runtime) RecordPlannerReply() { r.scheduler.RecordReply() }

// RecordIdleCycleResult mirrors `record_idle_cycle_result`.
func (r *Runtime) RecordIdleCycleResult(cycleEndReason string) {
	if trigger.IsIdleCycleReason(cycleEndReason) {
		r.mu.Lock()
		r.consecutiveIdleCount++
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	r.consecutiveIdleCount = 0
	r.mu.Unlock()
}

// ConsecutiveIdleCount returns the consecutive idle cycle count.
func (r *Runtime) ConsecutiveIdleCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.consecutiveIdleCount
}

// ---------------------------------------------------------------------------
// external message intervals
// ---------------------------------------------------------------------------

// RecentAverageExternalMessageInterval mirrors
// `_get_recent_average_external_message_interval`.
func (r *Runtime) RecentAverageExternalMessageInterval() (float64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneRecentExternalMessageIntervalsLocked(nowSeconds())
	if len(r.recentExternalMessageIntervals) == 0 {
		if r.lastExternalMessageReceivedAt == nil {
			return 0, false
		}
		return IdleCompensationMinAverageIntervalSeconds, true
	}
	total := 0.0
	for _, sample := range r.recentExternalMessageIntervals {
		total += sample.interval
	}
	average := total / float64(len(r.recentExternalMessageIntervals))
	return math.Max(average, IdleCompensationMinAverageIntervalSeconds), true
}

// LastExternalMessageReceivedAt mirrors `_last_external_message_received_at`.
func (r *Runtime) LastExternalMessageReceivedAt() (float64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastExternalMessageReceivedAt == nil {
		return 0, false
	}
	return *r.lastExternalMessageReceivedAt, true
}

// LastMessageReceivedAt mirrors `_last_message_received_at`.
func (r *Runtime) LastMessageReceivedAt() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastMessageReceivedAt
}

// recordExternalMessageInterval mirrors `_record_external_message_interval`.
func (r *Runtime) recordExternalMessageInterval(sessionMessage *message.SessionMessage, receivedAt float64) {
	if r.options.IsBotSelf != nil && r.options.IsBotSelf(sessionMessage.Platform, sessionMessage.UserID()) {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	previousReceivedAt := r.lastExternalMessageReceivedAt
	received := receivedAt
	r.lastExternalMessageReceivedAt = &received
	if previousReceivedAt == nil {
		return
	}
	messageInterval := math.Max(0.0, receivedAt-*previousReceivedAt)
	if messageInterval < ExternalMessageBurstIntervalSeconds {
		// 连发抖动：同一阵内的短间隔不代表真实发言节奏，跳过以免拉低平均间隔。
		return
	}
	r.recentExternalMessageIntervals = append(r.recentExternalMessageIntervals, timedInterval{at: receivedAt, interval: messageInterval})
	r.pruneRecentExternalMessageIntervalsLocked(receivedAt)
	r.log("%s 已记录外部消息接收间隔: %.2f 秒 最近30分钟样本数=%d",
		r.LogPrefix(), messageInterval, len(r.recentExternalMessageIntervals))
}

func (r *Runtime) pruneRecentExternalMessageIntervalsLocked(now float64) {
	expireBefore := now - ExternalMessageIntervalSampleWindowSeconds
	cut := 0
	for cut < len(r.recentExternalMessageIntervals) && r.recentExternalMessageIntervals[cut].at < expireBefore {
		cut++
	}
	if cut > 0 {
		r.recentExternalMessageIntervals = r.recentExternalMessageIntervals[cut:]
	}
}

// ---------------------------------------------------------------------------
// message cache
// ---------------------------------------------------------------------------

// PruneProcessedMessageCache mirrors `_prune_processed_message_cache`.
func (r *Runtime) PruneProcessedMessageCache() { r.pruneProcessedMessageCache() }

func (r *Runtime) pruneProcessedMessageCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	excessCount := len(r.messageCache) - MaxRetainedMessageCacheSize
	if excessCount <= 0 {
		return
	}
	removableCount := minInt(excessCount, r.lastProcessedIndex)
	if removableCount <= 0 {
		return
	}
	r.messageCache = r.messageCache[removableCount:]
	r.lastProcessedIndex = maxInt(0, r.lastProcessedIndex-removableCount)
	r.log("%s 已清理 Maisaka 旧消息缓存: 清理数量=%d 保留数量=%d",
		r.LogPrefix(), removableCount, len(r.messageCache))
}

// CollectPendingMessages mirrors `_collect_pending_messages`.
func (r *Runtime) CollectPendingMessages() []*message.SessionMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	startIndex := r.lastProcessedIndex
	if startIndex >= len(r.messageCache) {
		return nil
	}
	pending := append([]*message.SessionMessage{}, r.messageCache[startIndex:]...)
	r.lastProcessedIndex = len(r.messageCache)
	return pending
}

// ---------------------------------------------------------------------------
// wait state machine
// ---------------------------------------------------------------------------

// PendingWaitToolCallID returns the wait call id awaiting completion (nil when
// none).
func (r *Runtime) PendingWaitToolCallID() *string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingWaitToolCallID == nil {
		return nil
	}
	value := *r.pendingWaitToolCallID
	return &value
}

// HasPendingWaitToolCall mirrors `_has_pending_wait_tool_call`.
func (r *Runtime) HasPendingWaitToolCall() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pendingWaitToolCallID != nil
}

// GetPendingWaitToolCallIDs mirrors `_get_pending_wait_tool_call_ids`.
func (r *Runtime) GetPendingWaitToolCallIDs() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingWaitToolCallID == nil {
		return map[string]bool{}
	}
	return map[string]bool{*r.pendingWaitToolCallID: true}
}

// TryEnterWaitState mirrors `_try_enter_wait_state`: returns (entered,
// currentCount, maxCount).
func (r *Runtime) TryEnterWaitState(seconds *float64, toolCallID string, logicalTurnID string) (bool, int, int) {
	maxCount := r.options.maxConsecutiveWaitCount()
	r.mu.Lock()
	if r.consecutiveWaitCount >= maxCount {
		current := r.consecutiveWaitCount
		r.mu.Unlock()
		return false, current, maxCount
	}
	r.consecutiveWaitCount++
	current := r.consecutiveWaitCount
	r.mu.Unlock()
	if err := r.enterWaitState(seconds, toolCallID, logicalTurnID); err != nil {
		return false, current, maxCount
	}
	return true, current, maxCount
}

// EnterWaitState mirrors `_enter_wait_state`.
func (r *Runtime) enterWaitState(seconds *float64, toolCallID string, logicalTurnID string) error {
	if strings.TrimSpace(toolCallID) == "" || strings.TrimSpace(logicalTurnID) == "" {
		return fmt.Errorf("runtime: 进入 wait 状态必须绑定非空 tool_call_id 和 logical_turn_id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agentState = trigger.StateWait
	callID := toolCallID
	logicalID := logicalTurnID
	r.pendingWaitToolCallID = &callID
	r.pendingWaitLogicalTurnID = &logicalID
	startedAt := nowSeconds()
	r.pendingWaitStartedAt = &startedAt
	if seconds != nil {
		value := *seconds
		r.pendingWaitSeconds = &value
	} else {
		r.pendingWaitSeconds = nil
	}
	r.messageTurnScheduled = false
	r.cancelDeferredTurnTimerLocked()
	r.deferredTurnDelay = nil
	return nil
}

// WaitTimeoutSeconds returns the requested wait duration (nil when unbounded).
func (r *Runtime) WaitTimeoutSeconds() *float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingWaitSeconds == nil {
		return nil
	}
	value := *r.pendingWaitSeconds
	return &value
}

// HandleWaitTimeout mirrors `_schedule_wait_timeout` + its guards: it only
// wakes the loop when still waiting on the same tool call.
func (r *Runtime) HandleWaitTimeout(toolCallID string) bool {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return false
	}
	if r.agentState != trigger.StateWait {
		r.mu.Unlock()
		return false
	}
	if r.pendingWaitToolCallID == nil || *r.pendingWaitToolCallID != toolCallID {
		r.mu.Unlock()
		return false
	}
	r.mu.Unlock()

	r.log("%s Maisaka 等待已超时", r.LogPrefix())
	r.EnterRunningState()
	r.mu.Lock()
	r.enqueueTriggerLocked(TriggerTimeout)
	r.mu.Unlock()
	return true
}

// ResumeFromWaitForProactiveTrigger mirrors
// `_resume_from_wait_for_proactive_trigger`.
func (r *Runtime) ResumeFromWaitForProactiveTrigger() {
	if r.AgentState() != trigger.StateWait {
		return
	}
	r.EnterRunningState()
}

// ConsumePendingWaitState mirrors `_consume_pending_wait_state`; ok=false
// mirrors the Python RuntimeError.
func (r *Runtime) ConsumePendingWaitState() (toolCallID string, logicalTurnID string, elapsedSeconds float64, requestedSeconds *float64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingWaitToolCallID == nil || r.pendingWaitLogicalTurnID == nil {
		return "", "", 0, nil, false
	}
	toolCallID = *r.pendingWaitToolCallID
	logicalTurnID = *r.pendingWaitLogicalTurnID
	if r.pendingWaitStartedAt != nil {
		elapsedSeconds = math.Max(0.0, nowSeconds()-*r.pendingWaitStartedAt)
	}
	if r.pendingWaitSeconds != nil {
		value := *r.pendingWaitSeconds
		requestedSeconds = &value
	}
	r.pendingWaitToolCallID = nil
	r.pendingWaitLogicalTurnID = nil
	r.pendingWaitStartedAt = nil
	r.pendingWaitSeconds = nil
	return toolCallID, logicalTurnID, elapsedSeconds, requestedSeconds, true
}

// ConsecutiveWaitCount returns the current consecutive wait count.
func (r *Runtime) ConsecutiveWaitCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.consecutiveWaitCount
}

// ResetConsecutiveWaitCount mirrors `_reset_consecutive_wait_count`.
func (r *Runtime) ResetConsecutiveWaitCount(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resetConsecutiveWaitCountLocked(reason)
}

func (r *Runtime) resetConsecutiveWaitCountLocked(reason string) {
	if r.consecutiveWaitCount <= 0 {
		return
	}
	r.log("%s 连续 wait 计数已重置: 原计数=%d 原因=%s", r.LogPrefix(), r.consecutiveWaitCount, reason)
	r.consecutiveWaitCount = 0
}

// ClearWaitState mirrors `_clear_wait_state`.
func (r *Runtime) ClearWaitState() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearWaitStateLocked()
}

func (r *Runtime) clearWaitStateLocked() {
	r.pendingWaitToolCallID = nil
	r.pendingWaitLogicalTurnID = nil
	r.pendingWaitStartedAt = nil
	r.pendingWaitSeconds = nil
}

// ---------------------------------------------------------------------------
// internal loop plumbing
// ---------------------------------------------------------------------------

// Internal turn trigger kinds, mirroring `asyncio.Queue[Literal["message",
// "timeout", "proactive"]]`.
const (
	TriggerMessage   = "message"
	TriggerTimeout   = "timeout"
	TriggerProactive = "proactive"
)

// EnqueueProactiveTurn mirrors `_queue_proactive_turn`: it stores the proactive
// trigger message, resumes a waiting runtime and queues the proactive trigger.
func (r *Runtime) EnqueueProactiveTurn(triggerMessage *message.SessionMessage, logicalTurnID string) {
	r.SetProactiveTrigger(triggerMessage, logicalTurnID)
	r.ResumeFromWaitForProactiveTrigger()
	r.mu.Lock()
	r.enqueueTriggerLocked(TriggerProactive)
	r.mu.Unlock()
}

// NextTurnTrigger blocks until a trigger is queued or the runtime stops.
// ok=false means the runtime is no longer running.
func (r *Runtime) NextTurnTrigger(stop <-chan struct{}) (string, bool) {
	for {
		r.mu.Lock()
		if len(r.internalTurnQueue) > 0 {
			triggerKind := r.internalTurnQueue[0]
			r.internalTurnQueue = r.internalTurnQueue[1:]
			r.mu.Unlock()
			return triggerKind, true
		}
		running := r.running
		notify := r.turnNotify
		r.mu.Unlock()
		if !running {
			return "", false
		}
		select {
		case <-notify:
		case <-stop:
			return "", false
		}
	}
}

// DrainReadyTurnTriggers mirrors `_drain_ready_turn_triggers`.
func (r *Runtime) DrainReadyTurnTriggers(queuedTrigger string) (messageTriggered bool, timeoutTriggered bool, proactiveTriggered bool) {
	messageTriggered = queuedTrigger == TriggerMessage
	timeoutTriggered = queuedTrigger == TriggerTimeout
	proactiveTriggered = queuedTrigger == TriggerProactive
	for {
		r.mu.Lock()
		if len(r.internalTurnQueue) == 0 {
			r.mu.Unlock()
			break
		}
		next := r.internalTurnQueue[0]
		r.internalTurnQueue = r.internalTurnQueue[1:]
		r.mu.Unlock()
		switch next {
		case TriggerMessage:
			messageTriggered = true
		case TriggerTimeout:
			timeoutTriggered = true
		case TriggerProactive:
			proactiveTriggered = true
		}
	}
	return messageTriggered, timeoutTriggered, proactiveTriggered
}

// Start marks the runtime as running and starts the background loop, mirroring
// `start`.
func (r *Runtime) Start(stop <-chan struct{}) {
	r.mu.Lock()
	alreadyRunning := r.running
	r.running = true
	r.loopStop = stop
	r.mu.Unlock()
	if alreadyRunning {
		return
	}
	r.log("%s Maisaka 运行时已启动", r.LogPrefix())
	r.superviseEngineLoop(stop)
}

// EnsureBackgroundTasksRunning mirrors `_ensure_background_tasks_running`
// (trimmed to the loop supervision implemented here): 运行态下引擎循环
// goroutine 不在时重新拉起（自愈；幂等）。
func (r *Runtime) EnsureBackgroundTasksRunning() {
	if !r.IsRunning() {
		return
	}
	r.mu.Lock()
	stop := r.loopStop
	r.mu.Unlock()
	if stop == nil {
		return
	}
	r.superviseEngineLoop(stop)
}

// superviseEngineLoop 守护引擎循环 goroutine：panic 时清理排队触发并重建循环，
// 保证单条消息的异常不会击穿 goroutine 使进程退出（幂等：已有守护时直接返回）。
func (r *Runtime) superviseEngineLoop(stop <-chan struct{}) {
	r.mu.Lock()
	if r.loopSupervised {
		r.mu.Unlock()
		return
	}
	r.loopSupervised = true
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			r.loopSupervised = false
			r.mu.Unlock()
		}()
		for r.IsRunning() {
			if !r.runEngineLoop(stop) {
				return
			}
			// panic 自愈：runEngineLoop 已清理本轮排队触发，这里重建循环
			//（stop 关闭时下一次 RunLoop 会立即返回）。
		}
	}()
}

// runEngineLoop 运行一次引擎循环；panic 被拦截时返回 true（由守护层重建）。
func (r *Runtime) runEngineLoop(stop <-chan struct{}) (panicked bool) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log("%s Maisaka 内部循环 panic 已拦截，清理排队触发后自愈: %v\n%s",
				r.LogPrefix(), rec, debug.Stack())
			r.mu.Lock()
			r.internalTurnQueue = nil
			r.messageTurnScheduled = false
			r.mu.Unlock()
			panicked = true
		}
	}()
	if err := r.engine.RunLoop(stop); err != nil {
		r.log("%s Maisaka 内部循环发生异常: %v", r.LogPrefix(), err)
	}
	return false
}

// SetRoundObserver installs a monitoring observer for finished rounds.
func (r *Runtime) SetRoundObserver(observer func(RoundInfo)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roundObserver = observer
}

// RoundObserver returns the installed round observer (nil when unset).
func (r *Runtime) RoundObserver() func(RoundInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.roundObserver
}

// IsRunning reports whether the runtime loop is active.
func (r *Runtime) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Stop mirrors `stop`: it marks the runtime as stopped, wakes the loop and
// clears the wait state.
func (r *Runtime) Stop() {
	r.mu.Lock()
	r.running = false
	r.cancelDeferredTurnTimerLocked()
	r.deferredTurnDelay = nil
	select {
	case r.turnNotify <- struct{}{}:
	default:
	}
	r.mu.Unlock()
	r.EnterStopState()
	// T5：同步取消引擎作用域执行上下文——正在执行的 planner 请求 / 工具
	// （reply 段间 sleep、渠道发送）立即收到取消信号，不必等循环回到
	// NextTurnTrigger 才发现停止。
	r.engine.cancelExecution()
}

// WaitForMessageQuietPeriod mirrors `_wait_for_message_quiet_period`.
func (r *Runtime) WaitForMessageQuietPeriod() {
	r.mu.Lock()
	required := r.messageDebounceRequired
	r.mu.Unlock()
	if !required {
		return
	}
	debounceSeconds := r.options.MessageDebounceSeconds
	if debounceSeconds <= 0 {
		r.ClearMessageDebounceRequired()
		return
	}
	for r.IsRunning() {
		elapsed := nowSeconds() - r.LastMessageReceivedAt()
		remaining := debounceSeconds - elapsed
		if remaining <= 0 {
			break
		}
		time.Sleep(time.Duration(remaining * float64(time.Second)))
	}
	r.ClearMessageDebounceRequired()
}

// FindSourceMessageByID mirrors `find_source_message_by_id`.
func (r *Runtime) FindSourceMessageByID(messageID string) *message.SessionMessage {
	normalized := strings.TrimSpace(messageID)
	if normalized == "" {
		return nil
	}
	history := r.ChatHistory()
	for index := len(history) - 1; index >= 0; index-- {
		entry := history[index]
		if entry == nil {
			continue
		}
		if strings.TrimSpace(entry.MessageID()) != normalized {
			continue
		}
		if original := entry.OriginalMessage(); original != nil {
			return original
		}
	}
	return nil
}

// ExpandMessageIDAliases mirrors `expand_message_id_aliases`.
func (r *Runtime) ExpandMessageIDAliases(arguments map[string]any) map[string]any {
	history := r.ChatHistory()
	messageIDs := make([]string, 0, len(history))
	for index := len(history) - 1; index >= 0; index-- {
		if messageID := history[index].MessageID(); messageID != "" {
			messageIDs = append(messageIDs, messageID)
		}
	}
	aliasMap := chatcontext.BuildAliasMap(messageIDs)
	expanded := chatcontext.ExpandMessageIDAliases(arguments, aliasMap)
	if expandedMap, ok := expanded.(map[string]any); ok {
		return expandedMap
	}
	return arguments
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func nowTime() time.Time { return time.Now() }

// newLogicalTurnID mirrors `uuid.uuid4().hex` for the active logical turn id.
func newLogicalTurnID() string {
	return chatcontext.NewContextItemID()
}

// lastCachedMessage mirrors `self._runtime.message_cache[-1]`.
func (r *Runtime) lastCachedMessage() *message.SessionMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.messageCache) == 0 {
		return nil
	}
	return r.messageCache[len(r.messageCache)-1]
}

// consumeProactiveTriggerMessage mirrors `_consume_proactive_trigger_message`.
func (r *Runtime) consumeProactiveTriggerMessage() *message.SessionMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	triggerMessage := r.proactiveTriggerMessage
	r.proactiveTriggerMessage = nil
	return triggerMessage
}

// consumeProactiveLogicalTurnID mirrors `_consume_proactive_logical_turn_id`.
func (r *Runtime) consumeProactiveLogicalTurnID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	logicalTurnID := r.proactiveLogicalTurnID
	r.proactiveLogicalTurnID = ""
	return logicalTurnID
}

// SetProactiveTrigger installs the pending proactive trigger message (the
// plugin-主动 path of runtime.py; plugins are not ported, so callers use this for
// e.g. scheduled wake-ups).
func (r *Runtime) SetProactiveTrigger(triggerMessage *message.SessionMessage, logicalTurnID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.proactiveTriggerMessage = triggerMessage
	r.proactiveLogicalTurnID = logicalTurnID
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}

func minInt(left int, right int) int {
	if left < right {
		return left
	}
	return right
}
