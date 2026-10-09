// reasoning_process 路由 — Planner 快照历史查询（对齐 routers/reasoning_process.py
// 的历史请求预览面，Go 版以 MonitorBridge 缓存的 planner.progress 事件承载）。
package routers

import (
	"net/http"
	"strings"
)

// handleSnapshots 对齐快照历史查询：session 过滤 + limit。
func handleSnapshots(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Snapshots == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": true, "snapshots": []ReasoningSnapshot{}, "count": 0,
			})
			return
		}
		limit := queryInt(r, "limit", 50)
		if limit <= 0 || limit > 500 {
			limit = 50
		}
		sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
		snaps := d.Snapshots.RecentPlannerSnapshots(sessionID, limit)
		if snaps == nil {
			snaps = []ReasoningSnapshot{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "snapshots": snaps, "count": len(snaps),
		})
	}
}
