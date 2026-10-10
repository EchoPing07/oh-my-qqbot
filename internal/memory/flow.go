package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/message"
)

// This file mirrors 参考/MaiBot/src/services/memory_flow_service.py
// (behaviour is authoritative there): 三个后台写回 worker
// （PersonFact / ChatSummary / Image）+ MemoryAutomationService 触发点。
//
// Go 差异：asyncio.Queue(maxsize=256) → buffered channel(256) + goroutine；
// 游标/日志文件写入走 os.Rename 原子替换。

// QueueSize mirrors `asyncio.Queue(maxsize=256)`.
const QueueSize = 256

// ImageWritebackEntityPrefix 对齐 A_memorix image 记忆的外部引用前缀。
const ImageWritebackEntityPrefix = "image:"

// FactExtractionPromptTemplate 对齐 PersonFactWritebackService._extract_facts 的
// 内联 prompt（照抄文案，person_name 填入）。
const FactExtractionPromptTemplate = `你要从用户原始发言中提取“关于%s的稳定事实”。

目标人物：%s
用户原始发言证据：
%s

机器人回复：
%s

请只提取满足以下条件的事实：
1. 事实值必须能被“目标用户原始发言”直接支持，不能只来自机器人回复或邻近上下文。
2. 明确是关于目标人物本人的信息。
3. 具有相对稳定性，可以作为长期记忆保存。
4. 用简洁中文陈述句表达。
5. 如果用户原始发言中出现“我/我的/自己”，默认指目标人物，请先改写成关于目标人物的第三人称事实再输出。
6. 邻近上下文只能用于补全目标用户短答、省略、被追问的问题或代词指向。例如上下文问“你对什么过敏？”，目标用户答“青霉素”，可以提取“目标人物对青霉素过敏”。
7. 如果完整事实只能靠机器人回复或邻近上下文中的新增事实值成立，而目标用户原始发言没有确认或给出该事实值，不要提取。

不要提取：
- 机器人的情绪、计划、临时动作、客套话
- 仅由机器人提出的建议、猜测、玩笑、回忆或承诺
- 只适用于当前时刻的短期安排
- 不确定、猜测、反问
- 与目标人物无关的信息

严格输出 JSON 数组，每项包含 fact、evidence_message_id、evidence_quote。证据片段必须逐字摘自目标用户原始发言。请勿编造消息 ID。例：
[{"fact":"他喜欢打游戏","evidence_message_id":"消息ID","evidence_quote":"我喜欢打游戏"}]
如果没有可写入的事实，输出 []`

// PersonFactEvidence mirrors PersonFactEvidence（Go 版保持有序切片语义）。
type PersonFactEvidence struct {
	TargetMessages  []*message.SessionMessage
	ContextMessages []*message.SessionMessage
}

// ---------------------------------------------------------------------------
// PersonFactWritebackService
// ---------------------------------------------------------------------------

// PersonFactWritebackService 消息发送后抽取人物事实并写回内核 profile。
type PersonFactWritebackService struct {
	Enabled   bool
	LLM       LLMFunc
	Writer    PersonFactWriter
	Messages  MessageQueryFunc
	Counts    MessageCountFunc
	ByID      MessageByIDFunc
	IsBotSelf IsBotSelfFunc
	PersonID  PersonIDFunc
	// Sessions mirrors chat_manager.get_existing_session_by_session_id：用于解析
	// session.platform / session.user_id / session.group_id（私聊 user_id 是对端用户）。
	Sessions SessionLookupFunc

	// KernelDataDir 是历史事实重验游标落盘目录（对齐
	// a_memorix_host_service.get_runtime_data_dir()）。
	KernelDataDir string
	// Reverify 是历史人物事实重验实现（对齐 reverify_historical_person_facts）。
	Reverify func(ctx context.Context, cursor string, limit int) (ReverifyResult, error)
	// ReverifyInterval 默认 5s（对齐 `await asyncio.sleep(5)` 首轮等待）。
	ReverifyInterval time.Duration
	// ReverifyIdleInterval 默认 24h（`has_more` 为 false 时的间隔）。
	ReverifyIdleInterval time.Duration
	// ReverifyErrorInterval 默认 60s。
	ReverifyErrorInterval time.Duration

	Logf func(format string, args ...any)

	queue     chan *message.SessionMessage
	mu        sync.Mutex
	started   bool
	stopping  bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	processed int
	dropped   int
}

// ReverifyResult mirrors reverify_historical_person_facts 的返回字典。
type ReverifyResult struct {
	Processed  int
	Promoted   int
	NextCursor string
	HasMore    bool
}

// NewPersonFactWritebackService 构造 worker（channel 容量 256）。
func NewPersonFactWritebackService() *PersonFactWritebackService {
	return &PersonFactWritebackService{
		queue:                 make(chan *message.SessionMessage, QueueSize),
		ReverifyInterval:      5 * time.Second,
		ReverifyIdleInterval:  24 * time.Hour,
		ReverifyErrorInterval: 60 * time.Second,
	}
}

func (s *PersonFactWritebackService) log(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Start mirrors `PersonFactWritebackService.start`（幂等）。
func (s *PersonFactWritebackService) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started || s.stopping {
		s.mu.Unlock()
		return
	}
	s.started = true
	workerCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.workerLoop(workerCtx)
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.historicalReverifyLoop(workerCtx)
	}()
}

// Shutdown mirrors `PersonFactWritebackService.shutdown`。
func (s *PersonFactWritebackService) Shutdown() {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	s.stopping = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// Enqueue mirrors `PersonFactWritebackService.enqueue`（队列满时丢弃本次触发）。
//
// 返回 false 表示未入队（未启用 / 已停止 / 队列满）。
func (s *PersonFactWritebackService) Enqueue(msg *message.SessionMessage) bool {
	if !s.Enabled || msg == nil {
		return false
	}
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return false
	}
	started := s.started
	s.mu.Unlock()
	if !started {
		return false
	}
	select {
	case s.queue <- msg:
		return true
	default:
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
		s.log("人物事实写回队列已满，跳过本次回复")
		return false
	}
}

// Processed 返回已处理消息数（观测/测试用）。
func (s *PersonFactWritebackService) Processed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.processed
}

// Dropped 返回队列满丢弃数。
func (s *PersonFactWritebackService) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Pending 返回队列中待处理消息数。
func (s *PersonFactWritebackService) Pending() int { return len(s.queue) }

func (s *PersonFactWritebackService) workerLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-s.queue:
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						s.log("人物事实写回处理 panic: %v", recovered)
					}
				}()
				if err := s.HandleMessage(ctx, msg); err != nil {
					s.log("人物事实写回处理失败: %v", err)
				}
			}()
			s.mu.Lock()
			s.processed++
			s.mu.Unlock()
		}
	}
}

// HandleMessage mirrors `_handle_message`（导出以便单测与补偿路径复用）。
func (s *PersonFactWritebackService) HandleMessage(ctx context.Context, msg *message.SessionMessage) error {
	if msg == nil {
		return nil
	}
	replyText := processedText(msg)
	if replyText == "" {
		return nil
	}
	if LooksEphemeral(replyText) {
		return nil
	}
	targetPersonID := s.ResolveTargetPerson(ctx, msg)
	if targetPersonID == "" {
		return nil
	}
	evidence := s.CollectUserEvidence(ctx, msg, targetPersonID)
	if len(evidence.TargetMessages) == 0 {
		return nil
	}
	userEvidenceText := FormatUserEvidence(evidence)

	personName := personDisplayName(msg)
	if personName == "" {
		personName = targetPersonID
	}
	facts := s.extractFacts(ctx, targetPersonID, personName, replyText, userEvidenceText)
	if len(facts) == 0 {
		return nil
	}
	if s.Writer == nil {
		return nil
	}

	sessionID := strings.TrimSpace(msg.SessionID)
	if sessionID == "" {
		return nil
	}
	evidenceMessageIDs := make([]string, 0, len(evidence.TargetMessages))
	for _, item := range evidence.TargetMessages {
		if id := strings.TrimSpace(item.MessageID); id != "" {
			evidenceMessageIDs = append(evidenceMessageIDs, id)
		}
	}
	for _, item := range facts {
		fact := strings.TrimSpace(item.Fact)
		if fact == "" {
			continue
		}
		verified := false
		if s.ByID != nil {
			verified = VerifyDirectPersonFact(ctx, VerifyFactInput{
				Fact:              fact,
				EvidenceMessageID: item.EvidenceMessageID,
				EvidenceQuote:     item.EvidenceQuote,
				PersonID:          targetPersonID,
				PersonName:        personName,
				SessionID:         sessionID,
				ByID:              s.ByID,
				IsBotSelf:         s.IsBotSelf,
				PersonIDFor:       s.PersonID,
			})
		}
		profileFact := contracts.ProfileFact{
			Content:    fact,
			Category:   "stable_facts",
			Confidence: 1.0,
			Source:     "user_supported",
			CreatedAt:  msg.Timestamp,
			UpdatedAt:  time.Now(),
		}
		if verified {
			profileFact.Confidence = 1.0
			profileFact.Source = "user_supported:server_verified"
		} else {
			profileFact.Confidence = 0.5
			profileFact.Source = "user_supported:unverified"
		}
		_ = evidenceMessageIDs
		if err := s.Writer.UpsertPersonFact(ctx, targetPersonID, personName, profileFact); err != nil {
			s.log("人物事实写回内核失败: person_id=%s fact=%s err=%v", targetPersonID, truncateText(fact, 40), err)
		}
	}
	return nil
}

// ResolveTargetPerson mirrors `_resolve_target_person`（返回 person_id）。
func (s *PersonFactWritebackService) ResolveTargetPerson(
	ctx context.Context,
	msg *message.SessionMessage,
) string {
	platform := strings.TrimSpace(msg.Platform)
	userID := strings.TrimSpace(msg.MessageInfo.UserInfo.UserID)
	groupID := strings.TrimSpace(msg.GroupID())
	if s.Sessions != nil {
		if identity, ok := s.Sessions(msg.SessionID); ok && identity != nil {
			if identityPlatform := strings.TrimSpace(identity.Platform); identityPlatform != "" {
				platform = identityPlatform
			}
			if identity.IsGroup {
				groupID = strings.TrimSpace(identity.GroupID)
			}
			// session.user_id 是会话关联的用户（私聊对端 / 群聊触发者）。
			if identityUserID := strings.TrimSpace(identity.UserID); identityUserID != "" {
				userID = identityUserID
			}
		}
	}
	botSelf := func(platform, userID string) bool {
		return s.IsBotSelf != nil && s.IsBotSelf(platform, userID)
	}
	personIDOf := func(platform, userID string) string {
		if s.PersonID == nil {
			return ""
		}
		return s.PersonID(platform, userID)
	}

	if platform != "" && userID != "" && groupID == "" {
		if botSelf(platform, userID) {
			return ""
		}
		return personIDOf(platform, userID)
	}

	if replyTo := replyTargetID(msg); replyTo != "" && s.ByID != nil {
		if replies, err := s.ByID(ctx, msg.SessionID, replyTo); err == nil && len(replies) > 0 {
			if personID := personIDFromUserMessage(replies[0], platform, botSelf, personIDOf); personID != "" {
				return personID
			}
		}
	}

	if strings.TrimSpace(msg.SessionID) == "" || s.Messages == nil {
		return ""
	}
	timestamp := msg.Timestamp
	candidates, err := s.Messages(ctx, MessageQuery{
		SessionID:  msg.SessionID,
		BeforeTime: &timestamp,
		Limit:      6,
		LimitMode:  "latest",
		FilterBot:  true,
	})
	if err != nil || len(candidates) == 0 {
		return ""
	}
	for index := len(candidates) - 1; index >= 0; index-- {
		if personID := personIDFromUserMessage(candidates[index], platform, botSelf, personIDOf); personID != "" {
			return personID
		}
	}
	return ""
}

// CollectUserEvidence mirrors `_collect_user_evidence`.
func (s *PersonFactWritebackService) CollectUserEvidence(
	ctx context.Context,
	msg *message.SessionMessage,
	personID string,
) PersonFactEvidence {
	sessionID := strings.TrimSpace(msg.SessionID)
	if sessionID == "" || s.Messages == nil {
		return PersonFactEvidence{}
	}
	filter := func(items []*message.SessionMessage) []*message.SessionMessage {
		return filterTargetUserMessages(items, personID, msg.Platform, s.IsBotSelf, s.PersonID)
	}

	if replyTo := replyTargetID(msg); replyTo != "" && s.ByID != nil {
		if replies, err := s.ByID(ctx, sessionID, replyTo); err == nil {
			target := filter(replies)
			if len(target) > 0 {
				if len(target) > 3 {
					target = target[:3]
				}
				return PersonFactEvidence{
					TargetMessages:  target,
					ContextMessages: s.collectContextMessages(ctx, sessionID, msg, 8),
				}
			}
		}
	}

	timestamp := msg.Timestamp
	candidates, err := s.Messages(ctx, MessageQuery{
		SessionID:  sessionID,
		BeforeTime: &timestamp,
		Limit:      6,
		LimitMode:  "latest",
		FilterBot:  true,
	})
	if err != nil {
		return PersonFactEvidence{}
	}
	target := filter(candidates)
	if len(target) > 3 {
		target = target[len(target)-3:]
	}
	return PersonFactEvidence{TargetMessages: target, ContextMessages: candidates}
}

func (s *PersonFactWritebackService) collectContextMessages(
	ctx context.Context,
	sessionID string,
	msg *message.SessionMessage,
	limit int,
) []*message.SessionMessage {
	if s.Messages == nil {
		return nil
	}
	timestamp := msg.Timestamp
	items, err := s.Messages(ctx, MessageQuery{
		SessionID:  sessionID,
		BeforeTime: &timestamp,
		Limit:      limit,
		LimitMode:  "latest",
	})
	if err != nil {
		return nil
	}
	return items
}

type extractedFact struct {
	Fact              string
	EvidenceMessageID string
	EvidenceQuote     string
}

func (s *PersonFactWritebackService) extractFacts(
	ctx context.Context,
	personID string,
	personName string,
	replyText string,
	userEvidenceText string,
) []extractedFact {
	if s.LLM == nil {
		return nil
	}
	prompt := fmt.Sprintf(FactExtractionPromptTemplate, personName, personName, userEvidenceText, replyText)
	raw, err := s.LLM(ctx, LLMRequest{
		Task:        "utils",
		RequestType: "A_Memorix.person_fact_writeback",
		SessionID:   "",
		Messages:    []LLMMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		s.log("人物事实提取模型调用失败: %v", err)
		return nil
	}
	return ParseFactItems(raw)
}

// ParseFactItems mirrors `_parse_fact_items`（兼容旧格式：缺证据的字符串项）。
func ParseFactItems(raw string) []extractedFact {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil
	}
	var payload []any
	if err := json.Unmarshal([]byte(RepairJSONText(text)), &payload); err != nil {
		return nil
	}
	items := make([]extractedFact, 0, len(payload))
	seen := map[string]bool{}
	for _, rawItem := range payload {
		fact := ""
		item := extractedFact{}
		switch typed := rawItem.(type) {
		case map[string]any:
			fact = strings.TrimSpace(stringValue(typed["fact"]))
			item.EvidenceMessageID = strings.TrimSpace(stringValue(typed["evidence_message_id"]))
			item.EvidenceQuote = strings.TrimSpace(stringValue(typed["evidence_quote"]))
		case string:
			fact = strings.TrimSpace(typed)
		}
		if len([]rune(fact)) < 4 || seen[fact] {
			continue
		}
		seen[fact] = true
		item.Fact = fact
		items = append(items, item)
		if len(items) >= 5 {
			break
		}
	}
	return items
}

// LooksEphemeral mirrors `_looks_ephemeral`.
func LooksEphemeral(text string) bool {
	content := strings.TrimSpace(text)
	if content == "" {
		return true
	}
	markers := []string{"哈哈", "好的", "收到", "嗯嗯", "晚安", "早安", "拜拜", "谢谢", "在吗", "？"}
	if len([]rune(content)) <= 8 {
		for _, marker := range markers {
			if strings.Contains(content, marker) {
				return true
			}
		}
	}
	return false
}

// FormatUserEvidence mirrors `_format_user_evidence`.
func FormatUserEvidence(evidence PersonFactEvidence) string {
	targetLines := make([]string, 0, 3)
	targetIDs := map[string]bool{}
	limit := len(evidence.TargetMessages)
	if limit > 3 {
		limit = 3
	}
	for _, item := range evidence.TargetMessages[:limit] {
		line := formatEvidenceMessageLine(item, false, false)
		if line == "" {
			continue
		}
		if messageID := strings.TrimSpace(item.MessageID); messageID != "" {
			targetIDs[messageID] = true
			targetLines = append(targetLines, "- [消息ID: "+messageID+"] "+line)
		} else {
			targetLines = append(targetLines, "- "+line)
		}
	}

	contextLines := make([]string, 0, 8)
	contextLimit := len(evidence.ContextMessages)
	if contextLimit > 8 {
		contextLimit = 8
	}
	for _, item := range evidence.ContextMessages[:contextLimit] {
		markTarget := targetIDs[strings.TrimSpace(item.MessageID)]
		line := formatEvidenceMessageLine(item, true, markTarget)
		if line != "" {
			contextLines = append(contextLines, "- "+line)
		}
	}

	parts := make([]string, 0, 2)
	if len(targetLines) > 0 {
		parts = append(parts, "目标用户原始发言（事实值必须来自这里）：\n"+strings.Join(targetLines, "\n"))
	}
	if len(contextLines) > 0 {
		parts = append(parts,
			"邻近上下文（只用于理解省略、追问和指代，不能单独作为事实来源）：\n"+strings.Join(contextLines, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

func formatEvidenceMessageLine(item *message.SessionMessage, includeSender bool, markTarget bool) string {
	if item == nil {
		return ""
	}
	text := processedText(item)
	if text == "" {
		return ""
	}
	if !includeSender {
		return text
	}
	senderName := strings.TrimSpace(item.MessageInfo.UserInfo.DisplayName())
	prefix := ""
	if senderName != "" {
		prefix = senderName + ": "
	}
	targetMarker := ""
	if markTarget {
		targetMarker = " [目标用户发言]"
	}
	return prefix + text + targetMarker
}

func personIDFromUserMessage(
	item *message.SessionMessage,
	fallbackPlatform string,
	botSelf func(platform, userID string) bool,
	personIDOf func(platform, userID string) string,
) string {
	if item == nil {
		return ""
	}
	platform := strings.TrimSpace(item.Platform)
	if platform == "" {
		platform = fallbackPlatform
	}
	userID := strings.TrimSpace(item.MessageInfo.UserInfo.UserID)
	if platform == "" || userID == "" || botSelf(platform, userID) {
		return ""
	}
	return personIDOf(platform, userID)
}

func filterTargetUserMessages(
	items []*message.SessionMessage,
	personID string,
	fallbackPlatform string,
	botSelf func(platform, userID string) bool,
	personIDOf func(platform, userID string) string,
) []*message.SessionMessage {
	filtered := make([]*message.SessionMessage, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		if item == nil {
			continue
		}
		platform := strings.TrimSpace(item.Platform)
		if platform == "" {
			platform = fallbackPlatform
		}
		userID := strings.TrimSpace(item.MessageInfo.UserInfo.UserID)
		if platform == "" || userID == "" || botSelf(platform, userID) {
			continue
		}
		if personID != "" && personIDOf(platform, userID) != personID {
			continue
		}
		text := processedText(item)
		if text == "" {
			continue
		}
		dedupKey := strings.TrimSpace(item.MessageID)
		if dedupKey == "" {
			dedupKey = platform + ":" + userID + ":" + text
		}
		if seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true
		filtered = append(filtered, item)
	}
	return filtered
}

func replyTargetID(msg *message.SessionMessage) string {
	if msg == nil || msg.ReplyTo == nil {
		return ""
	}
	return strings.TrimSpace(*msg.ReplyTo)
}

func processedText(msg *message.SessionMessage) string {
	if msg == nil || msg.ProcessedPlainText == nil {
		return ""
	}
	return strings.TrimSpace(*msg.ProcessedPlainText)
}

func personDisplayName(msg *message.SessionMessage) string {
	if msg == nil {
		return ""
	}
	return strings.TrimSpace(msg.MessageInfo.UserInfo.DisplayName())
}

// ---------------------------------------------------------------------------
// 人物事实直证校验（对齐 services/person_fact_verifier.py）
// ---------------------------------------------------------------------------

// VerifyFactInput 是 verify_direct_person_fact 的入参集合。
type VerifyFactInput struct {
	Fact              string
	EvidenceMessageID string
	EvidenceQuote     string
	PersonID          string
	PersonName        string
	SessionID         string
	ByID              MessageByIDFunc
	IsBotSelf         IsBotSelfFunc
	PersonIDFor       PersonIDFunc
}

var (
	factTemporaryMarkers = []string{"今天", "昨天", "明天", "刚才", "暂时", "可能", "似乎", "也许", "打算", "计划"}
	factOtherSubjects    = []string{"我们", "我朋友", "我的朋友", "我同事", "我的同事", "我妈", "我爸", "我家人"}
	factSelfPrefixes     = []string{"我", "本人", "自己"}
	factThirdPerson      = []string{"他", "她", "用户", "该用户"}
)

// VerifyDirectPersonFact mirrors verify_direct_person_fact：只接受可定位到同一
// 聊天流、同一人物的直接自述和称谓改写。
func VerifyDirectPersonFact(ctx context.Context, input VerifyFactInput) bool {
	if strings.TrimSpace(input.Fact) == "" || strings.TrimSpace(input.EvidenceMessageID) == "" ||
		strings.TrimSpace(input.EvidenceQuote) == "" || strings.TrimSpace(input.PersonID) == "" ||
		strings.TrimSpace(input.SessionID) == "" {
		return false
	}
	quote := normalizeFactText(input.EvidenceQuote)
	if quote == "" {
		return false
	}
	for _, marker := range factTemporaryMarkers {
		if strings.Contains(quote, marker) {
			return false
		}
	}
	sourcePredicate := factPredicate(input.EvidenceQuote, input.PersonName, true)
	if sourcePredicate == "" {
		return false
	}
	factPredicateValue := factPredicate(input.Fact, input.PersonName, false)
	if factPredicateValue == "" || sourcePredicate != factPredicateValue {
		return false
	}
	if input.ByID == nil {
		return false
	}
	messages, err := input.ByID(ctx, input.SessionID, input.EvidenceMessageID)
	if err != nil || len(messages) != 1 || messages[0] == nil {
		return false
	}
	record := messages[0]
	platform := strings.TrimSpace(record.Platform)
	userID := strings.TrimSpace(record.MessageInfo.UserInfo.UserID)
	if platform == "" || userID == "" {
		return false
	}
	if input.IsBotSelf != nil && input.IsBotSelf(platform, userID) {
		return false
	}
	if input.PersonIDFor == nil || input.PersonIDFor(platform, userID) != input.PersonID {
		return false
	}
	messageText := strings.ToLower(normalizeFactText(processedText(record)))
	quotedText := strings.ToLower(strings.TrimSpace(normalizeFactText(input.EvidenceQuote)))
	position := strings.Index(messageText, quotedText)
	if position < 0 {
		return false
	}
	if strings.HasSuffix(quotedText, "？") || strings.HasSuffix(quotedText, "?") {
		return false
	}
	prefix := strings.TrimRight(messageText[:position], " \t")
	if prefix != "" {
		last := prefix[len(prefix)-1:]
		if !strings.Contains("。！？；\n", last) {
			return false
		}
	}
	suffix := strings.TrimSpace(messageText[position+len(quotedText):])
	return suffix == "" || suffix == "。" || suffix == "！" || suffix == "；"
}

// factPredicate mirrors `_predicate`。
func factPredicate(value string, personName string, firstPerson bool) string {
	normalized := normalizeFactText(value)
	if firstPerson {
		for _, prefix := range factOtherSubjects {
			if strings.HasPrefix(normalized, normalizeFactText(prefix)) {
				return ""
			}
		}
	}
	prefixes := make([]string, 0, 5)
	if firstPerson {
		prefixes = append(prefixes, factSelfPrefixes...)
	} else {
		prefixes = append(prefixes, personName)
		prefixes = append(prefixes, factThirdPerson...)
	}
	sortByLengthDesc(prefixes)
	for _, prefix := range prefixes {
		normalizedPrefix := normalizeFactText(prefix)
		if normalizedPrefix == "" {
			continue
		}
		if strings.HasPrefix(normalized, normalizedPrefix) {
			predicate := normalized[len(normalizedPrefix):]
			if len([]rune(predicate)) >= 2 {
				return predicate
			}
			return ""
		}
	}
	return ""
}

// normalizeFactText mirrors `_normalized_text`（NFKC casefold + 去非单词字符）。
// Go 版用「保留字母/数字/汉字/下划线」近似 `re.sub(r"[^\w]", "", ...)`。
func normalizeFactText(value string) string {
	lowered := strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, char := range lowered {
		switch {
		case char >= 'a' && char <= 'z':
			builder.WriteRune(char)
		case char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '_':
			builder.WriteRune(char)
		case char >= 0x2E80 && char <= 0x9FFF:
			builder.WriteRune(char)
		case char >= 0xF900 && char <= 0xFAFF:
			builder.WriteRune(char)
		case char >= 0xFF00 && char <= 0xFFEF:
			// 全角形式按半角/等价处理（NFKC 的近似：全角 ASCII 归一）。
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

func sortByLengthDesc(values []string) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0; cursor-- {
			if len([]rune(values[cursor])) > len([]rune(values[cursor-1])) {
				values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
				continue
			}
			break
		}
	}
}

// ---------------------------------------------------------------------------
// 历史事实重验游标
// ---------------------------------------------------------------------------

// FactReverifyCursorFilename mirrors `person_fact_reverify_cursor.json`.
const FactReverifyCursorFilename = "person_fact_reverify_cursor.json"

// FactReverifyProgress mirrors the cursor file payload.
type FactReverifyProgress struct {
	Cursor         string  `json:"cursor"`
	ProcessedTotal int     `json:"processed_total"`
	PromotedTotal  int     `json:"promoted_total"`
	UpdatedAt      float64 `json:"updated_at"`
}

// LoadFactReverifyProgress 读取游标文件（不存在/损坏时返回空进度）。
func LoadFactReverifyProgress(dataDir string) FactReverifyProgress {
	if strings.TrimSpace(dataDir) == "" {
		return FactReverifyProgress{}
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, FactReverifyCursorFilename))
	if err != nil {
		return FactReverifyProgress{}
	}
	var progress FactReverifyProgress
	if err := json.Unmarshal(raw, &progress); err != nil {
		return FactReverifyProgress{}
	}
	return progress
}

// SaveFactReverifyProgress 原子落盘（tmp + rename，对齐 os.replace 语义）。
func SaveFactReverifyProgress(dataDir string, progress FactReverifyProgress) error {
	if strings.TrimSpace(dataDir) == "" {
		return errors.New("memory: 人物事实重验缺少数据目录")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	progress.UpdatedAt = float64(time.Now().UnixNano()) / 1e9
	raw, err := json.Marshal(progress)
	if err != nil {
		return err
	}
	temporary := filepath.Join(dataDir, FactReverifyCursorFilename+".tmp")
	if err := os.WriteFile(temporary, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(dataDir, FactReverifyCursorFilename))
}

// RunFactReverifyBatch 执行一批历史事实重验并推进游标（对齐
// `_historical_reverify_loop` 的 state/progress 语义）。
func (s *PersonFactWritebackService) RunFactReverifyBatch(ctx context.Context) (ReverifyResult, error) {
	if s.Reverify == nil {
		return ReverifyResult{}, nil
	}
	progress := LoadFactReverifyProgress(s.KernelDataDir)
	result, err := s.Reverify(ctx, progress.Cursor, 50)
	if err != nil {
		return ReverifyResult{}, err
	}
	next := progress
	if !result.HasMore {
		next.Cursor = ""
	} else {
		next.Cursor = result.NextCursor
	}
	next.ProcessedTotal = progress.ProcessedTotal + result.Processed
	next.PromotedTotal = progress.PromotedTotal + result.Promoted
	if err := SaveFactReverifyProgress(s.KernelDataDir, next); err != nil {
		return result, err
	}
	if result.Promoted > 0 {
		s.log("历史人物事实重验完成一批：晋升 %d 条", result.Promoted)
	}
	return result, nil
}

func (s *PersonFactWritebackService) historicalReverifyLoop(ctx context.Context) {
	interval := s.ReverifyInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(interval):
	}
	idleInterval := s.ReverifyIdleInterval
	if idleInterval <= 0 {
		idleInterval = 24 * time.Hour
	}
	errorInterval := s.ReverifyErrorInterval
	if errorInterval <= 0 {
		errorInterval = 60 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		wait := idleInterval
		result, err := s.RunFactReverifyBatch(ctx)
		switch {
		case err != nil:
			s.log("历史人物事实重验失败: %v", err)
			wait = errorInterval
		case result.HasMore:
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// ---------------------------------------------------------------------------
// ChatSummaryWritebackService
// ---------------------------------------------------------------------------

// ChatSummaryWritebackState mirrors ChatSummaryWritebackState。
type ChatSummaryWritebackState struct {
	LastTriggerMessageCount int
	LastTriggerTime         float64
}

// ChatSummaryWritebackService 按累积消息量触发聊天摘要写回。
type ChatSummaryWritebackService struct {
	Enabled bool
	// MessageThreshold mirrors chat_summary_writeback_message_threshold（默认 36）。
	MessageThreshold int
	// ContextLength mirrors chat_summary_writeback_context_length（默认 36）。
	ContextLength int
	// BotName / PersonalityContext 供 a_memorix_chat_summary.prompt 渲染。
	BotName            string
	PersonalityContext string
	// PreviousSummaryContext mirrors `previous_summary_context`（默认空）。
	PreviousSummaryContext string

	LLM      LLMFunc
	Prompts  PromptFunc
	Kernel   SummaryIngester
	Messages MessageQueryFunc
	Counts   MessageCountFunc

	// CheckpointReader 恢复触发游标（对齐 `_load_last_trigger_message_count`）。
	CheckpointReader func(ctx context.Context, sessionID string) (int, bool)
	// StatePath 持久化会话触发游标（空字符串则仅内存）。
	StatePath string

	Now  func() time.Time
	Logf func(format string, args ...any)

	queue     chan *message.SessionMessage
	mu        sync.Mutex
	started   bool
	stopping  bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	states    map[string]*ChatSummaryWritebackState
	processed int
	dropped   int
}

// NewChatSummaryWritebackService 构造 worker（channel 容量 256）。
func NewChatSummaryWritebackService() *ChatSummaryWritebackService {
	return &ChatSummaryWritebackService{
		queue:            make(chan *message.SessionMessage, QueueSize),
		MessageThreshold: 36,
		ContextLength:    36,
		states:           map[string]*ChatSummaryWritebackState{},
	}
}

func (s *ChatSummaryWritebackService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *ChatSummaryWritebackService) log(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *ChatSummaryWritebackService) messageThreshold() int {
	if s.MessageThreshold > 0 {
		return s.MessageThreshold
	}
	return 1
}

func (s *ChatSummaryWritebackService) contextLength() int {
	if s.ContextLength > 0 {
		return s.ContextLength
	}
	return 1
}

// Start mirrors `ChatSummaryWritebackService.start`.
func (s *ChatSummaryWritebackService) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started || s.stopping {
		s.mu.Unlock()
		return
	}
	s.started = true
	workerCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.workerLoop(workerCtx)
	}()
}

// Shutdown mirrors `ChatSummaryWritebackService.shutdown`.
func (s *ChatSummaryWritebackService) Shutdown() {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	s.stopping = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// Enqueue mirrors `ChatSummaryWritebackService.enqueue`.
func (s *ChatSummaryWritebackService) Enqueue(msg *message.SessionMessage) bool {
	if !s.Enabled || msg == nil {
		return false
	}
	s.mu.Lock()
	if s.stopping || !s.started {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	select {
	case s.queue <- msg:
		return true
	default:
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
		s.log("聊天摘要写回队列已满，跳过本次触发")
		return false
	}
}

// Processed 返回已处理触发数。
func (s *ChatSummaryWritebackService) Processed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.processed
}

// Pending 返回队列长度。
func (s *ChatSummaryWritebackService) Pending() int { return len(s.queue) }

// State 返回会话触发状态快照（测试用）。
func (s *ChatSummaryWritebackService) State(sessionID string) (ChatSummaryWritebackState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[sessionID]
	if !ok {
		return ChatSummaryWritebackState{}, false
	}
	return *state, true
}

// SetState 注入会话触发状态（重启恢复用）。
func (s *ChatSummaryWritebackService) SetState(sessionID string, state ChatSummaryWritebackState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = map[string]*ChatSummaryWritebackState{}
	}
	s.states[sessionID] = &state
}

func (s *ChatSummaryWritebackService) workerLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-s.queue:
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						s.log("聊天摘要写回处理 panic: %v", recovered)
					}
				}()
				if err := s.HandleMessage(ctx, msg); err != nil {
					s.log("聊天摘要写回处理失败: %v", err)
				}
			}()
			s.mu.Lock()
			s.processed++
			s.mu.Unlock()
		}
	}
}

// HandleMessage mirrors `ChatSummaryWritebackService._handle_message`。
func (s *ChatSummaryWritebackService) HandleMessage(ctx context.Context, msg *message.SessionMessage) error {
	sessionID := strings.TrimSpace(msg.SessionID)
	if sessionID == "" {
		return nil
	}
	messageTime := msg.Timestamp
	total, err := s.countMessagesUntilTrigger(ctx, sessionID, messageTime)
	if err != nil || total <= 0 {
		return err
	}

	s.mu.Lock()
	state, ok := s.states[sessionID]
	if !ok {
		state = &ChatSummaryWritebackState{}
		s.states[sessionID] = state
	}
	s.mu.Unlock()

	if state.LastTriggerMessageCount == 0 && s.CheckpointReader != nil {
		if restored, found := s.CheckpointReader(ctx, sessionID); found && restored > 0 {
			state.LastTriggerMessageCount = restored
			state.LastTriggerTime = float64(s.now().Unix())
			s.persistState()
		}
	}

	pending := total - state.LastTriggerMessageCount
	if pending < 0 {
		pending = 0
	}
	if pending < s.messageThreshold() {
		return nil
	}

	contextLength := computeContextLength(s.contextLength(), pending)
	summaryText, err := s.generateSummary(ctx, sessionID, contextLength, msg)
	if err == nil && summaryText != "" && s.Kernel != nil {
		if err = s.Kernel.IngestSummary(ctx, sessionID, summaryText, messageTime); err != nil {
			s.log("聊天摘要自动写回失败: session_id=%s detail=%v", sessionID, err)
			return nil
		}
	} else if err != nil {
		s.log("聊天摘要自动写回失败: session_id=%s detail=%v", sessionID, err)
		return nil
	}

	s.mu.Lock()
	state.LastTriggerMessageCount = total
	state.LastTriggerTime = float64(s.now().Unix())
	s.mu.Unlock()
	s.persistState()
	s.log("聊天摘要自动写回成功: session_id=%s trigger=message_threshold total_messages=%d context_length=%d",
		sessionID, total, contextLength)
	return nil
}

func (s *ChatSummaryWritebackService) countMessagesUntilTrigger(
	ctx context.Context,
	sessionID string,
	messageTime time.Time,
) (int, error) {
	if s.Counts == nil {
		return 0, nil
	}
	return s.Counts(ctx, sessionID, &messageTime)
}

// generateSummary 渲染 a_memorix_chat_summary.prompt 并对当前窗口做增量总结。
func (s *ChatSummaryWritebackService) generateSummary(
	ctx context.Context,
	sessionID string,
	contextLength int,
	msg *message.SessionMessage,
) (string, error) {
	if s.LLM == nil || s.Prompts == nil || s.Messages == nil {
		return "", nil
	}
	messageTime := msg.Timestamp
	items, err := s.Messages(ctx, MessageQuery{
		SessionID:  sessionID,
		BeforeTime: &messageTime,
		Limit:      contextLength,
		LimitMode:  "latest",
	})
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", nil
	}
	chatHistory := FormatChatHistoryForSummary(items)
	prompt, err := s.Prompts("a_memorix_chat_summary", map[string]string{
		"bot_name":                 defaultString(s.BotName, "麦麦"),
		"personality_context":      s.PersonalityContext,
		"previous_summary_context": s.PreviousSummaryContext,
		"chat_history":             chatHistory,
		"image_evidence_catalog":   "",
	})
	if err != nil {
		return "", err
	}
	raw, err := s.LLM(ctx, LLMRequest{
		Task:        "utils",
		RequestType: "A_Memorix.chat_summary_writeback",
		SessionID:   sessionID,
		Messages:    []LLMMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", err
	}
	return ParseChatSummaryResponse(raw), nil
}

// ParseChatSummaryResponse 解析 a_memorix_chat_summary.prompt 的 JSON 输出，
// 只取 summary（空 summary 表示无增量，调用方跳过写入）。
func ParseChatSummaryResponse(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(RepairJSONText(raw)), &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(stringValue(payload["summary"]))
}

// FormatChatHistoryForSummary 把消息窗口渲染为「发言者: 内容」多行文本。
func FormatChatHistoryForSummary(items []*message.SessionMessage) string {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		text := processedText(item)
		if text == "" {
			continue
		}
		sender := strings.TrimSpace(item.MessageInfo.UserInfo.DisplayName())
		if sender == "" {
			sender = "未知用户"
		}
		lines = append(lines, fmt.Sprintf("[%s] %s: %s", item.Timestamp.Format("15:04:05"), sender, text))
	}
	return strings.Join(lines, "\n")
}

// computeContextLength mirrors `_effective_context_length`.
func computeContextLength(configuredContextLength int, pendingMessageCount int) int {
	configured := configuredContextLength
	if configured < 1 {
		configured = 1
	}
	pending := pendingMessageCount
	if pending < 1 {
		pending = 1
	}
	if configured < pending {
		return configured
	}
	return pending
}

type persistedChatSummaryState struct {
	States map[string]ChatSummaryWritebackState `json:"states"`
}

func (s *ChatSummaryWritebackService) persistState() {
	if strings.TrimSpace(s.StatePath) == "" {
		return
	}
	s.mu.Lock()
	payload := persistedChatSummaryState{States: map[string]ChatSummaryWritebackState{}}
	for sessionID, state := range s.states {
		payload.States[sessionID] = *state
	}
	s.mu.Unlock()
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.StatePath), 0o755); err != nil {
		return
	}
	temporary := s.StatePath + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(temporary, s.StatePath)
}

// LoadChatSummaryState 从磁盘恢复会话触发游标（重启后避免重复摘要）。
func (s *ChatSummaryWritebackService) LoadChatSummaryState() error {
	if strings.TrimSpace(s.StatePath) == "" {
		return nil
	}
	raw, err := os.ReadFile(s.StatePath)
	if err != nil {
		return nil
	}
	var payload persistedChatSummaryState
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = map[string]*ChatSummaryWritebackState{}
	}
	for sessionID, state := range payload.States {
		copied := state
		s.states[sessionID] = &copied
	}
	return nil
}

// ---------------------------------------------------------------------------
// ImageMemoryWritebackService
// ---------------------------------------------------------------------------

// ImageWritebackJob mirrors 一行 image_writeback_jobs。
type ImageWritebackJob struct {
	SessionID string
	MessageID string
	Status    string
	Attempts  int
	RetryAt   float64
	LastError string
	UpdatedAt float64
}

// ImageWritebackJournal 是图片写回任务队列（对齐 services/image_writeback_journal.py）。
//
// 默认内存实现（幂等的 map + 状态机）；路径非空时由调用方注入文件实现
// （TestImageWritebackJournalPersistence 覆盖 SQLite 版本语义）。
type ImageWritebackJournal struct {
	mu    sync.Mutex
	jobs  map[string]*ImageWritebackJob
	order []string
	// MaxAttempts mirrors image_memory.job_max_retries + 1。
	MaxAttempts int
	// Now 可注入（测试用）。
	Now func() float64
	// Backoff 计算失败后的重试延迟（nil 时用 `min(300, 2^attempts)` 秒）。
	Backoff func(attempts int) float64
}

// NewImageWritebackJournal 构造 journal。
func NewImageWritebackJournal() *ImageWritebackJournal {
	return &ImageWritebackJournal{jobs: map[string]*ImageWritebackJob{}, MaxAttempts: 6}
}

func (j *ImageWritebackJournal) now() float64 {
	if j.Now != nil {
		return j.Now()
	}
	return float64(time.Now().UnixNano()) / 1e9
}

func journalKey(sessionID, messageID string) string { return sessionID + "\x00" + messageID }

// Enqueue mirrors `ImageWritebackJournal.enqueue`（INSERT OR IGNORE，幂等）。
func (j *ImageWritebackJournal) Enqueue(sessionID, messageID string) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(messageID) == "" {
		return errors.New("图片入库任务缺少真实聊天流或消息 ID")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	key := journalKey(sessionID, messageID)
	if _, exists := j.jobs[key]; exists {
		return nil
	}
	j.jobs[key] = &ImageWritebackJob{
		SessionID: sessionID,
		MessageID: messageID,
		Status:    "pending",
		UpdatedAt: j.now(),
	}
	j.order = append(j.order, key)
	return nil
}

// NextJob mirrors `next_job`（成功前始终保留 pending）。
func (j *ImageWritebackJournal) NextJob() (ImageWritebackJob, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	var best *ImageWritebackJob
	for _, key := range j.order {
		job := j.jobs[key]
		if job == nil || job.Status != "pending" || job.RetryAt > now {
			continue
		}
		if best == nil || job.RetryAt < best.RetryAt ||
			(job.RetryAt == best.RetryAt && job.UpdatedAt < best.UpdatedAt) {
			best = job
		}
	}
	if best == nil {
		return ImageWritebackJob{}, false
	}
	return *best, true
}

// Complete mirrors `complete`.
func (j *ImageWritebackJournal) Complete(sessionID, messageID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job, ok := j.jobs[journalKey(sessionID, messageID)]; ok {
		job.Status = "done"
		job.LastError = ""
		job.UpdatedAt = j.now()
	}
}

// Fail mirrors `fail`（指数退避 min(300, 2^attempts)，超过上限转 failed）。
func (j *ImageWritebackJournal) Fail(job ImageWritebackJob, errText string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	record, ok := j.jobs[journalKey(job.SessionID, job.MessageID)]
	if !ok {
		return
	}
	record.Attempts++
	attempts := record.Attempts
	backoff := float64(int(1) << min(attempts, 9))
	if backoff > 300 {
		backoff = 300
	}
	if j.Backoff != nil {
		backoff = j.Backoff(attempts)
	}
	maxAttempts := j.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 6
	}
	status := "pending"
	if attempts >= maxAttempts {
		status = "failed"
	}
	record.Status = status
	if status == "failed" {
		record.RetryAt = 0
	} else {
		record.RetryAt = j.now() + backoff
	}
	record.LastError = errText
	record.UpdatedAt = j.now()
}

// RetryFailed mirrors `retry_failed`。
func (j *ImageWritebackJournal) RetryFailed() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	count := 0
	for _, job := range j.jobs {
		if job.Status != "failed" {
			continue
		}
		job.Status = "pending"
		job.Attempts = 0
		job.RetryAt = 0
		job.UpdatedAt = j.now()
		count++
	}
	return count
}

// Job 返回单条任务快照。
func (j *ImageWritebackJournal) Job(sessionID, messageID string) (ImageWritebackJob, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.jobs[journalKey(sessionID, messageID)]
	if !ok {
		return ImageWritebackJob{}, false
	}
	return *job, true
}

// Len 返回任务总数。
func (j *ImageWritebackJournal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.jobs)
}

// ImageMemoryWritebackService 把聊天消息里的图片（含描述）写入内核图片记忆。
//
// journal 保证重投递安全：同一 (session_id, message_id) 只会成功写入一次，
// 失败任务保留 pending 并可重试。
type ImageMemoryWritebackService struct {
	Enabled bool
	Kernel  ImageMemoryIngester
	// Descriptions 取某图片 hash 的缓存描述（对齐
	// image_manager.get_cached_image_description）。
	Descriptions func(imageHash string) string
	Journal      *ImageWritebackJournal
	// PollInterval mirrors image_memory.job_poll_interval_seconds（默认 2s）。
	PollInterval time.Duration

	Logf func(format string, args ...any)

	mu         sync.Mutex
	started    bool
	stopping   bool
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	resolverFn MessageResolver
	successes  int
	failures   int
}

// NewImageMemoryWritebackService 构造 worker。
func NewImageMemoryWritebackService() *ImageMemoryWritebackService {
	return &ImageMemoryWritebackService{
		Journal:      NewImageWritebackJournal(),
		PollInterval: 2 * time.Second,
	}
}

func (s *ImageMemoryWritebackService) log(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}


// Start mirrors `ImageMemoryWritebackService.start`（幂等；未启用时不启动）。
func (s *ImageMemoryWritebackService) Start(ctx context.Context) {
	if !s.Enabled {
		return
	}
	s.mu.Lock()
	if s.started || s.stopping {
		s.mu.Unlock()
		return
	}
	s.started = true
	workerCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.workerLoop(workerCtx)
	}()
}

// Shutdown mirrors `ImageMemoryWritebackService.shutdown`。
func (s *ImageMemoryWritebackService) Shutdown() {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	s.stopping = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// Enqueue mirrors `ImageMemoryWritebackService.enqueue`（只入队非空图片消息）。
func (s *ImageMemoryWritebackService) Enqueue(msg *message.SessionMessage) bool {
	if !s.Enabled || msg == nil || s.Journal == nil {
		return false
	}
	if len(IterMessageImageComponents(msg)) == 0 {
		return false
	}
	s.mu.Lock()
	stopping := s.stopping
	s.mu.Unlock()
	if stopping {
		return false
	}
	if err := s.Journal.Enqueue(msg.SessionID, msg.MessageID); err != nil {
		s.log("图片写回入队失败: %v", err)
		return false
	}
	return true
}

// Successes / Failures 返回观测计数。
func (s *ImageMemoryWritebackService) Successes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.successes
}

// Failures 返回失败计数。
func (s *ImageMemoryWritebackService) Failures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures
}

// HandleMessage mirrors `ImageMemoryWritebackService._handle_message`:
// 处理一条消息中的全部图片组件；成功组件由 external_ref 去重（幂等）。
func (s *ImageMemoryWritebackService) HandleMessage(ctx context.Context, msg *message.SessionMessage) (bool, error) {
	if msg == nil || s.Kernel == nil {
		return false, nil
	}
	components := IterMessageImageComponents(msg)
	if len(components) == 0 {
		return false, nil
	}
	chatID := strings.TrimSpace(msg.SessionID)
	messageID := strings.TrimSpace(msg.MessageID)
	if chatID == "" || messageID == "" {
		return false, errors.New("图片消息缺少真实 chat_id 或 message_id")
	}
	userStatement := ExplicitUserText(msg)
	var failures []error
	for _, component := range components {
		hash := strings.TrimSpace(component.BinaryHash)
		description := strings.TrimSpace(component.Content)
		if s.Descriptions != nil && hash != "" {
			if cached := strings.TrimSpace(s.Descriptions(hash)); cached != "" {
				description = "[图片：" + cached + "]"
			}
		}
		if description == "" && hash == "" {
			continue
		}
		if description == "" {
			description = userStatement
		}
		if description == "" {
			continue
		}
		if err := s.Kernel.IngestImageMemory(ctx, chatID, description, hash, msg.Timestamp); err != nil {
			failures = append(failures, fmt.Errorf("图片组件处理失败: message_id=%s path=%s: %w",
				messageID, component.ComponentPath, err))
		}
	}
	if len(failures) > 0 {
		return false, errors.Join(failures...)
	}
	return true, nil
}

func (s *ImageMemoryWritebackService) workerLoop(ctx context.Context) {
	interval := s.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		job, ok := s.Journal.NextJob()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
				continue
			}
		}
		// 任务里只有 (session_id, message_id)，消息内容由装配层通过 Resolver 提供。
		resolver := s.resolver()
		if resolver == nil {
			s.Journal.Fail(job, "图片写回缺少消息解析器")
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
				continue
			}
		}
		source, err := resolver(ctx, job.SessionID, job.MessageID)
		if err != nil || source == nil {
			s.Journal.Fail(job, "图片入库任务的来源消息不存在")
			s.mu.Lock()
			s.failures++
			s.mu.Unlock()
			continue
		}
		if _, err := s.HandleMessage(ctx, source); err != nil {
			s.Journal.Fail(job, err.Error())
			s.mu.Lock()
			s.failures++
			s.mu.Unlock()
			s.log("图片记忆写入失败，任务已保留: %v", err)
			continue
		}
		s.Journal.Complete(job.SessionID, job.MessageID)
		s.mu.Lock()
		s.successes++
		s.mu.Unlock()
	}
}

// MessageResolver 按 (session_id, message_id) 读取来源消息（装配层用
// message_repository 适配）。
type MessageResolver func(ctx context.Context, sessionID string, messageID string) (*message.SessionMessage, error)

// SetResolver 注入来源消息解析器（默认 nil：任务保留等待装配层注入）。
func (s *ImageMemoryWritebackService) SetResolver(resolver MessageResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolverFn = resolver
}

func (s *ImageMemoryWritebackService) resolver() MessageResolver {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolverFn
}

// BackfillMessages mirrors `backfill_messages`（幂等回填历史消息）。
func (s *ImageMemoryWritebackService) BackfillMessages(
	ctx context.Context,
	messages []*message.SessionMessage,
) map[string]int {
	processed, failed := 0, 0
	for _, msg := range messages {
		ok, err := s.HandleMessage(ctx, msg)
		if err != nil {
			failed++
			s.log("历史图片记忆回填失败: message_id=%s error=%v", msg.MessageID, err)
			continue
		}
		if ok {
			processed++
		}
	}
	return map[string]int{"processed_messages": processed, "failed_messages": failed}
}

// ImageComponentRef mirrors iter_message_image_components 的一项。
type ImageComponentRef struct {
	ComponentPath string
	BinaryHash    string
	Content       string
	BinaryData    []byte
}

// IterMessageImageComponents mirrors A_memorix 的 iter_message_image_components
// （只收 image 组件，含嵌套 forward 内容的稳定下标路径）。
func IterMessageImageComponents(msg *message.SessionMessage) []ImageComponentRef {
	if msg == nil || msg.RawMessage == nil {
		return nil
	}
	return iterImageComponents(msg.RawMessage.Components, "")
}

func iterImageComponents(components []message.Component, prefix string) []ImageComponentRef {
	out := make([]ImageComponentRef, 0, len(components))
	for index, component := range components {
		path := fmt.Sprintf("%d", index)
		if prefix != "" {
			path = prefix + "." + fmt.Sprintf("%d", index)
		}
		if image, ok := component.(*message.ImageComponent); ok {
			out = append(out, ImageComponentRef{
				ComponentPath: path,
				BinaryHash:    strings.TrimSpace(image.BinaryHash),
				Content:       strings.TrimSpace(image.Content),
				BinaryData:    image.BinaryData,
			})
		}
		switch nested := component.(type) {
		case *message.ForwardComponent:
			out = append(out, iterImageComponents(nested.Content, path)...)
		case *message.ForwardNodeComponent:
			for nodeIndex, node := range nested.ForwardComponents {
				if node == nil {
					continue
				}
				out = append(out, iterImageComponents(node.Content, fmt.Sprintf("%s.%d", path, nodeIndex))...)
			}
		}
	}
	return out
}

// ExplicitUserText mirrors `_explicit_user_text`（只取用户文本组件）。
func ExplicitUserText(msg *message.SessionMessage) string {
	if msg == nil || msg.RawMessage == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	for _, component := range msg.RawMessage.Components {
		text, ok := component.(*message.TextComponent)
		if !ok {
			continue
		}
		if trimmed := strings.TrimSpace(text.Text); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, " ")
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// ---------------------------------------------------------------------------
// MemoryAutomationService
// ---------------------------------------------------------------------------

// MemoryAutomationService mirrors MemoryAutomationService：三个 worker 的
// 触发点聚合（on_incoming_message → image；on_message_sent → fact + summary）。
type MemoryAutomationService struct {
	FactWriteback    *PersonFactWritebackService
	SummaryWriteback *ChatSummaryWritebackService
	ImageWriteback   *ImageMemoryWritebackService
	Logf             func(format string, args ...any)

	mu      sync.Mutex
	started bool
	ctx     context.Context
}

// NewMemoryAutomationService 构造三个 worker（未配置的字段按默认构造补齐）。
func NewMemoryAutomationService(
	fact *PersonFactWritebackService,
	summary *ChatSummaryWritebackService,
	image *ImageMemoryWritebackService,
) *MemoryAutomationService {
	if fact == nil {
		fact = NewPersonFactWritebackService()
	}
	if summary == nil {
		summary = NewChatSummaryWritebackService()
	}
	if image == nil {
		image = NewImageMemoryWritebackService()
	}
	return &MemoryAutomationService{FactWriteback: fact, SummaryWriteback: summary, ImageWriteback: image}
}

// Start mirrors `MemoryAutomationService.start`（幂等）。
func (s *MemoryAutomationService) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.ctx = ctx
	s.mu.Unlock()
	if s.FactWriteback != nil {
		s.FactWriteback.Start(ctx)
	}
	if s.SummaryWriteback != nil {
		s.SummaryWriteback.Start(ctx)
	}
	if s.ImageWriteback != nil {
		s.ImageWriteback.Start(ctx)
	}
}

// Started 报告服务是否已启动。
func (s *MemoryAutomationService) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// Shutdown mirrors `MemoryAutomationService.shutdown`（image → summary → fact）。
func (s *MemoryAutomationService) Shutdown() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	s.mu.Unlock()
	if s.ImageWriteback != nil {
		s.ImageWriteback.Shutdown()
	}
	if s.SummaryWriteback != nil {
		s.SummaryWriteback.Shutdown()
	}
	if s.FactWriteback != nil {
		s.FactWriteback.Shutdown()
	}
}

// OnIncomingMessage mirrors `on_incoming_message`（惰性启动 + 图片写回入队）。
func (s *MemoryAutomationService) OnIncomingMessage(msg *message.SessionMessage) {
	if !s.Started() && s.ctx != nil {
		s.Start(s.ctx)
	}
	if s.ImageWriteback != nil {
		s.ImageWriteback.Enqueue(msg)
	}
}

// OnMessageSent mirrors `on_message_sent`（事实 + 摘要写回入队）。
func (s *MemoryAutomationService) OnMessageSent(msg *message.SessionMessage) {
	if !s.Started() && s.ctx != nil {
		s.Start(s.ctx)
	}
	if s.FactWriteback != nil {
		s.FactWriteback.Enqueue(msg)
	}
	if s.SummaryWriteback != nil {
		s.SummaryWriteback.Enqueue(msg)
	}
}

// SetContext 注入惰性启动用的父 context。
func (s *MemoryAutomationService) SetContext(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctx = ctx
}

func min(a int, b int) int {
	if a < b {
		return a
	}
	return b
}
