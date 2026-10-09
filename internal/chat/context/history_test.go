package context

import (
	"testing"
	"time"

	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// builders
// ---------------------------------------------------------------------------

func testTime(seconds int) time.Time {
	return time.Date(2025, 10, 9, 12, 0, 0, 0, time.UTC).Add(time.Duration(seconds) * time.Second)
}

func userEntry(testing *testing.T, text string, at int) *SessionBackedMessage {
	testing.Helper()
	return NewSessionBackedMessage(
		message.NewMessageSequence(message.NewTextComponent(text)),
		text, testTime(at), "msg-"+text, nil, SourceUser,
	)
}

func assistantEntry(t *testing.T, text string, logicalTurnID string) *ModelOutputContextMessage {
	t.Helper()
	item, err := llm.NewContextItemBuilder().
		SetRole(llm.RoleAssistant).
		SetMeta(llm.ContextItemMeta{
			ItemID:        NewContextItemID(),
			LogicalTurnID: logicalTurnID,
			Timestamp:     testTime(1),
		}).
		AddTextPart(text).
		Build()
	if err != nil {
		t.Fatalf("build assistant item: %v", err)
	}
	return NewModelOutputContextMessage(item, SourceAssistant)
}

func callEntry(t *testing.T, logicalTurnID string, callID string, funcName string) *ModelOutputContextMessage {
	t.Helper()
	item, err := llm.NewFunctionCallItem(logicalTurnID, callID, funcName, map[string]any{"msg_id": "m1"})
	if err != nil {
		t.Fatalf("build function call item: %v", err)
	}
	item.Meta.Timestamp = testTime(2)
	return NewModelOutputContextMessage(item, SourceAssistant)
}

func resultEntry(t *testing.T, logicalTurnID string, callID string, toolName string) *ToolResultMessage {
	t.Helper()
	result, err := NewToolResultMessage("结果", testTime(3), callID, logicalTurnID, toolName, true)
	if err != nil {
		t.Fatalf("build tool result: %v", err)
	}
	return result
}

// ---------------------------------------------------------------------------
// orphan pruning
// ---------------------------------------------------------------------------

func TestDropLeadingOrphanToolResults(t *testing.T) {
	orphan := resultEntry(t, "turn-orphan", "call-orphan", "reply")
	kept := resultEntry(t, "turn-1", "call-1", "reply")
	history := []LLMContextMessage{
		orphan,
		kept,
		assistantEntry(t, "正文", "turn-1"),
		callEntry(t, "turn-1", "call-1", "reply"),
	}
	filtered, removed := DropLeadingOrphanToolResults(history)
	if removed != 1 || len(filtered) != 3 {
		t.Fatalf("removed = %d, len(filtered) = %d, want 1/3", removed, len(filtered))
	}
	if filtered[0] != kept {
		t.Fatalf("first kept entry = %+v", filtered[0])
	}
}

func TestDropOrphanToolResultsRemovesWholeTurn(t *testing.T) {
	// turn-1 is answered; turn-2's call is missing and its result is an orphan.
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		callEntry(t, "turn-1", "call-1", "reply"),
		resultEntry(t, "turn-1", "call-1", "reply"),
		assistantEntry(t, "无人回答的调用", "turn-2"),
		resultEntry(t, "turn-2", "call-2", "reply"),
	}
	filtered, removed := DropOrphanToolResults(history)
	if removed != 2 {
		t.Fatalf("removed = %d, want 2 (the assistant item and the orphan result)", removed)
	}
	if len(filtered) != 3 {
		t.Fatalf("len(filtered) = %d, want 3", len(filtered))
	}
	for _, entry := range filtered {
		if LogicalTurnIDOf(entry) == "turn-2" {
			t.Fatalf("turn-2 must be dropped entirely")
		}
	}
}

func TestDropUnansweredToolCallsRespectsPendingCallIDs(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		callEntry(t, "turn-wait", "call-wait", "wait"),
	}
	// Without the pending exemption the unanswered call is dropped.
	filtered, removed := DropUnansweredToolCalls(history, nil)
	if removed != 1 || len(filtered) != 1 {
		t.Fatalf("without pending: removed = %d, len = %d, want 1/1", removed, len(filtered))
	}
	// With the same id declared pending the call survives (wait 工具挂起).
	filtered, removed = DropUnansweredToolCalls(history, map[string]bool{"call-wait": true})
	if removed != 0 || len(filtered) != 2 {
		t.Fatalf("with pending: removed = %d, len = %d, want 0/2", removed, len(filtered))
	}
}

func TestNormalizeToolResultOrderMovesSeparatedResults(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		callEntry(t, "turn-1", "call-1", "reply"),
		userEntry(t, "interleaved", 1),
		resultEntry(t, "turn-1", "call-1", "reply"),
	}
	normalized, moved := NormalizeToolResultOrder(history)
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}
	if len(normalized) != 4 {
		t.Fatalf("len(normalized) = %d, want 4", len(normalized))
	}
	if _, ok := normalized[1].(*ModelOutputContextMessage); !ok {
		t.Fatalf("normalized[1] = %T, want the function call", normalized[1])
	}
	if _, ok := normalized[2].(*ToolResultMessage); !ok {
		t.Fatalf("normalized[2] = %T, want the tool result directly after the call", normalized[2])
	}
}

func TestNormalizeToolResultOrderNoopWhenOrdered(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		callEntry(t, "turn-1", "call-1", "reply"),
		resultEntry(t, "turn-1", "call-1", "reply"),
	}
	normalized, moved := NormalizeToolResultOrder(history)
	if moved != 0 || len(normalized) != 3 {
		t.Fatalf("moved = %d, len = %d, want 0/3", moved, len(normalized))
	}
}

func TestNormalizeToolCallResultPairsStats(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		callEntry(t, "turn-1", "call-1", "reply"),
		userEntry(t, "interleaved", 1),
		resultEntry(t, "turn-1", "call-1", "reply"),
		assistantEntry(t, "孤儿 turn", "turn-orphan"),
		resultEntry(t, "turn-orphan", "call-orphan", "reply"),
	}
	normalized, stats, err := NormalizeToolCallResultPairs(history, nil)
	if err != nil {
		t.Fatalf("NormalizeToolCallResultPairs: %v", err)
	}
	// drop_orphan_tool_results counts every removed entry of the orphan turn
	// (the assistant item *and* its result), mirroring `removed_count`.
	if stats.OrphanToolResults != 2 {
		t.Errorf("OrphanToolResults = %d, want 2", stats.OrphanToolResults)
	}
	if stats.MovedToolResults != 1 {
		t.Errorf("MovedToolResults = %d, want 1", stats.MovedToolResults)
	}
	if len(normalized) != 4 {
		t.Fatalf("len(normalized) = %d, want 4", len(normalized))
	}
}

func TestAnalyzeContextItemRelationsDetectsMisorderedPair(t *testing.T) {
	call := callEntry(t, "turn-1", "call-1", "reply").OutputItem
	result := resultEntry(t, "turn-1", "call-1", "reply").ToContextItem(false)
	report := AnalyzeContextItemRelations([]*llm.ContextItem{result, call}, nil)
	if len(report.MisplacedOutputCallIDs) != 1 || report.MisplacedOutputCallIDs[0] != "call-1" {
		t.Fatalf("misplaced = %v, want [call-1]", report.MisplacedOutputCallIDs)
	}
	if len(report.InvalidTurnIDs) != 1 || report.InvalidTurnIDs[0] != "turn-1" {
		t.Fatalf("invalid turns = %v, want [turn-1]", report.InvalidTurnIDs)
	}
}

func TestCollectAvailableToolCallIDsAndFoldedHistory(t *testing.T) {
	// `_collect_folded_tool_history_call_ids` reads
	// `message.processed_plain_text`, which for a SessionBackedMessage is the
	// visible text — so the folded body must live there.
	foldedText := "[已折叠的历史工具调用]\n- tool_call_id: call-folded\n  tool_name: query_memory\n  args: {}"
	folded := NewSessionBackedMessage(
		message.NewMessageSequence(message.NewTextComponent(foldedText)),
		foldedText, testTime(5), "optimized_tool_history:turn-9", nil, SourceOptimizedTools,
	)
	history := []LLMContextMessage{callEntry(t, "turn-1", "call-1", "reply"), folded}
	callIDs := CollectAvailableToolCallIDs(history)
	if !callIDs["call-1"] || callIDs["call-folded"] {
		t.Fatalf("CollectAvailableToolCallIDs = %v", callIDs)
	}
	foldedIDs := collectFoldedToolHistoryCallIDs(history)
	if !foldedIDs["call-folded"] {
		t.Fatalf("folded call ids = %v, want call-folded", foldedIDs)
	}
}

func TestIsOrphanToolResultMediaMessage(t *testing.T) {
	media := NewSessionBackedMessage(
		message.NewMessageSequence(message.NewTextComponent("[图片]")),
		"[图片]", testTime(6), "tool_result:call-1:0", nil, SourceToolResultMedia,
	)
	if isOrphanToolResultMediaMessage(media, map[string]bool{"call-1": true}) {
		t.Fatalf("owned media must not be treated as orphan")
	}
	if !isOrphanToolResultMediaMessage(media, map[string]bool{}) {
		t.Fatalf("media without an owning call must be treated as orphan")
	}
	// A different source kind is never considered orphan media.
	notMedia := NewSessionBackedMessage(nil, "x", testTime(6), "tool_result:call-1:0", nil, SourceUser)
	if isOrphanToolResultMediaMessage(notMedia, map[string]bool{}) {
		t.Fatalf("non-media source must not be treated as orphan media")
	}
}

// TestDropOrphanToolResultsKeepsPairedTurn pins the "工具调用/结果成对" rule: a
// complete turn survives untouched.
func TestDropOrphanToolResultsKeepsPairedTurn(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		callEntry(t, "turn-1", "call-1", "reply"),
		resultEntry(t, "turn-1", "call-1", "reply"),
		userEntry(t, "u2", 1),
	}
	filtered, removed := DropOrphanToolResults(history)
	if removed != 0 || len(filtered) != 4 {
		t.Fatalf("removed = %d, len = %d, want 0/4", removed, len(filtered))
	}
}

// TestDropOrphanToolResultsIgnoresUnansweredCalls pins the Python split:
// `drop_orphan_tool_results` only removes orphan *results* (and their turns),
// while unanswered *calls* are handled by `drop_unanswered_tool_calls`.
func TestDropOrphanToolResultsIgnoresUnansweredCalls(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		callEntry(t, "turn-1", "call-1", "reply"),
		userEntry(t, "u2", 1),
	}
	filtered, removed := DropOrphanToolResults(history)
	if removed != 0 || len(filtered) != 3 {
		t.Fatalf("DropOrphanToolResults: removed = %d, len = %d, want 0/3", removed, len(filtered))
	}

	filtered, removed = DropUnansweredToolCalls(history, nil)
	if removed != 1 {
		t.Fatalf("DropUnansweredToolCalls: removed = %d, want 1", removed)
	}
	if len(filtered) != 2 {
		t.Fatalf("len(filtered) = %d, want 2", len(filtered))
	}
	for _, entry := range filtered {
		if LogicalTurnIDOf(entry) == "turn-1" {
			t.Fatalf("the unanswered call must be dropped")
		}
	}
}

// TestNormalizeToolCallResultPairsRepairsSeparatedPair pins
// `normalize_tool_call_result_pairs` returning moved/repaired stats.
func TestNormalizeToolCallResultPairsRepairsSeparatedPair(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		callEntry(t, "turn-1", "call-1", "reply"),
		userEntry(t, "interleaved", 1),
		resultEntry(t, "turn-1", "call-1", "reply"),
	}
	normalized, stats, err := NormalizeToolCallResultPairs(history, nil)
	if err != nil {
		t.Fatalf("NormalizeToolCallResultPairs: %v", err)
	}
	if stats.MovedToolResults != 1 || stats.OrphanToolResults != 0 || stats.InvalidToolTurns != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(normalized) != 4 {
		t.Fatalf("len = %d, want 4", len(normalized))
	}
	if _, ok := normalized[2].(*ToolResultMessage); !ok {
		t.Fatalf("normalized[2] = %T, want the tool result", normalized[2])
	}
}

// TestCollectToolTurnAnchorIndices pins
// `collect_tool_turn_anchor_indices`.
func TestCollectToolTurnAnchorIndices(t *testing.T) {
	history := []LLMContextMessage{
		userEntry(t, "u1", 0),
		userEntry(t, "u2", 1),
		callEntry(t, "turn-1", "call-1", "reply"),
		resultEntry(t, "turn-1", "call-1", "reply"),
	}
	anchors := CollectToolTurnAnchorIndices(history, map[string]bool{"turn-1": true})
	if anchors["turn-1"] != 1 {
		t.Fatalf("anchor = %d, want 1 (the nearest preceding user entry)", anchors["turn-1"])
	}
	// Without any preceding user entry the turn gets no anchor.
	noAnchor := CollectToolTurnAnchorIndices(history[2:], map[string]bool{"turn-1": true})
	if _, exists := noAnchor["turn-1"]; exists {
		t.Fatalf("anchor = %v, want none", noAnchor)
	}
}

// ---------------------------------------------------------------------------
// prefixed sequences / visible text
// ---------------------------------------------------------------------------

func TestBuildPrefixedMessageSequence(t *testing.T) {
	sequence := message.NewMessageSequence(message.NewTextComponent("正文"))
	sequence.Reply("123")
	prefixed := BuildPrefixedMessageSequence(sequence, "<message x>\n")
	if len(prefixed.Components) != 1 {
		t.Fatalf("components = %d, want 1 (reply dropped)", len(prefixed.Components))
	}
	if got := prefixed.Components[0].(*message.TextComponent).Text; got != "<message x>\n正文" {
		t.Fatalf("text = %q", got)
	}

	// A leading non-text component inserts a new text component at the front.
	emojiFirst := message.NewMessageSequence(&message.EmojiComponent{
		ByteComponent: message.NewByteComponent("[表情包]", nil, ""),
	})
	prefixed = BuildPrefixedMessageSequence(emojiFirst, "<message x>\n")
	if len(prefixed.Components) != 2 {
		t.Fatalf("components = %d, want 2", len(prefixed.Components))
	}
	if prefixed.Components[0].(*message.TextComponent).Text != "<message x>\n" {
		t.Fatalf("inserted prefix = %q", prefixed.Components[0].(*message.TextComponent).Text)
	}
}

func TestBuildSessionMessageVisibleTextLayout(t *testing.T) {
	sessionMessage := message.NewSessionMessage("ROBOT1.0_abcdefghijklmnopqrstuvwxyz0123456789", testTime(0), "qq")
	sessionMessage.MessageInfo.UserInfo = message.NewUserInfo("10001", "小明", "小明同学")
	sessionMessage.RawMessage = message.NewMessageSequence(message.NewTextComponent("你好"))
	sessionMessage.IsNotify = false

	got := BuildSessionMessageVisibleText(sessionMessage, sessionMessage.RawMessage, true)
	want := "12:00:00[msg_id:mhitr4b][小明同学]你好"
	if got != want {
		t.Fatalf("visible text = %q, want %q", got, want)
	}

	// Notify messages hide the message id.
	sessionMessage.IsNotify = true
	got = BuildSessionMessageVisibleText(sessionMessage, sessionMessage.RawMessage, true)
	if got != "12:00:00[小明同学]你好" {
		t.Fatalf("notify visible text = %q", got)
	}

	// include_reply_components=false drops the quote component.
	sessionMessage.IsNotify = false
	sessionMessage.RawMessage = message.NewMessageSequence(message.NewTextComponent("正文"))
	sessionMessage.RawMessage.Reply("999")
	withReply := BuildSessionMessageVisibleText(sessionMessage, sessionMessage.RawMessage, true)
	if !contains(withReply, "[引用消息]999") {
		t.Fatalf("with reply = %q", withReply)
	}
	withoutReply := BuildSessionMessageVisibleText(sessionMessage, sessionMessage.RawMessage, false)
	if contains(withoutReply, "[引用消息]") {
		t.Fatalf("without reply = %q", withoutReply)
	}
}
