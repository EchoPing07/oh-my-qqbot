// 测试辅助 — slog record 构造与 strings reader。
package webui

import (
	"io"
	"log/slog"
	"strings"
	"time"
)

// stringsReader 是 io.Reader 别名（测试用）。
type stringsReader = strings.Reader

func newStringsReader(s string) *strings.Reader { return strings.NewReader(s) }

// logRecord 构造一条 slog.Record（module attr 模拟 slog 桥）。
func logRecord(level, message string) slog.Record {
	var l slog.Level
	switch level {
	case "DEBUG":
		l = slog.LevelDebug
	case "WARN":
		l = slog.LevelWarn
	case "ERROR":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	rec := slog.NewRecord(time.Now(), l, message, 0)
	rec.Add(slog.String("module", "test.module"))
	return rec
}

// 确保 io 被引用（logservice 实现文件中的 reader 使用）。
var _ io.Reader = (*strings.Reader)(nil)
