package learners

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// 向量数学：黄金向量钉子
// ---------------------------------------------------------------------------

func TestL2NormalizeGolden(t *testing.T) {
	got := L2Normalize([]float32{3, 4})
	if len(got) != 2 {
		t.Fatalf("维度应保持: %v", got)
	}
	if math.Abs(float64(got[0])-0.6) > 1e-6 || math.Abs(float64(got[1])-0.8) > 1e-6 {
		t.Fatalf("3-4-5 归一化黄金值不符: %v", got)
	}
	if norm := L2Normalize([]float32{0, 0}); norm != nil {
		t.Fatalf("零向量应返回 nil: %v", norm)
	}
	if norm := L2Normalize([]float32{float32(math.NaN()), 1}); norm != nil {
		t.Fatalf("NaN 应返回 nil: %v", norm)
	}
	if norm := L2Normalize([]float32{float32(math.Inf(1)), 1}); norm != nil {
		t.Fatalf("Inf 应返回 nil: %v", norm)
	}
	if norm := L2Normalize(nil); norm != nil {
		t.Fatalf("空向量应返回 nil: %v", norm)
	}
}

func TestCosineSimilarityGolden(t *testing.T) {
	cases := []struct {
		name     string
		left     []float32
		right    []float32
		expected float64
	}{
		{"同向", []float32{1, 0}, []float32{1, 0}, 1},
		{"正交", []float32{1, 0}, []float32{0, 1}, 0},
		{"反向", []float32{1, 0}, []float32{-1, 0}, -1},
		{"0.6/0.8", []float32{3, 4}, []float32{1, 0}, 0.6},
		{"维度不一致", []float32{1, 0}, []float32{1, 0, 0}, 0},
		{"零向量", []float32{0, 0}, []float32{1, 0}, 0},
		{"空向量", nil, nil, 0},
	}
	for _, testCase := range cases {
		got := CosineSimilarity(testCase.left, testCase.right)
		if math.Abs(got-testCase.expected) > 1e-6 {
			t.Fatalf("%s: got=%.9f want=%.9f", testCase.name, got, testCase.expected)
		}
	}
}

func TestDotProductDimensionMismatch(t *testing.T) {
	if got := DotProduct([]float32{1, 2}, []float32{1, 2, 3}); got != 0 {
		t.Fatalf("维度不一致应返回 0: %v", got)
	}
	if got := DotProduct([]float32{1, 2, 3}, []float32{1, 2, 3}); math.Abs(got-14) > 1e-9 {
		t.Fatalf("内积黄金值为 14: %.9f", got)
	}
}

// ---------------------------------------------------------------------------
// kmeans：数值稳定性 + 黄金标签
// ---------------------------------------------------------------------------

func TestCosineKMeansSeparatesTwoGoldenClusters(t *testing.T) {
	vectors := [][]float32{
		{1, 0, 0},
		{0.99, 0.05, 0},
		{0.98, 0.1, 0},
		{0, 1, 0},
		{0.05, 0.99, 0},
		{0.1, 0.98, 0},
	}
	labels, centers := CosineKMeans(vectors, KMeansParams{ClusterCount: 2, Seed: 20260621})
	if len(labels) != len(vectors) {
		t.Fatalf("标签数应等于样本数: %d", len(labels))
	}
	if len(centers) != 2 {
		t.Fatalf("应产生 2 个中心: %d", len(centers))
	}
	if labels[0] != labels[1] || labels[1] != labels[2] {
		t.Fatalf("第一簇应聚在一起: %v", labels)
	}
	if labels[3] != labels[4] || labels[4] != labels[5] {
		t.Fatalf("第二簇应聚在一起: %v", labels)
	}
	if labels[0] == labels[3] {
		t.Fatalf("两簇应分离: %v", labels)
	}
	// 中心必须归一化，且与簇内样本高相似。
	for index, center := range centers {
		norm := math.Sqrt(DotProduct(center, center))
		if math.Abs(norm-1) > 1e-5 {
			t.Fatalf("中心 %d 未归一化: norm=%.9f", index, norm)
		}
	}
	// 确定性：同一 seed 复用既有中心应得到同样标签。
	again, _ := CosineKMeans(vectors, KMeansParams{ClusterCount: 2, Seed: 20260621})
	for index := range labels {
		if labels[index] != again[index] {
			t.Fatalf("同 seed 结果应确定: %v vs %v", labels, again)
		}
	}
}

func TestCosineKMeansRepairsEmptyClusters(t *testing.T) {
	// 6 个几乎相同的向量 + 要求 3 个簇：必须每簇非空（空簇从成员充足的簇迁移）。
	vectors := [][]float32{
		{1, 0, 0}, {1, 0.001, 0}, {1, 0.002, 0},
		{0.999, 0.003, 0}, {0.998, 0.004, 0}, {0.997, 0.005, 0},
	}
	labels, centers := CosineKMeans(vectors, KMeansParams{ClusterCount: 3, Seed: 7})
	if len(centers) != 3 {
		t.Fatalf("应保持 3 个簇中心: %d", len(centers))
	}
	counts := map[int]int{}
	for _, label := range labels {
		counts[label]++
	}
	if len(counts) != 3 {
		t.Fatalf("每个簇都应有成员: %v", labels)
	}
}

func TestCosineKMeansSingleClusterAndSingleSample(t *testing.T) {
	labels, centers := CosineKMeans([][]float32{{3, 4}}, KMeansParams{ClusterCount: 3})
	if len(labels) != 1 || labels[0] != 0 {
		t.Fatalf("单样本应归簇 0: %v", labels)
	}
	if len(centers) != 1 || math.Abs(float64(centers[0][0])-0.6) > 1e-6 {
		t.Fatalf("单样本中心应为归一化样本: %v", centers)
	}

	labels, centers = CosineKMeans([][]float32{{1, 0}, {1, 0}, {1, 0}}, KMeansParams{ClusterCount: 1})
	if len(labels) != 3 || len(centers) != 1 {
		t.Fatalf("clusterCount=1 应全归簇 0: %v %v", labels, centers)
	}
}

func TestRepairEmptyClusterLabelsStable(t *testing.T) {
	labels := []int{0, 0, 0, 1, 1, 1}
	similarities := [][]float64{
		{0.9, 0.1}, {0.8, 0.2}, {0.7, 0.3}, {0.1, 0.9}, {0.2, 0.8}, {0.3, 0.7},
	}
	repaired := RepairEmptyClusterLabels(labels, similarities, 3)
	counts := map[int]int{}
	for _, label := range repaired {
		counts[label]++
	}
	if counts[0] == 0 || counts[1] == 0 || counts[2] == 0 {
		t.Fatalf("空簇应被修复: %v", repaired)
	}
	// 迁移的应是最不匹配原簇的样本（相似度 0.7 的下标 2）。
	if repaired[2] != 2 {
		t.Fatalf("应迁移最不匹配的样本到下标记 2: %v", repaired)
	}
}

func TestBuildClusterCentersMatchesMean(t *testing.T) {
	vectors := [][]float32{{1, 0}, {0, 1}}
	centers := BuildClusterCenters(vectors, []int{0, 1}, 2)
	if len(centers) != 2 {
		t.Fatalf("应产生 2 个中心: %d", len(centers))
	}
	// 簇 0 = 样本 0 → (1,0)；簇 1 = 样本 1 → (0,1)。
	if math.Abs(float64(centers[0][0])-1) > 1e-6 || math.Abs(float64(centers[0][1])) > 1e-6 {
		t.Fatalf("簇 0 中心应为 (1,0): %v", centers[0])
	}
	if math.Abs(float64(centers[1][0])) > 1e-6 || math.Abs(float64(centers[1][1])-1) > 1e-6 {
		t.Fatalf("簇 1 中心应为 (0,1): %v", centers[1])
	}
	if centers := BuildClusterCenters(vectors, []int{0, 0}, 2); centers != nil {
		t.Fatalf("空簇应返回 nil 触发重分配: %v", centers)
	}
}

func TestChooseClusterCountGolden(t *testing.T) {
	cases := []struct {
		samples  int
		previous int
		expected int
	}{
		{0, 0, 1},
		{1, 0, 1},
		{2, 0, 2},
		{100, 0, 80},
		{100, 20, 20},
		{10, 50, 10},
	}
	for _, testCase := range cases {
		if got := ChooseClusterCount(testCase.samples, testCase.previous); got != testCase.expected {
			t.Fatalf("ChooseClusterCount(%d,%d)=%d want=%d", testCase.samples, testCase.previous, got, testCase.expected)
		}
	}
}

// ---------------------------------------------------------------------------
// MMR：多样性采样
// ---------------------------------------------------------------------------

func TestSelectByMMRPrefersDiversity(t *testing.T) {
	candidates := []ScoredCandidate{
		{ID: 1, Score: 0.95},
		{ID: 2, Score: 0.94}, // 与 1 几乎重复
		{ID: 3, Score: 0.90}, // 与 1 完全不同的方向
	}
	vectors := map[int64][]float32{
		1: {1, 0},
		2: {1, 0.001},
		3: {0, 1},
	}
	selected := SelectByMMR(candidates, vectors, 2)
	if len(selected) != 2 {
		t.Fatalf("应选出 2 条: %d", len(selected))
	}
	if selected[0].ID != 1 {
		t.Fatalf("首条应为最高相关: %+v", selected[0])
	}
	if selected[1].ID != 3 {
		t.Fatalf("第二条应换成多样性更高的候选（MMR 惩罚近重复）: %+v", selected)
	}
}

func TestSelectByMMRHonoursLimitAndEmpty(t *testing.T) {
	if got := SelectByMMR(nil, nil, 3); got != nil {
		t.Fatalf("空候选应返回 nil: %v", got)
	}
	candidates := []ScoredCandidate{{ID: 1, Score: 0.5}}
	if got := SelectByMMR(candidates, map[int64][]float32{1: {1, 0}}, 0); got != nil {
		t.Fatalf("limit<=0 应返回 nil: %v", got)
	}
	got := SelectByMMR(candidates, map[int64][]float32{1: {1, 0}}, 5)
	if len(got) != 1 {
		t.Fatalf("limit 超出候选数应返回全部: %v", got)
	}
}

// ---------------------------------------------------------------------------
// 索引持久化与召回
// ---------------------------------------------------------------------------

// fakeEmbedder 是确定性假 embedding（字符 trigram 哈希投影）。
type fakeEmbedder struct{ dim int }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		out = append(out, f.encode(text))
	}
	return out, nil
}

func (f *fakeEmbedder) encode(text string) []float32 {
	vec := make([]float32, f.dim)
	runes := []rune(text)
	gram := 2
	if len(runes) < gram {
		gram = len(runes)
	}
	for index := 0; index+gram <= len(runes); index++ {
		var hash uint32 = 2166136261
		for _, char := range runes[index : index+gram] {
			hash ^= uint32(char)
			hash *= 16777619
		}
		vec[hash%uint32(f.dim)]++
	}
	normalized := L2Normalize(vec)
	if normalized == nil {
		vec[0] = 1
		return vec
	}
	return normalized
}

// dimEmbedder 对每个文本返回按首字符映射的单位向量（可精确钉死相似度）。
type dimEmbedder struct {
	dim int
}

func (d *dimEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		vec := make([]float32, d.dim)
		index := 0
		for _, char := range text {
			index = int(char) % d.dim
			break
		}
		vec[index] = 1
		out = append(out, vec)
	}
	return out, nil
}

func TestVectorIndexUpsertAndPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "expression_vector_index.json")
	index := NewExpressionVectorIndex(path)
	index.Embedder = (&fakeEmbedder{dim: 32}).Embed
	index.Profile = "profile-a"

	items := make([]UpsertItem, 0, 12)
	for position := 0; position < 12; position++ {
		items = append(items, UpsertItem{
			ID:        int64(position + 1),
			Situation: "情景" + itoaSmall(position),
			Style:     "风格" + itoaSmall(position),
			Count:     position + 1,
		})
	}
	reclustered, err := index.UpsertExpressions(context.Background(), items)
	if err != nil {
		t.Fatalf("写入索引失败: %v", err)
	}
	if !reclustered {
		t.Fatalf("首次写入应触发全量重聚类")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("索引文件应落盘: %v", err)
	}
	var payload persistedIndex
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("索引 JSON 应可解析: %v", err)
	}
	if payload.Version != VectorIndexVersion {
		t.Fatalf("版本应为 %d: %d", VectorIndexVersion, payload.Version)
	}
	if payload.ClusterState != ClusterStateStable {
		t.Fatalf("首次写入后应进入 STABLE: %q", payload.ClusterState)
	}
	if len(payload.Expressions) != 12 || len(payload.Vectors) != 12 {
		t.Fatalf("记录与向量数应一致: %d/%d", len(payload.Expressions), len(payload.Vectors))
	}
	if payload.Expressions[0].Fingerprint != ExpressionFingerprint(1, "情景0", "风格0") {
		t.Fatalf("指纹算法应照抄: %q", payload.Expressions[0].Fingerprint)
	}

	// 重启后（新实例）仍可读取并做召回。
	restarted := NewExpressionVectorIndex(path)
	restarted.Embedder = (&fakeEmbedder{dim: 32}).Embed
	restarted.Profile = "profile-a"
	snapshot, err := restarted.Snapshot()
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}
	if len(snapshot.Expressions) != 12 {
		t.Fatalf("快照应包含 12 条: %d", len(snapshot.Expressions))
	}

	scoped := make([]ScopedCandidate, 0, len(items))
	for _, item := range items {
		scoped = append(scoped, ScopedCandidate{ID: item.ID, Situation: item.Situation, Style: item.Style, Count: item.Count})
	}
	selected, err := restarted.SelectCandidates(context.Background(), "情景1\n风格1", scoped, 10, 16)
	if err != nil {
		t.Fatalf("召回失败: %v", err)
	}
	if len(selected) == 0 {
		t.Fatalf("召回不应为空")
	}
	if len(selected) > 10 {
		t.Fatalf("召回上限应生效: %d", len(selected))
	}
	for _, candidate := range selected {
		if candidate.ID == 0 || candidate.Situation == "" {
			t.Fatalf("召回项应带完整字段: %+v", candidate)
		}
		if candidate.Score != 0 {
			t.Fatalf("输出应清空 MMR 内部 score（对齐 Python 行为）: %+v", candidate)
		}
	}
}

func TestVectorIndexSelectCandidatesRequiresEnough(t *testing.T) {
	dir := t.TempDir()
	index := NewExpressionVectorIndex(filepath.Join(dir, "index.json"))
	index.Embedder = (&fakeEmbedder{dim: 16}).Embed
	items := make([]UpsertItem, 0, 5)
	scoped := make([]ScopedCandidate, 0, 5)
	for position := 0; position < 5; position++ {
		items = append(items, UpsertItem{ID: int64(position + 1), Situation: "情景" + itoaSmall(position), Style: "风格"})
		scoped = append(scoped, ScopedCandidate{ID: int64(position + 1), Situation: "情景" + itoaSmall(position), Style: "风格"})
	}
	if _, err := index.UpsertExpressions(context.Background(), items); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	selected, err := index.SelectCandidates(context.Background(), "情景0", scoped, 10, 16)
	if err != nil {
		t.Fatalf("候选不足不应报错: %v", err)
	}
	if selected != nil {
		t.Fatalf("候选 < 10 应跳过召回（对齐 Python）: %+v", selected)
	}
	if selected, err := index.SelectCandidates(context.Background(), "  ", scoped, 10, 16); err != nil || selected != nil {
		t.Fatalf("空 query 应跳过召回: %v %v", selected, err)
	}
}

func TestVectorIndexFingerprintMismatchFiltersCandidates(t *testing.T) {
	dir := t.TempDir()
	index := NewExpressionVectorIndex(filepath.Join(dir, "index.json"))
	index.Embedder = (&dimEmbedder{dim: 8}).Embed
	items := make([]UpsertItem, 0, 10)
	for position := 0; position < 10; position++ {
		items = append(items, UpsertItem{
			ID: int64(position + 1), Situation: "情景" + itoaSmall(position), Style: "风格" + itoaSmall(position),
		})
	}
	if _, err := index.UpsertExpressions(context.Background(), items); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	snapshot, err := index.Snapshot()
	if err != nil {
		t.Fatalf("快照失败: %v", err)
	}
	// 情景被改写 → 指纹不一致 → 该条被过滤。
	scoped := make([]ScopedCandidate, 0, 10)
	for position := 0; position < 10; position++ {
		situation := "情景" + itoaSmall(position)
		if position == 0 {
			situation = "被改写的场景"
		}
		scoped = append(scoped, ScopedCandidate{ID: int64(position + 1), Situation: situation, Style: "风格" + itoaSmall(position)})
	}
	filtered := FilterIndexedExpressions(snapshot, scoped, nil)
	if len(filtered) != 9 {
		t.Fatalf("指纹不一致的候选应被过滤: %d", len(filtered))
	}
	for _, expression := range filtered {
		if expression.ID == 1 {
			t.Fatalf("被改写的记录不应保留")
		}
	}
	// 作用域过滤回调可进一步裁剪。
	priority := func(candidate ScopedCandidate) bool { return candidate.Count >= 0 }
	if len(FilterIndexedExpressions(snapshot, scoped, priority)) != 9 {
		t.Fatalf("作用域回调应生效")
	}
	if len(FilterIndexedExpressions(snapshot, nil, nil)) != 0 {
		t.Fatalf("空候选应返回空")
	}
}

func TestVectorIndexIncrementalAssignsExistingClusters(t *testing.T) {
	dir := t.TempDir()
	index := NewExpressionVectorIndex(filepath.Join(dir, "index.json"))
	index.Embedder = (&dimEmbedder{dim: 8}).Embed
	// 40 条初始记录：1/41 ≈ 2.4% < FULL_RECLUSTER_CHANGE_RATIO(5%) → 走增量路径。
	initial := make([]UpsertItem, 0, 40)
	for position := 0; position < 40; position++ {
		initial = append(initial, UpsertItem{ID: int64(position + 1), Situation: "情景" + itoaSmall(position), Style: "风格"})
	}
	if _, err := index.UpsertExpressions(context.Background(), initial); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	payload, err := index.load()
	if err != nil {
		t.Fatalf("读取索引失败: %v", err)
	}
	clusterCount := payload.ClusterCount
	reclustered, err := index.UpsertExpressions(context.Background(), []UpsertItem{
		{ID: 99, Situation: "新情景", Style: "新风格"},
	})
	if err != nil {
		t.Fatalf("增量写入失败: %v", err)
	}
	if reclustered {
		t.Fatalf("变更比例低于阈值时应走增量分配")
	}
	updated, err := index.load()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if updated.ClusterCount != clusterCount {
		t.Fatalf("增量分配不应改变簇数量: %d → %d", clusterCount, updated.ClusterCount)
	}
	found := false
	for _, expression := range updated.Expressions {
		if expression.ID == 99 {
			found = true
			if expression.ClusterID < 0 || expression.ClusterID >= updated.ClusterCount {
				t.Fatalf("新记录应分配到已有簇: %d", expression.ClusterID)
			}
		}
	}
	if !found {
		t.Fatalf("新记录应写入索引")
	}
}

func TestVectorIndexReclustersOnChangeRatio(t *testing.T) {
	dir := t.TempDir()
	index := NewExpressionVectorIndex(filepath.Join(dir, "index.json"))
	index.Embedder = (&dimEmbedder{dim: 8}).Embed
	initial := make([]UpsertItem, 0, 12)
	for position := 0; position < 12; position++ {
		initial = append(initial, UpsertItem{ID: int64(position + 1), Situation: string(rune('a' + position)), Style: "风格"})
	}
	if _, err := index.UpsertExpressions(context.Background(), initial); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	// 3/12 = 25% ≥ 5% → 应全量重聚类。
	changed := []UpsertItem{}
	for position := 1; position <= 3; position++ {
		changed = append(changed, UpsertItem{ID: int64(position), Situation: "改写的场景" + itoaSmall(position), Style: "风格"})
	}
	reclustered, err := index.UpsertExpressions(context.Background(), changed)
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	if !reclustered {
		t.Fatalf("变更比例超阈值应触发全量重聚类")
	}
	payload, err := index.load()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if payload.ChangedSince != 0 {
		t.Fatalf("重聚类后应清零变更计数: %d", payload.ChangedSince)
	}
}

func TestNearestClusterPicksHighestCosine(t *testing.T) {
	centers := [][]float32{{1, 0}, {0, 1}}
	if got := NearestCluster([]float32{0.9, 0.1}, centers); got != 0 {
		t.Fatalf("应归到簇 0: %d", got)
	}
	if got := NearestCluster([]float32{0.1, 0.9}, centers); got != 1 {
		t.Fatalf("应归到簇 1: %d", got)
	}
	if got := NearestCluster([]float32{1, 1}, nil); got != 0 {
		t.Fatalf("无中心应返回 0: %d", got)
	}
}

func TestExpressionFingerprintAndEmbeddingText(t *testing.T) {
	fingerprint := ExpressionFingerprint(1, "  情景  一  ", "风格一")
	if fingerprint != ExpressionFingerprint(1, "情景 一", "风格一") {
		t.Fatalf("指纹应先压缩空白: %q", fingerprint)
	}
	if fingerprint == ExpressionFingerprint(2, "情景一", "风格一") {
		t.Fatalf("不同 id 指纹应不同")
	}
	if got := ExpressionEmbeddingText("情景", "风格"); got != "情景：情景\n风格：风格" {
		t.Fatalf("embedding 文本格式应照抄: %q", got)
	}
	if !strings.HasPrefix(fingerprint, "") || len(fingerprint) != 64 {
		t.Fatalf("指纹应为 sha256 hex: %q", fingerprint)
	}
}

// ---------------------------------------------------------------------------
// JSON 修复与风格规范化
// ---------------------------------------------------------------------------

func TestNormalizeExpressionStyleForLearning(t *testing.T) {
	cases := map[string]string{
		"使用 我嘞个xxxx":  "我嘞个xxxx",
		"对对对":         "对对对",
		"  使用   哈哈  ": "哈哈",
	}
	for input, expected := range cases {
		if got := NormalizeExpressionStyleForLearning(input); got != expected {
			t.Fatalf("NormalizeExpressionStyleForLearning(%q)=%q want=%q", input, got, expected)
		}
	}
	if !IsPromptExampleExpressionStyle("使用 对对对") {
		t.Fatalf("prompt 示例风格应被识别")
	}
	if IsPromptExampleExpressionStyle("今天天气不错") {
		t.Fatalf("正常风格不应被误判")
	}
}

func TestRepairJSONDocument(t *testing.T) {
	cases := map[string]string{
		"```json\n[{\"a\":1}]\n```":       `[{"a":1}]`,
		"前缀文字 {\"a\":1} 后缀":               `{"a":1}`,
		"[{\"a\":1}]":                     `[{"a":1}]`,
		"```\n{\"situation\":\"x\"}\n```": `{"situation":"x"}`,
		"no json here":                    "no json here",
	}
	for input, expected := range cases {
		if got := RepairJSONDocument(input); got != expected {
			t.Fatalf("RepairJSONDocument(%q)=%q want=%q", input, got, expected)
		}
	}
}

// ---------------------------------------------------------------------------
// 批次闸门并发
// ---------------------------------------------------------------------------

func TestExpressionGateSessionMutexAndGlobalLimit(t *testing.T) {
	gate := NewExpressionLearningBatchGate(3)
	first := gate.Acquire("s1")
	if !first.Acquired || first.ActiveCount != 1 || first.MaxCount != 3 {
		t.Fatalf("首次申请应成功: %+v", first)
	}
	busy := gate.Acquire("s1")
	if busy.Acquired || busy.Reason != GateSessionBusy {
		t.Fatalf("同会话应互斥: %+v", busy)
	}
	if second := gate.Acquire("s2"); !second.Acquired || second.ActiveCount != 2 {
		t.Fatalf("不同会话应放行: %+v", second)
	}
	if third := gate.Acquire("s3"); !third.Acquired || third.ActiveCount != 3 {
		t.Fatalf("第三个会话应放行: %+v", third)
	}
	limited := gate.Acquire("s4")
	if limited.Acquired || limited.Reason != GateGlobalLimit || limited.ActiveCount != 3 {
		t.Fatalf("全局上限应拦截: %+v", limited)
	}
	gate.Release("s1")
	if released := gate.Acquire("s4"); !released.Acquired {
		t.Fatalf("释放后应可申请: %+v", released)
	}
}

func TestExpressionGateDisabledWhenMaxZero(t *testing.T) {
	gate := NewExpressionLearningBatchGate(0)
	result := gate.Acquire("s1")
	if result.Acquired || result.Reason != GateMaxDisabled {
		t.Fatalf("max_expression_learner<=0 应拒绝: %+v", result)
	}
}

func TestExpressionGateConcurrentAcquireRespectsLimit(t *testing.T) {
	const maxCount = 3
	gate := NewExpressionLearningBatchGate(maxCount)
	var wg sync.WaitGroup
	var mu sync.Mutex
	acquired := 0
	peak := 0

	for workerIndex := 0; workerIndex < 24; workerIndex++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			sessionID := "session-" + itoaSmall(index%6)
			result := gate.Acquire(sessionID)
			if !result.Acquired {
				return
			}
			mu.Lock()
			acquired++
			if current := gate.ActiveCount(); current > peak {
				peak = current
			}
			mu.Unlock()
			gate.Release(sessionID)
		}(workerIndex)
	}
	wg.Wait()
	if acquired == 0 {
		t.Fatalf("并发申请应至少成功一次")
	}
	if peak > maxCount {
		t.Fatalf("峰值并发不应超过上限: %d > %d", peak, maxCount)
	}
	if gate.ActiveCount() != 0 {
		t.Fatalf("全部释放后活跃数应为 0: %d", gate.ActiveCount())
	}
	if gate.Acquired() != acquired || gate.Released() != acquired {
		t.Fatalf("申请/释放计数应配对: acquired=%d released=%d calls=%d", gate.Acquired(), gate.Released(), acquired)
	}
}

func TestJargonGateSameSemantics(t *testing.T) {
	gate := NewJargonLearningBatchGate(1)
	if !gate.Acquire("s1").Acquired {
		t.Fatalf("首次应成功")
	}
	blocked := gate.Acquire("s2")
	if blocked.Acquired || blocked.Reason != GateGlobalLimit {
		t.Fatalf("黑话闸门同样受全局上限约束: %+v", blocked)
	}
	if busy := gate.Acquire("s1"); busy.Reason != GateSessionBusy {
		t.Fatalf("同会话互斥优先: %+v", busy)
	}
	gate.Release("s1")
	if gate.ActiveCount() != 0 {
		t.Fatalf("释放后应为 0")
	}
	if disabled := NewJargonLearningBatchGate(0).Acquire("s1"); disabled.Reason != GateMaxDisabled {
		t.Fatalf("上限 0 应拒绝: %+v", disabled)
	}
}

func itoaSmall(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
