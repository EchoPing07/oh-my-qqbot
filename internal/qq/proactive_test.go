package qq

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// fixedClock 是一个可手动推进的时钟。
type fixedClock struct {
	mu   sync.Mutex
	now  time.Time
	zone *time.Location
}

func newFixedClock(start time.Time) *fixedClock {
	return &fixedClock{now: start}
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// 2026-03-08 23:50:00 Asia/Shanghai —— 逼近午夜, 便于测每日重置。
func testStart() time.Time {
	zone, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		zone = time.FixedZone("CST", 8*3600)
	}
	return time.Date(2026, 3, 8, 23, 50, 0, 0, zone)
}

func testZone() *time.Location { return testStart().Location() }

func newTestLedger(clock *fixedClock, opts ProactiveOptions) *ProactiveLedger {
	opts.Now = clock.Now
	opts.TimeZone = testZone()
	return NewProactiveLedger(opts)
}

// ---------------------------------------------------------------------------
// 滑动窗口
// ---------------------------------------------------------------------------

// TestProactiveGroupSlidingWindow 校验单群滑动窗口 20/qpm: 第 21 条被拒、窗口
// 滑出后可再发。
func TestProactiveGroupSlidingWindow(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{})
	scope := ProactiveScope{GroupID: "G1"}

	for i := 1; i <= DefaultGroupPerMinute; i++ {
		if err := ledger.Reserve(scope, clock.Now()); err != nil {
			t.Fatalf("第 %d 条应通过额度检查: %v", i, err)
		}
		clock.Advance(time.Second)
	}
	err := ledger.Check(scope, clock.Now())
	if err == nil {
		t.Fatal("第 21 条应超过单群窗口被拒")
	}
	quotaErr, ok := AsProactiveQuotaError(err)
	if !ok {
		t.Fatalf("应返回 *ProactiveQuotaError, got %T: %v", err, quotaErr)
	}
	if quotaErr.Kind != QuotaGroupMinute {
		t.Fatalf("应命中群分钟额度, got %q", quotaErr.Kind)
	}
	if quotaErr.Limit != DefaultGroupPerMinute || quotaErr.Used != DefaultGroupPerMinute {
		t.Fatalf("额度快照不符: used=%d limit=%d", quotaErr.Used, quotaErr.Limit)
	}
	if !IsProactiveQuotaError(err) {
		t.Fatal("IsProactiveQuotaError 应为 true")
	}

	// 滑到最老一条滚出窗口 (第 1 条在 t=0, t+60s 时出窗) → 可再发一条。
	clock.Advance(time.Minute + time.Second - 20*time.Second)
	if err := ledger.Reserve(scope, clock.Now()); err != nil {
		t.Fatalf("窗口滑出后应可再发: %v", err)
	}
	// 立即再发一条仍会被拒 (窗口已占满 20)。
	if err := ledger.Check(scope, clock.Now()); err == nil {
		t.Fatal("窗口仍占满时应被拒")
	}
}

// TestProactiveSlidingWindowRelease 校验 Release 回滚额度。
func TestProactiveSlidingWindowRelease(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{})
	scope := ProactiveScope{GroupID: "G1"}
	for i := 0; i < DefaultGroupPerMinute; i++ {
		if err := ledger.Reserve(scope, clock.Now()); err != nil {
			t.Fatalf("第 %d 条应通过: %v", i+1, err)
		}
	}
	if err := ledger.Check(scope, clock.Now()); err == nil {
		t.Fatal("占满后应被拒")
	}
	ledger.Release(scope, clock.Now())
	if err := ledger.Reserve(scope, clock.Now()); err != nil {
		t.Fatalf("Release 后应能再发: %v", err)
	}
}

// TestProactiveBotSlidingWindow 校验机器人全局 qpm (60 已认证 / 30 未认证)。
func TestProactiveBotSlidingWindow(t *testing.T) {
	tests := []struct {
		name     string
		verified bool
		want     int
	}{
		{"已认证", true, DefaultBotPerMinute},
		{"未认证", false, DefaultBotPerMinuteBasic},
	}
	for _, item := range tests {
		t.Run(item.name, func(t *testing.T) {
			clock := newFixedClock(testStart())
			ledger := newTestLedger(clock, ProactiveOptions{Verified: item.verified})
			// 每发一条换一个群, 规避单群 20 的限制, 只看全局额度。
			for i := 1; i <= item.want; i++ {
				scope := ProactiveScope{GroupID: "G" + strconv.Itoa(i)}
				if err := ledger.Reserve(scope, clock.Now()); err != nil {
					t.Fatalf("第 %d 条应通过额度检查: %v", i, err)
				}
			}
			err := ledger.Check(ProactiveScope{GroupID: "G-extra"}, clock.Now())
			quotaErr, ok := AsProactiveQuotaError(err)
			if !ok {
				t.Fatalf("应命中全局额度: %v", err)
			}
			if quotaErr.Kind != QuotaBotMinute {
				t.Fatalf("应命中全局分钟额度, got %q", quotaErr.Kind)
			}
			if quotaErr.Limit != item.want {
				t.Fatalf("全局上限不符: got %d want %d", quotaErr.Limit, item.want)
			}
		})
	}
}

// TestProactiveBotMinuteOverride 校验 BotPerMinute 可显式覆盖并可用负值表示不限。
func TestProactiveBotMinuteOverride(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{BotPerMinute: 2})
	for i := 1; i <= 2; i++ {
		if err := ledger.Reserve(ProactiveScope{GroupID: "G" + strconv.Itoa(i)}, clock.Now()); err != nil {
			t.Fatalf("第 %d 条应通过: %v", i, err)
		}
	}
	if err := ledger.Check(ProactiveScope{GroupID: "G3"}, clock.Now()); err == nil {
		t.Fatal("超过显式配置的全局上限应被拒")
	}

	unlimited := newTestLedger(clock, ProactiveOptions{BotPerMinute: -1, GroupPerDay: -1, GroupPerMinute: -1})
	for i := 0; i < 100; i++ {
		if err := unlimited.Reserve(ProactiveScope{GroupID: "G"}, clock.Now()); err != nil {
			t.Fatalf("不限额度时第 %d 条不应被拒: %v", i+1, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 每日额度 + 0 点重置
// ---------------------------------------------------------------------------

// TestProactiveDailyReset 校验每群每日 1000 与每日 0 点重置。
func TestProactiveDailyReset(t *testing.T) {
	// 从当天 00:00 起跑, 避免跨过本地 0 点把日计数提前重置。
	clock := newFixedClock(time.Date(2026, 3, 8, 0, 0, 0, 0, testZone()))
	ledger := newTestLedger(clock, ProactiveOptions{})
	scope := ProactiveScope{GroupID: "G1"}

	// 一天 1440 分钟 × 每分钟 20 条 = 28800 条, 但每日上限 1000 先到。
	spent := 0
	for i := 0; i < 2000; i++ {
		if err := ledger.Reserve(scope, clock.Now()); err != nil {
			break
		}
		spent++
		if spent%DefaultGroupPerMinute == 0 {
			// 每满一批就推进一分钟多一点 (滑动窗口是闭区间的 60 秒, 正好 60 秒时
			// 最老那条仍在窗口内), 避开单群窗口限制, 只看每日上限。
			clock.Advance(time.Minute + time.Second)
		}
	}
	if spent != DefaultGroupPerDay {
		t.Fatalf("每群每日应停在上限 %d 条, got %d", DefaultGroupPerDay, spent)
	}
	err := ledger.Check(scope, clock.Now())
	quotaErr, ok := AsProactiveQuotaError(err)
	if !ok {
		t.Fatalf("应命中每日额度: %v", err)
	}
	if quotaErr.Kind != QuotaGroupDay {
		t.Fatalf("应命中群每日额度, got %q", quotaErr.Kind)
	}
	usage := ledger.Usage("G1", clock.Now())
	if usage.GroupDayUsed != DefaultGroupPerDay {
		t.Fatalf("每日用量不符: %d", usage.GroupDayUsed)
	}
	if thatDay := usage.DayResetAt; !thatDay.After(clock.Now()) {
		t.Fatalf("重置时刻应在当前时间之后: %v", thatDay)
	}

	// 推进到本地次日 0 点之后: 日计数重置, 群滑动窗口也已空 → 可再发。
	clock.Advance(time.Until(nextMidnight(clock.Now(), testZone())) + time.Second)
	if err := ledger.Reserve(scope, clock.Now()); err != nil {
		t.Fatalf("跨日后应能再发: %v", err)
	}
	if usage := ledger.Usage("G1", clock.Now()); usage.GroupDayUsed != 1 {
		t.Fatalf("跨日后日计数应重置为 1, got %d", usage.GroupDayUsed)
	}
}

// TestProactiveNextDayResetsOnlyCounts 校验每天分别计数 (不同日期不串)。
func TestProactiveNextDayResetsOnlyCounts(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{})
	scope := ProactiveScope{GroupID: "G1"}
	for i := 0; i < 5; i++ {
		if err := ledger.Reserve(scope, clock.Now()); err != nil {
			t.Fatalf("第 %d 条应通过: %v", i+1, err)
		}
	}
	if usage := ledger.Usage("G1", clock.Now()); usage.GroupDayUsed != 5 {
		t.Fatalf("当日用量不符: %d", usage.GroupDayUsed)
	}
	clock.Advance(24 * time.Hour)
	if usage := ledger.Usage("G1", clock.Now()); usage.GroupDayUsed != 0 {
		t.Fatalf("次日应重置为 0, got %d", usage.GroupDayUsed)
	}
	if err := ledger.Reserve(scope, clock.Now()); err != nil {
		t.Fatalf("次日应能发送: %v", err)
	}
	if usage := ledger.Usage("G1", clock.Now()); usage.GroupDayUsed != 1 {
		t.Fatalf("次日首次发送后应为 1, got %d", usage.GroupDayUsed)
	}
}

// ---------------------------------------------------------------------------
// 并发
// ---------------------------------------------------------------------------

// TestProactiveConcurrentReserve 校验并发 Reserve 不会超发: 恰好 20 次成功。
func TestProactiveConcurrentReserve(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{})
	scope := ProactiveScope{GroupID: "G1"}

	const goroutines = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded, rejected := 0, 0
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := ledger.Reserve(scope, clock.Now())
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded++
			} else {
				rejected++
			}
		}()
	}
	wg.Wait()
	if succeeded != DefaultGroupPerMinute {
		t.Fatalf("并发下成功数应恰为额度上限 %d, got %d", DefaultGroupPerMinute, succeeded)
	}
	if rejected != goroutines-DefaultGroupPerMinute {
		t.Fatalf("被拒数不符: got %d want %d", rejected, goroutines-DefaultGroupPerMinute)
	}
}

// TestProactiveConcurrentMultipleGroups 校验多群并发时全局额度不被突破。
func TestProactiveConcurrentMultipleGroups(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{BotPerMinute: 5})
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			scope := ProactiveScope{GroupID: "G" + strconv.Itoa(i%50)}
			if err := ledger.Reserve(scope, clock.Now()); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if usage := ledger.Usage("", clock.Now()); usage.BotMinuteUsed != 5 {
		t.Fatalf("全局用量应恰为 5, got %d", usage.BotMinuteUsed)
	}
	if succeeded != 5 {
		t.Fatalf("并发成功数应恰为全局额度 5, got %d", succeeded)
	}
}

// ---------------------------------------------------------------------------
// 双模决策
// ---------------------------------------------------------------------------

// TestDecideSendModePassiveThenProactive 校验: 有触发且未超窗未超次 → 被动;
// 超过 5 次 → 主动。
func TestDecideSendModePassiveThenProactive(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{})
	scope := ProactiveScope{GroupID: "G1"}
	triggerTime := clock.Now()

	for i := 1; i <= PassiveMaxReplies; i++ {
		decision := DecideSendMode(ledger, scope, "TRIGGER_1", triggerTime, clock.Now())
		if decision.Mode != ModePassive {
			t.Fatalf("第 %d 次应为被动, got %s (reason=%s)", i, decision.Mode, decision.Reason)
		}
		if decision.TriggerID != "TRIGGER_1" {
			t.Fatalf("被动决策应带上触发 msg_id: %q", decision.TriggerID)
		}
		if decision.Seq != i {
			t.Fatalf("第 %d 次的 seq 应为 %d, got %d", i, i, decision.Seq)
		}
		if err := decision.Err(); err != nil {
			t.Fatalf("被动决策不应返回错误: %v", err)
		}
		// 发送成功后的记账钩子: 分配 seq 并计入次数。
		if seq := ledger.AllocatePassiveSeq("TRIGGER_1", clock.Now(), "第"+strconv.Itoa(i)+"句"); seq != i {
			t.Fatalf("记账钩子分配的 seq 应为 %d, got %d", i, seq)
		}
		clock.Advance(time.Second)
	}
	// 第 6 次: 超过被动次数上限 → 降级主动 (额度充足, 可发)。
	decision := DecideSendMode(ledger, scope, "TRIGGER_1", triggerTime, clock.Now())
	if decision.Mode != ModeProactive {
		t.Fatalf("超过 %d 次应降级主动, got %s", PassiveMaxReplies, decision.Mode)
	}
	if decision.Reason != "passive_limit_reached" {
		t.Fatalf("降级原因不符: %q", decision.Reason)
	}
	if decision.Err() != nil {
		t.Fatalf("额度充足时主动决策不应报错: %v", decision.Err())
	}
}

// TestDecideSendModeWindowExpiry 校验超过 5 分钟窗口 → 主动。
func TestDecideSendModeWindowExpiry(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{})
	scope := ProactiveScope{GroupID: "G1"}
	triggerTime := clock.Now()

	if decision := DecideSendMode(ledger, scope, "T1", triggerTime, clock.Now()); decision.Mode != ModePassive {
		t.Fatalf("窗口内应为被动: %s", decision.Mode)
	}
	clock.Advance(PassiveWindow - time.Second)
	if decision := DecideSendMode(ledger, scope, "T1", triggerTime, clock.Now()); decision.Mode != ModePassive {
		t.Fatalf("窗口边界内(4分59秒)应为被动: %s", decision.Mode)
	}
	clock.Advance(2 * time.Second) // 总共 5 分 1 秒
	decision := DecideSendMode(ledger, scope, "T1", triggerTime, clock.Now())
	if decision.Mode != ModeProactive {
		t.Fatalf("超过 5 分钟窗口应走主动: %s", decision.Mode)
	}
	if decision.Reason != "passive_window_expired" {
		t.Fatalf("原因应为窗口过期: %q", decision.Reason)
	}
}

// TestDecideSendModeQuotaExhausted 校验无触发消息 + 额度耗尽 → 返回可识别错误。
func TestDecideSendModeQuotaExhausted(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{})
	scope := ProactiveScope{GroupID: "G1"}
	for i := 0; i < DefaultGroupPerMinute; i++ {
		if err := ledger.Reserve(scope, clock.Now()); err != nil {
			t.Fatalf("第 %d 条应通过: %v", i+1, err)
		}
	}
	decision := DecideSendMode(ledger, scope, "", time.Time{}, clock.Now())
	if decision.Mode != ModeProactive {
		t.Fatalf("无触发消息应走主动: %s", decision.Mode)
	}
	err := decision.Err()
	if err == nil {
		t.Fatal("额度耗尽且无触发消息应返回错误")
	}
	if _, ok := AsProactiveQuotaError(err); !ok {
		t.Fatalf("应返回 *ProactiveQuotaError, got %T", err)
	}
	if decision.QuotaErr == nil || decision.QuotaErr.Kind != QuotaGroupMinute {
		t.Fatalf("决策应记录额度错误: %+v", decision.QuotaErr)
	}
	if decision.Reason != "no_trigger:quota_exhausted" {
		t.Fatalf("原因不符: %q", decision.Reason)
	}
}

// TestDecideSendModeNilLedger 校验无台账时 (仅窗口/次数语义) 不做额度检查。
func TestDecideSendModeNilLedger(t *testing.T) {
	now := testStart()
	decision := DecideSendMode(nil, ProactiveScope{GroupID: "G1"}, "", time.Time{}, now)
	if decision.Mode != ModeProactive || decision.Err() != nil {
		t.Fatalf("无台账时应走主动且不报错: %+v", decision)
	}
}

// TestAllocatePassiveSeqAndRollback 校验 msg_seq 分配与回滚。
func TestAllocatePassiveSeqAndRollback(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{})
	if seq := ledger.AllocatePassiveSeq("T1", clock.Now(), "第一句"); seq != 1 {
		t.Fatalf("首个 seq 应为 1, got %d", seq)
	}
	if seq := ledger.AllocatePassiveSeq("T1", clock.Now(), "第二句"); seq != 2 {
		t.Fatalf("第二个 seq 应为 2, got %d", seq)
	}
	ledger.CommitPassiveBody("T1", 2, "第二句(改)")
	if bodies := ledger.PassiveBodies("T1"); len(bodies) != 2 || bodies[1] != "第二句(改)" {
		t.Fatalf("正文回填不符: %v", bodies)
	}
	// 回滚: 次数回到 1。
	ledger.DropPassive("T1")
	if used := ledger.PassiveUsed("T1"); used != 1 {
		t.Fatalf("回滚后应剩 1 次, got %d", used)
	}
	if seq := ledger.AllocatePassiveSeq("T1", clock.Now(), "重试"); seq != 2 {
		t.Fatalf("回滚后应重新分配到 seq=2, got %d", seq)
	}
	// 未知 / 空触发 id 不记账。
	if seq := ledger.AllocatePassiveSeq("", clock.Now(), "x"); seq != 0 {
		t.Fatalf("空触发 id 不应分配 seq: %d", seq)
	}
	if used := ledger.PassiveUsed(""); used != 0 {
		t.Fatalf("空触发 id 不应有记录: %d", used)
	}
}

// TestPassivePrune 校验超过 1 小时的被动记录会被清理。
func TestPassivePrune(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{})
	ledger.AllocatePassiveSeq("OLD", clock.Now(), "很久以前")
	clock.Advance(2 * time.Hour)
	if used := ledger.PassiveUsed("OLD"); used != 0 {
		t.Fatalf("超期记录应被清理, got %d", used)
	}
	if seq := ledger.AllocatePassiveSeq("OLD", clock.Now(), "新的"); seq != 1 {
		t.Fatalf("清理后应重新从 1 开始, got %d", seq)
	}
}

// TestProactiveRejectCallback 校验拒绝回调会被触发。
func TestProactiveRejectCallback(t *testing.T) {
	clock := newFixedClock(testStart())
	var mu sync.Mutex
	calls := 0
	opts := ProactiveOptions{OnReject: func(scope ProactiveScope, err *ProactiveQuotaError) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if scope.GroupID != "G1" {
			t.Errorf("回调 scope 不符: %+v", scope)
		}
	}}
	ledger := newTestLedger(clock, opts)
	scope := ProactiveScope{GroupID: "G1"}
	for i := 0; i < DefaultGroupPerMinute+2; i++ {
		_ = ledger.Check(scope, clock.Now())
		_ = ledger.Reserve(scope, clock.Now())
	}
	mu.Lock()
	defer mu.Unlock()
	if calls == 0 {
		t.Fatal("额度不足时应触发回调")
	}
	if last := ledger.LastRejection(); last == nil || last.Kind != QuotaGroupMinute {
		t.Fatalf("应记录最近一次拒绝: %+v", last)
	}
}

// TestProactiveUsageSnapshot 校验用量快照的字段。
func TestProactiveUsageSnapshot(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{})
	scope := ProactiveScope{GroupID: "G1"}
	for i := 0; i < 3; i++ {
		if err := ledger.Reserve(scope, clock.Now()); err != nil {
			t.Fatalf("Reserve 失败: %v", err)
		}
	}
	clock.Advance(time.Second)
	usage := ledger.Usage("G1", clock.Now())
	if usage.GroupMinuteUsed != 3 || usage.GroupDayUsed != 3 || usage.BotMinuteUsed != 3 {
		t.Fatalf("用量快照不符: %+v", usage)
	}
	if usage.GroupMinuteLimit != DefaultGroupPerMinute || usage.GroupDayLimit != DefaultGroupPerDay {
		t.Fatalf("额度上限不符: %+v", usage)
	}
	if usage.BotMinuteLimit != DefaultBotPerMinuteBasic {
		t.Fatalf("未认证应默认 30 qpm, got %d", usage.BotMinuteLimit)
	}
	// 群内最老一条是 t0 → 窗口重置时刻为 t0+1min。
	if !usage.GroupWindowResetAt.Equal(testStart().Add(time.Minute)) {
		t.Fatalf("窗口重置时刻不符: %v", usage.GroupWindowResetAt)
	}
	if usage := ledger.Usage("", clock.Now()); usage.GroupDayUsed != 0 {
		t.Fatalf("空 groupID 快照不应含群用量: %d", usage.GroupDayUsed)
	}
}

// TestSendDecisionSeqValue 校验被动 seq 的下界保护。
func TestSendDecisionSeqValue(t *testing.T) {
	if got := (SendDecision{Mode: ModePassive}).SeqValue(); got != 1 {
		t.Fatalf("缺省 seq 应为 1, got %d", got)
	}
	if got := (SendDecision{Mode: ModePassive, Seq: 4}).SeqValue(); got != 4 {
		t.Fatalf("seq 应原样返回, got %d", got)
	}
}

// TestProactiveResetAndSetOptions 校验 Reset / SetOptions 的行为。
func TestProactiveResetAndSetOptions(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{})
	scope := ProactiveScope{GroupID: "G1"}
	if err := ledger.Reserve(scope, clock.Now()); err != nil {
		t.Fatalf("Reserve 失败: %v", err)
	}
	ledger.Reset()
	if usage := ledger.Usage("G1", clock.Now()); usage.GroupDayUsed != 0 || usage.BotMinuteUsed != 0 {
		t.Fatalf("Reset 后用量应清零: %+v", usage)
	}
	if ledger.LastRejection() != nil {
		t.Fatal("Reset 后不应保留拒绝记录")
	}
	ledger.SetOptions(ProactiveOptions{GroupPerMinute: -1, GroupPerDay: -1, BotPerMinute: -1, Now: clock.Now, TimeZone: testZone()})
	for i := 0; i < 500; i++ {
		if err := ledger.Reserve(scope, clock.Now()); err != nil {
			t.Fatalf("不限额度时不应被拒 (第 %d 条): %v", i+1, err)
		}
	}
}

// TestProactiveScopeLabel 校验额度作用域的可读标签 (日志用)。
func TestProactiveScopeLabel(t *testing.T) {
	if got := (ProactiveScope{GroupID: "G1"}).Label(); got != "group:G1" {
		t.Fatalf("群标签不符: %q", got)
	}
	if got := (ProactiveScope{UserID: "U1"}).Label(); got != "user:U1" {
		t.Fatalf("私聊标签不符: %q", got)
	}
	if got := (ProactiveScope{}).Label(); got != "unknown" {
		t.Fatalf("空作用域标签不符: %q", got)
	}
}

// TestProactiveQuotaErrorMessage 校验额度错误的错误信息可读性。
func TestProactiveQuotaErrorMessage(t *testing.T) {
	err := &ProactiveQuotaError{Kind: QuotaGroupDay, Scope: ProactiveScope{GroupID: "G1"}, Limit: 1000, Used: 1000, Remaining: 3 * time.Second}
	if got := err.Error(); !containsAll(got, "group_day", "group:G1", "1000/1000") {
		t.Fatalf("错误信息不可读: %q", got)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		found := false
		for i := 0; i+len(part) <= len(value); i++ {
			if value[i:i+len(part)] == part {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestDecideSendModeWithOverrides 校验窗口 / 次数可被覆盖。
func TestDecideSendModeWithOverrides(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := newTestLedger(clock, ProactiveOptions{})
	scope := ProactiveScope{GroupID: "G1"}
	triggerTime := clock.Now()

	// 次数上限改为 1: 第 2 次就降级主动。
	if decision := DecideSendModeWith(ledger, scope, "T1", triggerTime, clock.Now(), 0, 1); decision.Mode != ModePassive {
		t.Fatalf("第 1 次应为被动: %s", decision.Mode)
	}
	ledger.AllocatePassiveSeq("T1", clock.Now(), "第一句")
	if decision := DecideSendModeWith(ledger, scope, "T1", triggerTime, clock.Now(), 0, 1); decision.Mode != ModeProactive {
		t.Fatalf("次数上限为 1 时第 2 次应降级主动: %s", decision.Mode)
	}

	// 窗口改为 1 分钟: 1 分 30 秒后走主动。
	if decision := DecideSendModeWith(ledger, scope, "T2", triggerTime, clock.Now(), time.Minute, 0); decision.Mode != ModePassive {
		t.Fatalf("窗口内应为被动: %s", decision.Mode)
	}
	clock.Advance(90 * time.Second)
	if decision := DecideSendModeWith(ledger, scope, "T2", triggerTime, clock.Now(), time.Minute, 0); decision.Mode != ModeProactive {
		t.Fatalf("超过 1 分钟窗口应走主动: %s", decision.Mode)
	}
}
