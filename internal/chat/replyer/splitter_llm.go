package replyer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"oh-my-qqbot/internal/llm"
)

// This file mirrors 参考/MaiBot/src/chat/utils/llm_sentence_splitter.py
// (behaviour is authoritative there): 基于 LLM 的回复断句。

// NoSplitMarker mirrors _NO_SPLIT_MARKER.
const NoSplitMarker = "no_split"

// SplitterPrompt mirrors _SPLITTER_PROMPT（照抄文案）。
const SplitterPrompt = `
请不要思考，快速地把下面的回复进行断句，按自然语义拆成若干条适合逐条发送的消息。
要求：
1. 只能返回 JSON 字符串数组，例如 ["第一句。", "第二句？"]，不要 Markdown、解释或代码块。
2. 必须保留原文全部字符、标点、空格和换行，只能在原文已有字符之间选择切分位置，不能改写、增删或调整顺序。
3. 不要把括号、引号、颜文字或代码从中间拆开。
4. 如果不需要拆分，返回"no_split"。

原文：
`

// normalizeJSONPayload mirrors _normalize_json_payload:
// 去除模型偶尔添加的 Markdown JSON 代码块包装。
func normalizeJSONPayload(payload string) string {
	normalized := strings.TrimSpace(payload)
	if strings.HasPrefix(normalized, "```json") && strings.HasSuffix(normalized, "```") {
		return strings.TrimSpace(normalized[7 : len(normalized)-3])
	}
	if strings.HasPrefix(normalized, "```") && strings.HasSuffix(normalized, "```") {
		return strings.TrimSpace(normalized[3 : len(normalized)-3])
	}
	return normalized
}

// isNoSplitMarker mirrors _is_no_split_marker:
// 判断模型返回的是否为「不拆分」标记，兼容是否被写成 JSON 字符串。
func isNoSplitMarker(payload string) bool {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(payload), `"`)) == NoSplitMarker
}

// extractTrailingComma mirrors _extract_trailing_comma:
// 将段尾逗号移入分隔符，逐条发送时省略，压缩合并时恢复。
func extractTrailingComma(sentence string) [2]string {
	content := strings.TrimRight(sentence, "，, \t\r\n")
	separator := sentence[len(content):]
	if strings.TrimSpace(content) != "" &&
		(strings.Contains(separator, "，") || strings.Contains(separator, ",")) {
		return [2]string{content, separator}
	}
	return [2]string{sentence, ""}
}

// SplitSentence is the (content, separator) pair shared with the rule splitter.
type SplitSentence = [2]string

// SplitTextWithLLM mirrors split_text_with_llm:
// 调用 LLM 断句，并返回兼容规则断句器的句子元组。
//
// chatFunc 是包内最小 LLM 接口（不依赖 internal/llm 具体类型）；
// Python 版走 task_name="fast_model", request_type="response.splitter"。
func SplitTextWithLLM(ctx context.Context, text string, chatFunc ChatFunc) ([]SplitSentence, error) {
	if text == "" {
		return nil, nil
	}

	result, err := chatFunc(ctx, ReplyerChatRequest{
		TaskName:    "fast_model",
		RequestType: "response.splitter",
		Items:       []*llm.ContextItem{llm.NewUserItem(SplitterPrompt + text)},
		Context:     ctx,
	})
	if err != nil {
		return nil, err
	}
	payload := normalizeJSONPayload(result)

	// 模型判断不需要拆分时只返回标记，此时作为单独一段，并同样处理段尾逗号
	if isNoSplitMarker(payload) {
		return []SplitSentence{extractTrailingComma(text)}, nil
	}

	var sentences []string
	if err := json.Unmarshal([]byte(payload), &sentences); err != nil {
		return nil, fmt.Errorf("LLM 断句结果不是合法 JSON 数组: %w", err)
	}

	if len(sentences) == 0 {
		return nil, fmt.Errorf("LLM 断句结果必须是非空字符串数组")
	}
	for _, item := range sentences {
		var anyItem any
		_ = anyItem
		_ = item
	}
	// all strings 已经由 json.Unmarshal 到 []string 保证

	if len(sentences) == 1 && sentences[0] == NoSplitMarker {
		return []SplitSentence{extractTrailingComma(text)}, nil
	}

	// 换行不影响消息内容校验，其他字符（包括空格和标点）仍必须完整保留。
	comparisonText := strings.ReplaceAll(strings.ReplaceAll(text, "\r", ""), "\n", "")
	var reconstructed strings.Builder
	for _, sentence := range sentences {
		reconstructed.WriteString(strings.ReplaceAll(strings.ReplaceAll(sentence, "\r", ""), "\n", ""))
	}
	if reconstructed.String() != comparisonText {
		// 保留完整性校验，并显示首处差异
		comparison := []rune(comparisonText)
		rebuilt := []rune(reconstructed.String())
		mismatchIndex := len(comparison)
		if len(rebuilt) < mismatchIndex {
			mismatchIndex = len(rebuilt)
		}
		for index := 0; index < mismatchIndex; index++ {
			if comparison[index] != rebuilt[index] {
				mismatchIndex = index
				break
			}
		}
		contextStart := mismatchIndex - 20
		if contextStart < 0 {
			contextStart = 0
		}
		contextEnd := mismatchIndex + 21
		if contextEnd > len(comparison) {
			contextEnd = len(comparison)
		}
		return nil, fmt.Errorf(
			"LLM 断句结果未完整保留原文（忽略换行后）：首个差异位置=%d（从 0 开始），原文长度=%d，结果长度=%d，原文片段=%q，结果片段=%q",
			mismatchIndex, len(comparison), len(rebuilt),
			string(comparison[contextStart:contextEnd]),
			string(rebuilt[contextStart:min(contextEnd, len(rebuilt))]),
		)
	}

	// 完整性检查通过后再分离段尾逗号，避免改变模型输出校验与句内标点。
	resultSentences := make([]SplitSentence, 0, len(sentences))
	for _, sentence := range sentences {
		if sentence == "" {
			continue
		}
		resultSentences = append(resultSentences, extractTrailingComma(sentence))
	}
	return resultSentences, nil
}

func min(a int, b int) int {
	if a < b {
		return a
	}
	return b
}
