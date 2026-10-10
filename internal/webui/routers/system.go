// 系统路由 — 对齐 参考/MaiBot/src/webui/routers/system.py 的 /system/status
// 与 reload-config（news 代理面无前端消费方，随轻量化原则移除）。
package routers

import (
	"log/slog"
	"net/http"
	"runtime"
	"time"
)

// handleSystemStatus 对齐 GET /system/status（StatusResponse）。
func handleSystemStatus(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		startedAt := d.StartedAt
		if startedAt.IsZero() {
			startedAt = time.Now()
		}
		uptime := time.Since(startedAt).Seconds()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		writeJSON(w, http.StatusOK, map[string]any{
			"success":    true,
			"running":    true,
			"uptime":     uptime,
			"version":    d.Version,
			"start_time": startedAt.Format(time.RFC3339),
			"go_version": runtime.Version(),
			"memory": map[string]any{
				"heap_alloc_bytes": mem.HeapAlloc,
				"heap_sys_bytes":   mem.HeapSys,
				"sys_bytes":        mem.Sys,
				"num_goroutine":    runtime.NumGoroutine(),
				"num_cpu":          runtime.NumCPU(),
				"gc_count":         mem.NumGC,
			},
		})
	}
}

// handleReloadConfig 对齐 POST /system/reload-config：触发配置热重载。
// 经 reloadNow 立即重读磁盘双配置并分发回调（与 fsnotify 热重载同一路径），
// 返回当前热重载代数。无变化的场景也会递增代数（对齐「请求即重载」语义）。
func handleReloadConfig(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		if err := d.Config.ReloadNow("bot", "model"); err != nil {
			fail(w, http.StatusInternalServerError, "配置热重载失败: "+err.Error())
			return
		}
		slog.Info("配置已热重载（REST 触发）", "reload_revision", d.Config.ReloadRevision())
		ok(w, map[string]any{"message": "配置已热重载", "reload_revision": d.Config.ReloadRevision()})
	}
}
