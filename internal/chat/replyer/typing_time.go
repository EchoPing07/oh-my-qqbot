package replyer

import (
	"strings"
	"unicode"
)

// This file mirrors calculate_typing_time in
// 参考/MaiBot/src/chat/utils/utils.py (behaviour is authoritative there).
//
// Default typing parameters照抄 Python 版：中文 0.3s/字、英文 0.15s/字。

// TypingTimeDefaults mirrors the default arguments of calculate_typing_time.
const (
	// ChineseCharTime mirrors chinese_time=0.3.
	ChineseCharTime = 0.3
	// EnglishCharTime mirrors english_time=0.15.
	EnglishCharTime = 0.15
)

// isChineseChar mirrors the `\u4e00 <= char <= "\u9fff"` range checks.
func isChineseChar(char rune) bool {
	return '\u4e00' <= char && char <= '\u9fff'
}

// CalculateTypingTime mirrors calculate_typing_time:
//
//   - 中文 0.3s/字、英文（其余字符）0.15s/字；
//   - 只有一个中文字符（strip 后长度为 1）时使用 3 倍中文时间并加 0.3s 回车时间，
//     该分支提前返回，不经过 emoji 与 typing_speed 缩放（照抄 Python 分支顺序）；
//   - isEmoji=true 时固定 1 秒；
//   - typingSpeed<=0 时返回 0，否则总时长乘以 typing_speed。
func CalculateTypingTime(inputString string, typingSpeed float64, isEmoji bool) float64 {
	return calculateTypingTime(inputString, ChineseCharTime, EnglishCharTime, typingSpeed, isEmoji)
}

// calculateTypingTime is the parameterized core of calculate_typing_time.
func calculateTypingTime(inputString string, chineseTime float64, englishTime float64, typingSpeed float64, isEmoji bool) float64 {
	// 计算中文字符数
	chineseChars := 0
	for _, char := range inputString {
		if isChineseChar(char) {
			chineseChars++
		}
	}

	// 如果只有一个中文字符，使用3倍时间（含回车时间；照抄 Python 提前返回；
	// Python len(str) 按字符计数，Go 对应 rune 数）
	if chineseChars == 1 && len([]rune(strings.TrimSpace(inputString))) == 1 {
		return chineseTime*3 + 0.3
	}

	// 正常计算所有字符的输入时间
	totalTime := 0.0
	for _, char := range inputString {
		if isChineseChar(char) {
			totalTime += chineseTime
		} else {
			totalTime += englishTime
		}
	}
	if isEmoji {
		totalTime = 1
	}

	if typingSpeed <= 0 {
		return 0
	}
	totalTime *= typingSpeed

	return totalTime
}

// isEnglishLetterRune mirrors is_english_letter: 检查字符是否为英文字母（忽略大小写）。
func isEnglishLetterRune(char rune) bool {
	lower := unicode.ToLower(char)
	return 'a' <= lower && lower <= 'z'
}

// isEnglishLetterString is the string-form helper used by the splitter.
func isEnglishLetterString(char string) bool {
	for _, r := range char {
		// Python compares single characters; multi-rune lowering (rare) is
		// treated as "not an English letter" exactly like the string compare.
		return isEnglishLetterRune(r)
	}
	return false
}
