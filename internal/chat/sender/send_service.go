// Package chat/sender 实现 send_service（对照
// 参考/MaiBot/src/services/send_service.py，行为以该文件为权威）：
//
// 出站构造（_build_outbound_session_message）→ 渠道发送（双模，接口注入）
// → 成功回执回填（platform message id）→ 存库（storage_message）
// → 回写运行时历史 guided_reply（append_sent_message_to_chat_history）。
//
// 依赖接口注入：不 import internal/qq、internal/config。渠道层由装配层适配
// qq.Channel（包内给出最小 ChannelClient 接口定义）。
package sender

import (
	"context"
	"fmt"
	"strings"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/message"
)

// SendRequest 对齐 internal.SendRequest（contracts.go）：一次出站请求（多段）。
type SendRequest struct {
	SessionID      string
	ChatType       string // "group" | "private"
	GroupID        string
	GroupPlatform  string
	TargetUserID   string
	TargetUserName string
	ReplyTo        string // message id to quote (first segment only)
	Contents       []OutboundSegment
}

// OutboundSegment 对齐 internal.OutboundSegment。
type OutboundSegment struct {
	Type      string // "text" | "emoji" | "image"
	Text      string
	EmojiHash string
	ImagePath string
}

// SendResult 对齐 internal.SendResult。
type SendResult struct {
	MessageID string // platform message id of the sent message
	Passive   bool   // true when delivered as a passive reply
	Err       error
}

// ---------------------------------------------------------------------------
// 渠道层最小接口（装配层适配 internal/qq.Channel）
// ---------------------------------------------------------------------------

// ChannelOutbound 是一次渠道发送请求（字段与 qq.Outbound 同形）。
type ChannelOutbound struct {
	// GroupID 是群 openid（群聊必填）。
	GroupID string
	// UserID 是单聊对方 openid（私聊必填）。
	UserID string
	// Text 是文本正文（与 Media 二选一）。
	Text string
	// Media 是已上传的 file_info（与 Text 二选一）。
	Media string
	// TriggerID 是触发这轮回复的入站消息 id（被动模式用）；空表示只能走主动。
	TriggerID string
	// TriggerTime 是触发消息的到达时间（判被动窗口）。
	TriggerTime time.Time
	// QuoteMsgID 是要引用的平台消息 id（可空）。
	QuoteMsgID string
}

// ChannelResult 是一次渠道发送结果（字段与 qq.OutboundResult 同形）。
type ChannelResult struct {
	MessageID string
	Passive   bool
	Reason    string
}

// MediaUploader 是富媒体上传边界（qq.APIClient.UploadMedia 适配）。
type MediaUploader interface {
	// UploadMedia 上传媒体字节，返回 file_info（msg_type 7 凭据）。
	UploadMedia(ctx context.Context, payload []byte, name string) (string, error)
}

// ChannelClient 是渠道层的最小发送边界：
//
//	装配层实现（适配 internal/qq.Channel.Send）：Outbound{GroupID, UserID, Text,
//	Media, TriggerID, TriggerTime, QuoteMsgID} → (*OutboundResult, error)，
//	内部完成 DecideSendMode（被动优先 → 超窗降级主动）与额度/REFIDX 台账。
type ChannelClient interface {
	// SendChannelMessage 走一次双模发送。
	SendChannelMessage(ctx context.Context, outbound ChannelOutbound) (*ChannelResult, error)
	// MediaUploader 提供富媒体上传。
	MediaUploader
}

// SessionInfo 是 BotChatSession 的最小投影。
type SessionInfo struct {
	SessionID string
	Platform  string
	GroupID   string
	GroupName string
	UserID    string
}

// SessionLookup 是会话查询边界（对齐 chat_manager.get_session_by_session_id）。
type SessionLookup interface {
	// GetSession 返回目标会话（不存在返回 nil）。
	GetSession(sessionID string) *SessionInfo
}

// BotIdentity 是机器人身份（对齐 get_bot_account + global_config.bot.nickname）。
type BotIdentity struct {
	Platform string
	UserID   string
	Nickname string
}

// BotIdentityProvider 提供当前机器人身份（未配置返回 false）。
type BotIdentityProvider func(platform string) (BotIdentity, bool)

// HistoryWriter 是发送成功后回写运行时历史的边界
// （对齐 heartflow_manager → runtime.append_sent_message_to_chat_history）。
type HistoryWriter interface {
	// AppendSentMessage 把发送成功的消息按 sourceKind 写回运行时历史。
	AppendSentMessage(sessionID string, sessionMessage *message.SessionMessage, sourceKind string)
}

// MessageStore 是消息入库边界（对齐 MessageUtils.store_message_to_db）。
type MessageStore interface {
	// StoreMessage 把消息写入数据库，返回自增 id。
	StoreMessage(sessionMessage *message.SessionMessage) (int64, error)
}

// EmojiUsageRecorder 在表情真实发送成功后记录使用次数
// （对齐 _record_sent_emoji_usage → emoji_manager.update_emoji_usage_by_hash）。
type EmojiUsageRecorder interface {
	// RecordEmojiUsage 按 hash 记录一次表情使用。
	RecordEmojiUsage(emojiHash string)
}

// ---------------------------------------------------------------------------
// SendService
// ---------------------------------------------------------------------------

// SendService 对齐 send_service 模块：统一出站发送入口。
type SendService struct {
	channel     ChannelClient
	sessions    SessionLookup
	botIdentity BotIdentityProvider
	store       MessageStore
	history     HistoryWriter
	emojiUsage  EmojiUsageRecorder
	typingSpeed float64
	// SegmentDelay 模拟打字间隔（typing=true 时调用；测试可注入替身）。
	// 缺省为 calculate_typing_time × typing_speed。
	SegmentDelay func(processedPlainText string, isEmoji bool) time.Duration
	// Now 注入时钟。
	Now func() time.Time
}

// Options 是 SendService 的注入参数。
type Options struct {
	Channel     ChannelClient
	Sessions    SessionLookup
	BotIdentity BotIdentityProvider
	Store       MessageStore
	History     HistoryWriter
	EmojiUsage  EmojiUsageRecorder
	TypingSpeed float64
	Now         func() time.Time
}

// NewSendService 创建发送服务。
func NewSendService(options Options) *SendService {
	if options.Now == nil {
		options.Now = time.Now
	}
	service := &SendService{
		channel:     options.Channel,
		sessions:    options.Sessions,
		botIdentity: options.BotIdentity,
		store:       options.Store,
		history:     options.History,
		emojiUsage:  options.EmojiUsage,
		typingSpeed: options.TypingSpeed,
		Now:         options.Now,
	}
	service.SegmentDelay = DefaultSegmentDelay(options.TypingSpeed)
	return service
}

// DefaultSegmentDelay mirrors calculate_typing_time × typing_speed
// （复用 replyer 的实现语义：中文 0.3s/字、英文 0.15s/字）。
func DefaultSegmentDelay(typingSpeed float64) func(string, bool) time.Duration {
	return func(text string, isEmoji bool) time.Duration {
		return time.Duration(calculateTypingTimeSeconds(text, typingSpeed, isEmoji) * float64(time.Second))
	}
}

// calculateTypingTimeSeconds 是 calculate_typing_time 的独立实现（与
// internal/chat/replyer/typing_time.go 常数一致，避免 replyer 依赖）。
func calculateTypingTimeSeconds(inputString string, typingSpeed float64, isEmoji bool) float64 {
	const (
		chineseTime = 0.3
		englishTime = 0.15
	)
	chineseChars := 0
	for _, char := range inputString {
		if '\u4e00' <= char && char <= '\u9fff' {
			chineseChars++
		}
	}
	if chineseChars == 1 && len([]rune(strings.TrimSpace(inputString))) == 1 {
		return chineseTime*3 + 0.3
	}
	total := 0.0
	for _, char := range inputString {
		if '\u4e00' <= char && char <= '\u9fff' {
			total += chineseTime
		} else {
			total += englishTime
		}
	}
	if isEmoji {
		total = 1
	}
	if typingSpeed <= 0 {
		return 0
	}
	return total * typingSpeed
}

// ---------------------------------------------------------------------------
// 出站构造 — mirrors _build_outbound_session_message
// ---------------------------------------------------------------------------

// BuildOutboundSessionMessage 构造出站 SessionMessage：
// message_id = "send_api_<ms>"，user_info = 机器人身份，群聊补 GroupInfo，
// processed_plain_text 缺省时按组件摘要生成。
func (s *SendService) BuildOutboundSessionMessage(
	session *SessionInfo,
	sequence *message.MessageSequence,
	processedPlainText string,
	replyMessage *message.SessionMessage,
) (*message.SessionMessage, error) {
	if session == nil {
		return nil, fmt.Errorf("[SendService] 未找到聊天流")
	}
	identity, ok := s.botIdentityFor(session.Platform)
	if !ok {
		return nil, fmt.Errorf("[SendService] 平台 %s 未配置机器人账号，无法发送消息", session.Platform)
	}
	currentTime := s.Now()
	messageID := fmt.Sprintf("send_api_%d", currentTime.UnixMilli())

	outbound := message.NewSessionMessage(messageID, currentTime, session.Platform)
	outbound.MessageInfo.UserInfo = message.NewUserInfo(identity.UserID, identity.Nickname, "")
	if session.GroupID != "" {
		// 组名未知也保持群聊语义（空名不降级私聊）。
		outbound.MessageInfo.GroupInfo = &message.GroupInfo{
			GroupID:   session.GroupID,
			GroupName: strings.TrimSpace(session.GroupName),
		}
	}
	if sequence == nil {
		sequence = &message.MessageSequence{}
	}
	outbound.RawMessage = sequence.Clone()
	outbound.SessionID = session.SessionID
	normalized := strings.TrimSpace(processedPlainText)
	if normalized == "" {
		normalized = BuildProcessedPlainText(outbound)
	}
	outbound.ProcessedPlainText = message.StringPtr(normalized)
	if replyMessage != nil {
		outbound.ReplyTo = message.StringPtr(replyMessage.MessageID)
	}
	flags := detectOutboundFlags(outbound.RawMessage)
	outbound.IsEmoji = flags.isEmoji
	outbound.IsPicture = flags.isPicture
	outbound.IsCommand = flags.isCommand
	outbound.Initialized = true
	return outbound, nil
}

type outboundFlags struct{ isEmoji, isPicture, isCommand bool }

// detectOutboundFlags mirrors _detect_outbound_message_flags。
func detectOutboundFlags(sequence *message.MessageSequence) outboundFlags {
	var flags outboundFlags
	if sequence == nil {
		return flags
	}
	for _, component := range sequence.Components {
		switch component.FormatName() {
		case message.ComponentTypeEmoji:
			flags.isEmoji = true
		case message.ComponentTypeImage:
			flags.isPicture = true
		}
	}
	return flags
}

// BuildProcessedPlainText mirrors _build_processed_plain_text：组件文本摘要
// （日志与打字时长估算用）。
func BuildProcessedPlainText(sessionMessage *message.SessionMessage) string {
	var parts []string
	if sessionMessage == nil || sessionMessage.RawMessage == nil {
		return ""
	}
	for _, component := range sessionMessage.RawMessage.Components {
		switch typed := component.(type) {
		case *message.TextComponent:
			parts = append(parts, typed.Text)
		case *message.ImageComponent:
			parts = append(parts, orPlaceholder(strings.TrimSpace(typed.Content), "[图片]"))
		case *message.EmojiComponent:
			parts = append(parts, orPlaceholder(strings.TrimSpace(typed.Content), "[表情]"))
		case *message.VoiceComponent:
			parts = append(parts, orPlaceholder(strings.TrimSpace(typed.Content), "[语音]"))
		case *message.FileComponent:
			parts = append(parts, typed.ToPlainText())
		case *message.AtComponent:
			parts = append(parts, "@"+typed.DisplayName())
		case *message.ReplyComponent:
			content := ""
			if typed.TargetMessageContent != nil {
				content = *typed.TargetMessageContent
			}
			parts = append(parts, orPlaceholder(content, "[回复消息]"))
		default:
			parts = append(parts, "[自定义消息]")
		}
	}
	return strings.Join(parts, " ")
}

func orPlaceholder(content, placeholder string) string {
	if content == "" {
		return placeholder
	}
	return content
}

// ---------------------------------------------------------------------------
// 发送主链路 — mirrors send_session_message_with_message / _send_via_platform_io
// ---------------------------------------------------------------------------

// SendOptions 是单次发送的行为开关（对齐 send_session_message 关键字参数）。
type SendOptions struct {
	Typing               bool
	SetReply             bool
	ReplyMessageID       string
	StorageMessage       bool // 默认 true 语义由调用方传
	SyncToMaisakaHistory bool
	MaisakaSourceKind    string // 缺省 outbound_send
	TriggerID            string
	TriggerTime          time.Time
}

// SendSessionMessage 发送一条已构造的消息：
// 渠道发送 → 成功回执回填 → 表情使用记录 → 存库 → 回写历史。
// 失败即停（返回错误；多段由调用方逐段驱动）。
func (s *SendService) SendSessionMessage(
	ctx context.Context,
	outbound *message.SessionMessage,
	options SendOptions,
) (*message.SessionMessage, error) {
	if outbound == nil || strings.TrimSpace(outbound.MessageID) == "" {
		return nil, fmt.Errorf("消息缺少 message_id，无法发送")
	}
	session := s.sessionFor(outbound.SessionID)
	if session == nil {
		return nil, fmt.Errorf("[SendService] 未找到聊天流: %s", outbound.SessionID)
	}

	setReply := options.SetReply
	replyMessageID := options.ReplyMessageID
	if setReply && replyMessageID == "" && outbound.ReplyTo != nil {
		replyMessageID = *outbound.ReplyTo
	}
	if setReply && replyMessageID == "" {
		return nil, fmt.Errorf("set_reply=True 时必须提供 reply_message_id")
	}

	// 渠道载荷：文本组件 → Text；emoji/image → 上传媒体得 file_info。
	text, media, err := s.preparePayload(ctx, outbound)
	if err != nil {
		return nil, err
	}

	if options.Typing {
		s.simulateTyping(outbound)
	}

	result, err := s.channel.SendChannelMessage(ctx, ChannelOutbound{
		GroupID:     session.GroupID,
		UserID:      session.UserID,
		Text:        text,
		Media:       media,
		TriggerID:   options.TriggerID,
		TriggerTime: options.TriggerTime,
		QuoteMsgID:  replyMessageID,
	})
	if err != nil {
		return nil, fmt.Errorf("[SendService] 渠道发送失败: %w", err)
	}
	if result == nil || strings.TrimSpace(result.MessageID) == "" {
		return nil, fmt.Errorf("[SendService] 渠道发送未返回消息 id")
	}

	// 成功回执：平台消息 id 回填（对齐 _apply_successful_delivery_receipt）。
	externalMessageID := strings.TrimSpace(result.MessageID)
	outbound.PlatformMessageID = message.StringPtr(externalMessageID)
	if externalMessageID != outbound.MessageID {
		outbound.MessageID = externalMessageID
	}

	// 表情使用记录（对齐 _record_sent_emoji_usage）。
	s.recordEmojiUsage(outbound)

	// 存库（对齐 storage_message 分支）。
	if options.StorageMessage && s.store != nil {
		if _, err := s.store.StoreMessage(outbound); err != nil {
			return nil, fmt.Errorf("[SendService] 存储消息到数据库失败: %w", err)
		}
	}

	// 回写运行时历史（对齐 sync_to_maisaka_history）。
	if options.SyncToMaisakaHistory && s.history != nil {
		sourceKind := options.MaisakaSourceKind
		if sourceKind == "" {
			sourceKind = "outbound_send"
		}
		s.history.AppendSentMessage(outbound.SessionID, outbound, sourceKind)
	}
	return outbound, nil
}

// preparePayload 把 SessionMessage 的首组件转为渠道载荷。
func (s *SendService) preparePayload(ctx context.Context, outbound *message.SessionMessage) (string, string, error) {
	if outbound.RawMessage == nil || len(outbound.RawMessage.Components) == 0 {
		return "", "", fmt.Errorf("[SendService] 消息没有可发送内容")
	}
	switch typed := outbound.RawMessage.Components[0].(type) {
	case *message.TextComponent:
		if strings.TrimSpace(typed.Text) == "" {
			return "", "", fmt.Errorf("[SendService] 文本内容为空")
		}
		return typed.Text, "", nil
	case *message.EmojiComponent:
		media, err := s.uploadMedia(ctx, typed.BinaryHash, typed.BinaryData)
		return "", media, err
	case *message.ImageComponent:
		media, err := s.uploadMedia(ctx, typed.BinaryHash, typed.BinaryData)
		return "", media, err
	default:
		return "", "", fmt.Errorf("[SendService] 不支持的首组件类型: %s", outbound.RawMessage.Components[0].FormatName())
	}
}

func (s *SendService) uploadMedia(ctx context.Context, hash string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("[SendService] 媒体数据为空")
	}
	name := hash
	if name == "" {
		name = "media"
	}
	fileInfo, err := s.channel.UploadMedia(ctx, data, name)
	if err != nil {
		return "", fmt.Errorf("[SendService] 上传媒体失败: %w", err)
	}
	if strings.TrimSpace(fileInfo) == "" {
		return "", fmt.Errorf("[SendService] 上传媒体未返回 file_info")
	}
	return fileInfo, nil
}

func (s *SendService) recordEmojiUsage(outbound *message.SessionMessage) {
	if s.emojiUsage == nil || outbound.RawMessage == nil {
		return
	}
	for _, component := range outbound.RawMessage.Components {
		if emoji, ok := component.(*message.EmojiComponent); ok && strings.TrimSpace(emoji.BinaryHash) != "" {
			s.emojiUsage.RecordEmojiUsage(emoji.BinaryHash)
		}
	}
}

func (s *SendService) simulateTyping(outbound *message.SessionMessage) {
	if s.SegmentDelay == nil {
		return
	}
	plainText := ""
	if outbound.ProcessedPlainText != nil {
		plainText = *outbound.ProcessedPlainText
	}
	if delay := s.SegmentDelay(plainText, outbound.IsEmoji); delay > 0 {
		time.Sleep(delay)
	}
}

func (s *SendService) sessionFor(sessionID string) *SessionInfo {
	if s.sessions == nil {
		return nil
	}
	return s.sessions.GetSession(sessionID)
}

func (s *SendService) botIdentityFor(platform string) (BotIdentity, bool) {
	if s.botIdentity == nil {
		return BotIdentity{}, false
	}
	return s.botIdentity(platform)
}

// ---------------------------------------------------------------------------
// guided_reply 历史回写 — mirrors runtime.append_sent_message_to_chat_history
// ---------------------------------------------------------------------------

// RuntimeHistorySink 是 runtime 的历史追加边界（tools.ReplyRuntime 的方法子集）。
type RuntimeHistorySink interface {
	// AppendChatHistory mirrors runtime._chat_history.append。
	AppendChatHistory(entries ...chatcontext.LLMContextMessage)
}

// RuntimeFocusChecker 判断 focus mode 是否对当前会话生效（include_chat_id）。
type RuntimeFocusChecker interface {
	// FocusModeActiveForChat mirrors _is_focus_mode_active_for_current_chat。
	FocusModeActiveForChat() bool
}

// AppendSentMessageToHistory mirrors MaisakaHeartFlowChatting.
// append_sent_message_to_chat_history：构造 planner 前缀 + SessionBackedMessage
// 写回运行时历史（sourceKind 通常是 guided_reply）。
func AppendSentMessageToHistory(
	sink RuntimeHistorySink,
	focus RuntimeFocusChecker,
	sessionMessage *message.SessionMessage,
	sourceKind string,
) bool {
	if sink == nil || sessionMessage == nil {
		return false
	}
	if strings.TrimSpace(sourceKind) == "" {
		sourceKind = "guided_reply"
	}
	userInfo := sessionMessage.MessageInfo.UserInfo
	speakerName := userInfo.DisplayName()

	includeChatID := false
	if focus != nil {
		includeChatID = focus.FocusModeActiveForChat()
	}
	quoteIDs := chatcontext.ExtractQuoteIDsFromMessageSequence(sessionMessage.RawMessage)
	plannerPrefix := chatcontext.BuildPlannerPrefix(
		sessionMessage.Timestamp,
		speakerName,
		orEmpty(userInfo.UserCardname),
		sessionMessage.MessageID,
		sessionMessage.SessionID,
		quoteIDs,
		!sessionMessage.IsNotify && sessionMessage.MessageID != "",
		includeChatID,
		sourceKind == "guided_reply",
	)
	rawSequence := chatcontext.BuildPrefixedMessageSequence(sessionMessage.RawMessage, plannerPrefix)
	visibleText := chatcontext.BuildSessionMessageVisibleText(
		sessionMessage,
		sessionMessage.RawMessage,
		sourceKind != "guided_reply",
	)
	historyMessage := chatcontext.SessionBackedFromSessionMessage(
		sessionMessage,
		rawSequence,
		visibleText,
		sourceKind,
	)
	sink.AppendChatHistory(historyMessage)
	return true
}

func orEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// ---------------------------------------------------------------------------
// internal.Sender 契约实现（Send(ctx, req SendRequest) ([]SendResult, error)）
// ---------------------------------------------------------------------------

// Compile-time guard: the service below implements the contracts.Sender shape
// through ContractAdapter.
type contractShape interface {
	Send(ctx context.Context, req SendRequest) ([]SendResult, error)
}

// ContractAdapter 把 SendService 适配为 internal.Sender 契约
// （逐段发送：首段引用 ReplyTo；任何一段失败即停止并回执失败）。
type ContractAdapter struct {
	Service *SendService
}

// NewContractAdapter 创建契约适配器。
func NewContractAdapter(service *SendService) *ContractAdapter {
	return &ContractAdapter{Service: service}
}

// Send implements internal.Sender：
// 每段构造出站消息（emoji/image 段带二进制）→ SendSessionMessage；
// 任何一段失败即停止并回执失败（对齐 reply 工具的失败即停语义）。
func (a *ContractAdapter) Send(ctx context.Context, req SendRequest) ([]SendResult, error) {
	session := a.Service.sessionFor(req.SessionID)
	if session == nil {
		return nil, fmt.Errorf("[SendService] 未找到聊天流: %s", req.SessionID)
	}
	if len(req.Contents) == 0 {
		return nil, fmt.Errorf("[SendService] 发送内容为空")
	}
	results := make([]SendResult, 0, len(req.Contents))
	for index, segment := range req.Contents {
		var sequence *message.MessageSequence
		switch segment.Type {
		case "text":
			sequence = message.NewMessageSequence(message.NewTextComponent(segment.Text))
		case "emoji":
			sequence = message.NewMessageSequence(message.NewEmojiComponent(nil, segment.EmojiHash))
		case "image":
			sequence = message.NewMessageSequence(message.NewImageComponent(nil, segment.ImagePath))
		default:
			return results, fmt.Errorf("[SendService] 未知出站段类型: %s", segment.Type)
		}
		outbound, err := a.Service.BuildOutboundSessionMessage(session, sequence, "", nil)
		if err != nil {
			return results, err
		}
		// 首段引用 ReplyTo（对齐 reply 工具：第一段 quote 目标）；后续段直接发、
		// TriggerID 仅首段传（后续段由渠道层按被动窗口自动降级主动）。
		setReply := index == 0 && strings.TrimSpace(req.ReplyTo) != ""
		replyTo := ""
		if setReply {
			replyTo = req.ReplyTo
		}
		sent, err := a.Service.SendSessionMessage(ctx, outbound, SendOptions{
			SetReply:             setReply,
			ReplyMessageID:       replyTo,
			StorageMessage:       true,
			SyncToMaisakaHistory: true,
			MaisakaSourceKind:    "guided_reply",
			TriggerID:            replyTo,
		})
		if err != nil {
			// 失败即停：记录失败段并停止后续发送。
			results = append(results, SendResult{Err: err})
			return results, err
		}
		results = append(results, SendResult{
			MessageID: sent.MessageID,
			Passive:   sent.PlatformMessageID != nil && *sent.PlatformMessageID == sent.MessageID,
		})
	}
	return results, nil
}

var _ contractShape = (*ContractAdapter)(nil)
