// 鉴权路由 — 对齐 参考/MaiBot/src/webui/routes.py 的 auth/setup/ws-token 面。
package routers

import (
	"net/http"
	"time"
)

// handleAuthVerify 对齐 POST /api/webui/auth/verify：验证 token（成功即认为
// 已登录；Go 版鉴权通过 Authorization: Bearer 头携带，不再下发 Cookie）。
func handleAuthVerify(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Auth == nil {
			fail(w, http.StatusServiceUnavailable, "鉴权服务未启用")
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if !d.Auth.VerifyToken(body.Token) {
			writeJSON(w, http.StatusOK, map[string]any{
				"valid": false, "message": "Token 无效或已过期",
			})
			return
		}
		source := d.Auth.TokenSource()
		writeJSON(w, http.StatusOK, map[string]any{
			"valid":                 true,
			"message":               "Token 验证成功",
			"is_first_setup":        d.Auth.IsFirstSetup(),
			"token_source":          source,
			"requires_custom_token": source == "temporary",
		})
	}
}

// handleAuthLogout 对齐 POST /auth/logout（无 Cookie 语义，保持响应形）。
func handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"message": "已成功登出"})
}

// handleAuthCheck 对齐 GET /auth/check：前端判断是否已登录。
func handleAuthCheck(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Auth == nil || !d.Auth.VerifyToken(bearerToken(r)) {
			writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
			return
		}
		source := d.Auth.TokenSource()
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated":         true,
			"token_source":          source,
			"requires_custom_token": source == "temporary",
		})
	}
}

// handleAuthUpdate 对齐 POST /auth/update：设置自定义固定 token。
func handleAuthUpdate(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			NewToken string `json:"new_token"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		success, message := d.Auth.UpdateToken(body.NewToken)
		writeJSON(w, http.StatusOK, map[string]any{"success": success, "message": message})
	}
}

// handleAuthRegenerate 对齐 POST /auth/regenerate：重新生成 token。
func handleAuthRegenerate(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		newToken := d.Auth.RegenerateToken()
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "token": newToken, "message": "Token 已重新生成",
		})
	}
}

// handleSetupStatus 对齐 GET /setup/status。允许匿名访问（Python 版
// verify_token_optional 语义）：带有效 Bearer 时返回真实状态；未带/无效时
// authenticated=false 但仍回传 setup 状态，登录页向导依赖此响应分流。
func handleSetupStatus(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authenticated := d.Auth != nil && d.Auth.VerifyToken(bearerToken(r))
		source := d.Auth.TokenSource()
		writeJSON(w, http.StatusOK, map[string]any{
			"is_first_setup":        d.Auth.IsFirstSetup(),
			"token_source":          source,
			"requires_custom_token": source == "temporary",
			"authenticated":         authenticated,
			"message":               "状态查询成功",
		})
	}
}

// handleSetupComplete 对齐 POST /setup/complete。
func handleSetupComplete(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if success := d.Auth.MarkSetupCompleted(); !success {
			fail(w, http.StatusInternalServerError, "标记首次配置完成失败")
			return
		}
		ok(w, map[string]any{"message": "首次配置已完成"})
	}
}

// handleSetupReset 对齐 POST /setup/reset。
func handleSetupReset(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if success := d.Auth.ResetSetup(); !success {
			fail(w, http.StatusInternalServerError, "重置首次配置状态失败")
			return
		}
		ok(w, map[string]any{"message": "首次配置状态已重置"})
	}
}

// handleWSToken 对齐 GET /api/webui/ws-token：为 WS 握手签发 60s 一次性临时令牌。
// 未认证时返回 200 + success=false（对齐 Python 版避免前端因 401 刷新页面）。
func handleWSToken(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const wsTokenExpireSeconds = 60
		if d.Auth == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": false, "message": "鉴权服务未启用", "token": nil, "expires_in": 0,
			})
			return
		}
		token := bearerToken(r)
		if token == "" || !d.Auth.VerifyToken(token) {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": false, "message": "认证已过期，请重新登录", "token": nil, "expires_in": 0,
			})
			return
		}
		wsToken := d.Auth.GenerateWSToken(token)
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "token": wsToken, "expires_in": wsTokenExpireSeconds,
		})
	}
}

// requireAuth 包裹需要鉴权的路由：校验 Authorization: Bearer（对齐
// dependencies.require_auth 的 401 语义）。
func requireAuth(d *Deps, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Auth == nil {
			fail(w, http.StatusServiceUnavailable, "鉴权服务未启用")
			return
		}
		if !d.Auth.VerifyToken(bearerToken(r)) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="webui"`)
			fail(w, http.StatusUnauthorized, "Token 无效或已过期")
			return
		}
		next(w, r)
	}
}

// bearerToken 提取 Authorization: Bearer 头中的令牌。
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		return trimSpace(auth[len(prefix):])
	}
	return ""
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// unixNow 返回当前 unix 秒（监控/统计响应时间戳用）。
func unixNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }
