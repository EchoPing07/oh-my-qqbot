package retrieval

import (
	"context"
	"sort"
	"strings"
)

// GraphSource 是图关系召回与检索回表所需的存储访问面（由 storage 层实现）。
// 对齐 Python GraphRelationRecallService 用到的 GraphStore + MetadataStore 方法。
type GraphSource interface {
	// Graph 返回当前实体图快照（storage 侧缓存，Rev 变化时重建）。
	Graph(ctx context.Context) (*Graph, error)
	// RelationByHash 按 hash 读取关系（includeInactive=false）。
	RelationByHash(ctx context.Context, hash string) (*Relation, error)
	// RelationsByHashes 批量读取关系。
	RelationsByHashes(ctx context.Context, hashes []string) (map[string]*Relation, error)
	// RelationsForEntity 返回以实体名为 subject 或 object 的关系（对齐 get_relations）。
	RelationsForEntity(ctx context.Context, name string) ([]*Relation, error)
	// ParagraphsByRelation 返回关系的支撑段落。
	ParagraphsByRelation(ctx context.Context, relationHash string) ([]*Paragraph, error)
	// ParagraphsByEntity 返回实体的支撑段落。
	ParagraphsByEntity(ctx context.Context, entityHash string) ([]*Paragraph, error)
	// ParagraphsByHashes 批量读取段落。
	ParagraphsByHashes(ctx context.Context, hashes []string) (map[string]*Paragraph, error)
	// EntitiesByHashes 批量读取实体。
	EntitiesByHashes(ctx context.Context, hashes []string) (map[string]*Entity, error)
	// EntityByName 按规范化名称解析实体（不区分大小写）。
	EntityByName(ctx context.Context, name string) (*Entity, error)
	// EntityNamesByParagraphs 返回段落关联的实体名（供 PPR 增益与后验图门使用）。
	EntityNamesByParagraphs(ctx context.Context, paragraphHashes []string) (map[string][]string, error)
	// TemporalParagraphs 按时序索引查询段落（对齐 query_paragraphs_temporal）。
	TemporalParagraphs(ctx context.Context, opts TemporalQueryOptions, limit int) ([]*Paragraph, error)
}

// graphRecallParagraphCache 缓存一次召回期间的段落支撑关系算力开销。
type GraphRecallService struct {
	Config GraphRecallConfig
	Source GraphSource
}

// NewGraphRecallService 构造图召回服务。
func NewGraphRecallService(source GraphSource, config GraphRecallConfig) *GraphRecallService {
	if config.CandidateK < 1 {
		config.CandidateK = DefaultGraphRecallCandidateK
	}
	if config.MaxHop < 1 {
		config.MaxHop = DefaultGraphRecallMaxHop
	}
	if config.MaxPaths < 1 {
		config.MaxPaths = DefaultGraphRecallMaxPaths
	}
	return &GraphRecallService{Config: config, Source: source}
}

// Recall 收集关系候选；allowedRelationIDs 为 nil 表示不限制。
// 出处：core/retrieval/graph_relation_recall.py L73-L120。
func (s *GraphRecallService) Recall(
	ctx context.Context,
	seedEntities []string,
	allowedRelationIDs map[string]struct{},
) ([]GraphCandidate, error) {
	if !s.Config.Enabled || s.Source == nil {
		return nil, nil
	}
	if allowedRelationIDs != nil && len(allowedRelationIDs) == 0 {
		return nil, nil
	}
	graph, err := s.Source.Graph(ctx)
	if err != nil {
		return nil, err
	}
	seeds, err := s.normalizeSeeds(ctx, graph, seedEntities)
	if err != nil {
		return nil, err
	}
	if len(seeds) == 0 {
		return nil, nil
	}

	seen := map[string]struct{}{}
	var out []GraphCandidate
	if len(seeds) >= 2 {
		if err := s.collectDirectPair(ctx, graph, seeds[0], seeds[1], seen, allowedRelationIDs, &out); err != nil {
			return nil, err
		}
		if len(out) < DefaultGraphRecallTwoHopMinCount &&
			s.Config.AllowTwoHopPair &&
			len(out) < s.Config.CandidateK {
			if err := s.collectTwoHopPair(ctx, graph, seeds[0], seeds[1], seen, allowedRelationIDs, &out); err != nil {
				return nil, err
			}
		}
	} else {
		if err := s.collectOneHopSeed(ctx, graph, seeds[0], seen, allowedRelationIDs, &out); err != nil {
			return nil, err
		}
	}
	if len(out) > s.Config.CandidateK {
		out = out[:s.Config.CandidateK]
	}
	return out, nil
}

// normalizeSeeds 解析种子到真实节点名（最多 2 个，去重）。
// 出处：core/retrieval/graph_relation_recall.py L122-L138。
func (s *GraphRecallService) normalizeSeeds(ctx context.Context, graph *Graph, seedEntities []string) ([]string, error) {
	var out []string
	seen := map[string]struct{}{}
	limit := len(seedEntities)
	if limit > 2 {
		limit = 2
	}
	for _, raw := range seedEntities[:limit] {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		entity, err := s.Source.EntityByName(ctx, raw)
		if err != nil {
			return nil, err
		}
		if entity == nil {
			continue
		}
		node := entity.Name
		if canonical := graph.FindNode(node); canonical != "" {
			node = canonical
		}
		key := strings.ToLower(strings.TrimSpace(node))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, node)
	}
	return out, nil
}

// collectDirectPair 收集两个种子间的一跳直接关系。
// 出处：core/retrieval/graph_relation_recall.py L140-L158。
func (s *GraphRecallService) collectDirectPair(
	ctx context.Context, graph *Graph, seedA, seedB string,
	seen map[string]struct{}, allowed map[string]struct{}, out *[]GraphCandidate,
) error {
	hashes := append([]string{}, graph.EdgeRelations(seedA, seedB)...)
	hashes = append(hashes, graph.EdgeRelations(seedB, seedA)...)
	return s.appendRelationHashes(ctx, hashes, seen, allowed, out, "direct_pair", 1, []string{seedA, seedB})
}

// collectTwoHopPair 收集两跳路径上的关系。
// 出处：core/retrieval/graph_relation_recall.py L160-L195。
func (s *GraphRecallService) collectTwoHopPair(
	ctx context.Context, graph *Graph, seedA, seedB string,
	seen map[string]struct{}, allowed map[string]struct{}, out *[]GraphCandidate,
) error {
	paths := graph.FindPaths(seedA, seedB, 2, s.Config.MaxPaths)
	for _, pathNodes := range paths {
		if len(*out) >= s.Config.CandidateK {
			break
		}
		if len(pathNodes) != 3 {
			continue
		}
		for idx := 0; idx+1 < len(pathNodes); idx++ {
			if len(*out) >= s.Config.CandidateK {
				break
			}
			u, v := pathNodes[idx], pathNodes[idx+1]
			hashes := append([]string{}, graph.EdgeRelations(u, v)...)
			hashes = append(hashes, graph.EdgeRelations(v, u)...)
			if err := s.appendRelationHashes(ctx, hashes, seen, allowed, out, "two_hop_pair", 2,
				[]string{seedA, seedB}); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectOneHopSeed 收集单种子的一跳邻接关系。
// 出处：core/retrieval/graph_relation_recall.py L197-L216。
func (s *GraphRecallService) collectOneHopSeed(
	ctx context.Context, graph *Graph, seed string,
	seen map[string]struct{}, allowed map[string]struct{}, out *[]GraphCandidate,
) error {
	limit := s.Config.CandidateK
	if allowed != nil {
		limit = 0
	}
	incidents := graph.IncidentRelationHashes(seed)
	hashes := make([]string, 0, len(incidents))
	for _, h := range incidents {
		hashes = append(hashes, h)
		if limit > 0 && len(hashes) >= limit {
			break
		}
	}
	return s.appendRelationHashes(ctx, hashes, seen, allowed, out, "one_hop_seed", 1, []string{seed})
}

// appendRelationHashes 按排序去重后构造候选。
// 出处：core/retrieval/graph_relation_recall.py L238-L268。
func (s *GraphRecallService) appendRelationHashes(
	ctx context.Context, hashes []string, seen map[string]struct{}, allowed map[string]struct{},
	out *[]GraphCandidate, candidateType string, hops int, seeds []string,
) error {
	unique := map[string]struct{}{}
	for _, h := range hashes {
		if strings.TrimSpace(h) == "" {
			continue
		}
		unique[h] = struct{}{}
	}
	sortedHashes := make([]string, 0, len(unique))
	for h := range unique {
		sortedHashes = append(sortedHashes, h)
	}
	sort.Strings(sortedHashes)

	for _, h := range sortedHashes {
		if len(*out) >= s.Config.CandidateK {
			break
		}
		if _, ok := seen[h]; ok {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[h]; !ok {
				continue
			}
		}
		candidate, err := s.buildCandidate(ctx, h, candidateType, hops, seeds)
		if err != nil {
			return err
		}
		if candidate == nil {
			continue
		}
		seen[h] = struct{}{}
		*out = append(*out, *candidate)
	}
	return nil
}

// buildCandidate 读取关系与支撑段落数构造候选。
// 出处：core/retrieval/graph_relation_recall.py L270-L285。
func (s *GraphRecallService) buildCandidate(
	ctx context.Context, hash, candidateType string, hops int, seeds []string,
) (*GraphCandidate, error) {
	relation, err := s.Source.RelationByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if relation == nil {
		return nil, nil
	}
	paragraphs, err := s.Source.ParagraphsByRelation(ctx, hash)
	if err != nil {
		return nil, err
	}
	confidence := relation.Confidence
	if confidence <= 0 {
		confidence = 1.0
	}
	return &GraphCandidate{
		Hash:                     hash,
		Subject:                  relation.Subject,
		Predicate:                relation.Predicate,
		Object:                   relation.Object,
		Confidence:               confidence,
		GraphSeedEntities:        append([]string{}, seeds...),
		GraphHops:                hops,
		GraphCandidateType:       candidateType,
		SupportingParagraphCount: len(paragraphs),
	}, nil
}

// BuildGraphResultsFromSeeds 由种子实体构造关系检索结果（后验图门复用）。
// 出处：core/retrieval/posterior_graph.py L452-L519。
func (r *Retriever) BuildGraphResultsFromSeeds(
	ctx context.Context,
	seedEntities []string,
	temporal *TemporalQueryOptions,
	alpha float64,
	scope *Scope,
) ([]*Result, error) {
	if r.graphRecall == nil {
		return nil, nil
	}
	var allowed map[string]struct{}
	if scope != nil && len(scope.RelationIDs) > 0 {
		allowed = scope.RelationIDs
	}
	payloads, err := r.graphRecall.Recall(ctx, seedEntities, allowed)
	if err != nil {
		return nil, err
	}
	if len(payloads) == 0 {
		return nil, nil
	}
	results := make([]*Result, 0, len(payloads))
	for _, payload := range payloads {
		results = append(results, &Result{
			Hash:    payload.Hash,
			Content: strings.TrimSpace(payload.Subject + " " + payload.Predicate + " " + payload.Object),
			Score:   0,
			Type:    "relation",
			Source:  "posterior_graph_recall",
			Metadata: map[string]any{
				"subject":                    payload.Subject,
				"predicate":                  payload.Predicate,
				"object":                     payload.Object,
				"confidence":                 payload.Confidence,
				"graph_seed_entities":        payload.GraphSeedEntities,
				"graph_hops":                 payload.GraphHops,
				"graph_candidate_type":       payload.GraphCandidateType,
				"supporting_paragraph_count": payload.SupportingParagraphCount,
			},
		})
	}
	results, err = r.applyTemporalFilterToRelations(ctx, results, temporal)
	if err != nil {
		return nil, err
	}
	results = r.MergeRelationResultsGraphEnhanced(ctx, nil, nil, results)
	relationWeight := maxFloat(0, 1.0-alpha)
	for _, item := range results {
		item.Score *= relationWeight
		item.Source = "posterior_graph_competition"
	}
	return results, nil
}

// MergeRelationResultsGraphEnhanced 融合语义/稀疏/图结构三路关系候选。
// 出处：core/retrieval/dual_path.py L1005-L1079。
func (r *Retriever) MergeRelationResultsGraphEnhanced(
	ctx context.Context,
	vectorResults, sparseResults, graphResults []*Result,
) []*Result {
	merged := map[string]*Result{}
	sourceSets := map[string]map[string]struct{}{}
	order := make([]string, 0)
	for _, group := range [][]*Result{vectorResults, sparseResults, graphResults} {
		for _, item := range group {
			existing, ok := merged[item.Hash]
			if !ok {
				clone := &Result{
					Hash:     item.Hash,
					Content:  item.Content,
					Score:    item.Score,
					Type:     item.Type,
					Source:   item.Source,
					Metadata: map[string]any{},
				}
				for k, v := range item.Metadata {
					clone.Metadata[k] = v
				}
				merged[item.Hash] = clone
				order = append(order, item.Hash)
			} else {
				for k, v := range item.Metadata {
					if _, has := existing.Metadata[k]; !has {
						existing.Metadata[k] = v
					}
				}
			}
			sources, ok := sourceSets[item.Hash]
			if !ok {
				sources = map[string]struct{}{}
				sourceSets[item.Hash] = sources
			}
			name := strings.TrimSpace(item.Source)
			if name == "" {
				name = "relation_search"
			}
			sources[name] = struct{}{}
		}
	}

	vectorNorm := minMaxMapFromResults(vectorResults)
	sparseNorm := minMaxMapFromResults(sparseResults)

	out := make([]*Result, 0, len(order))
	for _, hash := range order {
		item := merged[hash]
		meta := item.Metadata
		semanticNorm := maxFloat(vectorNorm[hash], sparseNorm[hash])
		graphScore := GraphScoreForType(stringify(meta["graph_candidate_type"]))

		supportCount := int(toFloat(meta["supporting_paragraph_count"]))
		if supportCount < 0 {
			supportCount = 0
		}
		evidenceScore := minFloat(1.0, float64(supportCount)/RelationSupportParagraphTarget)
		meta["supporting_paragraph_count"] = supportCount
		if _, ok := meta["graph_seed_entities"]; !ok {
			meta["graph_seed_entities"] = []string{}
		}
		if _, ok := meta["graph_hops"]; ok {
			meta["graph_hops"] = int(toFloat(meta["graph_hops"]))
		}
		item.Score = RelationMergeSemanticWeight*semanticNorm +
			RelationMergeGraphWeight*graphScore +
			RelationMergeEvidenceWeight*evidenceScore

		sources := sourceSets[hash]
		if len(sources) > 1 {
			item.Source = "relation_fusion"
		} else {
			for name := range sources {
				item.Source = name
			}
		}
		out = append(out, item)
	}
	sortResults(out)
	return out
}

// applyTemporalFilterToRelations 关系时序过滤（用最佳支撑段落的 time_meta）。
// 出处：core/retrieval/dual_path.py L3133-L3153。
func (r *Retriever) applyTemporalFilterToRelations(
	ctx context.Context, results []*Result, temporal *TemporalQueryOptions,
) ([]*Result, error) {
	if temporal == nil {
		return results, nil
	}
	filtered := make([]*Result, 0, len(results))
	for _, item := range results {
		paragraphs, err := r.Source.ParagraphsByRelation(ctx, item.Hash)
		if err != nil {
			return nil, err
		}
		var bestMeta map[string]any
		bestTime := -1e308
		for _, paragraph := range paragraphs {
			if !r.isTemporalMatch(paragraph, *temporal) {
				continue
			}
			meta := buildTimeMeta(paragraph, temporal)
			effective := toFloat(meta["effective_end"])
			if effective >= bestTime {
				bestTime = effective
				bestMeta = meta
			}
		}
		if bestMeta == nil {
			continue
		}
		item.Metadata["time_meta"] = bestMeta
		filtered = append(filtered, item)
	}
	return sortResultsWithTemporal(filtered), nil
}

// minMaxMapFromResults 按 hash 生成 min-max 归一化分数表。
func minMaxMapFromResults(results []*Result) map[string]float64 {
	if len(results) == 0 {
		return map[string]float64{}
	}
	ids := make([]string, 0, len(results))
	scores := make([]float64, 0, len(results))
	for _, item := range results {
		ids = append(ids, item.Hash)
		scores = append(scores, item.Score)
	}
	return BuildMinMaxScoreMap(ids, scores)
}

// paragraphEntityNames 批量查询段落关联实体名。
func (r *Retriever) paragraphEntityNames(ctx context.Context, hashes []string) (map[string][]string, error) {
	if len(hashes) == 0 || r.Source == nil {
		return map[string][]string{}, nil
	}
	return r.Source.EntityNamesByParagraphs(ctx, hashes)
}

// temporalParagraphs 按时序条件查询段落。
func temporalParagraphs(
	ctx context.Context, source GraphSource, opts TemporalQueryOptions, scopeKey string, limit int,
) ([]*Paragraph, error) {
	_ = scopeKey
	if source == nil {
		return nil, nil
	}
	return source.TemporalParagraphs(ctx, opts, limit)
}

// SetEdgeRelations 记录一条边上的关系 hash（供 storage 层构建图时调用）。
func (g *Graph) SetEdgeRelations(from, to string, hashes []string) {
	if g.edgeRelations == nil {
		g.edgeRelations = map[string][]string{}
	}
	key := edgeKey(from, to)
	for _, h := range hashes {
		if strings.TrimSpace(h) == "" {
			continue
		}
		exists := false
		for _, old := range g.edgeRelations[key] {
			if old == h {
				exists = true
				break
			}
		}
		if !exists {
			g.edgeRelations[key] = append(g.edgeRelations[key], h)
		}
	}
}

// edgeKey 构造边键（分隔符使用 NUL，避免与实体名冲突）。
func edgeKey(from, to string) string { return from + "\u0000" + to }
