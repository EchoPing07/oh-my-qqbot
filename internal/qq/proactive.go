package qq

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 主动消息额度台账 (§6.12)
//
// QQ 官方对主动消息有三级额度:
//   - 单群滑动窗口 20 条 / 分钟 (qpm)
//   - 每群每日 1000 条 (每日 0 点重置)
//   - 机器人全局 60 或 30 条 / 分钟 (按认证等级, 配置选择)
//
// 发送前检查 (Check / Reserve), 发送成功后才记账 (Commit 或 Reserve 一次性完成), 两
// 者都在同一把 mutex 内完成, 并发安全且不会超发。额度不足返回 *ProactiveQuotaError,
// 调用方据此把「主动消息额度不足」回执给模型, 由 Planner 自行决定沉默。
//
// 被动回复不占额度, 但占「同一条消息最多 5 次」的次数, 这个计数也在本文件里
// (AllocatePassiveSeq / PassiveUsed), 与额度台账共用一把锁。
// ---------------------------------------------------------------------------

// 额度默认值 (来自官方限额; 均可由 ProactiveOptions 覆盖)。
const (
	// DefaultGroupPerMinute 是单群滑动窗口上限。
	DefaultGroupPerMinute = 20
	// DefaultGroupPerDay 是每群每日上限。
	DefaultGroupPerDay = 1000
	// DefaultBotPerMinute 是机器人全局每分钟上限 (已认证/高级)。
	DefaultBotPerMinute = 60
	// DefaultBotPerMinuteBasic 是未认证机器人的全局每分钟上限。
	DefaultBotPerMinuteBasic = 30
	// GroupWindow 是群滑动窗口长度。
	GroupWindow = time.Minute
	// BotWindow 是全局滑动窗口长度。
	BotWindow = time.Minute
)

// ProactiveOptions 配置额度台账。
type ProactiveOptions struct {
	// GroupPerMinute 是单群滑动窗口上限 (默认 20)。<=0 表示不限。
	GroupPerMinute int
	// GroupPerDay 是每群每日上限 (默认 1000)。<=0 表示不限。
	GroupPerDay int
	// BotPerMinute 是机器人全局滑动窗口上限; 0 表示按 Verified 选择默认值
	// (60 / 30)。<0 表示不限。
	BotPerMinute int
	// Verified 为真时用 60 qpm, 否则 30 qpm (仅当 BotPerMinute==0 生效)。
	Verified bool
	// TimeZone 决定「每日 0 点」的时区; nil 用 time.Local。
	TimeZone *time.Location
	// Now 提供当前时间 (测试注入), nil 用 time.Now。
	Now func() time.Time
	// OnReject 在额度不足时被调用 (用于记日志 / 监控事件)。
	OnReject func(ProactiveScope, *ProactiveQuotaError)
}

// normalized 返回填好默认值的选项副本。
func (o ProactiveOptions) normalized() ProactiveOptions {
	out := o
	if out.GroupPerMinute == 0 {
		out.GroupPerMinute = DefaultGroupPerMinute
	}
	if out.GroupPerDay == 0 {
		out.GroupPerDay = DefaultGroupPerDay
	}
	if out.BotPerMinute == 0 {
		if out.Verified {
			out.BotPerMinute = DefaultBotPerMinute
		} else {
			out.BotPerMinute = DefaultBotPerMinuteBasic
		}
	}
	return out
}

func (o ProactiveOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o ProactiveOptions) location() *time.Location {
	if o.TimeZone != nil {
		return o.TimeZone
	}
	return time.Local
}

// ProactiveQuotaKind 标记命中的是哪一级额度。
type ProactiveQuotaKind string

const (
	// QuotaGroupMinute 命中单群滑动窗口。
	QuotaGroupMinute ProactiveQuotaKind = "group_minute"
	// QuotaGroupDay 命中每群每日上限。
	QuotaGroupDay ProactiveQuotaKind = "group_day"
	// QuotaBotMinute 命中机器人全局滑动窗口。
	QuotaBotMinute ProactiveQuotaKind = "bot_minute"
)

// ProactiveScope 描述一次主动发送的目标。
type ProactiveScope struct {
	// GroupID 是群 openid; 私聊主动消息留空。
	GroupID string
	// UserID 是单聊目标 openid (群消息留空)。
	UserID string
}

// Label 给日志用的可读目标。
func (s ProactiveScope) Label() string {
	if s.GroupID != "" {
		return "group:" + s.GroupID
	}
	if s.UserID != "" {
		return "user:" + s.UserID
	}
	return "unknown"
}

// ProactiveQuotaError 是可识别的额度不足错误。
type ProactiveQuotaError struct {
	Kind      ProactiveQuotaKind
	Scope     ProactiveScope
	Limit     int
	Used      int
	ResetAt   time.Time
	Remaining time.Duration
}

// Error implements error.
func (e *ProactiveQuotaError) Error() string {
	return fmt.Sprintf("qq: 主动消息额度不足(%s): %s 已用 %d/%d, %s 后重置",
		e.Kind, e.Scope.Label(), e.Used, e.Limit, e.Remaining.Round(time.Second))
}

// IsProactiveQuotaError 判断错误是否是额度不足错误。
func IsProactiveQuotaError(err error) bool {
	_, ok := err.(*ProactiveQuotaError)
	return ok
}

// AsProactiveQuotaError 取出额度错误详情。
func AsProactiveQuotaError(err error) (*ProactiveQuotaError, bool) {
	typed, ok := err.(*ProactiveQuotaError)
	return typed, ok
}

// ---------------------------------------------------------------------------
// 台账
// ---------------------------------------------------------------------------

type groupQuota struct {
	timestamps []time.Time // 滑动窗口内的发送时刻 (升序)
	dayStamp   string      // 当前日计数归属的日期 (yyyy-mm-dd)
	dayCount   int
}

// ProactiveLedger 是主动消息额度台账。零值不可用, 用 NewProactiveLedger 创建。
type ProactiveLedger struct {
	mu      sync.Mutex
	opts    ProactiveOptions
	groups  map[string]*groupQuota
	bot     []time.Time
	lastErr *ProactiveQuotaError
	// pendingRejects 暂存待触发的 OnReject 回调 (P3d): rejectLocked 持锁时只入队,
	// 公开方法解锁后统一触发。
	pendingRejects []func()
	// passive 记着每个触发消息 id 已经完成的被动回复, 用于次数上限判定与
	// msg_seq 分配 (与主动额度正交: 被动回复不占额度, 但要占这个次数)。
	passive map[string][]passiveReply
}

// NewProactiveLedger 创建额度台账。
func NewProactiveLedger(opts ProactiveOptions) *ProactiveLedger {
	return &ProactiveLedger{opts: opts.normalized(), groups: map[string]*groupQuota{}}
}

// Options 返回生效的配置 (已填默认值)。
func (l *ProactiveLedger) Options() ProactiveOptions {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opts
}

// Check 判断一次主动发送是否还有额度。有额度返回 nil, 否则返回 *ProactiveQuotaError。
//
// 检查顺序按官方限额从窄到宽: 群分钟 → 群日 → 全局分钟。
func (l *ProactiveLedger) Check(scope ProactiveScope, now time.Time) error {
	if now.IsZero() {
		now = l.opts.now()
	}
	l.mu.Lock()
	err := l.checkLocked(scope, now)
	l.mu.Unlock()
	l.firePendingRejects()
	return err
}

// Commit 在发送成功后记账。必须在 Check 通过之后调用, 且只记成功的一次。
func (l *ProactiveLedger) Commit(scope ProactiveScope, now time.Time) {
	if now.IsZero() {
		now = l.opts.now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.commitLocked(scope, now)
}

// ProactiveTicket 是一次 Reserve 的回执 (B3): 记下这次记账写入了哪些计数, 供
// Release 精确回滚 —— 旧的"弹栈"式回滚在并发下会删掉别人的记录。
type ProactiveTicket struct {
	// Scope 是这次记账的目标。
	Scope ProactiveScope
	// At 是记账时刻 (群滑动窗口 / 全局窗口里写入的那条时间戳)。
	At time.Time
	// DayStamp 是日计数归属的日期键: 跨日回滚时不再扣减新一天的计数。
	DayStamp string
}

// Reserve 是 Check + Commit 的原子版本: 检查通过后立即记账, 避免并发超发。
// 返回非 nil 错误时没有记账; 成功时返回回执票据, 供 Release 按票据精确回滚 (B3)。
func (l *ProactiveLedger) Reserve(scope ProactiveScope, now time.Time) (ProactiveTicket, error) {
	if now.IsZero() {
		now = l.opts.now()
	}
	l.mu.Lock()
	err := l.checkLocked(scope, now)
	if err != nil {
		l.mu.Unlock()
		l.firePendingRejects()
		return ProactiveTicket{}, err
	}
	ticket := ProactiveTicket{Scope: scope, At: now}
	if scope.GroupID != "" {
		entry := l.groupForLocked(scope.GroupID, now)
		entry.timestamps = append(entry.timestamps, now)
		entry.dayCount++
		ticket.DayStamp = entry.dayStamp
	}
	l.bot = append(l.bot, now)
	l.mu.Unlock()
	l.firePendingRejects()
	return ticket, nil
}

// Release 按票据精确回滚一次 Reserve (B3): 只删票据对应的那条记账, 找不到票据
// (已被滑窗清理 / 重复回滚) 时不动其他记录。零值票据是 no-op。
func (l *ProactiveLedger) Release(ticket ProactiveTicket) {
	if ticket.At.IsZero() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if ticket.Scope.GroupID != "" {
		if entry, ok := l.groups[ticket.Scope.GroupID]; ok {
			kept, removed := removeTime(entry.timestamps, ticket.At)
			entry.timestamps = kept
			// 日计数只在同一日内、且确实删掉了票据对应的时间戳时才回滚 ——
			// 否则重复回滚会把日计数扣成负向偏差。
			if removed && entry.dayStamp == ticket.DayStamp && entry.dayCount > 0 {
				entry.dayCount--
			}
		}
	}
	l.bot, _ = removeTime(l.bot, ticket.At)
}

// Usage 返回当前额度用量快照 (供 WebUI / 日志)。
type ProactiveUsage struct {
	GroupMinuteUsed      int
	GroupMinuteLimit     int
	GroupDayUsed         int
	GroupDayLimit        int
	BotMinuteUsed        int
	BotMinuteLimit       int
	GroupWindowResetAt   time.Time
	BotWindowResetAt     time.Time
	DayResetAt           time.Time
	LastRejection        *ProactiveQuotaError
	LastGroupMinuteReset time.Time
}

// Usage 返回指定群 + 全局的额度快照。groupID 为空时只统计全局。
func (l *ProactiveLedger) Usage(groupID string, now time.Time) ProactiveUsage {
	if now.IsZero() {
		now = l.opts.now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	usage := ProactiveUsage{
		GroupMinuteLimit:   l.opts.GroupPerMinute,
		GroupDayLimit:      l.opts.GroupPerDay,
		BotMinuteLimit:     l.opts.BotPerMinute,
		BotMinuteUsed:      len(l.bot),
		DayResetAt:         nextMidnight(now, l.opts.location()),
		LastRejection:      l.lastErr,
		GroupWindowResetAt: now.Add(GroupWindow),
		BotWindowResetAt:   now.Add(BotWindow),
	}
	if entry, ok := l.groups[groupID]; ok && groupID != "" {
		usage.GroupMinuteUsed = len(entry.timestamps)
		usage.GroupDayUsed = entry.dayCount
		if len(entry.timestamps) > 0 {
			usage.GroupWindowResetAt = entry.timestamps[0].Add(GroupWindow)
			usage.LastGroupMinuteReset = entry.timestamps[0]
		}
	}
	if len(l.bot) > 0 {
		usage.BotWindowResetAt = l.bot[0].Add(BotWindow)
	}
	return usage
}

// LastRejection 返回最近一次额度拒绝 (可能为空)。
func (l *ProactiveLedger) LastRejection() *ProactiveQuotaError {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastErr
}

// Reset 清空台账 (配置热重载时用)。
func (l *ProactiveLedger) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.groups = map[string]*groupQuota{}
	l.bot = nil
	l.lastErr = nil
	l.pendingRejects = nil
	l.passive = nil
}

// SetOptions 更新配置 (不清空已记账的用量)。
func (l *ProactiveLedger) SetOptions(opts ProactiveOptions) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.opts = opts.normalized()
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (l *ProactiveLedger) checkLocked(scope ProactiveScope, now time.Time) error {
	l.pruneLocked(now)
	if scope.GroupID != "" {
		entry := l.groupForLocked(scope.GroupID, now)
		if l.opts.GroupPerMinute > 0 && len(entry.timestamps) >= l.opts.GroupPerMinute {
			return l.rejectLocked(QuotaGroupMinute, scope, len(entry.timestamps), l.opts.GroupPerMinute,
				entry.timestamps[0].Add(GroupWindow), now)
		}
		if l.opts.GroupPerDay > 0 && entry.dayCount >= l.opts.GroupPerDay {
			return l.rejectLocked(QuotaGroupDay, scope, entry.dayCount, l.opts.GroupPerDay,
				nextMidnight(now, l.opts.location()), now)
		}
	}
	if l.opts.BotPerMinute > 0 && len(l.bot) >= l.opts.BotPerMinute {
		return l.rejectLocked(QuotaBotMinute, scope, len(l.bot), l.opts.BotPerMinute,
			l.bot[0].Add(BotWindow), now)
	}
	return nil
}

func (l *ProactiveLedger) commitLocked(scope ProactiveScope, now time.Time) {
	if scope.GroupID != "" {
		entry := l.groupForLocked(scope.GroupID, now)
		entry.timestamps = append(entry.timestamps, now)
		entry.dayCount++
	}
	l.bot = append(l.bot, now)
}

func (l *ProactiveLedger) rejectLocked(kind ProactiveQuotaKind, scope ProactiveScope, used int, limit int, resetAt time.Time, now time.Time) error {
	remaining := resetAt.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	err := &ProactiveQuotaError{
		Kind:      kind,
		Scope:     scope,
		Limit:     limit,
		Used:      used,
		ResetAt:   resetAt,
		Remaining: remaining,
	}
	l.lastErr = err
	if onReject := l.opts.OnReject; onReject != nil {
		// P3d: OnReject 回调不再持锁调用 —— 回调里若再进台账会死锁。先暂存,
		// 由触发本次检查的公开方法在解锁后触发 (firePendingRejects)。
		callback := onReject
		l.pendingRejects = append(l.pendingRejects, func() { callback(scope, err) })
	}
	return err
}

// firePendingRejects 在锁外触发暂存的 OnReject 回调 (P3d)。
func (l *ProactiveLedger) firePendingRejects() {
	l.mu.Lock()
	callbacks := l.pendingRejects
	l.pendingRejects = nil
	l.mu.Unlock()
	for _, callback := range callbacks {
		callback()
	}
}

// groupForLocked 取 (或建) 一个群的计数; 日期变了就先重置日计数。
func (l *ProactiveLedger) groupForLocked(groupID string, now time.Time) *groupQuota {
	entry, ok := l.groups[groupID]
	if !ok {
		entry = &groupQuota{dayStamp: dayStamp(now, l.opts.location())}
		l.groups[groupID] = entry
		return entry
	}
	if stamp := dayStamp(now, l.opts.location()); entry.dayStamp != stamp {
		entry.dayStamp = stamp
		entry.dayCount = 0
	}
	return entry
}

// pruneLocked 淘汰滑窗外的时间戳。调用方持锁。
func (l *ProactiveLedger) pruneLocked(now time.Time) {
	for id, entry := range l.groups {
		entry.timestamps = dropOlderThan(entry.timestamps, now.Add(-GroupWindow))
		if len(entry.timestamps) == 0 && entry.dayStamp != dayStamp(now, l.opts.location()) {
			delete(l.groups, id)
		}
	}
	l.bot = dropOlderThan(l.bot, now.Add(-BotWindow))
}

// dropOlderThan 丢弃严格早于 cutoff 的时刻 (保持升序, 复用底层数组)。
func dropOlderThan(sorted []time.Time, cutoff time.Time) []time.Time {
	keep := 0
	for _, stamp := range sorted {
		if !stamp.Before(cutoff) {
			sorted[keep] = stamp
			keep++
		}
	}
	return sorted[:keep]
}

// removeTime 删除切片中第一条等于 target 的时刻 (票据回滚用, B3); 返回新切片与
// 是否确实删除了一条 —— 找不到时原样返回, 不动其他记录。
func removeTime(sorted []time.Time, target time.Time) ([]time.Time, bool) {
	for i, stamp := range sorted {
		if stamp.Equal(target) {
			return append(sorted[:i], sorted[i+1:]...), true
		}
	}
	return sorted, false
}

// dayStamp 取当天日期键 (yyyy-mm-dd, 按给定时区)。
func dayStamp(now time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	local := now.In(loc)
	return local.Format("2006-01-02")
}

// nextMidnight 返回下一个本地 0 点 (每日额度重置时刻)。
func nextMidnight(now time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	local := now.In(loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	if !local.Before(midnight) {
		midnight = midnight.AddDate(0, 0, 1)
	}
	return midnight
}

// ---------------------------------------------------------------------------
// 双模发送决策 (§6.12 发送决策)
// ---------------------------------------------------------------------------

// 被动回复的官方约束。
const (
	// PassiveWindow 是群聊被动回复的有效期 (带 msg_id 回复超过这个时长会被拒)。
	PassiveWindow = 5 * time.Minute
	// PassiveMaxReplies 是群聊同一条消息最多能被被动回复的次数。
	PassiveMaxReplies = 5
	// C2CPassiveWindow 是单聊 (C2C) 被动回复的有效期: 单聊窗口比群聊宽得多
	// (B2, 对齐 参考/qq-official-adapter/constants.py 与 Diana)。
	C2CPassiveWindow = 60 * time.Minute
	// C2CPassiveMaxReplies 是单聊同一条消息最多能被被动回复的次数 (B2)。
	C2CPassiveMaxReplies = 4
	// passiveWindowMargin 是判定被动窗口时预留的安全余量 (B4): 平台按服务端时间
	// 判定窗口, 本地时钟偏差与网络往返都会吃掉窗口边缘, 统一预留 60 秒 —— 即群聊
	// 实际可用 4 分钟 (对齐 Python GROUP_PASSIVE_REPLY_TTL_SEC=4*60)、单聊 59 分钟
	// (对齐 PRIVATE_PASSIVE_REPLY_TTL_SEC=59*60)。
	passiveWindowMargin = time.Minute
)

// SendMode 是一次出站消息的发送模式。
type SendMode string

const (
	// ModePassive 是被动回复: 带触发 msg_id + 自增 msg_seq。
	ModePassive SendMode = "passive"
	// ModeProactive 是主动消息: 不带 msg_id, 占额度。
	ModeProactive SendMode = "proactive"
)

// SendDecision 是 DecideSendMode 的结果。
type SendDecision struct {
	// Mode 是本次发送采用的模式。
	Mode SendMode
	// TriggerID 是被动模式下的触发消息 id (主动模式为空)。
	TriggerID string
	// Seq 是被动模式下的 msg_seq (从 1 起, 同 msg_id 内递增)。
	Seq int
	// Reason 说明为什么选了这个模式 (日志/监控用)。
	Reason string
	// QuotaErr 非 nil 时表示主动额度不足, 调用方应放弃发送。
	QuotaErr *ProactiveQuotaError
}

// Passive 返回是否是被动模式。
func (d SendDecision) Passive() bool { return d.Mode == ModePassive }

// Err 返回本次决策的错误 (额度不足时为 *ProactiveQuotaError)。
func (d SendDecision) Err() error {
	if d.QuotaErr != nil {
		return d.QuotaErr
	}
	return nil
}

// DecideSendMode 决定一次出站消息走被动还是主动 (§6.12 发送决策 1/2)。
//
// 规则: 有触发 msg_id 且未超窗口 (群 5 分钟 / 单聊 60 分钟, 各预留 60 秒余量) 且
// 该 msg_id 已被动回复次数 < 上限 (群 5 次 / 单聊 4 次) → 被动; 否则 → 主动
// (先过额度台账, 额度不足返回 *ProactiveQuotaError)。
//
// 每段独立决策: reply 工具的多段回复中, 前几段走被动, 超过次数上限的后续段自动
// 降级为主动 (额度不足则放弃该段)。
//
// now 为零值时用 ledger 的时钟。ledger 为 nil 时不做额度检查 (仅按窗口/次数决策)。
func DecideSendMode(ledger *ProactiveLedger, scope ProactiveScope, triggerID string, triggerTime time.Time, now time.Time) SendDecision {
	// B2: 窗口/次数交由 DecideSendModeWith 按 scope (群聊/单聊) 取默认。
	return DecideSendModeWith(ledger, scope, triggerID, triggerTime, now, 0, 0)
}

// DecideSendModeWith 是 DecideSendMode 的可配置版本: window / maxReplies 非正数时
// 按 scope 取默认 —— 群聊 5 分钟 / 5 次, 单聊 (无 GroupID) 60 分钟 / 4 次 (B2,
// 对齐 Python constants.py / Diana); 显式覆盖对群聊与单聊都生效。
// Channel 用它把 Options 的覆盖值接进来。
func DecideSendModeWith(ledger *ProactiveLedger, scope ProactiveScope, triggerID string, triggerTime time.Time, now time.Time, window time.Duration, maxReplies int) SendDecision {
	if scope.GroupID == "" {
		if window <= 0 {
			window = C2CPassiveWindow
		}
		if maxReplies <= 0 {
			maxReplies = C2CPassiveMaxReplies
		}
	} else {
		if window <= 0 {
			window = PassiveWindow
		}
		if maxReplies <= 0 {
			maxReplies = PassiveMaxReplies
		}
	}
	// B4: 判定窗口预留 60 秒安全余量 (对齐 Python 的 *_PASSIVE_REPLY_TTL_SEC);
	// 窗口比余量还短时不扣, 避免窗口被打穿。
	if window > passiveWindowMargin {
		window -= passiveWindowMargin
	}
	triggerID = trim(triggerID)
	if now.IsZero() {
		if ledger != nil {
			now = ledger.opts.now()
		} else {
			now = time.Now()
		}
	}
	if triggerID != "" && !triggerTime.IsZero() && now.Sub(triggerTime) <= window {
		used := 0
		if ledger != nil {
			used = ledger.PassiveUsed(triggerID)
		}
		if used < maxReplies {
			return SendDecision{
				Mode:      ModePassive,
				TriggerID: triggerID,
				Seq:       used + 1,
				Reason:    "passive:" + strconv.Itoa(used+1) + "/" + strconv.Itoa(maxReplies),
			}
		}
		return SendDecision{Mode: ModeProactive, Reason: "passive_limit_reached"}
	}
	reason := "no_trigger"
	switch {
	case triggerID == "":
		reason = "no_trigger"
	case triggerTime.IsZero():
		reason = "no_trigger_time"
	default:
		reason = "passive_window_expired"
	}
	if ledger == nil {
		return SendDecision{Mode: ModeProactive, Reason: reason}
	}
	if err := ledger.Check(scope, now); err != nil {
		quotaErr, _ := AsProactiveQuotaError(err)
		return SendDecision{Mode: ModeProactive, Reason: reason + ":quota_exhausted", QuotaErr: quotaErr}
	}
	return SendDecision{Mode: ModeProactive, Reason: reason}
}

// PassiveUsed 返回某个触发消息 id 已完成的被动回复次数。
func (l *ProactiveLedger) PassiveUsed(triggerID string) int {
	triggerID = trim(triggerID)
	if triggerID == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.passive == nil {
		return 0
	}
	l.prunePassiveLocked(l.opts.now())
	return len(l.passive[triggerID])
}

// passiveReply 是一次被动回复的记账条目: seq 是这次分配到的 msg_seq, at 是分配
// 时刻, body 由发送成功后的记账钩子回填。
type passiveReply struct {
	seq  int
	at   time.Time
	body string
}

// AllocatePassiveSeq 是发送前的记账钩子: 分配本次被动回复的 msg_seq (从 1 起)
// 并计入该触发消息已用的被动次数。
//
// 平台按 (msg_id, msg_seq) 去重, 同一触发消息的第 N 次回复必须用 seq=N; 重复会
// 命中 40054005。分配即计数 (失败的尝试也占一次, 与平台侧语义一致), 只有确认没
// 到达平台的失败才用 DropPassive 按票据 (seq) 回滚 (B3)。
func (l *ProactiveLedger) AllocatePassiveSeq(triggerID string, now time.Time, body string) int {
	triggerID = trim(triggerID)
	if triggerID == "" {
		return 0
	}
	if now.IsZero() {
		now = l.opts.now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.passive == nil {
		l.passive = map[string][]passiveReply{}
	}
	l.prunePassiveLocked(now)
	entries := l.passive[triggerID]
	// B3: 下一个 seq 取已有最大值 +1, 而不是 len+1 —— 按票据回滚可能删掉中间的
	// 条目, len+1 会撞上还活着的 seq。
	seq := 0
	for _, entry := range entries {
		if entry.seq > seq {
			seq = entry.seq
		}
	}
	seq++
	entries = append(entries, passiveReply{seq: seq, at: now, body: trim(body)})
	l.passive[triggerID] = entries
	return seq
}

// CommitPassiveBody 回填一次已分配被动回复的正文 (发送成功后的记账钩子)。
func (l *ProactiveLedger) CommitPassiveBody(triggerID string, seq int, body string) {
	triggerID = trim(triggerID)
	if triggerID == "" || seq <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.passive[triggerID]
	for i := range entries {
		if entries[i].seq == seq {
			entries[i].body = trim(body)
			return
		}
	}
}

// PassiveBodies 返回某个触发消息已发过的正文 (自检 / 测试用)。
func (l *ProactiveLedger) PassiveBodies(triggerID string) []string {
	triggerID = trim(triggerID)
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.passive[triggerID]))
	for _, entry := range l.passive[triggerID] {
		out = append(out, entry.body)
	}
	return out
}

// DropPassive 按票据 (msg_seq) 精确撤销一次被动记账 (发送未到达平台时回滚,
// 让次数可复用)。B3: 只删 seq 对应的条目, 找不到票据时不动其他记录 —— 旧的
// "弹栈"式回滚在并发下会删错别人的记录。
func (l *ProactiveLedger) DropPassive(triggerID string, seq int) {
	triggerID = trim(triggerID)
	if triggerID == "" || seq <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.passive[triggerID]
	for i, entry := range entries {
		if entry.seq == seq {
			entries = append(entries[:i], entries[i+1:]...)
			break
		}
	}
	if len(entries) == 0 {
		delete(l.passive, triggerID)
		return
	}
	l.passive[triggerID] = entries
}

// passiveTTL 是被动记账的保留时长: 超过被动窗口的记录不会再被用到。
const passiveTTL = time.Hour

func (l *ProactiveLedger) prunePassiveLocked(now time.Time) {
	for id, entries := range l.passive {
		kept := entries[:0]
		for _, entry := range entries {
			if now.Sub(entry.at) <= passiveTTL {
				kept = append(kept, entry)
			}
		}
		if len(kept) == 0 {
			delete(l.passive, id)
			continue
		}
		l.passive[id] = kept
	}
}

func trim(value string) string {
	start := 0
	end := len(value)
	for start < end && (value[start] == ' ' || value[start] == '\t' || value[start] == '\n' || value[start] == '\r') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\t' || value[end-1] == '\n' || value[end-1] == '\r') {
		end--
	}
	return value[start:end]
}
