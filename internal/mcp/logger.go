package mcp

import (
	"log/slog"
	"os"
	"sync"
)

// 本包唯一的 logger 入口：默认输出到 stderr（stdout 保留给 stdio 协议），
// 由装配层通过 SetLogger 注入项目的 slog。/ 未注入时降级为 Debug 级 stderr。
var (
	defaultLoggerMu sync.RWMutex
	defaultLogger   = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).With("component", "mcp")
)

// SetLogger 注入全局 logger（装配层在启动时调用一次）。
func SetLogger(logger *slog.Logger) {
	if logger == nil {
		return
	}
	defaultLoggerMu.Lock()
	defaultLogger = logger.With("component", "mcp")
	defaultLoggerMu.Unlock()
}

func loggerOrDefault(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	defaultLoggerMu.RLock()
	defer defaultLoggerMu.RUnlock()
	return defaultLogger
}
