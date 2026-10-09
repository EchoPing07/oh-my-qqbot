package trigger

import (
	"math"
	"testing"
)

// TestSpecConstants pins every constant of REWRITE_PLAN §6.3 in one table so a
// single failure names the drifted value (the Python sources are authoritative:
// 参考/MaiBot/src/maisaka/turn_trigger/*.py).
func TestSpecConstants(t *testing.T) {
	floats := []struct {
		name string
		got  float64
		want float64
	}{
		{"reply_likelihood._INTERCEPT", Intercept, -1.2005},
		{"reply_likelihood._WEIGHT_MENTION_BOT", WeightMentionBot, 1.5852},
		{"reply_likelihood._WEIGHT_AT_OTHER", WeightAtOther, -1.1157},
		{"reply_likelihood._WEIGHT_QUESTION_MARK", WeightQuestionMark, 0.4807},
		{"reply_likelihood._WEIGHT_PLACEHOLDER_ONLY", WeightPlaceholderOnly, -1.0281},
		{"reply_likelihood._WEIGHT_RECENT_SELF_RATIO", WeightRecentSelfRatio, 2.6525},
		{"reply_likelihood._WEIGHT_RECENT_MESSAGE_COUNT", WeightRecentMessageCount, -0.0367},
		{"reply_likelihood._WEIGHT_LOG_SECONDS_SINCE_BOT", WeightLogSecondsSinceBot, 0.0571},
		{"reply_likelihood._WEIGHT_PENDING_COUNT", WeightPendingCount, 0.1012},
		{"reply_likelihood.DEFAULT_SECONDS_SINCE_BOT_MESSAGE", DefaultSecondsSinceBotMessage, 3600.0},
		{"dynamic_gate.REPLY_WINDOW_SECONDS", ReplyWindowSeconds, 3600.0},
		{"dynamic_gate.FORCED_TURN_EXPECTED_REPLIES", ForcedTurnExpectedReplies, 0.91},
		{"dynamic_gate.VIRTUAL_ROUND_SECONDS", VirtualRoundSeconds, 40.0},
		{"gates.RECENT_PRESENCE_WINDOW_SECONDS", RecentPresenceWindowSeconds, 300.0},
	}
	for _, testCase := range floats {
		if math.Abs(testCase.got-testCase.want) > 1e-12 {
			t.Errorf("%s = %v, want %v", testCase.name, testCase.got, testCase.want)
		}
	}

	ints := []struct {
		name string
		got  int
		want int
	}{
		{"reply_likelihood.MAX_PENDING_COUNT", MaxPendingCount, 20},
		{"dynamic_gate.MIN_WINDOW_SCORE_COUNT", MinWindowScoreCount, 20},
		{"dynamic_gate.MAX_COUNTED_MESSAGE_IDS", MaxCountedMessageIDs, 2000},
		{"_STATIC_KEEP_RATIO_THRESHOLDS rows", len(StaticKeepRatioThresholds), 11},
	}
	for _, testCase := range ints {
		if testCase.got != testCase.want {
			t.Errorf("%s = %d, want %d", testCase.name, testCase.got, testCase.want)
		}
	}

	// chat.reply_timing defaults (official_configs.ChatReplyTimingConfig).
	backoff := DefaultReplyTimingBackoffConfig()
	if backoff.BaseSeconds != 15 || backoff.CapSeconds != 300 || backoff.StartCount != 2 ||
		backoff.BypassPendingCount != 6 {
		t.Errorf("no_action_backoff defaults = %+v, want base=15 cap=300 start=2 bypass=6", backoff)
	}

	// mode_policy.IDLE_CYCLE_REASONS.
	wantIdleReasons := []string{
		"planner_no_tool_end", "planner_wait_rest", "tool_pause:wait", "tool_stop_after_execution",
	}
	if len(IdleCycleReasons) != len(wantIdleReasons) {
		t.Fatalf("IdleCycleReasons = %v, want %v", IdleCycleReasons, wantIdleReasons)
	}
	for _, reason := range wantIdleReasons {
		if !IdleCycleReasons[reason] {
			t.Errorf("IdleCycleReasons[%q] = false, want true", reason)
		}
	}

	// TurnGateDecision literals.
	if TurnGateTrigger != "trigger" || TurnGateWait != "wait" || TurnGateDelay != "delay" {
		t.Errorf("TurnGateDecision literals changed: %q/%q/%q", TurnGateTrigger, TurnGateWait, TurnGateDelay)
	}
	// State literals shared with the runtime.
	if StateRunning != "running" || StateWait != "wait" || StateStop != "stop" {
		t.Errorf("state literals changed: %q/%q/%q", StateRunning, StateWait, StateStop)
	}
}
