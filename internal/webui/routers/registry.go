// 路由注册与响应辅助 — REST 形态对齐 FastAPI 版：路径前缀 /api/webui，
// 响应体 {"success": bool, ...}（对齐各 router 的返回结构）。
package routers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Register 把全部 REST 路由注册到 mux（对齐 routes.py 的路由清单；
// 前缀 /api/webui + 各 router 的 prefix）。
func Register(mux *http.ServeMux, d *Deps) {
	// ---- 主路由（routes.py，前缀 /api/webui）----
	mux.HandleFunc("GET /api/webui/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "healthy", "service": "oh-my-qqbot WebUI"})
	})
	mux.HandleFunc("POST /api/webui/auth/verify", handleAuthVerify(d))
	mux.HandleFunc("POST /api/webui/auth/logout", handleAuthLogout)
	mux.HandleFunc("GET /api/webui/auth/check", handleAuthCheck(d))
	mux.HandleFunc("POST /api/webui/auth/update", requireAuth(d, handleAuthUpdate(d)))
	mux.HandleFunc("POST /api/webui/auth/regenerate", requireAuth(d, handleAuthRegenerate(d)))
	// 对齐 Python 版 verify_token_optional：setup/status 允许匿名访问并自校验
	// token（未认证时 requires_custom_token/is_first_setup 仍返回真实状态），
	// 供登录页/向导在无 Bearer 头时探测。auth/verify 保持无鉴权（本身即登录）。
	mux.HandleFunc("GET /api/webui/setup/status", handleSetupStatus(d))
	mux.HandleFunc("POST /api/webui/setup/complete", requireAuth(d, handleSetupComplete(d)))
	mux.HandleFunc("POST /api/webui/setup/reset", requireAuth(d, handleSetupReset(d)))
	mux.HandleFunc("GET /api/webui/ws-token", handleWSToken(d))

	// ---- config 路由（routers/config.py，前缀 /api/webui/config）----
	mux.HandleFunc("GET /api/webui/config/bot", requireAuth(d, handleGetBotConfig(d)))
	mux.HandleFunc("POST /api/webui/config/bot", requireAuth(d, handlePutBotConfig(d)))
	mux.HandleFunc("PUT /api/webui/config/bot", requireAuth(d, handlePutBotConfig(d)))
	mux.HandleFunc("GET /api/webui/config/model", requireAuth(d, handleGetModelConfig(d)))
	mux.HandleFunc("POST /api/webui/config/model", requireAuth(d, handlePutModelConfig(d)))
	mux.HandleFunc("PUT /api/webui/config/model", requireAuth(d, handlePutModelConfig(d)))
	mux.HandleFunc("GET /api/webui/config/schema/bot", requireAuth(d, handleGetBotSchema(d)))

	// ---- models 路由（routers/model.py，前缀 /api/webui/models）----
	mux.HandleFunc("GET /api/webui/models/list", requireAuth(d, handleModelsList(d)))
	mux.HandleFunc("GET /api/webui/models/providers", requireAuth(d, handleProvidersList(d)))
	mux.HandleFunc("GET /api/webui/models/task-config", requireAuth(d, handleTaskConfig(d)))

	// ---- system 路由（routers/system.py，前缀 /api/webui/system）----
	mux.HandleFunc("GET /api/webui/system/status", requireAuth(d, handleSystemStatus(d)))
	mux.HandleFunc("GET /api/webui/system/news", requireAuth(d, handleSystemNews(d)))
	mux.HandleFunc("POST /api/webui/system/reload-config", requireAuth(d, handleReloadConfig(d)))

	// ---- logs 路由（logs_ws.py + 级别控制）----
	mux.HandleFunc("GET /api/webui/logs/recent", requireAuth(d, handleRecentLogs(d)))
	mux.HandleFunc("GET /api/webui/logs/level", requireAuth(d, handleGetLogLevel(d)))
	mux.HandleFunc("PUT /api/webui/logs/level", requireAuth(d, handleSetLogLevel(d)))

	// ---- memory 路由（routers/memory.py，简化 admin 面）----
	mux.HandleFunc("POST /api/webui/memory/search", requireAuth(d, handleMemorySearch(d)))
	mux.HandleFunc("GET /api/webui/memory/profiles", requireAuth(d, handleMemoryProfiles(d)))
	mux.HandleFunc("GET /api/webui/memory/profiles/{person_id}", requireAuth(d, handleMemoryProfile(d)))

	// ---- person 路由（routers/person.py）----
	mux.HandleFunc("GET /api/webui/person/list", requireAuth(d, handlePersonList(d)))
	mux.HandleFunc("DELETE /api/webui/person/{person_id}", requireAuth(d, handlePersonDelete(d)))
	mux.HandleFunc("GET /api/webui/person/stats/summary", requireAuth(d, handlePersonStats(d)))

	// ---- expression 路由（routers/expression.py）----
	mux.HandleFunc("GET /api/webui/expression/list", requireAuth(d, handleExpressionList(d)))
	mux.HandleFunc("DELETE /api/webui/expression/{expression_id}", requireAuth(d, handleExpressionDelete(d)))
	mux.HandleFunc("PUT /api/webui/expression/{expression_id}/check", requireAuth(d, handleExpressionCheck(d)))

	// ---- jargon 路由（routers/jargon.py）----
	mux.HandleFunc("GET /api/webui/jargon/list", requireAuth(d, handleJargonList(d)))
	mux.HandleFunc("DELETE /api/webui/jargon/{jargon_id}", requireAuth(d, handleJargonDelete(d)))
	mux.HandleFunc("PUT /api/webui/jargon/{jargon_id}", requireAuth(d, handleJargonUpdate(d)))

	// ---- emoji 路由（routers/emoji/routes.py）----
	mux.HandleFunc("GET /api/webui/emoji/list", requireAuth(d, handleEmojiList(d)))
	mux.HandleFunc("GET /api/webui/emoji/{emoji_id}", requireAuth(d, handleEmojiDetail(d)))
	mux.HandleFunc("DELETE /api/webui/emoji/{emoji_id}", requireAuth(d, handleEmojiDelete(d)))
	mux.HandleFunc("PUT /api/webui/emoji/{emoji_id}/ban", requireAuth(d, handleEmojiBan(d)))

	// ---- reasoning-process 路由（routers/reasoning_process.py）----
	mux.HandleFunc("GET /api/webui/reasoning-process/snapshots", requireAuth(d, handleSnapshots(d)))

	// ---- statistics 路由（routers/statistics.py）----
	mux.HandleFunc("GET /api/webui/statistics/summary", requireAuth(d, handleStatsSummary(d)))
	mux.HandleFunc("GET /api/webui/statistics/models", requireAuth(d, handleStatsModels(d)))
	mux.HandleFunc("GET /api/webui/statistics/daily", requireAuth(d, handleStatsDaily(d)))

	// ---- chat 路由（routers/chat/routes.py 简化面 + WEBUI_CHAT_PLATFORM 注入）----
	mux.HandleFunc("GET /api/webui/chat/sessions", requireAuth(d, handleChatSessions(d)))
	mux.HandleFunc("GET /api/webui/chat/history", requireAuth(d, handleChatHistory(d)))
	mux.HandleFunc("POST /api/webui/chat/send", requireAuth(d, handleChatSend(d)))
	mux.HandleFunc("GET /api/webui/chat/platforms", requireAuth(d, handleChatPlatforms(d)))

	// ---- mcp 路由（routers/mcp.py）----
	mux.HandleFunc("GET /api/webui/mcp/status", requireAuth(d, handleMCPStatus(d)))

	// ---- data-transfer 路由（routers/data_transfer.py 简化面）----
	mux.HandleFunc("POST /api/webui/data-transfer/export", requireAuth(d, handleExport(d)))
	mux.HandleFunc("POST /api/webui/data-transfer/import", requireAuth(d, handleImport(d)))

	// ---- news 路由（routers/news.py）----
	mux.HandleFunc("GET /api/webui/news", requireAuth(d, handleNews(d)))

	// ---- user-emojis 路由（routers/user_emoji.py 简化面）----
	mux.HandleFunc("GET /api/webui/user-emojis", requireAuth(d, handleUserEmojiList(d)))
	// S8：补上列表项 content_url 指向的内容回源路由。
	mux.HandleFunc("GET /api/webui/user-emojis/{emoji_id}/content", requireAuth(d, handleUserEmojiContent(d)))
	mux.HandleFunc("DELETE /api/webui/user-emojis/{emoji_id}", requireAuth(d, handleUserEmojiDelete(d)))
}

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

// fail 输出 {"success": false, "message": msg}（对齐 HTTPException detail 语义）。
func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"success": false, "message": msg})
}

// ok 输出 {"success": true, ...extra}。
func ok(w http.ResponseWriter, extra map[string]any) {
	body := map[string]any{"success": true}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, http.StatusOK, body)
}

// decodeJSON 读取请求体 JSON 到 dst。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是有效的 JSON: "+err.Error())
		return false
	}
	return true
}

// maxListLimit 是列表类查询参数的统一上限（S9：?limit=-1 直通 SQLite 等于
// 不限制，可拖全表；?limit=100000 可放大单请求成本）。
const maxListLimit = 500

// queryInt 读取整型查询参数（缺失/非法返回 def）。
// S9 统一钳制：<=0（含负数）回退默认值；超过 maxListLimit 截到上限。
// 调用点复核：limit/offset（resources/chat/memory/logs/reasoning）与
// statistics 的 hours/days 窗口参数均适用该钳制（负数→默认值，≤500）。
func queryInt(r *http.Request, name string, def int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n <= 0 {
		return def
	}
	if n > maxListLimit {
		return maxListLimit
	}
	return n
}

// pathValue 读取路径参数（Go 1.22+ ServeMux 通配符）。
func pathValue(r *http.Request, name string) string {
	return r.PathValue(name)
}
