package learners

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file mirrors 参考/MaiBot/src/learners/jargon_learner.py 与
// src/learners/jargon_explainer.py 以及 src/maisaka/jargon_context_matcher.py:
// 黑话学习批次闸门 + learn_jargon 解释入库 + 上下文机械匹配注入。

// MaxJargonReferenceMatches mirrors MAX_JARGON_REFERENCE_MATCHES.
const MaxJargonReferenceMatches = 10

// JargonReferenceHeader mirrors _JARGON_REFERENCE_HEADER.
const JargonReferenceHeader = "以下黑话来自当前上下文中其他用户消息的机械匹配，仅作理解聊天语境的参考："

// JargonReferenceDisplayPrefix mirrors _JARGON_REFERENCE_DISPLAY_PREFIX.
const JargonReferenceDisplayPrefix = "[黑话参考]"

// FilteredToolNames mirrors FILTERED_TOOL_NAMES.
var FilteredToolNames = map[string]bool{"query_person_profile": true, "wait": true}

// JargonLearningAcquireResult mirrors JargonLearningAcquireResult.
type JargonLearningAcquireResult struct {
	Acquired    bool
	Reason      string
	ActiveCount int
	MaxCount    int
}

// JargonLearningBatchGate mirrors JargonLearningBatchGate（同样受
// max_expression_learner 约束）。
type JargonLearningBatchGate struct {
	MaxCount int

	mu     sync.Mutex
	active map[string]bool
}

// NewJargonLearningBatchGate 构造闸门。
func NewJargonLearningBatchGate(maxCount int) *JargonLearningBatchGate {
	return &JargonLearningBatchGate{MaxCount: maxCount, active: map[string]bool{}}
}

// Acquire mirrors JargonLearningBatchGate.acquire.
func (g *JargonLearningBatchGate) Acquire(sessionID string) JargonLearningAcquireResult {
	maxCount := g.MaxCount
	if maxCount <= 0 {
		return JargonLearningAcquireResult{Acquired: false, Reason: GateMaxDisabled, MaxCount: maxCount}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	activeCount := len(g.active)
	if g.active[sessionID] {
		return JargonLearningAcquireResult{Acquired: false, Reason: GateSessionBusy, ActiveCount: activeCount, MaxCount: maxCount}
	}
	if activeCount >= maxCount {
		return JargonLearningAcquireResult{Acquired: false, Reason: GateGlobalLimit, ActiveCount: activeCount, MaxCount: maxCount}
	}
	g.active[sessionID] = true
	return JargonLearningAcquireResult{Acquired: true, ActiveCount: activeCount + 1, MaxCount: maxCount}
}

// Release mirrors JargonLearningBatchGate.release.
func (g *JargonLearningBatchGate) Release(sessionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.active, sessionID)
}

// ActiveCount 返回活跃会话数。
func (g *JargonLearningBatchGate) ActiveCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.active)
}

// ---------------------------------------------------------------------------
// JargonLearner
// ---------------------------------------------------------------------------

// JargonLearnerOptions 汇总黑话配置。
type JargonLearnerOptions struct {
	// BotName mirrors bot.nickname（黑话过滤）。
	BotName string
	// Use mirrors jargon.learning_list 的 use 开关。
	Use bool
	// Learn mirrors jargon.learning_list 的 learn 开关。
	Learn bool
}

// JargonLearner mirrors JargonLearner：从真实聊天消息抽取黑话候选并交给 Miner。
type JargonLearner struct {
	SessionID string
	Options   JargonLearnerOptions
	Gate      *JargonLearningBatchGate
	Miner     *JargonMiner
	Prompts   PromptFunc
	LLM       LLMFunc
	Sessions  SessionExistsFunc
	Logf      func(format string, args ...any)
}

func (l *JargonLearner) log(format string, args ...any) {
	if l.Logf != nil {
		l.Logf(format, args...)
	}
}

// LearnFromContextMessages mirrors JargonLearner.learn_from_context_messages。
func (l *JargonLearner) LearnFromContextMessages(
	ctx context.Context,
	contextMessages []chatcontextAlias,
	isBotSelf IsBotSelfFunc,
	maxLearners int,
) (bool, error) {
	sources := LearningSourceFromContext(contextMessages, isBotSelf)
	filtered := make([]LearningMessage, 0, len(sources))
	for _, msg := range sources {
		if !IsLearnableSessionMessage(msg) {
			continue
		}
		filtered = append(filtered, msg)
	}
	if len(filtered) < MinMessagesForExtraction {
		l.log("裁切历史可学习消息不足: 可学习=%d 阈值=%d", len(filtered), MinMessagesForExtraction)
		return false, nil
	}
	return l.LearnFromMessages(ctx, filtered, maxLearners)
}

// IsLearnableSessionMessage mirrors `_is_learnable_session_message`。
func IsLearnableSessionMessage(msg LearningMessage) bool {
	if msg.IsNotify {
		return false
	}
	// 独立表情包消息不参与黑话学习（对齐 `_is_standalone_emoji_message`）。
	if msg.IsEmoji {
		return false
	}
	trimmed := strings.TrimSpace(msg.Content)
	if trimmed == "" {
		return false
	}
	return !isEmojiOnlyText(trimmed)
}

// isEmojiOnlyText mirrors EMOJI_ONLY_MESSAGE_PATTERN（[表情包N] / [[表情包：desc]]）。
func isEmojiOnlyText(text string) bool {
	normalized := strings.TrimSpace(text)
	if normalized == "" {
		return false
	}
	rest := normalized
	for strings.HasPrefix(rest, "[表情包") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return false
		}
		rest = strings.TrimSpace(rest[end+1:])
	}
	return rest == ""
}

// LearnFromMessages mirrors `_learn_from_session_messages` + `_learn_from_sources`。
func (l *JargonLearner) LearnFromMessages(
	ctx context.Context,
	messages []LearningMessage,
	maxLearners int,
) (bool, error) {
	learningSessionID := ResolveLearningSessionID(l.SessionID, messages, l.Sessions)
	if learningSessionID == "" {
		l.log("黑话学习已跳过：无法解析到有效聊天流, learner_session_id=%s", l.SessionID)
		return false, nil
	}
	if l.Gate == nil {
		l.Gate = NewJargonLearningBatchGate(maxLearners)
	}
	acquire := l.Gate.Acquire(learningSessionID)
	if !acquire.Acquired {
		l.log("黑话学习批次被闸门拒绝: reason=%s active=%d max=%d session_id=%s",
			acquire.Reason, acquire.ActiveCount, acquire.MaxCount, learningSessionID)
		return false, nil
	}
	defer l.Gate.Release(learningSessionID)
	return l.runLearningBatch(ctx, messages, learningSessionID)
}

func (l *JargonLearner) runLearningBatch(
	ctx context.Context,
	messages []LearningMessage,
	learningSessionID string,
) (bool, error) {
	if l.LLM == nil || l.Prompts == nil {
		return false, fmt.Errorf("learners: 黑话学习缺少 LLM 或 prompt 渲染器")
	}
	chatStr := BuildJargonLearningChatString(messages)
	prompt, err := l.Prompts("learn_jargon", map[string]string{"chat_str": chatStr})
	if err != nil {
		return false, err
	}
	raw, err := l.LLM(ctx, LLMRequest{
		Task:        "learner",
		RequestType: "jargon.learner",
		SessionID:   learningSessionID,
		Messages: []LLMMessage{
			{Role: "system", Content: prompt},
			{Role: "user", Content: "请根据以上聊天消息输出 JSON。"},
		},
	})
	if err != nil {
		l.log("学习黑话失败: %v", err)
		return false, nil
	}
	entries := ParseJargonResponse(raw)
	if len(entries) == 0 {
		return false, nil
	}
	if l.Miner == nil {
		return false, nil
	}
	saved, updated, err := l.Miner.ProcessExtractedEntries(ctx, entries, messages, learningSessionID)
	if err != nil {
		return false, err
	}
	l.log("成功处理 %d 个黑话条目: 新增=%d 更新=%d", len(entries), saved, updated)
	return saved+updated > 0, nil
}

// BuildJargonLearningChatString mirrors `_build_learning_source_content` 的
// source_id 编号约定。
func BuildJargonLearningChatString(messages []LearningMessage) string {
	lines := make([]string, 0, len(messages))
	for index, msg := range messages {
		speakerKind := "USER"
		if msg.IsSelf {
			speakerKind = "SELF"
		}
		speakerName := strings.TrimSpace(msg.UserName)
		if speakerName == "" {
			speakerName = "未知用户"
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			content = "[空消息]"
		}
		lines = append(lines, fmt.Sprintf("<message source_id=\"%d\" speaker=\"%s\" name=\"%s\">%s</message>",
			index+1, speakerKind, speakerName, content))
	}
	return strings.Join(lines, "\n")
}

// ParseJargonResponse mirrors expression_utils.parse_jargon_response。
func ParseJargonResponse(response string) []JargonEntry {
	raw := RepairJSONDocument(response)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var payload []map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		var single map[string]any
		if err := json.Unmarshal([]byte(raw), &single); err != nil {
			return nil
		}
		payload = []map[string]any{single}
	}
	entries := make([]JargonEntry, 0, len(payload))
	for _, item := range payload {
		content := strings.TrimSpace(normalizeJSONString(item["content"]))
		sourceID := strings.TrimSpace(normalizeJSONString(item["source_id"]))
		if content == "" || sourceID == "" {
			continue
		}
		entries = append(entries, JargonEntry{Content: content, RawContent: []string{"source_id:" + sourceID}})
	}
	return entries
}

// IsSingleCharJargon mirrors is_single_char_jargon（单字黑话不入缓存）。
func IsSingleCharJargon(content string) bool {
	runes := []rune(strings.TrimSpace(content))
	if len(runes) != 1 {
		return false
	}
	char := runes[0]
	return (char >= 0x4E00 && char <= 0x9FFF) ||
		(char >= 'a' && char <= 'z') ||
		(char >= 'A' && char <= 'Z') ||
		(char >= '0' && char <= '9')
}

// ---------------------------------------------------------------------------
// 上下文机械匹配（对齐 maisaka/jargon_context_matcher.py）
// ---------------------------------------------------------------------------

// JargonCandidate mirrors _JargonCandidate.
type JargonCandidate struct {
	Content       string
	Meaning       string
	Count         int
	IsGlobal      bool
	SessionIDDict string
}

// JargonMatch mirrors _JargonMatch.
type JargonMatch struct {
	Content            string
	Meaning            string
	Score              float64
	FirstMessageIndex  int
	JargonCount        int
	HighFrequencyTerm  string
	HighFrequencyRank  int
	HighFrequencyCount int
}

// JargonMatchOptions 汇总黑话匹配配置。
type JargonMatchOptions struct {
	// GlobalSessionIDs 是当前会话可共享黑话的会话范围（对齐
	// resolve_jargon_group_scope 的 related_session_ids）。
	GlobalSessionIDs map[string]bool
	// HasGlobalShare mirrors has_global_share（命中全平台通配时忽略作用域）。
	HasGlobalShare bool
	// Use mirrors jargon.learning_list 的 use 开关。
	Use bool
	// Limit mirrors MAX_JARGON_REFERENCE_MATCHES。
	Limit int
}

// MatchJargonsForContext mirrors match_jargons_for_context.
//
// userTexts 是「当前上下文中其他用户消息」的文本（按上下文顺序）。
func MatchJargonsForContext(
	userTexts []string,
	candidates []JargonCandidate,
	options JargonMatchOptions,
) []JargonMatch {
	if !options.Use || len(userTexts) == 0 || len(candidates) == 0 {
		return nil
	}
	limit := options.Limit
	if limit <= 0 {
		limit = MaxJargonReferenceMatches
	}
	matchesByContent := map[string]JargonMatch{}
	for messageIndex, text := range userTexts {
		normalizedText := NormalizeMatchText(text)
		if normalizedText == "" {
			continue
		}
		for _, candidate := range candidates {
			contentKey := NormalizeMatchText(candidate.Content)
			if contentKey == "" {
				continue
			}
			if _, exists := matchesByContent[contentKey]; exists {
				continue
			}
			if !strings.Contains(normalizedText, contentKey) {
				continue
			}
			matchesByContent[contentKey] = JargonMatch{
				Content:           candidate.Content,
				Meaning:           candidate.Meaning,
				Score:             CalculateJargonMatchScore(candidate.Count, messageIndex, 0, 0, false),
				FirstMessageIndex: messageIndex,
				JargonCount:       candidate.Count,
			}
		}
	}
	matches := make([]JargonMatch, 0, len(matchesByContent))
	for _, match := range matchesByContent {
		matches = append(matches, match)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		if matches[i].FirstMessageIndex != matches[j].FirstMessageIndex {
			return matches[i].FirstMessageIndex < matches[j].FirstMessageIndex
		}
		if len([]rune(matches[i].Content)) != len([]rune(matches[j].Content)) {
			return len([]rune(matches[i].Content)) > len([]rune(matches[j].Content))
		}
		return matches[i].Content < matches[j].Content
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

// NormalizeMatchText mirrors `_normalize_match_text`.
func NormalizeMatchText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(text))), " ")
}

// CalculateJargonMatchScore mirrors `_calculate_match_score`.
func CalculateJargonMatchScore(
	candidateCount int,
	firstMessageIndex int,
	highFrequencyRank int,
	highFrequencyCount int,
	hitHighFrequency bool,
) float64 {
	highFrequencyScore := 0.0
	if hitHighFrequency {
		highFrequencyScore = 1000.0 + float64(highFrequencyCount)*2.0
		if highFrequencyRank > 0 {
			rankBonus := 100.0 - float64(highFrequencyRank)
			if rankBonus > 0 {
				highFrequencyScore += rankBonus
			}
		}
	}
	return float64(candidateCount) + highFrequencyScore - float64(firstMessageIndex)*0.01
}

// FilterJargonCandidates mirrors `_load_scoped_jargon_candidates` 的作用域过滤。
func FilterJargonCandidates(
	records []JargonRecord,
	options JargonMatchOptions,
) []JargonCandidate {
	candidates := make([]JargonCandidate, 0, len(records))
	for _, record := range records {
		content := strings.TrimSpace(record.Content)
		meaning := strings.TrimSpace(record.Meaning)
		if content == "" || meaning == "" || !record.IsJargon {
			continue
		}
		if !record.IsGlobal && !options.HasGlobalShare &&
			!JargonInScope(record.SessionIDDict, options.GlobalSessionIDs) {
			continue
		}
		candidates = append(candidates, JargonCandidate{
			Content:       content,
			Meaning:       meaning,
			Count:         record.Count,
			IsGlobal:      record.IsGlobal,
			SessionIDDict: record.SessionIDDict,
		})
	}
	return candidates
}

// JargonInScope mirrors `_jargon_in_scope`.
func JargonInScope(sessionIDDict string, relatedSessionIDs map[string]bool) bool {
	if len(relatedSessionIDs) == 0 {
		return false
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(sessionIDDict), &parsed); err != nil {
		return false
	}
	for sessionID := range parsed {
		if relatedSessionIDs[sessionID] {
			return true
		}
	}
	return false
}

// ExtractOtherUserTexts mirrors `_extract_other_user_texts`：只取当前上下文中
// 其他用户（非机器人）的文本消息。
func ExtractOtherUserTexts(
	contextMessages []chatcontextAlias,
	isBotSelf IsBotSelfFunc,
) []string {
	texts := make([]string, 0, len(contextMessages))
	seen := map[string]bool{}
	for _, entry := range contextMessages {
		sessionBacked, ok := entry.(*chatcontextSessionBacked)
		if !ok {
			continue
		}
		if sessionBacked.SourceKind != "user" {
			continue
		}
		original := sessionBacked.OriginalMessage()
		if original != nil && isBotSelf != nil &&
			isBotSelf(original.Platform, original.MessageInfo.UserInfo.UserID) {
			continue
		}
		if messageID := strings.TrimSpace(sessionBacked.MsgID); messageID != "" {
			if seen[messageID] {
				continue
			}
			seen[messageID] = true
		}
		text := strings.Join(strings.Fields(sessionBacked.VisibleText), " ")
		if text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

// BuildJargonReferenceMessage mirrors build_jargon_reference_message.
//
// 返回 nil 表示本轮没有命中（不注入）。
func BuildJargonReferenceMessage(
	userTexts []string,
	candidates []JargonCandidate,
	options JargonMatchOptions,
	excludedContents map[string]bool,
	now time.Time,
) *ReferencePayload {
	matches := MatchJargonsForContext(userTexts, candidates, options)
	if len(matches) == 0 {
		return nil
	}
	excluded := map[string]bool{}
	for content := range excludedContents {
		excluded[NormalizeMatchText(content)] = true
	}
	lines := []string{JargonReferenceHeader}
	count := 0
	for _, match := range matches {
		if excluded[NormalizeMatchText(match.Content)] {
			continue
		}
		hint := ""
		if match.HighFrequencyTerm != "" {
			hint = "，同时命中高频词"
		}
		lines = append(lines, fmt.Sprintf("%d. %s：%s%s", count+1, match.Content, match.Meaning, hint))
		count++
	}
	if count == 0 {
		return nil
	}
	return &ReferencePayload{
		Content:       strings.Join(lines, "\n"),
		Kind:          "jargon",
		DisplayPrefix: JargonReferenceDisplayPrefix,
	}
}

// ExtractJargonReferenceContents mirrors extract_jargon_reference_contents。
func ExtractJargonReferenceContents(contextMessages []chatcontextAlias) map[string]bool {
	contents := map[string]bool{}
	for _, entry := range contextMessages {
		reference, ok := entry.(*chatcontextReference)
		if !ok || string(reference.ReferenceKind) != "jargon" {
			continue
		}
		for _, line := range strings.Split(reference.Content, "\n") {
			if strings.TrimSpace(line) == JargonReferenceHeader {
				continue
			}
			content := extractJargonReferenceContentFromLine(line)
			if content != "" {
				contents[content] = true
			}
		}
	}
	return contents
}

func extractJargonReferenceContentFromLine(line string) string {
	normalized := strings.TrimSpace(line)
	// 去掉 `N. ` 前缀。
	if index := strings.Index(normalized, ". "); index > 0 && isDigits(normalized[:index]) {
		normalized = strings.TrimSpace(normalized[index+2:])
	}
	separator := strings.Index(normalized, "：")
	if separator <= 0 {
		return ""
	}
	return strings.TrimSpace(normalized[:separator])
}

// IsJargonReferenceText mirrors is_jargon_reference_text。
func IsJargonReferenceText(content string) bool {
	text := strings.TrimSpace(content)
	if index := strings.Index(text, ">"); strings.HasPrefix(text, "<message") && index > 0 {
		text = strings.TrimSpace(text[index+1:])
	}
	return strings.HasPrefix(text, JargonReferenceDisplayPrefix) || strings.HasPrefix(text, JargonReferenceHeader)
}

// ---------------------------------------------------------------------------
// JargonMiner：黑话解释与入库（对齐 jargon_miner.py 的推断链路）
// ---------------------------------------------------------------------------

// JargonInferenceThresholds mirrors JARGON_INFERENCE_THRESHOLDS.
var JargonInferenceThresholds = []int{4, 8, 25, 100}

// JargonMiner 写入/更新黑话，并在阈值处执行三段式含义推断。
type JargonMiner struct {
	Store   JargonStore
	Prompts PromptFunc
	LLM     LLMFunc
	// SessionName mirrors `session_name`（日志展示）。
	SessionName string
	// CacheLimit mirrors `cache_limit`（默认 50）。
	CacheLimit int
	// BotName mirrors bot.nickname（黑话过滤）。
	BotName string
	// Inferences 记录推断次数（观测/测试用）。
	Inferences int
	Logf       func(format string, args ...any)

	mu    sync.Mutex
	cache []string
}

// NewJargonMiner 构造 miner（缓存上限默认 50）。
func NewJargonMiner(sessionName string) *JargonMiner {
	return &JargonMiner{SessionName: sessionName, CacheLimit: 50}
}

func (m *JargonMiner) log(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// AddToCache mirrors `_add_to_cache`（单字黑话不入缓存；LRU 淘汰）。
func (m *JargonMiner) AddToCache(content string) {
	trimmed := strings.TrimSpace(content)
	if IsSingleCharJargon(trimmed) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for index, cached := range m.cache {
		if cached == trimmed {
			m.cache = append(m.cache[:index], m.cache[index+1:]...)
			m.cache = append(m.cache, trimmed)
			return
		}
	}
	m.cache = append(m.cache, trimmed)
	limit := m.CacheLimit
	if limit <= 0 {
		limit = 50
	}
	if len(m.cache) > limit {
		m.cache = m.cache[len(m.cache)-limit:]
	}
}

// CachedJargons mirrors `get_cached_jargons`。
func (m *JargonMiner) CachedJargons() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.cache...)
}

// ProcessExtractedEntries mirrors `process_extracted_entries`: 合并同批条目 →
// 作用域内匹配（MANUAL 优先）→ 更新或新增 → 按阈值触发推断。
//
// 返回 (saved, updated)。
func (m *JargonMiner) ProcessExtractedEntries(
	ctx context.Context,
	entries []JargonEntry,
	messages []LearningMessage,
	sessionID string,
) (int, int, error) {
	if len(entries) == 0 || m.Store == nil {
		return 0, 0, nil
	}
	merged := map[string]*JargonEntry{}
	order := make([]string, 0, len(entries))
	for _, entry := range entries {
		content := strings.TrimSpace(entry.Content)
		if content == "" {
			continue
		}
		if existing, ok := merged[content]; ok {
			existing.RawContent = dedupeStrings(append(existing.RawContent, entry.RawContent...))
			existing.EvidenceMessages = MergeEvidenceRefs(existing.EvidenceMessages, entry.EvidenceMessages)
			continue
		}
		copied := JargonEntry{
			Content:          content,
			RawContent:       dedupeStrings(entry.RawContent),
			EvidenceMessages: NormalizeEvidenceRefs(entry.EvidenceMessages),
		}
		merged[content] = &copied
		order = append(order, content)
	}
	if len(merged) == 0 {
		return 0, 0, nil
	}

	saved, updated := 0, 0
	for _, content := range order {
		entry := merged[content]
		if err := m.ValidateEntry(*entry, messages); err != nil {
			m.log("黑话条目被过滤: content=%s reason=%v", content, err)
			continue
		}
		record, err := m.Store.GetByContent(content)
		if err != nil {
			return saved, updated, err
		}
		if record != nil {
			if record.CreatedBy == "MANUAL" {
				m.AddToCache(content)
				continue
			}
			updatedRecord := *record
			updatedRecord.Count++
			updatedRecord.UpdatedTimestamp = nowUnix()
			updatedRecord.EvidenceMessages = mustJSON(MergeEvidenceRefs(
				decodeEvidenceRefs(record.EvidenceMessages), entry.EvidenceMessages))
			updatedRecord.SessionIDDict = bumpSessionIDDict(record.SessionIDDict, sessionID)
			if err := m.Store.Update(updatedRecord); err != nil {
				return saved, updated, err
			}
			updated++
			m.AddToCache(content)
			if m.ShouldInferMeaning(updatedRecord) {
				if _, err := m.InferMeaning(ctx, updatedRecord); err != nil {
					m.log("黑话含义推断失败: content=%s err=%v", content, err)
				}
			}
			continue
		}

		newRecord := JargonRecord{
			Content:            content,
			EvidenceMessages:   mustJSON(entry.EvidenceMessages),
			SessionIDDict:      mustJSON(map[string]int{sessionID: 1}),
			Count:              1,
			IsJargon:           false,
			Meaning:            "",
			CreatedBy:          "AI",
			CreatedTimestamp:   nowUnix(),
			UpdatedTimestamp:   nowUnix(),
			LastInferenceCount: 0,
		}
		if _, err := m.Store.Insert(newRecord); err != nil {
			return saved, updated, err
		}
		saved++
		m.AddToCache(content)
	}
	return saved, updated, nil
}

// ValidateEntry 过滤无效黑话条目（对齐 `_process_jargon_entries` 的跳过分支）。
func (m *JargonMiner) ValidateEntry(entry JargonEntry, messages []LearningMessage) error {
	content := strings.TrimSpace(entry.Content)
	if content == "" {
		return fmt.Errorf("empty_content")
	}
	if strings.Contains(content, "SELF") {
		return fmt.Errorf("contains_self")
	}
	if strings.TrimSpace(m.BotNameFilter()) != "" && strings.Contains(content, m.BotNameFilter()) {
		return fmt.Errorf("contains_bot_nickname")
	}
	if len(messages) == 0 {
		return nil
	}
	_ = messages
	return nil
}

// BotNameFilter 返回机器人昵称过滤值（为空表示不过滤）。
func (m *JargonMiner) BotNameFilter() string {
	return strings.TrimSpace(m.BotName)
}

// ShouldInferMeaning mirrors `_should_infer_meaning`.
func (m *JargonMiner) ShouldInferMeaning(record JargonRecord) bool {
	if record.CreatedBy == "MANUAL" || record.IsComplete {
		return false
	}
	count := record.Count
	lastInference := record.LastInferenceCount
	if count < JargonInferenceThresholds[0] {
		return false
	}
	if count <= lastInference {
		return false
	}
	nextThreshold := 0
	found := false
	for _, threshold := range JargonInferenceThresholds {
		if threshold > lastInference {
			nextThreshold = threshold
			found = true
			break
		}
	}
	if !found {
		return false
	}
	return count >= nextThreshold
}

// InferMeaning mirrors infer_meaning：三段式推断（上下文 → 仅词条 → 比较）。
//
// 返回是否完成一次写回。
func (m *JargonMiner) InferMeaning(ctx context.Context, record JargonRecord) (bool, error) {
	if record.CreatedBy == "MANUAL" {
		return false, nil
	}
	if m.LLM == nil || m.Prompts == nil || m.Store == nil {
		return false, nil
	}
	evidence := decodeEvidenceRefs(record.EvidenceMessages)
	if len(evidence) == 0 {
		return false, nil
	}
	rawContentText := formatEvidenceContextSegments(evidence)

	previousMeaning := strings.TrimSpace(record.Meaning)
	previousSection := ""
	previousInstruction := ""
	if previousMeaning != "" {
		previousSection = "\n**上一次推断的含义（仅供参考）**\n" + previousMeaning
		previousInstruction = "- 请参考上一次推断的含义，结合新的上下文信息，给出更准确或更新的推断结果"
	}
	prompt1, err := m.Prompts("jargon_inference_with_context", map[string]string{
		"bot_name":                     m.BotNameFilter(),
		"content":                      record.Content,
		"raw_content_list":             rawContentText,
		"previous_meaning_section":     previousSection,
		"previous_meaning_instruction": previousInstruction,
	})
	if err != nil {
		return false, err
	}
	response1, err := m.LLM(ctx, LLMRequest{
		Task: "learner", RequestType: "jargon.inference",
		Messages: []LLMMessage{{Role: "user", Content: prompt1}},
	})
	if err != nil {
		return false, err
	}
	inference1 := parseJargonInference(response1)
	if inference1 == nil {
		return false, nil
	}
	if noInfo, _ := inference1["no_info"].(bool); noInfo {
		updated := record
		updated.LastInferenceCount = record.Count
		updated.UpdatedTimestamp = nowUnix()
		if err := m.Store.Update(updated); err != nil {
			return false, err
		}
		return false, nil
	}
	meaning1 := strings.TrimSpace(normalizeJSONString(inference1["meaning"]))
	if meaning1 == "" {
		return false, nil
	}

	prompt2, err := m.Prompts("jargon_inference_content_only", map[string]string{"content": record.Content})
	if err != nil {
		return false, err
	}
	response2, err := m.LLM(ctx, LLMRequest{
		Task: "learner", RequestType: "jargon.inference",
		Messages: []LLMMessage{{Role: "user", Content: prompt2}},
	})
	if err != nil {
		return false, err
	}
	inference2 := parseJargonInference(response2)
	if inference2 == nil {
		return false, nil
	}

	prompt3, err := m.Prompts("jargon_compare_inference", map[string]string{
		"inference1": mustJSON(inference1),
		"inference2": mustJSON(inference2),
	})
	if err != nil {
		return false, err
	}
	response3, err := m.LLM(ctx, LLMRequest{
		Task: "learner", RequestType: "jargon.inference",
		Messages: []LLMMessage{{Role: "user", Content: prompt3}},
	})
	if err != nil {
		return false, err
	}
	comparison := parseJargonInference(response3)
	if comparison == nil {
		return false, nil
	}
	isSimilar, _ := comparison["is_similar"].(bool)
	isJargon := !isSimilar

	inferredMeaning := strings.TrimSpace(normalizeJSONString(inference1["meaning"]))
	finalizedMeaning := inferredMeaning
	if !isJargon {
		finalizedMeaning = previousMeaning
	}
	updated := record
	updated.IsJargon = isJargon
	updated.Meaning = finalizedMeaning
	updated.LastInferenceCount = record.Count
	updated.IsComplete = record.Count >= 100
	updated.UpdatedTimestamp = nowUnix()
	if err := m.Store.Update(updated); err != nil {
		return false, err
	}
	m.mu.Lock()
	m.Inferences++
	m.mu.Unlock()
	if isJargon {
		m.log("[黑话]%s的含义是 %s", record.Content, finalizedMeaning)
	} else {
		m.log("[%s]%s 不是黑话", m.SessionName, record.Content)
	}
	return true, nil
}

func parseJargonInference(response string) map[string]any {
	raw := RepairJSONDocument(response)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil
	}
	return payload
}

// formatEvidenceContextSegments mirrors `_format_evidence_context_segments`。
func formatEvidenceContextSegments(refs []JargonEvidenceRef) string {
	if len(refs) == 0 {
		return ""
	}
	lines := make([]string, 0, len(refs))
	for index, ref := range refs {
		lines = append(lines, fmt.Sprintf("- [%d] (%s) %s", index+1, ref.Platform, ref.MessageID))
	}
	return strings.Join([]string{
		"【对话片段 1】",
		strings.Join(lines, "\n"),
	}, "\n")
}

// NormalizeEvidenceRefs mirrors `_normalize_evidence_message_groups`.
func NormalizeEvidenceRefs(refs []JargonEvidenceRef) []JargonEvidenceRef {
	out := make([]JargonEvidenceRef, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		platform := strings.TrimSpace(ref.Platform)
		messageID := strings.TrimSpace(ref.MessageID)
		if platform == "" || messageID == "" {
			continue
		}
		key := platform + "\x00" + messageID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, JargonEvidenceRef{Platform: platform, MessageID: messageID})
	}
	return out
}

// MergeEvidenceRefs mirrors `_merge_evidence_message_groups`（顺序去重合并）。
func MergeEvidenceRefs(current []JargonEvidenceRef, added []JargonEvidenceRef) []JargonEvidenceRef {
	merged := NormalizeEvidenceRefs(current)
	seen := map[string]bool{}
	for _, ref := range merged {
		seen[ref.Platform+"\x00"+ref.MessageID] = true
	}
	for _, ref := range NormalizeEvidenceRefs(added) {
		key := ref.Platform + "\x00" + ref.MessageID
		if seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, ref)
	}
	return merged
}

func decodeEvidenceRefs(raw string) []JargonEvidenceRef {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var refs []JargonEvidenceRef
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return nil
	}
	return NormalizeEvidenceRefs(refs)
}

// bumpSessionIDDict mirrors `_update_jargon` 的 session_id_dict 计数递增。
func bumpSessionIDDict(raw string, sessionID string) string {
	counts := map[string]int{}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &counts); err != nil {
			counts = map[string]int{}
		}
	}
	if strings.TrimSpace(sessionID) == "" {
		return mustJSON(counts)
	}
	counts[sessionID] = counts[sessionID] + 1
	return mustJSON(counts)
}

func dedupeStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

// ---------------------------------------------------------------------------
// jargon_explainer：search_jargon
// ---------------------------------------------------------------------------

// SearchJargon mirrors jargon_explainer.search_jargon：关键词模糊搜索（大小写
// 不敏感）+ 作用域过滤 + 只返回有含义的黑话。
func SearchJargon(
	records []JargonRecord,
	keyword string,
	options JargonMatchOptions,
	caseSensitive bool,
	fuzzy bool,
	limit int,
) []JargonCandidate {
	cleanKeyword := strings.TrimSpace(keyword)
	if cleanKeyword == "" {
		return nil
	}
	if limit < 1 {
		limit = 10
	}
	results := make([]JargonCandidate, 0, limit)
	for _, record := range records {
		if !record.IsJargon || strings.TrimSpace(record.Meaning) == "" {
			continue
		}
		content := record.Content
		matched := false
		if caseSensitive {
			if fuzzy {
				matched = strings.Contains(content, cleanKeyword)
			} else {
				matched = content == cleanKeyword
			}
		} else {
			if fuzzy {
				matched = strings.Contains(strings.ToLower(content), strings.ToLower(cleanKeyword))
			} else {
				matched = strings.EqualFold(content, cleanKeyword)
			}
		}
		if !matched {
			continue
		}
		if !record.IsGlobal && !options.HasGlobalShare && !JargonInScope(record.SessionIDDict, options.GlobalSessionIDs) {
			continue
		}
		results = append(results, JargonCandidate{
			Content:       record.Content,
			Meaning:       record.Meaning,
			Count:         record.Count,
			IsGlobal:      record.IsGlobal,
			SessionIDDict: record.SessionIDDict,
		})
		if len(results) >= limit {
			break
		}
	}
	return results
}
