// 统计路由 — llm_usage 表聚合（按模型/任务/天），对齐 routers/statistics.py
// 的 summary / models 面（detailed 快照裁剪）。
package routers

import (
	"net/http"
	"time"
)

// ModelStat 对齐 ModelStatistics。
type ModelStat struct {
	ModelName       string  `json:"model_name"`
	RequestCount    int64   `json:"request_count"`
	TotalCost       float64 `json:"total_cost"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
	CacheHitTokens  int64   `json:"cache_hit_tokens"`
	CacheMissTokens int64   `json:"cache_miss_tokens"`
	AvgResponseTime float64 `json:"avg_response_time"`
}

// handleStatsSummary 对齐 GET /statistics/summary（时间窗口内的总量摘要）。
func handleStatsSummary(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		hours := queryInt(r, "hours", 24)
		since := time.Now().Add(-time.Duration(hours) * time.Hour)
		var requests, prompt, completion, total, cacheHit, cacheMiss int64
		var cost, avgTime float64
		err := d.DB.Read().QueryRow(`
			SELECT COUNT(*),
				COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
				COALESCE(SUM(total_tokens),0),
				COALESCE(SUM(CASE WHEN prompt_cache_enabled = 1 THEN prompt_cache_hit_tokens ELSE 0 END),0),
				COALESCE(SUM(CASE WHEN prompt_cache_enabled = 1 THEN prompt_cache_miss_tokens ELSE 0 END),0),
				COALESCE(SUM(cost),0), COALESCE(AVG(time_cost),0)
			FROM llm_usage WHERE timestamp >= ?`, formatTimeForDB(since)).Scan(
			&requests, &prompt, &completion, &total, &cacheHit, &cacheMiss, &cost, &avgTime)
		if err != nil {
			fail(w, http.StatusInternalServerError, "统计查询失败: "+err.Error())
			return
		}
		ok(w, map[string]any{
			"hours":             hours,
			"request_count":     requests,
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      total,
			"cache_hit_tokens":  cacheHit,
			"cache_miss_tokens": cacheMiss,
			"total_cost":        cost,
			"avg_response_time": avgTime,
		})
	}
}

// handleStatsModels 对齐 GET /statistics/models（按模型聚合，Top 10）。
func handleStatsModels(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		hours := queryInt(r, "hours", 24)
		since := time.Now().Add(-time.Duration(hours) * time.Hour)
		rows, err := d.DB.Read().Query(`
			SELECT COALESCE(model_assign_name, model_name, 'unknown') AS model,
				COUNT(*), COALESCE(SUM(cost),0),
				COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
				COALESCE(SUM(CASE WHEN prompt_cache_enabled = 1 THEN prompt_cache_hit_tokens ELSE 0 END),0),
				COALESCE(SUM(CASE WHEN prompt_cache_enabled = 1 THEN prompt_cache_miss_tokens ELSE 0 END),0),
				COALESCE(AVG(time_cost),0)
			FROM llm_usage WHERE timestamp >= ?
			GROUP BY model ORDER BY COUNT(*) DESC LIMIT 10`, formatTimeForDB(since))
		if err != nil {
			fail(w, http.StatusInternalServerError, "模型统计查询失败: "+err.Error())
			return
		}
		defer rows.Close()
		stats := []ModelStat{}
		for rows.Next() {
			var s ModelStat
			if err := rows.Scan(&s.ModelName, &s.RequestCount, &s.TotalCost,
				&s.InputTokens, &s.OutputTokens, &s.CacheHitTokens, &s.CacheMissTokens,
				&s.AvgResponseTime); err != nil {
				continue
			}
			s.TotalTokens = s.InputTokens + s.OutputTokens
			stats = append(stats, s)
		}
		ok(w, map[string]any{"models": stats, "hours": hours})
	}
}

// handleStatsDaily 按天聚合（Go 版补充面，供仪表盘折线）。
func handleStatsDaily(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		days := queryInt(r, "days", 7)
		if days <= 0 || days > 90 {
			days = 7
		}
		since := time.Now().AddDate(0, 0, -days)
		rows, err := d.DB.Read().Query(`
			SELECT DATE(timestamp) AS day, COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost),0)
			FROM llm_usage WHERE timestamp >= ?
			GROUP BY day ORDER BY day`, formatTimeForDB(since))
		if err != nil {
			fail(w, http.StatusInternalServerError, "按天统计查询失败: "+err.Error())
			return
		}
		defer rows.Close()
		type dailyStat struct {
			Day          string  `json:"day"`
			RequestCount int64   `json:"request_count"`
			TotalTokens  int64   `json:"total_tokens"`
			TotalCost    float64 `json:"total_cost"`
		}
		stats := []dailyStat{}
		for rows.Next() {
			var s dailyStat
			if err := rows.Scan(&s.Day, &s.RequestCount, &s.TotalTokens, &s.TotalCost); err != nil {
				continue
			}
			stats = append(stats, s)
		}
		ok(w, map[string]any{"daily": stats, "days": days})
	}
}

// formatTimeForDB 对齐 database.timeLayout（包内不 import internal/database 的
// 私有函数，保持同格式）。
func formatTimeForDB(t time.Time) string { return t.Format("2006-01-02 15:04:05.999999") }
