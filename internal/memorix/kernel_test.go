package memorix

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/memorix/storage"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// fakeEmbedder 是确定性假向量生成器（不真调 LLM）。
// 采用字符 trigram 哈希投影：语义相近（共享子串）的文本自然获得更高余弦。
type fakeEmbedder struct {
	dim   int
	err   error
	calls int
	mu    sync.Mutex
}

func newFakeEmbedder(dim int) *fakeEmbedder { return &fakeEmbedder{dim: dim} }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		out = append(out, fakeVector(text, f.dim))
	}
	return out, nil
}

// fakeVector 把文本的字符 trigram 投影到 dim 维并做 L2 归一化。
func fakeVector(text string, dim int) []float32 {
	vec := make([]float32, dim)
	runes := []rune(strings.ToLower(text))
	if len(runes) == 0 {
		vec[0] = 1
		return vec
	}
	gram := 3
	if len(runes) < gram {
		gram = 1
	}
	for i := 0; i+gram <= len(runes); i++ {
		var hash uint32 = 2166136261
		for _, r := range runes[i : i+gram] {
			hash ^= uint32(r)
			hash *= 16777619
		}
		vec[hash%uint32(dim)] += 1
	}
	// 补全局字符频率（短文本也能区分）。
	for _, r := range runes {
		vec[(uint32(r)*2654435761)%uint32(dim)] += 0.35
	}
	norm := float64(0)
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		vec[0] = 1
		return vec
	}
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

// fakeChat 返回可编程的 LLM 响应。
type fakeChat struct {
	response string
	err      error
	prompts  []string
	mu       sync.Mutex
}

func (f *fakeChat) Chat(_ context.Context, _ string, req ChatRequest) (*ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(req.Messages) > 0 {
		f.prompts = append(f.prompts, req.Messages[len(req.Messages)-1].Content)
	}
	if f.err != nil {
		return nil, f.err
	}
	return &ChatResponse{Content: f.response}, nil
}

func newTestKernel(t *testing.T, chat ChatCompleter, embedder Embedder) *Kernel {
	t.Helper()
	options := DefaultOptions()
	options.DBPath = filepath.Join(t.TempDir(), "a_memorix.db")
	options.DataDir = ""
	kernel, err := New(context.Background(), options, chat, embedder)
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	t.Cleanup(func() { _ = kernel.Close() })
	return kernel
}

// ---------------------------------------------------------------------------
// JSON 修复
// ---------------------------------------------------------------------------

// TestParseLooseJSONCodeFence 验证 markdown 围栏剥离。
func TestParseLooseJSONCodeFence(t *testing.T) {
	raw := "好的，结果如下：\n```json\n{\"entities\":[\"小明\"],\"relations\":[{\"subject\":\"小明\",\"predicate\":\"喜欢\",\"object\":\"咖啡\"}]}\n```\n希望有帮助！"
	payload, err := ParseLooseJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	entities := toStringSlice(payload["entities"])
	if len(entities) != 1 || entities[0] != "小明" {
		t.Errorf("entities: %+v", entities)
	}
}

// TestParseLooseJSONTrailingCommaAndBareKeys 验证尾随逗号与裸键修复。
func TestParseLooseJSONTrailingCommaAndBareKeys(t *testing.T) {
	raw := `{
		entities: ["小明", "咖啡",],
		relations: [
			{subject: "小明", predicate: "喜欢", object: "咖啡"},
		],
	}`
	payload, err := ParseLooseJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := len(toStringSlice(payload["entities"])); got != 2 {
		t.Errorf("entities: %d, want 2", got)
	}
	relations := toMapSlice(payload["relations"])
	if len(relations) != 1 || relations[0]["subject"] != "小明" {
		t.Errorf("relations: %+v", relations)
	}
}

// TestParseLooseJSONPythonLiteralsAndFullWidth 验证 Python 字面量与全角标点修复。
func TestParseLooseJSONPythonLiteralsAndFullWidth(t *testing.T) {
	raw := `{"entities": ["小明"], "relations": [], "enabled": True, "note": None， "score": False}`
	payload, err := ParseLooseJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if payload["enabled"] != true {
		t.Errorf("True 应转 true: %v", payload["enabled"])
	}
	if payload["note"] != nil {
		t.Errorf("None 应转 null: %v", payload["note"])
	}
	if payload["score"] != false {
		t.Errorf("False 应转 false: %v", payload["score"])
	}
}

// TestParseLooseJSONSingleQuotesAndUnclosed 验证单引号与未闭合括号修复。
func TestParseLooseJSONSingleQuotesAndUnclosed(t *testing.T) {
	raw := `{'entities': ['小明', '咖啡'], 'relations': [{'subject': '小明', 'object': '咖啡'}`
	payload, err := ParseLooseJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := len(toStringSlice(payload["entities"])); got != 2 {
		t.Errorf("entities: %d, want 2", got)
	}
	relations := toMapSlice(payload["relations"])
	if len(relations) != 1 || relations[0]["object"] != "咖啡" {
		t.Errorf("relations: %+v", relations)
	}
}

// TestParseLooseJSONErrors 验证完全无法解析时返回错误。
func TestParseLooseJSONErrors(t *testing.T) {
	if _, err := ParseLooseJSON("这里没有任何 JSON"); err == nil {
		t.Error("无 JSON 应报错")
	}
	if _, err := ParseLooseJSON(""); err == nil {
		t.Error("空串应报错")
	}
}

// TestParseLooseJSONAnyArray 验证数组解析。
func TestParseLooseJSONAnyArray(t *testing.T) {
	value, err := ParseLooseJSONAny("- 结果：\n[{\"a\": 1}, {\"b\": 2},]")
	if err != nil {
		t.Fatalf("parse any: %v", err)
	}
	list, ok := value.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("array: %+v", value)
	}
}

// ---------------------------------------------------------------------------
// 关系去重
// ---------------------------------------------------------------------------

// TestRelationHashKeyDedupe 验证 relation hash 去重（小写 + 去空格）。
// 出处：core/utils/hash.py L86-L98 / metadata_store.py L595-L609。
func TestRelationHashKeyDedupe(t *testing.T) {
	a := RelationHashKey(" 小明 ", "喜欢", "Coffee")
	b := RelationHashKey("小明", "喜欢", "coffee")
	if a != b {
		t.Errorf("规范化后 hash 应相同: %s vs %s", a, b)
	}
	if a == RelationHashKey("小明", "讨厌", "coffee") {
		t.Error("不同谓词应不同 hash")
	}
	relations := []ExtractedRelation{
		{Subject: "小明", Predicate: "喜欢", Object: "咖啡", Confidence: 0.6},
		{Subject: "小明", Predicate: "喜欢", Object: "咖啡", Confidence: 0.9},
		{Subject: "小明", Predicate: "喜欢", Object: "茶", Confidence: 0.7},
		{Subject: "", Predicate: "喜欢", Object: "咖啡"},
	}
	deduped := DedupeRelations(relations)
	if len(deduped) != 2 {
		t.Fatalf("dedupe: got %d, want 2 (%+v)", len(deduped), deduped)
	}
	if deduped[0].Confidence != 0.9 {
		t.Errorf("置信度应取最大: %v", deduped[0].Confidence)
	}
}

// ---------------------------------------------------------------------------
// 抽取
// ---------------------------------------------------------------------------

// TestExtractFromTextWithLLM 验证 LLM 抽取路径。
func TestExtractFromTextWithLLM(t *testing.T) {
	chat := &fakeChat{response: `{"entities":["小明","咖啡"],"relations":[{"subject":"小明","predicate":"喜欢","object":"咖啡"}],"events":["小明喝咖啡"]}`}
	kernel := newTestKernel(t, chat, newFakeEmbedder(64))
	extraction, err := kernel.ExtractFromText(context.Background(), "小明喜欢咖啡")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(extraction.Entities) != 2 {
		t.Errorf("entities: %+v", extraction.Entities)
	}
	if len(extraction.Relations) != 1 || extraction.Relations[0].Object != "咖啡" {
		t.Errorf("relations: %+v", extraction.Relations)
	}
	if len(extraction.Events) != 1 {
		t.Errorf("events: %+v", extraction.Events)
	}
}

// TestExtractFromTextDegradesToRules 验证 LLM 失败时降级为规则抽取（不返回错误）。
func TestExtractFromTextDegradesToRules(t *testing.T) {
	chat := &fakeChat{err: errors.New("llm 挂了")}
	kernel := newTestKernel(t, chat, newFakeEmbedder(64))
	extraction, err := kernel.ExtractFromText(context.Background(), "他提到了“手冲咖啡”的萃取")
	if err != nil {
		t.Fatalf("extract should not fail: %v", err)
	}
	if len(extraction.Entities) == 0 {
		t.Error("规则降级应至少抽到引号内实体")
	}
}

// TestExtractFromTextInvalidJSONDegrades 验证返回不可解析 JSON 时降级。
func TestExtractFromTextInvalidJSONDegrades(t *testing.T) {
	chat := &fakeChat{response: "这不是 JSON"}
	kernel := newTestKernel(t, chat, newFakeEmbedder(64))
	extraction, err := kernel.ExtractFromText(context.Background(), "「测试实体」")
	if err != nil {
		t.Fatalf("extract should not fail: %v", err)
	}
	if len(extraction.Entities) == 0 {
		t.Error("降级应抽到实体")
	}
}

// TestRuleBasedExtractEntities 验证规则抽取的去重与限制。
func TestRuleBasedExtractEntities(t *testing.T) {
	entities := RuleBasedExtractEntities(`"小明" 和 #咖啡# 与 MaiBot 以及 “小明”`)
	set := map[string]bool{}
	for _, entity := range entities {
		if set[strings.ToLower(entity)] {
			t.Errorf("实体重复: %v", entities)
		}
		set[strings.ToLower(entity)] = true
	}
	if !set["小明"] || !set["咖啡"] {
		t.Errorf("缺少预期实体: %v", entities)
	}
	// 超长实体被过滤。
	long := RuleBasedExtractEntities(`"` + strings.Repeat("字", 40) + `"`)
	if len(long) != 0 {
		t.Errorf("超长实体应被过滤: %v", long)
	}
}

// ---------------------------------------------------------------------------
// 嵌入降级
// ---------------------------------------------------------------------------

// TestEmbeddingDegradedAllowsMetadataOnlyWrite 验证嵌入失败时 metadata-only 写入。
// 出处：core/runtime/sdk_memory_kernel.py L474-L480（_allow_metadata_only_write）。
func TestEmbeddingDegradedAllowsMetadataOnlyWrite(t *testing.T) {
	embedder := newFakeEmbedder(32)
	embedder.err = errors.New("embedding 服务不可用")
	kernel := newTestKernel(t, &fakeChat{}, embedder)

	ctx := context.Background()
	if err := kernel.IngestText(ctx, "g1", "小明喜欢咖啡", time.Now()); err != nil {
		t.Fatalf("降级写入不应报错: %v", err)
	}
	if !kernel.EmbeddingClient().Degraded() {
		t.Error("应标记为降级")
	}
	if !kernel.EmbeddingClient().AllowMetadataOnlyWrite() {
		t.Error("默认配置应允许 metadata-only 写入")
	}
	// 段落已落库（无向量）。
	stats := kernel.Stats(ctx)
	if stats["paragraphs"] != 1 {
		t.Errorf("段落应已写入: %v", stats)
	}
	if stats["vectors"] != 0 {
		t.Errorf("降级时不应有向量: %v", stats["vectors"])
	}
}

// TestEmbeddingDegradedRejectsWhenDisallowed 验证关闭 metadata-only 后写入失败。
func TestEmbeddingDegradedRejectsWhenDisallowed(t *testing.T) {
	embedder := newFakeEmbedder(32)
	embedder.err = errors.New("embedding 服务不可用")
	options := DefaultOptions()
	options.DBPath = filepath.Join(t.TempDir(), "a_memorix.db")
	options.DataDir = ""
	options.Embedding.AllowMetadataOnlyWrite = false
	kernel, err := New(context.Background(), options, &fakeChat{}, embedder)
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	defer kernel.Close()
	if err := kernel.IngestText(context.Background(), "g1", "小明喜欢咖啡", time.Now()); err == nil {
		t.Error("禁止 metadata-only 时写入应报错")
	}
}

// TestEmbeddingDimensionDetection 验证维度探测与不一致拒绝。
// 出处：core/embedding/api_adapter.py L37-L71、L220-L228。
func TestEmbeddingDimensionDetection(t *testing.T) {
	embedder := newFakeEmbedder(48)
	kernel := newTestKernel(t, &fakeChat{}, embedder)
	ctx := context.Background()
	if kernel.EmbeddingClient().Detected() {
		t.Error("写入前不应标记为已探测")
	}
	if err := kernel.IngestText(ctx, "g1", "第一条记忆", time.Now()); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !kernel.EmbeddingClient().Detected() {
		t.Error("写入后应完成维度探测")
	}
	if got := kernel.EmbeddingClient().Dimension(); got != 48 {
		t.Errorf("dimension: got %d, want 48", got)
	}
	if got := kernel.Store().Dimension(); got != 48 {
		t.Errorf("store dimension: got %d, want 48", got)
	}
	// 维度变化 → 向量表拒绝写入（对齐 stored_vector_dimension 校验）。
	if err := kernel.Store().UpsertVector(ctx, "paragraph", "bad", make([]float32, 16)); err == nil {
		t.Error("维度不一致应报错")
	}
}

// TestEmbeddingNilEmbedder 验证 nil embedder 的降级路径。
func TestEmbeddingNilEmbedder(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, nil)
	if err := kernel.IngestText(context.Background(), "g1", "内容", time.Now()); err != nil {
		t.Fatalf("nil embedder 应降级而非报错: %v", err)
	}
	if !kernel.EmbeddingClient().Degraded() {
		t.Error("应标记降级")
	}
}

// ---------------------------------------------------------------------------
// 内核端到端
// ---------------------------------------------------------------------------

// TestKernelInterfaceCompliance 编译期与运行期双重确认接口实现。
func TestKernelInterfaceCompliance(t *testing.T) {
	var kernel contracts.MemoryKernel = newTestKernel(t, &fakeChat{}, newFakeEmbedder(32))
	if kernel == nil {
		t.Fatal("kernel 不应为 nil")
	}
}

// TestIngestAndSemanticSearch 验证写入后语义检索能命中。
func TestIngestAndSemanticSearch(t *testing.T) {
	chat := &fakeChat{response: `{"entities":["小明","咖啡"],"relations":[{"subject":"小明","predicate":"喜欢","object":"咖啡"}]}`}
	kernel := newTestKernel(t, chat, newFakeEmbedder(128))
	ctx := context.Background()
	now := time.Now()

	if err := kernel.IngestText(ctx, "g1", "小明非常喜欢喝手冲咖啡，每天下午都要来一杯", now); err != nil {
		t.Fatalf("ingest 1: %v", err)
	}
	if err := kernel.IngestText(ctx, "g1", "今天下午的天气预报说有暴雨", now.Add(time.Minute)); err != nil {
		t.Fatalf("ingest 2: %v", err)
	}

	hits, err := kernel.SearchMemory(ctx, "search", "小明喜欢喝手冲咖啡", "g1", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("语义检索应有命中")
	}
	found := false
	for _, hit := range hits {
		if strings.Contains(hit.Text, "手冲咖啡") {
			found = true
		}
		if hit.Kind != "paragraph" && hit.Kind != "relation" {
			t.Errorf("意外的命中类型: %s", hit.Kind)
		}
	}
	if !found {
		t.Errorf("应命中咖啡段落: %+v", hits)
	}
	if hits[0].Score <= 0 {
		t.Errorf("分数应为正: %v", hits[0].Score)
	}
}

// TestIngestIsIdempotent 验证同文本重复写入幂等。
func TestIngestIsIdempotent(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(32))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := kernel.IngestSummary(ctx, "g1", "今天群里聊了咖啡", time.Now()); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
	stats := kernel.Stats(ctx)
	if stats["paragraphs"] != 1 {
		t.Errorf("幂等失败: paragraphs=%v", stats["paragraphs"])
	}
}

// TestIngestEmptyText 验证空文本不写入。
func TestIngestEmptyText(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(32))
	ctx := context.Background()
	if err := kernel.IngestText(ctx, "g1", "   ", time.Now()); err != nil {
		t.Fatalf("空文本: %v", err)
	}
	if err := kernel.IngestSummary(ctx, "g1", "", time.Now()); err != nil {
		t.Fatalf("空摘要: %v", err)
	}
	if stats := kernel.Stats(ctx); stats["paragraphs"] != 0 {
		t.Errorf("不应写入段落: %v", stats)
	}
}

// TestSearchTimeMode 验证 time 模式按时间窗命中。
func TestSearchTimeMode(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(64))
	ctx := context.Background()
	old := time.Now().Add(-72 * time.Hour)
	recent := time.Now().Add(-time.Minute)
	if err := kernel.IngestText(ctx, "g1", "三天前讨论过的话题内容", old); err != nil {
		t.Fatalf("ingest old: %v", err)
	}
	if err := kernel.IngestText(ctx, "g1", "刚刚发生的对话内容", recent); err != nil {
		t.Fatalf("ingest recent: %v", err)
	}
	hits, err := kernel.SearchMemory(ctx, "time", "", "g1", 10)
	if err != nil {
		t.Fatalf("time search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("time 模式应有命中")
	}
	for _, hit := range hits {
		if !hit.Time.IsZero() && hit.Time.Before(time.Now().Add(-24*time.Hour)) {
			t.Errorf("time 模式不应返回 24h 前的记忆: %+v", hit)
		}
	}
}

// TestSearchHybridMode 验证 hybrid 模式的相对时间解析。
func TestSearchHybridMode(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(64))
	ctx := context.Background()
	if err := kernel.IngestText(ctx, "g1", "今天讨论了咖啡的萃取时间", time.Now()); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	hits, err := kernel.SearchMemory(ctx, "hybrid", "今天的咖啡讨论", "g1", 5)
	if err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	found := false
	for _, hit := range hits {
		if strings.Contains(hit.Text, "咖啡") {
			found = true
		}
	}
	if !found {
		t.Errorf("hybrid 应命中今天的咖啡记忆: %+v", hits)
	}
}

// TestSearchEpisodeMode 验证 episode 模式。
func TestSearchEpisodeMode(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(32))
	ctx := context.Background()
	if _, err := kernel.Store().UpsertEpisode(ctx, storage.Episode{
		Source:       "chat_stream:g1",
		Title:        "咖啡讨论",
		Summary:      "小明和张三讨论了手冲咖啡的萃取时间",
		Participants: []string{"小明", "张三"},
		Keywords:     []string{"咖啡", "手冲"},
	}); err != nil {
		t.Fatalf("upsert episode: %v", err)
	}
	hits, err := kernel.SearchMemory(ctx, "episode", "咖啡", "g1", 5)
	if err != nil {
		t.Fatalf("episode search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("episode hits: %+v", hits)
	}
	if hits[0].Kind != "episode" || hits[0].Text == "" {
		t.Errorf("episode hit: %+v", hits[0])
	}
}

// TestSearchAggregateMode 验证 aggregate 模式三路 RRF 混合。
func TestSearchAggregateMode(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(64))
	ctx := context.Background()
	if err := kernel.IngestText(ctx, "g1", "今天讨论了咖啡的萃取时间", time.Now()); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := kernel.Store().UpsertEpisode(ctx, storage.Episode{
		Source:   "chat_stream:g1",
		Title:    "咖啡讨论",
		Summary:  "小明和张三讨论了手冲咖啡的萃取时间",
		Keywords: []string{"咖啡"},
	}); err != nil {
		t.Fatalf("upsert episode: %v", err)
	}
	hits, err := kernel.SearchMemory(ctx, "aggregate", "咖啡", "g1", 10)
	if err != nil {
		t.Fatalf("aggregate search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("aggregate 应有命中")
	}
	// 分数应降序。
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Errorf("分数未降序: %+v", hits)
		}
	}
	kinds := map[string]bool{}
	for _, hit := range hits {
		kinds[hit.Kind] = true
	}
	if !kinds["paragraph"] && !kinds["episode"] {
		t.Errorf("aggregate 应混合多分支: %+v", hits)
	}
}

// TestSearchInvalidMode 验证非法模式报错。
func TestSearchInvalidMode(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(32))
	if _, err := kernel.SearchMemory(context.Background(), "semantic", "查询", "g1", 5); err == nil {
		t.Error("非法模式应报错")
	}
}

// TestSearchTopKClamp 验证 top_k 裁剪到 [1, 50]。
// 出处：core/utils/search_execution_service.py L144-L158。
func TestSearchTopKClamp(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(32))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := kernel.IngestText(ctx, "g1", "记忆内容片段"+strings.Repeat("x", i+1), time.Now()); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	hits, err := kernel.SearchMemory(ctx, "search", "记忆内容", "g1", 100)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// 阈值过滤 + min_results 补齐后不应超过请求上限。
	if len(hits) > 50 {
		t.Errorf("top_k 未裁剪: %d", len(hits))
	}
}

// TestSearchChatScopeIsolation 验证会话范围隔离。
func TestSearchChatScopeIsolation(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(64))
	ctx := context.Background()
	if err := kernel.IngestText(ctx, "g1", "一群的咖啡讨论内容", time.Now()); err != nil {
		t.Fatalf("ingest g1: %v", err)
	}
	if err := kernel.IngestText(ctx, "g2", "二群的咖啡讨论内容", time.Now()); err != nil {
		t.Fatalf("ingest g2: %v", err)
	}
	hits, err := kernel.SearchMemory(ctx, "search", "咖啡讨论", "g1", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, hit := range hits {
		if strings.Contains(hit.Text, "二群") {
			t.Errorf("不应跨会话召回: %+v", hit)
		}
	}
}

// TestGetPersonProfile 验证人物画像聚合。
func TestGetPersonProfile(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(32))
	ctx := context.Background()
	if err := kernel.UpsertPersonFact(ctx, "u1", "小明", contracts.ProfileFact{
		Category: "preference", Content: "喜欢手冲咖啡", Confidence: 0.9, Source: "chat",
	}); err != nil {
		t.Fatalf("upsert fact 1: %v", err)
	}
	if err := kernel.UpsertPersonFact(ctx, "u1", "小明", contracts.ProfileFact{
		Category: "identity", Content: "是一名后端工程师", Confidence: 0.8,
	}); err != nil {
		t.Fatalf("upsert fact 2: %v", err)
	}
	profile, err := kernel.GetPersonProfile(ctx, "u1")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if profile.PersonID != "u1" || profile.Name != "小明" {
		t.Errorf("profile: %+v", profile)
	}
	if len(profile.Facts) != 2 {
		t.Errorf("facts: %+v", profile.Facts)
	}
	if profile.Summary == "" {
		t.Error("应有汇总摘要")
	}
	if profile.UpdatedAt.IsZero() {
		t.Error("updated_at 不应为零值")
	}
}

// TestGetPersonProfileNotFound 验证不存在的人物返回空画像而非错误。
func TestGetPersonProfileNotFound(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(32))
	profile, err := kernel.GetPersonProfile(context.Background(), "不存在的人")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if profile == nil || len(profile.Facts) != 0 {
		t.Errorf("应为空画像: %+v", profile)
	}
}

// TestGetPersonProfileEmptyID 验证空 person_id 报错。
func TestGetPersonProfileEmptyID(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(32))
	if _, err := kernel.GetPersonProfile(context.Background(), "  "); err == nil {
		t.Error("空 person_id 应报错")
	}
}

// TestUpsertPersonFactValidation 验证事实写入参数校验。
func TestUpsertPersonFactValidation(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(32))
	ctx := context.Background()
	if err := kernel.UpsertPersonFact(ctx, "", "小明", contracts.ProfileFact{Content: "x"}); err == nil {
		t.Error("空 person_id 应报错")
	}
	if err := kernel.UpsertPersonFact(ctx, "u1", "小明", contracts.ProfileFact{Content: "   "}); err == nil {
		t.Error("空内容应报错")
	}
}

// TestGetPersonProfileRelations 验证画像关系来自图谱。
func TestGetPersonProfileRelations(t *testing.T) {
	chat := &fakeChat{response: `{"entities":["小明","咖啡"],"relations":[{"subject":"小明","predicate":"喜欢","object":"咖啡"}]}`}
	kernel := newTestKernel(t, chat, newFakeEmbedder(64))
	ctx := context.Background()
	if err := kernel.IngestText(ctx, "g1", "小明喜欢咖啡", time.Now()); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := kernel.UpsertPersonFact(ctx, "u1", "小明", contracts.ProfileFact{
		Category: "identity", Content: "喜欢咖啡", Confidence: 1.0,
	}); err != nil {
		t.Fatalf("upsert fact: %v", err)
	}
	profile, err := kernel.GetPersonProfile(ctx, "u1")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if len(profile.Relations) == 0 {
		t.Fatalf("应包含图谱关系: %+v", profile)
	}
	if profile.Relations[0].TargetPersonID != "咖啡" {
		t.Errorf("关系目标: %+v", profile.Relations[0])
	}
}

// TestIngestImageMemory 验证图片记忆写入与幂等（hash 相同只写一次）。
func TestIngestImageMemory(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(64))
	ctx := context.Background()
	now := time.Now()
	if err := kernel.IngestImageMemory(ctx, "g1", "一张手冲咖啡的照片，画面里有滤杯和电子秤", "hash-1", now); err != nil {
		t.Fatalf("ingest image: %v", err)
	}
	// 相同 hash 重复投递应幂等。
	if err := kernel.IngestImageMemory(ctx, "g1", "一张手冲咖啡的照片，画面里有滤杯和电子秤", "hash-1", now); err != nil {
		t.Fatalf("ingest image dup: %v", err)
	}
	if err := kernel.IngestImageMemory(ctx, "g1", "另一张照片", "hash-2", now); err != nil {
		t.Fatalf("ingest image 2: %v", err)
	}
	stats := kernel.Stats(ctx)
	if stats["paragraphs"] != 2 {
		t.Errorf("图片记忆幂等失败: %v", stats["paragraphs"])
	}
	hits, err := kernel.SearchMemory(ctx, "search", "手冲咖啡 滤杯 电子秤", "g1", 5)
	if err != nil {
		t.Fatalf("search image memory: %v", err)
	}
	found := false
	for _, hit := range hits {
		if strings.Contains(hit.Text, "滤杯") {
			found = true
		}
	}
	if !found {
		t.Errorf("图片记忆应可检索: %+v", hits)
	}
}

// TestIngestImageMemoryEmptyDescription 验证空描述不写入。
func TestIngestImageMemoryEmptyDescription(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(32))
	if err := kernel.IngestImageMemory(context.Background(), "g1", "  ", "h", time.Now()); err != nil {
		t.Fatalf("空描述: %v", err)
	}
}

// TestKernelCloseIsIdempotent 验证 Close 幂等且写入被拒。
func TestKernelCloseIsIdempotent(t *testing.T) {
	options := DefaultOptions()
	options.DBPath = filepath.Join(t.TempDir(), "a_memorix.db")
	options.DataDir = ""
	kernel, err := New(context.Background(), options, &fakeChat{}, newFakeEmbedder(32))
	if err != nil {
		t.Fatalf("new kernel: %v", err)
	}
	if err := kernel.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if err := kernel.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}
	if err := kernel.IngestText(context.Background(), "g1", "关闭后写入", time.Now()); err == nil {
		t.Error("关闭后写入应报错")
	}
}

// TestIngestPopulatesEntitiesAndRelationsFromRules 验证无 LLM 时规则实体也入库。
func TestIngestPopulatesEntitiesAndRelationsFromRules(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(64))
	ctx := context.Background()
	if err := kernel.IngestText(ctx, "g1", "他提到「手冲咖啡」和「V60滤杯」", time.Now()); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	names, err := kernel.Store().EntityNames(ctx)
	if err != nil {
		t.Fatalf("entity names: %v", err)
	}
	found := false
	for _, name := range names {
		if name == "手冲咖啡" {
			found = true
		}
	}
	if !found {
		t.Errorf("规则实体应入库: %v", names)
	}
	if _, err := kernel.Store().Graph(ctx); err != nil {
		t.Fatalf("graph: %v", err)
	}
}

// TestParseRelativeTimeWindow 验证相对时间窗解析。
func TestParseRelativeTimeWindow(t *testing.T) {
	now := time.Date(2025, 6, 15, 15, 0, 0, 0, time.UTC)
	cases := map[string]bool{
		"今天的对话":    true,
		"昨天聊了什么":   true,
		"最近三天的记忆":  true,
		"毫无时间词的查询": false,
	}
	for query, want := range cases {
		_, _, ok := parseRelativeTimeWindow(query, now)
		if ok != want {
			t.Errorf("%q: got %v, want %v", query, ok, want)
		}
	}
	from, to, ok := parseRelativeTimeWindow("今天的对话", now)
	if !ok || from >= to {
		t.Fatalf("时间窗非法: %v %v", from, to)
	}
	// 今天 00:00 ~ 明天 00:00
	if float64(now.Truncate(24*time.Hour).UnixNano())/1e9 > from {
		t.Errorf("今天窗口起点应 <= 当日 00:00: %v", from)
	}
}

// TestKnowledgeTypeMapping 验证 source_type → knowledge_type 映射。
func TestKnowledgeTypeMapping(t *testing.T) {
	cases := map[string]string{
		"chat_summary": "chat_summary",
		"chat_stream":  "chat_log",
		"image_memory": "image",
		"person_fact":  "person_fact",
		"其他":           "mixed",
	}
	for sourceType, want := range cases {
		if got := knowledgeTypeFor(sourceType); got != want {
			t.Errorf("%s: got %s, want %s", sourceType, got, want)
		}
	}
}

// TestKernelConcurrentReadWrite 验证并发读写不会死锁或数据竞争
// （检索并发读向量索引/图缓存；写入串行化）。
func TestKernelConcurrentReadWrite(t *testing.T) {
	kernel := newTestKernel(t, &fakeChat{}, newFakeEmbedder(64))
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_ = kernel.IngestText(ctx, "g1",
				"并发写入的记忆内容 "+string(rune('a'+index))+" 咖啡 检索", time.Now())
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = kernel.SearchMemory(ctx, "search", "咖啡 检索", "g1", 5)
		}()
	}
	wg.Wait()
	stats := kernel.Stats(ctx)
	if stats["paragraphs"] == 0 {
		t.Errorf("并发写入后应有段落: %v", stats)
	}
}
