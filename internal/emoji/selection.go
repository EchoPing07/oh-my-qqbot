package emoji

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parseSelectionJSON mirrors EmojiSelectionResult.model_validate_json：
// 从子代理输出中解析 JSON {emoji_index, reason}（容忍代码围栏与前后杂文，
// 取第一个 {...} 平衡块）。
func parseSelectionJSON(content string) (*EmojiSelectionResult, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, fmt.Errorf("emoji: 子代理输出为空")
	}
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("emoji: 子代理输出中没有 JSON 对象")
	}
	var result EmojiSelectionResult
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &result); err != nil {
		return nil, fmt.Errorf("emoji: 解析子代理 JSON 失败: %w", err)
	}
	if result.EmojiIndex < 1 {
		return nil, fmt.Errorf("emoji: 子代理序号无效: %d", result.EmojiIndex)
	}
	return &result, nil
}
