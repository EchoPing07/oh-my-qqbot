// Package llm implements the model layer of oh-my-qqbot.
//
// File map and the sources it is aligned with:
//
//   - context_item.go   → 参考/MaiBot/src/llm_models/payload_content/context_item.py
//   - openai_client.go  → 参考/MaiBot/src/llm_models/model_client/openai_client.py
//     (wire structures also informed by 参考/Diana/model/llm/openai_compatible.go
//     and types.go, engineering structure only)
//   - orchestrator.go   → 参考/MaiBot/src/llm_models/utils_model.py (LLMOrchestrator)
//   - usage.go          → 参考/MaiBot/src/llm_models/utils.py (LLMUsageRecorder)
//     and 参考/MaiBot/src/common/database/database_model.py (ModelUsage)
//   - embedding.go      → 参考/MaiBot/src/llm_models/model_client/openai_client.py
//     `get_embedding` (OpenAI-compatible /embeddings)
//
// The packages implement the cross-module internal.LLMLayer contract declared in
// internal/contracts.go without importing other internal packages, so parallel
// work packages can compile against it independently.
package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	contracts "oh-my-qqbot/internal"
)

// RoleType mirrors RoleType in context_item.py.
type RoleType = contracts.Role

// Context roles, re-exported for readability.
const (
	RoleSystem    = contracts.RoleSystem
	RoleUser      = contracts.RoleUser
	RoleAssistant = contracts.RoleAssistant
	RoleTool      = contracts.RoleTool
)

// SupportedImageFormats mirrors SUPPORTED_IMAGE_FORMATS in context_item.py.
var SupportedImageFormats = []string{"jpg", "jpeg", "png", "webp", "gif"}

// Content part kinds mirroring ContextTextPart / ContextImagePart /
// ContextRefusalPart.
const (
	ContentPartText    = "text"
	ContentPartImage   = "image"
	ContentPartRefusal = "refusal"
)

// ContextTextPart mirrors ContextTextPart: an immutable, non-empty text chunk.
type ContextTextPart struct {
	Text string
}

// NewContextTextPart validates the non-empty constraint of the Python model.
func NewContextTextPart(text string) (ContextTextPart, error) {
	if text == "" {
		return ContextTextPart{}, fmt.Errorf("llm: 文本内容片段不能为空字符串")
	}
	return ContextTextPart{Text: text}, nil
}

// ContextImagePart mirrors ContextImagePart (raw base64, no data URI prefix).
type ContextImagePart struct {
	ImageFormat string
	ImageBase64 string
}

// NewContextImagePart validates format and payload like the Python model.
func NewContextImagePart(imageFormat string, imageBase64 string) (ContextImagePart, error) {
	normalized := strings.ToLower(strings.TrimSpace(imageFormat))
	if !containsString(SupportedImageFormats, normalized) {
		return ContextImagePart{}, fmt.Errorf("llm: 不受支持的图片格式: %s", imageFormat)
	}
	if strings.TrimSpace(imageBase64) == "" {
		return ContextImagePart{}, fmt.Errorf("llm: 图片的 base64 编码不能为空")
	}
	return ContextImagePart{ImageFormat: normalized, ImageBase64: imageBase64}, nil
}

// NormalizedImageFormat mirrors ContextImagePart.normalized_image_format.
func (p ContextImagePart) NormalizedImageFormat() string {
	lowered := strings.ToLower(p.ImageFormat)
	if lowered == "jpg" || lowered == "jpeg" {
		return "jpeg"
	}
	return lowered
}

// DataURL renders the part as a data URI for the OpenAI `image_url` field.
func (p ContextImagePart) DataURL() string {
	return "data:image/" + p.NormalizedImageFormat() + ";base64," + p.ImageBase64
}

// ContextRefusalPart mirrors ContextRefusalPart.
type ContextRefusalPart struct {
	Refusal string
}

// ContextContentPart is one piece of an item's content (union of the three
// Python part dataclasses).
type ContextContentPart struct {
	Kind    string
	Text    string
	Image   *ContextImagePart
	Refusal string
}

// NewTextPart builds a text content part.
func NewTextPart(text string) ContextContentPart {
	return ContextContentPart{Kind: ContentPartText, Text: text}
}

// NewImagePart builds an image content part (validated).
func NewImagePart(imageFormat string, imageBase64 string) (ContextContentPart, error) {
	part, err := NewContextImagePart(imageFormat, imageBase64)
	if err != nil {
		return ContextContentPart{}, err
	}
	return ContextContentPart{Kind: ContentPartImage, Image: &part}, nil
}

// NewRefusalPart builds a refusal content part.
func NewRefusalPart(refusal string) ContextContentPart {
	return ContextContentPart{Kind: ContentPartRefusal, Refusal: refusal}
}

// ContextToolCall mirrors ContextToolCall (args kept as canonical JSON).
type ContextToolCall struct {
	CallID           string
	FuncName         string
	ArgsJSON         string
	ExtraContentJSON string
}

// NewContextToolCall builds a tool call from a map of arguments.
func NewContextToolCall(callID string, funcName string, args map[string]any) (*ContextToolCall, error) {
	if strings.TrimSpace(callID) == "" {
		return nil, fmt.Errorf("llm: 工具调用 ID 不能为空")
	}
	if strings.TrimSpace(funcName) == "" {
		return nil, fmt.Errorf("llm: 工具函数名称不能为空")
	}
	if args == nil {
		args = map[string]any{}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("llm: 工具参数必须是 JSON object: %w", err)
	}
	return &ContextToolCall{CallID: callID, FuncName: funcName, ArgsJSON: string(encoded)}, nil
}

// MaterializeArgs mirrors ContextToolCall.materialize_args.
func (c *ContextToolCall) MaterializeArgs() map[string]any {
	args := map[string]any{}
	if c == nil || strings.TrimSpace(c.ArgsJSON) == "" {
		return args
	}
	if err := json.Unmarshal([]byte(c.ArgsJSON), &args); err != nil || args == nil {
		return map[string]any{}
	}
	return args
}

// ContextItemKind enumerates the portable *input* context items. Reason of the
// port: MaiBot's frozen dataclass union is replaced with one struct + a kind
// tag, which keeps JSON round-trips trivial while preserving field semantics.
type ContextItemKind string

const (
	ItemKindSystem       ContextItemKind = "system"
	ItemKindUser         ContextItemKind = "user"
	ItemKindAssistant    ContextItemKind = "assistant"
	ItemKindToolResult   ContextItemKind = "function_call_output"
	ItemKindFunctionCall ContextItemKind = "function_call"
)

// ContextItemMeta mirrors ContextItemMeta (stable item identity + logical turn).
type ContextItemMeta struct {
	ItemID        string    `json:"item_id"`
	LogicalTurnID string    `json:"logical_turn_id,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

// NewContextItemMeta mirrors ContextItemMeta.create.
func NewContextItemMeta(logicalTurnID string, itemID string) ContextItemMeta {
	return ContextItemMeta{
		ItemID:        itemID,
		LogicalTurnID: logicalTurnID,
		Timestamp:     Now(),
	}
}

// ContextItem is the portable input/output context item.
type ContextItem struct {
	Meta  ContextItemMeta
	Kind  ContextItemKind
	Parts []ContextContentPart

	// ToolCall is set for ItemKindFunctionCall.
	ToolCall *ContextToolCall

	// Tool-result fields mirror FunctionCallOutputItem.
	CallID   string
	Output   string
	ToolName string
	Success  bool

	// Phase mirrors AssistantMessageItem.phase (e.g. "final").
	Phase string
}

// Role returns the provider-facing role of the item.
func (i *ContextItem) Role() RoleType {
	switch i.Kind {
	case ItemKindSystem:
		return RoleSystem
	case ItemKindAssistant:
		return RoleAssistant
	case ItemKindToolResult:
		return RoleTool
	default:
		return RoleUser
	}
}

// Text joins the text parts of the item (mirrors get_item_text).
func (i *ContextItem) Text() string {
	if i == nil {
		return ""
	}
	if i.Kind == ItemKindToolResult && i.Output != "" {
		return i.Output
	}
	parts := make([]string, 0, len(i.Parts))
	for _, part := range i.Parts {
		if part.Kind == ContentPartText && part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "")
}

// ContextItemBuilder mirrors ContextItemBuilder. Unlike the Python builder it
// records the first validation error instead of raising, so call chains stay
// readable in Go; Build reports that error.
type ContextItemBuilder struct {
	role       RoleType
	parts      []ContextContentPart
	toolCallID string
	toolName   string
	meta       ContextItemMeta
	phase      string
	err        error
}

// NewContextItemBuilder creates a builder defaulting to the user role, exactly
// like the Python constructor.
func NewContextItemBuilder() *ContextItemBuilder {
	return &ContextItemBuilder{role: RoleUser, meta: NewContextItemMeta("", randomItemID())}
}

// SetRole mirrors ContextItemBuilder.set_role.
func (b *ContextItemBuilder) SetRole(role RoleType) *ContextItemBuilder {
	b.role = role
	return b
}

// SetMeta mirrors ContextItemBuilder.set_meta.
func (b *ContextItemBuilder) SetMeta(meta ContextItemMeta) *ContextItemBuilder {
	b.meta = meta
	return b
}

// AddTextPart mirrors add_text_part / add_text_content.
func (b *ContextItemBuilder) AddTextPart(text string) *ContextItemBuilder {
	b.parts = append(b.parts, NewTextPart(text))
	return b
}

// AddImagePart mirrors add_image_base64_part / add_image_content.
func (b *ContextItemBuilder) AddImagePart(imageFormat string, imageBase64 string) *ContextItemBuilder {
	part, err := NewImagePart(imageFormat, imageBase64)
	if err != nil {
		b.fail(err)
		return b
	}
	b.parts = append(b.parts, part)
	return b
}

// SetToolCallID mirrors set_tool_call_id / add_tool_call.
func (b *ContextItemBuilder) SetToolCallID(toolCallID string) *ContextItemBuilder {
	if b.role != RoleTool {
		b.fail(fmt.Errorf("llm: 仅当角色为 Tool 时才能设置工具调用 ID"))
		return b
	}
	if strings.TrimSpace(toolCallID) == "" {
		b.fail(fmt.Errorf("llm: 工具调用 ID 不能为空"))
		return b
	}
	b.toolCallID = toolCallID
	return b
}

// SetToolName mirrors set_tool_name.
func (b *ContextItemBuilder) SetToolName(toolName string) *ContextItemBuilder {
	if b.role != RoleTool {
		b.fail(fmt.Errorf("llm: 仅当角色为 Tool 时才能设置工具名称"))
		return b
	}
	b.toolName = toolName
	return b
}

// Err reports the first validation error recorded by the builder.
func (b *ContextItemBuilder) Err() error { return b.err }

func (b *ContextItemBuilder) fail(err error) {
	if b.err == nil {
		b.err = err
	}
}

// SetPhase marks the assistant phase (e.g. "final").
func (b *ContextItemBuilder) SetPhase(phase string) *ContextItemBuilder {
	b.phase = phase
	return b
}

// Build mirrors ContextItemBuilder.build.
func (b *ContextItemBuilder) Build() (*ContextItem, error) {
	if b.err != nil {
		return nil, b.err
	}
	item := &ContextItem{Meta: b.meta, Parts: append([]ContextContentPart(nil), b.parts...), Phase: b.phase}
	switch b.role {
	case RoleSystem:
		item.Kind = ItemKindSystem
	case RoleUser:
		item.Kind = ItemKindUser
	case RoleAssistant:
		item.Kind = ItemKindAssistant
	case RoleTool:
		if b.toolCallID == "" {
			return nil, fmt.Errorf("llm: Tool Item 缺少工具调用 ID")
		}
		for _, part := range b.parts {
			if part.Kind != ContentPartText {
				return nil, fmt.Errorf("llm: 工具结果 Item 仅支持文本片段")
			}
		}
		item.Kind = ItemKindToolResult
		item.CallID = b.toolCallID
		item.ToolName = b.toolName
		item.Success = true
		var builder strings.Builder
		for _, part := range b.parts {
			if part.Kind == ContentPartText {
				builder.WriteString(part.Text)
			}
		}
		item.Output = builder.String()
	default:
		item.Kind = ItemKindUser
	}
	if item.Kind != ItemKindToolResult && len(item.Parts) == 0 {
		return nil, fmt.Errorf("llm: 上下文 Item 内容不能为空")
	}
	return item, nil
}

// NewSystemItem builds a system item (common shortcut).
func NewSystemItem(text string) *ContextItem {
	item, err := NewContextItemBuilder().SetRole(RoleSystem).AddTextPart(text).Build()
	if err != nil {
		// The only failure mode here is an empty part list, which is impossible
		// because a text part was just appended.
		return &ContextItem{Meta: NewContextItemMeta("", randomItemID()), Kind: ItemKindSystem, Parts: []ContextContentPart{NewTextPart(text)}}
	}
	return item
}

// NewUserItem builds a user item.
func NewUserItem(text string) *ContextItem {
	item, _ := NewContextItemBuilder().SetRole(RoleUser).AddTextPart(text).Build()
	return item
}

// NewAssistantItem builds an assistant item.
func NewAssistantItem(text string) *ContextItem {
	item, _ := NewContextItemBuilder().SetRole(RoleAssistant).AddTextPart(text).Build()
	return item
}

// NewToolResultItem builds a tool-result item.
func NewToolResultItem(callID string, output string, toolName string) *ContextItem {
	return &ContextItem{
		Meta:     NewContextItemMeta("", randomItemID()),
		Kind:     ItemKindToolResult,
		CallID:   callID,
		Output:   output,
		ToolName: toolName,
		Success:  true,
	}
}

// NewFunctionCallItem builds a function-call item (request side of the loop).
func NewFunctionCallItem(logicalTurnID string, callID string, funcName string, args map[string]any) (*ContextItem, error) {
	toolCall, err := NewContextToolCall(callID, funcName, args)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(logicalTurnID) == "" {
		return nil, fmt.Errorf("llm: function call 必须具有 logical_turn_id: %s", callID)
	}
	return &ContextItem{
		Meta:     NewContextItemMeta(logicalTurnID, randomItemID()),
		Kind:     ItemKindFunctionCall,
		ToolCall: toolCall,
	}, nil
}

// ItemsToMessages projects context items into the provider-agnostic contract
// messages (internal.ChatMessage). Function calls become assistant tool calls,
// tool results become tool messages, matching the OpenAI chat-completions shape
// produced by MaiBot's openai_client._convert_messages.
func ItemsToMessages(items []*ContextItem) []contracts.ChatMessage {
	messages := make([]contracts.ChatMessage, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		if item.Kind == ItemKindFunctionCall {
			call := contracts.ToolCall{}
			if item.ToolCall != nil {
				call.ID = item.ToolCall.CallID
				call.Name = item.ToolCall.FuncName
				call.ArgsJSON = item.ToolCall.ArgsJSON
			}
			messages = append(messages, contracts.ChatMessage{
				Role:      RoleAssistant,
				ToolCalls: []contracts.ToolCall{call},
			})
			continue
		}
		message := contracts.ChatMessage{Role: item.Role(), Content: item.Text()}
		if item.Kind == ItemKindToolResult {
			message.ToolCallID = item.CallID
			message.Name = item.ToolName
		}
		for _, part := range item.Parts {
			if part.Kind == ContentPartImage && part.Image != nil {
				message.Images = append(message.Images, contracts.ImagePart{
					Base64: part.Image.DataURL(),
					Detail: "auto",
				})
			}
		}
		messages = append(messages, message)
	}
	return messages
}

// MessagesToItems projects contract messages back into context items (used by
// callers that keep history as internal.ChatMessage).
func MessagesToItems(messages []contracts.ChatMessage) []*ContextItem {
	items := make([]*ContextItem, 0, len(messages))
	for _, message := range messages {
		item := &ContextItem{Meta: NewContextItemMeta("", randomItemID())}
		switch message.Role {
		case RoleSystem:
			item.Kind = ItemKindSystem
		case RoleAssistant:
			item.Kind = ItemKindAssistant
		case RoleTool:
			item.Kind = ItemKindToolResult
			item.CallID = message.ToolCallID
			item.ToolName = message.Name
			item.Output = message.Content
			item.Success = true
		default:
			item.Kind = ItemKindUser
		}
		if item.Kind != ItemKindToolResult {
			if message.Content != "" {
				item.Parts = append(item.Parts, NewTextPart(message.Content))
			}
			for _, image := range message.Images {
				format, base64Data := splitDataURL(image.Base64)
				native := ContextImagePart{ImageFormat: format, ImageBase64: base64Data}
				item.Parts = append(item.Parts, ContextContentPart{Kind: ContentPartImage, Image: &native})
			}
			for _, call := range message.ToolCalls {
				toolCall := &ContextToolCall{CallID: call.ID, FuncName: call.Name, ArgsJSON: defaultJSONObject(call.ArgsJSON)}
				item.Kind = ItemKindFunctionCall
				item.ToolCall = toolCall
			}
		}
		items = append(items, item)
	}
	return items
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func defaultJSONObject(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	if !json.Valid([]byte(raw)) {
		return "{}"
	}
	return raw
}

// splitDataURL mirrors the informal `data:<mime>;base64,<payload>` convention
// used across MaiBot when images travel as data URIs.
func splitDataURL(value string) (string, string) {
	if !strings.HasPrefix(value, "data:") {
		return "jpeg", value
	}
	rest := strings.TrimPrefix(value, "data:")
	mime, payload, found := strings.Cut(rest, ";base64,")
	if !found {
		return "jpeg", value
	}
	format := strings.TrimPrefix(mime, "image/")
	switch strings.ToLower(format) {
	case "jpg", "jpeg":
		return "jpeg", payload
	case "png", "webp", "gif":
		return strings.ToLower(format), payload
	default:
		return "jpeg", payload
	}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
