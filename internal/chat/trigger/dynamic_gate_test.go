package trigger

import (
	"math"
	"testing"
)

// TestStaticKeepRatioThresholdTable pins the 11-row offline table copied from
// `_STATIC_KEEP_RATIO_THRESHOLDS` in dynamic_gate.py.
func TestStaticKeepRatioThresholdTable(t *testing.T) {
	want := [][2]float64{
		{0.0, 1.0},
		{0.1, 0.605},
		{0.2, 0.482},
		{0.3, 0.430},
		{0.4, 0.376},
		{0.5, 0.336},
		{0.6, 0.313},
		{0.7, 0.290},
		{0.8, 0.250},
		{0.9, 0.206},
		{1.0, 0.0},
	}
	if len(StaticKeepRatioThresholds) != len(want) {
		t.Fatalf("StaticKeepRatioThresholds has %d rows, want %d", len(StaticKeepRatioThresholds), len(want))
	}
	for index, row := range want {
		got := StaticKeepRatioThresholds[index]
		if got[0] != row[0] || got[1] != row[1] {
			t.Errorf("row %d = %v, want %v", index, got, row)
		}
	}

	// Interpolated values reproduce `_static_threshold` exactly.
	interpolated := []struct {
		ratio float64
		want  float64
	}{
		{0.0, 1.0},
		{0.05, 0.8025},
		{0.1, 0.605},
		{0.15, 0.5435},
		{0.2, 0.482},
		{0.25, 0.456},
		{0.3, 0.43},
		{0.336, 0.41056},
		{0.4, 0.376},
		{0.5, 0.336},
		{0.6, 0.313},
		{0.7, 0.29},
		{0.75, 0.27},
		{0.8, 0.25},
		{0.9, 0.206},
		{0.95, 0.1030000000000001},
		{1.0, 0.0},
		{1.5, 0.0},
	}
	for _, testCase := range interpolated {
		got := StaticThreshold(testCase.ratio)
		if math.Abs(got-testCase.want) > 1e-12 {
			t.Errorf("StaticThreshold(%v) = %.17g, want %.17g", testCase.ratio, got, testCase.want)
		}
	}
}

func TestDynamicGateConstantsArePinned(t *testing.T) {
	if ReplyWindowSeconds != 3600.0 {
		t.Errorf("ReplyWindowSeconds = %v, want 3600", ReplyWindowSeconds)
	}
	if ForcedTurnExpectedReplies != 0.91 {
		t.Errorf("ForcedTurnExpectedReplies = %v, want 0.91", ForcedTurnExpectedReplies)
	}
	if MinWindowScoreCount != 20 {
		t.Errorf("MinWindowScoreCount = %d, want 20", MinWindowScoreCount)
	}
	if VirtualRoundSeconds != 40.0 {
		t.Errorf("VirtualRoundSeconds = %v, want 40", VirtualRoundSeconds)
	}
	if MaxCountedMessageIDs != 2000 {
		t.Errorf("MaxCountedMessageIDs = %d, want 2000", MaxCountedMessageIDs)
	}
}

// TestDynamicGateEvaluatePinnedDecisions reproduces the Python decision sequence
// used when the offline table was fitted.
func TestDynamicGateEvaluatePinnedDecisions(t *testing.T) {
	gate := NewDynamicReplyGate(ReplyWindowSeconds)
	const at = 1000.0
	gate.RecordProactiveDemand(0.30, at)

	first := gate.Evaluate(0.30, 0.5, at)
	assertDecision(t, "first", first, false, 0.30, 0.336, 0.30, 0.15, 0, 0.5)

	gate.RecordReply(at + 1)
	second := gate.Evaluate(0.30, 0.5, at+2)
	assertDecision(t, "after_one_reply", second, false, 0.30, 0.70375, 0.30, 0.15, 1, 0.075)

	gate.RecordForcedTurn(at + 3)
	third := gate.Evaluate(0.30, 0.5, at+4)
	assertDecision(t, "after_forced_turn", third, false, 0.30, 1.0, 1.21, 0.605, 1, 0.0)

	// Everything older than the 3600s window is pruned; only the forced
	// expectation survives (recorded at at+3).
	fourth := gate.Evaluate(0.30, 0.5, at+3601)
	assertDecision(t, "window_expired", fourth, false, 0.30, 0.336, 0.91, 0.455, 1, 0.5)
}

func assertDecision(
	t *testing.T,
	name string,
	got DynamicGateDecision,
	wantTrigger bool,
	wantProbability float64,
	wantThreshold float64,
	wantExpected float64,
	wantTarget float64,
	wantActual int,
	wantKeepRatio float64,
) {
	t.Helper()
	if got.ShouldTrigger != wantTrigger ||
		math.Abs(got.Probability-wantProbability) > 1e-12 ||
		math.Abs(got.Threshold-wantThreshold) > 1e-12 ||
		math.Abs(got.ExpectedReplies-wantExpected) > 1e-12 ||
		math.Abs(got.TargetReplies-wantTarget) > 1e-12 ||
		got.ActualReplies != wantActual ||
		math.Abs(got.KeepRatio-wantKeepRatio) > 1e-12 {
		t.Fatalf("%s = %+v (detail=%s), want {trigger:%v p:%v thr:%v expected:%v target:%v actual:%d keep:%v}",
			name, got, got.Detail(), wantTrigger, wantProbability, wantThreshold, wantExpected, wantTarget, wantActual, wantKeepRatio)
	}
}

// TestVirtualRoundOverwritesScore pins `record_proactive_demand`: inside an open
// round the score is replaced, at/after 40s a new round starts.
func TestVirtualRoundOverwritesScore(t *testing.T) {
	gate := NewDynamicReplyGate(ReplyWindowSeconds)
	gate.RecordProactiveDemand(0.2, 100.0)
	gate.RecordProactiveDemand(0.9, 130.0)
	if scores := gate.ProactiveScores(); len(scores) != 1 || scores[0] != 0.9 {
		t.Fatalf("scores inside open round = %v, want [0.9]", scores)
	}
	if !gate.IsRoundOpen(131.0) {
		t.Fatalf("IsRoundOpen(131) = false, want true")
	}
	if gate.IsRoundOpen(140.0) {
		t.Fatalf("IsRoundOpen(140) = true, want false (>= VIRTUAL_ROUND_SECONDS)")
	}
	gate.RecordProactiveDemand(0.55, 140.0)
	scores := gate.ProactiveScores()
	if len(scores) != 2 || scores[0] != 0.9 || scores[1] != 0.55 {
		t.Fatalf("scores after new round = %v, want [0.9 0.55]", scores)
	}
}

// TestThresholdUsesSortedQuantileWhenWindowIsFull exercises the >=20-sample path.
func TestThresholdUsesSortedQuantileWhenWindowIsFull(t *testing.T) {
	gate := NewDynamicReplyGate(ReplyWindowSeconds)
	for index := 0; index < 20; index++ {
		gate.RecordProactiveDemand(0.01*float64(index+1), 100.0+float64(index)*100)
		gate.CloseRound()
	}
	full := gate.Evaluate(0.5, 0.5, 3000.0)
	if math.Abs(full.Threshold-0.15) > 1e-12 {
		t.Errorf("threshold with 20 samples & keep_ratio 0.5 = %v, want 0.15", full.Threshold)
	}
	half := gate.Evaluate(0.5, 0.25, 3000.0)
	if math.Abs(half.Threshold-0.18) > 1e-12 {
		t.Errorf("threshold with 20 samples & keep_ratio 0.25 = %v, want 0.18", half.Threshold)
	}
}

// TestStaticFallbackBelowSampleThreshold exercises the offline table branch.
func TestStaticFallbackBelowSampleThreshold(t *testing.T) {
	gate := NewDynamicReplyGate(ReplyWindowSeconds)
	for index, score := range []float64{0.9, 0.8, 0.7, 0.6, 0.5} {
		gate.RecordProactiveDemand(score, 100.0+float64(index)*100)
		gate.CloseRound()
	}
	decision := gate.Evaluate(0.5, 0.5, 1000.0)
	if math.Abs(decision.ExpectedReplies-3.5) > 1e-12 {
		t.Errorf("expected_replies = %v, want 3.5", decision.ExpectedReplies)
	}
	if math.Abs(decision.TargetReplies-1.75) > 1e-12 {
		t.Errorf("target_replies = %v, want 1.75", decision.TargetReplies)
	}
	if math.Abs(decision.Threshold-0.336) > 1e-12 {
		t.Errorf("threshold = %v, want static 0.336", decision.Threshold)
	}
	if !decision.ShouldTrigger {
		t.Errorf("should_trigger = false, want true (0.5 >= 0.336)")
	}
}

// TestFrequencyOneOrZeroShortCircuits mirrors the `normalized_frequency >= 1.0`
// and `expected_proactive <= 0` branches.
func TestFrequencyOneOrZeroShortCircuits(t *testing.T) {
	gate := NewDynamicReplyGate(ReplyWindowSeconds)
	gate.RecordProactiveDemand(0.2, 100.0)
	always := gate.Evaluate(0.9, 1.0, 100.0)
	if always.Threshold != 0.0 || always.KeepRatio != 1.0 || !always.ShouldTrigger {
		t.Errorf("frequency=1 decision = %+v, want keep_ratio 1 / threshold 0 / trigger", always)
	}

	empty := NewDynamicReplyGate(ReplyWindowSeconds)
	gateDecision := empty.Evaluate(0.9, 0.4, 100.0)
	if math.Abs(gateDecision.KeepRatio-0.4) > 1e-12 {
		t.Errorf("keep_ratio without proactive scores = %v, want 0.4", gateDecision.KeepRatio)
	}
	if math.Abs(gateDecision.Threshold-0.376) > 1e-12 {
		t.Errorf("threshold without proactive scores = %v, want static 0.376 (keep_ratio 0.4)", gateDecision.Threshold)
	}
}

func TestTakeUncountedMessageIDsDeduplicatesAndTrims(t *testing.T) {
	gate := NewDynamicReplyGate(ReplyWindowSeconds)
	first := gate.TakeUncountedMessageIDs([]string{"a", "b", "a", ""})
	if len(first) != 2 || first[0] != "a" || first[1] != "b" {
		t.Fatalf("first = %v, want [a b]", first)
	}
	second := gate.TakeUncountedMessageIDs([]string{"b", "c"})
	if len(second) != 1 || second[0] != "c" {
		t.Fatalf("second = %v, want [c]", second)
	}

	// Overflow: only the newest MaxCountedMessageIDs ids stay remembered.
	overflow := NewDynamicReplyGate(ReplyWindowSeconds)
	ids := make([]string, 0, MaxCountedMessageIDs+10)
	for index := 0; index < MaxCountedMessageIDs+10; index++ {
		ids = append(ids, "id-"+itoa(index))
	}
	overflow.TakeUncountedMessageIDs(ids)
	if len(overflow.countedMessageOrder) != MaxCountedMessageIDs {
		t.Fatalf("retained ids = %d, want %d", len(overflow.countedMessageOrder), MaxCountedMessageIDs)
	}
	if again := overflow.TakeUncountedMessageIDs([]string{"id-0"}); len(again) != 1 {
		t.Fatalf("oldest id should have been forgotten, got %v", again)
	}
}
