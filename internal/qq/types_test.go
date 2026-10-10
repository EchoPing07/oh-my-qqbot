package qq

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// 网关帧
// ---------------------------------------------------------------------------

// TestGatewayFrameShapes 校验各类网关帧的序列化形态 (op/d/s 与官方文档一致)。
func TestGatewayFrameShapes(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			"identify",
			mustJSON(t, IdentifyFrame("QQBot token-abc", IntentDefault, nil)),
			`{"d":{"intents":33554432,"properties":{},"shard":[0,1],"token":"QQBot token-abc"},"op":2}`,
		},
		{
			"resume",
			mustJSON(t, ResumeFrame("QQBot token-abc", "sess-1", 42, 0)),
			`{"d":{"seq":42,"session_id":"sess-1","token":"QQBot token-abc"},"op":6}`,
		},
		{
			"heartbeat-with-seq",
			mustJSON(t, HeartbeatFrame(7)),
			`{"d":7,"op":1}`,
		},
		{
			"heartbeat-no-seq",
			mustJSON(t, HeartbeatFrame(0)),
			`{"op":1}`,
		},
	}
	for _, item := range tests {
		t.Run(item.name, func(t *testing.T) {
			if item.got != item.want {
				t.Fatalf("帧形态不符\n got: %s\nwant: %s", item.got, item.want)
			}
		})
	}
}

// TestIdentifyIntentIsGroupAndC2C 校验默认订阅的意图位是 GROUP_AND_C2C_EVENT (1<<25)。
func TestIdentifyIntentIsGroupAndC2C(t *testing.T) {
	raw, err := EncodeFrame(IdentifyFrame("t", 0, []int{0, 1}))
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	var decoded struct {
		Op int `json:"op"`
		D  struct {
			Intents int   `json:"intents"`
			Shard   []int `json:"shard"`
		} `json:"d"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if decoded.Op != OpIdentify {
		t.Fatalf("op 不符: got %d want %d", decoded.Op, OpIdentify)
	}
	if decoded.D.Intents != 1<<25 {
		t.Fatalf("intents 不符: got %d want %d", decoded.D.Intents, 1<<25)
	}
	if len(decoded.D.Shard) != 2 || decoded.D.Shard[0] != 0 || decoded.D.Shard[1] != 1 {
		t.Fatalf("shard 不符: %v", decoded.D.Shard)
	}
}

// TestResumePayloadShapes 校验 RESUME payload 的三种形态 (op 6 / op 13 / 带 seq 0)。
func TestResumePayloadShapes(t *testing.T) {
	tests := []struct {
		name     string
		resumeOp int
		want     string
	}{
		{"op6", OpResume, `{"d":{"seq":9,"session_id":"s","token":"QQBot tk"},"op":6}`},
		{"op13", OpResumeAlt, `{"d":{"seq":9,"session_id":"s","token":"QQBot tk"},"op":13}`},
		{"zero-seq", OpResume, `{"d":{"seq":9,"session_id":"s","token":"QQBot tk"},"op":6}`},
	}
	for _, item := range tests {
		t.Run(item.name, func(t *testing.T) {
			got := mustJSON(t, ResumeFrame("QQBot tk", "s", 9, item.resumeOp))
			if got != item.want {
				t.Fatalf("RESUME payload 不符\n got: %s\nwant: %s", got, item.want)
			}
		})
	}
}

// TestHelloFrameInterval 校验 hello 帧的心跳间隔解析 (毫秒 → Duration)。
func TestHelloFrameInterval(t *testing.T) {
	valid := GatewayPayload{Op: OpHello, Data: json.RawMessage(`{"heartbeat_interval":41250}`)}
	if got := HelloFrame(valid); got != 41250*time.Millisecond {
		t.Fatalf("心跳间隔不符: got %v want %v", got, 41250*time.Millisecond)
	}
	wrongOp := GatewayPayload{Op: OpHeartbeatAck, Data: json.RawMessage(`{"heartbeat_interval":1000}`)}
	if got := HelloFrame(wrongOp); got != 0 {
		t.Fatalf("非 hello 帧应返回 0, got %v", got)
	}
	broken := GatewayPayload{Op: OpHello, Data: json.RawMessage(`{"heartbeat_interval":"x"}`)}
	if got := HelloFrame(broken); got != 0 {
		t.Fatalf("坏帧应返回 0, got %v", got)
	}
}

// TestGatewayPayloadReceive 校验接收方向的 RawMessage 解析 (op/t/s/d 都在)。
func TestGatewayPayloadReceive(t *testing.T) {
	raw := `{"op":0,"s":12,"t":"GROUP_MESSAGE_CREATE","d":{"id":"MSG1"}}`
	var payload GatewayPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if payload.Op != OpDispatch || payload.S != 12 || payload.T != EventGroupMessageCreate {
		t.Fatalf("帧解析不符: %+v", payload)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload.Data, &body); err != nil || body.ID != "MSG1" {
		t.Fatalf("数据体解析不符: %+v err=%v", body, err)
	}
}

// ---------------------------------------------------------------------------
// 事件 payload 解析 (真实形态 JSON 样例)
// ---------------------------------------------------------------------------

// groupMessageSample 是官方 GROUP_MESSAGE_CREATE 的真实字段形态 (全量群消息)。
const groupMessageSample = `{
  "id": "08f8a1b0-2c3d-4e5f-9a6b-7c8d9e0f1a2b",
  "content": "  麦麦 帮我看看这个<faceType=6,faceId=\"0\",ext=\"eyJ0ZXh0IjoiW+iHquWItl0ifQ==\">",
  "timestamp": "2026-03-08T15:04:05+08:00",
  "author": {
    "id": "MEMBER_OPENID_1",
    "member_openid": "MEMBER_OPENID_1",
    "username": "群友A",
    "avatar": "https://q.qlogo.cn/headimg_dl?dst_uin=1",
    "bot": false
  },
  "member": {"nick": "群友A的群名片", "roles": ["1"]},
  "attachments": [
    {"content_type": "image/jpeg", "filename": "photo.jpg", "url": "https://multimedia.nt.qq.com.cn/download?appid=1&rkey=abc", "size": 102400, "width": 1920, "height": 1080}
  ],
  "group_openid": "GROUP_OPENID_1",
  "group_id": "GROUP_OPENID_1",
  "mentions": [
    {"id": "BOT_OPENID_1", "openid": "BOT_OPENID_1", "user_openid": "", "member_openid": "", "is_you": true}
  ],
  "message_type": 0,
  "message_scene": {"ext": ["msg_idx=REFIDX_IN_001", "auth_token=secret"]}
}`

// groupQuoteSample 是引用消息 (message_type=103): 被引用正文与附件在 msg_elements。
const groupQuoteSample = `{
  "id": "MSG_QUOTE_1",
  "content": "就是这句",
  "timestamp": "2026-03-08T15:10:00+08:00",
  "author": {"id": "MEMBER_OPENID_2", "member_openid": "MEMBER_OPENID_2", "username": "群友B"},
  "group_openid": "GROUP_OPENID_1",
  "mentions": [],
  "message_type": 103,
  "msg_elements": [
    {"content": "原话在这里", "attachments": [{"content_type": "image/png", "filename": "a.png", "url": "https://example.com/a.png", "size": 2048}]}
  ],
  "message_scene": {"ext": ["msg_idx=REFIDX_IN_002", "ref_msg_idx=REFIDX_OUT_001"]}
}`

// c2cSample 是单聊事件 (author.user_openid 是对端标识)。
const c2cSample = `{
  "id": "C2C_MSG_1",
  "content": "你好呀",
  "timestamp": "1773000000",
  "author": {"id": "USER_OPENID_1", "user_openid": "USER_OPENID_1", "username": "好友"},
  "mentions": [],
  "message_type": 0
}`

func decodeSample(t *testing.T, raw string) *EventPayload {
	t.Helper()
	var payload EventPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("解析样本失败: %v", err)
	}
	return &payload
}

// TestEventPayloadParse 校验群消息样本的全量字段解析。
func TestEventPayloadParse(t *testing.T) {
	payload := decodeSample(t, groupMessageSample)
	if payload.ID != "08f8a1b0-2c3d-4e5f-9a6b-7c8d9e0f1a2b" {
		t.Fatalf("id 不符: %q", payload.ID)
	}
	if payload.GroupOpenid() != "GROUP_OPENID_1" {
		t.Fatalf("group_openid 不符: %q", payload.GroupOpenid())
	}
	if payload.SenderID(EventGroupMessageCreate) != "MEMBER_OPENID_1" {
		t.Fatalf("sender 不符: %q", payload.SenderID(EventGroupMessageCreate))
	}
	if payload.SenderName() != "群友A的群名片" {
		t.Fatalf("sender 显示名应优先取群名片: %q", payload.SenderName())
	}
	if payload.MsgIdx() != "REFIDX_IN_001" {
		t.Fatalf("msg_idx 不符: %q", payload.MsgIdx())
	}
	if payload.RefMsgIdx() != "" {
		t.Fatalf("非引用消息不应有 ref_msg_idx: %q", payload.RefMsgIdx())
	}
	if !payload.BotMentioned("BOT_OPENID_1") {
		t.Fatal("mentions[0].is_you 为真, 应判定 @ 了机器人")
	}

	if len(payload.Attachments) != 1 || payload.Attachments[0].Kind() != "image" {
		t.Fatalf("附件解析不符: %+v", payload.Attachments)
	}
}

// TestEventPayloadQuoteAndC2C 校验引用消息与单聊事件的字段解析。
func TestEventPayloadQuoteAndC2C(t *testing.T) {
	quote := decodeSample(t, groupQuoteSample)
	if quote.MessageType != MessageTypeQuote {
		t.Fatalf("message_type 不符: %d", quote.MessageType)
	}
	if quote.RefMsgIdx() != "REFIDX_OUT_001" {
		t.Fatalf("ref_msg_idx 不符: %q", quote.RefMsgIdx())
	}
	if quote.MsgIdx() != "REFIDX_IN_002" {
		t.Fatalf("msg_idx 不符: %q", quote.MsgIdx())
	}
	if got := QuotedText(quote.MsgElements); got != "原话在这里" {
		t.Fatalf("被引用正文不符: %q", got)
	}
	if len(quote.MsgElements) != 1 || len(quote.MsgElements[0].Attachments) != 1 {
		t.Fatalf("msg_elements 解析不符: %+v", quote.MsgElements)
	}

	c2c := decodeSample(t, c2cSample)
	if c2c.SenderID(EventC2CMessageCreate) != "USER_OPENID_1" {
		t.Fatalf("单聊 sender 应取 user_openid: %q", c2c.SenderID(EventC2CMessageCreate))
	}
	if c2c.GroupOpenid() != "" {
		t.Fatalf("单聊不应有群标识: %q", c2c.GroupOpenid())
	}
}

// TestAttachmentKinds 校验 content_type → 附件类型的归一化。
func TestAttachmentKinds(t *testing.T) {
	tests := []struct {
		attachment Attachment
		want       string
	}{
		{Attachment{ContentType: "image/jpeg"}, "image"},
		{Attachment{ContentType: "image/png"}, "image"},
		{Attachment{ContentType: "image/gif"}, "image"},
		{Attachment{ContentType: "voice", AsrReferText: "转写文本"}, "voice"},
		{Attachment{ContentType: "audio/silk", AsrReferText: "语音转写"}, "voice"},
		{Attachment{ContentType: "audio/amr"}, "voice"},
		{Attachment{ContentType: "video"}, "video"},
		{Attachment{ContentType: "file", Filename: "a.pdf"}, "file"},
		{Attachment{ContentType: "", Filename: "b.PNG"}, "image"},
		{Attachment{ContentType: "text/plain"}, "file"},
	}
	for _, item := range tests {
		if got := item.attachment.Kind(); got != item.want {
			t.Fatalf("content_type=%q filename=%q → got %q want %q",
				item.attachment.ContentType, item.attachment.Filename, got, item.want)
		}
	}
}

// TestEventTime 校验时间戳的多种形态 (RFC3339 / unix 秒 / unix 毫秒 / 缺失)。
func TestEventTime(t *testing.T) {
	fallback := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := eventTime("2026-03-08T15:04:05+08:00", fallback); got.UTC().Format(time.RFC3339) != "2026-03-08T07:04:05Z" {
		t.Fatalf("RFC3339 解析不符: %v", got)
	}
	if got := eventTime(float64(1773000000), fallback); got.Unix() != 1773000000 {
		t.Fatalf("unix 秒解析不符: %v", got)
	}
	if got := eventTime(float64(1773000000000), fallback); got.UnixMilli() != 1773000000000 {
		t.Fatalf("unix 毫秒解析不符: %v", got)
	}
	if got := eventTime("", fallback); !got.Equal(fallback) {
		t.Fatalf("缺失时间戳应回退: %v", got)
	}
	if got := eventTime(nil, fallback); !got.Equal(fallback) {
		t.Fatalf("nil 时间戳应回退: %v", got)
	}
}

// ---------------------------------------------------------------------------
// 事件 → SessionMessage
// ---------------------------------------------------------------------------

// TestGroupMessageToSessionMessage 校验群消息的转换: @ 判定、组件、session_id。
func TestGroupMessageToSessionMessage(t *testing.T) {
	payload := decodeSample(t, groupMessageSample)
	now := time.Date(2026, 3, 8, 15, 0, 0, 0, time.UTC)
	refs := NewRefLedger(0, 0)
	refs.RecordOutbound("REFIDX_OUT_001", "SELF_MSG_1", "BOT_OPENID_1")

	converted, err := payload.ToSessionMessage(ConvertOptions{
		EventType: EventGroupMessageCreate,
		SelfID:    "BOT_OPENID_1",
		BotNames:  []string{"麦麦"},
		Now:       func() time.Time { return now },
		ResolveRef: func(refIdx string) (string, string, bool) {
			entry, ok := refs.Lookup(refIdx)
			return entry.MsgID, entry.UserID, ok
		},
	})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if !converted.IsAt {
		t.Fatal("mentions[].is_you 为真 → IsAt 必须为 true")
	}
	if !converted.IsMentioned {
		t.Fatal("文本命中机器人昵称 → IsMentioned 必须为 true")
	}
	if !converted.IsEmoji {
		t.Fatal("含表情标签 → IsEmoji 必须为 true")
	}
	if !converted.IsPicture {
		t.Fatal("含图片附件 → IsPicture 必须为 true")
	}
	if converted.Platform != PlatformQQ {
		t.Fatalf("platform 不符: %q", converted.Platform)
	}
	// 时间戳带 +08:00 偏移: 与 UTC 同一时刻即可。
	if !converted.Timestamp.Equal(time.Date(2026, 3, 8, 7, 4, 5, 0, time.UTC)) {
		t.Fatalf("时间戳不符: %v", converted.Timestamp)
	}
	if converted.UserID() != "MEMBER_OPENID_1" {
		t.Fatalf("user_id 不符: %q", converted.UserID())
	}
	if converted.GroupID() != "GROUP_OPENID_1" || converted.ChatType() != "group" {
		t.Fatalf("群信息不符: %+v", converted.MessageInfo.GroupInfo)
	}
	if converted.MessageInfo.UserInfo.DisplayName() != "群友A的群名片" {
		t.Fatalf("显示名不符: %q", converted.MessageInfo.UserInfo.DisplayName())
	}
	if converted.SessionID == "" {
		t.Fatal("session_id 不应为空")
	}
	// 期望的 session_id: md5("qq_GROUP_OPENID_1")。
	if want := mustSessionID(t, "qq", "", "GROUP_OPENID_1", "", ""); converted.SessionID != want {
		t.Fatalf("session_id 不符: got %s want %s", converted.SessionID, want)
	}
	if converted.MessageInfo.AdditionalConfig[KeyMsgIdx] != "REFIDX_IN_001" {
		t.Fatalf("msg_idx 未登记: %+v", converted.MessageInfo.AdditionalConfig)
	}

	// 文本: 表情标签翻成 [表情:[吃西瓜]], 且 <@id> 残留标记被剥掉。
	text := converted.ProcessDefaults()
	if strings.Contains(text, "<@") || strings.Contains(text, "faceType") {
		t.Fatalf("文本仍含协议残留: %q", text)
	}
	if !strings.Contains(text, "[表情:[自制]]") {
		t.Fatalf("表情标签未翻译: %q", text)
	}
	// 组件: at + text + image。
	kinds := componentKinds(converted.RawMessage)
	if len(kinds) != 3 || kinds[0] != "at" || kinds[1] != "text" || kinds[2] != "image" {
		t.Fatalf("组件序列不符: %v", kinds)
	}
}

// TestQuoteEventToSessionMessage 校验引用消息: ref_msg_idx 反查 → ReplyTo + Reply 组件。
func TestQuoteEventToSessionMessage(t *testing.T) {
	payload := decodeSample(t, groupQuoteSample)
	refs := NewRefLedger(0, 0)
	refs.RecordOutbound("REFIDX_OUT_001", "SELF_MSG_1", "BOT_OPENID_1")

	converted, err := payload.ToSessionMessage(ConvertOptions{
		EventType: EventGroupMessageCreate,
		SelfID:    "BOT_OPENID_1",
		ResolveRef: func(refIdx string) (string, string, bool) {
			entry, ok := refs.Lookup(refIdx)
			return entry.MsgID, entry.UserID, ok
		},
	})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if converted.ReplyTo == nil || *converted.ReplyTo != "SELF_MSG_1" {
		t.Fatalf("ReplyTo 应反查成平台消息 id SELF_MSG_1: %v", converted.ReplyTo)
	}
	kinds := componentKinds(converted.RawMessage)
	if len(kinds) == 0 || kinds[0] != "reply" {
		t.Fatalf("首个组件应为 reply: %v", kinds)
	}
	reply, ok := converted.RawMessage.Components[0].(*message.ReplyComponent)
	if !ok {
		t.Fatalf("reply 组件类型不符: %T", converted.RawMessage.Components[0])
	}
	if reply.TargetMessageID != "SELF_MSG_1" {
		t.Fatalf("reply 目标不符: %q", reply.TargetMessageID)
	}
	if reply.TargetMessageContent == nil || *reply.TargetMessageContent != "原话在这里" {
		t.Fatalf("被引用正文未回填: %v", reply.TargetMessageContent)
	}
	if converted.MessageInfo.AdditionalConfig[KeyRefMsgIdx] != "REFIDX_OUT_001" {
		t.Fatalf("ref_msg_idx 未登记: %+v", converted.MessageInfo.AdditionalConfig)
	}
	// 渲染应带上被引用内容。
	if text := converted.ProcessDefaults(); !strings.Contains(text, "原话在这里") {
		t.Fatalf("渲染文本应含被引用正文: %q", text)
	}
}

// TestUnresolvedQuoteKeepsRefKey 校验引用键查不到时仍保留 REFIDX_ 值且不设 ReplyTo。
func TestUnresolvedQuoteKeepsRefKey(t *testing.T) {
	payload := decodeSample(t, groupQuoteSample)
	converted, err := payload.ToSessionMessage(ConvertOptions{
		EventType: EventGroupMessageCreate,
		SelfID:    "BOT_OPENID_1",
	})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if converted.ReplyTo != nil {
		t.Fatalf("查不到引用时不应设 ReplyTo: %v", *converted.ReplyTo)
	}
	reply, ok := converted.RawMessage.Components[0].(*message.ReplyComponent)
	if !ok {
		t.Fatalf("首个组件应为 reply: %T", converted.RawMessage.Components[0])
	}
	if reply.TargetMessageID != "REFIDX_OUT_001" {
		t.Fatalf("未解析时应保留 REFIDX_ 键: %q", reply.TargetMessageID)
	}
}

// TestC2CToSessionMessage 校验单聊转换: 私聊 session_id 与 private 形态。
func TestC2CToSessionMessage(t *testing.T) {
	payload := decodeSample(t, c2cSample)
	converted, err := payload.ToSessionMessage(ConvertOptions{EventType: EventC2CMessageCreate, SelfID: "BOT_OPENID_1"})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if converted.ChatType() != "private" {
		t.Fatalf("单聊 ChatType 不符: %q", converted.ChatType())
	}
	if converted.UserID() != "USER_OPENID_1" {
		t.Fatalf("user_id 不符: %q", converted.UserID())
	}
	if want := mustSessionID(t, "qq", "USER_OPENID_1", "", "", ""); converted.SessionID != want {
		t.Fatalf("session_id 不符: got %s want %s", converted.SessionID, want)
	}
	if converted.IsAt {
		t.Fatal("单聊不应判定 IsAt")
	}
}

// TestGroupAtEventForcesAt 校验兼容事件 GROUP_AT_MESSAGE_CREATE 收到即被点名。
func TestGroupAtEventForcesAt(t *testing.T) {
	raw := `{"id":"AT_1","content":"在吗","author":{"id":"M1","member_openid":"M1","username":"A"},"group_openid":"G1","mentions":[]}`
	payload := decodeSample(t, raw)
	converted, err := payload.ToSessionMessage(ConvertOptions{EventType: EventGroupAtMessageCreate, SelfID: "BOT"})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if !converted.IsAt || !converted.IsMentioned {
		t.Fatal("GROUP_AT_MESSAGE_CREATE 应判定为被 @")
	}
}

// TestToSessionMessageRejects 校验转换的拒绝路径。
func TestToSessionMessageRejects(t *testing.T) {
	payload := decodeSample(t, groupMessageSample)
	if _, err := payload.ToSessionMessage(ConvertOptions{EventType: "READY"}); err == nil {
		t.Fatal("非消息事件应报错")
	}
	// 群消息缺 group_openid。
	noGroup := decodeSample(t, `{"id":"X","author":{"member_openid":"M1"}}`)
	if _, err := noGroup.ToSessionMessage(ConvertOptions{EventType: EventGroupMessageCreate}); err == nil {
		t.Fatal("群事件缺 group_openid 应报错")
	}
	// 缺发言者。
	noAuthor := decodeSample(t, `{"id":"X","group_openid":"G1"}`)
	if _, err := noAuthor.ToSessionMessage(ConvertOptions{EventType: EventGroupMessageCreate}); err == nil {
		t.Fatal("缺发言者标识应报错")
	}
}

// TestAttachmentsToComponents 校验各类附件转成的组件类型与语音转写文本。
func TestAttachmentsToComponents(t *testing.T) {
	payload := &EventPayload{Attachments: []Attachment{
		{ContentType: "image/png", URL: "https://x/1.png"},
		{ContentType: "voice", AsrReferText: "你好世界"},
		{ContentType: "voice"},
		{ContentType: "file", Filename: "a.pdf", Size: 2048, URL: "https://x/a.pdf"},
	}}
	components := attachmentComponents(payload.Attachments)
	if len(components) != 4 {
		t.Fatalf("组件数不符: %d", len(components))
	}
	voice, ok := components[1].(*message.VoiceComponent)
	if !ok || voice.Content != "你好世界" {
		t.Fatalf("语音转写文本未带上: %+v", components[1])
	}
	emptyVoice, ok := components[2].(*message.VoiceComponent)
	if !ok || emptyVoice.Content != message.VoicePlaceholder {
		t.Fatalf("无转写语音应回退占位符: %+v", components[2])
	}
	file, ok := components[3].(*message.FileComponent)
	if !ok || file.Name != "a.pdf" || file.Size != "2048" {
		t.Fatalf("文件组件不符: %+v", components[3])
	}
}

// TestRenderFaceTags 校验表情标签翻译与未知名回退。
func TestRenderFaceTags(t *testing.T) {
	named := `你好<faceType=6,faceId="0",ext="eyJ0ZXh0IjoiW+iHquWItl0ifQ==">呀`
	if got := RenderFaceTags(named); got != "你好[表情:[自制]]呀" {
		t.Fatalf("表情翻译不符: %q", got)
	}
	unknown := `x<faceType=4,faceId="",ext="">y`
	if got := RenderFaceTags(unknown); got != "x[表情]y" {
		t.Fatalf("无名字表情应回退 [表情]: %q", got)
	}
	plain := "没有表情"
	if got := RenderFaceTags(plain); got != plain {
		t.Fatalf("无标签文本应原样返回: %q", got)
	}
}

// TestStripMentionTokens 校验指向机器人的 <@id> 残留标记被剥掉。
func TestStripMentionTokens(t *testing.T) {
	mentions := []Mention{{ID: "BOT1", IsYou: true}, {ID: "OTHER", IsYou: false}}
	got := stripMentionTokens("看看 <@BOT1> 和 <@OTHER>", mentions, "BOT1")
	if got != "看看  和 <@OTHER>" {
		t.Fatalf("只应剥掉指向机器人的标记: %q", got)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustJSON(t *testing.T, frame GatewayPayload) string {
	t.Helper()
	raw, err := EncodeFrame(frame)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	return string(raw)
}

func mustSessionID(t *testing.T, platform string, userID string, groupID string, account string, scope string) string {
	t.Helper()
	id, err := calculateTestSessionID(platform, userID, groupID, account, scope)
	if err != nil {
		t.Fatalf("session_id 计算失败: %v", err)
	}
	return id
}

func componentKinds(sequence *message.MessageSequence) []string {
	if sequence == nil {
		return nil
	}
	out := make([]string, 0, len(sequence.Components))
	for _, component := range sequence.Components {
		out = append(out, component.FormatName())
	}
	return out
}

// calculateTestSessionID 复用 internal/message 的计算 (避免测试里重复实现 md5 拼接)。
func calculateTestSessionID(platform string, userID string, groupID string, account string, scope string) (string, error) {
	return message.CalculateSessionIDStrings(platform, userID, groupID, account, scope)
}
