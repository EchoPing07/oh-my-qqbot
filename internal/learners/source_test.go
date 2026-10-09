package learners_test

import (
	"context"
	"testing"

	"oh-my-qqbot/internal/chat/replyer"
	"oh-my-qqbot/internal/learners"
)

// 本文件验证 P6 的 ReplyerExpressionSource 与 P4b replyer.ExpressionSource 的
// 接口对接（跨包编译期断言 + 可选择器端到端选择）。

// 编译期断言（跨包）。
var _ replyer.ExpressionSource = (*learners.ReplyerExpressionSource)(nil)

// inMemoryStore 是 learners.ExpressionStore 的最小实现。
type inMemoryStore struct {
	rows []learners.ExpressionRow
}

func (s *inMemoryStore) LoadCandidates(_ string, checkedOnly bool) ([]learners.ExpressionRow, error) {
	out := make([]learners.ExpressionRow, 0, len(s.rows))
	for _, row := range s.rows {
		if checkedOnly && !row.Checked {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *inMemoryStore) FindExact(_ string, _ string, _ string) (*learners.ExpressionRow, error) {
	return nil, nil
}

func (s *inMemoryStore) Upsert(record learners.ExpressionRecord) (learners.ExpressionRecord, error) {
	return record, nil
}

func (s *inMemoryStore) UpdateChecked(_ int64, _ bool, _ string) error { return nil }

// TestReplyerSelectorConsumesLearnerSource 端到端：replyer 选择器读取 learners
// 的候选来源并按 habits 块渲染。
func TestReplyerSelectorConsumesLearnerSource(t *testing.T) {
	rows := make([]learners.ExpressionRow, 0, 12)
	for position := 0; position < 12; position++ {
		rows = append(rows, learners.ExpressionRow{
			ID:        int64(position + 1),
			Situation: "情景" + itoa(position),
			Style:     "风格" + itoa(position),
			Count:     position + 1,
		})
	}
	source := learners.NewReplyerExpressionSource(&inMemoryStore{rows: rows}, nil)

	selector := &replyer.MaisakaExpressionSelector{Source: source}
	result := selector.SelectForReply(context.Background(), replyer.SelectionInput{SessionID: "s1"})
	if len(result.SelectedExpressionIDs) == 0 {
		t.Fatalf("选择器应从未注入子代理的来源拿到随手候选池: %+v", result)
	}
	if result.ExpressionHabits == "" {
		t.Fatalf("应渲染表达习惯块")
	}
}

// TestReplyerSourceHonoursPromptExampleFilter 验证 prompt 示例风格被过滤。
func TestReplyerSourceHonoursPromptExampleFilter(t *testing.T) {
	rows := []learners.ExpressionRow{
		{ID: 1, Situation: "惊叹", Style: "使用 我嘞个xxxx", Count: 3}, // prompt 示例泄漏
		{ID: 2, Situation: "肯定", Style: "使用 绝了，这也太强了", Count: 2},
		{ID: 3, Situation: "空情景", Style: "  ", Count: 1},
	}
	source := learners.NewReplyerExpressionSource(&inMemoryStore{rows: rows}, nil)
	candidates := source.LoadAllCandidates("s1")
	if len(candidates) != 1 {
		t.Fatalf("prompt 示例风格应被过滤: %+v", candidates)
	}
	if candidates[0].ID != 2 || candidates[0].Style != "绝了，这也太强了" {
		t.Fatalf("style 应去掉「使用」前缀: %+v", candidates[0])
	}
	if source.UseVectorExpression() {
		t.Fatalf("无索引时不应报告启用向量召回")
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
