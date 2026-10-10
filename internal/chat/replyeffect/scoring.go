package replyeffect

import (
	"strings"
	"unicode/utf8"
)

// This file mirrors 参考/MaiBot/src/maisaka/reply_effect/scoring.py
// （行为以该文件为权威）：回复效果的确定性评分规则。
//
// 与 Python 版的差异：v6 起「情绪反馈」保留为分类而非连续分数
// （RECEPTION_CATEGORY_ORDER 顺序不变）；回应度已移除「回应速度」项，
// 其余三项按原始 45:25:20 归一化。

// ReceptionCategoryOrder mirrors RECEPTION_CATEGORY_ORDER。
var ReceptionCategoryOrder = []string{
	"appreciation", "playful", "neutral", "confusion",
	"factual_correction", "rejection", "bot_attack",
}

// ContributionValues mirrors CONTRIBUTION_VALUES。
var ContributionValues = map[string]float64{
	"advance":     1.0,
	"maintain":    0.6,
	"acknowledge": 0.3,
	"close":       0.1,
	"wrong_push":  0.0,
}

// Clamp mirrors scoring.clamp。
func Clamp(value, lower, upper float64) float64 {
	if value < lower {
		return lower
	}
	if value > upper {
		return upper
	}
	return value
}

// recordAssociations mirrors _record_associations：本记录自己收到的全部关联边。
func recordAssociations(record *Record) [][2]int {
	edges := make([][2]int, 0)
	for followupIndex := range record.FollowupMessages {
		followup := &record.FollowupMessages[followupIndex]
		for associationIndex := range followup.Associations {
			if followup.Associations[associationIndex].EffectID == record.EffectID {
				edges = append(edges, [2]int{followupIndex, associationIndex})
			}
		}
	}
	return edges
}

// breadthConfidenceFactor mirrors _breadth_confidence_factor。
func breadthConfidenceFactor(userCount int) float64 {
	return 0.75 + 0.25*Clamp(float64(userCount)/3.0, 0, 1)
}

// attributionReliability mirrors _attribution_reliability：
// 显式引用保持满额；语义关联按同消息的候选歧义数量折减。
func attributionReliability(followup *FollowupSnapshot, association *ReplyAssociation) float64 {
	if association.AttributionType == AttributionExplicitQuote {
		return Clamp(association.AttributionConfidence, 0, 1)
	}
	semanticCandidates := 0
	for index := range followup.Associations {
		if followup.Associations[index].AttributionType != AttributionExplicitQuote {
			semanticCandidates++
		}
	}
	if semanticCandidates < 1 {
		semanticCandidates = 1
	}
	return Clamp(association.AttributionConfidence, 0, 1) / float64(semanticCandidates)
}

// associationReliability mirrors _association_reliability：
// 关联正确性与情绪/贡献轴判断必须同时成立。
func associationReliability(followup *FollowupSnapshot, association *ReplyAssociation) float64 {
	return attributionReliability(followup, association) * Clamp(association.EvaluatorConfidence, 0, 1)
}

// CalculateResponseScore mirrors calculate_response_score → (score, confidence)。
func CalculateResponseScore(record *Record) (float64, float64) {
	edges := recordAssociations(record)
	if len(edges) == 0 {
		return 0.0, 0.0
	}
	confidences := make([]float64, 0, len(edges))
	perUser := map[string]float64{}
	for _, edge := range edges {
		followup := &record.FollowupMessages[edge[0]]
		association := &followup.Associations[edge[1]]
		confidences = append(confidences, association.AttributionConfidence)
		reliability := attributionReliability(followup, association)
		if current, ok := perUser[followup.UserID]; !ok || reliability > current {
			perUser[followup.UserID] = reliability
		}
	}
	presence := maxFloat(confidences)
	breadth := 0.0
	for _, value := range perUser {
		breadth += value
	}
	breadth = Clamp(breadth/3.0, 0, 1)
	depth := Clamp(sumFloat(confidences)/5.0, 0, 1)
	// 移除回应速度后，其余三项按原始 45:25:20 的比例归一化。
	score := 100.0 * (0.45*presence + 0.25*breadth + 0.20*depth) / 0.90
	confidence := 0.0
	for _, value := range perUser {
		confidence += value
	}
	confidence /= float64(len(perUser))
	confidence *= breadthConfidenceFactor(len(perUser))
	return roundTo(score, 2), roundTo(Clamp(confidence, 0, 1), 4)
}

// ClassifyReception mirrors classify_reception →
// (categories, counts, confidence)：保留 Bot 定向反馈的原始类别。
func ClassifyReception(record *Record) ([]string, map[string]int, float64) {
	categoryCounts := map[string]int{}
	perUserConfidence := map[string]float64{}
	for _, edge := range recordAssociations(record) {
		followup := &record.FollowupMessages[edge[0]]
		association := &followup.Associations[edge[1]]
		if association.StanceTarget != "bot_content" && association.StanceTarget != "bot_persona" {
			continue
		}
		weight := associationReliability(followup, association)
		if weight <= 0 {
			continue
		}
		categoryCounts[association.Stance]++
		if current, ok := perUserConfidence[followup.UserID]; !ok || weight > current {
			perUserConfidence[followup.UserID] = weight
		}
	}
	populated := map[string]int{}
	for _, category := range ReceptionCategoryOrder {
		if count := categoryCounts[category]; count > 0 {
			populated[category] = count
		}
	}
	if len(populated) == 0 {
		return nil, map[string]int{}, 0.0
	}
	confidence := 0.0
	for _, value := range perUserConfidence {
		confidence += value
	}
	confidence /= float64(len(perUserConfidence))
	confidence *= breadthConfidenceFactor(len(perUserConfidence))
	categories := make([]string, 0, len(populated))
	for _, category := range ReceptionCategoryOrder {
		if _, ok := populated[category]; ok {
			categories = append(categories, category)
		}
	}
	return categories, populated, roundTo(Clamp(confidence, 0, 1), 4)
}

// CalculateConversationScore mirrors calculate_conversation_score → (score, confidence)。
func CalculateConversationScore(record *Record) (float64, float64) {
	edges := recordAssociations(record)
	if len(edges) == 0 {
		return 0.0, 0.0
	}
	constructiveMass := 0.0
	constructiveUsers := map[string]bool{}
	relevantIDs := map[string]bool{}
	crossUserEdges := 0
	maxLatency := 0.0
	edgeMessageIDs := map[string]bool{}
	for _, edge := range edges {
		followup := &record.FollowupMessages[edge[0]]
		association := &followup.Associations[edge[1]]
		constructiveMass += association.AttributionConfidence * ContributionValues[association.Contribution]
		if ContributionValues[association.Contribution] > 0 {
			constructiveUsers[followup.UserID] = true
		}
		relevantIDs[followup.MessageID] = true
		if followup.LatencySeconds > maxLatency {
			maxLatency = followup.LatencySeconds
		}
		edgeMessageIDs[followup.MessageID] = true
	}
	for _, edge := range edges {
		followup := &record.FollowupMessages[edge[0]]
		association := &followup.Associations[edge[1]]
		if association.Contribution == "wrong_push" {
			continue
		}
		candidates := append([]string(nil), followup.QuoteTargetIDs...)
		if followup.ReplyTo != "" {
			candidates = append(candidates, followup.ReplyTo)
		}
		for _, candidate := range candidates {
			if relevantIDs[candidate] {
				crossUserEdges++
				break
			}
		}
	}
	observedMinutes := maxLatency / 60.0
	if observedMinutes < 1.0/60.0 {
		observedMinutes = 1.0 / 60.0
	}
	postRate := float64(len(edgeMessageIDs)) / observedMinutes
	preRate := float64(record.PreActivityCount) / 2.0
	activityLift := Clamp((postRate-preRate)/maxFloat2(preRate, 1.0), 0, 1)
	base := 0.35*Clamp(constructiveMass/4.0, 0, 1) +
		0.25*Clamp(float64(len(constructiveUsers))/3.0, 0, 1) +
		0.20*Clamp(float64(crossUserEdges)/3.0, 0, 1) +
		0.20*activityLift
	totalMass := 0.0
	wrongMass := 0.0
	perUserConfidence := map[string]float64{}
	for _, edge := range edges {
		followup := &record.FollowupMessages[edge[0]]
		association := &followup.Associations[edge[1]]
		totalMass += association.AttributionConfidence
		if association.Contribution == "wrong_push" {
			wrongMass += association.AttributionConfidence
		}
		reliability := associationReliability(followup, association)
		if current, ok := perUserConfidence[followup.UserID]; !ok || reliability > current {
			perUserConfidence[followup.UserID] = reliability
		}
	}
	wrongRatio := Clamp(wrongMass/maxFloat2(totalMass, 1.0), 0, 1)
	confidence := 0.0
	for _, value := range perUserConfidence {
		confidence += value
	}
	confidence /= float64(len(perUserConfidence))
	confidence *= breadthConfidenceFactor(len(perUserConfidence))
	return roundTo(100.0*base*(1.0-wrongRatio), 2), roundTo(Clamp(confidence, 0, 1), 4)
}

// ScoreRecord mirrors score_reply_effect。
func ScoreRecord(record *Record) *Scores {
	hasAssociations := len(recordAssociations(record)) > 0
	response, responseConfidence := CalculateResponseScore(record)
	receptionCategories, receptionCounts, receptionConfidence := ClassifyReception(record)
	conversation, conversationConfidence := CalculateConversationScore(record)

	type weighted struct {
		value  float64
		weight float64
	}
	weightedConfidences := []weighted{{responseConfidence, 0.40}, {conversationConfidence, 0.30}}
	if len(receptionCategories) > 0 {
		weightedConfidences = append(weightedConfidences, weighted{receptionConfidence, 0.30})
	}
	activeWeight := 0.0
	evidenceConfidence := 0.0
	for _, item := range weightedConfidences {
		activeWeight += item.weight
		evidenceConfidence += item.value * item.weight
	}
	evidenceConfidence /= activeWeight

	var confidence *float64
	if hasAssociations {
		value := roundTo(Clamp(0.35+0.65*evidenceConfidence, 0, 1), 4)
		confidence = &value
	}
	return &Scores{
		ResponseScore:                  response,
		ReceptionCategories:            receptionCategories,
		ReceptionCounts:                receptionCounts,
		ConversationScore:              conversation,
		Confidence:                     confidence,
		ResponseEvidenceConfidence:     responseConfidence,
		ReceptionEvidenceConfidence:    receptionConfidence,
		ConversationEvidenceConfidence: conversationConfidence,
	}
}

// NormalizeTextForPrompt mirrors normalize_text_for_prompt：折叠空白 + 限长截断。
func NormalizeTextForPrompt(text string, limit int) string {
	normalized := strings.Join(strings.Fields(text), " ")
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(normalized) <= limit {
		return normalized
	}
	runes := []rune(normalized)
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

// ActivityBucket mirrors activity_bucket。
func ActivityBucket(messageCount int) string {
	switch {
	case messageCount <= 1:
		return "low"
	case messageCount <= 5:
		return "medium"
	default:
		return "high"
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func trimSpace(value string) string { return strings.TrimSpace(value) }

func maxFloat(values []float64) float64 {
	best := 0.0
	for index, value := range values {
		if index == 0 || value > best {
			best = value
		}
	}
	return best
}

func maxFloat2(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func sumFloat(values []float64) float64 {
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total
}

// roundTo mirrors Python round(value, digits)（四舍五入到指定小数位）。
func roundTo(value float64, digits int) float64 {
	factor := 1.0
	for i := 0; i < digits; i++ {
		factor *= 10
	}
	scaled := value * factor
	rounded := float64(int64(scaled + sign(scaled)*0.5))
	return rounded / factor
}

func sign(value float64) float64 {
	if value < 0 {
		return -1
	}
	return 1
}
