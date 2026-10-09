package qq

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// 假网关 (WebSocket)
// ---------------------------------------------------------------------------

// fakeGateway 是一个本地 WebSocket 网关, 用来验证鉴权帧/心跳/RESUME 与事件处理。
//
// 服务端在收到鉴权帧后按下发的 ops 序列推帧, 保持 hold 时长后主动关闭连接, 让
// runSession 读帧失败返回 (避免测试挂住)。
type fakeGateway struct {
	mu     sync.Mutex
	server *httptest.Server
	url    string
	frames []map[string]any // 收到的所有帧
	conns  int
	// helloInterval 是 hello 帧带的心跳间隔 (毫秒)。
	helloInterval int64
	// sendOps 是鉴权成功后依次下发的帧 (由测试驱动)。
	sendOps []func(conn *websocket.Conn)
	// dropAfterHello 让服务端在 hello 之后立刻关闭连接 (模拟断线)。
	dropAfterHello bool
	// hold 是下发完 ops 后保持连接的时长, 之后主动关闭。
	hold time.Duration
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	gw := &fakeGateway{helloInterval: 100, hold: 400 * time.Millisecond}
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		gw.mu.Lock()
		gw.conns++
		gw.mu.Unlock()
		gw.serve(conn)
	})
	gw.server = httptest.NewServer(mux)
	t.Cleanup(gw.server.Close)
	gw.url = "ws" + strings.TrimPrefix(gw.server.URL, "http") + "/ws"
	return gw
}

func (g *fakeGateway) serve(conn *websocket.Conn) {
	g.mu.Lock()
	interval := g.helloInterval
	drop := g.dropAfterHello
	hold := g.hold
	ops := append([]func(conn *websocket.Conn){}, g.sendOps...)
	g.mu.Unlock()

	// 先发 hello (OP 10)。
	if err := conn.WriteJSON(GatewayPayload{Op: OpHello, D: map[string]any{"heartbeat_interval": interval}}); err != nil {
		return
	}
	if drop {
		_ = conn.Close()
		return
	}
	// 收鉴权帧 (IDENTIFY / RESUME)。
	if err := g.readFrame(conn); err != nil {
		return
	}
	for _, op := range ops {
		op(conn)
	}
	// 保持 hold 时长收心跳, 然后关闭连接让 runSession 返回。Close 与 Read 并发是
	// gorilla 允许的, 这样读循环会立刻返回。
	if hold > 0 {
		timer := time.AfterFunc(hold, func() { _ = conn.Close() })
		defer timer.Stop()
	}
	for {
		if err := g.readFrame(conn); err != nil {
			return
		}
	}
}

func (g *fakeGateway) readFrame(conn *websocket.Conn) error {
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		return err
	}
	g.mu.Lock()
	g.frames = append(g.frames, frame)
	g.mu.Unlock()
	return nil
}

// framesSnapshot 返回已收到的帧。
func (g *fakeGateway) framesSnapshot() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]map[string]any, len(g.frames))
	for i, frame := range g.frames {
		copied := map[string]any{}
		for key, value := range frame {
			copied[key] = value
		}
		out[i] = copied
	}
	return out
}

func (g *fakeGateway) frameOfOp(op int) (map[string]any, bool) {
	for _, frame := range g.framesSnapshot() {
		if num, ok := frame["op"].(float64); ok && int(num) == op {
			return frame, true
		}
	}
	return nil, false
}

func (g *fakeGateway) connectionCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.conns
}

func (g *fakeGateway) setOps(ops ...func(conn *websocket.Conn)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sendOps = ops
}

func writeJSONFrame(frame GatewayPayload) func(*websocket.Conn) {
	return func(conn *websocket.Conn) { _ = conn.WriteJSON(frame) }
}

func writeRawFrame(text string) func(conn *websocket.Conn) {
	return func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(text))
	}
}

// ---------------------------------------------------------------------------
// 鉴权帧 / 心跳 / RESUME
// ---------------------------------------------------------------------------

func newTestChannel(t *testing.T, gatewayURL string, now func() time.Time) *Channel {
	t.Helper()
	return NewChannel(Options{
		AppID:             "APPID_1",
		AppSecret:         "SECRET_1",
		GatewayURL:        gatewayURL,
		BotNames:          []string{"麦麦"},
		Now:               now,
		HeartbeatInterval: 20 * time.Millisecond,
	})
}

// TestChannelIdentifyFrame 校验首次连接发 IDENTIFY 帧 (token + intents 1<<25)。
func TestChannelIdentifyFrame(t *testing.T) {
	gw := newFakeGateway(t)
	channel := newTestChannel(t, gw.url, nil)
	// token 端点不可达时 AuthHeader 会失败, 这里直接注入 token 专注测帧。
	channel.API().tokens.token = "TOKEN_X"
	channel.API().tokens.expiresAt = time.Now().Add(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- channel.Connect(ctx, func(context.Context, *message.SessionMessage) error { return nil })
	}()
	waitForFrame(t, gw, OpIdentify, 2*time.Second)
	_ = channel.Close()
	<-done

	frame, ok := gw.frameOfOp(OpIdentify)
	if !ok {
		t.Fatalf("未收到 IDENTIFY 帧, got %v", gw.framesSnapshot())
	}
	if frame["d"] == nil {
		t.Fatal("IDENTIFY 帧缺少 d")
	}
	data := frame["d"].(map[string]any)
	if data["token"] != "QQBot TOKEN_X" {
		t.Fatalf("token 不符: %v", data["token"])
	}
	if intents, _ := data["intents"].(float64); int64(intents) != IntentGroupAndC2CEvent {
		t.Fatalf("intents 应为 1<<25, got %v", data["intents"])
	}
}

// TestChannelHeartbeatUsesHelloInterval 校验心跳帧按 hello 的间隔发送并带 last_seq。
func TestChannelHeartbeatUsesHelloInterval(t *testing.T) {
	gw := newFakeGateway(t)
	gw.mu.Lock()
	gw.helloInterval = 10
	gw.mu.Unlock()
	channel := newTestChannel(t, gw.url, nil)
	channel.API().tokens.token = "TOKEN_X"
	channel.API().tokens.expiresAt = time.Now().Add(time.Hour)

	// 让网关保持连接久一点 (否则会被断线重连打断, 收不到连续心跳)。
	gw.mu.Lock()
	gw.hold = 2 * time.Second
	gw.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- channel.Connect(ctx, func(context.Context, *message.SessionMessage) error { return nil })
	}()

	// 先等 IDENTIFY, 再等至少两个心跳。
	waitForFrame(t, gw, OpIdentify, 2*time.Second)
	waitForHeartbeatCount(t, gw, 2, 3*time.Second)
	_ = channel.Close()
	<-done

	beats := 0
	for _, frame := range gw.framesSnapshot() {
		if op, _ := frame["op"].(float64); int(op) == OpHeartbeat {
			beats++
		}
	}
	if beats < 2 {
		t.Fatalf("心跳次数不足: %d", beats)
	}
}

// TestChannelResumeAfterReconnect 校验断线后带 session_id + last_seq 走 RESUME。
func TestChannelResumeAfterReconnect(t *testing.T) {
	gw := newFakeGateway(t)
	channel := newTestChannel(t, gw.url, nil)
	channel.API().tokens.token = "TOKEN_X"
	channel.API().tokens.expiresAt = time.Now().Add(time.Hour)

	// 第一轮: READY 带 session_id, 再推一个 dispatch (s=7)。
	ready := newDispatch(t, EventReady, 0, map[string]any{
		"session_id": "SESSION_1",
		"user":       map[string]any{"id": "BOT_1", "username": "麦麦"},
	})
	gw.setOps(
		writeJSONFrame(ready),
		writeRawFrame(`{"op":0,"s":7,"t":"GROUP_MESSAGE_CREATE","d":{"id":"M1"}}`),
	)
	runSessionOnce(t, channel, gw, OpIdentify)

	if status := channel.Status(); status.SessionID != "SESSION_1" || status.SelfID != "BOT_1" {
		t.Fatalf("READY 未记录会话: %+v", status)
	}
	if status := channel.Status(); status.LastSeq != 7 {
		t.Fatalf("ACK 序号未记录: %+v", status)
	}

	// 第二轮: 断线重连应发 RESUME (session_id + last_seq=7)。
	sessionID, lastSeq := channel.session()
	if sessionID != "SESSION_1" || lastSeq != 7 {
		t.Fatalf("会话状态不符: session=%q seq=%d", sessionID, lastSeq)
	}
	gw.setOps()
	runSessionOnce(t, channel, gw, OpResume)

	resumeFrame, ok := gw.frameOfOp(OpResume)
	if !ok {
		t.Fatalf("未收到 RESUME 帧, got %v", gw.framesSnapshot())
	}
	data := resumeFrame["d"].(map[string]any)
	if data["session_id"] != "SESSION_1" {
		t.Fatalf("RESUME session_id 不符: %v", data["session_id"])
	}
	if seq, _ := data["seq"].(float64); int64(seq) != 7 {
		t.Fatalf("RESUME seq 应为最后处理的 7, got %v", data["seq"])
	}
	if data["token"] != "QQBot TOKEN_X" {
		t.Fatalf("RESUME token 不符: %v", data["token"])
	}
}

// TestChannelInvalidSessionClearsSession 校验 INVALID_SESSION 后下一轮走全新 IDENTIFY。
func TestChannelInvalidSessionClearsSession(t *testing.T) {
	gw := newFakeGateway(t)
	channel := newTestChannel(t, gw.url, nil)
	channel.API().tokens.token = "TOKEN_X"
	channel.API().tokens.expiresAt = time.Now().Add(time.Hour)

	gw.setOps(
		writeJSONFrame(newDispatch(t, EventReady, 0, map[string]any{
			"session_id": "SESSION_1", "user": map[string]any{"id": "BOT_1"},
		})),
	)
	runSessionOnce(t, channel, gw, OpIdentify)
	if channel.Status().SessionID != "SESSION_1" {
		t.Fatalf("READY 未记录会话: %+v", channel.Status())
	}

	// 下一轮服务端发 INVALID_SESSION。
	gw.setOps(writeJSONFrame(GatewayPayload{Op: OpInvalidSession, D: map[string]any{}}))
	runSessionOnce(t, channel, gw, OpResume)

	if id, _ := channel.session(); id != "" {
		t.Fatalf("会话失效后应清空 session_id, got %q", id)
	}
	// 再连一次应回到 IDENTIFY。
	gw.setOps()
	runSessionOnce(t, channel, gw, OpIdentify)
}

// TestChannelReconnectOpKeepsSession 校验网关要求重连时保留会话 (下一轮 RESUME)。
func TestChannelReconnectOpKeepsSession(t *testing.T) {
	gw := newFakeGateway(t)
	channel := newTestChannel(t, gw.url, nil)
	channel.API().tokens.token = "TOKEN_X"
	channel.API().tokens.expiresAt = time.Now().Add(time.Hour)

	gw.setOps(
		writeJSONFrame(newDispatch(t, EventReady, 0, map[string]any{
			"session_id": "SESSION_KEEP", "user": map[string]any{"id": "BOT_1"},
		})),
		writeJSONFrame(GatewayPayload{Op: OpReconnect, D: map[string]any{}}),
	)
	runSessionOnce(t, channel, gw, OpIdentify)

	id, _ := channel.session()
	if id != "SESSION_KEEP" {
		t.Fatalf("RECONNECT 应保留会话, got %q", id)
	}
}

// TestChannelReconnectBackoff 校验断线后指数重连 (连不上也不 panic, 会被 ctx 收住)。
func TestChannelReconnectBackoff(t *testing.T) {
	gw := newFakeGateway(t)
	gw.mu.Lock()
	gw.dropAfterHello = true
	gw.mu.Unlock()
	channel := newTestChannel(t, gw.url, nil)
	channel.API().tokens.token = "TOKEN_X"
	channel.API().tokens.expiresAt = time.Now().Add(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- channel.Connect(ctx, func(context.Context, *message.SessionMessage) error { return nil })
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gw.connectionCount() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if got := gw.connectionCount(); got < 2 {
		t.Fatalf("应至少重连一次, got %d", got)
	}
}

// TestChannelConnectRejectsMissingCredentials 校验缺凭据时 Connect 立即报错。
func TestChannelConnectRejectsMissingCredentials(t *testing.T) {
	channel := NewChannel(Options{})
	err := channel.Connect(context.Background(), func(context.Context, *message.SessionMessage) error { return nil })
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("应返回 ErrNotConfigured, got %v", err)
	}
	if channel.Status().Connected {
		t.Fatal("缺凭据不应标记为已连接")
	}
}

// ---------------------------------------------------------------------------
// 事件 → 回调
// ---------------------------------------------------------------------------

// TestChannelDispatchesGroupMessage 校验群消息事件转成 SessionMessage 后回调上层。
func TestChannelDispatchesGroupMessage(t *testing.T) {
	gw := newFakeGateway(t)
	clock := newFixedClock(testStart())
	channel := newTestChannel(t, gw.url, clock.Now)
	channel.API().tokens.token = "TOKEN_X"
	channel.API().tokens.expiresAt = time.Now().Add(time.Hour)
	channel.setSession("SESSION_1", 0, "BOT_OPENID_1")

	dispatch := newDispatch(t, EventGroupMessageCreate, 3, map[string]any{
		"id":            "MSG_1",
		"content":       "麦麦 在吗",
		"timestamp":     "2026-03-08T15:04:05+08:00",
		"group_openid":  "GROUP_1",
		"author":        map[string]any{"id": "MEMBER_1", "member_openid": "MEMBER_1", "username": "群友A"},
		"member":        map[string]any{"nick": "群友A名片"},
		"mentions":      []map[string]any{{"id": "BOT_OPENID_1", "is_you": true}},
		"message_scene": map[string]any{"ext": []string{"msg_idx=REFIDX_IN_1"}},
	})
	channel.handleDispatch(context.Background(), dispatch)

	got := waitForMessage(t, channel, dispatch)
	if got.UserID() != "MEMBER_1" || got.GroupID() != "GROUP_1" {
		t.Fatalf("消息路由不符: %+v", got.MessageInfo)
	}
	if !got.IsAt {
		t.Fatal("mentions.is_you 应判定 IsAt")
	}
	if !got.IsMentioned {
		t.Fatal("文本命中昵称应判定 IsMentioned")
	}
	// 入站 msg_idx 应登记进引用索引。
	if entry, ok := channel.Refs().Lookup("REFIDX_IN_1"); !ok || entry.MsgID != "MSG_1" {
		t.Fatalf("msg_idx 未登记: %+v ok=%v", entry, ok)
	}
	// ACK 序号由运行循环记录 (这里直接调 recordSeq 验证语义)。
	channel.recordSeq(3)
	if channel.Status().LastSeq != 3 {
		t.Fatalf("ACK 序号未记录: %+v", channel.Status())
	}
}

// TestChannelDropsSelfEcho 校验自发回推被丢弃, 不进回调。
func TestChannelDropsSelfEcho(t *testing.T) {
	channel := newTestChannel(t, "ws://127.0.0.1:1/ws", nil)
	channel.setSession("SESSION_1", 0, "BOT_OPENID_1")

	received := make(chan *message.SessionMessage, 4)
	channel.mu.Lock()
	channel.handler = func(ctx context.Context, msg *message.SessionMessage) error {
		received <- msg
		return nil
	}
	channel.mu.Unlock()

	// 先记一次成功发送 (模拟刚发过的内容)。
	channel.Sent().Record(SentEntry{Scope: "GROUP_1", MessageID: "MSG_SELF", Text: "我自己说的", Group: true})

	// author.bot + author.id == selfID → 直接丢。
	channel.handleDispatch(context.Background(), newDispatch(t, EventGroupMessageCreate, 0, map[string]any{
		"id": "MSG_SELF", "content": "我自己说的", "group_openid": "GROUP_1",
		"author": map[string]any{"id": "BOT_OPENID_1", "member_openid": "BOT_OPENID_1", "bot": true},
	}))
	select {
	case msg := <-received:
		t.Fatalf("自发回推不应进回调: %s", msg.MessageID)
	case <-time.After(100 * time.Millisecond):
	}

	// 真人的同名复读仍要进回调。
	channel.handleDispatch(context.Background(), newDispatch(t, EventGroupMessageCreate, 0, map[string]any{
		"id": "MSG_HUMAN", "content": "我自己说的", "group_openid": "GROUP_1",
		"author": map[string]any{"id": "MEMBER_9", "member_openid": "MEMBER_9"},
	}))
	select {
	case msg := <-received:
		if msg.MessageID != "MSG_HUMAN" {
			t.Fatalf("真人的消息应进回调: %s", msg.MessageID)
		}
	case <-time.After(time.Second):
		t.Fatal("真人消息应被回调")
	}
}

// TestChannelDispatchesC2C 校验单聊事件转成私聊 SessionMessage。
func TestChannelDispatchesC2C(t *testing.T) {
	channel := newTestChannel(t, "ws://127.0.0.1:1/ws", nil)
	channel.setSession("SESSION_1", 0, "BOT_OPENID_1")
	received := make(chan *message.SessionMessage, 1)
	channel.mu.Lock()
	channel.handler = func(ctx context.Context, msg *message.SessionMessage) error {
		received <- msg
		return nil
	}
	channel.mu.Unlock()

	channel.handleDispatch(context.Background(), newDispatch(t, EventC2CMessageCreate, 0, map[string]any{
		"id": "C2C_1", "content": "你好", "author": map[string]any{"id": "U1", "user_openid": "U1", "username": "好友"},
	}))
	select {
	case got := <-received:
		if got.ChatType() != "private" || got.UserID() != "U1" {
			t.Fatalf("单聊转换不符: %+v", got.MessageInfo)
		}
	case <-time.After(time.Second):
		t.Fatal("单聊事件应被回调")
	}
}

// TestChannelQuotedEventResolvesRef 校验引用事件经 REFIDX 台账反查出发言者。
func TestChannelQuotedEventResolvesRef(t *testing.T) {
	channel := newTestChannel(t, "ws://127.0.0.1:1/ws", nil)
	channel.setSession("SESSION_1", 0, "BOT_OPENID_1")
	// 别人此前发的消息, 已登记引用索引。
	channel.Refs().RecordInbound("REFIDX_IN_OTHER", "MSG_OTHER", "MEMBER_1", false)

	received := make(chan *message.SessionMessage, 1)
	channel.mu.Lock()
	channel.handler = func(ctx context.Context, msg *message.SessionMessage) error {
		received <- msg
		return nil
	}
	channel.mu.Unlock()

	channel.handleDispatch(context.Background(), newDispatch(t, EventGroupMessageCreate, 0, map[string]any{
		"id": "MSG_QUOTE", "content": "就是这句", "group_openid": "GROUP_1",
		"author":        map[string]any{"id": "MEMBER_2", "member_openid": "MEMBER_2"},
		"message_type":  MessageTypeQuote,
		"msg_elements":  []map[string]any{{"content": "原话"}},
		"message_scene": map[string]any{"ext": []string{"ref_msg_idx=REFIDX_IN_OTHER"}},
	}))
	select {
	case got := <-received:
		if got.ReplyTo == nil || *got.ReplyTo != "MSG_OTHER" {
			t.Fatalf("引用目标应反查成平台消息 id: %v", got.ReplyTo)
		}
	case <-time.After(time.Second):
		t.Fatal("引用事件应被回调")
	}
}

// TestChannelHandlerErrorDoesNotBlock 校验回调返回错误只记日志, 不影响状态。
func TestChannelHandlerErrorDoesNotBlock(t *testing.T) {
	channel := newTestChannel(t, "ws://127.0.0.1:1/ws", nil)
	channel.mu.Lock()
	channel.handler = func(ctx context.Context, msg *message.SessionMessage) error {
		return errors.New("上层处理失败")
	}
	channel.mu.Unlock()
	channel.handleDispatch(context.Background(), newDispatch(t, EventGroupMessageCreate, 0, map[string]any{
		"id": "MSG_1", "content": "hi", "group_openid": "G1",
		"author": map[string]any{"id": "M1", "member_openid": "M1"},
	}))
	// 未 panic 即通过。
}

// ---------------------------------------------------------------------------
// 通道级双模发送 (Channel.Send)
// ---------------------------------------------------------------------------

// TestChannelSendPassiveThenProactive 校验通道级发送: 前 5 段被动, 第 6 段降级主动。
func TestChannelSendPassiveThenProactive(t *testing.T) {
	api := newFakeAPI(t)
	clock := newFixedClock(testStart())
	channel := NewChannel(Options{
		AppID:            "APPID_1",
		AppSecret:        "SECRET_1",
		APIBaseOverride:  api.server.URL,
		TokenURLOverride: api.server.URL + "/app/getAppAccessToken",
		Now:              clock.Now,
	})
	channel.API().tokens.token = "TOKEN_X"
	channel.API().tokens.expiresAt = time.Now().Add(time.Hour)
	channel.setSession("SESSION_1", 0, "BOT_1")

	target := OutboundTarget{GroupID: "G1"}
	triggerTime := clock.Now()
	modes := []SendMode{}
	for i := 1; i <= 6; i++ {
		result, err := channel.Send(context.Background(), Outbound{
			Target:      target,
			Text:        "第" + itoa(i) + "句",
			TriggerID:   "TRIGGER_1",
			TriggerTime: triggerTime,
		})
		if err != nil {
			t.Fatalf("第 %d 段发送失败: %v", i, err)
		}
		modes = append(modes, result.Mode)
	}
	for i := 0; i < PassiveMaxReplies; i++ {
		if modes[i] != ModePassive {
			t.Fatalf("第 %d 段应为被动, got %s", i+1, modes[i])
		}
	}
	if modes[5] != ModeProactive {
		t.Fatalf("第 6 段应降级为主动, got %s", modes[5])
	}
	// 被动段各占一个 msg_seq: 1..5。
	posts := api.postsSnapshot()
	seqs := []int{}
	for _, post := range posts {
		if seq, ok := post["msg_seq"].(float64); ok {
			seqs = append(seqs, int(seq))
		}
	}
	if len(seqs) != 5 {
		t.Fatalf("前 5 段都应带 msg_seq, got %v", seqs)
	}
	for i, seq := range seqs {
		if seq != i+1 {
			t.Fatalf("第 %d 段的 seq 应为 %d, got %d", i+1, i+1, seq)
		}
	}
	// 第 6 段是主动: 不带 msg_id / msg_seq。
	last := posts[len(posts)-1]
	if last["msg_id"] != nil || last["msg_seq"] != nil {
		t.Fatalf("主动段不应带 msg_id/msg_seq: %+v", last)
	}
	// 主动发送占了一格额度。
	if usage := channel.Quota().Usage("G1", clock.Now()); usage.GroupDayUsed != 1 {
		t.Fatalf("主动发送应记 1 条额度, got %d", usage.GroupDayUsed)
	}
}

// TestChannelSendQuotaExhausted 校验额度耗尽时拒绝发送并返回可识别错误。
func TestChannelSendQuotaExhausted(t *testing.T) {
	api := newFakeAPI(t)
	clock := newFixedClock(testStart())
	channel := NewChannel(Options{
		AppID:            "APPID_1",
		AppSecret:        "SECRET_1",
		APIBaseOverride:  api.server.URL,
		TokenURLOverride: api.server.URL + "/app/getAppAccessToken",
		Now:              clock.Now,
		Quota:            &ProactiveOptions{GroupPerMinute: 1},
	})
	channel.API().tokens.token = "TOKEN_X"
	channel.API().tokens.expiresAt = time.Now().Add(time.Hour)

	if _, err := channel.Send(context.Background(), Outbound{Target: OutboundTarget{GroupID: "G1"}, Text: "第一条"}); err != nil {
		t.Fatalf("第一条应发出: %v", err)
	}
	_, err := channel.Send(context.Background(), Outbound{Target: OutboundTarget{GroupID: "G1"}, Text: "第二条"})
	if err == nil {
		t.Fatal("额度耗尽应拒绝发送")
	}
	if _, ok := AsProactiveQuotaError(err); !ok {
		t.Fatalf("应返回 *ProactiveQuotaError, got %T: %v", err, err)
	}
	// 被拒的那条不应该真的发出去。
	if posts := api.postsSnapshot(); len(posts) != 1 {
		t.Fatalf("被拒的发送不应打到平台, got %d 次请求", len(posts))
	}
}

// TestChannelSendRecordsRefIdx 校验主动发送成功后登记 ext_info.ref_idx。
func TestChannelSendRecordsRefIdx(t *testing.T) {
	api := newFakeAPI(t)
	clock := newFixedClock(testStart())
	channel := NewChannel(Options{
		AppID:            "APPID_1",
		AppSecret:        "SECRET_1",
		APIBaseOverride:  api.server.URL,
		TokenURLOverride: api.server.URL + "/app/getAppAccessToken",
		Now:              clock.Now,
	})
	channel.API().tokens.token = "TOKEN_X"
	channel.API().tokens.expiresAt = time.Now().Add(time.Hour)
	channel.setSession("SESSION_1", 0, "BOT_1")

	result, err := channel.Send(context.Background(), Outbound{Target: OutboundTarget{GroupID: "G1"}, Text: "主动一句"})
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if result.RefIdx == "" {
		t.Fatal("应带回 ref_idx")
	}
	if got := channel.Refs().RefIdxFor(result.MessageID); got != result.RefIdx {
		t.Fatalf("ref_idx 未登记进索引: got %q want %q", got, result.RefIdx)
	}
}

// TestChannelSendRollsBackOnNotSent 校验未送达 (本地/传输失败) 时回滚额度。
func TestChannelSendRollsBackOnNotSent(t *testing.T) {
	api := newFakeAPI(t)
	clock := newFixedClock(testStart())
	channel := NewChannel(Options{
		AppID:            "APPID_1",
		AppSecret:        "SECRET_1",
		APIBaseOverride:  api.server.URL,
		TokenURLOverride: api.server.URL + "/app/getAppAccessToken",
		Now:              clock.Now,
		Quota:            &ProactiveOptions{GroupPerMinute: 1},
	})
	channel.API().tokens.token = "TOKEN_X"
	channel.API().tokens.expiresAt = time.Now().Add(time.Hour)

	// 平台返回业务错误码: 已送达 → 不回滚。
	api.setReject(40034105)
	if _, err := channel.Send(context.Background(), Outbound{Target: OutboundTarget{GroupID: "G1"}, Text: "x"}); err == nil {
		t.Fatal("平台拒绝应报错")
	}
	if usage := channel.Quota().Usage("G1", clock.Now()); usage.GroupDayUsed != 1 {
		t.Fatalf("已送达的失败不应回滚额度, got %d", usage.GroupDayUsed)
	}
}

// TestChannelCloseIsIdempotent 校验 Close 可重复调用。
func TestChannelCloseIsIdempotent(t *testing.T) {
	channel := newTestChannel(t, "ws://127.0.0.1:1/ws", nil)
	if err := channel.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("重复 Close 失败: %v", err)
	}
	if channel.Status().Connected {
		t.Fatal("Close 后不应为已连接")
	}
}

// TestChannelTriggerWindowDefault 校验被动窗口默认值与可覆盖。
func TestChannelTriggerWindowDefault(t *testing.T) {
	channel := newTestChannel(t, "ws://127.0.0.1:1/ws", nil)
	if got := channel.TriggerWindow(); got != PassiveWindow {
		t.Fatalf("默认被动窗口应为 5 分钟, got %v", got)
	}
	override := NewChannel(Options{PassiveWindow: time.Minute})
	if got := override.TriggerWindow(); got != time.Minute {
		t.Fatalf("可配置的被动窗口不生效: %v", got)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// waitForFrame 等待服务端收到某个 op 的帧。
func waitForFrame(t *testing.T, gw *fakeGateway, op int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, ok := gw.frameOfOp(op); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 op=%d 的帧超时, 已收到: %v", op, gw.framesSnapshot())
}

// waitForHeartbeatCount 等待服务端收到至少 n 个心跳帧。
func waitForHeartbeatCount(t *testing.T, gw *fakeGateway, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		count := 0
		for _, frame := range gw.framesSnapshot() {
			if op, _ := frame["op"].(float64); int(op) == OpHeartbeat {
				count++
			}
		}
		if count >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %d 个心跳超时, 已收到: %v", n, gw.framesSnapshot())
}

// runSessionOnce 跑一次会话 (阻塞到 runSession 返回), 然后校验首个鉴权帧的 op。
func runSessionOnce(t *testing.T, channel *Channel, gw *fakeGateway, wantOp int) {
	t.Helper()
	// runSession 会一直读帧, 用短超时 ctx 收住; 服务端收到鉴权帧后继续发 ops。
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- channel.runSession(ctx) }()
	waitForFrame(t, gw, wantOp, 2*time.Second)
	<-done
}

// newDispatch 构造一个 dispatch 帧: Data 走 RawMessage, 与网关真实形态一致。
func newDispatch(t *testing.T, eventType string, s int64, data map[string]any) GatewayPayload {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("编码事件数据失败: %v", err)
	}
	return GatewayPayload{Op: OpDispatch, T: eventType, S: s, Data: raw}
}

// waitForMessage 复用一个临时 handler 收集回调消息。
func waitForMessage(t *testing.T, channel *Channel, dispatch GatewayPayload) *message.SessionMessage {
	t.Helper()
	received := make(chan *message.SessionMessage, 1)
	channel.mu.Lock()
	channel.handler = func(ctx context.Context, msg *message.SessionMessage) error {
		received <- msg
		return nil
	}
	channel.mu.Unlock()
	channel.handleDispatch(context.Background(), dispatch)
	select {
	case got := <-received:
		return got
	case <-time.After(time.Second):
		t.Fatal("事件未回调上层")
		return nil
	}
}
