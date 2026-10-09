package replyeffect

import (
	"context"
	"testing"
	"time"
)

// Manager 门面：RegisterReply / EnqueueFollowUp 触发点接口 + 异步随访队列。
func TestManagerRegisterAndEnqueueFollowUp(t *testing.T) {
	runner := &coveringRunner{}
	store := NewMemoryStore()
	clock := newFakeClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local))
	manager := NewManager(ManagerConfig{
		Tracker: TrackerConfig{
			Enabled:     true,
			BotIdentity: BotIdentity{Nickname: "麦麦"},
			JudgeRunner: runner.run,
			Store:       store,
			Clock:       clock,
		},
		RuntimeFor: func(sessionID string) Runtime {
			return RuntimeFunc{
				SessionIDValue:   sessionID,
				SessionNameValue: "群-" + sessionID,
				PlatformValue:    "qq",
				IsGroupValue:     true,
			}
		},
	})

	effectID, err := manager.RegisterReply(context.Background(), testSessionID, testBotReply, "M2")
	if err != nil {
		t.Fatalf("RegisterReply 失败: %v", err)
	}
	if len(effectID) != 36 {
		t.Fatalf("应返回 36 字符 effect_id, got %q", effectID)
	}

	manager.EnqueueFollowUp(&ObservedMessage{
		SessionID:   testSessionID,
		MessageID:   "F1",
		Timestamp:   clock.Now().Add(10 * time.Second),
		UserID:      "U1",
		Nickname:    "小明",
		PlainText:   "这条应该被异步记录",
		VisibleText: "这条应该被异步记录",
	})
	manager.Drain()

	records := manager.Records(context.Background(), testSessionID)
	if len(records) != 1 {
		t.Fatalf("应有 1 条记录, got %d", len(records))
	}
	if len(records[0].FollowupMessages) != 1 || records[0].FollowupMessages[0].MessageID != "F1" {
		t.Fatalf("随访消息未写入: %+v", records[0].FollowupMessages)
	}

	// 关闭：结算未完成记录（runtime_stop → incomplete）。
	manager.Close(context.Background())
	stored, err := store.GetByID(context.Background(), effectID)
	if err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	if stored.Status != StatusIncomplete {
		t.Fatalf("关闭后应结算为 incomplete, got %s", stored.Status)
	}
}

func TestManagerDisabled(t *testing.T) {
	manager := NewManager(ManagerConfig{Tracker: TrackerConfig{Enabled: false}})
	effectID, err := manager.RegisterReply(context.Background(), testSessionID, "hi", "M1")
	if err != nil || effectID != "" {
		t.Fatalf("禁用时 RegisterReply 应为 no-op: %q %v", effectID, err)
	}
	manager.EnqueueFollowUp(&ObservedMessage{SessionID: testSessionID, MessageID: "F1"})
	manager.Drain()
	if records := manager.Records(context.Background(), testSessionID); len(records) != 0 {
		t.Fatal("禁用时不应产生记录")
	}
	manager.Close(context.Background())
}

// 多会话隔离：不同 session 的随访互不影响。
func TestManagerKeepsSessionsSeparate(t *testing.T) {
	manager := NewManager(ManagerConfig{
		Tracker: TrackerConfig{
			Enabled:     true,
			JudgeRunner: (&coveringRunner{}).run,
			Clock:       newFakeClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)),
		},
	})
	for _, sessionID := range []string{"s1", "s2"} {
		if _, err := manager.RegisterReply(context.Background(), sessionID, "回复"+sessionID, "M1"); err != nil {
			t.Fatal(err)
		}
		manager.EnqueueFollowUp(&ObservedMessage{
			SessionID: sessionID, MessageID: "F1", UserID: "U1",
			PlainText: "消息" + sessionID, VisibleText: "消息" + sessionID,
		})
	}
	manager.Drain()
	for _, sessionID := range []string{"s1", "s2"} {
		records := manager.Records(context.Background(), sessionID)
		if len(records) != 1 {
			t.Fatalf("会话 %s 应有 1 条记录, got %d", sessionID, len(records))
		}
		if len(records[0].FollowupMessages) != 1 ||
			records[0].FollowupMessages[0].PlainText != "消息"+sessionID {
			t.Fatalf("会话 %s 随访串味: %+v", sessionID, records[0].FollowupMessages)
		}
	}
	manager.Close(context.Background())
}
