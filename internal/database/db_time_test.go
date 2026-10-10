// B1/B2 回归测试：DATETIME 时区对称编解码 + 会话历史逐行容错。
package database

import (
	"path/filepath"
	"testing"
	"time"
)

// pickForeignZone 从候选时区里选一个与当前进程 Local 偏移不同的时区，
// 保证「非 Local 时区」前提在任何机器上成立（Local 只有一个偏移）。
func pickForeignZone(instant time.Time) *time.Location {
	for _, offset := range []int{8 * 3600, -5 * 3600, 10 * 3600} {
		zone := time.FixedZone("foreign", offset)
		if _, localOffset := instant.In(time.Local).Zone(); localOffset != offset {
			return zone
		}
	}
	// 三选一必有一个不同于 Local（偏移空间远大于 3 个候选之外的情况不会发生）。
	return time.FixedZone("foreign", 8*3600)
}

// TestFormatTimeRoundTripNonLocalZone 回归 B1：非 Local 时区构造的时间经
// formatTime 落盘、parseTime 读回后必须表示同一时刻。旧实现按 t 自身时区
// Format 而 parseTime 一律按 time.Local 解析，跨时区漂移 8 小时。
func TestFormatTimeRoundTripNonLocalZone(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 123_000_000, time.UTC)
	in := base.In(pickForeignZone(base))

	parsed, err := parseTime(formatTime(in))
	if err != nil {
		t.Fatalf("parseTime: %v", err)
	}
	if !parsed.Equal(in) {
		t.Fatalf("往返后时刻漂移: in=%v(%s) parsed=%v(%s) diff=%v",
			in, in.Location(), parsed, parsed.Location(), parsed.Sub(in))
	}
}

// TestMessageTimestampRoundTripViaDB 回归 B1（真实存储路径）：非 Local 时区
// 的 Timestamp 经 InsertMessage → QueryMessagesBySession 后仍为同一时刻。
func TestMessageTimestampRoundTripViaDB(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	in := base.In(pickForeignZone(base))

	if _, err := db.InsertMessage(&Message{
		MessageID: "m-tz", Timestamp: in, Platform: "qq", UserID: "u1", SessionID: "s-tz",
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	msgs, err := db.QueryMessagesBySession("s-tz", 10)
	if err != nil {
		t.Fatalf("QueryMessagesBySession: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("应查回 1 条, got %d", len(msgs))
	}
	if !msgs[0].Timestamp.Equal(in) {
		t.Fatalf("时间戳漂移: in=%v parsed=%v diff=%v", in, msgs[0].Timestamp, msgs[0].Timestamp.Sub(in))
	}
}

// TestQueryMessagesBySessionSkipsBadRows 回归 B2：单个坏行（如历史脏数据的
// 不可解析时间戳）只 Warn 跳过，不毁掉整段会话历史。
func TestQueryMessagesBySessionSkipsBadRows(t *testing.T) {
	db := openTestDB(t)
	base := time.Unix(1700000000, 0).UTC()

	// 先插一条正常消息。
	if _, err := db.InsertMessage(&Message{
		MessageID: "m-good", Timestamp: base, Platform: "qq", UserID: "u1", SessionID: "s-bad",
		RawContent: "[]",
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	// 直接 SQL 注入一条坏时间戳的脏数据（绕过 formatTime 的规范化）。
	if _, err := db.write.Exec(`
		INSERT INTO mai_messages (message_id, timestamp, platform, user_id, session_id, raw_content)
		VALUES ('m-corrupt', 'not-a-timestamp', 'qq', 'u1', 's-bad', '{}')`); err != nil {
		t.Fatalf("注入坏行: %v", err)
	}
	// 再插一条正常消息（坏行之后）。
	if _, err := db.InsertMessage(&Message{
		MessageID: "m-good2", Timestamp: base.Add(time.Minute), Platform: "qq", UserID: "u1", SessionID: "s-bad",
		RawContent: "[]",
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	msgs, err := db.QueryMessagesBySession("s-bad", 10)
	if err != nil {
		t.Fatalf("坏行不应导致整体失败: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("应跳过坏行并返回 2 条好消息, got %d", len(msgs))
	}
	if msgs[0].MessageID != "m-good" || msgs[1].MessageID != "m-good2" {
		t.Fatalf("消息应按时间升序: %s, %s", msgs[0].MessageID, msgs[1].MessageID)
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "data", "mai.db"))
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
