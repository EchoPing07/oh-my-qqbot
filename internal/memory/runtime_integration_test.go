package memory_test

import (
	"context"
	"strings"
	"testing"
	"time"

	contracts "oh-my-qqbot/internal"
	chatcontext "oh-my-qqbot/internal/chat/context"
	chatruntime "oh-my-qqbot/internal/chat/runtime"
	"oh-my-qqbot/internal/memory"
	"oh-my-qqbot/internal/message"
)

// 本文件是跨包对接测试：确认 P6 的实现能直接注入 P4b 的 runtime 接口
// （chatruntime.MidTermMemorySummarizer）与 Context 的展示层类型。

// 编译期断言：MidTermSummarizer 满足 runtime.MidTermMemorySummarizer。
var _ chatruntime.MidTermMemorySummarizer = (*memory.MidTermSummarizer)(nil)

type stubEmbedder struct{}

func (stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for range texts {
		out = append(out, []float32{1, 0, 0, 0})
	}
	return out, nil
}

type stubLLM struct{ response string }

func (s stubLLM) Call(_ context.Context, _ memory.LLMRequest) (string, error) {
	return s.response, nil
}

func stubPrompts(name string, contextMap map[string]string) (string, error) {
	out := name
	for key, value := range contextMap {
		out += "|" + key + "=" + value
	}
	return out, nil
}

// TestMidTermSummarizerSatisfiesRuntimeInterface 验证运行时可注入并折叠历史。
func TestMidTermSummarizerSatisfiesRuntimeInterface(t *testing.T) {
	summarizer := &memory.MidTermSummarizer{
		LLM:       stubLLM{response: `{"summary":"大家在约球","recall_cues":["球赛安排"]}`}.Call,
		Prompts:   stubPrompts,
		Embedder:  stubEmbedder{}.Embed,
		SessionID: "session-1",
	}

	removed := make([]chatcontext.LLMContextMessage, 0, 3)
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	for index, text := range []string{"周末看球吗", "我看行", "我来买票"} {
		sessionMessage := message.NewSessionMessage("m"+text[:1]+string(rune('a'+index)), base.Add(time.Duration(index)*time.Minute), "qq")
		sessionMessage.MessageInfo.UserInfo = message.NewUserInfo("user-1", "阿强", "")
		sessionMessage.ProcessedPlainText = message.StringPtr(text)
		removed = append(removed, chatcontext.SessionBackedFromSessionMessage(
			sessionMessage, sessionMessage.RawMessage, text, chatcontext.SourceUser))
	}

	payload, ok, err := summarizer.SummarizeMidTermMemory(context.Background(), removed)
	if err != nil || !ok || payload == nil {
		t.Fatalf("runtime 注入点调用失败: ok=%v err=%v", ok, err)
	}

	// 用 context 包的构建器+折叠器还原 runtime 的 post-process 路径。
	summaryMessage := chatcontext.BuildMidTermMemoryComplexMessage(*payload, removed)
	if !chatcontext.IsMidTermMemoryMessage(summaryMessage) {
		t.Fatalf("构建结果应可被 IsMidTermMemoryMessage 识别")
	}
	history := chatcontext.InsertMidTermMemoryMessage(nil, summaryMessage, 10)
	if len(history) != 1 {
		t.Fatalf("空历史插入应为 1 条: %d", len(history))
	}
	if !strings.HasPrefix(summaryMessage.ProcessedPlainText(), "[聊天回想]") {
		t.Fatalf("可见文本应以 [聊天回想] 开头: %q", summaryMessage.ProcessedPlainText())
	}
	if !strings.Contains(summaryMessage.PromptText, chatcontext.MidTermMemoryPreviewPrefix) {
		t.Fatalf("prompt 文本应含预览前缀: %q", summaryMessage.PromptText)
	}
	if !strings.HasPrefix(summaryMessage.MsgID, "mtm:") {
		t.Fatalf("消息 id 应为 mtm: 前缀: %q", summaryMessage.MsgID)
	}
}

// TestRecallBuilderProducesChatContextReference 验证召回产物可直接进入历史。
func TestRecallBuilderProducesChatContextReference(t *testing.T) {
	queryText := "大家约好周末去看球赛"
	builder := &memory.RecallBuilder{
		Embedder: func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, 0, len(texts))
			for range texts {
				out = append(out, []float32{1, 0})
			}
			return out, nil
		},
	}
	summaryMessage := chatcontext.BuildMidTermMemoryComplexMessage(
		chatcontext.MidTermMemorySummaryPayload{
			TimeRange:    "2026-01-02 03:04:05 ~ 2026-01-02 03:05:05",
			Participants: []string{"阿强"},
			Summary:      "讨论周末球赛",
			RecallCues:   []map[string]any{{"text": "球赛", "embedding": []float32{1, 0}}},
		}, nil)

	sessionMessage := message.NewSessionMessage("m1", time.Now(), "qq")
	sessionMessage.MessageInfo.UserInfo = message.NewUserInfo("user-1", "阿强", "")
	sessionMessage.ProcessedPlainText = message.StringPtr(queryText)
	selected := []chatcontext.LLMContextMessage{chatcontext.SessionBackedFromSessionMessage(
		sessionMessage, sessionMessage.RawMessage, queryText, chatcontext.SourceUser)}

	reference, ok, err := builder.BuildReferenceMessage(context.Background(),
		[]chatcontext.LLMContextMessage{summaryMessage}, selected)
	if err != nil || !ok {
		t.Fatalf("召回应命中: ok=%v err=%v", ok, err)
	}
	if !chatcontext.IsMidTermMemoryReferenceMessage(reference) {
		t.Fatalf("召回结果应可被 IsMidTermMemoryReferenceMessage 识别: %q", reference.Content)
	}
	if reference.CountInContext() {
		t.Fatalf("参考消息不应计入上下文窗口")
	}
	if reference.RemainingUses != nil {
		t.Fatalf("remaining_uses 应为 nil（永久有效）")
	}
	if !strings.Contains(reference.ProcessedPlainText(), "[参考消息]") {
		t.Fatalf("应按 display_prefix 渲染可见文本: %q", reference.ProcessedPlainText())
	}
}

// TestMemoryServiceMatchesToolsSearcherContract 验证门面可作为 query_memory 工具支撑。
func TestMemoryServiceMatchesToolsSearcherContract(t *testing.T) {
	service := memory.NewMemoryService(memory.KernelSurface{Searcher: stubSearcher{}})
	result := service.QueryMemory(context.Background(), memory.QueryMemoryRequest{
		Query: "阿强的偏好", Mode: "search", ChatID: "s1", Limit: 5, RespectFilter: true,
	})
	if !result.Success || len(result.Hits) != 1 {
		t.Fatalf("工具查询支撑面应返回命中: %+v", result)
	}
	if result.Hits[0].Content == "" || result.Hits[0].Type == "" {
		t.Fatalf("命中应带 content/type: %+v", result.Hits[0])
	}
	// to_text 语义对齐 replyer 参考块渲染。
	if text := memory.SearchText(memory.MemorySearchResult{Hits: result.Hits, Success: true}, 5, 160); !strings.HasPrefix(text, "1. ") {
		t.Fatalf("to_text 应为编号列表: %q", text)
	}
}

type stubSearcher struct{}

func (stubSearcher) SearchMemory(_ context.Context, _ string, _ string, _ string, _ int) ([]contracts.MemoryHit, error) {
	return []contracts.MemoryHit{{
		Kind: "paragraph", ID: "h1", Text: "阿强对青霉素过敏", Score: 0.9, ChatID: "s1",
	}}, nil
}
