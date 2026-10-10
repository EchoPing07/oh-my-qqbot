// Package memory 实现 MaiBot 记忆上层行为：聊天回想（mid_term）、启发式记忆拉起、
// 人物画像注入、记忆自动写回三个 worker 与 MemoryService 门面。
//
// 行为对照（以 Python 源码为准）：
//   - 参考/MaiBot/src/maisaka/memory/mid_term.py            → midterm.go
//   - 参考/MaiBot/src/services/memory_flow_service.py      → flow.go
//   - 参考/MaiBot/src/maisaka/memory/heuristic_injector.py → heuristic.go
//   - 参考/MaiBot/src/maisaka/memory/person_profile.py     → profile.go
//   - 参考/MaiBot/src/services/memory_service.py           → service.go
//
// 依赖约定：本包不 import internal/config、internal/llm 的具体客户端、internal/qq。
// LLM 调用、embedding、prompt 渲染与内核访问一律通过包内最小接口注入。
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
)

// 聊天回想常数照抄 mid_term.py。
const (
	// MaxSummaryInputChars mirrors MAX_SUMMARY_INPUT_CHARS.
	MaxSummaryInputChars = 16000
	// MidTermMemoryRecallContextMessageLimit mirrors
	// MID_TERM_MEMORY_RECALL_CONTEXT_MESSAGE_LIMIT.
	MidTermMemoryRecallContextMessageLimit = 12
	// MidTermMemoryRecallContextTextLimit mirrors
	// MID_TERM_MEMORY_RECALL_CONTEXT_TEXT_LIMIT.
	MidTermMemoryRecallContextTextLimit = 2400
	// MidTermMemoryRecallSummaryTextLimit mirrors
	// MID_TERM_MEMORY_RECALL_SUMMARY_TEXT_LIMIT.
	MidTermMemoryRecallSummaryTextLimit = 1400
	// MidTermMemoryDefaultRecallThreshold mirrors
	// MID_TERM_MEMORY_DEFAULT_RECALL_THRESHOLD（严格大于阈值才召回）.
	MidTermMemoryDefaultRecallThreshold = 0.8
	// MidTermMemoryRecallQueryMessageTextLimit 对齐 `if len(text) > 360: text = text[-360:]`.
	MidTermMemoryRecallQueryMessageTextLimit = 360
	// MidTermMemorySummaryPromptName 是压缩用 prompt 名（prompts/zh-CN/*.prompt）。
	MidTermMemorySummaryPromptName = "mid_term_memory_summary"
	// MidTermMemoryRecallReferencePrefix mirrors `_format_mid_term_memory_reference` 的固定行。
	MidTermMemoryRecallReferencePrefix = "以下是根据当前上下文匹配到的一条聊天回想，只作为内部参考；仅在自然相关时使用，不要生硬复述。"
	// MidTermMemoryReferenceDisplayPrefix mirrors display_prefix="[参考消息]".
	MidTermMemoryReferenceDisplayPrefix = "[参考消息]"
)

// MidTermMemoryRecallCandidate mirrors MidTermMemoryRecallCandidate.
type MidTermMemoryRecallCandidate struct {
	Message     *chatcontext.ComplexSessionMessage
	Payload     chatcontext.MidTermMemorySummaryPayload
	SegmentText string
	Score       float64
	Embedding   []float32
}

// ---------------------------------------------------------------------------
// 压缩：MidTermSummarizer
// ---------------------------------------------------------------------------

// MidTermSummarizer 把被裁切的上下文消息压缩为一条聊天回想 payload。
//
// 它同时实现 runtime.MidTermMemorySummarizer（P4b 注入点）：
//
//	SummarizeMidTermMemory(ctx, removed) (*chatcontext.MidTermMemorySummaryPayload, bool, error)
type MidTermSummarizer struct {
	// LLM 是压缩模型（Python task_name="mid_memory"）。
	LLM LLMFunc
	// Prompts 渲染 prompts/zh-CN/mid_term_memory_summary.prompt。
	Prompts PromptFunc
	// Embedder 为 recall_cues 生成 embedding（nil 时只保留文本线索）。
	Embedder EmbedFunc
	// Kernel 收到摘要文本后写入长期记忆（§6.9：经 ingest_summary 写入内核）。
	Kernel SummaryIngester
	// SessionID / SessionIDFn 提供会话 id（runtime 的注入接口不带 session）。
	SessionID   string
	SessionIDFn func() string
	// BotNickname 用于 `_resolve_participant_name` 的 assistant 分支（默认「麦麦」）。
	BotNickname string
	// IngestToKernel=false 时跳过内核写入（测试/审计）。
	IngestToKernel bool
	Logf           func(format string, args ...any)
}


func (s *MidTermSummarizer) log(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *MidTermSummarizer) sessionID() string {
	if s.SessionIDFn != nil {
		return strings.TrimSpace(s.SessionIDFn())
	}
	return strings.TrimSpace(s.SessionID)
}

// SummarizeMidTermMemory mirrors build_mid_term_memory_message.
//
// ok=false 对应 Python 返回 None（没有可摘要文本 / 解析失败 / 输入为空）。
func (s *MidTermSummarizer) SummarizeMidTermMemory(
	ctx context.Context,
	removedMessages []chatcontext.LLMContextMessage,
) (*chatcontext.MidTermMemorySummaryPayload, bool, error) {
	sourceMessages := SelectSummarySourceMessages(removedMessages)
	if len(sourceMessages) == 0 {
		s.log("聊天回想跳过: 裁切消息中没有可摘要文本")
		return nil, false, nil
	}

	timeRange := BuildTimeRange(sourceMessages)
	participants := CollectParticipants(sourceMessages, s.botNickname())
	if s.Prompts == nil {
		return nil, false, fmt.Errorf("memory: 聊天回想缺少 prompt 渲染器")
	}
	instruction, err := s.Prompts(MidTermMemorySummaryPromptName, map[string]string{
		"time_range":        timeRange,
		"participants_text": joinParticipants(participants),
	})
	if err != nil {
		return nil, false, fmt.Errorf("memory: 渲染聊天回想 prompt 失败: %w", err)
	}

	promptMessages := BuildSummaryPromptMessages(sourceMessages, instruction)
	if len(promptMessages) <= 1 {
		s.log("聊天回想跳过: 摘要输入消息为空")
		return nil, false, nil
	}
	if s.LLM == nil {
		return nil, false, fmt.Errorf("memory: 聊天回想缺少 LLM 实现")
	}

	raw, err := s.LLM(ctx, LLMRequest{
		Task:         "mid_memory",
		RequestType:  "maisaka.mid_term_memory",
		SessionID:    s.sessionID(),
		SystemPrompt: instruction,
		Messages:     summaryPromptMessagesToLLM(promptMessages[1:]),
	})
	if err != nil {
		return nil, false, fmt.Errorf("memory: 聊天回想模型调用失败: %w", err)
	}

	payload, ok := ParseMidTermMemorySummaryResponse(raw, timeRange, participants)
	if !ok {
		s.log("聊天回想解析失败，已跳过本次插入: response=%s", truncateText(raw, 300))
		return nil, false, nil
	}

	cues := normalizeRecallCues(payload.RecallCues)
	payload.RecallCues = cues
	if s.Embedder != nil && len(cues) > 0 {
		texts := make([]string, 0, len(cues))
		for _, cue := range cues {
			texts = append(texts, stringValue(cue["text"]))
		}
		if embeddings, embedErr := s.Embedder(ctx, texts); embedErr == nil && len(embeddings) == len(cues) {
			for index, cue := range cues {
				cue["embedding"] = append([]float32{}, embeddings[index]...)
			}
		} else if embedErr != nil {
			s.log("聊天回想线索 embedding 失败，已降级为纯文本线索: %v", embedErr)
		}
	}

	if s.IngestToKernel && s.Kernel != nil {
		chatID := s.sessionID()
		if chatID != "" {
			ts := ResolveSummaryTimestamp(sourceMessages)
			if err := s.Kernel.IngestSummary(ctx, chatID, payload.Summary, ts); err != nil {
				// 内核写入失败不影响本轮回想插入（对齐 Python：长期记忆写入是可降级副作用）。
				s.log("聊天回想写入内核失败，已跳过: %v", err)
			}
		}
	}
	s.log("聊天回想生成内容: 时间范围=%s 参与人物=%s 召回线索=%d 条",
		timeRange, joinParticipants(participants), len(cues))
	return &payload, true, nil
}

// summaryPromptMessagesToLLM 把摘要输入消息转换为 LLM 边界消息（user 角色）。
func summaryPromptMessagesToLLM(messages []string) []LLMMessage {
	out := make([]LLMMessage, 0, len(messages))
	for _, text := range messages {
		out = append(out, LLMMessage{Role: "user", Content: text})
	}
	return out
}

func (s *MidTermSummarizer) botNickname() string {
	if strings.TrimSpace(s.BotNickname) != "" {
		return strings.TrimSpace(s.BotNickname)
	}
	return "麦麦"
}

// SelectSummarySourceMessages mirrors _select_summary_source_messages.
func SelectSummarySourceMessages(messages []chatcontext.LLMContextMessage) []chatcontext.LLMContextMessage {
	out := make([]chatcontext.LLMContextMessage, 0, len(messages))
	for _, entry := range messages {
		if entry == nil || entry.Role() != "user" {
			continue
		}
		if chatcontext.IsMidTermMemoryMessage(entry) {
			continue
		}
		if strings.TrimSpace(entry.ProcessedPlainText()) == "" {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// BuildTimeRange mirrors _build_time_range.
func BuildTimeRange(messages []chatcontext.LLMContextMessage) string {
	timestamps := make([]time.Time, 0, len(messages))
	for _, entry := range messages {
		if entry == nil {
			continue
		}
		timestamps = append(timestamps, entry.Timestamp())
	}
	if len(timestamps) == 0 {
		return "未知"
	}
	start := timestamps[0]
	end := timestamps[0]
	for _, ts := range timestamps {
		if ts.Before(start) {
			start = ts
		}
		if ts.After(end) {
			end = ts
		}
	}
	return start.Format("2006-01-02 15:04:05") + " ~ " + end.Format("2006-01-02 15:04:05")
}

// CollectParticipants mirrors _collect_participants.
func CollectParticipants(messages []chatcontext.LLMContextMessage, botNickname string) []string {
	participants := make([]string, 0, 4)
	seen := map[string]bool{}
	for _, entry := range messages {
		name := ResolveParticipantName(entry, botNickname)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		participants = append(participants, name)
	}
	return participants
}

// ResolveParticipantName mirrors _resolve_participant_name.
func ResolveParticipantName(entry chatcontext.LLMContextMessage, botNickname string) string {
	if entry == nil {
		return ""
	}
	if original := entry.OriginalMessage(); original != nil {
		if name := strings.TrimSpace(original.MessageInfo.UserInfo.DisplayName()); name != "" {
			return name
		}
	}
	if entry.Role() == "assistant" {
		if strings.TrimSpace(botNickname) != "" {
			return strings.TrimSpace(botNickname)
		}
		return "麦麦"
	}
	if complexMessage, ok := entry.(*chatcontext.ComplexSessionMessage); ok &&
		complexMessage.SourceKind == chatcontext.SourceOptimizedTools {
		return "历史工具调用"
	}
	return strings.TrimSpace(entry.Source())
}

// BuildSummaryPromptMessages mirrors _build_summary_prompt_messages：第一条是
// instruction，其后是逐条源消息（受 MAX_SUMMARY_INPUT_CHARS 约束）。
func BuildSummaryPromptMessages(
	sourceMessages []chatcontext.LLMContextMessage,
	instructionPrompt string,
) []string {
	promptMessages := []string{instructionPrompt}
	totalChars := 0
	for index, entry := range sourceMessages {
		text := strings.TrimSpace(entry.ProcessedPlainText())
		if text == "" {
			continue
		}
		rendered := fmt.Sprintf("[%d][%s] %s", index+1, entry.Role(), text)
		remaining := MaxSummaryInputChars - totalChars
		if remaining <= 0 {
			return promptMessages
		}
		if len([]rune(rendered)) > remaining {
			promptMessages = append(promptMessages, string([]rune(rendered)[:remaining]))
			return promptMessages
		}
		promptMessages = append(promptMessages, rendered)
		totalChars += len([]rune(rendered))
	}
	return promptMessages
}

// ParseMidTermMemorySummaryResponse mirrors _parse_summary_response.
func ParseMidTermMemorySummaryResponse(
	response string,
	timeRange string,
	participants []string,
) (chatcontext.MidTermMemorySummaryPayload, bool) {
	payload := loadJSONObjectPayload(response)
	if payload == nil {
		return chatcontext.MidTermMemorySummaryPayload{}, false
	}
	summary := stringValue(firstNonEmpty(payload["summary"], payload["long_summary"], payload["brief"]))
	if strings.TrimSpace(summary) == "" {
		return chatcontext.MidTermMemorySummaryPayload{}, false
	}
	rawCues := payload["recall_cues"]
	if rawCues == nil {
		rawCues = payload["match_segments"]
	}
	cues := make([]map[string]any, 0, 5)
	for _, cue := range normalizeRecallCueStrings(rawCues) {
		cues = append(cues, map[string]any{"text": cue})
	}
	return chatcontext.MidTermMemorySummaryPayload{
		TimeRange:    timeRange,
		Participants: participants,
		Summary:      strings.TrimSpace(summary),
		RecallCues:   cues,
	}, true
}

// normalizeRecallCueStrings mirrors _normalize_recall_cues（含 dict 形态的
// text/associations/reason 拼接与去重，最多 5 条）。
func normalizeRecallCueStrings(value any) []string {
	var raw []any
	switch typed := value.(type) {
	case string:
		for _, line := range strings.Split(typed, "\n") {
			if strings.TrimSpace(line) != "" {
				raw = append(raw, line)
			}
		}
	case []any:
		raw = typed
	case []string:
		for _, item := range typed {
			raw = append(raw, item)
		}
	}
	cues := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		cue := normalizeRecallCueText(item)
		if cue == "" || seen[cue] {
			continue
		}
		seen[cue] = true
		cues = append(cues, cue)
		if len(cues) >= 5 {
			break
		}
	}
	return cues
}

func normalizeRecallCueText(value any) string {
	if payload, ok := value.(map[string]any); ok {
		text := stringValue(firstNonEmpty(payload["text"], payload["query"], payload["content"]))
		parts := []string{strings.TrimSpace(text)}
		associations := coerceStringList(payload["associations"])
		if len(associations) > 0 {
			parts = append(parts, "关联: "+strings.Join(associations, "、"))
		}
		if reason := strings.TrimSpace(stringValue(payload["reason"])); reason != "" {
			parts = append(parts, "原因: "+reason)
		}
		return strings.TrimSpace(strings.Join(dedupeBlank(parts), "；"))
	}
	return strings.Join(strings.Fields(stringValue(value)), " ")
}

// normalizeRecallCues 把 payload 里的 cue map 统一为 {text, embedding?, model_name?}。
func normalizeRecallCues(cues []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(cues))
	for _, cue := range cues {
		if cue == nil {
			continue
		}
		text := normalizeRecallCueText(cue)
		if text == "" {
			continue
		}
		normalized := map[string]any{"text": text}
		if embedding := coerceEmbedding(cue["embedding"]); len(embedding) > 0 {
			normalized["embedding"] = embedding
		}
		if modelName := strings.TrimSpace(stringValue(cue["model_name"])); modelName != "" {
			normalized["model_name"] = modelName
		}
		out = append(out, normalized)
	}
	return out
}

// ---------------------------------------------------------------------------
// 召回：RecallBuilder
// ---------------------------------------------------------------------------

// RecallBuilder 基于当前 Planner 上下文召回最相关的一条聊天回想。
// 对齐 build_mid_term_memory_reference_message。
type RecallBuilder struct {
	// Embedder 生成 query embedding（必需；nil 时返回未命中）。
	Embedder EmbedFunc
	// Threshold 是召回阈值（默认 0.8，严格大于才命中）。
	Threshold float64
	Now       func() time.Time
	Logf      func(format string, args ...any)
}

func (r *RecallBuilder) threshold() float64 {
	if r.Threshold > 0 {
		return r.Threshold
	}
	return MidTermMemoryDefaultRecallThreshold
}

func (r *RecallBuilder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *RecallBuilder) log(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// BuildReferenceMessage mirrors build_mid_term_memory_reference_message.
//
// ok=false 表示未命中、无候选或已全部注入过。
func (r *RecallBuilder) BuildReferenceMessage(
	ctx context.Context,
	history []chatcontext.LLMContextMessage,
	selectedHistory []chatcontext.LLMContextMessage,
) (*chatcontext.ReferenceMessage, bool, error) {
	queryText := BuildRecallQueryText(selectedHistory)
	if queryText == "" {
		return nil, false, nil
	}

	candidates := CollectRecallCandidates(history)
	if len(candidates) == 0 {
		return nil, false, nil
	}
	recalledKeys, recalledSegments := CollectRecalledReferenceIdentities(selectedHistory)
	candidates = filterRecalledCandidates(candidates, recalledKeys, recalledSegments)
	if len(candidates) == 0 {
		if len(recalledKeys) > 0 || len(recalledSegments) > 0 {
			r.log("当前上下文已包含全部匹配的聊天回想参考，跳过重复召回")
		}
		return nil, false, nil
	}
	if r.Embedder == nil {
		return nil, false, fmt.Errorf("memory: 聊天回想召回缺少 embedding 实现")
	}

	embeddings, err := r.Embedder(ctx, []string{queryText})
	if err != nil {
		return nil, false, fmt.Errorf("memory: 聊天回想召回 embedding 失败: %w", err)
	}
	if len(embeddings) == 0 {
		return nil, false, nil
	}
	best := SelectBestRecallCandidate(candidates, embeddings[0], r.threshold())
	if best == nil {
		r.log("聊天回想召回未命中阈值")
		return nil, false, nil
	}
	r.log("聊天回想召回命中: msg_id=%s score=%.4f segment=%s",
		best.Message.MsgID, best.Score, truncateText(best.SegmentText, 120))
	reference := chatcontext.NewReferenceMessage(
		FormatRecallReference(*best),
		r.now(),
		chatcontext.ReferenceMemory,
		nil,
		MidTermMemoryReferenceDisplayPrefix,
	)
	return reference, true, nil
}

// InjectRecallReference 把命中参考追加到 history 副本（对齐
// `_refresh_mid_term_memory_reference_message` 的 append 语义）。
func (r *RecallBuilder) InjectRecallReference(
	ctx context.Context,
	history []chatcontext.LLMContextMessage,
	selectedHistory []chatcontext.LLMContextMessage,
) ([]chatcontext.LLMContextMessage, *chatcontext.ReferenceMessage, error) {
	reference, ok, err := r.BuildReferenceMessage(ctx, history, selectedHistory)
	if err != nil || !ok || reference == nil {
		return history, nil, err
	}
	updated := make([]chatcontext.LLMContextMessage, 0, len(history)+1)
	updated = append(updated, history...)
	updated = append(updated, reference)
	return updated, reference, nil
}

// BuildRecallQueryText mirrors _build_mid_term_memory_recall_query_text.
func BuildRecallQueryText(selectedHistory []chatcontext.LLMContextMessage) string {
	items := make([]string, 0, len(selectedHistory))
	for _, entry := range selectedHistory {
		if entry == nil {
			continue
		}
		if chatcontext.IsMidTermMemoryMessage(entry) || chatcontext.IsMidTermMemoryReferenceMessage(entry) {
			continue
		}
		if _, isReference := entry.(*chatcontext.ReferenceMessage); isReference {
			continue
		}
		text := strings.Join(strings.Fields(entry.ProcessedPlainText()), " ")
		if text == "" {
			continue
		}
		runes := []rune(text)
		if len(runes) > MidTermMemoryRecallQueryMessageTextLimit {
			text = strings.TrimSpace(string(runes[len(runes)-MidTermMemoryRecallQueryMessageTextLimit:]))
		}
		items = append(items, text)
	}
	if len(items) > MidTermMemoryRecallContextMessageLimit {
		items = items[len(items)-MidTermMemoryRecallContextMessageLimit:]
	}
	queryText := strings.Join(items, "\n")
	queryRunes := []rune(queryText)
	if len(queryRunes) <= MidTermMemoryRecallContextTextLimit {
		return queryText
	}
	return string(queryRunes[len(queryRunes)-MidTermMemoryRecallContextTextLimit:])
}

// CollectRecallCandidates mirrors _collect_mid_term_memory_recall_candidates.
func CollectRecallCandidates(history []chatcontext.LLMContextMessage) []MidTermMemoryRecallCandidate {
	candidates := make([]MidTermMemoryRecallCandidate, 0, 4)
	for _, entry := range history {
		complexMessage, ok := entry.(*chatcontext.ComplexSessionMessage)
		if !ok || !chatcontext.IsMidTermMemoryMessage(entry) {
			continue
		}
		payload := chatcontext.GetMidTermMemoryPayload(entry)
		for _, cue := range payload.RecallCues {
			text := strings.TrimSpace(stringValue(cue["text"]))
			embedding := coerceEmbedding(cue["embedding"])
			if text == "" || len(embedding) == 0 {
				continue
			}
			candidates = append(candidates, MidTermMemoryRecallCandidate{
				Message:     complexMessage,
				Payload:     payload,
				SegmentText: text,
				Embedding:   embedding,
			})
		}
	}
	return candidates
}

// CollectRecalledReferenceIdentities mirrors
// _collect_recalled_mid_term_memory_reference_identities.
func CollectRecalledReferenceIdentities(
	messages []chatcontext.LLMContextMessage,
) (map[string]bool, map[string]bool) {
	recalledKeys := map[string]bool{}
	recalledSegments := map[string]bool{}
	for _, entry := range messages {
		if entry == nil || !chatcontext.IsMidTermMemoryReferenceMessage(entry) {
			continue
		}
		content := entry.ProcessedPlainText()
		messageID := extractLabeledReferenceValue(content, "摘要ID")
		segmentText := extractLabeledReferenceValue(content, "匹配段")
		normalizedSegment := normalizeReferenceIdentityText(segmentText)
		if normalizedSegment == "" {
			continue
		}
		recalledSegments[normalizedSegment] = true
		normalizedMessageID := normalizeReferenceIdentityText(messageID)
		if normalizedMessageID != "" {
			recalledKeys[normalizedMessageID+"\x00"+normalizedSegment] = true
		}
	}
	return recalledKeys, recalledSegments
}

func filterRecalledCandidates(
	candidates []MidTermMemoryRecallCandidate,
	recalledKeys map[string]bool,
	recalledSegments map[string]bool,
) []MidTermMemoryRecallCandidate {
	out := make([]MidTermMemoryRecallCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		normalizedSegment := normalizeReferenceIdentityText(candidate.SegmentText)
		if normalizedSegment == "" {
			out = append(out, candidate)
			continue
		}
		messageID := normalizeReferenceIdentityText(candidate.Message.MsgID)
		if messageID != "" && recalledKeys[messageID+"\x00"+normalizedSegment] {
			continue
		}
		if recalledSegments[normalizedSegment] {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func extractLabeledReferenceValue(content string, label string) string {
	prefix := label + ":"
	for _, line := range strings.Split(content, "\n") {
		normalizedLine := strings.TrimSpace(line)
		if strings.HasPrefix(normalizedLine, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(normalizedLine, prefix))
		}
	}
	return ""
}

func normalizeReferenceIdentityText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// SelectBestRecallCandidate mirrors _select_best_recall_candidate.
func SelectBestRecallCandidate(
	candidates []MidTermMemoryRecallCandidate,
	queryEmbedding []float32,
	threshold float64,
) *MidTermMemoryRecallCandidate {
	var best *MidTermMemoryRecallCandidate
	for index := range candidates {
		candidate := candidates[index]
		if len(candidate.Embedding) == 0 {
			continue
		}
		score := CosineSimilarityFloat32(queryEmbedding, candidate.Embedding)
		scored := candidate
		scored.Score = score
		if best == nil || scored.Score > best.Score {
			bestCandidate := scored
			best = &bestCandidate
		}
	}
	if best == nil || best.Score <= threshold {
		return nil
	}
	return best
}

// FormatRecallReference mirrors _format_mid_term_memory_reference.
func FormatRecallReference(candidate MidTermMemoryRecallCandidate) string {
	payload := candidate.Payload
	messageID := strings.TrimSpace(candidate.Message.MsgID)
	timeRange := strings.TrimSpace(payload.TimeRange)
	if timeRange == "" {
		timeRange = "未知"
	}
	summary := strings.TrimSpace(payload.Summary)
	if summary == "" {
		summary = "无"
	}
	if len([]rune(summary)) > MidTermMemoryRecallSummaryTextLimit {
		summary = strings.TrimRight(string([]rune(summary)[:MidTermMemoryRecallSummaryTextLimit]), " ") + "..."
	}
	lines := []string{
		chatcontext.MidTermMemoryReferenceMarker,
		MidTermMemoryRecallReferencePrefix,
	}
	if messageID != "" {
		lines = append(lines, "摘要ID: "+messageID)
	}
	lines = append(lines,
		fmt.Sprintf("匹配分数: %.4f", candidate.Score),
		"匹配段: "+candidate.SegmentText,
		"时间范围: "+timeRange,
		"参与人物: "+joinParticipants(payload.Participants),
		"",
		"summary:",
		summary,
	)
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// BuildMidTermMemoryFullText mirrors build_mid_term_memory_full_text.
func BuildMidTermMemoryFullText(payload chatcontext.MidTermMemorySummaryPayload) string {
	timeRange := strings.TrimSpace(payload.TimeRange)
	if timeRange == "" {
		timeRange = "未知"
	}
	summary := strings.TrimSpace(payload.Summary)
	if summary == "" {
		summary = "无"
	}
	lines := []string{
		"【聊天回想】",
		"时间范围: " + timeRange,
		"参与人物: " + joinParticipants(payload.Participants),
		"",
		"summary:",
		summary,
	}
	cues := make([]string, 0, len(payload.RecallCues))
	for _, cue := range payload.RecallCues {
		if text := strings.TrimSpace(stringValue(cue["text"])); text != "" {
			cues = append(cues, text)
		}
	}
	if len(cues) > 0 {
		lines = append(lines, "", "召回线索:")
		for _, cue := range cues {
			lines = append(lines, "- "+cue)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ResolveSummaryTimestamp mirrors _resolve_summary_timestamp.
func ResolveSummaryTimestamp(messages []chatcontext.LLMContextMessage) time.Time {
	var latest time.Time
	for _, entry := range messages {
		if entry == nil {
			continue
		}
		if ts := entry.Timestamp(); ts.After(latest) {
			latest = ts
		}
	}
	if latest.IsZero() {
		return time.Now()
	}
	return latest
}

// ---------------------------------------------------------------------------
// 数值与 JSON 工具
// ---------------------------------------------------------------------------

// CosineSimilarityFloat32 mirrors _cosine_similarity（维度不一致时报错语义以
// 0 分处理，避免召回路径因单条脏数据崩溃）。
func CosineSimilarityFloat32(left, right []float32) float64 {
	if len(left) == 0 || len(right) == 0 || len(left) != len(right) {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		l := float64(left[index])
		r := float64(right[index])
		dot += l * r
		leftNorm += l * l
		rightNorm += r * r
	}
	if leftNorm <= 0 || rightNorm <= 0 {
		return 0
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}

func coerceEmbedding(value any) []float32 {
	switch typed := value.(type) {
	case []float32:
		if len(typed) == 0 {
			return nil
		}
		return append([]float32{}, typed...)
	case []float64:
		out := make([]float32, len(typed))
		for index, item := range typed {
			out[index] = float32(item)
		}
		return out
	case []any:
		out := make([]float32, 0, len(typed))
		for _, item := range typed {
			number, ok := toFloat64(item)
			if !ok {
				return nil
			}
			out = append(out, float32(number))
		}
		return out
	case json.RawMessage:
		var decoded []float64
		if err := json.Unmarshal(typed, &decoded); err != nil {
			return nil
		}
		out := make([]float32, len(decoded))
		for index, item := range decoded {
			out[index] = float32(item)
		}
		return out
	}
	return nil
}

func toFloat64(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	}
	return 0, false
}

// loadJSONObjectPayload mirrors _load_json_payload（fence 剥离 + 首个 {...} + 宽容修复）。
func loadJSONObjectPayload(response string) map[string]any {
	normalized := strings.TrimSpace(response)
	if normalized == "" {
		return nil
	}
	candidates := []string{normalized}
	if start := strings.Index(normalized, "```"); start >= 0 {
		rest := normalized[start+3:]
		rest = strings.TrimPrefix(rest, "json")
		rest = strings.TrimPrefix(rest, "JSON")
		if end := strings.Index(rest, "```"); end >= 0 {
			candidates = append(candidates, strings.TrimSpace(rest[:end]))
		}
	}
	objectStart := strings.Index(normalized, "{")
	objectEnd := strings.LastIndex(normalized, "}")
	if objectStart >= 0 && objectEnd > objectStart {
		candidates = append(candidates, normalized[objectStart:objectEnd+1])
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		var parsed map[string]any
		if err := json.Unmarshal([]byte(candidate), &parsed); err == nil {
			return parsed
		}
	}
	return nil
}

// RepairJSONText is a shared minimal json_repair substitute for the package
// (strip markdown fences, keep the outermost {...} or [...] block).
func RepairJSONText(raw string) string {
	normalized := strings.TrimSpace(raw)
	if strings.HasPrefix(normalized, "```") {
		normalized = strings.TrimPrefix(normalized, "```json")
		normalized = strings.TrimPrefix(normalized, "```JSON")
		normalized = strings.TrimPrefix(normalized, "```")
		normalized = strings.TrimSuffix(normalized, "```")
		normalized = strings.TrimSpace(normalized)
	}
	objectStart := strings.IndexAny(normalized, "{[")
	objectEnd := strings.LastIndexAny(normalized, "}]")
	if objectStart >= 0 && objectEnd > objectStart {
		return normalized[objectStart : objectEnd+1]
	}
	return normalized
}

func firstNonEmpty(values ...any) any {
	for _, value := range values {
		if value == nil {
			continue
		}
		if strings.TrimSpace(stringValue(value)) != "" {
			return value
		}
	}
	return nil
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprintf("%v", value)
}

func coerceStringList(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(stringValue(item)); text != "" {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

func dedupeBlank(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func joinParticipants(participants []string) string {
	if len(participants) == 0 {
		return "未知"
	}
	return strings.Join(participants, "、")
}

func truncateText(text string, maxLength int) string {
	normalized := strings.TrimSpace(text)
	runes := []rune(normalized)
	if len(runes) <= maxLength {
		return normalized
	}
	return string(runes[:maxLength]) + "..."
}
