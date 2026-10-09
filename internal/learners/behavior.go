package learners

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// This file mirrors the 实验骨架 of 参考/MaiBot/src/learners/behavior_*.py
// (behaviour is authoritative there): 场景分析（scenario analyzer）+ 行为记忆
// 召回 + behavior_style_prompt 注入。
//
// 默认关闭（对齐 experimental.enable_behavior_learning 默认 false）；本文件只
// 实现「开关骨架 + 场景分析接口占位 + 存储接口占位」，完整学习链路由 P11 补齐。

// 学习类型对齐 behavior_pattern_store.py。
const (
	// LearningObserved mirrors LEARNING_OBSERVED.
	LearningObserved = "observed_behavior"
	// LearningSelfReflection mirrors LEARNING_SELF_REFLECTION.
	LearningSelfReflection = "self_reflection"
)

// 行为参考文案（照抄 `_build_group_reference_text` / `_build_compact_scenario_text`）。
const (
	// BehaviorReferenceHeader mirrors reference_sections[0].
	BehaviorReferenceHeader = "以下是根据当前场景召回的过往互动经验。请结合当前上下文、人物关系和可用工具，" +
		"自然地吸收其中合适的部分，无需复现原做法或预设相同结果。"
	// BehaviorScenarioEmptyText mirrors "无可用场景画像。".
	BehaviorScenarioEmptyText = "无可用场景画像。"
	// BehaviorSelfReflectionHeader 照抄 reference_sections[2] 的固定行。
	BehaviorSelfReflectionHeader = "麦麦的过往经验：\n这些做法由麦麦在相似场景中采用过，可结合当前情况调整或组合。"
	// BehaviorObservedHeader 照抄 reference_sections[3] 的固定行。
	BehaviorObservedHeader = "其他人的互动经验：\n这些做法来自他人或群体，可以在适合麦麦当前身份、关系和情境时灵活借鉴。"
)

// BehaviorScenarioTagCluster mirrors BehaviorScenarioTagCluster.
type BehaviorScenarioTagCluster struct {
	Kind string
	Tags []string
}

// AllValues mirrors BehaviorScenarioTagCluster.all_values.
func (c BehaviorScenarioTagCluster) AllValues() []string {
	values := make([]string, 0, len(c.Tags))
	seen := map[string]bool{}
	for _, tag := range c.Tags {
		normalized := strings.Join(strings.Fields(strings.TrimSpace(tag)), " ")
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		values = append(values, normalized)
	}
	return values
}

// BehaviorScenarioProfile mirrors BehaviorScenarioProfile.
type BehaviorScenarioProfile struct {
	Summary     string
	TagClusters []BehaviorScenarioTagCluster
	Confidence  float64
}

// HasSignal mirrors BehaviorScenarioProfile.has_signal.
func (p BehaviorScenarioProfile) HasSignal() bool {
	return strings.TrimSpace(p.Summary) != "" || len(p.TagClusters) > 0
}

// TagClusterText mirrors BehaviorScenarioProfile.tag_cluster_text.
func (p BehaviorScenarioProfile) TagClusterText() string {
	values := make([]string, 0, len(p.TagClusters))
	for _, cluster := range p.TagClusters {
		values = append(values, cluster.AllValues()...)
	}
	return strings.Join(values, "、")
}

// ToPromptText mirrors BehaviorScenarioProfile.to_prompt_text.
func (p BehaviorScenarioProfile) ToPromptText() string {
	if !p.HasSignal() {
		return BehaviorScenarioEmptyText
	}
	payload := map[string]any{
		"summary":      p.Summary,
		"tag_clusters": p.domainPromptPayloads(),
		"need":         p.needPromptPayload(),
		"other_traits": p.otherTraitsPromptPayloads(),
		"confidence":   p.Confidence,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return BehaviorScenarioEmptyText
	}
	return string(raw)
}

func (p BehaviorScenarioProfile) domainPromptPayloads() []map[string]any {
	return p.promptPayloads("domain")
}

func (p BehaviorScenarioProfile) otherTraitsPromptPayloads() []map[string]any {
	return p.promptPayloads("attitude")
}

func (p BehaviorScenarioProfile) promptPayloads(kind string) []map[string]any {
	out := make([]map[string]any, 0, len(p.TagClusters))
	for _, cluster := range p.TagClusters {
		if cluster.Kind != kind {
			continue
		}
		values := cluster.AllValues()
		if len(values) == 0 {
			continue
		}
		out = append(out, map[string]any{"tag_name": values[0], "tag_aliases": values[1:]})
	}
	return out
}

// needPromptPayload mirrors `need_prompt_payload`（单个对象或 {}）。
func (p BehaviorScenarioProfile) needPromptPayload() map[string]any {
	for _, cluster := range p.TagClusters {
		if cluster.Kind != "need" {
			continue
		}
		values := cluster.AllValues()
		if len(values) == 0 {
			continue
		}
		return map[string]any{"tag_name": values[0], "tag_aliases": values[1:]}
	}
	return map[string]any{}
}

// ScenarioAgentRunner mirrors ScenarioAgentRunner（子代理执行器）。
type ScenarioAgentRunner func(ctx context.Context, systemPrompt string) (string, error)

// BehaviorScenarioAnalyzer mirrors behavior_scenario.analyze（接口占位 + 最小实现）。
//
// 完整实现（tag 归一化/泛化标签过滤/片段切分）由 P11 补齐；本骨架保证：
//   - 开关关闭时立即返回空画像，不产生任何 LLM 调用；
//   - 开关开启且注入 runner 时执行最小 JSON 解析。
type BehaviorScenarioAnalyzer struct {
	// Enabled mirrors experimental.enable_behavior_learning（默认 false）。
	Enabled bool
	// Runner 是场景分析子代理（nil 时返回空画像）。
	Runner ScenarioAgentRunner
	// Prompts 渲染 behavior_scene_analyze.prompt。
	Prompts PromptFunc
	// Constraint 是对齐 behavior_scenario.py 的任务约束文本。
	Constraint string
	// MaxTagClusters 限制 tag 簇数量（对齐 `_filter_generic_tag_clusters` 的裁剪口径）。
	MaxTagClusters int
}

// BehaviorScenarioConstraint mirrors BEHAVIOR_SCENARIO_CONSTRAINT_TEXT.
const BehaviorScenarioConstraint = "【行为表现情景分析任务约束】\n" +
	"你现在不是主 planner，不要续写聊天、不要判断是否需要回复、不要选择行为表现。\n" +
	"你只负责把当前上下文抽象成行为表现检索用的场景画像。\n" +
	"只能输出 JSON 对象，字段必须包含 summary、tag_clusters、need、other_traits、confidence；" +
	"tag_clusters 只表示领域概念，每项只能包含 tag_name、tag_aliases；" +
	"need 单独输出为包含 tag_name、tag_aliases 的对象；" +
	"other_traits 表示他人的特点和态度，输出 tag_name、tag_aliases 数组；" +
	"不要输出 kind、phase、risk、tags、name 或 cluster_key。"

// Analyze 执行一次场景分析（开关关闭时返回空画像，不调用模型）。
func (a *BehaviorScenarioAnalyzer) Analyze(ctx context.Context, contextText string) (BehaviorScenarioProfile, error) {
	if a == nil || !a.Enabled || a.Runner == nil || a.Prompts == nil {
		return BehaviorScenarioProfile{}, nil
	}
	includeContext := "true"
	prompt, err := a.Prompts("behavior_scene_analyze", map[string]string{
		"context_text":              contextText,
		"include_context_in_prompt": includeContext,
	})
	if err != nil {
		return BehaviorScenarioProfile{}, err
	}
	raw, err := a.Runner(ctx, strings.Join([]string{BehaviorScenarioConstraint, prompt}, "\n\n"))
	if err != nil {
		return BehaviorScenarioProfile{}, err
	}
	return ParseBehaviorScenarioProfile(raw, a.maxTagClusters()), nil
}

func (a *BehaviorScenarioAnalyzer) maxTagClusters() int {
	if a.MaxTagClusters > 0 {
		return a.MaxTagClusters
	}
	return 8
}

// ParseBehaviorScenarioProfile mirrors `_profile_from_mapping`（最小解析）。
func ParseBehaviorScenarioProfile(raw string, maxTagClusters int) BehaviorScenarioProfile {
	document := RepairJSONDocument(raw)
	if strings.TrimSpace(document) == "" {
		return BehaviorScenarioProfile{}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(document), &payload); err != nil {
		return BehaviorScenarioProfile{}
	}
	profile := BehaviorScenarioProfile{
		Summary: strings.TrimSpace(normalizeJSONString(payload["summary"])),
	}
	if confidence, ok := toFloat(payload["confidence"]); ok {
		profile.Confidence = confidence
	}
	for _, key := range []string{"tag_clusters", "other_traits"} {
		rawClusters, ok := payload[key].([]any)
		if !ok {
			continue
		}
		kind := "domain"
		if key == "other_traits" {
			kind = "attitude"
		}
		for _, rawCluster := range rawClusters {
			clusterMap, ok := rawCluster.(map[string]any)
			if !ok {
				continue
			}
			cluster := parseScenarioTagCluster(clusterMap, kind)
			if len(cluster.AllValues()) == 0 {
				continue
			}
			profile.TagClusters = append(profile.TagClusters, cluster)
		}
	}
	if needMap, ok := payload["need"].(map[string]any); ok {
		cluster := parseScenarioTagCluster(needMap, "need")
		if len(cluster.AllValues()) > 0 {
			profile.TagClusters = append(profile.TagClusters, cluster)
		}
	}
	if maxTagClusters <= 0 {
		maxTagClusters = 8
	}
	if len(profile.TagClusters) > maxTagClusters {
		profile.TagClusters = profile.TagClusters[:maxTagClusters]
	}
	return profile
}

func parseScenarioTagCluster(payload map[string]any, fallbackKind string) BehaviorScenarioTagCluster {
	kind := strings.TrimSpace(normalizeJSONString(payload["kind"]))
	if kind == "" {
		kind = fallbackKind
	}
	tags := make([]string, 0, 4)
	if name := strings.TrimSpace(normalizeJSONString(payload["tag_name"])); name != "" {
		tags = append(tags, name)
	}
	if aliases, ok := payload["tag_aliases"].([]any); ok {
		for _, alias := range aliases {
			if text := strings.TrimSpace(normalizeJSONString(alias)); text != "" {
				tags = append(tags, text)
			}
		}
	}
	return BehaviorScenarioTagCluster{Kind: kind, Tags: tags}
}

func toFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	}
	return 0, false
}

// BehaviorPattern mirrors behavior_pattern_store 的行为记录视图。
type BehaviorPattern struct {
	ID           int64
	LearningType string
	Action       string
	Outcome      string
	SceneCluster string
	Score        float64
	Count        int
}

// BehaviorPatternStore 是行为模式存储边界（P11 补齐实现）。
type BehaviorPatternStore interface {
	// LoadCandidates mirrors `_load_behavior_candidates`（按场景画像召回候选）。
	LoadCandidates(sessionID string, profile BehaviorScenarioProfile, maxCount int) ([]BehaviorPattern, error)
	// MarkSelected mirrors `mark_behavior_pattern_selected`（选中计数）。
	MarkSelected(id int64) error
}

// BehaviorPatternSelector mirrors BehaviorPatternSelector（召回 + 参考文案）。
type BehaviorPatternSelector struct {
	// Enabled mirrors experimental.enable_behavior_learning（默认 false）。
	Enabled bool
	// Store 是行为模式存储（nil 时返回空结果）。
	Store BehaviorPatternStore
	// Analyzer 是场景分析器（nil 时使用空画像）。
	Analyzer *BehaviorScenarioAnalyzer
	// MaxCount 是召回上限（对齐 `max(1, min(3, max_count))`）。
	MaxCount int
	Logf     func(format string, args ...any)
}

// BehaviorPatternRetrievalResult mirrors BehaviorPatternRetrievalResult.
type BehaviorPatternRetrievalResult struct {
	ReferenceText   string
	Behaviors       []BehaviorPattern
	ScenarioProfile BehaviorScenarioProfile
}

func (s *BehaviorPatternSelector) log(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *BehaviorPatternSelector) maxCount() int {
	if s.MaxCount > 0 && s.MaxCount < 3 {
		return s.MaxCount
	}
	return 3
}

// RetrieveForPlanner mirrors retrieve_for_planner（默认关闭：直接返回空结果）。
func (s *BehaviorPatternSelector) RetrieveForPlanner(
	ctx context.Context,
	sessionID string,
	contextText string,
) BehaviorPatternRetrievalResult {
	if s == nil || !s.Enabled || strings.TrimSpace(sessionID) == "" || s.Store == nil {
		return BehaviorPatternRetrievalResult{}
	}
	profile := BehaviorScenarioProfile{}
	if s.Analyzer != nil {
		analyzed, err := s.Analyzer.Analyze(ctx, contextText)
		if err != nil {
			s.log("行为表现场景分析失败，已跳过: %v", err)
			return BehaviorPatternRetrievalResult{}
		}
		profile = analyzed
	}
	candidates, err := s.Store.LoadCandidates(sessionID, profile, s.maxCount())
	if err != nil {
		s.log("行为表现召回失败，已跳过: %v", err)
		return BehaviorPatternRetrievalResult{ScenarioProfile: profile}
	}
	if len(candidates) == 0 {
		return BehaviorPatternRetrievalResult{ScenarioProfile: profile}
	}
	selected := candidates
	if len(selected) > s.maxCount() {
		selected = selected[:s.maxCount()]
	}
	for _, behavior := range selected {
		if err := s.Store.MarkSelected(behavior.ID); err != nil {
			s.log("标记行为表现选中失败: id=%d err=%v", behavior.ID, err)
		}
	}
	return BehaviorPatternRetrievalResult{
		ReferenceText:   BuildBehaviorReferenceText(selected, profile),
		Behaviors:       selected,
		ScenarioProfile: profile,
	}
}

// BuildBehaviorReferenceText mirrors `_build_group_reference_text`.
func BuildBehaviorReferenceText(behaviors []BehaviorPattern, profile BehaviorScenarioProfile) string {
	selfReflectionItems := make([]string, 0, len(behaviors))
	observedItems := make([]string, 0, len(behaviors))
	for _, behavior := range behaviors {
		action := strings.TrimSpace(behavior.Action)
		outcome := strings.TrimSpace(behavior.Outcome)
		switch behavior.LearningType {
		case LearningSelfReflection:
			selfReflectionItems = append(selfReflectionItems, fmt.Sprintf(
				"%d.\nbehavior_id：%d\n麦麦过去采用的做法：%s\n当时观察到的结果：%s",
				len(selfReflectionItems)+1, behavior.ID, action, outcome))
		case LearningObserved:
			observedItems = append(observedItems, fmt.Sprintf(
				"%d.\nbehavior_id：%d\n观察到的互动方式：%s\n观察到的后续变化：%s",
				len(observedItems)+1, behavior.ID, action, outcome))
		default:
			// 学习类型未知的候选被跳过（对齐 Python 的 warning 分支）。
		}
	}
	sections := []string{
		BehaviorReferenceHeader,
		"当前场景画像：\n" + BuildCompactScenarioText(profile),
	}
	if len(selfReflectionItems) > 0 {
		sections = append(sections, BehaviorSelfReflectionHeader+"\n"+strings.Join(selfReflectionItems, "\n"))
	}
	if len(observedItems) > 0 {
		sections = append(sections, BehaviorObservedHeader+"\n"+strings.Join(observedItems, "\n"))
	}
	if len(selfReflectionItems) == 0 && len(observedItems) == 0 {
		return ""
	}
	return strings.Join(sections, "\n\n")
}

// BuildCompactScenarioText mirrors `_build_compact_scenario_text`.
func BuildCompactScenarioText(profile BehaviorScenarioProfile) string {
	if !profile.HasSignal() {
		return BehaviorScenarioEmptyText
	}
	lines := make([]string, 0, 2)
	if strings.TrimSpace(profile.Summary) != "" {
		lines = append(lines, "场景摘要："+strings.TrimSpace(profile.Summary))
	}
	if tagText := profile.TagClusterText(); tagText != "" {
		lines = append(lines, "场景标签："+tagText)
	}
	if len(lines) == 0 {
		return BehaviorScenarioEmptyText
	}
	return strings.Join(lines, "\n")
}

// SortBehaviorPatternsByScore 按分数倒序（召回排序稳定）。
func SortBehaviorPatternsByScore(patterns []BehaviorPattern) []BehaviorPattern {
	out := append([]BehaviorPattern{}, patterns...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}
