package replyer

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/message"
	"oh-my-qqbot/internal/prompt"
)

// This file mirrors 参考/MaiBot/src/chat/replyer/maisaka_generator_base.py +
// maisaka_generator.py (behaviour is authoritative there):
// replyer prompt 组装（人设/风格/历史/目标消息块/附件提示）、重试与表达选择接入。
//
// LLM 调用通过包内最小接口（ChatFunc / SubAgentRunner / SplitterChatFunc）注入，
// 本包不 import internal/llm 的客户端类型（只用其 ContextItem 数据模型）。

// ChatFunc is the minimal LLM boundary of this package: one replyer-task chat
// request returning the completion text (Python LLMServiceClient.
// generate_response 的等价面). Mirrors express_model in maisaka_generator_base.
type ChatFunc func(ctx context.Context, request ReplyerChatRequest) (string, error)

// ReplyerChatRequest mirrors the pieces of generate_response_with_context the
// replyer actually controls: full request items plus the task selection knobs.
type ReplyerChatRequest struct {
	// TaskName mirrors the request task (default "replyer").
	TaskName string
	// RequestType mirrors request_type (default "maisaka.replyer").
	RequestType string
	// SessionID is the owning chat session.
	SessionID string
	// Items is the full request message list (system/history/user injections).
	Items []*llm.ContextItem
	// MaxTokens / Temperature are optional generation overrides (0 = unset).
	MaxTokens   int
	Temperature float64
	// Context carries cancellation from the tool layer.
	Context context.Context
}

// SubAgentRunner mirrors SubAgentRunner in maisaka_expression_selector:
// 运行一个临时子代理并返回文本（expression_selector 子代理）。
type SubAgentRunner func(ctx context.Context, systemPrompt string) (string, error)

// GeneratorConfig mirrors the global_config fields the generator reads
// (bot / personality / chat.reply_style / experimental / expression). Filled by
// the tool layer; the replyer package never imports internal/config.
type GeneratorConfig struct {
	// Bot
	BotNickname string
	AliasNames  []string
	// Personality
	Personality         string
	ReplyStyle          string
	MultipleReplyStyle  []string
	MultipleProbability float64
	// Experimental.EmotionTrait ("rational_calm"|"neutral"|"sentimental").
	EmotionTrait string
	// Chat.ReplyStyle group/private prompts and per-chat extras.
	GroupChatPrompt   string
	PrivateChatPrompt string
	// ChatPrompts 是 chat.reply_style.chat_prompts（按平台/目标匹配的额外注意事项）。
	ChatPrompts []ChatPromptItem
	// ExpressionUseVector mirrors expression.use_vector_expression
	// (reply 工具是否注入 expression_intent 参数)。
	ExpressionUseVector bool
	// IsBotSelf mirrors utils.is_bot_self (由装配层注入账号信息；nil 时恒为 false)。
	IsBotSelf func(platform string, userID string) bool
}

// ChatPromptItem mirrors chat.reply_style.chat_prompts entries.
type ChatPromptItem struct {
	Platform string
	ItemID   string
	RuleType string // "group" | "private"
	Prompt   string
}

// ReplyContext mirrors MaisakaReplyContext.
type ReplyContext struct {
	ExpressionHabits      string
	SelectedExpressionIDs []int64
	SelectedExpressions   []ExpressionCandidate
}

// ExpressionCandidate mirrors one expression candidate row (id/situation/style/count).
type ExpressionCandidate struct {
	ID        int64  `json:"id"`
	Situation string `json:"situation"`
	Style     string `json:"style"`
	Count     int    `json:"count"`
}

// ExpressionSource is the candidate provider of the expression selector
// (injected: learners/expression 包后续接 DB 与向量索引)。
type ExpressionSource interface {
	// LoadAllCandidates mirrors _load_all_expression_candidates.
	LoadAllCandidates(sessionID string) []ExpressionCandidate
	// BuildVectorCandidatePool mirrors _build_expression_candidate_pool's
	// vector path; ok=false means fall back to random candidates.
	BuildVectorCandidatePool(ctx context.Context, sessionID string, queryText string, all []ExpressionCandidate, poolSize int) ([]ExpressionCandidate, bool)
	// UseVectorExpression mirrors _use_vector_candidate_pool.
	UseVectorExpression() bool
}

// ReplyRequest mirrors generate_reply_with_context's used arguments.
type ReplyRequest struct {
	StreamID        string
	ReplyMessage    *message.SessionMessage
	ReplyReason     string
	ChatHistory     []chatcontext.LLMContextMessage
	ReplyToolArgs   map[string]any
	ExpressionHabit string // 外部直接给定的表达习惯（可选）
	// EnableVisualMessage mirrors _resolve_enable_visual_message (model visual).
	EnableVisualMessage bool
}

// ReplyGenerationResult mirrors ReplyGenerationResult (trimmed to used fields).
type ReplyGenerationResult struct {
	Success               bool
	ResponseText          string
	ErrorMessage          string
	SelectedExpressionIDs []int64
	SelectedExpressions   []ExpressionCandidate
	// RequestMessages 是最后一次请求的 Items（监控/调试用）。
	RequestMessages []*llm.ContextItem
}

// replyerMaxHookRetries mirrors REPLYER_MAX_HOOK_RETRIES (重试上限)。
const replyerMaxHookRetries = 3

// normalizeContent mirrors _normalize_content.
func normalizeContent(content string, limit int) string {
	normalized := strings.Join(strings.Fields(content), " ")
	if len([]rune(normalized)) > limit {
		return string([]rune(normalized)[:limit]) + "..."
	}
	return normalized
}

// BaseMaisakaReplyGenerator mirrors BaseMaisakaReplyGenerator.
type BaseMaisakaReplyGenerator struct {
	ChatStreamSessionID string
	ChatStreamIsGroup   bool
	RequestType         string
	Config              GeneratorConfig
	// PromptManager 渲染 prompts/zh-CN/maisaka_replyer.prompt（照抄模板）。
	PromptManager *prompt.Manager
	// ChatFunc 是 replyer 任务模型（照抄 express_model）。
	ChatFunc ChatFunc
	// SplitterChatFunc 是 fast_model 任务模型（LLM 断句路径；nil 时回退规则断句）。
	SplitterChatFunc ChatFunc
	// ExpressionSource 提供表达候选（nil 时跳过表达选择）。
	ExpressionSource ExpressionSource
	// PostProcess 是后处理配置（splitter/typo/typing 参数）。
	PostProcess PostProcessOptions
	// Now mirrors datetime.now()（测试可注入）。
	Now func() time.Time
}

// NewBaseMaisakaReplyGenerator mirrors __init__.
func NewBaseMaisakaReplyGenerator(
	chatStreamSessionID string,
	chatStreamIsGroup bool,
	config GeneratorConfig,
	promptManager *prompt.Manager,
	chatFunc ChatFunc,
) *BaseMaisakaReplyGenerator {
	if config.BotNickname == "" {
		config.BotNickname = "麦麦"
	}
	return &BaseMaisakaReplyGenerator{
		ChatStreamSessionID: chatStreamSessionID,
		ChatStreamIsGroup:   chatStreamIsGroup,
		RequestType:         "maisaka.replyer",
		Config:              config,
		PromptManager:       promptManager,
		ChatFunc:            chatFunc,
	}
}

func (g *BaseMaisakaReplyGenerator) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// BuildPersonalityPrompt mirrors _build_personality_prompt:
// 人设拼接 `你的名字是{bot_name}{aliases}。` + 人格 + 情绪后缀（PERSONALITY_EMOTION_SUFFIXES 照抄）。
func (g *BaseMaisakaReplyGenerator) BuildPersonalityPrompt() string {
	defer func() { _ = recover() }()
	botName := g.Config.BotNickname
	aliasNames := g.Config.AliasNames
	botAliases := ""
	if len(aliasNames) > 0 {
		botAliases = "，也有人叫你" + strings.Join(aliasNames, ",")
	}

	promptPersonality := strings.TrimSpace(g.Config.Personality)
	if promptPersonality == "" {
		promptPersonality = "是人类。"
	}

	promptLines := []string{fmt.Sprintf("你的名字是%s%s。", botName, botAliases), promptPersonality}
	emotionSuffix := buildPersonalityEmotionSuffix(g.Config.EmotionTrait)
	if emotionSuffix != "" {
		promptLines = append(promptLines, emotionSuffix)
	}
	return strings.Join(promptLines, "\n")
}

// buildPersonalityEmotionSuffix mirrors build_personality_emotion_suffix
// (PERSONALITY_EMOTION_SUFFIXES 文案照抄)。
func buildPersonalityEmotionSuffix(emotionTrait string) string {
	switch emotionTrait {
	case "rational_calm":
		return "你整体理性冷静，回应时更偏向客观、克制和清晰判断，少用强烈情绪表达。"
	case "sentimental":
		return "你更敏感细腻，容易被聊天氛围触动，会自然表现出一点惆怅、共情或情绪波动，但不要过度煽情。"
	default: // "neutral"
		return ""
	}
}

// SelectReplyStyle mirrors _select_reply_style.
func (g *BaseMaisakaReplyGenerator) SelectReplyStyle() string {
	return g.Config.ReplyStyle
}

// SelectTemporaryReplyStyle mirrors _select_temporary_reply_style:
// 按配置概率选择本次回复的一次性备用表达风格。
func (g *BaseMaisakaReplyGenerator) SelectTemporaryReplyStyle() string {
	candidateStyles := make([]string, 0)
	for _, style := range g.Config.MultipleReplyStyle {
		trimmed := strings.TrimSpace(style)
		if trimmed != "" {
			candidateStyles = append(candidateStyles, trimmed)
		}
	}
	if len(candidateStyles) == 0 {
		return ""
	}
	probability := g.Config.MultipleProbability
	if probability <= 0 {
		return ""
	}
	if randFloat() > probability {
		return ""
	}
	return candidateStyles[rand.Intn(len(candidateStyles))]
}

// buildTargetMessageBlock mirrors _build_target_message_block.
func (g *BaseMaisakaReplyGenerator) buildTargetMessageBlock(replyMessage *message.SessionMessage) string {
	if replyMessage == nil {
		return ""
	}

	userInfo := replyMessage.MessageInfo.UserInfo
	senderName := userInfo.DisplayName()
	botName := strings.TrimSpace(g.Config.BotNickname)
	if botName == "" {
		botName = senderName
	}
	targetMessageID := "未知"
	if replyMessage.MessageID != "" {
		targetMessageID = chatcontext.ToDisplayMessageID(replyMessage.MessageID)
	}
	quoteIDs := chatcontext.ExtractQuoteIDsFromMessageSequence(replyMessage.RawMessage)
	targetContent := normalizeContent(g.buildTargetMessageContent(replyMessage), 300)
	if targetContent == "" {
		targetContent = "[无可见文本内容]"
	}

	var targetLines []string
	if g.configIsBotSelf(replyMessage.Platform, userInfo.UserID) {
		targetLines = []string{
			fmt.Sprintf("你想要补充说明你自己（%s） 发送的 msg_id为 %s 的消息，"+
				"你可以在这条目标消息的基础上补充发言，不要把你自己的发言当成别人的发言。", botName, targetMessageID),
			fmt.Sprintf("- 你之前的发言内容：%s", targetContent),
		}
		return strings.Join(targetLines, "\n")
	}

	targetLines = []string{
		fmt.Sprintf("你想要回复的消息是 %s 发送的 msg_id为 %s 的消息，你这次要回复的就是这条目标消息，不要把其他历史消息当成当前回复对象。",
			senderName, targetMessageID),
	}
	if len(quoteIDs) > 0 {
		displayQuotes := make([]string, 0, len(quoteIDs))
		for _, quoteID := range quoteIDs {
			displayQuotes = append(displayQuotes, chatcontext.ToDisplayMessageID(quoteID))
		}
		targetLines = append(targetLines, "- quote="+strings.Join(displayQuotes, ","))
	}
	targetLines = append(targetLines, fmt.Sprintf("- 发言内容：%s", targetContent))
	return strings.Join(targetLines, "\n")
}

// configIsBotSelf mirrors utils.is_bot_self（账号映射由装配层注入）。
func (g *BaseMaisakaReplyGenerator) configIsBotSelf(platform string, userID string) bool {
	if g.Config.IsBotSelf != nil {
		return g.Config.IsBotSelf(platform, userID)
	}
	return false
}

// renderTargetAtComponent mirrors _render_target_at_component.
func renderTargetAtComponent(component *message.AtComponent) string {
	targetName := ""
	if component.TargetUserCardname != nil && strings.TrimSpace(*component.TargetUserCardname) != "" {
		targetName = strings.TrimSpace(*component.TargetUserCardname)
	} else if component.TargetUserNickname != nil && strings.TrimSpace(*component.TargetUserNickname) != "" {
		targetName = strings.TrimSpace(*component.TargetUserNickname)
	} else {
		targetName = component.TargetUserID
	}
	return strings.TrimSpace("@" + targetName)
}

// buildTargetMessageContent mirrors _build_target_message_content.
func (g *BaseMaisakaReplyGenerator) buildTargetMessageContent(replyMessage *message.SessionMessage) string {
	var renderedParts []string
	if replyMessage.RawMessage != nil {
		for _, component := range replyMessage.RawMessage.Components {
			switch c := component.(type) {
			case *message.TextComponent:
				if c.Text != "" {
					renderedParts = append(renderedParts, c.Text)
				}
			case *message.ReplyComponent:
				continue
			case *message.AtComponent:
				renderedAt := renderTargetAtComponent(c)
				if renderedAt != "" {
					renderedParts = append(renderedParts, renderedAt)
				}
			case *message.ImageComponent:
				content := strings.TrimSpace(c.Content)
				if content == "" {
					content = "[图片，识别中.....]"
				}
				renderedParts = append(renderedParts, content)
			case *message.EmojiComponent:
				content := strings.TrimSpace(c.Content)
				if content == "" {
					content = "[表情包]"
				}
				renderedParts = append(renderedParts, content)
			case *message.VoiceComponent:
				content := strings.TrimSpace(c.Content)
				if content == "" {
					content = "[语音消息]"
				}
				renderedParts = append(renderedParts, content)
			}
		}
	}
	var normalizedParts []string
	for _, part := range renderedParts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			normalizedParts = append(normalizedParts, trimmed)
		}
	}
	normalizedContent := strings.Join(normalizedParts, " ")
	if normalizedContent != "" {
		return normalizedContent
	}
	if replyMessage.ProcessedPlainText != nil {
		return strings.TrimSpace(*replyMessage.ProcessedPlainText)
	}
	return ""
}

// buildGroupChatAttentionBlock mirrors _build_group_chat_attention_block.
func (g *BaseMaisakaReplyGenerator) buildGroupChatAttentionBlock(sessionID string) string {
	if sessionID == "" {
		return ""
	}

	var promptLines []string
	if g.ChatStreamIsGroup {
		if groupChatPrompt := strings.TrimSpace(g.Config.GroupChatPrompt); groupChatPrompt != "" {
			promptLines = append(promptLines, "通用注意事项：\n"+groupChatPrompt)
		}
	} else {
		if privateChatPrompt := strings.TrimSpace(g.Config.PrivateChatPrompt); privateChatPrompt != "" {
			promptLines = append(promptLines, "通用注意事项：\n"+privateChatPrompt)
		}
	}

	if chatPrompt := strings.TrimSpace(g.getChatPromptForChat(sessionID)); chatPrompt != "" {
		promptLines = append(promptLines, "当前聊天额外注意事项：\n"+chatPrompt)
	}

	if len(promptLines) == 0 {
		return ""
	}
	return "在该聊天中的注意事项：\n" + strings.Join(promptLines, "\n\n") + "\n"
}

// getChatPromptForChat mirrors ChatConfigUtils.get_chat_prompt_for_chat.
func (g *BaseMaisakaReplyGenerator) getChatPromptForChat(sessionID string) string {
	var matched []string
	for _, item := range g.Config.ChatPrompts {
		platform := strings.TrimSpace(item.Platform)
		itemID := strings.TrimSpace(item.ItemID)
		ruleType := strings.TrimSpace(item.RuleType)
		promptContent := strings.TrimSpace(item.Prompt)
		if platform == "" || itemID == "" || promptContent == "" {
			continue
		}
		var configIsGroup bool
		switch ruleType {
		case "group":
			configIsGroup = true
		case "private":
			configIsGroup = false
		default:
			continue
		}
		if configIsGroup != g.ChatStreamIsGroup {
			continue
		}
		if platform == g.currentPlatform() && itemID == g.currentTargetID() {
			matched = append(matched, promptContent)
		} else if sessionID == g.ChatStreamSessionID && itemID == g.currentTargetID() && platform == g.currentPlatform() {
			matched = append(matched, promptContent)
		}
	}
	return strings.Join(matched, "\n")
}

// currentPlatform / currentTargetID 由装配层填写（此处返回会话已知信息）。
func (g *BaseMaisakaReplyGenerator) currentPlatform() string { return "" }
func (g *BaseMaisakaReplyGenerator) currentTargetID() string { return "" }

// buildReplyerOutputInstruction mirrors _build_replyer_output_instruction（zh-CN 文案照抄）。
func buildReplyerOutputInstruction() string {
	return "请注意不要输出多余内容(包括不必要的前后缀，冒号，括号，表情包，@等 )，只输出发言内容就好。"
}

// buildSystemPrompt mirrors _build_system_prompt（maisaka_replyer.prompt 渲染）。
func (g *BaseMaisakaReplyGenerator) buildSystemPrompt(streamID string) string {
	sessionID := streamID
	if sessionID == "" {
		sessionID = g.ChatStreamSessionID
	}

	systemPrompt := "你是一个友好的 AI 助手，请根据聊天记录自然回复。"
	if g.PromptManager == nil {
		return systemPrompt
	}
	// 手动拼装（Template 渲染需要全部变量，这里直接替换以保证文案与模板一致）。
	tpl, err := g.PromptManager.GetPrompt("maisaka_replyer")
	if err != nil || tpl == nil {
		return systemPrompt
	}
	identity := g.BuildPersonalityPrompt()
	fields := map[string]string{
		"identity":                   identity,
		"reply_style":                g.SelectReplyStyle(),
		"group_chat_attention_block": g.buildGroupChatAttentionBlock(sessionID),
		"replyer_output_instruction": buildReplyerOutputInstruction(),
	}
	raw := templateText(tpl)
	out := fieldPattern.ReplaceAllStringFunc(raw, func(match string) string {
		name := strings.Trim(match, "{}")
		if value, ok := fields[name]; ok {
			return value
		}
		return match
	})
	return strings.NewReplacer(prompt.LeftBraceRepl, "{", prompt.RightBraceRepl, "}").Replace(out)
}

// templateText 提取模板原文（prompt 包新增只读访问器）。
func templateText(tpl *prompt.Template) string {
	return prompt.TemplateText(tpl)
}

var fieldPattern = regexp.MustCompile(`\{([^{}]+)\}`)

// buildReplyInstruction mirrors _build_reply_instruction.
func buildReplyInstruction() string {
	return "请自然地回复。不要输出多余说明、括号、@ 或额外标记，" +
		"只输出实际要发言的内容。"
}

// buildReplyReferenceMessage mirrors _build_reply_reference_message.
func buildReplyReferenceMessage(replyReason string, replyReference string) string {
	normalizedReference := strings.TrimSpace(replyReference)
	if normalizedReference != "" {
		return normalizedReference
	}
	return strings.TrimSpace(replyReason)
}

// buildRequestedReplyStyleMessage mirrors _build_requested_reply_style_message.
func buildRequestedReplyStyleMessage(replyStyle string) string {
	styleMessages := ReplyStyleMessages
	normalizedReplyStyle := strings.TrimSpace(replyStyle)
	if normalizedReplyStyle == "" {
		return ""
	}
	if _, ok := styleMessages[normalizedReplyStyle]; !ok {
		return styleMessages["正常回复"]
	}
	return styleMessages[normalizedReplyStyle]
}

// buildTemporaryReplyStyleMessage mirrors _build_temporary_reply_style_message.
func buildTemporaryReplyStyleMessage(replyStyle string) string {
	normalizedReplyStyle := strings.TrimSpace(replyStyle)
	if normalizedReplyStyle == "" {
		return ""
	}
	return fmt.Sprintf("你的说话风格可以尝试：\n%s", normalizedReplyStyle)
}

// duplicateTargetReplyReminderArg mirrors DUPLICATE_TARGET_REPLY_REMINDER_ARG.
const DuplicateTargetReplyReminderArg = "_duplicate_target_reply_reminder"

// DuplicateTargetReplyReminderTemplate mirrors _DUPLICATE_TARGET_REPLY_REMINDER_TEMPLATE
// （文案照抄 reply.py）。
const DuplicateTargetReplyReminderTemplate = "你刚刚已经回复过这条消息，你刚刚的发言是：“{previous_reply}”\n" +
	"你现在想再次回复这条消息，进行补充，注意请不要和之前你的发言重复。"

// buildFinalUserMessage mirrors _build_final_user_message.
func (g *BaseMaisakaReplyGenerator) buildFinalUserMessage(
	chatHistory []chatcontext.LLMContextMessage,
	replyMessage *message.SessionMessage,
	replyRequirements string,
	replyToolArgs map[string]any,
) string {
	currentTime := g.now().Format("2006-01-02 15:04:05")
	sections := []string{fmt.Sprintf("当前时间：%s", currentTime)}
	targetMessageBlock := g.buildTargetMessageBlock(replyMessage)
	if targetMessageBlock != "" {
		sections = append(sections, targetMessageBlock)
	}
	duplicateReminder := ""
	if replyToolArgs != nil {
		duplicateReminder = strings.TrimSpace(toString(replyToolArgs[DuplicateTargetReplyReminderArg]))
	}
	if duplicateReminder != "" {
		sections = append(sections, duplicateReminder)
	}
	if strings.TrimSpace(replyRequirements) != "" {
		sections = append(sections, strings.TrimSpace(replyRequirements))
	}
	sections = append(sections, buildReplyInstruction())
	return strings.Join(sections, "\n\n")
}

// buildHistoryMessages mirrors _build_history_messages:
// replyer 只接收真实聊天上下文（过滤 Reference/ToolResult/工具媒体/聊天回想）。
func (g *BaseMaisakaReplyGenerator) buildHistoryMessages(
	chatHistory []chatcontext.LLMContextMessage,
	enableVisualMessage bool,
) []*llm.ContextItem {
	items := make([]*llm.ContextItem, 0)
	for _, historyMessage := range chatHistory {
		if shouldFilterReplyerHistoryMessage(historyMessage) {
			continue
		}
		sessionBacked, ok := historyMessage.(*chatcontext.SessionBackedMessage)
		if ok {
			guidedReply := extractGuidedBotReply(sessionBacked)
			if guidedReply != "" {
				items = append(items, llm.NewAssistantItem(guidedReply))
				continue
			}
			if item := historyMessage.ToContextItem(enableVisualMessage); item != nil {
				items = append(items, item)
			}
		}
	}
	return items
}

// extractGuidedBotReply mirrors _extract_guided_bot_reply + _extract_guided_reply_text.
func extractGuidedBotReply(message *chatcontext.SessionBackedMessage) string {
	// 只能根据结构化来源字段判断是否为 bot 自身写回的历史消息。
	if message.SourceKind != chatcontext.SourceGuidedReply {
		return ""
	}
	plainText := strings.TrimSpace(message.ProcessedPlainText())
	_, body, _ := chatcontext.ParseSpeakerContent(plainText)
	normalizedBody := strings.TrimSpace(body)
	if normalizedBody == "" {
		normalizedBody = strings.TrimSpace(chatcontext.BuildVisibleTextFromSequence(message.RawMessage))
	}
	if normalizedBody == "" {
		return ""
	}
	return normalizeContent(normalizedBody, 500)
}

// shouldFilterReplyerHistoryMessage mirrors _is_replyer_filtered_history_message.
func shouldFilterReplyerHistoryMessage(message chatcontext.LLMContextMessage) bool {
	switch message.(type) {
	case *chatcontext.ReferenceMessage, *chatcontext.ToolResultMessage:
		return true
	}
	if sessionBacked, ok := message.(*chatcontext.SessionBackedMessage); ok {
		if sessionBacked.SourceKind == chatcontext.SourceToolResultMedia {
			return true
		}
	}
	return chatcontext.IsMidTermMemoryMessage(message)
}

// toString normalizes any → string.
func toString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

// BuildRequestMessages mirrors _build_request_messages（非 retro 主路径）。
func (g *BaseMaisakaReplyGenerator) BuildRequestMessages(
	chatHistory []chatcontext.LLMContextMessage,
	replyMessage *message.SessionMessage,
	replyReason string,
	expressionHabits string,
	replyRequirements string,
	streamID string,
	enableVisualMessage bool,
	replyToolArgs map[string]any,
) []*llm.ContextItem {
	items := make([]*llm.ContextItem, 0)
	systemPrompt := g.buildSystemPrompt(streamID)
	finalUserMessage := g.buildFinalUserMessage(chatHistory, replyMessage, replyRequirements, replyToolArgs)
	temporaryReplyStyleMessage := buildTemporaryReplyStyleMessage(g.SelectTemporaryReplyStyle())
	replyReferenceMessage := buildReplyReferenceMessage(
		replyReason,
		toString(replyToolArgs["reply_reference"]),
	)
	requestedReplyStyleMessage := buildRequestedReplyStyleMessage(toString(replyToolArgs["reply_style"]))

	items = append(items, llm.NewSystemItem(systemPrompt))
	items = append(items, g.buildHistoryMessages(chatHistory, enableVisualMessage)...)
	if strings.TrimSpace(expressionHabits) != "" {
		items = append(items, llm.NewUserItem(strings.TrimSpace(expressionHabits)))
	}
	if temporaryReplyStyleMessage != "" {
		items = append(items, llm.NewUserItem(temporaryReplyStyleMessage))
	}
	if replyReferenceMessage != "" {
		items = append(items, llm.NewUserItem(replyReferenceMessage))
	}
	items = append(items, llm.NewUserItem(finalUserMessage))
	if requestedReplyStyleMessage != "" {
		items = append(items, llm.NewUserItem(requestedReplyStyleMessage))
	}
	return items
}

// GenerateReplyWithContext mirrors generate_reply_with_context（主体路径 + 重试）。
func (g *BaseMaisakaReplyGenerator) GenerateReplyWithContext(
	ctx context.Context,
	request ReplyRequest,
) (*ReplyGenerationResult, error) {
	result := &ReplyGenerationResult{}
	if g.ChatFunc == nil {
		result.ErrorMessage = "回复模型尚未初始化"
		return result, nil
	}
	if request.ChatHistory == nil {
		result.ErrorMessage = "聊天历史为空"
		return result, nil
	}

	filteredHistory := make([]chatcontext.LLMContextMessage, 0, len(request.ChatHistory))
	for _, entry := range request.ChatHistory {
		if !shouldFilterReplyerHistoryMessage(entry) {
			filteredHistory = append(filteredHistory, entry)
		}
	}

	// 表达方式选择（子代理；失败/缺失时静默跳过）
	replyContext := g.buildReplyContext(ctx, filteredHistory, request)
	mergedExpressionHabits := strings.TrimSpace(request.ExpressionHabit)
	if mergedExpressionHabits == "" {
		mergedExpressionHabits = replyContext.ExpressionHabits
	}
	result.SelectedExpressionIDs = replyContext.SelectedExpressionIDs
	result.SelectedExpressions = replyContext.SelectedExpressions

	// 重试循环（Hook 重试在 Go 版由调用方注入的 RetryCheck 驱动；此处保留上限语义）
	for attempt := 0; attempt <= replyerMaxHookRetries; attempt++ {
		requestMessages := g.BuildRequestMessages(
			filteredHistory,
			request.ReplyMessage,
			request.ReplyReason,
			mergedExpressionHabits,
			"",
			request.StreamID,
			request.EnableVisualMessage,
			request.ReplyToolArgs,
		)
		result.RequestMessages = requestMessages

		responseText, err := g.ChatFunc(ctx, ReplyerChatRequest{
			TaskName:    "replyer",
			RequestType: g.RequestType,
			SessionID:   request.StreamID,
			Items:       requestMessages,
			Context:     ctx,
		})
		if err != nil {
			result.ErrorMessage = err.Error()
			return result, err
		}
		responseText = strings.TrimSpace(responseText)
		if responseText == "" {
			result.ErrorMessage = "回复器返回了空内容"
			continue
		}
		result.Success = true
		result.ResponseText = responseText
		return result, nil
	}
	return result, nil
}

// buildReplyContext mirrors _build_reply_context（表达选择子代理接入点）。
func (g *BaseMaisakaReplyGenerator) buildReplyContext(
	ctx context.Context,
	filteredHistory []chatcontext.LLMContextMessage,
	request ReplyRequest,
) *ReplyContext {
	sessionID := request.StreamID
	if sessionID == "" {
		sessionID = g.ChatStreamSessionID
	}
	if sessionID == "" || g.ExpressionSource == nil {
		return &ReplyContext{}
	}
	selector := &MaisakaExpressionSelector{
		Source:   g.ExpressionSource,
		SubAgent: g.subAgentRunner,
	}
	selection := selector.SelectForReply(ctx, SelectionInput{
		SessionID:     sessionID,
		ChatHistory:   filteredHistory,
		ReplyMessage:  request.ReplyMessage,
		ReplyReason:   request.ReplyReason,
		ReplyToolArgs: request.ReplyToolArgs,
	})
	return &ReplyContext{
		ExpressionHabits:      selection.ExpressionHabits,
		SelectedExpressionIDs: selection.SelectedExpressionIDs,
		SelectedExpressions:   selection.SelectedExpressions,
	}
}

// subAgentRunner mirrors runtime.run_sub_agent 的小上下文调用（由装配层注入）。
func (g *BaseMaisakaReplyGenerator) subAgentRunner(ctx context.Context, systemPrompt string) (string, error) {
	if g.ChatFunc == nil {
		return "", fmt.Errorf("缺少子代理执行器")
	}
	return g.ChatFunc(ctx, ReplyerChatRequest{
		TaskName:    "utils",
		RequestType: "expression_selector",
		SessionID:   g.ChatStreamSessionID,
		Items:       []*llm.ContextItem{llm.NewSystemItem(systemPrompt)},
		Context:     ctx,
	})
}

// randInt mirrors random.randint(0, n-1).
func randInt(n int) int {
	if n <= 0 {
		return 0
	}
	return rand.Intn(n)
}
