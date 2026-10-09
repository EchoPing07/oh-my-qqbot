package context

import (
	"fmt"
	"strings"
	"testing"

	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// process_chat_history_after_cycle
// ---------------------------------------------------------------------------

// TestProcessChatHistoryConsumesOneShotReferences pins `consume_once()`: an
// expired reference is removed, a permanent one survives.
func TestProcessChatHistoryConsumesOneShotReferences(t *testing.T) {
	uses := 1
	expiring := NewReferenceMessage("一次性", testTime(1), ReferenceToolHint, &uses, "[参考消息]")
	permanent := NewReferenceMessage("常驻", testTime(2), ReferenceContextRestore, nil, "[参考消息]")
	history := []LLMContextMessage{permanent, expiring}

	result, err := ProcessChatHistoryAfterCycle(history, 40, false, nil)
	if err != nil {
		t.Fatalf("ProcessChatHistoryAfterCycle: %v", err)
	}
	if result.RemovedCount != 1 {
		t.Fatalf("RemovedCount = %d, want 1", result.RemovedCount)
	}
	if len(result.History) != 1 || result.History[0] != LLMContextMessage(permanent) {
		t.Fatalf("history = %+v", result.History)
	}
}

// TestProcessChatHistoryDropsFocusWakeupSources pins FOCUS_WAKEUP_SOURCE_KINDS.
func TestProcessChatHistoryDropsFocusWakeupSources(t *testing.T) {
	history := []LLMContextMessage{
		NewReferenceMessage("醒醒", testTime(1), ReferenceCustom, nil, "[参考消息]"),
		NewSessionBackedMessage(nil, "[系统]焦点唤醒", testTime(2), "focus", nil, FocusCooldownWakeupSource),
		NewSessionBackedMessage(nil, "[系统]at唤醒", testTime(3), "focus-at", nil, FocusAtWakeupSource),
		userEntry(t, "真实消息", 4),
	}
	// Give the two one-shot focus entries their sources back (the constructor
	// normalises an empty source to "user").
	result, err := ProcessChatHistoryAfterCycle(history, 40, false, nil)
	if err != nil {
		t.Fatalf("ProcessChatHistoryAfterCycle: %v", err)
	}
	if result.RemovedCount != 2 {
		t.Fatalf("RemovedCount = %d, want 2", result.RemovedCount)
	}
	if len(result.History) != 2 {
		t.Fatalf("history length = %d, want 2", len(result.History))
	}
}

// TestProcessChatHistoryTrimsToTarget pins the `trim_threshold = ceil(2 × size)`
// rule and the `target = 1 × size` trimming.
func TestProcessChatHistoryTrimsToTarget(t *testing.T) {
	history := make([]LLMContextMessage, 0, 30)
	for index := 0; index < 30; index++ {
		history = append(history, userEntry(t, fmt.Sprintf("msg-%02d", index), index))
	}
	maxContextSize := 5
	result, err := ProcessChatHistoryAfterCycle(history, maxContextSize, false, nil)
	if err != nil {
		t.Fatalf("ProcessChatHistoryAfterCycle: %v", err)
	}
	// 30 > ceil(5 * 2) = 10 → trimmed down to <= 5 counted messages.
	if result.RemainingContextCount > maxContextSize {
		t.Fatalf("RemainingContextCount = %d, want <= %d", result.RemainingContextCount, maxContextSize)
	}
	if len(result.RemovedMessages) == 0 {
		t.Fatalf("RemovedMessages is empty, want the trimmed batch")
	}
	// The trimmed batch is the oldest prefix.
	first := result.RemovedMessages[0].ProcessedPlainText()
	if !strings.Contains(first, "msg-00") {
		t.Fatalf("first removed = %q, want the oldest message", first)
	}

	// Below the threshold nothing is removed.
	if TrimTargetRatio != 1.0 || TrimThresholdRatio != 2.0 {
		t.Fatalf("trim ratios changed: %v/%v", TrimTargetRatio, TrimThresholdRatio)
	}
	small, err := ProcessChatHistoryAfterCycle(history[:8], maxContextSize, false, nil)
	if err != nil {
		t.Fatalf("ProcessChatHistoryAfterCycle: %v", err)
	}
	if small.RemovedCount != 0 || len(small.RemovedMessages) != 0 {
		t.Fatalf("small history must not be trimmed: %+v", small)
	}
}

// TestTrimKeepsToolPairsTogether pins the tool-turn atomicity of
// `_trim_history_to_context_target`: a user anchor and its following tool turn
// must not be split.
func TestTrimKeepsToolPairsTogether(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "old-1", 0),
		userEntry(t, "old-2", 1),
		userEntry(t, "anchor", 2),
		callEntry(t, "turn-anchor", "call-anchor", "reply"),
		resultEntry(t, "turn-anchor", "call-anchor", "reply"),
		userEntry(t, "new-1", 3),
		userEntry(t, "new-2", 4),
		userEntry(t, "new-3", 5),
		userEntry(t, "new-4", 6),
		userEntry(t, "new-5", 7),
	}
	// maxContextSize=4 → trimThreshold=8, counted=10 → trim to 4.
	result, err := ProcessChatHistoryAfterCycle(history, 4, false, nil)
	if err != nil {
		t.Fatalf("ProcessChatHistoryAfterCycle: %v", err)
	}
	if result.RemainingContextCount > 4 {
		t.Fatalf("RemainingContextCount = %d, want <= 4", result.RemainingContextCount)
	}
	// The surviving history must never start with a function call or a tool
	// result; if the anchor user message was removed so was the whole turn.
	hasCall, hasResult, hasAnchor := false, false, false
	for _, entry := range result.History {
		switch typed := entry.(type) {
		case *ModelOutputContextMessage:
			if len(typed.ToolCalls()) > 0 {
				hasCall = true
			}
		case *ToolResultMessage:
			hasResult = true
		}
		if strings.Contains(entry.ProcessedPlainText(), "anchor") {
			hasAnchor = true
		}
	}
	if hasCall != hasResult || hasCall != hasAnchor {
		t.Fatalf("tool turn split across the trim boundary: call=%v result=%v anchor=%v", hasCall, hasResult, hasAnchor)
	}
}

// TestProcessChatHistoryRemovesOrphanToolResults pins the orphan cleanup inside
// the post-processor ("裁剪时保留工具调用/结果成对（孤儿清理）").
func TestProcessChatHistoryRemovesOrphanToolResults(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		resultEntry(t, "turn-gone", "call-gone", "reply"),
		userEntry(t, "u2", 1),
	}
	result, err := ProcessChatHistoryAfterCycle(history, 40, false, nil)
	if err != nil {
		t.Fatalf("ProcessChatHistoryAfterCycle: %v", err)
	}
	if result.RemovedCount != 1 {
		t.Fatalf("RemovedCount = %d, want 1", result.RemovedCount)
	}
	for _, entry := range result.History {
		if _, ok := entry.(*ToolResultMessage); ok {
			t.Fatalf("orphan tool result survived")
		}
	}
}

// TestProcessChatHistoryKeepsPendingWaitCall pins the `pending_call_ids`
// exemption passed by the runtime (`_get_pending_wait_tool_call_ids`).
func TestProcessChatHistoryKeepsPendingWaitCall(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		callEntry(t, "turn-wait", "call-wait", "wait"),
	}
	pending := map[string]bool{"call-wait": true}
	result, err := ProcessChatHistoryAfterCycle(history, 40, false, pending)
	if err != nil {
		t.Fatalf("ProcessChatHistoryAfterCycle: %v", err)
	}
	if result.RemovedCount != 0 || len(result.History) != 2 {
		t.Fatalf("pending wait call must survive: %+v", result)
	}
}

// TestProcessChatHistoryFoldsOldToolTurns pins
// `enable_context_optimization`: only the latest ASSISTANT_OPTIMIZATION_KEEP_COUNT
// output units survive, older tool turns are folded into one user message.
func TestProcessChatHistoryFoldsOldToolTurns(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		callEntry(t, "turn-1", "call-1", "query_memory"),
		resultEntry(t, "turn-1", "call-1", "query_memory"),
		userEntry(t, "u2", 1),
		callEntry(t, "turn-2", "call-2", "fetch_history"),
		resultEntry(t, "turn-2", "call-2", "fetch_history"),
		userEntry(t, "u3", 2),
		assistantEntry(t, "第三轮思考", "turn-3"),
		userEntry(t, "u4", 3),
		assistantEntry(t, "第四轮思考", "turn-4"),
		userEntry(t, "u5", 4),
		assistantEntry(t, "第五轮思考", "turn-5"),
	}
	result, err := ProcessChatHistoryAfterCycle(history, 40, true, nil)
	if err != nil {
		t.Fatalf("ProcessChatHistoryAfterCycle: %v", err)
	}

	// Five output units exist (turn-1, turn-2, turn-3..5); keep=3 → the two
	// older tool turns are folded, the latest three assistant items survive.
	foldedFounds := 0
	foldedIDs := make([]string, 0, 2)
	keptTexts := make([]string, 0, len(result.History))
	for _, entry := range result.History {
		keptTexts = append(keptTexts, entry.ProcessedPlainText())
		if sessionBacked, ok := entry.(*SessionBackedMessage); ok && sessionBacked.SourceKind == SourceOptimizedTools {
			foldedFounds++
			foldedIDs = append(foldedIDs, sessionBacked.MsgID)
		}
	}
	if foldedFounds != 2 {
		t.Fatalf("folded messages = %d, want 2 (turn-1, turn-2); history=%v", foldedFounds, keptTexts)
	}
	wantFoldIDs := []string{"optimized_tool_history:turn-1", "optimized_tool_history:turn-2"}
	for index, wantID := range wantFoldIDs {
		if foldedIDs[index] != wantID {
			t.Errorf("folded id[%d] = %q, want %q", index, foldedIDs[index], wantID)
		}
	}
	if AssistantOptimizationKeepCount != 3 {
		t.Fatalf("AssistantOptimizationKeepCount = %d, want 3", AssistantOptimizationKeepCount)
	}

	joined := strings.Join(keptTexts, "\n")
	if !strings.Contains(joined, "已折叠的历史工具调用") {
		t.Fatalf("folded text missing: %v", keptTexts)
	}
	if strings.Contains(joined, "call-1") == false {
		t.Fatalf("folded text must keep the tool_call_id: %v", keptTexts)
	}
	// `reply`/`wait` calls are dropped from the folded summary.
	for _, dropped := range []string{"  tool_name: reply", "  tool_name: wait"} {
		if strings.Contains(joined, dropped) {
			t.Fatalf("trimmed tool call %q must be dropped from the fold", dropped)
		}
	}
	// The folded turn's tool result must not survive separately.
	for _, entry := range result.History {
		if result2, ok := entry.(*ToolResultMessage); ok && result2.ToolCallID == "call-1" {
			t.Fatalf("the folded turn's tool result must not survive")
		}
	}
}

// TestFoldedToolHistoryDropNames pins TRIMMED_TOOL_CALL_DROP_NAMES.
func TestFoldedToolHistoryDropNames(t *testing.T) {
	want := []string{"continue", "finish", "no_action", "reply", "wait"}
	if len(TrimmedToolCallDropNames) != len(want) {
		t.Fatalf("TrimmedToolCallDropNames = %v, want %v", TrimmedToolCallDropNames, want)
	}
	for _, name := range want {
		if !TrimmedToolCallDropNames[name] {
			t.Errorf("TrimmedToolCallDropNames[%s] = false, want true", name)
		}
	}
	if FoldedToolComplexMessageThreshold != 1024 {
		t.Errorf("FoldedToolComplexMessageThreshold = %d, want 1024", FoldedToolComplexMessageThreshold)
	}
}

// TestFoldedToolHistoryUsesComplexMessageForLongText pins the >1024 rune branch
// that upgrades the fold to a ComplexSessionMessage.
func TestFoldedToolHistoryUsesComplexMessageForLongText(t *testing.T) {
	longContent := strings.Repeat("长", FoldedToolComplexMessageThreshold+10)
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		callEntry(t, "turn-1", "call-1", "query_memory"),
		func() *ToolResultMessage {
			result, err := NewToolResultMessage(longContent, testTime(3), "call-1", "turn-1", "query_memory", true)
			if err != nil {
				t.Fatalf("NewToolResultMessage: %v", err)
			}
			return result
		}(),
		userEntry(t, "u2", 1),
		assistantEntry(t, "二", "turn-2"),
		userEntry(t, "u3", 2),
		assistantEntry(t, "三", "turn-3"),
		userEntry(t, "u4", 3),
		assistantEntry(t, "四", "turn-4"),
	}
	result, err := ProcessChatHistoryAfterCycle(history, 40, true, nil)
	if err != nil {
		t.Fatalf("ProcessChatHistoryAfterCycle: %v", err)
	}
	found := false
	for _, entry := range result.History {
		complexMessage, ok := entry.(*ComplexSessionMessage)
		if !ok {
			continue
		}
		if complexMessage.SourceKind == SourceOptimizedTools && complexMessage.ComplexMessageType == "tool_history" {
			found = true
		}
	}
	if !found {
		t.Fatalf("long folded tool history must become a ComplexSessionMessage")
	}
}

// TestParseToolSearchResultToolNames pins the folded tool_search shortening.
func TestParseToolSearchResultToolNames(t *testing.T) {
	structured := `{"matched_tool_names": ["query_memory", "fetch_history"]}`
	names := parseToolSearchResultToolNames(structured)
	if len(names) != 2 || names[0] != "query_memory" || names[1] != "fetch_history" {
		t.Fatalf("names = %v", names)
	}

	lineForm := "- query_memory（长期记忆检索）\n- fetch_history"
	names = parseToolSearchResultToolNames(lineForm)
	if len(names) != 2 || names[0] != "query_memory" || names[1] != "fetch_history" {
		t.Fatalf("line form names = %v", names)
	}
	if got := parseToolSearchResultToolNames("plain text"); len(got) != 0 {
		t.Fatalf("plain text names = %v, want none", got)
	}
}

// TestFormatTrimmedToolSearchCall pins `- tool_search: names (query=...)`.
func TestFormatTrimmedToolSearchCall(t *testing.T) {
	result, err := NewToolResultMessage(`{"matched_tool_names": ["query_memory"]}`, testTime(1), "call-1", "turn-1", "tool_search", true)
	if err != nil {
		t.Fatalf("NewToolResultMessage: %v", err)
	}
	got := formatTrimmedToolSearchCall(map[string]any{"query": "记忆"}, result)
	if got != "- tool_search: query_memory (query=记忆)" {
		t.Fatalf("got %q", got)
	}
	got = formatTrimmedToolSearchCall(nil, nil)
	if got != "- tool_search: 无" {
		t.Fatalf("got %q", got)
	}
}

// TestBuildTrimmedAssistantToolUserMessage pins the folded text layout.
func TestBuildTrimmedAssistantToolUserMessage(t *testing.T) {
	call := callEntry(t, "turn-1", "call-1", "query_memory")
	result := resultEntry(t, "turn-1", "call-1", "query_memory")
	folded := buildTrimmedAssistantToolUserMessage(
		[]*ModelOutputContextMessage{call},
		map[string]*ToolResultMessage{"call-1": result},
	)
	if folded == nil {
		t.Fatalf("folded message is nil")
	}
	text := folded.ProcessedPlainText()
	want := strings.Join([]string{
		"[已折叠的历史工具调用]",
		"- tool_call_id: call-1",
		"  tool_name: query_memory",
		`  args: {"msg_id":"m1"}`,
		"  result_status: success",
		"  result: 结果",
	}, "\n")
	if text != want {
		t.Fatalf("folded text = %q, want %q", text, want)
	}
	if sessionBacked, ok := folded.(*SessionBackedMessage); !ok || sessionBacked.MsgID != "optimized_tool_history:turn-1" {
		t.Fatalf("folded entry = %+v", folded)
	}

	// A turn without tool calls fails like the Python `return None`.
	if got := buildTrimmedAssistantToolUserMessage([]*ModelOutputContextMessage{assistantEntry(t, "文本", "turn-x")}, nil); got != nil {
		t.Fatalf("folded without tool calls = %+v, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// process result accounting
// ---------------------------------------------------------------------------

// TestProcessChatHistoryAccountings pins removed/changed/remaining counters.
func TestProcessChatHistoryAccountings(t *testing.T) {
	uses := 1
	history := []LLMContextMessage{
		NewReferenceMessage("一次性", testTime(0), ReferenceToolHint, &uses, "[参考消息]"),
		userEntry(t, "u1", 1),
		callEntry(t, "turn-1", "call-1", "reply"),
		userEntry(t, "interleaved", 2),
		resultEntry(t, "turn-1", "call-1", "reply"),
	}
	result, err := ProcessChatHistoryAfterCycle(history, 40, false, nil)
	if err != nil {
		t.Fatalf("ProcessChatHistoryAfterCycle: %v", err)
	}
	if result.RemovedCount != 1 {
		t.Errorf("RemovedCount = %d, want 1", result.RemovedCount)
	}
	if result.ChangedCount != 2 {
		t.Errorf("ChangedCount = %d, want 2 (1 removed + 1 moved)", result.ChangedCount)
	}
	// u1 + the function call + the interleaved user message are counted; the tool
	// result has count_in_context=false and the expired reference is gone.
	if result.RemainingContextCount != 3 {
		t.Errorf("RemainingContextCount = %d, want 3", result.RemainingContextCount)
	}
	if len(result.History) != 4 {
		t.Fatalf("history length = %d, want 4", len(result.History))
	}
}

// TestToolResultMediaSourceKindConstant pins
// TOOL_RESULT_MEDIA_SOURCE_KIND.
func TestToolResultMediaSourceKindConstant(t *testing.T) {
	if SourceToolResultMedia != "tool_result_media" {
		t.Errorf("SourceToolResultMedia = %q", SourceToolResultMedia)
	}
	if SourceOptimizedTools != "optimized_tool_history" {
		t.Errorf("SourceOptimizedTools = %q", SourceOptimizedTools)
	}
	if SourceGuidedReply != "guided_reply" {
		t.Errorf("SourceGuidedReply = %q", SourceGuidedReply)
	}
	if SourcePerception != "perception" {
		t.Errorf("SourcePerception = %q", SourcePerception)
	}
}

// TestMidTermMemorySurvivesTrim pins the `is_mid_term_memory_message` skip of
// `_trim_history_to_context_target` (回想消息折叠为单条 mid_term_memory).
func TestMidTermMemorySurvivesTrim(t *testing.T) {
	summary := BuildMidTermMemoryComplexMessage(MidTermMemorySummaryPayload{
		TimeRange:    "2025-10-09 12:00:00 ~ 2025-10-09 12:05:00",
		Participants: []string{"小明"},
		Summary:      "聊了天气",
	}, []LLMContextMessage{userEntry(t, "x", 0)})

	history := []LLMContextMessage{summary}
	for index := 0; index < 20; index++ {
		history = append(history, userEntry(t, fmt.Sprintf("m%02d", index), index))
	}
	result, err := ProcessChatHistoryAfterCycle(history, 3, false, nil)
	if err != nil {
		t.Fatalf("ProcessChatHistoryAfterCycle: %v", err)
	}
	found := false
	for _, entry := range result.History {
		if IsMidTermMemoryMessage(entry) {
			found = true
		}
	}
	if !found {
		t.Fatalf("mid-term memory message was trimmed away")
	}
	if result.RemainingContextCount > 3 {
		t.Fatalf("RemainingContextCount = %d, want <= 3", result.RemainingContextCount)
	}
}

// ---------------------------------------------------------------------------
// mid-term memory helpers
// ---------------------------------------------------------------------------

func TestIsMidTermMemoryMessage(t *testing.T) {
	summary := BuildMidTermMemoryComplexMessage(MidTermMemorySummaryPayload{
		TimeRange:    "range",
		Participants: []string{"甲", "乙"},
		Summary:      "摘要",
		RecallCues:   []map[string]any{{"text": "线索"}},
	}, nil)
	if !IsMidTermMemoryMessage(summary) {
		t.Fatalf("IsMidTermMemoryMessage = false")
	}
	if summary.SourceKind != MidTermMemorySourceKind || summary.ComplexMessageType != MidTermMemoryComplexType {
		t.Fatalf("source/message type = %q/%q", summary.SourceKind, summary.ComplexMessageType)
	}
	if !strings.Contains(summary.ProcessedPlainText(), MidTermMemoryUserName) {
		t.Fatalf("visible text = %q", summary.ProcessedPlainText())
	}
	if !strings.HasPrefix(summary.MsgID, "mtm:") {
		t.Fatalf("message id = %q", summary.MsgID)
	}
	if !strings.HasPrefix(summary.PromptText, "<message msg_id=\"") {
		t.Fatalf("planner prefix = %q", summary.PromptText)
	}

	// Payload round-trip (the DictComponent carries the recall cues).
	payload := GetMidTermMemoryPayload(summary)
	if payload.Summary != "摘要" || payload.TimeRange != "range" || len(payload.Participants) != 2 ||
		len(payload.RecallCues) != 1 {
		t.Fatalf("payload = %+v", payload)
	}

	// A plain complex (forward) message is not a mid-term memory.
	forward := NewComplexSessionMessage(nil, "x", testTime(0), "f", nil, SourceUser, "x", "forward")
	if IsMidTermMemoryMessage(forward) {
		t.Fatalf("forward complex message must not be a mid-term memory")
	}
}

func TestBuildMidTermMemoryPreviewText(t *testing.T) {
	preview := BuildMidTermMemoryPreviewText(MidTermMemorySummaryPayload{
		TimeRange:    "  ",
		Participants: nil,
		Summary:      "",
	})
	want := strings.Join([]string{"[聊天回想]", "时间范围: 未知", "参与人物: 未知", "summary: 无"}, "\n")
	if preview != want {
		t.Fatalf("preview = %q, want %q", preview, want)
	}
}

func TestInsertMidTermMemoryMessageKeepsLatest(t *testing.T) {
	history := []LLMContextMessage{userEntry(t, "u1", 0)}
	for index := 0; index < 3; index++ {
		summary := BuildMidTermMemoryComplexMessage(MidTermMemorySummaryPayload{
			TimeRange: "range",
			Summary:   fmt.Sprintf("摘要%d", index),
		}, nil)
		history = InsertMidTermMemoryMessage(history, summary, 2)
	}

	count := 0
	for _, entry := range history {
		if IsMidTermMemoryMessage(entry) {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("mid-term memory count = %d, want 2", count)
	}
	if len(history) != 3 {
		t.Fatalf("history = %+v, want two recalls plus the user message", history)
	}
	if !IsMidTermMemoryMessage(history[0]) || !IsMidTermMemoryMessage(history[1]) {
		t.Fatalf("the recalls must sit right after each other: %+v", history)
	}
	if IsMidTermMemoryMessage(history[2]) {
		t.Fatalf("the user message must stay after the recalls: %+v", history)
	}

	// max_summary_count <= 0 drops every recall.
	cleaned := InsertMidTermMemoryMessage(history, nil, 0)
	for _, entry := range cleaned {
		if IsMidTermMemoryMessage(entry) {
			t.Fatalf("recalls must be dropped when max_summary_count <= 0")
		}
	}
}

func TestIsMidTermMemoryReferenceMessage(t *testing.T) {
	reference := NewReferenceMessage(MidTermMemoryReferenceMarker+"\n匹配段: x", testTime(0), ReferenceMemory, nil, "[参考消息]")
	if !IsMidTermMemoryReferenceMessage(reference) {
		t.Fatalf("IsMidTermMemoryReferenceMessage = false")
	}
	other := NewReferenceMessage("别的参考", testTime(0), ReferenceMemory, nil, "[参考消息]")
	if IsMidTermMemoryReferenceMessage(other) {
		t.Fatalf("a plain memory reference must not match")
	}
	if MidTermMemoryReferenceMarker != "【聊天回想-内部参考】" {
		t.Fatalf("marker = %q", MidTermMemoryReferenceMarker)
	}
}

func TestBuildSummaryMessageIDIsDeterministic(t *testing.T) {
	first := BuildSummaryMessageID(testTime(0), "range", []string{"甲"}, "摘要")
	second := BuildSummaryMessageID(testTime(0), "range", []string{"甲"}, "摘要")
	if first != second {
		t.Fatalf("summary message id is not deterministic: %q vs %q", first, second)
	}
	if !strings.HasPrefix(first, "mtm:") || len(strings.Split(first, ":")) != 3 {
		t.Fatalf("summary message id = %q", first)
	}
	if other := BuildSummaryMessageID(testTime(0), "range", []string{"甲"}, "另一个摘要"); other == first {
		t.Fatalf("different summaries must produce different ids")
	}
}

// TestToBase36 pins the base36 helper used by the summary message id.
func TestToBase36(t *testing.T) {
	cases := map[int64]string{
		0:    "0",
		35:   "z",
		36:   "10",
		1295: "zz",
		1296: "100",
	}
	for input, want := range cases {
		if got := toBase36(input); got != want {
			t.Errorf("toBase36(%d) = %q, want %q", input, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// context selection
// ---------------------------------------------------------------------------

// TestSelectLLMContextMessagesCountsOnlyCountedMessages pins
// `count_in_context=false 不计数` inside the real selection path.
func TestSelectLLMContextMessagesCountsOnlyCountedMessages(t *testing.T) {
	history := []LLMContextMessage{}
	for index := 0; index < 6; index++ {
		history = append(history, userEntry(t, fmt.Sprintf("m%d", index), index))
		uses := 100
		history = append(history, NewReferenceMessage(
			fmt.Sprintf("ref%d", index), testTime(index), ReferenceToolHint, &uses, "[参考消息]"))
	}
	selection, err := SelectLLMContextMessages(history, RequestKindPlanner, 2, nil)
	if err != nil {
		t.Fatalf("SelectLLMContextMessages: %v", err)
	}
	// base=2 → cache window 4, so the newest 4 *counted* messages plus every
	// interleaved reference message are sent.
	counted := CountInContextMessages(selection.History)
	if counted != 4 {
		t.Fatalf("counted messages = %d, want 4 (cache window)", counted)
	}
	references := 0
	for _, entry := range selection.History {
		if _, ok := entry.(*ReferenceMessage); ok {
			references++
		}
	}
	if references != 4 {
		t.Fatalf("reference messages = %d, want 4 (never counted, always adjacent)", references)
	}
	if ContextSelectionCacheStabilityRatio != 2.0 {
		t.Fatalf("ContextSelectionCacheStabilityRatio = %v, want 2.0", ContextSelectionCacheStabilityRatio)
	}
}

// TestSelectLLMContextMessagesKeepsToolTurnWhole pins
// `_expand_selected_tool_turns` + `normalize_tool_call_result_pairs`: hitting a
// tool loop item inside the budget must pull in the whole logical turn and the
// nearest preceding user anchor.
func TestSelectLLMContextMessagesKeepsToolTurnWhole(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "old", 0),
		userEntry(t, "anchor", 1),
		callEntry(t, "turn-1", "call-1", "reply"),
		resultEntry(t, "turn-1", "call-1", "reply"),
		userEntry(t, "new1", 2),
	}
	// base=1 → cache window 2. The backward walk takes new1 (1) and the tool
	// result (not counted), then stops at 2 on the function call; the anchor user
	// message sits outside the walk and can only come from the turn expansion.
	selection, err := SelectLLMContextMessages(history, RequestKindPlanner, 1, nil)
	if err != nil {
		t.Fatalf("SelectLLMContextMessages: %v", err)
	}
	hasCall, hasResult, hasAnchor := false, false, false
	for _, entry := range selection.History {
		if _, ok := entry.(*ToolResultMessage); ok {
			hasResult = true
		}
		if output, ok := entry.(*ModelOutputContextMessage); ok && len(output.ToolCalls()) > 0 {
			hasCall = true
		}
		if strings.Contains(entry.ProcessedPlainText(), "anchor") {
			hasAnchor = true
		}
	}
	if !hasCall || !hasResult {
		t.Fatalf("tool pair incomplete: call=%v result=%v (%s)", hasCall, hasResult, selection.SelectionReason)
	}
	if !hasAnchor {
		t.Fatalf("the tool turn's user anchor must be pulled in as well (%s)", selection.SelectionReason)
	}
	// The result must directly follow the call (protocol order).
	callIndex, resultIndex := -1, -1
	for index, entry := range selection.History {
		if output, ok := entry.(*ModelOutputContextMessage); ok && len(output.ToolCalls()) > 0 {
			callIndex = index
		}
		if _, ok := entry.(*ToolResultMessage); ok {
			resultIndex = index
		}
	}
	if resultIndex != callIndex+1 {
		t.Fatalf("call at %d, result at %d, want the result directly after the call", callIndex, resultIndex)
	}
	if !strings.Contains(selection.SelectionReason, "cache_window 1->2") {
		t.Fatalf("selection reason = %q", selection.SelectionReason)
	}
}

// TestSelectLLMContextMessagesAlwaysSendsContextRestoreReferences pins
// `_collect_always_selected_reference_indices`.
func TestSelectLLMContextMessagesAlwaysSendsContextRestoreReferences(t *testing.T) {
	restore := NewReferenceMessage("离线恢复提示", testTime(0), ReferenceContextRestore, nil, "[参考消息]")
	history := []LLMContextMessage{restore}
	for index := 0; index < 30; index++ {
		history = append(history, userEntry(t, fmt.Sprintf("m%d", index), index))
	}
	selection, err := SelectLLMContextMessages(history, RequestKindPlanner, 2, nil)
	if err != nil {
		t.Fatalf("SelectLLMContextMessages: %v", err)
	}
	for _, entry := range selection.History {
		if entry == LLMContextMessage(restore) {
			return
		}
	}
	t.Fatalf("context_restore reference must always be selected")
}

// TestSelectLLMContextMessagesDropsMidTermMemoryFromPlanner pins
// `_filter_history_for_request_kind`: planner requests never see the folded
// mid-term memory entries.
func TestSelectLLMContextMessagesDropsMidTermMemoryFromPlanner(t *testing.T) {
	summary := BuildMidTermMemoryComplexMessage(MidTermMemorySummaryPayload{Summary: "摘要"}, nil)
	history := []LLMContextMessage{summary, userEntry(t, "u1", 1)}
	selection, err := SelectLLMContextMessages(history, RequestKindPlanner, 40, nil)
	if err != nil {
		t.Fatalf("SelectLLMContextMessages: %v", err)
	}
	for _, entry := range selection.History {
		if IsMidTermMemoryMessage(entry) {
			t.Fatalf("planner selection must not include mid-term memory entries")
		}
	}
	if len(selection.History) != 1 {
		t.Fatalf("history length = %d, want 1", len(selection.History))
	}
}

// TestSelectLLMContextMessagesEmptyHistory pins the "0 messages" reason.
func TestSelectLLMContextMessagesEmptyHistory(t *testing.T) {
	selection, err := SelectLLMContextMessages(nil, RequestKindPlanner, 40, nil)
	if err != nil {
		t.Fatalf("SelectLLMContextMessages: %v", err)
	}
	if selection.History != nil || selection.SelectionReason != "实际发送 0 条消息（tool 0 条，普通消息 0 条）" {
		t.Fatalf("selection = %+v", selection)
	}
}

// TestDropHeadContextMessages pins `_drop_head_context_messages`.
func TestDropHeadContextMessages(t *testing.T) {
	history := []LLMContextMessage{}
	for index := 0; index < 10; index++ {
		history = append(history, userEntry(t, fmt.Sprintf("m%d", index), index))
	}
	trimThreshold := 5
	trimmed := DropHeadContextMessages(history, 3, &trimThreshold)
	if len(trimmed) != 7 || trimmed[0] != LLMContextMessage(history[3]) {
		t.Fatalf("trimmed length = %d", len(trimmed))
	}
	// Below the trim threshold nothing is dropped.
	trimmed = DropHeadContextMessages(history[:4], 3, &trimThreshold)
	if len(trimmed) != 4 {
		t.Fatalf("below threshold length = %d, want 4", len(trimmed))
	}
	// drop count 0 is a copy.
	trimmed = DropHeadContextMessages(history, 0, nil)
	if len(trimmed) != 10 || &trimmed[0] == &history[0] && len(trimmed) == 0 {
		t.Fatalf("drop 0 must copy the history")
	}
}

// TestPlannerPrefixLayout pins planner_messages.build_planner_prefix.
func TestPlannerPrefixLayout(t *testing.T) {
	timestamp := testTime(0)
	longID := "ROBOT1.0_abcdefghijklmnopqrstuvwxyz0123456789"

	got := BuildPlannerPrefix(timestamp, "小明", "小明同学", longID, "session-1",
		[]string{longID, "short", longID}, true, true, true)
	want := "<message msg_id=\"mhitr4b\" chat_id=\"session-1\" quote=\"mhitr4b,short\" time=\"12:00:00\" user=\"小明\" group_card=\"小明同学\" is_self_message=\"true\">\n"
	if got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}

	minimal := BuildPlannerPrefix(timestamp, "小明", "", "", "", nil, false, false, false)
	if minimal != "<message time=\"12:00:00\" user=\"小明\">\n" {
		t.Fatalf("minimal prefix = %q", minimal)
	}
}

func TestBuildPlannerUserPrefixFromSessionMessage(t *testing.T) {
	sessionMessage := message.NewSessionMessage("ROBOT1.0_abcdefghijklmnopqrstuvwxyz0123456789", testTime(0), "qq")
	sessionMessage.MessageInfo.UserInfo = message.NewUserInfo("10001", "小明", "小明同学")
	sessionMessage.SessionID = "session-1"
	sessionMessage.RawMessage = message.NewMessageSequence(message.NewTextComponent("正文"))
	sessionMessage.RawMessage.Reply("999")

	got := BuildPlannerUserPrefixFromSessionMessage(sessionMessage, PlannerPrefixOptions{})
	want := "<message msg_id=\"mhitr4b\" quote=\"999\" time=\"12:00:00\" user=\"小明\" group_card=\"小明同学\">\n"
	if got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}

	// is_notify hides the message id (`include_message_id and not is_notify`).
	sessionMessage.IsNotify = true
	got = BuildPlannerUserPrefixFromSessionMessage(sessionMessage, PlannerPrefixOptions{})
	if strings.Contains(got, "msg_id=") {
		t.Fatalf("notify prefix must hide msg_id: %q", got)
	}

	// Self messages are flagged.
	sessionMessage.IsNotify = false
	got = BuildPlannerUserPrefixFromSessionMessage(sessionMessage, PlannerPrefixOptions{IsSelfMessage: true})
	if !strings.Contains(got, `is_self_message="true"`) {
		t.Fatalf("self prefix = %q", got)
	}
}

func TestBuildSessionBackedTextMessage(t *testing.T) {
	entry := BuildSessionBackedTextMessage(
		"小明", "你好", testTime(0), SourceGuidedReply, "", "ROBOT1.0_abcdefghijklmnopqrstuvwxyz0123456789",
		"session-1", nil, true, false, true,
	)
	if entry.SourceKind != SourceGuidedReply {
		t.Fatalf("source = %q", entry.SourceKind)
	}
	if !strings.HasPrefix(entry.RawMessage.Components[0].(*message.TextComponent).Text, "<message ") {
		t.Fatalf("raw text = %q", entry.RawMessage.Components[0].(*message.TextComponent).Text)
	}
	if entry.ProcessedPlainText() != "12:00:00[msg_id:mhitr4b][小明]你好" {
		t.Fatalf("visible text = %q", entry.ProcessedPlainText())
	}
}

// TestClearContextHelpers pins clear_context.py.
func TestClearContextHelpers(t *testing.T) {
	if !IsClearContextCommand(" /clear ") || IsClearContextCommand("/clear now") {
		t.Fatalf("IsClearContextCommand mismatch")
	}
	sessionMessage := message.NewSessionMessage("m1", testTime(0), "qq")
	MarkClearContextCommand(sessionMessage)
	if !IsClearContextMarker(sessionMessage) || !sessionMessage.IsCommand {
		t.Fatalf("marker not set")
	}
	before := message.NewSessionMessage("m0", testTime(0), "qq")
	after := message.NewSessionMessage("m2", testTime(1), "qq")
	selected := SelectMessagesAfterLatestClearMarker([]*message.SessionMessage{before, sessionMessage, after})
	if len(selected) != 1 || selected[0] != after {
		t.Fatalf("selected = %+v", selected)
	}
	if ClearContextCommand != "/clear" || ClearContextMarkerKey != "maisaka_context_cleared" ||
		ClearContextInterceptLevel != 1 {
		t.Fatalf("clear-context constants changed")
	}
}
