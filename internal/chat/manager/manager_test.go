package manager

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"oh-my-qqbot/internal/database"
	"oh-my-qqbot/internal/message"
)

func newTestManager(t *testing.T) (*ChatManager, *database.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m := NewChatManager(db)
	if err := m.Initialize(); err != nil {
		t.Fatal(err)
	}
	return m, db
}

func makeTestMessage(platform string, userID string, groupID string) *message.SessionMessage {
	info := message.NewUserInfo(userID, "昵称_"+userID, "卡片_"+userID)
	sessionMessage := &message.SessionMessage{
		MessageID: "msg_" + userID + "_" + groupID,
		Timestamp: time.Unix(1700000000, 0),
		Platform:  platform,
		MessageInfo: message.MessageInfo{
			UserInfo: info,
		},
		RawMessage: message.NewMessageSequence(message.NewTextComponent("你好")),
	}
	if groupID != "" {
		sessionMessage.MessageInfo.GroupInfo = &message.GroupInfo{GroupID: groupID, GroupName: "测试群"}
	} else {
		plain := "你好"
		sessionMessage.ProcessedPlainText = &plain
	}
	return sessionMessage
}

func TestGetOrCreateSessionGroupAndPrivate(t *testing.T) {
	m, _ := newTestManager(t)

	groupSession, err := m.GetOrCreateSession("qq", "u1", "g1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !groupSession.IsGroupSession() {
		t.Fatal("group_id 存在应为群聊")
	}
	privateSession, err := m.GetOrCreateSession("qq", "u1", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if privateSession.IsGroupSession() {
		t.Fatal("无 group_id 应为私聊")
	}
	// session_id 必须不同（群/私分流）
	if groupSession.SessionID == privateSession.SessionID {
		t.Fatalf("群/私 session_id 不应相同: %s", groupSession.SessionID)
	}
	// 二次获取应命中内存并刷新活跃时间
	again, err := m.GetOrCreateSession("qq", "u1", "g1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if again.SessionID != groupSession.SessionID {
		t.Fatalf("应返回同一会话: %s vs %s", again.SessionID, groupSession.SessionID)
	}
}

func TestSessionPersistenceAcrossInstances(t *testing.T) {
	m, _ := newTestManager(t)
	session, err := m.GetOrCreateSession("qq", "u1", "g1", "acc1", "scope1")
	if err != nil {
		t.Fatal(err)
	}
	if session.AccountID == nil || *session.AccountID != "acc1" {
		t.Fatalf("account_id 应记录: %v", session.AccountID)
	}

	// 新实例从 DB 加载
	m2 := NewChatManager(m.db)
	if err := m2.Initialize(); err != nil {
		t.Fatal(err)
	}
	loaded := m2.GetSessionBySessionID(session.SessionID)
	if loaded == nil {
		t.Fatalf("新实例应能从数据库加载会话")
	}
	if loaded.GroupID == nil || *loaded.GroupID != "g1" {
		t.Fatalf("加载的会话应保留 group_id: %v", loaded.GroupID)
	}
}

func TestRegisterMessageSetsSessionIDAndUpdatesIdentity(t *testing.T) {
	m, _ := newTestManager(t)
	session, err := m.GetOrCreateSession("qq", "u1", "g1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// get_or_create 阶段 user_id 允许暂存；register_message 后群聊身份应被清空
	msg := makeTestMessage("qq", "u2", "g1")
	if err := m.RegisterMessage(msg); err != nil {
		t.Fatal(err)
	}
	if msg.SessionID == "" || msg.SessionID != session.SessionID {
		t.Fatalf("register_message 应写入消息 session_id: %q vs %q", msg.SessionID, session.SessionID)
	}
	// 会话群名补齐；群聊用户身份清空（_update_session_identity 语义）
	if session.GroupName == nil || *session.GroupName != "测试群" {
		t.Fatalf("入站消息应补齐群名: %v", session.GroupName)
	}
	if session.UserID != nil {
		t.Fatalf("群聊会话不应保存最近发言人 user_id: %v", session.UserID)
	}
	// last_messages 应命中
	latest := m.GetSessionBySessionID(session.SessionID)
	if latest == nil || latest.Context == nil || latest.Context.Message != msg {
		t.Fatalf("get_session_by_session_id 应附带最近消息上下文")
	}
}

func TestPrivateSessionIdentityUpdate(t *testing.T) {
	m, _ := newTestManager(t)
	session, err := m.GetOrCreateSession("qq", "u1", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	msg := makeTestMessage("qq", "u1", "")
	msg.MessageInfo.UserInfo.UserNickname = "新昵称"
	if err := m.RegisterMessage(msg); err != nil {
		t.Fatal(err)
	}
	if session.UserNickname == nil || *session.UserNickname != "新昵称" {
		t.Fatalf("私聊身份应随入站消息更新: %v", session.UserNickname)
	}
}

func TestStoreMessageToDB(t *testing.T) {
	m, db := newTestManager(t)
	session, err := m.GetOrCreateSession("qq", "u1", "g1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m.SetReplyFrequencyProvider(func(sessionID string, isGroupChat bool) float64 { return 0.5 })

	msg := makeTestMessage("qq", "u1", "g1")
	if err := m.RegisterMessage(msg); err != nil {
		t.Fatal(err)
	}
	id, err := m.StoreMessageToDB(msg)
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("插入应返回自增 id: %d", id)
	}

	// 读回验证
	records, err := db.QueryMessagesBySession(session.SessionID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("应查到 1 条消息: %d", len(records))
	}
	record := records[0]
	if record.UserID != "u1" || record.MessageID != msg.MessageID {
		t.Fatalf("读回字段不匹配: %+v", record)
	}
	if record.GroupID == nil || *record.GroupID != "g1" {
		t.Fatalf("group_id 应入库: %v", record.GroupID)
	}
	if record.ReplyFrequency == nil || *record.ReplyFrequency != 0.5 {
		t.Fatalf("reply_frequency 应在入库前补充: %v", record.ReplyFrequency)
	}
	if !strings.Contains(record.RawContent, "你好") {
		t.Fatalf("raw_content 应为 JSON 序列化的 MessageSequence: %q", record.RawContent)
	}
}

func TestGetSessionName(t *testing.T) {
	m, _ := newTestManager(t)
	groupSession, err := m.GetOrCreateSession("qq", "u1", "g1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// 无 context/群名时返回空（对齐 Python get_session_name）
	if name := m.GetSessionName(groupSession.SessionID); name != "" {
		t.Fatalf("无群名/无上下文时应返回空: %q", name)
	}
	if err := m.RegisterMessage(makeTestMessage("qq", "u1", "g1")); err != nil {
		t.Fatal(err)
	}
	if name := m.GetSessionName(groupSession.SessionID); name != "测试群" {
		t.Fatalf("群聊会话名应取群名: %q", name)
	}
	privateSession, err := m.GetOrCreateSession("qq", "u9", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	privateSession.UserNickname = stringPtr("小九")
	if name := m.GetSessionName(privateSession.SessionID); name != "小九的私聊" {
		t.Fatalf("私聊会话名应按昵称生成: %q", name)
	}

	// T7 回归：昵称缺失时用 user_id 兜底，不再输出「的私聊」残缺名。
	anonymousSession, err := m.GetOrCreateSession("qq", "10086", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if name := m.GetSessionName(anonymousSession.SessionID); name != "10086的私聊" {
		t.Fatalf("昵称缺失的私聊应按 user_id 兜底: %q", name)
	}
}

func TestGetExistingSessionLoadsFromDB(t *testing.T) {
	m, _ := newTestManager(t)
	session, err := m.GetOrCreateSession("qq", "u1", "g1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// 清空内存缓存模拟重启后仅 DB 可用
	m.mu.Lock()
	m.sessions = map[string]*BotChatSession{}
	m.mu.Unlock()

	loaded := m.GetExistingSessionBySessionID(session.SessionID)
	if loaded == nil {
		t.Fatalf("get_existing_session 应从数据库加载")
	}
	if missing := m.GetExistingSessionBySessionID("  "); missing != nil {
		t.Fatalf("空 session_id 应返回 nil")
	}
}

func stringPtr(value string) *string { return &value }
