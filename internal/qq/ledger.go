package qq

import (
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// REFIDX 引用索引 (§6.12 ledger.go)
//
// 官方引用有两套键空间:
//   - 入站: message_scene.ext 的 msg_idx 是本条消息的引用索引键, ref_msg_idx 指向
//     被引用消息; 两者都是 REFIDX_ 前缀的不可解析值。
//   - 出站: 发送响应的 ext_info.ref_idx 是这条自发消息的引用索引键; 而
//     message_reference.message_id 必须填 REFIDX_ 形态 (引用别人用入站 msg_idx,
//     引用自己用 REFIDX 台账查到的 ref_idx), 所以两个方向都要能互查。
//
// 上层 (历史/引用目标) 用的是平台消息 id (入站 d.id / 出站响应 id), 因此这张表
// 维护 key ↔ msgID 的双向映射, 带 TTL 与容量上限。
// ---------------------------------------------------------------------------

const (
	// RefLedgerTTL 是引用索引的有效期。REFIDX_ 本身无时效说明, 这里与会话历史
	// 窗口对齐: 更早的消息在历史里也找不到了, 索引留着没意义。
	RefLedgerTTL = 24 * time.Hour
	// RefLedgerCap 限制索引条数, 防止长期运行无限增长。
	RefLedgerCap = 4096
	// RefKeyPrefix 是官方引用键的前缀。
	RefKeyPrefix = "REFIDX_"
)

// RefEntry 是一个引用索引条目。
type RefEntry struct {
	// Key 是 REFIDX_ 引用键。
	Key string
	// MsgID 是同一消息的平台 id (入站 d.id / 出站发送响应 id)。
	MsgID string
	// UserID 是这条消息的发言者 (出站记 selfID)。
	UserID string
	// Bot 标记这条消息是不是机器人发的。
	Bot bool
	// At 是登记时间。
	At time.Time
}

// RefLedger 是 REFIDX_ 引用索引的双向表。
type RefLedger struct {
	mu      sync.Mutex
	entries []RefEntry
	ttl     time.Duration
	cap     int
	now     func() time.Time
}

// NewRefLedger 创建引用索引; ttl <= 0 用 RefLedgerTTL, capacity <= 0 用 RefLedgerCap。
func NewRefLedger(ttl time.Duration, capacity int) *RefLedger {
	if ttl <= 0 {
		ttl = RefLedgerTTL
	}
	if capacity <= 0 {
		capacity = RefLedgerCap
	}
	return &RefLedger{ttl: ttl, cap: capacity}
}

// WithClock 注入时钟 (测试用), 返回自身便于链式构造。
func (l *RefLedger) WithClock(now func() time.Time) *RefLedger {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
	return l
}

func (l *RefLedger) time() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func (l *RefLedger) ttlValue() time.Duration {
	if l.ttl > 0 {
		return l.ttl
	}
	return RefLedgerTTL
}

func (l *RefLedger) capValue() int {
	if l.cap > 0 {
		return l.cap
	}
	return RefLedgerCap
}

// RecordInbound 登记一条入站消息的 msg_idx (发言者已知)。
func (l *RefLedger) RecordInbound(key, msgID, userID string, bot bool) {
	l.record(RefEntry{Key: key, MsgID: msgID, UserID: userID, Bot: bot, At: l.time()})
}

// RecordOutbound 登记一次出站发送返回的 ext_info.ref_idx (发言者是机器人)。
func (l *RefLedger) RecordOutbound(key, msgID, selfID string) {
	l.record(RefEntry{Key: key, MsgID: msgID, UserID: selfID, Bot: true, At: l.time()})
}

// Record 登记一条引用索引条目 (key 或 msgID 为空则丢弃)。
func (l *RefLedger) Record(entry RefEntry) { l.record(entry) }

func (l *RefLedger) record(entry RefEntry) {
	entry.Key = strings.TrimSpace(entry.Key)
	entry.MsgID = strings.TrimSpace(entry.MsgID)
	entry.UserID = strings.TrimSpace(entry.UserID)
	if entry.Key == "" || (entry.MsgID == "" && entry.UserID == "") {
		return
	}
	now := l.time()
	if entry.At.IsZero() {
		entry.At = now
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	for i := range l.entries {
		if l.entries[i].Key == entry.Key {
			l.entries[i] = entry
			return
		}
	}
	l.entries = append(l.entries, entry)
	if len(l.entries) > l.capValue() {
		l.entries = l.entries[len(l.entries)-l.capValue():]
	}
}

// Lookup 反查一个引用索引键 (入站 ref_msg_idx / msg_idx)。
func (l *RefLedger) Lookup(key string) (RefEntry, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return RefEntry{}, false
	}
	now := l.time()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	for i := range l.entries {
		if l.entries[i].Key == key {
			return l.entries[i], true
		}
	}
	return RefEntry{}, false
}

// RefIdxFor 由平台消息 id 反查它的 REFIDX_ 引用键。
//
// 出站引用 (message_reference) 必须填 REFIDX_ 形态, 而上层引用目标给的是平台
// 消息 id, 靠这个方法转过去。查不到 (进程重启后索引丢失、目标超出时效) 返回空串,
// 调用方应不带引用照常发送, 避免引用值非法被平台整条拒收。
func (l *RefLedger) RefIdxFor(msgID string) string {
	msgID = strings.TrimSpace(msgID)
	if msgID == "" {
		return ""
	}
	now := l.time()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	for i := range l.entries {
		if l.entries[i].MsgID == msgID {
			return l.entries[i].Key
		}
	}
	return ""
}

// MsgIDFor 由 REFIDX_ 引用键反查平台消息 id (Lookup 的便利形式)。
func (l *RefLedger) MsgIDFor(key string) string {
	entry, ok := l.Lookup(key)
	if !ok {
		return ""
	}
	return entry.MsgID
}

// RoundTrip 是一个引用键 → 平台 id → 引用键的往返, 供测试与自检用。
func (l *RefLedger) RoundTrip(key string) (msgID string, back string, ok bool) {
	entry, found := l.Lookup(key)
	if !found {
		return "", "", false
	}
	return entry.MsgID, l.RefIdxFor(entry.MsgID), true
}

// Len 返回当前条目数 (测试/诊断用)。
func (l *RefLedger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(l.time())
	return len(l.entries)
}

// Reset 清空索引。
func (l *RefLedger) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = nil
}

func (l *RefLedger) pruneLocked(now time.Time) {
	ttl := l.ttlValue()
	keep := 0
	for _, entry := range l.entries {
		if now.Sub(entry.At) <= ttl {
			l.entries[keep] = entry
			keep++
		}
	}
	l.entries = l.entries[:keep]
}

// IsRefKey 判断一个值是不是官方 REFIDX_ 引用键形态。
func IsRefKey(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), RefKeyPrefix)
}

// ---------------------------------------------------------------------------
// 自发消息台账 (§6.12 自发消息过滤)
//
// 开启「获取群内全部消息」后, 机器人自己的发言会以 GROUP_MESSAGE_CREATE 回推。
// 靠 2min TTL 的发送台账 + author.id 识别并丢弃: 不进触发器, 但对齐 MaiBot 的
// outbound_send 历史 (发送成功时已主动写历史, 回推的直接丢)。
// ---------------------------------------------------------------------------

const (
	// SentLedgerTTL 是发送记录的有效期; 平台回推通常在一两秒内, 留足余量即可。
	SentLedgerTTL = 2 * time.Minute
	// SentLedgerCap 限制记录条数, 防止刷屏时无限增长。
	SentLedgerCap = 1024
	// SelfEchoTolerance 是回推与发送记录的时间匹配容差。
	SelfEchoTolerance = SentLedgerTTL
)

// SentEntry 是一次成功发送的记录。
type SentEntry struct {
	// Scope 是发送目标 (群 openid 或单聊 user openid)。
	Scope string
	// MessageID 是平台返回的消息 id; 为空时靠正文 + 时间匹配。
	MessageID string
	// Text 是发送正文 (用于 id 对不上时的兜底匹配)。
	Text string
	// At 是发送时刻。
	At time.Time
	// Passive 标记这次发送走的是被动模式。
	Passive bool
	// Group 标记这条发送是群消息还是单聊 (回推识别时用同一 scope 匹配)。
	Group bool
}

// SentLedger 记录最近发出的消息, 用来认出平台回推的自发消息。
//
// 开启「获取群内全部消息」后, 机器人自己的发言会以 GROUP_MESSAGE_CREATE 回推。
// 只按 author.id == selfID 认自己的话, 平台一旦只带 member_openid 就会把回声当成
// 群友发言; 消息 id 对得上最可靠, id 不同空间时再退到同 scope + 同正文 + 时间容差
// 的匹配。调用方只在 author.bot 为真时才查, 所以正文匹配不会误伤真人复读。
type SentLedger struct {
	mu      sync.Mutex
	entries []SentEntry
	ttl     time.Duration
	cap     int
	now     func() time.Time
}

// NewSentLedger 创建发送台账; ttl <= 0 用 SentLedgerTTL, cap <= 0 用 SentLedgerCap。
func NewSentLedger(ttl time.Duration, capacity int) *SentLedger {
	if ttl <= 0 {
		ttl = SentLedgerTTL
	}
	if capacity <= 0 {
		capacity = SentLedgerCap
	}
	return &SentLedger{ttl: ttl, cap: capacity}
}

// WithClock 注入时钟 (测试用)。
func (l *SentLedger) WithClock(now func() time.Time) *SentLedger {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
	return l
}

func (l *SentLedger) time() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// Record 记一次成功发送。
func (l *SentLedger) Record(entry SentEntry) {
	entry.Scope = strings.TrimSpace(entry.Scope)
	entry.MessageID = strings.TrimSpace(entry.MessageID)
	entry.Text = strings.TrimSpace(entry.Text)
	if entry.Scope == "" || (entry.MessageID == "" && entry.Text == "") {
		return
	}
	now := l.time()
	if entry.At.IsZero() {
		entry.At = now
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	l.entries = append(l.entries, entry)
	if len(l.entries) > l.cap {
		l.entries = l.entries[len(l.entries)-l.cap:]
	}
}

// Matches 判断一条入站消息是不是自己刚发的; 命中后移除该记录, 避免同一条正文
// 之后被重复认领。
//
// 匹配优先级: 同 scope + 相同消息 id > 同 scope + 相同正文 (且时间在容差内)。
// 正文匹配只在调用方确认 author.bot 为真时使用, 不会误伤真人复读。
func (l *SentLedger) Matches(scope, messageID, text string) bool {
	scope = strings.TrimSpace(scope)
	messageID = strings.TrimSpace(messageID)
	text = strings.TrimSpace(text)
	if scope == "" || (messageID == "" && text == "") {
		return false
	}
	now := l.time()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	hit := -1
	for i, entry := range l.entries {
		if entry.Scope != scope {
			continue
		}
		if messageID != "" && entry.MessageID == messageID {
			hit = i
			break
		}
		if hit < 0 && text != "" && entry.Text == text && now.Sub(entry.At) <= SelfEchoTolerance {
			hit = i
		}
	}
	if hit < 0 {
		return false
	}
	l.entries = append(l.entries[:hit], l.entries[hit+1:]...)
	return true
}

// Take 匹配并取出命中的记录 (需要 id/时间时用), 未命中返回 false。
func (l *SentLedger) Take(scope, messageID, text string) (SentEntry, bool) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return SentEntry{}, false
	}
	now := l.time()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	for i, entry := range l.entries {
		if entry.Scope != scope {
			continue
		}
		if messageID != "" && entry.MessageID == messageID {
			l.removeAt(i)
			return entry, true
		}
	}
	return SentEntry{}, false
}

// Len 返回当前记录数。
func (l *SentLedger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(l.time())
	return len(l.entries)
}

// Reset 清空台账。
func (l *SentLedger) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = nil
}

func (l *SentLedger) removeAt(index int) {
	l.entries = append(l.entries[:index], l.entries[index+1:]...)
}

func (l *SentLedger) pruneLocked(now time.Time) {
	ttl := l.ttl
	if ttl <= 0 {
		ttl = SentLedgerTTL
	}
	keep := 0
	for _, entry := range l.entries {
		if now.Sub(entry.At) <= ttl {
			l.entries[keep] = entry
			keep++
		}
	}
	l.entries = l.entries[:keep]
}

// ---------------------------------------------------------------------------
// 自发判定
// ---------------------------------------------------------------------------

// SelfEchoResult 描述一次入站消息的自发判定结果。
type SelfEchoResult struct {
	// Self 为真时这条入站消息应被丢弃 (不进触发器)。
	Self bool
	// Reason 说明判定依据 (日志用): "author_id" / "ledger_id" / "ledger_text"。
	Reason string
	// SelfID 是判定出的机器人自身标识 (可能为空)。
	SelfID string
}

// DetectSelfEcho 判断一条入站消息是不是机器人自己的回推。
//
// 判定顺序:
//  1. payload author.bot 为真、且 author.id / member_openid 与 selfID 相同 → 自发
//  2. author.bot 为真、id 对不上 selfID, 发送台账 id 或 (同正文 + 时间容差) 命中 → 自发
//     (平台回推的 author.id 与 READY 给的 selfID 不是同一空间时靠这条兜底)
//  3. 否则不是自发
//
// ledger 为 nil 时只走第 1 条。scope 为空时按事件自动取群 openid (群聊) 或对端
// openid (单聊)。
func DetectSelfEcho(payload *EventPayload, eventType string, selfID string, ledger *SentLedger) SelfEchoResult {
	if payload == nil {
		return SelfEchoResult{}
	}
	selfID = strings.TrimSpace(selfID)
	authorID := strings.TrimSpace(payload.Author.ID)
	if payload.Author.Bot && selfID != "" {
		for _, candidate := range []string{authorID, strings.TrimSpace(payload.Author.MemberOpenID), strings.TrimSpace(payload.Author.UserOpenID)} {
			if candidate != "" && candidate == selfID {
				return SelfEchoResult{Self: true, Reason: "author_id", SelfID: selfID}
			}
		}
	}
	if payload.Author.Bot && ledger != nil {
		scope := payload.GroupOpenid()
		if scope == "" {
			scope = payload.Author.UserOpenID
		}
		if ledger.Matches(scope, strings.TrimSpace(payload.ID), strings.TrimSpace(payload.Content)) {
			id := selfID
			if id == "" {
				id = authorID
			}
			return SelfEchoResult{Self: true, Reason: "ledger_match", SelfID: id}
		}
	}
	return SelfEchoResult{Self: false}
}
