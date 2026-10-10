package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/message"
)

// 测试辅助。

type fakeReplyRuntime struct {
	sessionID    string
	isGroup      bool
	history      []chatcontext.LLMContextMessage
	messagesByID map[string]*message.SessionMessage
	recordReply  int
	appended     []chatcontext.LLMContextMessage
}

func (f *fakeReplyRuntime) SessionID() string    { return f.sessionID }
func (f *fakeReplyRuntime) IsGroupSession() bool { return f.isGroup }
func (f *fakeReplyRuntime) LogPrefix() string    { return "[" + f.sessionID + "]" }
func (f *fakeReplyRuntime) ChatHistory() []chatcontext.LLMContextMessage {
	return f.history
}
func (f *fakeReplyRuntime) FindSourceMessageByID(messageID string) *message.SessionMessage {
	return f.messagesByID[messageID]
}
func (f *fakeReplyRuntime) RecordPlannerReply() { f.recordReply++ }
func (f *fakeReplyRuntime) AppendChatHistory(entries ...chatcontext.LLMContextMessage) {
	f.appended = append(f.appended, entries...)
}

type fakeSender struct {
	sent      []ReplySendRequest
	results   []string
	failAfter int
}

func (f *fakeSender) SendReplySegment(_ context.Context, request ReplySendRequest) (string, error) {
	if f.failAfter >= 0 && len(f.sent) >= f.failAfter {
		return "", context.Canceled
	}
	f.sent = append(f.sent, request)
	id := "sent_" + strings.TrimSpace(request.Text)
	f.results = append(f.results, id)
	return id, nil
}

type fakeGenerator struct {
	text string
	err  error
}

func (f *fakeGenerator) GenerateReply(_ context.Context, _ ReplyGenerateRequest) (ReplyGenerateResult, error) {
	if f.err != nil {
		return ReplyGenerateResult{Success: false, ErrorMessage: f.err.Error()}, f.err
	}
	return ReplyGenerateResult{Success: true, ResponseText: f.text}, nil
}

type fakePostProcessor struct{}

func (fakePostProcessor) PostProcessSegments(replyText string, _ bool, _ bool, _ bool) []OutboundSegment {
	return []OutboundSegment{{Text: replyText}}
}

func newTestDeps(runtime *fakeReplyRuntime, sender *fakeSender) *ReplyDependencies {
	return &ReplyDependencies{
		Runtime:       runtime,
		Sender:        sender,
		Generator:     &fakeGenerator{text: "你好呀"},
		PostProcessor: fakePostProcessor{},
		SegmentDelay:  nil,
	}
}

// -- wait 工具 --

type fakeWaitRuntime struct {
	entered    bool
	count      int
	maxCount   int
	lastCallID string
	lastTurnID string
}

func (f *fakeWaitRuntime) TryEnterWaitState(seconds *float64, toolCallID string, logicalTurnID string) (bool, int, int) {
	f.lastCallID = toolCallID
	f.lastTurnID = logicalTurnID
	if f.count >= f.maxCount {
		return false, f.count, f.maxCount
	}
	f.count++
	f.entered = true
	return true, f.count, f.maxCount
}

func TestWaitToolEntersWaitState(t *testing.T) {
	runtime := &fakeWaitRuntime{maxCount: 3}
	result := HandleWaitTool(context.Background(), runtime, ToolInvocation{
		ToolName:      "wait",
		CallID:        "call_1",
		LogicalTurnID: "turn_1",
		Arguments:     map[string]any{"seconds": float64(30)},
	})
	if !result.Success {
		t.Fatalf("wait 应成功: %+v", result)
	}
	if !runtime.entered || runtime.lastCallID != "call_1" || runtime.lastTurnID != "turn_1" {
		t.Fatalf("wait 应绑定 tool_call_id/logical_turn_id: %+v", runtime)
	}
	wantContent := "当前对话循环进入等待状态，将固定等待 30 秒；期间收到的新消息不会提前打断本次等待。连续 wait 次数：1/3。"
	if result.Content != wantContent {
		t.Fatalf("wait 成功回执文案应照抄:\n got=%q\nwant=%q", result.Content, wantContent)
	}
	if result.Metadata["pause_execution"] != true {
		t.Fatalf("wait 应暂停执行: %v", result.Metadata)
	}
}

func TestWaitToolLimitReached(t *testing.T) {
	runtime := &fakeWaitRuntime{maxCount: 3, count: 3}
	result := HandleWaitTool(context.Background(), runtime, ToolInvocation{
		ToolName:  "wait",
		CallID:    "call_1",
		Arguments: map[string]any{"seconds": float64(30)},
	})
	if !result.Success {
		t.Fatalf("wait 上限回执仍为 success: %+v", result)
	}
	wantContent := "连续 wait 已达到上限 3 次，本次不再进入等待；视为多次等待后仍无后续，当前对话进入休息。"
	if result.Content != wantContent {
		t.Fatalf("wait 上限回执文案应照抄:\n got=%q\nwant=%q", result.Content, wantContent)
	}
	if result.Metadata["wait_rest"] != true {
		t.Fatalf("wait 上限应携带 wait_rest: %v", result.Metadata)
	}
}

// -- registry schema 形态 --

func TestRegistrySchemaShape(t *testing.T) {
	entries := BuiltinEntries(FeatureFlags{EnableMemoryQueryTool: true, BotNickname: "麦麦"}, nil)
	registry := NewToolRegistry(entries)

	specs := registry.VisibleToolSpecs()
	names := map[string]bool{}
	for _, spec := range specs {
		names[spec.Name] = true
		// OpenAI function schema 形态：parameters 必须是 object 且含 properties
		if spec.ParametersSchema["type"] != "object" {
			t.Fatalf("%s 的 parameters.type 应为 object: %v", spec.Name, spec.ParametersSchema)
		}
		if _, ok := spec.ParametersSchema["properties"]; !ok {
			t.Fatalf("%s 的 parameters 应含 properties", spec.Name)
		}
		definition := spec.ToLLMDefinition()
		if definition.Name != spec.Name || definition.Description == "" {
			t.Fatalf("ToLLMDefinition 应携带 name/description: %+v", definition)
		}
	}
	for _, expected := range []string{"wait", "reply", "query_memory", "send_emoji", "tool_search"} {
		if !names[expected] {
			t.Fatalf("可见工具应含 %s: %v", expected, names)
		}
	}
	if names["view_forward_message"] {
		t.Fatalf("view_forward_message 默认 deferred，不应出现在可见列表")
	}
	if names["show_emoji_list"] {
		t.Fatalf("use_new_send_logic=false 时 show_emoji_list 不应可见")
	}

	// deferred 提醒文案
	reminder := registry.BuildDeferredToolsReminder()
	if !strings.Contains(reminder, "<system-reminder>") ||
		!strings.Contains(reminder, "以下工具当前未直接暴露给你，但可以通过 tool_search 工具发现并在后续轮次中使用：") ||
		!strings.Contains(reminder, "1. view_forward_message: 根据 msg_id 逐层查看合并转发消息。首次调用只展开顶层；遇到嵌套转发时，使用返回内容中的 path 再次调用以继续展开。") ||
		!strings.Contains(reminder, "如需其中某个工具，请先调用 tool_search。tool_search 只负责发现工具，不直接执行。") ||
		!strings.Contains(reminder, "</system-reminder>") {
		t.Fatalf("deferred 提醒文案应照抄:\n%s", reminder)
	}

	// 发现后不再提醒
	if got := registry.DiscoverDeferredTools([]string{"view_forward_message"}); len(got) != 1 {
		t.Fatalf("首次发现应返回新发现: %v", got)
	}
	if got := registry.DiscoverDeferredTools([]string{"view_forward_message"}); len(got) != 0 {
		t.Fatalf("重复发现不应返回: %v", got)
	}
	if registry.BuildDeferredToolsReminder() != "" {
		t.Fatalf("全部发现后提醒应为空")
	}
}

func TestToolSearchTool(t *testing.T) {
	entries := BuiltinEntries(FeatureFlags{}, nil)
	registry := NewToolRegistry(entries)

	result := HandleToolSearchTool(registry, ToolInvocation{
		ToolName:  "tool_search",
		CallID:    "call_1",
		Arguments: map[string]any{"query": "forward"},
	})
	if !result.Success {
		t.Fatalf("tool_search 应成功: %+v", result)
	}
	if !strings.Contains(result.Content, "已找到 1 个 deferred tools，它们会在后续轮次中加入可用工具列表：") ||
		!strings.Contains(result.Content, "- view_forward_message（本次新发现）") {
		t.Fatalf("tool_search 回执文案应照抄:\n%s", result.Content)
	}

	// 再次搜索 → 此前已发现
	result = HandleToolSearchTool(registry, ToolInvocation{
		ToolName:  "tool_search",
		Arguments: map[string]any{"query": "forward"},
	})
	if !strings.Contains(result.Content, "- view_forward_message（此前已发现）") {
		t.Fatalf("再次搜索应显示此前已发现:\n%s", result.Content)
	}

	// 无匹配
	result = HandleToolSearchTool(registry, ToolInvocation{
		ToolName:  "tool_search",
		Arguments: map[string]any{"query": "nonexistent_tool"},
	})
	if !strings.Contains(result.Content, "未找到匹配的 deferred tools，请尝试更完整的工具名、前缀或其他关键词。") {
		t.Fatalf("无匹配回执文案应照抄: %s", result.Content)
	}

	// 空 query → 失败
	result = HandleToolSearchTool(registry, ToolInvocation{
		ToolName:  "tool_search",
		Arguments: map[string]any{"query": "  "},
	})
	if result.Success || result.ErrorMessage != "tool_search 需要提供非空的 `query` 字符串参数。" {
		t.Fatalf("空 query 应失败: %+v", result)
	}
}

func TestToolSearchScoring(t *testing.T) {
	entries := BuiltinEntries(FeatureFlags{}, nil)
	registry := NewToolRegistry(entries)
	// 精确名 > 前缀 > 包含
	matched := registry.SearchDeferredToolSpecs("view_forward_message", 5)
	if len(matched) != 1 || matched[0].Name != "view_forward_message" {
		t.Fatalf("精确匹配应得分最高: %v", matched)
	}
	matched = registry.SearchDeferredToolSpecs("view", 5)
	if len(matched) != 1 {
		t.Fatalf("前缀匹配应命中: %v", matched)
	}
}

// -- reply 工具 --

func TestReplyToolMissingMsgID(t *testing.T) {
	deps := newTestDeps(&fakeReplyRuntime{}, &fakeSender{failAfter: -1})
	result := HandleReplyTool(context.Background(), deps, ToolInvocation{
		ToolName:  "reply",
		CallID:    "call_1",
		Arguments: map[string]any{},
	})
	if result.Success || result.ErrorMessage != "reply 工具需要提供有效的 `msg_id` 参数。" {
		t.Fatalf("缺少 msg_id 应失败: %+v", result)
	}
}

func TestReplyToolTargetNotFound(t *testing.T) {
	runtime := &fakeReplyRuntime{messagesByID: map[string]*message.SessionMessage{}}
	deps := newTestDeps(runtime, &fakeSender{failAfter: -1})
	result := HandleReplyTool(context.Background(), deps, ToolInvocation{
		ToolName:  "reply",
		CallID:    "call_1",
		Arguments: map[string]any{"msg_id": "abc123"},
	})
	want := "未找到要回复的目标消息，msg_id=abc123"
	if result.Success || result.ErrorMessage != want {
		t.Fatalf("目标消息查不到应失败回执:\n got=%q\nwant=%q", result.ErrorMessage, want)
	}
}

func TestReplyToolSuccessReceiptAndRecordReply(t *testing.T) {
	target := makeMessage("m1", "张三", "在吗")
	runtime := &fakeReplyRuntime{
		messagesByID: map[string]*message.SessionMessage{"m1": target},
	}
	sender := &fakeSender{failAfter: -1}
	deps := newTestDeps(runtime, sender)
	deps.BotNameFn = func() string { return "麦麦" }
	deps.IsBotSelfFn = func(string, string) bool { return false }

	result := HandleReplyTool(context.Background(), deps, ToolInvocation{
		ToolName:  "reply",
		CallID:    "call_1",
		Arguments: map[string]any{"msg_id": "m1"},
	})
	if !result.Success {
		t.Fatalf("reply 应成功: %+v", result)
	}
	want := "\"麦麦\"已生成并向\"张三\"发送了回复\"你好呀\""
	if result.Content != want {
		t.Fatalf("成功回执文案应照抄:\n got=%q\nwant=%q", result.Content, want)
	}
	if runtime.recordReply != 1 {
		t.Fatalf("发送成功后应 record_reply: %d", runtime.recordReply)
	}
	// 第一条 quote 目标
	if len(sender.sent) != 1 || sender.sent[0].ReplyToMessageID != "m1" || !sender.sent[0].SetQuote {
		t.Fatalf("第一条应引用目标消息: %+v", sender.sent)
	}
}

func TestReplyToolSelfTargetReceipt(t *testing.T) {
	target := makeMessage("m1", "麦麦", "我自己说的话")
	runtime := &fakeReplyRuntime{messagesByID: map[string]*message.SessionMessage{"m1": target}}
	sender := &fakeSender{failAfter: -1}
	deps := newTestDeps(runtime, sender)
	deps.BotNameFn = func() string { return "麦麦" }
	deps.IsBotSelfFn = func(string, string) bool { return true }

	result := HandleReplyTool(context.Background(), deps, ToolInvocation{
		ToolName:  "reply",
		CallID:    "call_1",
		Arguments: map[string]any{"msg_id": "m1"},
	})
	want := "\"麦麦\"已生成并向当前会话发送了回复\"你好呀\""
	if result.Content != want {
		t.Fatalf("自身目标回执文案应照抄:\n got=%q\nwant=%q", result.Content, want)
	}
}

func TestReplyToolDuplicateTargetReminder(t *testing.T) {
	target := makeMessage("m1", "张三", "在吗")
	runtime := &fakeReplyRuntime{messagesByID: map[string]*message.SessionMessage{"m1": target}}
	// 历史里放一条引用 m1 的 guided_reply
	runtime.history = append(runtime.history, buildGuidedReply("m1", "刚回复过啦"))
	sender := &fakeSender{failAfter: -1}
	deps := newTestDeps(runtime, sender)
	deps.BotNameFn = func() string { return "麦麦" }

	var captured map[string]any
	deps.Generator = newCaptureGenerator(&captured, "好")

	result := HandleReplyTool(context.Background(), deps, ToolInvocation{
		ToolName:  "reply",
		CallID:    "call_1",
		Arguments: map[string]any{"msg_id": "m1"},
	})
	if !result.Success {
		t.Fatalf("reply 应成功: %+v", result)
	}
	reminder, _ := captured["_duplicate_target_reply_reminder"].(string)
	want := "你刚刚已经回复过这条消息，你刚刚的发言是：“刚回复过啦”\n" +
		"你现在想再次回复这条消息，进行补充，注意请不要和之前你的发言重复。"
	if reminder != want {
		t.Fatalf("重复回复提醒文案应照抄:\n got=%q\nwant=%q", reminder, want)
	}
}

func TestReplyToolNoDuplicateReminderWhenFresh(t *testing.T) {
	target := makeMessage("m1", "张三", "在吗")
	runtime := &fakeReplyRuntime{messagesByID: map[string]*message.SessionMessage{"m1": target}}
	deps := newTestDeps(runtime, &fakeSender{failAfter: -1})
	var captured map[string]any
	deps.Generator = newCaptureGenerator(&captured, "好")

	result := HandleReplyTool(context.Background(), deps, ToolInvocation{
		ToolName:  "reply",
		CallID:    "call_1",
		Arguments: map[string]any{"msg_id": "m1"},
	})
	if !result.Success {
		t.Fatalf("reply 应成功: %+v", result)
	}
	if _, exists := captured["_duplicate_target_reply_reminder"]; exists {
		t.Fatalf("首次回复不应携带重复提醒: %v", captured)
	}
}

func TestReplyToolSendFailureStops(t *testing.T) {
	target := makeMessage("m1", "张三", "在吗")
	runtime := &fakeReplyRuntime{messagesByID: map[string]*message.SessionMessage{"m1": target}}
	sender := &fakeSender{failAfter: 0} // 第一条即失败
	deps := newTestDeps(runtime, sender)
	deps.PostProcessor = multiSegmentPostProcessor{segments: []OutboundSegment{
		{Text: "第一段"}, {Text: "第二段"},
	}}
	deps.BotNameFn = func() string { return "麦麦" }

	result := HandleReplyTool(context.Background(), deps, ToolInvocation{
		ToolName:  "reply",
		CallID:    "call_1",
		Arguments: map[string]any{"msg_id": "m1"},
	})
	if result.Success || result.ErrorMessage != "可见回复生成成功，但发送失败。" {
		t.Fatalf("发送失败应回执失败: %+v", result)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("第一条失败后不应继续发送: %+v", sender.sent)
	}
	if runtime.recordReply != 0 {
		t.Fatalf("发送失败不应 record_reply: %d", runtime.recordReply)
	}
}

func TestReplyToolMidwayFailureStops(t *testing.T) {
	target := makeMessage("m1", "张三", "在吗")
	runtime := &fakeReplyRuntime{messagesByID: map[string]*message.SessionMessage{"m1": target}}
	sender := &fakeSender{failAfter: 1} // 第二条失败
	deps := newTestDeps(runtime, sender)
	deps.PostProcessor = multiSegmentPostProcessor{segments: []OutboundSegment{
		{Text: "第一段"}, {Text: "第二段"}, {Text: "第三段"},
	}}
	deps.BotNameFn = func() string { return "麦麦" }

	result := HandleReplyTool(context.Background(), deps, ToolInvocation{
		ToolName:  "reply",
		CallID:    "call_1",
		Arguments: map[string]any{"msg_id": "m1"},
	})
	if result.Success {
		t.Fatalf("中途失败应回执失败: %+v", result)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("任何一条失败即停: sent=%d", len(sender.sent))
	}
	if runtime.recordReply != 0 {
		t.Fatalf("发送失败不应 record_reply")
	}
}

func TestReplyToolSegmentsQuoteFirstOnly(t *testing.T) {
	target := makeMessage("m1", "张三", "在吗")
	runtime := &fakeReplyRuntime{messagesByID: map[string]*message.SessionMessage{"m1": target}}
	sender := &fakeSender{failAfter: -1}
	deps := newTestDeps(runtime, sender)
	deps.PostProcessor = multiSegmentPostProcessor{segments: []OutboundSegment{
		{Text: "第一段"}, {Text: "第二段"}, {Text: "第三段", QuotePrevious: true},
	}}
	deps.BotNameFn = func() string { return "麦麦" }
	// 无间隔等待（测试）
	deps.SegmentDelay = nil

	result := HandleReplyTool(context.Background(), deps, ToolInvocation{
		ToolName:  "reply",
		CallID:    "call_1",
		Arguments: map[string]any{"msg_id": "m1"},
	})
	if !result.Success {
		t.Fatalf("多段回复应成功: %+v", result)
	}
	if len(sender.sent) != 3 {
		t.Fatalf("应顺序发送 3 段: %d", len(sender.sent))
	}
	// 第一段 quote 目标
	if !sender.sent[0].SetQuote || sender.sent[0].ReplyToMessageID != "m1" {
		t.Fatalf("第一段应引用目标: %+v", sender.sent[0])
	}
	// 第二段不引用
	if sender.sent[1].SetQuote || sender.sent[1].QuotePrevious {
		t.Fatalf("第二段不应引用: %+v", sender.sent[1])
	}
	// 第三段引用上一条（纠正段）
	if !sender.sent[2].QuotePrevious || sender.sent[2].PreviousMessageID != "sent_第二段" {
		t.Fatalf("quote_previous 段应引用上一条已发送消息: %+v", sender.sent[2])
	}
	// 成功回执拼接全部分段
	if !strings.Contains(result.Content, "发送了回复\"第一段第二段第三段\"") {
		t.Fatalf("回执应包含全部分段: %q", result.Content)
	}
}

func TestReplyToolSetQuoteDisabledByConfig(t *testing.T) {
	// enable_reply_quote=false 时 effective_set_quote=false（第一段不引用）
	target := makeMessage("m1", "张三", "在吗")
	runtime := &fakeReplyRuntime{messagesByID: map[string]*message.SessionMessage{"m1": target}}
	deps := newTestDeps(runtime, &fakeSender{failAfter: -1})
	deps.BotNameFn = func() string { return "麦麦" }
	// 本实现 enableReplyQuote 恒 true（照抄默认配置）；set_quote=false 时也不引用
	result := HandleReplyTool(context.Background(), deps, ToolInvocation{
		ToolName:  "reply",
		CallID:    "call_1",
		Arguments: map[string]any{"msg_id": "m1", "set_quote": false},
	})
	if !result.Success {
		t.Fatalf("reply 应成功: %+v", result)
	}
	structured, _ := result.StructuredContent["effective_set_quote"].(bool)
	if structured {
		t.Fatalf("set_quote=false 时 effective_set_quote 应为 false: %v", result.StructuredContent)
	}
}

// -- query_memory 工具 --

type fakeMemorySearcher struct {
	result MemorySearchResult
	err    error
}

func (f *fakeMemorySearcher) SearchMemory(_ context.Context, mode string, query string, chatID string, topK int) (MemorySearchResult, error) {
	f.result.Success = true
	return f.result, f.err
}

func TestQueryMemoryToolValidation(t *testing.T) {
	deps := &QueryMemoryDeps{Searcher: &fakeMemorySearcher{}, SessionID: "s1", DefaultLimit: 5}
	result := HandleQueryMemoryTool(context.Background(), deps, ToolInvocation{
		ToolName:  "query_memory",
		Arguments: map[string]any{"mode": "bogus", "query": "x"},
	})
	if result.Success || !strings.Contains(result.ErrorMessage, "不支持的检索模式：bogus") {
		t.Fatalf("非法模式应失败: %+v", result)
	}
	result = HandleQueryMemoryTool(context.Background(), deps, ToolInvocation{
		ToolName:  "query_memory",
		Arguments: map[string]any{},
	})
	if result.Success || result.ErrorMessage != "query_memory 需要提供 query，或至少提供 time_start/time_end 中的一个。" {
		t.Fatalf("空参数应失败: %+v", result)
	}
}

func TestQueryMemoryToolHitContent(t *testing.T) {
	searcher := &fakeMemorySearcher{result: MemorySearchResult{
		Hits: []MemorySearchHit{{Content: "喜欢在深夜写代码", Type: "paragraph", Title: "习惯"}},
	}}
	deps := &QueryMemoryDeps{Searcher: searcher, SessionID: "s1", DefaultLimit: 5}
	result := HandleQueryMemoryTool(context.Background(), deps, ToolInvocation{
		ToolName:  "query_memory",
		Arguments: map[string]any{"query": "作息"},
	})
	if !result.Success {
		t.Fatalf("query_memory 应成功: %+v", result)
	}
	if result.Content != "1. 喜欢在深夜写代码" {
		t.Fatalf("命中内容渲染应对齐 to_text: %q", result.Content)
	}
	reference, _ := result.Metadata["replyer_memory_reference"].(string)
	if !strings.HasPrefix(reference, "【长期记忆检索结果-内部参考】") ||
		!strings.Contains(reference, "查询：作息") ||
		!strings.Contains(reference, "命中：1 条") {
		t.Fatalf("replyer 记忆参考应照抄格式:\n%s", reference)
	}
}

func TestQueryMemoryToolEmptyResult(t *testing.T) {
	searcher := &fakeMemorySearcher{result: MemorySearchResult{}}
	deps := &QueryMemoryDeps{Searcher: searcher, SessionID: "s1", DefaultLimit: 5}
	result := HandleQueryMemoryTool(context.Background(), deps, ToolInvocation{
		ToolName:  "query_memory",
		Arguments: map[string]any{"query": "没有的东西"},
	})
	if !result.Success || result.Content != "未找到匹配的长期记忆。" {
		t.Fatalf("空结果回执文案应照抄: %+v", result)
	}
}

// -- 表情包工具 --

func TestSendEmojiWithoutSender(t *testing.T) {
	result := HandleSendEmojiTool(context.Background(), nil, "s1", "", ToolInvocation{ToolName: "send_emoji"})
	if result.Success || result.ErrorMessage != emojiVLMNotConfiguredMessage {
		t.Fatalf("未注入发送器应回执 VLM 未配置: %+v", result)
	}
}

type fakeEmojiSender struct{}

func (fakeEmojiSender) SendEmojiByEmotion(_ context.Context, _ string, _ string, _ string) (string, string, error) {
	return "开心的猫", "开心", nil
}

func TestSendEmojiWithSender(t *testing.T) {
	result := HandleSendEmojiTool(context.Background(), fakeEmojiSender{}, "s1", "r", ToolInvocation{ToolName: "send_emoji"})
	if !result.Success || result.Content != emojiSuccessMessage {
		t.Fatalf("发送成功回执文案应照抄: %+v", result)
	}
	structured := result.StructuredContent
	if structured["matched_emotion"] != "开心" || structured["description"] != "开心的猫" {
		t.Fatalf("structured_content 应携带情绪命中: %v", structured)
	}
}

func TestShowEmojiListWithoutLister(t *testing.T) {
	result := HandleShowEmojiListTool(context.Background(), nil, ToolInvocation{ToolName: "show_emoji_list"})
	if result.Success || result.ErrorMessage != "当前表情包库中没有可用表情。" {
		t.Fatalf("未注入拼图器应回执无表情: %+v", result)
	}
}

// -- view_forward_message --

func TestViewForwardMessageValidation(t *testing.T) {
	runtime := &fakeReplyRuntime{}
	result := HandleViewForwardMessageTool(runtime, ToolInvocation{
		ToolName:  "view_forward_message",
		Arguments: map[string]any{},
	})
	if result.Success || result.ErrorMessage != "查看转发消息工具需要提供有效的 `msg_id` 参数。" {
		t.Fatalf("缺少 msg_id 应失败: %+v", result)
	}
	result = HandleViewForwardMessageTool(runtime, ToolInvocation{
		ToolName:  "view_forward_message",
		Arguments: map[string]any{"msg_id": "x", "path": []any{float64(-1)}},
	})
	if result.Success || result.ErrorMessage != "查看转发消息工具的 `path` 必须是由非负整数组成的数组。" {
		t.Fatalf("非法 path 应失败: %+v", result)
	}
	result = HandleViewForwardMessageTool(runtime, ToolInvocation{
		ToolName:  "view_forward_message",
		Arguments: map[string]any{"msg_id": "gone"},
	})
	if result.Success || result.ErrorMessage != "未找到目标转发消息，msg_id=gone" {
		t.Fatalf("目标不存在应失败: %+v", result)
	}
}

// -- fetch_history --

type fakeFetchRuntime struct {
	focusActive bool
	num         int
}

func (f *fakeFetchRuntime) FocusModeActiveForCurrentChat() bool { return f.focusActive }
func (f *fakeFetchRuntime) BuildFocusFetchHistoryResult(num int) (string, map[string]any, []PostHistoryEntry, error) {
	f.num = num
	return "历史消息", map[string]any{"count": num}, nil, nil
}

func TestFetchHistoryRequiresFocus(t *testing.T) {
	runtime := &fakeFetchRuntime{}
	result := HandleFetchHistoryTool(runtime, ToolInvocation{ToolName: "fetch_history"})
	if result.Success || result.ErrorMessage != "focus_mode 未启用，fetch_history 不可用。" {
		t.Fatalf("focus 未启用应失败: %+v", result)
	}
	runtime.focusActive = true
	result = HandleFetchHistoryTool(runtime, ToolInvocation{
		ToolName:  "fetch_history",
		Arguments: map[string]any{"num": float64(20)},
	})
	if !result.Success || runtime.num != 20 {
		t.Fatalf("focus 启用应执行: %+v num=%d", result, runtime.num)
	}
}

// 辅助构造。

func makeMessage(id string, nickname string, text string) *message.SessionMessage {
	info := message.NewUserInfo("u_"+id, nickname, "")
	plain := text
	return &message.SessionMessage{
		MessageID:          id,
		Timestamp:          timeNow(),
		Platform:           "qq",
		MessageInfo:        message.MessageInfo{UserInfo: info},
		RawMessage:         message.NewMessageSequence(message.NewTextComponent(text)),
		ProcessedPlainText: &plain,
	}
}

type captureGenerator struct {
	captured *map[string]any
	text     string
}

func (g captureGenerator) GenerateReply(_ context.Context, request ReplyGenerateRequest) (ReplyGenerateResult, error) {
	*g.captured = request.ReplyToolArgs
	return ReplyGenerateResult{Success: true, ResponseText: g.text}, nil
}

func newCaptureGenerator(captured *map[string]any, text string) ReplyGenerator {
	return captureGenerator{captured: captured, text: text}
}

type multiSegmentPostProcessor struct {
	segments []OutboundSegment
}

func (m multiSegmentPostProcessor) PostProcessSegments(string, bool, bool, bool) []OutboundSegment {
	return m.segments
}

// buildGuidedReply builds a guided_reply history entry quoting targetID.
func buildGuidedReply(targetID string, text string) *chatcontext.SessionBackedMessage {
	rawSequence := message.NewMessageSequence(message.NewReplyComponent(targetID), message.NewTextComponent(text))
	entry := chatcontext.NewSessionBackedMessage(
		rawSequence,
		text,
		timeNow(),
		"self_"+targetID,
		nil,
		chatcontext.SourceGuidedReply,
	)
	return entry
}

// timeNow is a test clock helper.
func timeNow() time.Time { return time.Unix(1700000000, 0) }

// sessionRuntimeForTest 组合 ReplyRuntime 与 wait 面（模拟 runtime.Runtime
// 同时满足 SessionRuntime 接口并集）。
type sessionRuntimeForTest struct {
	*fakeReplyRuntime
	wait fakeWaitRuntime
}

func (s *sessionRuntimeForTest) TryEnterWaitState(seconds *float64, toolCallID string, logicalTurnID string) (bool, int, int) {
	return s.wait.TryEnterWaitState(seconds, toolCallID, logicalTurnID)
}

// 回归 P0（根因 1）：BuiltinEntries 的每个目录项都必须携带可用 handler。
// 此前 8 个条目的 HandleTool 全为 nil，planner 一旦返回工具调用就会因
// 调用 nil 函数而崩溃进程。
func TestBuiltinEntriesHandlersWired(t *testing.T) {
	registry := NewToolRegistry(BuiltinEntries(FeatureFlags{BotNickname: "麦麦"}, nil))
	for _, name := range registry.Names() {
		if registry.HandlerOf(name) == nil {
			t.Fatalf("内置工具 %s 的 handler 未装配（nil handler 调用会 panic）", name)
		}
	}
	// tool_search 的 handler 需要注册表自身，由 NewToolRegistry 回填。
	if registry.HandlerOf("tool_search") == nil {
		t.Fatal("tool_search handler 应由注册表回填")
	}
}

// 防御 6：Invoke 对 nil handler 显式报错而不是 panic。
func TestRegistryInvokeNilHandlerDefense(t *testing.T) {
	registry := NewToolRegistry([]BuiltinToolEntry{{
		Name:    "no_handler_tool",
		GetSpec: GetWaitToolSpec,
		// HandleTool 故意缺省。
	}})
	result := registry.Invoke(context.Background(), &RuntimeContext{SessionID: "s1"}, ToolInvocation{
		ToolName:  "no_handler_tool",
		CallID:    "call_1",
		Arguments: map[string]any{},
	})
	if result.Success {
		t.Fatalf("nil handler 应返回失败结果: %+v", result)
	}
	if !strings.Contains(result.ErrorMessage, "没有可用的处理器") {
		t.Fatalf("错误信息应说明缺少处理器: %s", result.ErrorMessage)
	}
}

// 依赖未装配时内置工具按失败回执而不是 panic。
func TestBuiltinEntriesUnwiredDepsFailGracefully(t *testing.T) {
	registry := NewToolRegistry(BuiltinEntries(FeatureFlags{BotNickname: "麦麦"}, nil))
	for _, name := range []string{"wait", "reply", "view_forward_message", "query_memory", "fetch_history"} {
		result := registry.Invoke(context.Background(), &RuntimeContext{SessionID: "s1"}, ToolInvocation{
			ToolName: name,
			CallID:   "call_1",
			Arguments: map[string]any{
				"msg_id": "m1", "seconds": float64(1), "query": "q", "num": float64(1),
			},
		})
		if result.Success {
			t.Fatalf("%s 在依赖未装配时应失败回执: %+v", name, result)
		}
	}
}

// 装配后的 wait 工具经 registry 正常执行，且 LogicalTurnID/CallID 透传到
// 等待状态机（修复 5 的工具层断言）。
func TestRegistryWiredWaitToolExecutes(t *testing.T) {
	rt := &sessionRuntimeForTest{fakeReplyRuntime: &fakeReplyRuntime{sessionID: "s1"}}
	registry := NewToolRegistry(BuiltinEntries(FeatureFlags{BotNickname: "麦麦"}, &BuiltinToolDeps{
		RuntimeFor: func(string) SessionRuntime { return rt },
	}))
	result := registry.Invoke(context.Background(), &RuntimeContext{SessionID: "s1"}, ToolInvocation{
		ToolName:      "wait",
		CallID:        "call_1",
		LogicalTurnID: "turn_1",
		Arguments:     map[string]any{"seconds": float64(5)},
	})
	if !result.Success {
		t.Fatalf("wait 应经 registry 正常执行: %+v", result)
	}
	if rt.wait.lastTurnID != "turn_1" || rt.wait.lastCallID != "call_1" {
		t.Fatalf("LogicalTurnID/CallID 应透传: turn=%q call=%q", rt.wait.lastTurnID, rt.wait.lastCallID)
	}
}

// TestDiscoveredMapConcurrentAccess 验证 L7：共享 registry 的 discovered map
// 必须可并发读写。本机无 gcc、-race 不可用，因此用并发压力测试代替竞态检测
// ——无锁的并发 map 写会直接触发 runtime fatal（"concurrent map writes"），
// 压测足以暴露回归。
func TestDiscoveredMapConcurrentAccess(t *testing.T) {
	registry := NewToolRegistry(BuiltinEntries(FeatureFlags{}, nil))
	const goroutines = 32
	const rounds = 50
	var waitGroup sync.WaitGroup
	for index := 0; index < goroutines; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			for round := 0; round < rounds; round++ {
				switch index % 4 {
				case 0:
					registry.DiscoverDeferredTools([]string{"view_forward_message"})
				case 1:
					_ = registry.DiscoveredToolSpecs()
				case 2:
					_ = registry.BuildDeferredToolsReminder()
				default:
					_ = registry.SearchDeferredToolSpecs("forward", 5)
				}
			}
		}(index)
	}
	waitGroup.Wait()

	// 收敛校验：全部发现后 DiscoveredToolSpecs 应包含 view_forward_message。
	registry.DiscoverDeferredTools([]string{"view_forward_message"})
	found := false
	for _, spec := range registry.DiscoveredToolSpecs() {
		if spec.Name == "view_forward_message" {
			found = true
		}
	}
	if !found {
		t.Fatal("并发发现后 view_forward_message 应出现在已发现列表中")
	}
}
