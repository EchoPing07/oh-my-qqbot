package memorix_test

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"oh-my-qqbot/internal/memorix/retrieval"
	"oh-my-qqbot/internal/memorix/storage"
)

// fakeEmbedder 是确定性假向量生成器（trigram 哈希投影 + L2 归一化）。
type fakeEmbedder struct{ dim int }

func (f *fakeEmbedder) Encode(_ context.Context, text string) ([]float32, error) {
	return fakeVector(text, f.dim), nil
}

func (f *fakeEmbedder) Dimension() int { return f.dim }

func fakeVector(text string, dim int) []float32 {
	vec := make([]float32, dim)
	runes := []rune(text)
	if len(runes) == 0 {
		return vec
	}
	gram := 3
	if len(runes) < gram {
		gram = len(runes)
	}
	for i := 0; i+gram <= len(runes); i++ {
		var hash uint32 = 2166136261
		for _, r := range runes[i : i+gram] {
			hash ^= uint32(r)
			hash *= 16777619
		}
		vec[hash%uint32(dim)]++
	}
	norm := float64(0)
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		vec[0] = 1
		return vec
	}
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

// openStore 打开临时库。
func openStore(t *testing.T) *storage.Store {
	t.Helper()
	options := storage.DefaultOpenOptions()
	options.Path = filepath.Join(t.TempDir(), "a_memorix.db")
	store, err := storage.Open(context.Background(), options)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// vectorSource 把 storage.Store 适配为检索层向量源（与 memorix.Kernel 内部实现同形）。
type vectorSource struct{ store *storage.Store }

func (v vectorSource) Dimension() int { return v.store.Dimension() }
func (v vectorSource) Count() int {
	index := v.store.VectorIndexRef()
	if index == nil {
		return 0
	}
	return index.Count()
}
func (v vectorSource) SearchVectors(
	itemType string, query []float32, k int, allowed map[string]struct{},
) ([]retrieval.VectorHit, error) {
	return v.store.SearchVectors(itemType, query, k, allowed), nil
}

func (v vectorSource) SearchParagraphs(
	query []float32, k int, allowed map[string]struct{},
) ([]retrieval.VectorHit, error) {
	return v.store.SearchVectors("paragraph", query, k, allowed), nil
}
func (v vectorSource) SearchGraph(
	query []float32, k int, allowed map[string]struct{},
) ([]retrieval.VectorHit, error) {
	hits := v.store.SearchVectors("relation", query, k, allowed)
	out := make([]retrieval.VectorHit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, retrieval.VectorHit{ID: retrieval.GraphVectorID("relation", hit.ID), Score: hit.Score})
	}
	entities := v.store.SearchVectors("entity", query, k, allowed)
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

// seedCorpus 写入语料并返回段落 hash。
func seedCorpus(t *testing.T, store *storage.Store, embedder *fakeEmbedder) map[string]string {
	t.Helper()
	ctx := context.Background()
	documents := []struct {
		key     string
		content string
		old     bool
	}{
		{"coffee", "小明非常喜欢喝手冲咖啡，每天下午都会去楼下的咖啡店", false},
		{"weather", "今天天气预报说有暴雨，出门记得带伞", false},
		{"coding", "他最近在学习 Go 语言，写了很多关于 SQLite 存储的代码", false},
		{"history", "上个月团队讨论了新的记忆检索架构设计", true},
	}
	hashes := map[string]string{}
	for _, doc := range documents {
		ts := time.Now().Add(-time.Minute)
		if doc.old {
			ts = time.Now().Add(-45 * 24 * time.Hour)
		}
		tsFloat := float64(ts.UnixNano()) / 1e9
		hash, err := store.AddParagraph(ctx, storage.ParagraphInput{
			Content:   doc.content,
			Source:    "chat_stream:g1",
			ChatID:    "g1",
			EventTime: &tsFloat,
		})
		if err != nil {
			t.Fatalf("add paragraph %s: %v", doc.key, err)
		}
		hashes[doc.key] = hash
		vec, err := embedder.Encode(ctx, doc.content)
		if err != nil {
			t.Fatalf("encode %s: %v", doc.key, err)
		}
		if err := store.UpsertVector(ctx, "paragraph", hash, vec); err != nil {
			t.Fatalf("upsert paragraph vector %s: %v", doc.key, err)
		}
	}
	// 关系 + 实体向量（图证据池）。
	relations := []struct{ subject, predicate, object, paragraph string }{
		{"小明", "喜欢", "手冲咖啡", "coffee"},
		{"小明", "学习", "Go 语言", "coding"},
		{"团队", "讨论", "记忆检索架构", "history"},
	}
	for _, relation := range relations {
		hash, _, err := store.UpsertRelation(ctx, relation.subject, relation.predicate, relation.object,
			1.0, hashes[relation.paragraph], nil)
		if err != nil {
			t.Fatalf("upsert relation: %v", err)
		}
		if err := store.LinkParagraphRelation(ctx, hashes[relation.paragraph], hash); err != nil {
			t.Fatalf("link relation: %v", err)
		}
		entityHash, err := store.AddEntity(ctx, relation.subject)
		if err != nil {
			t.Fatalf("add entity: %v", err)
		}
		if err := store.LinkParagraphEntity(ctx, hashes[relation.paragraph], entityHash); err != nil {
			t.Fatalf("link entity: %v", err)
		}
		content := relation.subject + " " + relation.predicate + " " + relation.object
		vec, err := embedder.Encode(ctx, content)
		if err != nil {
			t.Fatalf("encode relation: %v", err)
		}
		if err := store.UpsertVector(ctx, "relation", hash, vec); err != nil {
			t.Fatalf("upsert relation vector: %v", err)
		}
		entityVec, err := embedder.Encode(ctx, relation.subject)
		if err != nil {
			t.Fatalf("encode entity: %v", err)
		}
		if err := store.UpsertVector(ctx, "entity", entityHash, entityVec); err != nil {
			t.Fatalf("upsert entity vector: %v", err)
		}
	}
	store.InvalidateGraph()
	return hashes
}

// TestDualVectorPoolEndToEnd 端到端验证双向量池检索：段落池 + 图证据池 + 稀疏 + 校准。
func TestDualVectorPoolEndToEnd(t *testing.T) {
	store := openStore(t)
	embedder := &fakeEmbedder{dim: 256}
	hashes := seedCorpus(t, store, embedder)

	config := retrieval.DefaultRetrieverConfig()
	config.VectorPools.ScoreCalibrationMethod = "semantic_minmax"
	index := retrieval.NewSparseIndex(store, config.Sparse)
	retriever := retrieval.NewRetriever(store, vectorSource{store: store}, index, embedder, config)

	results, err := retriever.Retrieve(context.Background(),
		"小明喜欢喝手冲咖啡", 5, retrieval.StrategyDualPath, nil, nil, nil)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("双路检索应有结果")
	}
	if results[0].Hash != hashes["coffee"] {
		t.Errorf("top1 应为咖啡段落: got %s (%s)", results[0].Hash, results[0].Content)
	}
	if results[0].Source != "dual_vector_pool" {
		t.Errorf("source: got %s, want dual_vector_pool", results[0].Source)
	}
	meta := retrieval.ScoreBreakdownOf(results[0])
	if retrieval.ToFloat(meta["semantic"]) <= 0 {
		t.Errorf("应记录 semantic 通道分: %+v", meta)
	}
	if meta["score_calibration_method"] != "semantic_minmax" {
		t.Errorf("校准方法: %v", meta["score_calibration_method"])
	}
	if meta["final"] == nil {
		t.Errorf("应记录 final 分: %+v", meta)
	}
	// 三通道权重配置已记录。
	if retrieval.ToFloat(meta["configured_semantic_weight"]) == 0 {
		t.Errorf("应记录配置权重: %+v", meta)
	}
}

// TestRetrieveTimeOnly 验证纯时序检索分支（query 为空）。
func TestRetrieveTimeOnly(t *testing.T) {
	store := openStore(t)
	embedder := &fakeEmbedder{dim: 128}
	hashes := seedCorpus(t, store, embedder)

	config := retrieval.DefaultRetrieverConfig()
	retriever := retrieval.NewRetriever(store, vectorSource{store: store},
		retrieval.NewSparseIndex(store, config.Sparse), embedder, config)

	from := float64(time.Now().Add(-24*time.Hour).UnixNano()) / 1e9
	temporal := retrieval.DefaultTemporalQueryOptions()
	temporal.TimeFrom = &from

	results, err := retriever.Retrieve(context.Background(), "", 10, retrieval.StrategyDualPath, &temporal, nil, nil)
	if err != nil {
		t.Fatalf("temporal retrieve: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("时序检索应有结果")
	}
	for _, item := range results {
		if item.Hash == hashes["history"] {
			t.Error("45 天前的历史段落不应落在 24h 窗口内")
		}
		if item.Source != "temporal_scan" {
			t.Errorf("source: got %s, want temporal_scan", item.Source)
		}
		if item.Score != 1.0 {
			t.Errorf("时序扫描分数应为 1.0: %v", item.Score)
		}
	}
	// 时间倒序（最新在前）。
	for i := 1; i < len(results); i++ {
		prev, _ := timeMetaEnd(results[i-1])
		cur, _ := timeMetaEnd(results[i])
		if cur > prev {
			t.Errorf("时间戳未倒序: %v > %v", cur, prev)
		}
	}
}

// TestRetrieveScopeFiltersResources 验证 scope 会真实收敛可见资源。
func TestRetrieveScopeFiltersResources(t *testing.T) {
	store := openStore(t)
	embedder := &fakeEmbedder{dim: 128}
	hashes := seedCorpus(t, store, embedder)

	config := retrieval.DefaultRetrieverConfig()
	retriever := retrieval.NewRetriever(store, vectorSource{store: store},
		retrieval.NewSparseIndex(store, config.Sparse), embedder, config)

	scope := retrieval.NewScope("test")
	scope.ParagraphIDs[hashes["weather"]] = struct{}{}
	results, err := retriever.Retrieve(context.Background(),
		"小明喜欢喝手冲咖啡", 5, retrieval.StrategyDualPath, nil, nil, scope)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	for _, item := range results {
		if item.Type == "paragraph" && item.Hash != hashes["weather"] {
			t.Errorf("scope 外的段落不应返回: %s", item.Hash)
		}
	}
	// 空 scope（无可见资源）→ 空结果。
	empty := &retrieval.Scope{Key: "empty"}
	if !empty.Empty() {
		t.Fatal("空 scope 判定失败")
	}
	results, err = retriever.Retrieve(context.Background(),
		"查询", 5, retrieval.StrategyDualPath, nil, nil, empty)
	if err != nil {
		t.Fatalf("retrieve empty scope: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("空 scope 应返回空: %+v", results)
	}
}

// TestRetrieveParagraphOnlyRRF 验证 paragraph_only 的 weighted RRF 融合。
func TestRetrieveParagraphOnlyRRF(t *testing.T) {
	store := openStore(t)
	embedder := &fakeEmbedder{dim: 256}
	seedCorpus(t, store, embedder)

	config := retrieval.DefaultRetrieverConfig()
	config.Sparse.Mode = "hybrid" // 强制同时启用稀疏，验证 RRF
	retriever := retrieval.NewRetriever(store, vectorSource{store: store},
		retrieval.NewSparseIndex(store, config.Sparse), embedder, config)

	results, err := retriever.Retrieve(context.Background(),
		"记忆检索架构", 5, retrieval.StrategyParagraphOnly, nil, nil, nil)
	if err != nil {
		t.Fatalf("paragraph only: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("应有结果")
	}
	found := false
	for _, item := range results {
		if item.Source == "fusion_rrf" {
			found = true
		}
		if item.Type != "paragraph" {
			t.Errorf("paragraph_only 不应返回关系: %+v", item)
		}
	}
	if !found {
		t.Errorf("hybrid 稀疏应产生 fusion_rrf 结果: %+v", results)
	}
}

// TestRerankWithPPRBoostsGraphNeighbours 验证 PPR 重排对图谱邻居段落的增益。
func TestRerankWithPPRBoostsGraphNeighbours(t *testing.T) {
	store := openStore(t)
	embedder := &fakeEmbedder{dim: 256}
	hashes := seedCorpus(t, store, embedder)

	config := retrieval.DefaultRetrieverConfig()
	config.PPRLocalEnabled = false // 全图 PPR，便于断言
	retriever := retrieval.NewRetriever(store, vectorSource{store: store},
		retrieval.NewSparseIndex(store, config.Sparse), embedder, config)

	ctx := context.Background()
	entities, err := retriever.ExtractEntities(ctx, "小明喜欢什么")
	if err != nil {
		t.Fatalf("extract entities: %v", err)
	}
	if _, ok := entities["小明"]; !ok {
		t.Fatalf("AC 应识别实体「小明」: %+v", entities)
	}

	base := []*retrieval.Result{
		{Hash: hashes["coffee"], Content: "咖啡", Score: 0.5, Type: "paragraph",
			Metadata: map[string]any{}},
		{Hash: hashes["weather"], Content: "天气", Score: 0.5, Type: "paragraph",
			Metadata: map[string]any{}},
	}
	reranked, err := retriever.RerankWithPPR(ctx, base, "小明喜欢什么")
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(reranked) != 2 {
		t.Fatalf("rerank 不应丢结果: %+v", reranked)
	}
	coffeeMeta := retrieval.ScoreBreakdownOf(reranked[0])
	if reranked[0].Hash != hashes["coffee"] {
		t.Fatalf("图邻居段落应被增益到第一: %+v", reranked)
	}
	if retrieval.ToFloat(coffeeMeta["ppr_boost"]) <= 0 {
		t.Errorf("应记录 ppr_boost: %+v", coffeeMeta)
	}
	if retrieval.ToFloat(coffeeMeta["ppr_signal"]) <= 0 {
		t.Errorf("应记录 ppr_signal: %+v", coffeeMeta)
	}
	if reranked[1].Score != 0.5 {
		t.Errorf("无实体段落分数不应变化: %v", reranked[1].Score)
	}
}

// TestBuildGraphResultsFromSeedsFixture 验证由种子构造图结果并加 PPR 关系权重。
func TestBuildGraphResultsFromSeedsFixture(t *testing.T) {
	store := openStore(t)
	embedder := &fakeEmbedder{dim: 128}
	seedCorpus(t, store, embedder)

	config := retrieval.DefaultRetrieverConfig()
	retriever := retrieval.NewRetriever(store, vectorSource{store: store},
		retrieval.NewSparseIndex(store, config.Sparse), embedder, config)

	results, err := retriever.BuildGraphResultsFromSeeds(context.Background(), []string{"小明"}, nil, 0.5, nil)
	if err != nil {
		t.Fatalf("build graph results: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("应由种子召回关系")
	}
	for _, item := range results {
		if item.Type != "relation" {
			t.Errorf("应为关系结果: %+v", item)
		}
		if item.Source != "posterior_graph_competition" {
			t.Errorf("source: got %s", item.Source)
		}
		if item.Score <= 0 {
			t.Errorf("关系权重 (1-alpha)=0.5 应产生正分: %v", item.Score)
		}
		meta := retrieval.ScoreBreakdownOf(item)
		_ = meta
		if item.Metadata["graph_candidate_type"] == "" {
			t.Errorf("应记录候选类型: %+v", item.Metadata)
		}
	}
}

// TestPosteriorGraphGateWithGraphEvidence 验证开启后验图门时的补位与决策语义。
func TestPosteriorGraphGateWithGraphEvidence(t *testing.T) {
	store := openStore(t)
	embedder := &fakeEmbedder{dim: 256}
	hashes := seedCorpus(t, store, embedder)

	config := retrieval.DefaultRetrieverConfig()
	config.PosteriorGraph.Enabled = true
	retriever := retrieval.NewRetriever(store, vectorSource{store: store},
		retrieval.NewSparseIndex(store, config.Sparse), embedder, config)

	// 只给一个低相关段落作为 base，让图门有机会补位。
	base := []*retrieval.Result{
		{Hash: hashes["weather"], Content: "今天天气预报说有暴雨", Score: 0.3, Type: "paragraph",
			Metadata: map[string]any{"score_breakdown": map[string]any{"semantic": 0.3}}},
	}
	out, err := retriever.ApplyPosteriorGraphGate(context.Background(), "小明喜欢什么咖啡",
		base, 5, nil, retrieval.RelationIntentContext{}, nil)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("图门不应清空结果")
	}
	// 原段落必须保留（图门只补位，不删除核心结果）。
	foundBase := false
	for _, item := range out {
		if item.Hash == hashes["weather"] {
			foundBase = true
		}
	}
	if !foundBase {
		t.Errorf("核心结果不应被删除: %+v", out)
	}
}

// TestEstimateGraphReliabilityFromVectors 验证真实图证据池下的可靠性估计与权重缩放。
func TestEstimateGraphReliabilityFromVectors(t *testing.T) {
	store := openStore(t)
	embedder := &fakeEmbedder{dim: 256}
	seedCorpus(t, store, embedder)

	config := retrieval.DefaultRetrieverConfig()
	retriever := retrieval.NewRetriever(store, vectorSource{store: store},
		retrieval.NewSparseIndex(store, config.Sparse), embedder, config)

	results, err := retriever.Retrieve(context.Background(),
		"小明 手冲咖啡 关系", 5, retrieval.StrategyDualPath, nil, nil, nil)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("应有结果")
	}
	meta := retrieval.ScoreBreakdownOf(results[0])
	if retrieval.ToFloat(meta["graph_weight"]) == 0 {
		t.Errorf("应记录图权重: %+v", meta)
	}
	// 图可靠性分量必须落在 [0,1]。
	for _, key := range []string{"graph_reliability", "graph_grounding_quality", "graph_channel_agreement", "graph_support_coverage"} {
		value := retrieval.ToFloat(meta[key])
		if value < 0 || value > 1 {
			t.Errorf("%s 超出 [0,1]: %v", key, value)
		}
	}
}

// TestRetrieveRelationIntentBoostsRelationWeight 验证关系意图会切换通道权重。
func TestRetrieveRelationIntentBoostsRelationWeight(t *testing.T) {
	store := openStore(t)
	embedder := &fakeEmbedder{dim: 256}
	seedCorpus(t, store, embedder)

	config := retrieval.DefaultRetrieverConfig()
	config.Sparse.Mode = "hybrid"
	retriever := retrieval.NewRetriever(store, vectorSource{store: store},
		retrieval.NewSparseIndex(store, config.Sparse), embedder, config)

	results, err := retriever.Retrieve(context.Background(),
		"小明和手冲咖啡是什么关系", 5, retrieval.StrategyDualPath, nil, nil, nil)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("应有结果")
	}
	meta := retrieval.ScoreBreakdownOf(results[0])
	if retrieval.ToFloat(meta["configured_graph_weight"]) != retrieval.DefaultRelationIntentGraphWeight {
		t.Errorf("关系意图应使用 graph_weight=%v: got %v",
			retrieval.DefaultRelationIntentGraphWeight, meta["configured_graph_weight"])
	}
	if retrieval.ToFloat(meta["configured_semantic_weight"]) != retrieval.DefaultRelationIntentSemanticWeight {
		t.Errorf("关系意图应使用 semantic_weight=%v: got %v",
			retrieval.DefaultRelationIntentSemanticWeight, meta["configured_semantic_weight"])
	}
}

// TestSparseOnlyFallbackWhenEmbeddingFails 验证嵌入失败时仍能靠 BM25 召回。
func TestSparseOnlyFallbackWhenEmbeddingFails(t *testing.T) {
	store := openStore(t)
	embedder := &fakeEmbedder{dim: 128}
	seedCorpus(t, store, embedder)

	config := retrieval.DefaultRetrieverConfig()
	retriever := retrieval.NewRetriever(store, nil, // 无向量源 → sparse-only
		retrieval.NewSparseIndex(store, config.Sparse), nil, config)

	results, err := retriever.Retrieve(context.Background(),
		"手冲咖啡", 5, retrieval.StrategyParagraphOnly, nil, nil, nil)
	if err != nil {
		t.Fatalf("sparse only: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("BM25 应召回咖啡段落")
	}
	for _, item := range results {
		if item.Source != "sparse_bm25" {
			t.Errorf("source: got %s, want sparse_bm25", item.Source)
		}
	}
}

// timeMetaEnd 读取结果 time_meta 的 effective_end。
func timeMetaEnd(item *retrieval.Result) (float64, bool) {
	meta, ok := item.Metadata["time_meta"].(map[string]any)
	if !ok {
		return 0, false
	}
	value := retrieval.ToFloat(meta["effective_end"])
	return value, value > 0
}

// TestRetrieveRelationsOnlyEntityPivot 验证关系单路的 Entity-Pivot 召回。
// 出处：core/retrieval/dual_path.py L1146-L1254。
func TestRetrieveRelationsOnlyEntityPivot(t *testing.T) {
	store := openStore(t)
	embedder := &fakeEmbedder{dim: 256}
	seedCorpus(t, store, embedder)

	config := retrieval.DefaultRetrieverConfig()
	retriever := retrieval.NewRetriever(store, vectorSource{store: store},
		retrieval.NewSparseIndex(store, config.Sparse), embedder, config)

	results, err := retriever.Retrieve(context.Background(),
		"小明 手冲咖啡", 5, retrieval.StrategyRelationOnly, nil, nil, nil)
	if err != nil {
		t.Fatalf("relation only: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("关系单路应有结果")
	}
	for _, item := range results {
		if item.Type != "relation" {
			t.Errorf("relation_only 只应返回关系: %+v", item)
		}
	}
	// 实体枢纽路径应能命中「小明 喜欢 手冲咖啡」。
	foundCoffee := false
	for _, item := range results {
		if item.Hash != "" && item.Metadata["object"] == "手冲咖啡" {
			foundCoffee = true
		}
	}
	if !foundCoffee {
		t.Errorf("应召回小明-咖啡关系: %+v", results)
	}
}

// TestSparseOnlyRelationOnly 验证无向量时的关系稀疏+图融合。
func TestSparseOnlyRelationOnly(t *testing.T) {
	store := openStore(t)
	embedder := &fakeEmbedder{dim: 128}
	seedCorpus(t, store, embedder)

	config := retrieval.DefaultRetrieverConfig()
	config.Sparse.Mode = "hybrid"
	retriever := retrieval.NewRetriever(store, nil,
		retrieval.NewSparseIndex(store, config.Sparse), nil, config)

	results, err := retriever.Retrieve(context.Background(),
		"小明 咖啡", 5, retrieval.StrategyRelationOnly, nil, nil, nil)
	if err != nil {
		t.Fatalf("sparse relation only: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("稀疏+图应召回关系")
	}
	for _, item := range results {
		if item.Type != "relation" {
			t.Errorf("应全部为关系: %+v", item)
		}
	}
}
