package message

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	contracts "oh-my-qqbot/internal"
)

// UserInfo mirrors `UserInfo` in
// 参考/MaiBot/src/common/data_models/mai_message_data_model.py.
type UserInfo struct {
	UserID       string  `json:"user_id"`
	UserNickname string  `json:"user_nickname"`
	UserCardname *string `json:"user_cardname"`
}

// NewUserInfo builds a UserInfo; an empty cardname stays nil (Python None).
func NewUserInfo(userID string, nickname string, cardname string) UserInfo {
	info := UserInfo{UserID: userID, UserNickname: nickname}
	if strings.TrimSpace(cardname) != "" {
		info.UserCardname = StringPtr(cardname)
	}
	return info
}

// DisplayName mirrors the `cardname or nickname or user_id` chain used by
// message.py when rendering senders.
func (u UserInfo) DisplayName() string {
	if u.UserCardname != nil && strings.TrimSpace(*u.UserCardname) != "" {
		return strings.TrimSpace(*u.UserCardname)
	}
	if strings.TrimSpace(u.UserNickname) != "" {
		return strings.TrimSpace(u.UserNickname)
	}
	return u.UserID
}

// GroupInfo mirrors `GroupInfo` in mai_message_data_model.py.
type GroupInfo struct {
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name"`
}

// MessageInfo mirrors `MessageInfo` in mai_message_data_model.py; group_info is
// nil for private chats (an empty group name must not demote a group chat).
type MessageInfo struct {
	UserInfo         UserInfo       `json:"user_info"`
	GroupInfo        *GroupInfo     `json:"group_info,omitempty"`
	AdditionalConfig map[string]any `json:"additional_config"`
}

// SessionMessage mirrors the fields of `SessionMessage` (message.py) on top of
// `MaiMessage` (mai_message_data_model.py).
//
// Persistence helpers below replace MaiBot's SQLModel glue with plain struct
// conversions so that internal/database can bind them without an import cycle.
type SessionMessage struct {
	// PlatformMessageID mirrors SessionMessage.platform_message_id: only set
	// when the adapter returned a final platform id.
	PlatformMessageID *string `json:"platform_message_id,omitempty"`

	MessageID string    `json:"message_id"`
	Timestamp time.Time `json:"timestamp"`
	Platform  string    `json:"platform"`

	MessageInfo MessageInfo `json:"message_info"`

	IsMentioned bool `json:"is_mentioned"`
	IsAt        bool `json:"is_at"`
	IsEmoji     bool `json:"is_emoji"`
	IsPicture   bool `json:"is_picture"`
	IsCommand   bool `json:"is_command"`
	IsNotify    bool `json:"is_notify"`

	SessionID string   `json:"session_id"`
	ReplyTo   *string  `json:"reply_to,omitempty"`
	ReplyFreq *float64 `json:"reply_frequency,omitempty"`

	ProcessedPlainText *string          `json:"processed_plain_text,omitempty"`
	RawMessage         *MessageSequence `json:"raw_message,omitempty"`

	// Initialized mirrors MaiMessage.initialized (kept for parity with the
	// Python model; the Go pipeline treats a processed plain text as ready).
	Initialized bool `json:"initialized"`
}

// NewSessionMessage mirrors MaiMessage.__init__(message_id, timestamp, platform).
func NewSessionMessage(messageID string, timestamp time.Time, platform string) *SessionMessage {
	return &SessionMessage{
		MessageID:   messageID,
		Timestamp:   timestamp,
		Platform:    platform,
		RawMessage:  &MessageSequence{},
		MessageInfo: MessageInfo{AdditionalConfig: map[string]any{}},
	}
}

// ChatType returns "group" for group chats, "private" otherwise.
func (m *SessionMessage) ChatType() string {
	if m.MessageInfo.GroupInfo != nil {
		return "group"
	}
	return "private"
}

// UserID is the sender id.
func (m *SessionMessage) UserID() string { return m.MessageInfo.UserInfo.UserID }

// GroupID returns the group id (empty for private chats).
func (m *SessionMessage) GroupID() string {
	if m.MessageInfo.GroupInfo == nil {
		return ""
	}
	return m.MessageInfo.GroupInfo.GroupID
}

// GroupName returns the group name (empty when unknown or private).
func (m *SessionMessage) GroupName() string {
	if m.MessageInfo.GroupInfo == nil {
		return ""
	}
	return m.MessageInfo.GroupInfo.GroupName
}

// AdditionalConfigMap always returns a non-nil metadata map.
func (m *SessionMessage) AdditionalConfigMap() map[string]any {
	if m.MessageInfo.AdditionalConfig == nil {
		m.MessageInfo.AdditionalConfig = map[string]any{}
	}
	return m.MessageInfo.AdditionalConfig
}

// EnsureSessionID recomputes the session id from platform + route metadata,
// mirroring ChatManager.register_message.
func (m *SessionMessage) EnsureSessionID() error {
	sessionID, err := SessionIDForMessage(m)
	if err != nil {
		return err
	}
	m.SessionID = sessionID
	return nil
}

// Process mirrors SessionMessage.process: it drops voice placeholder text
// components and renders `processed_plain_text` by joining the component texts
// with a single space.
func (m *SessionMessage) Process(options TextOptions) string {
	if m.RawMessage == nil {
		m.RawMessage = &MessageSequence{}
	}
	m.RawMessage.RemoveVoicePlaceholderTextComponents()
	text := m.RawMessage.ToText(options)
	m.ProcessedPlainText = StringPtr(text)
	return text
}

// ProcessDefaults mirrors process() with the Python default flags.
func (m *SessionMessage) ProcessDefaults() string { return m.Process(DefaultTextOptions()) }

// DebugString mirrors SessionMessage.to_debug_string.
func (m *SessionMessage) DebugString() string {
	sequence := m.RawMessage
	if sequence == nil {
		sequence = &MessageSequence{}
	}
	plainText := ""
	if m.ProcessedPlainText != nil {
		plainText = *m.ProcessedPlainText
	}
	replyTo := ""
	if m.ReplyTo != nil {
		replyTo = *m.ReplyTo
	}
	return FormatDebugMessage(m.MessageID, m.Platform, m.MessageInfo.UserInfo, m.MessageInfo.GroupInfo, replyTo, plainText, sequence)
}

// ---------------------------------------------------------------------------
// Database record conversion (aligned with MaiMessage.to_db_instance /
// from_db_instance; raw_content uses JSON instead of msgpack per §6.2)
// ---------------------------------------------------------------------------

// MessageRecord mirrors the `mai_messages` table row (REWRITE_PLAN §6.1) as a
// plain struct so internal/database can scan it without this package importing
// the database layer.
type MessageRecord struct {
	MessageID          string
	Timestamp          time.Time
	Platform           string
	UserID             string
	UserNickname       string
	UserCardname       *string
	GroupID            *string
	GroupName          *string
	IsMentioned        bool
	IsAt               bool
	IsEmoji            bool
	IsPicture          bool
	IsCommand          bool
	IsNotify           bool
	SessionID          string
	ReplyTo            *string
	RawContent         string
	ProcessedPlainText *string
	AdditionalConfig   *string
	ReplyFrequency     *float64
}

// ToDatabaseRecord mirrors MaiMessage.to_db_instance: additional_config is
// JSON-encoded (nil when empty) and raw_content carries the JSON-encoded
// MessageSequence.
func (m *SessionMessage) ToDatabaseRecord() (MessageRecord, error) {
	sequence := m.RawMessage
	if sequence == nil {
		sequence = &MessageSequence{}
	}
	rawContent, err := sequence.EncodeJSON()
	if err != nil {
		return MessageRecord{}, err
	}

	var additionalConfig *string
	if len(m.MessageInfo.AdditionalConfig) > 0 {
		encoded, err := json.Marshal(m.MessageInfo.AdditionalConfig)
		if err != nil {
			return MessageRecord{}, fmt.Errorf("message: encode additional_config: %w", err)
		}
		additionalConfig = StringPtr(string(encoded))
	}

	record := MessageRecord{
		MessageID:          m.MessageID,
		Timestamp:          m.Timestamp,
		Platform:           m.Platform,
		UserID:             m.MessageInfo.UserInfo.UserID,
		UserNickname:       m.MessageInfo.UserInfo.UserNickname,
		UserCardname:       m.MessageInfo.UserInfo.UserCardname,
		IsMentioned:        m.IsMentioned,
		IsAt:               m.IsAt,
		IsEmoji:            m.IsEmoji,
		IsPicture:          m.IsPicture,
		IsCommand:          m.IsCommand,
		IsNotify:           m.IsNotify,
		SessionID:          m.SessionID,
		ReplyTo:            m.ReplyTo,
		RawContent:         rawContent,
		ProcessedPlainText: m.ProcessedPlainText,
		AdditionalConfig:   additionalConfig,
		ReplyFrequency:     m.ReplyFreq,
	}
	if m.MessageInfo.GroupInfo != nil {
		record.GroupID = StringPtr(m.MessageInfo.GroupInfo.GroupID)
		record.GroupName = StringPtr(m.MessageInfo.GroupInfo.GroupName)
	}
	return record, nil
}

// FromDatabaseRecord mirrors MaiMessage.from_db_instance, including the rule
// that an empty group name must not demote a group message to a private one.
func FromDatabaseRecord(record MessageRecord) (*SessionMessage, error) {
	message := NewSessionMessage(record.MessageID, record.Timestamp, record.Platform)
	message.MessageInfo = MessageInfo{
		UserInfo: UserInfo{
			UserID:       record.UserID,
			UserNickname: record.UserNickname,
			UserCardname: record.UserCardname,
		},
		AdditionalConfig: map[string]any{},
	}
	if record.GroupID != nil && strings.TrimSpace(*record.GroupID) != "" {
		groupName := ""
		if record.GroupName != nil {
			groupName = *record.GroupName
		}
		message.MessageInfo.GroupInfo = &GroupInfo{GroupID: *record.GroupID, GroupName: groupName}
	}
	if record.AdditionalConfig != nil && strings.TrimSpace(*record.AdditionalConfig) != "" {
		if err := json.Unmarshal([]byte(*record.AdditionalConfig), &message.MessageInfo.AdditionalConfig); err != nil {
			return nil, fmt.Errorf("message: decode additional_config: %w", err)
		}
		if message.MessageInfo.AdditionalConfig == nil {
			message.MessageInfo.AdditionalConfig = map[string]any{}
		}
	}

	message.IsMentioned = record.IsMentioned
	message.IsAt = record.IsAt
	message.IsEmoji = record.IsEmoji
	message.IsPicture = record.IsPicture
	message.IsCommand = record.IsCommand
	message.IsNotify = record.IsNotify
	message.ReplyTo = record.ReplyTo
	message.ReplyFreq = record.ReplyFrequency
	message.SessionID = record.SessionID
	message.ProcessedPlainText = record.ProcessedPlainText
	message.Initialized = true

	sequence, err := DecodeJSON(record.RawContent)
	if err != nil {
		return nil, err
	}
	message.RawMessage = sequence
	return message, nil
}

// ---------------------------------------------------------------------------
// LLM projection
// ---------------------------------------------------------------------------

// BuildTextPrompt mirrors the human-readable prompt block built by
// MessageUtils.build_readable_message: "[HH:MM:SS][msg_id:alias][name]content".
func (m *SessionMessage) BuildTextPrompt(alias string) string {
	name := m.MessageInfo.UserInfo.DisplayName()
	return fmt.Sprintf("[%s][msg_id:%s][%s]%s",
		m.Timestamp.Format("15:04:05"), alias, name, derefString(m.ProcessedPlainText))
}

// ToChatMessage projects the message into the cross-module contract type
// internal.ChatMessage (user role). Images are passed as data URLs exactly like
// MaiBot does when building multimodal context items.
func (m *SessionMessage) ToChatMessage() contracts.ChatMessage {
	content := ""
	if m.ProcessedPlainText != nil {
		content = *m.ProcessedPlainText
	}
	message := contracts.ChatMessage{
		Role:    contracts.RoleUser,
		Content: content,
		Name:    m.MessageInfo.UserInfo.DisplayName(),
	}
	if m.RawMessage != nil {
		for _, component := range m.RawMessage.Components {
			image, ok := component.(*ImageComponent)
			if !ok || len(image.BinaryData) == 0 {
				continue
			}
			message.Images = append(message.Images, contracts.ImagePart{
				Base64: fmt.Sprintf("data:%s;base64,%s", detectImageMIME(image.BinaryData), encodeBase64(image.BinaryData)),
			})
		}
	}
	return message
}

// encodeBase64 mirrors Python's base64.b64encode(...).decode() used when a
// component is projected into a provider request.
func encodeBase64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

func detectImageMIME(data []byte) string {
	if len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n" {
		return "image/png"
	}
	if len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "image/jpeg"
	}
	if len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a") {
		return "image/gif"
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	return "image/jpeg"
}
