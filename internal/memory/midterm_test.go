package memory

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// fakeEmbedder 是确定性假 embedding（字符 trigram 哈希投影 + L2 归一化），
// 保证同一文本恒等向量、相似文本高余弦。
type fakeEmbedder struct {
	dim int
}

func (f *fakeEmbedder) encode(text string) []float32 {
	vec := make([]float32, f.dim)
	runes := []rune(text)
	gram := 3
	if len(runes) < gram {
		gram = len(runes)
	}
	for index := 0; index+gram <= len(runes); index++ {
		var hash uint32 = 2166136261
		for _, char := range runes[index : index+gram] {
			hash ^= uint32(char)
			hash *= 16777619
		}
		vec[hash%uint32(f.dim)]++
	}
	norm := 0.0
	for _, value := range vec {
		norm += float64(value) * float64(value)
	}
	if norm == 0 {
		vec[0] = 1
		return vec
	}
	out := make([]float32, f.dim)
	for index, value := range vec {
		out[index] = float32(float64(value) / norm)
	}
	return out
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		out = append(out, f.encode(text))
	}
	return out, nil
}

// fixedEmbedder 对每个文本返回预设向量（黄金向量钉子）。
type fixedEmbedder struct {
	vectors map[string][]float32
}

func (f *fixedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		out = append(out, f.vectors[text])
	}
	return out, nil
}

// fakeLLM 记录请求并返回预设响应。
type fakeLLM struct {
	response string
	requests []LLMRequest
}

func (f *fakeLLM) Call(_ context.Context, request LLMRequest) (string, error) {
	f.requests = append(f.requests, request)
	return f.response, nil
}

// fakePrompts 用简单替换模拟 prompt 渲染（不依赖 internal/prompt）。
func fakePrompts(templates map[string]string) PromptFunc {
	return func(name string, contextMap map[string]string) (string, error) {
		template, ok := templates[name]
		if !ok {
			return "", errPromptMissing(name)
		}
		out := template
		for key, value := range contextMap {
			out = strings.ReplaceAll(out, "{"+key+"}", value)
		}
		return out, nil
	}
}

type promptMissingError string

func (e promptMissingError) Error() string { return "prompt 不存在: " + string(e) }

func errPromptMissing(name string) error { return promptMissingError(name) }

// buildTestHistory 构造带时间戳的用户消息条目。
func buildTestHistory(t *testing.T, texts []string) []chatcontext.LLMContextMessage {
	t.Helper()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	history := make([]chatcontext.LLMContextMessage, 0, len(texts))
	for index, text := range texts {
		sessionMessage := message.NewSessionMessage(
			"msg-"+itoa(index),
			base.Add(time.Duration(index)*time.Minute),
			"qq",
		)
		sessionMessage.MessageInfo.UserInfo = message.NewUserInfo("user-1", "阿强", "")
		if index%3 == 2 {
			sessionMessage.MessageInfo.UserInfo = message.NewUserInfo("bot-1", "麦麦", "")
		}
		sessionMessage.ProcessedPlainText = message.StringPtr(text)
		history = append(history, chatcontext.SessionBackedFromSessionMessage(
			sessionMessage, sessionMessage.RawMessage, text, chatcontext.SourceUser))
	}
	return history
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// ---------------------------------------------------------------------------
// 压缩：SummarizeMidTermMemory
// ---------------------------------------------------------------------------

func TestSummarizeMidTermMemoryBuildsPayload(t *testing.T) {
	history := buildTestHistory(t, []string{
		"我们周末去看球赛吧", "好啊，我买票", "记得带身份证", "球场在东边",
	})
	embedder := &fakeEmbedder{dim: 64}
	llm := &fakeLLM{response: `{"summary":"大家约好周末去看球赛，由阿强买票，需要带身份证，球场在东边。","recall_cues":["周末球赛安排","买票与身份证","球场位置"]}`}
	summarizer := &MidTermSummarizer{
		LLM:       llm.Call,
		Prompts:   fakePrompts(map[string]string{MidTermMemorySummaryPromptName: "压缩 {time_range} {participants_text}"}),
		Embedder:  embedder.Embed,
		SessionID: "session-1",
	}
	payload, ok, err := summarizer.SummarizeMidTermMemory(context.Background(), history)
	if err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	if !ok || payload == nil {
		t.Fatalf("压缩应成功")
	}
	if !strings.Contains(payload.Summary, "球赛") {
		t.Fatalf("summary 应保留话题: %q", payload.Summary)
	}
	if len(payload.RecallCues) != 3 {
		t.Fatalf("recall_cues 应为 3 条: %d", len(payload.RecallCues))
	}
	for _, cue := range payload.RecallCues {
		embedding, ok := cue["embedding"].([]float32)
		if !ok || len(embedding) != 64 {
			t.Fatalf("线索应带 embedding: %#v", cue)
		}
	}
	if len(llm.requests) != 1 {
		t.Fatalf("应调用一次 LLM: %d", len(llm.requests))
	}
	if llm.requests[0].Task != "mid_memory" {
		t.Fatalf("task 应为 mid_memory: %s", llm.requests[0].Task)
	}
	if !strings.Contains(llm.requests[0].SystemPrompt, "2026-01-02") {
		t.Fatalf("instruction 应含时间范围: %q", llm.requests[0].SystemPrompt)
	}
}

func TestSummarizeMidTermMemorySkipsWhenNothingToSummarize(t *testing.T) {
	llm := &fakeLLM{response: "{}"}
	summarizer := &MidTermSummarizer{
		LLM:       llm.Call,
		Prompts:   fakePrompts(map[string]string{MidTermMemorySummaryPromptName: "x"}),
		SessionID: "session-1",
	}
	_, ok, err := summarizer.SummarizeMidTermMemory(context.Background(), nil)
	if err != nil || ok {
		t.Fatalf("空裁切应跳过: ok=%v err=%v", ok, err)
	}
	if len(llm.requests) != 0 {
		t.Fatalf("跳过时不应调用 LLM: %d", len(llm.requests))
	}
}

func TestSummarizeMidTermMemorySkipsUnparsableResponse(t *testing.T) {
	history := buildTestHistory(t, []string{"甲说了点什么"})
	summarizer := &MidTermSummarizer{
		LLM:       (&fakeLLM{response: "not json at all"}).Call,
		Prompts:   fakePrompts(map[string]string{MidTermMemorySummaryPromptName: "x"}),
		SessionID: "session-1",
	}
	_, ok, err := summarizer.SummarizeMidTermMemory(context.Background(), history)
	if err != nil || ok {
		t.Fatalf("解析失败应返回 ok=false: ok=%v err=%v", ok, err)
	}
}

func TestSummarizeMidTermMemoryWritesKernelSummary(t *testing.T) {
	history := buildTestHistory(t, []string{"明天下午三点开会"})
	kernel := &fakeSummaryIngester{}
	summarizer := &MidTermSummarizer{
		LLM:            (&fakeLLM{response: `{"summary":"明天下午三点开会","recall_cues":["会议安排"]}`}).Call,
		Prompts:        fakePrompts(map[string]string{MidTermMemorySummaryPromptName: "x"}),
		SessionIDFn:    func() string { return "session-7" },
		Kernel:         kernel,
		IngestToKernel: true,
	}
	payload, ok, err := summarizer.SummarizeMidTermMemory(context.Background(), history)
	if err != nil || !ok {
		t.Fatalf("压缩应成功: %v", err)
	}
	if kernel.chatID != "session-7" || kernel.summary != payload.Summary {
		t.Fatalf("摘要应写入内核: chatID=%q summary=%q", kernel.chatID, kernel.summary)
	}
}

type fakeSummaryIngester struct {
	chatID  string
	summary string
	calls   int
}

func (f *fakeSummaryIngester) IngestSummary(_ context.Context, chatID string, summary string, _ time.Time) error {
	f.calls++
	f.chatID = chatID
	f.summary = summary
	return nil
}

// ---------------------------------------------------------------------------
// 召回：阈值 0.8
// ---------------------------------------------------------------------------

func TestRecallReferenceThresholdStrictlyGreater(t *testing.T) {
	// 黄金向量：query 与 cue 的余弦 = 0.8（等于阈值 → 不命中）。
	query := []float32{1, 0, 0, 0}
	exactlyThreshold := []float32{0.8, 0.6, 0, 0} // cos = 0.8 (0.64+0.36=1)
	above := []float32{0.9, 0.435890, 0, 0}       // cos ≈ 0.9
	if got := CosineSimilarityFloat32(query, exactlyThreshold); math.Abs(got-0.8) > 1e-6 {
		t.Fatalf("测试向量未钉死 0.8: %.9f", got)
	}
	if got := CosineSimilarityFloat32(query, above); got <= 0.8 {
		t.Fatalf("测试向量应高于阈值: %.6f", got)
	}

	makeCandidate := func(embedding []float32) MidTermMemoryRecallCandidate {
		summaryMessage := chatcontext.BuildMidTermMemoryComplexMessage(
			chatcontext.MidTermMemorySummaryPayload{
				TimeRange:    "2026-01-02 03:04:05 ~ 2026-01-02 03:05:05",
				Participants: []string{"阿强"},
				Summary:      "关于球赛",
				RecallCues:   []map[string]any{{"text": "球赛", "embedding": embedding}},
			},
			nil,
		)
		return MidTermMemoryRecallCandidate{
			Message:     summaryMessage,
			Payload:     chatcontext.MidTermMemorySummaryPayload{Summary: "关于球赛", TimeRange: "x"},
			SegmentText: "球赛",
			Embedding:   embedding,
		}
	}

	if got := SelectBestRecallCandidate([]MidTermMemoryRecallCandidate{makeCandidate(exactlyThreshold)},
		query, MidTermMemoryDefaultRecallThreshold); got != nil {
		t.Fatalf("余弦等于阈值不应命中: score=%.4f", got.Score)
	}
	hit := SelectBestRecallCandidate([]MidTermMemoryRecallCandidate{makeCandidate(above)},
		query, MidTermMemoryDefaultRecallThreshold)
	if hit == nil {
		t.Fatalf("余弦高于阈值应命中")
	}
	if hit.Score <= MidTermMemoryDefaultRecallThreshold {
		t.Fatalf("命中分数应严格大于阈值: %.4f", hit.Score)
	}
}

func TestRecallBuilderPicksBestAndSkipsRecalled(t *testing.T) {
	queryText := "大家约好周末去看球赛"
	embedder := &fixedEmbedder{vectors: map[string][]float32{
		queryText: {1, 0, 0},
		"球赛":      {1, 0, 0},
		"别的":      {0, 1, 0},
	}}
	buildSummary := func(summary string, cues ...map[string]any) chatcontext.LLMContextMessage {
		return chatcontext.BuildMidTermMemoryComplexMessage(
			chatcontext.MidTermMemorySummaryPayload{
				TimeRange:    "2026-01-02 03:04:05 ~ 2026-01-02 03:05:05",
				Participants: []string{"阿强"},
				Summary:      summary,
				RecallCues:   cues,
			},
			nil,
		)
	}
	history := []chatcontext.LLMContextMessage{
		buildSummary("关于球赛的讨论", map[string]any{"text": "球赛", "embedding": []float32{1, 0, 0}}),
		buildSummary("别的话题", map[string]any{"text": "别的", "embedding": []float32{0, 1, 0}}),
	}
	userEntry := chatcontext.SessionBackedFromSessionMessage(
		mustSessionMessage(t, "msg-q", queryText), message.NewMessageSequence(), queryText, chatcontext.SourceUser)
	selected := []chatcontext.LLMContextMessage{userEntry}

	builder := &RecallBuilder{Embedder: embedder.Embed}
	reference, ok, err := builder.BuildReferenceMessage(context.Background(), history, selected)
	if err != nil || !ok {
		t.Fatalf("应命中召回: ok=%v err=%v", ok, err)
	}
	if reference.ReferenceKind != chatcontext.ReferenceMemory {
		t.Fatalf("参考类型应为 memory: %s", reference.ReferenceKind)
	}
	if !strings.HasPrefix(reference.Content, chatcontext.MidTermMemoryReferenceMarker) {
		t.Fatalf("参考文案应以【聊天回想-内部参考】开头: %q", firstLine(reference.Content))
	}
	if !strings.Contains(reference.Content, "匹配分数: 1.0000") {
		t.Fatalf("参考应带匹配分数: %q", reference.Content)
	}

	// 把该参考放进 selected → 同一段不应重复召回。
	selectedWithReference := append(append([]chatcontext.LLMContextMessage{}, selected...), reference)
	_, ok, err = builder.BuildReferenceMessage(context.Background(), history, selectedWithReference)
	if err != nil {
		t.Fatalf("重复召回不应报错: %v", err)
	}
	if ok {
		t.Fatalf("已注入过的匹配段不应重复召回")
	}
}

func TestRecallBuilderNoHitWhenBelowThreshold(t *testing.T) {
	queryText := "大家约好周末去看球赛"
	embedder := &fixedEmbedder{vectors: map[string][]float32{
		queryText: {1, 0, 0},
		"无关":      {0, 1, 0},
	}}
	history := []chatcontext.LLMContextMessage{
		chatcontext.BuildMidTermMemoryComplexMessage(
			chatcontext.MidTermMemorySummaryPayload{
				Summary:    "完全无关的摘要",
				RecallCues: []map[string]any{{"text": "无关", "embedding": []float32{0, 1, 0}}},
			}, nil),
	}
	userEntry := chatcontext.SessionBackedFromSessionMessage(
		mustSessionMessage(t, "msg-q", queryText), message.NewMessageSequence(), queryText, chatcontext.SourceUser)

	builder := &RecallBuilder{Embedder: embedder.Embed}
	_, ok, err := builder.BuildReferenceMessage(context.Background(),
		history, []chatcontext.LLMContextMessage{userEntry})
	if err != nil {
		t.Fatalf("低于阈值不应报错: %v", err)
	}
	if ok {
		t.Fatalf("余弦 0 不应命中阈值 0.8")
	}
}

func TestBuildRecallQueryTextSkipsRecallAndReferences(t *testing.T) {
	queryText := strings.Repeat("球", 20)
	entries := []chatcontext.LLMContextMessage{
		chatcontext.BuildMidTermMemoryComplexMessage(
			chatcontext.MidTermMemorySummaryPayload{Summary: "旧回想"}, nil),
		chatcontext.NewReferenceMessage("参考内容", time.Now(), chatcontext.ReferenceMemory, nil, ""),
		chatcontext.SessionBackedFromSessionMessage(
			mustSessionMessage(t, "m1", queryText), message.NewMessageSequence(), queryText, chatcontext.SourceUser),
	}
	got := BuildRecallQueryText(entries)
	if !strings.Contains(got, queryText) {
		t.Fatalf("query 应包含用户消息: %q", got)
	}
	if strings.Contains(got, "旧回想") || strings.Contains(got, "参考内容") {
		t.Fatalf("query 不应包含回想或参考消息: %q", got)
	}
}

func TestBuildRecallQueryTextTruncatesTail(t *testing.T) {
	long := strings.Repeat("长", 500)
	entry := chatcontext.SessionBackedFromSessionMessage(
		mustSessionMessage(t, "m1", long), message.NewMessageSequence(), long, chatcontext.SourceUser)
	got := BuildRecallQueryText([]chatcontext.LLMContextMessage{entry})
	if len([]rune(got)) != MidTermMemoryRecallQueryMessageTextLimit {
		t.Fatalf("单条消息应截到最后 360 字: %d", len([]rune(got)))
	}
}

func TestBuildTimeRangeAndParticipants(t *testing.T) {
	history := buildTestHistory(t, []string{"a", "b", "c"})
	if got := BuildTimeRange(history); got != "2026-01-02 03:04:05 ~ 2026-01-02 03:06:05" {
		t.Fatalf("时间范围格式应照抄: %q", got)
	}
	participants := CollectParticipants(history, "麦麦")
	if len(participants) != 2 {
		t.Fatalf("参与人物应去重: %v", participants)
	}
	if participants[0] != "阿强" || participants[1] != "麦麦" {
		t.Fatalf("参与人物顺序应为首个出现顺序: %v", participants)
	}
}

func TestCosineSimilarityFloat32EdgeCases(t *testing.T) {
	if got := CosineSimilarityFloat32(nil, nil); got != 0 {
		t.Fatalf("空向量应为 0: %v", got)
	}
	if got := CosineSimilarityFloat32([]float32{1, 0}, []float32{1, 0, 0}); got != 0 {
		t.Fatalf("维度不一致应为 0: %v", got)
	}
	if got := CosineSimilarityFloat32([]float32{0, 0}, []float32{1, 0}); got != 0 {
		t.Fatalf("零范数应为 0: %v", got)
	}
	if got := CosineSimilarityFloat32([]float32{1, 0}, []float32{1, 0}); got != 1 {
		t.Fatalf("同向量应为 1: %v", got)
	}
}

func mustSessionMessage(t *testing.T, messageID string, text string) *message.SessionMessage {
	t.Helper()
	msg := message.NewSessionMessage(messageID, time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local), "qq")
	msg.MessageInfo.UserInfo = message.NewUserInfo("user-1", "阿强", "")
	msg.ProcessedPlainText = message.StringPtr(text)
	return msg
}

func firstLine(text string) string {
	if index := strings.Index(text, "\n"); index >= 0 {
		return text[:index]
	}
	return text
}
