package retrieval

import (
	"math"
	"sort"
)

// ThresholdConfig 对齐 Python ThresholdConfig。
// 出处：core/retrieval/threshold.py L29-L48。
type ThresholdConfig struct {
	MinThreshold  float64
	MaxThreshold  float64
	Percentile    float64
	StdMultiplier float64
	MinResults    int
}

// DefaultThresholdConfig 返回照抄 Python 的默认阈值配置。
func DefaultThresholdConfig() ThresholdConfig {
	return ThresholdConfig{
		MinThreshold:  DefaultThresholdMin,
		MaxThreshold:  DefaultThresholdMax,
		Percentile:    DefaultThresholdPercentile,
		StdMultiplier: DefaultThresholdStdMult,
		MinResults:    DefaultThresholdMinResults,
	}
}

// DynamicThresholdFilter 是自适应阈值过滤器（ADAPTIVE 模式）。
// 出处：core/retrieval/threshold.py L76-L167。
type DynamicThresholdFilter struct {
	Config ThresholdConfig
}

// NewDynamicThresholdFilter 构造过滤器。
func NewDynamicThresholdFilter(config ThresholdConfig) *DynamicThresholdFilter {
	if config.MaxThreshold <= config.MinThreshold {
		config = DefaultThresholdConfig()
	}
	return &DynamicThresholdFilter{Config: config}
}

// Filter 按自适应阈值过滤；不足 min_results 时按分数补齐。
// 出处：core/retrieval/threshold.py L111-L160。
func (f *DynamicThresholdFilter) Filter(results []*Result) []*Result {
	if len(results) == 0 {
		return results
	}
	scores := make([]float64, 0, len(results))
	for _, item := range results {
		scores = append(scores, item.Score)
	}
	threshold := f.computeThreshold(scores)
	filtered := make([]*Result, 0, len(results))
	for _, item := range results {
		if item.Score >= threshold {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) < f.Config.MinResults {
		sorted := make([]*Result, len(results))
		copy(sorted, results)
		sortResults(sorted)
		limit := f.Config.MinResults
		if limit > len(sorted) {
			limit = len(sorted)
		}
		filtered = sorted[:limit]
	}
	return filtered
}

// computeThreshold 计算自适应阈值（percentile / std_dev / gap 的中位数，再裁剪到 [min,max]）。
// 出处：core/retrieval/threshold.py L167-L220。
func (f *DynamicThresholdFilter) computeThreshold(scores []float64) float64 {
	percentileThreshold := percentile(scores, f.Config.Percentile)
	stdDevThreshold := mean(scores) - f.Config.StdMultiplier*stdDev(scores)
	gapThreshold := gapDetectionThreshold(scores)
	gapCap := percentile(scores, 40.0)
	adaptiveGap := gapThreshold
	if gapCap < adaptiveGap {
		adaptiveGap = gapCap
	}
	candidates := []float64{percentileThreshold, stdDevThreshold, adaptiveGap}
	sort.Float64s(candidates)
	threshold := candidates[len(candidates)/2]
	if threshold < f.Config.MinThreshold {
		threshold = f.Config.MinThreshold
	}
	if threshold > f.Config.MaxThreshold {
		threshold = f.Config.MaxThreshold
	}
	return threshold
}

// percentile 线性插值百分位（对齐 numpy.percentile 默认 linear 插值）。
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	if len(sorted) == 1 {
		return sorted[0]
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	position := p / 100.0 * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	weight := position - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

func stdDev(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	avg := mean(values)
	total := 0.0
	for _, v := range values {
		diff := v - avg
		total += diff * diff
	}
	return math.Sqrt(total / float64(len(values)))
}

// gapDetectionThreshold 找分数分布最大断层两端的中点。
// 出处：core/retrieval/threshold.py L261-L296。
func gapDetectionThreshold(scores []float64) float64 {
	if len(scores) == 0 {
		return 0
	}
	sorted := make([]float64, len(scores))
	copy(sorted, scores)
	sort.Sort(sort.Reverse(sort.Float64Slice(sorted)))
	if len(sorted) < 2 {
		return sorted[0]
	}
	maxGapIndex := 0
	maxDrop := math.Inf(-1)
	for i := 0; i+1 < len(sorted); i++ {
		drop := sorted[i] - sorted[i+1]
		if drop > maxDrop {
			maxDrop = drop
			maxGapIndex = i
		}
	}
	return (sorted[maxGapIndex] + sorted[maxGapIndex+1]) / 2.0
}
