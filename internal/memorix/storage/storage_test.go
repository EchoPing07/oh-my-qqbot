package storage

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"oh-my-qqbot/internal/memorix/retrieval"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	options := DefaultOpenOptions()
	options.Path = filepath.Join(t.TempDir(), "a_memorix_test.db")
	store, err := Open(context.Background(), options)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestOpenCreatesSchemaAndFTS 验证建表与 FTS5 可用性。
func TestOpenCreatesSchemaAndFTS(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, table := range []string{
		"paragraphs", "entities", "relations", "episodes", "profiles", "profile_facts",
		"paragraph_links", "entity_links", "vectors", "paragraphs_fts", "relations_fts",
	} {
		var name string
		err := store.DB().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("表 %s 应存在: %v", table, err)
		}
	}
	if !store.HasFTS(ctx) {
		t.Fatal("FTS5 虚表不可用")
	}
}

// TestParagraphFTSRoundTrip 验证段落写入触发 FTS 索引并可 BM25 检索。
func TestParagraphFTSRoundTrip(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hash, err := store.AddParagraph(ctx, ParagraphInput{
		Content: "小明今天在上海喝了手冲咖啡",
		Source:  "chat_stream:g1",
		ChatID:  "g1",
	})
	if err != nil {
		t.Fatalf("add paragraph: %v", err)
	}
	if _, err := store.AddParagraph(ctx, ParagraphInput{
		Content: "今天天气不错",
		Source:  "chat_stream:g1",
		ChatID:  "g1",
	}); err != nil {
		t.Fatalf("add paragraph 2: %v", err)
	}

	rows, err := store.SearchParagraphsFTS(ctx, `"咖啡"`, 10, nil)
	if err != nil {
		t.Fatalf("fts search: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("fts hits: got %d, want 1 (%+v)", len(rows), rows)
	}
	if rows[0].Hash != hash {
		t.Errorf("hash: got %s, want %s", rows[0].Hash, hash)
	}
	// FTS5 bm25() 返回负值，越小越相关。
	if rows[0].BM25Score >= 0 {
		t.Errorf("bm25 分数应为负: %v", rows[0].BM25Score)
	}
	if rows[0].Content == "" {
		t.Error("FTS 结果应带回正文")
	}

	// 范围过滤生效。
	scoped, err := store.SearchParagraphsFTS(ctx, `"咖啡"`, 10, []string{"不存在的-hash"})
	if err != nil {
		t.Fatalf("fts scoped: %v", err)
	}
	if len(scoped) != 0 {
		t.Errorf("scope 过滤失败: %+v", scoped)
	}
}

// TestRelationFTSRoundTrip 验证关系写入触发 relations_fts 并可按三元组检索。
func TestRelationFTSRoundTrip(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hash, _, err := store.UpsertRelation(ctx, "小明", "喜欢", "咖啡", 1.0, "", nil)
	if err != nil {
		t.Fatalf("upsert relation: %v", err)
	}
	rows, err := store.SearchRelationsFTS(ctx, `"咖啡"`, 10, nil)
	if err != nil {
		t.Fatalf("fts relation search: %v", err)
	}
	if len(rows) != 1 || rows[0].Hash != hash {
		t.Fatalf("relation fts: %+v", rows)
	}
	if rows[0].Subject != "小明" || rows[0].Object != "咖啡" {
		t.Errorf("关系字段: %+v", rows[0])
	}
	if rows[0].Content != "小明 喜欢 咖啡" {
		t.Errorf("content 渲染: %q", rows[0].Content)
	}
}

// TestRelationHashCanonicalization 验证关系去重 hash（小写 + 去空格）。
// 出处：core/storage/metadata_store.py L595-L609。
func TestRelationHashCanonicalization(t *testing.T) {
	a, err := ComputeRelationHash("  小明 ", "喜欢", "Coffee")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	b, err := ComputeRelationHash("小明", "喜欢", "coffee")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if a != b {
		t.Errorf("规范化后 hash 应相同: %s vs %s", a, b)
	}
	if _, err := ComputeRelationHash("", "喜欢", "咖啡"); err == nil {
		t.Error("空 subject 应报错")
	}
	// 同 hash 幂等写入不产生重复行。
	store := openTestStore(t)
	ctx := context.Background()
	if _, _, err := store.UpsertRelation(ctx, "小明", "喜欢", "咖啡", 1.0, "p1", nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, _, err := store.UpsertRelation(ctx, "  小明  ", "喜欢", "咖啡", 0.8, "p2", nil); err != nil {
		t.Fatalf("upsert dup: %v", err)
	}
	rows, err := store.queryRelations(ctx, "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("应只有一条关系: %d", len(rows))
	}
}

// TestParagraphHashNormalize 验证段落 hash 的文本规范化。
// 出处：core/utils/hash.py L29-L63。
func TestParagraphHashNormalize(t *testing.T) {
	a := ComputeParagraphHash("  小明  喜欢\n咖啡  ")
	b := ComputeParagraphHash("小明 喜欢\n咖啡")
	if a != b {
		t.Errorf("规范化后应相同: %s vs %s", a, b)
	}
	if got := NormalizeText("a\r\nb\rc\t\td"); got != "a\nb\nc d" {
		t.Errorf("normalize: got %q", got)
	}
	if ComputeEntityHash("  MaiBot ") != ComputeEntityHash("maibot") {
		t.Error("实体 hash 应小写化")
	}
}

// TestVectorTableRoundTrip 验证向量 BLOB 编解码与内存索引重建。
func TestVectorTableRoundTrip(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	vec := []float32{0.1, 0.2, 0.3, 0.4}
	if err := store.UpsertVector(ctx, "paragraph", "p1", vec); err != nil {
		t.Fatalf("upsert vector: %v", err)
	}
	if store.Dimension() != 4 {
		t.Errorf("dimension: got %d, want 4", store.Dimension())
	}

	// BLOB 编解码保真。
	blob := EncodeVectorForTest(vec)
	decoded := DecodeVectorForTest(blob, 4)
	for i := range vec {
		if math.Abs(float64(decoded[i]-vec[i])) > 1e-6 {
			t.Errorf("decode[%d]: got %v, want %v", i, decoded[i], vec[i])
		}
	}

	// 维度不一致时拒绝写入。
	if err := store.UpsertVector(ctx, "paragraph", "p2", []float32{1, 2}); err == nil {
		t.Error("维度不匹配应报错")
	}

	// 重新打开后从 vectors 表重建索引。
	path := store.Path()
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(ctx, OpenOptions{Path: path, MaxReadConns: 4, BusyTimeoutMS: 3000, EnableFTS: true, QuantizationType: "float32"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Dimension() != 4 {
		t.Errorf("重建后维度: got %d, want 4", reopened.Dimension())
	}
	index := reopened.VectorIndexRef()
	if index == nil || index.Count() != 1 {
		t.Fatalf("重建后索引规模: %+v", index)
	}
	hits := reopened.SearchVectors("paragraph", []float32{0.1, 0.2, 0.3, 0.4}, 1, nil)
	if len(hits) != 1 || hits[0].ID != "p1" {
		t.Fatalf("重建后检索: %+v", hits)
	}
	if hits[0].Score < 0.999 {
		t.Errorf("同向量余弦应接近 1: %v", hits[0].Score)
	}
}

// TestVectorCosineRankingAndScope 验证余弦排序与 scope 过滤。
func TestVectorCosineRankingAndScope(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	// 归一化后内积 = 余弦。
	mustVector(t, store, "paragraph", "near", []float32{1, 0, 0})
	mustVector(t, store, "paragraph", "mid", []float32{0.7, 0.7, 0})
	mustVector(t, store, "paragraph", "far", []float32{0, 0, 1})

	hits := store.SearchVectors("paragraph", []float32{1, 0, 0}, 3, nil)
	if len(hits) != 3 {
		t.Fatalf("hits: %+v", hits)
	}
	if hits[0].ID != "near" || hits[1].ID != "mid" || hits[2].ID != "far" {
		t.Errorf("排序错误: %+v", hits)
	}
	if math.Abs(hits[0].Score-1.0) > 1e-6 {
		t.Errorf("near 余弦: got %v, want 1.0", hits[0].Score)
	}
	scoped := store.SearchVectors("paragraph", []float32{1, 0, 0}, 3, map[string]struct{}{"far": {}})
	if len(scoped) != 1 || scoped[0].ID != "far" {
		t.Errorf("scope 过滤: %+v", scoped)
	}
	if err := store.DeleteVector(ctx, "paragraph", "near"); err != nil {
		t.Fatalf("delete vector: %v", err)
	}
	after := store.SearchVectors("paragraph", []float32{1, 0, 0}, 3, nil)
	if len(after) != 2 {
		t.Errorf("删除后应为 2: %+v", after)
	}
}

func mustVector(t *testing.T, store *Store, itemType, id string, vec []float32) {
	t.Helper()
	if err := store.UpsertVector(context.Background(), itemType, id, vec); err != nil {
		t.Fatalf("upsert vector %s: %v", id, err)
	}
}

// TestLinkTablesAndRelations 验证段落↔实体/关系链接与回表查询。
func TestLinkTablesAndRelations(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	paragraphHash, err := store.AddParagraph(ctx, ParagraphInput{Content: "小明喜欢咖啡", ChatID: "g1"})
	if err != nil {
		t.Fatalf("add paragraph: %v", err)
	}
	entityHash, err := store.AddEntity(ctx, "小明")
	if err != nil {
		t.Fatalf("add entity: %v", err)
	}
	if err := store.LinkParagraphEntity(ctx, paragraphHash, entityHash); err != nil {
		t.Fatalf("link entity: %v", err)
	}
	relationHash, _, err := store.UpsertRelation(ctx, "小明", "喜欢", "咖啡", 0.9, paragraphHash, nil)
	if err != nil {
		t.Fatalf("upsert relation: %v", err)
	}
	if err := store.LinkParagraphRelation(ctx, paragraphHash, relationHash); err != nil {
		t.Fatalf("link relation: %v", err)
	}

	paragraphs, err := store.ParagraphsByEntity(ctx, entityHash)
	if err != nil || len(paragraphs) != 1 || paragraphs[0].Hash != paragraphHash {
		t.Fatalf("paragraphs by entity: %v %+v", err, paragraphs)
	}
	paragraphs, err = store.ParagraphsByRelation(ctx, relationHash)
	if err != nil || len(paragraphs) != 1 {
		t.Fatalf("paragraphs by relation: %v %+v", err, paragraphs)
	}
	names, err := store.EntityNamesByParagraphs(ctx, []string{paragraphHash})
	if err != nil || len(names[paragraphHash]) != 1 || names[paragraphHash][0] != "小明" {
		t.Fatalf("entity names by paragraph: %v %+v", err, names)
	}
	grouped, err := store.ParagraphHashesByRelationHashes(ctx, []string{relationHash})
	if err != nil || len(grouped[relationHash]) != 1 {
		t.Fatalf("paragraph hashes by relation: %v %+v", err, grouped)
	}
	entity, err := store.EntityByName(ctx, "  小明 ")
	if err != nil || entity == nil || entity.Hash != entityHash {
		t.Fatalf("entity by name: %v %+v", err, entity)
	}
	if entity.AppearanceCount != 1 {
		t.Errorf("appearance_count: %d", entity.AppearanceCount)
	}
}

// TestGraphBuildFromRelations 验证实体图由关系构建（节点小写 + 边上挂关系 hash）。
func TestGraphBuildFromRelations(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	paragraphHash, _ := store.AddParagraph(ctx, ParagraphInput{Content: "小明喜欢咖啡，咖啡产自上海"})
	r1, _, err := store.UpsertRelation(ctx, "小明", "喜欢", "咖啡", 0.9, paragraphHash, nil)
	if err != nil {
		t.Fatalf("relation 1: %v", err)
	}
	r2, _, err := store.UpsertRelation(ctx, "咖啡", "产自", "上海", 0.7, paragraphHash, nil)
	if err != nil {
		t.Fatalf("relation 2: %v", err)
	}

	graph, err := store.Graph(ctx)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if graph.NumNodes() < 3 {
		t.Errorf("节点数: got %d, want >= 3", graph.NumNodes())
	}
	if graph.FindNode("小明") != "小明" {
		t.Errorf("应能解析节点 小明: %q", graph.FindNode("小明"))
	}
	if graph.FindNode("不存在") != "" {
		t.Error("不存在的节点应返回空")
	}
	relations := graph.EdgeRelations("小明", "咖啡")
	if len(relations) != 1 || relations[0] != r1 {
		t.Errorf("边上关系 hash: %+v (want %s)", relations, r1)
	}
	incident := graph.IncidentRelationHashes("咖啡")
	set := map[string]bool{}
	for _, hash := range incident {
		set[hash] = true
	}
	if !set[r1] || !set[r2] {
		t.Errorf("咖啡 的邻接关系应包含 r1 与 r2: %+v", incident)
	}
	if edge := graph.WeightedNeighbors("小明"); len(edge) != 1 || math.Abs(edge[0].Weight-0.9) > 1e-9 {
		t.Errorf("边权重应为置信度: %+v", edge)
	}
}

// TestTemporalParagraphsQuery 验证时序查询（event_time_range 优先）。
func TestTemporalParagraphsQuery(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	old := float64(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()) / 1e9
	recent := float64(time.Now().Add(-time.Hour).UnixNano()) / 1e9
	if _, err := store.AddParagraph(ctx, ParagraphInput{Content: "很久以前的事", EventTime: &old, ChatID: "g1"}); err != nil {
		t.Fatalf("add old: %v", err)
	}
	if _, err := store.AddParagraph(ctx, ParagraphInput{Content: "刚刚发生的事", EventTime: &recent, ChatID: "g1"}); err != nil {
		t.Fatalf("add recent: %v", err)
	}

	from := float64(time.Now().Add(-24*time.Hour).UnixNano()) / 1e9
	options := retrieval.DefaultTemporalQueryOptions()
	options.TimeFrom = &from
	rows, err := store.TemporalParagraphs(ctx, options, 10)
	if err != nil {
		t.Fatalf("temporal query: %v", err)
	}
	if len(rows) != 1 || rows[0].Content != "刚刚发生的事" {
		t.Fatalf("temporal filter: %+v", rows)
	}

	// 无时间条件下 created_at 兜底。
	fallback := retrieval.DefaultTemporalQueryOptions()
	fallback.TimeFrom = &from
	hash, err := store.AddParagraph(ctx, ParagraphInput{Content: "无事件时间的段落"})
	if err != nil {
		t.Fatalf("add fallback: %v", err)
	}
	rows, err = store.TemporalParagraphs(ctx, fallback, 10)
	if err != nil {
		t.Fatalf("temporal fallback: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.Hash == hash {
			found = true
		}
	}
	if !found {
		t.Error("created_at 兜底未命中")
	}
}

// TestProfileFactsAndRelations 验证人物事实与画像关系读取。
func TestProfileFactsAndRelations(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.UpsertProfile(ctx, "u1", "小明", ""); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}
	if err := store.UpsertProfileFact(ctx, ProfileFact{
		PersonID: "u1", Category: "preference", Content: "喜欢喝手冲咖啡", Confidence: 0.8, Source: "chat",
	}); err != nil {
		t.Fatalf("upsert fact: %v", err)
	}
	// 同内容二次写入：置信度取大值，不重复。
	if err := store.UpsertProfileFact(ctx, ProfileFact{
		PersonID: "u1", Category: "preference", Content: "喜欢喝手冲咖啡", Confidence: 0.95, Source: "chat",
	}); err != nil {
		t.Fatalf("upsert fact dup: %v", err)
	}
	facts, err := store.ProfileFacts(ctx, "u1", 0)
	if err != nil {
		t.Fatalf("profile facts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts: %d, want 1", len(facts))
	}
	if math.Abs(facts[0].Confidence-0.95) > 1e-9 {
		t.Errorf("置信度应取大值: %v", facts[0].Confidence)
	}
	name, _, _, err := store.Profile(ctx, "u1")
	if err != nil || name != "小明" {
		t.Fatalf("profile name: %v %q", err, name)
	}
	if _, _, _, err := store.Profile(ctx, "不存在"); err != nil {
		t.Fatalf("不存在的 profile 应返回空: %v", err)
	}

	// 人物关系（实体名小写匹配）。
	paragraphHash, _ := store.AddParagraph(ctx, ParagraphInput{Content: "小明喜欢咖啡"})
	relationHash, _, err := store.UpsertRelation(ctx, "小明", "喜欢", "咖啡", 0.9, paragraphHash, nil)
	if err != nil {
		t.Fatalf("relation: %v", err)
	}
	relations, err := store.RelationsForPerson(ctx, "小明", 10)
	if err != nil {
		t.Fatalf("relations for person: %v", err)
	}
	if len(relations) != 1 || relations[0].Hash != relationHash {
		t.Fatalf("relations: %+v", relations)
	}
}

// TestEpisodeUpsertAndSearch 验证 episode 写入与词法检索。
func TestEpisodeUpsertAndSearch(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	id, err := store.UpsertEpisode(ctx, Episode{
		Source:       "chat_stream:g1",
		Title:        "咖啡讨论",
		Summary:      "小明和张三讨论了手冲咖啡的萃取时间",
		Participants: []string{"小明", "张三"},
		Keywords:     []string{"咖啡", "手冲"},
		EvidenceIDs:  []string{"p1", "p2"},
	})
	if err != nil {
		t.Fatalf("upsert episode: %v", err)
	}
	if id == "" {
		t.Fatal("episode id 不应为空")
	}
	episodes, err := store.SearchEpisodes(ctx, []string{"咖啡"}, 5)
	if err != nil {
		t.Fatalf("search episodes: %v", err)
	}
	if len(episodes) != 1 || episodes[0].ID != id {
		t.Fatalf("episodes: %+v", episodes)
	}
	if len(episodes[0].Participants) != 2 || episodes[0].ParagraphCount != 0 {
		t.Errorf("episode 字段: %+v", episodes[0])
	}
	ids, err := store.EpisodesByParagraph(ctx, "p1")
	if err != nil || len(ids) != 1 {
		t.Fatalf("episodes by paragraph: %v %+v", err, ids)
	}
	// title 命中权重高于 keywords（1.0 vs 0.6）。
	byTitle, err := store.SearchEpisodes(ctx, []string{"咖啡讨论"}, 5)
	if err != nil {
		t.Fatalf("search by title: %v", err)
	}
	if len(byTitle) != 1 {
		t.Errorf("title 命中: %+v", byTitle)
	}
}

// TestExternalRefsIdempotency 验证外部 id 幂等映射。
func TestExternalRefsIdempotency(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ref, err := store.GetExternalRef(ctx, "chat_stream:g1:hash")
	if err != nil {
		t.Fatalf("get ref: %v", err)
	}
	if ref != nil {
		t.Fatalf("未写入时应为 nil: %+v", ref)
	}
	if err := store.UpsertExternalRef(ctx, "chat_stream:g1:hash", "p1", "chat_stream", map[string]any{"chat_id": "g1"}); err != nil {
		t.Fatalf("upsert ref: %v", err)
	}
	ref, err = store.GetExternalRef(ctx, "chat_stream:g1:hash")
	if err != nil || ref == nil {
		t.Fatalf("get ref: %v %+v", err, ref)
	}
	if ref["paragraph_hash"] != "p1" || ref["source_type"] != "chat_stream" {
		t.Errorf("ref: %+v", ref)
	}
}

// TestStatsSnapshot 验证统计快照字段。
func TestStatsSnapshot(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddParagraph(ctx, ParagraphInput{Content: "内容"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	stats := store.StatsSnapshot(ctx)
	if stats["paragraphs"] != 1 {
		t.Errorf("paragraphs: %v", stats["paragraphs"])
	}
	if stats["fts_available"] != true {
		t.Errorf("fts_available: %v", stats["fts_available"])
	}
	if _, ok := stats["dimension"]; !ok {
		t.Error("缺少 dimension 字段")
	}
}
