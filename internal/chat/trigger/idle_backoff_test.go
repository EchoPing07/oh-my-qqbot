package trigger

import (
	"fmt"
	"math"
	"testing"
)

type stubBackoffRuntime struct {
	isGroup     bool
	focusActive bool
	deferCalls  []float64
}

func (s *stubBackoffRuntime) IsGroupSession() bool { return s.isGroup }

func (s *stubBackoffRuntime) FocusModeActiveForCurrentChat() bool { return s.focusActive }

func (s *stubBackoffRuntime) DeferMessageTurnCheck(delaySeconds float64) {
	s.deferCalls = append(s.deferCalls, delaySeconds)
}

func (s *stubBackoffRuntime) LogPrefix() string { return "[test]" }

// TestIdleBackoffCurve pins `min(cap, base * 2^(count - start))` from
// idle_backoff.py with the MaiBot defaults (base 15, cap 300, start 2).
func TestIdleBackoffCurve(t *testing.T) {
	defaults := DefaultReplyTimingBackoffConfig()
	if defaults.BaseSeconds != 15 || defaults.CapSeconds != 300 || defaults.StartCount != 2 ||
		defaults.BypassPendingCount != 6 {
		t.Fatalf("defaults = %+v, want {15 300 2 6}", defaults)
	}

	runtime := &stubBackoffRuntime{isGroup: true}
	now := 1000.0
	controller := NewIdleBackoffController(runtime, defaults, func() float64 { return now }, nil)

	wantByCount := map[int]float64{
		0: 0,   // below start_count
		1: 0,   // below start_count
		2: 15,  // base * 2^0
		3: 30,  // base * 2^1
		4: 60,  // base * 2^2
		5: 120, // base * 2^3
		6: 240, // base * 2^4
		7: 300, // capped
		8: 300,
	}
	for count := 0; count <= 8; count++ {
		controller.count = count
		if got := controller.BackoffSeconds(); got != wantByCount[count] {
			t.Errorf("BackoffSeconds(count=%d) = %v, want %v", count, got, wantByCount[count])
		}
	}
}

// TestIdleBackoffCustomStartAndCap exercises a non-default configuration.
func TestIdleBackoffCustomStartAndCap(t *testing.T) {
	config := ReplyTimingBackoffConfig{BaseSeconds: 10, CapSeconds: 35, StartCount: 1, BypassPendingCount: 3}
	controller := NewIdleBackoffController(&stubBackoffRuntime{isGroup: true}, config, nil, nil)
	for count, want := range map[int]float64{1: 10, 2: 20, 3: 35, 4: 35} {
		controller.count = count
		if got := controller.BackoffSeconds(); got != want {
			t.Errorf("BackoffSeconds(count=%d) = %v, want %v", count, got, want)
		}
	}

	// base/cap <= 0 disables the backoff entirely.
	disabled := NewIdleBackoffController(&stubBackoffRuntime{isGroup: true},
		ReplyTimingBackoffConfig{BaseSeconds: 0, CapSeconds: 300, StartCount: 1}, nil, nil)
	disabled.count = 5
	if got := disabled.BackoffSeconds(); got != 0 {
		t.Errorf("BackoffSeconds() with base=0 = %v, want 0", got)
	}
}

// TestIdleBackoffOnlyAppliesToGroups pins "只对群聊生效".
func TestIdleBackoffOnlyAppliesToGroups(t *testing.T) {
	config := DefaultReplyTimingBackoffConfig()
	for _, testCase := range []struct {
		name    string
		isGroup bool
	}{
		{"group", true},
		{"private", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runtime := &stubBackoffRuntime{isGroup: testCase.isGroup}
			controller := NewIdleBackoffController(runtime, config, func() float64 { return 1000 }, nil)
			controller.RecordCycleResult("planner_no_tool_end")
			if testCase.isGroup {
				// count=1 < start_count=2 → no window yet (mirrors `_get_backoff_seconds`).
				if controller.Count() != 1 || controller.Until() != 0 {
					t.Fatalf("after one idle cycle: count=%d until=%v, want 1/0",
						controller.Count(), controller.Until())
				}
				controller.RecordCycleResult("planner_no_tool_end")
				if controller.Count() != 2 || controller.Until() != 1015 {
					t.Fatalf("after two idle cycles: count=%d until=%v, want 2/1015",
						controller.Count(), controller.Until())
				}
			} else {
				if controller.Count() != 0 || controller.Until() != 0 {
					t.Fatalf("private chat must reset the backoff, got count=%d until=%v",
						controller.Count(), controller.Until())
				}
			}
		})
	}
}

// TestIdleBackoffNonIdleReasonResets pins `is_idle_cycle_reason` handling.
func TestIdleBackoffNonIdleReasonResets(t *testing.T) {
	runtime := &stubBackoffRuntime{isGroup: true}
	controller := NewIdleBackoffController(runtime, DefaultReplyTimingBackoffConfig(), func() float64 { return 1000 }, nil)
	controller.RecordCycleResult("planner_no_tool_end")
	controller.RecordCycleResult("planner_no_tool_end")
	if controller.Count() != 2 {
		t.Fatalf("count = %d, want 2", controller.Count())
	}
	controller.RecordCycleResult("max_rounds")
	if controller.Count() != 0 || controller.Until() != 0 {
		t.Fatalf("non-idle reason must reset, got count=%d until=%v", controller.Count(), controller.Until())
	}

	for reason, want := range map[string]bool{
		"planner_no_tool_end":       true,
		"planner_wait_rest":         true,
		"tool_pause:wait":           true,
		"tool_stop_after_execution": true,
		" tool_pause:wait ":         true,
		"tool_continue":             false,
		"max_rounds":                false,
		"planner_interrupted":       false,
		"":                          false,
	} {
		if got := IsIdleCycleReason(reason); got != want {
			t.Errorf("IsIdleCycleReason(%q) = %v, want %v", reason, got, want)
		}
	}
}

// TestIdleBackoffShouldDelay pins the timing checks and the
// bypass_pending_count interruption.
func TestIdleBackoffShouldDelay(t *testing.T) {
	config := DefaultReplyTimingBackoffConfig()
	now := 1000.0
	runtime := &stubBackoffRuntime{isGroup: true}
	controller := NewIdleBackoffController(runtime, config, func() float64 { return now }, func(string, ...any) {})

	// No active backoff window.
	if controller.ShouldDelay(1) {
		t.Fatalf("ShouldDelay without an active window = true")
	}

	controller.RecordCycleResult("tool_pause:wait")
	controller.RecordCycleResult("planner_wait_rest")
	if controller.Until() != 1015 {
		t.Fatalf("until = %v, want 1015", controller.Until())
	}

	now = 1005
	if !controller.ShouldDelay(1) {
		t.Fatalf("ShouldDelay inside the window = false")
	}
	if len(runtime.deferCalls) != 1 || math.Abs(runtime.deferCalls[0]-10) > 1e-9 {
		t.Fatalf("defer calls = %v, want [10]", runtime.deferCalls)
	}

	// bypass_pending_count = 6 interrupts the wait without deferring.
	if controller.ShouldDelay(6) {
		t.Fatalf("pending >= bypass count must not delay")
	}
	if controller.Until() != 1015 {
		t.Fatalf("bypass must not clear the window, until=%v", controller.Until())
	}
	if len(runtime.deferCalls) != 1 {
		t.Fatalf("bypass must not call defer again, calls=%v", runtime.deferCalls)
	}

	// bypass 0 disables the interruption.
	noBypass := NewIdleBackoffController(runtime,
		ReplyTimingBackoffConfig{BaseSeconds: 15, CapSeconds: 300, StartCount: 2, BypassPendingCount: 0},
		func() float64 { return now }, nil)
	noBypass.RecordCycleResult("planner_no_tool_end")
	noBypass.RecordCycleResult("planner_no_tool_end")
	if !noBypass.ShouldDelay(1000) {
		t.Fatalf("bypass=0 must keep delaying")
	}

	// Expired window clears itself.
	now = 1016
	if controller.ShouldDelay(1) {
		t.Fatalf("expired window must not delay")
	}
	if controller.Until() != 0 {
		t.Fatalf("until = %v, want 0 after expiry", controller.Until())
	}
}

// TestIdleBackoffFocusModeResets pins the focus-mode reset of should_delay.
func TestIdleBackoffFocusModeResets(t *testing.T) {
	runtime := &stubBackoffRuntime{isGroup: true}
	controller := NewIdleBackoffController(runtime, DefaultReplyTimingBackoffConfig(), func() float64 { return 1000 }, nil)
	controller.RecordCycleResult("planner_no_tool_end")
	controller.RecordCycleResult("planner_no_tool_end")
	if controller.Count() != 2 {
		t.Fatalf("count = %d, want 2", controller.Count())
	}

	runtime.focusActive = true
	if controller.ShouldDelay(1) {
		t.Fatalf("focus mode must not delay")
	}
	if controller.Count() != 0 || controller.Until() != 0 {
		t.Fatalf("focus mode must reset the backoff state, got count=%d until=%v", controller.Count(), controller.Until())
	}
}

// TestLogFormatKeepsPlannerSource pins the log line shape used by the Python
// controller (helps diffing against the reference logs).
func TestLogFormatKeepsPlannerSource(t *testing.T) {
	lines := make([]string, 0)
	runtime := &stubBackoffRuntime{isGroup: true}
	controller := NewIdleBackoffController(runtime, DefaultReplyTimingBackoffConfig(),
		func() float64 { return 1000 }, func(format string, args ...any) {
			lines = append(lines, fmt.Sprintf(format, args...))
		})
	controller.RecordCycleResult("planner_no_tool_end")
	controller.RecordCycleResult("planner_no_tool_end")
	want := "[test] 连续空闲退避已更新: 来源=planner 连续次数=2 退避=15.00 秒"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("log lines = %v, want [%q]", lines, want)
	}
}
