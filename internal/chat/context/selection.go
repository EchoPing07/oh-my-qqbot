package context

import (
	"fmt"
	"sort"
)

// This file mirrors MaisakaChatLoopService.select_llm_context_messages and its
// helpers in 参考/MaiBot/src/maisaka/chat_loop_service.py:
// `_filter_history_for_request_kind`, `_collect_always_selected_reference_indices`,
// `_expand_selected_tool_turns` and `_get_history_logical_turn_id`.

// ContextSelectionCacheStabilityRatio mirrors
// CONTEXT_SELECTION_CACHE_STABILITY_RATIO (the cache window is widened by this
// factor so prompt-prefix caching stays stable across calls).
const ContextSelectionCacheStabilityRatio = 2.0

// Request kind values mirroring the `request_kind` argument of
// select_llm_context_messages.
const (
	RequestKindPlanner            = "planner"
	RequestKindExpressionSelector = "expression_selector"
)

// ContextSelection mirrors the (history, selection_reason) tuple returned by
// select_llm_context_messages. The Go struct also surfaces the accounting
// numbers the Python code embeds only in `selection_reason`.
type ContextSelection struct {
	History         []LLMContextMessage
	SelectionReason string
	// Overflow reports how many extra context messages the tool-turn closure
	// added beyond the effective window (Python `tool_turn_overflow`).
	Overflow int
}

// SelectLLMContextMessages mirrors chat_loop_service.select_llm_context_messages.
//
// maxContextSize <= 0 falls back to 1, exactly like the Python
// `max(1, int(max_context_size or global_config.chat.max_context_size))` when the
// caller passes the configured value.
func SelectLLMContextMessages(
	chatHistory []LLMContextMessage,
	requestKind string,
	maxContextSize int,
	enableVisualMessage *bool,
) (ContextSelection, error) {
	filtered := FilterHistoryForRequestKind(chatHistory, requestKind)
	baseContextSize := maxContextSize
	if baseContextSize < 1 {
		baseContextSize = 1
	}
	effectiveContextSize := baseContextSize
	if widened := int(float64(baseContextSize) * ContextSelectionCacheStabilityRatio); widened > effectiveContextSize {
		effectiveContextSize = widened
	}

	activeEnableVisual := true
	if enableVisualMessage != nil {
		activeEnableVisual = *enableVisualMessage
	} else {
		activeEnableVisual = resolveEnableVisualMessage(requestKind)
	}

	alwaysSelectedIndices := collectAlwaysSelectedReferenceIndices(filtered, activeEnableVisual)

	selectedIndices := make([]int, 0)
	countedMessageCount := 0
	for index := len(filtered) - 1; index >= 0; index-- {
		entry := filtered[index]
		if len(BuildContextItemsFromHistoryEntry(entry, activeEnableVisual)) == 0 {
			continue
		}
		selectedIndices = append(selectedIndices, index)
		if entry.CountInContext() {
			countedMessageCount++
			if countedMessageCount >= effectiveContextSize {
				break
			}
		}
	}

	selectedSet := map[int]bool{}
	for _, index := range alwaysSelectedIndices {
		selectedSet[index] = true
	}
	for _, index := range selectedIndices {
		selectedSet[index] = true
	}
	if len(selectedSet) == 0 {
		return ContextSelection{
			History:         nil,
			SelectionReason: "实际发送 0 条消息（tool 0 条，普通消息 0 条）",
		}, nil
	}
	orderedIndices := make([]int, 0, len(selectedSet))
	for index := range selectedSet {
		orderedIndices = append(orderedIndices, index)
	}
	sort.Ints(orderedIndices)
	selectedHistory := make([]LLMContextMessage, 0, len(orderedIndices))
	for _, index := range orderedIndices {
		selectedHistory = append(selectedHistory, filtered[index])
	}

	selectedHistory = expandSelectedToolTurns(filtered, selectedHistory)
	selectedHistory, _, err := NormalizeToolCallResultPairs(selectedHistory, nil)
	if err != nil {
		return ContextSelection{}, err
	}

	selectedContextCount := countInContext(selectedHistory)
	toolTurnOverflow := selectedContextCount - effectiveContextSize
	if toolTurnOverflow < 0 {
		toolTurnOverflow = 0
	}
	toolMessageCount := 0
	for _, entry := range selectedHistory {
		if _, ok := entry.(*ToolResultMessage); ok {
			toolMessageCount++
		}
	}
	normalMessageCount := len(selectedHistory) - toolMessageCount

	overflowText := ""
	if toolTurnOverflow > 0 {
		overflowText = fmt.Sprintf("|tool_turn_overflow +%d", toolTurnOverflow)
	}
	selectionReason := fmt.Sprintf(
		"实际发送 %d 条消息|消息 %d 条|tool %d 条|cache_window %d->%d%s",
		len(selectedHistory), normalMessageCount, toolMessageCount, baseContextSize, effectiveContextSize, overflowText,
	)
	return ContextSelection{
		History:         selectedHistory,
		SelectionReason: selectionReason,
		Overflow:        toolTurnOverflow,
	}, nil
}

// FilterHistoryForRequestKind mirrors
// `_filter_history_for_request_kind`.
func FilterHistoryForRequestKind(chatHistory []LLMContextMessage, requestKind string) []LLMContextMessage {
	filtered := make([]LLMContextMessage, 0, len(chatHistory))
	for _, entry := range chatHistory {
		if entry == nil {
			continue
		}
		switch requestKind {
		case RequestKindExpressionSelector:
			if _, ok := entry.(*SessionBackedMessage); !ok {
				continue
			}
		case RequestKindPlanner:
			if IsMidTermMemoryMessage(entry) {
				continue
			}
		default:
			if entry.Source() == string(ReferenceBehaviorPattern) || IsMidTermMemoryMessage(entry) {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func collectAlwaysSelectedReferenceIndices(chatHistory []LLMContextMessage, enableVisualMessage bool) []int {
	indices := make([]int, 0)
	for index, entry := range chatHistory {
		reference, ok := entry.(*ReferenceMessage)
		if !ok || reference.ReferenceKind != ReferenceContextRestore {
			continue
		}
		if len(BuildContextItemsFromHistoryEntry(entry, enableVisualMessage)) == 0 {
			continue
		}
		indices = append(indices, index)
	}
	return indices
}

// expandSelectedToolTurns mirrors `_expand_selected_tool_turns`: when a tool
// loop entry is inside the budget, the whole logical turn (plus its preceding
// user anchor) is pulled in.
func expandSelectedToolTurns(
	fullHistory []LLMContextMessage,
	selectedHistory []LLMContextMessage,
) []LLMContextMessage {
	toolTurnIDs := map[string]bool{}
	for _, entry := range fullHistory {
		turnID := LogicalTurnIDOf(entry)
		if turnID == "" {
			continue
		}
		switch typed := entry.(type) {
		case *ModelOutputContextMessage:
			if len(typed.ToolCalls()) > 0 {
				toolTurnIDs[turnID] = true
			}
		case *ToolResultMessage:
			toolTurnIDs[turnID] = true
		}
	}

	selectedTurnIDs := map[string]bool{}
	for _, entry := range selectedHistory {
		turnID := LogicalTurnIDOf(entry)
		if turnID != "" && toolTurnIDs[turnID] {
			selectedTurnIDs[turnID] = true
		}
	}
	selectedIdentities := make(map[LLMContextMessage]bool, len(selectedHistory))
	for _, chosen := range selectedHistory {
		selectedIdentities[chosen] = true
	}
	selected := make(map[int]bool, len(selectedHistory))
	for index, entry := range fullHistory {
		if selectedIdentities[entry] {
			selected[index] = true
		}
	}
	anchorIndexByTurnID := CollectToolTurnAnchorIndices(fullHistory, selectedTurnIDs)
	for _, anchorIndex := range anchorIndexByTurnID {
		selected[anchorIndex] = true
	}

	expanded := make([]LLMContextMessage, 0, len(selected))
	for index, entry := range fullHistory {
		if selected[index] || selectedTurnIDs[LogicalTurnIDOf(entry)] {
			expanded = append(expanded, entry)
		}
	}
	return expanded
}

// resolveEnableVisualMessage mirrors `_resolve_enable_visual_message`. The real
// Python implementation inspects the configured planner model's `visual` flag
// (`resolve_enable_visual_planner`); without importing the model config the Go
// port defaults to the Python default `enable_visual_message=True`. Callers that
// know the model capability pass `enableVisualMessage` explicitly.
func resolveEnableVisualMessage(_ string) bool {
	return true
}

// DropHeadContextMessages mirrors
// MaisakaHeartFlowChatting._drop_head_context_messages: drop the first
// dropContextCount *counted* context messages and clean leading orphan tool
// results afterwards.
func DropHeadContextMessages(
	chatHistory []LLMContextMessage,
	dropContextCount int,
	trimThresholdContextCount *int,
) []LLMContextMessage {
	if dropContextCount <= 0 {
		return append([]LLMContextMessage{}, chatHistory...)
	}
	contextMessageCount := countInContext(chatHistory)
	if trimThresholdContextCount != nil && contextMessageCount <= *trimThresholdContextCount {
		return append([]LLMContextMessage{}, chatHistory...)
	}
	if contextMessageCount <= dropContextCount {
		return append([]LLMContextMessage{}, chatHistory...)
	}

	firstKeptIndex := 0
	droppedContextCount := 0
	for firstKeptIndex < len(chatHistory) && droppedContextCount < dropContextCount {
		if chatHistory[firstKeptIndex].CountInContext() {
			droppedContextCount++
		}
		firstKeptIndex++
	}
	trimmed := append([]LLMContextMessage{}, chatHistory[firstKeptIndex:]...)
	trimmed, _ = DropLeadingOrphanToolResults(trimmed)
	return trimmed
}

// CountInContextMessages exposes the counted-context helper for callers that
// need the same accounting as the post-processor.
func CountInContextMessages(chatHistory []LLMContextMessage) int { return countInContext(chatHistory) }
