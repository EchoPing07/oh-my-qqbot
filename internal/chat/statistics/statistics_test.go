package statistics

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"oh-my-qqbot/internal/database"
)

// 固定时钟基准（统计分桶断言用）。
var testBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)

func testDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertMessage(t *testing.T, db *database.DB, messageID, sessionID, userID, platform string, at time.Time, isAt bool) {
	t.Helper()
	groupID := "G1"
	_, err := db.Write().Exec(`
		INSERT OR REPLACE INTO mai_messages (id, message_id, timestamp, platform, user_id, user_nickname, group_id, group_name,
			is_at, session_id, raw_content)
		SELECT COALESCE((SELECT id FROM mai_messages WHERE message_id = ?), NULL), ?,?,?,?,?,?,?,?,?,'{}'`,
		messageID, messageID, formatTime(at), platform, userID, "用户"+userID, groupID, "测试群",
		boolToInt(isAt), sessionID)
	if err != nil {
		t.Fatalf("插入消息失败: %v", err)
	}
}

func insertUsage(t *testing.T, db *database.DB, model, task, sessionID string, at time.Time,
	promptTokens, completionTokens int64, cost, timeCost float64, cacheEnabled bool, hit, miss int64) {
	t.Helper()
	_, err := db.Write().Exec(`
		INSERT INTO llm_usage (model_name, model_assign_name, model_api_provider_name, session_id, task_name,
			request_type, time_cost, timestamp, prompt_tokens, completion_tokens, total_tokens,
			prompt_cache_enabled, prompt_cache_hit_tokens, prompt_cache_miss_tokens, cost)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		model, model, "provider", sessionID, task, "test.request", timeCost, formatTime(at),
		promptTokens, completionTokens, promptTokens+completionTokens,
		boolToInt(cacheEnabled), hit, miss, cost)
	if err != nil {
		t.Fatalf("插入用量失败: %v", err)
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// 摘要聚合：消息量 / 回复量 / 触发率 / LLM 用量。
func TestSummaryAggregation(t *testing.T) {
	db := testDB(t)
	botID := "BOT-1"
	// 3 条用户消息、1 条机器人回复、1 条 @ 消息。
	insertMessage(t, db, "m1", "g:1", "U1", "qq", testBase.Add(-10*time.Minute), false)
	insertMessage(t, db, "m2", "g:1", "U2", "qq", testBase.Add(-9*time.Minute), false)
	insertMessage(t, db, "m3", "g:1", "U3", "qq", testBase.Add(-8*time.Minute), true)
	insertMessage(t, db, "m4", "g:1", botID, "qq", testBase.Add(-7*time.Minute), false)
	insertMessage(t, db, "m5", "g:1", "U1", "qq", testBase.Add(-30*time.Hour), false) // 窗口外

	insertUsage(t, db, "m-a", "replyer", "g:1", testBase.Add(-6*time.Minute), 100, 50, 0.02, 1.5, true, 80, 20)
	insertUsage(t, db, "m-a", "utils", "g:1", testBase.Add(-5*time.Minute), 20, 10, 0.001, 0.4, true, 10, 10)
	insertUsage(t, db, "m-b", "planner", "g:1", testBase.Add(-4*time.Minute), 200, 80, 0.05, 2.0, false, 0, 0)
	insertUsage(t, db, "m-a", "replyer", "g:1", testBase.Add(-40*time.Hour), 10, 10, 9.9, 1.0, false, 0, 0) // 窗口外

	service := New(Options{
		DB:                 db,
		Now:                func() time.Time { return testBase },
		OnlineTimeProvider: func() time.Duration { return 2 * time.Hour },
		BotUserIDs:         []string{botID},
	})

	start := testBase.Add(-24 * time.Hour)
	summary, err := service.GetSummary(context.Background(), start, testBase)
	if err != nil {
		t.Fatalf("GetSummary 失败: %v", err)
	}
	if summary.TotalMessages != 4 {
		t.Fatalf("窗口内消息数应为 4, got %d", summary.TotalMessages)
	}
	if summary.TotalReplies != 1 {
		t.Fatalf("机器人消息数应为 1, got %d", summary.TotalReplies)
	}
	if summary.TotalRequests != 3 {
		t.Fatalf("窗口内请求数应为 3, got %d", summary.TotalRequests)
	}
	if summary.InputTokens != 320 || summary.OutputTokens != 140 || summary.TotalTokens != 460 {
		t.Fatalf("token 统计不符: %+v", summary)
	}
	if summary.CacheHitTokens != 90 || summary.CacheMissTokens != 30 {
		t.Fatalf("缓存 token 统计不符: %+v", summary)
	}
	if summary.ChatCacheHitTokens != 80 || summary.ChatCacheMissTokens != 20 {
		t.Fatalf("聊天任务缓存统计应只含 replyer/planner: %+v", summary)
	}
	if summary.CacheHitRate == nil || *summary.CacheHitRate != 0.75 {
		t.Fatalf("缓存命中率应为 0.75, got %v", summary.CacheHitRate)
	}
	if summary.ReplyRate != 0.3333 {
		t.Fatalf("触发率应为 1/3, got %v", summary.ReplyRate)
	}
	wantAvg := (1.5 + 0.4 + 2.0) / 3.0
	if diff := summary.AvgResponseTime - wantAvg; diff > 0.0001 || diff < -0.0001 {
		t.Fatalf("平均响应时间不符: %v want %v", summary.AvgResponseTime, wantAvg)
	}
	if summary.OnlineTime != 7200 {
		t.Fatalf("在线时间不符: %v", summary.OnlineTime)
	}
	if diff := summary.CostPerHour - (0.071 / 2.0); diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("每小时花费不符: %v", summary.CostPerHour)
	}
	if diff := summary.TokensPerHour - (460.0 / 2.0); diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("每小时 token 不符: %v", summary.TokensPerHour)
	}
}

// 按会话聚合：消息量 / 回复量 / 触发率 / LLM 用量（按消息量降序）。
func TestSessionStatisticsAggregation(t *testing.T) {
	db := testDB(t)
	botID := "BOT-1"
	for index := 0; index < 3; index++ {
		insertMessage(t, db, fmt.Sprintf("g1-%d", index), "g:1", "U1", "qq",
			testBase.Add(-time.Duration(10-index)*time.Minute), false)
	}
	insertMessage(t, db, "g1-bot", "g:1", botID, "qq", testBase.Add(-6*time.Minute), false)
	for index := 0; index < 2; index++ {
		insertMessage(t, db, fmt.Sprintf("p1-%d", index), "p:1", "U2", "qq",
			testBase.Add(-time.Duration(5-index)*time.Minute), false)
	}
	insertUsage(t, db, "m-a", "replyer", "g:1", testBase.Add(-6*time.Minute), 10, 20, 0.01, 1.0, false, 0, 0)
	insertUsage(t, db, "m-a", "utils", "p:1", testBase.Add(-5*time.Minute), 5, 5, 0.002, 0.5, false, 0, 0)

	service := New(Options{DB: db, Now: func() time.Time { return testBase }, BotUserIDs: []string{botID}})
	stats, err := service.GetSessionStatistics(context.Background(), testBase.Add(-24*time.Hour), testBase, 10)
	if err != nil {
		t.Fatalf("GetSessionStatistics 失败: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("应有 2 个会话, got %d", len(stats))
	}
	if stats[0].SessionID != "g:1" || stats[0].MessageCount != 4 || stats[0].ReplyCount != 1 ||
		stats[0].UserMessages != 3 {
		t.Fatalf("g:1 统计不符: %+v", stats[0])
	}
	if stats[0].TriggerRate != 0.3333 {
		t.Fatalf("g:1 触发率不符: %v", stats[0].TriggerRate)
	}
	if stats[0].LLMRequests != 1 || stats[0].LLMTokens != 30 {
		t.Fatalf("g:1 LLM 用量未关联: %+v", stats[0])
	}
	if stats[0].SessionName != "测试群" || stats[0].ChatType != "group" {
		t.Fatalf("g:1 会话名/类型不符: %+v", stats[0])
	}
	if stats[1].SessionID != "p:1" || stats[1].MessageCount != 2 || stats[1].ReplyCount != 0 ||
		stats[1].TriggerRate != 0 {
		t.Fatalf("p:1 统计不符: %+v", stats[1])
	}
	if stats[1].LLMRequests != 1 || stats[1].LLMCost != 0.002 {
		t.Fatalf("p:1 LLM 用量表有关联: %+v", stats[1])
	}
}

// 无机器人账号注入时，回复量恒为 0（宁可少算不误算）。
func TestSessionStatisticsWithoutBotUserIDs(t *testing.T) {
	db := testDB(t)
	insertMessage(t, db, "g1-a", "g:1", "U1", "qq", testBase.Add(-time.Minute), false)
	insertMessage(t, db, "g1-b", "g:1", "BOT-1", "qq", testBase.Add(-time.Minute), false)
	service := New(Options{DB: db, Now: func() time.Time { return testBase }})
	stats, err := service.GetSessionStatistics(context.Background(), testBase.Add(-time.Hour), testBase, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].ReplyCount != 0 || stats[0].UserMessages != 2 {
		t.Fatalf("未注入机器人账号时回复量应为 0: %+v", stats)
	}
	service.SetBotUserIDs([]string{"BOT-1"})
	stats, err = service.GetSessionStatistics(context.Background(), testBase.Add(-time.Hour), testBase, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats[0].ReplyCount != 1 || stats[0].UserMessages != 1 || stats[0].TriggerRate != 1 {
		t.Fatalf("注入机器人账号后统计不符: %+v", stats[0])
	}
}

// 按天聚合：消息量 / 回复量 / 触发率 / LLM 用量，且零值天被补齐。
func TestDailyMessageStatistics(t *testing.T) {
	db := testDB(t)
	botID := "BOT-1"
	dayOne := time.Date(2026, 2, 27, 10, 0, 0, 0, time.Local)
	dayThree := time.Date(2026, 3, 1, 9, 0, 0, 0, time.Local)
	insertMessage(t, db, "d1-a", "g:1", "U1", "qq", dayOne, false)
	insertMessage(t, db, "d1-b", "g:1", "U2", "qq", dayOne.Add(time.Minute), false)
	insertMessage(t, db, "d1-bot", "g:1", botID, "qq", dayOne.Add(2*time.Minute), false)
	insertMessage(t, db, "d3-a", "g:1", "U1", "qq", dayThree, false)
	insertUsage(t, db, "m-a", "replyer", "g:1", dayThree, 10, 20, 0.01, 1.2, false, 0, 0)

	service := New(Options{DB: db, Now: func() time.Time { return testBase }, BotUserIDs: []string{botID}})
	stats, err := service.GetDailyMessageStatistics(context.Background(), dayOne, dayThree)
	if err != nil {
		t.Fatalf("GetDailyMessageStatistics 失败: %v", err)
	}
	if len(stats) != 3 {
		t.Fatalf("应补齐 3 天, got %d", len(stats))
	}
	if stats[0].Day != "2026-02-27T00:00:00" || stats[0].MessageCount != 3 ||
		stats[0].ReplyCount != 1 || stats[0].UserMessages != 2 {
		t.Fatalf("第一天统计不符: %+v", stats[0])
	}
	if stats[0].ReplyRate != 0.5 || stats[0].TriggerRate != 0.5 {
		t.Fatalf("第一天触发率不符: %+v", stats[0])
	}
	if stats[1].MessageCount != 0 || stats[1].ReplyRate != 0 || stats[1].LLMRequests != 0 {
		t.Fatalf("空缺天应为零值: %+v", stats[1])
	}
	if stats[2].MessageCount != 1 || stats[2].LLMRequests != 1 || stats[2].LLMTokens != 30 ||
		stats[2].LLMCost != 0.01 {
		t.Fatalf("第三天统计不符: %+v", stats[2])
	}
}

// 按小时聚合与零值补齐。
func TestHourlyMessageStatistics(t *testing.T) {
	db := testDB(t)
	base := time.Date(2026, 3, 1, 9, 30, 0, 0, time.Local)
	insertMessage(t, db, "h1", "g:1", "U1", "qq", base, false)
	insertMessage(t, db, "h2", "g:1", "BOT-1", "qq", base.Add(5*time.Minute), false)
	insertMessage(t, db, "h4", "g:1", "U1", "qq", base.Add(2*time.Hour), false)

	service := New(Options{DB: db, Now: func() time.Time { return testBase }, BotUserIDs: []string{"BOT-1"}})
	stats, err := service.GetHourlyMessageStatistics(context.Background(), base, base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("GetHourlyMessageStatistics 失败: %v", err)
	}
	if len(stats) != 3 {
		t.Fatalf("应补齐 3 个小时桶, got %d", len(stats))
	}
	if stats[0].Hour != "2026-03-01T09:00:00" || stats[0].MessageCount != 2 || stats[0].ReplyCount != 1 {
		t.Fatalf("首个小时桶不符: %+v", stats[0])
	}
	if stats[0].TriggerRate != 1 {
		t.Fatalf("首个小时触发率应为 1/1, got %v", stats[0].TriggerRate)
	}
	if stats[1].MessageCount != 0 || stats[1].Hour != "2026-03-01T10:00:00" {
		t.Fatalf("中间空桶不符: %+v", stats[1])
	}
	if stats[2].MessageCount != 1 || stats[2].ReplyCount != 0 {
		t.Fatalf("末个小时桶不符: %+v", stats[2])
	}
}

// 模型统计与时间序列（llm_usage）。
func TestModelAndTimeSeriesStatistics(t *testing.T) {
	db := testDB(t)
	insertUsage(t, db, "model-a", "replyer", "g:1", testBase.Add(-2*time.Hour), 100, 20, 0.02, 1.0, true, 50, 50)
	insertUsage(t, db, "model-a", "planner", "g:1", testBase.Add(-2*time.Hour), 100, 20, 0.02, 3.0, false, 0, 0)
	insertUsage(t, db, "model-b", "utils", "g:1", testBase.Add(-1*time.Hour), 50, 10, 0.01, 0.5, false, 0, 0)

	service := New(Options{DB: db, Now: func() time.Time { return testBase }})
	models, err := service.GetModelStatistics(context.Background(), testBase.Add(-24*time.Hour), testBase)
	if err != nil {
		t.Fatalf("GetModelStatistics 失败: %v", err)
	}
	t.Logf("models=%+v", models)
	if len(models) != 2 || models[0].ModelName != "model-a" || models[0].RequestCount != 2 {
		t.Fatalf("模型统计不符: %+v", models)
	}
	if models[0].TotalTokens != 240 || models[0].CacheHitRate == nil || *models[0].CacheHitRate != 0.5 {
		t.Fatalf("model-a 统计不符: %+v", models[0])
	}
	if models[0].AvgResponseTime != 2.0 {
		t.Fatalf("model-a 平均耗时不符: %v", models[0].AvgResponseTime)
	}

	points, err := service.GetHourlyStatistics(context.Background(), testBase.Add(-3*time.Hour), testBase)
	if err != nil {
		t.Fatalf("GetHourlyStatistics 失败: %v", err)
	}
	if len(points) != 4 {
		t.Fatalf("应补齐 4 个小时桶, got %d", len(points))
	}
	if points[0].Requests != 0 || points[1].Requests != 2 || points[2].Requests != 1 || points[3].Requests != 0 {
		t.Fatalf("小时序列不符: %+v", points)
	}
	if points[1].Tokens != 240 {
		t.Fatalf("小时 token 不符: %+v", points[1])
	}

	days, err := service.GetDailyStatistics(context.Background(), testBase.AddDate(0, 0, -2), testBase)
	if err != nil {
		t.Fatalf("GetDailyStatistics 失败: %v", err)
	}
	if len(days) != 3 || days[2].Requests != 3 || days[2].Tokens != 300 {
		t.Fatalf("天序列不符: %+v", days)
	}

	activities, err := service.GetRecentActivity(context.Background(), testBase.Add(-24*time.Hour), testBase, 10)
	if err != nil {
		t.Fatalf("GetRecentActivity 失败: %v", err)
	}
	if len(activities) != 3 {
		t.Fatalf("近期活动应有 3 条, got %d", len(activities))
	}
	if activities[0].Model != "model-b" || activities[0].Tokens != 60 {
		t.Fatalf("近期活动应按时间倒序: %+v", activities[0])
	}
}

// Dashboard 组合面。
func TestDashboard(t *testing.T) {
	db := testDB(t)
	insertMessage(t, db, "m1", "g:1", "U1", "qq", testBase.Add(-time.Minute), false)
	insertUsage(t, db, "m-a", "replyer", "g:1", testBase.Add(-time.Minute), 10, 10, 0.01, 1.0, false, 0, 0)
	service := New(Options{
		DB:         db,
		Now:        func() time.Time { return testBase },
		BotUserIDs: []string{"BOT-1"},
	})
	dashboard, err := service.GetDashboard(context.Background(), 24)
	if err != nil {
		t.Fatalf("GetDashboard 失败: %v", err)
	}
	if dashboard.Summary.TotalMessages != 1 || dashboard.Summary.TotalRequests != 1 {
		t.Fatalf("dashboard 摘要不符: %+v", dashboard.Summary)
	}
	if len(dashboard.ModelStats) != 1 || len(dashboard.HourlyData) != 25 || len(dashboard.HourlyMessages) != 25 {
		t.Fatalf("dashboard 时间序列长度异常: models=%d hourly=%d hourlyMsg=%d",
			len(dashboard.ModelStats), len(dashboard.HourlyData), len(dashboard.HourlyMessages))
	}
	if len(dashboard.DailyMessages) != 7 {
		t.Fatalf("dashboard 按天消息应为 7 天, got %d", len(dashboard.DailyMessages))
	}
	if len(dashboard.Sessions) != 1 || dashboard.Sessions[0].SessionID != "g:1" {
		t.Fatalf("dashboard 会话列表不符: %+v", dashboard.Sessions)
	}
	if dashboard.GeneratedAt == "" || dashboard.WindowStart == "" || dashboard.WindowEnd == "" {
		t.Fatal("dashboard 应带生成时间与窗口")
	}
}

// 内存记录源（不依赖 DB）。
func TestMemoryRecordSourceAggregation(t *testing.T) {
	source := NewMemoryRecordSource()
	source.Add("g:1", "测试群", "group", testBase.Add(-30*time.Minute), false, false)
	source.Add("g:1", "测试群", "group", testBase.Add(-20*time.Minute), false, true)
	source.Add("g:1", "测试群", "group", testBase.Add(-10*time.Minute), true, false)
	source.Add("p:1", "私聊", "private", testBase.Add(-5*time.Minute), false, false)
	source.Add("g:1", "测试群", "group", testBase.Add(-48*time.Hour), false, false) // 窗口外

	service := New(Options{
		Records: source,
		Now:     func() time.Time { return testBase },
	})
	sessions, err := service.GetSessionStatistics(context.Background(), testBase.Add(-24*time.Hour), testBase, 10)
	if err != nil {
		t.Fatalf("GetSessionStatistics 失败: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("应有 2 个会话, got %d", len(sessions))
	}
	if sessions[0].SessionID != "g:1" || sessions[0].MessageCount != 3 || sessions[0].ReplyCount != 1 ||
		sessions[0].UserMessages != 2 || sessions[0].AtCount != 1 {
		t.Fatalf("g:1 内存统计不符: %+v", sessions[0])
	}
	if sessions[1].SessionID != "p:1" || sessions[1].MessageCount != 1 || sessions[1].ReplyCount != 0 {
		t.Fatalf("p:1 内存统计不符: %+v", sessions[1])
	}
	daily, err := service.GetDailyMessageStatistics(context.Background(), testBase.AddDate(0, 0, -2), testBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 3 || daily[2].MessageCount != 4 || daily[2].ActiveChats != 2 {
		t.Fatalf("按天内存统计不符: %+v", daily)
	}
	if daily[2].ReplyCount != 1 || daily[2].UserMessages != 3 || daily[2].TriggerRate != 0.3333 {
		t.Fatalf("按天回复/触发率不符: %+v", daily[2])
	}
}

// 无数据源时的降级行为。
func TestServiceWithoutDataSource(t *testing.T) {
	service := New(Options{Now: func() time.Time { return testBase }})
	if service.Available() {
		t.Fatal("无数据源时 Available 应为 false")
	}
	summary, err := service.GetSummary(context.Background(), testBase.Add(-time.Hour), testBase)
	if err != nil {
		t.Fatalf("空服务不应报错: %v", err)
	}
	if summary.TotalMessages != 0 || summary.TotalRequests != 0 || summary.ReplyRate != 0 {
		t.Fatalf("空服务应返回零值摘要: %+v", summary)
	}
	if points, err := service.GetHourlyStatistics(context.Background(), testBase.Add(-2*time.Hour), testBase); err != nil || len(points) != 3 {
		t.Fatalf("空服务应补齐零值桶: %v %d", err, len(points))
	}
	if hours, err := service.GetHourlyMessageStatistics(context.Background(), testBase.Add(-time.Hour), testBase); err != nil || len(hours) != 2 {
		t.Fatalf("空服务小时消息应补齐: %v %d", err, len(hours))
	}
}

func TestCacheHitRateNilWhenNoCache(t *testing.T) {
	if rate := CacheHitRate(0, 0); rate != nil {
		t.Fatalf("无缓存数据时应为 nil: %v", rate)
	}
	rate := CacheHitRate(3, 1)
	if rate == nil || *rate != 0.75 {
		t.Fatalf("命中率不符: %v", rate)
	}
}

func TestNormalizeWindowRejectsInverted(t *testing.T) {
	if _, _, err := normalizeWindow(testBase, testBase.Add(-time.Hour)); err == nil {
		t.Fatal("起点晚于终点应报错")
	}
}
