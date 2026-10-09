// Package chat/statistics 实现聊天与 LLM 用量统计聚合：
//
//	参考/MaiBot/src/chat/utils/statistic.py（StatisticOutput 的统计面）
//	参考/MaiBot/src/services/statistics_service.py（summary/model/hourly/daily/activity）
//	参考/MaiBot/src/services/statistics_aggregation_service.py（按会话消息数聚合）
//	参考/MaiBot/src/webui/schemas/statistics.py（响应字段名）
//
// 与 Python 版的差异：
//   - 消息量/回复量/触发率统计从内部数据库读取（mai_messages），不再依赖聚合汇总表；
//     statistics_message_hourly / statistics_model_hourly 这类增量表在 Go 版不落地。
//   - 输出结构直接对齐 WebUI statistics 路由需要的 JSON 字段（新增按会话与按天的
//     消息量/回复量/触发率三个看板维度）。
//
// 依赖边界：本包只依赖 internal/database（不 import internal/config、internal/qq、
// internal/chat 其他子包）。
package statistics

import (
	"context"
	"fmt"
	"strings"
	"time"

	"oh-my-qqbot/internal/database"
)

// 默认窗口（对齐 statistics_service 的默认值）。
const (
	// DefaultHours mirrors hours=24 的默认窗口。
	DefaultHours = 24
	// DefaultDays 是「按天」看板的默认天数（对齐 get_daily_statistics 的 7 天缓存窗口）。
	DefaultDays = 7
	// MaxDays 限制按天查询范围，避免全表扫描。
	MaxDays = 90
	// DefaultTopSessions 是会话排行的默认条数。
	DefaultTopSessions = 50
	// DefaultRecentActivity mirrors get_recent_activity(limit=10)。
	DefaultRecentActivity = 10
)

// timeLayout 与 internal/database 的 DATETIME 存储格式保持一致。
const timeLayout = "2006-01-02 15:04:05.999999"

// formatTime 按数据库格式编码时间（避免 import internal/database 的私有函数）。
func formatTime(t time.Time) string { return t.Format(timeLayout) }

// Options 是统计服务参数。
type Options struct {
	// DB 是数据库（nil 时仅统计 Records 注入的内存记录）。
	DB *database.DB
	// Records 是额外的消息记录源（可空；用于测试与进程内数据）。
	Records RecordSource
	// Now 是时钟（nil 时使用 time.Now）。
	Now func() time.Time
	// OnlineTimeProvider 返回进程在线时长（nil 时按 0 计）。
	OnlineTimeProvider func() time.Duration
	// BotUserIDs 是机器人自身账号 id 列表（mirrors utils.is_bot_self 的账号集合）。
	// 用于从 mai_messages 中区分「回复量」与「用户消息量」；为空时回复量按 0 计。
	BotUserIDs []string
	// Location 是按天/按小时分桶所用时区（nil 时使用 time.Local）。
	Location *time.Location
}

// Service 提供统计聚合查询。
type Service struct {
	db         *database.DB
	records    RecordSource
	now        func() time.Time
	onlineTime func() time.Duration
	botUserIDs []string
	location   *time.Location
}

// New 构造统计服务。
func New(opts Options) *Service {
	service := &Service{
		db:         opts.DB,
		records:    opts.Records,
		now:        opts.Now,
		onlineTime: opts.OnlineTimeProvider,
		botUserIDs: append([]string(nil), opts.BotUserIDs...),
		location:   opts.Location,
	}
	if service.now == nil {
		service.now = time.Now
	}
	if service.onlineTime == nil {
		service.onlineTime = func() time.Duration { return 0 }
	}
	if service.location == nil {
		service.location = time.Local
	}
	return service
}

// SetBotUserIDs 更新机器人账号集合（热重载配置后由装配层调用）。
func (s *Service) SetBotUserIDs(userIDs []string) {
	s.botUserIDs = append([]string(nil), userIDs...)
}

// botMarker 返回 SQL 条件片段与参数：`AND user_id IN (?,?,...)` 形式的判定前缀。
// 账号列表为空时返回 "1=0"，即回复量恒为 0（宁可少算不误算）。
func (s *Service) botCondition() (string, []any) {
	ids := make([]string, 0, len(s.botUserIDs))
	seen := map[string]bool{}
	for _, raw := range s.botUserIDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return "1=0", nil
	}
	placeholders := "?" + strings.Repeat(",?", len(ids)-1)
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return "user_id IN (" + placeholders + ")", args
}

// Available 报告数据源是否可用（DB 或内存记录源至少一个存在）。
func (s *Service) Available() bool { return s.db != nil || s.records != nil }

// ---------------------------------------------------------------------------
// 响应结构（字段名对齐 webui/schemas/statistics.py 与 Go 版 WebUI 路由）
// ---------------------------------------------------------------------------

// Summary mirrors StatisticsSummary（裁剪：消息量/回复量/触发率为 Go 版新增面）。
type Summary struct {
	TotalRequests       int64    `json:"total_requests"`
	TotalCost           float64  `json:"total_cost"`
	TotalTokens         int64    `json:"total_tokens"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CacheHitTokens      int64    `json:"cache_hit_tokens"`
	CacheMissTokens     int64    `json:"cache_miss_tokens"`
	CacheHitRate        *float64 `json:"cache_hit_rate"`
	ChatCacheHitTokens  int64    `json:"chat_cache_hit_tokens"`
	ChatCacheMissTokens int64    `json:"chat_cache_miss_tokens"`
	ChatCacheHitRate    *float64 `json:"chat_cache_hit_rate"`
	OnlineTime          float64  `json:"online_time"`
	TotalMessages       int64    `json:"total_messages"`
	TotalReplies        int64    `json:"total_replies"`
	ReplyRate           float64  `json:"reply_rate"`
	TriggerRate         float64  `json:"trigger_rate"`
	AvgResponseTime     float64  `json:"avg_response_time"`
	CostPerHour         float64  `json:"cost_per_hour"`
	TokensPerHour       float64  `json:"tokens_per_hour"`
}

// ModelStat mirrors ModelStatistics。
type ModelStat struct {
	ModelName       string   `json:"model_name"`
	RequestCount    int64    `json:"request_count"`
	TotalCost       float64  `json:"total_cost"`
	TotalTokens     int64    `json:"total_tokens"`
	InputTokens     int64    `json:"input_tokens"`
	OutputTokens    int64    `json:"output_tokens"`
	CacheHitTokens  int64    `json:"cache_hit_tokens"`
	CacheMissTokens int64    `json:"cache_miss_tokens"`
	CacheHitRate    *float64 `json:"cache_hit_rate"`
	AvgResponseTime float64  `json:"avg_response_time"`
}

// TimeSeriesPoint mirrors TimeSeriesData（hourly_data / daily_data 的元素）。
type TimeSeriesPoint struct {
	Timestamp       string  `json:"timestamp"`
	OnlineSeconds   float64 `json:"online_seconds"`
	Requests        int64   `json:"requests"`
	Cost            float64 `json:"cost"`
	Tokens          int64   `json:"tokens"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CacheHitTokens  int64   `json:"cache_hit_tokens"`
	CacheMissTokens int64   `json:"cache_miss_tokens"`
}

// ActivityItem mirrors get_recent_activity 的条目（字段名与 Python 版一致）。
type ActivityItem struct {
	Timestamp       string  `json:"timestamp"`
	Model           string  `json:"model"`
	RequestType     string  `json:"request_type"`
	Tokens          int64   `json:"tokens"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CacheHitTokens  int64   `json:"cache_hit_tokens"`
	CacheMissTokens int64   `json:"cache_miss_tokens"`
	Cost            float64 `json:"cost"`
	TimeCost        float64 `json:"time_cost"`
}

// SessionMessageStat 是按会话聚合的消息量/回复量/触发率。
type SessionMessageStat struct {
	SessionID      string  `json:"session_id"`
	SessionName    string  `json:"session_name"`
	ChatType       string  `json:"chat_type"`
	MessageCount   int64   `json:"message_count"`
	UserMessages   int64   `json:"user_messages"`
	ReplyCount     int64   `json:"reply_count"`
	ReplyRate      float64 `json:"reply_rate"`
	TriggerRate    float64 `json:"trigger_rate"`
	AtCount        int64   `json:"at_count"`
	MentionedCount int64   `json:"mentioned_count"`
	CommandCount   int64   `json:"command_count"`
	EmojiCount     int64   `json:"emoji_count"`
	PictureCount   int64   `json:"picture_count"`
	LastMessageAt  string  `json:"last_message_at"`
	LLMRequests    int64   `json:"llm_requests"`
	LLMTokens      int64   `json:"llm_tokens"`
	LLMCost        float64 `json:"llm_cost"`
}

// DailyMessageStat 是按天聚合的消息量/回复量/触发率。
type DailyMessageStat struct {
	Date         string  `json:"date"`
	Day          string  `json:"day"`
	MessageCount int64   `json:"message_count"`
	UserMessages int64   `json:"user_messages"`
	ReplyCount   int64   `json:"reply_count"`
	ReplyRate    float64 `json:"reply_rate"`
	TriggerRate  float64 `json:"trigger_rate"`
	ActiveChats  int64   `json:"active_chats"`
	LLMRequests  int64   `json:"llm_requests"`
	LLMTokens    int64   `json:"llm_tokens"`
	LLMCost      float64 `json:"llm_cost"`
}

// HourlyMessageStat 是按小时聚合的消息量（dashboard 用）。
type HourlyMessageStat struct {
	Hour         string  `json:"hour"`
	Timestamp    string  `json:"timestamp"`
	MessageCount int64   `json:"message_count"`
	UserMessages int64   `json:"user_messages"`
	ReplyCount   int64   `json:"reply_count"`
	ReplyRate    float64 `json:"reply_rate"`
	TriggerRate  float64 `json:"trigger_rate"`
}

// Dashboard mirrors DashboardData（summary + 各时间序列 + 近期活动）。
type Dashboard struct {
	Summary        Summary              `json:"summary"`
	ModelStats     []ModelStat          `json:"model_stats"`
	HourlyData     []TimeSeriesPoint    `json:"hourly_data"`
	DailyData      []TimeSeriesPoint    `json:"daily_data"`
	HourlyMessages []HourlyMessageStat  `json:"hourly_messages"`
	DailyMessages  []DailyMessageStat   `json:"daily_messages"`
	RecentActivity []ActivityItem       `json:"recent_activity"`
	Sessions       []SessionMessageStat `json:"sessions"`
	GeneratedAt    string               `json:"generated_at"`
	WindowStart    string               `json:"window_start"`
	WindowEnd      string               `json:"window_end"`
}

// ---------------------------------------------------------------------------
// 顶层查询
// ---------------------------------------------------------------------------

// GetSummary mirrors get_summary_statistics(start_time, end_time)（含消息量统计）。
func (s *Service) GetSummary(ctx context.Context, start, end time.Time) (Summary, error) {
	summary, err := s.usageSummary(ctx, start, end)
	if err != nil {
		return Summary{}, err
	}
	counts, err := s.messageCounts(ctx, start, end)
	if err != nil {
		return Summary{}, err
	}
	summary.TotalMessages = counts.MessageCount
	summary.TotalReplies = counts.ReplyCount
	summary.ReplyRate = ratio(counts.ReplyCount, counts.UserMessages)
	summary.TriggerRate = summary.ReplyRate
	summary.OnlineTime = s.onlineTime().Seconds()
	if summary.OnlineTime > 0 {
		onlineHours := summary.OnlineTime / 3600.0
		summary.CostPerHour = summary.TotalCost / onlineHours
		summary.TokensPerHour = float64(summary.TotalTokens) / onlineHours
	}
	return summary, nil
}

// GetModelStatistics mirrors get_model_statistics(start_time, end_time?)。
func (s *Service) GetModelStatistics(ctx context.Context, start, end time.Time) ([]ModelStat, error) {
	return s.modelStatistics(ctx, start, end, 10)
}

// GetHourlyStatistics mirrors get_hourly_statistics(start_time, end_time)
// （时间桶按整点补齐零值）。
func (s *Service) GetHourlyStatistics(ctx context.Context, start, end time.Time) ([]TimeSeriesPoint, error) {
	return s.timeSeries(ctx, start, end, "hour")
}

// GetDailyStatistics mirrors get_daily_statistics(start_time, end_time)
// （时间桶按天补齐零值）。
func (s *Service) GetDailyStatistics(ctx context.Context, start, end time.Time) ([]TimeSeriesPoint, error) {
	return s.timeSeries(ctx, start, end, "day")
}

// GetRecentActivity mirrors get_recent_activity。
func (s *Service) GetRecentActivity(ctx context.Context, start, end time.Time, limit int) ([]ActivityItem, error) {
	if limit <= 0 {
		limit = DefaultRecentActivity
	}
	return s.recentActivity(ctx, start, end, limit)
}

// GetSessionStatistics 按会话聚合消息量/回复量/触发率（Go 版新增面，
// 对齐 statistics_aggregation_service.fetch_message_count_by_chat_since 的语义）。
func (s *Service) GetSessionStatistics(ctx context.Context, start, end time.Time, limit int) ([]SessionMessageStat, error) {
	if limit <= 0 {
		limit = DefaultTopSessions
	}
	return s.sessionStatistics(ctx, start, end, limit)
}

// GetDailyMessageStatistics 按天聚合消息量/回复量/触发率。
func (s *Service) GetDailyMessageStatistics(ctx context.Context, start, end time.Time) ([]DailyMessageStat, error) {
	return s.dailyMessageStatistics(ctx, start, end)
}

// GetHourlyMessageStatistics 按小时聚合消息量/回复量/触发率。
func (s *Service) GetHourlyMessageStatistics(ctx context.Context, start, end time.Time) ([]HourlyMessageStat, error) {
	return s.hourlyMessageStatistics(ctx, start, end)
}

// GetDashboard mirrors get_dashboard_statistics(hours)。hours<=0 时取 24。
func (s *Service) GetDashboard(ctx context.Context, hours int) (Dashboard, error) {
	now := s.now()
	if hours <= 0 {
		hours = DefaultHours
	}
	start := now.Add(-time.Duration(hours) * time.Hour)
	dashboard := Dashboard{
		GeneratedAt: now.Format(time.RFC3339),
		WindowStart: start.Format(time.RFC3339),
		WindowEnd:   now.Format(time.RFC3339),
	}
	var err error
	if dashboard.Summary, err = s.GetSummary(ctx, start, now); err != nil {
		return Dashboard{}, err
	}
	if dashboard.ModelStats, err = s.GetModelStatistics(ctx, start, now); err != nil {
		return Dashboard{}, err
	}
	if dashboard.HourlyData, err = s.GetHourlyStatistics(ctx, start, now); err != nil {
		return Dashboard{}, err
	}
	if dashboard.DailyData, err = s.GetDailyStatistics(ctx, start, now); err != nil {
		return Dashboard{}, err
	}
	if dashboard.HourlyMessages, err = s.GetHourlyMessageStatistics(ctx, start, now); err != nil {
		return Dashboard{}, err
	}
	if dashboard.DailyMessages, err = s.GetDailyMessageStatistics(ctx, bucketStart(now, "day", s.location).AddDate(0, 0, -6), now); err != nil {
		return Dashboard{}, err
	}
	if dashboard.RecentActivity, err = s.GetRecentActivity(ctx, start, now, DefaultRecentActivity); err != nil {
		return Dashboard{}, err
	}
	if dashboard.Sessions, err = s.GetSessionStatistics(ctx, start, now, DefaultTopSessions); err != nil {
		return Dashboard{}, err
	}
	return dashboard, nil
}

// ---------------------------------------------------------------------------
// 内存记录源（测试与进程内数据）
// ---------------------------------------------------------------------------

// RecordSource 是进程内消息记录源（按会话时间的消息计数）。
// 生产路径用 database.DB；此接口让测试与上层（例如 WebUI 虚拟平台）能在不写库时
// 也参与统计聚合。
type RecordSource interface {
	// SessionCounts 返回 [start,end] 内的按会话统计（含机器人自身消息）。
	SessionCounts(start, end time.Time) ([]SessionCount, error)
	// BucketCounts 返回 [start,end] 内按时间桶（day/hour，键为 bucketKey 形态）的统计。
	BucketCounts(start, end time.Time, unit string) ([]BucketCount, error)
}

// SessionCount 是单会话的消息计数（触发率的输入）。
type SessionCount struct {
	SessionID     string
	SessionName   string
	ChatType      string
	MessageCount  int64
	UserMessages  int64
	ReplyCount    int64
	AtCount       int64
	LastMessageAt time.Time
}

// BucketCount 是单个时间桶的消息计数。
type BucketCount struct {
	Key          string
	SessionID    string
	MessageCount int64
	UserMessages int64
	ReplyCount   int64
}

// MemoryRecordSource 是 RecordSource 的内存实现。
type MemoryRecordSource struct {
	records []memoryRecord
}

type memoryRecord struct {
	sessionID   string
	sessionName string
	chatType    string
	timestamp   time.Time
	isBot       bool
	isAt        bool
}

// NewMemoryRecordSource 构造内存记录源。
func NewMemoryRecordSource() *MemoryRecordSource { return &MemoryRecordSource{} }

// Add 追加一条消息记录（isBot 标记机器人自身发言）。
func (s *MemoryRecordSource) Add(sessionID, sessionName, chatType string, timestamp time.Time, isBot, isAt bool) {
	if chatType == "" {
		if strings.Contains(sessionID, "group") || sessionName == "" {
			chatType = "group"
		}
	}
	s.records = append(s.records, memoryRecord{
		sessionID: sessionID, sessionName: sessionName, chatType: chatType,
		timestamp: timestamp, isBot: isBot, isAt: isAt,
	})
}

// SessionCounts implements RecordSource.
func (s *MemoryRecordSource) SessionCounts(start, end time.Time) ([]SessionCount, error) {
	ordered := make([]string, 0)
	byID := map[string]*SessionCount{}
	for _, record := range s.records {
		if record.timestamp.Before(start) || record.timestamp.After(end) {
			continue
		}
		item, ok := byID[record.sessionID]
		if !ok {
			item = &SessionCount{SessionID: record.sessionID, SessionName: record.sessionName, ChatType: record.chatType}
			byID[record.sessionID] = item
			ordered = append(ordered, record.sessionID)
		}
		applyRecord(item, record)
	}
	out := make([]SessionCount, 0, len(ordered))
	for _, sessionID := range ordered {
		out = append(out, *byID[sessionID])
	}
	return out, nil
}

// BucketCounts implements RecordSource（unit = "day" | "hour"）。
func (s *MemoryRecordSource) BucketCounts(start, end time.Time, unit string) ([]BucketCount, error) {
	type key struct {
		bucket    string
		sessionID string
	}
	ordered := make([]key, 0)
	byKey := map[key]*BucketCount{}
	for _, record := range s.records {
		if record.timestamp.Before(start) || record.timestamp.After(end) {
			continue
		}
		bucket := bucketKey(bucketStart(record.timestamp, unit, time.Local), unit)
		entry := key{bucket: bucket, sessionID: record.sessionID}
		item, ok := byKey[entry]
		if !ok {
			item = &BucketCount{Key: bucket, SessionID: record.sessionID}
			byKey[entry] = item
			ordered = append(ordered, entry)
		}
		item.MessageCount++
		if record.isBot {
			item.ReplyCount++
		} else {
			item.UserMessages++
		}
	}
	out := make([]BucketCount, 0, len(ordered))
	for _, entry := range ordered {
		out = append(out, *byKey[entry])
	}
	return out, nil
}

// applyRecord 把一条记录计入会话统计。
func applyRecord(item *SessionCount, record memoryRecord) {
	item.MessageCount++
	if record.isBot {
		item.ReplyCount++
	} else {
		item.UserMessages++
		if record.isAt {
			item.AtCount++
		}
	}
	if record.timestamp.After(item.LastMessageAt) {
		item.LastMessageAt = record.timestamp
	}
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// ratio mirrors 触发率/回复率：bot 发言数 ÷ 用户消息数（用户消息数 0 时为 0）。
func ratio(replyCount, userMessages int64) float64 {
	if userMessages <= 0 {
		return 0
	}
	return roundTo(roundFloat(float64(replyCount)/float64(userMessages)), 4)
}

// CacheHitRate mirrors _calculate_cache_hit_rate。
func CacheHitRate(hit, miss int64) *float64 {
	total := hit + miss
	if total <= 0 {
		return nil
	}
	value := roundTo(float64(hit)/float64(total), 4)
	return &value
}

func roundTo(value float64, digits int) float64 {
	factor := 1.0
	for i := 0; i < digits; i++ {
		factor *= 10
	}
	scaled := value * factor
	return float64(int64(scaled+0.5)) / factor
}

// normalizeWindow 校验并修正查询窗口。
func normalizeWindow(start, end time.Time) (time.Time, time.Time, error) {
	if end.IsZero() {
		end = time.Now()
	}
	if start.After(end) {
		return time.Time{}, time.Time{}, fmt.Errorf("统计窗口起点晚于终点")
	}
	return start, end, nil
}

// bucketStart 把时间对齐到小时/天（按 Service 时区）。
func bucketStart(t time.Time, unit string, location *time.Location) time.Time {
	local := t.In(location)
	if unit == "hour" {
		return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, location)
	}
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
}

// bucketKey 生成时间桶键（与 SQL strftime 输出同形）。
func bucketKey(t time.Time, unit string) string {
	if unit == "hour" {
		return t.Format("2006-01-02T15:00:00")
	}
	return t.Format("2006-01-02T00:00:00")
}

// timeSeriesLayoutFor 返回解析 llm_usage.timestamp 的候选布局。
var timeSeriesLayouts = []string{
	timeLayout,
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// parseDBTime 解析数据库时间列（按 Service 时区）。
func parseDBTime(value string, location *time.Location) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, nil
	}
	for _, layout := range timeSeriesLayouts {
		if parsed, err := time.ParseInLocation(layout, trimmed, location); err == nil {
			return parsed, nil
		}
	}
	if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return parsed, nil
	}
	return time.Time{}, fmt.Errorf("无法解析时间: %s", value)
}
