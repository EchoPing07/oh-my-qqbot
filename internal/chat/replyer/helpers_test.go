package replyer

import (
	"context"
	"fmt"
	"time"

	"oh-my-qqbot/internal/message"
)

// 测试辅助类型与别名。

type contextT = context.Context

func testContext() context.Context { return context.Background() }

func fakeCandidates(n int) []ExpressionCandidate {
	candidates := make([]ExpressionCandidate, 0, n)
	for i := 0; i < n; i++ {
		candidates = append(candidates, ExpressionCandidate{
			ID:        int64(i + 1),
			Situation: fmt.Sprintf("场景%d", i+1),
			Style:     fmt.Sprintf("风格%d", i+1),
			Count:     i + 1,
		})
	}
	return candidates
}

type fakeExpressionSource struct {
	candidates []ExpressionCandidate
}

func (f *fakeExpressionSource) LoadAllCandidates(sessionID string) []ExpressionCandidate {
	return f.candidates
}

func (f *fakeExpressionSource) BuildVectorCandidatePool(
	ctx contextT, sessionID string, queryText string, all []ExpressionCandidate, poolSize int,
) ([]ExpressionCandidate, bool) {
	return nil, false
}

func (f *fakeExpressionSource) UseVectorExpression() bool { return false }

// pinyinOf returns the Tone3 pinyin of a single Chinese char ("" if none).
func pinyinOf(char string) string {
	result := ""
	for _, r := range char {
		pys := singlePinyin(r)
		if len(pys) == 0 {
			continue
		}
		result = pys[0]
		break
	}
	return result
}

// makeSessionMessage builds a SessionMessage for tests.
func makeSessionMessage(userID string, nickname string, text string, isSelf bool) *message.SessionMessage {
	cardname := ""
	info := message.NewUserInfo(userID, nickname, cardname)
	if isSelf {
		info = message.NewUserInfo(userID, nickname, cardname)
	}
	sessionMessage := &message.SessionMessage{
		MessageID: fmt.Sprintf("msg_%s", userID),
		Timestamp: time.Now(),
		Platform:  "qq",
		MessageInfo: message.MessageInfo{
			UserInfo: info,
		},
		RawMessage: message.NewMessageSequence(message.NewTextComponent(text)),
	}
	if text != "" {
		plain := text
		sessionMessage.ProcessedPlainText = &plain
	}
	return sessionMessage
}
