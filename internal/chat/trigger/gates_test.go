package trigger

import (
	"math"
	"strings"
	"testing"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// test doubles
// ---------------------------------------------------------------------------

type stubDynamicRuntime struct {
	isGroup bool
	history []chatcontext.LLMContextMessage
}

func (s *stubDynamicRuntime) IsGroupSession() bool { return s.isGroup }

func (s *stubDynamicRuntime) ChatHistory() []chatcontext.LLMContextMessage { return s.history }

type stubFrequencyRuntime struct {
	interval     float64
	hasInterval  bool
	lastExternal float64
	hasExternal  bool
	lastReceived float64
}

func (s *stubFrequencyRuntime) RecentAverageExternalMessageInterval() (float64, bool) {
	return s.interval, s.hasInterval
}

func (s *stubFrequencyRuntime) LastExternalMessageReceivedAt() (float64, bool) {
	return s.lastExternal, s.hasExternal
}

func (s *stubFrequencyRuntime) LastMessageReceivedAt() float64 { return s.lastReceived }

func testMessage(messageID string, at float64, options ...func(*message.SessionMessage)) *message.SessionMessage {
	sessionMessage := message.NewSessionMessage(messageID, timeFromUnixSeconds(at), "qq")
	sessionMessage.ProcessedPlainText = message.StringPtr("有问题吗？")
	sessionMessage.SessionID = "session"
	for _, option := range options {
		option(sessionMessage)
	}
	return sessionMessage
}


func withUser(userID string) func(*message.SessionMessage) {
	return func(m *message.SessionMessage) {
		m.MessageInfo.UserInfo = message.UserInfo{UserID: userID, UserNickname: userID}
	}
}

// ---------------------------------------------------------------------------
// FrequencyThresholdTurnGate
// ---------------------------------------------------------------------------

func TestFrequencyGateTriggersAtThreshold(t *testing.T) {
	gate := NewFrequencyThresholdTurnGate(&stubFrequencyRuntime{}, nil)
	result := gate.Evaluate(3, 3)
	if !result.ShouldTrigger() {
		t.Fatalf("result = %+v, want trigger", result)
	}
	if result.Detail != "pending=3 达到阈值=3 判定=进入Planner" {
		t.Errorf("detail = %q", result.Detail)
	}
}

// TestFrequencyGateIdleCompensation pins the empty-window compensation maths of
// `_calculate_idle_compensation` (the cap is trigger_threshold - 1).
func TestFrequencyGateIdleCompensation(t *testing.T) {
	cases := []struct {
		name          string
		pending       int
		threshold     int
		now           float64
		interval      float64
		hasInterval   bool
		lastExternal  float64
		hasExternal   bool
		lastReceived  float64
		wantDecision  TurnGateDecision
		wantDelay     *float64
		wantDetailHas string
	}{
		{
			name: "pure_silence_never_triggers", pending: 0, threshold: 4, now: 1000,
			interval: 60, hasInterval: true, lastExternal: 0, hasExternal: true,
			wantDecision: TurnGateWait, wantDetailHas: "pending=0，不允许纯沉默触发",
		},
		{
			name: "no_interval_sample", pending: 2, threshold: 4, now: 1000,
			hasInterval: false, hasExternal: true,
			wantDecision: TurnGateWait, wantDetailHas: "平均消息间隔不可用，无法进行空窗补偿",
		},
		{
			name: "compensation_not_reached_delays", pending: 2, threshold: 4, now: 1000,
			interval: 60, hasInterval: true, lastExternal: 900, hasExternal: true,
			wantDecision: TurnGateDelay, wantDelay: floatPtr(20),
			wantDetailHas: "平均间隔=60.00s 空窗=100.00s 空窗折算=1.67 等效消息数=3.67/4 延迟=20.00s",
		},
		{
			name: "compensation_capped_at_threshold_minus_one", pending: 2, threshold: 4, now: 1000,
			interval: 60, hasInterval: true, lastExternal: 800, hasExternal: true,
			wantDecision:  TurnGateTrigger,
			wantDetailHas: "平均间隔=60.00s 空窗=200.00s 空窗折算=3.00 等效消息数=5.00/4",
		},
		{
			name: "falls_back_to_last_message_received_at", pending: 1, threshold: 2, now: 1000,
			interval: 40, hasInterval: true, hasExternal: false, lastReceived: 980,
			wantDecision: TurnGateDelay, wantDelay: floatPtr(20),
			wantDetailHas: "平均间隔=40.00s 空窗=20.00s 空窗折算=0.50 等效消息数=1.50/2 延迟=20.00s",
		},
		{
			name: "threshold_one_with_pending_one", pending: 1, threshold: 1, now: 1000,
			interval: 40, hasInterval: true, lastExternal: 1000, hasExternal: true,
			wantDecision: TurnGateTrigger, wantDetailHas: "达到阈值=1",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runtime := &stubFrequencyRuntime{
				interval:     testCase.interval,
				hasInterval:  testCase.hasInterval,
				lastExternal: testCase.lastExternal,
				hasExternal:  testCase.hasExternal,
				lastReceived: testCase.lastReceived,
			}
			gate := NewFrequencyThresholdTurnGate(runtime, func() float64 { return testCase.now })
			result := gate.Evaluate(testCase.pending, testCase.threshold)
			if result.Decision != testCase.wantDecision {
				t.Fatalf("decision = %q (%s), want %q", result.Decision, result.Detail, testCase.wantDecision)
			}
			if testCase.wantDelay != nil {
				if result.DelaySeconds == nil {
					t.Fatalf("delay = nil, want %v", *testCase.wantDelay)
				}
				if math.Abs(*result.DelaySeconds-*testCase.wantDelay) > 1e-9 {
					t.Fatalf("delay = %v, want %v", *result.DelaySeconds, *testCase.wantDelay)
				}
			}
			if !strings.Contains(result.Detail, testCase.wantDetailHas) {
				t.Fatalf("detail = %q, want substring %q", result.Detail, testCase.wantDetailHas)
			}
		})
	}
}

func floatPtr(value float64) *float64 { return &value }

// ---------------------------------------------------------------------------
// DynamicReplyTurnGate
// ---------------------------------------------------------------------------

func TestDynamicGatePrivateChatAlwaysTriggers(t *testing.T) {
	gate := NewDynamicReplyTurnGate(&stubDynamicRuntime{isGroup: false}, func() float64 { return 1000 }, nil)
	result := gate.Evaluate([]*message.SessionMessage{testMessage("m1", 999)}, 0.5)
	if !result.ShouldTrigger() {
		t.Fatalf("result = %+v, want trigger", result)
	}
	if result.Detail != "私聊不设动态门控 判定=进入Planner" {
		t.Errorf("detail = %q", result.Detail)
	}
}

func TestDynamicGateWithoutExternalMessagesWaits(t *testing.T) {
	runtime := &stubDynamicRuntime{isGroup: true}
	gate := NewDynamicReplyTurnGate(runtime, func() float64 { return 1000 }, func(platform, userID string) bool {
		return userID == "bot"
	})
	result := gate.Evaluate([]*message.SessionMessage{testMessage("m1", 999, withUser("bot"))}, 0.5)
	if result.Decision != TurnGateWait {
		t.Fatalf("result = %+v, want wait", result)
	}
	if result.Detail != "没有新的外部消息 判定=等待更多消息" {
		t.Errorf("detail = %q", result.Detail)
	}
}

// TestLikelihoodInputRecentPresence mirrors
// test_recent_presence_excludes_expired_pending_but_keeps_batch_size.
func TestLikelihoodInputRecentPresence(t *testing.T) {
	const now = 10_000.0
	pending := make([]*message.SessionMessage, 0, 122)
	for index := 0; index < 120; index++ {
		pending = append(pending, testMessage("m"+itoa(index), now-1800))
	}
	pending = append(pending, testMessage("boundary", now-300), testMessage("recent", now-1))

	gate := NewDynamicReplyTurnGate(&stubDynamicRuntime{isGroup: true}, func() float64 { return now }, nil)
	snapshot := gate.buildLikelihoodInput(pending, now, pending)
	if snapshot.RecentMessageCount != 2 {
		t.Errorf("recent_message_count = %d, want 2", snapshot.RecentMessageCount)
	}
	if snapshot.PendingCount != 122 {
		t.Errorf("pending_count = %d, want 122", snapshot.PendingCount)
	}
	if !snapshot.HasQuestionMark {
		t.Errorf("has_question_mark = false, want true")
	}
}

// TestLikelihoodInputSharesRecentPresenceAcrossRounds mirrors
// test_round_and_release_share_recent_presence_and_self_ratio.
func TestLikelihoodInputSharesRecentPresenceAcrossRounds(t *testing.T) {
	const now = 10_000.0
	history := []chatcontext.LLMContextMessage{
		chatcontext.NewSessionBackedMessage(nil, "[u]a", timeFromUnixSeconds(now-400), "h1", nil, chatcontext.SourceUser),
		chatcontext.NewSessionBackedMessage(nil, "[u]b", timeFromUnixSeconds(now-20), "h2", nil, chatcontext.SourceUser),
		chatcontext.NewSessionBackedMessage(nil, "[b]c", timeFromUnixSeconds(now-10), "h3", nil, chatcontext.SourceGuidedReply),
	}
	pending := make([]*message.SessionMessage, 0, 11)
	for index := 0; index < 10; index++ {
		pending = append(pending, testMessage("m"+itoa(index), now-30))
	}
	pending = append(pending, testMessage("expired", now-301))

	gate := NewDynamicReplyTurnGate(&stubDynamicRuntime{isGroup: true, history: history}, func() float64 { return now }, nil)
	roundSnapshot := gate.buildLikelihoodInput(pending[len(pending)-2:len(pending)-1], now, pending)
	fullSnapshot := gate.buildLikelihoodInput(pending, now, pending)

	if roundSnapshot.RecentMessageCount != 12 || fullSnapshot.RecentMessageCount != 12 {
		t.Errorf("recent_message_count = %d/%d, want 12/12",
			roundSnapshot.RecentMessageCount, fullSnapshot.RecentMessageCount)
	}
	wantRatio := 1.0 / 12.0
	if math.Abs(roundSnapshot.RecentSelfRatio-wantRatio) > 1e-12 ||
		math.Abs(fullSnapshot.RecentSelfRatio-wantRatio) > 1e-12 {
		t.Errorf("recent_self_ratio = %v/%v, want %v",
			roundSnapshot.RecentSelfRatio, fullSnapshot.RecentSelfRatio, wantRatio)
	}
	if roundSnapshot.PendingCount != 1 || fullSnapshot.PendingCount != 11 {
		t.Errorf("pending_count = %d/%d, want 1/11", roundSnapshot.PendingCount, fullSnapshot.PendingCount)
	}
	if math.Abs(roundSnapshot.SecondsSinceBotMessage-10) > 1e-9 {
		t.Errorf("seconds_since_bot = %v, want 10", roundSnapshot.SecondsSinceBotMessage)
	}
}

// TestSecondsSinceBotFallsBackToDefault mirrors DEFAULT_SECONDS_SINCE_BOT_MESSAGE.
func TestSecondsSinceBotFallsBackToDefault(t *testing.T) {
	const now = 10_000.0
	history := []chatcontext.LLMContextMessage{
		chatcontext.NewSessionBackedMessage(nil, "[u]a", timeFromUnixSeconds(now-5000), "h1", nil, chatcontext.SourceUser),
	}
	gate := NewDynamicReplyTurnGate(&stubDynamicRuntime{isGroup: true, history: history}, func() float64 { return now }, nil)
	snapshot := gate.buildLikelihoodInput(nil, now, nil)
	if snapshot.SecondsSinceBotMessage != DefaultSecondsSinceBotMessage {
		t.Errorf("seconds_since_bot = %v, want %v", snapshot.SecondsSinceBotMessage, DefaultSecondsSinceBotMessage)
	}
}

// TestDynamicGateHotBacklogRecovers mirrors
// test_hot_backlog_recovers_when_messages_leave_recent_window.
func TestDynamicGateHotBacklogRecovers(t *testing.T) {
	now := 10_000.0
	pending := make([]*message.SessionMessage, 0, 121)
	for index := 0; index < 120; index++ {
		pending = append(pending, testMessage("m"+itoa(index), now))
	}
	gate := NewDynamicReplyTurnGate(&stubDynamicRuntime{isGroup: true}, func() float64 { return now }, nil)
	if gate.Evaluate(pending, 0.8).ShouldTrigger() {
		t.Fatalf("hot backlog should not trigger")
	}

	now += 301
	pending = append(pending, testMessage("new-after-quiet", now))
	if !gate.Evaluate(pending, 0.8).ShouldTrigger() {
		t.Fatalf("after the backlog left the recent window the gate should trigger")
	}
}

// TestDynamicGateRoundScoresUseWholeRecentBatch mirrors
// test_virtual_round_scores_include_other_recent_pending_messages.
func TestDynamicGateRoundScoresUseWholeRecentBatch(t *testing.T) {
	now := 10_000.0
	pending := make([]*message.SessionMessage, 0, 123)
	for index := 0; index < 120; index++ {
		pending = append(pending, testMessage("m"+itoa(index), now))
	}
	gate := NewDynamicReplyTurnGate(&stubDynamicRuntime{isGroup: true}, func() float64 { return now }, func(platform, userID string) bool {
		return userID == "bot"
	})
	if gate.Evaluate(pending, 0.8).ShouldTrigger() {
		t.Fatalf("hot backlog should not trigger")
	}

	now += 41
	pending = append(pending, testMessage("new", now), testMessage("self", now, withUser("bot")))
	gate.Evaluate(pending, 0.8)

	external := pending[:len(pending)-1]
	expected := EstimateReplyProbability(gate.buildLikelihoodInput(external[len(external)-1:], now, external))
	scores := gate.gate.ProactiveScores()
	lastScore := scores[len(scores)-1]
	if math.Abs(lastScore-expected) > 1e-12 {
		t.Fatalf("last proactive score = %v, want %v", lastScore, expected)
	}
	snapshot := gate.buildLikelihoodInput(external, now, external)
	if snapshot.RecentMessageCount != 121 {
		t.Fatalf("recent_message_count = %d, want 121", snapshot.RecentMessageCount)
	}
}

// TestRecordForcedTurnCountsOncePerBatch mirrors `record_forced_turn`: a batch is
// only counted once thanks to the uncounted-id bookkeeping.
func TestRecordForcedTurnCountsOncePerBatch(t *testing.T) {
	now := 5_000.0
	gate := NewDynamicReplyTurnGate(&stubDynamicRuntime{isGroup: true}, func() float64 { return now }, nil)
	batch := []*message.SessionMessage{testMessage("a", now-1), testMessage("b", now-1)}
	gate.RecordForcedTurn(batch)
	gate.RecordForcedTurn(batch)
	decision := gate.gate.Evaluate(0.0, 0.5, now)
	if math.Abs(decision.ExpectedReplies-ForcedTurnExpectedReplies) > 1e-12 {
		t.Fatalf("expected forced replies = %v, want %v (counted once)",
			decision.ExpectedReplies, ForcedTurnExpectedReplies)
	}
}

// TestDynamicGateVirtualRoundScoreReuse verifies the virtual-round bookkeeping is
// refined (not duplicated) while the round is open. frequency=0 keeps keep_ratio
// at 0 so the gate cannot trigger and the round stays open.
func TestDynamicGateVirtualRoundScoreReuse(t *testing.T) {
	now := 1_000.0
	gate := NewDynamicReplyTurnGate(&stubDynamicRuntime{isGroup: true}, func() float64 { return now }, nil)
	batch := []*message.SessionMessage{testMessage("a", now-1), testMessage("b", now-1)}
	gate.Evaluate(batch, 0.0)
	firstScores := append([]float64{}, gate.gate.ProactiveScores()...)

	now += 10
	batch = append(batch, testMessage("c", now-1))
	gate.Evaluate(batch, 0.0)
	secondScores := gate.gate.ProactiveScores()
	if len(firstScores) != 1 || len(secondScores) != 1 {
		t.Fatalf("scores = %v / %v, want a single virtual-round score", firstScores, secondScores)
	}
	if math.Abs(secondScores[0]-firstScores[0]) < 1e-15 {
		t.Fatalf("score should be refreshed when new messages join the open round")
	}
}

// TestDynamicGateRoundRestartsAfterCloseRound verifies that after a closed round
// a new score row is appended instead of overwriting the previous one.
func TestDynamicGateRoundRestartsAfterCloseRound(t *testing.T) {
	now := 1_000.0
	gate := NewDynamicReplyTurnGate(&stubDynamicRuntime{isGroup: true}, func() float64 { return now }, nil)
	gate.Evaluate([]*message.SessionMessage{testMessage("a", now-1)}, 0.0)
	gate.gate.CloseRound()
	now += 5
	gate.Evaluate([]*message.SessionMessage{testMessage("b", now-1)}, 0.0)
	if scores := gate.gate.ProactiveScores(); len(scores) != 2 {
		t.Fatalf("scores = %v, want two rows after an explicit close_round", scores)
	}
}

// TestDynamicGateClosesRoundOnTrigger verifies `close_round()` on a trigger.
func TestDynamicGateClosesRoundOnTrigger(t *testing.T) {
	now := 1_000.0
	gate := NewDynamicReplyTurnGate(&stubDynamicRuntime{isGroup: true}, func() float64 { return now }, nil)
	gate.gate.RecordProactiveDemand(0.99, now)
	if !gate.Evaluate([]*message.SessionMessage{testMessage("a", now-1)}, 0.5).ShouldTrigger() {
		t.Fatalf("high probability should trigger")
	}
	if gate.gate.IsRoundOpen(now) {
		t.Fatalf("trigger must close the virtual round")
	}
}

// TestDynamicGateDetailFormat pins the log line shape of DynamicGateDecision.detail.
func TestDynamicGateDetailFormat(t *testing.T) {
	decision := DynamicGateDecision{
		Probability: 0.87, Threshold: 0.31, ExpectedReplies: 12.4,
		TargetReplies: 6.2, ActualReplies: 3, KeepRatio: 0.5,
	}
	want := "概率=0.87 阈值=0.31 预计回复=12.4 目标=6.2 已回复=3 保留比例=0.50"
	if decision.Detail() != want {
		t.Fatalf("detail = %q, want %q", decision.Detail(), want)
	}
}
