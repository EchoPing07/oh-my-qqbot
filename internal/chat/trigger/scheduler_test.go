package trigger

import (
	"fmt"
	"math"
	"strings"
	"testing"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/message"
)

// stubSchedulerRuntime implements SchedulerRuntime for the scheduler tests.
type stubSchedulerRuntime struct {
	sessionID  string
	isGroup    bool
	agentState string
	scheduled  bool
	pending    []*message.SessionMessage
	frequency  float64
	silent     bool
	threshold  int
	forced     bool
	history    []chatcontext.LLMContextMessage
	backoff    *IdleBackoffController
	logs       []string

	runningEntered int
	stopEntered    int
	enqueued       int
	deferred       []float64

	interval     float64
	hasInterval  bool
	lastExternal float64
	hasExternal  bool
	lastReceived float64
}

func (s *stubSchedulerRuntime) SessionID() string                          { return s.sessionID }
func (s *stubSchedulerRuntime) IsGroupSession() bool                       { return s.isGroup }
func (s *stubSchedulerRuntime) LogPrefix() string                          { return "[s]" }
func (s *stubSchedulerRuntime) AgentState() string                         { return s.agentState }
func (s *stubSchedulerRuntime) MessageTurnScheduled() bool                 { return s.scheduled }
func (s *stubSchedulerRuntime) EnterRunningState()                         { s.runningEntered++ }
func (s *stubSchedulerRuntime) EnterStopState()                            { s.stopEntered++ }
func (s *stubSchedulerRuntime) PendingMessageCount() int                   { return len(s.pending) }
func (s *stubSchedulerRuntime) PendingMessages() []*message.SessionMessage { return s.pending }
func (s *stubSchedulerRuntime) EffectiveReplyFrequency() float64           { return s.frequency }
func (s *stubSchedulerRuntime) IsReplyFrequencySilent() bool               { return s.silent }
func (s *stubSchedulerRuntime) MessageTriggerThreshold() int               { return s.threshold }
func (s *stubSchedulerRuntime) HasForcedTurnTrigger() bool                 { return s.forced }
func (s *stubSchedulerRuntime) EnqueueMessageTurn()                        { s.enqueued++ }
func (s *stubSchedulerRuntime) DeferMessageTurnCheck(delay float64) {
	s.deferred = append(s.deferred, delay)
}
func (s *stubSchedulerRuntime) IdleBackoff() *IdleBackoffController { return s.backoff }

func (s *stubSchedulerRuntime) ChatHistory() []chatcontext.LLMContextMessage { return s.history }

func (s *stubSchedulerRuntime) RecentAverageExternalMessageInterval() (float64, bool) {
	return s.interval, s.hasInterval
}

func (s *stubSchedulerRuntime) LastExternalMessageReceivedAt() (float64, bool) {
	return s.lastExternal, s.hasExternal
}

func (s *stubSchedulerRuntime) LastMessageReceivedAt() float64 { return s.lastReceived }

func (s *stubSchedulerRuntime) FocusModeActiveForCurrentChat() bool { return false }

func newStubScheduler() *stubSchedulerRuntime {
	runtime := &stubSchedulerRuntime{sessionID: "s1", isGroup: true, agentState: StateStop}
	runtime.backoff = NewIdleBackoffController(runtime, DefaultReplyTimingBackoffConfig(),
		func() float64 { return 1000 }, func(format string, args ...any) {
			runtime.logs = append(runtime.logs, fmt.Sprintf(format, args...))
		})
	return runtime
}

func newScheduler(runtime *stubSchedulerRuntime, options SchedulerOptions) *MessageTurnScheduler {
	if options.Now == nil {
		options.Now = func() float64 { return 1000 }
	}
	if options.Log == nil {
		options.Log = func(format string, args ...any) {
			runtime.logs = append(runtime.logs, fmt.Sprintf(format, args...))
		}
	}
	return NewMessageTurnScheduler(runtime, options)
}

// TestSchedulerSilentConsumptionPinsFrequencyZeroBranch mirrors the
// `_is_reply_frequency_silent()` early return of schedule_message_turn.
func TestSchedulerSilentConsumptionPinsFrequencyZeroBranch(t *testing.T) {
	runtime := newStubScheduler()
	runtime.pending = []*message.SessionMessage{testMessage("a", 999)}
	runtime.frequency = 0
	runtime.silent = true
	scheduler := newScheduler(runtime, SchedulerOptions{DynamicReplyTriggerEnabled: true})

	scheduler.ScheduleMessageTurn()
	if runtime.enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1 (静默消费 still consumes the batch)", runtime.enqueued)
	}
	assertLogContains(t, runtime.logs, "判定=静默消费")
}

// TestSchedulerForcedTurnRecordsExpectedReplies mirrors the
// `_has_forced_turn_trigger()` branch incl. `record_forced_turn`.
func TestSchedulerForcedTurnRecordsExpectedReplies(t *testing.T) {
	runtime := newStubScheduler()
	runtime.pending = []*message.SessionMessage{testMessage("a", 999), testMessage("b", 999)}
	runtime.frequency = 0.5
	runtime.forced = true
	scheduler := newScheduler(runtime, SchedulerOptions{DynamicReplyTriggerEnabled: true})

	scheduler.ScheduleMessageTurn()
	if runtime.enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1", runtime.enqueued)
	}
	decision := scheduler.DynamicGate().Evaluate(0.0, 0.5, 1000)
	if math.Abs(decision.ExpectedReplies-ForcedTurnExpectedReplies) > 1e-12 {
		t.Fatalf("expected forced replies = %v, want %v", decision.ExpectedReplies, ForcedTurnExpectedReplies)
	}
	assertLogContains(t, runtime.logs, "判定=强制触发")
}

// TestSchedulerDynamicModeUsesDynamicGate pins the
// `is_dynamic_reply_trigger_enabled` branch: the frequency threshold gate is not
// consulted.
func TestSchedulerDynamicModeUsesDynamicGate(t *testing.T) {
	runtime := newStubScheduler()
	runtime.pending = []*message.SessionMessage{testMessage("a", 999, withUser("bot"))}
	runtime.frequency = 0.5
	runtime.threshold = 1
	scheduler := newScheduler(runtime, SchedulerOptions{
		DynamicReplyTriggerEnabled: true,
		IsBotSelf:                  func(platform, userID string) bool { return userID == "bot" },
	})

	scheduler.ScheduleMessageTurn()
	assertLogContains(t, runtime.logs, "没有新的外部消息 判定=等待更多消息")
	if runtime.enqueued != 0 {
		t.Fatalf("enqueued = %d, want 0", runtime.enqueued)
	}
}

// TestSchedulerFrequencyModeTriggersAtThreshold pins the frequency branch.
func TestSchedulerFrequencyModeTriggersAtThreshold(t *testing.T) {
	runtime := newStubScheduler()
	runtime.pending = []*message.SessionMessage{testMessage("a", 999), testMessage("b", 999)}
	runtime.frequency = 1.0
	runtime.threshold = 2
	scheduler := newScheduler(runtime, SchedulerOptions{})

	scheduler.ScheduleMessageTurn()
	if runtime.enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1", runtime.enqueued)
	}
	assertLogContains(t, runtime.logs, "pending=2 达到阈值=2 判定=进入Planner")
}

// TestSchedulerFrequencyModeDefersWhenIdleCompensationSaysSo pins the
// `_defer_message_turn_check` call on the delay decision.
func TestSchedulerFrequencyModeDefersWhenIdleCompensationSaysSo(t *testing.T) {
	runtime := newStubScheduler()
	runtime.pending = []*message.SessionMessage{testMessage("a", 999)}
	runtime.frequency = 0.25
	runtime.threshold = 4
	runtime.interval = 60
	runtime.hasInterval = true
	runtime.lastExternal = 900
	runtime.hasExternal = true
	scheduler := newScheduler(runtime, SchedulerOptions{})

	scheduler.ScheduleMessageTurn()
	if runtime.enqueued != 0 {
		t.Fatalf("enqueued = %d, want 0", runtime.enqueued)
	}
	// idle = 100s, equivalent = 1 + min(100/60, 3) = 2.67 < 4
	// delay = (4 - 1) * 60 - 100 = 80
	if len(runtime.deferred) != 1 || math.Abs(runtime.deferred[0]-80) > 1e-9 {
		t.Fatalf("deferred = %v, want [80]", runtime.deferred)
	}
	assertLogContains(t, runtime.logs, "判定=延迟检查")
}

// TestSchedulerPrivateChatWakesFromWait mirrors the private-chat wait wake-up.
func TestSchedulerPrivateChatWakesFromWait(t *testing.T) {
	runtime := newStubScheduler()
	runtime.isGroup = false
	runtime.agentState = StateWait
	runtime.pending = []*message.SessionMessage{testMessage("a", 999)}
	runtime.frequency = 0.5
	runtime.threshold = 2
	scheduler := newScheduler(runtime, SchedulerOptions{})

	scheduler.ScheduleMessageTurn()
	if runtime.runningEntered != 1 {
		t.Fatalf("EnterRunningState calls = %d, want 1", runtime.runningEntered)
	}
	assertLogContains(t, runtime.logs, "私聊 wait 期间收到新消息，结束等待并进入 Planner")
}

// TestSchedulerGroupChatKeepsWaiting mirrors the group-chat branch that keeps
// waiting for the wait timeout.
func TestSchedulerGroupChatKeepsWaiting(t *testing.T) {
	runtime := newStubScheduler()
	runtime.agentState = StateWait
	runtime.pending = []*message.SessionMessage{testMessage("a", 999)}
	runtime.frequency = 0.5
	scheduler := newScheduler(runtime, SchedulerOptions{})

	scheduler.ScheduleMessageTurn()
	if runtime.enqueued != 0 {
		t.Fatalf("enqueued = %d, want 0 (group chat must keep waiting)", runtime.enqueued)
	}
}

// TestSchedulerSilentWaitEntersStop mirrors the silent-frequency wait branch.
func TestSchedulerSilentWaitEntersStop(t *testing.T) {
	runtime := newStubScheduler()
	runtime.agentState = StateWait
	runtime.pending = []*message.SessionMessage{testMessage("a", 999)}
	runtime.frequency = 0
	runtime.silent = true
	scheduler := newScheduler(runtime, SchedulerOptions{})

	scheduler.ScheduleMessageTurn()
	if runtime.stopEntered != 1 {
		t.Fatalf("EnterStopState calls = %d, want 1", runtime.stopEntered)
	}
	if runtime.enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1", runtime.enqueued)
	}
}

// TestSchedulerSkipsWhenAlreadyScheduled mirrors `_message_turn_scheduled`.
func TestSchedulerSkipsWhenAlreadyScheduled(t *testing.T) {
	runtime := newStubScheduler()
	runtime.pending = []*message.SessionMessage{testMessage("a", 999)}
	runtime.frequency = 1
	runtime.threshold = 1
	runtime.scheduled = true
	scheduler := newScheduler(runtime, SchedulerOptions{})

	scheduler.ScheduleMessageTurn()
	if runtime.enqueued != 0 {
		t.Fatalf("enqueued = %d, want 0", runtime.enqueued)
	}
}

// TestSchedulerWithoutPendingMessagesReturns pins the `pending_count <= 0` guard.
func TestSchedulerWithoutPendingMessagesReturns(t *testing.T) {
	runtime := newStubScheduler()
	runtime.frequency = 1
	scheduler := newScheduler(runtime, SchedulerOptions{})
	scheduler.ScheduleMessageTurn()
	if runtime.enqueued != 0 {
		t.Fatalf("enqueued = %d, want 0", runtime.enqueued)
	}
}

// TestSchedulerFocusGateBlocksDecision mirrors `focus_mode_manager.can_decide`.
func TestSchedulerFocusGateBlocksDecision(t *testing.T) {
	runtime := newStubScheduler()
	runtime.pending = []*message.SessionMessage{testMessage("a", 999)}
	runtime.frequency = 1
	runtime.threshold = 1
	scheduler := newScheduler(runtime, SchedulerOptions{
		FocusGate: func(sessionID string, isGroupChat bool) bool { return false },
	})

	scheduler.ScheduleMessageTurn()
	if runtime.enqueued != 0 {
		t.Fatalf("enqueued = %d, want 0", runtime.enqueued)
	}
	assertLogContains(t, runtime.logs, "当前不在 focus 状态，跳过 Maisaka 决策调度")
}

// TestSchedulerIdleBackoffShortCircuits pins the `_idle_backoff.should_delay`
// call between the forced-turn check and the threshold gates.
func TestSchedulerIdleBackoffShortCircuits(t *testing.T) {
	runtime := newStubScheduler()
	runtime.pending = make([]*message.SessionMessage, 3)
	for index := range runtime.pending {
		runtime.pending[index] = testMessage("m"+itoa(index), 999)
	}
	runtime.frequency = 0.25
	runtime.threshold = 4
	// Two idle cycles arm the group backoff window (base 15s starting at count=2).
	runtime.backoff.RecordCycleResult("planner_no_tool_end")
	runtime.backoff.RecordCycleResult("planner_no_tool_end")

	scheduler := newScheduler(runtime, SchedulerOptions{})
	scheduler.ScheduleMessageTurn()
	if runtime.enqueued != 0 {
		t.Fatalf("enqueued = %d, want 0 while the idle backoff is active", runtime.enqueued)
	}
	assertLogContains(t, runtime.logs, "空闲退避中，延迟")
}

// TestSchedulerRecordReplyFeedsDynamicGate pins `record_reply`.
func TestSchedulerRecordReplyFeedsDynamicGate(t *testing.T) {
	runtime := newStubScheduler()
	scheduler := newScheduler(runtime, SchedulerOptions{DynamicReplyTriggerEnabled: true})
	scheduler.RecordReply()
	decision := scheduler.DynamicGate().Evaluate(0.0, 0.5, 1000)
	if decision.ActualReplies != 1 {
		t.Fatalf("actual replies = %d, want 1", decision.ActualReplies)
	}
}

func assertLogContains(t *testing.T, logs []string, needle string) {
	t.Helper()
	for _, line := range logs {
		if strings.Contains(line, needle) {
			return
		}
	}
	t.Fatalf("logs %v do not contain %q", logs, needle)
}
