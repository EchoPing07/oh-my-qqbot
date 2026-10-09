package learners

import (
	"context"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 实验骨架：默认关闭
// ---------------------------------------------------------------------------

func TestBehaviorSelectorDisabledByDefault(t *testing.T) {
	selector := &BehaviorPatternSelector{Store: &fakeBehaviorStore{}}
	result := selector.RetrieveForPlanner(context.Background(), "s1", "上下文")
	if result.ReferenceText != "" || len(result.Behaviors) != 0 {
		t.Fatalf("默认关闭应返回空结果: %+v", result)
	}
}

func TestBehaviorScenarioAnalyzerDisabledMakesNoLLMCall(t *testing.T) {
	called := false
	analyzer := &BehaviorScenarioAnalyzer{
		Enabled: false,
		Runner: func(context.Context, string) (string, error) {
			called = true
			return `{"summary":"不该出现"}`, nil
		},
		Prompts: fakePrompts(map[string]string{"behavior_scene_analyze": "{context_text}"}),
	}
	profile, err := analyzer.Analyze(context.Background(), "上下文")
	if err != nil || profile.HasSignal() {
		t.Fatalf("关闭时不应产出画像: %+v err=%v", profile, err)
	}
	if called {
		t.Fatalf("关闭时不应调用子代理")
	}
}

func TestBehaviorScenarioConstraintTextIsCopied(t *testing.T) {
	for _, keyword := range []string{"【行为表现情景分析任务约束】", "tag_clusters", "other_traits", "confidence"} {
		if !strings.Contains(BehaviorScenarioConstraint, keyword) {
			t.Fatalf("任务约束应照抄，缺少 %q:\n%s", keyword, BehaviorScenarioConstraint)
		}
	}
}

func TestParseBehaviorScenarioProfile(t *testing.T) {
	raw := "```json\n" + `{
		"summary": "用户在讨论技术问题",
		"tag_clusters": [{"tag_name": "技术讨论", "tag_aliases": ["代码", "技术"]}],
		"need": {"tag_name": "求助", "tag_aliases": ["需要帮助"]},
		"other_traits": [{"tag_name": "友好", "tag_aliases": ["客气"]}],
		"confidence": 0.8
	}` + "\n```"
	profile := ParseBehaviorScenarioProfile(raw, 8)
	if !profile.HasSignal() {
		t.Fatalf("应解析出画像信号: %+v", profile)
	}
	if profile.Summary != "用户在讨论技术问题" {
		t.Fatalf("summary 解析失败: %q", profile.Summary)
	}
	if len(profile.TagClusters) != 3 {
		t.Fatalf("应解析 3 个 tag 簇（domain/need/attitude）: %+v", profile.TagClusters)
	}
	kinds := map[string]bool{}
	for _, cluster := range profile.TagClusters {
		kinds[cluster.Kind] = true
	}
	if !kinds["domain"] || !kinds["need"] || !kinds["attitude"] {
		t.Fatalf("tag 簇类型应映射: %+v", kinds)
	}
	if profile.Confidence != 0.8 {
		t.Fatalf("confidence 解析失败: %v", profile.Confidence)
	}
	// 顺序照抄 `_profile_from_mapping`：tag_clusters(domain) → other_traits(attitude) → need。
	if token := profile.TagClusterText(); token != "技术讨论、代码、技术、友好、客气、求助、需要帮助" {
		t.Fatalf("tag 文本应拼接全部值: %q", token)
	}
	if !strings.Contains(profile.ToPromptText(), "tag_name") {
		t.Fatalf("prompt 文本应含 tag_name: %q", profile.ToPromptText())
	}
	if !strings.Contains(profile.ToPromptText(), `"need"`) {
		t.Fatalf("prompt 文本应含 need: %q", profile.ToPromptText())
	}

	if got := ParseBehaviorScenarioProfile("not json", 8); got.HasSignal() {
		t.Fatalf("非法 JSON 应返回空画像: %+v", got)
	}
	empty := BehaviorScenarioProfile{}
	if empty.ToPromptText() != BehaviorScenarioEmptyText {
		t.Fatalf("空画像文案应照抄: %q", empty.ToPromptText())
	}
	if BuildCompactScenarioText(empty) != BehaviorScenarioEmptyText {
		t.Fatalf("紧凑场景文案应照抄: %q", BuildCompactScenarioText(empty))
	}
}

func TestParseBehaviorScenarioProfileTruncatesTagClusters(t *testing.T) {
	clusters := make([]string, 0, 12)
	for index := 0; index < 12; index++ {
		clusters = append(clusters, `{"tag_name":"标签`+itoaSmall(index)+`"}`)
	}
	raw := `{"summary":"x","tag_clusters":[` + strings.Join(clusters, ",") + `],"confidence":0.5}`
	profile := ParseBehaviorScenarioProfile(raw, 3)
	if len(profile.TagClusters) != 3 {
		t.Fatalf("tag 簇应被裁剪到上限: %d", len(profile.TagClusters))
	}
}

func TestBuildBehaviorReferenceTextSections(t *testing.T) {
	behaviors := []BehaviorPattern{
		{ID: 1, LearningType: LearningSelfReflection, Action: "主动询问", Outcome: "对方继续聊"},
		{ID: 2, LearningType: LearningObserved, Action: "分享图片", Outcome: "气氛活跃"},
		{ID: 3, LearningType: "unknown", Action: "x", Outcome: "y"},
	}
	profile := BehaviorScenarioProfile{Summary: "技术讨论", TagClusters: []BehaviorScenarioTagCluster{
		{Kind: "domain", Tags: []string{"技术"}},
	}}
	text := BuildBehaviorReferenceText(behaviors, profile)
	if !strings.HasPrefix(text, BehaviorReferenceHeader) {
		t.Fatalf("首段文案应照抄: %q", text[:40])
	}
	if !strings.Contains(text, "场景摘要：技术讨论") || !strings.Contains(text, "场景标签：技术") {
		t.Fatalf("应含紧凑场景画像: %q", text)
	}
	if !strings.Contains(text, BehaviorSelfReflectionHeader) || !strings.Contains(text, "麦麦过去采用的做法：主动询问") {
		t.Fatalf("应含自我反思段: %q", text)
	}
	if !strings.Contains(text, BehaviorObservedHeader) || !strings.Contains(text, "观察到的互动方式：分享图片") {
		t.Fatalf("应含观察段: %q", text)
	}
	if strings.Contains(text, "unknown") {
		t.Fatalf("未知学习类型应被跳过: %q", text)
	}
	// 全部未知类型 → 无有效条目 → 返回空。
	if empty := BuildBehaviorReferenceText([]BehaviorPattern{{ID: 1, LearningType: "unknown"}}, profile); empty != "" {
		t.Fatalf("无有效条目应返回空: %q", empty)
	}
}

func TestBehaviorSelectorEnabledPath(t *testing.T) {
	store := &fakeBehaviorStore{patterns: []BehaviorPattern{
		{ID: 1, LearningType: LearningSelfReflection, Action: "主动询问", Outcome: "对方继续聊", Score: 0.9},
	}}
	selector := &BehaviorPatternSelector{Enabled: true, Store: store, MaxCount: 3}
	result := selector.RetrieveForPlanner(context.Background(), "s1", "上下文")
	if result.ReferenceText == "" {
		t.Fatalf("开启后应召回参考文案: %+v", result)
	}
	if len(result.Behaviors) != 1 || !store.marked {
		t.Fatalf("应标记选中: %+v marked=%v", result.Behaviors, store.marked)
	}
	// 缺 session_id 应跳过。
	if empty := selector.RetrieveForPlanner(context.Background(), "", "上下文"); empty.ReferenceText != "" {
		t.Fatalf("缺 session_id 应跳过: %+v", empty)
	}
	// Store 为 nil 应跳过。
	if empty := (&BehaviorPatternSelector{Enabled: true}).RetrieveForPlanner(context.Background(), "s1", "x"); empty.ReferenceText != "" {
		t.Fatalf("无存储应跳过: %+v", empty)
	}
}

type fakeBehaviorStore struct {
	patterns []BehaviorPattern
	marked   bool
}

func (f *fakeBehaviorStore) LoadCandidates(_ string, _ BehaviorScenarioProfile, _ int) ([]BehaviorPattern, error) {
	return f.patterns, nil
}

func (f *fakeBehaviorStore) MarkSelected(_ int64) error {
	f.marked = true
	return nil
}

func TestSortBehaviorPatternsByScore(t *testing.T) {
	sorted := SortBehaviorPatternsByScore([]BehaviorPattern{{ID: 1, Score: 0.1}, {ID: 2, Score: 0.9}})
	if sorted[0].ID != 2 {
		t.Fatalf("应按分数倒序: %+v", sorted)
	}
}

func TestBehaviorScenarioAnalyzerEnabledParses(t *testing.T) {
	analyzer := &BehaviorScenarioAnalyzer{
		Enabled: true,
		Runner: func(_ context.Context, prompt string) (string, error) {
			if !strings.Contains(prompt, BehaviorScenarioConstraint) {
				t.Fatalf("子代理 prompt 应带任务约束")
			}
			return `{"summary":"轻松闲聊","tag_clusters":[],"need":{},"other_traits":[],"confidence":0.4}`, nil
		},
		Prompts: fakePrompts(map[string]string{"behavior_scene_analyze": "{context_text}|{include_context_in_prompt}"}),
	}
	profile, err := analyzer.Analyze(context.Background(), "上下文")
	if err != nil {
		t.Fatalf("分析失败: %v", err)
	}
	if profile.Summary != "轻松闲聊" || profile.Confidence != 0.4 {
		t.Fatalf("画像解析不符: %+v", profile)
	}
}
