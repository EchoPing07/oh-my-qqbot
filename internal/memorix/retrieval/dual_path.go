package retrieval

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Embedder 是检索层需要的最小嵌入接口（由 memorix.Embedder 实现/注入）。
type Embedder interface {
	// Encode 计算单条查询的嵌入；失败返回错误。
	Encode(ctx context.Context, text string) ([]float32, error)
	// Dimension 返回当前向量维度（0 表示未知）。
	Dimension() int
}

// VectorSource 是段落/关系/实体向量的检索面（由 storage 层内存索引实现）。
type VectorSource interface {
	// Dimension 返回索引维度。
	Dimension() int
	// Count 返回索引规模（用于 PPR 局部模式判定）。
	Count() int
	// SearchParagraphs 向量召回段落。
	SearchParagraphs(query []float32, k int, allowed map[string]struct{}) ([]VectorHit, error)
	// SearchGraph 向量召回图资产，ID 形如 "relation:<hash>" / "entity:<hash>"。
	SearchGraph(query []float32, k int, allowed map[string]struct{}) ([]VectorHit, error)
	// SearchVectors 按资产类型召回向量（"entity" / "relation" / "paragraph"）。
	SearchVectors(itemType string, query []float32, k int, allowed map[string]struct{}) ([]VectorHit, error)
}

// RetrieverConfig 是 DualPathRetriever 的完整配置，默认值照抄 Python。
type RetrieverConfig struct {
	TopKParagraphs int
	TopKRelations  int
	TopKFinal      int
	// Alpha 是段落与关系的融合权重：0=仅关系，1=仅段落。
	Alpha            float64
	EnablePPR        bool
	PPRAlpha         float64
	PPRTimeout       time.Duration
	PPRLocalEnabled  bool
	PPRLocalMaxNodes int
	PPRLocalHops     int
	PPRLocalMinNodes int
	EnableParallel   bool
	Strategy         Strategy
	Sparse           SparseConfig
	Fusion           FusionConfig
	RelationIntent   RelationIntentConfig
	VectorPools      VectorPoolsConfig
	GraphRecall      GraphRecallConfig
	PosteriorGraph   PosteriorConfig
}

// DefaultRetrieverConfig 返回照抄 Python DualPathRetrieverConfig 默认值。
func DefaultRetrieverConfig() RetrieverConfig {
	return RetrieverConfig{
		TopKParagraphs:   DefaultTopKParagraphs,
		TopKRelations:    DefaultTopKRelations,
		TopKFinal:        DefaultTopKFinal,
		Alpha:            DefaultAlpha,
		EnablePPR:        true,
		PPRAlpha:         DefaultPPRAlpha,
		PPRTimeout:       time.Duration(DefaultPPRTimeout * float64(time.Second)),
		PPRLocalEnabled:  DefaultPPRLocalEnabled,
		PPRLocalMaxNodes: DefaultPPRLocalMaxNodes,
		PPRLocalHops:     DefaultPPRLocalHops,
		PPRLocalMinNodes: DefaultPPRLocalMinNodes,
		EnableParallel:   true,
		Strategy:         StrategyDualPath,
		Sparse:           DefaultSparseConfig(),
		Fusion:           DefaultFusionConfig(),
		RelationIntent:   DefaultRelationIntentConfig(),
		VectorPools:      DefaultVectorPoolsConfig(),
		GraphRecall:      DefaultGraphRecallConfig(),
		PosteriorGraph:   DefaultPosteriorConfig(),
	}
}

// Retriever 是双路检索器。
//
// 说明：Python 版默认走 `vector_pools.mode == "dual"` 的双向量池路径
// （core/retrieval/dual_path.py L1276-L1283），Go 版同样以此为默认路径；
// 因此本实现组合了「段落向量池 + 图证据池 + 稀疏段 + 图可靠性缩放」
// 的一条主链，并保留段落路/关系路的经典融合入口（FuseResults）。
type Retriever struct {
	Config  RetrieverConfig
	Source  GraphSource
	Vectors VectorSource
	Index   *SparseIndex
	Embed   Embedder

	ppr *PersonalizedPageRank
	// acMu 保护 AC 自动机缓存（检索可被多 goroutine 并发调用）。
	acMu sync.Mutex
	ac   *AhoCorasick
	// acRev 记录 AC 自动机对应的实体版本，用于按 revision 重建。
	acRev       int64
	graphRecall *GraphRecallService
	relationRe  *regexp.Regexp
}

// NewRetriever 创建双路检索器；embed/vectors/index 均可为 nil（纯稀疏降级）。
func NewRetriever(
	source GraphSource,
	vectors VectorSource,
	index *SparseIndex,
	embed Embedder,
	config RetrieverConfig,
) *Retriever {
	if config.PPRAlpha <= 0 || config.PPRAlpha >= 1 {
		config.PPRAlpha = DefaultPPRAlpha
	}
	pprConfig := DefaultPageRankConfig()
	pprConfig.Alpha = config.PPRAlpha
	re, err := regexp.Compile("(?i)" + RelationIntentPattern)
	if err != nil {
		re = regexp.MustCompile(`(?i)关系`)
	}
	return &Retriever{
		Config:      config,
		Source:      source,
		Vectors:     vectors,
		Index:       index,
		Embed:       embed,
		ppr:         NewPersonalizedPageRank(pprConfig),
		relationRe:  re,
		graphRecall: NewGraphRecallService(source, config.GraphRecall),
	}
}

// Retrieve 执行一次检索（对齐 DualPathRetriever.retrieve 的分派语义）。
// 出处：core/retrieval/dual_path.py L415-L478。
func (r *Retriever) Retrieve(
	ctx context.Context,
	query string,
	topK int,
	strategy Strategy,
	temporal *TemporalQueryOptions,
	enablePPR *bool,
	scope *Scope,
) ([]*Result, error) {
	if topK <= 0 {
		topK = r.Config.TopKFinal
	}
	if scope != nil && scope.Empty() {
		return nil, nil
	}
	if strategy == "" {
		strategy = r.Config.Strategy
	}
	usePPR := r.Config.EnablePPR
	if enablePPR != nil {
		usePPR = *enablePPR
	}
	intent := r.buildRelationIntentContext(query, topK)

	// 无语义 query 的纯时序查询直接走时序索引。
	if temporal != nil && strings.TrimSpace(query) == "" {
		return r.retrieveTemporalOnly(ctx, *temporal, topK, scope)
	}

	switch strategy {
	case StrategyParagraphOnly:
		return r.retrieveParagraphsOnly(ctx, query, topK, temporal, scope)
	case StrategyRelationOnly:
		return r.retrieveRelationsOnly(ctx, query, topK, temporal, scope)
	default:
		return r.retrieveDualPath(ctx, query, topK, temporal, intent, usePPR, scope)
	}
}

// IsRelationIntentQuery 判定查询是否为关系意图。
// 出处：core/retrieval/dual_path.py L479-L486、L396-L399。
func (r *Retriever) IsRelationIntentQuery(query string) bool {
	q := strings.TrimSpace(query)
	if q == "" {
		return false
	}
	if strings.Contains(q, "|") || strings.Contains(q, "->") {
		return true
	}
	return r.relationRe.MatchString(q)
}

// buildRelationIntentContext 构造关系意图上下文。
// 出处：core/retrieval/dual_path.py L487-L505。
func (r *Retriever) buildRelationIntentContext(query string, topK int) RelationIntentContext {
	cfg := r.Config.RelationIntent
	enabled := cfg.Enabled && r.IsRelationIntentQuery(query)
	base := r.Config.TopKRelations
	if base < 1 {
		base = DefaultTopKRelations
	}
	relationTopK := maxInt(base, topK)
	ctx := RelationIntentContext{RelationTopK: relationTopK}
	if !enabled {
		return ctx
	}
	relationTopK = maxInt(relationTopK, relationTopK*maxInt(1, cfg.RelationCandidateMultiplier))
	alpha := cfg.AlphaOverride
	ctx.Enabled = true
	ctx.AlphaOverride = &alpha
	ctx.RelationTopK = relationTopK
	ctx.PreserveTopRelations = cfg.PreserveTopRelations
	ctx.ForceRelationSparse = cfg.ForceRelationSparse
	ctx.PairPredicateRerank = cfg.PairPredicateRerankEnabled
	ctx.PairPredicateLimit = cfg.PairPredicateLimit
	return ctx
}

// retrieveParagraphsOnly 段落单路检索：向量 TopK + BM25 的 weighted RRF 融合。
// 出处：core/retrieval/dual_path.py L1081-L1143。
func (r *Retriever) retrieveParagraphsOnly(
	ctx context.Context,
	query string,
	topK int,
	temporal *TemporalQueryOptions,
	scope *Scope,
) ([]*Result, error) {
	var vectorResults []*Result
	var vectorIDs []string
	var queryEmbedding []float32
	embeddingOK := false
	if r.Embed != nil && r.Vectors != nil {
		vec, err := r.Embed.Encode(ctx, query)
		if err == nil {
			queryEmbedding = vec
			embeddingOK = r.isEmbeddingReady(vec)
		}
	}
	if embeddingOK {
		candidateK := r.expandTemporalCandidateK(topK*2, temporal)
		hits, err := r.Vectors.SearchParagraphs(queryEmbedding, candidateK, paragraphAllowed(scope))
		if err != nil {
			return nil, err
		}
		paragraphMap, err := r.Source.ParagraphsByHashes(ctx, hitIDs(hits))
		if err != nil {
			return nil, err
		}
		for _, hit := range hits {
			paragraph, ok := paragraphMap[hit.ID]
			if !ok || paragraph == nil {
				continue
			}
			if temporal != nil && !r.isTemporalMatch(paragraph, *temporal) {
				continue
			}
			vectorResults = append(vectorResults, &Result{
				Hash:    paragraph.Hash,
				Content: paragraph.Content,
				Score:   hit.Score,
				Type:    "paragraph",
				Source:  "paragraph_search",
				Metadata: map[string]any{
					"word_count": paragraph.WordCount,
					"time_meta":  buildTimeMeta(paragraph, temporal),
				},
			})
			vectorIDs = append(vectorIDs, paragraph.Hash)
		}
	}

	var sparseIDs []string
	sparseByHash := map[string]SparseRow{}
	vectorScores := make([]float64, 0, len(vectorResults))
	for _, item := range vectorResults {
		vectorScores = append(vectorScores, item.Score)
	}
	if r.Index != nil && r.Index.ShouldUseSparse(embeddingOK, vectorScores) {
		rows, err := r.Index.Search(ctx, query, capTemporalScan(topK, temporal), scopeList(scope, scopeKeysParagraph))
		if err != nil {
			return nil, err
		}
		rows = TrimSparseTail(rows)
		idList := make([]string, 0, len(rows))
		for _, row := range rows {
			idList = append(idList, row.Hash)
		}
		paragraphMap, err := r.Source.ParagraphsByHashes(ctx, idList)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			paragraph, ok := paragraphMap[row.Hash]
			if !ok || paragraph == nil {
				continue
			}
			if temporal != nil && !r.isTemporalMatch(paragraph, *temporal) {
				continue
			}
			sparseIDs = append(sparseIDs, row.Hash)
			sparseByHash[row.Hash] = row
		}
	}

	switch {
	case len(vectorResults) > 0 && len(sparseIDs) > 0 && r.Config.Fusion.Method == "weighted_rrf":
		scoreMap, ordered := FuseRankedListsWeightedRRF(
			vectorIDs, sparseIDs, r.Config.Fusion.RRFK,
			r.Config.Fusion.VectorWeight, r.Config.Fusion.BM25Weight,
			r.Config.Fusion.NormalizeScore && r.Config.Fusion.NormalizeMethod == "minmax",
		)
		byHash := map[string]*Result{}
		for _, item := range vectorResults {
			byHash[item.Hash] = item
		}
		out := make([]*Result, 0, len(ordered))
		for _, hash := range ordered {
			item, ok := byHash[hash]
			if !ok {
				if row, ok := sparseByHash[hash]; ok {
					item = &Result{
						Hash:     row.Hash,
						Content:  row.Content,
						Type:     "paragraph",
						Metadata: map[string]any{"bm25_score": row.BM25Score},
					}
				}
			}
			if item == nil {
				continue
			}
			item.Score = scoreMap[hash]
			item.Source = "fusion_rrf"
			out = append(out, item)
		}
		out = sortResultsWithTemporalIf(out, temporal)
		return truncateResults(out, topK), nil
	case len(vectorResults) > 0 && len(sparseIDs) > 0:
		out := append([]*Result{}, vectorResults...)
		for _, hash := range sparseIDs {
			row := sparseByHash[hash]
			exists := false
			for _, item := range out {
				if item.Hash == hash {
					exists = true
					break
				}
			}
			if exists {
				continue
			}
			out = append(out, &Result{
				Hash:     row.Hash,
				Content:  row.Content,
				Score:    row.Score,
				Type:     "paragraph",
				Source:   "sparse_bm25",
				Metadata: map[string]any{"bm25_score": row.BM25Score},
			})
		}
		sortResults(out)
		out = sortResultsWithTemporalIf(out, temporal)
		return truncateResults(out, topK), nil
	case len(vectorResults) > 0:
		out := sortResultsWithTemporalIf(vectorResults, temporal)
		return truncateResults(out, topK), nil
	default:
		out := make([]*Result, 0, len(sparseIDs))
		for _, hash := range sparseIDs {
			row := sparseByHash[hash]
			out = append(out, &Result{
				Hash:     row.Hash,
				Content:  row.Content,
				Score:    row.Score,
				Type:     "paragraph",
				Source:   "sparse_bm25",
				Metadata: map[string]any{"bm25_score": row.BM25Score},
			})
		}
		out = sortResultsWithTemporalIf(out, temporal)
		return truncateResults(out, topK), nil
	}
}

func sortResultsWithTemporalIf(results []*Result, temporal *TemporalQueryOptions) []*Result {
	if temporal == nil {
		sortResults(results)
		return results
	}
	return sortResultsWithTemporal(results)
}

// ---------------------------------------------------------------------------
// 主链路：双向量池检索（对齐 _retrieve_dual_vector_pools）
// 出处：core/retrieval/dual_path.py L2131-L2338。
// ---------------------------------------------------------------------------

func (r *Retriever) retrieveDualPath(
	ctx context.Context,
	query string,
	topK int,
	temporal *TemporalQueryOptions,
	intent RelationIntentContext,
	enablePPR bool,
	scope *Scope,
) ([]*Result, error) {
	semanticWeight, sparseWeight, graphWeight, graphTopK := r.dualPoolWeights(intent)

	// 1) 查询嵌入。
	var queryEmbedding []float32
	embeddingOK := false
	if r.Embed != nil {
		vec, err := r.Embed.Encode(ctx, query)
		if err == nil {
			queryEmbedding = vec
			embeddingOK = r.isEmbeddingReady(vec)
		}
	}

	candidates := map[string]*Result{}
	paragraphTopK := r.expandTemporalCandidateK(maxInt(topK, r.Config.VectorPools.ParagraphTopK), temporal)

	if embeddingOK && r.Vectors != nil {
		// 段落向量池。
		paraHits, err := r.Vectors.SearchParagraphs(queryEmbedding, paragraphTopK, paragraphAllowed(scope))
		if err != nil {
			return nil, err
		}
		paragraphMap, err := r.Source.ParagraphsByHashes(ctx, hitIDs(paraHits))
		if err != nil {
			return nil, err
		}
		for _, hit := range paraHits {
			paragraph, ok := paragraphMap[hit.ID]
			if !ok || paragraph == nil {
				continue
			}
			if temporal != nil && !r.isTemporalMatch(paragraph, *temporal) {
				continue
			}
			candidate := ensureParagraphCandidate(candidates, paragraph, temporal)
			AddCandidateScore(candidate, "semantic", hit.Score, "paragraph_vector_pool")
		}

		// 图证据池（关系/实体向量 → 支撑段落，带 grounding 软衰减）。
		graphEvidence, err := r.collectDualGraphEvidence(
			ctx, queryEmbedding, r.expandTemporalCandidateK(graphTopK, temporal), temporal, scope)
		if err != nil {
			return nil, err
		}
		hashes := make([]string, 0, len(graphEvidence))
		for hash := range graphEvidence {
			hashes = append(hashes, hash)
		}
		sort.Strings(hashes) // 确定性顺序
		for _, hash := range hashes {
			payload := graphEvidence[hash]
			paragraph, _ := payload["paragraph"].(*Paragraph)
			if paragraph == nil {
				continue
			}
			evidenceList, _ := payload["evidence"].([]evidencePair)
			candidate := ensureParagraphCandidate(candidates, paragraph, temporal)
			AppendCandidateSource(candidate, "graph_vector_pool")
			for _, pair := range evidenceList {
				AppendGraphEvidence(candidate, pair.Evidence, pair.Score)
			}
			scoreBreakdown(candidate)["graph_evidence"] = AggregateGraphEvidenceScore(evidenceItems(candidate))
		}
	}

	// 2) 稀疏段落召回（auto 模式下按向量最高分决定是否启用）。
	vectorScores := make([]float64, 0, len(candidates))
	for _, candidate := range candidates {
		meta := scoreBreakdown(candidate)
		vectorScores = append(vectorScores, toFloat(meta["semantic"]))
	}
	if r.Index != nil && r.Index.ShouldUseSparse(embeddingOK, vectorScores) {
		searched := maxInt(topK*2, r.Config.Sparse.CandidateK)
		searched = capTemporalScan(searched, temporal)
		rows, err := r.Index.Search(ctx, query, searched, scopeList(scope, scopeKeysParagraph))
		if err != nil {
			return nil, err
		}
		rows = TrimSparseTail(rows)
		if r.Config.Fusion.NormalizeScore && r.Config.Fusion.NormalizeMethod == "minmax" {
			rows = normalizeSparseRows(rows)
		}
		idList := make([]string, 0, len(rows))
		for _, row := range rows {
			idList = append(idList, row.Hash)
		}
		paragraphMap, err := r.Source.ParagraphsByHashes(ctx, idList)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			paragraph, ok := paragraphMap[row.Hash]
			if !ok || paragraph == nil {
				continue
			}
			if temporal != nil && !r.isTemporalMatch(paragraph, *temporal) {
				continue
			}
			candidate := ensureParagraphCandidate(candidates, paragraph, temporal)
			AddCandidateScore(candidate, "sparse", row.Score, "paragraph_sparse")
			scoreBreakdown(candidate)["bm25_score"] = row.BM25Score
		}
	}

	// 3) 图可靠性缩放 + 三通道融合。
	results := make([]*Result, 0, len(candidates))
	for _, hash := range sortedKeys(candidates) {
		results = append(results, candidates[hash])
	}
	configuredSemantic, configuredSparse, configuredGraph := semanticWeight, sparseWeight, graphWeight
	semanticWeight, sparseWeight, graphWeight, reliability := CalibrateDualPoolWeights(
		results, semanticWeight, sparseWeight, graphWeight, maxInt(10, topK*2))

	scoreMaps := map[string]map[string]float64{"semantic": {}, "sparse": {}, "graph": {}}
	for _, item := range results {
		meta := scoreBreakdown(item)
		if _, ok := meta["semantic"]; ok {
			scoreMaps["semantic"][item.Hash] = toFloat(meta["semantic"])
		}
		if _, ok := meta["sparse"]; ok {
			scoreMaps["sparse"][item.Hash] = toFloat(meta["sparse"])
		}
		graphScore := toFloat(meta["graph_evidence"])
		if graphScore <= 0 {
			graphScore = AggregateGraphEvidenceScore(evidenceItems(item))
		}
		if graphScore > 0 {
			scoreMaps["graph"][item.Hash] = graphScore
		}
	}
	finalScores, calibratedMaps, err := FuseScoreMaps(
		scoreMaps,
		map[string]float64{"semantic": semanticWeight, "sparse": sparseWeight, "graph": graphWeight},
		r.Config.VectorPools.ScoreCalibrationMethod,
		r.Config.VectorPools.ScoreCalibrationRRFK,
	)
	if err != nil {
		return nil, err
	}
	for _, item := range results {
		meta := scoreBreakdown(item)
		semanticScore := toFloat(meta["semantic"])
		sparseScore := toFloat(meta["sparse"])
		graphScore := toFloat(meta["graph_evidence"])
		if graphScore <= 0 {
			graphScore = AggregateGraphEvidenceScore(evidenceItems(item))
		}
		meta["semantic"] = semanticScore
		meta["sparse"] = sparseScore
		meta["graph_evidence"] = graphScore
		meta["calibrated_semantic"] = calibratedMaps["semantic"][item.Hash]
		meta["calibrated_sparse"] = calibratedMaps["sparse"][item.Hash]
		meta["calibrated_graph"] = calibratedMaps["graph"][item.Hash]
		meta["score_calibration_method"] = r.Config.VectorPools.ScoreCalibrationMethod
		meta["semantic_weight"] = semanticWeight
		meta["sparse_weight"] = sparseWeight
		meta["graph_weight"] = graphWeight
		meta["configured_semantic_weight"] = configuredSemantic
		meta["configured_sparse_weight"] = configuredSparse
		meta["configured_graph_weight"] = configuredGraph
		meta["graph_reliability"] = reliability.Score
		meta["graph_grounding_quality"] = reliability.GroundingQuality
		meta["graph_channel_agreement"] = reliability.ChannelAgreement
		meta["graph_support_coverage"] = reliability.SupportCoverage
		meta["graph_evidence_count"] = reliability.EvidenceCount
		meta["graph_grounded_relation_count"] = reliability.GroundedRelationCount
		meta["graph_relation_count"] = reliability.RelationCount
		meta["final"] = finalScores[item.Hash]
		item.Score = finalScores[item.Hash]
		item.Source = "dual_vector_pool"
	}
	sortResults(results)

	// 4) PPR 重排（段落实体 PPR 增益）。
	if enablePPR {
		results, err = r.rerankWithPPR(ctx, results, query)
		if err != nil {
			return nil, err
		}
	}

	// 5) 时序排序。
	if temporal != nil {
		results = sortResultsWithTemporal(results)
	}

	// 6) 后验图门。
	results, err = r.ApplyPosteriorGraphGate(ctx, query, results, topK, temporal, intent, scope)
	if err != nil {
		return nil, err
	}
	if len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}

// dualPoolWeights 按关系意图选择三通道权重与图池 top_k。
// 出处：core/retrieval/dual_path.py L1576-L1590。
func (r *Retriever) dualPoolWeights(intent RelationIntentContext) (float64, float64, float64, int) {
	cfg := r.Config.VectorPools
	if intent.Enabled {
		return cfg.RelationIntentSemanticWt, cfg.RelationIntentSparseWt, cfg.RelationIntentGraphWt, cfg.RelationIntentGraphTopK
	}
	return cfg.SemanticWeight, cfg.SparseWeight, cfg.GraphWeight, cfg.GraphTopK
}

type evidencePair struct {
	Evidence map[string]any
	Score    float64
}

// collectDualGraphEvidence 从图向量池收集证据并展开到支撑段落。
// 出处：core/retrieval/dual_path.py L1917-L2035。
func (r *Retriever) collectDualGraphEvidence(
	ctx context.Context,
	queryEmbedding []float32,
	topK int,
	temporal *TemporalQueryOptions,
	scope *Scope,
) (map[string]map[string]any, error) {
	cfg := r.Config.VectorPools
	allowed := graphAllowed(scope)
	hits, err := r.Vectors.SearchGraph(queryEmbedding, topK, allowed)
	if err != nil {
		return nil, err
	}

	type parsedItem struct {
		Type  string
		Hash  string
		Score float64
	}
	var parsed []parsedItem
	var relationHashes, entityHashes []string
	for _, hit := range hits {
		itemType, hash := parseGraphVectorID(hit.ID)
		if itemType == "" || hash == "" {
			continue
		}
		parsed = append(parsed, parsedItem{Type: itemType, Hash: hash, Score: hit.Score})
		if itemType == "relation" {
			relationHashes = append(relationHashes, hash)
		} else {
			entityHashes = append(entityHashes, hash)
		}
	}
	normalizeInput := make([]struct {
		Type  string
		Hash  string
		Score float64
	}, 0, len(parsed))
	for _, item := range parsed {
		normalizeInput = append(normalizeInput, struct {
			Type  string
			Hash  string
			Score float64
		}{Type: item.Type, Hash: item.Hash, Score: item.Score})
	}
	normalizedItems := NormalizeGraphScoresByType(normalizeInput)

	relations, err := r.Source.RelationsByHashes(ctx, relationHashes)
	if err != nil {
		return nil, err
	}
	entities, err := r.Source.EntitiesByHashes(ctx, entityHashes)
	if err != nil {
		return nil, err
	}
	paragraphsByRelation := map[string][]*Paragraph{}
	for _, hash := range dedupeStrings(relationHashes) {
		paragraphs, err := r.Source.ParagraphsByRelation(ctx, hash)
		if err != nil {
			return nil, err
		}
		paragraphsByRelation[hash] = paragraphs
	}
	paragraphsByEntity := map[string][]*Paragraph{}
	for _, hash := range dedupeStrings(entityHashes) {
		paragraphs, err := r.Source.ParagraphsByEntity(ctx, hash)
		if err != nil {
			return nil, err
		}
		paragraphsByEntity[hash] = paragraphs
	}

	type expanded struct {
		score    float64
		order    int
		para     *Paragraph
		evidence map[string]any
	}
	var entries []expanded
	for itemIndex, item := range normalizedItems {
		var (
			evidenceScore float64
			paragraphs    []*Paragraph
			evidence      map[string]any
		)
		if item.Type == "relation" {
			relation := relations[item.Hash]
			if relation == nil {
				continue
			}
			evidenceScore = item.NormalizedScore * cfg.RelationEvidenceWeight
			paragraphs = limitParagraphs(paragraphsByRelation[item.Hash], cfg.RelationExpandPerHit)
			evidence = map[string]any{
				"type":             "relation",
				"hash":             item.Hash,
				"subject":          relation.Subject,
				"predicate":        relation.Predicate,
				"object":           relation.Object,
				"source":           "graph_vector_relation",
				"raw_score":        item.RawScore,
				"normalized_score": item.NormalizedScore,
			}
		} else {
			entity := entities[item.Hash]
			if entity == nil {
				continue
			}
			evidenceScore = item.NormalizedScore * cfg.EntityEvidenceWeight
			paragraphs = limitParagraphs(paragraphsByEntity[item.Hash], cfg.EntityExpandPerHit)
			evidence = map[string]any{
				"type":             "entity",
				"hash":             item.Hash,
				"name":             entity.Name,
				"source":           "graph_vector_entity",
				"raw_score":        item.RawScore,
				"normalized_score": item.NormalizedScore,
			}
		}
		for paragraphIndex, paragraph := range paragraphs {
			if paragraph == nil || strings.TrimSpace(paragraph.Hash) == "" {
				continue
			}
			if !scope.hasParagraph(paragraph.Hash) {
				continue
			}
			if temporal != nil && !r.isTemporalMatch(paragraph, *temporal) {
				continue
			}
			grounding := GraphEvidenceGroundingFactor(evidence, paragraph.Content)
			grounded := make(map[string]any, len(evidence)+1)
			for k, v := range evidence {
				grounded[k] = v
			}
			grounded["grounding_factor"] = grounding
			order := itemIndex*maxInt(1, maxInt(cfg.RelationExpandPerHit, cfg.EntityExpandPerHit)) + paragraphIndex
			entries = append(entries, expanded{
				score:    evidenceScore * grounding,
				order:    order,
				para:     paragraph,
				evidence: grounded,
			})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score > entries[j].score
		}
		return entries[i].order < entries[j].order
	})
	if cfg.GraphExpandParagraphK > 0 && len(entries) > cfg.GraphExpandParagraphK {
		entries = entries[:cfg.GraphExpandParagraphK]
	}

	out := map[string]map[string]any{}
	order := make([]string, 0, len(entries))
	for _, entry := range entries {
		payload, ok := out[entry.para.Hash]
		if !ok {
			payload = map[string]any{"paragraph": entry.para, "evidence": []evidencePair{}}
			out[entry.para.Hash] = payload
			order = append(order, entry.para.Hash)
		}
		pairs, _ := payload["evidence"].([]evidencePair)
		payload["evidence"] = append(pairs, evidencePair{Evidence: entry.evidence, Score: entry.score})
	}
	return out, nil
}

// retrieveRelationsOnly 关系单路检索（Entity-Pivot 策略）。
// 出处：core/retrieval/dual_path.py L1146-L1254。
func (r *Retriever) retrieveRelationsOnly(
	ctx context.Context,
	query string,
	topK int,
	temporal *TemporalQueryOptions,
	scope *Scope,
) ([]*Result, error) {
	var vectorResults []*Result
	var queryEmbedding []float32
	embeddingOK := false
	if r.Embed != nil && r.Vectors != nil {
		vec, err := r.Embed.Encode(ctx, query)
		if err == nil {
			queryEmbedding = vec
			embeddingOK = r.isEmbeddingReady(vec)
		}
	}
	if embeddingOK {
		// 1) 检索实体向量（扩大范围以召回足够实体）。
		candidateK := r.expandTemporalCandidateK(topK*3, temporal)
		hits, err := r.Vectors.SearchVectors("entity", queryEmbedding, candidateK, entityAllowed(scope))
		if err != nil {
			return nil, err
		}
		entityMap, err := r.Source.EntitiesByHashes(ctx, hitIDs(hits))
		if err != nil {
			return nil, err
		}
		seenRelations := map[string]struct{}{}
		relationScores := map[string]float64{}
		order := make([]string, 0)
		for _, hit := range hits {
			entity := entityMap[hit.ID]
			if entity == nil {
				continue
			}
			// 2) 以实体为枢纽扩展关系。
			related, err := r.Source.RelationsForEntity(ctx, entity.Name)
			if err != nil {
				return nil, err
			}
			for _, relation := range related {
				if !scope.hasRelation(relation.Hash) {
					continue
				}
				if _, ok := seenRelations[relation.Hash]; ok {
					continue
				}
				seenRelations[relation.Hash] = struct{}{}
				relationScores[relation.Hash] = hit.Score
				order = append(order, relation.Hash)
			}
		}
		hashes := make([]string, 0, len(order))
		scores := make([]float64, 0, len(order))
		for _, hash := range order {
			hashes = append(hashes, hash)
			scores = append(scores, relationScores[hash])
		}
		relations, err := r.Source.RelationsByHashes(ctx, hashes)
		if err != nil {
			return nil, err
		}
		for i, hash := range hashes {
			relation := relations[hash]
			if relation == nil {
				continue
			}
			vectorResults = append(vectorResults, &Result{
				Hash:    hash,
				Content: relation.Content(),
				Score:   scores[i],
				Type:    "relation",
				Source:  "relation_search (via entity)",
				Metadata: map[string]any{
					"subject":   relation.Subject,
					"predicate": relation.Predicate,
					"object":    relation.Object,
				},
			})
		}
	}
	if temporal != nil {
		filtered, err := r.applyTemporalFilterToRelations(ctx, vectorResults, temporal)
		if err != nil {
			return nil, err
		}
		vectorResults = filtered
	}

	// 3) 稀疏关系召回（fallback / hybrid）。
	var sparseResults []*Result
	vectorScores := make([]float64, 0, len(vectorResults))
	for _, item := range vectorResults {
		vectorScores = append(vectorScores, item.Score)
	}
	if r.Index != nil && r.Index.ShouldUseSparse(embeddingOK, vectorScores) {
		rows, err := r.Index.SearchRelations(ctx, query, capTemporalScan(topK, temporal), scopeList(scope, scopeKeysRelation))
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			sparseResults = append(sparseResults, &Result{
				Hash:    row.Hash,
				Content: row.Content,
				Score:   row.Score,
				Type:    "relation",
				Source:  "sparse_relation_bm25",
				Metadata: map[string]any{
					"subject":    row.Subject,
					"predicate":  row.Predicate,
					"object":     row.Object,
					"bm25_score": row.BM25Score,
				},
			})
		}
	}

	// 4) 图结构召回。
	graphResults, err := r.searchRelationsGraph(ctx, query, temporal, scope)
	if err != nil {
		return nil, err
	}

	var results []*Result
	switch {
	case len(graphResults) > 0:
		results = r.MergeRelationResultsGraphEnhanced(ctx, vectorResults, sparseResults, graphResults)
	case len(vectorResults) > 0 && len(sparseResults) > 0:
		results = mergeRelationResults(vectorResults, sparseResults)
	case len(vectorResults) > 0:
		results = vectorResults
	default:
		results = sparseResults
	}
	return truncateResults(results, topK), nil
}

// searchRelationsGraph 由查询实体种子做受控图召回。
// 出处：core/retrieval/dual_path.py L674-L717。
func (r *Retriever) searchRelationsGraph(
	ctx context.Context, query string, temporal *TemporalQueryOptions, scope *Scope,
) ([]*Result, error) {
	if r.graphRecall == nil || !r.Config.GraphRecall.Enabled {
		return nil, nil
	}
	seeds, err := r.ExtractGraphSeedEntities(ctx, query, 2)
	if err != nil {
		return nil, err
	}
	if len(seeds) == 0 {
		return nil, nil
	}
	var allowed map[string]struct{}
	if scope != nil && len(scope.RelationIDs) > 0 {
		allowed = scope.RelationIDs
	}
	payloads, err := r.graphRecall.Recall(ctx, seeds, allowed)
	if err != nil {
		return nil, err
	}
	results := make([]*Result, 0, len(payloads))
	for _, payload := range payloads {
		results = append(results, &Result{
			Hash:    payload.Hash,
			Content: strings.TrimSpace(payload.Subject + " " + payload.Predicate + " " + payload.Object),
			Score:   0,
			Type:    "relation",
			Source:  "graph_relation_recall",
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
	return r.applyTemporalFilterToRelations(ctx, results, temporal)
}

// mergeRelationResults 按 hash 去重并保留更高分，来源标记 relation_fusion。
// 出处：core/retrieval/dual_path.py L986-L1003。
func mergeRelationResults(vectorResults, sparseResults []*Result) []*Result {
	merged := map[string]*Result{}
	order := make([]string, 0)
	for _, item := range vectorResults {
		merged[item.Hash] = item
		order = append(order, item.Hash)
	}
	for _, item := range sparseResults {
		old, ok := merged[item.Hash]
		if !ok {
			merged[item.Hash] = item
			order = append(order, item.Hash)
			continue
		}
		if item.Score > old.Score {
			merged[item.Hash] = item
		} else if old.Source != item.Source {
			old.Source = "relation_fusion"
		}
	}
	out := make([]*Result, 0, len(order))
	for _, hash := range order {
		out = append(out, merged[hash])
	}
	sortResults(out)
	return out
}

// ---------------------------------------------------------------------------
// 经典双路（段落路 + 关系路）融合，供 paragraph_only / relation_only 与
// 外部显式调用，语义对齐 _fuse_results。
// 出处：core/retrieval/dual_path.py L2510-L2590。
// ---------------------------------------------------------------------------

// FuseResults 融合段落与关系结果。
func (r *Retriever) FuseResults(
	ctx context.Context,
	paraResults, relResults []*Result,
	alphaOverride *float64,
	preserveTopRelations int,
) ([]*Result, error) {
	alpha := r.Config.Alpha
	if alphaOverride != nil {
		alpha = *alphaOverride
	}
	for _, item := range paraResults {
		item.Score *= alpha
		item.Source = "fusion"
	}
	for _, item := range relResults {
		item.Score *= (1 - alpha)
		item.Source = "fusion"
	}

	preserved := map[string]struct{}{}
	if preserveTopRelations > 0 && len(relResults) > 0 {
		ranked := make([]*Result, len(relResults))
		copy(ranked, relResults)
		sortResults(ranked)
		limit := minInt(preserveTopRelations, len(ranked))
		for _, item := range ranked[:limit] {
			preserved[item.Hash] = struct{}{}
		}
	}

	all := append(append([]*Result{}, paraResults...), relResults...)
	sortResults(all)

	// 关系 → 支撑段落映射，用于「段落已入选则关系去重」。
	support := map[string][]string{}
	var relationHashes []string
	for _, item := range all {
		if item.Type == "relation" {
			relationHashes = append(relationHashes, item.Hash)
		}
	}
	for _, hash := range dedupeStrings(relationHashes) {
		paragraphs, err := r.Source.ParagraphsByRelation(ctx, hash)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(paragraphs))
		for _, p := range paragraphs {
			ids = append(ids, p.Hash)
		}
		support[hash] = ids
	}

	seenParagraphs := map[string]struct{}{}
	seenItems := map[string]struct{}{}
	out := make([]*Result, 0, len(all))
	for _, item := range all {
		if _, ok := seenItems[item.Hash]; ok {
			continue
		}
		if item.Type == "paragraph" {
			if _, ok := seenParagraphs[item.Hash]; !ok {
				seenParagraphs[item.Hash] = struct{}{}
				seenItems[item.Hash] = struct{}{}
				out = append(out, item)
			}
			continue
		}
		if _, ok := preserved[item.Hash]; ok {
			seenItems[item.Hash] = struct{}{}
			out = append(out, item)
			continue
		}
		if hashes := support[item.Hash]; len(hashes) > 0 {
			skip := false
			for _, h := range hashes {
				if _, ok := seenParagraphs[h]; ok {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
		}
		seenItems[item.Hash] = struct{}{}
		out = append(out, item)
	}
	sortResults(out)
	return out, nil
}

// RerankWithPPR 用实体 PPR 分数给段落结果加增益。
// 出处：core/retrieval/dual_path.py L2677-L2740。
func (r *Retriever) RerankWithPPR(ctx context.Context, results []*Result, query string) ([]*Result, error) {
	return r.rerankWithPPR(ctx, results, query)
}

func (r *Retriever) rerankWithPPR(ctx context.Context, results []*Result, query string) ([]*Result, error) {
	if r.Source == nil || len(results) == 0 {
		return results, nil
	}
	entities, err := r.ExtractEntities(ctx, query)
	if err != nil {
		return results, nil
	}
	// 图节点名是小写的规范化名，AC 命中结果即为节点名。
	if len(entities) == 0 {
		return results, nil
	}
	graph, err := r.Source.Graph(ctx)
	if err != nil {
		return results, nil
	}
	var pprScores map[string]float64
	if r.Config.PPRLocalEnabled && graph.NumNodes() >= r.Config.PPRLocalMinNodes {
		pprScores = r.ppr.ComputeLocal(graph, entities, r.Config.PPRLocalMaxNodes, r.Config.PPRLocalHops)
	}
	if len(pprScores) == 0 {
		pprScores = r.ppr.Compute(graph, entities)
	}
	if len(pprScores) == 0 {
		return results, nil
	}
	byName := make(map[string]float64, len(pprScores))
	for name, score := range pprScores {
		byName[strings.ToLower(strings.TrimSpace(name))] = score
	}

	// 段落 → 实体名映射。
	paragraphHashes := make([]string, 0, len(results))
	for _, item := range results {
		if item.Type == "paragraph" {
			paragraphHashes = append(paragraphHashes, item.Hash)
		}
	}
	entitiesByParagraph, err := r.paragraphEntityNames(ctx, paragraphHashes)
	if err != nil {
		return results, nil
	}
	for _, item := range results {
		if item.Type != "paragraph" {
			continue
		}
		var scores []float64
		for _, name := range entitiesByParagraph[item.Hash] {
			if score, ok := byName[strings.ToLower(strings.TrimSpace(name))]; ok {
				scores = append(scores, score)
			}
		}
		if len(scores) == 0 {
			continue
		}
		sort.Sort(sort.Reverse(sort.Float64Slice(scores)))
		focus := scores
		if len(focus) > PPRBoostFocusSize {
			focus = focus[:PPRBoostFocusSize]
		}
		signal := mean(focus)
		weight := PPRBoostWeightSingle
		if len(focus) >= 2 {
			weight = PPRBoostWeightMultiple
		}
		boost := signal * weight
		meta := scoreBreakdown(item)
		meta["ppr_signal"] = round4(signal)
		meta["ppr_focus_entity_count"] = len(focus)
		meta["ppr_boost"] = round4(boost)
		item.Score += boost
	}
	sortResults(results)
	return results, nil
}

// ---------------------------------------------------------------------------
// 段落候选构造与实体抽取
// ---------------------------------------------------------------------------

func ensureParagraphCandidate(candidates map[string]*Result, paragraph *Paragraph, temporal *TemporalQueryOptions) *Result {
	item, ok := candidates[paragraph.Hash]
	if ok {
		return item
	}
	metadata := map[string]any{
		"word_count":      paragraph.WordCount,
		"score_breakdown": map[string]any{},
		"evidence_items":  []map[string]any{},
	}
	if temporal != nil {
		metadata["time_meta"] = buildTimeMeta(paragraph, temporal)
	}
	item = &Result{
		Hash:     paragraph.Hash,
		Content:  paragraph.Content,
		Type:     "paragraph",
		Source:   "dual_vector_pool",
		Metadata: metadata,
	}
	candidates[paragraph.Hash] = item
	return item
}

// buildTimeMeta 构造统一 time_meta 结构。
// 出处：core/retrieval/dual_path.py L3002-L3024。
func buildTimeMeta(paragraph *Paragraph, temporal *TemporalQueryOptions) map[string]any {
	allowFallback := true
	if temporal != nil {
		allowFallback = temporal.AllowCreatedFallback
	}
	start, end, basis, ok := paragraph.EffectiveTime(allowFallback)
	meta := map[string]any{
		"allow_created_fallback": allowFallback,
	}
	if ok {
		meta["effective_start"] = start
		meta["effective_end"] = end
		meta["match_basis"] = basis
	} else {
		meta["effective_start"] = nil
		meta["effective_end"] = nil
		meta["match_basis"] = nil
	}
	return meta
}

// ExtractEntities 用 Aho-Corasick 从文本抽取实体（{实体名: 出现次数}）。
// 出处：core/retrieval/dual_path.py L3175-L3208。
func (r *Retriever) ExtractEntities(ctx context.Context, text string) (map[string]float64, error) {
	if r.Source == nil {
		return map[string]float64{}, nil
	}
	graph, err := r.Source.Graph(ctx)
	if err != nil {
		return nil, err
	}
	names := append([]string{}, graph.Nodes...)
	if len(names) == 0 {
		return map[string]float64{}, nil
	}
	r.acMu.Lock()
	if r.ac == nil || r.acRev != graph.Rev {
		ac := NewAhoCorasick()
		for _, name := range names {
			ac.AddPattern(strings.ToLower(name))
		}
		ac.Build()
		r.ac = ac
		r.acRev = graph.Rev
	}
	matcher := r.ac
	r.acMu.Unlock()
	return matcher.ExtractEntities(text), nil
}

// ExtractGraphSeedEntities 取排序后的前 limit 个种子实体。
// 出处：core/retrieval/dual_path.py L664-L673。
func (r *Retriever) ExtractGraphSeedEntities(ctx context.Context, query string, limit int) ([]string, error) {
	entities, err := r.ExtractEntities(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(entities) == 0 || limit <= 0 {
		return nil, nil
	}
	names := make([]string, 0, len(entities))
	for name := range entities {
		names = append(names, name)
	}
	sort.SliceStable(names, func(i, j int) bool {
		if entities[names[i]] != entities[names[j]] {
			return entities[names[i]] > entities[names[j]]
		}
		if len([]rune(names[i])) != len([]rune(names[j])) {
			return len([]rune(names[i])) > len([]rune(names[j]))
		}
		return names[i] < names[j]
	})
	if limit > len(names) {
		limit = len(names)
	}
	return names[:limit], nil
}

// ---------------------------------------------------------------------------
// 时序检索与过滤
// ---------------------------------------------------------------------------

// retrieveTemporalOnly 无语义 query 时直接走时序索引。
// 出处：core/retrieval/dual_path.py L2930-L2968。
func (r *Retriever) retrieveTemporalOnly(
	ctx context.Context, temporal TemporalQueryOptions, topK int, scope *Scope,
) ([]*Result, error) {
	limit := capTemporalScan(topK*maxInt(1, temporal.CandidateMultiplier), &temporal)
	scope_ := r.scopeKey(scope)
	paragraphs, err := temporalParagraphs(ctx, r.Source, temporal, scope_, limit)
	if err != nil {
		return nil, err
	}
	results := make([]*Result, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		results = append(results, &Result{
			Hash:    paragraph.Hash,
			Content: paragraph.Content,
			Score:   1.0,
			Type:    "paragraph",
			Source:  "temporal_scan",
			Metadata: map[string]any{
				"word_count": paragraph.WordCount,
				"time_meta":  buildTimeMeta(paragraph, &temporal),
			},
		})
	}
	results = sortResultsWithTemporal(results)
	if len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}

func (r *Retriever) scopeKey(scope *Scope) string {
	if scope == nil {
		return ""
	}
	return scope.Key
}

// isTemporalMatch 判断段落是否命中时序筛选。
// 出处：core/retrieval/dual_path.py L3039-L3061。
func (r *Retriever) isTemporalMatch(paragraph *Paragraph, temporal TemporalQueryOptions) bool {
	if temporal.Source != "" && paragraph.Source != temporal.Source {
		return false
	}
	if temporal.Person != "" && !r.matchesPersonFilter(paragraph, temporal.Person) {
		return false
	}
	start, end, _, ok := paragraph.EffectiveTime(temporal.AllowCreatedFallback)
	if !ok {
		return false
	}
	switch {
	case temporal.TimeFrom != nil && temporal.TimeTo != nil:
		return end >= *temporal.TimeFrom && start <= *temporal.TimeTo
	case temporal.TimeFrom != nil:
		return end >= *temporal.TimeFrom
	case temporal.TimeTo != nil:
		return start <= *temporal.TimeTo
	default:
		return true
	}
}

// matchesPersonFilter 判定段落是否属于指定人物（对齐 _matches_person_filter）。
// 出处：core/retrieval/dual_path.py L3026-L3037。
func (r *Retriever) matchesPersonFilter(paragraph *Paragraph, person string) bool {
	target := strings.ToLower(strings.TrimSpace(person))
	if target == "" {
		return true
	}
	if paragraph.Metadata != nil {
		for _, key := range []string{"person_ids", "participants"} {
			raw, ok := paragraph.Metadata[key]
			if !ok {
				continue
			}
			switch values := raw.(type) {
			case []string:
				for _, value := range values {
					if strings.ToLower(strings.TrimSpace(value)) == target {
						return true
					}
				}
			case []any:
				for _, value := range values {
					if strings.ToLower(strings.TrimSpace(stringify(value))) == target {
						return true
					}
				}
			}
		}
	}
	return false
}

// sortResultsWithTemporal 语义优先、时间次排序（新到旧）。
// 出处：core/retrieval/dual_path.py L3155-L3173。
func sortResultsWithTemporal(results []*Result) []*Result {
	sort.SliceStable(results, func(i, j int) bool {
		si, sj := results[i].Score, results[j].Score
		if si != sj {
			return si > sj
		}
		return temporalKey(results[i]) > temporalKey(results[j])
	})
	return results
}

func temporalKey(item *Result) float64 {
	meta, ok := item.Metadata["time_meta"].(map[string]any)
	if !ok {
		return -1e308
	}
	if v := toFloat(meta["effective_end"]); v != 0 {
		return v
	}
	return toFloat(meta["effective_start"])
}

// ---------------------------------------------------------------------------
// 工具函数
// ---------------------------------------------------------------------------

func (r *Retriever) isEmbeddingReady(vec []float32) bool {
	if len(vec) == 0 {
		return false
	}
	for _, v := range vec {
		if v != v { // NaN
			return false
		}
	}
	if r.Vectors == nil {
		return true
	}
	expected := r.Vectors.Dimension()
	if expected <= 0 {
		return false
	}
	if len(vec) != expected {
		return false
	}
	return true
}

func (r *Retriever) expandTemporalCandidateK(candidateK int, temporal *TemporalQueryOptions) int {
	multiplier := 1
	if temporal != nil && temporal.CandidateMultiplier > 1 {
		multiplier = temporal.CandidateMultiplier
	}
	return capTemporalScan(maxInt(1, candidateK)*multiplier, temporal)
}

// capTemporalScan 对时序模式候选召回数应用 max_scan 上限。
// 出处：core/retrieval/dual_path.py L507-L516。
func capTemporalScan(candidateK int, temporal *TemporalQueryOptions) int {
	k := maxInt(1, candidateK)
	if temporal != nil && temporal.MaxScan > 0 {
		k = minInt(k, temporal.MaxScan)
	}
	return maxInt(1, k)
}

func entityAllowed(scope *Scope) map[string]struct{} {
	if scope == nil || len(scope.EntityIDs) == 0 {
		return nil
	}
	return scope.EntityIDs
}

type scopeKind int

const (
	scopeKeysParagraph scopeKind = iota
	scopeKeysRelation
	scopeKeysEntity
)

func scopeList(scope *Scope, kind scopeKind) []string {
	if scope == nil {
		return nil
	}
	var source map[string]struct{}
	switch kind {
	case scopeKeysRelation:
		source = scope.RelationIDs
	case scopeKeysEntity:
		source = scope.EntityIDs
	default:
		source = scope.ParagraphIDs
	}
	if len(source) == 0 {
		return nil
	}
	out := make([]string, 0, len(source))
	for id := range source {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func paragraphAllowed(scope *Scope) map[string]struct{} {
	if scope == nil || len(scope.ParagraphIDs) == 0 {
		return nil
	}
	return scope.ParagraphIDs
}

func graphAllowed(scope *Scope) map[string]struct{} {
	if scope == nil || (len(scope.RelationIDs) == 0 && len(scope.EntityIDs) == 0) {
		return nil
	}
	out := make(map[string]struct{}, len(scope.RelationIDs)+len(scope.EntityIDs))
	for hash := range scope.RelationIDs {
		out["relation:"+hash] = struct{}{}
	}
	for hash := range scope.EntityIDs {
		out["entity:"+hash] = struct{}{}
	}
	return out
}

// GraphVectorID 构造图向量池中的资产 ID。
// 出处：core/retrieval/dual_path.py L1565-L1568。
func GraphVectorID(itemType, hash string) string { return itemType + ":" + hash }

// parseGraphVectorID 解析 "relation:<hash>" / "entity:<hash>"。
func parseGraphVectorID(value string) (string, string) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return "", ""
	}
	itemType := strings.TrimSpace(parts[0])
	hash := strings.TrimSpace(parts[1])
	if itemType != "relation" && itemType != "entity" {
		return "", ""
	}
	if hash == "" {
		return "", ""
	}
	return itemType, hash
}

func hitIDs(hits []VectorHit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.ID)
	}
	return out
}

func limitParagraphs(list []*Paragraph, limit int) []*Paragraph {
	if limit <= 0 || len(list) <= limit {
		return list
	}
	return list[:limit]
}

func dedupeStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func sortedKeys(m map[string]*Result) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortResults(results []*Result) {
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Hash < results[j].Hash
	})
}

func normalizeSparseRows(rows []SparseRow) []SparseRow {
	scores := make([]float64, len(rows))
	for i, row := range rows {
		scores[i] = row.Score
	}
	normed := NormalizeScoresMinMax(scores)
	for i := range rows {
		rows[i].Score = normed[i]
	}
	return rows
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total / float64(len(values))
}

func round4(value float64) float64 {
	return float64(int64(value*10000+sign(value)*0.5)) / 10000
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
