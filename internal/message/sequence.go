package message

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MessageSequence mirrors MessageSequence in
// 参考/MaiBot/src/common/data_models/message_component_data_model.py, plus the
// persistence helpers of MessageUtils in
// 参考/MaiBot/src/common/utils/utils_message.py.
//
// MaiBot stores `raw_content` as msgpack; REWRITE_PLAN §6.2 replaces that with
// JSON while keeping the exact same dict layout, so the two implementations stay
// byte-compatible in structure (and testable against the Python fixtures).
type MessageSequence struct {
	Components []Component
}

// NewMessageSequence creates a sequence from an explicit component list.
func NewMessageSequence(components ...Component) *MessageSequence {
	return &MessageSequence{Components: components}
}

// Clone deep-copies the whole sequence.
func (s *MessageSequence) Clone() *MessageSequence {
	if s == nil {
		return nil
	}
	return &MessageSequence{Components: CloneComponents(s.Components)}
}

// Len returns the number of components.
func (s *MessageSequence) Len() int {
	if s == nil {
		return 0
	}
	return len(s.Components)
}

// ---------------------------------------------------------------------------
// Chainable builders (mirroring the Python `text()/image()/at()` helpers)
// ---------------------------------------------------------------------------

// Text appends a text component.
func (s *MessageSequence) Text(text string) *MessageSequence {
	s.Components = append(s.Components, NewTextComponent(text))
	return s
}

// Image appends an image component, deriving the sha256 hash from the bytes.
func (s *MessageSequence) Image(binaryData []byte, content string) *MessageSequence {
	s.Components = append(s.Components, NewImageComponent(binaryData, content))
	return s
}

// Emoji appends an emoji component.
func (s *MessageSequence) Emoji(binaryData []byte, content string) *MessageSequence {
	s.Components = append(s.Components, NewEmojiComponent(binaryData, content))
	return s
}

// Voice appends a voice component.
func (s *MessageSequence) Voice(binaryData []byte, content string) *MessageSequence {
	s.Components = append(s.Components, NewVoiceComponent(binaryData, content))
	return s
}

// File appends a file component.
func (s *MessageSequence) File(file *FileComponent) *MessageSequence {
	if file != nil {
		s.Components = append(s.Components, file)
	}
	return s
}

// At appends an at component targeting targetUserID.
func (s *MessageSequence) At(targetUserID string) *MessageSequence {
	s.Components = append(s.Components, NewAtComponent(targetUserID))
	return s
}

// Reply appends a reply component targeting targetMessageID.
func (s *MessageSequence) Reply(targetMessageID string) *MessageSequence {
	s.Components = append(s.Components, NewReplyComponent(targetMessageID))
	return s
}

// Append appends an already built component (skipping nil).
func (s *MessageSequence) Append(components ...Component) *MessageSequence {
	for _, component := range components {
		if component != nil {
			s.Components = append(s.Components, component)
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// Serialization
// ---------------------------------------------------------------------------

// ToDict mirrors MessageSequence.to_dict: a list of component dictionaries.
func (s *MessageSequence) ToDict() ([]map[string]any, error) {
	if s == nil {
		return []map[string]any{}, nil
	}
	out := make([]map[string]any, 0, len(s.Components))
	for _, component := range s.Components {
		if component == nil {
			continue
		}
		raw, err := json.Marshal(component)
		if err != nil {
			return nil, fmt.Errorf("message: encode component %s: %w", component.FormatName(), err)
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("message: normalize component %s: %w", component.FormatName(), err)
		}
		out = append(out, item)
	}
	return out, nil
}

// FromDict mirrors MessageSequence.from_dict: rebuilds components from the
// dictionary list, degrading unofficial types to DictComponent.
func FromDict(items []map[string]any) (*MessageSequence, error) {
	sequence := &MessageSequence{Components: make([]Component, 0, len(items))}
	for index, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("message: encode component #%d: %w", index, err)
		}
		component, err := componentFromJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("message: decode component #%d: %w", index, err)
		}
		sequence.Components = append(sequence.Components, component)
	}
	return sequence, nil
}

// MarshalJSON emits the JSON array form used for the `raw_content` column
// (replacing MaiBot's msgpack payload).
func (s *MessageSequence) MarshalJSON() ([]byte, error) {
	items, err := s.ToDict()
	if err != nil {
		return nil, err
	}
	return json.Marshal(items)
}

// UnmarshalJSON accepts either a bare array (native form) or a legacy object
// wrapper such as {"components":[...]} / {"raw_content":[...]}.
func (s *MessageSequence) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		s.Components = nil
		return nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal(data, &wrapper); err != nil {
			return fmt.Errorf("message: decode sequence wrapper: %w", err)
		}
		for _, key := range []string{"components", "raw_content", "data"} {
			if inner, ok := wrapper[key]; ok {
				return s.UnmarshalJSON(inner)
			}
		}
		return fmt.Errorf("message: sequence wrapper has no components list")
	}
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("message: decode sequence list: %w", err)
	}
	sequence, err := FromDict(items)
	if err != nil {
		return err
	}
	s.Components = sequence.Components
	return nil
}

// EncodeJSON serializes the sequence for the `raw_content` column.
// Aligned with MessageUtils.from_MaiSeq_to_db_record_msg (msgpack → JSON).
func (s *MessageSequence) EncodeJSON() (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("message: encode sequence: %w", err)
	}
	return string(raw), nil
}

// DecodeJSON parses the `raw_content` column back into a sequence.
// Aligned with MessageUtils.from_db_record_msg_to_MaiSeq.
func DecodeJSON(rawContent string) (*MessageSequence, error) {
	trimmed := strings.TrimSpace(rawContent)
	if trimmed == "" {
		return &MessageSequence{}, nil
	}
	sequence := &MessageSequence{}
	if err := json.Unmarshal([]byte(trimmed), sequence); err != nil {
		return nil, fmt.Errorf("message: decode raw_content: %w", err)
	}
	return sequence, nil
}

// ---------------------------------------------------------------------------
// Plain-text rendering
// ---------------------------------------------------------------------------

// TextOptions controls ToText rendering. The heavy async paths (image/emoji
// description generation, voice transcription, DB lookups for replies) are
// injected as optional renderers so this package stays free of I/O
// dependencies; when a renderer is nil the Python fallback text is used.
type TextOptions struct {
	// EnableHeavyMediaAnalysis mirrors `enable_heavy_media_analysis`: when
	// false the renderer must not block on description generation.
	EnableHeavyMediaAnalysis bool
	// EnableVoiceTranscription mirrors `enable_voice_transcription`.
	EnableVoiceTranscription bool
	// RenderImage returns the text for an image component (nil → empty string,
	// matching the "still pending" behaviour of process_image_component).
	RenderImage func(*ImageComponent) string
	// RenderEmoji returns the text for an emoji component (nil → EmojiPlaceholder).
	RenderEmoji func(*EmojiComponent) string
	// RenderVoice returns the text for a voice component (nil → VoiceFailedText
	// when transcription is enabled, VoicePlaceholder otherwise).
	RenderVoice func(*VoiceComponent) string
	// LookupReply resolves a quoted message id into its content and sender.
	// Returning ok=false yields "[回复了一条消息，但原消息已无法访问]".
	LookupReply func(targetMessageID string) (content string, senderName string, ok bool)
}

// DefaultTextOptions mirrors the Python defaults
// (enable_heavy_media_analysis=True, enable_voice_transcription=True).
func DefaultTextOptions() TextOptions {
	return TextOptions{EnableHeavyMediaAnalysis: true, EnableVoiceTranscription: true}
}

// ToText mirrors SessionMessage.process: joins the rendered components with a
// single space, skipping errors the way Python logs and drops them.
func (s *MessageSequence) ToText(options TextOptions) string {
	if s == nil {
		return ""
	}
	rendered := make([]string, 0, len(s.Components))
	for _, component := range s.Components {
		if component == nil {
			continue
		}
		rendered = append(rendered, renderComponent(component, options))
	}
	return strings.Join(rendered, " ")
}

// ToPlainText renders with default options and no I/O hooks.
func (s *MessageSequence) ToPlainText() string { return s.ToText(DefaultTextOptions()) }

// RenderComponent mirrors SessionMessage.process_single_component for one
// component (public so callers can render components they pull out of a
// sequence themselves).
func RenderComponent(component Component, options TextOptions) string {
	return renderComponent(component, options)
}

func renderComponent(component Component, options TextOptions) string {
	switch typed := component.(type) {
	case *TextComponent:
		return typed.Text
	case *ImageComponent:
		if normalized := strings.TrimSpace(typed.Content); normalized != "" {
			typed.Content = normalized
			return normalized
		}
		if options.RenderImage != nil {
			rendered := options.RenderImage(typed)
			typed.Content = rendered
			return rendered
		}
		return ""
	case *EmojiComponent:
		if normalized := strings.TrimSpace(typed.Content); normalized != "" {
			typed.Content = normalized
			return normalized
		}
		if options.RenderEmoji != nil {
			rendered := options.RenderEmoji(typed)
			typed.Content = rendered
			return rendered
		}
		return EmojiPlaceholder
	case *VoiceComponent:
		if normalized := strings.TrimSpace(typed.Content); normalized != "" {
			typed.Content = normalized
			return normalized
		}
		if !options.EnableVoiceTranscription {
			typed.Content = VoicePlaceholder
			return typed.Content
		}
		if options.RenderVoice != nil {
			rendered := options.RenderVoice(typed)
			typed.Content = rendered
			return rendered
		}
		return VoiceFailedText
	case *FileComponent:
		return typed.ToPlainText()
	case *AtComponent:
		return "@" + typed.DisplayName()
	case *ReplyComponent:
		return renderReply(typed, options)
	case *ForwardNodeComponent:
		return renderForward(typed, options, 0)
	case *DictComponent:
		return renderDict(typed)
	default:
		return "[" + component.FormatName() + "]"
	}
}

func renderReply(component *ReplyComponent, options TextOptions) string {
	if component.TargetMessageContent != nil && strings.TrimSpace(*component.TargetMessageContent) != "" {
		return strings.TrimSpace(*component.TargetMessageContent)
	}
	if options.LookupReply != nil {
		content, senderName, ok := options.LookupReply(component.TargetMessageID)
		if ok {
			component.TargetMessageContent = StringPtr(content)
			if senderName != "" {
				component.TargetMessageSenderNickname = StringPtr(senderName)
			}
			name := senderName
			if name == "" {
				name = component.SenderDisplayName()
			}
			return fmt.Sprintf("[回复了%s的消息: %s]", name, content)
		}
	}
	if component.TargetMessageContent != nil {
		name := component.SenderDisplayName()
		return fmt.Sprintf("[回复了%s的消息: %s]", name, *component.TargetMessageContent)
	}
	return "[回复了一条消息，但原消息已无法访问]"
}

// renderForward mirrors process_forward_component, including the
// `-`*depth*2 indentation and the 【合并转发消息: \n...\n】 wrapper.
func renderForward(component *ForwardNodeComponent, options TextOptions, depth int) string {
	lines := make([]string, 0, len(component.ForwardComponents))
	for _, node := range component.ForwardComponents {
		if node == nil {
			continue
		}
		texts := make([]string, 0, len(node.Content))
		for _, child := range node.Content {
			if child == nil {
				continue
			}
			texts = append(texts, renderComponent(child, options))
		}
		indent := strings.Repeat("-", depth*2)
		lines = append(lines, fmt.Sprintf("%s 【%s】: %s", indent, node.DisplayName(), strings.Join(texts, " ")))
	}
	return "【合并转发消息: \n" + strings.Join(lines, "\n") + "\n】"
}

// renderDict mirrors SessionMessage.process_dict_component.
func renderDict(component *DictComponent) string {
	rawType := strings.ToLower(strings.TrimSpace(toStringValue(component.Data["type"])))
	if rawType == "" {
		return "[复杂消息]"
	}
	rawPayload := component.Data["data"]
	if rawType == "file" {
		if payload, ok := rawPayload.(map[string]any); ok {
			return buildFileDictText(payload)
		}
		if payload, ok := component.Data["data"].(map[string]any); ok {
			return buildFileDictText(payload)
		}
	}
	if rawType == "json" {
		if payload, ok := rawPayload.(map[string]any); ok {
			if text := strings.TrimSpace(toStringValue(payload["text"])); text != "" {
				return text
			}
			if prompt := strings.TrimSpace(toStringValue(payload["prompt"])); prompt != "" {
				return prompt
			}
		}
		return "[json]"
	}
	return "[" + rawType + "]"
}

// buildFileDictText mirrors SessionMessage._build_file_component_text.
func buildFileDictText(payload map[string]any) string {
	name := firstStringValue(payload, "name", "file", "file_name", "filename")
	size := firstStringValue(payload, "size", "file_size")
	url := firstStringValue(payload, "url", "file_url")
	parts := make([]string, 0, 2)
	if name != "" {
		parts = append(parts, name)
	}
	if size != "" {
		parts = append(parts, "大小: "+size)
	}
	text := "[文件]"
	if len(parts) > 0 {
		text = "[文件] " + strings.Join(parts, "，")
	}
	if url != "" {
		text += "，链接: " + url
	}
	return text
}

// RemoveVoicePlaceholderTextComponents mirrors
// SessionMessage._remove_voice_placeholder_text_components: whenever a voice
// component is present, adapters' "[voice]" text placeholders are dropped.
func (s *MessageSequence) RemoveVoicePlaceholderTextComponents() {
	if s == nil {
		return
	}
	hasVoice := false
	for _, component := range s.Components {
		if _, ok := component.(*VoiceComponent); ok {
			hasVoice = true
			break
		}
	}
	if !hasVoice {
		return
	}
	filtered := make([]Component, 0, len(s.Components))
	for _, component := range s.Components {
		if text, ok := component.(*TextComponent); ok && strings.EqualFold(strings.TrimSpace(text.Text), "[voice]") {
			continue
		}
		filtered = append(filtered, component)
	}
	s.Components = filtered
}

// ---------------------------------------------------------------------------
// Message id assignment
// ---------------------------------------------------------------------------

// AssignMessageID mirrors chat/utils/utils.py assign_message_ids for a single
// message: `m{index}{random suffix}` where the suffix uses 1..9 for lists of at
// most 100 messages and 10..99 above that.
func AssignMessageID(index int, total int, randomSuffix int) string {
	low, high := 1, 9
	if total > 100 {
		low, high = 10, 99
	}
	span := high - low + 1
	suffix := low + ((randomSuffix%span)+span)%span
	return fmt.Sprintf("m%d%d", index+1, suffix)
}

// AssignMessageIDs mirrors assign_message_ids for a whole batch using the
// provided pseudo random source (deterministic in tests).
func AssignMessageIDs(total int, randomInt func(low int, high int) int) []string {
	low, high := 1, 9
	if total > 100 {
		low, high = 10, 99
	}
	used := make(map[string]struct{}, total)
	ids := make([]string, 0, total)
	for index := 0; index < total; index++ {
		for {
			suffix := low
			if randomInt != nil {
				suffix = randomInt(low, high)
			}
			candidate := fmt.Sprintf("m%d%d", index+1, suffix)
			if _, exists := used[candidate]; !exists {
				used[candidate] = struct{}{}
				ids = append(ids, candidate)
				break
			}
		}
	}
	return ids
}

// FormatDebugMessage mirrors SessionMessage.to_debug_string for logging.
func FormatDebugMessage(messageID string, platform string, user UserInfo, group *GroupInfo, replyTo string, plainText string, sequence *MessageSequence) string {
	chatType := "private"
	groupID := ""
	groupName := ""
	if group != nil {
		chatType = "group"
		groupID = group.GroupID
		groupName = group.GroupName
	}
	summaries := make([]string, 0)
	if sequence != nil {
		for _, component := range sequence.Components {
			summaries = append(summaries, summarizeComponent(component))
		}
	}
	rawComponents := "empty"
	if len(summaries) > 0 {
		rawComponents = strings.Join(summaries, ", ")
	}
	return fmt.Sprintf(
		"SessionMessage(message_id='%s', platform='%s', chat_type='%s', group_id='%s', group_name='%s', "+
			"user_id='%s', user_nickname='%s', user_cardname='%s', reply_to='%s', processed_plain_text=%s, raw_components=[%s])",
		messageID, platform, chatType, groupID, groupName,
		user.UserID, user.UserNickname, derefString(user.UserCardname), replyTo,
		truncateText(plainText, 120), rawComponents,
	)
}

func summarizeComponent(component Component) string {
	switch typed := component.(type) {
	case *TextComponent:
		return fmt.Sprintf("Text(text=%s)", truncateText(typed.Text, 80))
	case *ImageComponent:
		return fmt.Sprintf("Image(content=%s)", truncateText(typed.Content, 60))
	case *EmojiComponent:
		return fmt.Sprintf("Emoji(content=%s)", truncateText(typed.Content, 60))
	case *AtComponent:
		return fmt.Sprintf("At(target='%s')", typed.DisplayName())
	case *VoiceComponent:
		return fmt.Sprintf("Voice(content=%s)", truncateText(typed.Content, 60))
	case *FileComponent:
		return fmt.Sprintf("File(name='%s', size='%s')", typed.Name, typed.Size)
	case *ReplyComponent:
		return fmt.Sprintf("Reply(target_message_id='%s', target_sender='%s', target_content=%s)",
			typed.TargetMessageID, typed.SenderDisplayName(), truncateText(derefString(typed.TargetMessageContent), 80))
	case *ForwardNodeComponent:
		return fmt.Sprintf("ForwardNode(count=%d)", len(typed.ForwardComponents))
	default:
		return fmt.Sprintf("%T", component)
	}
}

// truncateText mirrors SessionMessage._truncate_text (escaped CR/LF and repr
// style single quotes).
func truncateText(text string, maxLength int) string {
	normalized := strings.ReplaceAll(text, "\r", "\\r")
	normalized = strings.ReplaceAll(normalized, "\n", "\\n")
	if len([]rune(normalized)) <= maxLength {
		return "'" + normalized + "'"
	}
	return "'" + string([]rune(normalized)[:maxLength]) + "...'"
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// Now is the injectable time source used by the message package (tests pin it).
var Now = time.Now
