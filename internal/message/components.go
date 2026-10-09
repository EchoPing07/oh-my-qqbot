// Package message implements the component-based chat message model.
//
// Aligned with the following MaiBot sources (behaviour is authoritative there):
//
//   - 参考/MaiBot/src/common/data_models/message_component_data_model.py
//     (MessageSequence / TextComponent / ImageComponent / EmojiComponent /
//     VoiceComponent / FileComponent / AtComponent / ReplyComponent /
//     ForwardComponent / ForwardNodeComponent / DictComponent / ByteComponent)
//   - 参考/MaiBot/src/chat/message_receive/message.py (SessionMessage.process,
//     process_single_component and the per-component text rendering)
//   - 参考/MaiBot/src/common/data_models/mai_message_data_model.py
//     (UserInfo / GroupInfo / MessageInfo / MaiMessage)
//   - 参考/MaiBot/src/common/utils/utils_session.py (calculate_session_id)
//   - 参考/MaiBot/src/platform_io/route_key_factory.py (route component keys)
//
// The Go port replaces MaiBot's msgpack persistence of raw_content with JSON
// (REWRITE_PLAN §6.2) while keeping the exact same dict layout.
package message

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Component type tags, mirroring each Python component's `format_name`.
const (
	ComponentTypeText        = "text"
	ComponentTypeImage       = "image"
	ComponentTypeEmoji       = "emoji"
	ComponentTypeVoice       = "voice"
	ComponentTypeFile        = "file"
	ComponentTypeAt          = "at"
	ComponentTypeReply       = "reply"
	ComponentTypeForward     = "forward"
	ComponentTypeForwardNode = "forward_node"
	ComponentTypeDict        = "dict"
)

// Placeholder texts used when a binary component has no processed content.
// Aligned with MessageSequence._ensure_binary_component_content call sites and
// SessionMessage.process_voice_component.
const (
	EmojiPlaceholder = "[表情包]"
	VoicePlaceholder = "[语音消息]"
	VoiceFailedText  = "[语音消息，转录失败]"
)

// ErrEmptyForwardComponents mirrors the Python assertion
// "ForwardNodeComponent 的 forward_components 不能为空列表".
var ErrEmptyForwardComponents = errors.New("message: forward 节点的内容不能为空")

// Component is one element of a MessageSequence. It mirrors
// BaseMessageComponentModel (format_name / clone).
type Component interface {
	// FormatName returns the component type tag ("text", "image", ...).
	FormatName() string
	// Clone returns a deep copy of the component.
	Clone() Component
}

// ---------------------------------------------------------------------------
// ByteComponent — shared binary payload with sha256 identity
// ---------------------------------------------------------------------------

// ByteComponent mirrors ByteComponent in
// message_component_data_model.py: it keeps the processed text `content`, the
// raw bytes and the sha256 `binary_hash` that identifies the payload.
type ByteComponent struct {
	Content    string `json:"content"`
	BinaryData []byte `json:"-"`
	BinaryHash string `json:"hash"`
}

// NewByteComponent builds a ByteComponent. When binaryData is non-empty the
// hash is recomputed with sha256, exactly like the Python constructor.
func NewByteComponent(content string, binaryData []byte, binaryHash string) ByteComponent {
	if len(binaryData) > 0 {
		sum := sha256.Sum256(binaryData)
		binaryHash = hex.EncodeToString(sum[:])
	}
	return ByteComponent{Content: content, BinaryData: binaryData, BinaryHash: binaryHash}
}

// Clone deep-copies the component including the binary payload.
func (b ByteComponent) Clone() ByteComponent {
	cloned := b
	if len(b.BinaryData) > 0 {
		cloned.BinaryData = append([]byte(nil), b.BinaryData...)
	}
	return cloned
}

// ensureContent mirrors MessageSequence._ensure_binary_component_content:
// stripped content, falling back to a stable placeholder when empty.
func (b ByteComponent) ensureContent(fallback string) string {
	if normalized := strings.TrimSpace(b.Content); normalized != "" {
		return normalized
	}
	return fallback
}

// ---------------------------------------------------------------------------
// TextComponent
// ---------------------------------------------------------------------------

// TextComponent mirrors TextComponent (format_name "text").
type TextComponent struct {
	Text string
}

// NewTextComponent creates a text component.
func NewTextComponent(text string) *TextComponent { return &TextComponent{Text: text} }

// FormatName implements Component.
func (c *TextComponent) FormatName() string { return ComponentTypeText }

// Clone implements Component.
func (c *TextComponent) Clone() Component { cloned := *c; return &cloned }

// MarshalJSON emits {"type":"text","data":text}.
func (c *TextComponent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}{ComponentTypeText, c.Text})
}

// UnmarshalJSON reads {"type":"text","data":text}.
func (c *TextComponent) UnmarshalJSON(data []byte) error {
	var payload struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("message: decode text component: %w", err)
	}
	c.Text = payload.Data
	return nil
}

// ---------------------------------------------------------------------------
// ImageComponent / EmojiComponent / VoiceComponent
// ---------------------------------------------------------------------------

// ImageComponent mirrors ImageComponent (format_name "image").
type ImageComponent struct {
	ByteComponent
}

// NewImageComponent builds an image component and derives the sha256 hash from
// the payload (MessageSequence.image in Python).
func NewImageComponent(binaryData []byte, content string) *ImageComponent {
	return &ImageComponent{ByteComponent: NewByteComponent(content, binaryData, "")}
}

// FormatName implements Component.
func (c *ImageComponent) FormatName() string { return ComponentTypeImage }

// Clone implements Component.
func (c *ImageComponent) Clone() Component {
	cloned := *c
	cloned.ByteComponent = c.ByteComponent.Clone()
	return &cloned
}

// MarshalJSON emits {"type":"image","data":<stripped content>,"hash":hash}
// (Python uses `item.content.strip()` for image components).
func (c *ImageComponent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Data string `json:"data"`
		Hash string `json:"hash"`
	}{ComponentTypeImage, strings.TrimSpace(c.Content), c.BinaryHash})
}

// UnmarshalJSON reads {"type":"image","data":content,"hash":hash}.
func (c *ImageComponent) UnmarshalJSON(data []byte) error {
	var payload struct {
		Data string `json:"data"`
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("message: decode image component: %w", err)
	}
	c.ByteComponent = ByteComponent{Content: payload.Data, BinaryHash: payload.Hash}
	return nil
}

// EmojiComponent mirrors EmojiComponent (format_name "emoji").
type EmojiComponent struct {
	ByteComponent
}

// NewEmojiComponent builds an emoji component.
func NewEmojiComponent(binaryData []byte, content string) *EmojiComponent {
	return &EmojiComponent{ByteComponent: NewByteComponent(content, binaryData, "")}
}

// FormatName implements Component.
func (c *EmojiComponent) FormatName() string { return ComponentTypeEmoji }

// Clone implements Component.
func (c *EmojiComponent) Clone() Component {
	cloned := *c
	cloned.ByteComponent = c.ByteComponent.Clone()
	return &cloned
}

// MarshalJSON emits the emoji placeholder when no description was produced.
func (c *EmojiComponent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Data string `json:"data"`
		Hash string `json:"hash"`
	}{ComponentTypeEmoji, c.ensureContent(EmojiPlaceholder), c.BinaryHash})
}

// UnmarshalJSON reads {"type":"emoji","data":content,"hash":hash}.
func (c *EmojiComponent) UnmarshalJSON(data []byte) error {
	var payload struct {
		Data string `json:"data"`
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("message: decode emoji component: %w", err)
	}
	c.ByteComponent = ByteComponent{Content: payload.Data, BinaryHash: payload.Hash}
	return nil
}

// VoiceComponent mirrors VoiceComponent (format_name "voice").
type VoiceComponent struct {
	ByteComponent
}

// NewVoiceComponent builds a voice component.
func NewVoiceComponent(binaryData []byte, content string) *VoiceComponent {
	return &VoiceComponent{ByteComponent: NewByteComponent(content, binaryData, "")}
}

// FormatName implements Component.
func (c *VoiceComponent) FormatName() string { return ComponentTypeVoice }

// Clone implements Component.
func (c *VoiceComponent) Clone() Component {
	cloned := *c
	cloned.ByteComponent = c.ByteComponent.Clone()
	return &cloned
}

// MarshalJSON emits the voice placeholder when no transcription exists.
func (c *VoiceComponent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Data string `json:"data"`
		Hash string `json:"hash"`
	}{ComponentTypeVoice, c.ensureContent(VoicePlaceholder), c.BinaryHash})
}

// UnmarshalJSON reads {"type":"voice","data":content,"hash":hash}.
func (c *VoiceComponent) UnmarshalJSON(data []byte) error {
	var payload struct {
		Data string `json:"data"`
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("message: decode voice component: %w", err)
	}
	c.ByteComponent = ByteComponent{Content: payload.Data, BinaryHash: payload.Hash}
	return nil
}

// ---------------------------------------------------------------------------
// FileComponent
// ---------------------------------------------------------------------------

// FileComponent mirrors FileComponent (format_name "file").
type FileComponent struct {
	Name       string
	Size       string
	URL        string
	FileID     string
	MIMEType   string
	Base64Data string
}

// FormatName implements Component.
func (c *FileComponent) FormatName() string { return ComponentTypeFile }

// Clone implements Component.
func (c *FileComponent) Clone() Component { cloned := *c; return &cloned }

// ToPayload mirrors FileComponent.to_payload: only non-empty keys are emitted.
func (c *FileComponent) ToPayload() map[string]any {
	payload := map[string]any{}
	if c.Name != "" {
		payload["name"] = c.Name
	}
	if c.Size != "" {
		payload["size"] = c.Size
	}
	if c.URL != "" {
		payload["url"] = c.URL
	}
	if c.FileID != "" {
		payload["file_id"] = c.FileID
	}
	if c.MIMEType != "" {
		payload["mime_type"] = c.MIMEType
	}
	if c.Base64Data != "" {
		payload["base64"] = c.Base64Data
	}
	return payload
}

// FromPayload mirrors FileComponent.from_payload, including its key aliases.
func (c *FileComponent) FromPayload(payload map[string]any) *FileComponent {
	return &FileComponent{
		Name:       firstStringValue(payload, "name", "file", "file_name", "filename"),
		Size:       firstStringValue(payload, "size", "file_size"),
		URL:        firstStringValue(payload, "url", "file_url"),
		FileID:     firstStringValue(payload, "file_id", "id"),
		MIMEType:   firstStringValue(payload, "mime_type", "mimeType"),
		Base64Data: firstStringValue(payload, "base64"),
	}
}

// ToPlainText mirrors FileComponent.to_plain_text.
func (c *FileComponent) ToPlainText() string {
	parts := make([]string, 0, 5)
	if c.Name != "" {
		parts = append(parts, c.Name)
	}
	if c.Size != "" {
		parts = append(parts, "大小: "+c.Size)
	}
	if c.MIMEType != "" {
		parts = append(parts, "类型: "+c.MIMEType)
	}
	if c.URL != "" {
		parts = append(parts, "链接: "+c.URL)
	}
	if c.FileID != "" {
		parts = append(parts, "文件ID: "+c.FileID)
	}
	if len(parts) == 0 {
		return "[文件]"
	}
	return "[文件] " + strings.Join(parts, "，")
}

// MarshalJSON emits {"type":"file","data":{...payload...}}.
func (c *FileComponent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}{ComponentTypeFile, c.ToPayload()})
}

// UnmarshalJSON reads a file payload object (or a legacy plain string name).
func (c *FileComponent) UnmarshalJSON(data []byte) error {
	var payload struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("message: decode file component: %w", err)
	}
	trimmed := strings.TrimSpace(string(payload.Data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var name string
		if err := json.Unmarshal(payload.Data, &name); err != nil {
			return fmt.Errorf("message: decode file component name: %w", err)
		}
		c.Name = name
		return nil
	}
	var object map[string]any
	if err := json.Unmarshal(payload.Data, &object); err != nil {
		return fmt.Errorf("message: decode file component payload: %w", err)
	}
	*c = *c.FromPayload(object)
	return nil
}

// ---------------------------------------------------------------------------
// AtComponent
// ---------------------------------------------------------------------------

// AtComponent mirrors AtComponent (format_name "at"), including the
// `uses_configured_bot_nickname` flag used when the bot's own nickname was
// filled in from config instead of the platform.
type AtComponent struct {
	TargetUserID              string
	TargetUserNickname        *string
	TargetUserCardname        *string
	UsesConfiguredBotNickname bool
}

// NewAtComponent creates an at component for targetUserID.
func NewAtComponent(targetUserID string) *AtComponent {
	return &AtComponent{TargetUserID: targetUserID}
}

// FormatName implements Component.
func (c *AtComponent) FormatName() string { return ComponentTypeAt }

// Clone implements Component.
func (c *AtComponent) Clone() Component {
	cloned := *c
	if c.TargetUserNickname != nil {
		value := *c.TargetUserNickname
		cloned.TargetUserNickname = &value
	}
	if c.TargetUserCardname != nil {
		value := *c.TargetUserCardname
		cloned.TargetUserCardname = &value
	}
	return &cloned
}

// DisplayName mirrors the Python fallback chain used for rendering:
// cardname || nickname || user_id (message.py process_at_component).
func (c *AtComponent) DisplayName() string {
	if c.TargetUserCardname != nil && strings.TrimSpace(*c.TargetUserCardname) != "" {
		return strings.TrimSpace(*c.TargetUserCardname)
	}
	if c.TargetUserNickname != nil && strings.TrimSpace(*c.TargetUserNickname) != "" {
		return strings.TrimSpace(*c.TargetUserNickname)
	}
	return c.TargetUserID
}

// MarshalJSON emits the Python `to_dict` shape (all four keys are always
// present; unknown values are null).
func (c *AtComponent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Data struct {
			TargetUserID              string  `json:"target_user_id"`
			TargetUserNickname        *string `json:"target_user_nickname"`
			TargetUserCardname        *string `json:"target_user_cardname"`
			UsesConfiguredBotNickname bool    `json:"uses_configured_bot_nickname"`
		} `json:"data"`
	}{
		Type: ComponentTypeAt,
		Data: struct {
			TargetUserID              string  `json:"target_user_id"`
			TargetUserNickname        *string `json:"target_user_nickname"`
			TargetUserCardname        *string `json:"target_user_cardname"`
			UsesConfiguredBotNickname bool    `json:"uses_configured_bot_nickname"`
		}{
			TargetUserID:              c.TargetUserID,
			TargetUserNickname:        c.TargetUserNickname,
			TargetUserCardname:        c.TargetUserCardname,
			UsesConfiguredBotNickname: c.UsesConfiguredBotNickname,
		},
	})
}

// UnmarshalJSON reads the at component dict.
func (c *AtComponent) UnmarshalJSON(data []byte) error {
	var payload struct {
		Data struct {
			TargetUserID              string  `json:"target_user_id"`
			TargetUserNickname        *string `json:"target_user_nickname"`
			TargetUserCardname        *string `json:"target_user_cardname"`
			UsesConfiguredBotNickname bool    `json:"uses_configured_bot_nickname"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("message: decode at component: %w", err)
	}
	c.TargetUserID = payload.Data.TargetUserID
	c.TargetUserNickname = payload.Data.TargetUserNickname
	c.TargetUserCardname = payload.Data.TargetUserCardname
	c.UsesConfiguredBotNickname = payload.Data.UsesConfiguredBotNickname
	return nil
}

// ---------------------------------------------------------------------------
// ReplyComponent
// ---------------------------------------------------------------------------

// ReplyComponent mirrors ReplyComponent (format_name "reply").
//
// Note: like the Python implementation, MessageSequence serialization only
// keeps `target_message_id` — the resolved content/sender cache is deliberately
// dropped on the wire.
type ReplyComponent struct {
	TargetMessageID             string
	TargetMessageContent        *string
	TargetMessageSenderID       *string
	TargetMessageSenderNickname *string
	TargetMessageSenderCardname *string
}

// NewReplyComponent creates a reply component for targetMessageID.
func NewReplyComponent(targetMessageID string) *ReplyComponent {
	return &ReplyComponent{TargetMessageID: targetMessageID}
}

// FormatName implements Component.
func (c *ReplyComponent) FormatName() string { return ComponentTypeReply }

// Clone implements Component.
func (c *ReplyComponent) Clone() Component {
	cloned := *c
	cloned.TargetMessageContent = cloneStringPtr(c.TargetMessageContent)
	cloned.TargetMessageSenderID = cloneStringPtr(c.TargetMessageSenderID)
	cloned.TargetMessageSenderNickname = cloneStringPtr(c.TargetMessageSenderNickname)
	cloned.TargetMessageSenderCardname = cloneStringPtr(c.TargetMessageSenderCardname)
	return &cloned
}

// SenderDisplayName mirrors the Python fallback chain
// cardname || nickname || user_id.
func (c *ReplyComponent) SenderDisplayName() string {
	if c.TargetMessageSenderCardname != nil && strings.TrimSpace(*c.TargetMessageSenderCardname) != "" {
		return strings.TrimSpace(*c.TargetMessageSenderCardname)
	}
	if c.TargetMessageSenderNickname != nil && strings.TrimSpace(*c.TargetMessageSenderNickname) != "" {
		return strings.TrimSpace(*c.TargetMessageSenderNickname)
	}
	if c.TargetMessageSenderID != nil {
		return strings.TrimSpace(*c.TargetMessageSenderID)
	}
	return ""
}

// MarshalJSON emits {"type":"reply","data":target_message_id}.
func (c *ReplyComponent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}{ComponentTypeReply, c.TargetMessageID})
}

// UnmarshalJSON reads {"type":"reply","data":target_message_id}.
func (c *ReplyComponent) UnmarshalJSON(data []byte) error {
	var payload struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("message: decode reply component: %w", err)
	}
	c.TargetMessageID = payload.Data
	return nil
}

// ---------------------------------------------------------------------------
// ForwardComponent / ForwardNodeComponent
// ---------------------------------------------------------------------------

// ForwardComponent mirrors ForwardComponent: one node of a merged-forward
// message (format_name "forward", wire tag "forward").
type ForwardComponent struct {
	UserID       *string
	UserNickname string
	UserCardname *string
	MessageID    string
	Content      []Component
}

// FormatName implements Component.
func (c *ForwardComponent) FormatName() string { return ComponentTypeForward }

// Clone implements Component.
func (c *ForwardComponent) Clone() Component {
	cloned := *c
	cloned.UserID = cloneStringPtr(c.UserID)
	cloned.UserCardname = cloneStringPtr(c.UserCardname)
	cloned.Content = CloneComponents(c.Content)
	return &cloned
}

// DisplayName mirrors the fallback chain used by process_forward_component:
// cardname || nickname || user_id || "未知用户".
func (c *ForwardComponent) DisplayName() string {
	if c.UserCardname != nil && strings.TrimSpace(*c.UserCardname) != "" {
		return strings.TrimSpace(*c.UserCardname)
	}
	if strings.TrimSpace(c.UserNickname) != "" {
		return strings.TrimSpace(c.UserNickname)
	}
	if c.UserID != nil && strings.TrimSpace(*c.UserID) != "" {
		return strings.TrimSpace(*c.UserID)
	}
	return "未知用户"
}

// ForwardNodeComponent mirrors ForwardNodeComponent (format_name
// "forward_node"); it serializes to the same "forward" wire tag as the Python
// `_item_2_dict` implementation.
type ForwardNodeComponent struct {
	ForwardComponents []*ForwardComponent
}

// NewForwardNodeComponent builds a forward node component, validating the
// non-empty constraint of the Python constructor.
func NewForwardNodeComponent(nodes []*ForwardComponent) (*ForwardNodeComponent, error) {
	if len(nodes) == 0 {
		return nil, ErrEmptyForwardComponents
	}
	return &ForwardNodeComponent{ForwardComponents: nodes}, nil
}

// FormatName implements Component.
func (c *ForwardNodeComponent) FormatName() string { return ComponentTypeForwardNode }

// Clone implements Component.
func (c *ForwardNodeComponent) Clone() Component {
	nodes := make([]*ForwardComponent, 0, len(c.ForwardComponents))
	for _, node := range c.ForwardComponents {
		cloned, _ := node.Clone().(*ForwardComponent)
		nodes = append(nodes, cloned)
	}
	return &ForwardNodeComponent{ForwardComponents: nodes}
}

// MarshalJSON emits {"type":"forward","data":[node...]}.
func (c *ForwardNodeComponent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string              `json:"type"`
		Data []*ForwardComponent `json:"data"`
	}{ComponentTypeForward, c.ForwardComponents})
}

// MarshalJSON emits one node dict: user_id / user_nickname / user_cardname /
// message_id / content (nested component dicts).
func (c *ForwardComponent) MarshalJSON() ([]byte, error) {
	content := make([]json.RawMessage, 0, len(c.Content))
	for _, component := range c.Content {
		raw, err := json.Marshal(component)
		if err != nil {
			return nil, err
		}
		content = append(content, raw)
	}
	return json.Marshal(struct {
		UserID       *string           `json:"user_id"`
		UserNickname string            `json:"user_nickname"`
		UserCardname *string           `json:"user_cardname"`
		MessageID    string            `json:"message_id"`
		Content      []json.RawMessage `json:"content"`
	}{c.UserID, c.UserNickname, c.UserCardname, c.MessageID, content})
}

// ---------------------------------------------------------------------------
// DictComponent
// ---------------------------------------------------------------------------

// DictComponent mirrors DictComponent: an opaque fallback for component types
// this port does not model yet. Unknown wire types degrade to DictComponent,
// exactly like the Python `_dict_2_item` fallback.
type DictComponent struct {
	Data map[string]any
}

// FormatName implements Component.
func (c *DictComponent) FormatName() string { return ComponentTypeDict }

// Clone implements Component (shallow copy of the payload map, deep enough for
// the untyped values kept by the fallback path).
func (c *DictComponent) Clone() Component {
	cloned := map[string]any{}
	for key, value := range c.Data {
		cloned[key] = value
	}
	return &DictComponent{Data: cloned}
}

// MarshalJSON emits {"type":"dict","data":{...}}.
func (c *DictComponent) MarshalJSON() ([]byte, error) {
	data := c.Data
	if data == nil {
		data = map[string]any{}
	}
	return json.Marshal(struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}{ComponentTypeDict, data})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// CloneComponents deep-copies a component slice.
func CloneComponents(components []Component) []Component {
	if components == nil {
		return nil
	}
	cloned := make([]Component, 0, len(components))
	for _, component := range components {
		if component == nil {
			continue
		}
		cloned = append(cloned, component.Clone())
	}
	return cloned
}

func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// StringPtr is a small helper for building optional string fields.
func StringPtr(value string) *string { return &value }

// Float64Ptr is a small helper for building optional float fields.
func Float64Ptr(value float64) *float64 { return &value }

// firstStringValue mirrors RouteKeyFactory._pick_string style lookups: the
// first key whose value normalizes to a non-empty string wins.
func firstStringValue(mapping map[string]any, keys ...string) string {
	for _, key := range keys {
		raw, ok := mapping[key]
		if !ok || raw == nil {
			continue
		}
		normalized := strings.TrimSpace(toStringValue(raw))
		if normalized != "" {
			return normalized
		}
	}
	return ""
}

// toStringValue mirrors Python's `str(value).strip()` used across the
// parsing helpers (booleans become "True"/"False" there; keep Go's canonical
// spelling so values stay JSON-friendly).
func toStringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case nil:
		return ""
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case float64:
		return trimFloat(typed)
	case float32:
		return trimFloat(float64(typed))
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func trimFloat(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d", int64(value))
	}
	return fmt.Sprintf("%v", value)
}

// componentFromJSON decodes one component dict. Unknown/unofficial types
// degrade to DictComponent (Python logs a warning and does the same).
func componentFromJSON(raw json.RawMessage) (Component, error) {
	var probe struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("message: decode component envelope: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(probe.Type)) {
	case ComponentTypeText:
		component := &TextComponent{}
		return component, component.UnmarshalJSON(raw)
	case ComponentTypeImage:
		component := &ImageComponent{}
		return component, component.UnmarshalJSON(raw)
	case ComponentTypeEmoji:
		component := &EmojiComponent{}
		return component, component.UnmarshalJSON(raw)
	case ComponentTypeVoice:
		component := &VoiceComponent{}
		return component, component.UnmarshalJSON(raw)
	case ComponentTypeFile:
		component := &FileComponent{}
		return component, component.UnmarshalJSON(raw)
	case ComponentTypeAt:
		component := &AtComponent{}
		return component, component.UnmarshalJSON(raw)
	case ComponentTypeReply:
		component := &ReplyComponent{}
		return component, component.UnmarshalJSON(raw)
	case ComponentTypeForward, ComponentTypeForwardNode:
		return forwardNodeFromJSON(probe.Data)
	case ComponentTypeDict:
		return dictComponentFromJSON(probe.Data)
	case "":
		return &DictComponent{Data: map[string]any{}}, nil
	default:
		// Unofficial component type: keep the payload untouched.
		var payload map[string]any
		if len(probe.Data) > 0 {
			_ = json.Unmarshal(probe.Data, &payload)
		}
		if payload == nil {
			payload = map[string]any{}
		}
		return &DictComponent{Data: payload}, nil
	}
}

// dictComponentFromJSON mirrors the Python special case where a dict component
// wrapping a file payload is promoted back to a FileComponent.
func dictComponentFromJSON(data json.RawMessage) (Component, error) {
	if len(data) == 0 {
		return &DictComponent{Data: map[string]any{}}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("message: decode dict component: %w", err)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	if strings.EqualFold(strings.TrimSpace(toStringValue(payload["type"])), ComponentTypeFile) {
		if inner, ok := payload["data"].(map[string]any); ok {
			return (&FileComponent{}).FromPayload(inner), nil
		}
		return (&FileComponent{}).FromPayload(payload), nil
	}
	return &DictComponent{Data: payload}, nil
}

// forwardNodeFromJSON rebuilds a ForwardNodeComponent from the wire array.
func forwardNodeFromJSON(data json.RawMessage) (Component, error) {
	if len(data) == 0 {
		return nil, ErrEmptyForwardComponents
	}
	var rawNodes []struct {
		UserID       *string           `json:"user_id"`
		UserNickname string            `json:"user_nickname"`
		UserCardname *string           `json:"user_cardname"`
		MessageID    string            `json:"message_id"`
		Content      []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &rawNodes); err != nil {
		return nil, fmt.Errorf("message: decode forward component: %w", err)
	}
	nodes := make([]*ForwardComponent, 0, len(rawNodes))
	for _, rawNode := range rawNodes {
		content := make([]Component, 0, len(rawNode.Content))
		for _, rawChild := range rawNode.Content {
			child, err := componentFromJSON(rawChild)
			if err != nil {
				return nil, err
			}
			content = append(content, child)
		}
		nodes = append(nodes, &ForwardComponent{
			UserID:       rawNode.UserID,
			UserNickname: rawNode.UserNickname,
			UserCardname: rawNode.UserCardname,
			MessageID:    rawNode.MessageID,
			Content:      content,
		})
	}
	return NewForwardNodeComponent(nodes)
}
