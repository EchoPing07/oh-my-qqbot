package statistics

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

// This file 实现从 internal/database（mai_messages / llm_usage）读取的聚合查询。
// 对齐 参考/MaiBot/src/services/statistics_service.py 的查询语义，
// 但统一走「按需聚合」而不是增量汇总表。

// usageSummary mirrors _get_summary_statistics_sync 的 llm_usage 分支。
func (s *Service) usageSummary(ctx context.Context, start, end time.Time) (Summary, error) {
	summary := Summary{}
	if s.db == nil {
		return summary, nil
	}
	row := s.db.Read().QueryRow(`
		SELECT COUNT(*),
			COALESCE(SUM(cost), 0),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(CASE WHEN prompt_cache_enabled = 1 THEN prompt_cache_hit_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN prompt_cache_enabled = 1 THEN prompt_cache_miss_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN task_name IN ('replyer','planner') AND prompt_cache_enabled = 1
				THEN prompt_cache_hit_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN task_name IN ('replyer','planner') AND prompt_cache_enabled = 1
				THEN prompt_cache_miss_tokens ELSE 0 END), 0),
			COALESCE(AVG(time_cost), 0)
		FROM llm_usage WHERE timestamp >= ? AND timestamp <= ?`,
		formatTime(start), formatTime(end),
	)
	if err := row.Scan(&summary.TotalRequests, &summary.TotalCost, &summary.InputTokens,
		&summary.OutputTokens, &summary.CacheHitTokens, &summary.CacheMissTokens,
		&summary.ChatCacheHitTokens, &summary.ChatCacheMissTokens, &summary.AvgResponseTime); err != nil {
		return Summary{}, fmt.Errorf("统计摘要查询失败: %w", err)
	}
	summary.TotalTokens = summary.InputTokens + summary.OutputTokens
	summary.CacheHitRate = CacheHitRate(summary.CacheHitTokens, summary.CacheMissTokens)
	summary.ChatCacheHitRate = CacheHitRate(summary.ChatCacheHitTokens, summary.ChatCacheMissTokens)
	return summary, nil
}

// modelStatistics mirrors _get_model_statistics_sync（Top N，按请求数降序）。
func (s *Service) modelStatistics(ctx context.Context, start, end time.Time, limit int) ([]ModelStat, error) {
	if s.db == nil {
		return []ModelStat{}, nil
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Read().Query(`
		SELECT COALESCE(model_assign_name, model_name, 'unknown') AS model_name,
			COUNT(*),
			COALESCE(SUM(cost), 0),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(CASE WHEN prompt_cache_enabled = 1 THEN prompt_cache_hit_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN prompt_cache_enabled = 1 THEN prompt_cache_miss_tokens ELSE 0 END), 0),
			COALESCE(AVG(time_cost), 0)
		FROM llm_usage WHERE timestamp >= ? AND timestamp <= ?
		GROUP BY model_name ORDER BY COUNT(*) DESC LIMIT ?`,
		formatTime(start), formatTime(end), limit)
	if err != nil {
		return nil, fmt.Errorf("模型统计查询失败: %w", err)
	}
	defer rows.Close()
	stats := []ModelStat{}
	for rows.Next() {
		var stat ModelStat
		if err := rows.Scan(&stat.ModelName, &stat.RequestCount, &stat.TotalCost,
			&stat.InputTokens, &stat.OutputTokens, &stat.CacheHitTokens,
			&stat.CacheMissTokens, &stat.AvgResponseTime); err != nil {
			return nil, fmt.Errorf("模型统计扫描失败: %w", err)
		}
		stat.TotalTokens = stat.InputTokens + stat.OutputTokens
		stat.CacheHitRate = CacheHitRate(stat.CacheHitTokens, stat.CacheMissTokens)
		stats = append(stats, stat)
	}
	return stats, rows.Err()
}

// timeSeries mirrors _get_hourly_statistics_sync / _get_daily_statistics_sync
// （含零值补齐）。
func (s *Service) timeSeries(ctx context.Context, start, end time.Time, unit string) ([]TimeSeriesPoint, error) {
	var windowStart, windowEnd time.Time
	var err error
	if windowStart, windowEnd, err = normalizeWindow(start, end); err != nil {
		return nil, err
	}
	buckets := map[string]*TimeSeriesPoint{}
	if s.db != nil {
		format := "%Y-%m-%dT%H:00:00"
		step := time.Hour
		if unit == "day" {
			format = "%Y-%m-%dT00:00:00"
			step = 0
		}
		_ = step
		rows, err := s.db.Read().Query(`
			SELECT strftime(?, timestamp) AS bucket,
				COUNT(*),
				COALESCE(SUM(cost), 0),
				COALESCE(SUM(prompt_tokens), 0),
				COALESCE(SUM(completion_tokens), 0),
				COALESCE(SUM(CASE WHEN prompt_cache_enabled = 1 THEN prompt_cache_hit_tokens ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN prompt_cache_enabled = 1 THEN prompt_cache_miss_tokens ELSE 0 END), 0)
			FROM llm_usage WHERE timestamp >= ? AND timestamp <= ?
			GROUP BY bucket ORDER BY bucket ASC`,
			format, formatTime(windowStart), formatTime(windowEnd))
		if err != nil {
			return nil, fmt.Errorf("时间序列查询失败: %w", err)
		}
		for rows.Next() {
			point := &TimeSeriesPoint{}
			if err := rows.Scan(&point.Timestamp, &point.Requests, &point.Cost,
				&point.InputTokens, &point.OutputTokens, &point.CacheHitTokens,
				&point.CacheMissTokens); err != nil {
				rows.Close()
				return nil, fmt.Errorf("时间序列扫描失败: %w", err)
			}
			point.Tokens = point.InputTokens + point.OutputTokens
			buckets[point.Timestamp] = point
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	// 补齐零值桶（对齐 Python 的 while current <= end_time 循环）。
	points := make([]TimeSeriesPoint, 0, len(buckets))
	current := bucketStart(windowStart, unit, s.location)
	last := bucketStart(windowEnd, unit, s.location)
	for !current.After(last) {
		key := bucketKey(current, unit)
		if existing, ok := buckets[key]; ok {
			points = append(points, *existing)
		} else {
			points = append(points, TimeSeriesPoint{Timestamp: key})
		}
		if unit == "hour" {
			current = current.Add(time.Hour)
		} else {
			current = current.AddDate(0, 0, 1)
		}
	}
	return points, nil
}

// recentActivity mirrors _get_recent_activity_sync。
func (s *Service) recentActivity(ctx context.Context, start, end time.Time, limit int) ([]ActivityItem, error) {
	if s.db == nil {
		return []ActivityItem{}, nil
	}
	rows, err := s.db.Read().Query(`
		SELECT timestamp, COALESCE(model_assign_name, model_name, 'unknown'),
			request_type,
			COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0),
			CASE WHEN prompt_cache_enabled = 1 THEN COALESCE(prompt_cache_hit_tokens, 0) ELSE 0 END,
			CASE WHEN prompt_cache_enabled = 1 THEN COALESCE(prompt_cache_miss_tokens, 0) ELSE 0 END,
			COALESCE(cost, 0), COALESCE(time_cost, 0)
		FROM llm_usage WHERE timestamp >= ? AND timestamp <= ?
		ORDER BY timestamp DESC LIMIT ?`,
		formatTime(start), formatTime(end), limit)
	if err != nil {
		return nil, fmt.Errorf("近期活动查询失败: %w", err)
	}
	defer rows.Close()
	items := []ActivityItem{}
	for rows.Next() {
		var item ActivityItem
		var timestamp string
		if err := rows.Scan(&timestamp, &item.Model, &item.RequestType,
			&item.InputTokens, &item.OutputTokens, &item.CacheHitTokens,
			&item.CacheMissTokens, &item.Cost, &item.TimeCost); err != nil {
			return nil, fmt.Errorf("近期活动扫描失败: %w", err)
		}
		item.Tokens = item.InputTokens + item.OutputTokens
		parsed, err := parseDBTime(timestamp, s.location)
		if err == nil && !parsed.IsZero() {
			item.Timestamp = parsed.Format(time.RFC3339)
		} else {
			item.Timestamp = timestamp
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// messageCounts 汇总窗口内的消息量/回复量（对齐 count_messages 的过滤语义）。
func (s *Service) messageCounts(ctx context.Context, start, end time.Time) (SessionCount, error) {
	counts := SessionCount{}
	sessions, err := s.sessionCounts(ctx, start, end, 0)
	if err != nil {
		return SessionCount{}, err
	}
	for _, session := range sessions {
		counts.MessageCount += session.MessageCount
		counts.UserMessages += session.UserMessages
		counts.ReplyCount += session.ReplyCount
		counts.AtCount += session.AtCount
	}
	return counts, nil
}

// sessionCounts 按会话聚合（DB + 内存记录源合并）。
func (s *Service) sessionCounts(ctx context.Context, start, end time.Time, limit int) ([]SessionCount, error) {
	merged := map[string]*SessionCount{}
	order := make([]string, 0)
	merge := func(session SessionCount) {
		item, ok := merged[session.SessionID]
		if !ok {
			item = &SessionCount{
				SessionID:   session.SessionID,
				SessionName: session.SessionName,
				ChatType:    session.ChatType,
			}
			merged[session.SessionID] = item
			order = append(order, session.SessionID)
		}
		item.MessageCount += session.MessageCount
		item.UserMessages += session.UserMessages
		item.ReplyCount += session.ReplyCount
		item.AtCount += session.AtCount
		if item.SessionName == "" {
			item.SessionName = session.SessionName
		}
		if item.ChatType == "" {
			item.ChatType = session.ChatType
		}
		if session.LastMessageAt.After(item.LastMessageAt) {
			item.LastMessageAt = session.LastMessageAt
		}
	}

	if s.db != nil {
		// mai_messages 不存「是否机器人发言」标记，按装配层注入的 BotUserIDs 判定。
		// 未注入账号时回复量恒为 0（宁可少算不误算），其余计数不受影响。
		botCondition, botArgs := s.botCondition()
		query := `
			SELECT session_id,
				COALESCE(MAX(group_name), '') AS group_name,
				COALESCE(MAX(CASE WHEN group_id IS NOT NULL AND group_id != '' THEN 'group' ELSE 'private' END), 'group'),
				COUNT(*),
				COUNT(CASE WHEN NOT (` + botCondition + `) THEN 1 END),
				COUNT(CASE WHEN ` + botCondition + ` THEN 1 END),
				COUNT(CASE WHEN is_at = 1 THEN 1 END),
				COALESCE(MAX(timestamp), '')
			FROM mai_messages WHERE timestamp >= ? AND timestamp <= ?
			GROUP BY session_id`
		args := make([]any, 0, len(botArgs)*2+2)
		args = append(args, botArgs...)
		args = append(args, botArgs...)
		args = append(args, formatTime(start), formatTime(end))
		rows, err := s.db.Read().Query(query, args...)
		if err != nil {
			return nil, fmt.Errorf("会话消息统计查询失败: %w", err)
		}
		for rows.Next() {
			var item SessionCount
			var lastTimestamp string
			var groupName string
			if err := rows.Scan(&item.SessionID, &groupName, &item.ChatType,
				&item.MessageCount, &item.UserMessages, &item.ReplyCount,
				&item.AtCount, &lastTimestamp); err != nil {
				rows.Close()
				return nil, fmt.Errorf("会话消息统计扫描失败: %w", err)
			}
			item.SessionName = groupName
			if parsed, err := parseDBTime(lastTimestamp, s.location); err == nil {
				item.LastMessageAt = parsed
			}
			merge(item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	if s.records != nil {
		sessions, err := s.records.SessionCounts(start, end)
		if err != nil {
			return nil, err
		}
		for _, session := range sessions {
			merge(session)
		}
	}

	out := make([]SessionCount, 0, len(order))
	for _, sessionID := range order {
		out = append(out, *merged[sessionID])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MessageCount == out[j].MessageCount {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].MessageCount > out[j].MessageCount
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// sessionStatistics 合并 LLM 用量后输出会话维度统计。
func (s *Service) sessionStatistics(ctx context.Context, start, end time.Time, limit int) ([]SessionMessageStat, error) {
	windowStart, windowEnd, err := normalizeWindow(start, end)
	if err != nil {
		return nil, err
	}
	sessions, err := s.sessionCounts(ctx, windowStart, windowEnd, limit)
	if err != nil {
		return nil, err
	}
	usage := map[string]struct {
		requests int64
		tokens   int64
		cost     float64
	}{}
	if s.db != nil {
		rows, err := s.db.Read().Query(`
			SELECT session_id, COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost), 0)
			FROM llm_usage WHERE timestamp >= ? AND timestamp <= ? AND session_id != ''
			GROUP BY session_id`, formatTime(windowStart), formatTime(windowEnd))
		if err != nil {
			return nil, fmt.Errorf("会话 LLM 用量查询失败: %w", err)
		}
		for rows.Next() {
			var sessionID string
			var requests, tokens int64
			var cost float64
			if err := rows.Scan(&sessionID, &requests, &tokens, &cost); err != nil {
				rows.Close()
				return nil, err
			}
			usage[sessionID] = struct {
				requests int64
				tokens   int64
				cost     float64
			}{requests, tokens, cost}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	stats := make([]SessionMessageStat, 0, len(sessions))
	for _, session := range sessions {
		item := SessionMessageStat{
			SessionID:     session.SessionID,
			SessionName:   session.SessionName,
			ChatType:      session.ChatType,
			MessageCount:  session.MessageCount,
			UserMessages:  session.UserMessages,
			ReplyCount:    session.ReplyCount,
			ReplyRate:     ratio(session.ReplyCount, session.UserMessages),
			TriggerRate:   ratio(session.ReplyCount, session.UserMessages),
			AtCount:       session.AtCount,
			LastMessageAt: session.LastMessageAt.Format(time.RFC3339),
		}
		if item.ChatType == "" {
			item.ChatType = "group"
		}
		if item.SessionName == "" {
			item.SessionName = session.SessionID
		}
		if detail, ok := usage[session.SessionID]; ok {
			item.LLMRequests = detail.requests
			item.LLMTokens = detail.tokens
			item.LLMCost = detail.cost
		}
		stats = append(stats, item)
	}
	return stats, nil
}

// dailyMessageStatistics 按天聚合消息量/回复量/触发率（零值补齐）。
func (s *Service) dailyMessageStatistics(ctx context.Context, start, end time.Time) ([]DailyMessageStat, error) {
	windowStart, windowEnd, err := normalizeWindow(start, end)
	if err != nil {
		return nil, err
	}
	buckets := map[string]*DailyMessageStat{}
	if s.db != nil {
		botCondition, botArgs := s.botCondition()
		query := `
			SELECT strftime('%Y-%m-%dT00:00:00', timestamp) AS day,
				COUNT(*),
				COUNT(CASE WHEN NOT (` + botCondition + `) THEN 1 END),
				COUNT(CASE WHEN ` + botCondition + ` THEN 1 END),
				COUNT(DISTINCT session_id)
			FROM mai_messages WHERE timestamp >= ? AND timestamp <= ?
			GROUP BY day ORDER BY day ASC`
		args := make([]any, 0, len(botArgs)*2+2)
		args = append(args, botArgs...)
		args = append(args, botArgs...)
		args = append(args, formatTime(windowStart), formatTime(windowEnd))
		rows, err := s.db.Read().Query(query, args...)
		if err != nil {
			return nil, fmt.Errorf("按天消息统计查询失败: %w", err)
		}
		for rows.Next() {
			item := &DailyMessageStat{}
			if err := rows.Scan(&item.Day, &item.MessageCount, &item.UserMessages,
				&item.ReplyCount, &item.ActiveChats); err != nil {
				rows.Close()
				return nil, fmt.Errorf("按天消息统计扫描失败: %w", err)
			}
			item.Date = item.Day
			buckets[item.Day] = item
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()

		usageRows, err := s.db.Read().Query(`
			SELECT strftime('%Y-%m-%dT00:00:00', timestamp) AS day,
				COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost), 0)
			FROM llm_usage WHERE timestamp >= ? AND timestamp <= ?
			GROUP BY day ORDER BY day ASC`,
			formatTime(windowStart), formatTime(windowEnd))
		if err != nil {
			return nil, fmt.Errorf("按天用量统计查询失败: %w", err)
		}
		for usageRows.Next() {
			var day string
			var requests, tokens int64
			var cost float64
			if err := usageRows.Scan(&day, &requests, &tokens, &cost); err != nil {
				usageRows.Close()
				return nil, err
			}
			item, ok := buckets[day]
			if !ok {
				item = &DailyMessageStat{Day: day, Date: day}
				buckets[day] = item
			}
			item.LLMRequests = requests
			item.LLMTokens = tokens
			item.LLMCost = cost
		}
		if err := usageRows.Err(); err != nil {
			usageRows.Close()
			return nil, err
		}
		usageRows.Close()
	}
	// 内存记录源（合并到按天桶）：使用 BucketCounts 保留真实消息时间。
	if s.records != nil {
		sessions, err := s.records.SessionCounts(windowStart, windowEnd)
		if err != nil {
			return nil, err
		}
		activeChats := map[string]map[string]bool{}
		for _, session := range sessions {
			day := bucketKey(bucketStart(session.LastMessageAt, "day", s.location), "day")
			if activeChats[day] == nil {
				activeChats[day] = map[string]bool{}
			}
			activeChats[day][session.SessionID] = true
		}
		bucketsFromSource, err := s.records.BucketCounts(windowStart, windowEnd, "day")
		if err != nil {
			return nil, err
		}
		for _, bucket := range bucketsFromSource {
			target, ok := buckets[bucket.Key]
			if !ok {
				target = &DailyMessageStat{Day: bucket.Key, Date: bucket.Key}
				buckets[bucket.Key] = target
			}
			target.MessageCount += bucket.MessageCount
			target.UserMessages += bucket.UserMessages
			target.ReplyCount += bucket.ReplyCount
		}
		for key, ids := range activeChats {
			target, ok := buckets[key]
			if !ok {
				target = &DailyMessageStat{Day: key, Date: key}
				buckets[key] = target
			}
			target.ActiveChats += int64(len(ids))
		}
	}

	// 零值补齐。
	out := make([]DailyMessageStat, 0, len(buckets))
	current := bucketStart(windowStart, "day", s.location)
	last := bucketStart(windowEnd, "day", s.location)
	for !current.After(last) {
		key := bucketKey(current, "day")
		if existing, ok := buckets[key]; ok {
			existing.ReplyRate = ratio(existing.ReplyCount, existing.UserMessages)
			existing.TriggerRate = existing.ReplyRate
			out = append(out, *existing)
		} else {
			out = append(out, DailyMessageStat{Day: key, Date: key})
		}
		current = current.AddDate(0, 0, 1)
	}
	return out, nil
}

// hourlyMessageStatistics 按小时聚合消息量/回复量/触发率（零值补齐）。
func (s *Service) hourlyMessageStatistics(ctx context.Context, start, end time.Time) ([]HourlyMessageStat, error) {
	windowStart, windowEnd, err := normalizeWindow(start, end)
	if err != nil {
		return nil, err
	}
	buckets := map[string]*HourlyMessageStat{}
	if s.db != nil {
		botCondition, botArgs := s.botCondition()
		query := `
			SELECT strftime('%Y-%m-%dT%H:00:00', timestamp) AS hour,
				COUNT(*),
				COUNT(CASE WHEN NOT (` + botCondition + `) THEN 1 END),
				COUNT(CASE WHEN ` + botCondition + ` THEN 1 END)
			FROM mai_messages WHERE timestamp >= ? AND timestamp <= ?
			GROUP BY hour ORDER BY hour ASC`
		args := make([]any, 0, len(botArgs)*2+2)
		args = append(args, botArgs...)
		args = append(args, botArgs...)
		args = append(args, formatTime(windowStart), formatTime(windowEnd))
		rows, err := s.db.Read().Query(query, args...)
		if err != nil {
			return nil, fmt.Errorf("按小时消息统计查询失败: %w", err)
		}
		for rows.Next() {
			item := &HourlyMessageStat{}
			if err := rows.Scan(&item.Hour, &item.MessageCount, &item.UserMessages, &item.ReplyCount); err != nil {
				rows.Close()
				return nil, fmt.Errorf("按小时消息统计扫描失败: %w", err)
			}
			item.Timestamp = item.Hour
			buckets[item.Hour] = item
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	if s.records != nil {
		bucketsFromSource, err := s.records.BucketCounts(windowStart, windowEnd, "hour")
		if err != nil {
			return nil, err
		}
		for _, bucket := range bucketsFromSource {
			item, ok := buckets[bucket.Key]
			if !ok {
				item = &HourlyMessageStat{Hour: bucket.Key, Timestamp: bucket.Key}
				buckets[bucket.Key] = item
			}
			item.MessageCount += bucket.MessageCount
			item.UserMessages += bucket.UserMessages
			item.ReplyCount += bucket.ReplyCount
		}
	}
	out := make([]HourlyMessageStat, 0, len(buckets))
	current := bucketStart(windowStart, "hour", s.location)
	last := bucketStart(windowEnd, "hour", s.location)
	for !current.After(last) {
		key := bucketKey(current, "hour")
		if existing, ok := buckets[key]; ok {
			existing.ReplyRate = ratio(existing.ReplyCount, existing.UserMessages)
			existing.TriggerRate = existing.ReplyRate
			out = append(out, *existing)
		} else {
			out = append(out, HourlyMessageStat{Hour: key, Timestamp: key})
		}
		current = current.Add(time.Hour)
	}
	return out, nil
}

// roundFloat 保留给评分/比率工具使用（math 引入占位：NaN 视为 0）。
func roundFloat(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}
