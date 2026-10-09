package trigger

import (
	"math"
	"testing"
)

// The expected values below are produced by re-running the frozen Python
// implementation (参考/MaiBot/src/maisaka/turn_trigger/reply_likelihood.py) on the
// same feature vectors; they pin the intercept and every fitted weight.
func TestEstimateReplyProbabilityPinnedValues(t *testing.T) {
	cases := []struct {
		name  string
		input ReplyLikelihoodInput
		want  float64
	}{
		{"neutral", ReplyLikelihoodInput{SecondsSinceBotMessage: 3600}, 0.3245564748028923},
		{"neutral_pending1", ReplyLikelihoodInput{SecondsSinceBotMessage: 3600, PendingCount: 1}, 0.3471229034658555},
		{"neutral_pending20_capped", ReplyLikelihoodInput{SecondsSinceBotMessage: 3600, PendingCount: 20}, 0.784331627182798},
		{"neutral_pending100_capped_like_20", ReplyLikelihoodInput{SecondsSinceBotMessage: 3600, PendingCount: 100}, 0.784331627182798},
		{"mention", ReplyLikelihoodInput{MentionBot: true, SecondsSinceBotMessage: 3600}, 0.7010473031473048},
		{"at_other", ReplyLikelihoodInput{AtOther: true, SecondsSinceBotMessage: 3600}, 0.1360361824767588},
		{"question", ReplyLikelihoodInput{HasQuestionMark: true, SecondsSinceBotMessage: 3600}, 0.4372796196949449},
		{"placeholder", ReplyLikelihoodInput{PlaceholderOnly: true, SecondsSinceBotMessage: 3600}, 0.1466638905474217},
		{
			"full",
			ReplyLikelihoodInput{
				MentionBot: true, AtOther: true, HasQuestionMark: true, PlaceholderOnly: true,
				RecentSelfRatio: 1.0, RecentMessageCount: 12, SecondsSinceBotMessage: 45, PendingCount: 7,
			},
			0.865380314717405,
		},
		{
			"clamped_negative_features",
			ReplyLikelihoodInput{
				RecentSelfRatio: 2.5, RecentMessageCount: -5, SecondsSinceBotMessage: -10, PendingCount: 100,
			},
			0.9699971279925801,
		},
		{
			"boundary_300_messages",
			ReplyLikelihoodInput{RecentMessageCount: 300, SecondsSinceBotMessage: 1, PendingCount: 20},
			3.9195034434036866e-05,
		},
		{
			"ratio_half_ten_messages_two_minutes",
			ReplyLikelihoodInput{RecentSelfRatio: 0.5, RecentMessageCount: 10, SecondsSinceBotMessage: 120, PendingCount: 3},
			0.5832646450747622,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := EstimateReplyProbability(testCase.input)
			if math.Abs(got-testCase.want) > 1e-12 {
				t.Fatalf("EstimateReplyProbability() = %.17g, want %.17g", got, testCase.want)
			}
		})
	}
}

// TestFittedConstantsArePinned guards the exact numbers copied from
// reply_likelihood.py — a typo here silently changes every gating decision.
func TestFittedConstantsArePinned(t *testing.T) {
	if Intercept != -1.2005 {
		t.Errorf("Intercept = %v, want -1.2005", Intercept)
	}
	weights := map[string]float64{
		"mention_bot":           WeightMentionBot,
		"at_other":              WeightAtOther,
		"question_mark":         WeightQuestionMark,
		"placeholder_only":      WeightPlaceholderOnly,
		"recent_self_ratio":     WeightRecentSelfRatio,
		"recent_message_count":  WeightRecentMessageCount,
		"log_seconds_since_bot": WeightLogSecondsSinceBot,
		"pending_count":         WeightPendingCount,
	}
	want := map[string]float64{
		"mention_bot":           1.5852,
		"at_other":              -1.1157,
		"question_mark":         0.4807,
		"placeholder_only":      -1.0281,
		"recent_self_ratio":     2.6525,
		"recent_message_count":  -0.0367,
		"log_seconds_since_bot": 0.0571,
		"pending_count":         0.1012,
	}
	for name, value := range weights {
		if value != want[name] {
			t.Errorf("weight[%s] = %v, want %v", name, value, want[name])
		}
	}
	if MaxPendingCount != 20 {
		t.Errorf("MaxPendingCount = %d, want 20", MaxPendingCount)
	}
	if DefaultSecondsSinceBotMessage != 3600.0 {
		t.Errorf("DefaultSecondsSinceBotMessage = %v, want 3600", DefaultSecondsSinceBotMessage)
	}
	wantPrefixes := []string{"[表情包", "[图片", "[消息类型]", "[CQ:image", "[语音", "[文件]", "[卡片"}
	if len(MediaPlaceholderPrefixes) != len(wantPrefixes) {
		t.Fatalf("MediaPlaceholderPrefixes = %v, want %v", MediaPlaceholderPrefixes, wantPrefixes)
	}
	for index, prefix := range wantPrefixes {
		if MediaPlaceholderPrefixes[index] != prefix {
			t.Errorf("MediaPlaceholderPrefixes[%d] = %q, want %q", index, MediaPlaceholderPrefixes[index], prefix)
		}
	}
}

func TestSummarizePendingTexts(t *testing.T) {
	cases := []struct {
		name             string
		texts            []string
		hasAtBot         bool
		wantAtOther      bool
		wantQuestionMark bool
		wantPlaceholder  bool
	}{
		{"placeholders", []string{"[表情包]", " [图片，识别中.....] "}, false, false, false, true},
		{"question_after_blank", []string{"  ", "hi?"}, false, false, true, false},
		{"at_suppressed_by_bot_at", []string{"@某某"}, true, false, false, false},
		{"at_other", []string{"@某某 你看"}, false, true, false, false},
		{"fullwidth_question", []string{"在吗？"}, false, false, true, false},
		{"empty", []string{"", "  "}, false, false, false, false},
		{"mixed_placeholder_and_text", []string{"[表情包]", "文字"}, false, false, false, false},
		{"cq_image_prefix", []string{"[CQ:image,file=x]"}, false, false, false, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			atOther, questionMark, placeholderOnly := SummarizePendingTexts(testCase.texts, testCase.hasAtBot)
			if atOther != testCase.wantAtOther ||
				questionMark != testCase.wantQuestionMark ||
				placeholderOnly != testCase.wantPlaceholder {
				t.Fatalf("SummarizePendingTexts() = (%v, %v, %v), want (%v, %v, %v)",
					atOther, questionMark, placeholderOnly,
					testCase.wantAtOther, testCase.wantQuestionMark, testCase.wantPlaceholder)
			}
		})
	}
}
