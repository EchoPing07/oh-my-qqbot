package replyeffect

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"oh-my-qqbot/internal/database"
)

// DatabaseStore 落库/读取往返（mai_reply_effects 表）。
func TestDatabaseStoreRoundTrip(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	store := NewDatabaseStore(db)
	ctx := context.Background()

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)
	record := &Record{
		EffectID:          "11111111-1111-4111-8111-111111111111",
		Status:            StatusPending,
		CreatedAt:         FormatTimestamp(now),
		UpdatedAt:         FormatTimestamp(now),
		Session:           SessionSnapshot{SessionID: "g:1", SessionName: "群1", ChatType: "group"},
		Reply:             ReplySnapshot{ReplyText: "回复正文", ModelName: "m1", PromptFingerprint: "pf"},
		TargetUser:        UserSnapshot{UserID: "U1", Nickname: "小明"},
		FollowupMessages:  []FollowupSnapshot{},
		ContextSnapshot:   []ContextItem{},
		EvaluationVersion: EvaluationVersion,
		SchemaVersion:     SchemaVersion,
	}
	if err := store.Save(ctx, record); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	// 同 effect_id 二次写入应为 upsert（状态变更反复落库）。
	record.Status = StatusFinalized
	record.FinalizedAt = FormatTimestamp(now.Add(30 * time.Minute))
	confidence := 0.62
	record.Scores = &Scores{ResponseScore: 42.5, ConversationScore: 30.0, Confidence: &confidence}
	if err := store.Save(ctx, record); err != nil {
		t.Fatalf("二次 Save 失败: %v", err)
	}

	loaded, err := store.GetByID(ctx, record.EffectID)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if loaded == nil || loaded.Status != StatusFinalized || loaded.Scores == nil ||
		loaded.Scores.ResponseScore != 42.5 || loaded.Session.SessionName != "群1" {
		t.Fatalf("读取结果不符: %+v", loaded)
	}

	unfinished, err := store.LoadUnfinished(ctx, "g:1")
	if err != nil {
		t.Fatalf("LoadUnfinished 失败: %v", err)
	}
	if len(unfinished) != 0 {
		t.Fatalf("finalized 记录不应出现在未结算列表: %d", len(unfinished))
	}

	// 写一条 evaluation_failed 记录（可由 start 恢复重试）。
	failed := record.Clone()
	failed.EffectID = "22222222-2222-4222-8222-222222222222"
	failed.Status = StatusEvaluationFailed
	failed.EvaluationError = "回复效果评审连续两次校验失败：Request timed out."
	if err := store.Save(ctx, failed); err != nil {
		t.Fatal(err)
	}
	unfinished, err = store.LoadUnfinished(ctx, "g:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(unfinished) != 1 || unfinished[0].EffectID != failed.EffectID {
		t.Fatalf("可恢复记录加载不符: %+v", unfinished)
	}
	byIDs, err := store.LoadByIDs(ctx, []string{record.EffectID, failed.EffectID})
	if err != nil || len(byIDs) != 2 {
		t.Fatalf("LoadByIDs 不符: %v %d", err, len(byIDs))
	}
	count, err := store.CountBySession(ctx, "g:1")
	if err != nil || count != 2 {
		t.Fatalf("CountBySession 不符: %v %d", err, count)
	}
	listed, err := store.ListBySession(ctx, "g:1", 10)
	if err != nil || len(listed) != 2 {
		t.Fatalf("ListBySession 不符: %v %d", err, len(listed))
	}
	// 时间升序：pending→finalized 的原始记录在前。
	if listed[0].EffectID != record.EffectID {
		t.Fatalf("ListBySession 应为时间升序: %+v", listed)
	}
	if missing, err := store.GetByID(ctx, "missing"); err != nil || missing != nil {
		t.Fatalf("不存在的记录应返回 nil: %v %v", missing, err)
	}
}

// Tracker + DatabaseStore 端到端（注册 → 随访 → 评审 → 落库）。
func TestTrackerWithDatabaseStoreEndToEnd(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	store := NewDatabaseStore(db)
	clock := newFakeClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local))
	runner := &coveringRunner{}
	tracker := NewTracker(RuntimeFunc{
		SessionIDValue: testSessionID, PlatformValue: "qq", IsGroupValue: true, SessionNameValue: "测试群",
	}, TrackerConfig{
		Enabled:     true,
		BotIdentity: BotIdentity{Nickname: "麦麦"},
		JudgeRunner: runner.run,
		Store:       store,
		Clock:       clock,
	})

	record, err := tracker.RegisterReply(context.Background(), TrackReplyRequest{
		ToolCallID:       "call-1",
		TargetMessageID:  "M1",
		TargetUser:       UserSnapshot{UserID: "U1", Nickname: "小明"},
		ReplyText:        "嗯，我觉得可以",
		ReplySegments:    []string{"嗯，我觉得可以"},
		SendResults:      []map[string]any{{"index": 0, "message_id": "BOT-1", "success": true}},
		ContextSnapshot:  []ContextItem{},
		PlannerReasoning: "用户提问",
		Now:              clock.Now(),
	})
	if err != nil || record == nil {
		t.Fatalf("登记失败: %v", err)
	}
	clock.Advance(30 * time.Second)
	tracker.ObserveUserMessage(context.Background(), &ObservedMessage{
		SessionID: testSessionID, MessageID: "F1", Timestamp: clock.Now(), UserID: "U2",
		Nickname: "小红", ReplyTo: "BOT-1", QuoteTargetIDs: []string{"BOT-1"},
		PlainText: "好，听你的", VisibleText: "好，听你的",
	})
	clock.Advance(ObservationWindow)
	tracker.WaitForIdle()

	final, err := store.GetByID(context.Background(), record.EffectID)
	if err != nil || final == nil {
		t.Fatalf("读取失败: %v", err)
	}
	if final.Status != StatusFinalized {
		t.Fatalf("应结算为 finalized, got %s (err=%s)", final.Status, final.EvaluationError)
	}
	if final.Scores == nil || final.Scores.ResponseScore <= 0 {
		t.Fatalf("应产出分数: %+v", final.Scores)
	}
	if len(final.FollowupMessages) != 1 ||
		final.FollowupMessages[0].Associations[0].AttributionType != AttributionExplicitQuote {
		t.Fatalf("显式引用关联应随记录落库: %+v", final.FollowupMessages)
	}
}
