package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"oh-my-qqbot/internal/memorix/retrieval"
)

// Graph 构建实体图快照（对齐 GraphStore：节点=规范化实体名，边由关系产生，
// 边权重=关系置信度累加）。
//
// 出处：core/storage/graph_store.py（_adjacency 由 add_relation 维护）、
// core/retrieval/graph_relation_recall.py（沿边取 relation hash）。
func (s *Store) Graph(ctx context.Context) (*retrieval.Graph, error) {
	s.graphMu.Lock()
	defer s.graphMu.Unlock()

	var revision int64
	if err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(COUNT(1), 0) FROM relations WHERE (is_inactive IS NULL OR is_inactive = 0)`).
		Scan(&revision); err != nil {
		return nil, fmt.Errorf("memorix/storage: 统计关系失败: %w", err)
	}
	var entityRevision int64
	if err := s.read.QueryRowContext(ctx,
		`SELECT COALESCE(COUNT(1), 0) FROM entities WHERE (is_deleted IS NULL OR is_deleted = 0)`).
		Scan(&entityRevision); err != nil {
		return nil, fmt.Errorf("memorix/storage: 统计实体失败: %w", err)
	}
	combined := revision*1_000_003 + entityRevision
	if s.graph != nil && s.graphRev == combined {
		return s.graph, nil
	}

	rows, err := s.read.QueryContext(ctx, `
		SELECT hash, subject, predicate, object, confidence
		FROM relations WHERE (is_inactive IS NULL OR is_inactive = 0)
		ORDER BY hash`)
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: 查询关系失败: %w", err)
	}
	defer rows.Close()

	graph := retrieval.NewGraph()
	for rows.Next() {
		var (
			hash, subject, predicate, object string
			confidence                       float64
		)
		if err := rows.Scan(&hash, &subject, &predicate, &object, &confidence); err != nil {
			return nil, fmt.Errorf("memorix/storage: 解析关系行失败: %w", err)
		}
		if confidence <= 0 {
			confidence = 1.0
		}
		// 图节点使用规范化名（对齐 GraphStore._canonicalize）。
		from := canonicalEntityName(subject)
		to := canonicalEntityName(object)
		if from == "" || to == "" {
			continue
		}
		graph.AddEdge(from, to, confidence)
		graph.SetEdgeRelations(from, to, []string{hash})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 孤立实体（只有出/入自环之外无关系）也作为节点保留，以便 AC 匹配与 PPR 种子。
	names, err := s.EntityNames(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		canonical := canonicalEntityName(name)
		if canonical == "" {
			continue
		}
		if _, ok := graph.Out[canonical]; !ok {
			graph.AddEdge(canonical, canonical, 0)
			graph.RemoveEdge(canonical, canonical)
			graph.AddNode(canonical)
		}
	}
	graph.Rev = combined
	s.graph = graph
	s.graphRev = combined
	return graph, nil
}

// InvalidateGraph 主动失效图缓存（写入关系后由上层调用）。
func (s *Store) InvalidateGraph() {
	s.graphMu.Lock()
	s.graphRev = -1
	s.graph = nil
	s.graphMu.Unlock()
}

// LookupEntities 返回 name → hash 映射（供 AC 匹配回映射）。
func (s *Store) LookupEntities(ctx context.Context) (map[string]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT hash, name FROM entities WHERE (is_deleted IS NULL OR is_deleted = 0)`)
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: 查询实体失败: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var hash, name string
		if err := rows.Scan(&hash, &name); err != nil {
			return nil, err
		}
		out[name] = hash
	}
	return out, rows.Err()
}

// EntityTopRelations 返回实体（按名）关联度最高的关系，用于画像 relations。
func (s *Store) EntityTopRelations(ctx context.Context, personName string, limit int) ([]*retrieval.Relation, error) {
	return s.RelationsForPerson(ctx, personName, limit)
}

// StatsSnapshot 汇总库内规模（供 admin/日志）。
func (s *Store) StatsSnapshot(ctx context.Context) map[string]any {
	counts := map[string]any{}
	for _, table := range []string{"paragraphs", "entities", "relations", "episodes", "profiles", "vectors", "profile_facts"} {
		var count int
		if err := s.read.QueryRowContext(ctx, `SELECT COUNT(1) FROM `+table).Scan(&count); err == nil {
			counts[table] = count
		}
	}
	counts["fts_available"] = s.FTSSearchable(ctx)
	counts["dimension"] = s.Dimension()
	return counts
}

// SortedEntityNames 返回排序后的实体名（确定性遍历）。
func (s *Store) SortedEntityNames(ctx context.Context) ([]string, error) {
	names, err := s.EntityNames(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// DeleteParagraph 逻辑删除段落（对齐 is_deleted 语义）。
func (s *Store) DeleteParagraph(ctx context.Context, hash string) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE paragraphs SET is_deleted = 1, updated_at = ? WHERE hash = ?`, nowTS(), hash)
	if err != nil {
		return fmt.Errorf("memorix/storage: 删除段落失败: %w", err)
	}
	return nil
}

// ParagraphsForChat 返回某会话的段落（limit <= 0 表示不限制）。
// 供检索 scope 解析与 episode 生成使用。
func (s *Store) ParagraphsForChat(ctx context.Context, chatID string, limit int) ([]*retrieval.Paragraph, error) {
	if limit <= 0 {
		return s.queryParagraphs(ctx, `
			WHERE chat_id = ? AND (is_deleted IS NULL OR is_deleted = 0)
			ORDER BY created_at DESC`, chatID)
	}
	return s.queryParagraphs(ctx, `
		WHERE chat_id = ? AND (is_deleted IS NULL OR is_deleted = 0)
		ORDER BY created_at DESC LIMIT ?`, chatID, limit)
}

// RelationHashesForChat 返回某会话段落关联的全部关系 hash（检索 scope 用）。
func (s *Store) RelationHashesForChat(ctx context.Context, chatID string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT DISTINCT pl.relation_hash
		FROM paragraph_links pl JOIN paragraphs p ON p.hash = pl.paragraph_hash
		WHERE p.chat_id = ? AND (p.is_deleted IS NULL OR p.is_deleted = 0)`, chatID)
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: 查询会话关系失败: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		out = append(out, hash)
	}
	return out, rows.Err()
}

// EntityHashesForChat 返回某会话段落关联的全部实体 hash（检索 scope 用）。
func (s *Store) EntityHashesForChat(ctx context.Context, chatID string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT DISTINCT el.entity_hash
		FROM entity_links el JOIN paragraphs p ON p.hash = el.paragraph_hash
		WHERE p.chat_id = ? AND (p.is_deleted IS NULL OR p.is_deleted = 0)`, chatID)
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: 查询会话实体失败: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		out = append(out, hash)
	}
	return out, rows.Err()
}

// EnsureStringSet 把字符串切片转为集合（供 scope 构造复用）。
func EnsureStringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			continue
		}
		out[strings.TrimSpace(v)] = struct{}{}
	}
	return out
}
