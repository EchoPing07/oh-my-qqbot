package learners

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// fakeJargonStore 是内存 jargons 表。
type fakeJargonStore struct {
	mu      sync.Mutex
	records []JargonRecord
	nextID  int64
}

func (s *fakeJargonStore) ListJargons() ([]JargonRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]JargonRecord{}, s.records...), nil
}

func (s *fakeJargonStore) GetByContent(content string) (*JargonRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range s.records {
		if record.Content == content {
			copied := record
			return &copied, nil
		}
	}
	return nil, nil
}

func (s *fakeJargonStore) Insert(record JargonRecord) (JargonRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	record.ID = s.nextID
	s.records = append(s.records, record)
	return record, nil
}

func (s *fakeJargonStore) Update(record JargonRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.records {
		if s.records[index].ID == record.ID {
			s.records[index] = record
			return nil
		}
	}
	s.records = append(s.records, record)
	return nil
}

func (s *fakeJargonStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func (s *fakeJargonStore) at(index int) JargonRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records[index]
}

// ---------------------------------------------------------------------------
// 分词与词频（对齐 message_word_frequency_service）
// ---------------------------------------------------------------------------

func TestRemoveTokenizationNoise(t *testing.T) {
	got := RemoveTokenizationNoise("看 https://example.com/x 和 www.a.com 还有 [CQ:at,qq=1] @某人 内容")
	if strings.Contains(got, "https") || strings.Contains(got, "www.a.com") || strings.Contains(got, "[CQ:") || strings.Contains(got, "@某人") {
		t.Fatalf("URL/CQ/@ 噪声应被移除: %q", got)
	}
	if !strings.Contains(got, "内容") {
		t.Fatalf("正文应保留: %q", got)
	}
}

func TestTokenizeMeaningfulTermsStopWordsAndTechTerms(t *testing.T) {
	terms := TokenizeMeaningfulTerms("这个是 CPU 和 GPU 的问题吗", nil)
	if containsString(terms, "这个") || containsString(terms, "问题") {
		t.Fatalf("停用词应被过滤: %v", terms)
	}
	if !containsString(terms, "cpu") || !containsString(terms, "gpu") {
		t.Fatalf("技术词应被识别（小写化）: %v", terms)
	}
	if terms := TokenizeMeaningfulTerms("12345 ... ====", nil); len(terms) != 0 {
		t.Fatalf("纯数字/标点应被过滤: %v", terms)
	}
}

func TestBuildMessageWordFrequencyTerms(t *testing.T) {
	texts := []string{
		"这个 yyds 真的绝了",
		"yyds yyds 好用",
		"xswl 太搞笑了",
	}
	items := BuildMessageWordFrequencyTerms(texts, WordFrequencyOptions{Limit: 20, MinCount: 2})
	if len(items) == 0 {
		t.Fatalf("应统计出词频项")
	}
	found := false
	for _, item := range items {
		if item.Term == "yyds" {
			found = true
			if item.Count != 3 {
				t.Fatalf("yyds 应出现 3 次: %+v", item)
			}
			if item.MessageCount != 2 {
				t.Fatalf("yyds 应覆盖 2 条消息: %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("应统计到 yyds: %+v", items)
	}
	// 排序键：(-count, -message_count, -len, term)
	for index := 1; index < len(items); index++ {
		if items[index-1].Count < items[index].Count {
			t.Fatalf("结果应按频次倒序: %+v", items)
		}
	}
}

func TestMergeHighFrequencyTermsRanking(t *testing.T) {
	existing := []HighFrequencyTerm{
		{Term: "yyds", OccurrenceCount: 2, MessageCount: 2},
		{Term: "xswl", OccurrenceCount: 5, MessageCount: 3},
	}
	terms := []WordFrequencyItem{
		{Term: "YYDS", Count: 4, MessageCount: 2},
		{Term: "新词", Count: 1, MessageCount: 1},
	}
	merged := MergeHighFrequencyTerms(existing, terms, 1000)
	if len(merged) != 3 {
		t.Fatalf("应按 term 归一化合并: %+v", merged)
	}
	if merged[0].Term != "YYDS" || merged[0].OccurrenceCount != 6 || merged[0].MessageCount != 4 {
		t.Fatalf("命中项应合并计数并保留最新写法: %+v", merged[0])
	}
	if merged[0].Rank != 1 {
		t.Fatalf("rank 应重排: %+v", merged)
	}
	if merged[1].Term != "xswl" || merged[2].Term != "新词" {
		t.Fatalf("排序应为 count desc: %+v", merged)
	}
	total := merged[0].Frequency + merged[1].Frequency + merged[2].Frequency
	if total < 0.999 || total > 1.001 {
		t.Fatalf("频率应归一化: %.6f", total)
	}
	// max_terms 截断。
	if got := MergeHighFrequencyTerms(existing, terms, 2); len(got) != 2 {
		t.Fatalf("max_terms 应截断: %d", len(got))
	}
}

func TestIterPhrases(t *testing.T) {
	phrases := IterPhrases([]string{"今天", "天气", "不错", "啊"}, 3)
	if len(phrases) == 0 {
		t.Fatalf("应产出短语")
	}
	// 纯中文 token 直接拼接；CJK 数 >= 4 才保留（对齐 _is_meaningful_phrase）。
	if !containsString(phrases, "今天天气") {
		t.Fatalf("应产出 2-gram 短语: %v", phrases)
	}
	if !containsString(phrases, "今天天气不错") {
		t.Fatalf("应产出 3-gram 短语: %v", phrases)
	}
	// CJK 少于 4 个的短语（如「这个 yyds」）应被过滤。
	mixed := IterPhrases([]string{"这个", "yyds", "真的", "强"}, 3)
	if containsString(mixed, "这个 yyds") {
		t.Fatalf("CJK < 4 的混合短语应被过滤: %v", mixed)
	}
	if !containsString(mixed, "这个 yyds 真的") {
		t.Fatalf("含拉丁 token 的短语应保留: %v", mixed)
	}
	// 相同 token 组成的短语（set 大小 1）应被过滤。
	if got := IterPhrases([]string{"哈哈", "哈哈"}, 2); len(got) != 0 {
		t.Fatalf("重复 token 短语应被过滤: %v", got)
	}
}

// ---------------------------------------------------------------------------
// 共现挖掘
// ---------------------------------------------------------------------------

func TestMineCooccurrenceCandidates(t *testing.T) {
	texts := []string{
		"yyds 这个游戏真的好玩",
		"yyds 你也来玩这个游戏",
		"yyds 一起开黑",
		"xswl 太好笑了",
	}
	candidates := MineCooccurrenceCandidates(texts, CooccurrenceOptions{
		Limit: 20, MinCount: 2, MinDocs: 2, MaxAssociations: 3,
	})
	if len(candidates) == 0 {
		t.Fatalf("应挖出共现候选")
	}
	top := candidates[0]
	if top.Term != "yyds" {
		t.Fatalf("覆盖消息数最多的应为 yyds: %+v", top)
	}
	if top.Count != 3 || top.MessageCount != 3 {
		t.Fatalf("yyds 统计应为 3/3: %+v", top)
	}
	if len(top.CooccurrenceWith) == 0 {
		t.Fatalf("应计算同现词: %+v", top)
	}
	for _, candidate := range candidates {
		if candidate.MessageCount < 2 {
			t.Fatalf("MinDocs=2 应过滤低频项: %+v", candidate)
		}
	}
	// 排序：消息覆盖数优先。
	for index := 1; index < len(candidates); index++ {
		if candidates[index-1].Score < candidates[index].Score {
			t.Fatalf("结果应按共现分数倒序: %+v", candidates)
		}
	}
	if got := MineCooccurrenceCandidates(nil, DefaultCooccurrenceOptions()); got != nil {
		t.Fatalf("空输入应返回 nil: %v", got)
	}
}

func TestFormatCooccurrenceSummary(t *testing.T) {
	summary := FormatCooccurrenceSummary([]CooccurrenceCandidate{
		{Term: "yyds", Count: 3, MessageCount: 3, CooccurrenceWith: []string{"游戏"}},
	}, 5)
	if !strings.Contains(summary, "yyds") || !strings.Contains(summary, "同现=游戏") {
		t.Fatalf("摘要格式不符: %q", summary)
	}
}

// ---------------------------------------------------------------------------
// 上下文匹配（机械匹配 + 注入）
// ---------------------------------------------------------------------------

func TestMatchJargonsForContextScoresAndOrder(t *testing.T) {
	candidates := []JargonCandidate{
		{Content: "yyds", Meaning: "永远的神", Count: 10},
		{Content: "xswl", Meaning: "笑死我了", Count: 5},
		{Content: "绝绝子", Meaning: "非常厉害", Count: 3},
	}
	texts := []string{"yyds 真的 绝绝子", "xswl"}
	matches := MatchJargonsForContext(texts, candidates, JargonMatchOptions{Use: true, Limit: 10})
	if len(matches) != 3 {
		t.Fatalf("三条都应命中: %+v", matches)
	}
	// score = count - first_message_index*0.01 → yyds(10) > xswl(4.99) > 绝绝子(3)。
	if matches[0].Content != "yyds" || matches[1].Content != "xswl" || matches[2].Content != "绝绝子" {
		t.Fatalf("应按 (score, 首次出现位置) 排序: %+v", matches)
	}
	if matches[0].Score <= matches[1].Score {
		t.Fatalf("count 高的应分更高: %+v", matches)
	}
	if matches[1].FirstMessageIndex != 1 {
		t.Fatalf("xswl 应记录在第二条消息命中: %+v", matches[1])
	}

	// limit 生效。
	limited := MatchJargonsForContext(texts, candidates, JargonMatchOptions{Use: true, Limit: 2})
	if len(limited) != 2 {
		t.Fatalf("limit 应生效: %d", len(limited))
	}
	// use=false 不匹配。
	if got := MatchJargonsForContext(texts, candidates, JargonMatchOptions{Use: false}); len(got) != 0 {
		t.Fatalf("use=false 应跳过: %v", got)
	}
	// 大小写不敏感。
	upper := MatchJargonsForContext([]string{"YYDS 太强"}, candidates, JargonMatchOptions{Use: true, Limit: 10})
	if len(upper) != 1 || upper[0].Content != "yyds" {
		t.Fatalf("匹配应大小写不敏感: %+v", upper)
	}
}

func TestCalculateJargonMatchScoreGolden(t *testing.T) {
	base := CalculateJargonMatchScore(5, 1, 0, 0, false)
	if base != 4.99 {
		t.Fatalf("基础分应为 count - index*0.01: %.4f", base)
	}
	withHighFrequency := CalculateJargonMatchScore(5, 1, 3, 40, true)
	// 1000 + 40*2 + (100-3) + 5 - 0.01 = 1181.99
	if withHighFrequency != 1181.99 {
		t.Fatalf("高频词加权应照抄: %.4f", withHighFrequency)
	}
}

func TestFilterJargonCandidatesScope(t *testing.T) {
	records := []JargonRecord{
		{ID: 1, Content: "a", Meaning: "含义a", IsJargon: true, Count: 5, SessionIDDict: `{"s1":3}`},
		{ID: 2, Content: "b", Meaning: "含义b", IsJargon: true, Count: 4, SessionIDDict: `{"s2":1}`},
		{ID: 3, Content: "c", Meaning: "含义c", IsJargon: true, IsGlobal: true, Count: 3, SessionIDDict: `{}`},
		{ID: 4, Content: "d", Meaning: "", IsJargon: true, Count: 2, SessionIDDict: `{"s1":1}`},
		{ID: 5, Content: "e", Meaning: "含义e", IsJargon: false, Count: 2, SessionIDDict: `{"s1":1}`},
	}
	candidates := FilterJargonCandidates(records, JargonMatchOptions{GlobalSessionIDs: map[string]bool{"s1": true}})
	if len(candidates) != 2 {
		t.Fatalf("应只保留作用域内 + 全局 + 有含义 + is_jargon: %+v", candidates)
	}
	if candidates[0].Content != "a" || candidates[1].Content != "c" {
		t.Fatalf("候选集不符: %+v", candidates)
	}
	// has_global_share 命中时忽略作用域（对齐 has_global_share 分支）。
	shared := FilterJargonCandidates(records, JargonMatchOptions{HasGlobalShare: true, GlobalSessionIDs: map[string]bool{"s1": true}})
	if len(shared) != 3 {
		t.Fatalf("全局共享应放行全部作用域: %+v", shared)
	}
	// 非法 session_id_dict 应跳过。
	broken := FilterJargonCandidates([]JargonRecord{
		{ID: 9, Content: "z", Meaning: "含义", IsJargon: true, SessionIDDict: "not json"},
	}, JargonMatchOptions{GlobalSessionIDs: map[string]bool{"s1": true}})
	if len(broken) != 0 {
		t.Fatalf("损坏作用域应跳过: %+v", broken)
	}
}

func TestBuildJargonReferenceMessage(t *testing.T) {
	candidates := []JargonCandidate{{Content: "yyds", Meaning: "永远的神", Count: 10}}
	reference := BuildJargonReferenceMessage(
		[]string{"yyds 太强了"}, candidates,
		JargonMatchOptions{Use: true, Limit: 10}, nil, time.Now())
	if reference == nil {
		t.Fatalf("应产出参考消息")
	}
	if reference.Kind != "jargon" {
		t.Fatalf("参考类型应为 jargon: %q", reference.Kind)
	}
	if reference.DisplayPrefix != JargonReferenceDisplayPrefix {
		t.Fatalf("展示前缀应照抄: %q", reference.DisplayPrefix)
	}
	if !strings.HasPrefix(reference.Content, JargonReferenceHeader) {
		t.Fatalf("首行应为固定说明: %q", reference.Content)
	}
	if !strings.Contains(reference.Content, "1. yyds：永远的神") {
		t.Fatalf("条目格式应照抄: %q", reference.Content)
	}

	// excluded_contents 过滤。
	if got := BuildJargonReferenceMessage([]string{"yyds 太强了"}, candidates,
		JargonMatchOptions{Use: true, Limit: 10}, map[string]bool{"yyds": true}, time.Now()); got != nil {
		t.Fatalf("已注入内容应被排除: %+v", got)
	}
	// 无命中返回 nil。
	if got := BuildJargonReferenceMessage([]string{"普通内容"}, candidates,
		JargonMatchOptions{Use: true, Limit: 10}, nil, time.Now()); got != nil {
		t.Fatalf("无命中应返回 nil: %+v", got)
	}
}

func TestExtractJargonReferenceContents(t *testing.T) {
	reference := BuildJargonReferenceMessage(
		[]string{"yyds 太强了"}, []JargonCandidate{{Content: "yyds", Meaning: "永远的神", Count: 3}},
		JargonMatchOptions{Use: true, Limit: 10}, nil, time.Now())
	contextMessages := []chatcontextAlias{
		chatcontext.NewReferenceMessage(reference.Content, time.Now(), chatcontext.ReferenceJargon, nil, JargonReferenceDisplayPrefix),
	}
	contents := ExtractJargonReferenceContents(contextMessages)
	if !contents["yyds"] || len(contents) != 1 {
		t.Fatalf("应提取已注入词条: %+v", contents)
	}
	if !IsJargonReferenceText(reference.Content) {
		t.Fatalf("应识别黑话参考文本")
	}
	if IsJargonReferenceText("随便一段话") {
		t.Fatalf("普通文本不应误判")
	}
}

func TestExtractOtherUserTexts(t *testing.T) {
	userMessage := newContextBacked("m1", "user-1", "普通用户发言")
	botMessage := newContextBacked("m2", "bot-1", "机器人发言")
	texts := ExtractOtherUserTexts(
		[]chatcontextAlias{userMessage, botMessage, userMessage},
		func(_ string, userID string) bool { return userID == "bot-1" })
	if len(texts) != 1 || texts[0] != "普通用户发言" {
		t.Fatalf("只应取非机器人用户文本且去重: %v", texts)
	}
}

func TestSearchJargon(t *testing.T) {
	records := []JargonRecord{
		{ID: 1, Content: "yyds", Meaning: "永远的神", IsJargon: true, Count: 10, SessionIDDict: `{"s1":3}`},
		{ID: 2, Content: "YYDS 缩写", Meaning: "同 yyds", IsJargon: true, Count: 2, SessionIDDict: `{"s1":1}`},
		{ID: 3, Content: "xswl", Meaning: "笑死", IsJargon: true, Count: 5, SessionIDDict: `{"s2":1}`},
		{ID: 4, Content: "yyds", Meaning: "", IsJargon: true, Count: 1, SessionIDDict: `{"s1":1}`},
	}
	options := JargonMatchOptions{GlobalSessionIDs: map[string]bool{"s1": true}}
	results := SearchJargon(records, "yyds", options, false, true, 10)
	if len(results) != 2 {
		t.Fatalf("模糊搜索应命中作用域内的 2 条: %+v", results)
	}
	if results[0].Content != "yyds" {
		t.Fatalf("顺序应按原顺序: %+v", results)
	}
	exact := SearchJargon(records, "YYDS", options, false, false, 10)
	if len(exact) != 1 {
		t.Fatalf("大小写不敏感精确匹配应命中 1 条: %+v", exact)
	}
	if got := SearchJargon(records, "  ", options, false, true, 10); got != nil {
		t.Fatalf("空关键词应返回 nil: %v", got)
	}
	if got := SearchJargon(records, "yyds", options, false, true, 1); len(got) != 1 {
		t.Fatalf("limit 应生效: %v", got)
	}
}

// newContextBacked 构造一条 user 来源的上下文消息。
func newContextBacked(messageID string, userID string, text string) chatcontextAlias {
	sessionMessage := message.NewSessionMessage(messageID, time.Now(), "qq")
	sessionMessage.SessionID = "s1"
	sessionMessage.MessageInfo.UserInfo = message.NewUserInfo(userID, "用户"+userID, "")
	sessionMessage.ProcessedPlainText = message.StringPtr(text)
	return chatcontext.SessionBackedFromSessionMessage(
		sessionMessage, sessionMessage.RawMessage, text, chatcontext.SourceUser)
}

// ---------------------------------------------------------------------------
// JargonLearner + JargonMiner
// ---------------------------------------------------------------------------

func TestJargonLearnerMinesAndStores(t *testing.T) {
	store := &fakeJargonStore{}
	messages := learningMessages(10)
	for index := range messages {
		messages[index].Content = "yyds 真的强"
	}
	miner := NewJargonMiner("测试群")
	miner.Store = store
	learner := &JargonLearner{
		SessionID: "s1",
		Prompts:   fakePrompts(map[string]string{"learn_jargon": "{chat_str}"}),
		LLM:       (&fakeLLM{responses: []string{`[{"content":"yyds","source_id":"1"}]`}}).Call,
		Miner:     miner,
	}
	ok, err := learner.LearnFromMessages(context.Background(), messages, 3)
	if err != nil || !ok {
		t.Fatalf("黑话学习应成功: ok=%v err=%v", ok, err)
	}
	if store.count() != 1 {
		t.Fatalf("应写入 1 条黑话: %d", store.count())
	}
	record := store.at(0)
	if record.Content != "yyds" || record.Count != 1 || record.CreatedBy != "AI" {
		t.Fatalf("新条目应初始化 count=1/AI: %+v", record)
	}
	if record.SessionIDDict != `{"s1":1}` {
		t.Fatalf("session_id_dict 应计数: %q", record.SessionIDDict)
	}
	if got := miner.CachedJargons(); len(got) != 1 || got[0] != "yyds" {
		t.Fatalf("命中黑话应进缓存: %v", got)
	}
}

func TestJargonLearnerMinMessagesGate(t *testing.T) {
	learner := &JargonLearner{
		SessionID: "s1",
		Prompts:   fakePrompts(map[string]string{"learn_jargon": "{chat_str}"}),
		LLM:       (&fakeLLM{}).Call,
		Miner:     NewJargonMiner("测试群"),
	}
	ok, err := learner.LearnFromMessages(context.Background(), learningMessages(3), 3)
	if err != nil || ok {
		t.Fatalf("消息不足应跳过: ok=%v err=%v", ok, err)
	}
}

func TestJargonMinerMergesCountsAndSkipsManual(t *testing.T) {
	store := &fakeJargonStore{}
	miner := NewJargonMiner("测试群")
	miner.Store = store
	messages := learningMessages(10)

	entries := []JargonEntry{{
		Content:    "yyds",
		RawContent: []string{"语料"},
		EvidenceMessages: []JargonEvidenceRef{
			{Platform: "qq", MessageID: "m1"},
			{Platform: "qq", MessageID: "m1"}, // 重复引用应去重
		},
	}}
	if _, _, err := miner.ProcessExtractedEntries(context.Background(), entries, messages, "s1"); err != nil {
		t.Fatalf("写入黑话失败: %v", err)
	}
	if _, _, err := miner.ProcessExtractedEntries(context.Background(), entries, messages, "s1"); err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}
	record := store.at(0)
	if record.Count != 2 {
		t.Fatalf("同一黑话应累加计数: %d", record.Count)
	}
	if record.SessionIDDict != `{"s1":2}` {
		t.Fatalf("session_id_dict 应累加: %q", record.SessionIDDict)
	}
	if record.EvidenceMessages != `[{"Platform":"qq","MessageID":"m1"}]` {
		t.Fatalf("证据引用应去重合并: %q", record.EvidenceMessages)
	}

	// MANUAL 记录跳过更新。
	manualStore := &fakeJargonStore{records: []JargonRecord{{
		ID: 1, Content: "手动词", Count: 5, CreatedBy: "MANUAL",
	}}}
	manualMiner := NewJargonMiner("测试群")
	manualMiner.Store = manualStore
	if _, _, err := manualMiner.ProcessExtractedEntries(context.Background(),
		[]JargonEntry{{Content: "手动词"}}, messages, "s1"); err != nil {
		t.Fatalf("手动记录不应报错: %v", err)
	}
	if manualStore.at(0).Count != 5 {
		t.Fatalf("MANUAL 记录不应被 AI 覆盖: %+v", manualStore.at(0))
	}
}

func TestJargonMinerRejectsSelfAndBotName(t *testing.T) {
	store := &fakeJargonStore{}
	miner := NewJargonMiner("测试群")
	miner.Store = store
	miner.BotName = "麦麦"
	messages := learningMessages(10)
	saved, updated, err := miner.ProcessExtractedEntries(context.Background(), []JargonEntry{
		{Content: "SELF 的口头禅"},
		{Content: "麦麦专用词"},
		{Content: ""},
	}, messages, "s1")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if saved != 0 || updated != 0 || store.count() != 0 {
		t.Fatalf("无效条目应被拒绝: saved=%d updated=%d count=%d", saved, updated, store.count())
	}
}

func TestShouldInferMeaningThresholds(t *testing.T) {
	miner := NewJargonMiner("测试群")
	cases := []struct {
		count         int
		lastInference int
		expected      bool
	}{
		{3, 0, false},
		{4, 0, true},
		{7, 4, false},
		{8, 4, true},
		{25, 8, true},
		{100, 25, true},
		{100, 100, false}, // count <= last_inference
		{150, 100, false}, // 无下一个阈值
	}
	for _, testCase := range cases {
		record := JargonRecord{Count: testCase.count, LastInferenceCount: testCase.lastInference}
		if got := miner.ShouldInferMeaning(record); got != testCase.expected {
			t.Fatalf("ShouldInferMeaning(count=%d,last=%d)=%v want=%v",
				testCase.count, testCase.lastInference, got, testCase.expected)
		}
	}
	if miner.ShouldInferMeaning(JargonRecord{Count: 100, IsComplete: true}) {
		t.Fatalf("已完成的黑话不应再推断")
	}
	if miner.ShouldInferMeaning(JargonRecord{Count: 100, CreatedBy: "MANUAL"}) {
		t.Fatalf("手动记录不应推断")
	}
}

func TestInferMeaningThreeStageWritesJargon(t *testing.T) {
	store := &fakeJargonStore{records: []JargonRecord{{
		ID: 1, Content: "yyds", Count: 4, CreatedBy: "AI",
		EvidenceMessages: `[{"Platform":"qq","MessageID":"m1"}]`,
	}}}
	llm := &fakeLLM{responses: []string{
		`{"meaning":"永远的神，表示极度赞美","no_info":false}`, // 推断 1
		`{"meaning":"某个缩写"}`,                        // 推断 2
		`{"is_similar":false,"reason":"含义不同"}`,      // 比较 → 是黑话
	}}
	miner := NewJargonMiner("测试群")
	miner.Store = store
	miner.LLM = llm.Call
	miner.Prompts = fakePrompts(map[string]string{
		"jargon_inference_with_context": "{bot_name}|{content}|{raw_content_list}{previous_meaning_section}{previous_meaning_instruction}",
		"jargon_inference_content_only": "{content}",
		"jargon_compare_inference":      "{inference1}|{inference2}",
	})
	done, err := miner.InferMeaning(context.Background(), store.at(0))
	if err != nil || !done {
		t.Fatalf("推断应完成: done=%v err=%v", done, err)
	}
	record := store.at(0)
	if !record.IsJargon || !strings.Contains(record.Meaning, "永远的神") {
		t.Fatalf("推断结果应写回: %+v", record)
	}
	if record.LastInferenceCount != 4 {
		t.Fatalf("应记录 last_inference_count: %d", record.LastInferenceCount)
	}
	if miner.Inferences != 1 {
		t.Fatalf("推断计数应为 1: %d", miner.Inferences)
	}
}

func TestInferMeaningSimilarMeansNotJargon(t *testing.T) {
	store := &fakeJargonStore{records: []JargonRecord{{
		ID: 1, Content: "普通词", Count: 4, CreatedBy: "AI", Meaning: "旧含义",
		EvidenceMessages: `[{"Platform":"qq","MessageID":"m1"}]`,
	}}}
	miner := NewJargonMiner("测试群")
	miner.Store = store
	miner.LLM = (&fakeLLM{responses: []string{
		`{"meaning":"普通含义","no_info":false}`,
		`{"meaning":"普通含义"}`,
		`{"is_similar":true,"reason":"基本相同"}`,
	}}).Call
	miner.Prompts = fakePrompts(map[string]string{
		"jargon_inference_with_context": "{content}{raw_content_list}{previous_meaning_section}{previous_meaning_instruction}{bot_name}",
		"jargon_inference_content_only": "{content}",
		"jargon_compare_inference":      "{inference1}{inference2}",
	})
	if _, err := miner.InferMeaning(context.Background(), store.at(0)); err != nil {
		t.Fatalf("推断失败: %v", err)
	}
	record := store.at(0)
	if record.IsJargon {
		t.Fatalf("相似推断应判定为非黑话: %+v", record)
	}
	if record.Meaning != "旧含义" {
		t.Fatalf("非黑话应保留上一次含义: %q", record.Meaning)
	}
}

func TestInferMeaningNoInfoRecordsLastCount(t *testing.T) {
	store := &fakeJargonStore{records: []JargonRecord{{
		ID: 1, Content: "yyds", Count: 8, CreatedBy: "AI",
		EvidenceMessages: `[{"Platform":"qq","MessageID":"m1"}]`,
	}}}
	miner := NewJargonMiner("测试群")
	miner.Store = store
	miner.LLM = (&fakeLLM{responses: []string{`{"meaning":"","no_info":true}`}}).Call
	miner.Prompts = fakePrompts(map[string]string{
		"jargon_inference_with_context": "{content}{raw_content_list}{previous_meaning_section}{previous_meaning_instruction}{bot_name}",
	})
	done, err := miner.InferMeaning(context.Background(), store.at(0))
	if err != nil || done {
		t.Fatalf("信息不足应放弃推断: done=%v err=%v", done, err)
	}
	if store.at(0).LastInferenceCount != 8 {
		t.Fatalf("no_info 应记录 last_inference_count 以避免重复尝试: %d", store.at(0).LastInferenceCount)
	}
}

func TestJargonCacheLimitsAndSingleChar(t *testing.T) {
	miner := NewJargonMiner("测试群")
	miner.CacheLimit = 3
	miner.AddToCache("单")
	miner.AddToCache("a")
	miner.AddToCache("词一")
	miner.AddToCache("词二")
	miner.AddToCache("词三")
	miner.AddToCache("词四")
	cached := miner.CachedJargons()
	if len(cached) != 3 {
		t.Fatalf("缓存应限制为 3: %v", cached)
	}
	if containsString(cached, "词一") {
		t.Fatalf("最旧的应被淘汰: %v", cached)
	}
	if containsString(cached, "单") || containsString(cached, "a") {
		t.Fatalf("单字黑话不应入缓存: %v", cached)
	}
	if !IsSingleCharJargon("字") || !IsSingleCharJargon("a") || IsSingleCharJargon("ab") || IsSingleCharJargon("，") {
		t.Fatalf("单字判定规则不符")
	}
}

func TestParseJargonResponseAndEvidenceRefs(t *testing.T) {
	entries := ParseJargonResponse("```json\n[{\"content\":\"yyds\",\"source_id\":\"3\"},{\"content\":\"\",\"source_id\":\"4\"}]\n```")
	if len(entries) != 1 || entries[0].Content != "yyds" {
		t.Fatalf("应解析 1 条黑话: %+v", entries)
	}
	merged := MergeEvidenceRefs(
		[]JargonEvidenceRef{{Platform: "qq", MessageID: "m1"}},
		[]JargonEvidenceRef{{Platform: "qq", MessageID: "m1"}, {Platform: "qq", MessageID: "m2"}, {Platform: "", MessageID: "m3"}},
	)
	if len(merged) != 2 || merged[1].MessageID != "m2" {
		t.Fatalf("证据引用应顺序去重合并: %+v", merged)
	}
}

func TestBumpSessionIDDict(t *testing.T) {
	if got := bumpSessionIDDict(`{"s1":2}`, "s1"); got != `{"s1":3}` {
		t.Fatalf("应累加计数: %q", got)
	}
	if got := bumpSessionIDDict("", "s2"); got != `{"s2":1}` {
		t.Fatalf("空字典应初始化: %q", got)
	}
	if got := bumpSessionIDDict("broken", ""); got != `{}` {
		t.Fatalf("损坏字典应重置且空会话不计数: %q", got)
	}
}

func TestIsLearnableSessionMessage(t *testing.T) {
	if IsLearnableSessionMessage(LearningMessage{Content: "正常发言"}) != true {
		t.Fatalf("正常消息应可学习")
	}
	if IsLearnableSessionMessage(LearningMessage{Content: "发言", IsNotify: true}) {
		t.Fatalf("通知消息不可学习")
	}
	if IsLearnableSessionMessage(LearningMessage{Content: "发言", IsEmoji: true}) {
		t.Fatalf("表情消息不可学习")
	}
	if IsLearnableSessionMessage(LearningMessage{Content: "[表情包3]  "}) {
		t.Fatalf("仅表情包占位的消息不可学习")
	}
	if !IsLearnableSessionMessage(LearningMessage{Content: "[表情包3] 附带文字"}) {
		t.Fatalf("表情包 + 文字应可学习")
	}
	if IsLearnableSessionMessage(LearningMessage{Content: "   "}) {
		t.Fatalf("空白消息不可学习")
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
