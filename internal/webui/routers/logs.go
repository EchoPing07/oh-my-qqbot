// 日志路由 — Ring buffer 最近 N 条 + slog handler 实时流（WS logs 域）。
// 对齐 logs_ws.py 的 load_recent_logs / broadcast_log 与前端日志字段
// {id, timestamp, level, module, message}。
package routers

import (
	"net/http"
)

// handleRecentLogs 对齐「最近 N 条日志」查询（前端日志页快照）。
func handleRecentLogs(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Logs == nil {
			fail(w, http.StatusServiceUnavailable, "日志服务未启用")
			return
		}
		// S9：limit 经 queryInt 统一钳制（1..500，负数/非法回退默认 100）。
		limit := queryInt(r, "limit", 100)
		level := r.URL.Query().Get("level")
		ok(w, map[string]any{"logs": d.Logs.Recent(limit, level), "total": d.Logs.Total()})
	}
}

// handleGetLogLevel 读取当前 slog 级别。
func handleGetLogLevel(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Logs == nil {
			fail(w, http.StatusServiceUnavailable, "日志服务未启用")
			return
		}
		ok(w, map[string]any{"level": d.Logs.LevelName()})
	}
}

// handleSetLogLevel 设置 slog 级别（DEBUG/INFO/WARN/ERROR）。
func handleSetLogLevel(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Logs == nil {
			fail(w, http.StatusServiceUnavailable, "日志服务未启用")
			return
		}
		var body struct {
			Level string `json:"level"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if !d.Logs.SetLevelByName(body.Level) {
			fail(w, http.StatusBadRequest, "无效的日志级别: "+body.Level+"（支持 DEBUG/INFO/WARN/ERROR）")
			return
		}
		ok(w, map[string]any{"level": d.Logs.LevelName()})
	}
}
