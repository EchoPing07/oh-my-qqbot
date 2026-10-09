// Package context implements the Maisaka internal LLM context abstraction:
// the four LLMContextMessage kinds, history trimming/tool-call alignment and
// the visible-text / message-id-alias adapters.
//
// Aligned with the following MaiBot sources (behaviour is authoritative there):
//
//   - 参考/MaiBot/src/maisaka/context/messages.py      → messages.go
//   - 参考/MaiBot/src/maisaka/context/history.py       → history.go
//   - 参考/MaiBot/src/maisaka/context/message_adapter.py → this file
//   - 参考/MaiBot/src/maisaka/context/message_id_alias.py → this file
//   - 参考/MaiBot/src/maisaka/context/planner_messages.py → planner_messages.go
//   - 参考/MaiBot/src/maisaka/context/post_processor.py  → post_processor.go
//   - 参考/MaiBot/src/maisaka/memory/mid_term.py (message part) → midterm.go
//   - 参考/MaiBot/src/maisaka/chat_loop_service.py
//     (select_llm_context_messages)                     → selection.go
package context

import (
	"crypto/sha1"
	"encoding/base32"
	"regexp"
	"strings"
	"time"

	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// message_id_alias.py — model-visible short aliases for long message ids
// ---------------------------------------------------------------------------

const (
	// MaxPlainMessageIDLength mirrors MAX_PLAIN_MESSAGE_ID_LENGTH: ids not
	// longer than this are shown verbatim (and are never aliased again).
	MaxPlainMessageIDLength = 12
	// AliasPrefix mirrors ALIAS_PREFIX.
	AliasPrefix = "m"
	// AliasHashLength mirrors ALIAS_HASH_LENGTH.
	AliasHashLength = 6
)

// ToDisplayMessageID mirrors message_id_alias.to_display_message_id: long ids
// (e.g. QQ official `ROBOT1.0_...`) are shortened to a stable
// `m` + 6 lowercase base32 chars of the sha1 digest; short ids pass through.
func ToDisplayMessageID(messageID string) string {
	normalized := strings.TrimSpace(messageID)
	if len(normalized) <= MaxPlainMessageIDLength {
		return normalized
	}
	sum := sha1.Sum([]byte(normalized))
	encoded := strings.ToLower(base32.StdEncoding.EncodeToString(sum[:]))
	if len(encoded) > AliasHashLength {
		encoded = encoded[:AliasHashLength]
	}
	return AliasPrefix + encoded
}

// BuildAliasMap mirrors message_id_alias.build_alias_map: 别名 -> 原始 ID，冲突时
// 先出现的优先（调用方按新到旧传入）。
func BuildAliasMap(messageIDs []string) map[string]string {
	aliasMap := map[string]string{}
	for _, rawMessageID := range messageIDs {
		messageID := strings.TrimSpace(rawMessageID)
		alias := ToDisplayMessageID(messageID)
		if alias != messageID {
			if _, exists := aliasMap[alias]; !exists {
				aliasMap[alias] = messageID
			}
		}
	}
	return aliasMap
}

// ExpandMessageIDAliases mirrors message_id_alias.expand_message_id_aliases:
// recursively replace tool arguments equal to an alias with the original id.
func ExpandMessageIDAliases(value any, aliasMap map[string]string) any {
	if len(aliasMap) == 0 {
		return value
	}
	switch typed := value.(type) {
	case string:
		if expanded, ok := aliasMap[strings.TrimSpace(typed)]; ok {
			return expanded
		}
		return typed
	case map[string]any:
		expanded := make(map[string]any, len(typed))
		for key, item := range typed {
			expanded[key] = ExpandMessageIDAliases(item, aliasMap)
		}
		return expanded
	case []any:
		expanded := make([]any, 0, len(typed))
		for _, item := range typed {
			expanded = append(expanded, ExpandMessageIDAliases(item, aliasMap))
		}
		return expanded
	default:
		return value
	}
}

// ---------------------------------------------------------------------------
// message_adapter.py — visible text rendering
// ---------------------------------------------------------------------------

// SpeakerPrefixPattern mirrors SPEAKER_PREFIX_PATTERN
// `^(?:([0-9]{2}:[0-9]{2}:[0-9]{2}))?(?:\[msg_id:([^\]]+)\])?\[([^\]]+)\](.*)$`
// with re.DOTALL.
var SpeakerPrefixPattern = regexp.MustCompile(
	`(?s)^(?:(?P<timestamp>\d{2}:\d{2}:\d{2}))?(?:\[msg_id:(?P<message_id>[^\]]+)\])?\[(?P<speaker>[^\]]+)\](?P<content>.*)$`)

// FormatSpeakerContent mirrors message_adapter.format_speaker_content:
// `{HH:MM:SS}[msg_id:别名][名字]{content}`.
func FormatSpeakerContent(speakerName string, content string, timestamp *time.Time, messageID string) string {
	timePrefix := ""
	if timestamp != nil {
		timePrefix = timestamp.Format("15:04:05")
	}
	messageIDPrefix := ""
	if strings.TrimSpace(messageID) != "" {
		messageIDPrefix = "[msg_id:" + ToDisplayMessageID(messageID) + "]"
	}
	return timePrefix + messageIDPrefix + "[" + speakerName + "]" + content
}

// ParseSpeakerContent mirrors message_adapter.parse_speaker_content; ok is
// false when the text does not carry the speaker prefix (Python returns None).
func ParseSpeakerContent(content string) (speaker string, visibleText string, ok bool) {
	match := SpeakerPrefixPattern.FindStringSubmatch(content)
	if match == nil {
		return "", content, false
	}
	index := SpeakerPrefixPattern.SubexpIndex("speaker")
	return match[index], match[SpeakerPrefixPattern.SubexpIndex("content")], true
}

// CloneMessageSequence mirrors message_adapter.clone_message_sequence.
func CloneMessageSequence(sequence *message.MessageSequence) *message.MessageSequence {
	if sequence == nil {
		return nil
	}
	return sequence.Clone()
}

// RenderAtComponentText mirrors the shared `cardname || nickname || user_id`
// @-rendering used by message_adapter/_render_at_component_text.
func RenderAtComponentText(component *message.AtComponent) string {
	return strings.TrimSpace("@" + component.DisplayName())
}

func renderVoiceComponentText(component *message.VoiceComponent) string {
	if normalized := strings.TrimSpace(component.Content); normalized != "" {
		return normalized
	}
	return "[语音消息]"
}

// BuildVisibleTextFromSequence mirrors
// message_adapter.build_visible_text_from_sequence: it renders the text,
// emoji/image/voice/at/reply components of a sequence into the visible text
// shown to the model (reply targets become `[引用消息]id` and prefix the next
// visible part with `[发言内容]`).
func BuildVisibleTextFromSequence(sequence *message.MessageSequence) string {
	if sequence == nil {
		return ""
	}
	parts := make([]string, 0, len(sequence.Components))
	pendingReplyBodyPrefix := false

	appendVisiblePart := func(text string) {
		if text == "" {
			return
		}
		if pendingReplyBodyPrefix {
			parts = append(parts, "\n[发言内容]"+text)
			pendingReplyBodyPrefix = false
			return
		}
		parts = append(parts, text)
	}

	for _, component := range sequence.Components {
		switch typed := component.(type) {
		case *message.TextComponent:
			match := SpeakerPrefixPattern.FindStringSubmatch(typed.Text)
			if match == nil {
				appendVisiblePart(typed.Text)
				continue
			}
			var normalized strings.Builder
			if timestamp := match[SpeakerPrefixPattern.SubexpIndex("timestamp")]; timestamp != "" {
				normalized.WriteString(timestamp)
			}
			if messageID := match[SpeakerPrefixPattern.SubexpIndex("message_id")]; messageID != "" {
				normalized.WriteString("[msg_id:" + ToDisplayMessageID(messageID) + "]")
			}
			normalized.WriteString("[" + match[SpeakerPrefixPattern.SubexpIndex("speaker")] + "]")
			normalized.WriteString(match[SpeakerPrefixPattern.SubexpIndex("content")])
			appendVisiblePart(normalized.String())
		case *message.EmojiComponent:
			appendVisiblePart(componentTextOr(typed.Content, message.EmojiPlaceholder))
		case *message.ImageComponent:
			appendVisiblePart(componentTextOr(typed.Content, "[图片，识别中.....]"))
		case *message.VoiceComponent:
			appendVisiblePart(renderVoiceComponentText(typed))
		case *message.AtComponent:
			appendVisiblePart(RenderAtComponentText(typed))
		case *message.ReplyComponent:
			targetMessageID := strings.TrimSpace(typed.TargetMessageID)
			if targetMessageID != "" {
				parts = append(parts, "[引用消息]"+ToDisplayMessageID(targetMessageID))
				pendingReplyBodyPrefix = true
			}
		}
	}
	return strings.Join(parts, "")
}

func componentTextOr(content string, fallback string) string {
	if normalized := strings.TrimSpace(content); normalized != "" {
		return normalized
	}
	return fallback
}

// ---------------------------------------------------------------------------
// messages.py — content-item helpers shared with the context messages
// ---------------------------------------------------------------------------

// MEDIA_PLACEHOLDER / rendering helpers used by messages.py's
// `_append_image_component` / `_append_emoji_component` / `_append_at_component`.
const (
	EmojiVisualPlaceholder = "[消息类型]表情包"
	ImageVisualPlaceholder = "[图片，识别中.....]"
	EmojiFallbackText      = "[表情包]"
	VoiceFallbackText      = "[语音消息]"
)

// GuessImageFormat mirrors `_guess_image_format` (PIL sniffing replaced with a
// magic-byte check). Empty when the payload is not a supported image.
func GuessImageFormat(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "jpeg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "webp"
	case len(data) >= 2 && data[0] == 'B' && data[1] == 'M':
		return "bmp"
	default:
		return ""
	}
}
