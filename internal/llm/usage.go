package llm

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	contracts "oh-my-qqbot/internal"
)

// ModelUsage mirrors the `llm_usage` table of
// 参考/MaiBot/src/common/database/database_model.py (class ModelUsage) and the
// record built by LLMUsageRecorder.record_usage_to_database in
// 参考/MaiBot/src/llm_models/utils.py.
//
// The struct is plain (no database import) so P1's internal/database can bind
// it through the UsageWriter interface below without a package cycle.
type ModelUsage struct {
	ID int64 `json:"id"`

	// Model information
	ModelName       string `json:"model_name"`        // 模型实际名称（供应商 identifier）
	ModelAssignName string `json:"model_assign_name"` // 模型分配名称（用户自定义 name）
	ProviderName    string `json:"model_api_provider_name"`

	// Request information
	SessionID   string    `json:"session_id"`
	TaskName    string    `json:"task_name"`
	RequestType string    `json:"request_type"`
	TimeCost    float64   `json:"time_cost"`
	Timestamp   time.Time `json:"timestamp"`

	// Token accounting
	PromptTokens          int  `json:"prompt_tokens"`
	CompletionTokens      int  `json:"completion_tokens"`
	TotalTokens           int  `json:"total_tokens"`
	PromptCacheEnabled    bool `json:"prompt_cache_enabled"`
	PromptCacheHitTokens  int  `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int  `json:"prompt_cache_miss_tokens"`

	// Cost in 元 (prices are 元 / M token)
	Cost float64 `json:"cost"`

	// UserID mirrors the `user_id` argument of record_usage_to_database
	// (called with "system" for chat traffic). Not a column of the Python
	// table but kept so the writer can log/segment if it needs to.
	UserID string `json:"user_id,omitempty"`
	// Success records whether the attempt that produced this usage succeeded.
	Success bool `json:"success,omitempty"`
}

// UsageWriter persists ModelUsage rows. internal/database implements this by
// writing into `llm_usage`; main wires the implementation in, which keeps this
// package compilable while P1 is still in flight.
//
// A nil writer disables persistence (the Python recorder logs and swallows
// database errors, so a failed write never fails a request).
type UsageWriter interface {
	InsertUsage(ctx context.Context, usage ModelUsage) error
}

// UsageWriterFunc adapts a function to the UsageWriter interface.
type UsageWriterFunc func(ctx context.Context, usage ModelUsage) error

// InsertUsage implements UsageWriter.
func (f UsageWriterFunc) InsertUsage(ctx context.Context, usage ModelUsage) error {
	return f(ctx, usage)
}

// CacheReportingTracker mirrors `_CACHE_REPORTING_MODELS`: providers that
// reported prompt-cache token fields at least once are remembered, so later
// responses missing the fields (some gateways omit them on zero hits) still
// count as "cache enabled" in statistics.
type CacheReportingTracker struct {
	mu      sync.Mutex
	reports map[string]bool
}

// NewCacheReportingTracker creates an empty tracker.
func NewCacheReportingTracker() *CacheReportingTracker {
	return &CacheReportingTracker{reports: map[string]bool{}}
}

// Observe records the cache-reporting behaviour of one response.
// Key mirrors the Python `(api_provider, model_identifier)` tuple.
func (t *CacheReportingTracker) Observe(provider string, modelIdentifier string, reported bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if reported {
		t.reports[cacheKey(provider, modelIdentifier)] = true
	}
}

// Known reports whether a provider/model pair ever reported cache fields.
func (t *CacheReportingTracker) Known(provider string, modelIdentifier string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reports[cacheKey(provider, modelIdentifier)]
}

func cacheKey(provider string, modelIdentifier string) string {
	return strings.TrimSpace(provider) + "\x00" + strings.TrimSpace(modelIdentifier)
}

// UsageRecorder mirrors LLMUsageRecorder: it prices a response, fills the
// `prompt_cache_enabled` flag and hands the row to the configured writer.
type UsageRecorder struct {
	Writer  UsageWriter
	Tracker *CacheReportingTracker

	// Now is the clock used for the recorded_at timestamp (injectable in tests).
	Now func() time.Time
}

// NewUsageRecorder creates a recorder; a nil writer disables persistence.
func NewUsageRecorder(writer UsageWriter) *UsageRecorder {
	return &UsageRecorder{Writer: writer, Tracker: NewCacheReportingTracker(), Now: time.Now}
}

// Record mirrors record_usage_to_database.
//
// `requestStartedAt` is the timestamp used to resolve the applicable price
// period (aligned with the Python implementation); `timeCost` is the request
// duration in seconds.
func (r *UsageRecorder) Record(ctx context.Context, params RecordParams) error {
	if r == nil || r.Writer == nil {
		return nil
	}
	recordedAt := time.Now()
	if r.Now != nil {
		recordedAt = r.Now()
	}
	period := resolvePricePeriod(params.Model, params.RequestStartedAt)
	cost := roundCost(calculateCost(params.Usage, period, params.Model))

	cacheReported := params.Usage.CacheEnabled
	r.Tracker.Observe(params.Provider.Name, params.Model.ModelIdentifier, cacheReported)
	cacheEnabled := cacheReported || r.Tracker.Known(params.Provider.Name, params.Model.ModelIdentifier)

	usage := ModelUsage{
		ModelName:             firstNonEmpty(params.Model.ModelIdentifier, params.Model.Name),
		ModelAssignName:       params.Model.Name,
		ProviderName:          params.Provider.Name,
		SessionID:             strings.TrimSpace(params.SessionID),
		TaskName:              params.TaskName,
		RequestType:           firstNonEmpty(params.RequestType, params.TaskName),
		TimeCost:              roundDurationSeconds(params.TimeCost),
		Timestamp:             recordedAt,
		PromptTokens:          params.Usage.PromptTokens,
		CompletionTokens:      params.Usage.CompletionTokens,
		TotalTokens:           params.Usage.TotalTokens,
		PromptCacheEnabled:    cacheEnabled,
		PromptCacheHitTokens:  params.Usage.CacheHitTokens,
		PromptCacheMissTokens: params.Usage.CacheMissTokens,
		Cost:                  cost,
		UserID:                firstNonEmpty(params.UserID, "system"),
		Success:               params.Success,
	}
	return r.Writer.InsertUsage(ctx, usage)
}

// RecordParams mirrors the keyword arguments of record_usage_to_database.
type RecordParams struct {
	Model            ModelInfo
	Provider         APIProvider
	Usage            contracts.Usage
	UserID           string
	RequestType      string
	TaskName         string
	SessionID        string
	TimeCost         float64
	RequestStartedAt time.Time
	Success          bool
}

// ---------------------------------------------------------------------------
// pricing (mirrors LLMUsageRecorder._calculate_input_cost / _resolve_prices)
// ---------------------------------------------------------------------------

// CalculateInputCost mirrors `_calculate_input_cost`: cache hits use
// cache_price_in, misses use price_in; missing cache accounting falls back to
// "everything is a miss".
func CalculateInputCost(usage contracts.Usage, period ModelPricePeriod) float64 {
	promptTokens := usage.PromptTokens
	hitTokens := usage.CacheHitTokens
	missTokens := usage.CacheMissTokens
	if missTokens == 0 && hitTokens > 0 {
		if derived := promptTokens - hitTokens; derived > 0 {
			missTokens = derived
		}
	}
	if hitTokens+missTokens == 0 {
		missTokens = promptTokens
	}
	cachedCost := (float64(hitTokens) / 1_000_000) * period.CachePriceIn
	uncachedCost := (float64(missTokens) / 1_000_000) * period.PriceIn
	return cachedCost + uncachedCost
}

// calculateCost mirrors record_usage_to_database: input cost + output cost.
func calculateCost(usage contracts.Usage, period ModelPricePeriod, model ModelInfo) float64 {
	inputCost := CalculateInputCost(usage, period)
	outputCost := (float64(usage.CompletionTokens) / 1_000_000) * firstNonZero(period.PriceOut, model.PriceOut)
	return inputCost + outputCost
}

// resolvePricePeriod mirrors `_resolve_prices`: pick the daily recurring period
// containing `at`, falling back to the model's base prices (expressed as a
// synthetic period).
func resolvePricePeriod(model ModelInfo, at time.Time) ModelPricePeriod {
	if len(model.PricePeriods) == 0 {
		return ModelPricePeriod{PriceIn: model.PriceIn, PriceOut: model.PriceOut, CachePriceIn: model.CachePriceIn}
	}
	if at.IsZero() {
		at = time.Now()
	}
	minutes := at.Hour()*60 + at.Minute()
	sorted := append([]ModelPricePeriod(nil), model.PricePeriods...)
	sort.SliceStable(sorted, func(left, right int) bool { return sorted[left].StartTime < sorted[right].StartTime })
	for _, period := range sorted {
		start, startOK := parseClockMinutes(period.StartTime)
		end, endOK := parseClockMinutes(period.EndTime)
		if !startOK || !endOK {
			continue
		}
		if start == end {
			continue
		}
		if start < end {
			if minutes >= start && minutes < end {
				return period
			}
			continue
		}
		// Cross-midnight window, e.g. 22:00 → 04:00.
		if minutes >= start || minutes < end {
			return period
		}
	}
	return ModelPricePeriod{PriceIn: model.PriceIn, PriceOut: model.PriceOut, CachePriceIn: model.CachePriceIn}
}

func parseClockMinutes(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if value == "24:00" {
		return 24 * 60, true
	}
	hourText, minuteText, found := strings.Cut(value, ":")
	if !found {
		return 0, false
	}
	hour, errHour := toInt(strings.TrimSpace(hourText))
	minute, errMinute := toInt(strings.TrimSpace(minuteText))
	if errHour != true || errMinute != true || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

func roundCost(value float64) float64 {
	return float64(int64(value*1_000_000+0.5)) / 1_000_000
}

func roundDurationSeconds(value float64) float64 {
	return float64(int64(value*1000+0.5)) / 1000
}

func firstNonZero(values ...float64) float64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
