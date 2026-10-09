package retrieval

import (
	"math"
	"strings"
	"unicode"
)

// mathPow 是 math.Pow 的薄封装（集中数值原语便于单测）。
func mathPow(base, exp float64) float64 { return math.Pow(base, exp) }

// normalizeNFKC 近似 Python 的 unicodedata.normalize("NFKC", text)。
//
// Go 标准库不含完整 NFKC；A_memorix 只用它做逐字落地（grounding）比较
// （见 core/retrieval/dual_path.py L1891-L1895），关键需求是
// ① 全角/半角折叠 ② 兼容字符（如全角标点）归一。这里实现这两类映射，
// 覆盖中英混排文本的实际比较场景。
func normalizeNFKC(text string) string {
	if text == "" {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		b.WriteRune(foldCompatibility(r))
	}
	return b.String()
}

// foldCompatibility 折叠常见兼容字符。
func foldCompatibility(r rune) rune {
	switch {
	// 全角 ASCII（U+FF01..U+FF5E）→ 半角（U+0021..U+007E）。
	case r >= 0xFF01 && r <= 0xFF5E:
		return r - 0xFEE0
	// 全角空格 → 半角空格。
	case r == 0x3000:
		return ' '
	// 常见全角标点兼容映射（NFKC 语义）。
	case r == 0x2018 || r == 0x2019 || r == 0x02BC:
		return '\''
	case r == 0x201C || r == 0x201D:
		return '"'
	case r == 0x2010 || r == 0x2011 || r == 0x2012 || r == 0x2013 || r == 0x2014 || r == 0x2015:
		return '-'
	case r == 0xFEFF:
		return 0 // BOM
	}
	return r
}

// CaseFold 近似 Python str.casefold()：小写化 + 常见变体折叠。
func CaseFold(text string) string {
	return strings.ToLowerSpecial(unicode.TurkishCase, normalizeNFKC(text))
}
