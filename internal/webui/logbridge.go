// logbridge — 宿主把主程序 slog 接入 WebUI 日志服务的入口。
// 典型用法（cmd/bot/main.go）：
//
//	logService := webui.NewLogBridge(levelVar)     // 先建桥
//	slog.SetDefault(slog.New(slog.NewMultiHandler(stderrHandler, logService)))
//	// 之后 NewServer(Options{Logs: logService, LogLevelVar: levelVar, ...})
package webui

import (
	"log/slog"

	"oh-my-qqbot/internal/webui/routers"
)

// NewLogBridge 创建与 Server 共享的日志服务实例（未绑定 hub）。
// levelVar 同时供宿主 slog handler 与 /logs/level 接口使用（传 nil 则内部自建）。
func NewLogBridge(levelVar *slog.LevelVar) *routers.LogService {
	return routers.NewShared(0, levelVar)
}