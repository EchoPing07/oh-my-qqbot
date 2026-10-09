package context

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/message"
)

// This file mirrors 参考/MaiBot/src/maisaka/context/post_processor.py:
// `process_chat_history_after_cycle` plus the assistant-history optimisation,
// the folded tool history and the context-target trimming.
const (
	// TrimTargetRatio mirrors TRIM_TARGET_RATIO.
	TrimTargetRatio = 1.0
	// TrimThresholdRatio mirrors TRIM_THRESHOLD_RATIO.
	TrimThresholdRatio = 2.0
	// AssistantOptimizationKeepCount mirrors ASSISTANT_OPTIMIZATION_KEEP_COUNT.
	AssistantOptimizationKeepCount = 3
	// FoldedToolComplexMessageThreshold mirrors
	// FOLDED_TOOL_COMPLEX_MESSAGE_THRESHOLD.
	FoldedToolComplexMessageThreshold = 1024
)

// TrimmedToolCallDropNames mirrors TRIMMED_TOOL_CALL_DROP_NAMES: tool calls
// dropped when folding a tool turn into a plain user message.
var TrimmedToolCallDropNames = map[string]bool{
	"continue":  true,
	"finish":    true,
	"no_action": true,
	"reply":     true,
	"wait":      true,
}

// HistoryPostProcessResult mirrors HistoryPostProcessResult.
type HistoryPostProcessResult struct {
	History               []LLMContextMessage
	RemovedMessages       []LLMContextMessage
	RemovedCount          int
	ChangedCount          int
	RemainingContextCount int
}

// ProcessChatHistoryAfterCycle mirrors
// post_processor.process_chat_history_after_cycle: consume one-shot reference
// messages, structurally normalize the tool protocol, optionally fold old
// assistant tool turns and finally trim oldest context down to the target.
//
// `enableContextOptimization` mirrors `enable_context_optimization`; the mid-term
// memory folding itself lives in midterm.go (`InsertMidTermMemoryMessage`) and is
// driven by the runtime when this result removed messages.
func ProcessChatHistoryAfterCycle(
	chatHistory []LLMContextMessage,
	maxContextSize int,
	enableContextOptimization bool,
	pendingCallIDs map[string]bool,
) (HistoryPostProcessResult, error) {
	processed := make([]LLMContextMessage, 0, len(chatHistory))
	oneShotRemovedCount := 0
	for _, entry := range chatHistory {
		if entry == nil {
			continue
		}
		if FocusWakeupSourceKinds[entry.Source()] {
			oneShotRemovedCount++
			continue
		}
		if !entry.ConsumeOnce() {
			oneShotRemovedCount++
			continue
		}
		processed = append(processed, entry)
	}

	var err error
	var normalizedRemovedCount, movedToolResultCount int
	processed, normalizedRemovedCount, movedToolResultCount, err = normalizeHistoryStructure(processed, pendingCallIDs)
	if err != nil {
		return HistoryPostProcessResult{}, err
	}
	remainingContextCount := countInContext(processed)

	optimizedRemovedCount := 0
	if enableContextOptimization {
		var removed []LLMContextMessage
		processed, removed = trimAssistantHistoryToLatest(processed, AssistantOptimizationKeepCount)
		if len(removed) > 0 {
			var removedAfterOptimizeCount, movedAfterOptimizeCount int
			processed, removedAfterOptimizeCount, movedAfterOptimizeCount, err = normalizeHistoryStructure(processed, pendingCallIDs)
			if err != nil {
				return HistoryPostProcessResult{}, err
			}
			optimizedRemovedCount = len(removed) + removedAfterOptimizeCount
			movedToolResultCount += movedAfterOptimizeCount
			remainingContextCount = countInContext(processed)
		}
	}

	compactRemovedCount := 0
	var removedMessages []LLMContextMessage
	trimThreshold := int(math.Ceil(float64(maxContextSize) * TrimThresholdRatio))
	if remainingContextCount > trimThreshold {
		targetContextCount := int(math.Max(1, float64(maxContextSize)*TrimTargetRatio))
		processed, removedMessages = trimHistoryToContextTarget(processed, targetContextCount)
		var removedAfterTrimCount, movedAfterTrimCount int
		processed, removedAfterTrimCount, movedAfterTrimCount, err = normalizeHistoryStructure(processed, pendingCallIDs)
		if err != nil {
			return HistoryPostProcessResult{}, err
		}
		compactRemovedCount = len(removedMessages) + removedAfterTrimCount
		movedToolResultCount += movedAfterTrimCount
	}

	remainingContextCount = countInContext(processed)
	removedCount := oneShotRemovedCount + normalizedRemovedCount + optimizedRemovedCount + compactRemovedCount
	changedCount := removedCount + movedToolResultCount
	return HistoryPostProcessResult{
		History:               processed,
		RemovedMessages:       removedMessages,
		RemovedCount:          removedCount,
		ChangedCount:          changedCount,
		RemainingContextCount: remainingContextCount,
	}, nil
}

func normalizeHistoryStructure(
	chatHistory []LLMContextMessage,
	pendingCallIDs map[string]bool,
) ([]LLMContextMessage, int, int, error) {
	processed, stats, err := NormalizeToolCallResultPairs(chatHistory, pendingCallIDs)
	if err != nil {
		return processed, 0, 0, err
	}
	processed, leadingOrphanRemovedCount := DropLeadingOrphanToolResults(processed)
	removedCount := stats.OrphanToolResults + stats.UnansweredToolCalls + stats.InvalidToolTurns + leadingOrphanRemovedCount
	return processed, removedCount, stats.MovedToolResults, nil
}

func countInContext(chatHistory []LLMContextMessage) int {
	total := 0
	for _, entry := range chatHistory {
		if entry != nil && entry.CountInContext() {
			total++
		}
	}
	return total
}

// ---------------------------------------------------------------------------
// _trim_assistant_history_to_latest
// ---------------------------------------------------------------------------

type outputUnitID struct {
	kind string
	id   string
}

// trimAssistantHistoryToLatest mirrors `_trim_assistant_history_to_latest`: it
// returns the optimized history plus the removed entries (Python mutates the
// list in place and returns removed_messages).
func trimAssistantHistoryToLatest(
	chatHistory []LLMContextMessage,
	keepCount int,
) ([]LLMContextMessage, []LLMContextMessage) {
	normalizedKeepCount := int(math.Max(0, float64(keepCount)))
	toolTurnIDs := collectToolTurnIDs(chatHistory)
	outputUnitIDs := make([]outputUnitID, 0)
	seenUnits := map[outputUnitID]bool{}
	for _, entry := range chatHistory {
		output, ok := entry.(*ModelOutputContextMessage)
		if !ok {
			continue
		}
		unit := outputUnitForMessage(output, toolTurnIDs)
		if seenUnits[unit] {
			continue
		}
		seenUnits[unit] = true
		outputUnitIDs = append(outputUnitIDs, unit)
	}
	removeCount := len(outputUnitIDs) - normalizedKeepCount
	if removeCount <= 0 {
		return chatHistory, nil
	}

	removedUnitIDs := map[outputUnitID]bool{}
	for _, unit := range outputUnitIDs[:removeCount] {
		removedUnitIDs[unit] = true
	}
	removeIndexes := map[int]bool{}
	for index, entry := range chatHistory {
		output, ok := entry.(*ModelOutputContextMessage)
		if !ok {
			continue
		}
		if removedUnitIDs[outputUnitForMessage(output, toolTurnIDs)] {
			removeIndexes[index] = true
		}
	}

	removedMessages := make([]LLMContextMessage, 0, len(removeIndexes))
	for index, entry := range chatHistory {
		if removeIndexes[index] {
			removedMessages = append(removedMessages, entry)
		}
	}

	toolResultByCallID := map[string]*ToolResultMessage{}
	for _, entry := range chatHistory {
		result, ok := entry.(*ToolResultMessage)
		if !ok || strings.TrimSpace(result.ToolCallID) == "" {
			continue
		}
		toolResultByCallID[result.ToolCallID] = result
	}
	preservedToolResultIDs := map[string]bool{}
	for _, entry := range removedMessages {
		output, ok := entry.(*ModelOutputContextMessage)
		if !ok {
			continue
		}
		for _, toolCall := range output.ToolCalls() {
			if _, exists := toolResultByCallID[toolCall.CallID]; exists {
				preservedToolResultIDs[toolCall.CallID] = true
			}
		}
	}

	foldedMessageByIndex := map[int]LLMContextMessage{}
	for unit := range removedUnitIDs {
		if unit.kind != "turn" {
			continue
		}
		unitIndexes := make([]int, 0)
		unitMessages := make([]*ModelOutputContextMessage, 0)
		for index, entry := range chatHistory {
			output, ok := entry.(*ModelOutputContextMessage)
			if !ok || !removeIndexes[index] {
				continue
			}
			if output.LogicalTurnID() != unit.id {
				continue
			}
			unitIndexes = append(unitIndexes, index)
			unitMessages = append(unitMessages, output)
		}
		if len(unitIndexes) == 0 {
			continue
		}
		if folded := buildTrimmedAssistantToolUserMessage(unitMessages, toolResultByCallID); folded != nil {
			foldedMessageByIndex[unitIndexes[0]] = folded
		}
	}

	optimized := make([]LLMContextMessage, 0, len(chatHistory))
	for index, entry := range chatHistory {
		if removeIndexes[index] {
			if folded, ok := foldedMessageByIndex[index]; ok {
				optimized = append(optimized, folded)
			}
			continue
		}
		if result, ok := entry.(*ToolResultMessage); ok && preservedToolResultIDs[result.ToolCallID] {
			continue
		}
		optimized = append(optimized, entry)
	}
	return optimized, removedMessages
}

func outputUnitForMessage(output *ModelOutputContextMessage, toolTurnIDs map[string]bool) outputUnitID {
	logicalTurnID := output.LogicalTurnID()
	if toolTurnIDs[logicalTurnID] {
		return outputUnitID{kind: "turn", id: logicalTurnID}
	}
	return outputUnitID{kind: "item", id: output.ItemID()}
}

func collectToolTurnIDs(chatHistory []LLMContextMessage) map[string]bool {
	turnIDs := map[string]bool{}
	for _, entry := range chatHistory {
		switch typed := entry.(type) {
		case *ModelOutputContextMessage:
			turnID := typed.LogicalTurnID()
			if turnID != "" && len(typed.ToolCalls()) > 0 {
				turnIDs[turnID] = true
			}
		case *ToolResultMessage:
			turnID := typed.LogicalTurnID()
			if turnID != "" {
				turnIDs[turnID] = true
			}
		}
	}
	return turnIDs
}

// ---------------------------------------------------------------------------
// _build_trimmed_assistant_tool_user_message
// ---------------------------------------------------------------------------

func buildTrimmedAssistantToolUserMessage(
	assistantMessages []*ModelOutputContextMessage,
	toolResultByCallID map[string]*ToolResultMessage,
) LLMContextMessage {
	toolCalls := make([]ToolCall, 0)
	for _, assistantMessage := range assistantMessages {
		toolCalls = append(toolCalls, assistantMessage.ToolCalls()...)
	}
	if len(toolCalls) == 0 {
		return nil
	}

	sections := make([]string, 0, len(toolCalls))
	for _, toolCall := range toolCalls {
		if TrimmedToolCallDropNames[toolCall.FuncName] {
			continue
		}
		toolResult := toolResultByCallID[toolCall.CallID]
		if toolCall.FuncName == "tool_search" {
			sections = append(sections, formatTrimmedToolSearchCall(toolCall.Args, toolResult))
			continue
		}
		lines := []string{
			fmt.Sprintf("- tool_call_id: %s", toolCall.CallID),
			fmt.Sprintf("  tool_name: %s", toolCall.FuncName),
			fmt.Sprintf("  args: %s", toolCall.ArgsJSON()),
		}
		if toolResult != nil {
			resultStatus := "failed"
			if toolResult.Success {
				resultStatus = "success"
			}
			lines = append(lines,
				fmt.Sprintf("  result_status: %s", resultStatus),
				"  result: "+toolResult.Content,
			)
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	if len(sections) == 0 {
		return nil
	}

	foldedText := "[已折叠的历史工具调用]\n" + strings.Join(sections, "\n")
	firstMessage := assistantMessages[0]
	logicalTurnID := firstMessage.LogicalTurnID()
	messageID := "optimized_tool_history:" + logicalTurnID
	sequence := message.NewMessageSequence(message.NewTextComponent(foldedText))
	if len([]rune(foldedText)) > FoldedToolComplexMessageThreshold {
		return NewComplexSessionMessage(
			sequence,
			foldedText,
			firstMessage.Timestamp(),
			messageID,
			nil,
			optimizedToolHistorySource,
			foldedText,
			"tool_history",
		)
	}
	return NewSessionBackedMessage(
		sequence,
		foldedText,
		firstMessage.Timestamp(),
		messageID,
		nil,
		optimizedToolHistorySource,
	)
}

func formatTrimmedToolSearchCall(args map[string]any, toolResult *ToolResultMessage) string {
	query := ""
	if raw, ok := args["query"]; ok {
		query = strings.TrimSpace(fmt.Sprintf("%v", raw))
	}
	matchedToolNames := parseToolSearchResultToolNames(toolResultContent(toolResult))
	matchedText := "无"
	if len(matchedToolNames) > 0 {
		matchedText = strings.Join(matchedToolNames, ", ")
	}
	if query != "" {
		return fmt.Sprintf("- tool_search: %s (query=%s)", matchedText, query)
	}
	return "- tool_search: " + matchedText
}

func toolResultContent(toolResult *ToolResultMessage) string {
	if toolResult == nil {
		return ""
	}
	return toolResult.Content
}

// parseToolSearchResultToolNames mirrors `_parse_tool_search_result_tool_names`.
func parseToolSearchResultToolNames(content string) []string {
	var structured map[string]any
	if err := json.Unmarshal([]byte(content), &structured); err == nil {
		if rawNames, ok := structured["matched_tool_names"].([]any); ok {
			names := make([]string, 0, len(rawNames))
			for _, rawName := range rawNames {
				normalized := strings.TrimSpace(fmt.Sprintf("%v", rawName))
				if normalized != "" {
					names = append(names, normalized)
				}
			}
			return names
		}
	}

	names := make([]string, 0)
	for _, rawLine := range strings.Split(content, "\n") {
		normalizedLine := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(normalizedLine, "- ") {
			continue
		}
		normalizedName := strings.TrimSpace(strings.SplitN(normalizedLine[2:], "（", 2)[0])
		if normalizedName != "" {
			names = append(names, normalizedName)
		}
	}
	return names
}

// ---------------------------------------------------------------------------
// _trim_history_to_context_target
// ---------------------------------------------------------------------------

// trimHistoryToContextTarget mirrors `_trim_history_to_context_target`: it
// returns the trimmed history plus the removed entries.
func trimHistoryToContextTarget(
	chatHistory []LLMContextMessage,
	targetContextCount int,
) ([]LLMContextMessage, []LLMContextMessage) {
	remainingContextCount := countInContext(chatHistory)
	if remainingContextCount <= targetContextCount {
		return chatHistory, nil
	}

	removeIndexes := make([]int, 0)
	visitedIndexes := map[int]bool{}
	toolTurnIDs := collectToolTurnIDs(chatHistory)
	for index, entry := range chatHistory {
		if visitedIndexes[index] {
			continue
		}
		if IsMidTermMemoryMessage(entry) {
			continue
		}

		unitIndexes := map[int]bool{index: true}
		logicalTurnID := LogicalTurnIDOf(entry)
		if toolTurnIDs[logicalTurnID] {
			unitIndexes = map[int]bool{}
			for candidateIndex, candidate := range chatHistory {
				if LogicalTurnIDOf(candidate) == logicalTurnID {
					unitIndexes[candidateIndex] = true
				}
			}
		} else if entry.Role() == string(llm.RoleUser) {
			// user 消息触发的工具调用不能在裁切点两侧拆开，否则剩余历史会从
			// function call 开始，违反部分模型的消息顺序要求。
			anchoredToolTurnIDs := collectFollowingToolTurnIDs(chatHistory, index, toolTurnIDs)
			for candidateIndex, candidate := range chatHistory {
				if anchoredToolTurnIDs[LogicalTurnIDOf(candidate)] {
					unitIndexes[candidateIndex] = true
				}
			}
		}

		for unitIndex := range unitIndexes {
			visitedIndexes[unitIndex] = true
			removeIndexes = append(removeIndexes, unitIndex)
		}
		for unitIndex := range unitIndexes {
			if chatHistory[unitIndex].CountInContext() {
				remainingContextCount--
			}
		}
		if remainingContextCount <= targetContextCount {
			break
		}
	}
	if len(removeIndexes) == 0 {
		return chatHistory, nil
	}

	sort.Ints(removeIndexes)
	removeSet := map[int]bool{}
	removedMessages := make([]LLMContextMessage, 0, len(removeIndexes))
	for _, index := range removeIndexes {
		if removeSet[index] {
			continue
		}
		removeSet[index] = true
		removedMessages = append(removedMessages, chatHistory[index])
	}

	// Python deletes the removed indexes from the same list in reverse order; the
	// Go port rebuilds the slice, which is the same observable result.
	trimmed := make([]LLMContextMessage, 0, len(chatHistory)-len(removeSet))
	for index, entry := range chatHistory {
		if removeSet[index] {
			continue
		}
		trimmed = append(trimmed, entry)
	}
	return trimmed, removedMessages
}

func collectFollowingToolTurnIDs(
	chatHistory []LLMContextMessage,
	userIndex int,
	toolTurnIDs map[string]bool,
) map[string]bool {
	following := map[string]bool{}
	for index := userIndex + 1; index < len(chatHistory); index++ {
		if chatHistory[index].Role() == string(llm.RoleUser) {
			break
		}
		logicalTurnID := LogicalTurnIDOf(chatHistory[index])
		if toolTurnIDs[logicalTurnID] {
			following[logicalTurnID] = true
		}
	}
	return following
}
