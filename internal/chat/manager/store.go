package manager

import (
	"fmt"
	"strings"
	"time"

	"oh-my-qqbot/internal/database"
	"oh-my-qqbot/internal/message"
)

// This file mirrors the MessageUtils.store_message_to_db path of
// 参考/MaiBot/src/common/utils/utils_message.py（由 chat_manager 体系收口）:
// 存储消息到数据库（无 update 机制），写入前补充 reply_frequency。

// ReplyFrequencyProvider 在消息入库前补充当前会话的生效回复频率
// （对齐 fill_reply_frequency_if_available：runtime 生效频率 → ChatConfigUtils.get_talk_value）。
type ReplyFrequencyProvider func(sessionID string, isGroupChat bool) float64

// StoreMessageToDB mirrors MessageUtils.store_message_to_db:
// 存储消息到数据库，此方法没有 update 机制。
//
// Python 版用线程锁串行化所有写入路径；Go 版由 database.DB 的单写连接承载。
func (m *ChatManager) StoreMessageToDB(sessionMessage *message.SessionMessage) (int64, error) {
	if sessionMessage == nil {
		return 0, fmt.Errorf("消息为空，无法入库")
	}
	m.fillReplyFrequencyIfAvailable(sessionMessage)
	record := messageToDBRecord(sessionMessage)
	id, err := m.db.InsertMessage(record)
	if err != nil {
		return 0, fmt.Errorf("存储消息到数据库失败: %w", err)
	}
	return id, nil
}

// fillReplyFrequencyIfAvailable mirrors MessageUtils.fill_reply_frequency_if_available.
func (m *ChatManager) fillReplyFrequencyIfAvailable(sessionMessage *message.SessionMessage) {
	if sessionMessage.ReplyFreq != nil {
		return
	}
	sessionID := strings.TrimSpace(sessionMessage.SessionID)
	if sessionID == "" {
		return
	}
	if m.replyFrequencyProvider == nil {
		return
	}
	frequency := m.replyFrequencyProvider(sessionID, sessionMessage.MessageInfo.GroupInfo != nil)
	sessionMessage.ReplyFreq = &frequency
}

// messageToDBRecord mirrors SessionMessage.to_db_instance（raw_content 走 JSON 序列化）。
func messageToDBRecord(sessionMessage *message.SessionMessage) *database.Message {
	record := &database.Message{
		MessageID:   sessionMessage.MessageID,
		Timestamp:   sessionMessage.Timestamp,
		Platform:    sessionMessage.Platform,
		IsMentioned: sessionMessage.IsMentioned,
		IsAt:        sessionMessage.IsAt,
		SessionID:   sessionMessage.SessionID,
		IsEmoji:     sessionMessage.IsEmoji,
		IsPicture:   sessionMessage.IsPicture,
		IsCommand:   sessionMessage.IsCommand,
		IsNotify:    sessionMessage.IsNotify,
	}
	userInfo := sessionMessage.MessageInfo.UserInfo
	record.UserID = userInfo.UserID
	record.UserNickname = userInfo.UserNickname
	if userInfo.UserCardname != nil {
		cardname := *userInfo.UserCardname
		record.UserCardname = &cardname
	}
	if groupInfo := sessionMessage.MessageInfo.GroupInfo; groupInfo != nil {
		groupID := groupInfo.GroupID
		groupName := groupInfo.GroupName
		record.GroupID = &groupID
		record.GroupName = &groupName
	}
	if sessionMessage.ReplyTo != nil {
		replyTo := *sessionMessage.ReplyTo
		record.ReplyTo = &replyTo
	}
	if sessionMessage.RawMessage != nil {
		encoded, err := database.EncodeRawContent(sessionMessage.RawMessage)
		if err == nil {
			record.RawContent = encoded
		}
	}
	if sessionMessage.ProcessedPlainText != nil {
		plain := *sessionMessage.ProcessedPlainText
		record.ProcessedPlainText = &plain
	}
	if additional := sessionMessage.MessageInfo.AdditionalConfig; len(additional) > 0 {
		encoded, err := database.EncodeRawContent(additional)
		if err == nil {
			record.AdditionalConfig = &encoded
		}
	}
	if sessionMessage.ReplyFreq != nil {
		frequency := *sessionMessage.ReplyFreq
		record.ReplyFrequency = &frequency
	}
	return record
}

// StoreMessageToDBAt is the timestamp-override form used by tests.
func (m *ChatManager) StoreMessageToDBAt(sessionMessage *message.SessionMessage, at time.Time) (int64, error) {
	if sessionMessage != nil && sessionMessage.Timestamp.IsZero() {
		sessionMessage.Timestamp = at
	}
	return m.StoreMessageToDB(sessionMessage)
}
