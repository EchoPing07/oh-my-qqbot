package context

import (
	"strings"
	"testing"

	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/message"
)

// TestFoldedHistoryCallIDsAreMediaOwners covers
// `_collect_available_tool_call_ids` / `_collect_folded_tool_history_call_ids`.
func TestFoldedHistoryCallIDsAreMediaOwners(t *testing.T) {
	foldedText := strings.Join([]string{
		"[已折叠的历史工具调用]",
		"- tool_call_id: call-folded",
		"  tool_name: query_memory",
		"  args: {}",
	}, "\n")
	// The folded entry keeps the same text in both `visible_text` (used by
	// `_parse_folded_tool_history_call_ids`) and the raw sequence.
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

	// Folded ids are accepted as media owners by `drop_orphan_tool_results`.
	media := NewSessionBackedMessage(
		message.NewMessageSequence(message.NewTextComponent("[图片]")),
		"[图片]", testTime(6), "tool_result:call-folded:0", nil, SourceToolResultMedia,
	)
	_, removed := DropOrphanToolResults([]LLMContextMessage{folded, media})
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (the folded history owns the media)", removed)
	}
	_, removed = DropOrphanToolResults([]LLMContextMessage{media})
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (no folded owner present)", removed)
	}
}

// TestAnalyzeContextItemRelationsCountsEveryReportField pins the report fields
// used by the HISTORY validation.
func TestAnalyzeContextItemRelationsCountsEveryReportField(t *testing.T) {
	call := callEntry(t, "turn-1", "call-1", "reply").OutputItem
	result := resultEntry(t, "turn-1", "call-1", "reply").ToContextItem(false)
	// Same item_id twice → duplicate_item_ids + invalid turn.
	duplicate := call
	report := AnalyzeContextItemRelations([]*llm.ContextItem{call, result, duplicate}, nil)
	if len(report.DuplicateItemIDs) != 1 {
		t.Errorf("DuplicateItemIDs = %v, want one id", report.DuplicateItemIDs)
	}
	if len(report.DuplicateCallIDs) != 1 {
		t.Errorf("DuplicateCallIDs = %v, want one id", report.DuplicateCallIDs)
	}
	if len(report.InvalidTurnIDs) != 1 || report.InvalidTurnIDs[0] != "turn-1" {
		t.Errorf("InvalidTurnIDs = %v, want [turn-1]", report.InvalidTurnIDs)
	}
	if len(report.UnansweredCallIDs) != 0 {
		t.Errorf("UnansweredCallIDs = %v, want none", report.UnansweredCallIDs)
	}

	// An unanswered call (no result) is reported.
	report = AnalyzeContextItemRelations([]*llm.ContextItem{call}, nil)
	if len(report.UnansweredCallIDs) != 1 || report.UnansweredCallIDs[0] != "call-1" {
		t.Errorf("UnansweredCallIDs = %v, want [call-1]", report.UnansweredCallIDs)
	}
	// …unless it is declared pending.
	report = AnalyzeContextItemRelations([]*llm.ContextItem{call}, map[string]bool{"call-1": true})
	if len(report.UnansweredCallIDs) != 0 {
		t.Errorf("pending calls must be exempt, got %v", report.UnansweredCallIDs)
	}

	// An orphan output (result without call) is reported.
	report = AnalyzeContextItemRelations([]*llm.ContextItem{result}, nil)
	if len(report.OrphanOutputCallIDs) != 1 || report.OrphanOutputCallIDs[0] != "call-1" {
		t.Errorf("OrphanOutputCallIDs = %v, want [call-1]", report.OrphanOutputCallIDs)
	}
}
