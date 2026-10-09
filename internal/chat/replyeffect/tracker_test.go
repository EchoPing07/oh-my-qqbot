package replyeffect

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// 测试辅助：构造固定 LLM 评审输出的假 runner 与可注入假时钟的 tracker。

const (
	testSessionID = "g:test-session"
	testBotReply  = "我觉得这个方案可以，先按你的思路试试。"
)

// fixedJudgeRunner 返回固定 JSON，并记录收到的 prompt。
type fixedJudgeRunner struct {
	responses []string
	prompts   []string
	index     int
}

func (r *fixedJudgeRunner) run(_ context.Context, prompt string) (string, error) {
	r.prompts = append(r.prompts, prompt)
	if r.index >= len(r.responses) {
		return "", fmt.Errorf("judge runner 无更多响应")
	}
	response := r.responses[r.index]
	r.index++
	return response, nil
}

// lifecycleJudgeResponse 是生命周期测试用的评审输出：F1 保持显式引用关联（锁定的
// explicit_quote 必须被重新给出），F2 由 LLM 建立语义关联。
func lifecycleJudgeResponse() string {
	payload := map[string]any{
		"strategy": map[string]any{"primary": "answer", "secondary": []any{"opinion"}, "confidence": 0.8},
		"messages": []any{
			judgeMessage("F1", judgeAssociation("c1", 1.0, "neutral", "maintain", 0.6)),
			judgeMessage("F2", judgeAssociation("c1", 0.9, "appreciation", "advance", 0.7)),
		},
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

// judgeMessage 构造一条评审消息载荷（无关联时 associations 为空数组）。
func judgeMessage(messageID string, associations ...map[string]any) map[string]any {
	if associations == nil {
		associations = []map[string]any{}
	}
	return map[string]any{"message_id": messageID, "associations": associations}
}

// judgeAssociation 构造一条评审关联载荷。
func judgeAssociation(candidateID string, attributionConfidence float64, stance, contribution string, confidence float64) map[string]any {
	return map[string]any{
		"candidate_id":           candidateID,
		"attribution_confidence": attributionConfidence,
		"stance_target":          "bot_content",
		"stance":                 stance,
		"contribution":           contribution,
		"reason":                 "用户接着 Bot 的发言继续讨论",
		"evidence_spans":         []any{"证据片段"},
		"confidence":             confidence,
	}
}

// coveringRunner 是「总是合法」的评审替身：从 prompt 中读出全部后续消息 id，
// 逐条返回空关联（满足 judge 的逐条覆盖契约）。
type coveringRunner struct {
	prompts   []string
	responses []string
}

// followupIDPattern 匹配 judge prompt 中的 `message_id=<id>`。
var followupIDPattern = regexp.MustCompile(`message_id=(\S+)`)

// confirmedCandidatePattern 匹配 prompt 中锁定的显式引用关联 `已确认关联=[c1]`。
var confirmedCandidatePattern = regexp.MustCompile(`已确认关联=\[([c0-9 ]+)\]`)

func (r *coveringRunner) run(_ context.Context, prompt string) (string, error) {
	r.prompts = append(r.prompts, prompt)
	seen := map[string]bool{}
	ids := make([]string, 0)
	associations := map[string][]map[string]any{}
	for _, line := range strings.Split(prompt, "\n") {
		match := followupIDPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		messageID := match[1]
		if !seen[messageID] {
			seen[messageID] = true
			ids = append(ids, messageID)
		}
		if confirmed := confirmedCandidatePattern.FindStringSubmatch(line); confirmed != nil {
			for _, alias := range strings.Fields(confirmed[1]) {
				associations[messageID] = append(associations[messageID],
					judgeAssociation(alias, 1.0, "neutral", "maintain", 0.6))
			}
		}
	}
	response := buildJudgment(ids, associations)
	r.responses = append(r.responses, response)
	return response, nil
}

// associatedJudgeResponse 构造「一条消息承接 Bot 回复 + 其余消息无关联」的合法评审 JSON。
func associatedJudgeResponse(messageID string, candidateID string, otherMessageIDs ...string) string {
	messages := []map[string]any{
		{
			"message_id": messageID,
			"associations": []any{map[string]any{
				"candidate_id":           candidateID,
				"attribution_confidence": 0.9,
				"stance_target":          "bot_content",
				"stance":                 "appreciation",
				"contribution":           "advance",
				"reason":                 "用户明确接着 Bot 的建议继续讨论",
				"evidence_spans":         []any{"那按你说的试试"},
				"confidence":             0.7,
			}},
		},
	}
	for _, other := range otherMessageIDs {
		messages = append(messages, map[string]any{"message_id": other, "associations": []any{}})
	}
	payload := map[string]any{
		"strategy": map[string]any{"primary": "answer", "secondary": []any{"opinion"}, "confidence": 0.8},
		"messages": messages,
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

func newTestTracker(runner JudgeRunner) (*Tracker, *fakeClock, *MemoryStore) {
	clock := newFakeClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local))
	store := NewMemoryStore()
	tracker := NewTracker(
		RuntimeFunc{
			SessionIDValue:   testSessionID,
			SessionNameValue: "测试群",
			PlatformValue:    "qq",
			IsGroupValue:     true,
			GroupIDValue:     "G1",
		},
		TrackerConfig{
			Enabled:     true,
			BotIdentity: BotIdentity{Nickname: "麦麦", AliasNames: []string{"麦穗"}},
			JudgeRunner: runner,
			Store:       store,
			Clock:       clock,
		},
	)
	return tracker, clock, store
}

// contextSnapshotAt 构造 Bot 回复前的上下文快照（两条用户消息 + 一条 assistant）。
func contextSnapshotAt(now time.Time) []ContextItem {
	return []ContextItem{
		{
			MessageID: "M1",
			Source:    "user",
			Role:      "user",
			Timestamp: FormatTimestamp(now.Add(-90 * time.Second)),
			Text:      "大家看这个方案怎么样",
			Sender:    &ContextSender{UserID: "U1", Nickname: "小明", DisplayName: "小明"},
		},
		{
			MessageID: "M2",
			Source:    "user",
			Role:      "user",
			Timestamp: FormatTimestamp(now.Add(-30 * time.Second)),
			Text:      "我倾向先做小范围验证",
			Sender:    &ContextSender{UserID: "U2", Nickname: "小红", DisplayName: "小红"},
		},
		{
			MessageID: "M3",
			Source:    "assistant",
			Role:      "assistant",
			Timestamp: FormatTimestamp(now.Add(-15 * time.Second)),
			Text:      "（内部推理，不应计入活动量统计）",
		},
	}
}

func registerTestReply(t *testing.T, tracker *Tracker, clock *fakeClock) *Record {
	t.Helper()
	now := clock.Now()
	record, err := tracker.RegisterReply(context.Background(), TrackReplyRequest{
		ToolCallID:       "call-1",
		TargetMessageID:  "M2",
		TargetUser:       UserSnapshot{UserID: "U2", Nickname: "小红"},
		SetQuote:         true,
		ReplyText:        testBotReply,
		ReplySegments:    []string{"我觉得这个方案可以，", "先按你的思路试试。"},
		PlannerReasoning: "用户征求方案意见",
		SendResults: []map[string]any{
			{"index": 0, "message_id": "BOT-1", "set_quote": true, "success": true},
			{"index": 1, "message_id": "BOT-2", "set_quote": false, "success": true},
		},
		ContextSnapshot: snapshotWithoutAssistantRole(contextSnapshotAt(now)),
		Now:             now,
	})
	if err != nil {
		t.Fatalf("登记回复效果失败: %v", err)
	}
	if record == nil {
		t.Fatal("Enabled=true 时 RegisterReply 不应返回 nil")
	}
	return record
}

// snapshotWithoutAssistantRole 去掉 assistant role 的条目（用户消息才会进入快照）。
func snapshotWithoutAssistantRole(items []ContextItem) []ContextItem {
	out := make([]ContextItem, 0, len(items))
	for _, item := range items {
		if item.Role == "assistant" {
			continue
		}
		out = append(out, item)
	}
	return out
}

// TestReplyEffectLifecycle 覆盖任务书要求的核心状态机：
// 注册 → 随访 → 评审 → 落库。
func TestReplyEffectLifecycle(t *testing.T) {
	runner := &fixedJudgeRunner{responses: []string{lifecycleJudgeResponse()}}
	tracker, clock, store := newTestTracker(runner.run)
	record := registerTestReply(t, tracker, clock)

	if len(record.EffectID) != 36 {
		t.Fatalf("effect_id 应为 36 字符 UUID, got %q", record.EffectID)
	}
	if record.Status != StatusPending {
		t.Fatalf("登记后状态应为 pending, got %s", record.Status)
	}
	if record.Session.SessionID != testSessionID || record.Session.ChatType != "group" {
		t.Fatalf("会话快照不符: %+v", record.Session)
	}
	if len(record.Reply.SentMessageIDs) != 2 || record.Reply.SentMessageIDs[0] != "BOT-1" {
		t.Fatalf("sent_message_ids 不符: %v", record.Reply.SentMessageIDs)
	}
	// 快照中有两条用户消息（assistant 被排除），且都在 2 分钟窗口内。
	if record.PreActivityCount != 2 || record.PreActivityBucket != "medium" {
		t.Fatalf("pre_activity 统计不符: count=%d bucket=%s", record.PreActivityCount, record.PreActivityBucket)
	}
	if tracker.PendingCount() != 1 {
		t.Fatalf("观察窗口内应有 1 条 pending, got %d", tracker.PendingCount())
	}

	// 随访 1：普通消息，显式引用 Bot 已发出消息 BOT-2（应锁定 explicit_quote 关联）。
	clock.Advance(20 * time.Second)
	tracker.ObserveUserMessage(context.Background(), &ObservedMessage{
		SessionID:      testSessionID,
		MessageID:      "F1",
		Timestamp:      clock.Now(),
		UserID:         "U2",
		Nickname:       "小红",
		VisibleText:    "我先按这个试试",
		PlainText:      "我先按这个试试",
		ReplyTo:        "BOT-2",
		QuoteTargetIDs: []string{"BOT-2"},
	})
	// 随访 2：LLM 将给这条消息建立语义关联。
	clock.Advance(40 * time.Second)
	tracker.ObserveUserMessage(context.Background(), &ObservedMessage{
		SessionID:   testSessionID,
		MessageID:   "F2",
		Timestamp:   clock.Now(),
		UserID:      "U3",
		Nickname:    "小刚",
		VisibleText: "那按你说的试试，效果不错",
		PlainText:   "那按你说的试试，效果不错",
	})

	stored, err := store.GetByID(context.Background(), record.EffectID)
	if err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	if len(stored.FollowupMessages) != 2 {
		t.Fatalf("应有 2 条随访消息, got %d", len(stored.FollowupMessages))
	}
	first := stored.FollowupMessages[0]
	if len(first.Associations) != 1 ||
		first.Associations[0].AttributionType != AttributionExplicitQuote ||
		first.Associations[0].EffectID != record.EffectID {
		t.Fatalf("显式引用关联未锁定: %+v", first.Associations)
	}
	if len(first.CandidateEffectIDs) != 1 || first.CandidateEffectIDs[0] != record.EffectID {
		t.Fatalf("候选集合不符: %v", first.CandidateEffectIDs)
	}
	if first.IsTargetUser != true {
		t.Fatal("F1 来自目标用户，is_target_user 应为 true")
	}

	// 观察窗口到期 → 触发评审。
	clock.Advance(ObservationWindow)
	tracker.WaitForIdle()

	final, err := store.GetByID(context.Background(), record.EffectID)
	if err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	if final.Status != StatusFinalized {
		t.Fatalf("评审后状态应为 finalized, got %s (err=%s)", final.Status, final.EvaluationError)
	}
	if final.FinalizeReason != ReasonWindowTimeout {
		t.Fatalf("结束原因应为 window_timeout, got %s", final.FinalizeReason)
	}
	if final.Reply.StrategyPrimary != "answer" || final.Reply.StrategyConfidence != 0.8 {
		t.Fatalf("策略回填不符: %s %.2f", final.Reply.StrategyPrimary, final.Reply.StrategyConfidence)
	}
	if len(final.Reply.StrategySecondary) != 1 || final.Reply.StrategySecondary[0] != "opinion" {
		t.Fatalf("策略 secondary 不符: %v", final.Reply.StrategySecondary)
	}
	if final.Scores == nil {
		t.Fatal("finalized 记录必须有 scores")
	}
	if final.Scores.ResponseScore <= 0 {
		t.Fatalf("回应度应大于 0, got %v", final.Scores.ResponseScore)
	}
	if len(final.Scores.ReceptionCategories) != 2 ||
		final.Scores.ReceptionCategories[0] != "appreciation" ||
		final.Scores.ReceptionCategories[1] != "neutral" {
		t.Fatalf("情绪分类不符: %v", final.Scores.ReceptionCategories)
	}
	if final.Scores.Confidence == nil || *final.Scores.Confidence < 0.35 {
		t.Fatalf("置信度不符: %v", final.Scores.Confidence)
	}
	if final.FollowupSummary.TotalCount != 2 || final.FollowupSummary.AssociatedCount != 2 ||
		final.FollowupSummary.ParticipantCount != 2 {
		t.Fatalf("随访摘要不符: %+v", final.FollowupSummary)
	}
	if final.ConfidenceNote != "已完成完整观察窗口与语义归因。" {
		t.Fatalf("置信度文案不符: %s", final.ConfidenceNote)
	}
	if final.EvaluationVersion != EvaluationVersion || final.SchemaVersion != SchemaVersion {
		t.Fatalf("版本字段不符: %+v", final)
	}
	if len(runner.prompts) != 1 {
		t.Fatalf("应只调用一次评审, got %d", len(runner.prompts))
	}
	if !strings.Contains(runner.prompts[0], "麦麦、麦穗") {
		t.Fatalf("评审 prompt 应包含 Bot 身份，got: %s", runner.prompts[0])
	}
	if tracker.PendingCount() != 0 {
		t.Fatalf("评审后不应残留 pending, got %d", tracker.PendingCount())
	}
	// 显式引用关联在评审合并后仍保持 explicit_quote（同一 effect 只保留一条关联，
	// 与 Python _apply_associations 的 effect_id 合并语义一致）。
	merged := final.FollowupMessages[0].Associations
	if len(merged) != 1 || merged[0].AttributionType != AttributionExplicitQuote ||
		merged[0].AttributionConfidence != 1.0 {
		t.Fatalf("评审后 F1 的显式引用关联应保持锁定, got %+v", merged)
	}
}

// TestFollowupLimitTriggersEvaluation 覆盖 SESSION_FOLLOWUP_LIMIT 结算路径。
func TestFollowupLimitTriggersEvaluation(t *testing.T) {
	runner := &coveringRunner{}
	tracker, clock, store := newTestTracker(runner.run)
	record := registerTestReply(t, tracker, clock)

	for index := 0; index < SessionFollowupLimit; index++ {
		clock.Advance(time.Second)
		messageID := fmt.Sprintf("FU%d", index)
		tracker.ObserveUserMessage(context.Background(), &ObservedMessage{
			SessionID:   testSessionID,
			MessageID:   messageID,
			Timestamp:   clock.Now(),
			UserID:      "U9",
			Nickname:    "路人",
			VisibleText: "随便说点什么",
			PlainText:   "随便说点什么",
		})
	}
	tracker.WaitForIdle()

	final, err := store.GetByID(context.Background(), record.EffectID)
	if err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	if final.FinalizeReason != ReasonFollowupsLimit {
		t.Fatalf("结束原因应为 session_followups_limit, got %s", final.FinalizeReason)
	}
	if final.Status != StatusFinalized {
		t.Fatalf("状态应为 finalized, got %s (err=%s)", final.Status, final.EvaluationError)
	}
	if len(final.FollowupMessages) != SessionFollowupLimit {
		t.Fatalf("随访条数应为 %d, got %d", SessionFollowupLimit, len(final.FollowupMessages))
	}
	if final.Scores == nil || final.Scores.ResponseScore != 0 {
		t.Fatalf("无关联时应给出 0 分回应度: %+v", final.Scores)
	}
	if final.Scores.Confidence != nil {
		t.Fatalf("无关联时置信度应为 null, got %v", *final.Scores.Confidence)
	}
}

// TestIncompleteOnRuntimeStop 覆盖非完整观察窗口的结算（runtime_stop）。
func TestIncompleteOnRuntimeStop(t *testing.T) {
	runner := &fixedJudgeRunner{}
	tracker, clock, store := newTestTracker(runner.run)
	record := registerTestReply(t, tracker, clock)

	tracker.FinalizeAll(context.Background(), "")
	final, err := store.GetByID(context.Background(), record.EffectID)
	if err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	if final.Status != StatusIncomplete {
		t.Fatalf("runtime_stop 应结算为 incomplete, got %s", final.Status)
	}
	if final.Scores != nil {
		t.Fatalf("incomplete 不应有 scores: %+v", final.Scores)
	}
	if final.ConfidenceNote != "观察窗口不完整，未进行评分。" {
		t.Fatalf("置信度文案不符: %s", final.ConfidenceNote)
	}
	if len(runner.prompts) != 0 {
		t.Fatal("incomplete 不应触发 LLM 评审")
	}
	if tracker.PendingCount() != 0 {
		t.Fatal("结算后不应残留 pending")
	}
}

// TestEvaluationFailureAndRetry 覆盖「连续两次校验失败」路径与重试 prompt 附言。
func TestEvaluationFailureAndRetry(t *testing.T) {
	runner := &fixedJudgeRunner{responses: []string{
		`{"strategy": {"primary": "not_a_strategy"}, "messages": []}`,
		`{"strategy": {"primary": "answer", "secondary": [], "confidence": 0.5}, "messages": []}`,
	}}
	tracker, clock, store := newTestTracker(runner.run)
	record := registerTestReply(t, tracker, clock)

	clock.Advance(time.Minute)
	tracker.ObserveUserMessage(context.Background(), &ObservedMessage{
		SessionID:   testSessionID,
		MessageID:   "F1",
		Timestamp:   clock.Now(),
		UserID:      "U1",
		Nickname:    "小明",
		VisibleText: "让我看看",
		PlainText:   "让我看看",
	})
	clock.Advance(ObservationWindow)
	tracker.WaitForIdle()

	final, err := store.GetByID(context.Background(), record.EffectID)
	if err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	if final.Status != StatusEvaluationFailed {
		t.Fatalf("两次校验失败应记为 evaluation_failed, got %s", final.Status)
	}
	if !strings.Contains(final.EvaluationError, "回复效果评审连续两次校验失败") {
		t.Fatalf("错误文案不符: %s", final.EvaluationError)
	}
	if final.ConfidenceNote != "评审输出校验失败，本记录未参与策略统计。" {
		t.Fatalf("置信度文案不符: %s", final.ConfidenceNote)
	}
	if len(runner.prompts) != 2 {
		t.Fatalf("应重试一次, got %d 次调用", len(runner.prompts))
	}
	if !strings.Contains(runner.prompts[1], "上一次输出校验失败") {
		t.Fatal("第二次 prompt 应携带校验错误附言")
	}
}

// TestJudgeValidationRetrySucceeds 覆盖「第一次校验失败、第二次成功」路径。
func TestJudgeValidationRetrySucceeds(t *testing.T) {
	runner := &fixedJudgeRunner{responses: []string{
		`{"strategy": {"primary": "answer", "secondary": [], "confidence": 0.5}, "messages": []}`,
		buildEmptyJudgment([]string{"F1"}),
	}}
	tracker, clock, store := newTestTracker(runner.run)
	record := registerTestReply(t, tracker, clock)
	clock.Advance(time.Minute)
	tracker.ObserveUserMessage(context.Background(), &ObservedMessage{
		SessionID: testSessionID, MessageID: "F1", Timestamp: clock.Now(),
		UserID: "U1", Nickname: "小明", PlainText: "让我看看", VisibleText: "让我看看",
	})
	clock.Advance(ObservationWindow)
	tracker.WaitForIdle()
	final, _ := store.GetByID(context.Background(), record.EffectID)
	if final.Status != StatusFinalized {
		t.Fatalf("重试成功后应为 finalized, got %s (err=%s)", final.Status, final.EvaluationError)
	}
	if len(runner.prompts) != 2 {
		t.Fatalf("应有两次调用, got %d", len(runner.prompts))
	}
}

// TestJudgeRunnerErrorPropagates 覆盖 LLM 调用本身失败的路径。
func TestJudgeRunnerErrorPropagates(t *testing.T) {
	runner := &fixedJudgeRunner{responses: []string{}}
	tracker, clock, store := newTestTracker(runner.run)
	record := registerTestReply(t, tracker, clock)
	tracker.Finalize(context.Background(), record.EffectID, ReasonWindowTimeout)
	tracker.WaitForIdle()

	final, _ := store.GetByID(context.Background(), record.EffectID)
	if final.Status != StatusEvaluationFailed {
		t.Fatalf("runner 报错应记为 evaluation_failed, got %s", final.Status)
	}
	if final.EvaluationError == "" {
		t.Fatal("应记录 evaluation_error")
	}
}

// TestDisabledTrackerIsNoop 覆盖实验性开关默认关。
func TestDisabledTrackerIsNoop(t *testing.T) {
	store := NewMemoryStore()
	tracker := NewTracker(RuntimeFunc{SessionIDValue: testSessionID}, TrackerConfig{
		Enabled: false,
		Store:   store,
	})
	record, err := tracker.RegisterReply(context.Background(), TrackReplyRequest{TargetMessageID: "M1"})
	if err != nil || record != nil {
		t.Fatalf("禁用时 RegisterReply 应为 no-op: record=%v err=%v", record, err)
	}
	tracker.ObserveUserMessage(context.Background(), &ObservedMessage{SessionID: testSessionID, MessageID: "F1"})
	tracker.FinalizeAll(context.Background(), "")
	if len(store.All()) != 0 {
		t.Fatal("禁用时不应写入任何记录")
	}
}

// TestObserveOtherSessionIgnored 覆盖会话隔离。
func TestObserveOtherSessionIgnored(t *testing.T) {
	runner := &coveringRunner{}
	tracker, clock, store := newTestTracker(runner.run)
	record := registerTestReply(t, tracker, clock)
	tracker.ObserveUserMessage(context.Background(), &ObservedMessage{
		SessionID: "g:other-session",
		MessageID: "F1",
		Timestamp: clock.Now(),
		UserID:    "U1",
		PlainText: "别的会话的消息",
	})
	stored, _ := store.GetByID(context.Background(), record.EffectID)
	if len(stored.FollowupMessages) != 0 {
		t.Fatal("其他会话的消息不应写入本会话记录")
	}
}

// TestRestartRestoresUnfinished 覆盖 start 的恢复语义（pending 记录重建计时器）。
func TestRestartRestoresUnfinished(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local))
	store := NewMemoryStore()
	firstRunner := &coveringRunner{}
	first := NewTracker(RuntimeFunc{SessionIDValue: testSessionID}, TrackerConfig{
		Enabled: true, Store: store, Clock: clock, JudgeRunner: firstRunner.run,
	})
	record := registerTestReply(t, first, clock)

	// 新 tracker 复用同一 store（模拟进程内重建），恢复 pending 记录。
	runner := &coveringRunner{}
	second := NewTracker(RuntimeFunc{SessionIDValue: testSessionID}, TrackerConfig{
		Enabled: true, Store: store, Clock: clock, JudgeRunner: runner.run,
	})
	second.Start(context.Background())
	if second.PendingCount() != 1 {
		t.Fatalf("恢复后应有 1 条 pending, got %d", second.PendingCount())
	}
	clock.Advance(ObservationWindow)
	second.WaitForIdle()
	final, _ := store.GetByID(context.Background(), record.EffectID)
	if final.Status != StatusFinalized {
		t.Fatalf("恢复记录应可结算, got %s", final.Status)
	}
	if len(runner.prompts) == 0 {
		t.Fatal("恢复后的 timer 应触发评审")
	}
}

// TestRestartEvaluatingRecordRecovered 覆盖启动时评估中记录的重新评审
// （Python 语义：沿用原 finalize_reason，非完整窗口原因则结算为 incomplete）。
func TestRestartEvaluatingRecordRecovered(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local))
	store := NewMemoryStore()
	stuck := &Record{
		EffectID:          "11111111-2222-4333-8444-555555555555",
		Status:            StatusEvaluating,
		FinalizeReason:    ReasonWindowTimeout,
		CreatedAt:         FormatTimestamp(clock.Now()),
		UpdatedAt:         FormatTimestamp(clock.Now()),
		Session:           SessionSnapshot{SessionID: testSessionID, ChatType: "group"},
		SchemaVersion:     SchemaVersion,
		EvaluationVersion: EvaluationVersion,
	}
	if err := store.Save(context.Background(), stuck); err != nil {
		t.Fatal(err)
	}
	runner := &coveringRunner{}
	tracker := NewTracker(RuntimeFunc{SessionIDValue: testSessionID}, TrackerConfig{
		Enabled: true, Store: store, Clock: clock, JudgeRunner: runner.run,
	})
	tracker.Start(context.Background())
	tracker.WaitForIdle()
	final, _ := store.GetByID(context.Background(), stuck.EffectID)
	if final.Status != StatusFinalized {
		t.Fatalf("评估中记录恢复后应重新评审, got %s (err=%s)", final.Status, final.EvaluationError)
	}
	if final.FinalizeReason != ReasonWindowTimeout {
		t.Fatalf("恢复应沿用原结束原因, got %s", final.FinalizeReason)
	}
}

// TestRestartEvaluatingWithoutReasonMarkedIncomplete mirrors Python 的退化路径：
// evaluating 记录没有 finalize_reason 时（runtime_recovery）按不完整结算。
func TestRestartEvaluatingWithoutReasonMarkedIncomplete(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local))
	store := NewMemoryStore()
	stuck := &Record{
		EffectID:          "22222222-3333-4444-8555-666666666666",
		Status:            StatusEvaluating,
		CreatedAt:         FormatTimestamp(clock.Now()),
		UpdatedAt:         FormatTimestamp(clock.Now()),
		Session:           SessionSnapshot{SessionID: testSessionID, ChatType: "group"},
		SchemaVersion:     SchemaVersion,
		EvaluationVersion: EvaluationVersion,
	}
	if err := store.Save(context.Background(), stuck); err != nil {
		t.Fatal(err)
	}
	tracker := NewTracker(RuntimeFunc{SessionIDValue: testSessionID}, TrackerConfig{
		Enabled: true, Store: store, Clock: clock, JudgeRunner: (&coveringRunner{}).run,
	})
	tracker.Start(context.Background())
	tracker.WaitForIdle()
	final, _ := store.GetByID(context.Background(), stuck.EffectID)
	if final.Status != StatusIncomplete || final.FinalizeReason != ReasonRuntimeRecovery {
		t.Fatalf("无结束原因的 evaluating 记录应记为 incomplete/runtime_recovery, got %s/%s",
			final.Status, final.FinalizeReason)
	}
}

// buildEmptyJudgment 构造覆盖全部后续消息、且全部无关联的合法评审 JSON。
func buildEmptyJudgment(messageIDs []string) string {
	return buildJudgment(messageIDs, nil)
}

// buildJudgment 构造覆盖全部后续消息的合法评审 JSON（可指定每条消息的关联）。
func buildJudgment(messageIDs []string, associations map[string][]map[string]any) string {
	messages := make([]map[string]any, 0, len(messageIDs))
	for _, messageID := range messageIDs {
		items := associations[messageID]
		if items == nil {
			items = []map[string]any{}
		}
		messages = append(messages, map[string]any{"message_id": messageID, "associations": items})
	}
	payload := map[string]any{
		"strategy": map[string]any{"primary": "other", "secondary": []any{}, "confidence": 0.4},
		"messages": messages,
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}
