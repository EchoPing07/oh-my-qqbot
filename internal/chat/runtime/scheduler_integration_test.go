package runtime

import (
	"fmt"
	"math"
	"testing"
	"time"

	"oh-my-qqbot/internal/chat/trigger"
	"oh-my-qqbot/internal/message"
)

// These tests drive the real trigger.MessageTurnScheduler through the runtime
// surface, pinning the dynamic / frequency / forced / silent decisions
// end-to-end (trigger package ↔ runtime package wiring).

// TestSchedulerIntegrationSilentFrequency pins the frequency=0 short circuit.
func TestSchedulerIntegrationSilentFrequency(t *testing.T) {
	runtime := NewRuntime(Options{
		SessionID:              "session",
		IsGroupSession:         true,
		MaxContextSize:         40,
		BaseReplyFrequency:     0,
		MessageDebounceSeconds: 0.01,
		Log:                    func(string, ...any) {},
	})
	runtime.Start(nil)
	defer runtime.Stop()

	runtime.RegisterMessage(runtimeMessage("m1", time.Now()))
	if !runtime.MessageTurnScheduled() {
		t.Fatalf("silent mode must still consume the batch")
	}
	triggerKind, ok := runtime.NextTurnTrigger(nil)
	if !ok || triggerKind != TriggerMessage {
		t.Fatalf("trigger = %q/%v", triggerKind, ok)
	}
	if !runtime.IsReplyFrequencySilent() {
		t.Fatalf("IsReplyFrequencySilent = false")
	}
}

// TestSchedulerIntegrationDynamicMode pins the dynamic gate branch wired through
// `reply_trigger_mode = dynamic`.
func TestSchedulerIntegrationDynamicMode(t *testing.T) {
	now := float64(time.Now().UnixNano()) / 1e9
	runtime := newTestRuntime(Options{
		IsGroupSession:             true,
		BaseReplyFrequency:         0.5,
		DynamicReplyTriggerEnabled: true,
	})
	runtime.scheduler = trigger.NewMessageTurnScheduler(runtime, trigger.SchedulerOptions{
		DynamicReplyTriggerEnabled: true,
		Now:                        func() float64 { return now },
		IsBotSelf:                  func(platform, userID string) bool { return userID == "bot" },
		Log:                        func(string, ...any) {},
	})
	runtime.Start(nil)
	defer runtime.Stop()

	// A placeholder-only message scores 0.1549, below the 0.336 static threshold
	// (keep_ratio = 0.5), so the dynamic gate holds it back.
	placeholder := runtimeMessage("m1", time.Now())
	placeholder.ProcessedPlainText = message.StringPtr("[表情包]")
	runtime.RegisterMessage(placeholder)
	if runtime.MessageTurnScheduled() {
		t.Fatalf("dynamic gate should hold back a single low-likelihood message")
	}

	// A question-mark message scores 0.4532 and is released.
	question := runtimeMessage("m2-question", time.Now())
	question.ProcessedPlainText = message.StringPtr("在看什么呢？")
	runtime.RegisterMessage(question)
	if !runtime.MessageTurnScheduled() {
		t.Fatalf("a question-mark message must be released by the dynamic gate")
	}

	// A @ message is a forced turn: it records the expected replies and enqueues.
	forcedRuntime := newTestRuntime(Options{
		IsGroupSession:             true,
		BaseReplyFrequency:         0.5,
		DynamicReplyTriggerEnabled: true,
	})
	forcedRuntime.scheduler = trigger.NewMessageTurnScheduler(forcedRuntime, trigger.SchedulerOptions{
		DynamicReplyTriggerEnabled: true,
		Now:                        func() float64 { return now },
		Log:                        func(string, ...any) {},
	})
	forcedRuntime.Start(nil)
	defer forcedRuntime.Stop()

	forced := runtimeMessage("m3", time.Now())
	forced.IsAt = true
	forcedRuntime.RegisterMessage(forced)
	if !forcedRuntime.MessageTurnScheduled() {
		t.Fatalf("@ message must force the turn")
	}
	if !forcedRuntime.HasForcedTurnTrigger() {
		t.Fatalf("forced turn state not armed")
	}
	decision := forcedRuntime.scheduler.DynamicGate().Evaluate(0.0, 0.5, now)
	if math.Abs(decision.ExpectedReplies-trigger.ForcedTurnExpectedReplies) > 1e-12 {
		t.Fatalf("forced expected replies = %v, want %v", decision.ExpectedReplies, trigger.ForcedTurnExpectedReplies)
	}
}

// TestSchedulerIntegrationFrequencyMode pins the frequency threshold branch,
// including the idle-compensation delay wiring.
func TestSchedulerIntegrationFrequencyMode(t *testing.T) {
	runtime := newTestRuntime(Options{
		IsGroupSession:     true,
		BaseReplyFrequency: 0.25, // threshold 4
	})
	runtime.Start(nil)
	defer runtime.Stop()

	// One message sets the last-external timestamp, so the frequency gate has an
	// interval sample (the 30s floor) and schedules the deferred re-check:
	// delay = (4 - 1) * 30 - ~0 = ~90s.
	runtime.RegisterMessage(runtimeMessage("m1", time.Now()))
	if runtime.MessageTurnScheduled() {
		t.Fatalf("1/4 messages must not trigger")
	}
	delay := runtime.DeferredTurnDelay()
	if delay == nil || *delay < 85 || *delay > 90 {
		t.Fatalf("deferred delay = %v, want ~90s", delay)
	}

	// Reaching the threshold triggers immediately.
	for index := 0; index < 3; index++ {
		runtime.RegisterMessage(runtimeMessage(fmt.Sprintf("m%d", index), time.Now()))
	}
	if !runtime.MessageTurnScheduled() {
		t.Fatalf("4/4 messages must trigger")
	}
	if runtime.DeferredTurnDelay() != nil {
		t.Fatalf("enqueueing must cancel the deferred check")
	}
}

// TestSchedulerIntegrationIdleBackoffBlocks pins the idle backoff wiring through
// the real scheduler (including the bypass_pending_count interruption).
func TestSchedulerIntegrationIdleBackoffBlocks(t *testing.T) {
	runtime := newTestRuntime(Options{
		IsGroupSession:     true,
		BaseReplyFrequency: 0.25, // threshold 4
	})
	runtime.Start(nil)
	defer runtime.Stop()

	// Two idle cycles arm the 15s window (start_count = 2).
	runtime.IdleBackoff().RecordCycleResult("planner_no_tool_end")
	runtime.IdleBackoff().RecordCycleResult("planner_no_tool_end")
	if until := runtime.IdleBackoff().Until(); until <= 0 {
		t.Fatalf("backoff window not armed: %v", until)
	}
	if runtime.IdleBackoff().BackoffSeconds() != 15 {
		t.Fatalf("backoff seconds = %v, want 15", runtime.IdleBackoff().BackoffSeconds())
	}

	// Below the bypass count (6) the batch is delayed and a re-check scheduled.
	runtime.RegisterMessage(runtimeMessage("m1", time.Now()))
	if runtime.MessageTurnScheduled() {
		t.Fatalf("the idle backoff must hold the message batch back")
	}
	if delay := runtime.DeferredTurnDelay(); delay == nil || *delay <= 0 {
		t.Fatalf("the idle backoff must schedule a deferred re-check, got %v", delay)
	}

	// Pending messages >= bypass_pending_count (6) interrupt the backoff.
	for index := 0; index < 5; index++ {
		runtime.RegisterMessage(runtimeMessage(fmt.Sprintf("b%d", index), time.Now()))
	}
	if !runtime.MessageTurnScheduled() {
		t.Fatalf("6 pending messages must bypass the idle backoff")
	}
	if until := runtime.IdleBackoff().Until(); until <= 0 {
		t.Fatalf("the bypass must not reset the window: %v", until)
	}
}

// TestSchedulerIntegrationReplyFeedback pins
// `record_planner_reply` feeding the dynamic gate.
func TestSchedulerIntegrationReplyFeedback(t *testing.T) {
	now := float64(time.Now().UnixNano()) / 1e9
	runtime := newTestRuntime(Options{IsGroupSession: true, BaseReplyFrequency: 0.5})
	runtime.scheduler = trigger.NewMessageTurnScheduler(runtime, trigger.SchedulerOptions{
		DynamicReplyTriggerEnabled: true,
		Now:                        func() float64 { return now },
		Log:                        func(string, ...any) {},
	})
	runtime.RecordPlannerReply()
	decision := runtime.scheduler.DynamicGate().Evaluate(0.5, 0.5, now)
	if decision.ActualReplies != 1 {
		t.Fatalf("actual replies = %d, want 1", decision.ActualReplies)
	}
}

// TestIdleBackoffRecordingFromCycleResults pins the runtime →
// `_idle_backoff.record_cycle_result` mapping used by the engine.
func TestIdleBackoffRecordingFromCycleResults(t *testing.T) {
	runtime := newTestRuntime(Options{IsGroupSession: true})
	for _, reason := range []string{"planner_no_tool_end", "planner_wait_rest", "tool_pause:wait", "tool_stop_after_execution"} {
		before := runtime.IdleBackoff().Count()
		runtime.IdleBackoff().RecordCycleResult(reason)
		if runtime.IdleBackoff().Count() != before+1 {
			t.Fatalf("reason %q did not increment the idle count", reason)
		}
	}
	// A non-idle reason resets.
	runtime.IdleBackoff().RecordCycleResult("max_rounds")
	if runtime.IdleBackoff().Count() != 0 {
		t.Fatalf("max_rounds must reset the idle count")
	}

	// The separate consecutive-idle counter follows `record_idle_cycle_result`.
	runtime.RecordIdleCycleResult("planner_no_tool_end")
	runtime.RecordIdleCycleResult("planner_no_tool_end")
	if runtime.ConsecutiveIdleCount() != 2 {
		t.Fatalf("consecutive idle count = %d, want 2", runtime.ConsecutiveIdleCount())
	}
	runtime.RecordIdleCycleResult("tool_continue")
	if runtime.ConsecutiveIdleCount() != 0 {
		t.Fatalf("tool_continue must reset the idle count")
	}
}

// TestTriggerBackoffConfigDefaults pins the config surface used by the runtime.
func TestTriggerBackoffConfigDefaults(t *testing.T) {
	defaults := trigger.DefaultReplyTimingBackoffConfig()
	if math.Abs(defaults.BaseSeconds-15) > 1e-9 || math.Abs(defaults.CapSeconds-300) > 1e-9 ||
		defaults.StartCount != 2 || defaults.BypassPendingCount != 6 {
		t.Fatalf("defaults = %+v, want base=15 cap=300 start=2 bypass=6", defaults)
	}
	// `NewRuntime` fills in the defaults when the caller passes the zero value.
	runtime := newTestRuntime(Options{})
	if math.Abs(runtime.Options().IdleBackoffConfig.BaseSeconds-15) > 1e-9 {
		t.Fatalf("runtime backoff config = %+v", runtime.Options().IdleBackoffConfig)
	}
}
