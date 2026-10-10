package trigger

import (
	"fmt"
	"sync"

	"oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/message"
)

// This file mirrors 参考/MaiBot/src/maisaka/turn_trigger/gates.py: the dynamic
// reply gate and the frequency-threshold gate.

// TurnGateDecision mirrors the TurnGateDecision literal.
type TurnGateDecision string

// Turn gate decisions.
const (
	TurnGateTrigger TurnGateDecision = "trigger"
	TurnGateWait    TurnGateDecision = "wait"
	TurnGateDelay   TurnGateDecision = "delay"
)

// RecentPresenceWindowSeconds mirrors RECENT_PRESENCE_WINDOW_SECONDS: the window
// used to measure the recent message volume and the bot's message share.
const RecentPresenceWindowSeconds = 300.0

// TurnGateResult mirrors TurnGateResult.
type TurnGateResult struct {
	Decision     TurnGateDecision
	Detail       string
	DelaySeconds *float64
}

// ShouldTrigger mirrors TurnGateResult.should_trigger.
func (r TurnGateResult) ShouldTrigger() bool { return r.Decision == TurnGateTrigger }

// DynamicGateRuntime is the runtime surface DynamicReplyTurnGate needs
// (mirrors the `runtime` attribute usage in gates.py).
type DynamicGateRuntime interface {
	// IsGroupSession mirrors `runtime.chat_stream.is_group_session`.
	IsGroupSession() bool
	// ChatHistory mirrors `runtime._chat_history`.
	ChatHistory() []context.LLMContextMessage
}

// DynamicReplyTurnGate mirrors gates.DynamicReplyTurnGate.
//
// T1 修复：roundIDs 是 Evaluate 可变状态，Evaluate 会从引擎与渠道两侧 goroutine
// 并发进入（经由 MessageTurnScheduler），加 mutex 保护。
type DynamicReplyTurnGate struct {
	mu        sync.Mutex
	runtime   DynamicGateRuntime
	gate      *DynamicReplyGate
	roundIDs  map[string]bool
	now       func() float64
	isBotSelf func(platform string, userID string) bool
}

// NewDynamicReplyTurnGate mirrors DynamicReplyTurnGate.__init__ with the Go
// injectable clock / bot-identity hook.
func NewDynamicReplyTurnGate(runtime DynamicGateRuntime, now func() float64, isBotSelf func(platform string, userID string) bool) *DynamicReplyTurnGate {
	if now == nil {
		now = defaultNow
	}
	if isBotSelf == nil {
		isBotSelf = DefaultIsBotSelf
	}
	return &DynamicReplyTurnGate{
		runtime:   runtime,
		gate:      NewDynamicReplyGate(ReplyWindowSeconds),
		roundIDs:  map[string]bool{},
		now:       now,
		isBotSelf: isBotSelf,
	}
}

// DefaultIsBotSelf mirrors `is_bot_self` for an unconfigured platform: without
// a bot-account list the Python version warns once and returns false.
func DefaultIsBotSelf(platform string, userID string) bool { return false }

// RecordReply mirrors DynamicReplyTurnGate.record_reply.
func (g *DynamicReplyTurnGate) RecordReply() {
	g.gate.RecordReply(g.now())
}

// RecordForcedTurn mirrors DynamicReplyTurnGate.record_forced_turn.
func (g *DynamicReplyTurnGate) RecordForcedTurn(pendingMessages []*message.SessionMessage) {
	externalMessages := g.filterExternalMessages(pendingMessages)
	ids := make([]string, 0, len(externalMessages))
	for _, externalMessage := range externalMessages {
		ids = append(ids, externalMessage.MessageID)
	}
	if len(g.gate.TakeUncountedMessageIDs(ids)) > 0 {
		g.gate.RecordForcedTurn(g.now())
	}
}

// Evaluate mirrors DynamicReplyTurnGate.evaluate.
func (g *DynamicReplyTurnGate) Evaluate(pendingMessages []*message.SessionMessage, frequency float64) TurnGateResult {
	externalMessages := g.filterExternalMessages(pendingMessages)
	if len(externalMessages) == 0 {
		return TurnGateResult{Decision: TurnGateWait, Detail: "没有新的外部消息 判定=等待更多消息"}
	}
	if !g.runtime.IsGroupSession() {
		// 拟合数据只覆盖群聊；私聊里每条消息都是对麦麦说的，不设门控
		//（对齐 gates.py 的 `if not runtime.chat_stream.is_group_session` 分支）。
		return TurnGateResult{Decision: TurnGateTrigger, Detail: "私聊不设动态门控 判定=进入Planner"}
	}

	now := g.now()
	newMessageIDs := map[string]bool{}
	for _, messageID := range g.gate.TakeUncountedMessageIDs(messageIDsOf(externalMessages)) {
		newMessageIDs[messageID] = true
	}
	if len(newMessageIDs) > 0 {
		// 预计回复数按虚拟轮次计：只用本轮次内到达的消息估计，被门控拦下而积压的旧消息不重复计入
		// T1：roundIDs 只在锁内读写。
		g.mu.Lock()
		if !g.gate.IsRoundOpen(now) {
			g.roundIDs = map[string]bool{}
		}
		for messageID := range newMessageIDs {
			g.roundIDs[messageID] = true
		}
		roundMessages := make([]*message.SessionMessage, 0, len(externalMessages))
		for _, externalMessage := range externalMessages {
			if g.roundIDs[externalMessage.MessageID] {
				roundMessages = append(roundMessages, externalMessage)
			}
		}
		g.mu.Unlock()
		g.gate.RecordProactiveDemand(
			EstimateReplyProbability(g.buildLikelihoodInput(roundMessages, now, externalMessages)),
			now,
		)
	}

	probability := EstimateReplyProbability(g.buildLikelihoodInput(externalMessages, now, externalMessages))
	gateDecision := g.gate.Evaluate(probability, frequency, now)
	if gateDecision.ShouldTrigger {
		g.gate.CloseRound()
	}
	decisionLabel := "等待更多消息"
	if gateDecision.ShouldTrigger {
		decisionLabel = "进入Planner"
	}
	decision := TurnGateWait
	if gateDecision.ShouldTrigger {
		decision = TurnGateTrigger
	}
	return TurnGateResult{
		Decision: decision,
		Detail:   fmt.Sprintf("动态门控: %s 判定=%s", gateDecision.Detail(), decisionLabel),
	}
}

func (g *DynamicReplyTurnGate) filterExternalMessages(pendingMessages []*message.SessionMessage) []*message.SessionMessage {
	external := make([]*message.SessionMessage, 0, len(pendingMessages))
	for _, pendingMessage := range pendingMessages {
		if pendingMessage == nil {
			continue
		}
		if g.isBotSelf(pendingMessage.Platform, pendingMessage.UserID()) {
			continue
		}
		external = append(external, pendingMessage)
	}
	return external
}

// buildLikelihoodInput mirrors `_build_likelihood_input`.
func (g *DynamicReplyTurnGate) buildLikelihoodInput(
	messages []*message.SessionMessage,
	now float64,
	pendingMessages []*message.SessionMessage,
) ReplyLikelihoodInput {
	texts := make([]string, 0, len(messages))
	hasAtBot := false
	mentionBot := false
	for _, entry := range messages {
		if entry.ProcessedPlainText != nil {
			texts = append(texts, *entry.ProcessedPlainText)
		} else {
			texts = append(texts, "")
		}
		if entry.IsAt {
			hasAtBot = true
		}
		if entry.IsMentioned {
			mentionBot = true
		}
	}
	atOther, hasQuestionMark, placeholderOnly := SummarizePendingTexts(texts, hasAtBot)
	recentSelfCount, recentHistoryCount, secondsSinceBot := g.summarizeRecentHistory(now)
	// 待处理消息尚未写入内部历史，只补上最近 5 分钟内的消息，避免旧积压持续压低概率。
	// 近期活跃度属于整个聊天流；虚拟轮次评分与放行评分必须使用同一批待处理消息统计。
	currentTime := timeFromUnixSeconds(now)
	recentPendingCount := 0
	for _, pendingMessage := range pendingMessages {
		if pendingMessage == nil {
			continue
		}
		if currentTime.Sub(pendingMessage.Timestamp).Seconds() <= RecentPresenceWindowSeconds {
			recentPendingCount++
		}
	}
	recentMessageCount := recentHistoryCount + recentPendingCount
	denominator := recentMessageCount
	if denominator < 1 {
		denominator = 1
	}
	return ReplyLikelihoodInput{
		MentionBot:             mentionBot,
		AtOther:                atOther,
		HasQuestionMark:        hasQuestionMark,
		PlaceholderOnly:        placeholderOnly,
		RecentSelfRatio:        float64(recentSelfCount) / float64(denominator),
		RecentMessageCount:     recentMessageCount,
		SecondsSinceBotMessage: secondsSinceBot,
		PendingCount:           len(messages),
	}
}

// summarizeRecentHistory mirrors `_summarize_recent_history`.
func (g *DynamicReplyTurnGate) summarizeRecentHistory(now float64) (recentSelfCount int, recentTotalCount int, secondsSinceBot float64) {
	currentTime := timeFromUnixSeconds(now)
	secondsSinceBot = -1
	history := g.runtime.ChatHistory()
	for index := len(history) - 1; index >= 0; index-- {
		entry := history[index]
		if entry == nil || !entry.CountInContext() {
			continue
		}
		elapsedSeconds := currentTime.Sub(entry.Timestamp()).Seconds()
		isSelfMessage := entry.Source() == context.SourceGuidedReply
		if isSelfMessage && secondsSinceBot < 0 {
			secondsSinceBot = maxFloat(0.0, elapsedSeconds)
		}
		if elapsedSeconds > RecentPresenceWindowSeconds {
			// 窗口外只需要继续找到麦麦最近一次发言
			if secondsSinceBot >= 0 {
				break
			}
			continue
		}
		recentTotalCount++
		if isSelfMessage {
			recentSelfCount++
		}
	}
	if secondsSinceBot < 0 {
		secondsSinceBot = DefaultSecondsSinceBotMessage
	}
	return recentSelfCount, recentTotalCount, secondsSinceBot
}

// ---------------------------------------------------------------------------
// FrequencyThresholdTurnGate
// ---------------------------------------------------------------------------

// FrequencyGateRuntime is the runtime surface FrequencyThresholdTurnGate needs.
type FrequencyGateRuntime interface {
	// RecentAverageExternalMessageInterval mirrors
	// `_get_recent_average_external_message_interval`.
	RecentAverageExternalMessageInterval() (float64, bool)
	// LastExternalMessageReceivedAt mirrors `_last_external_message_received_at`.
	LastExternalMessageReceivedAt() (float64, bool)
	// LastMessageReceivedAt mirrors `_last_message_received_at`.
	LastMessageReceivedAt() float64
}

// FrequencyThresholdTurnGate mirrors gates.FrequencyThresholdTurnGate.
type FrequencyThresholdTurnGate struct {
	runtime FrequencyGateRuntime
	now     func() float64
}

// NewFrequencyThresholdTurnGate builds the frequency gate.
func NewFrequencyThresholdTurnGate(runtime FrequencyGateRuntime, now func() float64) *FrequencyThresholdTurnGate {
	if now == nil {
		now = defaultNow
	}
	return &FrequencyThresholdTurnGate{runtime: runtime, now: now}
}

// Evaluate mirrors FrequencyThresholdTurnGate.evaluate.
func (g *FrequencyThresholdTurnGate) Evaluate(pendingCount int, triggerThreshold int) TurnGateResult {
	if pendingCount >= triggerThreshold {
		return TurnGateResult{
			Decision: TurnGateTrigger,
			Detail:   fmt.Sprintf("pending=%d 达到阈值=%d 判定=进入Planner", pendingCount, triggerThreshold),
		}
	}

	idleCompensationTriggered, delaySeconds, idleDetail := g.calculateIdleCompensation(pendingCount, triggerThreshold)
	if idleCompensationTriggered {
		return TurnGateResult{Decision: TurnGateTrigger, Detail: idleDetail + " 判定=空窗补偿进入Planner"}
	}
	if delaySeconds != nil {
		return TurnGateResult{
			Decision:     TurnGateDelay,
			Detail:       idleDetail + " 判定=延迟检查",
			DelaySeconds: delaySeconds,
		}
	}
	return TurnGateResult{Decision: TurnGateWait, Detail: idleDetail + " 判定=等待更多消息"}
}

// calculateIdleCompensation mirrors `_calculate_idle_compensation`.
func (g *FrequencyThresholdTurnGate) calculateIdleCompensation(
	pendingCount int,
	triggerThreshold int,
) (triggered bool, delaySeconds *float64, detail string) {
	// 与下方折算封顶互为双保险：纯沉默（pending_count == 0）一律不触发。
	if pendingCount < 1 {
		return false, nil, "pending=0，不允许纯沉默触发"
	}

	averageMessageInterval, ok := g.runtime.RecentAverageExternalMessageInterval()
	if !ok || averageMessageInterval <= 0 {
		return false, nil, "平均消息间隔不可用，无法进行空窗补偿"
	}

	lastExternalReceivedAt, ok := g.runtime.LastExternalMessageReceivedAt()
	if !ok {
		lastExternalReceivedAt = g.runtime.LastMessageReceivedAt()
	}
	idleSeconds := maxFloat(0.0, g.now()-lastExternalReceivedAt)
	// 即便空窗无限长，也不能让纯沉默跨过阈值。
	idleEquivalentCount := minFloat(
		idleSeconds/averageMessageInterval,
		float64(maxInt(0, triggerThreshold-1)),
	)
	equivalentMessageCount := float64(pendingCount) + idleEquivalentCount
	detail = fmt.Sprintf(
		"平均间隔=%.2fs 空窗=%.2fs 空窗折算=%.2f 等效消息数=%.2f/%d",
		averageMessageInterval, idleSeconds, idleEquivalentCount, equivalentMessageCount, triggerThreshold,
	)
	if equivalentMessageCount >= float64(triggerThreshold) {
		return true, nil, detail
	}

	computedDelay := maxFloat(0.0, float64(triggerThreshold-pendingCount)*averageMessageInterval-idleSeconds)
	detail = fmt.Sprintf("%s 延迟=%.2fs", detail, computedDelay)
	return false, &computedDelay, detail
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func messageIDsOf(messages []*message.SessionMessage) []string {
	ids := make([]string, 0, len(messages))
	for _, entry := range messages {
		if entry == nil {
			continue
		}
		ids = append(ids, entry.MessageID)
	}
	return ids
}
