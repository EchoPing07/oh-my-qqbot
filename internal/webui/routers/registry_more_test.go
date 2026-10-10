// S8/S9 补充测试 — user-emojis 内容回源路由 + queryInt 分页钳制。
package routers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// TestQueryIntClamping S9：queryInt 统一钳制（负数/0→默认值，>500→500）。
func TestQueryIntClamping(t *testing.T) {
	cases := []struct {
		raw  string
		def  int
		want int
	}{
		{"", 100, 100},      // 缺省
		{"-1", 100, 100},    // 负数 → 默认值（防 ?limit=-1 全表）
		{"0", 100, 100},     // 0 → 默认值
		{"50", 100, 50},     // 正常值
		{"99999", 100, 500}, // 超上限 → 500
		{"500", 100, 500},   // 恰好上限
		{"abc", 100, 100},   // 非法 → 默认值
		{" 42 ", 100, 42},   // 前后空白容忍
		{"-5", 24, 24},      // offset/hours 类同样回默认
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		req.URL.RawQuery = "limit=" + url.QueryEscape(c.raw)
		if got := queryInt(req, "limit", c.def); got != c.want {
			t.Fatalf("queryInt(limit=%q, def=%d) = %d, want %d", c.raw, c.def, got, c.want)
		}
	}
	// offset 负数归 0（默认值即 0）。
	req := httptest.NewRequest("GET", "/", nil)
	req.URL.RawQuery = "offset=-3"
	if got := queryInt(req, "offset", 0); got != 0 {
		t.Fatalf("queryInt(offset=-3) = %d, want 0", got)
	}
}

// TestListRouteNegativeLimitClamped 路由级：负 limit 不再直通 SQLite 全表。
func TestListRouteNegativeLimitClamped(t *testing.T) {
	env := newTestEnv(t, func(d *Deps) {
		// logs/recent 需要日志服务（否则 503）。
		d.Logs = NewLogService(10, nil, nil)
	})
	for _, path := range []string{
		"/api/webui/chat/sessions?limit=-1",
		"/api/webui/chat/sessions?limit=99999",
		"/api/webui/person/list?limit=-1&offset=-2",
		"/api/webui/expression/list?limit=-1",
		"/api/webui/jargon/list?limit=-1",
		"/api/webui/logs/recent?limit=-1",
	} {
		out := env.do(t, "GET", path, nil, http.StatusOK)
		if out["success"] != true {
			t.Fatalf("%s 应成功返回（钳制后正常执行）", path)
		}
	}
}

// TestUserEmojiContentRoute S8：content_url 指向的内容回源路由可用。
func TestUserEmojiContentRoute(t *testing.T) {
	env := newTestEnv(t, nil)
	// 造一个用户表情文件。
	dir := filepath.Join(env.deps.DataDir, "webui_user_emojis", "user1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("fake-png-bytes")
	if err := os.WriteFile(filepath.Join(dir, "abc.png"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	// 带鉴权正常回源。
	req := httptest.NewRequest("GET", "/api/webui/user-emojis/abc/content?user_id=user1", nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("content 回源应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(content) {
		t.Fatalf("content 内容不符: %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type 应为 image/png, got %q", got)
	}

	// 未鉴权 → 401。
	req = httptest.NewRequest("GET", "/api/webui/user-emojis/abc/content?user_id=user1", nil)
	rec = httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401, got %d", rec.Code)
	}

	// 路径穿越尝试 → 404（sanitizeUserID 剥离特殊字符）。
	traversal := fmt.Sprintf("/api/webui/user-emojis/%s/content?user_id=user1", "..%2F..%2Fwebui")
	req = httptest.NewRequest("GET", traversal, nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	rec = httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("路径穿越应 404, got %d", rec.Code)
	}

	// 不存在的 ID → 404。
	req = httptest.NewRequest("GET", "/api/webui/user-emojis/none/content?user_id=user1", nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	rec = httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的表情应 404, got %d", rec.Code)
	}
}
