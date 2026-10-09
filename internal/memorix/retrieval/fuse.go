package retrieval

import (
	"errors"
	"math"
	"sort"
)

// NormalizeCalibrationMethod 规范化校准方法名并校验合法性。
// 出处：core/retrieval/score_calibration.py L37-L44。
func NormalizeCalibrationMethod(method string) (string, error) {
	token := method
	if token == "" {
		token = "none"
	}
	for _, valid := range ScoreCalibrationMethods {
		if token == valid {
			return token, nil
		}
	}
	return "", errors.New("memorix: 不支持的分数校准方法: " + token)
}

func finiteScoreMap(scores map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(scores))
	for key, value := range scores {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		out[key] = value
	}
	return out
}

// averageRankScores 返回并列项共享名次的降序排名分数，首名为 1。
// 出处：core/retrieval/score_calibration.py L50-L58。
func averageRankScores(scores map[string]float64) map[string]float64 {
	if len(scores) == 0 {
		return map[string]float64{}
	}
	values := make([]float64, 0, len(scores))
	seen := map[float64]struct{}{}
	for _, v := range scores {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		values = append(values, v)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(values)))
	rankByValue := make(map[float64]float64, len(values))
	for i, v := range values {
		rankByValue[v] = float64(i + 1)
	}
	out := make(map[string]float64, len(scores))
	for key, value := range scores {
		out[key] = rankByValue[value]
	}
	return out
}

// CalibrateScoreMap 将单通道的查询内分数转换到可比较范围。
// 出处：core/retrieval/score_calibration.py L61-L115。
func CalibrateScoreMap(scores map[string]float64, method string) (map[string]float64, error) {
	normalized, err := NormalizeCalibrationMethod(method)
	if err != nil {
		return nil, err
	}
	finite := finiteScoreMap(scores)
	if len(finite) == 0 || normalized == "none" {
		return finite, nil
	}
	if _, isRRF := scoreCalibrationRRFChannels[normalized]; isRRF {
		return nil, errors.New("memorix: RRF 方法需要通过 FuseScoreMaps 融合通道")
	}

	values := make([]float64, 0, len(finite))
	lower, upper := math.Inf(1), math.Inf(-1)
	for _, v := range finite {
		values = append(values, v)
		if v < lower {
			lower = v
		}
		if v > upper {
			upper = v
		}
	}
	if upper-lower < 1e-12 {
		out := make(map[string]float64, len(finite))
		for key := range finite {
			out[key] = 1.0
		}
		return out, nil
	}

	switch normalized {
	case "minmax":
		scale := upper - lower
		out := make(map[string]float64, len(finite))
		for key, value := range finite {
			out[key] = (value - lower) / scale
		}
		return out, nil
	case "rank_percentile":
		ranks := averageRankScores(finite)
		distinct := len(values)
		if distinct <= 1 {
			out := make(map[string]float64, len(finite))
			for key := range finite {
				out[key] = 1.0
			}
			return out, nil
		}
		out := make(map[string]float64, len(finite))
		for key, rank := range ranks {
			out[key] = 1.0 - (rank-1.0)/float64(distinct-1)
		}
		return out, nil
	}

	center := median(values)
	deviations := make([]float64, 0, len(values))
	for _, v := range values {
		deviations = append(deviations, math.Abs(v-center))
	}
	robustScale := 1.4826 * median(deviations)
	if robustScale < 1e-12 {
		robustScale = math.Max((upper-lower)/4.0, 1e-12)
	}

	switch normalized {
	case "robust_sigmoid":
		out := make(map[string]float64, len(finite))
		for key, value := range finite {
			z := clamp((value-center)/robustScale, -12.0, 12.0)
			out[key] = 1.0 / (1.0 + math.Exp(-z))
		}
		return out, nil
	case "softmax":
		out := make(map[string]float64, len(finite))
		for key, value := range finite {
			out[key] = math.Exp(clamp((value-upper)/robustScale, -50.0, 0.0))
		}
		return out, nil
	}
	return nil, errors.New("memorix: 未处理的分数校准方法: " + normalized)
}

// FuseScoreMaps 校准并融合多个候选分数通道。
// 返回 (最终分数, 各通道校准后分数)。
//
// 出处：core/retrieval/score_calibration.py L118-L177。
func FuseScoreMaps(
	scoreMaps map[string]map[string]float64,
	weights map[string]float64,
	method string,
	rrfK int,
) (map[string]float64, map[string]map[string]float64, error) {
	normalized, err := NormalizeCalibrationMethod(method)
	if err != nil {
		return nil, nil, err
	}
	finite := make(map[string]map[string]float64, len(scoreMaps))
	candidates := map[string]struct{}{}
	for name, scores := range scoreMaps {
		clean := finiteScoreMap(scores)
		finite[name] = clean
		for id := range clean {
			candidates[id] = struct{}{}
		}
	}

	calibrated := make(map[string]map[string]float64, len(finite))
	if channels, isRRF := scoreCalibrationRRFChannels[normalized]; isRRF {
		safeK := rrfK
		if safeK < 1 {
			safeK = 1
		}
		allowed := map[string]struct{}{}
		for _, ch := range channels {
			allowed[ch] = struct{}{}
		}
		for channel, scores := range finite {
			if channels != nil {
				if _, ok := allowed[channel]; !ok {
					calibrated[channel] = copyMap(scores)
					continue
				}
			}
			ranks := averageRankScores(scores)
			out := make(map[string]float64, len(ranks))
			for key, rank := range ranks {
				out[key] = (float64(safeK) + 1.0) / (float64(safeK) + rank)
			}
			calibrated[channel] = out
		}
	} else if len(normalized) > 9 && normalized[:9] == "semantic_" {
		semanticMethod := normalized[len("semantic_"):]
		for channel, scores := range finite {
			useMethod := "none"
			if channel == "semantic" {
				useMethod = semanticMethod
			}
			out, err := CalibrateScoreMap(scores, useMethod)
			if err != nil {
				return nil, nil, err
			}
			calibrated[channel] = out
		}
	} else {
		for channel, scores := range finite {
			out, err := CalibrateScoreMap(scores, normalized)
			if err != nil {
				return nil, nil, err
			}
			calibrated[channel] = out
		}
	}

	final := make(map[string]float64, len(candidates))
	for id := range candidates {
		final[id] = 0.0
	}
	for channel, scores := range calibrated {
		weight := weights[channel]
		for id, score := range scores {
			final[id] += weight * score
		}
	}
	return final, calibrated, nil
}

// OrderedIDs 按分数降序、ID 升序返回稳定排序的 id 列表。
// 出处：core/retrieval/score_calibration.py L180-L189。
func OrderedIDs(scores map[string]float64) []string {
	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids
}

// NormalizeScoresMinMax 原地 min-max 归一化（对齐 _normalize_scores_minmax）。
// 出处：core/retrieval/dual_path.py L614-L625。
func NormalizeScoresMinMax(scores []float64) []float64 {
	if len(scores) == 0 {
		return scores
	}
	lo, hi := scores[0], scores[0]
	for _, v := range scores[1:] {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	if hi-lo < 1e-12 {
		out := make([]float64, len(scores))
		for i := range out {
			out[i] = 1.0
		}
		return out
	}
	out := make([]float64, len(scores))
	for i, v := range scores {
		out[i] = (v - lo) / (hi - lo)
	}
	return out
}

// BuildMinMaxScoreMap 按 hash 生成 min-max 归一化分数表。
// 出处：core/retrieval/dual_path.py L627-L636。
func BuildMinMaxScoreMap(ids []string, scores []float64) map[string]float64 {
	if len(ids) == 0 || len(ids) != len(scores) {
		return map[string]float64{}
	}
	lo, hi := scores[0], scores[0]
	for _, v := range scores[1:] {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	out := make(map[string]float64, len(ids))
	if hi-lo < 1e-12 {
		for _, id := range ids {
			out[id] = 1.0
		}
		return out
	}
	for i, id := range ids {
		out[id] = (scores[i] - lo) / (hi - lo)
	}
	return out
}

// FuseRankedListsWeightedRRF 按 weighted RRF 融合两路段落召回。
// 出处：core/retrieval/dual_path.py L817-L857。
func FuseRankedListsWeightedRRF(
	vectorIDs []string,
	sparseIDs []string,
	rrfK int,
	vectorWeight float64,
	bm25Weight float64,
	normalize bool,
) (map[string]float64, []string) {
	if rrfK < 1 {
		rrfK = DefaultFusionRRFK
	}
	scoreMap := map[string]float64{}
	order := make([]string, 0, len(vectorIDs)+len(sparseIDs))
	seen := map[string]struct{}{}
	for rank, id := range vectorIDs {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			order = append(order, id)
		}
		scoreMap[id] += vectorWeight * (1.0 / float64(rrfK+rank+1))
	}
	for rank, id := range sparseIDs {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			order = append(order, id)
		}
		scoreMap[id] += bm25Weight * (1.0 / float64(rrfK+rank+1))
	}
	if normalize {
		ids := make([]string, 0, len(scoreMap))
		scores := make([]float64, 0, len(scoreMap))
		for _, id := range order {
			ids = append(ids, id)
			scores = append(scores, scoreMap[id])
		}
		normed := NormalizeScoresMinMax(scores)
		for i, id := range ids {
			scoreMap[id] = normed[i]
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		if scoreMap[order[i]] != scoreMap[order[j]] {
			return scoreMap[order[i]] > scoreMap[order[j]]
		}
		return order[i] < order[j]
	})
	return scoreMap, order
}

func copyMap(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2.0
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ClipUnit 把值截断到 [0, 1]（对齐 DualPathRetriever._clip_unit）。
// 出处：core/retrieval/dual_path.py L1727-L1728。
func ClipUnit(value float64) float64 { return clamp(value, 0.0, 1.0) }
