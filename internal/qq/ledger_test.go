package qq

import (
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// REFIDX 引用索引
// ---------------------------------------------------------------------------

// TestRefLedgerRoundTrip 校验引用键 → 平台 id → 引用键的双向往返。
func TestRefLedgerRoundTrip(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewRefLedger(0, 0).WithClock(clock.Now)

	// 入站: 别人发的消息, 登记 msg_idx。
	ledger.RecordInbound("REFIDX_IN_1", "MSG_1", "MEMBER_1", false)
	// 出站: 自己发的消息, 登记 ext_info.ref_idx。
	ledger.RecordOutbound("REFIDX_OUT_1", "MSG_2", "BOT_1")

	tests := []struct {
		name  string
		key   string
		msgID string
		user  string
		bot   bool
	}{
		{"别人的消息", "REFIDX_IN_1", "MSG_1", "MEMBER_1", false},
		{"自己的消息", "REFIDX_OUT_1", "MSG_2", "BOT_1", true},
	}
	for _, item := range tests {
		t.Run(item.name, func(t *testing.T) {
			if got := ledger.MsgIDFor(item.key); got != item.msgID {
				t.Fatalf("key→msgID 不符: got %q want %q", got, item.msgID)
			}
			if got := ledger.RefIdxFor(item.msgID); got != item.key {
				t.Fatalf("msgID→key 不符: got %q want %q", got, item.key)
			}
			msgID, back, ok := ledger.RoundTrip(item.key)
			if !ok || msgID != item.msgID || back != item.key {
				t.Fatalf("往返不符: msgID=%q back=%q ok=%v", msgID, back, ok)
			}
			entry, found := ledger.Lookup(item.key)
			if !found || entry.UserID != item.user || entry.Bot != item.bot {
				t.Fatalf("条目不符: %+v found=%v", entry, found)
			}
		})
	}
}

// TestRefLedgerSelfQuoteRoundTrip 校验「引用自己」的路径:
// 发送响应 ref_idx 登记后, 别人引用它时入站 ref_msg_idx 能反查回自己那条。
func TestRefLedgerSelfQuoteRoundTrip(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewRefLedger(0, 0).WithClock(clock.Now)
	ledger.RecordOutbound("REFIDX_OUT_SELF", "MSG_SELF", "BOT_1")

	// 别人引用了自己这条: 入站 ref_msg_idx = 出站登记的 ref_idx。
	entry, ok := ledger.Lookup("REFIDX_OUT_SELF")
	if !ok {
		t.Fatal("应能反查到自发消息")
	}
	if entry.MsgID != "MSG_SELF" || entry.UserID != "BOT_1" || !entry.Bot {
		t.Fatalf("自发条目不符: %+v", entry)
	}
	// 反查成平台消息 id, 供上层当引用目标用。
	if got := ledger.MsgIDFor(entry.Key); got != "MSG_SELF" {
		t.Fatalf("反查平台 id 不符: %q", got)
	}
}

// TestRefLedgerTTL 校验过期条目会被清理, 且 TTL 可配置。
func TestRefLedgerTTL(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewRefLedger(time.Minute, 0).WithClock(clock.Now)
	ledger.RecordInbound("REFIDX_OLD", "MSG_OLD", "MEMBER_1", false)

	if _, ok := ledger.Lookup("REFIDX_OLD"); !ok {
		t.Fatal("未过期应能查到")
	}
	clock.Advance(2 * time.Minute)
	if _, ok := ledger.Lookup("REFIDX_OLD"); ok {
		t.Fatal("过期应被清理")
	}
	if got := ledger.Len(); got != 0 {
		t.Fatalf("清理后应为空, got %d", got)
	}

	// 默认 TTL 24h: 一小时内不应过期。
	long := NewRefLedger(0, 0).WithClock(clock.Now)
	long.RecordInbound("REFIDX_KEEP", "MSG_KEEP", "MEMBER_1", false)
	clock.Advance(time.Hour)
	if _, ok := long.Lookup("REFIDX_KEEP"); !ok {
		t.Fatal("默认 TTL 24h 内不应过期")
	}
}

// TestRefLedgerCap 校验容量上限 (超容量丢最老的)。
func TestRefLedgerCap(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewRefLedger(0, 2).WithClock(clock.Now)
	ledger.RecordInbound("REFIDX_A", "MSG_A", "MEMBER_1", false)
	ledger.RecordInbound("REFIDX_B", "MSG_B", "MEMBER_2", false)
	ledger.RecordInbound("REFIDX_C", "MSG_C", "MEMBER_3", false)

	if got := ledger.Len(); got != 2 {
		t.Fatalf("容量应封顶 2, got %d", got)
	}
	if _, ok := ledger.Lookup("REFIDX_A"); ok {
		t.Fatal("超出容量应丢最老的条目")
	}
	if _, ok := ledger.Lookup("REFIDX_C"); !ok {
		t.Fatal("最新条目应保留")
	}
}

// TestRefLedgerOverwrite 校验同键重复登记会覆盖 (保留最新)。
func TestRefLedgerOverwrite(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewRefLedger(0, 0).WithClock(clock.Now)
	ledger.RecordInbound("REFIDX_X", "MSG_OLD", "MEMBER_1", false)
	ledger.RecordInbound("REFIDX_X", "MSG_NEW", "MEMBER_2", false)

	if got := ledger.Len(); got != 1 {
		t.Fatalf("同键应覆盖而非新增, got %d", got)
	}
	entry, _ := ledger.Lookup("REFIDX_X")
	if entry.MsgID != "MSG_NEW" || entry.UserID != "MEMBER_2" {
		t.Fatalf("应保留最新登记: %+v", entry)
	}
}

// TestRefLedgerIgnoresBlank 校验空键/空值被丢弃。
func TestRefLedgerIgnoresBlank(t *testing.T) {
	ledger := NewRefLedger(0, 0)
	ledger.RecordInbound("", "MSG", "MEMBER_1", false)
	ledger.RecordInbound("REFIDX_Y", "", "", false)
	if got := ledger.Len(); got != 0 {
		t.Fatalf("空键或空值应被丢弃, got %d", got)
	}
}

// TestIsRefKeyAndRoundTrip 校验 REFIDX_ 形态判定。
func TestIsRefKey(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"REFIDX_abc", true},
		{"REFIDX_", true},
		{"MSG_1", false},
		{"", false},
		{"  REFIDX_x", true},
	}
	for _, item := range tests {
		if got := IsRefKey(item.value); got != item.want {
			t.Fatalf("IsRefKey(%q) = %v want %v", item.value, got, item.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 自发消息台账
// ---------------------------------------------------------------------------

// TestSentLedgerMatchesByID 校验按消息 id 匹配并只认领一次。
func TestSentLedgerMatchesByID(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewSentLedger(0, 0).WithClock(clock.Now)
	ledger.Record(SentEntry{Scope: "G1", MessageID: "MSG_1", Text: "你好", Group: true})

	if !ledger.Matches("G1", "MSG_1", "你好") {
		t.Fatal("同群同 id 应命中")
	}
	if ledger.Matches("G1", "MSG_1", "你好") {
		t.Fatal("同一条记录只能认领一次")
	}
	if ledger.Matches("G2", "MSG_1", "你好") {
		t.Fatal("不同 scope 不应命中")
	}
}

// TestSentLedgerMatchesByText 校验 id 不同空间时退到正文 + 时间容差匹配。
func TestSentLedgerMatchesByText(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewSentLedger(0, 0).WithClock(clock.Now)
	ledger.Record(SentEntry{Scope: "G1", MessageID: "", Text: "复读一句"})

	if !ledger.Matches("G1", "ECHOED_ID", "复读一句") {
		t.Fatal("同群同正文应命中")
	}
	if ledger.Matches("G1", "ECHOED_ID", "复读一句") {
		t.Fatal("不应重复认领")
	}
}

// TestSentLedgerTextOutsideTolerance 校验超出时间容差的正文不匹配。
func TestSentLedgerTextOutsideTolerance(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewSentLedger(time.Minute, 0).WithClock(clock.Now)
	ledger.Record(SentEntry{Scope: "G1", Text: "很久以前"})

	clock.Advance(5 * time.Minute)
	if ledger.Matches("G1", "", "很久以前") {
		t.Fatal("超出容差不应命中正文匹配")
	}
	if ledger.Len() != 0 {
		t.Fatalf("超期记录应被清理, got %d", ledger.Len())
	}
}

// TestSentLedgerTTL 校验 2min TTL 清理。
func TestSentLedgerTTL(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewSentLedger(0, 0).WithClock(clock.Now)
	ledger.Record(SentEntry{Scope: "G1", MessageID: "MSG_1", Text: "x"})
	if ledger.Len() != 1 {
		t.Fatalf("应先有 1 条, got %d", ledger.Len())
	}
	clock.Advance(3 * time.Minute)
	if ledger.Len() != 0 {
		t.Fatalf("2min TTL 后应清理, got %d", ledger.Len())
	}
}

// TestSentLedgerCap 校验容量上限。
func TestSentLedgerCap(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewSentLedger(0, 2).WithClock(clock.Now)
	for i := 1; i <= 5; i++ {
		ledger.Record(SentEntry{Scope: "G1", MessageID: itoa(i)})
	}
	if got := ledger.Len(); got != 2 {
		t.Fatalf("容量应封顶 2, got %d", got)
	}
	if !ledger.Matches("G1", "4", "") || !ledger.Matches("G1", "5", "") {
		t.Fatal("应保留最新的两条")
	}
}

// TestSentLedgerTakeAndReset 校验 Take / Reset / 空输入。
func TestSentLedgerTakeAndReset(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewSentLedger(0, 0).WithClock(clock.Now)
	ledger.Record(SentEntry{Scope: "G1", MessageID: "MSG_1", Text: "你好", Passive: true})

	entry, ok := ledger.Take("G1", "MSG_1", "")
	if !ok || entry.Passive != true {
		t.Fatalf("Take 应命中并带回记录: %+v ok=%v", entry, ok)
	}
	if _, ok := ledger.Take("G1", "MSG_1", ""); ok {
		t.Fatal("不应重复 Take")
	}
	if _, ok := ledger.Take("", "MSG_1", ""); ok {
		t.Fatal("空 scope 不应命中")
	}

	ledger.Record(SentEntry{Scope: "G1", MessageID: "MSG_2"})
	ledger.Reset()
	if got := ledger.Len(); got != 0 {
		t.Fatalf("Reset 后应为空, got %d", got)
	}
	// 空内容 / 空 scope 不登记。
	ledger.Record(SentEntry{Scope: "", MessageID: "MSG_3"})
	ledger.Record(SentEntry{Scope: "G1"})
	if got := ledger.Len(); got != 0 {
		t.Fatalf("无效记录应被丢弃, got %d", got)
	}
	if ledger.Matches("", "", "") {
		t.Fatal("空输入不应命中")
	}
}

// TestRefLedgerReset 校验索引清空。
func TestRefLedgerReset(t *testing.T) {
	ledger := NewRefLedger(0, 0)
	ledger.RecordInbound("REFIDX_Z", "MSG_Z", "MEMBER_1", false)
	ledger.Reset()
	if got := ledger.Len(); got != 0 {
		t.Fatalf("Reset 后应为空, got %d", got)
	}
	if _, ok := ledger.Lookup("REFIDX_Z"); ok {
		t.Fatal("Reset 后不应查到条目")
	}
}

// TestRefLedgerConcurrentRecord 校验并发登记安全且不丢最新键。
func TestRefLedgerConcurrentRecord(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewRefLedger(0, 0).WithClock(clock.Now)
	var wg sync.WaitGroup
	for i := 1; i <= 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ledger.RecordInbound("REFIDX_"+itoa(i), "MSG_"+itoa(i), "MEMBER_1", false)
		}(i)
	}
	wg.Wait()
	if got := ledger.MsgIDFor("REFIDX_200"); got != "MSG_200" {
		t.Fatalf("最新键丢失: %q", got)
	}
	if got := ledger.Len(); got != 200 {
		t.Fatalf("条目数不符: got %d want 200", got)
	}
}

// TestSentLedgerConcurrentRecord 校验并发登记与认领。
func TestSentLedgerConcurrentRecord(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewSentLedger(0, 0).WithClock(clock.Now)
	var wg sync.WaitGroup
	for i := 1; i <= 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ledger.Record(SentEntry{Scope: "G1", MessageID: "MSG_" + itoa(i)})
		}(i)
	}
	wg.Wait()
	if got := ledger.Len(); got != 100 {
		t.Fatalf("并发登记数不符: got %d want 100", got)
	}
	claims := 0
	for i := 1; i <= 100; i++ {
		if ledger.Matches("G1", "MSG_"+itoa(i), "") {
			claims++
		}
	}
	if claims != 100 {
		t.Fatalf("认领数不符: got %d want 100", claims)
	}
}

// ---------------------------------------------------------------------------
// 自发判定
// ---------------------------------------------------------------------------

func selfEchoPayload(id string, authorID string, memberOpenID string, bot bool, content string) *EventPayload {
	return &EventPayload{
		ID:          id,
		Content:     content,
		GroupOpenID: "G1",
		Author: Author{
			ID:           authorID,
			MemberOpenID: firstNonEmpty(memberOpenID, authorID),
			Bot:          bot,
		},
	}
}

// TestDetectSelfEchoByAuthorID 校验 author.bot + id 命中 selfID → 自发。
func TestDetectSelfEchoByAuthorID(t *testing.T) {
	payload := selfEchoPayload("MSG_1", "BOT_1", "BOT_1", true, "我刚说的")
	got := DetectSelfEcho(payload, EventGroupMessageCreate, "BOT_1", NewSentLedger(0, 0))
	if !got.Self || got.Reason != "author_id" || got.SelfID != "BOT_1" {
		t.Fatalf("应判定为自发: %+v", got)
	}
}

// TestDetectSelfEchoByMemberOpenID 校验 author.id 对不上但 member_openid 命中。
func TestDetectSelfEchoByMemberOpenID(t *testing.T) {
	payload := selfEchoPayload("MSG_1", "OTHER_ID", "BOT_1", true, "我刚说的")
	got := DetectSelfEcho(payload, EventGroupMessageCreate, "BOT_1", NewSentLedger(0, 0))
	if !got.Self || got.Reason != "author_id" {
		t.Fatalf("member_openid 命中也应判为自发: %+v", got)
	}
}

// TestDetectSelfEchoByLedger 校验 id 不同空间时靠发送台账兜底。
func TestDetectSelfEchoByLedger(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewSentLedger(0, 0).WithClock(clock.Now)
	ledger.Record(SentEntry{Scope: "G1", MessageID: "MSG_1", Text: "我刚说的", Group: true})

	// 回推的 author.id 与 selfID 不是同一空间。
	payload := selfEchoPayload("MSG_1", "UNKNOWN_SPACE_ID", "UNKNOWN_SPACE_ID", true, "我刚说的")
	got := DetectSelfEcho(payload, EventGroupMessageCreate, "", ledger)
	if !got.Self || got.Reason != "ledger_match" {
		t.Fatalf("台账兜底应命中: %+v", got)
	}
	if got.SelfID != "UNKNOWN_SPACE_ID" {
		t.Fatalf("selfID 未知时应回退 author.id: %q", got.SelfID)
	}
}

// TestDetectSelfEchoNotBot 校验真人消息不判为自发 (即使正文与台账一致)。
func TestDetectSelfEchoNotBot(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewSentLedger(0, 0).WithClock(clock.Now)
	ledger.Record(SentEntry{Scope: "G1", MessageID: "MSG_1", Text: "复读一句", Group: true})

	payload := selfEchoPayload("MSG_OTHER", "MEMBER_9", "MEMBER_9", false, "复读一句")
	got := DetectSelfEcho(payload, EventGroupMessageCreate, "BOT_1", ledger)
	if got.Self {
		t.Fatal("真人复读不应判为自发")
	}
	// 台账里我们自己那条仍在, 未被误认领。
	if !ledger.Matches("G1", "MSG_1", "") {
		t.Fatal("真人消息不应消耗台账记录")
	}
}

// TestDetectSelfEchoNilPayload 校验空 payload 安全。
func TestDetectSelfEchoNilPayload(t *testing.T) {
	if got := DetectSelfEcho(nil, EventGroupMessageCreate, "BOT_1", NewSentLedger(0, 0)); got.Self {
		t.Fatal("空 payload 不应判为自发")
	}
}

// TestDetectSelfEchoPrivateScope 校验单聊回推用对端 openid 当 scope。
func TestDetectSelfEchoPrivateScope(t *testing.T) {
	clock := newFixedClock(testStart())
	ledger := NewSentLedger(0, 0).WithClock(clock.Now)
	ledger.Record(SentEntry{Scope: "USER_OPENID_1", MessageID: "C2C_1", Text: "你好"})

	payload := &EventPayload{
		ID:      "C2C_1",
		Content: "你好",
		Author: Author{
			ID:         "UNKNOWN",
			UserOpenID: "USER_OPENID_1",
			Bot:        true,
		},
	}
	got := DetectSelfEcho(payload, EventC2CMessageCreate, "", ledger)
	if !got.Self || got.Reason != "ledger_match" {
		t.Fatalf("单聊回推应命中台账: %+v", got)
	}
}
