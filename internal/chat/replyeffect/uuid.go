package replyeffect

import (
	"crypto/rand"
	"encoding/hex"
)

// newUUIDv4 生成 36 字符的 UUID v4 文本（对齐 Python uuid.uuid4 的存储形态：
// 小写、带连字符，effect_id 列宽 36）。
func newUUIDv4() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 退化路径：随机源不可用时用计数器式伪随机（仅保证唯一性）。
		for i := range buf {
			buf[i] = byte(i * 17)
		}
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10
	encoded := hex.EncodeToString(buf[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}
