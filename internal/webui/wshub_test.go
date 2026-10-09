// WSHub 测试 — httptest + gorilla websocket Dial：鉴权握手（临时 token 一次性）、
// 订阅/退订、广播、ping/pong、快照回放。
package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testHubEnv 组装 httptest server + token manager + hub。
type testHubEnv struct {
	tm     *TokenManager
	hub    *WSHub
	server *httptest.Server
}

func newTestHubEnv(t *testing.T) *testHubEnv {
	t.Helper()
	tm := newTestTokenManager(t)
	hub := NewWSHub(tm)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/webui/ws", hub.HandleWS)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &testHubEnv{tm: tm, hub: hub, server: server}
}

// wsURL 组装 WS 地址（可带 query）。
func (e *testHubEnv) wsURL(query string) string {
	url := "ws" + strings.TrimPrefix(e.server.URL, "http") + "/api/webui/ws"
	if query != "" {
		url += "?" + query
	}
	return url
}

// dialWS 建立 WS 连接。
func dialWS(t *testing.T, url string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	return websocket.DefaultDialer.Dial(url, header)
}

// recvMsg 读取一条消息（带超时）。
func recvMsg(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读 WS 消息失败: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("消息不是 JSON: %v", err)
	}
	return msg
}

func TestWSHandshakeRejectsUnauthenticated(t *testing.T) {
	env := newTestHubEnv(t)
	_, resp, err := dialWS(t, env.wsURL(""), nil)
	if err == nil {
		t.Fatalf("未认证握手应被拒绝")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("应返回 401, got %v", resp)
	}
}

// 临时 token 只能用一次：第二个连接用同一 token 必须失败。
func TestWSHandshakeTempTokenOneShot(t *testing.T) {
	env := newTestHubEnv(t)
	token := env.tm.GenerateWSToken(env.tm.Token())

	conn, _, err := dialWS(t, env.wsURL("token="+token), nil)
	if err != nil {
		t.Fatalf("首次握手应成功: %v", err)
	}
	msg := recvMsg(t, conn)
	if msg["op"] != "event" || msg["event"] != "ready" {
		t.Fatalf("应先收到 ready 事件, got %v", msg)
	}
	conn.Close()

	_, resp, err := dialWS(t, env.wsURL("token="+token), nil)
	if err == nil {
		t.Fatalf("同一临时 token 第二次握手应被拒绝")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("应返回 401, got %v", resp)
	}
}

// Bearer 头握手（开发调试路径）。
func TestWSHandshakeBearer(t *testing.T) {
	env := newTestHubEnv(t)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+env.tm.Token())
	conn, _, err := dialWS(t, env.wsURL(""), header)
	if err != nil {
		t.Fatalf("Bearer 握手应成功: %v", err)
	}
	defer conn.Close()
	msg := recvMsg(t, conn)
	if msg["event"] != "ready" {
		t.Fatalf("应收到 ready 事件, got %v", msg)
	}
}

func TestWSSubscribeAndBroadcast(t *testing.T) {
	env := newTestHubEnv(t)
	conn, _, err := dialWS(t, env.wsURL("token="+env.tm.GenerateWSToken(env.tm.Token())), nil)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer conn.Close()
	_ = recvMsg(t, conn) // ready

	// 订阅 logs:main。
	if err := conn.WriteJSON(map[string]any{
		"op": "subscribe", "id": "s1", "domain": "logs", "topic": "main",
	}); err != nil {
		t.Fatalf("发送订阅失败: %v", err)
	}
	resp := recvMsg(t, conn)
	if resp["op"] != "response" || resp["ok"] != true {
		t.Fatalf("订阅响应应 ok, got %v", resp)
	}

	// 广播到 logs:main → 客户端应收到 entry 事件。
	env.hub.Broadcast("logs", "main", "entry", map[string]any{"entry": map[string]any{"message": "hello"}})
	msg := recvMsg(t, conn)
	if msg["op"] != "event" || msg["domain"] != "logs" || msg["event"] != "entry" {
		t.Fatalf("应收到 logs entry 事件, got %v", msg)
	}

	// 广播到未订阅域 → 收不到（用超时验证）。
	env.hub.Broadcast("chat", "main", "message", map[string]any{"x": 1})
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatalf("未订阅的域不应收到广播")
	}
}

func TestWSUnsubscribeStopsDelivery(t *testing.T) {
	env := newTestHubEnv(t)
	conn, _, err := dialWS(t, env.wsURL("token="+env.tm.GenerateWSToken(env.tm.Token())), nil)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer conn.Close()
	_ = recvMsg(t, conn)

	// 订阅 maisaka_monitor（响应后会跟一条 planner.reset 事件）。
	if err := conn.WriteJSON(map[string]any{
		"op": "subscribe", "id": "s1", "domain": "maisaka_monitor", "topic": "main",
	}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if resp := recvMsg(t, conn); resp["ok"] != true {
		t.Fatalf("订阅应成功: %v", resp)
	}
	if msg := recvMsg(t, conn); msg["event"] != "planner.reset" {
		t.Fatalf("应收到 planner.reset: %v", msg)
	}
	// 退订。
	if err := conn.WriteJSON(map[string]any{
		"op": "unsubscribe", "id": "u1", "domain": "maisaka_monitor", "topic": "main",
	}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if resp := recvMsg(t, conn); resp["ok"] != true {
		t.Fatalf("退订应成功: %v", resp)
	}
	env.hub.Broadcast("maisaka_monitor", "main", "planner.progress", map[string]any{"k": 1})
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatalf("退订后不应再收到广播")
	}
}

func TestWSPingPong(t *testing.T) {
	env := newTestHubEnv(t)
	conn, _, err := dialWS(t, env.wsURL("token="+env.tm.GenerateWSToken(env.tm.Token())), nil)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer conn.Close()
	_ = recvMsg(t, conn)
	if err := conn.WriteJSON(map[string]any{"op": "ping"}); err != nil {
		t.Fatalf("发送 ping 失败: %v", err)
	}
	msg := recvMsg(t, conn)
	if msg["op"] != "pong" {
		t.Fatalf("应收到 pong, got %v", msg)
	}
}

// 订阅 maisaka_monitor 应先收到 planner.reset 标记（对齐 Python 版流程）。
func TestWSMonitorSubscribeSendsReset(t *testing.T) {
	env := newTestHubEnv(t)
	conn, _, err := dialWS(t, env.wsURL("token="+env.tm.GenerateWSToken(env.tm.Token())), nil)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer conn.Close()
	_ = recvMsg(t, conn)

	if err := conn.WriteJSON(map[string]any{
		"op": "subscribe", "id": "s2", "domain": "maisaka_monitor", "topic": "main",
	}); err != nil {
		t.Fatalf("发送订阅失败: %v", err)
	}
	if resp := recvMsg(t, conn); resp["ok"] != true {
		t.Fatalf("订阅响应应 ok: %v", resp)
	}
	msg := recvMsg(t, conn)
	if msg["event"] != "planner.reset" {
		t.Fatalf("订阅监控域应先收到 planner.reset, got %v", msg)
	}
}

// 不支持的订阅目标返回错误响应。
func TestWSUnsupportedSubscription(t *testing.T) {
	env := newTestHubEnv(t)
	conn, _, err := dialWS(t, env.wsURL("token="+env.tm.GenerateWSToken(env.tm.Token())), nil)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer conn.Close()
	_ = recvMsg(t, conn)

	if err := conn.WriteJSON(map[string]any{
		"op": "subscribe", "id": "s3", "domain": "plugin_runtime", "topic": "main",
	}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	resp := recvMsg(t, conn)
	if resp["ok"] != false {
		t.Fatalf("不支持的目标应返回 ok=false, got %v", resp)
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "unsupported_subscription" {
		t.Fatalf("应返回 unsupported_subscription 错误, got %v", errObj)
	}
}
