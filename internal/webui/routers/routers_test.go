// routers 包测试 — 路由注册、鉴权中间件、chat/history/statistics（SQLite）、
// user_emoji、news 代理、data_transfer 导出导入。
package routers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	config "oh-my-qqbot/internal/config"
	"oh-my-qqbot/internal/database"
)

// testEnv 组装 mux + deps（带 SQLite 与 config.Manager）。
type testEnv struct {
	mux   *http.ServeMux
	deps  *Deps
	token string
}

func newTestEnv(t *testing.T, mutate func(d *Deps)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mgr := config.NewManager(filepath.Join(dir, "config"))
	if err := mgr.Initialize(); err != nil {
		t.Fatalf("初始化配置: %v", err)
	}

	deps := &Deps{
		Config:    mgr,
		DB:        db,
		Version:   "test",
		StartedAt: time.Now(),
		DataDir:   dir,
	}
	if mutate != nil {
		mutate(deps)
	}
	// 内联 TokenManager 桩（避免环依赖 webui 包）。
	auth := &stubAuth{token: "test-token-12345"}
	deps.Auth = auth
	mux := http.NewServeMux()
	Register(mux, deps)
	return &testEnv{mux: mux, deps: deps, token: auth.token}
}

func (e *testEnv) do(t *testing.T, method, path string, body any, wantStatus int) map[string]any {
	t.Helper()
	var req *http.Request
	if body != nil {
		data, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, strings.NewReader(string(data)))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s = %d, want %d; body=%s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return out
}

// stubAuth 是 AuthAPI 桩。
type stubAuth struct{ token string }

func (s *stubAuth) Token() string                     { return s.token }
func (s *stubAuth) VerifyToken(t string) bool         { return t == s.token }
func (s *stubAuth) UpdateToken(string) (bool, string) { return true, "ok" }
func (s *stubAuth) RegenerateToken() string           { return s.token }
func (s *stubAuth) TokenSource() string               { return "temporary" }
func (s *stubAuth) IsFirstSetup() bool                { return true }
func (s *stubAuth) MarkSetupCompleted() bool          { return true }
func (s *stubAuth) ResetSetup() bool                  { return true }
func (s *stubAuth) GenerateWSToken(string) string     { return "ws-temp-token" }
func (s *stubAuth) VerifyWSToken(t string) bool       { return t == "ws-temp-token" }

func TestRegisterHealthNoAuth(t *testing.T) {
	env := newTestEnv(t, nil)
	env.do(t, "GET", "/api/webui/health", nil, http.StatusOK)
	// 未带 token 的受保护路由 401。
	req := httptest.NewRequest("GET", "/api/webui/system/status", nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401, got %d", rec.Code)
	}
}

func TestChatSessionsAndHistory(t *testing.T) {
	env := newTestEnv(t, nil)
	// 灌数据：一个会话 + 两条消息。
	_, err := env.deps.DB.Write().Exec(`
		INSERT INTO chat_sessions (session_id, created_timestamp, last_active_timestamp,
			user_id, user_nickname, group_id, group_name, platform)
		VALUES ('sess-1', ?, ?, 'u1', 'alice', 'g1', '测试群', 'qq')`,
		formatTimeForDB(time.Now()), formatTimeForDB(time.Now()))
	if err != nil {
		t.Fatalf("灌会话: %v", err)
	}
	for i, text := range []string{"你好", "早上好"} {
		_, err := env.deps.DB.Write().Exec(`
			INSERT INTO mai_messages (message_id, timestamp, platform, user_id, user_nickname,
				group_id, session_id, raw_content, processed_plain_text)
			VALUES (?, ?, 'qq', 'u1', 'alice', 'g1', 'sess-1', '{}', ?)`,
			"m"+string(rune('0'+i)), formatTimeForDB(time.Now().Add(time.Duration(i)*time.Second)), text)
		if err != nil {
			t.Fatalf("灌消息: %v", err)
		}
	}

	out := env.do(t, "GET", "/api/webui/chat/sessions", nil, http.StatusOK)
	if out["count"] != float64(1) {
		t.Fatalf("应返回 1 个会话: %v", out)
	}
	out = env.do(t, "GET", "/api/webui/chat/history?session_id=sess-1&limit=10", nil, http.StatusOK)
	msgs := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应返回 2 条消息: %v", out)
	}
	first := msgs[0].(map[string]any)
	if first["text"] != "你好" {
		t.Fatalf("消息应按时间升序: %v", first)
	}
	// 缺 session_id → 400。
	env.do(t, "GET", "/api/webui/chat/history", nil, http.StatusBadRequest)
}

func TestStatisticsRoutes(t *testing.T) {
	env := newTestEnv(t, nil)
	// 灌 llm_usage。
	for i := 0; i < 3; i++ {
		_, err := env.deps.DB.Write().Exec(`
			INSERT INTO llm_usage (model_name, model_api_provider_name, session_id, task_name,
				request_type, time_cost, timestamp, prompt_tokens, completion_tokens, total_tokens, cost)
			VALUES ('test-model', 'prov', 'sess-1', 'planner', 'chat', 0.5, ?, 10, 20, 30, 0.01)`,
			formatTimeForDB(time.Now()))
		if err != nil {
			t.Fatalf("灌 usage: %v", err)
		}
	}
	out := env.do(t, "GET", "/api/webui/statistics/summary?hours=24", nil, http.StatusOK)
	if out["request_count"] != float64(3) || out["total_tokens"] != float64(90) {
		t.Fatalf("summary 聚合错误: %v", out)
	}
	out = env.do(t, "GET", "/api/webui/statistics/models?hours=24", nil, http.StatusOK)
	models := out["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models 聚合应 1 行: %v", out)
	}
	m := models[0].(map[string]any)
	if m["model_name"] != "test-model" || m["request_count"] != float64(3) {
		t.Fatalf("models 行错误: %v", m)
	}
	env.do(t, "GET", "/api/webui/statistics/daily?days=7", nil, http.StatusOK)
}

func TestResourceRoutes(t *testing.T) {
	env := newTestEnv(t, nil)
	db := env.deps.DB
	// person。
	db.Write().Exec(`INSERT INTO person_info (is_known, person_id, person_name, platform, user_id, user_nickname, know_counts)
		VALUES (1, 'pid-1', '小明', 'qq', 'u1', 'nick', 3)`)
	out := env.do(t, "GET", "/api/webui/person/list", nil, http.StatusOK)
	if out["count"] != float64(1) {
		t.Fatalf("person/list 应 1 行: %v", out)
	}
	env.do(t, "DELETE", "/api/webui/person/pid-1", nil, http.StatusOK)
	env.do(t, "DELETE", "/api/webui/person/missing", nil, http.StatusNotFound)

	// expression。
	res, _ := db.Write().Exec(`INSERT INTO expressions (situation, style, content_list, count, last_active_time, create_time, checked)
		VALUES ('situ', 'styl', '[]', 2, ?, ?, 0)`, formatTimeForDB(time.Now()), formatTimeForDB(time.Now()))
	exprID, _ := res.LastInsertId()
	out = env.do(t, "GET", "/api/webui/expression/list", nil, http.StatusOK)
	if out["count"] != float64(1) {
		t.Fatalf("expression/list 应 1 行")
	}
	env.do(t, "PUT", "/api/webui/expression/"+itoa(int(exprID))+"/check", map[string]any{"checked": true}, http.StatusOK)

	// jargon。
	res, _ = db.Write().Exec(`INSERT INTO jargons (content, meaning, count, is_jargon, created_by, created_timestamp, updated_timestamp)
		VALUES ('绝绝子', '非常好', 5, 0, 'AI', ?, ?)`, formatTimeForDB(time.Now()), formatTimeForDB(time.Now()))
	jargonID, _ := res.LastInsertId()
	env.do(t, "GET", "/api/webui/jargon/list", nil, http.StatusOK)
	env.do(t, "PUT", "/api/webui/jargon/"+itoa(int(jargonID)), map[string]any{"is_jargon": true}, http.StatusOK)
	env.do(t, "DELETE", "/api/webui/jargon/"+itoa(int(jargonID)), nil, http.StatusOK)

	// emoji。
	db.Write().Exec(`INSERT INTO images (image_hash, description, full_path, image_type, is_registered, record_time)
		VALUES ('hash-1', '开心', 'data/emoji/hash-1.png', 'emoji', 1, ?)`, formatTimeForDB(time.Now()))
	out = env.do(t, "GET", "/api/webui/emoji/list", nil, http.StatusOK)
	if out["count"] != float64(1) {
		t.Fatalf("emoji/list 应 1 行")
	}
	env.do(t, "GET", "/api/webui/emoji/hash-1", nil, http.StatusOK)
	env.do(t, "PUT", "/api/webui/emoji/hash-1/ban", map[string]any{"banned": true}, http.StatusOK)
	env.do(t, "DELETE", "/api/webui/emoji/hash-1", nil, http.StatusOK)
}

func TestLogsRoutes(t *testing.T) {
	env := newTestEnv(t, func(d *Deps) {
		d.Logs = NewLogService(16, nil, nil)
	})
	d := env.deps.Logs
	d.Handle(nil, logRecordOf("INFO", "first"))
	d.Handle(nil, logRecordOf("ERROR", "second"))

	out := env.do(t, "GET", "/api/webui/logs/recent?limit=10", nil, http.StatusOK)
	if out["total"] != float64(2) {
		t.Fatalf("logs/recent 应 2 条: %v", out)
	}
	logs := out["logs"].([]any)
	if len(logs) != 2 || logs[0].(map[string]any)["level"] != "INFO" {
		t.Fatalf("日志应旧→新: %v", logs)
	}
	out = env.do(t, "GET", "/api/webui/logs/level", nil, http.StatusOK)
	if out["level"] != "INFO" {
		t.Fatalf("默认级别 INFO: %v", out)
	}
	env.do(t, "PUT", "/api/webui/logs/level", map[string]any{"level": "DEBUG"}, http.StatusOK)
	env.do(t, "PUT", "/api/webui/logs/level", map[string]any{"level": "bogus"}, http.StatusBadRequest)
}

// logRecordOf 构造测试 slog record。
func logRecordOf(level, message string) recordLike {
	var l slogLevel
	switch level {
	case "DEBUG":
		l = slogLevelDebug
	case "WARN":
		l = slogLevelWarn
	case "ERROR":
		l = slogLevelError
	default:
		l = slogLevelInfo
	}
	return newRecord(l, message)
}

func TestDataTransferRoundTrip(t *testing.T) {
	env := newTestEnv(t, nil)
	// 导出。
	out := env.do(t, "POST", "/api/webui/data-transfer/export", nil, http.StatusOK)
	if out["bot_config"] == nil || out["model_config"] == nil {
		t.Fatalf("导出应含双配置: %v", out)
	}
	// 导入（把导出的内容原样灌回）。
	env.do(t, "POST", "/api/webui/data-transfer/import", out, http.StatusOK)
	// 缺少可识别配置 → 400。
	env.do(t, "POST", "/api/webui/data-transfer/import", map[string]any{"extra": 1}, http.StatusBadRequest)
}

func TestUserEmojiRoutes(t *testing.T) {
	env := newTestEnv(t, nil)
	dir := filepath.Join(env.deps.DataDir, "webui_user_emojis", "userabc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "abcd1234.png"), []byte("png"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	out := env.do(t, "GET", "/api/webui/user-emojis?user_id=userabc", nil, http.StatusOK)
	if out["count"] != float64(1) {
		t.Fatalf("应列出 1 个表情: %v", out)
	}
	env.do(t, "DELETE", "/api/webui/user-emojis/abcd1234?user_id=userabc", nil, http.StatusOK)
	// 路径穿越清洗。
	env.do(t, "GET", "/api/webui/user-emojis?user_id=../evil", nil, http.StatusOK)
}

func TestMemoryRoutesDegraded(t *testing.T) {
	env := newTestEnv(t, nil)
	env.do(t, "POST", "/api/webui/memory/search", map[string]any{"query": "x"}, http.StatusServiceUnavailable)
	env.do(t, "GET", "/api/webui/memory/profiles", nil, http.StatusServiceUnavailable)
}

// TestLogServiceRecordAttrsNotDropped 回归 P1：record 级 attrs（每次调用传的
// "err", err 等）必须进入 message，否则 WebUI 日志只剩光秃秃的警告
// （docs/DEBUG_REPORT_2026-10-10_link_failures.md §4）。
func TestLogServiceRecordAttrsNotDropped(t *testing.T) {
	d := NewLogService(16, nil, nil)
	rec := newRecord(slogLevelWarn, "心流会话创建失败")
	rec.AddAttrs(slog.String("err", "未找到 session_id=xxx 对应的聊天流"))
	if err := d.Handle(nil, rec); err != nil {
		t.Fatal(err)
	}
	entries := d.Recent(10, "")
	if len(entries) != 1 {
		t.Fatalf("应 1 条日志: %d", len(entries))
	}
	msg := entries[0].Message
	if !strings.Contains(msg, "心流会话创建失败") || !strings.Contains(msg, "err=未找到 session_id=xxx 对应的聊天流") {
		t.Fatalf("record attrs 应并入 message: %q", msg)
	}
}

// TestLogServiceWithAttrsNoDuplicateAppend 确认预置 attrs（WithAttrs）与
// record attrs 同时存在时不双重拼接。
func TestLogServiceWithAttrsNoDuplicateAppend(t *testing.T) {
	base := NewLogService(16, nil, nil)
	h := base.WithAttrs([]slog.Attr{slog.String("session", "s1")})
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "turn done", 0)
	rec.AddAttrs(slog.String("round", "r1"))
	if err := h.Handle(nil, rec); err != nil {
		t.Fatal(err)
	}
	entries := base.Recent(10, "")
	if len(entries) != 1 {
		t.Fatalf("应 1 条日志: %d", len(entries))
	}
	msg := entries[0].Message
	if strings.Count(msg, "session=s1") != 1 || strings.Count(msg, "round=r1") != 1 {
		t.Fatalf("预置与 record attrs 应各出现一次: %q", msg)
	}
}

// TestBotSchemaPutsBotSectionFirst 「机器人设置」的节导航必须把「机器人」排在第一位
// （内含 AppID / AppSecret 等首次启用必填凭据），且保持服务端声明的有序输出。
func TestBotSchemaPutsBotSectionFirst(t *testing.T) {
	env := newTestEnv(t, nil)
	req := httptest.NewRequest("GET", "/api/webui/config/schema/bot", nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("schema 应 200, got %d", rec.Code)
	}
	var body struct {
		Schema struct {
			Properties []struct {
				Name  string `json:"name"`
				Title string `json:"title"`
			} `json:"properties"`
		} `json:"schema"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("schema 响应解析失败: %v", err)
	}
	props := body.Schema.Properties
	if len(props) == 0 {
		t.Fatal("schema 不应为空")
	}
	if props[0].Name != "bot" || props[0].Title != "机器人" {
		t.Fatalf("第一节应为 bot/机器人, got %s/%s", props[0].Name, props[0].Title)
	}
}
