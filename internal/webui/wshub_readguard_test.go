// S5 WS 读防护测试 — 超大帧断开 + 心跳保活。
package webui

import (
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dialAuthedWS 以 Bearer 方式建立已认证连接并读完 ready 事件。
func dialAuthedWS(t *testing.T, env *testHubEnv) *websocket.Conn {
	t.Helper()
	conn, _, err := dialWS(t, env.wsURL(""), map[string][]string{
		"Authorization": {"Bearer " + env.tm.Token()},
	})
	if err != nil {
		t.Fatalf("WS 握手失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// 服务端连接后先发 system/ready。
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("读 ready 事件失败: %v", err)
	}
	return conn
}

// TestWSOversizedMessageDisconnected 超过读上限（64KB）的帧会被断开。
func TestWSOversizedMessageDisconnected(t *testing.T) {
	env := newTestHubEnv(t)
	conn := dialAuthedWS(t, env)

	// 发送 65KB 消息（> wsReadLimit）。
	big := []byte(`{"op":"ping","pad":"` + strings.Repeat("a", 65*1024) + `"}`)
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, big); err != nil {
		t.Fatalf("发送超大帧失败: %v", err)
	}
	// 服务端读到超限帧即断开：客户端应收到错误（close/EOF）。
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatalf("超限帧后连接应被服务端断开")
	}
	// 清理是异步的：轮询等待连接被摘除。
	deadline := time.Now().Add(3 * time.Second)
	for env.hub.ClientCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if env.hub.ClientCount() != 0 {
		t.Fatalf("断开后连接应被清理, remaining=%d", env.hub.ClientCount())
	}
}

// TestWSHeartbeatKeepsAlive 缩短心跳参数：多轮 ping 周期内连接保持可用。
func TestWSHeartbeatKeepsAlive(t *testing.T) {
	origLimit, origWait, origPeriod := wsReadLimit, wsReadWait, wsPingPeriod
	wsReadWait = 2 * time.Second
	wsPingPeriod = 300 * time.Millisecond
	defer func() {
		wsReadLimit, wsReadWait, wsPingPeriod = origLimit, origWait, origPeriod
	}()

	env := newTestHubEnv(t)
	conn := dialAuthedWS(t, env)

	// 经历 3 个 ping 周期（gorilla 客户端默认 ping handler 自动回 pong，
	// 服务端读 deadline 应被持续续期）。
	time.Sleep(3*wsPingPeriod + 500*time.Millisecond)

	// 连接仍可用：发送协议层 ping，应收到 pong。
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := conn.WriteJSON(map[string]any{"op": "ping"}); err != nil {
		t.Fatalf("心跳周期后发送失败: %v", err)
	}
	msg := recvMsg(t, conn)
	if msg["op"] != "pong" {
		t.Fatalf("应收到 pong, got %v", msg)
	}
	if env.hub.ClientCount() != 1 {
		t.Fatalf("心跳正常时连接不应被回收, remaining=%d", env.hub.ClientCount())
	}
}

// TestWSDeadConnectionReaped 不回 pong 的死连接在 wsReadWait 内被回收。
func TestWSDeadConnectionReaped(t *testing.T) {
	origLimit, origWait, origPeriod := wsReadLimit, wsReadWait, wsPingPeriod
	wsReadWait = 1 * time.Second
	wsPingPeriod = 10 * time.Minute // 不发 ping：模拟客户端不回 pong
	defer func() {
		wsReadLimit, wsReadWait, wsPingPeriod = origLimit, origWait, origPeriod
	}()

	env := newTestHubEnv(t)
	conn := dialAuthedWS(t, env)

	// 读 deadline 1s 到期 → 服务端断开并清理。
	deadline := time.Now().Add(5 * time.Second)
	for env.hub.ClientCount() == 1 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if env.hub.ClientCount() != 0 {
		t.Fatalf("死连接应被回收, remaining=%d", env.hub.ClientCount())
	}
	_ = conn.Close()
}
