package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// heuristic
// ---------------------------------------------------------------------------

func TestHeuristicMemorySkipsWhenDisabled(t *testing.T) {
	injector := NewHeuristicMemoryInjector(DefaultHeuristicMemoryOptions())
	injector.LLM = (&fakeLLM{response: "印象"}).Call
	injector.Sessions = func(string) (*SessionIdentity, bool) {
		return &SessionIdentity{SessionID: "s1", Platform: "qq"}, true
	}
	reference, err := injector.BuildInjectionMessage(context.Background(), "s1")
	if err != nil {
		t.Fatalf("禁用时不应报错: %v", err)
	}
	if reference != "" {
		t.Fatalf("禁用时不应注入: %q", reference)
	}
}

func TestHeuristicMemoryRequiresWindowAndInterval(t *testing.T) {
	options := DefaultHeuristicMemoryOptions()
	options.Enabled = true
	options.WindowSize = 3
	injector := NewHeuristicMemoryInjector(options)
	injector.Sessions = func(string) (*SessionIdentity, bool) {
		return &SessionIdentity{SessionID: "s1", Platform: "qq", IsGroup: true}, true
	}
	injector.Counts = func(context.Context, string, *time.Time) (int, error) { return 2, nil }
	got, err := injector.BuildInjectionMessage(context.Background(), "s1")
	if err != nil || got != "" {
		t.Fatalf("消息数不足窗口应跳过: %q err=%v", got, err)
	}

	injector.Counts = func(context.Context, string, *time.Time) (int, error) { return 30, nil }
	var query MessageQuery
	injector.Messages = func(_ context.Context, q MessageQuery) ([]*message.SessionMessage, error) {
		query = q
		out := make([]*message.SessionMessage, 0, q.Limit)
		for index := 0; index < q.Limit; index++ {
			out = append(out, newFlowMessage("s1", "u"+itoa(index), "user-1", "在聊比赛", time.Now()))
		}
		return out, nil
	}
	llm := &fakeLLM{response: "大家正在聊球赛"}
	injector.LLM = llm.Call
	injector.Prompts = fakePrompts(map[string]string{
		"heuristic_memory_impression": "{chat_identity}|{message_window}",
	})
	injector.Kernel = &fakeSearcher{hits: []contracts.MemoryHit{{
		Kind: "paragraph", ID: "h1", Text: "上次聊过球赛，阿强买票", ChatID: "s1",
	}}}
	got, err = injector.BuildInjectionMessage(context.Background(), "s1")
	if err != nil {
		t.Fatalf("召回失败: %v", err)
	}
	if query.Limit != 3 || query.LimitMode != "latest" {
		t.Fatalf("应按窗口大小取最近消息: %+v", query)
	}
	if !strings.HasPrefix(got, HeuristicMemoryReferenceMarker) {
		t.Fatalf("应带【启发式记忆-内部参考】标记: %q", firstLine(got))
	}
	if !strings.Contains(got, "阿强买票") {
		t.Fatalf("应包含命中内容: %q", got)
	}

	// TTL 缓存：第二次调用不应再次检索/调用模型。
	before := injector.SearchCalls
	got2, err := injector.BuildInjectionMessage(context.Background(), "s1")
	if err != nil || got2 != got {
		t.Fatalf("缓存期内应返回同一参考: %q err=%v", got2, err)
	}
	if injector.SearchCalls != before {
		t.Fatalf("缓存期内不应再次检索: %d", injector.SearchCalls)
	}
}

func TestHeuristicMemoryCanTriggerRules(t *testing.T) {
	// last_message_count = 0 → 首次允许
	if !CanTriggerHeuristicRecall(HeuristicMemoryRecallState{}, 100, 1000, 180, 60) {
		t.Fatalf("首轮应允许触发")
	}
	// 间隔不足
	state := HeuristicMemoryRecallState{LastMessageCount: 100, LastRecallAt: 1000}
	if CanTriggerHeuristicRecall(state, 200, 1100, 180, 60) {
		t.Fatalf("未达最小间隔不应触发")
	}
	// 增量不足
	if CanTriggerHeuristicRecall(state, 120, 1300, 180, 60) {
		t.Fatalf("新增消息不足不应触发")
	}
	// 满足间隔 + 增量
	if !CanTriggerHeuristicRecall(state, 200, 1300, 180, 60) {
		t.Fatalf("满足间隔与增量应触发")
	}
}

func TestHeuristicHitScopeFiltering(t *testing.T) {
	injector := NewHeuristicMemoryInjector(HeuristicMemoryOptions{Limit: 3})
	contextData := HeuristicMemoryContext{
		Session:         &SessionIdentity{SessionID: "s1", Platform: "qq", IsGroup: true},
		ActivePersonIDs: map[string]bool{"qq:user-1": true},
	}
	// 同会话 chat_summary 命中允许。
	if !injector.IsHitAllowed(contracts.MemoryHit{Kind: "paragraph", ChatID: "s1"}, contextData) {
		t.Fatalf("同会话命中应允许")
	}
	// 其他会话命中默认拒绝。
	if injector.IsHitAllowed(contracts.MemoryHit{Kind: "paragraph", ChatID: "s2"}, contextData) {
		t.Fatalf("跨会话命中默认应拒绝")
	}
	// 无 chat_id 的命中拒绝。
	if injector.IsHitAllowed(contracts.MemoryHit{Kind: "paragraph"}, contextData) {
		t.Fatalf("无来源命中应拒绝")
	}
	// episode 只接受本会话 chat_summary 来源。
	if injector.IsHitAllowed(contracts.MemoryHit{Kind: "episode", ChatID: "s1"}, contextData) {
		t.Fatalf("episode 需要显式 chat_summary 来源")
	}
}

func TestFormatHeuristicMemoryReferenceTruncates(t *testing.T) {
	hits := []contracts.MemoryHit{
		{Text: strings.Repeat("长", 300)},
		{Text: "短"},
	}
	got := FormatHeuristicMemoryReference(hits, 200)
	if len([]rune(got)) > 203 {
		t.Fatalf("应截断到 max_chars: %d", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("截断后应带省略号: %q", got[maxInt(0, len(got)-10):])
	}
	if empty := FormatHeuristicMemoryReference(nil, 200); empty != "" {
		t.Fatalf("无命中应返回空: %q", empty)
	}
}

func TestHeuristicMessageWindowFormat(t *testing.T) {
	messages := []*message.SessionMessage{
		newFlowMessage("s1", "u1", "user-1", "第一行\n第二行", time.Now()),
	}
	got := FormatHeuristicMessageWindow(messages)
	if !strings.Contains(got, "- 阿强: 第一行 第二行") {
		t.Fatalf("窗口格式应压平换行: %q", got)
	}
	if empty := FormatHeuristicMessageWindow(nil); empty != "no text messages available" {
		t.Fatalf("空窗口文案应照抄: %q", empty)
	}
}

func TestHeuristicChatIdentityFormat(t *testing.T) {
	identity := &SessionIdentity{
		SessionID: "s1", Platform: "qq", GroupID: "g1", IsGroup: true, DisplayName: "测试群",
	}
	got := FormatHeuristicChatIdentity(identity)
	want := "chat_type: group\ndisplay_name: 测试群\nplatform: qq\ngroup_id: g1"
	if got != want {
		t.Fatalf("聊天流身份文案应照抄:\n got=%q\nwant=%q", got, want)
	}
}

// fakeSearcher 是 MemorySearcher 假实现。
type fakeSearcher struct {
	hits  []contracts.MemoryHit
	calls int
}

func (f *fakeSearcher) SearchMemory(_ context.Context, _ string, _ string, _ string, _ int) ([]contracts.MemoryHit, error) {
	f.calls++
	return f.hits, nil
}

// ---------------------------------------------------------------------------
// profile
// ---------------------------------------------------------------------------

func TestPersonProfileInjectorCollectsCandidatesInOrder(t *testing.T) {
	injector := NewPersonProfileInjector()
	injector.ResolvePersonID = func(platform string, userID string, personName string) string {
		switch {
		case userID != "":
			return platform + ":" + userID
		case personName != "":
			return "name:" + personName
		}
		return ""
	}
	injector.IsBotSelf = testIsBotSelf

	anchor := newFlowMessage("s1", "u1", "user-1", "看看这个", time.Now())
	anchor.MessageInfo.GroupInfo = &message.GroupInfo{GroupID: "g1", GroupName: "群"}
	anchor.MessageInfo.UserInfo = message.NewUserInfo("user-1", "阿强", "")
	anchor.RawMessage = message.NewMessageSequence(
		message.NewTextComponent("看看这个"),
		message.NewAtComponent("user-2"),
	)

	pending := []*message.SessionMessage{
		newFlowMessage("s1", "u2", "user-3", "在吗", time.Now()),
		anchor,
	}
	candidates := injector.CollectCandidates(anchor, pending)
	if len(candidates) < 2 {
		t.Fatalf("应收集到发言者 + at 候选: %+v", candidates)
	}
	if candidates[0].PersonID != "qq:user-1" || candidates[0].Source != "recent_speaker" {
		t.Fatalf("首个候选应为锚点发言者: %+v", candidates[0])
	}
	if candidates[1].PersonID != "qq:user-2" || candidates[1].Source != "at_user" {
		t.Fatalf("第二个候选应为被 @ 用户: %+v", candidates[1])
	}
}

func TestPersonProfileInjectorPrivateChatOnlySender(t *testing.T) {
	injector := NewPersonProfileInjector()
	injector.ResolvePersonID = func(platform string, userID string, _ string) string { return platform + ":" + userID }
	injector.IsBotSelf = testIsBotSelf
	anchor := newFlowMessage("p1", "u1", "user-9", "私聊内容", time.Now())
	candidates := injector.CollectCandidates(anchor, nil)
	if len(candidates) != 1 || candidates[0].Source != "private_current_user" {
		t.Fatalf("私聊应只收集当前用户: %+v", candidates)
	}
}

func TestPersonProfileInjectorBuildsReferenceBlock(t *testing.T) {
	injector := NewPersonProfileInjector()
	injector.ResolvePersonID = func(platform string, userID string, _ string) string { return platform + ":" + userID }
	injector.IsBotSelf = testIsBotSelf
	injector.Kernel = &fakeProfileReader{profile: &contracts.PersonProfile{
		PersonID: "qq:user-1",
		Name:     "阿强",
		Facts: []contracts.ProfileFact{
			{Category: "identity", Content: "是群里的老成员"},
			{Category: "stable_facts", Content: "对青霉素过敏"},
			{Category: "recent_interaction", Content: "昨天问过球赛"},
		},
	}}
	anchor := newFlowMessage("s1", "u1", "user-1", "今天怎么样", time.Now())
	messages, err := injector.BuildInjectionMessages(context.Background(), anchor, nil)
	if err != nil {
		t.Fatalf("画像注入失败: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("应产出 1 条注入消息: %d", len(messages))
	}
	block := messages[0]
	if !strings.HasPrefix(block, ProfileReferenceMarker) {
		t.Fatalf("应以【人物画像-内部参考】开头: %q", firstLine(block))
	}
	if !strings.Contains(block, "阿强：") || !strings.Contains(block, "对青霉素过敏") {
		t.Fatalf("应包含人物名与画像内容: %q", block)
	}
	if !strings.Contains(block, ProfileReferenceFooter) {
		t.Fatalf("应包含结尾提示: %q", block)
	}
}

func TestPersonProfileInjectorDisabled(t *testing.T) {
	injector := NewPersonProfileInjector()
	injector.Enabled = false
	injector.ResolvePersonID = func(platform string, userID string, _ string) string { return platform + ":" + userID }
	anchor := newFlowMessage("s1", "u1", "user-1", "内容", time.Now())
	messages, err := injector.BuildInjectionMessages(context.Background(), anchor, nil)
	if err != nil || len(messages) != 0 {
		t.Fatalf("关闭时不应注入: %v %v", messages, err)
	}
}

type fakeProfileReader struct {
	profile *contracts.PersonProfile
}

func (f *fakeProfileReader) GetPersonProfile(_ context.Context, _ string) (*contracts.PersonProfile, error) {
	return f.profile, nil
}

func TestStructuredProfileRoundTrip(t *testing.T) {
	text := BuildStructuredProfileText(StructuredProfileInput{
		PersonID:           "qq:user-1",
		PrimaryName:        "阿强",
		Aliases:            []string{"强哥", "强哥"},
		StableFacts:        []string{"对青霉素过敏", "- 对青霉素过敏"},
		RecentInteractions: []string{"昨天问过球赛", "今天问过电影", "前天问过天气"},
		UncertainNotes:     []string{"可能住在东边"},
	})
	if !IsStructuredProfileText(text) {
		t.Fatalf("生成的画像文本应满足结构化协议:\n%s", text)
	}
	if !strings.Contains(text, "别名: 强哥、强哥") {
		t.Fatalf("别名行应照抄 Python 的 join 拼接: %s", text)
	}
	if strings.Count(text, "对青霉素过敏") != 1 {
		t.Fatalf("事实 bullet 应去重: %s", text)
	}
	sections := ParseProfileSections(text)
	if bullets := countBullets(sections["近期互动"]); bullets != 3 {
		t.Fatalf("近期互动应保留 3 条: %v", sections["近期互动"])
	}
	injection := BuildProfileInjectionText(text, nil, false)
	if strings.Contains(injection, "## 维护备注") {
		t.Fatalf("注入文本不应包含维护备注: %q", injection)
	}
	if strings.Count(injection, "## 近期互动") != 1 || !strings.Contains(injection, "昨天问过球赛") {
		t.Fatalf("注入文本应包含近期互动（最多 2 条）: %q", injection)
	}
	if strings.Contains(injection, "前天问过天气") {
		t.Fatalf("近期互动应限制为最近 2 条: %q", injection)
	}
}

// countBullets 统计段落里的有效 bullet（解析会保留分隔空行）。
func countBullets(lines []string) int {
	count := 0
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "- ") {
			count++
		}
	}
	return count
}

func TestReplaceProfileSection(t *testing.T) {
	text := BuildStructuredProfileText(StructuredProfileInput{
		PersonID:    "qq:user-1",
		PrimaryName: "阿强",
		StableFacts: []string{"旧事实"},
	})
	updated, err := ReplaceProfileSection(text, "稳定了解", []string{"新事实"})
	if err != nil {
		t.Fatalf("替换段落失败: %v", err)
	}
	if strings.Contains(updated, "旧事实") || !strings.Contains(updated, "新事实") {
		t.Fatalf("段落应被替换: %s", updated)
	}
	if _, err := ReplaceProfileSection(text, "不存在的段落", nil); err == nil {
		t.Fatalf("未知段落应报错")
	}
}

func TestToMemorySearchHitsMapping(t *testing.T) {
	hits := ToMemorySearchHits([]contracts.MemoryHit{{
		Kind: "paragraph", ID: "h1", Text: "内容", Score: 0.9, ChatID: "s1", Similarity: 0.88,
	}})
	if len(hits) != 1 || hits[0].Content != "内容" || hits[0].Type != "paragraph" {
		t.Fatalf("命中视图应映射: %+v", hits)
	}
	if hits[0].Metadata["chat_id"] != "s1" {
		t.Fatalf("元数据应带 chat_id: %+v", hits[0].Metadata)
	}
}

// ---------------------------------------------------------------------------
// service
// ---------------------------------------------------------------------------

func TestMemoryServiceSearchAndText(t *testing.T) {
	searcher := &fakeSearcher{hits: []contracts.MemoryHit{
		{Kind: "paragraph", ID: "h1", Text: "阿强对青霉素过敏", Score: 0.9, ChatID: "s1"},
		{Kind: "paragraph", ID: "h2", Text: "阿强喜欢打球", Score: 0.8, ChatID: "s1"},
	}}
	service := NewMemoryService(KernelSurface{Searcher: searcher})
	result := service.Search(context.Background(), "阿强", SearchOptions{ChatID: "s1", Limit: 2})
	if !result.Success || len(result.Hits) != 2 {
		t.Fatalf("检索应成功: %+v", result)
	}
	text := SearchText(result, 2, 160)
	if !strings.Contains(text, "1. 阿强对青霉素过敏") {
		t.Fatalf("to_text 格式应照抄: %q", text)
	}

	empty := service.Search(context.Background(), "  ", SearchOptions{})
	if empty.Success || len(empty.Hits) != 0 {
		t.Fatalf("空 query 应返回空结果: %+v", empty)
	}
}

func TestMemoryServiceSearchWithoutKernel(t *testing.T) {
	service := NewMemoryService(KernelSurface{})
	result := service.Search(context.Background(), "查询", SearchOptions{})
	if result.Success || result.Error == "" {
		t.Fatalf("内核不可用应报错: %+v", result)
	}
}

func TestMemoryServiceIngestSummary(t *testing.T) {
	kernel := &fakeSummaryIngester{}
	service := NewMemoryService(KernelSurface{Summaries: kernel})
	result := service.IngestSummary(context.Background(), IngestSummaryOptions{
		ChatID: "s1", Text: "窗口摘要", ExternalID: "chat_summary:s1:10",
	}, nil)
	if !result.Success || result.Summary != "窗口摘要" {
		t.Fatalf("摘要写入应成功: %+v", result)
	}
	if kernel.calls != 1 || kernel.summary != "窗口摘要" {
		t.Fatalf("内核应收到摘요: %+v", kernel)
	}
}

func TestMemoryServiceIngestSummaryGeneratesFromChat(t *testing.T) {
	kernel := &fakeSummaryIngester{}
	service := NewMemoryService(KernelSurface{Summaries: kernel})
	repository := &fakeRepository{}
	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.Local)
	repository.add(newFlowMessage("s1", "u1", "user-1", "增量事实", base))
	generator := &ChatSummaryGenerator{
		LLM:      (&fakeLLM{response: `{"summary":"窗口增量摘要"}`}).Call,
		Prompts:  fakePrompts(map[string]string{"a_memorix_chat_summary": "{chat_history}"}),
		Messages: repository.Query,
	}
	result := service.IngestSummary(context.Background(), IngestSummaryOptions{
		ChatID:   "s1",
		Metadata: map[string]any{"generate_from_chat": true, "context_length": 10},
		TimeEnd:  &base,
	}, generator)
	if !result.Success || result.Summary != "窗口增量摘要" {
		t.Fatalf("应从聊天流生成摘要: %+v", result)
	}
}

func TestMemoryServiceGetPersonProfile(t *testing.T) {
	reader := &fakeProfileReader{profile: &contracts.PersonProfile{
		PersonID: "qq:user-1",
		Name:     "阿强",
		Facts: []contracts.ProfileFact{
			{ID: "f1", Category: "stable_facts", Content: "对青霉素过敏", Confidence: 1.0},
			{ID: "f2", Category: "stable_facts", Content: "喜欢打球", Confidence: 0.9},
		},
	}}
	service := NewMemoryService(KernelSurface{Profiles: reader})
	result := service.GetPersonProfile(context.Background(), "qq:user-1", 1)
	if len(result.Traits) != 1 || result.Traits[0] != "对青霉素过敏" {
		t.Fatalf("limit 应生效: %+v", result)
	}
	if len(result.Evidence) != 1 || result.Evidence[0]["fact_id"] != "f1" {
		t.Fatalf("证据应携带事实 id: %+v", result.Evidence)
	}

	if _, err := service.QueryPersonProfile(context.Background(), "", "", 5); err == nil {
		t.Fatalf("缺 person_id 与 person_name 应报错")
	}
	queried, err := service.QueryPersonProfile(context.Background(), "qq:user-1", "", 5)
	if err != nil || len(queried.Traits) != 2 {
		t.Fatalf("画像查询应成功: %+v err=%v", queried, err)
	}
}

func TestMemoryServiceQueryMemoryDowngradesTimeMode(t *testing.T) {
	searcher := &fakeSearcher{}
	service := NewMemoryService(KernelSurface{Searcher: searcher})
	result := service.QueryMemory(context.Background(), QueryMemoryRequest{
		Query: "上周的会议", Mode: "time", ChatID: "s1", Limit: 5,
	})
	if result.EffectiveMode != "search" {
		t.Fatalf("time 模式缺时间条件应降级为 search: %q", result.EffectiveMode)
	}
	withTime := time.Now()
	result = service.QueryMemory(context.Background(), QueryMemoryRequest{
		Query: "上周的会议", Mode: "time", ChatID: "s1", Limit: 5, TimeStart: &withTime,
	})
	if result.EffectiveMode != "time" {
		t.Fatalf("有时间条件应保留 time 模式: %q", result.EffectiveMode)
	}
}

func TestMemoryServiceIngestImageMemory(t *testing.T) {
	kernel := &fakeKernel{}
	service := NewMemoryService(KernelSurface{Images: kernel})
	result := service.IngestImageMemory(context.Background(), "s1", "一只猫", "hash-1", time.Now())
	if !result.Success || kernel.ImageCount() != 1 {
		t.Fatalf("图片记忆写入应成功: %+v", result)
	}
	if empty := service.IngestImageMemory(context.Background(), "s1", "", "", time.Now()); empty.Success {
		t.Fatalf("空描述应跳过")
	}
}

func TestWritebackPersonFactsMarksVerification(t *testing.T) {
	kernel := &fakeKernel{}
	written, err := WritebackPersonFacts(context.Background(), kernel, "qq:user-1", "阿强", []VerifiedPersonFact{
		{Fact: "对青霉素过敏", Verified: true},
		{Fact: "可能住在东边", Verified: false},
	})
	if err != nil || written != 2 {
		t.Fatalf("写回应成功: written=%d err=%v", written, err)
	}
	if kernel.facts[0].Confidence != 1.0 || kernel.facts[1].Confidence != 0.5 {
		t.Fatalf("校验标记应映射置信度: %+v", kernel.facts)
	}
	if _, err := WritebackPersonFacts(context.Background(), nil, "x", "y", nil); err != nil {
		t.Fatalf("writer 为 nil 不应报错: %v", err)
	}
}

func TestParseFactItemsCompat(t *testing.T) {
	items := ParseFactItems(`[{"fact":"阿强喜欢打游戏","evidence_message_id":"m1","evidence_quote":"我喜欢打游戏"}, "太短", "阿强喜欢打游戏"]`)
	if len(items) != 1 {
		t.Fatalf("应去重并过滤短事实: %+v", items)
	}
	if items[0].EvidenceMessageID != "m1" {
		t.Fatalf("应保留证据消息 id: %+v", items[0])
	}
	if got := ParseFactItems("not json"); got != nil {
		t.Fatalf("非法 JSON 应返回 nil: %+v", got)
	}
}

// TestHeuristicHitAllowedWithResolvedSource 验证来源反查路径（对齐
// `_is_hit_allowed(resolved_source=...)`）。
func TestHeuristicHitAllowedWithResolvedSource(t *testing.T) {
	injector := NewHeuristicMemoryInjector(HeuristicMemoryOptions{Limit: 3})
	contextData := HeuristicMemoryContext{
		Session:         &SessionIdentity{SessionID: "s1", Platform: "qq", IsGroup: true},
		ActivePersonIDs: map[string]bool{"qq:user-1": true},
	}

	// 命中自身没有 chat_id；反查得到 chat_summary:s1 → 允许。
	if !injector.IsHitAllowedWithSource(contracts.MemoryHit{Kind: "paragraph", ID: "h1"}, contextData,
		HitSource{Source: "chat_summary:s1", SourceType: "chat_summary", ChatID: "s1"}) {
		t.Fatalf("反查为本会话摘要应放行")
	}
	// 反查为其他会话 → 拒绝。
	if injector.IsHitAllowedWithSource(contracts.MemoryHit{Kind: "paragraph", ID: "h1"}, contextData,
		HitSource{Source: "chat_summary:s2", SourceType: "chat_summary", ChatID: "s2"}) {
		t.Fatalf("反查为其他会话应拒绝")
	}
	// 反查为活跃人物的事实 → 允许。
	if !injector.IsHitAllowedWithSource(contracts.MemoryHit{Kind: "paragraph", ID: "h2"}, contextData,
		HitSource{Source: "person_fact:qq:user-1", SourceType: "person_fact", PersonID: "qq:user-1"}) {
		t.Fatalf("活跃人物事实应放行")
	}
	// 非活跃人物的事实 → 拒绝。
	if injector.IsHitAllowedWithSource(contracts.MemoryHit{Kind: "paragraph", ID: "h3"}, contextData,
		HitSource{Source: "person_fact:qq:user-9", SourceType: "person_fact", PersonID: "qq:user-9"}) {
		t.Fatalf("非活跃人物事实应拒绝")
	}
	// 未反查且无 chat_id → 拒绝（默认关闭跨会话）。
	if injector.IsHitAllowed(contracts.MemoryHit{Kind: "paragraph", ID: "h4"}, contextData) {
		t.Fatalf("无来源命中应拒绝")
	}
}

// TestHeuristicResolverIsUsed 验证 Resolver 只对缺少 scope 提示的命中调用。
func TestHeuristicResolverIsUsed(t *testing.T) {
	options := DefaultHeuristicMemoryOptions()
	options.Enabled = true
	options.WindowSize = 1
	options.MinNewMessages = 1
	injector := NewHeuristicMemoryInjector(options)
	injector.Sessions = func(string) (*SessionIdentity, bool) {
		return &SessionIdentity{SessionID: "s1", Platform: "qq", IsGroup: true}, true
	}
	injector.Counts = func(context.Context, string, *time.Time) (int, error) { return 10, nil }
	injector.Messages = func(_ context.Context, _ MessageQuery) ([]*message.SessionMessage, error) {
		return []*message.SessionMessage{newFlowMessage("s1", "u1", "user-1", "在聊比赛", time.Now())}, nil
	}
	injector.LLM = (&fakeLLM{response: "印象"}).Call
	injector.Prompts = fakePrompts(map[string]string{"heuristic_memory_impression": "{chat_identity}{message_window}"})
	injector.Kernel = &fakeSearcher{hits: []contracts.MemoryHit{
		{Kind: "paragraph", ID: "needs-lookup", Text: "需要反查的来源"},
		{Kind: "paragraph", ID: "with-chat", Text: "自带 chat_id", ChatID: "s1"},
	}}
	var requested []string
	injector.Resolver = func(_ context.Context, ids []string) map[string]HitSource {
		requested = append(requested, ids...)
		return map[string]HitSource{"needs-lookup": {Source: "chat_summary:s1", SourceType: "chat_summary", ChatID: "s1"}}
	}
	got, err := injector.BuildInjectionMessage(context.Background(), "s1")
	if err != nil {
		t.Fatalf("召回失败: %v", err)
	}
	if len(requested) != 1 || requested[0] != "needs-lookup" {
		t.Fatalf("只应反查缺少 scope 提示的命中: %v", requested)
	}
	if !strings.Contains(got, "需要反查的来源") || !strings.Contains(got, "自带 chat_id") {
		t.Fatalf("两条命中都应出现在参考中: %q", got)
	}
}
