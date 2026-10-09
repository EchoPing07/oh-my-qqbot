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

// fakeLLM 返回预设响应序列（按调用次数）；
type fakeLLM struct {
	responses []string
	mu        sync.Mutex
	requests  []LLMRequest
}

func (f *fakeLLM) Call(_ context.Context, request LLMRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := len(f.requests)
	f.requests = append(f.requests, request)
	if len(f.responses) == 0 {
		return "", nil
	}
	if index >= len(f.responses) {
		return f.responses[len(f.responses)-1], nil
	}
	return f.responses[index], nil
}

func (f *fakeLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// fakePrompts 用简单替换模拟 prompt 渲染。
func fakePrompts(templates map[string]string) PromptFunc {
	return func(name string, contextMap map[string]string) (string, error) {
		template, ok := templates[name]
		if !ok {
			return "", errMissingPrompt{name: name}
		}
		out := template
		for key, value := range contextMap {
			out = strings.ReplaceAll(out, "{"+key+"}", value)
		}
		return out, nil
	}
}

type errMissingPrompt struct{ name string }

func (e errMissingPrompt) Error() string { return "prompt 不存在: " + e.name }

// fakeExpressionStore 是内存 expressions 表。
type fakeExpressionStore struct {
	mu       sync.Mutex
	records  []ExpressionRecord
	nextID   int64
	checkedC map[int64]bool
	modified map[int64]string
}

func newFakeExpressionStore() *fakeExpressionStore {
	return &fakeExpressionStore{checkedC: map[int64]bool{}, modified: map[int64]string{}}
}

func (s *fakeExpressionStore) LoadCandidates(sessionID string, checkedOnly bool) ([]ExpressionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ExpressionRow, 0, len(s.records))
	for _, record := range s.records {
		if sessionID != "" && record.SessionID != nil && *record.SessionID != "" && *record.SessionID != sessionID {
			continue
		}
		if checkedOnly && !record.Checked {
			continue
		}
		modifiedBy := ""
		if record.ModifiedBy != nil && *record.ModifiedBy == "AI" {
			// 学习器在 self_reflect 打开时标记 modified_by=AI，但 checked 为 false；
			// 这里保持与 DB 读取一致（modified_by 原样透出）。
			modifiedBy = *record.ModifiedBy
		}
		out = append(out, ExpressionRow{
			ID:         record.ID,
			Situation:  record.Situation,
			Style:      record.Style,
			Count:      record.Count,
			SessionID:  record.SessionID,
			Checked:    record.Checked,
			ModifiedBy: modifiedBy,
		})
	}
	return out, nil
}

func (s *fakeExpressionStore) FindExact(sessionID string, situation string, style string) (*ExpressionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range s.records {
		if record.Situation != situation {
			continue
		}
		if NormalizeExpressionStyleForLearning(record.Style) != style {
			continue
		}
		if sessionID != "" && record.SessionID != nil && *record.SessionID != "" && *record.SessionID != sessionID {
			continue
		}
		return &ExpressionRow{
			ID:        record.ID,
			Situation: record.Situation,
			Style:     record.Style,
			Count:     record.Count,
			SessionID: record.SessionID,
			Checked:   record.Checked,
		}, nil
	}
	return nil, nil
}

func (s *fakeExpressionStore) Upsert(record ExpressionRecord) (ExpressionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.ID != 0 {
		for index := range s.records {
			if s.records[index].ID == record.ID {
				s.records[index].Count = record.Count
				s.records[index].LastActiveTime = record.LastActiveTime
				s.records[index].Checked = record.Checked
				s.records[index].ModifiedBy = record.ModifiedBy
				return s.records[index], nil
			}
		}
	}
	s.nextID++
	record.ID = s.nextID
	if record.SessionID == nil && record.Style != "" {
		// 保持原样（无会话归属的表达为全局表达）。
		_ = record
	}
	s.records = append(s.records, record)
	return record, nil
}

func (s *fakeExpressionStore) UpdateChecked(id int64, checked bool, modifiedBy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.records {
		if s.records[index].ID == id {
			s.records[index].Checked = checked
			if modifiedBy != "" {
				copyValue := modifiedBy
				s.records[index].ModifiedBy = &copyValue
			}
		}
	}
	return nil
}

func (s *fakeExpressionStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func (s *fakeExpressionStore) recordAt(index int) ExpressionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records[index]
}

// learningMessages 构造一批学习素材。
func learningMessages(count int) []LearningMessage {
	out := make([]LearningMessage, 0, count)
	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.Local)
	for index := 0; index < count; index++ {
		out = append(out, LearningMessage{
			MessageID:        "m" + itoaSmall(index),
			SessionID:        "s1",
			Platform:         "qq",
			UserID:           "user-1",
			UserName:         "阿强",
			Content:          "第" + itoaSmall(index) + "条发言",
			TimestampSeconds: float64(base.Add(time.Duration(index) * time.Minute).Unix()),
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// 候选解析与过滤
// ---------------------------------------------------------------------------

func TestParseExpressionResponse(t *testing.T) {
	raw := "```json\n" + `[{"situation":"表达惊叹","style":"使用 我嘞个xxxx","source_id":"3"},` +
		`{"situation":"讽刺赞同","style":"使用 对对对","source_id":"4"},` +
		`{"situation":"","style":"无效","source_id":"5"},` +
		`{"content":"yyds","source_id":"6"}]` + "\n```"
	expressions := ParseExpressionResponse(raw)
	if len(expressions) != 2 {
		t.Fatalf("应解析出 2 条表达: %+v", expressions)
	}
	if expressions[0].Style != "我嘞个xxxx" || expressions[0].SourceID != "3" {
		t.Fatalf("style 应去掉「使用」前缀: %+v", expressions[0])
	}
	if got := ParseExpressionResponse("not json"); got != nil {
		t.Fatalf("非法响应应返回 nil: %v", got)
	}
	if got := ParseExpressionResponse(`[{"situation":"x","style":"y","source_id":""}]`); len(got) != 0 {
		t.Fatalf("缺 source_id 应被丢弃: %v", got)
	}
}

func TestFilterExpressionsRules(t *testing.T) {
	learner := &ExpressionLearner{Options: ExpressionLearnerOptions{BotName: "麦麦", AliasNames: []string{"小麦"}}}
	messages := []LearningMessage{
		{Content: "今天天气不错", IsSelf: false},
		{Content: "麦麦：我在", IsSelf: true},
		{Content: "[表情包]哈哈", IsSelf: false},
		{Content: "发了张[图片：猫]", IsSelf: false},
		{Content: "阿强说了点东西", IsSelf: false},
		{Content: "麦麦说得对", IsSelf: false},
	}
	got := learner.FilterExpressions([]ExtractedExpression{
		{Situation: "天气好", Style: "哈哈", SourceID: "1"},     // 通过
		{Situation: "被夸", Style: "谢谢", SourceID: "2"},      // SELF 消息 → 丢弃
		{Situation: "开心", Style: "哈哈", SourceID: "3"},      // 表情包上下文 → 丢弃
		{Situation: "分享", Style: "看看", SourceID: "4"},      // 图片上下文 → 丢弃
		{Situation: "自指", Style: "SELF 风格", SourceID: "5"}, // 含 SELF → 丢弃
		{Situation: "称呼", Style: "麦麦", SourceID: "6"},      // style 与 bot 名重复 → 丢弃
		{Situation: "越界", Style: "x", SourceID: "99"},      // source_id 越界 → 丢弃
		{Situation: "非法", Style: "x", SourceID: "abc"},     // source_id 非数字 → 丢弃
	}, messages)
	if len(got) != 1 || got[0].Style != "哈哈" {
		t.Fatalf("过滤后应只剩 1 条: %+v", got)
	}
}

func TestResolveLearningSessionIDPrefersRegisteredMostFrequent(t *testing.T) {
	messages := []LearningMessage{
		{SessionID: "s-a"}, {SessionID: "s-b"}, {SessionID: "s-b"},
	}
	sessions := func(sessionID string) bool { return sessionID == "s-b" }
	if got := ResolveLearningSessionID("s-learner", messages, sessions); got != "s-b" {
		t.Fatalf("应选出现最多且已注册的会话: %q", got)
	}
	// 都不注册时回退到 learner_session_id（同样要求已注册）。
	if got := ResolveLearningSessionID("s-learner", messages, func(string) bool { return false }); got != "" {
		t.Fatalf("无可用会话应返回空: %q", got)
	}
	if got := ResolveLearningSessionID("s-learner", nil, func(sessionID string) bool { return sessionID == "s-learner" }); got != "s-learner" {
		t.Fatalf("无消息时应回退到 learner 会话: %q", got)
	}
}

// ---------------------------------------------------------------------------
// 学习批次：LLM → self_reflect → 入库 → 向量索引
// ---------------------------------------------------------------------------

func TestExpressionLearnerLearnWritesAndSyncsIndex(t *testing.T) {
	store := newFakeExpressionStore()
	dir := t.TempDir()
	indexPath := dir + "/expression_vector_index.json"
	llm := &fakeLLM{responses: []string{
		`[{"situation":"表示肯定","style":"使用 对对对","source_id":"1"}]`,
		`{"suitable":true,"reason":"泛用"}`, // self_reflect
	}}
	prompts := fakePrompts(map[string]string{
		"learn_style":           "{chat_str}",
		"expression_evaluation": "{situation}|{style}|{criteria_list}",
	})
	learner := &ExpressionLearner{
		SessionID: "s1",
		Options: ExpressionLearnerOptions{
			SelfReflect:         true,
			UseVectorExpression: true,
			VectorIndexPath:     indexPath,
			BotName:             "麦麦",
		},
		Store:    store,
		Prompts:  prompts,
		LLM:      llm.Call,
		Embedder: (&dimEmbedder{dim: 8}).Embed,
	}
	ok, err := learner.LearnFromMessages(context.Background(), learningMessages(10), 3)
	if err != nil {
		t.Fatalf("学习失败: %v", err)
	}
	if !ok {
		t.Fatalf("学习应成功")
	}
	if store.count() != 1 {
		t.Fatalf("应写入 1 条表达: %d", store.count())
	}
	record := store.recordAt(0)
	if record.Style != "对对对" || record.Count != 1 {
		t.Fatalf("入库内容应为规范化后的 style: %+v", record)
	}
	if record.Checked {
		t.Fatalf("AI 学习结果不应标记已人工审核: %+v", record)
	}
	if llm.callCount() != 2 {
		t.Fatalf("应调用学习 + 审核各一次: %d", llm.callCount())
	}
	if learner.Learned != 1 {
		t.Fatalf("学习计数应为 1: %d", learner.Learned)
	}

	// 向量索引应同步。
	index := NewExpressionVectorIndex(indexPath)
	index.Embedder = (&dimEmbedder{dim: 8}).Embed
	snapshot, err := index.Snapshot()
	if err != nil {
		t.Fatalf("读取索引失败: %v", err)
	}
	if len(snapshot.Expressions) != 1 || snapshot.Expressions[0].ID != record.ID {
		t.Fatalf("索引应同步表达记录: %+v", snapshot.Expressions)
	}
}

func TestExpressionLearnerSelfReflectRejectionSkipsWrite(t *testing.T) {
	store := newFakeExpressionStore()
	llm := &fakeLLM{responses: []string{
		`[{"situation":"特指","style":"使用 某某人的口头禅","source_id":"1"}]`,
		`{"suitable":false,"reason":"太特指"}`,
	}}
	learner := &ExpressionLearner{
		SessionID: "s1",
		Options:   ExpressionLearnerOptions{SelfReflect: true},
		Store:     store,
		Prompts: fakePrompts(map[string]string{
			"learn_style":           "{chat_str}",
			"expression_evaluation": "{situation}{style}{criteria_list}",
		}),
		LLM: llm.Call,
	}
	ok, err := learner.LearnFromMessages(context.Background(), learningMessages(10), 3)
	if err != nil {
		t.Fatalf("学习不应报错: %v", err)
	}
	if ok || store.count() != 0 {
		t.Fatalf("审核不通过不应入库: ok=%v count=%d", ok, store.count())
	}
}

func TestExpressionLearnerMergesExactDuplicate(t *testing.T) {
	store := newFakeExpressionStore()
	sessionID := "s1"
	store.records = append(store.records, ExpressionRecord{
		ID: 1, Situation: "表示肯定", Style: "对对对", Count: 1, SessionID: &sessionID,
	})
	store.nextID = 1
	learner := &ExpressionLearner{
		SessionID: "s1",
		Options:   ExpressionLearnerOptions{},
		Store:     store,
		Prompts: fakePrompts(map[string]string{
			"learn_style": "{chat_str}",
		}),
		LLM: (&fakeLLM{responses: []string{`[{"situation":"表示肯定","style":"使用 对对对","source_id":"1"}]`}}).Call,
	}
	ok, err := learner.LearnFromMessages(context.Background(), learningMessages(10), 3)
	if err != nil || !ok {
		t.Fatalf("学习应成功: ok=%v err=%v", ok, err)
	}
	if store.count() != 1 {
		t.Fatalf("完全一致的表达应合并: %d", store.count())
	}
	if got := store.recordAt(0).Count; got != 2 {
		t.Fatalf("合并应 count+1: %d", got)
	}
}

func TestExpressionLearnerSkipsTooManyCandidates(t *testing.T) {
	store := newFakeExpressionStore()
	items := make([]string, 0, 21)
	for index := 0; index < 21; index++ {
		items = append(items, `{"situation":"情景`+itoaSmall(index)+`","style":"风格`+itoaSmall(index)+`","source_id":"1"}`)
	}
	learner := &ExpressionLearner{
		SessionID: "s1",
		Store:     store,
		Prompts:   fakePrompts(map[string]string{"learn_style": "{chat_str}"}),
		LLM:       (&fakeLLM{responses: []string{"[" + strings.Join(items, ",") + "]"}}).Call,
	}
	ok, err := learner.LearnFromMessages(context.Background(), learningMessages(10), 3)
	if err != nil || ok {
		t.Fatalf("超过 20 条应整批丢弃: ok=%v err=%v", ok, err)
	}
	if store.count() != 0 {
		t.Fatalf("超限批次不应入库: %d", store.count())
	}
}

func TestExpressionLearnerRequiresMinMessagesFromContext(t *testing.T) {
	learner := &ExpressionLearner{
		SessionID: "s1",
		Store:     newFakeExpressionStore(),
		Prompts:   fakePrompts(map[string]string{"learn_style": "{chat_str}"}),
		LLM:       (&fakeLLM{}).Call,
	}
	contextMessages := make([]chatcontext.LLMContextMessage, 0, 3)
	for index := 0; index < 3; index++ {
		sessionMessage := message.NewSessionMessage("m"+itoaSmall(index), time.Now(), "qq")
		sessionMessage.SessionID = "s1"
		sessionMessage.MessageInfo.UserInfo = message.NewUserInfo("user-1", "阿强", "")
		sessionMessage.ProcessedPlainText = message.StringPtr("发言")
		contextMessages = append(contextMessages, chatcontext.SessionBackedFromSessionMessage(
			sessionMessage, sessionMessage.RawMessage, "发言", chatcontext.SourceUser))
	}
	ok, err := learner.LearnFromContextMessages(context.Background(), contextMessages, nil, 3)
	if err != nil || ok {
		t.Fatalf("消息不足 10 条应跳过: ok=%v err=%v", ok, err)
	}
}

func TestExpressionEvaluationFallbackParsing(t *testing.T) {
	suitable, _ := ParseExpressionEvaluation(`{"suitable":true,"reason":"合适"}`)
	if !suitable {
		t.Fatalf("标准 JSON 应解析成功")
	}
	suitable, _ = ParseExpressionEvaluation("```json\n{\"suitable\": false, \"reason\": \"太特指\"}\n```")
	if suitable {
		t.Fatalf("markdown fence 应剥离并解析")
	}
	if suitable, _ := ParseExpressionEvaluation("garbage"); suitable {
		t.Fatalf("无法解析应返回 false")
	}
}

func TestBuildExpressionLearningMessages(t *testing.T) {
	messages := []LearningMessage{
		{UserName: "阿强", Content: "第一句", IsSelf: false},
		{UserName: "麦麦", Content: "回复", IsSelf: true},
	}
	built := BuildExpressionLearningMessages("系统提示", messages)
	if len(built) != 4 {
		t.Fatalf("应构造 system + 2 条 user + 收尾 user: %d", len(built))
	}
	if built[0].Role != "system" || !strings.Contains(built[0].Content, "source_id") {
		t.Fatalf("首条应为含 source_id 说明的 system: %+v", built[0])
	}
	if !strings.Contains(built[1].Content, "[speaker:USER]") || !strings.Contains(built[2].Content, "[speaker:SELF]") {
		t.Fatalf("应标注 speaker 类型: %+v", built)
	}
	if built[1].Content != built[1].Content || !strings.Contains(built[1].Content, "[source_id:1]") {
		t.Fatalf("source_id 应从 1 开始: %q", built[1].Content)
	}
	if built[3].Content != "请根据以上聊天消息输出 JSON。" {
		t.Fatalf("收尾指令应照抄: %q", built[3].Content)
	}
}

func TestLearningSourceFromContextDeduplicates(t *testing.T) {
	sessionMessage := message.NewSessionMessage("m1", time.Now(), "qq")
	sessionMessage.SessionID = "s1"
	sessionMessage.MessageInfo.UserInfo = message.NewUserInfo("user-1", "阿强", "")
	sessionMessage.ProcessedPlainText = message.StringPtr("内容")
	entry := chatcontext.SessionBackedFromSessionMessage(
		sessionMessage, sessionMessage.RawMessage, "内容", chatcontext.SourceUser)
	reference := chatcontext.NewReferenceMessage("参考", time.Now(), chatcontext.ReferenceMemory, nil, "")
	sources := LearningSourceFromContext(
		[]chatcontext.LLMContextMessage{entry, entry, reference},
		func(_ string, userID string) bool { return userID == "bot-1" })
	if len(sources) != 1 {
		t.Fatalf("应去重且只保留真实聊天消息: %d", len(sources))
	}
	if sources[0].UserName != "阿强" || sources[0].Content != "内容" {
		t.Fatalf("投影字段不符: %+v", sources[0])
	}
}
