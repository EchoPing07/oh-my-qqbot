package retrieval

import (
	"math"
	"sort"
	"strings"
	"sync"
)

// PageRankConfig 对齐 Python PageRankConfig。
// 出处：core/retrieval/pagerank.py L20-L30。
type PageRankConfig struct {
	Alpha         float64
	MaxIter       int
	Tol           float64
	Normalize     bool
	MinIterations int
}

// DefaultPageRankConfig 返回照抄 Python 的默认配置。
func DefaultPageRankConfig() PageRankConfig {
	return PageRankConfig{
		Alpha:         DefaultPPRAlpha,
		MaxIter:       DefaultPPRMaxIter,
		Tol:           DefaultPPRTol,
		Normalize:     DefaultPPRNormalize,
		MinIterations: DefaultPPRMinIterations,
	}
}

// WeightedEdge 是一条带权有向边。
type WeightedEdge struct {
	To     string
	Weight float64
}

// Graph 是 PPR 需要的稀疏图视图：出边邻接表（列归一化前的原始权重）。
// 对齐 Python GraphStore._adjacency 的语义（行=源节点，列=目标节点）。
type Graph struct {
	// Out 是出边邻接表：节点 → [(目标, 权重)]。
	Out map[string][]WeightedEdge
	// In 是入边邻接表（用于两跳路径召回与局部子图收集）。
	In map[string][]WeightedEdge
	// Nodes 是全部节点（用于均匀随机跳转与结果归一化）。
	Nodes []string
	// nodeSet 是节点存在性索引（含只有入边或只有出边的节点）。
	nodeSet map[string]struct{}
	// Rev 用于缓存失效（对齐 Python 的 node_revision）。
	Rev int64
	// edgeRelations 记录每条有向边上挂载的关系 hash：A_memorix 的图是实体图，
	// 边由关系产生，图关系召回需要沿边取回关系 hash。
	edgeRelations map[string][]string
}

// NewGraph 创建空图。
func NewGraph() *Graph {
	return &Graph{
		Out:     map[string][]WeightedEdge{},
		In:      map[string][]WeightedEdge{},
		nodeSet: map[string]struct{}{},
	}
}

// AddEdge 添加一条有向边；重复边权重累加（对齐 dijkstra/csr 的 coo 累加语义）。
func (g *Graph) AddEdge(from, to string, weight float64) {
	if from == "" || to == "" || weight <= 0 {
		return
	}
	if g.nodeSet == nil {
		g.nodeSet = map[string]struct{}{}
	}
	for _, node := range []string{from, to} {
		if _, ok := g.nodeSet[node]; ok {
			continue
		}
		g.nodeSet[node] = struct{}{}
		g.Nodes = append(g.Nodes, node)
	}
	if _, ok := g.Out[from]; !ok {
		g.Out[from] = nil
	}
	g.Out[from] = appendWeighted(g.Out[from], to, weight)
	g.In[to] = appendWeighted(g.In[to], from, weight)
	g.Rev++
}

func appendWeighted(list []WeightedEdge, to string, weight float64) []WeightedEdge {
	for i := range list {
		if list[i].To == to {
			list[i].Weight += weight
			return list
		}
	}
	return append(list, WeightedEdge{To: to, Weight: weight})
}

// Neighbors 返回出邻居节点名（对齐 GraphStore.get_neighbors）。
func (g *Graph) Neighbors(node string) []string {
	edges := g.Out[node]
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, e.To)
	}
	return out
}

// InNeighbors 返回入邻居节点名（对齐 GraphStore.get_in_neighbors）。
func (g *Graph) InNeighbors(node string) []string {
	edges := g.In[node]
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, e.To)
	}
	return out
}

// WeightedNeighbors 返回出邻居及权重（对齐 GraphStore.get_weighted_neighbors）。
func (g *Graph) WeightedNeighbors(node string) []WeightedEdge {
	edges := g.Out[node]
	out := make([]WeightedEdge, len(edges))
	copy(out, edges)
	return out
}

// NumNodes 返回节点数。
func (g *Graph) NumNodes() int { return len(g.Nodes) }

// NumEdges 返回有向边数。
func (g *Graph) NumEdges() int {
	n := 0
	for _, edges := range g.Out {
		n += len(edges)
	}
	return n
}

// AddNode 显式登记一个孤立节点（无出边）。
func (g *Graph) AddNode(node string) {
	if node == "" {
		return
	}
	if g.nodeSet == nil {
		g.nodeSet = map[string]struct{}{}
	}
	if _, ok := g.nodeSet[node]; ok {
		return
	}
	g.nodeSet[node] = struct{}{}
	g.Nodes = append(g.Nodes, node)
	if _, ok := g.Out[node]; !ok {
		g.Out[node] = nil
	}
}

// RemoveEdge 删除 from→to 的边（以及对应边上的关系 hash）。
func (g *Graph) RemoveEdge(from, to string) {
	if g == nil {
		return
	}
	delete(g.edgeRelations, from+"\x00"+to)
	changed := false
	edges := g.Out[from]
	kept := edges[:0]
	for _, e := range edges {
		if e.To == to {
			changed = true
			continue
		}
		kept = append(kept, e)
	}
	if changed {
		g.Out[from] = kept
	}
}

// HasNode 判定节点是否存在。
func (g *Graph) HasNode(node string) bool {
	return g.FindNode(node) != ""
}

// FindNode 按精确名或规范化名（小写去空格）查找节点，未命中返回空串。
// 出处：core/storage/graph_store.py L780-L807。
func (g *Graph) FindNode(node string) string {
	if _, ok := g.nodeSet[node]; ok {
		return node
	}
	canon := canonicalNodeName(node)
	if canon == "" {
		return ""
	}
	if _, ok := g.nodeSet[canon]; ok {
		return canon
	}
	// 兼容仅通过 Out/In 手工构造的图（未走 AddEdge）。
	if _, ok := g.Out[node]; ok {
		return node
	}
	if _, ok := g.In[node]; ok {
		return node
	}
	return ""
}

// canonicalNodeName 对齐 GraphStore._canonicalize（strip + lower）。
// 出处：core/storage/graph_store.py L264-L276（_canonicalize_name）。
func canonicalNodeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// EdgeRelations 返回 from→to 边上挂载的关系 hash。
func (g *Graph) EdgeRelations(from, to string) []string {
	if g == nil {
		return nil
	}
	return g.edgeRelations[from+"\x00"+to]
}

// IncidentRelationHashes 返回节点一跳范围内（入边 + 出边）的全部关系 hash，
// 对齐 GraphStore.get_incident_relation_hashes。
// 出处：core/retrieval/graph_relation_recall.py L197-L216。
func (g *Graph) IncidentRelationHashes(node string) []string {
	if g == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, edge := range g.Out[node] {
		for _, h := range g.EdgeRelations(node, edge.To) {
			if _, ok := seen[h]; ok {
				continue
			}
			seen[h] = struct{}{}
			out = append(out, h)
		}
	}
	for _, edge := range g.In[node] {
		for _, h := range g.EdgeRelations(edge.To, node) {
			if _, ok := seen[h]; ok {
				continue
			}
			seen[h] = struct{}{}
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// PersonalizedPageRank 是稀疏幂迭代实现。
//
// 更新公式（对齐 core/storage/graph_store.py L1176-L1190 与
// core/retrieval/dual_path.py L2870-L2884）：
//
//	p_new = alpha * (M^T @ p) + (1 - alpha) * personalization
//	其中 M 为按出度列归一化的转移矩阵；悬挂节点的概率质量按 alpha * mass * p 补偿。
type PersonalizedPageRank struct {
	Config PageRankConfig

	// mu 保护 LastIterations（PPR 计算可被并发检索调用）。
	mu sync.Mutex
	// lastIterations 记录上次计算的迭代次数（统计用途，见 LastIterations）。
	lastIterations int
}

// LastIterations 返回上次计算的迭代次数。
func (p *PersonalizedPageRank) LastIterations() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastIterations
}

func (p *PersonalizedPageRank) setIterations(value int) {
	p.mu.Lock()
	p.lastIterations = value
	p.mu.Unlock()
}

// NewPersonalizedPageRank 创建 PPR 计算器。
func NewPersonalizedPageRank(config PageRankConfig) *PersonalizedPageRank {
	if config.Alpha < 0 || config.Alpha >= 1 {
		config.Alpha = DefaultPPRAlpha
	}
	if config.MaxIter <= 0 {
		config.MaxIter = DefaultPPRMaxIter
	}
	if config.Tol <= 0 {
		config.Tol = DefaultPPRTol
	}
	if config.MinIterations < 0 {
		config.MinIterations = 0
	}
	if config.MinIterations >= config.MaxIter {
		config.MinIterations = config.MaxIter - 1
		if config.MinIterations < 0 {
			config.MinIterations = 0
		}
	}
	return &PersonalizedPageRank{Config: config}
}

// Compute 计算全图 PPR；personalization 为空时退化为均匀分布。
// 返回值在 Normalize=true 时按总和归一到 1。
//
// 出处：core/retrieval/pagerank.py L106-L162（归一化）
// 与 core/storage/graph_store.py L1111-L1195（幂迭代）。
func (p *PersonalizedPageRank) Compute(g *Graph, personalization map[string]float64) map[string]float64 {
	if g == nil || len(g.Nodes) == 0 {
		return map[string]float64{}
	}
	scores, iterations := p.iterate(g, g.Nodes, nil, personalization, nil)
	p.setIterations(iterations)
	if p.Config.Normalize {
		total := 0.0
		for _, v := range scores {
			total += v
		}
		if total > 0 {
			for k := range scores {
				scores[k] /= total
			}
		}
	}
	return scores
}

// ComputeLocal 在种子节点的局部子图上计算 PPR。
// 对齐 core/retrieval/dual_path.py L2812-L2898：
// BFS 收集 maxNodes 个节点（出边 + 入边），maxHops 跳内。
func (p *PersonalizedPageRank) ComputeLocal(
	g *Graph,
	seeds map[string]float64,
	maxNodes int,
	maxHops int,
) map[string]float64 {
	if g == nil || len(seeds) == 0 {
		return map[string]float64{}
	}
	if maxNodes < 16 {
		maxNodes = 16
	}
	if maxHops < 1 {
		maxHops = 1
	}

	// 解析种子到真实节点名（累计权重）。
	resolved := map[string]float64{}
	order := make([]string, 0, len(seeds))
	for name, weight := range seeds {
		node := g.findNode(name)
		if node == "" || weight <= 0 {
			continue
		}
		if _, ok := resolved[node]; !ok {
			order = append(order, node)
		}
		resolved[node] += weight
	}
	if len(resolved) == 0 {
		return map[string]float64{}
	}
	// 保持确定性顺序。
	sort.Strings(order)

	type queued struct {
		node  string
		depth int
	}
	queue := make([]queued, 0, len(order))
	for _, name := range order {
		queue = append(queue, queued{node: name})
	}
	seen := map[string]struct{}{}
	nodes := make([]string, 0, maxNodes)
	for len(queue) > 0 && len(seen) < maxNodes {
		item := queue[0]
		queue = queue[1:]
		if _, ok := seen[item.node]; ok {
			continue
		}
		seen[item.node] = struct{}{}
		nodes = append(nodes, item.node)
		if item.depth >= maxHops {
			continue
		}
		neighbors := append([]string{}, g.Neighbors(item.node)...)
		neighbors = append(neighbors, g.InNeighbors(item.node)...)
		for _, neighbor := range neighbors {
			if _, ok := seen[neighbor]; ok {
				continue
			}
			if len(seen)+len(queue) >= maxNodes {
				continue
			}
			queue = append(queue, queued{node: neighbor, depth: item.depth + 1})
		}
	}
	if len(nodes) == 0 {
		return map[string]float64{}
	}

	// 局部化个性化向量（仅保留子图内种子）。
	localSeeds := map[string]float64{}
	totalSeed := 0.0
	for _, node := range nodes {
		if weight, ok := resolved[node]; ok && weight > 0 {
			localSeeds[node] = weight
			totalSeed += weight
		}
	}
	if totalSeed <= 0 {
		return map[string]float64{}
	}

	scores, iterations := p.iterate(g, nodes, localSeeds, localSeeds, nil)
	p.setIterations(iterations)
	total := 0.0
	for _, v := range scores {
		total += v
	}
	if total > 0 {
		for k := range scores {
			scores[k] /= total
		}
	}
	return scores
}

// iterate 是共用的幂迭代核心。
//   - nodes 限定参与计算的节点子集（nil 表示全图）
//   - personalization 为原始个性化向量（未归一化）
//   - normalizationSeeds 指定归一化范围（局部 PPR 时只统计子图内种子），nil 表示全图
//   - out 在非 nil 时写回中间过程（调试用）
func (p *PersonalizedPageRank) iterate(
	g *Graph,
	nodes []string,
	_ map[string]float64,
	personalization map[string]float64,
	_ map[string]float64,
) (map[string]float64, int) {
	n := len(nodes)
	index := make(map[string]int, n)
	for i, node := range nodes {
		index[node] = i
	}

	// 构建列归一化转移矩阵：M[col=源][row=目标] = w / outDegree(源)
	type entry struct {
		to int
		w  float64
	}
	cols := make([][]entry, n)
	for i, node := range nodes {
		edges := g.WeightedNeighbors(node)
		total := 0.0
		filtered := make([]WeightedEdge, 0, len(edges))
		for _, e := range edges {
			if e.Weight <= 0 {
				continue
			}
			if _, ok := index[e.To]; !ok {
				continue
			}
			filtered = append(filtered, e)
			total += e.Weight
		}
		if total <= 0 {
			continue
		}
		row := make([]entry, 0, len(filtered))
		for _, e := range filtered {
			row = append(row, entry{to: index[e.To], w: e.Weight / total})
		}
		cols[i] = row
	}

	// 个性化向量归一化。
	p0 := make([]float64, n)
	if len(personalization) == 0 {
		for i := range p0 {
			p0[i] = 1.0 / float64(n)
		}
	} else {
		for name, weight := range personalization {
			if idx, ok := index[name]; ok && weight > 0 && !math.IsInf(weight, 0) {
				p0[idx] += weight
			}
		}
		sum := 0.0
		for _, v := range p0 {
			sum += v
		}
		if sum == 0 {
			for i := range p0 {
				p0[i] = 1.0 / float64(n)
			}
		} else {
			for i := range p0 {
				p0[i] /= sum
			}
		}
	}

	scores := make([]float64, n)
	copy(scores, p0)
	alpha := p.Config.Alpha
	iterations := 0
	for iteration := 0; iteration < p.Config.MaxIter; iteration++ {
		next := make([]float64, n)
		// next = alpha * (M^T @ scores)；列归一化后按源节点把质量推给目标节点。
		for col, row := range cols {
			source := scores[col]
			if source == 0 {
				continue
			}
			for _, e := range row {
				next[e.to] += alpha * source * e.w
			}
		}
		danglingMass := 0.0
		for i := range cols {
			if len(cols[i]) == 0 {
				danglingMass += scores[i]
			}
		}
		for i := 0; i < n; i++ {
			next[i] += (1 - alpha) * p0[i]
			if danglingMass > 0 {
				next[i] += alpha * danglingMass * p0[i]
			}
		}
		diff := 0.0
		for i := range next {
			diff += math.Abs(next[i] - scores[i])
		}
		scores = next
		iterations = iteration + 1
		if iterations >= p.Config.MinIterations && diff < p.Config.Tol {
			break
		}
	}

	out := make(map[string]float64, n)
	for i, node := range nodes {
		out[node] = scores[i]
	}
	return out, iterations
}

// findNode 是 FindNode 的内部别名（保留以兼容早期调用点）。
func (g *Graph) findNode(name string) string { return g.FindNode(name) }

// FindPaths 返回两节点间不超过 maxDepth 的简单路径（节点序列），对齐 GraphStore.find_paths。
// 出处：core/retrieval/graph_relation_recall.py L170-L179（max_depth=2 的两跳召回）。
func (g *Graph) FindPaths(from, to string, maxDepth, maxPaths int) [][]string {
	if from == "" || to == "" || maxDepth <= 0 || maxPaths <= 0 {
		return nil
	}
	if g.findNode(from) == "" || g.findNode(to) == "" {
		return nil
	}
	var results [][]string
	path := []string{from}
	visited := map[string]struct{}{from: {}}
	var walk func(node string)
	walk = func(node string) {
		if len(results) >= maxPaths {
			return
		}
		if node == to && len(path) > 1 {
			cp := make([]string, len(path))
			copy(cp, path)
			results = append(results, cp)
			return
		}
		if len(path)-1 >= maxDepth {
			return
		}
		neighbors := append([]string{}, g.Neighbors(node)...)
		neighbors = append(neighbors, g.InNeighbors(node)...)
		sort.Strings(neighbors)
		for _, next := range neighbors {
			if _, ok := visited[next]; ok {
				continue
			}
			visited[next] = struct{}{}
			path = append(path, next)
			walk(next)
			path = path[:len(path)-1]
			delete(visited, next)
			if len(results) >= maxPaths {
				return
			}
		}
	}
	walk(from)
	return results
}

// RankNodes 按分数降序、节点名升序返回稳定排序结果。
// 出处：core/retrieval/pagerank.py L287-L310。
func RankNodes(scores map[string]float64, topK int, minScore float64) []WeightedEdge {
	out := make([]WeightedEdge, 0, len(scores))
	for node, score := range scores {
		if score >= minScore {
			out = append(out, WeightedEdge{To: node, Weight: score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].To < out[j].To
	})
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out
}
