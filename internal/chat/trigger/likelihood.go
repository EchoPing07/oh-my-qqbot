// Package trigger implements the Maisaka message-trigger layer: the reply
// likelihood regression, the dynamic reply gate, the frequency threshold gate,
// the idle backoff controller and the per-session turn scheduler.
//
// Aligned with 参考/MaiBot/src/maisaka/turn_trigger/ (behaviour and constants are
// authoritative there):
//
//   - reply_likelihood.py → likelihood.go
//   - dynamic_gate.py     → dynamic_gate.go
//   - gates.py            → gates.go
//   - idle_backoff.py     → idle_backoff.go
//   - scheduler.py        → scheduler.go
//
// Every fitted weight, window and offline threshold table below is copied
// verbatim from the Python source; tests pin the exact numbers.
package trigger

import (
	"math"
	"strings"
)

// Reply-likelihood constants, copied verbatim from reply_likelihood.py.
const (
	// MaxPendingCount mirrors MAX_PENDING_COUNT: the pending count cap applied
	// before the regression, matching the truncation used when fitting.
	MaxPendingCount = 20
	// DefaultSecondsSinceBotMessage mirrors
	// DEFAULT_SECONDS_SINCE_BOT_MESSAGE: the gap assumed when the context holds
	// no bot message.
	DefaultSecondsSinceBotMessage = 3600.0
)

// MediaPlaceholderPrefixes mirrors MEDIA_PLACEHOLDER_PREFIXES: messages that
// only carry media placeholders and no text.
var MediaPlaceholderPrefixes = []string{
	"[表情包",
	"[图片",
	"[消息类型]",
	"[CQ:image",
	"[语音",
	"[文件]",
	"[卡片",
}

// Fitted regression weights (logit = intercept + Σ weight × feature), copied
// verbatim from `_INTERCEPT` / `_WEIGHT_*` in reply_likelihood.py.
const (
	Intercept                = -1.2005
	WeightMentionBot         = 1.5852
	WeightAtOther            = -1.1157
	WeightQuestionMark       = 0.4807
	WeightPlaceholderOnly    = -1.0281
	WeightRecentSelfRatio    = 2.6525
	WeightRecentMessageCount = -0.0367
	WeightLogSecondsSinceBot = 0.0571
	WeightPendingCount       = 0.1012
)

// ReplyLikelihoodInput mirrors ReplyLikelihoodInput: the conversation snapshot
// needed to estimate the reply probability.
type ReplyLikelihoodInput struct {
	// MentionBot reports whether the new messages mention the bot by name.
	MentionBot bool
	// AtOther reports whether the new messages @ somebody other than the bot.
	AtOther bool
	// HasQuestionMark reports whether the new messages contain a question mark.
	HasQuestionMark bool
	// PlaceholderOnly reports whether the new messages are all media
	// placeholders.
	PlaceholderOnly bool
	// RecentSelfRatio is the bot's share of the last 5 minutes of messages.
	RecentSelfRatio float64
	// RecentMessageCount is the message count of the last 5 minutes.
	RecentMessageCount int
	// SecondsSinceBotMessage is the gap since the bot's previous message.
	SecondsSinceBotMessage float64
	// PendingCount is the number of new external messages in this batch.
	PendingCount int
}

// SummarizePendingTexts mirrors reply_likelihood.summarize_pending_texts: it
// derives (at_other, has_question_mark, placeholder_only) from the new message
// texts.
func SummarizePendingTexts(texts []string, hasAtBot bool) (atOther bool, hasQuestionMark bool, placeholderOnly bool) {
	stripped := make([]string, 0, len(texts))
	for _, text := range texts {
		if normalized := strings.TrimSpace(text); normalized != "" {
			stripped = append(stripped, normalized)
		}
	}
	if !hasAtBot {
		for _, text := range stripped {
			if strings.Contains(text, "@") {
				atOther = true
				break
			}
		}
	}
	for _, text := range stripped {
		if strings.Contains(text, "?") || strings.Contains(text, "？") {
			hasQuestionMark = true
			break
		}
	}
	if len(stripped) > 0 {
		placeholderOnly = true
		for _, text := range stripped {
			matched := false
			for _, prefix := range MediaPlaceholderPrefixes {
				if strings.HasPrefix(text, prefix) {
					matched = true
					break
				}
			}
			if !matched {
				placeholderOnly = false
				break
			}
		}
	}
	return atOther, hasQuestionMark, placeholderOnly
}

// EstimateReplyProbability mirrors
// reply_likelihood.estimate_reply_probability: the logistic regression over the
// eight fitted features (clamped exactly like the Python version).
func EstimateReplyProbability(input ReplyLikelihoodInput) float64 {
	logit := Intercept +
		WeightMentionBot*boolToFloat(input.MentionBot) +
		WeightAtOther*boolToFloat(input.AtOther) +
		WeightQuestionMark*boolToFloat(input.HasQuestionMark) +
		WeightPlaceholderOnly*boolToFloat(input.PlaceholderOnly) +
		WeightRecentSelfRatio*math.Min(1.0, math.Max(0.0, input.RecentSelfRatio)) +
		WeightRecentMessageCount*float64(maxInt(0, input.RecentMessageCount)) +
		WeightLogSecondsSinceBot*math.Log1p(math.Max(0.0, input.SecondsSinceBotMessage)) +
		WeightPendingCount*float64(minInt(MaxPendingCount, maxInt(0, input.PendingCount)))
	return 1.0 / (1.0 + math.Exp(-logit))
}

func boolToFloat(value bool) float64 {
	if value {
		return 1.0
	}
	return 0.0
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}

func minInt(left int, right int) int {
	if left < right {
		return left
	}
	return right
}
