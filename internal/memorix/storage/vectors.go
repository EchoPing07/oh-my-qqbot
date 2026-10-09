package storage

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"oh-my-qqbot/internal/memorix/retrieval"
)

// VectorIndex 是内存 flat 内积索引：向量在写入时做 L2 归一化，
// 检索时内积即余弦相似度（对齐 §6.14 的「L2 归一化后余弦」）。
//
// Python 版用 faiss（core/storage/vector_store.py）；Go 版规模 ≤ 百万级，
// 采用纯 Go 遍历。全部向量按 item_type 分池存储，避免跨池误召回。
type VectorIndex struct {
	dim int
	// pools: item_type → id → 归一化向量
	pools map[string]map[string][]float32
	count int
}

// NewVectorIndex 创建指定维度的空索引（dim=0 表示延迟确定）。
func NewVectorIndex(dim int) *VectorIndex {
	return &VectorIndex{dim: dim, pools: map[string]map[string][]float32{}}
}

// Dimension 返回索引维度。
func (v *VectorIndex) Dimension() int { return v.dim }

// Count 返回全部向量数量。
func (v *VectorIndex) Count() int { return v.count }

// CountByType 返回某类向量数量。
func (v *VectorIndex) CountByType(itemType string) int { return len(v.pools[itemType]) }

// Add 写入（或覆盖）一条向量。
func (v *VectorIndex) Add(itemType, id string, vec []float32, normalize bool) error {
	if id == "" {
		return fmt.Errorf("memorix/storage: 向量 id 不能为空")
	}
	if v.dim <= 0 {
		v.dim = len(vec)
	} else if len(vec) != v.dim {
		return fmt.Errorf("memorix/storage: 向量维度不匹配: 期望 %d, 实际 %d", v.dim, len(vec))
	}
	stored := vec
	if normalize {
		normalized, _ := safeNormalize(vec)
		stored = normalized
	}
	pool, ok := v.pools[itemType]
	if !ok {
		pool = map[string][]float32{}
		v.pools[itemType] = pool
	}
	if _, exists := pool[id]; !exists {
		v.count++
	}
	pool[id] = stored
	return nil
}

// Remove 删除一条向量，返回是否存在。
func (v *VectorIndex) Remove(itemType, id string) bool {
	pool, ok := v.pools[itemType]
	if !ok {
		return false
	}
	if _, exists := pool[id]; !exists {
		return false
	}
	delete(pool, id)
	v.count--
	return true
}

// Reset 清空索引并设定维度。
func (v *VectorIndex) Reset(dim int) {
	v.dim = dim
	v.pools = map[string]map[string][]float32{}
	v.count = 0
}

// Search 在指定池内做内积 TopK（查询向量不要求已归一化，函数内部归一化）。
// allowed 非 nil 时只考虑其中的 id（对齐 VectorStore.search 的 allowed_ids）。
func (v *VectorIndex) Search(itemType string, query []float32, k int, allowed map[string]struct{}) []retrieval.VectorHit {
	pool, ok := v.pools[itemType]
	if !ok || len(pool) == 0 || k <= 0 || len(query) == 0 {
		return nil
	}
	if v.dim > 0 && len(query) != v.dim {
		return nil
	}
	q, norm := safeNormalize(query)
	if norm <= 0 {
		return nil
	}
	hits := make([]retrieval.VectorHit, 0, len(pool))
	for id, vec := range pool {
		if allowed != nil {
			if _, ok := allowed[id]; !ok {
				continue
			}
		}
		score := float64(0)
		n := len(vec)
		if len(q) < n {
			n = len(q)
		}
		for i := 0; i < n; i++ {
			score += float64(q[i]) * float64(vec[i])
		}
		hits = append(hits, retrieval.VectorHit{ID: id, Score: score})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// ---------------------------------------------------------------------------
// 向量表持久化
// ---------------------------------------------------------------------------

// encodeVector 把 float32 切片编码为 little-endian BLOB。
// 出处：core/storage/vector_store.py 用 numpy tobytes（同样是行优先 little-endian）。
func encodeVector(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

// decodeVector 从 BLOB 解码 float32 切片。
func decodeVector(buf []byte, dim int) []float32 {
	if dim <= 0 || len(buf) < dim*4 {
		return nil
	}
	out := make([]float32, dim)
	for i := 0; i < dim; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return out
}

// loadVectorIndex 启动时从 vectors 表重建内存 flat 索引。
func (s *Store) loadVectorIndex(ctx context.Context) error {
	rows, err := s.read.QueryContext(ctx, `SELECT id, item_type, dim, vector FROM vectors`)
	if err != nil {
		return fmt.Errorf("memorix/storage: 读取 vectors 失败: %w", err)
	}
	defer rows.Close()

	index := NewVectorIndex(0)
	for rows.Next() {
		var (
			id       string
			itemType string
			dim      int
			blob     []byte
		)
		if err := rows.Scan(&id, &itemType, &dim, &blob); err != nil {
			return fmt.Errorf("memorix/storage: 解析 vectors 行失败: %w", err)
		}
		vec := decodeVector(blob, dim)
		if len(vec) == 0 {
			continue
		}
		// 表中已是归一化向量，不再二次归一化。
		_ = index.Add(itemType, id, vec, false)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("memorix/storage: 遍历 vectors 失败: %w", err)
	}
	s.vectorMu.Lock()
	s.vectors = index
	s.dim = index.Dimension()
	s.vectorMu.Unlock()
	return nil
}

// VectorIndexRef 返回当前内存向量索引（只读语义由调用方保证）。
func (s *Store) VectorIndexRef() *VectorIndex {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.vectors
}

// Dimension 返回向量维度（0 表示尚未写入任何向量）。
func (s *Store) Dimension() int {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.dim
}

// UpsertVector 持久化并索引一条向量，同时返回归一化后的向量。
func (s *Store) UpsertVector(ctx context.Context, itemType, id string, vec []float32) error {
	if id == "" {
		return fmt.Errorf("memorix/storage: 向量 id 不能为空")
	}
	if len(vec) == 0 {
		return fmt.Errorf("memorix/storage: 向量为空")
	}
	normalized, norm := safeNormalize(vec)
	if norm <= 0 {
		return fmt.Errorf("memorix/storage: 向量范数为 0")
	}
	if err := s.ensureDimension(len(vec)); err != nil {
		return err
	}
	now := nowTS()
	_, err := s.write.ExecContext(ctx, `
		INSERT INTO vectors(id, item_type, dim, quantization, vector, norm, created_at, updated_at)
		VALUES (?, ?, ?, 'float32', ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			item_type=excluded.item_type, dim=excluded.dim, vector=excluded.vector,
			norm=excluded.norm, updated_at=excluded.updated_at`,
		id, itemType, len(vec), encodeVector(normalized), norm, now, now)
	if err != nil {
		return fmt.Errorf("memorix/storage: 写 vectors 失败: %w", err)
	}
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	if s.vectors == nil {
		s.vectors = NewVectorIndex(len(vec))
	}
	return s.vectors.Add(itemType, id, normalized, false)
}

// DeleteVector 删除一条向量（内存 + 表）。
func (s *Store) DeleteVector(ctx context.Context, itemType, id string) error {
	if _, err := s.write.ExecContext(ctx, `DELETE FROM vectors WHERE id = ?`, id); err != nil {
		return fmt.Errorf("memorix/storage: 删除 vectors 失败: %w", err)
	}
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	if s.vectors != nil {
		s.vectors.Remove(itemType, id)
	}
	return nil
}

// DeleteVectorsByType 删除某类全部向量。
func (s *Store) DeleteVectorsByType(ctx context.Context, itemType string) error {
	if _, err := s.write.ExecContext(ctx, `DELETE FROM vectors WHERE item_type = ?`, itemType); err != nil {
		return fmt.Errorf("memorix/storage: 按类型删除 vectors 失败: %w", err)
	}
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	if s.vectors != nil {
		count := s.vectors.CountByType(itemType)
		s.vectors.pools[itemType] = map[string][]float32{}
		s.vectors.count -= count
	}
	return nil
}

// ensureDimension 固定索引维度（首个写入向量决定维度；不一致直接报错）。
// 对齐 Python 的 stored_vector_dimension 校验语义。
func (s *Store) ensureDimension(dim int) error {
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	if s.dim == 0 {
		s.dim = dim
		if s.vectors == nil {
			s.vectors = NewVectorIndex(dim)
		}
		return nil
	}
	if s.dim != dim {
		return fmt.Errorf("memorix/storage: 向量维度不匹配: 已存 %d, 新写入 %d", s.dim, dim)
	}
	return nil
}

// SearchVectors 在指定池内做余弦 TopK。
func (s *Store) SearchVectors(itemType string, query []float32, k int, allowed map[string]struct{}) []retrieval.VectorHit {
	s.vectorMu.RLock()
	index := s.vectors
	s.vectorMu.RUnlock()
	if index == nil {
		return nil
	}
	return index.Search(itemType, query, k, allowed)
}

// VectorStats 返回向量池统计（供 admin/日志）。
func (s *Store) VectorStats(ctx context.Context) map[string]int {
	out := map[string]int{}
	rows, err := s.read.QueryContext(ctx, `SELECT item_type, COUNT(1) FROM vectors GROUP BY item_type`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var itemType string
		var count int
		if err := rows.Scan(&itemType, &count); err == nil {
			out[itemType] = count
		}
	}
	return out
}

// scanNullString 读取可空字符串列。
func scanNullString(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}
