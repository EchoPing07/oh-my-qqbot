// Package qq implements the QQ 官方机器人渠道 (QQ official bot channel):
// the WebSocket gateway (receive) and the REST API (send).
//
// Scope (REWRITE_PLAN §6.12 + §2 渠道约束):
//
//   - Events: GROUP_MESSAGE_CREATE (全量, 需管理端开启「接收所有消息」),
//     GROUP_AT_MESSAGE_CREATE (未开全量时的兼容事件, 字段一致),
//     C2C_MESSAGE_CREATE (单聊)。@ 机器人靠 payload mentions[].is_you 识别。
//   - 出站双模: 被动回复(带触发 msg_id + 自增 msg_seq) 优先, 超窗(5min)或超次数(5 次)
//     降级为主动消息(不带 msg_id), 主动消息先过额度台账。
//   - 富媒体: 分片上传拿 file_info, 再发 msg_type 7。
//   - 引用: 入站 message_scene.ext 的 msg_idx / ref_msg_idx 是 REFIDX_ 键空间,
//     出站靠发送响应 ext_info.ref_idx 登记, 双向靠 ledger.RefLedger 互查。
//
// This package must not import internal/chat or internal/config: configuration
// arrives through the package-local Options struct (see channel.go).
//
// 工程实现参考 参考/Diana/model/assistant/qq_official*.go, 但**不照抄**其
// 「群里主动消息能力已被平台关闭(40034105)」的假设: 本包按双模发送实现,
// 主动消息由额度台账承载并在被拒时返回可识别错误。
package qq

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

// PlatformQQ is the platform tag written into message.SessionMessage.Platform.
const PlatformQQ = "qq"

// Gateway op codes (官方 WebSocket 网关, 与 Discord 同族)。
const (
	// OpDispatch 是服务端下发的业务事件 (OP 0)。
	OpDispatch = 0
	// OpHeartbeat 是客户端上报心跳 (OP 1)。
	OpHeartbeat = 1
	// OpIdentify 是首次鉴权 (OP 2)。
	OpIdentify = 2
	// OpResume 是断线重连后的会话恢复 (OP 6)。
	OpResume = 6
	// OpReconnect 是服务端要求客户端重连 (OP 7)。
	OpReconnect = 7
	// OpInvalidSession 是会话失效, 必须重新 IDENTIFY (OP 9)。
	OpInvalidSession = 9
	// OpHello 是连接建立后服务端先发的 hello 帧, 携带 heartbeat_interval (OP 10)。
	OpHello = 10
	// OpHeartbeatAck 是心跳应答 (OP 11)。
	OpHeartbeatAck = 11
	// OpResumeAlt 是部分文档/网关给出的另一种 resume op (OP 13)。本包发送时默认用
	// OpResume(6), 解析入站帧时 6 与 13 都按 resume 语义接受。
	OpResumeAlt = 13
)

// Event intents (订阅的事件位)。
const (
	// IntentGroupAndC2CEvent 订阅群聊 + 单聊消息事件。全量群消息
	// (GROUP_MESSAGE_CREATE) 与 @ 消息 (GROUP_AT_MESSAGE_CREATE) 共用这一位,
	// 单聊 C2C_MESSAGE_CREATE 也在其中。
	IntentGroupAndC2CEvent = 1 << 25
	// IntentPublicGuildMessages 订阅频道消息; 未开通频道能力时订阅会被网关拒绝,
	// 默认不订阅。
	IntentPublicGuildMessages = 1 << 30
	// IntentDefault 是本包默认订阅的意图集合。
	IntentDefault = IntentGroupAndC2CEvent
)

// 业务事件名。
const (
	EventGroupMessageCreate   = "GROUP_MESSAGE_CREATE"
	EventGroupAtMessageCreate = "GROUP_AT_MESSAGE_CREATE"
	EventC2CMessageCreate     = "C2C_MESSAGE_CREATE"
	EventReady                = "READY"
	EventResumed              = "RESUMED"
)

// 消息形态 (入站 payload 的 message_type)。
const (
	// MessageTypeText 是普通消息。
	MessageTypeText = 0
	// MessageTypeMergedForward 是合并转发(聊天记录)原件。
	MessageTypeMergedForward = 102
	// MessageTypeQuote 是引用消息: 被引用正文与附件直接放在 msg_elements 里,
	// 引用索引在 message_scene.ext 的 ref_msg_idx。
	MessageTypeQuote = 103
)

// ---------------------------------------------------------------------------
// Gateway framing
// ---------------------------------------------------------------------------

// GatewayPayload 是网关帧。接收方向统一走 Data (json.RawMessage), 发送方向用 D
// 塞任意结构; MarshalJSON 只在需要时输出 d / t / s。
type GatewayPayload struct {
	Op   int             `json:"op"`
	S    int64           `json:"s,omitempty"`
	T    string          `json:"t,omitempty"`
	Data json.RawMessage `json:"d,omitempty"`
	D    any             `json:"-"`
}

// MarshalJSON 让发送方向可以直接塞任意 d, 接收方向仍保留 RawMessage。
//
// D 优先 (发送方向); D 为空时回落到 Data, 这样直接转发入站帧也能保留 d。
func (p GatewayPayload) MarshalJSON() ([]byte, error) {
	out := map[string]any{"op": p.Op}
	switch {
	case p.D != nil:
		out["d"] = p.D
	case len(p.Data) > 0:
		out["d"] = json.RawMessage(p.Data)
	}
	if p.T != "" {
		out["t"] = p.T
	}
	if p.S > 0 {
		out["s"] = p.S
	}
	return json.Marshal(out)
}

// EncodeFrame 序列化一个网关帧 (便于测试与断言)。
func EncodeFrame(p GatewayPayload) ([]byte, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("qq: 编码网关帧失败: %w", err)
	}
	return raw, nil
}

// IdentifyFrame 构造鉴权帧 (OP 2)。
func IdentifyFrame(token string, intents int, shard []int) GatewayPayload {
	if intents == 0 {
		intents = IntentDefault
	}
	if len(shard) == 0 {
		shard = []int{0, 1}
	}
	return GatewayPayload{Op: OpIdentify, D: map[string]any{
		"token":      token,
		"intents":    intents,
		"shard":      shard,
		"properties": map[string]string{},
	}}
}

// ResumeFrame 构造会话恢复帧; resumeOp 为 0 时用 OpResume(6)。
func ResumeFrame(token string, sessionID string, seq int64, resumeOp int) GatewayPayload {
	if resumeOp == 0 {
		resumeOp = OpResume
	}
	return GatewayPayload{Op: resumeOp, D: map[string]any{
		"token":      token,
		"session_id": sessionID,
		"seq":        seq,
	}}
}

// HeartbeatFrame 构造心跳帧 (OP 1); seq 为已处理的最后一个序号, 0 时不带 d。
func HeartbeatFrame(seq int64) GatewayPayload {
	var data any
	if seq > 0 {
		data = seq
	}
	return GatewayPayload{Op: OpHeartbeat, D: data}
}

// HelloFrame 解析 hello 帧 (OP 10) 的心跳间隔; 非 hello 帧或缺失字段返回 0。
func HelloFrame(payload GatewayPayload) time.Duration {
	if payload.Op != OpHello || len(payload.Data) == 0 {
		return 0
	}
	var data struct {
		HeartbeatInterval int64 `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(payload.Data, &data); err != nil {
		return 0
	}
	return time.Duration(data.HeartbeatInterval) * time.Millisecond
}

// ---------------------------------------------------------------------------
// 事件 payload
// ---------------------------------------------------------------------------

// Author 是发言者; 群消息用 member_openid, 单聊用 user_openid, id 为兜底。
type Author struct {
	ID           string `json:"id"`
	UserOpenID   string `json:"user_openid"`
	MemberOpenID string `json:"member_openid"`
	Username     string `json:"username"`
	Avatar       string `json:"avatar,omitempty"`
	Bot          bool   `json:"bot,omitempty"`
}

// Member 是群成员资料 (nick 为群名片)。
type Member struct {
	Nick  string   `json:"nick"`
	Roles []string `json:"roles"`
}

// Mention 是全量群消息里一个 @ 目标的描述。
type Mention struct {
	ID           string `json:"id"`
	OpenID       string `json:"openid"`
	UserOpenID   string `json:"user_openid"`
	MemberOpenID string `json:"member_openid"`
	// IsYou 是网关给的「这个 @ 指向当前机器人」标记。
	IsYou bool `json:"is_you"`
}

// PointsTo 判断这个 @ 是否指向当前机器人: 优先认 is_you, 拿不到再按 id 比对。
func (m Mention) PointsTo(selfID string) bool {
	if m.IsYou {
		return true
	}
	if selfID == "" {
		return false
	}
	for _, id := range []string{m.ID, m.OpenID, m.UserOpenID, m.MemberOpenID} {
		if id != "" && id == selfID {
			return true
		}
	}
	return false
}

// Attachment 是消息附带的一条富媒体附件。图片/语音/视频/文件靠 content_type 区分:
// 语音是裸值 voice (附带 asr_refer_text 平台转写), 文件是裸值 file, 图片是 MIME。
type Attachment struct {
	ContentType  string `json:"content_type"`
	Filename     string `json:"filename"`
	URL          string `json:"url"`
	Size         int64  `json:"size"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	AsrReferText string `json:"asr_refer_text"`
}

// Kind 归一化附件类型: image / voice / video / file。
func (a Attachment) Kind() string {
	contentType := strings.ToLower(strings.TrimSpace(a.ContentType))
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return "image"
	case contentType == "voice":
		return "voice"
	case contentType == "video" || strings.HasPrefix(contentType, "video/"):
		return "video"
	case contentType == "file":
		return "file"
	}
	if hasImageExtension(a.Filename) {
		return "image"
	}
	return "file"
}

// MsgElement 是引用消息 (message_type=103) 里被引用内容的一个元素。
type MsgElement struct {
	Content     string       `json:"content"`
	Attachments []Attachment `json:"attachments"`
}

// MessageScene 是消息自带的场景信息, 只有 ext 数组被用到。
type MessageScene struct {
	Ext []string `json:"ext"`
}

// MessageReference 属于发送方向协议; 入站事件一般不携带, 保留解析以兼容。
type MessageReference struct {
	MessageID             string `json:"message_id"`
	IgnoreGetMessageError bool   `json:"ignore_get_message_error,omitempty"`
}

// EventPayload 是群聊 / 单聊消息的公共字段。
//
// GROUP_MESSAGE_CREATE / GROUP_AT_MESSAGE_CREATE / C2C_MESSAGE_CREATE 三者字段
// 一致: 群标识在 group_openid (全量事件另给同值 group_id), 单聊在 author.user_openid。
type EventPayload struct {
	ID          string       `json:"id"`
	Content     string       `json:"content"`
	Timestamp   any          `json:"timestamp"`
	Author      Author       `json:"author"`
	Member      Member       `json:"member"`
	Mentions    []Mention    `json:"mentions"`
	Attachments []Attachment `json:"attachments"`

	GroupOpenID string `json:"group_openid"`
	// GroupID 是全量群消息 (GROUP_MESSAGE_CREATE) 里携带的群标识, 与 group_openid
	// 同值; 取不到后者时兜底。
	GroupID string `json:"group_id"`

	// ChannelID / GuildID 只出现在频道消息里; 本包不订阅频道意图, 仅保留解析。
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`

	MessageReference *MessageReference `json:"message_reference"`
	MessageType      int               `json:"message_type"`
	MsgElements      []MsgElement      `json:"msg_elements"`
	MessageScene     *MessageScene     `json:"message_scene"`
}

// SceneExtValue 取 message_scene.ext 里某个键的值。值是整串比对 (REFIDX_ 键不可
// 解析), 不能按 = 切分——base64 值理论上可能带 = 填充。
func (e *EventPayload) SceneExtValue(key string) string {
	if e == nil || e.MessageScene == nil {
		return ""
	}
	prefix := key + "="
	for _, item := range e.MessageScene.Ext {
		if value, ok := strings.CutPrefix(strings.TrimSpace(item), prefix); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// MsgIdx 是本条消息的引用索引键 (REFIDX_ 空间), 用于别人引用它时反查。
func (e *EventPayload) MsgIdx() string { return e.SceneExtValue("msg_idx") }

// RefMsgIdx 指向被引用消息的引用索引键。
func (e *EventPayload) RefMsgIdx() string { return e.SceneExtValue("ref_msg_idx") }

// GroupOpenid 取群标识 (group_openid 优先, 兜底 group_id)。
func (e *EventPayload) GroupOpenid() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.GroupOpenID) != "" {
		return strings.TrimSpace(e.GroupOpenID)
	}
	return strings.TrimSpace(e.GroupID)
}

// SenderID 取发言者标识: 群消息用 member_openid, 单聊用 user_openid, 兜底 id。
func (e *EventPayload) SenderID(eventType string) string {
	if e == nil {
		return ""
	}
	if eventType == EventC2CMessageCreate {
		return firstNonEmpty(e.Author.UserOpenID, e.Author.ID)
	}
	return firstNonEmpty(e.Author.MemberOpenID, e.Author.ID, e.Author.UserOpenID)
}

// SenderName 取发言者显示名: 群名片 > 用户名 > id。
func (e *EventPayload) SenderName() string {
	if e == nil {
		return ""
	}
	return firstNonEmpty(e.Member.Nick, e.Author.Username, e.Author.ID)
}

// BotMentioned 判断这条消息是否 @ 了当前机器人。
func (e *EventPayload) BotMentioned(selfID string) bool {
	if e == nil {
		return false
	}
	for _, mention := range e.Mentions {
		if mention.PointsTo(selfID) {
			return true
		}
	}
	return false
}

// IsGroupEvent 判断事件类型是否群聊事件。
func IsGroupEvent(eventType string) bool {
	switch eventType {
	case EventGroupMessageCreate, EventGroupAtMessageCreate:
		return true
	default:
		return false
	}
}

// IsMessageEvent 判断事件类型是否本包处理的消息事件。
func IsMessageEvent(eventType string) bool {
	switch eventType {
	case EventGroupMessageCreate, EventGroupAtMessageCreate, EventC2CMessageCreate:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// 事件 → SessionMessage
// ---------------------------------------------------------------------------

// ConvertOptions 控制事件到 internal/message.SessionMessage 的转换。
//
// 转换不依赖 ledger / config: 引用索引的反查通过 ResolveRef 注入 (由 Channel 把
// RefLedger 接进来), 拿不到引用就照常交付。
type ConvertOptions struct {
	// EventType 是网关事件名 (决定群聊/私聊)。
	EventType string
	// Platform 写入 SessionMessage.Platform, 默认 PlatformQQ("qq")。
	Platform string
	// SelfID 是机器人自己的 openid (READY 事件给出), 用于 is_you 兜底比对。
	SelfID string
	// BotNames 是机器人昵称 + 别名: 文本命中即为 is_mentioned。
	BotNames []string
	// AccountID / Scope 是 session_id 的可选路由组件 (多开时区分)。
	AccountID string
	Scope     string
	// Now 提供当前时间 (payload 时间戳缺失或不可解析时用), nil → time.Now。
	Now func() time.Time
	// ResolveRef 把 REFIDX_ 引用键反查成 (平台消息 id, 发言者名, 是否命中)。
	ResolveRef func(refIdx string) (string, string, bool)
}

func (o ConvertOptions) platform() string {
	if strings.TrimSpace(o.Platform) == "" {
		return PlatformQQ
	}
	return strings.TrimSpace(o.Platform)
}

func (o ConvertOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// AdditionalConfig metadata keys carried into SessionMessage.MessageInfo.
const (
	KeyEventType   = "qq_event_type"
	KeyMsgIdx      = "qq_msg_idx"
	KeyRefMsgIdx   = "qq_ref_msg_idx"
	KeyAuthorID    = "qq_author_id"
	KeyMessageType = "qq_message_type"
	KeyIsBotEcho   = "qq_bot_echo"
	KeyMediaURLs   = "qq_media_urls"
)

// ToSessionMessage 把事件转成组件化的 SessionMessage (§6.12)。
//
// 转换规则:
//   - mentions[].is_you == true → IsAt = true (并把 content 里残留的 <@id> 标记剥掉)
//   - 文本命中 BotNames → IsMentioned = true (对齐 is_mentioned_bot_in_message)
//   - message_scene.ext.msg_idx 登记进 additional_config; ref_msg_idx 经 ResolveRef
//     反查成平台消息 id 后写入 ReplyTo 并挂 ReplyComponent
//   - attachments 按 content_type 转成 Image / Voice / File 组件
//   - 表情标签 <faceType=...> 翻成 [表情:名字] 并把 IsEmoji 置真
func (e *EventPayload) ToSessionMessage(opts ConvertOptions) (*message.SessionMessage, error) {
	if e == nil {
		return nil, fmt.Errorf("qq: 事件 payload 为空")
	}
	eventType := strings.TrimSpace(opts.EventType)
	if !IsMessageEvent(eventType) {
		return nil, fmt.Errorf("qq: 不支持的事件类型 %q", eventType)
	}
	isGroup := IsGroupEvent(eventType)
	userID := e.SenderID(eventType)
	if userID == "" {
		return nil, fmt.Errorf("qq: 事件缺少发言者标识")
	}
	groupID := ""
	if isGroup {
		groupID = e.GroupOpenid()
		if groupID == "" {
			return nil, fmt.Errorf("qq: 群聊事件缺少 group_openid")
		}
	}

	now := opts.now()
	timestamp := eventTime(e.Timestamp, now)
	msg := message.NewSessionMessage(strings.TrimSpace(e.ID), timestamp, opts.platform())
	msg.Initialized = true
	if id := strings.TrimSpace(e.ID); id != "" {
		msg.PlatformMessageID = &id
	}

	// 文本: 先翻表情标签, 再剥掉指向机器人的 <@id> 残留标记。
	hadFaceTag := containsFaceTag(e.Content)
	text := RenderFaceTags(strings.TrimSpace(e.Content))
	isAt := e.BotMentioned(opts.SelfID)
	if isAt {
		text = stripMentionTokens(text, e.Mentions, opts.SelfID)
	}
	msg.IsEmoji = hadFaceTag
	// GROUP_AT_MESSAGE_CREATE 只推 @ 了机器人的消息, 收到即被点名。
	if eventType == EventGroupAtMessageCreate {
		isAt = true
	}
	msg.IsAt = isAt
	msg.IsMentioned = isAt || mentionsBotName(text, opts.BotNames)

	// 引用: 入站给的是 REFIDX_ 键, 反查成平台消息 id 才能给上层用。
	var replyTo string
	refKey := firstNonEmpty(e.RefMsgIdx(), refOfMessageReference(e.MessageReference))
	quotedText := QuotedText(e.MsgElements)
	if refKey != "" {
		resolved, senderName, ok := "", "", false
		if opts.ResolveRef != nil {
			resolved, senderName, ok = opts.ResolveRef(refKey)
		}
		target := refKey
		if ok && resolved != "" {
			target = resolved
			replyTo = resolved
		}
		reply := message.NewReplyComponent(target)
		if quotedText != "" {
			reply.TargetMessageContent = message.StringPtr(quotedText)
		}
		if ok && senderName != "" {
			reply.TargetMessageSenderNickname = message.StringPtr(senderName)
		}
		msg.RawMessage.Append(reply)
		if replyTo != "" {
			msg.ReplyTo = &replyTo
		}
	}

	// @ 组件: 只挂指向机器人的那个, 避免群里一串 @ 把上下文灌满。
	if isAt {
		target := opts.SelfID
		if target == "" {
			for _, mention := range e.Mentions {
				if mention.IsYou && mention.OpenID != "" {
					target = mention.OpenID
					break
				}
			}
		}
		msg.RawMessage.At(target)
	}
	if text != "" {
		msg.RawMessage.Text(text)
	}
	mediaURLs := []string{}
	for _, component := range attachmentComponents(e.Attachments) {
		msg.RawMessage.Append(component)
		if _, ok := component.(*message.ImageComponent); ok {
			msg.IsPicture = true
		}
	}
	for _, attachment := range e.Attachments {
		if url := strings.TrimSpace(attachment.URL); url != "" {
			mediaURLs = append(mediaURLs, url)
		}
	}

	msg.MessageInfo.UserInfo = message.NewUserInfo(userID, firstNonEmpty(e.Author.Username, e.Member.Nick), e.Member.Nick)
	if isGroup {
		msg.MessageInfo.GroupInfo = &message.GroupInfo{GroupID: groupID}
	}
	config := msg.AdditionalConfigMap()
	config[KeyEventType] = eventType
	config[KeyMessageType] = e.MessageType
	config[KeyAuthorID] = firstNonEmpty(e.Author.ID, userID)
	if e.Author.Bot {
		config[KeyIsBotEcho] = true
	}
	if idx := e.MsgIdx(); idx != "" {
		config[KeyMsgIdx] = idx
	}
	if refKey != "" {
		config[KeyRefMsgIdx] = refKey
	}
	if len(mediaURLs) > 0 {
		config[KeyMediaURLs] = mediaURLs
	}
	if opts.AccountID != "" {
		config["platform_io_account_id"] = opts.AccountID
	}
	if opts.Scope != "" {
		config["platform_io_scope"] = opts.Scope
	}
	if err := msg.EnsureSessionID(); err != nil {
		return nil, err
	}
	return msg, nil
}

// QuotedText 把引用消息 msg_elements 里的被引用正文拼出来 (多行用 \n 连接)。
func QuotedText(elements []MsgElement) string {
	parts := make([]string, 0, len(elements))
	for _, element := range elements {
		text := RenderFaceTags(strings.TrimSpace(element.Content))
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// attachmentComponents 把附件转成组件化消息的组件。
func attachmentComponents(attachments []Attachment) []message.Component {
	components := make([]message.Component, 0, len(attachments))
	for _, attachment := range attachments {
		switch attachment.Kind() {
		case "image":
			components = append(components, message.NewImageComponent(nil, "[图片]"))
		case "voice":
			text := strings.TrimSpace(attachment.AsrReferText)
			if text == "" {
				text = message.VoicePlaceholder
			}
			components = append(components, message.NewVoiceComponent(nil, text))
		default:
			file := &message.FileComponent{
				Name:     firstNonEmpty(attachment.Filename, attachment.Kind()),
				URL:      strings.TrimSpace(attachment.URL),
				MIMEType: strings.TrimSpace(attachment.ContentType),
			}
			if attachment.Size > 0 {
				file.Size = strconv.FormatInt(attachment.Size, 10)
			}
			components = append(components, file)
		}
	}
	return components
}

// mentionsBotName 对齐 MaiBot 的 is_mentioned_bot_in_message: 文本里出现机器人
// 名字或别名即算被提到 (不要求是真的 @)。
func mentionsBotName(text string, names []string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" && strings.Contains(text, name) {
			return true
		}
	}
	return false
}

// stripMentionTokens 去掉 content 里残留的 <@openid> 标记。
//
// 文档说 content 已去除 @ 前缀, 但解析不到位时会把一串十六进制带进模型上下文,
// 所以按 mentions 里指向自己的条目再剥一次。
func stripMentionTokens(text string, mentions []Mention, selfID string) string {
	for _, mention := range mentions {
		if !mention.PointsTo(selfID) {
			continue
		}
		for _, id := range []string{mention.ID, mention.OpenID, mention.UserOpenID, mention.MemberOpenID, selfID} {
			if id == "" {
				continue
			}
			text = strings.ReplaceAll(text, "<@"+id+">", "")
			text = strings.ReplaceAll(text, "<@!"+id+">", "")
		}
	}
	return strings.TrimSpace(text)
}

// ---------------------------------------------------------------------------
// 表情标签
// ---------------------------------------------------------------------------

// faceTagPattern 匹配 content 里的表情标签: 表情商城的
// <faceType=4,faceId="",ext="…"> 与系统表情的 <faceType=6,faceId="0",ext="…">。
const faceTagPrefix = "<faceType"

var (
	compiledFaceTagPattern    = regexp.MustCompile(`<faceType=\d+[^>]*>`)
	compiledFaceTagExtPattern = regexp.MustCompile(`ext="([^"]*)"`)
)

func faceTagPattern() *regexp.Regexp    { return compiledFaceTagPattern }
func faceTagExtPattern() *regexp.Regexp { return compiledFaceTagExtPattern }

// RenderFaceTags 把 content 里的表情标签翻成可读文本。
//
// 名字只藏在标签 ext 里: base64 解开是 JSON, text 字段就是表情名 (自带方括号,
// 如 [吃芒果])。取不到名字时退回 [表情], 至少让模型知道这里有个表情。
func RenderFaceTags(content string) string {
	if !strings.Contains(content, faceTagPrefix) {
		return content
	}
	return faceTagPattern().ReplaceAllStringFunc(content, func(tag string) string {
		if name := FaceTagName(tag); name != "" {
			return "[表情:" + name + "]"
		}
		return "[表情]"
	})
}

// containsFaceTag 判断文本里是否含表情标签。
func containsFaceTag(content string) bool { return strings.Contains(content, faceTagPrefix) }

// FaceTagName 解出表情标签里的名字, 取不到返回空串。
func FaceTagName(tag string) string {
	match := faceTagExtPattern().FindStringSubmatch(tag)
	if len(match) < 2 {
		return ""
	}
	decoded, err := decodeBase64String(match[1])
	if err != nil {
		return ""
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Text)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func refOfMessageReference(ref *MessageReference) string {
	if ref == nil {
		return ""
	}
	return strings.TrimSpace(ref.MessageID)
}

// eventTime 归一化平台时间戳: RFC3339 字符串 / 常见中文格式 / 秒或毫秒 unix 值。
func eventTime(raw any, fallback time.Time) time.Time {
	switch value := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return fallback
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
			if parsed, err := time.Parse(layout, trimmed); err == nil {
				return parsed
			}
		}
		if number, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return unixTime(number, fallback)
		}
	case float64:
		return unixTime(value, fallback)
	case float32:
		return unixTime(float64(value), fallback)
	case int64:
		return unixTime(float64(value), fallback)
	case json.Number:
		if number, err := value.Float64(); err == nil {
			return unixTime(number, fallback)
		}
	}
	return fallback
}

func unixTime(value float64, fallback time.Time) time.Time {
	switch {
	case value <= 0:
		return fallback
	case value > 1e12:
		return time.UnixMilli(int64(value))
	default:
		return time.Unix(int64(value), 0)
	}
}

func hasImageExtension(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// decodeBase64String 解 base64, 标准与 URL 变体、缺填充都接受。
func decodeBase64String(value string) ([]byte, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, fmt.Errorf("qq: 空的 base64 值")
	}
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(trimmed); err == nil {
		return decoded, nil
	}
	return base64.RawURLEncoding.DecodeString(trimmed)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
