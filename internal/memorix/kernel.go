package memorix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/memorix/retrieval"
	"oh-my-qqbot/internal/memorix/storage"
)

// RetrievalOptions 是内核检索配置（默认值 = Python 原版常数，见 retrieval/constants.go）。
type RetrievalOptions struct {
	// Strategy 对齐 retrieval 策略枚举（paragraph_only|relation_only|dual_path）。
	Strategy string
	// TopKParagraphs / TopKRelations / TopKFinal 对齐 retrieval.top_k_*。
	TopKParagraphs int
	TopKRelations  int
	TopKFinal      int
	// Alpha 对齐 retrieval.alpha。
	Alpha float64
	// EnablePPR / PPRAlpha 对齐 retrieval.enable_ppr / ppr_alpha。
	EnablePPR bool
	PPRAlpha  float64
	// PPRLocalMaxNodes / PPRLocalHops / PPRLocalMinNodes 对齐 PPR 局部模式参数。
	PPRLocalMaxNodes int
	PPRLocalHops     int
	PPRLocalMinNodes int
	// SemanticWeight / SparseWeight / GraphWeight 对齐 vector_pools 三通道权重。
	SemanticWeight float64
	SparseWeight   float64
	GraphWeight    float64
	// ScoreCalibrationMethod 对齐 vector_pools.score_calibration_method。
	ScoreCalibrationMethod string
	// Sparse 对齐 retrieval.sparse 节。
	Sparse retrieval.SparseConfig
	// Fusion 对齐 retrieval.fusion 节。
	Fusion retrieval.FusionConfig
	// RelationIntent 对齐 relation_intent 配置。
	RelationIntent retrieval.RelationIntentConfig
	// GraphRecall 对齐 graph_recall 配置。
	GraphRecall retrieval.GraphRecallConfig
	// PosteriorGraph 对齐 posterior_graph 配置。
	PosteriorGraph retrieval.PosteriorConfig
	// Threshold 对齐 threshold 节（动态阈值过滤）。
	Threshold retrieval.ThresholdConfig
	// TemporalDefaultTopK 对齐 retrieval.temporal.default_top_k（默认 10）。
	TemporalDefaultTopK int
}

// DefaultTemporalTopK 对齐 retrieval.temporal.default_top_k 默认值。
// 出处：core/utils/search_execution_service.py L150-L152。
const DefaultTemporalTopK = 10

// DefaultRetrievalOptions 返回照抄 Python 常数的检索配置。
func DefaultRetrievalOptions() RetrievalOptions {
	pools := retrieval.DefaultVectorPoolsConfig()
	return RetrievalOptions{
		Strategy:               string(retrieval.StrategyDualPath),
		TopKParagraphs:         retrieval.DefaultTopKParagraphs,
		TopKRelations:          retrieval.DefaultTopKRelations,
		TopKFinal:              retrieval.DefaultTopKFinal,
		Alpha:                  retrieval.DefaultAlpha,
		EnablePPR:              true,
		PPRAlpha:               retrieval.DefaultPPRAlpha,
		PPRLocalMaxNodes:       retrieval.DefaultPPRLocalMaxNodes,
		PPRLocalHops:           retrieval.DefaultPPRLocalHops,
		PPRLocalMinNodes:       retrieval.DefaultPPRLocalMinNodes,
		SemanticWeight:         pools.SemanticWeight,
		SparseWeight:           pools.SparseWeight,
		GraphWeight:            pools.GraphWeight,
		ScoreCalibrationMethod: pools.ScoreCalibrationMethod,
		Sparse:                 retrieval.DefaultSparseConfig(),
		Fusion:                 retrieval.DefaultFusionConfig(),
		RelationIntent:         retrieval.DefaultRelationIntentConfig(),
		GraphRecall:            retrieval.DefaultGraphRecallConfig(),
		PosteriorGraph:         retrieval.DefaultPosteriorConfig(),
		Threshold:              retrieval.DefaultThresholdConfig(),
		TemporalDefaultTopK:    DefaultTemporalTopK,
	}
}

// Kernel 是 A_memorix 内核，实现 internal.MemoryKernel。
type Kernel struct {
	options Options
	store   *storage.Store
	embed   *EmbeddingClient
	chat    ChatCompleter
	index   *retrieval.SparseIndex
	search  *retrieval.Retriever

	writeMu sync.Mutex
	closed  bool
}

// 编译期断言：Kernel 必须满足 internal.MemoryKernel。
var _ contracts.MemoryKernel = (*Kernel)(nil)

// New 构造内核并初始化存储层（等价 SDKMemoryKernel 的 initialize）
// 与检索器（维度探测后再做一次向量索引对齐）。
//
// chat 可为 nil（抽取降级为规则模式）；embedder 可为 nil（允许 metadata-only 写入）。
func New(ctx context.Context, options Options, chat ChatCompleter, embedder Embedder) (*Kernel, error) {
	if options.DataDir == "" && options.DBPath == "" {
		options.DataDir = "data/a-memorix"
	}
	if options.Retrieval.TopKFinal <= 0 {
		options.Retrieval = DefaultRetrievalOptions()
	}
	store, err := storage.Open(ctx, storage.OpenOptions{
		Path:             options.DBPath,
		Dir:              options.DataDir,
		MaxReadConns:     options.MaxReadConns,
		BusyTimeoutMS:    int(options.BusyTimeout / time.Millisecond),
		QuantizationType: "float32",
		EnableFTS:        options.EnableFTS,
	})
	if err != nil {
		return nil, err
	}
	k := &Kernel{
		options: options,
		store:   store,
		embed:   NewEmbeddingClient(embedder, options.Embedding),
		chat:    chat,
	}
	// 向量索引维度以已落库向量为准（对齐 stored_vector_dimension 校验）；
	// 尚未落库时用配置维度预置，但不视为「已探测」。
	if dim := store.Dimension(); dim > 0 {
		k.embed.seedDimension(dim)
	} else if options.Embedding.Dimension > 0 {
		k.embed.seedDimension(options.Embedding.Dimension)
	}
	k.rebuildRetriever()
	return k, nil
}

// rebuildRetriever 依据当前配置与存储构建检索器。
func (k *Kernel) rebuildRetriever() {
	cfg := k.options.Retrieval
	sparseConfig := cfg.Sparse
	retrieverConfig := retrieval.RetrieverConfig{
		TopKParagraphs:   cfg.TopKParagraphs,
		TopKRelations:    cfg.TopKRelations,
		TopKFinal:        cfg.TopKFinal,
		Alpha:            cfg.Alpha,
		EnablePPR:        cfg.EnablePPR,
		PPRAlpha:         cfg.PPRAlpha,
		PPRLocalEnabled:  true,
		PPRLocalMaxNodes: cfg.PPRLocalMaxNodes,
		PPRLocalHops:     cfg.PPRLocalHops,
		PPRLocalMinNodes: cfg.PPRLocalMinNodes,
		Strategy:         retrieval.Strategy(cfg.Strategy),
		Sparse:           sparseConfig,
		Fusion:           cfg.Fusion,
		RelationIntent:   cfg.RelationIntent,
		VectorPools:      retrieval.DefaultVectorPoolsConfig(),
		GraphRecall:      cfg.GraphRecall,
		PosteriorGraph:   cfg.PosteriorGraph,
	}
	retrieverConfig.VectorPools.SemanticWeight = cfg.SemanticWeight
	retrieverConfig.VectorPools.SparseWeight = cfg.SparseWeight
	retrieverConfig.VectorPools.GraphWeight = cfg.GraphWeight
	retrieverConfig.VectorPools.ScoreCalibrationMethod = cfg.ScoreCalibrationMethod
	if retrieverConfig.Strategy == "" {
		retrieverConfig.Strategy = retrieval.StrategyDualPath
	}
	if retrieverConfig.PPRAlpha <= 0 {
		retrieverConfig.PPRAlpha = retrieval.DefaultPPRAlpha
	}

	index := retrieval.NewSparseIndex(k.store, sparseConfig)
	k.index = index
	k.search = retrieval.NewRetriever(k.store, k.storeVectorSource(), index, k.embed, retrieverConfig)
}

// storeVectorSource 把 storage.Store 适配为检索层需要的向量源。
func (k *Kernel) storeVectorSource() retrieval.VectorSource { return storeVectorSource{store: k.store} }

type storeVectorSource struct{ store *storage.Store }

func (s storeVectorSource) Dimension() int { return s.store.Dimension() }
func (s storeVectorSource) Count() int {
	index := s.store.VectorIndexRef()
	if index == nil {
		return 0
	}
	return index.Count()
}
func (s storeVectorSource) SearchParagraphs(
	query []float32, k int, allowed map[string]struct{},
) ([]retrieval.VectorHit, error) {
	return s.store.SearchVectors("paragraph", query, k, allowed), nil
}
func (s storeVectorSource) SearchVectors(
	itemType string, query []float32, k int, allowed map[string]struct{},
) ([]retrieval.VectorHit, error) {
	return s.store.SearchVectors(itemType, query, k, allowed), nil
}

func (s storeVectorSource) SearchGraph(
	query []float32, k int, allowed map[string]struct{},
) ([]retrieval.VectorHit, error) {
	relations := s.store.SearchVectors("relation", query, k, allowed)
	entities := s.store.SearchVectors("entity", query, k, allowed)
	out := make([]retrieval.VectorHit, 0, len(relations)+len(entities))
	for _, hit := range relations {
		out = append(out, retrieval.VectorHit{ID: retrieval.GraphVectorID("relation", hit.ID), Score: hit.Score})
	}
	for _, hit := range entities {
		out = append(out, retrieval.VectorHit{ID: retrieval.GraphVectorID("entity", hit.ID), Score: hit.Score})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// Store 暴露底层存储（供 admin 面与测试使用）。
func (k *Kernel) Store() *storage.Store { return k.store }

// Retriever 暴露双路检索器（供 admin/调试使用）。
func (k *Kernel) Retriever() *retrieval.Retriever { return k.search }

// EmbeddingClient 暴露嵌入客户端（维度探测/降级状态）。
func (k *Kernel) EmbeddingClient() *EmbeddingClient { return k.embed }

// ---------------------------------------------------------------------------
// 写入
// ---------------------------------------------------------------------------

// IngestSummary 写入聊天摘要段落（chat recall 路径）。
// 语义对齐 SDKMemoryKernel.ingest_summary（core/runtime/sdk_memory_kernel.py L892-L921）
// → IngestService.ingest_summary（core/runtime/services/ingest_service.py L254-L319）：
// 摘要统一落到 ingest_text，source_type=chat_summary，kind=chat_summary。
func (k *Kernel) IngestSummary(ctx context.Context, chatID string, summary string, ts time.Time) error {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return nil
	}
	externalID := "chat_summary:" + chatID + ":" + sha256Hex(summary)
	return k.ingest(ctx, ingestRequest{
		externalID: externalID,
		sourceType: "chat_summary",
		text:       summary,
		chatID:     chatID,
		timestamp:  ts,
		metadata: map[string]any{
			"kind":        "chat_summary",
			"external_id": externalID,
			"chat_id":     chatID,
			"source_type": "chat_summary",
		},
		extract: true,
	})
}

// IngestText 写入原始文本，抽取实体/关系/段落。
// 语义对齐 IngestService.ingest_text（core/runtime/services/ingest_service.py L321-L475）。
func (k *Kernel) IngestText(ctx context.Context, chatID string, text string, ts time.Time) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	externalID := "chat_stream:" + chatID + ":" + sha256Hex(text)
	return k.ingest(ctx, ingestRequest{
		externalID: externalID,
		sourceType: "chat_stream",
		text:       text,
		chatID:     chatID,
		timestamp:  ts,
		metadata: map[string]any{
			"external_id": externalID,
			"chat_id":     chatID,
			"source_type": "chat_stream",
		},
		extract: true,
	})
}

// IngestImageMemory 把图片描述作为图片记忆写入（对齐 image_memory 写回语义）。
func (k *Kernel) IngestImageMemory(ctx context.Context, chatID string, description string, hash string, ts time.Time) error {
	description = strings.TrimSpace(description)
	if description == "" {
		return nil
	}
	externalID := "image:" + strings.TrimSpace(hash)
	if externalID == "image:" {
		externalID = "image:" + sha256Hex(description)
	}
	return k.ingest(ctx, ingestRequest{
		externalID: externalID,
		sourceType: "image_memory",
		text:       description,
		chatID:     chatID,
		timestamp:  ts,
		metadata: map[string]any{
			"kind":        "image_memory",
			"image_hash":  hash,
			"external_id": externalID,
			"chat_id":     chatID,
			"source_type": "image_memory",
		},
		extract: true,
	})
}

type ingestRequest struct {
	externalID string
	sourceType string
	text       string
	chatID     string
	timestamp  time.Time
	metadata   map[string]any
	extract    bool
}

// ingest 是写入主链路：幂等段落 → 向量（或降级）→ 实体/关系 → 链接。
func (k *Kernel) ingest(ctx context.Context, req ingestRequest) error {
	if k.closed {
		return fmt.Errorf("memorix: 内核已关闭")
	}
	k.writeMu.Lock()
	defer k.writeMu.Unlock()

	existing, err := k.store.GetExternalRef(ctx, req.externalID)
	if err != nil {
		return wrapError("ingest", err)
	}
	if existing != nil {
		// 已写入过（幂等）：不重复写入。
		return nil
	}

	content := storage.NormalizeText(req.text)
	if content == "" {
		return nil
	}
	ts := req.timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	tsFloat := float64(ts.UnixNano()) / 1e9

	paragraphHash, err := k.store.AddParagraph(ctx, storage.ParagraphInput{
		Content:       content,
		Source:        req.sourceType + ":" + req.chatID,
		ChatID:        req.chatID,
		EventTime:     &tsFloat,
		KnowledgeType: knowledgeTypeFor(req.sourceType),
		Metadata:      req.metadata,
	})
	if err != nil {
		return wrapError("ingest", err)
	}

	// 段落向量：失败时按配置允许 metadata-only 写入。
	if err := k.writeParagraphVector(ctx, paragraphHash, content); err != nil {
		if !k.embed.AllowMetadataOnlyWrite() {
			return wrapError("ingest", err)
		}
	}

	// 实体/关系抽取。
	var extraction *Extraction
	if req.extract {
		extracted, err := k.ExtractFromText(ctx, content)
		if err != nil {
			return wrapError("ingest", err)
		}
		extraction = extracted
	} else {
		extraction = &Extraction{}
	}
	entities := dedupeStrings(append(append([]string{}, extraction.Entities...), RuleBasedExtractEntities(content)...))
	for _, name := range entities {
		entityHash, err := k.store.AddEntity(ctx, name)
		if err != nil {
			return wrapError("ingest", err)
		}
		if err := k.store.LinkParagraphEntity(ctx, paragraphHash, entityHash); err != nil {
			return wrapError("ingest", err)
		}
		if err := k.writeEntityVector(ctx, entityHash, name); err != nil && !k.embed.AllowMetadataOnlyWrite() {
			return wrapError("ingest", err)
		}
	}
	relations := DedupeRelations(extraction.Relations)
	for _, relation := range relations {
		relationHash, _, err := k.store.UpsertRelation(ctx, relation.Subject, relation.Predicate, relation.Object,
			relation.Confidence, paragraphHash, map[string]any{
				"external_id": req.externalID,
				"source_type": req.sourceType,
			})
		if err != nil {
			return wrapError("ingest", err)
		}
		if err := k.store.LinkParagraphRelation(ctx, paragraphHash, relationHash); err != nil {
			return wrapError("ingest", err)
		}
		content := strings.TrimSpace(relation.Subject + " " + relation.Predicate + " " + relation.Object)
		if err := k.writeRelationVector(ctx, relationHash, content); err != nil && !k.embed.AllowMetadataOnlyWrite() {
			return wrapError("ingest", err)
		}
	}
	k.store.InvalidateGraph()

	return k.store.UpsertExternalRef(ctx, req.externalID, paragraphHash, req.sourceType, map[string]any{
		"chat_id": req.chatID,
	})
}

// writeParagraphVector 写段落向量；嵌入不可用时返回错误（由调用方决定降级）。
func (k *Kernel) writeParagraphVector(ctx context.Context, hash, content string) error {
	vec, err := k.embed.Encode(ctx, content)
	if err != nil {
		return err
	}
	return k.store.UpsertVector(ctx, "paragraph", hash, vec)
}

func (k *Kernel) writeEntityVector(ctx context.Context, hash, name string) error {
	vec, err := k.embed.Encode(ctx, name)
	if err != nil {
		return err
	}
	return k.store.UpsertVector(ctx, "entity", hash, vec)
}

func (k *Kernel) writeRelationVector(ctx context.Context, hash, content string) error {
	vec, err := k.embed.Encode(ctx, content)
	if err != nil {
		return err
	}
	return k.store.UpsertVector(ctx, "relation", hash, vec)
}

func knowledgeTypeFor(sourceType string) string {
	switch strings.TrimSpace(sourceType) {
	case "chat_summary":
		return "chat_summary"
	case "chat_stream":
		return "chat_log"
	case "image_memory":
		return "image"
	case "person_fact":
		return "person_fact"
	default:
		return "mixed"
	}
}

// ---------------------------------------------------------------------------
// 检索
// ---------------------------------------------------------------------------

// SearchMemory 执行双路检索；mode 取 search|time|hybrid|episode|aggregate。
// 语义对齐 MemorySearchService.search_memory（core/runtime/services/memory_search_service.py L20-L140）。
func (k *Kernel) SearchMemory(ctx context.Context, mode string, query string, chatID string, topK int) ([]contracts.MemoryHit, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "search"
	}
	switch mode {
	case "search", "time", "hybrid", "episode", "aggregate":
	default:
		return nil, fmt.Errorf("memorix: 不支持的检索模式: %s（仅支持 search/time/hybrid/episode/aggregate）", mode)
	}
	// top_k 对齐 SearchExecutionService._resolve_top_k：
	// time/hybrid 用 temporal.default_top_k，其余默认 5（宿主 memory_query_default_limit），
	// 统一裁剪到 [1, 50]。
	if topK <= 0 {
		if mode == "time" || mode == "hybrid" {
			topK = k.options.Retrieval.TemporalDefaultTopK
			if topK <= 0 {
				topK = DefaultTemporalTopK
			}
		} else {
			topK = 5
		}
	}
	if topK > 50 {
		topK = 50
	}
	if topK < 1 {
		topK = 1
	}
	query = strings.TrimSpace(query)

	switch mode {
	case "episode":
		return k.searchEpisodes(ctx, query, topK)
	case "aggregate":
		return k.searchAggregate(ctx, query, chatID, topK)
	}

	temporal := k.temporalForQuery(mode, query)
	scope, err := k.scopeForChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	results, err := k.search.Retrieve(ctx, query, topK, "", temporal, nil, scope)
	if err != nil {
		return nil, wrapError("search", err)
	}
	// 动态阈值过滤（对齐 ThresholdFilter.filter）：
	// time 模式且 query 为空时跳过（对齐 skip_threshold_when_query_empty=true 默认值）。
	shouldApplyThreshold := !(mode == "time" && query == "")
	if shouldApplyThreshold && len(results) > 0 {
		filter := retrieval.NewDynamicThresholdFilter(k.options.Retrieval.Threshold)
		results = filter.Filter(results)
	}
	hits := make([]contracts.MemoryHit, 0, len(results))
	for _, item := range results {
		hit := contracts.MemoryHit{
			Kind:       item.Type,
			ID:         item.Hash,
			Text:       item.Content,
			Score:      item.Score,
			ChatID:     chatID,
			Similarity: similarityOf(item),
		}
		if hit.Time, _ = itemTime(item); hit.Time.IsZero() {
			// 段落的 created_at 兜底（时间检索需要）。
			if item.Type == "paragraph" {
				if paragraph, err := k.store.GetParagraph(ctx, item.Hash); err == nil && paragraph != nil {
					hit.Time = paragraph.CreatedAt
				}
			}
		}
		hits = append(hits, hit)
	}
	// 会话范围过滤（对齐 resolve_retrieval_scope + _filter_hits_by_retrieval_scope）。
	if strings.TrimSpace(chatID) != "" {
		hits = k.filterHitsByChat(ctx, hits, chatID)
	}
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// scopeForChat 解析会话可见资源集合（对齐 resolve_retrieval_scope 语义）：
// 空 scope 在 Python 里表示「无可见资源」而非「不限制」，因此这里必须填充真实 id。
// 出处：core/retrieval/dual_path.py L101-L112 与 memory_search_service.py L42-L46。
func (k *Kernel) scopeForChat(ctx context.Context, chatID string) (*retrieval.Scope, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return nil, nil
	}
	scope := retrieval.NewScope(chatID)
	paragraphs, err := k.store.ParagraphsForChat(ctx, chatID, 0)
	if err != nil {
		return nil, wrapError("search_scope", err)
	}
	for _, paragraph := range paragraphs {
		scope.ParagraphIDs[paragraph.Hash] = struct{}{}
	}
	relations, err := k.store.RelationHashesForChat(ctx, chatID)
	if err != nil {
		return nil, wrapError("search_scope", err)
	}
	for _, hash := range relations {
		scope.RelationIDs[hash] = struct{}{}
	}
	entities, err := k.store.EntityHashesForChat(ctx, chatID)
	if err != nil {
		return nil, wrapError("search_scope", err)
	}
	for _, hash := range entities {
		scope.EntityIDs[hash] = struct{}{}
	}
	return scope, nil
}

// temporalForQuery 构造时序窗口：time 模式默认取最近 24 小时；
// hybrid 模式在语义召回上叠加时间过滤（对齐 QueryType search/time/hybrid 语义）。
func (k *Kernel) temporalForQuery(mode, query string) *retrieval.TemporalQueryOptions {
	opts := retrieval.DefaultTemporalQueryOptions()
	switch mode {
	case "time":
		from := float64(time.Now().Add(-24*time.Hour).UnixNano()) / 1e9
		opts.TimeFrom = &from
	case "hybrid":
		// 从查询里解析相对时间窗（如「今天」「昨天」「最近三天」）。
		if from, to, ok := parseRelativeTimeWindow(query, time.Now()); ok {
			opts.TimeFrom = &from
			opts.TimeTo = &to
		} else {
			from := float64(time.Now().Add(-7*24*time.Hour).UnixNano()) / 1e9
			opts.TimeFrom = &from
		}
	default:
		return nil
	}
	return &opts
}

// searchEpisodes 检索情景记忆（episode 分支）。
func (k *Kernel) searchEpisodes(ctx context.Context, query string, topK int) ([]contracts.MemoryHit, error) {
	tokens := k.index.Tokenizer().Tokenize(query)
	tokens = retrieval.MatchTokens(tokens)
	episodes, err := k.store.SearchEpisodes(ctx, tokens, topK)
	if err != nil {
		return nil, wrapError("search_episode", err)
	}
	hits := make([]contracts.MemoryHit, 0, len(episodes))
	for rank, episode := range episodes {
		score := 1.0 / float64(rank+1)
		var ts time.Time
		if episode.EventEnd != nil {
			ts = float64ToTime(*episode.EventEnd)
		} else if episode.EventStart != nil {
			ts = float64ToTime(*episode.EventStart)
		} else {
			ts = episode.UpdatedAt
		}
		hits = append(hits, contracts.MemoryHit{
			Kind:       "episode",
			ID:         episode.ID,
			Text:       episode.Summary,
			Score:      score,
			Time:       ts,
			Similarity: score,
		})
	}
	return hits, nil
}

// searchAggregate 聚合三种分支并按 RRF 混合（对齐 aggregate_query_service）。
// 出处：core/utils/aggregate_query_service.py L108-L158（weight/(rrf_k+rank)）。
func (k *Kernel) searchAggregate(
	ctx context.Context, query string, chatID string, topK int,
) ([]contracts.MemoryHit, error) {
	branches := map[string][]contracts.MemoryHit{}
	searchHits, err := k.SearchMemory(ctx, "search", query, chatID, topK)
	if err != nil {
		return nil, err
	}
	branches["search"] = searchHits
	timeHits, err := k.SearchMemory(ctx, "time", query, chatID, topK)
	if err != nil {
		return nil, err
	}
	branches["time"] = timeHits
	episodeHits, err := k.SearchMemory(ctx, "episode", query, chatID, topK)
	if err != nil {
		return nil, err
	}
	branches["episode"] = episodeHits

	weights := map[string]float64{
		"search":  retrieval.AggregateDefaultSearchWeight,
		"time":    retrieval.AggregateDefaultTimeWeight,
		"episode": retrieval.AggregateDefaultEpisodeWeight,
	}
	order := []string{"search", "time", "episode"}
	type bucket struct {
		hit   contracts.MemoryHit
		score float64
	}
	byID := map[string]*bucket{}
	var ids []string
	for _, branch := range order {
		weight := weights[branch]
		for rank, hit := range branches[branch] {
			key := branch + ":" + hit.ID
			item, ok := byID[key]
			if !ok {
				copyHit := hit
				item = &bucket{hit: copyHit}
				byID[key] = item
				ids = append(ids, key)
			}
			item.score += weight / (retrieval.AggregateDefaultRRFK + float64(rank+1))
		}
	}
	sort.SliceStable(ids, func(i, j int) bool {
		if byID[ids[i]].score != byID[ids[j]].score {
			return byID[ids[i]].score > byID[ids[j]].score
		}
		return ids[i] < ids[j]
	})
	out := make([]contracts.MemoryHit, 0, len(ids))
	for _, key := range ids {
		item := byID[key]
		item.hit.Score = item.score
		out = append(out, item.hit)
		if len(out) >= topK {
			break
		}
	}
	return out, nil
}

// filterHitsByChat 按会话可见性过滤命中（段落/关系都要落在该会话内）。
// 对齐 _filter_hits_by_retrieval_scope（memory_search_service.py L120-L140）。
func (k *Kernel) filterHitsByChat(
	ctx context.Context, hits []contracts.MemoryHit, chatID string,
) []contracts.MemoryHit {
	out := make([]contracts.MemoryHit, 0, len(hits))
	for _, hit := range hits {
		switch hit.Kind {
		case "paragraph":
			paragraph, err := k.store.GetParagraph(ctx, hit.ID)
			if err != nil || paragraph == nil {
				continue
			}
			if paragraph.ChatID != "" && paragraph.ChatID != chatID {
				continue
			}
			hit.ChatID = paragraph.ChatID
			hit.Time = paragraph.CreatedAt
			out = append(out, hit)
		case "relation":
			paragraphs, err := k.store.ParagraphsByRelation(ctx, hit.ID)
			if err != nil || len(paragraphs) == 0 {
				out = append(out, hit)
				continue
			}
			match := false
			for _, paragraph := range paragraphs {
				if paragraph.ChatID == "" || paragraph.ChatID == chatID {
					match = true
					hit.ChatID = paragraph.ChatID
					if ts := paragraphTime(paragraph); !ts.IsZero() {
						hit.Time = ts
					}
					break
				}
			}
			if !match {
				continue
			}
			out = append(out, hit)
		case "episode":
			out = append(out, hit)
		default:
			out = append(out, hit)
		}
	}
	return out
}

func paragraphTime(paragraph *retrieval.Paragraph) time.Time {
	if paragraph == nil {
		return time.Time{}
	}
	if paragraph.EventEnd != nil {
		return float64ToTime(*paragraph.EventEnd)
	}
	if paragraph.EventStart != nil {
		return float64ToTime(*paragraph.EventStart)
	}
	if paragraph.EventTime != nil {
		return float64ToTime(*paragraph.EventTime)
	}
	return paragraph.CreatedAt
}

func itemTime(item *retrieval.Result) (time.Time, bool) {
	meta, ok := item.Metadata["time_meta"].(map[string]any)
	if !ok {
		return time.Time{}, false
	}
	if v := retrieval.ToFloat(meta["effective_end"]); v > 0 {
		return float64ToTime(v), true
	}
	if v := retrieval.ToFloat(meta["effective_start"]); v > 0 {
		return float64ToTime(v), true
	}
	return time.Time{}, false
}

func similarityOf(item *retrieval.Result) float64 {
	meta := retrieval.ScoreBreakdownOf(item)
	best := meta["semantic"]
	if v := retrieval.ToFloat(meta["sparse"]); retrieval.ToFloat(best) < v {
		best = meta["sparse"]
	}
	if v := retrieval.ToFloat(meta["graph_evidence"]); retrieval.ToFloat(best) < v {
		best = meta["graph_evidence"]
	}
	if value := retrieval.ToFloat(best); value > 0 {
		return value
	}
	return item.Score
}

func float64ToTime(value float64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	sec := int64(value)
	nsec := int64((value - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

// ---------------------------------------------------------------------------
// 人物画像
// ---------------------------------------------------------------------------

// GetPersonProfile 返回聚合后的人物画像。
// 语义对齐 SDKMemoryKernel.get_person_profile（sdk_memory_kernel.py L1032-L1040）
// 与 person_profile_service 的输出去重口径。
func (k *Kernel) GetPersonProfile(ctx context.Context, personID string) (*contracts.PersonProfile, error) {
	personID = strings.TrimSpace(personID)
	if personID == "" {
		return nil, fmt.Errorf("memorix: person_id 不能为空")
	}
	facts, err := k.store.ProfileFacts(ctx, personID, 0)
	if err != nil {
		return nil, wrapError("get_person_profile", err)
	}
	name, summary, updatedAt, err := k.store.Profile(ctx, personID)
	if err != nil {
		return nil, wrapError("get_person_profile", err)
	}
	profile := &contracts.PersonProfile{
		PersonID:  personID,
		Name:      name,
		Summary:   summary,
		Facts:     make([]contracts.ProfileFact, 0, len(facts)),
		UpdatedAt: updatedAt,
	}
	for _, fact := range facts {
		profile.Facts = append(profile.Facts, contracts.ProfileFact{
			ID:         fact.ID,
			Category:   fact.Category,
			Content:    fact.Content,
			Confidence: fact.Confidence,
			Source:     fact.Source,
			CreatedAt:  fact.CreatedAt,
			UpdatedAt:  fact.UpdatedAt,
		})
		if fact.UpdatedAt.After(profile.UpdatedAt) {
			profile.UpdatedAt = fact.UpdatedAt
		}
		if profile.Name == "" && fact.Category == "identity" {
			profile.Name = fact.Content
		}
	}
	if profile.UpdatedAt.IsZero() {
		profile.UpdatedAt = time.Now().UTC()
	}

	// 关系：以人物名为实体查图谱中的一跳关系。
	relations, err := k.store.RelationsForPerson(ctx, pickName(name, personID), 50)
	if err != nil {
		return nil, wrapError("get_person_profile", err)
	}
	target := strings.ToLower(strings.TrimSpace(pickName(name, personID)))
	for _, relation := range relations {
		kind := relation.Predicate
		other := relation.Object
		if strings.ToLower(strings.TrimSpace(relation.Object)) == target {
			other = relation.Subject
			kind = "inverse:" + relation.Predicate
		}
		if strings.ToLower(strings.TrimSpace(other)) == target {
			continue
		}
		profile.Relations = append(profile.Relations, contracts.ProfileRelation{
			TargetPersonID: other,
			Kind:           kind,
			Confidence:     relation.Confidence,
		})
	}
	if profile.Summary == "" && len(profile.Facts) > 0 {
		profile.Summary = strings.Join(factContents(profile.Facts), "；")
	}
	return profile, nil
}

// UpsertPersonFact 写入一条人物事实。
func (k *Kernel) UpsertPersonFact(ctx context.Context, personID string, name string, fact contracts.ProfileFact) error {
	personID = strings.TrimSpace(personID)
	if personID == "" {
		return fmt.Errorf("memorix: person_id 不能为空")
	}
	content := strings.TrimSpace(fact.Content)
	if content == "" {
		return fmt.Errorf("memorix: fact 内容不能为空")
	}
	if err := k.store.UpsertProfile(ctx, personID, name, ""); err != nil {
		return wrapError("upsert_person_fact", err)
	}
	if err := k.store.UpsertProfileFact(ctx, storage.ProfileFact{
		ID:         fact.ID,
		PersonID:   personID,
		Category:   fact.Category,
		Content:    content,
		Confidence: fact.Confidence,
		Source:     fact.Source,
	}); err != nil {
		return wrapError("upsert_person_fact", err)
	}
	return nil
}

// Close 关闭内核存储。
func (k *Kernel) Close() error {
	k.writeMu.Lock()
	defer k.writeMu.Unlock()
	if k.closed {
		return nil
	}
	k.closed = true
	if k.store == nil {
		return nil
	}
	return k.store.Close()
}

// Stats 返回内核规模快照（供 admin/日志）。
func (k *Kernel) Stats(ctx context.Context) map[string]any {
	stats := k.store.StatsSnapshot(ctx)
	stats["embedding_degraded"] = k.embed.Degraded()
	stats["embedding_dimension"] = k.embed.Dimension()
	return stats
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func dedupeStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func factContents(facts []contracts.ProfileFact) []string {
	out := make([]string, 0, len(facts))
	for _, fact := range facts {
		if strings.TrimSpace(fact.Content) != "" {
			out = append(out, strings.TrimSpace(fact.Content))
		}
	}
	return out
}

func pickName(name, fallback string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return fallback
}

// parseRelativeTimeWindow 解析常见中文相对时间表达，返回 [from, to) 时间窗。
// 对齐 core/utils/time_parser.py parse_query_time_range 的常用面（今天/昨天/前天/最近N天）。
func parseRelativeTimeWindow(query string, now time.Time) (float64, float64, bool) {
	text := strings.ToLower(strings.TrimSpace(query))
	if text == "" {
		return 0, 0, false
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	toFloat := func(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }
	switch {
	case strings.Contains(text, "今天") || strings.Contains(text, "今日"):
		return toFloat(dayStart), toFloat(dayStart.Add(24 * time.Hour)), true
	case strings.Contains(text, "昨天") || strings.Contains(text, "昨日"):
		return toFloat(dayStart.Add(-24 * time.Hour)), toFloat(dayStart), true
	case strings.Contains(text, "前天"):
		return toFloat(dayStart.Add(-48 * time.Hour)), toFloat(dayStart.Add(-24 * time.Hour)), true
	case strings.Contains(text, "本周") || strings.Contains(text, "这周"):
		offset := int(dayStart.Weekday()+6) % 7
		weekStart := dayStart.AddDate(0, 0, -offset)
		return toFloat(weekStart), toFloat(now.Add(time.Hour)), true
	case strings.Contains(text, "最近"):
		days := 3
		for _, candidate := range []int{1, 2, 3, 5, 7, 14, 30} {
			if strings.Contains(text, fmt.Sprintf("%d", candidate)) {
				days = candidate
				break
			}
		}
		return toFloat(now.AddDate(0, 0, -days)), toFloat(now.Add(time.Hour)), true
	}
	return 0, 0, false
}
