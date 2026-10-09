// 测试辅助 — slog record 构造与整数格式化。
package routers

import (
	"log/slog"
	"strconv"
	"time"
)

// recordLike / slogLevel 等是 slog.Record 的测试别名（避免测试文件直接拼类型）。
type recordLike = slog.Record

type slogLevel = slog.Level

const (
	slogLevelDebug = slog.LevelDebug
	slogLevelInfo  = slog.LevelInfo
	slogLevelWarn  = slog.LevelWarn
	slogLevelError = slog.LevelError
)

// newRecord 构造一条 slog.Record（带 module attr）。
func newRecord(l slog.Level, message string) slog.Record {
	rec := slog.NewRecord(time.Now(), l, message, 0)
	rec.Add(slog.String("module", "test"))
	return rec
}

// itoa 整数格式化（测试路径 ID 拼接用）。
func itoa(n int) string { return strconv.Itoa(n) }
