package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	chatruntime "oh-my-qqbot/internal/chat/runtime"
	"oh-my-qqbot/internal/message"
)

// This file mirrors 参考/MaiBot/src/maisaka/builtin_tool/reply.py
// (behaviour is authoritative there): reply 内置工具，最核心。
//
// 发送流程：第一条 quote 目标消息、其余顺序发；每条间隔 =
// calculate_typing_time × typing_speed（sender 侧模拟，由 Sender 契约承载）；
// 任何一条失败即停止并回执失败。成功回执文案照抄 Python。

// ReplyStyleMessages mirrors _build_requested_reply_style_message 的 style_messages。
var ReplyStyleMessages = map[string]string{
	"简短表达": "请简短的回复，允许句子残缺，奇怪表达，倒装，省略，符合口语习惯，符合省力随意回复习惯",
	"正常回复": "",
	"长回复":  "可以针对问题做出较为详细的评论和说明",
}

// replyToolInternalArguments mirrors _REPLY_TOOL_INTERNAL_ARGUMENTS。
var replyToolInternalArguments = map[string]bool{
	"msg_id": true, "set_quote": true, "_reply_id": true, "_plugin_reply_prompt": true,
}

// duplicateTargetReplyReminderArg mirrors _DUPLICATE_TARGET_REPLY_REMINDER_ARG。
const duplicateTargetReplyReminderArg = "_duplicate_target_reply_reminder"

// DuplicateTargetReplyReminderTemplate mirrors _DUPLICATE_TARGET_REPLY_REMINDER_TEMPLATE
// （文案照抄）。
const DuplicateTargetReplyReminderTemplate = "你刚刚已经回复过这条消息，你刚刚的发言是：“{previous_reply}”\n" +
	"你现在想再次回复这条消息，进行补充，注意请不要和之前你的发言重复。"

// ---------------------------------------------------------------------------
// reply 工具依赖（结构体注入；不 import qq/config）
// ---------------------------------------------------------------------------

// OutboundSegment mirrors one outbound piece after post-processing.
type OutboundSegment struct {
	// Text 是该段文本（PostProcessReplySegments 产出）。
	Text string
	// QuotePrevious 表示该条需引用上一条（错别字纠正段）。
	QuotePrevious bool
}

// ReplySender 是 reply 工具的最小发送边界（与 internal/contracts.Sender 兼容的
// 方法子集；由 internal/chat/sender(P7) 实现）。逐条发送：第一条 quote 目标，
// 每条间隔由 sender 按 typing_time × typing_speed 模拟。
type ReplySender interface {
	// SendReplySegment 发送一段回复。
	//   - segmentIndex == 0 且 setQuote 时引用 replyToMessageID；
	//   - quotePrevious 时引用上一条已发送消息（previousMessageID）。
	SendReplySegment(ctx context.Context, request ReplySendRequest) (sentMessageID string, err error)
}

// ReplySendRequest describes one outbound segment for ReplySender.
type ReplySendRequest struct {
	SessionID         string
	SegmentIndex      int
	Text              string
	SetQuote          bool
	ReplyToMessageID  string // index==0 时引用的目标消息
	QuotePrevious     bool   // 引用上一条已发送消息
	PreviousMessageID string
	AttachAtUserIDs   []string
	AttachEmojiIndex  int // 0 表示无
	IsGroupSession    bool
	GroupID           string
	TargetUserID      string
	TargetUserName    string
}

// ReplyGenerator is the replyer boundary the tool consumes
// (BaseMaisakaReplyGenerator.GenerateReplyWithContext 的窄接口).
type ReplyGenerator interface {
	// GenerateReply 生成可见回复文本（含表达选择与重试）。
	GenerateReply(ctx context.Context, request ReplyGenerateRequest) (ReplyGenerateResult, error)
}

// ReplyGenerateRequest mirrors the generator inputs.
type ReplyGenerateRequest struct {
	StreamID      string
	ReplyMessage  *message.SessionMessage
	ReplyReason   string
	ChatHistory   []chatcontext.LLMContextMessage
	ReplyToolArgs map[string]any
}

// ReplyGenerateResult mirrors ReplyGenerationResult (trimmed).
type ReplyGenerateResult struct {
	Success      bool
	ResponseText string
	ErrorMessage string
}

// ReplyPostProcessor mirrors context.post_process_reply_segments（后处理入口）。
type ReplyPostProcessor interface {
	// PostProcessSegments 处理回复文本并保留纠正段引用提示。
	PostProcessSegments(replyText string, skipPostProcess bool, enableSplitter bool, enableChineseTypo bool) []OutboundSegment
}

// ReplyRuntime 是 reply 工具需要的 runtime 面（由 P4a runtime + 装配层适配）。
type ReplyRuntime interface {
	// SessionID / IsGroupSession / LogPrefix。
	SessionID() string
	IsGroupSession() bool
	LogPrefix() string
	// ChatHistory mirrors runtime._chat_history（重复回复检测）。
	ChatHistory() []chatcontext.LLMContextMessage
	// FindSourceMessageByID mirrors find_source_message_by_id.
	FindSourceMessageByID(messageID string) *message.SessionMessage
	// RecordPlannerReply mirrors record_planner_reply → scheduler.record_reply()。
	RecordPlannerReply()
	// AppendChatHistory mirrors runtime._chat_history.append（guided_reply 写回）。
	AppendChatHistory(entries ...chatcontext.LLMContextMessage)
}

// ReplyDependencies 聚合 reply 工具的全部注入依赖。
type ReplyDependencies struct {
	Runtime       ReplyRuntime
	Sender        ReplySender
	Generator     ReplyGenerator
	PostProcessor ReplyPostProcessor
	// TypingSpeed mirrors response_post_process.typing_speed（sender 用；
	// 这里传递给 sender 的 per-segment sleep 预估由 sender 实现，值为配置直传）。
	TypingSpeed float64
	// SegmentDelay 在发送段之间等待 typing_time×speed（由工具层驱动，
	// 与 Python `typing=index > 0` 的 send_service 行为一致；测试可注入替身）。
	SegmentDelay func(segmentText string, isEmoji bool) time.Duration
	// OnMessageSent 在每段发送成功后被调用（存库 + 写回运行时历史 guided_reply）。
	OnMessageSent func(segmentText string, sentMessageID string)
	// BotNameFn mirrors global_config.bot.nickname（回执文案使用）。
	BotNameFn func() string
	// IsBotSelfFn mirrors utils.is_bot_self。
	IsBotSelfFn func(platform string, userID string) bool
	// Now mirrors datetime.now()。
	Now func() time.Time
}

// GetReplyToolSpec mirrors get_tool_spec（参数 schema 照抄）。
func GetReplyToolSpec(deps *ReplyDependencies, useNewEmojiSendLogic bool, useVectorExpression bool) ToolSpec {
	properties := map[string]any{
		"msg_id": map[string]any{
			"type":        "string",
			"description": "要回复的消息msg_id。",
		},
		"set_quote": map[string]any{
			"type":        "boolean",
			"description": "以引用回复的方式发送这条回复，当发言人数过多，聊天比较乱时使用。",
			"default":     true,
		},
		"reply_reference": map[string]any{
			"type":        "string",
			"description": "有助于回复的信息，包括当前聊天状态、人物关系、事实信息、回忆信息。",
		},
		"reply_style": map[string]any{
			"type":        "string",
			"description": "可选。控制本次回复的篇幅和表达方式；正常回复不会附加额外要求。",
			"enum":        []string{"简短表达", "正常回复", "长回复"},
		},
	}
	if useVectorExpression {
		properties["expression_intent"] = map[string]any{
			"type": "object",
			"description": "可选。给 replyer 表达方式选择使用的结构化意图，不是回复正文。" +
				"当这次回复需要特定语气、场景或话术时填写，避免表达选择只按关键词匹配。",
			"properties": map[string]any{
				"focus":     map[string]any{"type": "string", "description": "本次表达主要应该贴合的目标消息、片段或话题。"},
				"reply_act": map[string]any{"type": "string", "description": "这次回复要完成的动作，例如澄清、安抚、调侃、追问、解释边界。"},
				"scene":     map[string]any{"type": "string", "description": "当前表达场景，例如技术排查、截图分享、撒娇玩笑、价格询问。"},
				"tone":      map[string]any{"type": "string", "description": "期望语气，例如轻松、可靠、吐槽、委婉、简短肯定。"},
				"prefer":    map[string]any{"type": "array", "description": "优先考虑的表达类型或话术倾向。", "items": map[string]any{"type": "string"}},
				"avoid":     map[string]any{"type": "array", "description": "需要避免的表达类型、误判方向或不该注入的具体结论。", "items": map[string]any{"type": "string"}},
			},
		}
	}
	properties["attach_pic"] = map[string]any{
		"type": "array",
		"description": "可选。随本次回复附加一张或多张上下文图片。每项使用 msg_id + index，" +
			"或使用 media_index=tool_result:<call_id>:<item_index> 指向工具返回媒体。",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"msg_id":      map[string]any{"type": "string", "description": "图片所在的消息编号。", "default": ""},
				"media_index": map[string]any{"type": "string", "description": "工具返回媒体索引，例如 tool_result:call_x:1；与 msg_id 二选一。", "default": ""},
				"index":       map[string]any{"type": "integer", "description": "同一消息中的图片序号，从 0 开始。", "default": 0},
			},
		},
		"default": []any{},
	}
	if useNewEmojiSendLogic {
		properties["attach_emoji"] = map[string]any{
			"type":        "integer",
			"minimum":     1,
			"description": "可选。从 show_emoji_list 的拼图选择一个表情包，填写图片序号，在文字后单独发送。",
		}
	}
	if deps != nil && deps.Runtime != nil {
		// enable_reply_at 时才注入 attach_at（照抄 get_tool_spec 分支）。
		properties["attach_at"] = map[string]any{
			"type":        "array",
			"description": "可选。随本次回复 at 一个或多个目标消息的发送者，填写目标 msg_id。",
			"items":       map[string]any{"type": "string"},
			"default":     []any{},
		}
	}
	return ToolSpec{
		Name:        "reply",
		Description: "根据当前思考生成并发送可见回复；需要向用户展示上下文或工具返回的图片时，使用 attach_pic 随回复附加图片。",
		ParametersSchema: map[string]any{
			"type":       "object",
			"properties": properties,
			"required":   []string{"msg_id"},
		},
		ProviderName: "maisaka_builtin",
		ProviderType: "builtin",
		Enabled:      true,
		Stage:        StageAction,
		Visibility:   VisibilityVisible,
	}
}

// HandleReplyTool mirrors handle_tool（reply）。
func HandleReplyTool(ctx context.Context, deps *ReplyDependencies, invocation ToolInvocation) ToolResult {
	toolName := invocation.ToolName
	rawArguments := invocation.Arguments
	if rawArguments == nil {
		rawArguments = map[string]any{}
	}

	targetMessageID := strings.TrimSpace(toString(rawArguments["msg_id"]))
	setQuote := toBool(rawArguments["set_quote"], true)

	attachAtPresent := rawArguments["attach_at"] != nil && !isEmptyList(rawArguments["attach_at"])
	_ = attachAtPresent

	if targetMessageID == "" {
		return BuildFailureResult(toolName, "reply 工具需要提供有效的 `msg_id` 参数。", nil, nil)
	}

	runtime := deps.Runtime
	targetMessage := runtime.FindSourceMessageByID(targetMessageID)
	if targetMessage == nil {
		return BuildFailureResult(
			toolName,
			fmt.Sprintf("未找到要回复的目标消息，msg_id=%s", chatcontext.ToDisplayMessageID(targetMessageID)),
			nil, nil)
	}

	// 重复回复同一目标 → 附加提醒（照抄 _find_recent_reply_to_target / _with_duplicate_target_reply_reminder）
	replyToolArgs := map[string]any{}
	for key, value := range rawArguments {
		if replyToolInternalArguments[key] {
			continue
		}
		replyToolArgs[key] = value
	}
	previousTargetReply := findRecentReplyToTarget(runtime.ChatHistory(), targetMessageID)
	if previousTargetReply != "" {
		replyToolArgs[duplicateTargetReplyReminderArg] = strings.ReplaceAll(
			DuplicateTargetReplyReminderTemplate, "{previous_reply}", previousTargetReply)
	}

	// 生成可见回复
	enableReplyQuote := true
	replyReason := strings.TrimSpace(toString(rawArguments["reply_reference"]))
	if replyReason == "" {
		replyReason = invocation.Reasoning
	}
	generateResult, err := deps.Generator.GenerateReply(ctx, ReplyGenerateRequest{
		StreamID:      runtime.SessionID(),
		ReplyMessage:  targetMessage,
		ReplyReason:   replyReason,
		ChatHistory:   runtime.ChatHistory(),
		ReplyToolArgs: replyToolArgs,
	})
	if err != nil || !generateResult.Success || strings.TrimSpace(generateResult.ResponseText) == "" {
		return BuildFailureResult(toolName, "生成可见回复失败。", nil, map[string]any{"error": generateResult.ErrorMessage})
	}
	replyText := strings.TrimSpace(generateResult.ResponseText)

	// 后处理（断句/错别字/颜文字）
	skipPostProcess := toBool(rawArguments["skip_post_process"], false)
	segments := deps.PostProcessor.PostProcessSegments(replyText, skipPostProcess, true, true)

	// 逐条发送：第一条 quote 目标、其余顺序发；任何一条失败即停
	effectiveSetQuote := setQuote && enableReplyQuote
	sentMessageIDs := make([]string, 0, len(segments))
	sendResults := make([]map[string]any, 0, len(segments))
	previousSentMessageID := ""
	sent := false
	for index, segment := range segments {
		segmentSetQuote, replyToID, quotePreviousID := resolveSegmentReplyContext(
			index, segment.QuotePrevious, effectiveSetQuote, targetMessageID, previousSentMessageID)
		sentMessageID, sendErr := deps.Sender.SendReplySegment(ctx, ReplySendRequest{
			SessionID:         runtime.SessionID(),
			SegmentIndex:      index,
			Text:              segment.Text,
			SetQuote:          segmentSetQuote,
			ReplyToMessageID:  replyToID,
			QuotePrevious:     quotePreviousID != "",
			PreviousMessageID: quotePreviousID,
			IsGroupSession:    runtime.IsGroupSession(),
		})
		if sendErr != nil || sentMessageID == "" {
			sendResults = append(sendResults, map[string]any{
				"index": index, "segment": segment.Text,
				"set_quote": segmentSetQuote, "success": false,
			})
			sent = false
			break
		}
		if sentMessageID != "" {
			sentMessageIDs = append(sentMessageIDs, sentMessageID)
		}
		sendResults = append(sendResults, map[string]any{
			"index": index, "segment": segment.Text,
			"set_quote": segmentSetQuote, "success": true,
			"message_id": sentMessageID,
		})
		previousSentMessageID = sentMessageID
		sent = true
		if deps.OnMessageSent != nil {
			deps.OnMessageSent(segment.Text, sentMessageID)
		}
		// 每条间隔 = calculate_typing_time × typing_speed（首条前不打字，对齐 typing=index>0）
		if index < len(segments)-1 && deps.SegmentDelay != nil {
			delay := deps.SegmentDelay(segments[index+1].Text, false)
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return BuildFailureResult(toolName, "发送可见回复时发生异常。", nil, nil)
				}
			}
		}
	}

	if !sent {
		return BuildFailureResult(toolName, "可见回复生成成功，但发送失败。", map[string]any{
			"msg_id":              targetMessageID,
			"set_quote":           setQuote,
			"effective_set_quote": effectiveSetQuote,
			"reply_segments":      segmentTexts(segments),
			"send_results":        sendResults,
		}, nil)
	}

	combinedReplyText := strings.Join(segmentTexts(segments), "")
	targetUserInfo := targetMessage.MessageInfo.UserInfo
	botName := strings.TrimSpace(deps.botName())
	replyReceipt := ""
	targetUserName := ""
	if deps.isBotSelf()(targetMessage.Platform, targetUserInfo.UserID) {
		// 自身消息只是回复的引用锚点，不能把其作者当作实际收件人。
		replyReceipt = fmt.Sprintf("\"%s\"已生成并向当前会话发送了回复\"%s\"", botName, combinedReplyText)
	} else {
		targetUserName = targetUserInfo.DisplayName()
		replyReceipt = fmt.Sprintf("\"%s\"已生成并向\"%s\"发送了回复\"%s\"", botName, targetUserName, combinedReplyText)
	}

	// 发送成功后 record_reply()（动态门控统计）。
	runtime.RecordPlannerReply()

	metadata := map[string]any{
		"sent_message_ids": sentMessageIDs,
		"send_results":     sendResults,
	}
	return BuildSuccessResult(toolName, replyReceipt, map[string]any{
		"msg_id":              targetMessageID,
		"set_quote":           setQuote,
		"effective_set_quote": effectiveSetQuote,
		"reply_text":          combinedReplyText,
		"reply_segments":      segmentTexts(segments),
		"send_results":        sendResults,
		"target_user_name":    targetUserName,
	}, metadata, nil)
}

// botName mirrors global_config.bot.nickname（默认回退 MaiSaka 同源常量）。
func (d *ReplyDependencies) botName() string {
	if d.BotNameFn != nil {
		return d.BotNameFn()
	}
	return ""
}

// isBotSelf mirrors utils.is_bot_self（未注入时恒为 false，对齐 Python 未配置账号）。
func (d *ReplyDependencies) isBotSelf() func(platform string, userID string) bool {
	if d.IsBotSelfFn != nil {
		return d.IsBotSelfFn
	}
	return func(string, string) bool { return false }
}

// resolveSegmentReplyContext mirrors _resolve_segment_reply_context.
func resolveSegmentReplyContext(
	index int,
	quotePrevious bool,
	effectiveSetQuote bool,
	targetMessageID string,
	previousSentMessageID string,
) (setQuote bool, replyToID string, quotePreviousID string) {
	if index == 0 {
		return effectiveSetQuote, targetMessageID, ""
	}
	if quotePrevious && previousSentMessageID != "" {
		return true, "", previousSentMessageID
	}
	return false, targetMessageID, ""
}

// findRecentReplyToTarget mirrors _find_recent_reply_to_target:
// 查找最近一次已经引用回复过目标消息的 bot 发言。
func findRecentReplyToTarget(chatHistory []chatcontext.LLMContextMessage, targetMessageID string) string {
	normalizedTargetMessageID := strings.TrimSpace(targetMessageID)
	if normalizedTargetMessageID == "" {
		return ""
	}
	for index := len(chatHistory) - 1; index >= 0; index-- {
		entry := chatHistory[index]
		sessionBacked, ok := entry.(*chatcontext.SessionBackedMessage)
		if !ok {
			continue
		}
		if sessionBacked.SourceKind != chatcontext.SourceGuidedReply {
			continue
		}
		quoteIDs := chatcontext.ExtractQuoteIDsFromMessageSequence(sessionBacked.RawMessage)
		contains := false
		for _, quoteID := range quoteIDs {
			if quoteID == normalizedTargetMessageID {
				contains = true
				break
			}
		}
		if !contains {
			continue
		}
		replyText := extractGuidedReplyText(sessionBacked)
		if replyText != "" {
			return replyText
		}
	}
	return ""
}

// extractGuidedReplyText mirrors _extract_guided_reply_text.
func extractGuidedReplyText(message *chatcontext.SessionBackedMessage) string {
	plainText := strings.TrimSpace(message.ProcessedPlainText())
	_, body, _ := chatcontext.ParseSpeakerContent(plainText)
	normalizedText := strings.TrimSpace(body)
	if normalizedText == "" {
		normalizedText = strings.TrimSpace(chatcontext.BuildVisibleTextFromSequence(message.RawMessage))
	}
	return strings.Join(strings.Fields(normalizedText), " ")
}

// AppendGuidedReplyToChatHistory mirrors
// BuiltinToolRuntimeContext.append_guided_reply_to_chat_history:
// 将引导回复写回 Maisaka 历史（无原始 SessionMessage 的发送路径使用）。
func AppendGuidedReplyToChatHistory(
	runtime ReplyRuntime,
	botName string,
	replyText string,
	now func() time.Time,
) {
	timestamp := time.Now()
	if now != nil {
		timestamp = now()
	}
	historyMessage := chatcontext.BuildSessionBackedTextMessage(
		botName,
		replyText,
		timestamp,
		chatcontext.SourceGuidedReply,
		"",
		"",
		runtime.SessionID(),
		nil,
		false,
		false,
		true,
	)
	runtime.AppendChatHistory(historyMessage)
}

// ---------------------------------------------------------------------------
// 小工具函数
// ---------------------------------------------------------------------------

func toString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func toBool(value any, fallback bool) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		normalized := strings.ToLower(strings.TrimSpace(v))
		switch normalized {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		return fallback
	default:
		return fallback
	}
}

func isEmptyList(value any) bool {
	switch v := value.(type) {
	case []any:
		return len(v) == 0
	case []string:
		return len(v) == 0
	case string:
		return strings.TrimSpace(v) == ""
	case nil:
		return true
	}
	return false
}

func segmentTexts(segments []OutboundSegment) []string {
	texts := make([]string, 0, len(segments))
	for _, segment := range segments {
		texts = append(texts, segment.Text)
	}
	return texts
}

// argsToJSON is a debugging helper mirroring json.dumps of tool args.
func argsToJSON(args map[string]any) string {
	encoded, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// 编译期对齐检查：ReplyDependencies 的 Runtime 必须同时满足历史追加面。
var _ interface {
	AppendChatHistory(...chatcontext.LLMContextMessage)
} = (chatRuntimeHistoryAppender)(nil)

type chatRuntimeHistoryAppender interface {
	AppendChatHistory(entries ...chatcontext.LLMContextMessage)
}

// 防止 chatruntime 未使用告警（规划器装配层会引用其类型常量）。
var _ = chatruntime.MaxInternalRounds
