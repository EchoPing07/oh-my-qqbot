package replyer

import (
	"regexp"
	"strings"
)

// This file mirrors protect_kaomoji / recover_kaomoji in
// 参考/MaiBot/src/chat/utils/utils.py (behaviour is authoritative there):
// 识别并保护句子中的颜文字（含括号与无括号），替换为占位符后再恢复。

// kaomojiPattern mirrors kaomoji_pattern:
//
//	([(（\[【] 非括号字符* 至少一个非中文/英文/数字/空格字符 非括号字符* [)）\]】])
//	| ([▼▽・ᴥω･﹏^><≧≦￣｀´∀ヮДд︿﹀へ｡ﾟ╥╯╰︶︹•⁄]{2,15})
//
// 非 ASCII 字符用 \x{...} 码点写法，等价于 Python 原字符集：
// [( （ [ 【 / ) ] ） 】 / 一-龥。
var kaomojiPattern = regexp.MustCompile(
	"(" +
		"[\\x{28}\\x{5B}\\x{FF08}\\x{3010}]" +
		"[^\\x{28}\\x{29}\\x{5B}\\x{5D}\\x{FF08}\\x{FF09}\\x{3010}\\x{3011}]*?" +
		"[^\\x{4E00}-\\x{9FA5}a-zA-Z0-9\\s]" +
		"[^\\x{28}\\x{29}\\x{5B}\\x{5D}\\x{FF08}\\x{FF09}\\x{3010}\\x{3011}]*?" +
		"[\\x{29}\\x{5D}\\x{FF09}\\x{3011}]" +
		")" +
		"|([▼▽・ᴥω･﹏^><≧≦￣｀´∀ヮДд︿﹀へ｡ﾟ╥╯╰︶︹•⁄]{2,15})",
)

// KaomojiMapping mirrors the {占位符: 颜文字} mapping table.
type KaomojiMapping map[string]string

// ProtectKaomoji mirrors protect_kaomoji: 返回 (处理后的句子, 占位符映射)。
//
// 「[表情包…]」占位文本不参与保护（照抄 Python 的 skip 分支）。
func ProtectKaomoji(sentence string) (string, KaomojiMapping) {
	kaomojiMatches := kaomojiPattern.FindAllStringSubmatch(sentence, -1)
	placeholderToKaomoji := KaomojiMapping{}

	for _, match := range kaomojiMatches {
		kaomoji := match[1]
		if kaomoji == "" {
			kaomoji = match[2]
		}
		if strings.HasPrefix(kaomoji, "[表情包") && strings.HasSuffix(kaomoji, "]") {
			continue
		}
		idx := len(placeholderToKaomoji)
		placeholder := "__KAOMOJI_" + itoa(idx) + "__"
		sentence = strings.Replace(sentence, kaomoji, placeholder, 1)
		placeholderToKaomoji[placeholder] = kaomoji
	}

	return sentence, placeholderToKaomoji
}

// RecoverKaomoji mirrors recover_kaomoji: 根据映射表恢复句子中的颜文字。
func RecoverKaomoji(sentences []string, placeholderToKaomoji KaomojiMapping) []string {
	recoveredSentences := make([]string, 0, len(sentences))
	for _, sentence := range sentences {
		for placeholder, kaomoji := range placeholderToKaomoji {
			sentence = strings.ReplaceAll(sentence, placeholder, kaomoji)
		}
		recoveredSentences = append(recoveredSentences, sentence)
	}
	return recoveredSentences
}

// itoa is a tiny local helper avoiding a strconv import for clarity parity.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append(digits, byte('0'+value%10))
		value /= 10
	}
	if negative {
		digits = append(digits, '-')
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}
