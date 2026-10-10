// reply_wiring.go — 装配层的工具运行期接线：把 qq.Channel / ChatManager /
// Heartflow / LLM / prompt / replyer / sender 适配成内置工具（wait/reply 等）
// 所需的注入边界（tools.BuiltinToolDeps）。
//
// 背景（P0 崩溃链根因 2）：catalog.BuiltinEntries 只声明工具；handler 依赖的
// 会话运行时、发送服务、回复生成器、后处理器此前从未装配，即使 handler 存在，
// reply 工具第一行访问 nil Runtime 也会 panic。全部依赖在此按会话惰性解析，
// 解析失败返回 nil 让工具回执失败而不是 panic。
package assemble

import (
	"context"
	"fmt"
	"strings"
	"time"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/chat/manager"
	"oh-my-qqbot/internal/chat/replyer"
	"oh-my-qqbot/internal/chat/runtime"
	"oh-my-qqbot/internal/chat/sender"
	"oh-my-qqbot/internal/chat/tools"
	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/memorix"
	"oh-my-qqbot/internal/message"
	"oh-my-qqbot/internal/prompt"
	"oh-my-qqbot/internal/qq"
)

// ---------------------------------------------------------------------------
// 编译期对齐检查
// ---------------------------------------------------------------------------

// runtime.Runtime 必须满足内置工具的会话运行时面（wait/view_forward/reply）。
var _ tools.SessionRuntime = (*runtime.Runtime)(nil)

var (
	_ tools.ReplySender          = (*replySegmentSender)(nil)
	_ tools.ReplyGenerator       = replyGeneratorAdapter{}
	_ tools.ReplyPostProcessor   = replyPostProcessor{}
	_ tools.MemorySearcher       = memorixMemorySearcher{}
	_ tools.FetchHistoryRuntime  = focusHistoryAdapter{}
	_ sender.ChannelClient       = qqChannelClient{}
	_ sender.SessionLookup       = chatSessionLookup{}
	_ sender.MessageStore        = chatMessageStore{}
	_ sender.HistoryWriter       = heartflowHistoryWriter{}
	_ sender.RuntimeFocusChecker = runtimeFocusAdapter{}
)

// ---------------------------------------------------------------------------
// 工具目录依赖
// ---------------------------------------------------------------------------

// builtinToolDeps 按会话解析内置工具的运行期依赖（全部惰性；未装配时对应
// 工具回执失败而不是 panic）。
func (a *App) builtinToolDeps() *tools.BuiltinToolDeps {
	return &tools.BuiltinToolDeps{
		RuntimeFor:      a.sessionRuntime,
		ReplyFor:        a.replyDependencies,
		FocusHistoryFor: a.focusHistoryRuntime,
		Memory:          a.memorySearcher(),
		// EmojiSender/EmojiLister 暂不接线（internal/emoji 未落地）：
		// 留空时 send_emoji/show_emoji_list 按「视觉模型未配置/无可用表情」
		// 的骨架语义回执（与 Python 未配置行为一致）。
	}
}

// sessionRuntime 返回指定会话的 runtime（内置工具的 wait/view_forward/reply 面）。
// 注意：必须显式判 nil 再装箱，否则 (*runtime.Runtime)(nil) 会成为非 nil
// 接口值，工具层的 nil 检查失效（typed-nil 陷阱）。
func (a *App) sessionRuntime(sessionID string) tools.SessionRuntime {
	rt := a.heartflowRuntime(sessionID)
	if rt == nil {
		return nil
	}
	return rt
}

// heartflowRuntime 返回指定会话的 runtime 实例（不存在返回 nil）。
func (a *App) heartflowRuntime(sessionID string) *runtime.Runtime {
	if a.Heartflow == nil {
		return nil
	}
	return a.Heartflow.GetChat(sessionID)
}

// ---------------------------------------------------------------------------
// query_memory — memorix.Kernel → tools.MemorySearcher
// ---------------------------------------------------------------------------

// memorySearcher 返回长期记忆检索适配（Kernel 未装配时返回 nil）。
func (a *App) memorySearcher() tools.MemorySearcher {
	if a.Kernel == nil {
		return nil
	}
	return memorixMemorySearcher{kernel: a.Kernel}
}

// memorixMemorySearcher 把 memorix.Kernel.SearchMemory 适配为工具层检索面。
type memorixMemorySearcher struct{ kernel *memorix.Kernel }

// SearchMemory 执行一次记忆检索并映射为工具层结果。
func (m memorixMemorySearcher) SearchMemory(ctx context.Context, mode string, query string, chatID string, topK int) (tools.MemorySearchResult, error) {
	hits, err := m.kernel.SearchMemory(ctx, mode, query, chatID, topK)
	if err != nil {
		return tools.MemorySearchResult{}, err
	}
	result := tools.MemorySearchResult{Success: true}
	for _, hit := range hits {
		result.Hits = append(result.Hits, tools.MemorySearchHit{
			Content: hit.Text,
			Score:   hit.Score,
			Type:    hit.Kind,
			Hash:    hit.ID,
			Metadata: map[string]any{
				"chat_id":    hit.ChatID,
				"similarity": hit.Similarity,
			},
		})
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// fetch_history — 焦点模式门面
// ---------------------------------------------------------------------------

// focusHistoryRuntime 返回 fetch_history 的会话门面（会话不存在时返回 nil）。
func (a *App) focusHistoryRuntime(sessionID string) tools.FetchHistoryRuntime {
	rt := a.heartflowRuntime(sessionID)
	if rt == nil {
		return nil
	}
	return focusHistoryAdapter{runtime: rt}
}

// focusHistoryAdapter 把会话 runtime 适配为 tools.FetchHistoryRuntime。
// BuildFocusFetchHistoryResult 属于焦点模式后续工作包，暂以明确错误回执
// （焦点未激活时仍沿用工具层的原始失败文案）。
type focusHistoryAdapter struct{ runtime *runtime.Runtime }

// FocusModeActiveForCurrentChat mirrors runtime 的焦点探针。
func (f focusHistoryAdapter) FocusModeActiveForCurrentChat() bool {
	return f.runtime.FocusModeActiveForCurrentChat()
}

// BuildFocusFetchHistoryResult 焦点历史构建器尚未接入。
func (f focusHistoryAdapter) BuildFocusFetchHistoryResult(int) (string, map[string]any, []tools.PostHistoryEntry, error) {
	return "", nil, nil, fmt.Errorf("焦点历史构建器尚未接入（focus fetch history builder 未装配）")
}

// ---------------------------------------------------------------------------
// reply 工具依赖
// ---------------------------------------------------------------------------

// replyDependencies 按会话装配 reply 工具依赖（runtime + sender + generator +
// postprocessor）。会话运行时不存在时返回 nil（工具层回执失败）。
func (a *App) replyDependencies(sessionID string) *tools.ReplyDependencies {
	rt := a.sessionRuntime(sessionID)
	if rt == nil {
		return nil
	}
	global := a.Cfg.GetGlobalConfig()
	typingSpeed := global.ResponsePostProcess.TypingSpeed
	return &tools.ReplyDependencies{
		Runtime:       rt,
		Sender:        a.replySegmentSender(),
		Generator:     a.newReplyGenerator(sessionID, rt.IsGroupSession()),
		PostProcessor: a.replyPostProcessor(),
		TypingSpeed:   typingSpeed,
		// 每条发送间隔 = calculate_typing_time × typing_speed（sender 侧模拟）。
		SegmentDelay: sender.DefaultSegmentDelay(typingSpeed),
		BotNameFn: func() string {
			return a.Cfg.GetGlobalConfig().Bot.Nickname
		},
		IsBotSelfFn: func(platform, userID string) bool {
			return userID != "" && userID == a.Cfg.GetGlobalConfig().Bot.QQAccount
		},
	}
}

// ---------------------------------------------------------------------------
// 发送链路 — qq.Channel → sender.SendService → tools.ReplySender
// ---------------------------------------------------------------------------

// qqChannelClient 把 *qq.Channel（不改 internal/qq）适配为 sender.ChannelClient：
// 双模决策（被动优先 → 超窗降级主动）与额度台账由 Channel.Send 内部完成。
type qqChannelClient struct{ channel *qq.Channel }

// SendChannelMessage 走一次双模发送。
func (c qqChannelClient) SendChannelMessage(ctx context.Context, outbound sender.ChannelOutbound) (*sender.ChannelResult, error) {
	if c.channel == nil {
		return nil, fmt.Errorf("QQ 渠道未连接，无法发送消息")
	}
	target := qq.OutboundTarget{GroupID: outbound.GroupID, UserID: outbound.UserID}
	result, err := c.channel.Send(ctx, qq.Outbound{
		Target:      target,
		Text:        outbound.Text,
		Media:       outbound.Media,
		TriggerID:   outbound.TriggerID,
		TriggerTime: outbound.TriggerTime,
		QuoteMsgID:  outbound.QuoteMsgID,
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("qq: 渠道发送未返回结果")
	}
	return &sender.ChannelResult{MessageID: result.MessageID, Passive: result.Mode == qq.ModePassive, Reason: result.Reason}, nil
}

// UploadMedia 适配富媒体上传。当前 reply 工具只发文本段，富媒体上传需要
// 会话前缀（group/user openid），工具层请求未携带目标；这里以明确错误回执，
// 待 emoji/图片出站接线时扩展。
func (c qqChannelClient) UploadMedia(context.Context, []byte, string) (string, error) {
	return "", fmt.Errorf("富媒体上传尚未接线（需要会话目标上下文）")
}

// chatSessionLookup 把 ChatManager 适配为 sender.SessionLookup。
type chatSessionLookup struct{ manager *manager.ChatManager }

// GetSession 返回目标会话的最小投影（不存在返回 nil）。
func (l chatSessionLookup) GetSession(sessionID string) *sender.SessionInfo {
	if l.manager == nil {
		return nil
	}
	session := l.manager.GetSessionBySessionID(sessionID)
	if session == nil {
		return nil
	}
	info := &sender.SessionInfo{
		SessionID: session.SessionID,
		Platform:  session.Platform,
	}
	if session.GroupID != nil {
		info.GroupID = *session.GroupID
	}
	if session.GroupName != nil {
		info.GroupName = *session.GroupName
	}
	if session.UserID != nil {
		info.UserID = *session.UserID
	}
	return info
}

// chatMessageStore 把 ChatManager 适配为 sender.MessageStore。
type chatMessageStore struct{ manager *manager.ChatManager }

// StoreMessage 把消息写入数据库。
func (s chatMessageStore) StoreMessage(sessionMessage *message.SessionMessage) (int64, error) {
	return s.manager.StoreMessageToDB(sessionMessage)
}

// heartflowHistoryWriter 把发送成功的消息写回对应会话的运行时历史
// （对齐 heartflow_manager → runtime.append_sent_message_to_chat_history）。
type heartflowHistoryWriter struct{ app *App }

// AppendSentMessage 按会话路由历史回写（会话不存在时静默跳过）。
func (w heartflowHistoryWriter) AppendSentMessage(sessionID string, sessionMessage *message.SessionMessage, sourceKind string) {
	rt := w.app.heartflowRuntime(sessionID)
	if rt == nil {
		return
	}
	sender.AppendSentMessageToHistory(rt, runtimeFocusAdapter{runtime: rt}, sessionMessage, sourceKind)
}

// runtimeFocusAdapter 把 runtime 的焦点探针适配为 sender.RuntimeFocusChecker。
type runtimeFocusAdapter struct{ runtime *runtime.Runtime }

// FocusModeActiveForChat mirrors _is_focus_mode_active_for_current_chat。
func (a runtimeFocusAdapter) FocusModeActiveForChat() bool {
	return a.runtime.FocusModeActiveForCurrentChat()
}

// sendService 返回共享的出站发送服务（惰性构建；Channel 未连接时构建成功、
// 发送阶段报错，避免装配顺序耦合）。
func (a *App) sendService() *sender.SendService {
	a.sendOnce.Do(func() {
		global := a.Cfg.GetGlobalConfig()
		a.sendSvc = sender.NewSendService(sender.Options{
			Channel:  qqChannelClient{channel: a.Channel},
			Sessions: chatSessionLookup{manager: a.ChatManager},
			BotIdentity: func(platform string) (sender.BotIdentity, bool) {
				cfg := a.Cfg.GetGlobalConfig()
				if strings.TrimSpace(cfg.Bot.QQAccount) == "" {
					return sender.BotIdentity{}, false
				}
				return sender.BotIdentity{
					Platform: platform,
					UserID:   cfg.Bot.QQAccount,
					Nickname: cfg.Bot.Nickname,
				}, true
			},
			Store:       chatMessageStore{manager: a.ChatManager},
			History:     heartflowHistoryWriter{app: a},
			TypingSpeed: global.ResponsePostProcess.TypingSpeed,
		})
	})
	return a.sendSvc
}

// replySegmentSender 返回 reply 工具的逐段发送边界（惰性单例）。
func (a *App) replySegmentSender() tools.ReplySender {
	a.replySenderOnce.Do(func() {
		a.replySenderImpl = &replySegmentSender{app: a}
	})
	return a.replySenderImpl
}

// replySegmentSender 把 tools.ReplySender 的单段请求映射到 SendService：
// 每段一条文本消息；首段引用目标消息、错别字纠正段引用上一条已发送消息。
type replySegmentSender struct{ app *App }

// SendReplySegment 发送一段回复并返回平台消息 id。
func (s *replySegmentSender) SendReplySegment(ctx context.Context, req tools.ReplySendRequest) (string, error) {
	service := s.app.sendService()
	session := chatSessionLookup{manager: s.app.ChatManager}.GetSession(req.SessionID)
	if session == nil {
		return "", fmt.Errorf("未找到聊天流: %s", req.SessionID)
	}
	sequence := message.NewMessageSequence(message.NewTextComponent(req.Text))
	outbound, err := service.BuildOutboundSessionMessage(session, sequence, "", nil)
	if err != nil {
		return "", err
	}
	// 引用目标：首段引用目标消息，纠正段引用上一条已发送消息。
	quoteID := req.ReplyToMessageID
	if req.QuotePrevious {
		quoteID = req.PreviousMessageID
	}
	sent, err := service.SendSessionMessage(ctx, outbound, sender.SendOptions{
		SetReply:             strings.TrimSpace(quoteID) != "",
		ReplyMessageID:       quoteID,
		StorageMessage:       true,
		SyncToMaisakaHistory: true,
		MaisakaSourceKind:    "guided_reply",
		TriggerID:            quoteID,
		TriggerTime:          s.triggerTimeFor(req.SessionID, quoteID),
	})
	if err != nil {
		return "", err
	}
	return sent.MessageID, nil
}

// triggerTimeFor 解析被动回复窗口的触发时间（目标消息到达时间）；解析失败
// 返回零值（渠道层按主动模式降级，走额度台账）。
func (s *replySegmentSender) triggerTimeFor(sessionID string, messageID string) time.Time {
	if strings.TrimSpace(messageID) == "" {
		return time.Time{}
	}
	rt := s.app.sessionRuntime(sessionID)
	if rt == nil {
		return time.Time{}
	}
	if target := rt.FindSourceMessageByID(messageID); target != nil {
		return target.Timestamp
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// 回复生成 — llm/prompt/replyer → tools.ReplyGenerator
// ---------------------------------------------------------------------------

// promptManager 返回共享的 prompt 管理器（惰性加载；失败时降级为 nil，
// generator 会回退到内置兜底 system prompt）。
func (a *App) promptManager() *prompt.Manager {
	a.promptOnce.Do(func() {
		prompts := prompt.NewManager(a.PromptsDir(), "", "zh-CN")
		if err := prompts.LoadPrompts(); err != nil {
			a.log.Warn("加载 prompts 目录失败，回复生成将使用内置兜底提示词", "err", err)
			return
		}
		a.promptMgr = prompts
	})
	return a.promptMgr
}

// replyerChatFunc 把 llm.Orchestrator 适配为 replyer.ChatFunc。
func (a *App) replyerChatFunc() replyer.ChatFunc {
	return func(ctx context.Context, request replyer.ReplyerChatRequest) (string, error) {
		req := contracts.ChatRequest{Messages: llm.ItemsToMessages(request.Items)}
		if request.MaxTokens > 0 {
			req.MaxTokens = request.MaxTokens
		}
		resp, err := a.LLM.ChatWithSession(ctx, request.TaskName, request.SessionID, req)
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}

// newReplyGenerator 构建指定会话的回复生成器（人设/风格/提示词装配）。
func (a *App) newReplyGenerator(sessionID string, isGroup bool) tools.ReplyGenerator {
	global := a.Cfg.GetGlobalConfig()
	config := replyer.GeneratorConfig{
		BotNickname:         global.Bot.Nickname,
		AliasNames:          global.Bot.AliasNames,
		Personality:         global.Personality.Personality,
		ReplyStyle:          global.Personality.ReplyStyle,
		MultipleReplyStyle:  global.Personality.MultipleReplyStyle,
		MultipleProbability: global.Personality.MultipleProbability,
		EmotionTrait:        global.Experimental.EmotionTrait,
		GroupChatPrompt:     global.Chat.ReplyStyle.GroupChatPrompt,
		PrivateChatPrompt:   global.Chat.ReplyStyle.PrivateChatPrompt,
		ExpressionUseVector: global.Expression.UseVectorExpression,
		IsBotSelf: func(platform, userID string) bool {
			return userID != "" && userID == a.Cfg.GetGlobalConfig().Bot.QQAccount
		},
	}
	for _, item := range global.Chat.ReplyStyle.ChatPrompts {
		config.ChatPrompts = append(config.ChatPrompts, replyer.ChatPromptItem{
			Platform: item.Platform,
			ItemID:   item.ItemID,
			RuleType: item.RuleType,
			Prompt:   item.Prompt,
		})
	}
	generator := replyer.NewBaseMaisakaReplyGenerator(sessionID, isGroup, config, a.promptManager(), a.replyerChatFunc())
	return replyGeneratorAdapter{generator: generator}
}

// replyGeneratorAdapter 把 BaseMaisakaReplyGenerator 适配为 tools.ReplyGenerator。
type replyGeneratorAdapter struct {
	generator *replyer.BaseMaisakaReplyGenerator
}

// GenerateReply 生成可见回复文本。
func (a replyGeneratorAdapter) GenerateReply(ctx context.Context, request tools.ReplyGenerateRequest) (tools.ReplyGenerateResult, error) {
	result, err := a.generator.GenerateReplyWithContext(ctx, replyer.ReplyRequest{
		StreamID:      request.StreamID,
		ReplyMessage:  request.ReplyMessage,
		ReplyReason:   request.ReplyReason,
		ChatHistory:   request.ChatHistory,
		ReplyToolArgs: request.ReplyToolArgs,
	})
	if result == nil {
		return tools.ReplyGenerateResult{}, err
	}
	return tools.ReplyGenerateResult{
		Success:      result.Success,
		ResponseText: result.ResponseText,
		ErrorMessage: result.ErrorMessage,
	}, err
}

// ---------------------------------------------------------------------------
// 后处理 — replyer 后处理管线 → tools.ReplyPostProcessor
// ---------------------------------------------------------------------------

// replyPostProcessor 返回回复后处理器（断句/错别字/颜文字保护；配置直传）。
func (a *App) replyPostProcessor() tools.ReplyPostProcessor {
	global := a.Cfg.GetGlobalConfig()
	options := replyer.PostProcessOptions{
		EnableResponsePostProcess: global.ResponsePostProcess.EnableResponsePostProcess,
		SplitterEnable:            global.ResponseSplitter.Enable,
		SplitterMode:              global.ResponseSplitter.Mode,
		MaxLength:                 global.ResponseSplitter.MaxLength,
		MaxSentenceNum:            global.ResponseSplitter.MaxSentenceNum,
		MaxSplitNum:               global.ResponseSplitter.MaxSplitNum,
		EnableOverflowReturnAll:   global.ResponseSplitter.EnableOverflowReturnAll,
		EnableKaomojiProtection:   global.ResponseSplitter.EnableKaomojiProtection,
		BotNickname:               global.Bot.Nickname,
	}
	if global.ChineseTypo.Enable {
		options.Typo = &replyer.TypoOptions{
			ErrorRate:                  global.ChineseTypo.ErrorRate,
			MinFreq:                    global.ChineseTypo.MinFreq,
			ToneErrorRate:              global.ChineseTypo.ToneErrorRate,
			WordReplaceRate:            global.ChineseTypo.WordReplaceRate,
			EnableCorrectionQuote:      global.ChineseTypo.EnableCorrectionQuote,
			CorrectionQuoteProbability: global.ChineseTypo.CorrectionQuoteProbability,
		}
	}
	return replyPostProcessor{options: options}
}

// replyPostProcessor 把 replyer 的后处理管线适配为 tools.ReplyPostProcessor。
type replyPostProcessor struct{ options replyer.PostProcessOptions }

// PostProcessSegments 处理回复文本并保留纠正段引用提示。
func (p replyPostProcessor) PostProcessSegments(replyText string, skipPostProcess bool, enableSplitter bool, enableChineseTypo bool) []tools.OutboundSegment {
	segments := replyer.PostProcessReplySegments(replyText, skipPostProcess, enableSplitter, enableChineseTypo, p.options)
	out := make([]tools.OutboundSegment, 0, len(segments))
	for _, segment := range segments {
		out = append(out, tools.OutboundSegment{Text: segment.Text, QuotePrevious: segment.QuotePrevious})
	}
	return out
}
