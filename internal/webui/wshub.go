// WSHub — WS 统一通道（对齐 参考/MaiBot/src/webui/routers/websocket/unified.py
// 与 manager.py）：
//   - 端点 /api/webui/ws：握手经一次性临时 token（?token=）或 Bearer 头；
//   - 客户端消息协议：{op: ping|subscribe|unsubscribe|call, id, domain, topic, data}；
//   - 服务端事件：{op: "event", domain, event, topic, data}；
//   - 响应：{op: "response", id, ok, data|error}；
//   - 域模型：logs / maisaka_monitor / chat（Python 版 plugin_* 域裁剪）。
package webui

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"oh-my-qqbot/internal/webui/routers"
)

// 支持的订阅域/主题（对齐 unified.py 的 _handle_subscribe 白名单）。
const (
	DomainLogs           = "logs"
	DomainMaisakaMonitor = "maisaka_monitor"
	DomainChat           = "chat"
	TopicMain            = "main"
)

// WS 事件名（对齐 events.py emit_* 系列 + logs/监控快照事件）。
const (
	EventWSLogEntry      = "entry"
	EventWSLogSnapshot   = "snapshot"
	EventWSPlannerReset  = "planner.reset"
	EventWSStageSnapshot = "stage.snapshot"
	EventWSChatMessage   = "message"
)

// upgrader 是 gorilla WS 升级器（跨域来源由 CORS 中间件层控制，这里放宽
// CheckOrigin：升级前已有 token 鉴权兜底）。
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// WS 读写防护参数（S5：此前读循环无读上限/无读 deadline，慢连接或恶意客户端
// 可悬挂连接、放大内存）。可变量便于测试缩短周期。
var (
	// wsReadLimit 是单帧读上限（64KB：客户端协议消息只有 ping/subscribe 等
	// 小 JSON；超限直接断开，防超大帧内存放大）。
	wsReadLimit int64 = 64 << 10
	// wsReadWait 是读 deadline 容忍期：期内未收到任何消息/pong 即断开。
	wsReadWait = 60 * time.Second
	// wsPingPeriod 是服务端 ping 周期（须 < wsReadWait，留出 pong 往返余量）。
	wsPingPeriod = 50 * time.Second
)

// wsClient 是一条已认证连接。
type wsClient struct {
	id   string
	conn *websocket.Conn
	mu   sync.Mutex // 写锁（gorilla 并发写不安全）
	subs map[string]bool
	// sendCh 出站队列（写 pump 独占消费，避免慢消费者阻塞广播方）。
	sendCh chan []byte
	closed chan struct{}
	once   sync.Once
}

// WSHub 管理全部 WS 连接与订阅。
type WSHub struct {
	tm *TokenManager
	// logs 由 Server 装配时注入（logs 快照回放用；nil 时跳过回放）。
	logs logSource

	mu      sync.RWMutex
	clients map[string]*wsClient
	nextID  int64
}

// logSource 是日志环形缓冲的最小面（routers.LogService 满足）。
type logSource interface {
	Recent(limit int, level string) []routers.LogEntry
}

// NewWSHub 创建 hub。
func NewWSHub(tm *TokenManager) *WSHub {
	return &WSHub{tm: tm, clients: map[string]*wsClient{}}
}

// HandleWS 处理 /api/webui/ws 升级（对齐 websocket_endpoint）。
func (h *WSHub) HandleWS(w http.ResponseWriter, r *http.Request) {
	if !h.authenticate(r) {
		slog.Warn("统一 WebSocket 连接被拒绝：认证失败")
		http.Error(w, "认证失败，请重新登录", http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.nextID++
	id := "ws-" + itoa(int(h.nextID))
	client := &wsClient{
		id:     id,
		conn:   conn,
		subs:   map[string]bool{},
		sendCh: make(chan []byte, 64),
		closed: make(chan struct{}),
	}
	h.clients[id] = client
	h.mu.Unlock()
	slog.Info("统一 WebSocket 客户端已连接", "connection", id)

	go h.writePump(client)
	h.sendEvent(client, "system", "ready", "", map[string]any{
		"connection_id": id, "timestamp": float64(time.Now().UnixNano()) / 1e9,
	})
	h.readPump(client)
}

// HandleWSLogs 处理 /api/webui/ws/logs（对齐 logs_ws.py 的独立日志端点；
// 升级后自动订阅 logs:main 并回放最近 100 条）。
func (h *WSHub) HandleWSLogs(w http.ResponseWriter, r *http.Request) {
	if !h.authenticate(r) {
		slog.Warn("日志 WebSocket 连接被拒绝：认证失败")
		http.Error(w, "认证失败，请重新登录", http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.nextID++
	id := "wslog-" + itoa(int(h.nextID))
	client := &wsClient{
		id:     id,
		conn:   conn,
		subs:   map[string]bool{DomainLogs + ":" + TopicMain: true},
		sendCh: make(chan []byte, 64),
		closed: make(chan struct{}),
	}
	h.clients[id] = client
	h.mu.Unlock()
	slog.Info("日志 WebSocket 客户端已连接", "connection", id)

	go h.writePump(client)
	// 回放最近日志（对齐连接建立后立即发送历史日志）。
	h.replayLogs(client, 100)
	h.readPump(client)
}

// authenticate 校验握手：一次性临时 token（query ?token=，优先）或 Bearer 头
// （对齐 authenticate_websocket_connection：临时 token → cookie 的顺序；
// Go 版鉴权经 Bearer 头，cookie 语义裁剪）。
func (h *WSHub) authenticate(r *http.Request) bool {
	if h.tm == nil {
		return false
	}
	token := r.URL.Query().Get("token")
	if token != "" && h.tm.VerifyWSToken(token) {
		return true
	}
	// 降级：Bearer 头直连（开发调试用）。
	if bearer := bearerTokenOf(r); bearer != "" && h.tm.VerifyToken(bearer) {
		return true
	}
	return false
}

// bearerTokenOf 提取 Authorization: Bearer 头。
func bearerTokenOf(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		return auth[len(prefix):]
	}
	return ""
}

// configureReadGuard 为连接挂上 S5 读防护：单帧上限 + 读 deadline +
// pong 续期（收到 pong 即把读 deadline 重推 wsReadWait）。
func configureReadGuard(client *wsClient) {
	client.conn.SetReadLimit(wsReadLimit)
	client.conn.SetPongHandler(func(string) error {
		return client.conn.SetReadDeadline(time.Now().Add(wsReadWait))
	})
}

// readPump 读循环：解析客户端消息并分发（对齐 handle_client_message）。
// S5：每轮读之前重置读 deadline；对端存活由 ping/pong 续期，死连接
// （断网/无 pong）在 wsReadWait 内回收，超限帧直接断开。
func (h *WSHub) readPump(client *wsClient) {
	defer h.disconnect(client)
	configureReadGuard(client)
	for {
		_ = client.conn.SetReadDeadline(time.Now().Add(wsReadWait))
		_, raw, err := client.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				slog.Warn("WS 读连接异常断开", "connection", client.id, "err", err)
			}
			return
		}
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			h.sendResponse(client, nil, false, nil, map[string]any{
				"code": "invalid_message", "message": "消息必须是 JSON 对象",
			})
			continue
		}
		op := strOf(msg["op"])
		requestID, _ := msg["id"].(string)
		switch op {
		case "ping":
			h.sendPong(client, float64(time.Now().UnixNano())/1e9)
		case "subscribe":
			h.handleSubscribe(client, requestID, msg)
		case "unsubscribe":
			h.handleUnsubscribe(client, requestID, msg)
		default:
			h.sendResponse(client, requestID, false, nil, map[string]any{
				"code": "unsupported_operation", "message": "不支持的操作: " + op,
			})
		}
	}
}

// handleSubscribe 处理订阅（对齐 _handle_subscribe 白名单）。
func (h *WSHub) handleSubscribe(client *wsClient, requestID string, msg map[string]any) {
	domain := strOf(msg["domain"])
	topic := strOf(msg["topic"])
	if topic == "" {
		topic = TopicMain
	}
	data, _ := msg["data"].(map[string]any)
	switch domain {
	case DomainLogs:
		if topic != TopicMain {
			h.sendResponse(client, requestID, false, nil, unsupportedSubscription(domain, topic))
			return
		}
		h.subscribe(client, domain, topic)
		h.sendResponse(client, requestID, true, map[string]any{"domain": domain, "topic": topic}, nil)
		h.replayLogs(client, intOf(data["replay"], 100))
	case DomainMaisakaMonitor:
		if topic != TopicMain {
			h.sendResponse(client, requestID, false, nil, unsupportedSubscription(domain, topic))
			return
		}
		h.subscribe(client, domain, topic)
		h.sendResponse(client, requestID, true, map[string]any{"domain": domain, "topic": topic}, nil)
		// 对齐 maisaka_monitor 订阅流程：先发 planner.reset 标记。
		h.sendEvent(client, domain, EventWSPlannerReset, topic, map[string]any{})
	case DomainChat:
		if topic != TopicMain {
			h.sendResponse(client, requestID, false, nil, unsupportedSubscription(domain, topic))
			return
		}
		h.subscribe(client, domain, topic)
		h.sendResponse(client, requestID, true, map[string]any{"domain": domain, "topic": topic}, nil)
	default:
		h.sendResponse(client, requestID, false, nil, unsupportedSubscription(domain, topic))
	}
}

func unsupportedSubscription(domain, topic string) map[string]any {
	return map[string]any{"code": "unsupported_subscription", "message": "不支持的订阅目标: " + domain + ":" + topic}
}

// handleUnsubscribe 处理退订。
func (h *WSHub) handleUnsubscribe(client *wsClient, requestID string, msg map[string]any) {
	domain := strOf(msg["domain"])
	topic := strOf(msg["topic"])
	if domain == "" || topic == "" {
		h.sendResponse(client, requestID, false, nil, map[string]any{
			"code": "invalid_unsubscribe", "message": "退订请求缺少 domain 或 topic",
		})
		return
	}
	h.unsubscribe(client, domain, topic)
	h.sendResponse(client, requestID, true, map[string]any{"domain": domain, "topic": topic}, nil)
}

// subscribe 登记订阅。
func (h *WSHub) subscribe(client *wsClient, domain, topic string) {
	client.mu.Lock()
	client.subs[domain+":"+topic] = true
	client.mu.Unlock()
}

// unsubscribe 取消订阅。
func (h *WSHub) unsubscribe(client *wsClient, domain, topic string) {
	client.mu.Lock()
	delete(client.subs, domain+":"+topic)
	client.mu.Unlock()
}

// replayLogs 发送最近日志快照（对齐 send_event(logs, "snapshot", ...)）。
func (h *WSHub) replayLogs(client *wsClient, limit int) {
	if h.logs == nil || limit <= 0 {
		return
	}
	entries := h.logs.Recent(limit, "")
	h.sendEvent(client, DomainLogs, EventWSLogSnapshot, TopicMain, map[string]any{"entries": entries})
}

// Broadcast 实现 routers.Broadcaster：向订阅了 domain:topic 的全部连接广播。
func (h *WSHub) Broadcast(domain, topic, event string, data any) {
	if topic == "" {
		topic = TopicMain
	}
	key := domain + ":" + topic
	payload, err := json.Marshal(map[string]any{
		"op": "event", "domain": domain, "event": event, "topic": topic, "data": data,
	})
	if err != nil {
		return
	}
	h.mu.RLock()
	targets := make([]*wsClient, 0, len(h.clients))
	for _, c := range h.clients {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.mu.Lock()
		subscribed := c.subs[key]
		c.mu.Unlock()
		if subscribed {
			h.enqueue(c, payload)
		}
	}
}

// sendEvent 发送统一事件给单连接。
func (h *WSHub) sendEvent(client *wsClient, domain, event, topic string, data any) {
	payload, err := json.Marshal(map[string]any{
		"op": "event", "domain": domain, "event": event, "data": data,
	})
	if err != nil {
		return
	}
	if topic != "" {
		msg := map[string]any{
			"op": "event", "domain": domain, "event": event, "topic": topic, "data": data,
		}
		payload, err = json.Marshal(msg)
		if err != nil {
			return
		}
	}
	h.enqueue(client, payload)
}

// sendResponse 发送统一响应。
func (h *WSHub) sendResponse(client *wsClient, requestID any, okFlag bool, data, errObj map[string]any) {
	msg := map[string]any{"op": "response", "id": requestID, "ok": okFlag}
	if data != nil {
		msg["data"] = data
	}
	if errObj != nil {
		msg["error"] = errObj
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.enqueue(client, payload)
}

// sendPong 心跳响应。
func (h *WSHub) sendPong(client *wsClient, ts float64) {
	payload, err := json.Marshal(map[string]any{"op": "pong", "ts": ts})
	if err != nil {
		return
	}
	h.enqueue(client, payload)
}

// enqueue 入出站队列（满则丢弃，防慢消费者）。
func (h *WSHub) enqueue(client *wsClient, payload []byte) {
	select {
	case client.sendCh <- payload:
	case <-client.closed:
	default:
		slog.Warn("WS 出站队列已满，丢弃消息", "connection", client.id)
	}
}

// writePump 写循环。S5：按 wsPingPeriod 周期发 ping，配合读侧 pong 续期
// 维持心跳（WriteControl 允许与其他写并发，仍取写锁保持串行）。
func (h *WSHub) writePump(client *wsClient) {
	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()
	for {
		select {
		case payload := <-client.sendCh:
			client.mu.Lock()
			err := client.conn.WriteMessage(websocket.TextMessage, payload)
			client.mu.Unlock()
			if err != nil {
				h.disconnect(client)
				return
			}
		case <-ticker.C:
			client.mu.Lock()
			err := client.conn.WriteControl(websocket.PingMessage, nil,
				time.Now().Add(10*time.Second))
			client.mu.Unlock()
			if err != nil {
				h.disconnect(client)
				return
			}
		case <-client.closed:
			return
		}
	}
}

// disconnect 清理连接（对齐 finally 分支）。
func (h *WSHub) disconnect(client *wsClient) {
	client.once.Do(func() {
		close(client.closed)
		_ = client.conn.Close()
		h.mu.Lock()
		delete(h.clients, client.id)
		count := len(h.clients)
		h.mu.Unlock()
		slog.Info("统一 WebSocket 客户端已断开", "connection", client.id, "remaining", count)
	})
}

// ClientCount 返回当前连接数（诊断/测试用）。
func (h *WSHub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// strOf 取字符串字段。
func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// intOf 取整型字段（支持 float64/int/string）。
func intOf(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		var out int
		if _, err := sscanfInt(n, &out); err == nil {
			return out
		}
	}
	return def
}

// sscanfInt 极简整数解析（避免 strconv 导入散落）。
func sscanfInt(s string, out *int) (bool, error) {
	n := 0
	neg := false
	for i, ch := range s {
		if i == 0 && ch == '-' {
			neg = true
			continue
		}
		if ch < '0' || ch > '9' {
			return false, errParseInt
		}
		n = n*10 + int(ch-'0')
	}
	if neg {
		n = -n
	}
	*out = n
	return true, nil
}

type constErr string

func (e constErr) Error() string { return string(e) }

const errParseInt = constErr("not an int")

// logsField 是 hub 关联的日志服务（Server 装配时注入；用于 logs 快照回放）。
// 放在文件尾以贴近使用点；由 Server.wireHubLogs 赋值。
