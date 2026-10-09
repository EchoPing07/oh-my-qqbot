package qq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// WS 网关 (§6.12 channel.go)
//
//   - 鉴权: token + intents (GROUP_AND_C2C_EVENT = 1<<25)
//   - 心跳: 按 hello 帧的 heartbeat_interval, 带最后处理的 seq
//   - ACK: 记录每个 dispatch 的 s (last_seq), 供 RESUME 与心跳使用
//   - RECONNECT → RESUME (session_id + last_seq); INVALID_SESSION → 全新 IDENTIFY
//   - 断线指数重连 (1s → 30s 封顶)
//   - 事件解析 → message.SessionMessage → 经回调分发给上层
//
// 回调注入, 不 import chat/config。
// ---------------------------------------------------------------------------

const (
	// defaultHeartbeat 是 hello 帧缺失/无效时的心跳间隔兜底。
	defaultHeartbeat = 30 * time.Second
	// heartbeatSlop 是心跳间隔的下限保护, 防止网关给了一个极小值把连接打满。
	heartbeatSlop = 5 * time.Second
	// reconnectBase 是重连退避的起始间隔。
	reconnectBase = time.Second
	// reconnectMax 是重连退避的上限。
	reconnectMax = 30 * time.Second
	// readyTimeout 是等待 READY / RESUMED 的超时。
	readyTimeout = 15 * time.Second
	// readLimit 是单帧读取上限。
	readLimit = 4 << 20
)

// ErrNotConfigured 是凭据缺失时 Connect 返回的错误。
var ErrNotConfigured = errors.New("qq: AppID 和 AppSecret 都必须配置")

// Options 配置 Channel (包内配置, 不引入 internal/config)。
type Options struct {
	// AppID / AppSecret 是开放平台凭据。
	AppID     string
	AppSecret string
	// Sandbox 走沙箱环境。
	Sandbox bool
	// Intents 是订阅的意图位; 0 用 IntentDefault (1<<25)。
	Intents int
	// Shard 是分片信息; 空用 []int{0,1}。
	Shard []int
	// BotNames 是机器人昵称与别名, 用于命中文本判定 is_mentioned。
	BotNames []string
	// Platform 写入 SessionMessage.Platform; 空用 "qq"。
	Platform string
	// AccountID / Scope 是 session_id 的可选路由组件 (多开时区分)。
	AccountID string
	Scope     string
	// PassiveWindow 覆盖被动回复有效期; <=0 用 5 分钟。
	PassiveWindow time.Duration
	// PassiveMaxReplies 覆盖同一条消息的被动回复次数上限; <=0 用 5。
	PassiveMaxReplies int
	// Logger 是结构化日志; nil 用 slog.Default()。
	Logger *slog.Logger
	// Now 注入时钟 (测试用)。
	Now func() time.Time
	// HTTPClient 复用外部 HTTP 客户端。
	HTTPClient *http.Client
	// APIBaseOverride / TokenURLOverride 覆写域名 (测试用)。
	APIBaseOverride  string
	TokenURLOverride string
	// HeartbeatInterval 强制心跳间隔 (测试用); >0 时覆盖 hello 帧给的值。
	HeartbeatInterval time.Duration
	// Dialer 覆写 websocket 拨号器 (测试用)。
	Dialer *websocket.Dialer
	// GatewayURL 直接指定网关地址, 跳过 GET /gateway (测试用)。
	GatewayURL string
	// Quota 是主动额度配置; 为 nil 时用默认额度。
	Quota *ProactiveOptions
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) intents() int {
	if o.Intents == 0 {
		return IntentDefault
	}
	return o.Intents
}

// Handler 是事件回调: 收到一条入站消息时调用。
//
// 返回错误只用于日志/监控, 不影响网关继续收事件。
type Handler func(ctx context.Context, msg *message.SessionMessage) error

// Status 是通道状态 (供 WebUI / 健康检查)。
type Status struct {
	Connected bool
	SelfID    string
	SessionID string
	LastSeq   int64
	LastError string
	UpdatedAt time.Time
}

// Channel 是 QQ 官方 WS 网关通道。用 NewChannel 创建。
type Channel struct {
	mu        sync.RWMutex
	opts      Options
	log       *slog.Logger
	api       *APIClient
	handler   Handler
	quota     *ProactiveLedger
	refs      *RefLedger
	sent      *SentLedger
	cancel    context.CancelFunc
	conn      *websocket.Conn
	connMu    sync.Mutex
	sessionID string
	lastSeq   int64
	statusMu  sync.RWMutex
	status    Status
	gateway   string
	dialer    *websocket.Dialer
}

// NewChannel 创建一个 QQ 官方通道。
func NewChannel(opts Options) *Channel {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	api := NewAPIClient(APIOptions{
		AppID:            opts.AppID,
		AppSecret:        opts.AppSecret,
		Sandbox:          opts.Sandbox,
		APIBaseOverride:  opts.APIBaseOverride,
		TokenURLOverride: opts.TokenURLOverride,
		HTTPClient:       opts.HTTPClient,
		Now:              opts.Now,
		Logger: func(format string, args ...any) {
			logger.Info(fmt.Sprintf(format, args...))
		},
	})
	quotaOpts := ProactiveOptions{Now: opts.Now}
	if opts.Quota != nil {
		quotaOpts = *opts.Quota
		quotaOpts.Now = opts.Now
	}
	channel := &Channel{
		opts:   opts,
		log:    logger,
		api:    api,
		quota:  NewProactiveLedger(quotaOpts),
		refs:   NewRefLedger(0, 0),
		sent:   NewSentLedger(0, 0),
		status: Status{UpdatedAt: opts.now()},
	}
	if opts.Now != nil {
		channel.refs.WithClock(opts.Now)
		channel.sent.WithClock(opts.Now)
	}
	api.SetRefLedger(channel.refs)
	api.SetSentLedger(channel.sent)
	api.SetQuotaLedger(channel.quota)
	if opts.Dialer != nil {
		channel.dialer = opts.Dialer
	}
	channel.gateway = strings.TrimSpace(opts.GatewayURL)
	return channel
}

// API 返回底层 REST 客户端 (上层发消息用)。
func (c *Channel) API() *APIClient { return c.api }

// Quota 返回主动额度台账。
func (c *Channel) Quota() *ProactiveLedger { return c.quota }

// Refs 返回 REFIDX 引用索引。
func (c *Channel) Refs() *RefLedger { return c.refs }

// Sent 返回自发消息台账。
func (c *Channel) Sent() *SentLedger { return c.sent }

// Status 返回通道状态。
func (c *Channel) Status() Status {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.status
}

func (c *Channel) setStatus(connected bool, selfID string, lastErr string) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.status.Connected = connected
	if selfID != "" {
		c.status.SelfID = selfID
	}
	c.status.LastError = lastErr
	c.status.UpdatedAt = c.opts.now()
}

func (c *Channel) setSession(sessionID string, seq int64, selfID string) {
	c.statusMu.Lock()
	c.status.SessionID = sessionID
	if seq > 0 {
		c.status.LastSeq = seq
	}
	if selfID != "" {
		c.status.SelfID = selfID
	}
	c.status.UpdatedAt = c.opts.now()
	c.statusMu.Unlock()
	c.connMu.Lock()
	c.sessionID = sessionID
	if seq > 0 {
		c.lastSeq = seq
	}
	c.connMu.Unlock()
}

func (c *Channel) session() (string, int64) {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.sessionID, c.lastSeq
}

// ---------------------------------------------------------------------------
// 连接循环
// ---------------------------------------------------------------------------

// Connect 建立网关连接并持续接收事件, 直到 ctx 取消。
//
// 退出时连接已关闭; 返回的 error 通常是 ctx.Err()。
func (c *Channel) Connect(ctx context.Context, handler Handler) error {
	c.mu.RLock()
	appID := strings.TrimSpace(c.opts.AppID)
	secret := strings.TrimSpace(c.opts.AppSecret)
	c.mu.RUnlock()
	if appID == "" || secret == "" {
		c.setStatus(false, "", ErrNotConfigured.Error())
		return ErrNotConfigured
	}
	c.mu.Lock()
	c.handler = handler
	c.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	defer cancel()

	backoff := reconnectBase
	for runCtx.Err() == nil {
		err := c.runSession(runCtx)
		if runCtx.Err() != nil {
			break
		}
		if err != nil {
			c.log.Warn("qq: 网关会话结束, 准备重连", "err", err, "backoff", backoff)
			c.setStatus(false, c.Status().SelfID, err.Error())
		}
		select {
		case <-runCtx.Done():
		case <-time.After(backoff):
		}
		if backoff < reconnectMax {
			backoff *= 2
			if backoff > reconnectMax {
				backoff = reconnectMax
			}
		}
	}
	return runCtx.Err()
}

// Close 断开网关连接。
func (c *Channel) Close() error {
	c.mu.Lock()
	cancel := c.cancel
	c.cancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.closeConn()
	c.setStatus(false, c.Status().SelfID, "")
	return nil
}

func (c *Channel) closeConn() {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// runSession 跑完一次网关连接的生命周期, 返回时连接已关闭。
func (c *Channel) runSession(ctx context.Context) error {
	gateway, err := c.gatewayURL(ctx)
	if err != nil {
		return err
	}
	auth, err := c.api.AuthHeader(ctx)
	if err != nil {
		return err
	}
	dialer := c.dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, _, err := dialer.DialContext(ctx, gateway, nil)
	if err != nil {
		return fmt.Errorf("qq: 连接网关失败: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(readLimit)
	c.connMu.Lock()
	c.conn = conn
	sessionID, lastSeq := c.sessionID, c.lastSeq
	c.connMu.Unlock()

	// OP 10 Hello 带来心跳间隔, 必须先收到它才能开始鉴权。
	var hello GatewayPayload
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("qq: 读取 Hello 失败: %w", err)
	}
	interval := HelloFrame(hello)
	if interval <= heartbeatSlop {
		interval = defaultHeartbeat
	}
	if c.opts.HeartbeatInterval > 0 && c.opts.HeartbeatInterval < interval {
		// 显式配置更短时以配置为准 (测试用短间隔把心跳跑起来); 网关给的间隔是
		// 服务端要求, 不允许发得比它更密。
		interval = c.opts.HeartbeatInterval
	}

	// 有 session 就 RESUME 续上, 否则 IDENTIFY 开新会话。
	if err := c.authenticate(conn, auth, sessionID, lastSeq); err != nil {
		return err
	}

	sessionCtx, stop := context.WithCancel(ctx)
	defer stop()
	heartbeats := make(chan error, 1)
	go func() {
		heartbeats <- c.heartbeatLoop(sessionCtx, conn, interval)
	}()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var payload GatewayPayload
		if err := conn.ReadJSON(&payload); err != nil {
			return fmt.Errorf("qq: 网关连接中断: %w", err)
		}
		// ACK 序号: 每个 dispatch 的 s 都要记下来, 心跳与 RESUME 都用它。
		if payload.S > 0 {
			c.recordSeq(payload.S)
		}
		switch payload.Op {
		case OpDispatch:
			c.handleDispatch(ctx, payload)
		case OpHeartbeatAck:
			// 心跳应答, 无需处理 (连接正常)。
		case OpInvalidSession:
			// 会话失效: 清掉 session 让下一轮走全新 IDENTIFY, 否则会一直被拒。
			c.setSession("", 0, "")
			return fmt.Errorf("qq: 会话失效, 将重新鉴权")
		case OpReconnect:
			// 服务端要求重连: 保留 session_id/last_seq, 下一轮走 RESUME。
			return fmt.Errorf("qq: 网关要求重连")
		case OpResume, OpResumeAlt:
			// RESUME 成功回执 (部分网关以带 op 的帧回应)。
			c.setStatus(true, c.Status().SelfID, "")
		}
	}
}

// authenticate 发送 IDENTIFY 或 RESUME 帧。
func (c *Channel) authenticate(conn *websocket.Conn, auth string, sessionID string, lastSeq int64) error {
	var frame GatewayPayload
	if sessionID != "" {
		frame = ResumeFrame(auth, sessionID, lastSeq, 0)
	} else {
		frame = IdentifyFrame(auth, c.opts.intents(), c.opts.Shard)
	}
	if err := conn.WriteJSON(frame); err != nil {
		return fmt.Errorf("qq: 鉴权失败: %w", err)
	}
	return nil
}

// heartbeatLoop 按网关给的间隔上报心跳, 附带最后处理的 seq。
//
// 间隔已由 runSession 归一化 (含 heartbeatSlop 下限), 这里只兜底非法值。
func (c *Channel) heartbeatLoop(ctx context.Context, conn *websocket.Conn, interval time.Duration) error {
	if interval <= 0 {
		interval = defaultHeartbeat
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			c.connMu.Lock()
			seq := c.lastSeq
			c.connMu.Unlock()
			encoded, err := EncodeFrame(HeartbeatFrame(seq))
			if err != nil {
				return err
			}
			if err := conn.WriteMessage(websocket.TextMessage, encoded); err != nil {
				return fmt.Errorf("qq: 心跳发送失败: %w", err)
			}
		}
	}
}

func (c *Channel) recordSeq(seq int64) {
	c.connMu.Lock()
	if seq > c.lastSeq {
		c.lastSeq = seq
	}
	c.connMu.Unlock()
	c.statusMu.Lock()
	if seq > c.status.LastSeq {
		c.status.LastSeq = seq
	}
	c.statusMu.Unlock()
}

// gatewayURL 取网关地址: 显式配置优先, 否则 GET /gateway。
func (c *Channel) gatewayURL(ctx context.Context) (string, error) {
	c.mu.RLock()
	configured := c.gateway
	c.mu.RUnlock()
	if configured != "" {
		return configured, nil
	}
	url, err := c.api.GatewayURL(ctx)
	if err != nil {
		return "", err
	}
	return url, nil
}

// ---------------------------------------------------------------------------
// 事件分发
// ---------------------------------------------------------------------------

// handleDispatch 处理 OP 0 的业务事件。
func (c *Channel) handleDispatch(ctx context.Context, payload GatewayPayload) {
	switch payload.T {
	case EventReady:
		var ready struct {
			SessionID string `json:"session_id"`
			User      struct {
				ID       string `json:"id"`
				Username string `json:"username"`
			} `json:"user"`
		}
		if err := json.Unmarshal(payload.Data, &ready); err == nil {
			c.setSession(ready.SessionID, 0, ready.User.ID)
			c.setStatus(true, ready.User.ID, "")
			c.log.Info("qq: 网关就绪", "session", ready.SessionID, "self", ready.User.ID)
		}
		return
	case EventResumed:
		c.setStatus(true, c.Status().SelfID, "")
		c.log.Info("qq: 会话已恢复", "session", c.Status().SessionID)
		return
	}

	if !IsMessageEvent(payload.T) {
		return
	}
	var source EventPayload
	if err := json.Unmarshal(payload.Data, &source); err != nil {
		c.log.Warn("qq: 解析事件失败", "event", payload.T, "err", err)
		return
	}
	selfID := c.Status().SelfID
	// 自发过滤: 全量模式下平台会回推机器人自己的发言 (author.bot), 靠发送台账
	// 识别并丢弃, 不进触发器。
	if echo := DetectSelfEcho(&source, payload.T, selfID, c.sent); echo.Self {
		c.log.Debug("qq: 丢弃自发回推", "reason", echo.Reason, "id", source.ID)
		return
	}
	// 入站登记: 本条消息的 msg_idx 进引用索引, 别人引用它时才能反查发言者。
	// 必须在 handler 之前做, 晚一步就查不到自己刚才那条。
	if idx := source.MsgIdx(); idx != "" {
		c.refs.RecordInbound(idx, strings.TrimSpace(source.ID), source.SenderID(payload.T), source.Author.Bot)
		c.log.Debug("qq: 登记引用索引", "idx", idx, "msg", source.ID)
	}

	converted, err := source.ToSessionMessage(ConvertOptions{
		EventType: payload.T,
		Platform:  c.opts.Platform,
		SelfID:    selfID,
		BotNames:  c.opts.BotNames,
		AccountID: c.opts.AccountID,
		Scope:     c.opts.Scope,
		Now:       c.opts.Now,
		ResolveRef: func(refIdx string) (string, string, bool) {
			entry, ok := c.refs.Lookup(refIdx)
			if !ok {
				return "", "", false
			}
			return entry.MsgID, entry.UserID, true
		},
	})
	if err != nil {
		c.log.Warn("qq: 转换消息失败", "event", payload.T, "err", err)
		return
	}
	converted.ProcessDefaults()
	// 触发时间记进 metadata: 上层做双模决策时要判断这条消息还能不能被被动回复。
	if converted.ReplyTo != nil && converted.MessageInfo.AdditionalConfig != nil {
		converted.MessageInfo.AdditionalConfig["qq_reply_to"] = *converted.ReplyTo
	}
	converted.MessageInfo.AdditionalConfig["qq_platform_msg_id"] = strings.TrimSpace(source.ID)

	c.mu.RLock()
	handler := c.handler
	c.mu.RUnlock()
	if handler == nil {
		return
	}
	if err := handler(ctx, converted); err != nil {
		c.log.Warn("qq: 事件回调返回错误", "event", payload.T, "err", err)
	}
}

// ---------------------------------------------------------------------------
// 出站: 双模发送 + 额度 (§6.12)
// ---------------------------------------------------------------------------

// Outbound 描述一次上层发起的出站发送。
type Outbound struct {
	// Target 是发送目标。
	Target OutboundTarget
	// Text 是文本正文 (与 Media 二选一)。
	Text string
	// Media 是已上传的 file_info (与 Text 二选一)。
	Media string
	// TriggerID 是触发这轮回复的入站消息 id (被动模式用); 空表示只能走主动。
	TriggerID string
	// TriggerTime 是触发消息的到达时间 (判 5 分钟窗口)。
	TriggerTime time.Time
	// QuoteMsgID 是要引用的平台消息 id (可空)。
	QuoteMsgID string
}

// OutboundResult 是一次出站发送的结果。
type OutboundResult struct {
	// MessageID 是平台返回的消息 id (被额度拒绝时为空)。
	MessageID string
	// RefIdx 是平台返回的引用索引键。
	RefIdx string
	// Mode 是实际采用的发送模式。
	Mode SendMode
	// Seq 是被动模式的 msg_seq (主动为 0)。
	Seq int
	// Reason 是决策说明。
	Reason string
}

// Send 走一次完整的双模发送: 决策 → 发送 → 记账。
//
// 决策 (§6.12):
//  1. 有触发 msg_id 且 ≤ PassiveWindow 且该 msg_id 被动回复次数 < 5 → 被动
//  2. 否则 → 主动 (先过额度台账; 额度不足返回 *ProactiveQuotaError, 不发送)
//
// 成功后原子更新额度 (主动) 或被动次数 (被动), 并把 ext_info.ref_idx 登记进
// REFIDX 索引 (供别人引用这条自发消息时反查)。
func (c *Channel) Send(ctx context.Context, out Outbound) (*OutboundResult, error) {
	now := c.opts.now()
	scope := out.Target.Scope()
	decision := DecideSendModeWith(c.quota, scope, out.TriggerID, out.TriggerTime, now,
		c.opts.PassiveWindow, c.opts.PassiveMaxReplies)
	if err := decision.Err(); err != nil {
		c.log.Warn("qq: 主动消息额度不足, 放弃发送", "scope", scope.Label(), "err", err)
		return nil, err
	}

	// 被动模式: 发送前分配 msg_seq 并计入次数 (失败的尝试也占一次, 与平台语义一致)。
	if decision.Mode == ModePassive {
		seq := c.quota.AllocatePassiveSeq(out.TriggerID, now, out.Text)
		decision.Seq = seq
	}
	// 主动模式: 发送前占额度 (Reserve), 失败且未到达平台时回滚。
	if decision.Mode == ModeProactive {
		if err := c.quota.Reserve(scope, now); err != nil {
			return nil, err
		}
	}

	resp, err := c.api.Send(ctx, SendRequest{
		Target:     out.Target,
		Text:       out.Text,
		Media:      out.Media,
		Decision:   decision,
		QuoteMsgID: out.QuoteMsgID,
		// 去重命中时向台账领一个全新序号, 避免撞上本次回复后续段已经排到的 seq。
		Reseq: func(oldSeq int) int {
			if strings.TrimSpace(out.TriggerID) == "" {
				return oldSeq + 1
			}
			return c.quota.AllocatePassiveSeq(out.TriggerID, now, out.Text)
		},
	})
	if err != nil {
		// 失败回滚: 只有明确没到达平台 (本地拒绝/传输错误到达不了) 才回滚次数。
		rollback := true
		if sendErr, ok := err.(*APISendError); ok && sendErr.How == SentDelivered {
			rollback = false
		}
		if rollback {
			if decision.Mode == ModePassive {
				c.quota.DropPassive(out.TriggerID)
			} else {
				c.quota.Release(scope, now)
			}
		}
		return nil, err
	}

	if decision.Mode == ModePassive {
		c.quota.CommitPassiveBody(out.TriggerID, resp.Seq, out.Text)
	}
	// ext_info.ref_idx 登记进引用索引: 别人引用这条自发消息时, 入站 ref_msg_idx
	// 会等于它, 不登记就无法反查被引用的是自己哪句话。
	if resp.RefIdx != "" {
		c.refs.RecordOutbound(resp.RefIdx, resp.MessageID, c.Status().SelfID)
	}
	// 主动额度已在 Reserve 时记账, 这里不再重复 Commit。
	return &OutboundResult{
		MessageID: resp.MessageID,
		RefIdx:    resp.RefIdx,
		Mode:      decision.Mode,
		Seq:       resp.Seq,
		Reason:    decision.Reason,
	}, nil
}

// SendText 是发送纯文本的便利封装。
func (c *Channel) SendText(ctx context.Context, target OutboundTarget, text string, triggerID string, triggerTime time.Time, quoteMsgID string) (*OutboundResult, error) {
	return c.Send(ctx, Outbound{
		Target:      target,
		Text:        text,
		TriggerID:   triggerID,
		TriggerTime: triggerTime,
		QuoteMsgID:  quoteMsgID,
	})
}

// TriggerWindow 返回本通道的被动窗口时长。
func (c *Channel) TriggerWindow() time.Duration {
	if c.opts.PassiveWindow > 0 {
		return c.opts.PassiveWindow
	}
	return PassiveWindow
}
