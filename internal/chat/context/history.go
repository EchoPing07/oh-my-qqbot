package context

import (
	"fmt"
	"sort"
	"strings"

	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/message"
)

// This file mirrors 参考/MaiBot/src/maisaka/context/history.py plus the
// relation kernel of 参考/MaiBot/src/llm_models/payload_content/context_protocol.py
// (`analyze_context_item_relations` / `prune_context_items_for_history`).
//
// Python runtime errors are surfaced as Go errors so callers can log them; the
// pruning decisions themselves are identical.

// TOOL_RESULT_MEDIA_SOURCE_KIND / OPTIMIZED_TOOL_HISTORY_SOURCE_KIND mirror the
// constants of history.py (the Go names live in messages.go).
const (
	toolResultMediaSourceKind  = SourceToolResultMedia
	optimizedToolHistorySource = SourceOptimizedTools
)

// BuildPrefixedMessageSequence mirrors
// history.build_prefixed_message_sequence: clone the sequence, drop reply
// components and prepend `planner_prefix` to the first text component (or
// insert a new leading text component).
func BuildPrefixedMessageSequence(sourceSequence *message.MessageSequence, plannerPrefix string) *message.MessageSequence {
	components := make([]message.Component, 0)
	if sourceSequence != nil {
		for _, component := range CloneMessageSequence(sourceSequence).Components {
			if _, isReply := component.(*message.ReplyComponent); isReply {
				continue
			}
			components = append(components, component)
		}
	}
	if len(components) > 0 {
		if text, ok := components[0].(*message.TextComponent); ok {
			text.Text = plannerPrefix + text.Text
			return message.NewMessageSequence(components...)
		}
	}
	return message.NewMessageSequence(append([]message.Component{message.NewTextComponent(plannerPrefix)}, components...)...)
}

// BuildSessionMessageVisibleText mirrors
// history.build_session_message_visible_text: `[HH:MM:SS][msg_id:别名][名字]` +
// rendered components.
func BuildSessionMessageVisibleText(
	sessionMessage *message.SessionMessage,
	sourceSequence *message.MessageSequence,
	includeReplyComponents bool,
) string {
	normalizedSequence := sourceSequence
	if normalizedSequence == nil {
		normalizedSequence = sessionMessage.RawMessage
	}
	userInfo := sessionMessage.MessageInfo.UserInfo
	speakerName := userInfo.DisplayName()

	visibleMessageID := sessionMessage.MessageID
	if sessionMessage.IsNotify {
		visibleMessageID = ""
	}

	timestamp := sessionMessage.Timestamp
	visibleSequence := message.NewMessageSequence(
		message.NewTextComponent(FormatSpeakerContent(speakerName, "", &timestamp, visibleMessageID)),
	)
	if normalizedSequence != nil {
		for _, component := range CloneMessageSequence(normalizedSequence).Components {
			if !includeReplyComponents {
				if _, isReply := component.(*message.ReplyComponent); isReply {
					continue
				}
			}
			visibleSequence.Components = append(visibleSequence.Components, component)
		}
	}
	return strings.TrimSpace(BuildVisibleTextFromSequence(visibleSequence))
}

// ---------------------------------------------------------------------------
// Orphan tool result / tool call pruning
// ---------------------------------------------------------------------------

// DropLeadingOrphanToolResults mirrors
// history.drop_leading_orphan_tool_results: remove tool results at the head of
// the history that have no matching tool_call anywhere in the history.
func DropLeadingOrphanToolResults(chatHistory []LLMContextMessage) ([]LLMContextMessage, int) {
	if len(chatHistory) == 0 {
		return chatHistory, 0
	}
	availableToolCallIDs := CollectAvailableToolCallIDs(chatHistory)
	firstValidIndex := 0
	for firstValidIndex < len(chatHistory) {
		result, ok := chatHistory[firstValidIndex].(*ToolResultMessage)
		if !ok {
			break
		}
		if availableToolCallIDs[result.ToolCallID] {
			break
		}
		firstValidIndex++
	}
	if firstValidIndex == 0 {
		return chatHistory, 0
	}
	return chatHistory[firstValidIndex:], firstValidIndex
}

// DropOrphanToolResults mirrors history.drop_orphan_tool_results: remove tool
// results missing a matching tool_call (and their whole logical turn), plus
// orphan tool-result media messages.
func DropOrphanToolResults(chatHistory []LLMContextMessage) ([]LLMContextMessage, int) {
	if len(chatHistory) == 0 {
		return chatHistory, 0
	}
	availableToolCallIDs := CollectAvailableToolCallIDs(chatHistory)
	foldedToolCallIDs := collectFoldedToolHistoryCallIDs(chatHistory)
	availableMediaOwnerIDs := map[string]bool{}
	for callID := range availableToolCallIDs {
		availableMediaOwnerIDs[callID] = true
	}
	for callID := range foldedToolCallIDs {
		availableMediaOwnerIDs[callID] = true
	}

	invalidTurnIDs := map[string]bool{}
	for _, entry := range chatHistory {
		result, ok := entry.(*ToolResultMessage)
		if !ok || availableToolCallIDs[result.ToolCallID] {
			continue
		}
		if strings.TrimSpace(result.LogicalTurnID_) != "" {
			invalidTurnIDs[result.LogicalTurnID_] = true
		}
	}

	filtered := make([]LLMContextMessage, 0, len(chatHistory))
	removedCount := 0
	for _, entry := range chatHistory {
		if turnID := LogicalTurnIDOf(entry); turnID != "" && invalidTurnIDs[turnID] {
			removedCount++
			continue
		}
		if result, ok := entry.(*ToolResultMessage); ok && !availableToolCallIDs[result.ToolCallID] {
			removedCount++
			continue
		}
		if isOrphanToolResultMediaMessage(entry, availableMediaOwnerIDs) {
			removedCount++
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered, removedCount
}

// NormalizeToolCallStats mirrors the dict returned by
// history.normalize_tool_call_result_pairs.
type NormalizeToolCallStats struct {
	OrphanToolResults   int
	UnansweredToolCalls int
	MovedToolResults    int
	InvalidToolTurns    int
}

// NormalizeToolCallResultPairs mirrors history.normalize_tool_call_result_pairs.
func NormalizeToolCallResultPairs(
	chatHistory []LLMContextMessage,
	pendingCallIDs map[string]bool,
) ([]LLMContextMessage, NormalizeToolCallStats, error) {
	processed, orphanCount := DropOrphanToolResults(chatHistory)
	processed, unansweredCount := DropUnansweredToolCalls(processed, pendingCallIDs)
	processed, movedCount := NormalizeToolResultOrder(processed)
	processed, invalidCount, err := DropInvalidToolTurns(processed, pendingCallIDs)
	if err != nil {
		return processed, NormalizeToolCallStats{}, err
	}
	return processed, NormalizeToolCallStats{
		OrphanToolResults:   orphanCount,
		UnansweredToolCalls: unansweredCount,
		MovedToolResults:    movedCount,
		InvalidToolTurns:    invalidCount,
	}, nil
}

// CollectToolTurnAnchorIndices mirrors
// history.collect_tool_turn_anchor_indices: locate the nearest preceding `user`
// context entry for every logical turn.
func CollectToolTurnAnchorIndices(chatHistory []LLMContextMessage, logicalTurnIDs map[string]bool) map[string]int {
	firstTurnIndexByID := map[string]int{}
	for index, entry := range chatHistory {
		logicalTurnID := LogicalTurnIDOf(entry)
		if logicalTurnID == "" || !logicalTurnIDs[logicalTurnID] {
			continue
		}
		if _, exists := firstTurnIndexByID[logicalTurnID]; !exists {
			firstTurnIndexByID[logicalTurnID] = index
		}
	}

	anchorIndexByTurnID := map[string]int{}
	for logicalTurnID, firstTurnIndex := range firstTurnIndexByID {
		for index := firstTurnIndex - 1; index >= 0; index-- {
			if chatHistory[index].Role() == string(llm.RoleUser) {
				anchorIndexByTurnID[logicalTurnID] = index
				break
			}
		}
	}
	return anchorIndexByTurnID
}

// DropUnansweredToolCalls mirrors history.drop_unanswered_tool_calls.
func DropUnansweredToolCalls(
	chatHistory []LLMContextMessage,
	pendingCallIDs map[string]bool,
) ([]LLMContextMessage, int) {
	if len(chatHistory) == 0 {
		return chatHistory, 0
	}
	report := AnalyzeContextItemRelations(buildToolRelationItems(chatHistory), pendingCallIDs)
	unansweredCallIDs := map[string]bool{}
	for _, callID := range report.UnansweredCallIDs {
		unansweredCallIDs[callID] = true
	}
	if len(unansweredCallIDs) == 0 {
		return chatHistory, 0
	}

	invalidTurnIDs := map[string]bool{}
	unansweredItemIDs := map[string]bool{}
	unansweredCount := 0
	for _, entry := range chatHistory {
		output, ok := entry.(*ModelOutputContextMessage)
		if !ok || output.OutputItem == nil || output.OutputItem.Kind != llm.ItemKindFunctionCall {
			continue
		}
		if output.OutputItem.ToolCall == nil || !unansweredCallIDs[output.OutputItem.ToolCall.CallID] {
			continue
		}
		unansweredCount++
		if turnID := output.OutputItem.Meta.LogicalTurnID; turnID != "" {
			invalidTurnIDs[turnID] = true
		}
		unansweredItemIDs[output.OutputItem.Meta.ItemID] = true
	}

	filtered := make([]LLMContextMessage, 0, len(chatHistory))
	for _, entry := range chatHistory {
		if turnID := LogicalTurnIDOf(entry); turnID != "" && invalidTurnIDs[turnID] {
			continue
		}
		if output, ok := entry.(*ModelOutputContextMessage); ok && output.OutputItem != nil {
			if unansweredItemIDs[output.OutputItem.Meta.ItemID] {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	return filtered, unansweredCount
}

// DropInvalidToolTurns mirrors history.drop_invalid_tool_turns: remove whole
// logical turns judged invalid by the shared relation kernel.
func DropInvalidToolTurns(
	chatHistory []LLMContextMessage,
	pendingCallIDs map[string]bool,
) ([]LLMContextMessage, int, error) {
	_, report, err := PruneContextItemsForHistory(buildToolRelationItems(chatHistory), pendingCallIDs)
	if err != nil {
		return chatHistory, 0, err
	}
	invalidTurnIDs := map[string]bool{}
	for _, turnID := range report.InvalidTurnIDs {
		invalidTurnIDs[turnID] = true
	}
	if len(invalidTurnIDs) == 0 {
		return chatHistory, 0, nil
	}
	filtered := make([]LLMContextMessage, 0, len(chatHistory))
	for _, entry := range chatHistory {
		if turnID := LogicalTurnIDOf(entry); turnID != "" && invalidTurnIDs[turnID] {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered, len(chatHistory) - len(filtered), nil
}

// NormalizeToolResultOrder mirrors history.normalize_tool_result_order: move a
// tool result that got separated from its tool_call right after the call.
func NormalizeToolResultOrder(chatHistory []LLMContextMessage) ([]LLMContextMessage, int) {
	if len(chatHistory) == 0 {
		return chatHistory, 0
	}
	consumedIndexes := map[int]bool{}
	normalized := make([]LLMContextMessage, 0, len(chatHistory))
	movedCount := 0

	for index := 0; index < len(chatHistory); index++ {
		if consumedIndexes[index] {
			continue
		}
		entry := chatHistory[index]
		if _, ok := entry.(*ModelOutputContextMessage); !ok {
			normalized = append(normalized, entry)
			continue
		}

		outputIndexes := make([]int, 0)
		cursor := index
		for cursor < len(chatHistory) {
			if _, ok := chatHistory[cursor].(*ModelOutputContextMessage); !ok {
				break
			}
			if !consumedIndexes[cursor] {
				outputIndexes = append(outputIndexes, cursor)
			}
			cursor++
		}

		toolCalls := make([]ToolCall, 0)
		for _, outputIndex := range outputIndexes {
			consumedIndexes[outputIndex] = true
			output := chatHistory[outputIndex].(*ModelOutputContextMessage)
			normalized = append(normalized, output)
			toolCalls = append(toolCalls, output.ToolCalls()...)
		}

		appendedToolResultCount := 0
		for _, toolCall := range toolCalls {
			toolCallID := strings.TrimSpace(toolCall.CallID)
			if toolCallID == "" {
				continue
			}
			matchingIndex := findToolResultIndex(chatHistory, toolCallID, outputIndexes[0]+1, consumedIndexes)
			if matchingIndex < 0 {
				continue
			}
			consumedIndexes[matchingIndex] = true
			normalized = append(normalized, chatHistory[matchingIndex])
			expectedIndex := outputIndexes[0] + len(outputIndexes) + appendedToolResultCount
			if matchingIndex != expectedIndex {
				movedCount++
			}
			appendedToolResultCount++
		}
		index = cursor - 1
	}

	if movedCount <= 0 {
		return chatHistory, 0
	}
	return normalized, movedCount
}

func findToolResultIndex(
	chatHistory []LLMContextMessage,
	toolCallID string,
	startIndex int,
	consumedIndexes map[int]bool,
) int {
	for index := startIndex; index < len(chatHistory); index++ {
		if consumedIndexes[index] {
			continue
		}
		result, ok := chatHistory[index].(*ToolResultMessage)
		if !ok {
			continue
		}
		if result.ToolCallID == toolCallID {
			return index
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// helpers shared with post_processor.go
// ---------------------------------------------------------------------------

// LogicalTurnIDOf mirrors `_get_logical_turn_id`: the owning logical tool turn
// of a model output or tool result entry ("" for everything else).
func LogicalTurnIDOf(entry LLMContextMessage) string {
	switch typed := entry.(type) {
	case *ModelOutputContextMessage:
		return typed.LogicalTurnID()
	case *ToolResultMessage:
		return typed.LogicalTurnID()
	default:
		return ""
	}
}

// CollectAvailableToolCallIDs mirrors `_collect_available_tool_call_ids`.
func CollectAvailableToolCallIDs(chatHistory []LLMContextMessage) map[string]bool {
	callIDs := map[string]bool{}
	for _, entry := range chatHistory {
		output, ok := entry.(*ModelOutputContextMessage)
		if !ok {
			continue
		}
		for _, toolCall := range output.ToolCalls() {
			if strings.TrimSpace(toolCall.CallID) != "" {
				callIDs[toolCall.CallID] = true
			}
		}
	}
	return callIDs
}

func collectFoldedToolHistoryCallIDs(chatHistory []LLMContextMessage) map[string]bool {
	callIDs := map[string]bool{}
	for _, entry := range chatHistory {
		sessionBacked, ok := entry.(*SessionBackedMessage)
		if !ok || sessionBacked.SourceKind != optimizedToolHistorySource {
			continue
		}
		for callID := range parseFoldedToolHistoryCallIDs(sessionBacked.ProcessedPlainText()) {
			callIDs[callID] = true
		}
	}
	return callIDs
}

// parseFoldedToolHistoryCallIDs mirrors
// `_parse_folded_tool_history_call_ids`: read `- tool_call_id: <id>` lines.
func parseFoldedToolHistoryCallIDs(content string) map[string]bool {
	callIDs := map[string]bool{}
	for _, rawLine := range strings.Split(content, "\n") {
		normalizedLine := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(normalizedLine, "- tool_call_id:") {
			continue
		}
		_, rawCallID, _ := strings.Cut(normalizedLine, ":")
		normalizedCallID := strings.TrimSpace(rawCallID)
		if normalizedCallID != "" {
			callIDs[normalizedCallID] = true
		}
	}
	return callIDs
}

// isOrphanToolResultMediaMessage mirrors
// `_is_orphan_tool_result_media_message`: a media entry split out of a tool
// result whose owning tool_call is gone.
func isOrphanToolResultMediaMessage(entry LLMContextMessage, availableMediaOwnerIDs map[string]bool) bool {
	sessionBacked, ok := entry.(*SessionBackedMessage)
	if !ok || sessionBacked.SourceKind != toolResultMediaSourceKind {
		return false
	}
	messageID := strings.TrimSpace(sessionBacked.MsgID)
	if !strings.HasPrefix(messageID, "tool_result:") {
		return false
	}
	remaining := strings.TrimPrefix(messageID, "tool_result:")
	// rpartition(":") keeps the segment before the *last* separator.
	toolCallID := ""
	if index := strings.LastIndex(remaining, ":"); index >= 0 {
		toolCallID = remaining[:index]
	}
	return toolCallID != "" && !availableMediaOwnerIDs[toolCallID]
}

func buildToolRelationItems(chatHistory []LLMContextMessage) []*llm.ContextItem {
	items := make([]*llm.ContextItem, 0, len(chatHistory))
	for _, entry := range chatHistory {
		switch typed := entry.(type) {
		case *ModelOutputContextMessage:
			if typed.OutputItem != nil {
				items = append(items, typed.OutputItem)
			}
		case *ToolResultMessage:
			items = append(items, typed.ToContextItem(false))
		}
	}
	return items
}

// ---------------------------------------------------------------------------
// context_protocol.py — relation kernel
// ---------------------------------------------------------------------------

// ContextRelationReport mirrors context_protocol.ContextRelationReport.
type ContextRelationReport struct {
	DuplicateItemIDs       []string
	DuplicateCallIDs       []string
	DuplicateOutputCallIDs []string
	OrphanOutputCallIDs    []string
	UnansweredCallIDs      []string
	MisplacedOutputCallIDs []string
	MismatchedTurnCallIDs  []string
	MissingTurnCallIDs     []string
	InvalidTurnIDs         []string
}

// AnalyzeContextItemRelations mirrors
// context_protocol.analyze_context_item_relations: analyse call/output relation
// by list position without mutating the input.
//
// The Python function cannot fail; the Go port keeps the same signature shape for
// symmetry with `PruneContextItemsForHistory` but never returns an error.
func AnalyzeContextItemRelations(items []*llm.ContextItem, pendingCallIDs map[string]bool) ContextRelationReport {
	type indexedCall struct {
		index int
		item  *llm.ContextItem
	}
	itemsByID := map[string]*llm.ContextItem{}
	duplicateItemIDs := map[string]bool{}
	calls := map[string]indexedCall{}
	duplicateCallIDs := map[string]bool{}
	outputs := map[string]indexedCall{}
	duplicateOutputCallIDs := map[string]bool{}
	missingTurnCallIDs := map[string]bool{}
	invalidTurnIDs := map[string]bool{}

	for index, item := range items {
		if item == nil {
			continue
		}
		itemID := item.Meta.ItemID
		if previous, exists := itemsByID[itemID]; exists {
			duplicateItemIDs[itemID] = true
			if previous.Meta.LogicalTurnID != "" {
				invalidTurnIDs[previous.Meta.LogicalTurnID] = true
			}
			if item.Meta.LogicalTurnID != "" {
				invalidTurnIDs[item.Meta.LogicalTurnID] = true
			}
		} else {
			itemsByID[itemID] = item
		}

		switch item.Kind {
		case llm.ItemKindFunctionCall:
			callID := ""
			if item.ToolCall != nil {
				callID = item.ToolCall.CallID
			}
			if previous, exists := calls[callID]; exists {
				duplicateCallIDs[callID] = true
				if previous.item.Meta.LogicalTurnID != "" {
					invalidTurnIDs[previous.item.Meta.LogicalTurnID] = true
				}
				if item.Meta.LogicalTurnID != "" {
					invalidTurnIDs[item.Meta.LogicalTurnID] = true
				}
			} else {
				calls[callID] = indexedCall{index: index, item: item}
			}
			if item.Meta.LogicalTurnID == "" {
				missingTurnCallIDs[callID] = true
			}
			continue
		case llm.ItemKindToolResult:
			callID := item.CallID
			if previous, exists := outputs[callID]; exists {
				duplicateOutputCallIDs[callID] = true
				if previous.item.Meta.LogicalTurnID != "" {
					invalidTurnIDs[previous.item.Meta.LogicalTurnID] = true
				}
				if item.Meta.LogicalTurnID != "" {
					invalidTurnIDs[item.Meta.LogicalTurnID] = true
				}
			} else {
				outputs[callID] = indexedCall{index: index, item: item}
			}
			if item.Meta.LogicalTurnID == "" {
				missingTurnCallIDs[callID] = true
			}
		}
	}

	orphanOutputCallIDs := map[string]bool{}
	for callID := range outputs {
		if _, exists := calls[callID]; !exists {
			orphanOutputCallIDs[callID] = true
		}
	}
	unansweredCallIDs := map[string]bool{}
	for callID := range calls {
		if _, exists := outputs[callID]; exists {
			continue
		}
		if pendingCallIDs[callID] {
			continue
		}
		unansweredCallIDs[callID] = true
	}
	misplacedOutputCallIDs := map[string]bool{}
	mismatchedTurnCallIDs := map[string]bool{}
	for callID := range calls {
		output, hasOutput := outputs[callID]
		if !hasOutput {
			continue
		}
		call := calls[callID]
		callTurnID := call.item.Meta.LogicalTurnID
		outputTurnID := output.item.Meta.LogicalTurnID
		if output.index <= call.index {
			misplacedOutputCallIDs[callID] = true
		}
		if callTurnID != outputTurnID {
			mismatchedTurnCallIDs[callID] = true
		}
		if output.index <= call.index || callTurnID != outputTurnID {
			if callTurnID != "" {
				invalidTurnIDs[callTurnID] = true
			}
			if outputTurnID != "" {
				invalidTurnIDs[outputTurnID] = true
			}
		}
	}
	for callID := range orphanOutputCallIDs {
		if turnID := outputs[callID].item.Meta.LogicalTurnID; turnID != "" {
			invalidTurnIDs[turnID] = true
		}
	}
	for callID := range unansweredCallIDs {
		if turnID := calls[callID].item.Meta.LogicalTurnID; turnID != "" {
			invalidTurnIDs[turnID] = true
		}
	}

	return ContextRelationReport{
		DuplicateItemIDs:       sortedKeys(duplicateItemIDs),
		DuplicateCallIDs:       sortedKeys(duplicateCallIDs),
		DuplicateOutputCallIDs: sortedKeys(duplicateOutputCallIDs),
		OrphanOutputCallIDs:    sortedKeys(orphanOutputCallIDs),
		UnansweredCallIDs:      sortedKeys(unansweredCallIDs),
		MisplacedOutputCallIDs: sortedKeys(misplacedOutputCallIDs),
		MismatchedTurnCallIDs:  sortedKeys(mismatchedTurnCallIDs),
		MissingTurnCallIDs:     sortedKeys(missingTurnCallIDs),
		InvalidTurnIDs:         sortedKeys(invalidTurnIDs),
	}
}

// PruneContextItemsForHistory mirrors
// context_protocol.prune_context_items_for_history: drop invalid turns, then
// validate the retained items with the HISTORY rules.
func PruneContextItemsForHistory(
	items []*llm.ContextItem,
	pendingCallIDs map[string]bool,
) ([]*llm.ContextItem, ContextRelationReport, error) {
	report := AnalyzeContextItemRelations(items, pendingCallIDs)
	invalidTurnIDs := map[string]bool{}
	for _, turnID := range report.InvalidTurnIDs {
		invalidTurnIDs[turnID] = true
	}
	retained := make([]*llm.ContextItem, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		if invalidTurnIDs[item.Meta.LogicalTurnID] {
			continue
		}
		retained = append(retained, item)
	}
	if err := validateContextItemsForHistory(retained, pendingCallIDs); err != nil {
		return retained, report, err
	}
	return retained, report, nil
}

func validateContextItemsForHistory(items []*llm.ContextItem, pendingCallIDs map[string]bool) error {
	report := AnalyzeContextItemRelations(items, pendingCallIDs)
	if len(report.DuplicateItemIDs) > 0 {
		return fmt.Errorf("context: Context Items 存在重复 item_id: %v", report.DuplicateItemIDs)
	}
	if len(report.DuplicateCallIDs) > 0 {
		return fmt.Errorf("context: Context Items 存在重复 function call: %v", report.DuplicateCallIDs)
	}
	if len(report.DuplicateOutputCallIDs) > 0 {
		return fmt.Errorf("context: Context Items 存在重复 function output: %v", report.DuplicateOutputCallIDs)
	}
	if len(report.MissingTurnCallIDs) > 0 {
		return fmt.Errorf("context: 工具 Item 缺少 logical_turn_id: %v", report.MissingTurnCallIDs)
	}
	if len(report.OrphanOutputCallIDs) > 0 {
		return fmt.Errorf("context: Context Items 存在孤儿 function output: %v", report.OrphanOutputCallIDs)
	}
	if len(report.MisplacedOutputCallIDs) > 0 {
		return fmt.Errorf("context: function output 位于调用之前: %v", report.MisplacedOutputCallIDs)
	}
	if len(report.MismatchedTurnCallIDs) > 0 {
		return fmt.Errorf("context: function call/output logical_turn_id 不一致: %v", report.MismatchedTurnCallIDs)
	}
	if len(report.UnansweredCallIDs) > 0 {
		return fmt.Errorf("context: Context Items 存在未回答 function call: %v", report.UnansweredCallIDs)
	}
	return nil
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
