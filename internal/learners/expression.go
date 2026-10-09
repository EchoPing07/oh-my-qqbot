package learners

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// This file mirrors 参考/MaiBot/src/learners/expression_learner.py
// (behaviour is authoritative there): 表达学习批次闸门 + 候选抽取 + self_reflect
// 检查 + 入库 + 向量索引增量更新。

// MaxExpressionsPerBatch mirrors `if len(expressions) > 20: expressions = []`.
const MaxExpressionsPerBatch = 20

// MinMessagesForExtraction mirrors ExpressionLearner.min_messages_for_extraction.
const MinMessagesForExtraction = 10

// ExpressionLearningReason 是闸门拒绝原因的取值（对齐 acquire 的返回值）。
const (
	GateAcquired        = ""
	GateMaxDisabled     = "max_expression_learner <= 0"
	GateSessionBusy     = "session_busy"
	GateGlobalLimit     = "global_limit"
	GateInvalidMaxCount = "invalid_max_count"
)

// ExpressionLearningAcquireResult mirrors ExpressionLearningAcquireResult.
type ExpressionLearningAcquireResult struct {
	Acquired    bool
	Reason      string
	ActiveCount int
	MaxCount    int
}

// ExpressionLearningBatchGate mirrors ExpressionLearningBatchGate：会话互斥 +
// 全局并发上限（max_expression_learner，默认 3）。
type ExpressionLearningBatchGate struct {
	// MaxCount mirrors expression.max_expression_learner。
	MaxCount int

	mu       sync.Mutex
	active   map[string]bool
	acquired int
	released int
}

// NewExpressionLearningBatchGate 构造闸门。
func NewExpressionLearningBatchGate(maxCount int) *ExpressionLearningBatchGate {
	return &ExpressionLearningBatchGate{MaxCount: maxCount, active: map[string]bool{}}
}

// Acquire mirrors ExpressionLearningBatchGate.acquire.
func (g *ExpressionLearningBatchGate) Acquire(sessionID string) ExpressionLearningAcquireResult {
	maxCount := g.MaxCount
	if maxCount <= 0 {
		return ExpressionLearningAcquireResult{Acquired: false, Reason: GateMaxDisabled, ActiveCount: 0, MaxCount: maxCount}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	activeCount := len(g.active)
	if g.active[sessionID] {
		return ExpressionLearningAcquireResult{Acquired: false, Reason: GateSessionBusy, ActiveCount: activeCount, MaxCount: maxCount}
	}
	if activeCount >= maxCount {
		return ExpressionLearningAcquireResult{Acquired: false, Reason: GateGlobalLimit, ActiveCount: activeCount, MaxCount: maxCount}
	}
	g.active[sessionID] = true
	g.acquired++
	return ExpressionLearningAcquireResult{Acquired: true, ActiveCount: activeCount + 1, MaxCount: maxCount}
}

// Release mirrors ExpressionLearningBatchGate.release.
func (g *ExpressionLearningBatchGate) Release(sessionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active[sessionID] {
		delete(g.active, sessionID)
		g.released++
	}
}

// ActiveCount 返回当前活跃会话数。
func (g *ExpressionLearningBatchGate) ActiveCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.active)
}

// Acquired / Released 返回累计计数（测试/观测用）。
func (g *ExpressionLearningBatchGate) Acquired() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.acquired
}

// Released 返回累计释放计数。
func (g *ExpressionLearningBatchGate) Released() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.released
}

// ---------------------------------------------------------------------------
// ExpressionLearner
// ---------------------------------------------------------------------------

// ExpressionLearnerOptions 汇总表达学习配置（对齐 global_config.expression）。
type ExpressionLearnerOptions struct {
	// SelfReflect mirrors expression_self_reflect。
	SelfReflect bool
	// CheckedOnly mirrors expression_checked_only（候选只取人工审核过的）。
	CheckedOnly bool
	// UseVectorExpression mirrors use_vector_expression（写库后同步向量索引）。
	UseVectorExpression bool
	// BotName / AliasNames mirror bot.nickname / bot.alias_names（style 过滤）。
	BotName    string
	AliasNames []string
	// VectorIndexPath mirrors expression_vector_index_path。
	VectorIndexPath string
}

// ExpressionLearner mirrors ExpressionLearner。
type ExpressionLearner struct {
	// SessionID 是学习器所属会话（对齐 ExpressionLearner(session_id)）。
	SessionID string
	// Options 是表达学习配置。
	Options ExpressionLearnerOptions
	// Gate 是批次闸门（必填；nil 时使用 MaxExpressionLearner 新建）。
	Gate *ExpressionLearningBatchGate
	// Store 是表达方式持久化边界。
	Store ExpressionStore
	// Index 是表达向量索引（UseVectorExpression 开启时同步）。
	Index *ExpressionVectorIndex
	// Prompts 渲染 learn_style / expression_evaluation。
	Prompts PromptFunc
	// LLM 是学习模型（learner 任务）。
	LLM LLMFunc
	// Judge 是 self_reflect 审核模型（nil 时复用 LLM）。
	Judge LLMFunc
	// Embedder 是表达向量生成的 embedding 边界（Index 未注入时用它构造索引）。
	Embedder EmbedFunc
	// EmbeddingProfile / EmbeddingModel 是索引的 embedding 后端标识（观测用）。
	EmbeddingProfile string
	EmbeddingModel   string
	// Sessions 判定会话是否已注册（对齐 chat_manager.get_existing_session_by_session_id）。
	Sessions SessionExistsFunc
	// SessionNames mirrors chat_manager.get_session_name。
	SessionNames SessionDisplayNameFunc
	// Logf 日志回调。
	Logf func(format string, args ...any)
	// Learned 累计成功写入的表达方式数（观测/测试用）。
	Learned int

	mu sync.Mutex
}

func (l *ExpressionLearner) log(format string, args ...any) {
	if l.Logf != nil {
		l.Logf(format, args...)
	}
}

// gate 返回闸门（惰性初始化）。
func (l *ExpressionLearner) gate(maxCount int) *ExpressionLearningBatchGate {
	if l.Gate != nil {
		return l.Gate
	}
	l.Gate = NewExpressionLearningBatchGate(maxCount)
	return l.Gate
}

// LearnFromContextMessages mirrors learn_from_context_messages：从被裁切的上下文
// 消息中学习表达方式（只保留真实聊天消息）。
func (l *ExpressionLearner) LearnFromContextMessages(
	ctx context.Context,
	contextMessages []chatcontextAlias,
	isBotSelf IsBotSelfFunc,
	maxLearners int,
) (bool, error) {
	sources := LearningSourceFromContext(contextMessages, isBotSelf)
	if len(sources) == 0 {
		l.log("裁切历史中没有可用于表达学习的真实聊天消息")
		return false, nil
	}
	if len(sources) < MinMessagesForExtraction {
		l.log("裁切历史可学习消息不足: 可学习=%d 阈值=%d", len(sources), MinMessagesForExtraction)
		return false, nil
	}
	return l.LearnFromMessages(ctx, sources, maxLearners)
}

// LearnFromMessages mirrors `_learn_from_session_messages` 的闸门 + 批次执行。
func (l *ExpressionLearner) LearnFromMessages(
	ctx context.Context,
	messages []LearningMessage,
	maxLearners int,
) (bool, error) {
	learningSessionID := ResolveLearningSessionID(l.SessionID, messages, l.Sessions)
	if learningSessionID == "" {
		l.log("表达学习已跳过：无法解析到有效聊天流, learner_session_id=%s", l.SessionID)
		return false, nil
	}
	if learningSessionID != l.SessionID {
		l.log("表达学习会话 ID 已按真实消息修正: learner_session_id=%s learning_session_id=%s",
			l.SessionID, learningSessionID)
	}

	gate := l.gate(maxLearners)
	acquire := gate.Acquire(learningSessionID)
	if !acquire.Acquired {
		switch acquire.Reason {
		case GateSessionBusy:
			l.log("%s 已有表达学习批次正在运行，放弃新的批次", learningSessionID)
		case GateGlobalLimit:
			l.log("表达学习全局并发已满，放弃新的批次: active=%d, max=%d, session_id=%s",
				acquire.ActiveCount, acquire.MaxCount, learningSessionID)
		default:
			l.log("表达学习并发配置无效，放弃新的批次: max_expression_learner=%d, session_id=%s",
				acquire.MaxCount, learningSessionID)
		}
		return false, nil
	}
	defer gate.Release(learningSessionID)

	return l.runLearningBatch(ctx, messages, learningSessionID)
}

// ResolveLearningSessionID mirrors `_resolve_learning_session_id`.
func ResolveLearningSessionID(
	learnerSessionID string,
	messages []LearningMessage,
	sessions SessionExistsFunc,
) string {
	exists := func(sessionID string) bool {
		if strings.TrimSpace(sessionID) == "" {
			return false
		}
		if sessions == nil {
			return true
		}
		return sessions(sessionID)
	}
	counts := map[string]int{}
	order := make([]string, 0, len(messages))
	for _, msg := range messages {
		sessionID := strings.TrimSpace(msg.SessionID)
		if sessionID == "" {
			continue
		}
		if _, seen := counts[sessionID]; !seen {
			order = append(order, sessionID)
		}
		counts[sessionID]++
	}
	sort.SliceStable(order, func(i, j int) bool {
		if counts[order[i]] != counts[order[j]] {
			return counts[order[i]] > counts[order[j]]
		}
		return order[i] < order[j]
	})
	for _, sessionID := range order {
		if exists(sessionID) {
			return sessionID
		}
	}
	if exists(learnerSessionID) {
		return learnerSessionID
	}
	l := strings.TrimSpace(learnerSessionID)
	_ = l
	return ""
}

// runLearningBatch mirrors `_run_learning_batch`.
func (l *ExpressionLearner) runLearningBatch(
	ctx context.Context,
	messages []LearningMessage,
	learningSessionID string,
) (bool, error) {
	if l.LLM == nil || l.Prompts == nil {
		return false, fmt.Errorf("learners: 表达学习缺少 LLM 或 prompt 渲染器")
	}
	readableMessage := "聊天记录将在后续多条 user message 中给出；请以每条消息中的 source_id 作为来源行编号。"
	systemPrompt, err := l.Prompts("learn_style", map[string]string{
		"chat_str": readableMessage,
	})
	if err != nil {
		return false, err
	}
	requestMessages := BuildExpressionLearningMessages(systemPrompt, messages)
	raw, err := l.LLM(ctx, LLMRequest{
		Task:        "learner",
		RequestType: "expression.learner",
		SessionID:   learningSessionID,
		Messages:    requestMessages,
	})
	if err != nil {
		l.log("学习表达方式失败: %v", err)
		return false, nil
	}

	expressions := ParseExpressionResponse(raw)
	if len(expressions) > MaxExpressionsPerBatch {
		l.log("表达方式数量超过20: %d，已丢弃本批", len(expressions))
		expressions = nil
	}
	if len(expressions) == 0 {
		l.log("没有可学习的表达方式")
		return false, nil
	}

	learnt := l.FilterExpressions(expressions, messages)
	if len(learnt) == 0 {
		l.log("没有可学习的表达方式通过过滤")
		return false, nil
	}

	written := make([]ExpressionRow, 0, len(learnt))
	for _, expression := range learnt {
		if l.Options.SelfReflect && !l.checkExpressionBeforeUpsert(ctx, expression.Situation, expression.Style, learningSessionID) {
			continue
		}
		record, err := l.upsertExpression(ctx, expression.Situation, expression.Style, learningSessionID)
		if err != nil {
			l.log("创建表达方式失败: %v", err)
			continue
		}
		if record != nil {
			written = append(written, *record)
		}
	}
	if len(written) == 0 {
		return false, nil
	}
	l.syncVectorIndex(ctx, written)
	l.mu.Lock()
	l.Learned += len(written)
	l.mu.Unlock()
	return true, nil
}

// BuildExpressionLearningMessages mirrors `_build_multi_learning_messages`.
func BuildExpressionLearningMessages(systemPrompt string, messages []LearningMessage) []LLMMessage {
	out := make([]LLMMessage, 0, len(messages)+2)
	out = append(out, LLMMessage{
		Role: "system",
		Content: systemPrompt + "\n\n" +
			"注意：聊天记录会在后续多条 user message 中给出。每条消息内的 source_id " +
			"是本轮学习的来源编号；speaker=SELF 的消息只作为上下文，不要从 SELF 的发言中学习。",
	})
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
		out = append(out, LLMMessage{
			Role: "user",
			Content: strings.Join([]string{
				fmt.Sprintf("[source_id:%d]", index+1),
				"[speaker:" + speakerKind + "]",
				"[name:" + speakerName + "]",
				"[content]",
				content,
			}, "\n"),
		})
	}
	out = append(out, LLMMessage{Role: "user", Content: "请根据以上聊天消息输出 JSON。"})
	return out
}

// ParseExpressionResponse mirrors expression_utils.parse_expression_response
// （只取表达方式列表；黑话由独立 learner 处理）。
func ParseExpressionResponse(response string) []ExtractedExpression {
	if strings.TrimSpace(response) == "" {
		return nil
	}
	raw := RepairJSONDocument(response)
	var parsedList []map[string]any
	var single map[string]any
	if err := json.Unmarshal([]byte(raw), &single); err == nil {
		parsedList = []map[string]any{single}
	} else {
		var list []map[string]any
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return nil
		}
		parsedList = list
	}
	expressions := make([]ExtractedExpression, 0, len(parsedList))
	for _, item := range parsedList {
		situation := normalizeJSONString(item["situation"])
		style := NormalizeExpressionStyleForLearning(normalizeJSONString(item["style"]))
		sourceID := normalizeJSONString(item["source_id"])
		if situation == "" || style == "" || sourceID == "" {
			continue
		}
		expressions = append(expressions, ExtractedExpression{
			Situation: situation,
			Style:     style,
			SourceID:  sourceID,
		})
	}
	return expressions
}

func normalizeJSONString(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value))
}

// FilterExpressions mirrors `_filter_expressions`.
func (l *ExpressionLearner) FilterExpressions(
	expressions []ExtractedExpression,
	messages []LearningMessage,
) []FilteredExpression {
	banned := map[string]bool{}
	if botName := strings.TrimSpace(l.Options.BotName); botName != "" {
		banned[strings.ToLower(botName)] = true
	}
	for _, alias := range l.Options.AliasNames {
		if trimmed := strings.TrimSpace(alias); trimmed != "" {
			banned[strings.ToLower(trimmed)] = true
		}
	}

	out := make([]FilteredExpression, 0, len(expressions))
	for _, expression := range expressions {
		sourceID := strings.TrimSpace(expression.SourceID)
		if !isDigits(sourceID) {
			continue
		}
		lineIndex := atoiSafe(sourceID) - 1
		if lineIndex < 0 || lineIndex >= len(messages) {
			continue
		}
		current := messages[lineIndex]
		if current.IsSelf {
			continue
		}
		contextText := strings.TrimSpace(current.Content)
		if contextText == "" {
			continue
		}
		normalizedStyle := NormalizeExpressionStyleForLearning(expression.Style)
		if normalizedStyle == "" {
			continue
		}
		if strings.Contains(expression.Situation, "SELF") || strings.Contains(normalizedStyle, "SELF") ||
			strings.Contains(contextText, "SELF") {
			continue
		}
		if banned[strings.ToLower(normalizedStyle)] {
			continue
		}
		if strings.Contains(expression.Situation, "[表情包") || strings.Contains(normalizedStyle, "[表情包") ||
			strings.Contains(contextText, "[表情包") {
			continue
		}
		if strings.Contains(expression.Situation, "[图片") || strings.Contains(normalizedStyle, "[图片") ||
			strings.Contains(contextText, "[图片") {
			continue
		}
		out = append(out, FilteredExpression{Situation: expression.Situation, Style: normalizedStyle})
	}
	return out
}

// checkExpressionBeforeUpsert mirrors `_check_expression_before_upsert`
// （expression_evaluation.prompt 的 LLM 审核）。
func (l *ExpressionLearner) checkExpressionBeforeUpsert(
	ctx context.Context,
	situation string,
	style string,
	sessionID string,
) bool {
	judge := l.Judge
	if judge == nil {
		judge = l.LLM
	}
	if judge == nil || l.Prompts == nil {
		return true
	}
	criteria := []string{
		"表达方式或言语风格是否与使用条件或使用情景匹配",
		"允许部分语法错误或口语化或缺省出现",
		"表达方式不能太过特指，需要具有泛用性",
		"一般不涉及具体的人名或名称",
	}
	criteriaList := make([]string, 0, len(criteria))
	for index, criterion := range criteria {
		criteriaList = append(criteriaList, fmt.Sprintf("%d. %s", index+1, criterion))
	}
	prompt, err := l.Prompts("expression_evaluation", map[string]string{
		"situation":     situation,
		"style":         style,
		"criteria_list": strings.Join(criteriaList, "\n"),
	})
	if err != nil {
		l.log("渲染表达方式审核 prompt 失败: %v", err)
		return false
	}
	raw, err := judge(ctx, LLMRequest{
		Task:        "learner",
		RequestType: "expression.check",
		SessionID:   sessionID,
		Messages:    []LLMMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		l.log("检查表达方式时发生错误: %v", err)
		return false
	}
	suitable, _ := ParseExpressionEvaluation(raw)
	return suitable
}

// ParseExpressionEvaluation mirrors parse_evaluation_response（返回 suitable/reason）。
func ParseExpressionEvaluation(response string) (bool, string) {
	raw := RepairJSONDocument(response)
	if strings.TrimSpace(raw) == "" {
		return false, ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		// 兜底：正则式提取 suitable / reason（对齐 Python 的兜底分支）。
		lowered := strings.ToLower(raw)
		index := strings.Index(lowered, "\"suitable\"")
		if index < 0 {
			return false, ""
		}
		rest := lowered[index:]
		suitable := strings.Contains(rest, "true")
		return suitable, ""
	}
	suitable, _ := payload["suitable"].(bool)
	reason := normalizeJSONString(payload["reason"])
	if reason == "" {
		if text, ok := payload["reason"].(string); ok {
			reason = strings.TrimSpace(text)
		}
	}
	return suitable, reason
}

// upsertExpression mirrors `_upsert_expression_to_db`（完全一致则 count+1）。
func (l *ExpressionLearner) upsertExpression(
	ctx context.Context,
	situation string,
	style string,
	sessionID string,
) (*ExpressionRow, error) {
	if l.Store == nil {
		return nil, nil
	}
	normalizedSituation := strings.TrimSpace(situation)
	normalizedStyle := NormalizeExpressionStyleForLearning(style)
	if normalizedSituation == "" || normalizedStyle == "" {
		return nil, nil
	}
	existing, err := l.Store.FindExact(sessionID, normalizedSituation, normalizedStyle)
	if err != nil {
		return nil, err
	}
	modifiedBy := ""
	if l.Options.SelfReflect {
		modifiedBy = "AI"
	}
	if existing != nil {
		record := ExpressionRecord{
			ID:             existing.ID,
			Situation:      existing.Situation,
			Style:          existing.Style,
			Count:          existing.Count + 1,
			Checked:        false,
			LastActiveTime: nowUnix(),
			CreateTime:     nowUnix(),
		}
		if sessionID != "" {
			sessionIDCopy := sessionID
			record.SessionID = &sessionIDCopy
		}
		if modifiedBy != "" {
			modifiedByCopy := modifiedBy
			record.ModifiedBy = &modifiedByCopy
		}
		updated, err := l.Store.Upsert(record)
		if err != nil {
			return nil, err
		}
		return &ExpressionRow{
			ID:         updated.ID,
			Situation:  updated.Situation,
			Style:      updated.Style,
			Count:      updated.Count,
			Checked:    updated.Checked,
			SessionID:  updated.SessionID,
			ModifiedBy: modifiedBy,
		}, nil
	}

	record := ExpressionRecord{
		Situation:      normalizedSituation,
		Style:          normalizedStyle,
		ContentList:    mustJSON([]string{normalizedSituation}),
		Count:          1,
		LastActiveTime: nowUnix(),
		CreateTime:     nowUnix(),
		Checked:        false,
	}
	if sessionID != "" {
		sessionIDCopy := sessionID
		record.SessionID = &sessionIDCopy
	}
	if modifiedBy != "" {
		modifiedByCopy := modifiedBy
		record.ModifiedBy = &modifiedByCopy
	}
	created, err := l.Store.Upsert(record)
	if err != nil {
		return nil, err
	}
	return &ExpressionRow{
		ID:         created.ID,
		Situation:  created.Situation,
		Style:      created.Style,
		Count:      created.Count,
		Checked:    created.Checked,
		SessionID:  created.SessionID,
		ModifiedBy: modifiedBy,
	}, nil
}

// syncVectorIndex mirrors `_sync_expression_vector_index_batch`（向量索引是派生
// 数据：同步失败不得把已成功的学习改报为失败）。
func (l *ExpressionLearner) syncVectorIndex(ctx context.Context, written []ExpressionRow) {
	if !l.Options.UseVectorExpression {
		return
	}
	if l.Index == nil {
		l.Index = NewExpressionVectorIndex(l.Options.VectorIndexPath)
		l.Index.Embedder = l.Embedder
		l.Index.Profile = l.EmbeddingProfile
		l.Index.Model = l.EmbeddingModel
	}
	items := make([]UpsertItem, 0, len(written))
	for _, record := range written {
		sessionID := ""
		if record.SessionID != nil {
			sessionID = *record.SessionID
		}
		items = append(items, UpsertItem{
			ID:         record.ID,
			Situation:  record.Situation,
			Style:      record.Style,
			Count:      record.Count,
			SessionID:  sessionID,
			Checked:    record.Checked,
			ModifiedBy: record.ModifiedBy,
		})
	}
	if _, err := l.Index.UpsertExpressions(ctx, items); err != nil {
		l.log("表达向量索引同步失败，已保留数据库学习结果: count=%d err=%v", len(items), err)
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// RepairJSONDocument 剥离 markdown fence 并保留最外层 JSON 块。
func RepairJSONDocument(raw string) string {
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

func mustJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func atoiSafe(value string) int {
	result := 0
	for _, char := range value {
		if char < '0' || char > '9' {
			return result
		}
		result = result*10 + int(char-'0')
		if result > 1<<30 {
			return result
		}
	}
	return result
}

func nowUnix() int64 { return timeNow().Unix() }
