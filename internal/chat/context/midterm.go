package context

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"oh-my-qqbot/internal/message"
)

// This file mirrors the *message part* of
// 参考/MaiBot/src/maisaka/memory/mid_term.py (is_mid_term_memory_message /
// build_mid_term_memory_complex_message / insert_mid_term_memory_message /
// build_mid_term_memory_preview_text / _build_summary_message_id). The LLM
// summarisation itself belongs to the memory work package (P6): the runtime
// receives the summary payload and folds it through InsertMidTermMemoryMessage.
const (
	// MidTermMemoryComponentType mirrors MID_TERM_MEMORY_COMPONENT_TYPE.
	MidTermMemoryComponentType = "mid_term_memory"
	// MidTermMemorySourceKind mirrors MID_TERM_MEMORY_SOURCE_KIND.
	MidTermMemorySourceKind = "mid_term_memory"
	// MidTermMemoryComplexType mirrors MID_TERM_MEMORY_COMPLEX_TYPE.
	MidTermMemoryComplexType = "mid_term_memory"
	// MidTermMemoryUserName mirrors MID_TERM_MEMORY_USER_NAME.
	MidTermMemoryUserName = "聊天回想"
	// MidTermMemoryReferenceMarker mirrors MID_TERM_MEMORY_REFERENCE_MARKER.
	MidTermMemoryReferenceMarker = "【聊天回想-内部参考】"
	// MidTermMemoryPreviewPrefix mirrors the preview heading of
	// build_mid_term_memory_preview_text.
	MidTermMemoryPreviewPrefix = "[聊天回想]"
)

// MidTermMemorySummaryPayload mirrors the `data` payload stored inside the
// DictComponent of a chat recall message.
type MidTermMemorySummaryPayload struct {
	TimeRange    string           `json:"time_range"`
	Participants []string         `json:"participants"`
	Summary      string           `json:"summary"`
	RecallCues   []map[string]any `json:"recall_cues"`
}

// IsMidTermMemoryMessage mirrors mid_term.is_mid_term_memory_message.
func IsMidTermMemoryMessage(entry LLMContextMessage) bool {
	complexMessage, ok := entry.(*ComplexSessionMessage)
	if !ok {
		return false
	}
	return complexMessage.SourceKind == MidTermMemorySourceKind &&
		complexMessage.ComplexMessageType == MidTermMemoryComplexType
}

// IsMidTermMemoryReferenceMessage mirrors
// mid_term.is_mid_term_memory_reference_message.
func IsMidTermMemoryReferenceMessage(entry LLMContextMessage) bool {
	reference, ok := entry.(*ReferenceMessage)
	if !ok {
		return false
	}
	return reference.ReferenceKind == ReferenceMemory &&
		strings.HasPrefix(reference.Content, MidTermMemoryReferenceMarker)
}

// BuildMidTermMemoryPreviewText mirrors
// mid_term.build_mid_term_memory_preview_text.
func BuildMidTermMemoryPreviewText(payload MidTermMemorySummaryPayload) string {
	timeRange := strings.TrimSpace(payload.TimeRange)
	if timeRange == "" {
		timeRange = "未知"
	}
	participants := "未知"
	if len(payload.Participants) > 0 {
		participants = strings.Join(payload.Participants, "、")
	}
	summary := strings.TrimSpace(payload.Summary)
	if summary == "" {
		summary = "无"
	}
	return strings.Join([]string{
		MidTermMemoryPreviewPrefix,
		"时间范围: " + timeRange,
		"参与人物: " + participants,
		"summary: " + summary,
	}, "\n")
}

// BuildMidTermMemoryComplexMessage mirrors
// mid_term.build_mid_term_memory_complex_message: it assembles the recall entry
// (DictComponent payload + visible text + planner prefix).
func BuildMidTermMemoryComplexMessage(
	payload MidTermMemorySummaryPayload,
	sourceMessages []LLMContextMessage,
) *ComplexSessionMessage {
	timestamp := resolveSummaryTimestamp(sourceMessages)
	participantsText := "未知"
	if len(payload.Participants) > 0 {
		participantsText = strings.Join(payload.Participants, "、")
	}
	timeRange := strings.TrimSpace(payload.TimeRange)
	if timeRange == "" {
		timeRange = "未知"
	}
	messageID := BuildSummaryMessageID(timestamp, timeRange, payload.Participants, payload.Summary)
	previewText := BuildMidTermMemoryPreviewText(payload)
	plannerPrefix := BuildSummaryPlannerPrefix(timestamp, messageID)
	visibleText := strings.Join([]string{
		"[" + MidTermMemoryUserName + "]",
		"时间范围: " + timeRange,
		"参与人物: " + participantsText,
		"summary: " + strings.TrimSpace(payload.Summary),
	}, "\n")

	dictPayload := map[string]any{
		"type": MidTermMemoryComponentType,
		"data": map[string]any{
			"time_range":   timeRange,
			"participants": payload.Participants,
			"summary":      strings.TrimSpace(payload.Summary),
			"recall_cues":  payload.RecallCues,
		},
	}
	sequence := message.NewMessageSequence(&message.DictComponent{Data: dictPayload})
	return NewComplexSessionMessage(
		sequence,
		visibleText,
		timestamp,
		messageID,
		nil,
		MidTermMemorySourceKind,
		plannerPrefix+previewText,
		MidTermMemoryComplexType,
	)
}

// InsertMidTermMemoryMessage mirrors
// mid_term.insert_mid_term_memory_message: insert the new recall right after the
// previous one and keep at most maxSummaryCount recalls.
func InsertMidTermMemoryMessage(
	history []LLMContextMessage,
	summaryMessage *ComplexSessionMessage,
	maxSummaryCount int,
) []LLMContextMessage {
	if maxSummaryCount <= 0 {
		filtered := make([]LLMContextMessage, 0, len(history))
		for _, entry := range history {
			if IsMidTermMemoryMessage(entry) {
				continue
			}
			filtered = append(filtered, entry)
		}
		return filtered
	}

	insertIndex := findLastMidTermMemoryIndex(history)
	updated := make([]LLMContextMessage, 0, len(history)+1)
	updated = append(updated, history[:insertIndex+1]...)
	updated = append(updated, summaryMessage)
	updated = append(updated, history[insertIndex+1:]...)
	return trimMidTermMemoryMessages(updated, maxSummaryCount)
}

func findLastMidTermMemoryIndex(history []LLMContextMessage) int {
	lastIndex := -1
	for index, entry := range history {
		if IsMidTermMemoryMessage(entry) {
			lastIndex = index
		}
	}
	return lastIndex
}

func trimMidTermMemoryMessages(history []LLMContextMessage, maxSummaryCount int) []LLMContextMessage {
	summaryIndexes := make([]int, 0)
	for index, entry := range history {
		if IsMidTermMemoryMessage(entry) {
			summaryIndexes = append(summaryIndexes, index)
		}
	}
	excessCount := len(summaryIndexes) - maxSummaryCount
	if excessCount <= 0 {
		return history
	}
	dropSet := map[int]bool{}
	for _, index := range summaryIndexes[:excessCount] {
		dropSet[index] = true
	}
	filtered := make([]LLMContextMessage, 0, len(history)-excessCount)
	for index, entry := range history {
		if dropSet[index] {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

// GetMidTermMemoryPayload mirrors `_get_mid_term_memory_payload`.
func GetMidTermMemoryPayload(entry LLMContextMessage) MidTermMemorySummaryPayload {
	complexMessage, ok := entry.(*ComplexSessionMessage)
	if !ok || !IsMidTermMemoryMessage(entry) {
		return MidTermMemorySummaryPayload{}
	}
	if complexMessage.RawMessage == nil {
		return MidTermMemorySummaryPayload{}
	}
	for _, component := range complexMessage.RawMessage.Components {
		dictComponent, ok := component.(*message.DictComponent)
		if !ok {
			continue
		}
		rawType, _ := dictComponent.Data["type"].(string)
		if strings.TrimSpace(rawType) != MidTermMemoryComponentType {
			continue
		}
		rawData, ok := dictComponent.Data["data"].(map[string]any)
		if !ok {
			return MidTermMemorySummaryPayload{}
		}
		payload := MidTermMemorySummaryPayload{
			TimeRange: stringValue(rawData["time_range"]),
			Summary:   resolvePayloadSummary(rawData),
		}
		if participants, ok := rawData["participants"].([]any); ok {
			for _, participant := range participants {
				normalized := strings.TrimSpace(fmt.Sprintf("%v", participant))
				if normalized != "" {
					payload.Participants = append(payload.Participants, normalized)
				}
			}
		} else if participants, ok := rawData["participants"].([]string); ok {
			payload.Participants = append(payload.Participants, participants...)
		}
		payload.RecallCues = coerceRecallCues(rawData["recall_cues"])
		return payload
	}
	return MidTermMemorySummaryPayload{}
}

// coerceRecallCues mirrors `_iter_recall_cue_payloads`: the payload keeps the
// cue objects as a list of dicts (JSON round-trips yield []any, an in-memory
// payload yields []map[string]any).
func coerceRecallCues(value any) []map[string]any {
	cues := make([]map[string]any, 0)
	switch typed := value.(type) {
	case []map[string]any:
		for _, cue := range typed {
			if cue != nil {
				cues = append(cues, cue)
			}
		}
	case []any:
		for _, cue := range typed {
			if cuePayload, ok := cue.(map[string]any); ok {
				cues = append(cues, cuePayload)
			}
		}
	}
	return cues
}

func resolvePayloadSummary(rawData map[string]any) string {
	for _, key := range []string{"summary", "long_summary", "brief"} {
		if value := strings.TrimSpace(stringValue(rawData[key])); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprintf("%v", value)
}

// BuildSummaryMessageID mirrors `_build_summary_message_id`
// (`mtm:{base36(ms)}:{sha1[:8]}`).
func BuildSummaryMessageID(timestamp time.Time, timeRange string, participants []string, summary string) string {
	digestSource := strings.Join([]string{timeRange, strings.Join(participants, "、"), summary}, "\n")
	sum := sha1.Sum([]byte(digestSource))
	digest := hex.EncodeToString(sum[:])[:8]
	return fmt.Sprintf("mtm:%s:%s", toBase36(timestamp.UnixMilli()), digest)
}

// BuildSummaryPlannerPrefix mirrors `_build_summary_planner_prefix`.
func BuildSummaryPlannerPrefix(timestamp time.Time, messageID string) string {
	return fmt.Sprintf("<message msg_id=\"%s\" time=\"%s\">\n",
		ToDisplayMessageID(messageID), timestamp.Format("15:04:05"))
}

func resolveSummaryTimestamp(messages []LLMContextMessage) time.Time {
	var latest time.Time
	for _, entry := range messages {
		if entry == nil {
			continue
		}
		timestamp := entry.Timestamp()
		if timestamp.After(latest) {
			latest = timestamp
		}
	}
	if latest.IsZero() {
		return time.Now()
	}
	return latest
}

func toBase36(value int64) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	if value <= 0 {
		return "0"
	}
	digits := make([]string, 0, 13)
	for value > 0 {
		digits = append(digits, string(alphabet[value%36]))
		value /= 36
	}
	// reverse
	for left, right := 0, len(digits)-1; left < right; left, right = left+1, right-1 {
		digits[left], digits[right] = digits[right], digits[left]
	}
	return strings.Join(digits, "")
}
