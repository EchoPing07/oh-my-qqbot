package transfer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"oh-my-qqbot/internal/database"
)

var testBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)

func testDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "transfer.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func strPtr(value string) *string { return &value }

// seed 写入一份覆盖全部分片的样本数据。
func seed(t *testing.T, db *database.DB) {
	t.Helper()
	if _, err := db.Write().Exec(`
		INSERT INTO mai_messages (message_id, timestamp, platform, user_id, user_nickname, user_cardname,
			group_id, group_name, is_mentioned, is_at, session_id, reply_to, is_emoji, is_picture,
			is_command, is_notify, raw_content, processed_plain_text, additional_config, reply_frequency)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"M1", formatTransferTime(testBase), "qq", "U1", "小明", nil,
		strPtr("G1"), strPtr("测试群"), 1, 0, "g:1", nil, 0, 1,
		0, 0, `{"components":[{"type":"text","data":{"text":"你好"}}]}`, "你好", nil, 0.5,
	); err != nil {
		t.Fatalf("插入消息失败: %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO mai_messages (message_id, timestamp, platform, user_id, user_nickname,
			session_id, reply_to, raw_content, processed_plain_text)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		"M2", formatTransferTime(testBase.Add(time.Minute)), "qq", "U2", "小红",
		"g:1", strPtr("M1"), "{}", "回复你"); err != nil {
		t.Fatalf("插入消息失败: %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO person_info (is_known, person_id, person_name, name_reason, platform, user_id,
			user_nickname, group_cardname, memory_points, know_counts, first_known_time, last_known_time)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		1, "p1", strPtr("小明"), strPtr("用户自称"), "qq", "U1", "小明", strPtr("群名片"),
		strPtr("喜欢猫"), 3, formatTransferTime(testBase), formatTransferTime(testBase),
	); err != nil {
		t.Fatalf("插入人物失败: %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO expressions (situation, style, content_list, count, last_active_time, create_time,
			session_id, checked, modified_by) VALUES (?,?,?,?,?,?,?,?,?)`,
		"被夸奖", "谦逊", `["哪里哪里","还差得远"]`, 5,
		formatTransferTime(testBase), formatTransferTime(testBase), nil, 1, nil,
	); err != nil {
		t.Fatalf("插入表达失败: %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO jargons (content, evidence_messages, meaning, session_id_dict, count, is_jargon,
			is_complete, is_global, last_inference_count, created_by, created_timestamp, updated_timestamp)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"打豆豆", strPtr(`[{"message_id":"M1"}]`), "指玩游戏", `{"g:1":2}`, 2, 1,
		0, 0, 1, "AI", formatTransferTime(testBase), formatTransferTime(testBase),
	); err != nil {
		t.Fatalf("插入黑话失败: %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO images (image_hash, description, full_path, image_type, query_count, is_registered,
			is_banned, no_file_flag, record_time, register_time, last_used_time, vlm_processed)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"hash-a", "一只橘猫", "data/emoji/a.png", "emoji", 4, 1, 0, 0,
		formatTransferTime(testBase), formatTransferTime(testBase), nil, 1,
	); err != nil {
		t.Fatalf("插入图片失败: %v", err)
	}
}

// 全量导出 → 导入到空库的往返（数据等值）。
func TestExportImportRoundTrip(t *testing.T) {
	source := testDB(t)
	seed(t, source)
	exporter := NewDatabaseStore(source)

	ctx := context.Background()
	bundle, err := Export(ctx, exporter, ExportOptions{
		Version: "test-1",
		Now:     func() time.Time { return testBase },
	})
	if err != nil {
		t.Fatalf("Export 失败: %v", err)
	}
	if bundle.Format != Format || bundle.FormatVersion != FormatVersion {
		t.Fatalf("导出包格式不符: %+v", bundle)
	}
	if bundle.ExportedAt != testBase.UTC().Format(time.RFC3339) {
		t.Fatalf("导出时间不符: %s", bundle.ExportedAt)
	}
	if len(bundle.Included) != len(AllSections) {
		t.Fatalf("默认应导出全部分片: %v", bundle.Included)
	}
	for _, section := range AllSections {
		if bundle.Counts[section] == 0 {
			t.Fatalf("分片 %s 应含数据", section)
		}
	}
	if bundle.Counts[SectionMessages] != 2 || bundle.Counts[SectionPersons] != 1 ||
		bundle.Counts[SectionExpressions] != 1 || bundle.Counts[SectionJargons] != 1 ||
		bundle.Counts[SectionImages] != 1 {
		t.Fatalf("分片计数不符: %+v", bundle.Counts)
	}

	payload, err := MarshalJSONBytes(bundle)
	if err != nil {
		t.Fatalf("MarshalJSONBytes 失败: %v", err)
	}
	parsed, err := ParseBundle(payload)
	if err != nil {
		t.Fatalf("ParseBundle 失败: %v", err)
	}

	target := testDB(t)
	importer := NewDatabaseStore(target)
	result, err := Import(ctx, importer, parsed, ImportOptions{})
	if err != nil {
		t.Fatalf("Import 失败: %v", err)
	}
	if result.Total != 6 || result.Skipped != 0 || result.Updated != 0 {
		t.Fatalf("导入统计不符: %+v", result)
	}
	for _, section := range AllSections {
		if stat := result.Sections[section]; stat.Imported != bundle.Counts[section] {
			t.Fatalf("分片 %s 导入数不符: %+v", section, stat)
		}
	}

	// 用第二份导出比对全部记录等值（忽略 id 等库内自增字段）。
	reExported, err := Export(ctx, importer, ExportOptions{Now: func() time.Time { return testBase }})
	if err != nil {
		t.Fatalf("二次 Export 失败: %v", err)
	}
	assertPayloadEqual(t, bundle.Data, reExported.Data)
}

// 导入幂等：重复导入同包应全部 skipped，overwrite 时 updated。
func TestImportIdempotentAndOverwrite(t *testing.T) {
	source := testDB(t)
	seed(t, source)
	ctx := context.Background()
	bundle, err := Export(ctx, NewDatabaseStore(source), ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}

	target := testDB(t)
	importer := NewDatabaseStore(target)
	if _, err := Import(ctx, importer, bundle, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	second, err := Import(ctx, importer, bundle, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Total != 0 || second.Skipped != 6 {
		t.Fatalf("重复导入应全部跳过: %+v", second)
	}

	// 修改一条记录并用 overwrite 覆盖；同时追加一条新记录验证 Imported。
	modified := *bundle
	modified.Data = bundle.Data
	modified.Data.Persons = append(append([]PersonRecord(nil), bundle.Data.Persons...), PersonRecord{
		IsKnown: true, PersonID: "p2", Platform: "qq", UserID: "U2", UserNickname: "小红",
	})
	modified.Counts = map[string]int{}
	for _, section := range AllSections {
		modified.Counts[section] = modified.Data.SectionCount(section)
	}
	modified.Data.Persons[0].KnowCounts = 99
	overwritten, err := Import(ctx, importer, &modified, ImportOptions{Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if overwritten.Updated != 6 || overwritten.Total != 1 || overwritten.Skipped != 0 {
		t.Fatalf("覆盖导入统计不符: %+v", overwritten)
	}
	var knowCounts int
	if err := target.Read().QueryRow(`SELECT know_counts FROM person_info WHERE person_id = 'p1'`).Scan(&knowCounts); err != nil {
		t.Fatal(err)
	}
	if knowCounts != 99 {
		t.Fatalf("覆盖后 know_counts 应为 99, got %d", knowCounts)
	}
}

// 分片选择：只导/只导部分分片。
func TestSelectiveSections(t *testing.T) {
	source := testDB(t)
	seed(t, source)
	ctx := context.Background()
	bundle, err := Export(ctx, NewDatabaseStore(source), ExportOptions{
		Sections: []string{SectionPersons, SectionJargons},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Included) != 2 || bundle.Included[0] != SectionPersons || bundle.Included[1] != SectionJargons {
		t.Fatalf("分片选择不符: %v", bundle.Included)
	}
	if bundle.Counts[SectionMessages] != 0 || bundle.Data.SectionCount(SectionMessages) != 0 {
		t.Fatal("未选分片不应含数据")
	}
	target := testDB(t)
	result, err := Import(ctx, NewDatabaseStore(target), bundle, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Sections[SectionMessages]; ok {
		t.Fatal("未导出分片不应出现在导入结果")
	}
	if result.Total != 2 {
		t.Fatalf("应导入 2 条, got %+v", result)
	}
	var messageCount int
	if err := target.Read().QueryRow(`SELECT COUNT(*) FROM mai_messages`).Scan(&messageCount); err != nil {
		t.Fatal(err)
	}
	if messageCount != 0 {
		t.Fatal("未选分片不应写入数据库")
	}
}

// 非法输入与格式校验。
func TestParseBundleValidation(t *testing.T) {
	if _, err := ParseBundle([]byte("not json")); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if _, err := ParseBundle([]byte(`{"format":"other","format_version":1}`)); err == nil {
		t.Fatal("格式不符应报错")
	}
	if _, err := ParseBundle([]byte(`{"format":"maibot-data-archive","format_version":2}`)); err == nil {
		t.Fatal("版本不符应报错")
	}
	// 声明计数与数据体不一致。
	bad := map[string]any{
		"format":         Format,
		"format_version": FormatVersion,
		"included":       []string{SectionPersons},
		"counts":         map[string]int{SectionPersons: 5},
		"data":           Payload{Persons: []PersonRecord{{PersonID: "p1"}}},
	}
	payload, _ := json.Marshal(bad)
	if _, err := ParseBundle(payload); err == nil {
		t.Fatal("计数校验失败应报错")
	}
}

func TestExportRejectsUnknownSection(t *testing.T) {
	if _, err := Export(context.Background(), NewDatabaseStore(testDB(t)), ExportOptions{
		Sections: []string{"unknown"},
	}); err == nil {
		t.Fatal("未知分片应报错")
	}
	if _, err := Import(context.Background(), NewDatabaseStore(testDB(t)), &Bundle{
		Format: Format, FormatVersion: FormatVersion, Included: []string{"unknown"},
	}, ImportOptions{}); err == nil {
		t.Fatal("未知分片导入应报错")
	}
}

func TestStoreRequiredForExportImport(t *testing.T) {
	if _, err := Export(context.Background(), nil, ExportOptions{}); err == nil {
		t.Fatal("缺少 store 应报错")
	}
	if _, err := Import(context.Background(), nil, &Bundle{}, ImportOptions{}); err == nil {
		t.Fatal("缺少 store 应报错")
	}
}

// limit 生效。
func TestExportLimitPerSection(t *testing.T) {
	source := testDB(t)
	seed(t, source)
	bundle, err := Export(context.Background(), NewDatabaseStore(source), ExportOptions{
		Sections:        []string{SectionMessages},
		LimitPerSection: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Data.Messages) != 1 || bundle.Counts[SectionMessages] != 1 {
		t.Fatalf("limit 未生效: %+v", bundle.Counts)
	}
}

// 缺字段的记录应报错（导入校验）。
func TestImportRejectsInvalidRecords(t *testing.T) {
	target := testDB(t)
	store := NewDatabaseStore(target)
	if _, err := store.ImportMessages(context.Background(), []MessageRecord{{}}, false); err == nil {
		t.Fatal("缺少 message_id 应报错")
	}
	if _, err := store.ImportPersons(context.Background(), []PersonRecord{{}}, false); err == nil {
		t.Fatal("缺少 person_id 应报错")
	}
	if _, err := store.ImportExpressions(context.Background(), []ExpressionRecord{{}}, false); err == nil {
		t.Fatal("缺少 situation/style 应报错")
	}
	if _, err := store.ImportJargons(context.Background(), []JargonRecord{{}}, false); err == nil {
		t.Fatal("缺少 content 应报错")
	}
	if _, err := store.ImportImages(context.Background(), []ImageRecord{{}}, false); err == nil {
		t.Fatal("缺少 image_hash 应报错")
	}
}

// 时间解析兼容 RFC3339 与数据库格式。
func TestParseTransferTimeLayouts(t *testing.T) {
	for _, raw := range []string{
		"2026-03-01T12:00:00+08:00",
		"2026-03-01T12:00:00Z",
		"2026-03-01 12:00:00",
		"2026-03-01",
	} {
		if _, err := parseTransferTime(raw); err != nil {
			t.Fatalf("时间 %q 应可解析: %v", raw, err)
		}
	}
	if parsed, err := parseTransferTime(""); err != nil || !parsed.IsZero() {
		t.Fatalf("空时间应返回零值: %v %v", parsed, err)
	}
	if _, err := parseTransferTime("不是时间"); err == nil {
		t.Fatal("非法时间应报错")
	}
}

func TestSortSectionsStableOrder(t *testing.T) {
	sorted := SortSections([]string{SectionImages, SectionMessages, SectionJargons})
	want := []string{SectionMessages, SectionJargons, SectionImages}
	for index := range want {
		if sorted[index] != want[index] {
			t.Fatalf("分片顺序不符: %v", sorted)
		}
	}
}

func TestSectionLabelAndCounts(t *testing.T) {
	if SectionLabel(SectionMessages) != "消息" || SectionLabel(SectionPersons) != "人物" ||
		SectionLabel(SectionExpressions) != "表达" || SectionLabel(SectionJargons) != "黑话" ||
		SectionLabel(SectionImages) != "图片描述" || SectionLabel("x") != "x" {
		t.Fatal("分片名文案不符")
	}
	payload := Payload{Messages: []MessageRecord{{MessageID: "m"}}}
	if payload.SectionCount(SectionMessages) != 1 || payload.SectionCount("x") != 0 {
		t.Fatal("SectionCount 不符")
	}
	if counts := payload.Counts(); len(counts) != 1 || counts[SectionMessages] != 1 {
		t.Fatalf("Counts 应只含非空分片: %v", counts)
	}
}

// assertPayloadEqual 比较两个数据体（按 JSON 语义，忽略切片 nil/空差异）。
func assertPayloadEqual(t *testing.T, want, got Payload) {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	// 记录顺序必须稳定（导出按主键升序）。
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("往返数据不等值:\nwant=%s\ngot =%s", wantJSON, gotJSON)
	}
}

// 全量往返的字段级断言（消息 raw_content / 人物 memory_points / 表达 content_list 保留）。
func TestRoundTripFieldFidelity(t *testing.T) {
	source := testDB(t)
	seed(t, source)
	ctx := context.Background()
	bundle, err := Export(ctx, NewDatabaseStore(source), ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	target := testDB(t)
	if _, err := Import(ctx, NewDatabaseStore(target), bundle, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	var rawContent, plainText string
	var replyFrequency float64
	if err := target.Read().QueryRow(
		`SELECT raw_content, processed_plain_text, reply_frequency FROM mai_messages WHERE message_id = 'M1'`).
		Scan(&rawContent, &plainText, &replyFrequency); err != nil {
		t.Fatal(err)
	}
	if rawContent != `{"components":[{"type":"text","data":{"text":"你好"}}]}` || plainText != "你好" {
		t.Fatalf("消息字段保真失败: %q %q", rawContent, plainText)
	}
	if replyFrequency != 0.5 {
		t.Fatalf("reply_frequency 保真失败: %v", replyFrequency)
	}
	var memoryPoints, cardname string
	if err := target.Read().QueryRow(
		`SELECT memory_points, group_cardname FROM person_info WHERE person_id = 'p1'`).
		Scan(&memoryPoints, &cardname); err != nil {
		t.Fatal(err)
	}
	if memoryPoints != "喜欢猫" || cardname != "群名片" {
		t.Fatalf("人物字段保真失败: %q %q", memoryPoints, cardname)
	}
	var contentList string
	if err := target.Read().QueryRow(
		`SELECT content_list FROM expressions WHERE situation = '被夸奖'`).Scan(&contentList); err != nil {
		t.Fatal(err)
	}
	if contentList != `["哪里哪里","还差得远"]` {
		t.Fatalf("表达字段保真失败: %q", contentList)
	}
	var isJargon int
	var meaning string
	if err := target.Read().QueryRow(
		`SELECT is_jargon, meaning FROM jargons WHERE content = '打豆豆'`).Scan(&isJargon, &meaning); err != nil {
		t.Fatal(err)
	}
	if isJargon != 1 || meaning != "指玩游戏" {
		t.Fatalf("黑话字段保真失败: %d %q", isJargon, meaning)
	}
	var description string
	var vlmProcessed int
	if err := target.Read().QueryRow(
		`SELECT description, vlm_processed FROM images WHERE image_hash = 'hash-a' AND image_type = 'emoji'`).
		Scan(&description, &vlmProcessed); err != nil {
		t.Fatal(err)
	}
	if description != "一只橘猫" || vlmProcessed != 1 {
		t.Fatalf("图片描述保真失败: %q %d", description, vlmProcessed)
	}
}
