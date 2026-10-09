package sender

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/database"
	"oh-my-qqbot/internal/message"
)

// fakeChannel 记录发送请求；可配置失败。
type fakeChannel struct {
	mu       sync.Mutex
	sent     []ChannelOutbound
	fail     bool
	uploads  int
	media    string
	messageN int
}

func (f *fakeChannel) SendChannelMessage(ctx context.Context, outbound ChannelOutbound) (*ChannelResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("channel down")
	}
	f.sent = append(f.sent, outbound)
	f.messageN++
	return &ChannelResult{MessageID: "ROBOT_msg_" + string(rune('0'+f.messageN)), Passive: outbound.TriggerID != ""}, nil
}

func (f *fakeChannel) UploadMedia(ctx context.Context, payload []byte, name string) (string, error) {
	if f.fail {
		return "", errors.New("upload failed")
	}
	f.uploads++
	f.media = "file_info_" + name
	return f.media, nil
}

type fakeSessions struct{ byID map[string]*SessionInfo }

func (s *fakeSessions) GetSession(sessionID string) *SessionInfo { return s.byID[sessionID] }

type fakeHistory struct {
	mu       sync.Mutex
	appended []historyEntry
}

type historyEntry struct {
	sessionID  string
	messageID  string
	sourceKind string
	visible    string
}

func (h *fakeHistory) AppendSentMessage(sessionID string, sessionMessage *message.SessionMessage, sourceKind string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.appended = append(h.appended, historyEntry{
		sessionID:  sessionID,
		messageID:  sessionMessage.MessageID,
		sourceKind: sourceKind,
		visible:    orEmpty(sessionMessage.ProcessedPlainText),
	})
}

type fakeEmojiUsage struct{ hashes []string }

func (f *fakeEmojiUsage) RecordEmojiUsage(hash string) { f.hashes = append(f.hashes, hash) }

func identityProvider(platform string) (BotIdentity, bool) {
	return BotIdentity{Platform: platform, UserID: "bot123", Nickname: "麦麦"}, true
}

func newTestService(t *testing.T, channel *fakeChannel, history *fakeHistory) (*SendService, *database.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	service := NewSendService(Options{
		Channel:     channel,
		Sessions:    &fakeSessions{byID: map[string]*SessionInfo{"s1": {SessionID: "s1", Platform: "qq", GroupID: "g1", GroupName: "测试群"}}},
		BotIdentity: identityProvider,
		Store:       dbStoreFunc(db),
		History:     history,
		TypingSpeed: 1.0,
	})
	return service, db
}

// dbStoreFunc 把 *database.DB 适配为 MessageStore（真实入库路径）。
func dbStoreFunc(db *database.DB) MessageStore {
	return dbMessageStore{db: db}
}

type dbMessageStore struct{ db *database.DB }

func (s dbMessageStore) StoreMessage(sessionMessage *message.SessionMessage) (int64, error) {
	record, err := sessionMessage.ToDatabaseRecord()
	if err != nil {
		return 0, err
	}
	return s.db.InsertMessage(&database.Message{
		MessageID:          record.MessageID,
		Timestamp:          record.Timestamp,
		Platform:           record.Platform,
		UserID:             record.UserID,
		UserNickname:       record.UserNickname,
		UserCardname:       record.UserCardname,
		GroupID:            record.GroupID,
		GroupName:          record.GroupName,
		IsMentioned:        record.IsMentioned,
		IsAt:               record.IsAt,
		SessionID:          record.SessionID,
		ReplyTo:            record.ReplyTo,
		IsEmoji:            record.IsEmoji,
		IsPicture:          record.IsPicture,
		IsCommand:          record.IsCommand,
		IsNotify:           record.IsNotify,
		RawContent:         record.RawContent,
		ProcessedPlainText: record.ProcessedPlainText,
		AdditionalConfig:   record.AdditionalConfig,
		ReplyFrequency:     record.ReplyFrequency,
	})
}

// 出站构造：send_api_<ms> id、机器人身份、群信息、组件摘要。
func TestBuildOutboundSessionMessage(t *testing.T) {
	channel := &fakeChannel{}
	service, _ := newTestService(t, channel, &fakeHistory{})
	session := &SessionInfo{SessionID: "s1", Platform: "qq", GroupID: "g1", GroupName: "测试群"}
	sequence := message.NewMessageSequence(message.NewTextComponent("你好呀"))

	outbound, err := service.BuildOutboundSessionMessage(session, sequence, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(outbound.MessageID, "send_api_") {
		t.Fatalf("message_id 应为 send_api_<ms>: %q", outbound.MessageID)
	}
	if outbound.MessageInfo.UserInfo.UserID != "bot123" || outbound.MessageInfo.UserInfo.UserNickname != "麦麦" {
		t.Fatalf("user_info 应为机器人身份: %+v", outbound.MessageInfo.UserInfo)
	}
	if outbound.MessageInfo.GroupInfo == nil || outbound.MessageInfo.GroupInfo.GroupName != "测试群" {
		t.Fatalf("群聊应带 GroupInfo: %+v", outbound.MessageInfo.GroupInfo)
	}
	if outbound.ProcessedPlainText == nil || *outbound.ProcessedPlainText != "你好呀" {
		t.Fatalf("processed_plain_text 应为组件摘要: %v", outbound.ProcessedPlainText)
	}
	if outbound.IsPicture || outbound.IsEmoji {
		t.Fatal("文本消息不应带媒体标志")
	}

	// emoji 组件 → is_emoji 标志 + [表情] 摘要。
	emojiSeq := message.NewMessageSequence(message.NewEmojiComponent([]byte("fake"), ""))
	emojiOutbound, err := service.BuildOutboundSessionMessage(session, emojiSeq, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !emojiOutbound.IsEmoji {
		t.Fatal("emoji 组件应置 is_emoji")
	}
	if *emojiOutbound.ProcessedPlainText != "[表情]" {
		t.Fatalf("空 emoji 内容摘要应为 [表情]: %q", *emojiOutbound.ProcessedPlainText)
	}

	// 未配置机器人身份时报错。
	serviceNoBot := NewSendService(Options{BotIdentity: func(string) (BotIdentity, bool) { return BotIdentity{}, false }})
	if _, err := serviceNoBot.BuildOutboundSessionMessage(session, sequence, "", nil); err == nil {
		t.Fatal("未配置机器人账号应报错")
	}
}

// 主链路：构造 → 渠道 → 存库 → 回写历史；成功回执回填平台 id。
func TestSendSessionMessageHappyPath(t *testing.T) {
	channel := &fakeChannel{}
	history := &fakeHistory{}
	service, db := newTestService(t, channel, history)
	session := &SessionInfo{SessionID: "s1", Platform: "qq", GroupID: "g1", GroupName: "测试群"}
	sequence := message.NewMessageSequence(message.NewTextComponent("第一条回复"))

	outbound, err := service.BuildOutboundSessionMessage(session, sequence, "第一条回复", nil)
	if err != nil {
		t.Fatal(err)
	}
	originalID := outbound.MessageID
	outbound.ReplyTo = message.StringPtr("target_msg_1")

	sent, err := service.SendSessionMessage(context.Background(), outbound, SendOptions{
		SetReply:             true,
		StorageMessage:       true,
		SyncToMaisakaHistory: true,
		MaisakaSourceKind:    "guided_reply",
		TriggerID:            "target_msg_1",
		TriggerTime:          time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 平台 id 回填
	if sent.MessageID == originalID || !strings.HasPrefix(sent.MessageID, "ROBOT_msg_") {
		t.Fatalf("平台消息 id 应回填: %q", sent.MessageID)
	}
	if sent.PlatformMessageID == nil || *sent.PlatformMessageID != sent.MessageID {
		t.Fatalf("platform_message_id 应记录: %v", sent.PlatformMessageID)
	}
	// 渠道收到 quote
	if len(channel.sent) != 1 {
		t.Fatalf("渠道应收到 1 条: %d", len(channel.sent))
	}
	if channel.sent[0].QuoteMsgID != "target_msg_1" || channel.sent[0].GroupID != "g1" {
		t.Fatalf("渠道参数不符: %+v", channel.sent[0])
	}
	// 存库
	record, err := db.GetMessageByID(sent.MessageID)
	if err != nil || record == nil {
		t.Fatalf("消息应入库: %v %v", record, err)
	}
	if record.SessionID != "s1" || record.ProcessedPlainText == nil || *record.ProcessedPlainText != "第一条回复" {
		t.Fatalf("入库记录不符: %+v", record)
	}
	// 回写历史 guided_reply
	if len(history.appended) != 1 {
		t.Fatalf("历史应回写 1 条: %d", len(history.appended))
	}
	entry := history.appended[0]
	if entry.sourceKind != "guided_reply" || entry.sessionID != "s1" || entry.messageID != sent.MessageID {
		t.Fatalf("历史回写不符: %+v", entry)
	}
}

// 失败即停：渠道失败返回错误，不入库、不回写历史。
func TestSendFailureStops(t *testing.T) {
	channel := &fakeChannel{fail: true}
	history := &fakeHistory{}
	service, _ := newTestService(t, channel, history)
	session := &SessionInfo{SessionID: "s1", Platform: "qq", GroupID: "g1"}
	sequence := message.NewMessageSequence(message.NewTextComponent("你好"))

	outbound, err := service.BuildOutboundSessionMessage(session, sequence, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SendSessionMessage(context.Background(), outbound, SendOptions{StorageMessage: true}); err == nil {
		t.Fatal("渠道失败应返回错误")
	}
	if len(history.appended) != 0 {
		t.Fatal("失败不应回写历史")
	}
}

// set_reply 缺 reply_message_id 报错（对齐 _prepare_message_for_platform_io）。
func TestSetReplyRequiresReplyID(t *testing.T) {
	channel := &fakeChannel{}
	service, _ := newTestService(t, channel, &fakeHistory{})
	session := &SessionInfo{SessionID: "s1", Platform: "qq", GroupID: "g1"}
	outbound, err := service.BuildOutboundSessionMessage(session, message.NewMessageSequence(message.NewTextComponent("x")), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SendSessionMessage(context.Background(), outbound, SendOptions{SetReply: true}); err == nil {
		t.Fatal("set_reply=True 无 reply_message_id 应报错")
	}
}

// 表情发送：上传媒体 → msg_type 7 载荷 → 使用记录。
func TestSendEmojiViaChannel(t *testing.T) {
	channel := &fakeChannel{}
	history := &fakeHistory{}
	service, _ := newTestService(t, channel, history)
	emojiUsage := &fakeEmojiUsage{}
	service.emojiUsage = emojiUsage
	session := &SessionInfo{SessionID: "s1", Platform: "qq", GroupID: "g1"}
	sequence := message.NewMessageSequence(message.NewEmojiComponent([]byte("emoji-bytes"), ""))
	emojiHash := sequence.Components[0].(*message.EmojiComponent).BinaryHash // 构造器按 sha256 重算

	outbound, err := service.BuildOutboundSessionMessage(session, sequence, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SendSessionMessage(context.Background(), outbound, SendOptions{StorageMessage: true}); err != nil {
		t.Fatal(err)
	}
	if channel.uploads != 1 {
		t.Fatalf("应上传媒体 1 次: %d", channel.uploads)
	}
	if channel.sent[0].Media != "file_info_"+emojiHash {
		t.Fatalf("渠道应收到 file_info: %q", channel.sent[0].Media)
	}
	if len(emojiUsage.hashes) != 1 || emojiUsage.hashes[0] != emojiHash {
		t.Fatalf("表情使用应记录: %v", emojiUsage.hashes)
	}
}

// guided_reply 历史回写：planner 前缀 + SessionBackedMessage。
type fakeSink struct {
	entries []chatcontext.LLMContextMessage
}

func (f *fakeSink) AppendChatHistory(entries ...chatcontext.LLMContextMessage) {
	f.entries = append(f.entries, entries...)
}

func TestAppendSentMessageToHistory(t *testing.T) {
	sink := &fakeSink{}
	now := time.Unix(1700000000, 0)
	sessionMessage := message.NewSessionMessage("m1", now, "qq")
	sessionMessage.MessageInfo.UserInfo = message.NewUserInfo("u1", "麦麦", "")
	sessionMessage.RawMessage = message.NewMessageSequence(message.NewTextComponent("回复内容"))
	sessionMessage.SessionID = "s1"

	if !AppendSentMessageToHistory(sink, nil, sessionMessage, "guided_reply") {
		t.Fatal("回写应成功")
	}
	if len(sink.entries) != 1 {
		t.Fatalf("应追加 1 条历史: %d", len(sink.entries))
	}
	entry, ok := sink.entries[0].(*chatcontext.SessionBackedMessage)
	if !ok {
		t.Fatalf("应为 SessionBackedMessage: %T", sink.entries[0])
	}
	if entry.SourceKind != "guided_reply" {
		t.Fatalf("sourceKind 应为 guided_reply: %q", entry.SourceKind)
	}
	// guided_reply 的可见文本不含引用组件；raw 前缀含 is_self_message。
	rawText := entry.RawMessage.Components[0].(*message.TextComponent).Text
	if !strings.Contains(rawText, "is_self_message=\"true\"") {
		t.Fatalf("guided_reply 前缀应带 is_self_message: %q", rawText)
	}
	if !strings.Contains(rawText, "user=\"麦麦\"") {
		t.Fatalf("前缀应带 user: %q", rawText)
	}
	// outbound_send 的可见文本保留引用组件（此处无引用，验证 sourceKind 差异）。
	sink2 := &fakeSink{}
	if !AppendSentMessageToHistory(sink2, nil, sessionMessage, "outbound_send") {
		t.Fatal("回写应成功")
	}
	entry2 := sink2.entries[0].(*chatcontext.SessionBackedMessage)
	if entry2.SourceKind != "outbound_send" {
		t.Fatalf("sourceKind 应为 outbound_send: %q", entry2.SourceKind)
	}
	if strings.Contains(entry2.RawMessage.Components[0].(*message.TextComponent).Text, "is_self_message") {
		t.Fatal("outbound_send 不应带 is_self_message")
	}
}

// ContractAdapter：逐段发送、失败即停、首段引用。
func TestContractAdapterSegments(t *testing.T) {
	channel := &fakeChannel{}
	history := &fakeHistory{}
	service, db := newTestService(t, channel, history)
	adapter := NewContractAdapter(service)

	results, err := adapter.Send(context.Background(), SendRequest{
		SessionID: "s1",
		ReplyTo:   "target_1",
		Contents: []OutboundSegment{
			{Type: "text", Text: "第一段"},
			{Type: "text", Text: "第二段"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].MessageID == "" || results[1].MessageID == "" {
		t.Fatalf("两段都应成功: %+v", results)
	}
	if len(channel.sent) != 2 {
		t.Fatalf("渠道应收到 2 条: %d", len(channel.sent))
	}
	// 首段带 quote，次段不带。
	if channel.sent[0].QuoteMsgID != "target_1" || channel.sent[1].QuoteMsgID != "" {
		t.Fatalf("引用语义不符: %+v", channel.sent)
	}
	// 每段都入库 + 回写 guided_reply。
	count, err := db.CountMessagesBySession("s1")
	if err != nil || count != 2 {
		t.Fatalf("两段都应入库: %d %v", count, err)
	}
	if len(history.appended) != 2 {
		t.Fatalf("两段都应回写历史: %d", len(history.appended))
	}
}

// ContractAdapter：任何一段失败即停止（第二段渠道失败）。
func TestContractAdapterStopsOnFailure(t *testing.T) {
	channel := &fakeChannel{}
	history := &fakeHistory{}
	service, db := newTestService(t, channel, history)
	adapter := NewContractAdapter(service)

	// 注入渠道在第二次发送时失败。
	failAfter := 1
	channelFail := &countingFailChannel{inner: channel, failAfter: failAfter}
	service.channel = channelFail

	results, err := adapter.Send(context.Background(), SendRequest{
		SessionID: "s1",
		Contents: []OutboundSegment{
			{Type: "text", Text: "第一段"},
			{Type: "text", Text: "第二段"},
			{Type: "text", Text: "第三段"},
		},
	})
	if err == nil {
		t.Fatal("第二段失败应返回错误")
	}
	if len(results) != 2 {
		t.Fatalf("失败即停：结果应含成功 1 段 + 失败 1 段: %d", len(results))
	}
	if results[1].Err == nil {
		t.Fatal("失败段应带 Err")
	}
	if len(history.appended) != 1 {
		t.Fatalf("失败即停：仅第一段回写历史: %d", len(history.appended))
	}
	count, _ := db.CountMessagesBySession("s1")
	if count != 1 {
		t.Fatalf("仅第一段入库: %d", count)
	}
}

type countingFailChannel struct {
	inner     *fakeChannel
	failAfter int
	calls     int
}

func (c *countingFailChannel) SendChannelMessage(ctx context.Context, outbound ChannelOutbound) (*ChannelResult, error) {
	c.calls++
	if c.calls > c.failAfter {
		return nil, errors.New("第二段渠道失败")
	}
	return c.inner.SendChannelMessage(ctx, outbound)
}

func (c *countingFailChannel) UploadMedia(ctx context.Context, payload []byte, name string) (string, error) {
	return c.inner.UploadMedia(ctx, payload, name)
}

// 打字间隔：中文 0.3s/字、英文 0.15s/字、emoji 固定 1s、单中文 3 倍。
func TestCalculateTypingTimeSeconds(t *testing.T) {
	if got := calculateTypingTimeSeconds("你好", 1.0, false); got < 0.59 || got > 0.61 {
		t.Fatalf("两个中文应约 0.6s: %v", got)
	}
	if got := calculateTypingTimeSeconds("ab", 1.0, false); got < 0.29 || got > 0.31 {
		t.Fatalf("两个英文应约 0.3s: %v", got)
	}
	if got := calculateTypingTimeSeconds("嗨", 1.0, false); got < 1.19 || got > 1.21 {
		t.Fatalf("单中文应 3 倍+0.3: %v", got)
	}
	if got := calculateTypingTimeSeconds("任意长文本", 1.0, true); got != 1 {
		t.Fatalf("emoji 应固定 1s: %v", got)
	}
	if got := calculateTypingTimeSeconds("你好", 2.0, false); got < 1.19 || got > 1.21 {
		t.Fatalf("typing_speed=2 应翻倍: %v", got)
	}
	if got := calculateTypingTimeSeconds("你好", 0, false); got != 0 {
		t.Fatalf("typing_speed<=0 应为 0: %v", got)
	}
	// DefaultSegmentDelay 输出时长。
	delay := DefaultSegmentDelay(1.0)("你好", false)
	if delay < 590*time.Millisecond || delay > 610*time.Millisecond {
		t.Fatalf("时长应约 600ms: %v", delay)
	}
}

// 编译期对齐 internal.Sender 契约（Send(ctx, contracts.SendRequest)）。
func TestContractAdapterImplementsSenderShape(t *testing.T) {
	service, _ := newTestService(t, &fakeChannel{}, &fakeHistory{})
	var _ interface {
		Send(ctx context.Context, req SendRequest) ([]SendResult, error)
	} = NewContractAdapter(service)
	// 渠道适配边界形状。
	var _ ChannelClient = &fakeChannel{}
	_ = os.Getenv
}
