package emoji

import (
	"strings"
)

// splitTagText 按 Python `re.split(r"[,，、;；\r\n\t]+", ...)` 的语义切分。
func splitTagText(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case ',', '，', '、', ';', '；', '\r', '\n', '\t':
			return true
		}
		return false
	})
}

// trimSpace 去首尾空白（对应 Python str.strip()）。
func trimSpace(s string) string { return strings.TrimSpace(s) }

// toLower 对应 Python str.lower()。
func toLower(s string) string { return strings.ToLower(s) }
