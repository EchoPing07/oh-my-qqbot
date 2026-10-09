package context

import (
	"fmt"
	"html"
	"strings"
	"time"

	"oh-my-qqbot/internal/message"
)

// This file mirrors 参考/MaiBot/src/maisaka/context/planner_messages.py:
// the `<message ...>` planner prefix and the helpers converting a
// SessionMessage into a history entry.

// BuildPlannerPrefix mirrors planner_messages.build_planner_prefix.
//
// The attribute order is exactly `msg_id chat_id quote time user group_card
// is_self_message`, values are HTML-escaped with quote=True.
func BuildPlannerPrefix(
	timestamp time.Time,
	userName string,
	groupCard string,
	messageID string,
	chatID string,
	quoteIDs []string,
	includeMessageID bool,
	includeChatID bool,
	isSelfMessage bool,
) string {
	attrs := make([]string, 0, 7)
	if includeMessageID {
		attrs = append(attrs, fmt.Sprintf("msg_id=\"%s\"", html.EscapeString(ToDisplayMessageID(messageID))))
	}
	if includeChatID {
		normalizedChatID := strings.TrimSpace(chatID)
		if normalizedChatID != "" {
			attrs = append(attrs, fmt.Sprintf("chat_id=\"%s\"", html.EscapeString(normalizedChatID)))
		}
	}
	if normalizedQuote := formatQuoteIDs(quoteIDs); normalizedQuote != "" {
		attrs = append(attrs, fmt.Sprintf("quote=\"%s\"", html.EscapeString(normalizedQuote)))
	}
	attrs = append(attrs,
		fmt.Sprintf("time=\"%s\"", html.EscapeString(timestamp.Format("15:04:05"))),
		fmt.Sprintf("user=\"%s\"", html.EscapeString(userName)),
	)
	if normalizedGroupCard := strings.TrimSpace(groupCard); normalizedGroupCard != "" {
		attrs = append(attrs, fmt.Sprintf("group_card=\"%s\"", html.EscapeString(normalizedGroupCard)))
	}
	if isSelfMessage {
		attrs = append(attrs, "is_self_message=\"true\"")
	}
	return "<message " + strings.Join(attrs, " ") + ">\n"
}

func formatQuoteIDs(quoteIDs []string) string {
	normalizedIDs := make([]string, 0, len(quoteIDs))
	seen := map[string]bool{}
	for _, rawQuoteID := range quoteIDs {
		quoteID := ToDisplayMessageID(rawQuoteID)
		if quoteID == "" || seen[quoteID] {
			continue
		}
		seen[quoteID] = true
		normalizedIDs = append(normalizedIDs, quoteID)
	}
	return strings.Join(normalizedIDs, ",")
}

// ExtractQuoteIDsFromMessageSequence mirrors
// planner_messages.extract_quote_ids_from_message_sequence.
func ExtractQuoteIDsFromMessageSequence(sequence *message.MessageSequence) []string {
	if sequence == nil {
		return nil
	}
	quoteIDs := make([]string, 0)
	seen := map[string]bool{}
	for _, component := range sequence.Components {
		replyComponent, ok := component.(*message.ReplyComponent)
		if !ok {
			continue
		}
		if strings.TrimSpace(replyComponent.TargetMessageID) == "" || seen[replyComponent.TargetMessageID] {
			continue
		}
		seen[replyComponent.TargetMessageID] = true
		quoteIDs = append(quoteIDs, replyComponent.TargetMessageID)
	}
	return quoteIDs
}

// PlannerPrefixOptions carries the optional switches of
// build_planner_user_prefix_from_session_message.
type PlannerPrefixOptions struct {
	IncludeMessageID *bool
	IncludeChatID    bool
	IsSelfMessage    bool
}

// BuildPlannerUserPrefixFromSessionMessage mirrors
// planner_messages.build_planner_user_prefix_from_session_message.
func BuildPlannerUserPrefixFromSessionMessage(sessionMessage *message.SessionMessage, options PlannerPrefixOptions) string {
	userInfo := sessionMessage.MessageInfo.UserInfo
	userName := userInfo.UserNickname
	if strings.TrimSpace(userName) == "" {
		userName = userInfo.UserID
	}
	includeMessageID := true
	if options.IncludeMessageID != nil {
		includeMessageID = *options.IncludeMessageID
	}
	includeMessageID = includeMessageID && !sessionMessage.IsNotify && sessionMessage.MessageID != ""
	groupCard := ""
	if userInfo.UserCardname != nil {
		groupCard = *userInfo.UserCardname
	}
	return BuildPlannerPrefix(
		sessionMessage.Timestamp,
		userName,
		groupCard,
		sessionMessage.MessageID,
		sessionMessage.SessionID,
		ExtractQuoteIDsFromMessageSequence(sessionMessage.RawMessage),
		includeMessageID,
		options.IncludeChatID,
		options.IsSelfMessage,
	)
}

// BuildSessionBackedTextMessage mirrors
// planner_messages.build_session_backed_text_message.
func BuildSessionBackedTextMessage(
	speakerName string,
	text string,
	timestamp time.Time,
	sourceKind string,
	groupCard string,
	messageID string,
	chatID string,
	quoteIDs []string,
	includeMessageID bool,
	includeChatID bool,
	isSelfMessage bool,
) *SessionBackedMessage {
	plannerPrefix := BuildPlannerPrefix(
		timestamp,
		speakerName,
		groupCard,
		messageID,
		chatID,
		quoteIDs,
		includeMessageID,
		includeChatID,
		isSelfMessage,
	)
	visibleMessageID := ""
	if includeMessageID {
		visibleMessageID = messageID
	}
	return NewSessionBackedMessage(
		message.NewMessageSequence(message.NewTextComponent(plannerPrefix+text)),
		FormatSpeakerContent(speakerName, text, &timestamp, visibleMessageID),
		timestamp,
		messageID,
		nil,
		sourceKind,
	)
}

// ---------------------------------------------------------------------------
// clear_context.py helpers
// ---------------------------------------------------------------------------

// ClearContextCommand mirrors CLEAR_CONTEXT_COMMAND.
const ClearContextCommand = "/clear"

// ClearContextMarkerKey mirrors CLEAR_CONTEXT_MARKER_KEY.
const ClearContextMarkerKey = "maisaka_context_cleared"

// ClearContextInterceptLevel mirrors CLEAR_CONTEXT_INTERCEPT_LEVEL.
const ClearContextInterceptLevel = 1

// IsClearContextCommand mirrors clear_context.is_clear_context_command.
func IsClearContextCommand(text string) bool {
	return strings.TrimSpace(text) == ClearContextCommand
}

// MarkClearContextCommand mirrors clear_context.mark_clear_context_command.
func MarkClearContextCommand(sessionMessage *message.SessionMessage) {
	sessionMessage.IsCommand = true
	if sessionMessage.MessageInfo.AdditionalConfig == nil {
		sessionMessage.MessageInfo.AdditionalConfig = map[string]any{}
	}
	sessionMessage.MessageInfo.AdditionalConfig[ClearContextMarkerKey] = true
	sessionMessage.MessageInfo.AdditionalConfig["intercept_message_level"] = ClearContextInterceptLevel
}

// IsClearContextMarker mirrors clear_context.is_clear_context_marker.
func IsClearContextMarker(sessionMessage *message.SessionMessage) bool {
	if !sessionMessage.IsCommand || sessionMessage.MessageInfo.AdditionalConfig == nil {
		return false
	}
	value, ok := sessionMessage.MessageInfo.AdditionalConfig[ClearContextMarkerKey]
	if !ok {
		return false
	}
	flag, ok := value.(bool)
	return ok && flag
}

// SelectMessagesAfterLatestClearMarker mirrors
// clear_context.select_messages_after_latest_clear_marker.
func SelectMessagesAfterLatestClearMarker(messages []*message.SessionMessage) []*message.SessionMessage {
	restoreStartIndex := 0
	for index, sessionMessage := range messages {
		if IsClearContextMarker(sessionMessage) {
			restoreStartIndex = index + 1
		}
	}
	selected := make([]*message.SessionMessage, 0, len(messages))
	for _, sessionMessage := range messages[restoreStartIndex:] {
		if sessionMessage.IsCommand {
			continue
		}
		selected = append(selected, sessionMessage)
	}
	return selected
}
