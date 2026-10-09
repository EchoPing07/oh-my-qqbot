package retrieval

import (
	"context"
	"math"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// BM25
// ---------------------------------------------------------------------------

// fakeFTS 是内存 FTS 双路替身：按 token 覆盖度模拟 BM25 排序。
type fakeFTS struct {
	paragraphs map[string]string
	relations  map[string]string
}

func (f *fakeFTS) SearchParagraphsFTS(
	_ context.Context, matchQuery string, limit int, allowedIDs []string,
) ([]SparseRow, error) {
	return f.search(f.paragraphs, matchQuery, limit, allowedIDs), nil
}

func (f *fakeFTS) SearchRelationsFTS(
	_ context.Context, matchQuery string, limit int, allowedIDs []string,
) ([]SparseRow, error) {
	return f.search(f.relations, matchQuery, limit, allowedIDs), nil
}

func (f *fakeFTS) search(
	docs map[string]string, matchQuery string, limit int, allowedIDs []string,
) []SparseRow {
	allowed := map[string]struct{}{}
	for _, id := range allowedIDs {
		allowed[id] = struct{}{}
	}
	tokens := parseMatchTokens(matchQuery)
	var rows []SparseRow
	for id, content := range docs {
		if allowedIDs != nil {
			if _, ok := allowed[id]; !ok {
				continue
			}
		}
		score := 0.0
		matched := 0
		for _, token := range tokens {
			if containsToken(content, token) {
				matched++
				score -= 1.0
			}
		}
		if matched == 0 {
			continue
		}
		rows = append(rows, SparseRow{Hash: id, Content: content, BM25Score: score})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].BM25Score != rows[j].BM25Score {
			return rows[i].BM25Score < rows[j].BM25Score
		}
		return rows[i].Hash < rows[j].Hash
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func parseMatchTokens(matchQuery string) []string {
	var out []string
	var current []rune
	inQuote := false
	for _, r := range matchQuery {
		switch {
		case r == '"':
			if inQuote {
				out = append(out, string(current))
				current = nil
			}
			inQuote = !inQuote
		case inQuote:
			current = append(current, r)
		}
	}
	return out
}

func containsToken(content, token string) bool {
	return len(token) > 0 && (stringIndex(content, token) >= 0)
}

func stringIndex(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestBM25ScorerRanksMoreMatchesHigher 验证内存 BM25 打分器单调性。
func TestBM25ScorerRanksMoreMatchesHigher(t *testing.T) {
	scorer := NewBM25Scorer()
	docA := scorer.Add([]string{"记忆", "检索", "记忆"})
	docB := scorer.Add([]string{"记忆", "闲聊"})
	docC := scorer.Add([]string{"闲聊", "天气"})
	query := []string{"记忆", "检索"}
	if scorer.Score(docA, query) <= scorer.Score(docB, query) {
		t.Errorf("doc 命中更多词应分数更高: A=%v B=%v",
			scorer.Score(docA, query), scorer.Score(docB, query))
	}
	if scorer.Score(docC, query) != 0 {
		t.Errorf("无命中文档应为 0, got %v", scorer.Score(docC, query))
	}
	// IDF：只出现在一处的词权重更高。
	single := scorer.Score(docA, []string{"检索"})
	common := scorer.Score(docA, []string{"记忆"})
	if single <= common {
		t.Errorf("稀缺词 IDF 应更高: 检索=%v 记忆=%v", single, common)
	}
}

// TestSparseIndexSearchRanksAndCounts 验证 FTS+BM25 检索的排序与命中统计。
func TestSparseIndexSearchRanksAndCounts(t *testing.T) {
	store := &fakeFTS{paragraphs: map[string]string{
		"p1": "小明今天在咖啡店谈记忆检索",
		"p2": "小明喜欢喝咖啡",
		"p3": "天气不错",
	}}
	index := NewSparseIndex(store, DefaultSparseConfig())
	rows, err := index.Search(context.Background(), "记忆检索", 10, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected hits")
	}
	if rows[0].Hash != "p1" {
		t.Errorf("top hit: got %s, want p1", rows[0].Hash)
	}
	if rows[0].Rank != 1 {
		t.Errorf("rank: got %d, want 1", rows[0].Rank)
	}
	if rows[0].Score != -rows[0].BM25Score {
		t.Errorf("score 应为 bm25 取反: score=%v bm25=%v", rows[0].Score, rows[0].BM25Score)
	}
	if rows[0].MatchedTokenCount == 0 || rows[0].MatchedTokenRatio <= 0 {
		t.Errorf("命中统计缺失: count=%d ratio=%v", rows[0].MatchedTokenCount, rows[0].MatchedTokenRatio)
	}
}

// TestSparseIndexHonoursScope 验证 allowed_ids 范围过滤。
func TestSparseIndexHonoursScope(t *testing.T) {
	store := &fakeFTS{paragraphs: map[string]string{
		"p1": "记忆检索",
		"p2": "记忆检索",
	}}
	index := NewSparseIndex(store, DefaultSparseConfig())
	rows, err := index.Search(context.Background(), "记忆", 10, []string{"p2"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) != 1 || rows[0].Hash != "p2" {
		t.Fatalf("scope filter failed: %+v", rows)
	}
	empty, err := index.Search(context.Background(), "记忆", 10, []string{})
	if err != nil {
		t.Fatalf("search empty scope: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("空 scope 应返回空, got %d", len(empty))
	}
}

// TestMatchTokensFiltersLowSignalSingles 验证单字中文 token 过滤。
// 出处：core/retrieval/sparse_bm25.py L421-L436。
func TestMatchTokensFiltersLowSignalSingles(t *testing.T) {
	tokens := []string{"的", "记忆", "检索"}
	got := MatchTokens(tokens)
	for _, token := range got {
		if token == "的" {
			t.Errorf("单字低信息 token 应被过滤: %v", got)
		}
	}
	if len(got) != 2 {
		t.Errorf("tokens: got %v, want [记忆 检索]", got)
	}
	// 全是单字时回退保留。
	allSingle := MatchTokens([]string{"的", "了"})
	if len(allSingle) != 2 {
		t.Errorf("全单字时应回退保留: %v", allSingle)
	}
}

// TestBuildMatchQueryEscapesAndCaps 验证 MATCH 表达式构造。
func TestBuildMatchQueryEscapesAndCaps(t *testing.T) {
	got := BuildMatchQuery([]string{"记忆", `引"号`})
	want := `"记忆" OR "引""号"`
	if got != want {
		t.Errorf("match query: got %q, want %q", got, want)
	}
	long := make([]string, 100)
	for i := range long {
		long[i] = "t"
	}
	query := BuildMatchQuery(long)
	count := 0
	for _, r := range query {
		if r == '"' {
			count++
		}
	}
	if count/2 != SparseMatchQueryMaxTokens {
		t.Errorf("token 上限: got %d, want %d", count/2, SparseMatchQueryMaxTokens)
	}
}

// TestShouldUseSparseAutoMode 验证 auto 模式回落判定（阈值 0.45）。
// 出处：core/retrieval/dual_path.py L573-L600。
func TestShouldUseSparseAutoMode(t *testing.T) {
	index := NewSparseIndex(&fakeFTS{}, DefaultSparseConfig())
	if index.ShouldUseSparse(true, []float64{0.9, 0.8}) {
		t.Error("高相似度时不应启用 sparse")
	}
	if !index.ShouldUseSparse(true, []float64{0.44, 0.3}) {
		t.Error("最高分 < 0.45 时应启用 sparse")
	}
	if index.ShouldUseSparse(true, []float64{0.45, 0.3}) {
		t.Error("最高分 == 0.45 时不应启用 sparse（严格小于）")
	}
	if !index.ShouldUseSparse(false, nil) {
		t.Error("embedding 失败时应启用 sparse")
	}
	if !index.ShouldUseSparse(true, nil) {
		t.Error("向量结果为空时应启用 sparse")
	}
	hybrid := DefaultSparseConfig()
	hybrid.Mode = "hybrid"
	if !NewSparseIndex(&fakeFTS{}, hybrid).ShouldUseSparse(true, []float64{0.99}) {
		t.Error("hybrid 模式应始终启用 sparse")
	}
}

// TestTrimSparseTail 验证稀疏尾部裁剪（相对分下限 0.2 与命中数阈值）。
// 出处：core/retrieval/dual_path.py L901-L946。
func TestTrimSparseTail(t *testing.T) {
	rows := []SparseRow{
		{Hash: "a", Score: 1.0, MatchedTokenCount: 3, MatchedTokenRatio: 0.6},
		{Hash: "b", Score: 0.9, MatchedTokenCount: 2, MatchedTokenRatio: 0.4},
		{Hash: "c", Score: 0.05, MatchedTokenCount: 1, MatchedTokenRatio: 0.1},
		{Hash: "d", Score: 0.05, MatchedTokenCount: 2, MatchedTokenRatio: 0.2},
	}
	kept := TrimSparseTail(rows)
	if len(kept) != 3 {
		t.Fatalf("kept: got %d, want 3 (c 应被裁掉)", len(kept))
	}
	for _, row := range kept {
		if row.Hash == "c" {
			t.Error("c 应被裁剪")
		}
	}
	if len(TrimSparseTail(rows[:2])) != 2 {
		t.Error("<= 2 行时不裁剪")
	}
}

// ---------------------------------------------------------------------------
// Aho-Corasick
// ---------------------------------------------------------------------------

// TestAhoCorasickFindAllCounts 验证重叠匹配计数（对齐 Python find_all）。
func TestAhoCorasickFindAllCounts(t *testing.T) {
	ac := NewAhoCorasick()
	ac.AddPattern("he")
	ac.AddPattern("she")
	ac.AddPattern("his")
	ac.AddPattern("hers")
	ac.Build()
	stats := ac.FindAll("ushers")
	// ushers: he(1) + she(1) + hers(1) = 3 次命中（含 fail 链继承）。
	if stats["he"] != 1 {
		t.Errorf("he 命中: got %d, want 1", stats["he"])
	}
	if stats["she"] != 1 {
		t.Errorf("she 命中: got %d, want 1", stats["she"])
	}
	if stats["hers"] != 1 {
		t.Errorf("hers 命中: got %d, want 1", stats["hers"])
	}
	if stats["his"] != 0 {
		t.Errorf("his 不应命中: got %d", stats["his"])
	}
}

// TestAhoCorasickMatchAllPositions 验证命中位置（rune 下标）。
func TestAhoCorasickMatchAllPositions(t *testing.T) {
	ac := NewAhoCorasick()
	ac.AddPattern("小明")
	ac.AddPattern("咖啡")
	ac.Build()
	matches := ac.MatchAll("小明喜欢咖啡")
	if len(matches) != 2 {
		t.Fatalf("matches: got %d, want 2 (%+v)", len(matches), matches)
	}
	if matches[0].Pattern != "小明" || matches[0].Start != 0 || matches[0].End != 2 {
		t.Errorf("first match: %+v", matches[0])
	}
	if matches[1].Pattern != "咖啡" || matches[1].Start != 4 || matches[1].End != 6 {
		t.Errorf("second match: %+v", matches[1])
	}
}

// TestAhoCorasickExtractEntities 验证实体抽取权重（出现次数）。
// 出处：core/retrieval/dual_path.py L3175-L3208。
func TestAhoCorasickExtractEntities(t *testing.T) {
	// 检索器建库时按小写注册模式（对齐 dual_path._extract_entities 的 add_pattern(entity.lower())）。
	ac := NewAhoCorasick()
	for _, name := range []string{"小明", "咖啡", "maibot"} {
		ac.AddPattern(name)
	}
	ac.Build()
	entities := ac.ExtractEntities("小明喜欢咖啡，小明也喜欢 MaiBot 和咖啡")
	if entities["小明"] != 2 {
		t.Errorf("小明 权重: got %v, want 2", entities["小明"])
	}
	if entities["咖啡"] != 2 {
		t.Errorf("咖啡 权重: got %v, want 2", entities["咖啡"])
	}
	if entities["maibot"] != 1 {
		t.Errorf("maibot（小写匹配）权重: got %v, want 1", entities["maibot"])
	}
}

// TestAhoCorasickNoPatterns 验证空匹配器行为。
func TestAhoCorasickNoPatterns(t *testing.T) {
	ac := NewAhoCorasick()
	ac.Build()
	if got := ac.FindAll("任意文本"); len(got) != 0 {
		t.Errorf("空匹配器应无命中: %v", got)
	}
	if got := ac.ExtractEntities("任意文本"); len(got) != 0 {
		t.Errorf("空匹配器实体应为空: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Personalized PageRank
// ---------------------------------------------------------------------------

// TestPageRankConvergesAndNormalizes 验证幂迭代收敛与归一化。
// 出处：core/storage/graph_store.py L1175-L1195。
func TestPageRankConvergesAndNormalizes(t *testing.T) {
	graph := NewGraph()
	graph.AddEdge("a", "b", 1)
	graph.AddEdge("b", "c", 1)
	graph.AddEdge("c", "a", 1)
	graph.AddEdge("c", "b", 1)

	ppr := NewPersonalizedPageRank(DefaultPageRankConfig())
	scores := ppr.Compute(graph, nil)
	total := 0.0
	for _, score := range scores {
		total += score
	}
	if math.Abs(total-1.0) > 1e-9 {
		t.Errorf("归一化总和: got %v, want 1.0", total)
	}
	iterations := ppr.LastIterations()
	if iterations >= DefaultPPRMaxIter {
		t.Errorf("应在 max_iter 内收敛: iterations=%d", iterations)
	}
	if iterations < DefaultPPRMinIterations-1 {
		t.Errorf("至少迭代 min_iterations: iterations=%d", iterations)
	}
	// 节点 c 有两条出边，其入链权重更高 → b（被 a、c 指向）应高于 a。
	if scores["b"] <= scores["a"] {
		t.Errorf("期望 b > a, got b=%v a=%v", scores["b"], scores["a"])
	}
}

// TestPageRankPersonalization 验证个性化向量影响力。
func TestPageRankPersonalization(t *testing.T) {
	graph := NewGraph()
	graph.AddEdge("seed", "near", 1)
	graph.AddEdge("far1", "far2", 1)
	graph.AddEdge("far2", "far1", 1)

	ppr := NewPersonalizedPageRank(DefaultPageRankConfig())
	scores := ppr.Compute(graph, map[string]float64{"seed": 100})
	ranked := RankNodes(scores, 0, 0)
	if len(ranked) == 0 {
		t.Fatal("expected nodes")
	}
	if ranked[0].To != "seed" {
		t.Errorf("个性化种子应排第一: got %v", ranked[0])
	}
	if scores["near"] <= scores["far1"] {
		t.Errorf("种子邻居应高于远离种子: near=%v far1=%v", scores["near"], scores["far1"])
	}
}

// TestPageRankDanglingNodes 验证悬挂节点的概率质量补偿。
func TestPageRankDanglingNodes(t *testing.T) {
	graph := NewGraph()
	graph.AddEdge("a", "b", 1)
	graph.AddNode("dangling")

	ppr := NewPersonalizedPageRank(DefaultPageRankConfig())
	scores := ppr.Compute(graph, map[string]float64{"a": 1})
	total := 0.0
	for _, score := range scores {
		total += score
	}
	if math.Abs(total-1.0) > 1e-9 {
		t.Errorf("悬挂节点场景总和: got %v, want 1.0", total)
	}
	if scores["a"] <= 0 || scores["b"] <= 0 {
		t.Errorf("a/b 应有正分: %+v", scores)
	}
}

// TestComputeLocalPPRRestrictsSubgraph 验证局部 PPR 只覆盖 BFS 子图。
// 出处：core/retrieval/dual_path.py L2812-L2898。
func TestComputeLocalPPRRestrictsSubgraph(t *testing.T) {
	graph := NewGraph()
	graph.AddEdge("seed", "hop1", 1)
	graph.AddEdge("hop1", "hop2", 1)
	graph.AddEdge("hop2", "hop3", 1)
	graph.AddEdge("hop3", "hop4", 1)
	graph.AddEdge("other", "island", 1)

	ppr := NewPersonalizedPageRank(DefaultPageRankConfig())
	scores := ppr.ComputeLocal(graph, map[string]float64{"seed": 1}, 64, 2)
	if len(scores) == 0 {
		t.Fatal("expected local scores")
	}
	if _, ok := scores["seed"]; !ok {
		t.Error("局部子图应包含种子")
	}
	if _, ok := scores["hop3"]; ok {
		t.Error("2 跳限制下不应包含 hop3")
	}
	if _, ok := scores["island"]; ok {
		t.Error("局部子图不应包含无关连通分量")
	}
	total := 0.0
	for _, score := range scores {
		total += score
	}
	if math.Abs(total-1.0) > 1e-9 {
		t.Errorf("局部 PPR 归一化: got %v, want 1.0", total)
	}
}

// TestFindPathsTwoHop 验证两跳路径召回。
// 出处：core/retrieval/graph_relation_recall.py L170-L179。
func TestFindPathsTwoHop(t *testing.T) {
	graph := NewGraph()
	graph.AddEdge("a", "mid", 1)
	graph.AddEdge("mid", "b", 1)
	graph.AddEdge("a", "direct", 1)
	paths := graph.FindPaths("a", "b", 2, 4)
	if len(paths) != 1 {
		t.Fatalf("paths: got %d, want 1 (%v)", len(paths), paths)
	}
	if len(paths[0]) != 3 || paths[0][0] != "a" || paths[0][2] != "b" {
		t.Errorf("path: %v", paths[0])
	}
	if got := graph.FindPaths("a", "b", 1, 4); len(got) != 0 {
		t.Errorf("1 跳不应找到 a→b: %v", got)
	}
}

// ---------------------------------------------------------------------------
// 融合
// ---------------------------------------------------------------------------

// TestFuseScoreMapsWeighted 验证三通道加权融合。
// 出处：core/retrieval/score_calibration.py L118-L177。
func TestFuseScoreMapsWeighted(t *testing.T) {
	maps := map[string]map[string]float64{
		"semantic": {"a": 0.9, "b": 0.1},
		"sparse":   {"a": 0.2, "c": 0.8},
		"graph":    {"b": 0.7},
	}
	weights := map[string]float64{"semantic": 0.65, "sparse": 0.20, "graph": 0.15}
	final, calibrated, err := FuseScoreMaps(maps, weights, "none", 60)
	if err != nil {
		t.Fatalf("fuse: %v", err)
	}
	wantA := 0.65*0.9 + 0.20*0.2
	if math.Abs(final["a"]-wantA) > 1e-12 {
		t.Errorf("a: got %v, want %v", final["a"], wantA)
	}
	wantB := 0.65*0.1 + 0.15*0.7
	if math.Abs(final["b"]-wantB) > 1e-12 {
		t.Errorf("b: got %v, want %v", final["b"], wantB)
	}
	if math.Abs(calibrated["semantic"]["a"]-0.9) > 1e-12 {
		t.Errorf("none 方法应保持原分: %v", calibrated["semantic"]["a"])
	}
}

// TestFuseScoreMapsWeightedRRF 验证 weighted_rrf 的 (k+1)/(k+rank) 校准。
// 出处：core/retrieval/score_calibration.py L143-L149。
func TestFuseScoreMapsWeightedRRF(t *testing.T) {
	maps := map[string]map[string]float64{
		"semantic": {"a": 1.0, "b": 0.5},
		"graph":    {"b": 0.9},
	}
	weights := map[string]float64{"semantic": 1.0, "graph": 1.0}
	final, calibrated, err := FuseScoreMaps(maps, weights, "weighted_rrf", 60)
	if err != nil {
		t.Fatalf("fuse: %v", err)
	}
	if math.Abs(calibrated["semantic"]["a"]-(61.0/61.0)) > 1e-12 {
		t.Errorf("rank1 RRF: got %v, want 1.0", calibrated["semantic"]["a"])
	}
	if math.Abs(calibrated["semantic"]["b"]-(61.0/62.0)) > 1e-12 {
		t.Errorf("rank2 RRF: got %v, want 61/62", calibrated["semantic"]["b"])
	}
	// b 在两个通道都出现 → 融合最高。
	if final["b"] <= final["a"] {
		t.Errorf("双通道候选应更高: a=%v b=%v", final["a"], final["b"])
	}
}

// TestCalibrateScoreMapMinMax 验证 min-max 校准与退化情形。
func TestCalibrateScoreMapMinMax(t *testing.T) {
	got, err := CalibrateScoreMap(map[string]float64{"a": 10, "b": 5, "c": 0}, "minmax")
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if got["a"] != 1.0 || got["c"] != 0.0 || math.Abs(got["b"]-0.5) > 1e-12 {
		t.Errorf("minmax: %+v", got)
	}
	flat, err := CalibrateScoreMap(map[string]float64{"a": 1, "b": 1}, "minmax")
	if err != nil {
		t.Fatalf("calibrate flat: %v", err)
	}
	if flat["a"] != 1.0 || flat["b"] != 1.0 {
		t.Errorf("同分应全为 1.0: %+v", flat)
	}
	if _, err := CalibrateScoreMap(map[string]float64{"a": 1}, "不存在的算法"); err == nil {
		t.Error("非法校准方法应报错")
	}
	if _, err := CalibrateScoreMap(map[string]float64{"a": 1}, "weighted_rrf"); err == nil {
		t.Error("RRF 方法不应走单通道校准")
	}
}

// TestSemanticRRFChannelSelection 验证 semantic_* 前缀只对 semantic 通道生效。
// 出处：core/retrieval/score_calibration.py L150-L158。
func TestSemanticRRFChannelSelection(t *testing.T) {
	maps := map[string]map[string]float64{
		"semantic": {"a": 10, "b": 0},
		"sparse":   {"a": 10, "b": 0},
	}
	_, calibrated, err := FuseScoreMaps(maps, map[string]float64{"semantic": 1, "sparse": 1}, "semantic_minmax", 60)
	if err != nil {
		t.Fatalf("fuse: %v", err)
	}
	// semantic 通道被 min-max 校准，sparse 通道保持原分。
	if calibrated["semantic"]["a"] != 1.0 {
		t.Errorf("semantic 应被校准: %v", calibrated["semantic"]["a"])
	}
	if calibrated["sparse"]["a"] != 10 {
		t.Errorf("sparse 应保持原分: %v", calibrated["sparse"]["a"])
	}
}

// TestFuseResultsAlphaWeighting 验证 _fuse_results 的 alpha 加权与关系去重。
// 出处：core/retrieval/dual_path.py L2510-L2590。
func TestFuseResultsAlphaWeighting(t *testing.T) {
	source := &stubSource{
		relations: map[string]*Relation{
			"r1": {Hash: "r1", Subject: "小明", Predicate: "喜欢", Object: "咖啡"},
		},
		paragraphsByRelation: map[string][]*Paragraph{
			"r1": {{Hash: "p1", Content: "小明 喜欢 咖啡"}},
		},
	}
	retriever := NewRetriever(source, nil, nil, nil, DefaultRetrieverConfig())
	alpha := 0.5
	fused, err := retriever.FuseResults(context.Background(),
		[]*Result{{Hash: "p1", Content: "小明 喜欢 咖啡", Score: 1.0, Type: "paragraph"}},
		[]*Result{{Hash: "r1", Content: "小明 喜欢 咖啡", Score: 1.0, Type: "relation"}},
		&alpha, 0)
	if err != nil {
		t.Fatalf("fuse: %v", err)
	}
	// 段落已入选 → 关系因支撑段落重复被去掉。
	if len(fused) != 1 || fused[0].Type != "paragraph" {
		t.Fatalf("去重后应只保留段落: %+v", fused)
	}
	if math.Abs(fused[0].Score-0.5) > 1e-12 {
		t.Errorf("alpha 加权: got %v, want 0.5", fused[0].Score)
	}
	if fused[0].Source != "fusion" {
		t.Errorf("source: got %s, want fusion", fused[0].Source)
	}
}

// TestFuseResultsPreserveTopRelations 验证 preserve_top_relations 豁免。
func TestFuseResultsPreserveTopRelations(t *testing.T) {
	source := &stubSource{
		paragraphsByRelation: map[string][]*Paragraph{
			"r1": {{Hash: "p1", Content: "内容"}},
		},
	}
	retriever := NewRetriever(source, nil, nil, nil, DefaultRetrieverConfig())
	alpha := 0.5
	fused, err := retriever.FuseResults(context.Background(),
		[]*Result{{Hash: "p1", Content: "内容", Score: 1.0, Type: "paragraph"}},
		[]*Result{{Hash: "r1", Content: "内容", Score: 1.0, Type: "relation"}},
		&alpha, 3)
	if err != nil {
		t.Fatalf("fuse: %v", err)
	}
	if len(fused) != 2 {
		t.Fatalf("preserve_top_relations 应保留关系: %+v", fused)
	}
}

// TestMergeRelationResultsGraphEnhanced 验证三路关系合并权重 0.60/0.30/0.10。
// 出处：core/retrieval/dual_path.py L1005-L1079。
func TestMergeRelationResultsGraphEnhanced(t *testing.T) {
	source := &stubSource{}
	retriever := NewRetriever(source, nil, nil, nil, DefaultRetrieverConfig())
	vector := []*Result{{Hash: "r1", Content: "a b c", Score: 0.8, Type: "relation", Source: "relation_search"}}
	graph := []*Result{{
		Hash: "r1", Content: "a b c", Score: 0.0, Type: "relation", Source: "graph_relation_recall",
		Metadata: map[string]any{
			"graph_candidate_type":       "direct_pair",
			"supporting_paragraph_count": 3,
		},
	}}
	merged := retriever.MergeRelationResultsGraphEnhanced(context.Background(), vector, nil, graph)
	if len(merged) != 1 {
		t.Fatalf("merged: %+v", merged)
	}
	// semantic_norm=1.0（单元素 min-max）, graph=1.0, evidence=min(1, 3/3)=1.0
	want := RelationMergeSemanticWeight + RelationMergeGraphWeight + RelationMergeEvidenceWeight
	if math.Abs(merged[0].Score-want) > 1e-9 {
		t.Errorf("merged score: got %v, want %v", merged[0].Score, want)
	}
	if merged[0].Source != "relation_fusion" {
		t.Errorf("source: got %s, want relation_fusion", merged[0].Source)
	}
}

// TestGraphScoreForType 验证候选类型权重映射。
// 出处：core/retrieval/dual_path.py L1015-L1019。
func TestGraphScoreForType(t *testing.T) {
	cases := map[string]float64{
		"direct_pair":  GraphScoreDirectPair,
		"one_hop_seed": GraphScoreOneHopSeed,
		"two_hop_pair": GraphScoreTwoHopPair,
		"unknown":      GraphScoreUnknownType,
	}
	for name, want := range cases {
		if got := GraphScoreForType(name); got != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
}

// TestGraphRecallServiceCandidates 验证图关系召回的三类候选构造。
// 出处：core/retrieval/graph_relation_recall.py L73-L285。
func TestGraphRecallServiceCandidates(t *testing.T) {
	graph := NewGraph()
	graph.AddEdge("小明", "咖啡", 1.0)
	graph.SetEdgeRelations("小明", "咖啡", []string{"r1"})
	graph.AddEdge("咖啡", "上海", 0.8)
	graph.SetEdgeRelations("咖啡", "上海", []string{"r2"})
	graph.Rev = 7

	source := &stubSource{
		graph: graph,
		entities: map[string]*Entity{
			"小明": {Hash: "e1", Name: "小明"},
			"上海": {Hash: "e2", Name: "上海"},
		},
		relations: map[string]*Relation{
			"r1": {Hash: "r1", Subject: "小明", Predicate: "喜欢", Object: "咖啡", Confidence: 0.9},
			"r2": {Hash: "r2", Subject: "咖啡", Predicate: "产自", Object: "上海", Confidence: 0.7},
		},
		paragraphsByRelation: map[string][]*Paragraph{
			"r1": {{Hash: "p1", Content: "小明 喜欢 咖啡"}},
			"r2": {{Hash: "p2", Content: "咖啡 产自 上海"}},
		},
	}
	service := NewGraphRecallService(source, DefaultGraphRecallConfig())

	// 单种子 → one_hop_seed。
	single, err := service.Recall(context.Background(), []string{"小明"}, nil)
	if err != nil {
		t.Fatalf("recall single: %v", err)
	}
	if len(single) != 1 || single[0].GraphCandidateType != "one_hop_seed" {
		t.Fatalf("one hop: %+v", single)
	}
	if single[0].SupportingParagraphCount != 1 || single[0].GraphHops != 1 {
		t.Errorf("candidate meta: %+v", single[0])
	}

	// 双种子无直连 → 两跳候选。
	pair, err := service.Recall(context.Background(), []string{"小明", "上海"}, nil)
	if err != nil {
		t.Fatalf("recall pair: %v", err)
	}
	types := map[string]bool{}
	for _, candidate := range pair {
		types[candidate.GraphCandidateType] = true
	}
	if !types["two_hop_pair"] {
		t.Errorf("应包含两跳候选: %+v", pair)
	}

	// 范围过滤：单种子的一跳邻接关系只有 r1。
	scoped, err := service.Recall(context.Background(), []string{"小明"}, map[string]struct{}{"r1": {}})
	if err != nil {
		t.Fatalf("recall scoped: %v", err)
	}
	if len(scoped) != 1 || scoped[0].Hash != "r1" {
		t.Errorf("scope filter: %+v", scoped)
	}
	// 范围里不含任何一跳关系 → 空（对齐 get_incident_relation_hashes 语义）。
	excluded, err := service.Recall(context.Background(), []string{"小明"}, map[string]struct{}{"r2": {}})
	if err != nil {
		t.Fatalf("recall excluded: %v", err)
	}
	if len(excluded) != 0 {
		t.Errorf("r2 不是小明的一跳关系: %+v", excluded)
	}
	empty, err := service.Recall(context.Background(), []string{"小明"}, map[string]struct{}{})
	if err != nil {
		t.Fatalf("recall empty scope: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("空 scope 应无候选: %+v", empty)
	}
}

// TestRetrieverRelationIntent 验证关系意图判定与 alpha 覆盖。
// 出处：core/retrieval/dual_path.py L479-L505。
func TestRetrieverRelationIntent(t *testing.T) {
	retriever := NewRetriever(nil, nil, nil, nil, DefaultRetrieverConfig())
	cases := map[string]bool{
		"小明和咖啡什么关系": true,
		"小明 | 咖啡":   true,
		"小明 -> 咖啡":  true,
		"有哪些关系":     true,
		"今天天气怎么样":   false,
	}
	for query, want := range cases {
		if got := retriever.IsRelationIntentQuery(query); got != want {
			t.Errorf("%q: got %v, want %v", query, got, want)
		}
	}
	ctx := retriever.buildRelationIntentContext("小明和咖啡什么关系", 10)
	if !ctx.Enabled {
		t.Fatal("应识别为关系意图")
	}
	if ctx.AlphaOverride == nil || *ctx.AlphaOverride != DefaultRelationIntentAlphaOverride {
		t.Errorf("alpha override: %v", ctx.AlphaOverride)
	}
	if ctx.PreserveTopRelations != DefaultPreserveTopRelations {
		t.Errorf("preserve_top_relations: %d", ctx.PreserveTopRelations)
	}
	if ctx.RelationTopK < DefaultTopKRelations*DefaultRelationCandidateMultiplier {
		t.Errorf("relation_top_k 应放大: %d", ctx.RelationTopK)
	}
}

// TestDualPoolWeightsRelationIntentSwitch 验证关系意图下的权重切换。
// 出处：core/retrieval/dual_path.py L1576-L1590。
func TestDualPoolWeightsRelationIntentSwitch(t *testing.T) {
	retriever := NewRetriever(nil, nil, nil, nil, DefaultRetrieverConfig())
	semantic, sparse, graph, topK := retriever.dualPoolWeights(RelationIntentContext{})
	if semantic != DefaultVectorPoolSemanticWeight || sparse != DefaultVectorPoolSparseWeight ||
		graph != DefaultVectorPoolGraphWeight || topK != DefaultGraphPoolTopK {
		t.Errorf("默认权重: %v %v %v %d", semantic, sparse, graph, topK)
	}
	semantic, sparse, graph, topK = retriever.dualPoolWeights(RelationIntentContext{Enabled: true})
	if semantic != DefaultRelationIntentSemanticWeight || sparse != DefaultRelationIntentSparseWeight ||
		graph != DefaultRelationIntentGraphWeight || topK != DefaultRelationIntentGraphTopK {
		t.Errorf("关系意图权重: %v %v %v %d", semantic, sparse, graph, topK)
	}
}

// TestTemporalScanCaps 验证时序候选上限（multiplier 8 / max_scan 1000）。
func TestTemporalScanCaps(t *testing.T) {
	options := DefaultTemporalQueryOptions()
	if options.CandidateMultiplier != DefaultTemporalCandidateMultiplier || options.MaxScan != DefaultTemporalMaxScan {
		t.Fatalf("defaults: %+v", options)
	}
	retriever := NewRetriever(nil, nil, nil, nil, DefaultRetrieverConfig())
	if got := retriever.expandTemporalCandidateK(20, &options); got != 160 {
		t.Errorf("expand: got %d, want 160", got)
	}
	if got := retriever.expandTemporalCandidateK(200, &options); got != DefaultTemporalMaxScan {
		t.Errorf("max_scan cap: got %d, want %d", got, DefaultTemporalMaxScan)
	}
	if got := retriever.expandTemporalCandidateK(20, nil); got != 20 {
		t.Errorf("无 temporal 时不变: got %d", got)
	}
}

// TestPosteriorGraphGateDisabledByDefault 验证后验图门默认关闭直通。
// 出处：core/retrieval/posterior_graph.py L639-L646。
func TestPosteriorGraphGateDisabledByDefault(t *testing.T) {
	retriever := NewRetriever(&stubSource{}, nil, nil, nil, DefaultRetrieverConfig())
	base := []*Result{{Hash: "p1", Score: 1.0, Type: "paragraph"}, {Hash: "p2", Score: 0.9, Type: "paragraph"}}
	out, err := retriever.ApplyPosteriorGraphGate(context.Background(), "查询", base, 1, nil, RelationIntentContext{}, nil)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if len(out) != 1 || out[0].Hash != "p1" {
		t.Errorf("默认关闭时应直接截断: %+v", out)
	}
}

// TestFindScoreCliff 验证断崖检测（drop_ratio 0.15 / min_core_results 2）。
// 出处：core/retrieval/posterior_graph.py L318-L344。
func TestFindScoreCliff(t *testing.T) {
	results := []*Result{
		{Score: 1.0}, {Score: 0.95}, {Score: 0.9}, {Score: 0.2}, {Score: 0.1},
	}
	cliff := findScoreCliff(results, DefaultPosteriorDropRatio, DefaultPosteriorMinCoreResults)
	if cliff != 3 {
		t.Errorf("cliff: got %d, want 3", cliff)
	}
	short := []*Result{{Score: 1.0}, {Score: 0.5}}
	if got := findScoreCliff(short, DefaultPosteriorDropRatio, DefaultPosteriorMinCoreResults); got != len(short) {
		t.Errorf("短列表应全保留: got %d", got)
	}
	if got := findScoreCliff(nil, DefaultPosteriorDropRatio, DefaultPosteriorMinCoreResults); got != 0 {
		t.Errorf("空列表应返回 0: got %d", got)
	}
}

// TestPeripheralDefaults 验证页面/稀疏/融合的默认常数装配。
func TestPeripheralDefaults(t *testing.T) {
	sparse := DefaultSparseConfig()
	if sparse.CandidateK != DefaultSparseCandidateK || sparse.RelationCandidateK != DefaultSparseRelationCandidateK {
		t.Errorf("sparse defaults: %+v", sparse)
	}
	if sparse.MaxDocLen != DefaultSparseMaxDocLen || sparse.RelationMaxDocLen != DefaultSparseRelationMaxDocLen {
		t.Errorf("sparse doc len: %+v", sparse)
	}
	fusion := DefaultFusionConfig()
	sum := fusion.VectorWeight + fusion.BM25Weight
	if math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("融合权重应归一: %v", sum)
	}
	if fusion.RRFK != DefaultFusionRRFK || fusion.Method != DefaultFusionMethod {
		t.Errorf("fusion defaults: %+v", fusion)
	}
	// 权重归一化：非 1 输入被归一。
	custom := FusionConfig{VectorWeight: 7, BM25Weight: 3}
	custom.normalize()
	if math.Abs(custom.VectorWeight-0.7) > 1e-12 || math.Abs(custom.BM25Weight-0.3) > 1e-12 {
		t.Errorf("归一化失败: %+v", custom)
	}
	// 全零回落到默认。
	zero := FusionConfig{}
	zero.normalize()
	if zero.VectorWeight != DefaultFusionVectorWeight || zero.BM25Weight != DefaultFusionBM25Weight {
		t.Errorf("零权重回退失败: %+v", zero)
	}
}

// TestDynamicThresholdFilter 验证自适应阈值过滤与 min_results 补齐。
// 出处：core/retrieval/threshold.py L111-L160。
func TestDynamicThresholdFilter(t *testing.T) {
	filter := NewDynamicThresholdFilter(DefaultThresholdConfig())
	results := []*Result{
		{Hash: "a", Score: 0.9}, {Hash: "b", Score: 0.85}, {Hash: "c", Score: 0.8},
		{Hash: "d", Score: 0.75}, {Hash: "e", Score: 0.4},
	}
	got := filter.Filter(results)
	if len(got) == 0 {
		t.Fatal("不应全部过滤掉")
	}
	if len(got) < DefaultThresholdMinResults {
		t.Errorf("至少保留 min_results=%d: got %d", DefaultThresholdMinResults, len(got))
	}
	for _, item := range got {
		if item.Score < DefaultThresholdMin {
			t.Errorf("低于 min_threshold 的结果应被过滤: %+v", item)
		}
	}
}

// TestTokenizerModes 验证三种分词模式都产出 token。
func TestTokenizerModes(t *testing.T) {
	for _, mode := range []string{"jieba", "mixed", "char_2gram"} {
		tokenizer := NewTokenizer(mode, 2)
		tokens := tokenizer.Tokenize("小明喜欢上海咖啡")
		if len(tokens) == 0 {
			t.Errorf("%s 模式分词为空", mode)
		}
	}
	// char_2gram 应产出二元组。
	tokens := NewTokenizer("char_2gram", 2).Tokenize("abcd")
	found := false
	for _, token := range tokens {
		if token == "ab" {
			found = true
		}
	}
	if !found {
		t.Errorf("char_2gram 应产出二元组: %v", tokens)
	}
	// 空文本。
	if got := NewTokenizer("jieba", 2).Tokenize("   "); len(got) != 0 {
		t.Errorf("空白文本应无 token: %v", got)
	}
}

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// stubSource 是 GraphSource 的内存实现。
type stubSource struct {
	graph                *Graph
	entities             map[string]*Entity
	relations            map[string]*Relation
	paragraphsByRelation map[string][]*Paragraph
	paragraphsByEntity   map[string][]*Paragraph
	paragraphs           map[string]*Paragraph
	temporal             []*Paragraph
}

func (s *stubSource) Graph(_ context.Context) (*Graph, error) {
	if s.graph == nil {
		return NewGraph(), nil
	}
	return s.graph, nil
}

func (s *stubSource) RelationByHash(_ context.Context, hash string) (*Relation, error) {
	return s.relations[hash], nil
}

func (s *stubSource) RelationsForEntity(_ context.Context, name string) ([]*Relation, error) {
	target := strings.ToLower(strings.TrimSpace(name))
	var out []*Relation
	for _, relation := range s.relations {
		if strings.ToLower(relation.Subject) == target || strings.ToLower(relation.Object) == target {
			out = append(out, relation)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out, nil
}

func (s *stubSource) RelationsByHashes(_ context.Context, hashes []string) (map[string]*Relation, error) {
	out := map[string]*Relation{}
	for _, hash := range hashes {
		if relation, ok := s.relations[hash]; ok {
			out[hash] = relation
		}
	}
	return out, nil
}

func (s *stubSource) ParagraphsByRelation(_ context.Context, hash string) ([]*Paragraph, error) {
	return s.paragraphsByRelation[hash], nil
}

func (s *stubSource) ParagraphsByEntity(_ context.Context, hash string) ([]*Paragraph, error) {
	return s.paragraphsByEntity[hash], nil
}

func (s *stubSource) ParagraphsByHashes(_ context.Context, hashes []string) (map[string]*Paragraph, error) {
	out := map[string]*Paragraph{}
	for _, hash := range hashes {
		if paragraph, ok := s.paragraphs[hash]; ok {
			out[hash] = paragraph
			continue
		}
		for _, list := range s.paragraphsByRelation {
			for _, paragraph := range list {
				if paragraph.Hash == hash {
					out[hash] = paragraph
				}
			}
		}
	}
	return out, nil
}

func (s *stubSource) EntitiesByHashes(_ context.Context, hashes []string) (map[string]*Entity, error) {
	out := map[string]*Entity{}
	for _, hash := range hashes {
		for _, entity := range s.entities {
			if entity.Hash == hash {
				out[hash] = entity
			}
		}
	}
	return out, nil
}

func (s *stubSource) EntityByName(_ context.Context, name string) (*Entity, error) {
	return s.entities[name], nil
}

func (s *stubSource) EntityNamesByParagraphs(_ context.Context, hashes []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, hash := range hashes {
		out[hash] = []string{"小明"}
	}
	return out, nil
}

func (s *stubSource) TemporalParagraphs(_ context.Context, _ TemporalQueryOptions, limit int) ([]*Paragraph, error) {
	if limit > 0 && len(s.temporal) > limit {
		return s.temporal[:limit], nil
	}
	return s.temporal, nil
}
