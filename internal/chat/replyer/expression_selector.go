package replyer

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/message"
)

// This file mirrors 参考/MaiBot/src/chat/replyer/maisaka_expression_selector.py
// (behaviour is authoritative there): replyer 侧表达方式筛选与子代理二次选择。
//
// 候选来源（DB / 向量索引）通过 ExpressionSource 注入（learners 包后续实现）；
// LLM 子代理通过 SubAgentRunner 注入；本包不依赖 internal/config。

// MaxSelectedExpressions mirrors MAX_SELECTED_EXPRESSIONS.
const MaxSelectedExpressions = 5

// vectorClusterPoolSize mirrors _VECTOR_CLUSTER_POOL_SIZE.
const vectorClusterPoolSize = 16

// SubAgentRunnerFunc is the callable form of the expression sub-agent.
type SubAgentRunnerFunc func(ctx context.Context, systemPrompt string) (string, error)

// MaisakaExpressionSelector mirrors MaisakaExpressionSelector.
type MaisakaExpressionSelector struct {
	Source     ExpressionSource
	SubAgent   SubAgentRunnerFunc
	MaxResults int
	// Now mirrors datetime.now()（测试可注入）。
	Now func() time.Time
}

// SelectionInput mirrors select_for_reply's arguments.
type SelectionInput struct {
	SessionID     string
	ChatHistory   []chatcontext.LLMContextMessage
	ReplyMessage  *message.SessionMessage
	ReplyReason   string
	ReplyToolArgs map[string]any
}

// SelectionResult mirrors MaisakaExpressionSelectionResult.
type SelectionResult struct {
	ExpressionHabits      string
	SelectedExpressionIDs []int64
	SelectedExpressions   []ExpressionCandidate
}

// WeightedSample mirrors learner_utils_old.weighted_sample（按 count 权重抽样）。
func WeightedSample(population []ExpressionCandidate, k int) []ExpressionCandidate {
	if len(population) == 0 || k <= 0 {
		return nil
	}
	if len(population) <= k {
		return append([]ExpressionCandidate{}, population...)
	}

	selected := make([]ExpressionCandidate, 0, k)
	populationCopy := append([]ExpressionCandidate{}, population...)

	for range k {
		weights := computeWeights(populationCopy)
		totalWeight := 0.0
		for _, weight := range weights {
			totalWeight += weight
		}
		if totalWeight <= 0 {
			// 回退到均匀随机
			idx := randInt(len(populationCopy))
			selected = append(selected, populationCopy[idx])
			populationCopy = append(populationCopy[:idx], populationCopy[idx+1:]...)
			continue
		}
		threshold := randFloat() * totalWeight
		cumulative := 0.0
		for idx, weight := range weights {
			cumulative += weight
			if threshold <= cumulative {
				selected = append(selected, populationCopy[idx])
				populationCopy = append(populationCopy[:idx], populationCopy[idx+1:]...)
				break
			}
		}
	}
	return selected
}

// computeWeights mirrors _compute_weights（线性映射到 [1,5] 区间）。
func computeWeights(population []ExpressionCandidate) []float64 {
	if len(population) == 0 {
		return nil
	}
	counts := make([]float64, 0, len(population))
	for _, item := range population {
		countValue := float64(item.Count)
		if countValue < 0 {
			countValue = 0
		}
		counts = append(counts, countValue)
	}
	minCount := counts[0]
	maxCount := counts[0]
	for _, count := range counts {
		if count < minCount {
			minCount = count
		}
		if count > maxCount {
			maxCount = count
		}
	}
	weights := make([]float64, 0, len(counts))
	if maxCount == minCount {
		for range counts {
			weights = append(weights, 1.0)
		}
		return weights
	}
	for _, countValue := range counts {
		normalized := (countValue - minCount) / (maxCount - minCount)
		weights = append(weights, 1.0+normalized*4.0) // 1~5
	}
	return weights
}

// SampleLegacyExpressionCandidates mirrors _sample_legacy_expression_candidates:
// 高频候选 5 条 + 随机候选 5 条（候选总数 < 10 时直接不选）。
func SampleLegacyExpressionCandidates(allCandidates []ExpressionCandidate) []ExpressionCandidate {
	if len(allCandidates) < 10 {
		return nil
	}

	var highCountCandidates []ExpressionCandidate
	for _, item := range allCandidates {
		if item.Count > 1 {
			highCountCandidates = append(highCountCandidates, item)
		}
	}
	var selectedHigh []ExpressionCandidate
	if len(highCountCandidates) >= 10 {
		selectedHigh = WeightedSample(highCountCandidates, minInt(len(highCountCandidates), 5))
	}
	selectedRandom := WeightedSample(allCandidates, minInt(len(allCandidates), 5))

	candidatePool := make([]ExpressionCandidate, 0)
	seenIDs := map[int64]bool{}
	for _, candidate := range append(append([]ExpressionCandidate{}, selectedHigh...), selectedRandom...) {
		if seenIDs[candidate.ID] {
			continue
		}
		seenIDs[candidate.ID] = true
		candidatePool = append(candidatePool, candidate)
	}
	return candidatePool
}

// BuildExpressionHabitsBlock mirrors _build_expression_habits_block（文案照抄）。
func BuildExpressionHabitsBlock(selectedExpressions []ExpressionCandidate) string {
	if len(selectedExpressions) == 0 {
		return ""
	}
	lines := make([]string, 0, len(selectedExpressions))
	for _, expression := range selectedExpressions {
		lines = append(lines, fmt.Sprintf(
			"- 当“%s”时，可以用“%s”来表达。", expression.Situation, expression.Style))
	}
	return "【表达习惯参考，请视情况自然的使用】\n" + strings.Join(lines, "\n")
}


// formatExpressionIntent mirrors _format_expression_intent.
func formatExpressionIntent(replyToolArgs map[string]any) string {
	if replyToolArgs == nil {
		return ""
	}
	rawIntent, ok := replyToolArgs["expression_intent"]
	if !ok {
		return ""
	}
	var lines []string
	fieldLabels := [][2]string{
		{"focus", "贴合对象"},
		{"reply_act", "回复动作"},
		{"scene", "表达场景"},
		{"tone", "期望语气"},
		{"prefer", "优先表达"},
		{"avoid", "避免表达"},
	}
	if intentMap, ok := rawIntent.(map[string]any); ok {
		for _, pair := range fieldLabels {
			fieldValue := intentMap[pair[0]]
			normalizedValue := ""
			switch v := fieldValue.(type) {
			case []any:
				parts := make([]string, 0)
				for _, item := range v {
					if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
						parts = append(parts, strings.TrimSpace(s))
					}
				}
				normalizedValue = strings.Join(parts, "、")
			case string:
				normalizedValue = strings.TrimSpace(v)
			}
			if normalizedValue != "" {
				lines = append(lines, fmt.Sprintf("- %s：%s", pair[1], normalizedValue))
			}
		}
	} else if s, ok := rawIntent.(string); ok {
		if strings.TrimSpace(s) != "" {
			lines = append(lines, fmt.Sprintf("- 表达意图：%s", strings.TrimSpace(s)))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "表达选择意图：\n" + strings.Join(lines, "\n")
}

// buildExpressionQueryText mirrors _build_expression_query_text.
func buildExpressionQueryText(replyReason string, replyToolArgs map[string]any) string {
	var queryParts []string
	if intentBlock := formatExpressionIntent(replyToolArgs); intentBlock != "" {
		queryParts = append(queryParts, intentBlock)
	}

	replyReference := ""
	if replyToolArgs != nil {
		replyReference = strings.TrimSpace(toString(replyToolArgs["reply_reference"]))
	}
	if replyReference != "" {
		queryParts = append(queryParts, "回复信息参考：\n"+replyReference)
	} else {
		normalizedReplyReason := strings.TrimSpace(replyReason)
		if normalizedReplyReason != "" {
			queryParts = append(queryParts, "Planner 推理：\n"+normalizedReplyReason)
		}
	}
	return strings.Join(queryParts, "\n\n")
}

// BuildSelectorPrompt mirrors _build_selector_prompt（文案照抄）。
func BuildSelectorPrompt(candidates []ExpressionCandidate) string {
	candidateLines := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidateLines = append(candidateLines,
			fmt.Sprintf("%d: 情景=%s | 风格=%s", candidate.ID, candidate.Situation, candidate.Style))
	}
	return strings.Join([]string{
		"你是 Maisaka 的表达方式选择子代理。",
		"你只负责根据下方真实聊天上下文，为这一次可见回复挑选最合适的表达方式。",
		fmt.Sprintf("请只从下面候选中选择 0 到 %d 条最适合当前语境的表达方式。", MaxSelectedExpressions),
		"优先考虑自然、贴合上下文、不生硬、不模板化。",
		"如果没有明显合适的，就返回空数组。",
		"严格只输出 JSON，对象格式为 {\"selected_ids\":[123,456]}。",
		"",
		"候选表达方式：\n" + strings.Join(candidateLines, "\n"),
	}, "\n")
}

// parseSelectedIDs mirrors _parse_selected_ids（JSON 修复：Go 版只做宽容提取）。
func parseSelectedIDs(rawResponse string, candidates []ExpressionCandidate) []int64 {
	if strings.TrimSpace(rawResponse) == "" {
		return nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(repairJSON(rawResponse)), &parsed); err != nil {
		return nil
	}
	rawSelectedIDs, _ := parsed["selected_ids"].([]any)
	candidateMap := map[int64]bool{}
	for _, candidate := range candidates {
		candidateMap[candidate.ID] = true
	}
	selectedIDs := make([]int64, 0)
	for _, rawID := range rawSelectedIDs {
		id, ok := toInt64(rawID)
		if !ok || !candidateMap[id] {
			continue
		}
		contains := false
		for _, existing := range selectedIDs {
			if existing == id {
				contains = true
				break
			}
		}
		if contains {
			continue
		}
		selectedIDs = append(selectedIDs, id)
		if len(selectedIDs) >= MaxSelectedExpressions {
			break
		}
	}
	return selectedIDs
}

// repairJSON is the minimal json_repair substitute: strip markdown fences and
// grab the outermost {...} block.
func repairJSON(raw string) string {
	normalized := strings.TrimSpace(raw)
	if strings.HasPrefix(normalized, "```") {
		normalized = strings.TrimPrefix(normalized, "```json")
		normalized = strings.TrimPrefix(normalized, "```")
		normalized = strings.TrimSuffix(normalized, "```")
		normalized = strings.TrimSpace(normalized)
	}
	start := strings.Index(normalized, "{")
	end := strings.LastIndex(normalized, "}")
	if start >= 0 && end > start {
		return normalized[start : end+1]
	}
	return normalized
}

func toInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	}
	return 0, false
}

// SelectForReply mirrors select_for_reply.
func (s *MaisakaExpressionSelector) SelectForReply(ctx context.Context, input SelectionInput) SelectionResult {
	if input.SessionID == "" || s.Source == nil {
		return SelectionResult{}
	}

	allCandidates := s.Source.LoadAllCandidates(input.SessionID)
	if len(allCandidates) < 10 {
		return SelectionResult{}
	}

	var candidates []ExpressionCandidate
	if s.Source.UseVectorExpression() {
		queryText := buildExpressionQueryText(input.ReplyReason, input.ReplyToolArgs)
		vectorCandidates, ok := s.Source.BuildVectorCandidatePool(
			ctx, input.SessionID, queryText, allCandidates, vectorClusterPoolSize)
		if ok && len(vectorCandidates) > 0 {
			candidates = vectorCandidates
		} else {
			candidates = SampleLegacyExpressionCandidates(allCandidates)
		}
	} else {
		candidates = SampleLegacyExpressionCandidates(allCandidates)
	}
	if len(candidates) == 0 {
		return SelectionResult{}
	}

	// 子代理选择；缺失/失败时回退直接注入
	result := s.buildSelectionResult(candidates, nil)
	if s.SubAgent != nil {
		selectorPrompt := BuildSelectorPrompt(candidates)
		if rawResponse, err := s.SubAgent(ctx, selectorPrompt); err == nil {
			selectedIDs := parseSelectedIDs(rawResponse, candidates)
			result = s.buildSelectionResult(candidates, selectedIDs)
		} else {
			result = s.buildDirectSelectionResult(candidates)
		}
	} else {
		result = s.buildDirectSelectionResult(candidates)
	}
	return result
}

// buildDirectSelectionResult mirrors _build_direct_selection_result.
func (s *MaisakaExpressionSelector) buildDirectSelectionResult(candidates []ExpressionCandidate) SelectionResult {
	selectedExpressions := append([]ExpressionCandidate{}, candidates...)
	return SelectionResult{
		ExpressionHabits:      BuildExpressionHabitsBlock(selectedExpressions),
		SelectedExpressionIDs: candidateIDs(selectedExpressions),
		SelectedExpressions:   selectedExpressions,
	}
}

// buildSelectionResult mirrors _build_selection_result_from_ids.
func (s *MaisakaExpressionSelector) buildSelectionResult(candidates []ExpressionCandidate, selectedIDs []int64) SelectionResult {
	candidateMap := map[int64]ExpressionCandidate{}
	for _, candidate := range candidates {
		candidateMap[candidate.ID] = candidate
	}
	selectedExpressions := make([]ExpressionCandidate, 0, len(selectedIDs))
	for _, id := range selectedIDs {
		if candidate, ok := candidateMap[id]; ok {
			selectedExpressions = append(selectedExpressions, candidate)
		}
	}
	return SelectionResult{
		ExpressionHabits:      BuildExpressionHabitsBlock(selectedExpressions),
		SelectedExpressionIDs: selectedIDs,
		SelectedExpressions:   selectedExpressions,
	}
}

func candidateIDs(candidates []ExpressionCandidate) []int64 {
	ids := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func minInt(a int, b int) int {
	if a < b {
		return a
	}
	return b
}
