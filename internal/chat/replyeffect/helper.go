package replyeffect

import (
	"strconv"
)

// formatFloat mirrors Python str(float) 在 JSON 数值上的常见输出形态
// （整数值不带小数点，其余保持最短表示）。
func formatFloat(value float64) string {
	if value == float64(int64(value)) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}
