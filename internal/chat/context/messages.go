package context

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/message"
)

// Constants mirroring 参考/MaiBot/src/maisaka/context/messages.py.
const (
	// ForwardPreviewLimit mirrors FORWARD_PREVIEW_LIMIT.
	ForwardPreviewLimit = 4
	// FocusCooldownWakeupSource mirrors FOCUS_COOLDOWN_WAKEUP_SOURCE.
	FocusCooldownWakeupSource = "focus_cooldown_wakeup"
	// FocusAtWakeupSource mirrors FOCUS_AT_WAKEUP_SOURCE.
	FocusAtWakeupSource = "focus_at_wakeup"
)

// FocusWakeupSourceKinds mirrors FOCUS_WAKEUP_SOURCE_KINDS.
var FocusWakeupSourceKinds = map[string]bool{
	FocusCooldownWakeupSource: true,
	FocusAtWakeupSource:       true,
}

// Source kinds used across the Maisaka context layer.
const (
	SourceUser            = "user"
	SourceGuidedReply     = "guided_reply"
	SourceAssistant       = "assistant"
	SourcePerception      = "perception"
	SourceMidTermMemory   = "mid_term_memory"
	SourceOptimizedTools  = "optimized_tool_history"
	SourceToolResultMedia = "tool_result_media"
	SourceOutboundSend    = "outbound_send"
)

// ReferenceMessageType mirrors ReferenceMessageType in messages.py.
type ReferenceMessageType string

// Reference message types.
const (
	ReferenceBehaviorPattern ReferenceMessageType = "behavior_pattern"
	ReferenceContextRestore  ReferenceMessageType = "context_restore"
	ReferenceCustom          ReferenceMessageType = "custom"
	ReferenceJargon          ReferenceMessageType = "jargon"
	ReferenceMemory          ReferenceMessageType = "memory"
	ReferencePlannerToolHint ReferenceMessageType = "planner_tool_hint"
	ReferenceToolHint        ReferenceMessageType = "tool_hint"
)

// ModelOutputKind mirrors the ModelOutputItem union used by
// ModelOutputContextMessage (`AssistantMessageItem`, `ReasoningItem`,
// `FunctionCallItem`, `ProviderActivityItem`, `ProviderOpaqueItem`).
type ModelOutputKind string

// Model output item kinds.
const (
	ModelOutputAssistant        ModelOutputKind = "assistant"
	ModelOutputReasoning        ModelOutputKind = "reasoning"
	ModelOutputFunctionCall     ModelOutputKind = "function_call"
	ModelOutputProviderActivity ModelOutputKind = "provider_activity"
	ModelOutputProviderOpaque   ModelOutputKind = "provider_opaque"
)

// LLMContextMessage mirrors LLMContextMessage (messages.py): the unified
// context entry used by the Maisaka history.
type LLMContextMessage interface {
	// Role returns the LLM message role (`user` / `assistant` / `tool` /
	// `reasoning` / `provider_activity` / `provider_opaque`).
	Role() string
	// ProcessedPlainText returns the readable plain text of the entry.
	ProcessedPlainText() string
	// CountInContext reports whether the entry consumes the normal
	// user/assistant context window (Python `count_in_context`).
	CountInContext() bool
	// Source returns the message source (Python `source`).
	Source() string
	// Timestamp returns the entry timestamp.
	Timestamp() time.Time
	// ToContextItem converts the entry into a unique Context Item; nil when
	// the entry has no model-facing content.
	ToContextItem(enableVisualMessage bool) *llm.ContextItem
	// ConsumeOnce consumes one lifecycle; false means the entry must be
	// dropped by the post-processor (Python `consume_once`).
	ConsumeOnce() bool
	// MessageID returns the underlying message id ("" when unavailable).
	MessageID() string
	// OriginalMessage returns the original SessionMessage when available.
	OriginalMessage() *message.SessionMessage
}

// ---------------------------------------------------------------------------
// SessionBackedMessage
// ---------------------------------------------------------------------------

// SessionBackedMessage mirrors SessionBackedMessage: a real conversation
// message projected into the model context.
type SessionBackedMessage struct {
	RawMessage    *message.MessageSequence
	VisibleText   string
	MsgTimestamp  time.Time
	MsgID         string
	Original      *message.SessionMessage
	SourceKind    string
	ContextItemID string
}

// NewSessionBackedMessage builds a session-backed entry.
func NewSessionBackedMessage(
	rawMessage *message.MessageSequence,
	visibleText string,
	timestamp time.Time,
	messageID string,
	original *message.SessionMessage,
	sourceKind string,
) *SessionBackedMessage {
	if strings.TrimSpace(sourceKind) == "" {
		sourceKind = SourceUser
	}
	return &SessionBackedMessage{
		RawMessage:    rawMessage,
		VisibleText:   visibleText,
		MsgTimestamp:  timestamp,
		MsgID:         messageID,
		Original:      original,
		SourceKind:    sourceKind,
		ContextItemID: NewContextItemID(),
	}
}

// SessionBackedFromSessionMessage mirrors
// SessionBackedMessage.from_session_message.
func SessionBackedFromSessionMessage(
	sessionMessage *message.SessionMessage,
	rawMessage *message.MessageSequence,
	visibleText string,
	sourceKind string,
) *SessionBackedMessage {
	if strings.TrimSpace(sourceKind) == "" {
		sourceKind = SourceUser
	}
	if rawMessage == nil {
		rawMessage = &message.MessageSequence{}
	}
	return &SessionBackedMessage{
		RawMessage:    rawMessage,
		VisibleText:   visibleText,
		MsgTimestamp:  sessionMessage.Timestamp,
		MsgID:         sessionMessage.MessageID,
		Original:      sessionMessage,
		SourceKind:    sourceKind,
		ContextItemID: NewContextItemID(),
	}
}

// Role implements LLMContextMessage.
func (m *SessionBackedMessage) Role() string { return string(llm.RoleUser) }

// ProcessedPlainText implements LLMContextMessage.
func (m *SessionBackedMessage) ProcessedPlainText() string { return m.VisibleText }

// CountInContext implements LLMContextMessage.
func (m *SessionBackedMessage) CountInContext() bool { return true }

// Source implements LLMContextMessage.
func (m *SessionBackedMessage) Source() string { return m.SourceKind }

// Timestamp implements LLMContextMessage.
func (m *SessionBackedMessage) Timestamp() time.Time { return m.MsgTimestamp }

// MessageID implements LLMContextMessage.
func (m *SessionBackedMessage) MessageID() string { return m.MsgID }

// OriginalMessage implements LLMContextMessage.
func (m *SessionBackedMessage) OriginalMessage() *message.SessionMessage { return m.Original }

// ConsumeOnce implements LLMContextMessage.
func (m *SessionBackedMessage) ConsumeOnce() bool { return true }

// ToContextItem implements LLMContextMessage: build the item from the raw
// message sequence, falling back to visible_text when nothing renders.
func (m *SessionBackedMessage) ToContextItem(enableVisualMessage bool) *llm.ContextItem {
	item, err := BuildItemFromSequence(
		llm.RoleUser,
		m.RawMessage,
		m.ProcessedPlainText(),
		ItemBuildOptions{
			EnableVisualMessage: enableVisualMessage,
			Meta:                &llm.ContextItemMeta{ItemID: m.ContextItemID, Timestamp: m.MsgTimestamp},
		},
	)
	if err != nil {
		return nil
	}
	return item
}

// ---------------------------------------------------------------------------
// ComplexSessionMessage
// ---------------------------------------------------------------------------

// ComplexSessionMessage mirrors ComplexSessionMessage: a session message that
// must be expanded through a forward-browsing tool.
type ComplexSessionMessage struct {
	SessionBackedMessage
	PromptText         string
	ComplexMessageType string
}

// NewComplexSessionMessage builds a complex (forward / mid-term memory) entry.
func NewComplexSessionMessage(
	rawMessage *message.MessageSequence,
	visibleText string,
	timestamp time.Time,
	messageID string,
	original *message.SessionMessage,
	sourceKind string,
	promptText string,
	complexMessageType string,
) *ComplexSessionMessage {
	base := NewSessionBackedMessage(rawMessage, visibleText, timestamp, messageID, original, sourceKind)
	if strings.TrimSpace(complexMessageType) == "" {
		complexMessageType = "forward"
	}
	return &ComplexSessionMessage{
		SessionBackedMessage: *base,
		PromptText:           promptText,
		ComplexMessageType:   complexMessageType,
	}
}

// Source implements LLMContextMessage: `{source_kind}:{complex_message_type}`.
func (m *ComplexSessionMessage) Source() string {
	return m.SourceKind + ":" + m.ComplexMessageType
}

// ProcessedPlainText implements LLMContextMessage (the visible text is the
// folded/expanded body).
func (m *ComplexSessionMessage) ProcessedPlainText() string {
	return m.SessionBackedMessage.ProcessedPlainText()
}

// ToContextItem implements LLMContextMessage: the complex entry is projected as
// a single text item built from prompt_text.
func (m *ComplexSessionMessage) ToContextItem(enableVisualMessage bool) *llm.ContextItem {
	_ = enableVisualMessage
	sequence := message.NewMessageSequence(message.NewTextComponent(m.PromptText))
	item, err := BuildItemFromSequence(
		llm.RoleUser,
		sequence,
		m.PromptText,
		ItemBuildOptions{
			Meta: &llm.ContextItemMeta{ItemID: m.ContextItemID, Timestamp: m.MsgTimestamp},
		},
	)
	if err != nil {
		return nil
	}
	return item
}

// ComplexFromSessionMessage mirrors
// ComplexSessionMessage.from_session_message; ok is false when the sequence
// renders no prompt text (Python returns None).
func ComplexFromSessionMessage(
	sessionMessage *message.SessionMessage,
	plannerPrefix string,
	visibleText string,
	sourceKind string,
) (*ComplexSessionMessage, bool) {
	promptText := BuildComplexMessagePromptText(sessionMessage.RawMessage)
	if promptText == "" {
		return nil, false
	}
	return NewComplexSessionMessage(
		sessionMessage.RawMessage,
		visibleText,
		sessionMessage.Timestamp,
		sessionMessage.MessageID,
		sessionMessage,
		sourceKind,
		plannerPrefix+promptText,
		"forward",
	), true
}

// ---------------------------------------------------------------------------
// ReferenceMessage
// ---------------------------------------------------------------------------

// ReferenceMessage mirrors ReferenceMessage: a temporary hint that is not
// counted into the context window.
type ReferenceMessage struct {
	Content       string
	MsgTimestamp  time.Time
	ReferenceKind ReferenceMessageType
	RemainingUses *int
	DisplayPrefix string
	ContextItemID string
	JargonEntries [][2]string
}

// NewReferenceMessage mirrors ReferenceMessage's dataclass defaults.
func NewReferenceMessage(content string, timestamp time.Time, kind ReferenceMessageType, remainingUses *int, displayPrefix string) *ReferenceMessage {
	if strings.TrimSpace(string(kind)) == "" {
		kind = ReferenceCustom
	}
	if strings.TrimSpace(displayPrefix) == "" {
		displayPrefix = "[参考消息]"
	}
	return &ReferenceMessage{
		Content:       content,
		MsgTimestamp:  timestamp,
		ReferenceKind: kind,
		RemainingUses: remainingUses,
		DisplayPrefix: displayPrefix,
		ContextItemID: NewContextItemID(),
	}
}

// Role implements LLMContextMessage.
func (m *ReferenceMessage) Role() string { return string(llm.RoleUser) }

// ProcessedPlainText implements LLMContextMessage.
func (m *ReferenceMessage) ProcessedPlainText() string {
	return strings.TrimSpace(m.DisplayPrefix + "\n" + m.Content)
}

// CountInContext implements LLMContextMessage: reference messages never count.
func (m *ReferenceMessage) CountInContext() bool { return false }

// Source implements LLMContextMessage.
func (m *ReferenceMessage) Source() string { return string(m.ReferenceKind) }

// Timestamp implements LLMContextMessage.
func (m *ReferenceMessage) Timestamp() time.Time { return m.MsgTimestamp }

// MessageID implements LLMContextMessage (references carry no message id).
func (m *ReferenceMessage) MessageID() string { return "" }

// OriginalMessage implements LLMContextMessage.
func (m *ReferenceMessage) OriginalMessage() *message.SessionMessage { return nil }

// ToContextItem implements LLMContextMessage.
func (m *ReferenceMessage) ToContextItem(enableVisualMessage bool) *llm.ContextItem {
	_ = enableVisualMessage
	text := m.ProcessedPlainText()
	sequence := message.NewMessageSequence(message.NewTextComponent(text))
	item, err := BuildItemFromSequence(
		llm.RoleUser,
		sequence,
		text,
		ItemBuildOptions{Meta: &llm.ContextItemMeta{ItemID: m.ContextItemID, Timestamp: m.MsgTimestamp}},
	)
	if err != nil {
		return nil
	}
	return item
}

// ConsumeOnce implements LLMContextMessage: nil means "keep forever",
// otherwise decrement and drop at zero.
func (m *ReferenceMessage) ConsumeOnce() bool {
	if m.RemainingUses == nil {
		return true
	}
	*m.RemainingUses--
	return *m.RemainingUses > 0
}

// ---------------------------------------------------------------------------
// ModelOutputContextMessage
// ---------------------------------------------------------------------------

// ModelOutputContextMessage mirrors ModelOutputContextMessage: one model output
// item stored into the Maisaka history.
type ModelOutputContextMessage struct {
	OutputItem *llm.ContextItem
	SourceKind string
}

// NewModelOutputContextMessage builds one history entry from a model output
// item. Item kinds of the input `ContextItem` are used as the ModelOutputItem
// discriminator, matching `ReasoningItem | AssistantMessageItem |
// FunctionCallItem | ProviderActivityItem | ProviderOpaqueItem`.
func NewModelOutputContextMessage(outputItem *llm.ContextItem, sourceKind string) *ModelOutputContextMessage {
	if strings.TrimSpace(sourceKind) == "" {
		sourceKind = SourceAssistant
	}
	return &ModelOutputContextMessage{OutputItem: outputItem, SourceKind: sourceKind}
}

// Kind returns the ModelOutputItem union member of this entry.
func (m *ModelOutputContextMessage) Kind() ModelOutputKind {
	if m.OutputItem == nil {
		return ModelOutputAssistant
	}
	switch m.OutputItem.Kind {
	case llm.ItemKindFunctionCall:
		return ModelOutputFunctionCall
	case llm.ItemKindAssistant:
		if m.OutputItem.Phase == string(ModelOutputReasoning) {
			return ModelOutputReasoning
		}
		if m.OutputItem.Phase == string(ModelOutputProviderActivity) {
			return ModelOutputProviderActivity
		}
		if m.OutputItem.Phase == string(ModelOutputProviderOpaque) {
			return ModelOutputProviderOpaque
		}
		return ModelOutputAssistant
	default:
		return ModelOutputAssistant
	}
}

// Role implements LLMContextMessage: reasoning / provider items expose their own
// pseudo-roles exactly like the Python implementation.
func (m *ModelOutputContextMessage) Role() string {
	switch m.Kind() {
	case ModelOutputReasoning:
		return "reasoning"
	case ModelOutputProviderActivity:
		return "provider_activity"
	case ModelOutputProviderOpaque:
		return "provider_opaque"
	default:
		return string(llm.RoleAssistant)
	}
}

// ProcessedPlainText implements LLMContextMessage.
func (m *ModelOutputContextMessage) ProcessedPlainText() string {
	if m.OutputItem == nil {
		return ""
	}
	return m.OutputItem.Text()
}

// CountInContext implements LLMContextMessage: perception items are excluded.
func (m *ModelOutputContextMessage) CountInContext() bool { return m.SourceKind != SourcePerception }

// Source implements LLMContextMessage.
func (m *ModelOutputContextMessage) Source() string { return m.SourceKind }

// Timestamp implements LLMContextMessage.
func (m *ModelOutputContextMessage) Timestamp() time.Time {
	if m.OutputItem == nil {
		return time.Time{}
	}
	return m.OutputItem.Meta.Timestamp
}

// MessageID implements LLMContextMessage (model output carries no message id).
func (m *ModelOutputContextMessage) MessageID() string { return "" }

// OriginalMessage implements LLMContextMessage.
func (m *ModelOutputContextMessage) OriginalMessage() *message.SessionMessage { return nil }

// LogicalTurnID returns the logical tool turn id ("" when unset).
func (m *ModelOutputContextMessage) LogicalTurnID() string {
	if m.OutputItem == nil {
		return ""
	}
	return m.OutputItem.Meta.LogicalTurnID
}

// ItemID returns the stable context item id.
func (m *ModelOutputContextMessage) ItemID() string {
	if m.OutputItem == nil {
		return ""
	}
	return m.OutputItem.Meta.ItemID
}

// ToolCalls mirrors the `tool_calls` property: every function call carried by
// this item (materialized args).
func (m *ModelOutputContextMessage) ToolCalls() []ToolCall {
	if m.OutputItem == nil || m.OutputItem.Kind != llm.ItemKindFunctionCall || m.OutputItem.ToolCall == nil {
		return nil
	}
	return []ToolCall{{
		CallID:       m.OutputItem.ToolCall.CallID,
		FuncName:     m.OutputItem.ToolCall.FuncName,
		Args:         m.OutputItem.ToolCall.MaterializeArgs(),
		ExtraContent: m.OutputItem.ToolCall.ExtraContentJSON,
	}}
}

// ConsumeOnce implements LLMContextMessage.
func (m *ModelOutputContextMessage) ConsumeOnce() bool { return true }

// ToContextItem implements LLMContextMessage.
func (m *ModelOutputContextMessage) ToContextItem(enableVisualMessage bool) *llm.ContextItem {
	_ = enableVisualMessage
	return m.OutputItem
}

// BuildModelOutputContextMessages mirrors
// build_model_output_context_messages: wrap each output item without creating a
// response-level container.
func BuildModelOutputContextMessages(outputItems []*llm.ContextItem, sourceKind string) []LLMContextMessage {
	if strings.TrimSpace(sourceKind) == "" {
		sourceKind = SourceAssistant
	}
	messages := make([]LLMContextMessage, 0, len(outputItems))
	for _, item := range outputItems {
		if item == nil {
			continue
		}
		messages = append(messages, NewModelOutputContextMessage(item, sourceKind))
	}
	return messages
}

// ToolCall mirrors the ToolCall view exposed by ModelOutputContextMessage.
type ToolCall struct {
	CallID       string
	FuncName     string
	Args         map[string]any
	ExtraContent string
}

// ArgsJSON mirrors ToolCall.materialize_args serialized with sorted keys, used
// by the folded tool history renderer (`json.dumps(..., sort_keys=True)`).
func (c ToolCall) ArgsJSON() string {
	if len(c.Args) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(sortedMap(c.Args))
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// ---------------------------------------------------------------------------
// ToolResultMessage
// ---------------------------------------------------------------------------

// ToolResultMessage mirrors ToolResultMessage.
type ToolResultMessage struct {
	Content        string
	MsgTimestamp   time.Time
	ToolCallID     string
	LogicalTurnID_ string
	ToolName       string
	Success        bool
	Metadata       map[string]any
	ContextItemID  string
}

// NewToolResultMessage builds a tool result entry, validating the same
// invariants as the Python `__post_init__`.
func NewToolResultMessage(
	content string,
	timestamp time.Time,
	toolCallID string,
	logicalTurnID string,
	toolName string,
	success bool,
) (*ToolResultMessage, error) {
	if strings.TrimSpace(toolCallID) == "" {
		return nil, fmt.Errorf("context: 工具结果的调用 ID 不能为空")
	}
	if strings.TrimSpace(logicalTurnID) == "" {
		return nil, fmt.Errorf("context: 工具结果必须具有 logical_turn_id: %s", toolCallID)
	}
	return &ToolResultMessage{
		Content:        content,
		MsgTimestamp:   timestamp,
		ToolCallID:     toolCallID,
		LogicalTurnID_: logicalTurnID,
		ToolName:       toolName,
		Success:        success,
		Metadata:       map[string]any{},
		ContextItemID:  NewContextItemID(),
	}, nil
}

// LogicalTurnID returns the owning logical tool turn.
func (m *ToolResultMessage) LogicalTurnID() string { return m.LogicalTurnID_ }

// Role implements LLMContextMessage.
func (m *ToolResultMessage) Role() string { return string(llm.RoleTool) }

// ProcessedPlainText implements LLMContextMessage.
func (m *ToolResultMessage) ProcessedPlainText() string { return m.Content }

// CountInContext implements LLMContextMessage: tool results never count.
func (m *ToolResultMessage) CountInContext() bool { return false }

// Source implements LLMContextMessage.
func (m *ToolResultMessage) Source() string {
	if strings.TrimSpace(m.ToolName) != "" {
		return m.ToolName
	}
	return "tool"
}

// Timestamp implements LLMContextMessage.
func (m *ToolResultMessage) Timestamp() time.Time { return m.MsgTimestamp }

// MessageID implements LLMContextMessage (tool results carry no message id).
func (m *ToolResultMessage) MessageID() string { return "" }

// OriginalMessage implements LLMContextMessage.
func (m *ToolResultMessage) OriginalMessage() *message.SessionMessage { return nil }

// ConsumeOnce implements LLMContextMessage.
func (m *ToolResultMessage) ConsumeOnce() bool { return true }

// ToContextItem implements LLMContextMessage.
func (m *ToolResultMessage) ToContextItem(enableVisualMessage bool) *llm.ContextItem {
	_ = enableVisualMessage
	return &llm.ContextItem{
		Meta: llm.ContextItemMeta{
			ItemID:        m.ContextItemID,
			LogicalTurnID: m.LogicalTurnID_,
			Timestamp:     m.MsgTimestamp,
		},
		Kind:     llm.ItemKindToolResult,
		CallID:   m.ToolCallID,
		Output:   m.Content,
		ToolName: m.ToolName,
		Success:  m.Success,
	}
}

// ---------------------------------------------------------------------------
// build_context_items_from_history_entry
// ---------------------------------------------------------------------------

// BuildContextItemsFromHistoryEntry mirrors
// build_context_items_from_history_entry: a history entry converts into zero or
// one Context Items.
func BuildContextItemsFromHistoryEntry(entry LLMContextMessage, enableVisualMessage bool) []*llm.ContextItem {
	if entry == nil {
		return nil
	}
	item := entry.ToContextItem(enableVisualMessage)
	if item == nil {
		return nil
	}
	return []*llm.ContextItem{item}
}

// BuildItemFromSequence mirrors `_build_item_from_sequence` in messages.py: it
// walks a component sequence, appending text/image parts, and falls back to
// fallbackText when nothing rendered. ok=false mirrors the Python `return None`.
func BuildItemFromSequence(
	role contracts.Role,
	sequence *message.MessageSequence,
	fallbackText string,
	options ItemBuildOptions,
) (*llm.ContextItem, error) {
	builder := llm.NewContextItemBuilder().SetRole(role)
	if options.Meta != nil {
		builder.SetMeta(*options.Meta)
	}
	if role == llm.RoleTool {
		if options.ToolCallID != "" {
			builder.SetToolCallID(options.ToolCallID)
		}
		if options.ToolName != "" {
			builder.SetToolName(options.ToolName)
		}
	}

	hasContent := false
	if sequence != nil {
		for _, component := range sequence.Components {
			switch typed := component.(type) {
			case *message.TextComponent:
				if strings.TrimSpace(typed.Text) != "" {
					builder.AddTextPart(typed.Text)
					hasContent = true
				}
			case *message.EmojiComponent:
				ok := appendEmojiComponent(builder, typed, options.EnableVisualMessage)
				hasContent = hasContent || ok
			case *message.ImageComponent:
				ok := appendImageComponent(builder, typed, options.EnableVisualMessage)
				hasContent = hasContent || ok
			case *message.VoiceComponent:
				builder.AddTextPart(componentTextOr(typed.Content, VoiceFallbackText))
				hasContent = true
			case *message.FileComponent:
				builder.AddTextPart(typed.ToPlainText())
				hasContent = true
			case *message.AtComponent:
				rendered := RenderAtComponentText(typed)
				if rendered != "" {
					builder.AddTextPart(rendered)
					hasContent = true
				}
			case *message.ReplyComponent:
				// `_append_reply_component` returns False: the reply relation is
				// carried by the message meta instead of the body.
			}
		}
	}

	if !hasContent && strings.TrimSpace(fallbackText) != "" {
		builder.AddTextPart(fallbackText)
		hasContent = true
	}
	if !hasContent {
		return nil, nil
	}
	return builder.Build()
}

// ItemBuildOptions carries the optional builder inputs of BuildItemFromSequence.
type ItemBuildOptions struct {
	EnableVisualMessage bool
	ToolCallID          string
	ToolName            string
	Meta                *llm.ContextItemMeta
}

func appendEmojiComponent(builder *llm.ContextItemBuilder, component *message.EmojiComponent, enableVisualMessage bool) bool {
	imageFormat := GuessImageFormat(component.BinaryData)
	if enableVisualMessage && imageFormat != "" && len(component.BinaryData) > 0 {
		builder.AddTextPart(EmojiVisualPlaceholder)
		builder.AddImagePart(imageFormat, encodeBase64(component.BinaryData))
		return true
	}
	if normalized := strings.TrimSpace(component.Content); normalized != "" {
		builder.AddTextPart(normalized)
		return true
	}
	builder.AddTextPart(EmojiFallbackText)
	return true
}

func appendImageComponent(builder *llm.ContextItemBuilder, component *message.ImageComponent, enableVisualMessage bool) bool {
	imageFormat := GuessImageFormat(component.BinaryData)
	if enableVisualMessage && imageFormat != "" && len(component.BinaryData) > 0 {
		builder.AddImagePart(imageFormat, encodeBase64(component.BinaryData))
		return true
	}
	if normalized := strings.TrimSpace(component.Content); normalized != "" {
		builder.AddTextPart(normalized)
		return true
	}
	builder.AddTextPart(ImageVisualPlaceholder)
	return true
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// NewContextItemID mirrors `uuid.uuid4().hex` (random 32 hex chars).
func NewContextItemID() string { return randomHex(16) }

func sortedMap(values map[string]any) map[string]any {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sorted := make(map[string]any, len(values))
	for _, key := range keys {
		sorted[key] = values[key]
	}
	return sorted
}

func randomHex(byteCount int) string {
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err != nil {
		now := time.Now().UnixNano()
		for index := range buffer {
			buffer[index] = byte(now >> (uint(index%8) * 8))
		}
	}
	return hex.EncodeToString(buffer)
}

func encodeBase64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }
