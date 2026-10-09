package learners

import (
	"context"
	"strings"

	"oh-my-qqbot/internal/chat/replyer"
)

// This file provides the concrete implementation of the P4b read-only interface
// 参考/MaiBot/src/chat/replyer/maisaka_expression_selector.py's candidate source
// (declared as replyer.ExpressionSource in internal/chat/replyer/generator.go).
//
// 依赖方向说明：本包对 internal/chat/replyer 只做「接口对齐」用途（只读），
// 与 replyer 之间不存在环（replyer 不 import learners）。

// 编译期断言：ReplyerExpressionSource 满足 replyer.ExpressionSource。
var _ replyer.ExpressionSource = (*ReplyerExpressionSource)(nil)

// ReplyerExpressionSource 把表达学习器的仓储与向量索引暴露为 replyer 的
// 候选来源（replyer 侧由 MaisakaExpressionSelector 消费）。
type ReplyerExpressionSource struct {
	// Store 是表达方式仓储（读取候选）。
	Store ExpressionStore
	// Index 是表达向量索引（向量召回池；nil 时回退随手候选）。
	Index *ExpressionVectorIndex
	// CheckedOnly mirrors expression_checked_only（只取人工审核过的候选）。
	CheckedOnly bool
	// UseVector mirrors use_vector_expression。
	UseVector bool
	// PoolSize mirrors expression_vector_candidate_pool_size（默认 50）。
	PoolSize int
	// ClusterPoolSize mirrors MaisakaExpressionSelector._VECTOR_CLUSTER_POOL_SIZE（16）。
	ClusterPoolSize int
	Logf            func(format string, args ...any)
}

// NewReplyerExpressionSource 构造 replyer 侧候选来源。
func NewReplyerExpressionSource(store ExpressionStore, index *ExpressionVectorIndex) *ReplyerExpressionSource {
	return &ReplyerExpressionSource{Store: store, Index: index, UseVector: true}
}

func (s *ReplyerExpressionSource) log(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// LoadAllCandidates mirrors `_load_all_expression_candidates`.
func (s *ReplyerExpressionSource) LoadAllCandidates(sessionID string) []replyer.ExpressionCandidate {
	if s == nil || s.Store == nil {
		return nil
	}
	rows, err := s.Store.LoadCandidates(sessionID, s.CheckedOnly)
	if err != nil {
		s.log("加载表达候选失败: %v", err)
		return nil
	}
	candidates := make([]replyer.ExpressionCandidate, 0, len(rows))
	for _, row := range rows {
		normalizedStyle := NormalizeExpressionStyleForLearning(row.Style)
		situation := strings.TrimSpace(row.Situation)
		if row.ID == 0 || situation == "" || normalizedStyle == "" {
			continue
		}
		if IsPromptExampleExpressionStyle(normalizedStyle) {
			continue
		}
		count := row.Count
		if count < 1 {
			count = 1
		}
		candidates = append(candidates, replyer.ExpressionCandidate{
			ID:        row.ID,
			Situation: situation,
			Style:     normalizedStyle,
			Count:     count,
		})
	}
	return candidates
}

// BuildVectorCandidatePool mirrors `_build_expression_candidate_pool` 的向量分支。
//
// ok=false 表示调用方应回退到随手候选（对齐 Python 的 fallback 语义）。
func (s *ReplyerExpressionSource) BuildVectorCandidatePool(
	ctx context.Context,
	sessionID string,
	queryText string,
	all []replyer.ExpressionCandidate,
	poolSize int,
) ([]replyer.ExpressionCandidate, bool) {
	if s == nil || s.Index == nil || len(all) == 0 {
		return nil, false
	}
	scoped := make([]ScopedCandidate, 0, len(all))
	byID := make(map[int64]replyer.ExpressionCandidate, len(all))
	for _, candidate := range all {
		scoped = append(scoped, ScopedCandidate{
			ID:        candidate.ID,
			Situation: candidate.Situation,
			Style:     candidate.Style,
			Count:     candidate.Count,
		})
		byID[candidate.ID] = candidate
	}
	effectivePool := poolSize
	if effectivePool <= 0 {
		effectivePool = s.PoolSize
	}
	if effectivePool <= 0 {
		effectivePool = 50
	}
	clusterPool := s.ClusterPoolSize
	if clusterPool <= 0 {
		clusterPool = 16
	}
	scored, err := s.Index.SelectCandidates(ctx, queryText, scoped, effectivePool, clusterPool)
	if err != nil {
		s.log("表达向量召回失败，回退随手候选: %v", err)
		return nil, false
	}
	if len(scored) == 0 {
		return nil, false
	}
	out := make([]replyer.ExpressionCandidate, 0, len(scored))
	for _, item := range scored {
		candidate, ok := byID[item.ID]
		if !ok {
			continue
		}
		// 对齐 select_candidates 的输出：count 以索引外的作用域候选为准。
		candidate.Count = item.Count
		out = append(out, candidate)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// UseVectorExpression mirrors `_use_vector_candidate_pool`.
func (s *ReplyerExpressionSource) UseVectorExpression() bool {
	if s == nil {
		return false
	}
	return s.UseVector && s.Index != nil
}
