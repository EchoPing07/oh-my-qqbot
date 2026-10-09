// Server 装配测试 — 静态托管/SPA fallback、鉴权中间件 401、config 路由读写、
// monitor 桥路由、日志服务、statistics/chat/memory 路由依赖降级。
package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	monitor "oh-my-qqbot/internal/monitor"

	"oh-my-qqbot/internal/webui/routers"
)

// newTestServer 组装测试 Server（配置 + 静态目录可选）。
func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.DataDir == "" {
		opts.DataDir = t.TempDir()
	}
	if opts.ConfigDir == "" {
		opts.ConfigDir = t.TempDir()
	}
	s, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(s.Monitor.Close)
	return s
}

// getJSON 发起带鉴权头的 GET/POST 并解析响应。
func doJSON(t *testing.T, h http.Handler, token, method, path string, body any, wantStatus int) map[string]any {
	t.Helper()
	var req *http.Request
	if body != nil {
		data, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, strings.NewReader(string(data)))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s = %d, want %d; body=%s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return out
}

func TestAuthMiddleware401(t *testing.T) {
	s := newTestServer(t, Options{})
	h := s.Handler()
	// 无 token → 401。
	doJSON(t, h, "", "GET", "/api/webui/system/status", nil, http.StatusUnauthorized)
	// 错误 token → 401。
	doJSON(t, h, "wrong-token", "GET", "/api/webui/system/status", nil, http.StatusUnauthorized)
	// 正确 token → 200。
	out := doJSON(t, h, s.TokenManager.Token(), "GET", "/api/webui/system/status", nil, http.StatusOK)
	if out["running"] != true {
		t.Fatalf("system/status 应返回 running=true, got %v", out)
	}
	if out["version"] == nil || out["uptime"] == nil {
		t.Fatalf("system/status 应含 version/uptime: %v", out)
	}
	// health 无需鉴权。
	doJSON(t, h, "", "GET", "/api/webui/health", nil, http.StatusOK)
}

func TestStaticSPAFallback(t *testing.T) {
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<html><body>SPA</body></html>"), 0o644); err != nil {
		t.Fatalf("写 index.html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dist, "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatalf("写 app.js: %v", err)
	}
	s := newTestServer(t, Options{DistDir: dist})
	h := s.Handler()

	// 静态文件命中。
	req := httptest.NewRequest("GET", "/app.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "console.log(1)" {
		t.Fatalf("静态文件应直接返回: %d %s", rec.Code, rec.Body.String())
	}

	// 根路径回 index.html。
	req = httptest.NewRequest("GET", "/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "<html><body>SPA</body></html>" {
		t.Fatalf("根路径应回 index.html: %d %s", rec.Code, rec.Body.String())
	}

	// SPA fallback：无后缀路径回 index.html。
	req = httptest.NewRequest("GET", "/chat/some/route", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "<html><body>SPA</body></html>" {
		t.Fatalf("无后缀路径应 SPA fallback: %d %s", rec.Code, rec.Body.String())
	}

	// 带后缀但不存在 → 404。
	req = httptest.NewRequest("GET", "/missing.js", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("缺失静态文件应 404, got %d", rec.Code)
	}

	// api 前缀不做 SPA 回退。
	req = httptest.NewRequest("GET", "/api/nonexistent", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("api 前缀未匹配应 404, got %d", rec.Code)
	}
}

func TestConfigRoutesReadWrite(t *testing.T) {
	s := newTestServer(t, Options{})
	h := s.Handler()
	token := s.TokenManager.Token()

	// 读 bot 配置。
	out := doJSON(t, h, token, "GET", "/api/webui/config/bot", nil, http.StatusOK)
	if out["success"] != true || out["config"] == nil {
		t.Fatalf("GET config/bot 应返回 config: %v", out)
	}
	cfgMap, _ := out["config"].(map[string]any)
	if cfgMap["bot"] == nil {
		t.Fatalf("bot 配置应含 bot 节: %v", cfgMap)
	}

	// 改 personality.personality 后写回 → 再读应生效。
	cfgMap["personality"].(map[string]any)["personality"] = "测试人格-WebUI"
	doJSON(t, h, token, "PUT", "/api/webui/config/bot", map[string]any{"config": cfgMap}, http.StatusOK)
	out = doJSON(t, h, token, "GET", "/api/webui/config/bot", nil, http.StatusOK)
	cfgMap = out["config"].(map[string]any)
	if got := cfgMap["personality"].(map[string]any)["personality"]; got != "测试人格-WebUI" {
		t.Fatalf("写入的配置未生效: %v", got)
	}

	// 磁盘文件应已更新（热重载生效面）。
	raw, err := os.ReadFile(s.Deps.Config.BotConfigPath())
	if err != nil {
		t.Fatalf("读配置文件: %v", err)
	}
	if !contains(string(raw), "测试人格-WebUI") {
		t.Fatalf("配置文件未落盘更新")
	}

	// model 配置读写。
	out = doJSON(t, h, token, "GET", "/api/webui/config/model", nil, http.StatusOK)
	if out["config"] == nil {
		t.Fatalf("GET config/model 应返回 config")
	}
	doJSON(t, h, token, "PUT", "/api/webui/config/model", out, http.StatusOK)

	// 无效 model 配置（空 models）应 400。
	doJSON(t, h, token, "PUT", "/api/webui/config/model",
		map[string]any{"config": map[string]any{"models": []any{}, "api_providers": []any{}}},
		http.StatusBadRequest)

	// schema 与 models/task-config 查询。
	doJSON(t, h, token, "GET", "/api/webui/config/schema/bot", nil, http.StatusOK)
	doJSON(t, h, token, "GET", "/api/webui/models/list", nil, http.StatusOK)
	doJSON(t, h, token, "GET", "/api/webui/models/providers", nil, http.StatusOK)
	doJSON(t, h, token, "GET", "/api/webui/models/task-config", nil, http.StatusOK)
}

func TestAuthRoutes(t *testing.T) {
	s := newTestServer(t, Options{})
	h := s.Handler()
	token := s.TokenManager.Token()

	// auth/verify 正确/错误。
	out := doJSON(t, h, "", "POST", "/api/webui/auth/verify", map[string]any{"token": token}, http.StatusOK)
	if out["valid"] != true {
		t.Fatalf("verify 应成功: %v", out)
	}
	out = doJSON(t, h, "", "POST", "/api/webui/auth/verify", map[string]any{"token": "bad"}, http.StatusOK)
	if out["valid"] != false {
		t.Fatalf("错误 token verify 应 valid=false")
	}

	// auth/check。
	out = doJSON(t, h, token, "GET", "/api/webui/auth/check", nil, http.StatusOK)
	if out["authenticated"] != true {
		t.Fatalf("auth/check 应 authenticated")
	}

	// ws-token 签发 + 消费。
	out = doJSON(t, h, token, "GET", "/api/webui/ws-token", nil, http.StatusOK)
	if out["success"] != true {
		t.Fatalf("ws-token 应签发成功: %v", out)
	}
	wsToken, _ := out["token"].(string)
	if wsToken == "" || !s.TokenManager.VerifyWSToken(wsToken) {
		t.Fatalf("签发的 ws-token 应可消费")
	}
	// 未认证的 ws-token 请求返回 200 + success=false（对齐 Python 版）。
	out = doJSON(t, h, "", "GET", "/api/webui/ws-token", nil, http.StatusOK)
	if out["success"] != false {
		t.Fatalf("未认证 ws-token 应 success=false")
	}

	// setup 流程。
	out = doJSON(t, h, token, "GET", "/api/webui/setup/status", nil, http.StatusOK)
	if out["is_first_setup"] != true {
		t.Fatalf("新装应为 first_setup")
	}
	doJSON(t, h, token, "POST", "/api/webui/setup/complete", nil, http.StatusOK)
	out = doJSON(t, h, token, "GET", "/api/webui/setup/status", nil, http.StatusOK)
	if out["is_first_setup"] != false {
		t.Fatalf("complete 后不应为 first_setup")
	}

	// regenerate。
	out = doJSON(t, h, s.TokenManager.Token(), "POST", "/api/webui/auth/regenerate", nil, http.StatusOK)
	newToken, _ := out["token"].(string)
	if newToken == "" || newToken == token {
		t.Fatalf("regenerate 应返回新 token")
	}
}

// monitor 桥：事件 → 快照缓存 + WS 域广播。
func TestMonitorBridgeRoutesEvents(t *testing.T) {
	bus := monitor.NewBus()
	s := newTestServer(t, Options{Bus: bus, SnapshotHistory: 8})
	h := s.Handler()

	// 发送 planner.progress 事件。
	monitor.EmitPlannerProgress(bus, monitor.PlannerProgressParams{
		SessionID: "sess-1", CycleID: 1, RunID: "run-a",
		Content: "分析消息", AgentState: "RUNNING", TotalTokens: 100,
	})
	monitor.EmitMessageIngested(bus, monitor.MessageParams{
		SessionID: "sess-1", SpeakerName: "alice", Content: "你好", MessageID: "m1",
		Timestamp: float64(time.Now().UnixNano()) / 1e9,
	})
	// 等待桥消费。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snaps := s.Monitor.RecentPlannerSnapshots("", 10); len(snaps) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	snaps := s.Monitor.RecentPlannerSnapshots("", 10)
	if len(snaps) != 1 {
		t.Fatalf("应缓存 1 条 planner 快照, got %d", len(snaps))
	}
	if snaps[0].SessionID != "sess-1" {
		t.Fatalf("快照 session 过滤错误: %v", snaps[0].SessionID)
	}
	// session 过滤。
	if got := s.Monitor.RecentPlannerSnapshots("sess-2", 10); len(got) != 0 {
		t.Fatalf("不匹配 session 应为空")
	}
	// REST 查询。
	out := doJSON(t, h, s.TokenManager.Token(), "GET", "/api/webui/reasoning-process/snapshots?limit=10", nil, http.StatusOK)
	if out["count"] != float64(1) {
		t.Fatalf("snapshots 接口应返回 1 条: %v", out)
	}
	// 环形上限。
	for i := 0; i < 20; i++ {
		monitor.EmitPlannerProgress(bus, monitor.PlannerProgressParams{
			SessionID: "sess-1", CycleID: i, RunID: "run-b",
		})
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.Monitor.RecentPlannerSnapshots("", 100)) == 8 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(s.Monitor.RecentPlannerSnapshots("", 100)); got != 8 {
		t.Fatalf("快照应受 history=8 上限, got %d", got)
	}
}

// monitor 桥把事件路由到 WS 域（订阅客户端可收到）。
func TestMonitorBridgeBroadcastsToWS(t *testing.T) {
	bus := monitor.NewBus()
	s := newTestServer(t, Options{Bus: bus})
	received := make(chan string, 4)
	s.Deps.Hub = &captureHub{onBroadcast: func(domain, topic, event string, data any) {
		if domain == "maisaka_monitor" {
			received <- event
		}
	}}
	// 桥持有的是构造时的 hub 引用，重建桥以使用 captureHub。
	s.Monitor.Close()
	s.Monitor = NewMonitorBridge(bus, s.Deps.Hub, 10)
	t.Cleanup(s.Monitor.Close)

	monitor.EmitLLMRetry(bus, monitor.LLMRetryParams{
		SessionID: "s", TaskName: "planner", ModelName: "m", Attempt: 2, MaxAttempts: 3,
	})
	monitor.EmitMessageSent(bus, monitor.MessageParams{SessionID: "s", Content: "hi"})

	deadline := time.After(2 * time.Second)
	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case ev := <-received:
			got[ev] = true
		case <-deadline:
			t.Fatalf("应路由 llm.retry 与 message.sent 到 maisaka_monitor, got %v", got)
		}
	}
	if !got["llm.retry"] || !got["message.sent"] {
		t.Fatalf("事件路由不完整: %v", got)
	}
}

// captureHub 记录广播调用。
type captureHub struct {
	onBroadcast func(domain, topic, event string, data any)
}

func (c *captureHub) Broadcast(domain, topic, event string, data any) {
	c.onBroadcast(domain, topic, event, data)
}

// 日志服务：ring buffer + 级别过滤 + 广播。
func TestLogService(t *testing.T) {
	var broadcastEvents []string
	hub := &captureHub{onBroadcast: func(domain, topic, event string, data any) {
		broadcastEvents = append(broadcastEvents, event)
	}}
	svc := routers.NewLogService(8, nil, hub)
	svc.Handle(nil, logRecord("INFO", "hello world"))
	svc.Handle(nil, logRecord("ERROR", "boom"))
	if len(svc.Recent(10, "")) != 2 {
		t.Fatalf("应缓存 2 条日志")
	}
	if len(svc.Recent(10, "ERROR")) != 1 {
		t.Fatalf("ERROR 过滤应剩 1 条")
	}
	if svc.Recent(10, "")[0].Level != "INFO" {
		t.Fatalf("日志应按时间序（旧→新）")
	}
	if svc.LevelName() != "INFO" {
		t.Fatalf("默认级别 INFO")
	}
	if !svc.SetLevelByName("DEBUG") || svc.LevelName() != "DEBUG" {
		t.Fatalf("级别切换失败")
	}
	if svc.SetLevelByName("NOPE") {
		t.Fatalf("非法级别应拒绝")
	}
	if len(broadcastEvents) != 2 || broadcastEvents[0] != "entry" {
		t.Fatalf("日志应广播 entry 事件: %v", broadcastEvents)
	}
}

// CORS 开关注入。
func TestCORSMiddleware(t *testing.T) {
	s := newTestServer(t, Options{CORS: []string{"http://example.com"}})
	h := s.Handler()
	req := httptest.NewRequest("OPTIONS", "/api/webui/system/status", nil)
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS 预检应 204, got %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "http://example.com" {
		t.Fatalf("CORS 头缺失")
	}
}

// 依赖降级：Memory/MCP/Chat 未注入时的响应形态。
func TestDegradedRoutes(t *testing.T) {
	s := newTestServer(t, Options{})
	h := s.Handler()
	token := s.TokenManager.Token()

	out := doJSON(t, h, token, "POST", "/api/webui/memory/search",
		map[string]any{"query": "x"}, http.StatusServiceUnavailable)
	if out["success"] != false {
		t.Fatalf("无记忆内核应 503")
	}
	out = doJSON(t, h, token, "GET", "/api/webui/mcp/status", nil, http.StatusOK)
	if out["enabled"] != false {
		t.Fatalf("无 MCP provider 应返回 disabled 快照")
	}
	out = doJSON(t, h, token, "POST", "/api/webui/chat/send",
		map[string]any{"content": "hi", "user_name": "tester"}, http.StatusNotImplemented)
	if out["success"] != false {
		t.Fatalf("无 ChatInjector 应 501")
	}
	// chat/sessions 无 DB → 503。
	doJSON(t, h, token, "GET", "/api/webui/chat/sessions", nil, http.StatusServiceUnavailable)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
