// mcp 路由 — 对齐 routers/mcp.py 的 GET /mcp/status（经 StatusProvider 最小接口，
// 不 import internal/mcp；未注入时返回 disabled 快照）。
package routers

import (
	"net/http"
)

// handleMCPStatus 对齐 GET /mcp/status。
func handleMCPStatus(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.MCP == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"success":      true,
				"enabled":      false,
				"initialized":  false,
				"server_count": 0,
				"tool_count":   0,
				"servers":      []any{},
			})
			return
		}
		snapshot := d.MCP.StatusSnapshot()
		if snapshot == nil {
			snapshot = map[string]any{}
		}
		body := map[string]any{"success": true}
		for k, v := range snapshot {
			body[k] = v
		}
		writeJSON(w, http.StatusOK, body)
	}
}
