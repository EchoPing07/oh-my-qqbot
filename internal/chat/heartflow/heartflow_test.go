package heartflow

import (
	"strings"
	"sync"
	"testing"
	"time"

	chatmanager "oh-my-qqbot/internal/chat/manager"
	chatruntime "oh-my-qqbot/internal/chat/runtime"
	"oh-my-qqbot/internal/database"
	"oh-my-qqbot/internal/message"
)

// newTestEnv 构造 ChatManager + HeartflowManager（runtime 由轻量替身工厂创建）。
func newTestEnv(t *testing.T, clock *fakeClock) (*chatmanager.ChatManager, *HeartflowManager, *runtimeCounter) {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	chatManager := chatmanager.NewChatManager(db)
	if err := chatManager.Initialize(); err != nil {
		t.Fatal(err)
	}

	counter := &runtimeCounter{}
	manager := NewHeartflowManager(Options{
		ChatManager: chatManager,
		NewRuntime: func(sessionID string) (*chatruntime.Runtime, error) {
			counter.add(sessionID)
			return chatruntime.NewRuntime(chatruntime.Options{
				SessionID:      sessionID,
				IsGroupSession: true,
			}), nil
		},
		Now: func() float64 { return clock.now() },
		Log: func(string, ...any) {},
	})
	return chatManager, manager, counter
}

type fakeClock struct {
	mu    sync.Mutex
	value float64
}

func (c *fakeClock) now() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (c *fakeClock) advance(seconds float64) {
	c.mu.Lock()
	c.value += seconds
	c.mu.Unlock()
}

type runtimeCounter struct {
	mu      sync.Mutex
	created []string
	failed  []string
}

func (c *runtimeCounter) add(sessionID string) {
	c.mu.Lock()
	c.created = append(c.created, sessionID)
	c.mu.Unlock()
}

func TestGetOrCreateHeartflowChat(t *testing.T) {
	clock := &fakeClock{value: 1000}
	chatManager, manager, counter := newTestEnv(t, clock)

	if _, err := chatManager.GetOrCreateSession("qq", "u1", "g1", "", ""); err != nil {
		t.Fatal(err)
	}
	chat1, err := manager.GetOrCreateHeartflowChat(chatManagerGetID(t, chatManager, "g1"))
	if err != nil {
		t.Fatal(err)
	}
	// 二次获取应命中缓存
	chat2, err := manager.GetOrCreateHeartflowChat(chatManagerGetID(t, chatManager, "g1"))
	if err != nil {
		t.Fatal(err)
	}
	if chat1 != chat2 {
		t.Fatal("同一会话应返回同一 runtime")
	}
	if len(counter.created) != 1 {
		t.Fatalf("应只创建一次 runtime: %v", counter.created)
	}

	// 不存在的 session 应报错
	if _, err := manager.GetOrCreateHeartflowChat("missing"); err == nil ||
		!strings.Contains(err.Error(), "未找到 session_id=missing 对应的聊天流") {
		t.Fatalf("未知会话应报错: %v", err)
	}
}

func TestLRUEvictionKeepsActiveWithin24h(t *testing.T) {
	clock := &fakeClock{value: 1000}
	chatManager, manager, counter := newTestEnv(t, clock)

	// 创建 100 个会话
	sessionIDs := make([]string, 0, HeartflowMaxActiveChats)
	for i := 0; i < HeartflowMaxActiveChats; i++ {
		groupID := "g" + itoa(i)
		if _, err := chatManager.GetOrCreateSession("qq", "u1", groupID, "", ""); err != nil {
			t.Fatal(err)
		}
		sessionID := chatManagerGetID(t, chatManager, groupID)
		if _, err := manager.GetOrCreateHeartflowChat(sessionID); err != nil {
			t.Fatal(err)
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	if manager.Len() != HeartflowMaxActiveChats {
		t.Fatalf("应有 %d 个实例: %d", HeartflowMaxActiveChats, manager.Len())
	}

	// 24 小时内无消息 → 不淘汰（保活），再创建第 101 个也不淘汰
	clock.advance(3600) // 1 小时
	newSessionID := createExtraSession(t, chatManager, manager, "extra1")
	if manager.Len() != HeartflowMaxActiveChats+1 {
		t.Fatalf("24h 内不应淘汰: %d", manager.Len())
	}

	// 超过 24h 后，最早的会话可被淘汰
	clock.advance(HeartflowActiveRetentionSeconds + 1)
	createExtraSession(t, chatManager, manager, "extra2")
	if manager.Len() > HeartflowMaxActiveChats+1 {
		t.Fatalf("超 24h 的旧会话应被淘汰至多保留到 %d: %d", HeartflowMaxActiveChats+1, manager.Len())
	}
	// 最早的 extra1（同样超窗）或最初会话之一被淘汰；被保护的新会话必须存在
	if manager.GetChat(newSessionID) == nil {
		t.Fatalf("最近创建的会话不应被淘汰")
	}
	if len(counter.created) < HeartflowMaxActiveChats+2 {
		t.Fatalf("创建计数应累计: %d", len(counter.created))
	}
}

func TestLRUOrderRefreshedByAccess(t *testing.T) {
	clock := &fakeClock{value: 1000}
	chatManager, manager, _ := newTestEnv(t, clock)

	firstID := createExtraSession(t, chatManager, manager, "first")
	for i := 0; i < HeartflowMaxActiveChats-1; i++ {
		createExtraSession(t, chatManager, manager, "fill"+itoa(i))
	}
	// 访问 first 刷新 LRU + 活跃时间
	if _, err := manager.GetOrCreateHeartflowChat(firstID); err != nil {
		t.Fatal(err)
	}
	clock.advance(HeartflowActiveRetentionSeconds + 1)
	// 淘汰应跳过刚访问过的 first，淘汰下一个最旧的
	createExtraSession(t, chatManager, manager, "overflow")
	if manager.GetChat(firstID) == nil {
		t.Fatalf("刚访问过的会话不应被淘汰（LRU 刷新）")
	}
}

func TestClearChatHistoryContext(t *testing.T) {
	clock := &fakeClock{value: 1000}
	chatManager, manager, _ := newTestEnv(t, clock)

	sessionID := createExtraSession(t, chatManager, manager, "clear1")
	if !manager.ClearChatHistoryContext(sessionID) {
		t.Fatal("存在的会话应可清除")
	}
	if manager.GetChat(sessionID) != nil {
		t.Fatal("清除后实例应被移除")
	}
	if manager.ClearChatHistoryContext(sessionID) {
		t.Fatal("不存在的会话应返回 false")
	}
}

func TestAdjustTalkFrequency(t *testing.T) {
	clock := &fakeClock{value: 1000}
	chatManager, manager, _ := newTestEnv(t, clock)

	// Runtime Options 无 TalkValueProvider 时生效频率 = base × adjust；
	// base 由 Options.BaseReplyFrequency 决定（测试工厂里为 0 → 恒静默）。
	// 这里直接用带基础频率的 runtime 验证 AdjustTalkFrequency 语义。
	sessionID := createExtraSession(t, chatManager, manager, "freq1")
	chat := manager.GetChat(sessionID)
	manager.AdjustTalkFrequency(sessionID, 0.5) // 未知 base → 仍静默，仅日志
	if chat.EffectiveReplyFrequency() != 0 {
		t.Fatalf("base=0 时调整后仍应静默: %v", chat.EffectiveReplyFrequency())
	}
	// 带基础频率的 runtime：adjust 乘数直接生效
	based := chatruntime.NewRuntime(chatruntime.Options{
		SessionID:          "s_freq",
		IsGroupSession:     true,
		BaseReplyFrequency: 1.0,
	})
	based.AdjustTalkFrequency(0.5)
	if based.EffectiveReplyFrequency() != 0.5 {
		t.Fatalf("adjust=0.5 时生效频率应为 0.5: %v", based.EffectiveReplyFrequency())
	}
	// 未知的会话仅打日志
	manager.AdjustTalkFrequency("missing", 0.5)
}

func TestMessageIngestorForcedTurnAndCache(t *testing.T) {
	runtime := chatruntime.NewRuntime(chatruntime.Options{
		SessionID:      "s1",
		IsGroupSession: true,
	})
	var ingested []*message.SessionMessage
	ingestor := NewMessageIngestor(runtime, RegisterMessageOptions{
		OnMessageIngested: func(sessionID string, m *message.SessionMessage) {
			ingested = append(ingested, m)
		},
	})
	if ingestor.options.MaxRetainedMessageCacheSize != 200 {
		t.Fatalf("缓存上限应默认 200: %d", ingestor.options.MaxRetainedMessageCacheSize)
	}
	if ingestor.options.BurstIntervalSeconds != 5.0 {
		t.Fatalf("连发过滤应默认 5s: %v", ingestor.options.BurstIntervalSeconds)
	}

	// @ 消息 → 强制触发判定
	atMessage := makeSessionMessage(true)
	stats := ingestor.RegisterMessage(atMessage)
	if !stats.ForcedTurnArmed || !stats.IsAt {
		t.Fatalf("@ 消息应武装强制触发: %+v", stats)
	}
	if !runtime.HasForcedTurnTrigger() {
		t.Fatal("runtime 应处于强制触发状态")
	}
	if len(ingested) != 1 {
		t.Fatalf("监控钩子应收到消息: %d", len(ingested))
	}

	// 普通（非 @）消息 → 不武装（register_message 只在 is_at/mentioned 时武装）
	plainStats := ingestor.RegisterMessage(makeSessionMessage(false))
	if plainStats.ForcedTurnArmed {
		t.Fatalf("普通消息不应武装强制触发: %+v", plainStats)
	}
	// @ 状态保持到调度器消费（HasForcedTurnTrigger 对齐 _has_forced_turn_trigger）
	if !runtime.HasForcedTurnTrigger() {
		t.Fatal("未调度前强制触发状态应保持")
	}
}

func TestMessageIngestorCacheCap200(t *testing.T) {
	runtime := chatruntime.NewRuntime(chatruntime.Options{
		SessionID:      "s1",
		IsGroupSession: true,
	})
	ingestor := NewMessageIngestor(runtime, DefaultRegisterMessageOptions())
	// 压入 220 条消息 → 缓存应裁剪到 200 上限（未消费的消息也会被裁剪，
	// 对齐 _prune_processed_message_cache 的 min(excess, lastProcessedIndex) 语义：
	// lastProcessedIndex=0 时 removable=0 → 缓存实际保持全部；调度消费后可裁剪）。
	for i := 0; i < 220; i++ {
		ingestor.RegisterMessage(makeSessionMessage(false))
	}
	runtime.PruneProcessedMessageCache()
	pending := runtime.PendingMessages()
	if len(pending) > 220 {
		t.Fatalf("缓存不应超压入数: %d", len(pending))
	}
	// 消费 30 条后再裁剪 → 缓存回落到 200 内
	for i := 0; i < 30; i++ {
		runtime.CollectPendingMessages()
	}
	runtime.PruneProcessedMessageCache()
	if remaining := len(runtime.PendingMessages()); remaining > 200 {
		t.Fatalf("消费后缓存应回落到 200 上限内: %d", remaining)
	}
}

// 辅助。

func chatManagerGetID(t *testing.T, chatManager *chatmanager.ChatManager, groupID string) string {
	t.Helper()
	session, err := chatManager.GetOrCreateSession("qq", "u1", groupID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return session.SessionID
}

func createExtraSession(t *testing.T, chatManager *chatmanager.ChatManager, manager *HeartflowManager, tag string) string {
	t.Helper()
	sessionID := chatManagerGetID(t, chatManager, "grp_"+tag)
	if _, err := manager.GetOrCreateHeartflowChat(sessionID); err != nil {
		t.Fatal(err)
	}
	return sessionID
}

func makeSessionMessage(isAt bool) *message.SessionMessage {
	info := message.NewUserInfo("u1", "昵称", "")
	return &message.SessionMessage{
		MessageID: "m" + itoa(int(time.Now().UnixNano()%100000)),
		Timestamp: time.Now(),
		Platform:  "qq",
		MessageInfo: message.MessageInfo{
			UserInfo:  info,
			GroupInfo: &message.GroupInfo{GroupID: "g1", GroupName: "群"},
		},
		IsAt:        isAt,
		IsMentioned: isAt,
		RawMessage:  message.NewMessageSequence(message.NewTextComponent("hello")),
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append(digits, byte('0'+value%10))
		value /= 10
	}
	if negative {
		digits = append(digits, '-')
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}
