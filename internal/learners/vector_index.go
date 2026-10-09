// Package learners 实现 MaiBot 学习系统：表达学习（含批次闸门与向量索引）、
// 黑话挖掘/解释/上下文注入、行为学习实验骨架。
//
// 行为对照（以 Python 源码为准）：
//   - 参考/MaiBot/src/learners/expression_learner.py            → expression.go
//   - 参考/MaiBot/src/chat/replyer/expression_vector_index.py   → vector_index.go
//   - 参考/MaiBot/src/learners/jargon_learner.py                → jargon.go
//   - 参考/MaiBot/src/learners/jargon_miner.py                  → jargon_miner.go
//   - 参考/MaiBot/src/learners/jargon_explainer.py              → jargon.go
//   - 参考/MaiBot/src/learners/behavior_*.py                    → behavior.go
//
// 依赖约定：本包不 import internal/config、internal/llm 具体类型、internal/qq。
// LLM/embedding 调用与存储一律通过包内最小接口注入。
package learners

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 常数照抄 expression_vector_index.py。
const (
	// VectorCandidateHardLimit mirrors VECTOR_CANDIDATE_HARD_LIMIT.
	VectorCandidateHardLimit = 50
	// VectorIndexVersion mirrors VECTOR_INDEX_VERSION.
	VectorIndexVersion = 2
	// VectorItemWeight mirrors VECTOR_ITEM_WEIGHT.
	VectorItemWeight = 0.875
	// VectorClusterWeight mirrors VECTOR_CLUSTER_WEIGHT.
	VectorClusterWeight = 0.125
	// VectorDiversityLambda mirrors VECTOR_DIVERSITY_LAMBDA.
	VectorDiversityLambda = 0.85
	// FullReclusterChangeRatio mirrors FULL_RECLUSTER_CHANGE_RATIO.
	FullReclusterChangeRatio = 0.05
	// ClusterStateBootstrapping mirrors CLUSTER_STATE_BOOTSTRAPPING.
	ClusterStateBootstrapping = "BOOTSTRAPPING"
	// ClusterStateStable mirrors CLUSTER_STATE_STABLE.
	ClusterStateStable = "STABLE"
	// KMeansSeed mirrors `_run_kmeans(seed=20260621)`.
	KMeansSeed = 20260621
	// KMeansMaxIter mirrors `_run_kmeans(max_iter=100)`.
	KMeansMaxIter = 100
)

// PromptExampleExpressionStyles mirrors PROMPT_EXAMPLE_EXPRESSION_STYLES.
var PromptExampleExpressionStyles = map[string]bool{
	"我嘞个xxxx": true,
	"对对对":     true,
	"这么强！":    true,
}

// IndexedExpression mirrors IndexedExpression.
type IndexedExpression struct {
	ID          int64
	Situation   string
	Style       string
	Count       int
	Fingerprint string
	Profile     string
	Model       string
	Dimension   int
	ClusterID   int
	Index       int
}

// VectorIndexSnapshot mirrors ExpressionVectorIndexSnapshot.
type VectorIndexSnapshot struct {
	Path             string
	Profile          string
	Expressions      []IndexedExpression
	ProfileVectors   []float32
	VectorDim        int
	ClusterCenters   [][]float32
	ClusterCount     int
	ClusterState     string
	IndexedByCluster map[int][]IndexedExpression
}

// UpsertItem mirrors ExpressionVectorIndexUpsertItem.
type UpsertItem struct {
	ID         int64
	Situation  string
	Style      string
	Count      int
	SessionID  string
	Checked    bool
	ModifiedBy string
}

// ScoredCandidate mirrors select_candidates 的返回项（对齐 replyer 侧
// ExpressionCandidate 需要的 id/situation/style/count 字段 + 诊断字段）。
type ScoredCandidate struct {
	ID                int64   `json:"id"`
	Situation         string  `json:"situation"`
	Style             string  `json:"style"`
	Count             int     `json:"count"`
	SelectorScore     float64 `json:"selector_score"`
	ItemSimilarity    float64 `json:"item_similarity"`
	ClusterSimilarity float64 `json:"cluster_similarity"`
	ClusterID         int     `json:"cluster_id"`
	// Score 仅用于 MMR 排序，输出时删除（对齐 Python 行为）。
	Score float64 `json:"-"`
}

// persistedIndex mirrors the JSON payload written to
// expression_vector_index_path（Python 把向量放在 .npz；Go 版合并进 JSON，
// 便于单文件持久化与版本控制）。
type persistedIndex struct {
	Version      int                   `json:"version"`
	Profile      string                `json:"profile"`
	Model        string                `json:"model"`
	Dimension    int                   `json:"dimension"`
	ClusterState string                `json:"cluster_state"`
	ClusterCount int                   `json:"cluster_count"`
	Centers      [][]float32           `json:"cluster_centers"`
	Vectors      map[string][]float32  `json:"vectors"`
	ChangedSince int                   `json:"changes_since_recluster"`
	Expressions  []persistedExpression `json:"expressions"`
}

type persistedExpression struct {
	ID          int64  `json:"id"`
	Situation   string `json:"situation"`
	Style       string `json:"style"`
	Count       int    `json:"count"`
	SessionID   string `json:"session_id"`
	Checked     bool   `json:"checked"`
	ModifiedBy  string `json:"modified_by"`
	Fingerprint string `json:"fingerprint"`
	Embedding   string `json:"embedding_model"`
	Dimension   int    `json:"embedding_dimension"`
	ClusterID   int    `json:"cluster_id"`
	VectorIndex int    `json:"vector_index"`
}

// ExpressionVectorIndex mirrors ExpressionVectorIndex（JSON 持久化 + 余弦 TopK +
// kmeans 聚类 + MMR 多样性采样，纯 Go 手写）。
type ExpressionVectorIndex struct {
	// Path 是索引 JSON 路径（对齐 expression_vector_index_path）。
	Path string
	// Embedder 生成表达向量（nil 时索引只保留元数据，向量召回降级）。
	Embedder EmbedFunc
	// Profile 是 embedding 后端标识（对齐 embedding_profile_marker）。
	Profile string
	// Model 是 embedding 模型名（观测用）。
	Model string
	// VectorDim 是期望维度（<=0 时按首个向量推断）。
	VectorDim int

	mu sync.Mutex
}

// NewExpressionVectorIndex 构造索引。
func NewExpressionVectorIndex(path string) *ExpressionVectorIndex {
	return &ExpressionVectorIndex{Path: path}
}

// NormalizeIndexText mirrors normalize_text.
func NormalizeIndexText(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

// ExpressionFingerprint mirrors expression_fingerprint.
func ExpressionFingerprint(expressionID int64, situation string, style string) string {
	raw := fmt.Sprintf("%d\n%s\n%s", expressionID, NormalizeIndexText(situation), NormalizeIndexText(style))
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ExpressionEmbeddingText mirrors expression_embedding_text.
func ExpressionEmbeddingText(situation string, style string) string {
	return "情景：" + NormalizeIndexText(situation) + "\n风格：" + NormalizeIndexText(style)
}

// NormalizeExpressionStyleForLearning mirrors
// learn_expression_style_utils.normalize_expression_style_for_learning.
func NormalizeExpressionStyleForLearning(style string) string {
	normalized := strings.TrimSpace(style)
	if strings.HasPrefix(normalized, "使用") {
		return strings.TrimSpace(strings.TrimPrefix(normalized, "使用"))
	}
	return normalized
}

// IsPromptExampleExpressionStyle mirrors is_prompt_example_expression_style.
func IsPromptExampleExpressionStyle(style string) bool {
	return PromptExampleExpressionStyles[NormalizeExpressionStyleForLearning(style)]
}

// ---------------------------------------------------------------------------
// 向量数学（纯 Go，注意数值稳定性）
// ---------------------------------------------------------------------------

// L2Normalize mirrors l2_normalize（零向量/含 NaN 向量返回 nil）。
func L2Normalize(vector []float32) []float32 {
	if len(vector) == 0 {
		return nil
	}
	sum := 0.0
	for _, value := range vector {
		v := float64(value)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
		sum += v * v
	}
	norm := math.Sqrt(sum)
	if norm <= 0 {
		return nil
	}
	out := make([]float32, len(vector))
	for index, value := range vector {
		out[index] = float32(float64(value) / norm)
	}
	return out
}

// DotProduct returns the inner product of two equal-length vectors
// (0 when the lengths differ).
func DotProduct(left, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return 0
	}
	sum := 0.0
	for index := range left {
		sum += float64(left[index]) * float64(right[index])
	}
	return sum
}

// CosineSimilarity returns the cosine similarity of two vectors (0 on
// dimension mismatch or zero norm).
func CosineSimilarity(left, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		l := float64(left[index])
		r := float64(right[index])
		dot += l * r
		leftNorm += l * l
		rightNorm += r * r
	}
	if leftNorm <= 0 || rightNorm <= 0 {
		return 0
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (v *ExpressionVectorIndex) load() (*persistedIndex, error) {
	path := strings.TrimSpace(v.Path)
	if path == "" {
		return nil, fmt.Errorf("learners: 表达向量索引未配置路径")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &persistedIndex{Version: VectorIndexVersion, Vectors: map[string][]float32{}}, nil
		}
		return nil, err
	}
	var payload persistedIndex
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("learners: 表达向量索引解析失败: %w", err)
	}
	if payload.Version != 0 && (payload.Version < 1 || payload.Version > VectorIndexVersion) {
		return nil, fmt.Errorf("learners: 表达向量索引版本不匹配: %d", payload.Version)
	}
	if payload.Vectors == nil {
		payload.Vectors = map[string][]float32{}
	}
	return &payload, nil
}

func (v *ExpressionVectorIndex) save(payload *persistedIndex) error {
	path := strings.TrimSpace(v.Path)
	if path == "" {
		return fmt.Errorf("learners: 表达向量索引未配置路径")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// UpsertExpressions mirrors upsert_expressions：写入/更新表达记录，按需重聚类。
//
// 返回 reclustered 表示本次是否执行了全量重聚类。
func (v *ExpressionVectorIndex) UpsertExpressions(ctx context.Context, items []UpsertItem) (reclustered bool, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(items) == 0 {
		return false, nil
	}
	payload, err := v.load()
	if err != nil {
		return false, err
	}
	if payload.Version == 0 {
		payload.Version = VectorIndexVersion
	}
	profile := strings.TrimSpace(v.Profile)
	if profile == "" {
		profile = "default"
	}

	embeddings := make([][]float32, 0, len(items))
	if v.Embedder != nil {
		texts := make([]string, 0, len(items))
		for _, item := range items {
			texts = append(texts, ExpressionEmbeddingText(item.Situation, item.Style))
		}
		embeddings, err = v.Embedder(ctx, texts)
		if err != nil {
			return false, fmt.Errorf("learners: 表达向量生成失败: %w", err)
		}
		if len(embeddings) != len(items) {
			return false, fmt.Errorf("learners: 表达向量数量不匹配: got=%d want=%d", len(embeddings), len(items))
		}
	}

	existing := make(map[int64]int, len(payload.Expressions))
	for index, expression := range payload.Expressions {
		existing[expression.ID] = index
	}

	changed := 0
	nextVectors := make(map[string][]float32, len(payload.Vectors))
	for key, vector := range payload.Vectors {
		nextVectors[key] = vector
	}

	for index, item := range items {
		situation := NormalizeIndexText(item.Situation)
		style := NormalizeIndexText(item.Style)
		fingerprint := ExpressionFingerprint(item.ID, situation, style)
		vector := []float32(nil)
		if index < len(embeddings) {
			vector = L2Normalize(embeddings[index])
		}
		if vector == nil {
			// 降级：保留既有向量（若有），否则只写元数据。
			vector = nextVectors[fmt.Sprintf("%d", item.ID)]
		}
		record := persistedExpression{
			ID:          item.ID,
			Situation:   situation,
			Style:       style,
			Count:       item.Count,
			SessionID:   item.SessionID,
			Checked:     item.Checked,
			ModifiedBy:  item.ModifiedBy,
			Fingerprint: fingerprint,
			Dimension:   len(vector),
			ClusterID:   0,
			VectorIndex: 0,
		}
		if position, ok := existing[item.ID]; ok {
			previous := payload.Expressions[position]
			record.ClusterID = previous.ClusterID
			record.VectorIndex = previous.VectorIndex
			if previous.Fingerprint != fingerprint {
				changed++
			}
			payload.Expressions[position] = record
		} else {
			record.VectorIndex = len(payload.Expressions)
			payload.Expressions = append(payload.Expressions, record)
			changed++
		}
		if len(vector) > 0 {
			nextVectors[fmt.Sprintf("%d", item.ID)] = vector
		}
	}

	payload.Vectors = nextVectors
	payload.Profile = profile
	if strings.TrimSpace(v.Model) != "" {
		payload.Model = strings.TrimSpace(v.Model)
	}
	if payload.Dimension == 0 {
		payload.Dimension = v.detectDimension(payload)
	}
	payload.ChangedSince += changed

	// 重建向量矩阵与聚类。
	records := make([]IndexedExpression, 0, len(payload.Expressions))
	for index, expression := range payload.Expressions {
		vector := payload.Vectors[fmt.Sprintf("%d", expression.ID)]
		if len(vector) == 0 {
			continue
		}
		records = append(records, IndexedExpression{
			ID:          expression.ID,
			Situation:   expression.Situation,
			Style:       expression.Style,
			Count:       expression.Count,
			Fingerprint: expression.Fingerprint,
			Profile:     profile,
			Model:       payload.Model,
			Dimension:   len(vector),
			ClusterID:   expression.ClusterID,
			Index:       index,
		})
	}

	needRecluster := payload.ClusterState != ClusterStateStable || len(payload.Centers) == 0
	if !needRecluster && len(records) > 0 {
		ratio := float64(payload.ChangedSince) / float64(len(records))
		if ratio >= FullReclusterChangeRatio {
			needRecluster = true
		}
	}
	if needRecluster {
		v.recluster(payload, records)
		payload.ClusterState = ClusterStateStable
		payload.ChangedSince = 0
		reclustered = true
	} else {
		// 增量：把记录分配到最近的既有簇（对齐 `_select_nearest_clusters`）。
		centers := centersFrom(payload.Centers)
		for index := range payload.Expressions {
			expression := &payload.Expressions[index]
			vector := payload.Vectors[fmt.Sprintf("%d", expression.ID)]
			if len(vector) == 0 || len(centers) == 0 {
				continue
			}
			expression.ClusterID = NearestCluster(vector, centers)
		}
	}
	if err := v.save(payload); err != nil {
		return reclustered, err
	}
	return reclustered, nil
}

// detectDimension 返回索引中第一个非空向量维度（0 表示无可推断维度）。
func (v *ExpressionVectorIndex) detectDimension(payload *persistedIndex) int {
	if v.VectorDim > 0 {
		return v.VectorDim
	}
	for _, vector := range payload.Vectors {
		if len(vector) > 0 {
			return len(vector)
		}
	}
	return 0
}

// recluster 执行全量 cosine kmeans 并写回聚类中心。
func (v *ExpressionVectorIndex) recluster(payload *persistedIndex, records []IndexedExpression) {
	vectors := make([][]float32, 0, len(records))
	indices := make([]int, 0, len(records))
	for index, expression := range payload.Expressions {
		vector := payload.Vectors[fmt.Sprintf("%d", expression.ID)]
		if len(vector) == 0 {
			continue
		}
		vectors = append(vectors, vector)
		indices = append(indices, index)
	}
	if len(vectors) == 0 {
		payload.Centers = nil
		payload.ClusterCount = 0
		return
	}
	clusterCount := ChooseClusterCount(len(vectors), payload.ClusterCount)
	labels, centers := CosineKMeans(vectors, KMeansParams{
		ClusterCount: clusterCount,
		Seed:         KMeansSeed,
		MaxIter:      KMeansMaxIter,
	})
	for position, index := range indices {
		payload.Expressions[index].ClusterID = labels[position]
	}
	payload.Centers = centers
	payload.ClusterCount = len(centers)
}

// NearestCluster mirrors `_select_nearest_cluster`（返回最近簇下标）。
func NearestCluster(vector []float32, centers [][]float32) int {
	if len(centers) == 0 {
		return 0
	}
	best := 0
	bestScore := math.Inf(-1)
	normalized := L2Normalize(vector)
	for index, center := range centers {
		normalizedCenter := L2Normalize(center)
		if normalizedCenter == nil {
			continue
		}
		score := DotProduct(normalized, normalizedCenter)
		if score > bestScore {
			bestScore = score
			best = index
		}
	}
	return best
}

func centersFrom(centers [][]float32) [][]float32 {
	out := make([][]float32, 0, len(centers))
	for _, center := range centers {
		if len(center) > 0 {
			out = append(out, center)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 快照与召回
// ---------------------------------------------------------------------------

// FilterIndexedExpressions mirrors `_filter_indexed_expressions`：只保留当前
// 作用域候选、指纹一致的索引记录。
func FilterIndexedExpressions(
	snapshot *VectorIndexSnapshot,
	candidates []ScopedCandidate,
	priority SessionPriorityFunc,
) []IndexedExpression {
	if snapshot == nil || len(candidates) == 0 {
		return nil
	}
	scoped := make(map[int64]ScopedCandidate, len(candidates))
	for _, candidate := range candidates {
		scoped[candidate.ID] = candidate
	}
	out := make([]IndexedExpression, 0, len(snapshot.Expressions))
	for _, expression := range snapshot.Expressions {
		candidate, ok := scoped[expression.ID]
		if !ok {
			continue
		}
		if priority != nil {
			// priority 与候选参与者的会话 id 集合做主序（对齐作用域过滤）。
			if !priority(candidate) {
				continue
			}
		}
		situation := NormalizeIndexText(candidate.Situation)
		style := NormalizeIndexText(candidate.Style)
		fingerprint := ExpressionFingerprint(expression.ID, situation, style)
		if expression.Fingerprint != "" && expression.Fingerprint != fingerprint {
			continue
		}
		copied := expression
		copied.Situation = situation
		copied.Style = style
		copied.Fingerprint = fingerprint
		copied.Count = candidate.Count
		out = append(out, copied)
	}
	return out
}

// ScopedCandidate 是索引过滤用的候选视图。
type ScopedCandidate struct {
	ID        int64
	Situation string
	Style     string
	Count     int
}

// SessionPriorityFunc 判定候选是否属于当前聊天流作用域。
type SessionPriorityFunc func(candidate ScopedCandidate) bool

// SelectCandidates mirrors select_candidates：聚类粗筛 + 打分 + MMR 重排。
func (v *ExpressionVectorIndex) SelectCandidates(
	ctx context.Context,
	queryText string,
	candidates []ScopedCandidate,
	poolSize int,
	clusterPoolSize int,
) ([]ScoredCandidate, error) {
	normalizedQuery := NormalizeIndexText(queryText)
	if normalizedQuery == "" {
		return nil, nil
	}
	v.mu.Lock()
	snapshot, err := v.snapshotLocked()
	v.mu.Unlock()
	if err != nil || snapshot == nil || v.Embedder == nil {
		return nil, err
	}

	indexed := FilterIndexedExpressions(snapshot, candidates, nil)
	if len(indexed) < 10 {
		return nil, nil
	}
	embeddings, err := v.Embedder(ctx, []string{normalizedQuery})
	if err != nil {
		return nil, fmt.Errorf("learners: 表达向量 query 生成失败: %w", err)
	}
	if len(embeddings) == 0 {
		return nil, nil
	}
	queryVector := L2Normalize(embeddings[0])
	if queryVector == nil {
		return nil, nil
	}
	if snapshot.VectorDim > 0 && len(queryVector) != snapshot.VectorDim {
		return nil, fmt.Errorf("learners: 表达向量 query 维度与索引不一致: query=%d index=%d",
			len(queryVector), snapshot.VectorDim)
	}

	clusterScores := make([]float64, 0, len(snapshot.ClusterCenters))
	for _, center := range snapshot.ClusterCenters {
		clusterScores = append(clusterScores, DotProduct(center, queryVector))
	}
	orderedClusters := make([]int, len(clusterScores))
	for index := range orderedClusters {
		orderedClusters[index] = index
	}
	sort.SliceStable(orderedClusters, func(i, j int) bool {
		return clusterScores[orderedClusters[i]] > clusterScores[orderedClusters[j]]
	})

	clusterPool := clusterPoolSize
	if clusterPool < 1 {
		clusterPool = 1
	}
	limit := poolSize
	if limit < 1 {
		limit = 1
	}
	if limit > VectorCandidateHardLimit {
		limit = VectorCandidateHardLimit
	}

	pool := make([]IndexedExpression, 0, len(indexed))
	selectedClusters := 0
	for _, clusterID := range orderedClusters {
		members := snapshot.IndexedByCluster[clusterID]
		if len(members) == 0 {
			continue
		}
		selectedClusters++
		pool = append(pool, members...)
		if selectedClusters >= clusterPool && len(pool) >= limit {
			break
		}
	}
	if len(pool) == 0 {
		return nil, nil
	}

	totalWeight := VectorItemWeight + VectorClusterWeight
	scored := make([]ScoredCandidate, 0, len(pool))
	for _, candidate := range pool {
		vector := v.vectorFor(candidate.ID)
		if vector == nil {
			continue
		}
		itemSimilarity := DotProduct(vector, queryVector)
		clusterSimilarity := 0.0
		if candidate.ClusterID >= 0 && candidate.ClusterID < len(clusterScores) {
			clusterSimilarity = clusterScores[candidate.ClusterID]
		}
		score := (itemSimilarity*VectorItemWeight + clusterSimilarity*VectorClusterWeight) / totalWeight
		scored = append(scored, ScoredCandidate{
			ID:                candidate.ID,
			Situation:         candidate.Situation,
			Style:             candidate.Style,
			Count:             candidate.Count,
			SelectorScore:     round4(score),
			ItemSimilarity:    round4(itemSimilarity),
			ClusterSimilarity: round4(clusterSimilarity),
			ClusterID:         candidate.ClusterID,
			Score:             score,
		})
	}

	vectorByID := make(map[int64][]float32, len(scored))
	for _, candidate := range scored {
		vectorByID[candidate.ID] = v.vectorFor(candidate.ID)
	}
	selected := SelectByMMR(scored, vectorByID, limit)
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].Score > selected[j].Score })
	for index := range selected {
		selected[index].Score = 0
	}
	return selected, nil
}

func (v *ExpressionVectorIndex) snapshotLocked() (*VectorIndexSnapshot, error) {
	payload, err := v.load()
	if err != nil {
		return nil, err
	}
	snapshot := &VectorIndexSnapshot{
		Path:             v.Path,
		Profile:          payload.Profile,
		VectorDim:        payload.Dimension,
		ClusterCenters:   payload.Centers,
		ClusterCount:     payload.ClusterCount,
		ClusterState:     payload.ClusterState,
		IndexedByCluster: map[int][]IndexedExpression{},
	}
	for index, expression := range payload.Expressions {
		vector := payload.Vectors[fmt.Sprintf("%d", expression.ID)]
		if len(vector) == 0 {
			continue
		}
		indexed := IndexedExpression{
			ID:          expression.ID,
			Situation:   expression.Situation,
			Style:       expression.Style,
			Count:       expression.Count,
			Fingerprint: expression.Fingerprint,
			Profile:     payload.Profile,
			Model:       expression.Embedding,
			Dimension:   len(vector),
			ClusterID:   expression.ClusterID,
			Index:       index,
		}
		snapshot.Expressions = append(snapshot.Expressions, indexed)
		snapshot.IndexedByCluster[indexed.ClusterID] = append(snapshot.IndexedByCluster[indexed.ClusterID], indexed)
	}
	if snapshot.VectorDim == 0 {
		snapshot.VectorDim = v.detectDimension(payload)
	}
	return snapshot, nil
}

func (v *ExpressionVectorIndex) vectorFor(expressionID int64) []float32 {
	payload, err := v.load()
	if err != nil {
		return nil
	}
	return payload.Vectors[fmt.Sprintf("%d", expressionID)]
}

// Snapshot 返回索引快照（只读，观测用）。
func (v *ExpressionVectorIndex) Snapshot() (*VectorIndexSnapshot, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.snapshotLocked()
}

// ---------------------------------------------------------------------------
// kmeans / MMR
// ---------------------------------------------------------------------------

// KMeansParams mirrors `_run_kmeans` 的参数。
type KMeansParams struct {
	ClusterCount int
	// InitialCenters 非空时使用既有中心（增量重聚类路径）。
	InitialCenters [][]float32
	Seed           int64
	MaxIter        int
}

// CosineKMeans mirrors `_run_kmeans`：归一化向量上的确定性 cosine kmeans。
//
// 返回样本标签与归一化聚类中心；每个簇保证非空（空簇从成员充足的簇迁移）。
func CosineKMeans(vectors [][]float32, params KMeansParams) ([]int, [][]float32) {
	sampleCount := len(vectors)
	if sampleCount == 0 {
		return nil, nil
	}
	clusterCount := params.ClusterCount
	if clusterCount <= 1 || sampleCount <= 1 {
		labels := make([]int, sampleCount)
		center := meanNormalized(vectors)
		if center == nil {
			center = make([]float32, len(vectors[0]))
		}
		return labels, [][]float32{center}
	}
	if clusterCount > sampleCount {
		clusterCount = sampleCount
	}
	maxIter := params.MaxIter
	if maxIter <= 0 {
		maxIter = KMeansMaxIter
	}

	centroids := make([][]float32, 0, clusterCount)
	if len(params.InitialCenters) >= clusterCount {
		for _, center := range params.InitialCenters[:clusterCount] {
			normalized := L2Normalize(center)
			if normalized != nil {
				centroids = append(centroids, normalized)
			}
		}
	}
	if len(centroids) < clusterCount {
		centroids = kmeansPlusPlusInit(vectors, clusterCount, params.Seed)
	}
	if len(centroids) < clusterCount {
		return nil, nil
	}

	labels := make([]int, sampleCount)
	for index := range labels {
		labels[index] = -1
	}
	for iteration := 0; iteration < maxIter; iteration++ {
		nextLabels := make([]int, sampleCount)
		similarities := make([][]float64, sampleCount)
		for sampleIndex, vector := range vectors {
			normalized := L2Normalize(vector)
			scores := make([]float64, len(centroids))
			best := 0
			bestScore := math.Inf(-1)
			for centerIndex, center := range centroids {
				score := DotProduct(normalized, center)
				scores[centerIndex] = score
				if score > bestScore {
					bestScore = score
					best = centerIndex
				}
			}
			nextLabels[sampleIndex] = best
			similarities[sampleIndex] = scores
		}
		nextLabels = RepairEmptyClusterLabels(nextLabels, similarities, len(centroids))
		nextCentroids := BuildClusterCenters(vectors, nextLabels, len(centroids))
		if nextCentroids == nil {
			return labels, centroids
		}
		converged := true
		for index := range labels {
			if labels[index] != nextLabels[index] {
				converged = false
				break
			}
		}
		labels = nextLabels
		centroids = nextCentroids
		if converged {
			break
		}
	}
	for clusterID := 0; clusterID < len(centroids); clusterID++ {
		found := false
		for _, label := range labels {
			if label == clusterID {
				found = true
				break
			}
		}
		if !found {
			// 兜底：把最后一个样本迁到空簇，保证不变量成立。
			labels[len(labels)-1] = clusterID
		}
	}
	return labels, centroids
}

// kmeansPlusPlusInit mirrors the incremental kmeans++ seeding of `_run_kmeans`.
func kmeansPlusPlusInit(vectors [][]float32, clusterCount int, seed int64) [][]float32 {
	sampleCount := len(vectors)
	if sampleCount == 0 || clusterCount <= 0 {
		return nil
	}
	random := newDeterministicRand(seed)
	normalized := make([][]float32, sampleCount)
	for index, vector := range vectors {
		normalized[index] = L2Normalize(vector)
		if normalized[index] == nil {
			normalized[index] = make([]float32, len(vector))
		}
	}
	first := int(random.IntN(sampleCount))
	selected := []int{first}
	closest := make([]float64, sampleCount)
	for index := range closest {
		distance := 1.0 - DotProduct(normalized[index], normalized[first])
		if distance < 0 {
			distance = 0
		}
		closest[index] = distance
	}
	closest[first] = 0

	for len(selected) < clusterCount {
		total := 0.0
		for _, distance := range closest {
			total += distance
		}
		next := -1
		if total <= 0 {
			used := map[int]bool{}
			for _, index := range selected {
				used[index] = true
			}
			for index := 0; index < sampleCount; index++ {
				if !used[index] {
					next = index
					break
				}
			}
		} else {
			target := random.Float64() * total
			cumulative := 0.0
			for index, distance := range closest {
				cumulative += distance
				if cumulative >= target {
					next = index
					break
				}
			}
			if next < 0 {
				next = sampleCount - 1
			}
			for containsInt(selected, next) {
				next = (next + 1) % sampleCount
			}
		}
		if next < 0 {
			break
		}
		selected = append(selected, next)
		for index := range closest {
			distance := 1.0 - DotProduct(normalized[index], normalized[next])
			if distance < 0 {
				distance = 0
			}
			if distance < closest[index] {
				closest[index] = distance
			}
		}
		for _, index := range selected {
			closest[index] = 0
		}
	}
	centers := make([][]float32, 0, len(selected))
	for _, index := range selected {
		centers = append(centers, normalized[index])
	}
	return centers
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// RepairEmptyClusterLabels mirrors `_repair_empty_cluster_labels`.
func RepairEmptyClusterLabels(labels []int, similarities [][]float64, clusterCount int) []int {
	if clusterCount <= 0 {
		return labels
	}
	repaired := append([]int{}, labels...)
	counts := make([]int, clusterCount)
	for _, label := range repaired {
		if label >= 0 && label < clusterCount {
			counts[label]++
		}
	}
	for emptyCluster := 0; emptyCluster < clusterCount; emptyCluster++ {
		if counts[emptyCluster] > 0 {
			continue
		}
		donor := -1
		donorScore := math.Inf(1)
		for index, label := range repaired {
			if label < 0 || label >= clusterCount || counts[label] <= 1 {
				continue
			}
			score := 0.0
			if index < len(similarities) && label < len(similarities[index]) {
				score = similarities[index][label]
			}
			if score < donorScore {
				donorScore = score
				donor = index
			}
		}
		if donor < 0 {
			// 无可用捐赠者：该簇保持为空，交由调用方的不变量兜底处理。
			continue
		}
		counts[repaired[donor]]--
		repaired[donor] = emptyCluster
		counts[emptyCluster]++
	}
	return repaired
}

// BuildClusterCenters mirrors `_build_cluster_centers_from_labels`.
func BuildClusterCenters(vectors [][]float32, labels []int, clusterCount int) [][]float32 {
	centers := make([][]float32, 0, clusterCount)
	for clusterID := 0; clusterID < clusterCount; clusterID++ {
		members := make([][]float32, 0, len(vectors))
		for index, label := range labels {
			if label == clusterID {
				normalized := L2Normalize(vectors[index])
				if normalized != nil {
					members = append(members, normalized)
				}
			}
		}
		if len(members) == 0 {
			return nil
		}
		center := meanNormalized(members)
		if center == nil {
			return nil
		}
		centers = append(centers, center)
	}
	return centers
}

func meanNormalized(vectors [][]float32) []float32 {
	if len(vectors) == 0 {
		return nil
	}
	dim := len(vectors[0])
	center := make([]float64, dim)
	for _, vector := range vectors {
		if len(vector) != dim {
			return nil
		}
		for index, value := range vector {
			center[index] += float64(value)
		}
	}
	out := make([]float32, dim)
	sum := 0.0
	for index, value := range center {
		value /= float64(len(vectors))
		out[index] = float32(value)
		sum += value * value
	}
	norm := math.Sqrt(sum)
	if norm <= 0 {
		return nil
	}
	for index := range out {
		out[index] = float32(float64(out[index]) / norm)
	}
	return out
}

// ChooseClusterCount mirrors `_choose_cluster_count`.
func ChooseClusterCount(sampleCount int, previousClusterCount int) int {
	if sampleCount <= 1 {
		return 1
	}
	if previousClusterCount > 0 {
		count := previousClusterCount
		if count > sampleCount {
			count = sampleCount
		}
		if count < 1 {
			count = 1
		}
		return count
	}
	count := sampleCount
	if count > 80 {
		count = 80
	}
	if count < 2 {
		count = 2
	}
	return count
}

// SelectByMMR mirrors `_select_by_mmr`：增量向量化 MMR 避免候选池过度集中。
func SelectByMMR(
	scored []ScoredCandidate,
	vectorByID map[int64][]float32,
	limit int,
) []ScoredCandidate {
	if len(scored) == 0 || limit <= 0 {
		return nil
	}
	if VectorDiversityLambda >= 0.999 {
		sorted := append([]ScoredCandidate{}, scored...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
		if len(sorted) > limit {
			sorted = sorted[:limit]
		}
		return sorted
	}
	vectors := make([][]float32, len(scored))
	for index, candidate := range scored {
		vectors[index] = vectorByID[candidate.ID]
	}
	selectedMask := make([]bool, len(scored))
	penalties := make([]float64, len(scored))
	selected := make([]ScoredCandidate, 0, limit)
	for selection := 0; selection < limit && selection < len(scored); selection++ {
		bestPosition := -1
		bestScore := math.Inf(-1)
		for index, candidate := range scored {
			if selectedMask[index] {
				continue
			}
			score := VectorDiversityLambda * candidate.Score
			if selection > 0 {
				score -= (1.0 - VectorDiversityLambda) * penalties[index]
			}
			if score > bestScore {
				bestScore = score
				bestPosition = index
			}
		}
		if bestPosition < 0 {
			break
		}
		selectedMask[bestPosition] = true
		selected = append(selected, scored[bestPosition])
		for index := range scored {
			similarity := CosineSimilarity(vectors[index], vectors[bestPosition])
			if selection == 0 {
				penalties[index] = similarity
				continue
			}
			if similarity > penalties[index] {
				penalties[index] = similarity
			}
		}
	}
	return selected
}

func round4(value float64) float64 {
	return math.Round(value*10000) / 10000
}

// deterministicRand 是 math/rand 的确定性替代（固定 seed，跨版本稳定）。
type deterministicRand struct {
	state uint64
}

func newDeterministicRand(seed int64) *deterministicRand {
	state := uint64(seed)
	if state == 0 {
		state = 0x9E3779B97F4A7C15
	}
	return &deterministicRand{state: state}
}

func (r *deterministicRand) next() uint64 {
	// splitmix64
	r.state += 0x9E3779B97F4A7C15
	z := r.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *deterministicRand) IntN(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func (r *deterministicRand) Float64() float64 {
	return float64(r.next()>>11) / float64(1<<53)
}

// embeddedDummy 已删除（保留注释说明：IndexedExpression 不再需要占位方法）。

// timeNow 让测试可注入（默认 time.Now）。
var timeNow = func() time.Time { return time.Now() }

// SetTimeNow 注入时间源（测试用）。
func SetTimeNow(fn func() time.Time) { timeNow = fn }
