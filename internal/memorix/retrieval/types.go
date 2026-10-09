package retrieval

import (
	"sort"
	"strings"
	"time"
)

// Strategy 是检索策略（对齐 Python RetrievalStrategy）。
// 出处：core/retrieval/dual_path.py L59-L65。
type Strategy string

const (
	// StrategyParagraphOnly 仅段落检索。
	StrategyParagraphOnly Strategy = "paragraph_only"
	// StrategyRelationOnly 仅关系检索。
	StrategyRelationOnly Strategy = "relation_only"
	// StrategyDualPath 双路检索（默认）。
	StrategyDualPath Strategy = "dual_path"
)

// Result 是一条检索结果（对齐 Python RetrievalResult）。
// 出处：core/retrieval/dual_path.py L68-L98。
type Result struct {
	Hash     string
	Content  string
	Score    float64
	Type     string // "paragraph" | "relation"
	Source   string
	Metadata map[string]any
}

// Paragraph 是段落元数据视图（对齐 metadata_store.add_paragraph 的行结构）。
type Paragraph struct {
	Hash       string
	Content    string
	Source     string
	ChatID     string
	WordCount  int
	CreatedAt  time.Time
	UpdatedAt  time.Time
	EventTime  *float64
	EventStart *float64
	EventEnd   *float64
	// KnowledgeType 对齐 paragraphs.knowledge_type。
	KnowledgeType string
	Metadata      map[string]any
}

// EffectiveTime 返回段落有效时间区间与命中依据。
// 出处：core/retrieval/dual_path.py L2970-L3001。
func (p *Paragraph) EffectiveTime(allowCreatedFallback bool) (start, end float64, basis string, ok bool) {
	if p.EventStart != nil || p.EventEnd != nil {
		switch {
		case p.EventStart != nil:
			start = *p.EventStart
		case p.EventTime != nil:
			start = *p.EventTime
		default:
			start = deref(p.EventEnd)
		}
		switch {
		case p.EventEnd != nil:
			end = *p.EventEnd
		case p.EventTime != nil:
			end = *p.EventTime
		default:
			end = deref(p.EventStart)
		}
		return start, end, "event_time_range", true
	}
	if p.EventTime != nil {
		return *p.EventTime, *p.EventTime, "event_time", true
	}
	if allowCreatedFallback && !p.CreatedAt.IsZero() {
		ts := float64(p.CreatedAt.UnixNano()) / 1e9
		return ts, ts, "created_at_fallback", true
	}
	return 0, 0, "", false
}

func deref(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// Relation 是三元组视图（对齐 relations 表）。
type Relation struct {
	Hash       string
	Subject    string
	Predicate  string
	Object     string
	Confidence float64
	CreatedAt  time.Time
	Metadata   map[string]any
}

// Content 渲染为 "subject predicate object"（对齐 GraphRelationCandidate.to_payload）。
// 出处：core/retrieval/graph_relation_recall.py L45。
func (r *Relation) Content() string {
	return strings.TrimSpace(r.Subject + " " + r.Predicate + " " + r.Object)
}

// Entity 是实体视图（对齐 entities 表）。
type Entity struct {
	Hash            string
	Name            string
	AppearanceCount int
}

// VectorHit 是一次向量检索命中。
type VectorHit struct {
	ID    string
	Score float64
}

// TemporalQueryOptions 对齐 Python TemporalQueryOptions。
// 出处：core/retrieval/dual_path.py L192-L202。
type TemporalQueryOptions struct {
	TimeFrom             *float64
	TimeTo               *float64
	Person               string
	Source               string
	AllowCreatedFallback bool
	CandidateMultiplier  int
	MaxScan              int
}

// DefaultTemporalQueryOptions 返回照抄 Python 的默认时序选项。
func DefaultTemporalQueryOptions() TemporalQueryOptions {
	return TemporalQueryOptions{
		AllowCreatedFallback: true,
		CandidateMultiplier:  DefaultTemporalCandidateMultiplier,
		MaxScan:              DefaultTemporalMaxScan,
	}
}

// Scope 是一次检索已解析出的可见资源集合（对齐 Python RetrievalScope）。
// 出处：core/retrieval/dual_path.py L101-L112。
type Scope struct {
	Key          string
	ParagraphIDs map[string]struct{}
	RelationIDs  map[string]struct{}
	EntityIDs    map[string]struct{}
	EpisodeIDs   map[string]struct{}
}

// NewScope 创建空 scope。
func NewScope(key string) *Scope {
	return &Scope{
		Key:          key,
		ParagraphIDs: map[string]struct{}{},
		RelationIDs:  map[string]struct{}{},
		EntityIDs:    map[string]struct{}{},
		EpisodeIDs:   map[string]struct{}{},
	}
}

// Empty 判定 scope 是否不含任何可见资源。
func (s *Scope) Empty() bool {
	if s == nil {
		return false
	}
	return len(s.ParagraphIDs) == 0 && len(s.RelationIDs) == 0 && len(s.EpisodeIDs) == 0
}

func (s *Scope) hasParagraph(hash string) bool {
	if s == nil || len(s.ParagraphIDs) == 0 {
		return true
	}
	_, ok := s.ParagraphIDs[hash]
	return ok
}

func (s *Scope) hasRelation(hash string) bool {
	if s == nil || len(s.RelationIDs) == 0 {
		return true
	}
	_, ok := s.RelationIDs[hash]
	return ok
}

// RelationIntentConfig 对齐 Python RelationIntentConfig。
// 出处：core/retrieval/dual_path.py L205-L223。
type RelationIntentConfig struct {
	Enabled                     bool
	AlphaOverride               float64
	RelationCandidateMultiplier int
	PreserveTopRelations        int
	ForceRelationSparse         bool
	PairPredicateRerankEnabled  bool
	PairPredicateLimit          int
}

// DefaultRelationIntentConfig 返回照抄 Python 的默认关系意图配置。
func DefaultRelationIntentConfig() RelationIntentConfig {
	return RelationIntentConfig{
		Enabled:                     DefaultRelationIntentEnabled,
		AlphaOverride:               DefaultRelationIntentAlphaOverride,
		RelationCandidateMultiplier: DefaultRelationCandidateMultiplier,
		PreserveTopRelations:        DefaultPreserveTopRelations,
		ForceRelationSparse:         DefaultForceRelationSparse,
		PairPredicateRerankEnabled:  DefaultPairPredicateRerankEnabled,
		PairPredicateLimit:          DefaultPairPredicateLimit,
	}
}

// RelationIntentContext 是一次查询解析出的关系意图上下文。
// 出处：core/retrieval/dual_path.py L487-L505。
type RelationIntentContext struct {
	Enabled              bool
	AlphaOverride        *float64
	RelationTopK         int
	PreserveTopRelations int
	ForceRelationSparse  bool
	PairPredicateRerank  bool
	PairPredicateLimit   int
}

// VectorPoolsConfig 对齐 Python VectorPoolsConfig。
// 出处：core/retrieval/dual_path.py L226-L285。
type VectorPoolsConfig struct {
	Mode                     string
	ParagraphTopK            int
	GraphTopK                int
	GraphExpandParagraphK    int
	RelationExpandPerHit     int
	EntityExpandPerHit       int
	RelationEvidenceWeight   float64
	EntityEvidenceWeight     float64
	SemanticWeight           float64
	SparseWeight             float64
	GraphWeight              float64
	ScoreCalibrationMethod   string
	ScoreCalibrationRRFK     int
	RelationIntentGraphTopK  int
	RelationIntentSemanticWt float64
	RelationIntentSparseWt   float64
	RelationIntentGraphWt    float64
	ReturnRelationItems      bool
}

// DefaultVectorPoolsConfig 返回照抄 Python 的默认双向量池配置。
func DefaultVectorPoolsConfig() VectorPoolsConfig {
	return VectorPoolsConfig{
		Mode:                     DefaultVectorPoolMode,
		ParagraphTopK:            DefaultParagraphPoolTopK,
		GraphTopK:                DefaultGraphPoolTopK,
		GraphExpandParagraphK:    DefaultGraphExpandParagraphK,
		RelationExpandPerHit:     DefaultRelationExpandPerHit,
		EntityExpandPerHit:       DefaultEntityExpandPerHit,
		RelationEvidenceWeight:   DefaultRelationEvidenceWeight,
		EntityEvidenceWeight:     DefaultEntityEvidenceWeight,
		SemanticWeight:           DefaultVectorPoolSemanticWeight,
		SparseWeight:             DefaultVectorPoolSparseWeight,
		GraphWeight:              DefaultVectorPoolGraphWeight,
		ScoreCalibrationMethod:   DefaultScoreCalibrationMethod,
		ScoreCalibrationRRFK:     DefaultScoreCalibrationRRFK,
		RelationIntentGraphTopK:  DefaultRelationIntentGraphTopK,
		RelationIntentSemanticWt: DefaultRelationIntentSemanticWeight,
		RelationIntentSparseWt:   DefaultRelationIntentSparseWeight,
		RelationIntentGraphWt:    DefaultRelationIntentGraphWeight,
		ReturnRelationItems:      DefaultKeepRelationItemsAfterIntent,
	}
}

// FusionConfig 对齐 Python FusionConfig。
// 出处：core/retrieval/dual_path.py L287-L310。
type FusionConfig struct {
	Method          string
	RRFK            int
	VectorWeight    float64
	BM25Weight      float64
	NormalizeScore  bool
	NormalizeMethod string
}

// DefaultFusionConfig 返回照抄 Python 的默认融合配置（权重归一到 1）。
func DefaultFusionConfig() FusionConfig {
	cfg := FusionConfig{
		Method:          DefaultFusionMethod,
		RRFK:            DefaultFusionRRFK,
		VectorWeight:    DefaultFusionVectorWeight,
		BM25Weight:      DefaultFusionBM25Weight,
		NormalizeScore:  DefaultNormalizeScore,
		NormalizeMethod: DefaultNormalizeMethod,
	}
	cfg.normalize()
	return cfg
}

func (c *FusionConfig) normalize() {
	if c.Method == "" {
		c.Method = DefaultFusionMethod
	}
	c.Method = strings.ToLower(c.Method)
	if c.NormalizeMethod == "" {
		c.NormalizeMethod = DefaultNormalizeMethod
	}
	c.NormalizeMethod = strings.ToLower(c.NormalizeMethod)
	if c.RRFK < 1 {
		c.RRFK = DefaultFusionRRFK
	}
	if c.VectorWeight < 0 {
		c.VectorWeight = 0
	}
	if c.BM25Weight < 0 {
		c.BM25Weight = 0
	}
	sum := c.VectorWeight + c.BM25Weight
	switch {
	case sum <= 0:
		c.VectorWeight, c.BM25Weight = DefaultFusionVectorWeight, DefaultFusionBM25Weight
	case sum != 1.0:
		c.VectorWeight /= sum
		c.BM25Weight /= sum
	}
}

// GraphRecallConfig 对齐 Python GraphRelationRecallConfig。
// 出处：core/retrieval/graph_relation_recall.py L21-L33。
type GraphRecallConfig struct {
	Enabled         bool
	CandidateK      int
	MaxHop          int
	AllowTwoHopPair bool
	MaxPaths        int
}

// DefaultGraphRecallConfig 返回照抄 Python 的受控图召回配置。
func DefaultGraphRecallConfig() GraphRecallConfig {
	return GraphRecallConfig{
		Enabled:         DefaultGraphRecallEnabled,
		CandidateK:      DefaultGraphRecallCandidateK,
		MaxHop:          DefaultGraphRecallMaxHop,
		AllowTwoHopPair: DefaultGraphRecallAllowTwoHop,
		MaxPaths:        DefaultGraphRecallMaxPaths,
	}
}

// GraphCandidate 是图结构生成的关系候选。
// 出处：core/retrieval/graph_relation_recall.py L37-L59。
type GraphCandidate struct {
	Hash                     string
	Subject                  string
	Predicate                string
	Object                   string
	Confidence               float64
	GraphSeedEntities        []string
	GraphHops                int
	GraphCandidateType       string
	SupportingParagraphCount int
}

// GraphScoreForType 返回图候选类型对应的结构分。
// 出处：core/retrieval/dual_path.py L1015-L1019。
func GraphScoreForType(candidateType string) float64 {
	switch candidateType {
	case "direct_pair":
		return GraphScoreDirectPair
	case "one_hop_seed":
		return GraphScoreOneHopSeed
	case "two_hop_pair":
		return GraphScoreTwoHopPair
	default:
		return GraphScoreUnknownType
	}
}

// GraphEvidenceGroundingFactor 按证据在支撑段落中的逐字落地程度软衰减。
// 出处：core/retrieval/dual_path.py L1886-L1916。
func GraphEvidenceGroundingFactor(evidence map[string]any, paragraphText string) float64 {
	// 对齐 _graph_evidence_grounding_factor 的 NFKC + casefold 逐字比较。
	normalized := CaseFold(paragraphText)
	isGrounded := func(value any) bool {
		token := strings.TrimSpace(CaseFold(stringify(value)))
		return token != "" && strings.Contains(normalized, token)
	}
	if stringify(evidence["type"]) == "entity" {
		if isGrounded(evidence["name"]) {
			return 1.0
		}
		return EntityUngroundedFactor
	}
	subjectGrounded := isGrounded(evidence["subject"])
	predicateGrounded := isGrounded(evidence["predicate"])
	objectGrounded := isGrounded(evidence["object"])
	switch {
	case subjectGrounded && predicateGrounded && objectGrounded:
		return 1.0
	case subjectGrounded && objectGrounded:
		return RelationBothEntitiesGroundingFactor
	case subjectGrounded || objectGrounded:
		return RelationSingleEntityGroundingFactor
	case predicateGrounded:
		return RelationPredicateOnlyGroundingFactor
	default:
		return RelationUngroundedFactor
	}
}

// AggregateGraphEvidenceScore 聚合候选的图证据分。
// 出处：core/retrieval/dual_path.py L1701-L1725。
func AggregateGraphEvidenceScore(evidenceItems []map[string]any) float64 {
	var relationScores, entityScores []float64
	for _, item := range evidenceItems {
		score := toFloat(item["score"])
		switch stringify(item["type"]) {
		case "relation":
			relationScores = append(relationScores, score)
		case "entity":
			entityScores = append(entityScores, score)
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(relationScores)))
	graphScore := 0.0
	if len(relationScores) > 0 {
		graphScore += GraphEvidenceTopRelationWeight * relationScores[0]
		for _, score := range relationScores[1:] {
			graphScore += GraphEvidenceExtraRelationWt * score
		}
	}
	total := 0.0
	for _, score := range entityScores {
		total += score
	}
	graphScore += GraphEvidenceEntityWeight * total
	if graphScore > GraphEvidenceAggregateScoreClip {
		return GraphEvidenceAggregateScoreClip
	}
	return graphScore
}

// GraphReliabilityEstimate 是查询局部图证据可信度估计。
// 出处：core/retrieval/dual_path.py L45-L57。
type GraphReliabilityEstimate struct {
	Score                 float64
	GroundingQuality      float64
	ChannelAgreement      float64
	SupportCoverage       float64
	EvidenceCount         int
	GroundedRelationCount int
	RelationCount         int
}

// EstimateGraphReliability 用证据落地、多通道一致性和关系覆盖估计可信度。
// 出处：core/retrieval/dual_path.py L1730-L1831。
func EstimateGraphReliability(candidates []*Result, scanLimit int) GraphReliabilityEstimate {
	type ranked struct {
		graphScore float64
		candidate  *Result
	}
	var rankedCandidates []ranked
	bestIndependent := 0.0
	for _, candidate := range candidates {
		meta := scoreBreakdown(candidate)
		semantic := maxFloat(0, toFloat(meta["semantic"]))
		sparse := maxFloat(0, toFloat(meta["sparse"]))
		bestIndependent = maxFloat(bestIndependent, semantic, sparse)
		graphScore := maxFloat(0, toFloat(meta["graph_evidence"]))
		if graphScore <= 0 {
			graphScore = AggregateGraphEvidenceScore(evidenceItems(candidate))
		}
		if graphScore > 0 {
			rankedCandidates = append(rankedCandidates, ranked{graphScore: graphScore, candidate: candidate})
		}
	}
	sort.SliceStable(rankedCandidates, func(i, j int) bool {
		return rankedCandidates[i].graphScore > rankedCandidates[j].graphScore
	})
	if scanLimit < 1 {
		scanLimit = 1
	}
	if len(rankedCandidates) > scanLimit {
		rankedCandidates = rankedCandidates[:scanLimit]
	}
	if len(rankedCandidates) == 0 {
		return GraphReliabilityEstimate{}
	}

	graphScoreTotal := 0.0
	agreementTotal := 0.0
	groundingWeightTotal := 0.0
	groundingQualityTotal := 0.0
	evidenceCount := 0
	groundedRelations := map[string]struct{}{}
	relations := map[string]struct{}{}

	for _, item := range rankedCandidates {
		graphScoreTotal += item.graphScore
		meta := scoreBreakdown(item.candidate)
		independent := maxFloat(0, toFloat(meta["semantic"]), toFloat(meta["sparse"]))
		if bestIndependent > 0 {
			agreementTotal += item.graphScore * ClipUnit(independent/bestIndependent)
		}
		for _, evidence := range evidenceItems(item.candidate) {
			evidenceType := strings.ToLower(strings.TrimSpace(stringify(evidence["type"])))
			if evidenceType != "relation" && evidenceType != "entity" {
				continue
			}
			normalizedScore := maxFloat(0, toFloat(evidence["normalized_score"]))
			if normalizedScore <= 0 {
				continue
			}
			groundingFactor := ClipUnit(toFloat(evidence["grounding_factor"]))
			var normalizedGrounding float64
			if evidenceType == "relation" {
				normalizedGrounding = ClipUnit(
					(groundingFactor - RelationUngroundedFactor) / (1.0 - RelationUngroundedFactor),
				)
				hash := strings.TrimSpace(stringify(evidence["hash"]))
				if hash != "" {
					relations[hash] = struct{}{}
					if groundingFactor >= RelationBothEntitiesGroundingFactor {
						groundedRelations[hash] = struct{}{}
					}
				}
			} else {
				// 实体在段落中出现只能证明局部落地，不能单独证明关系链可靠。
				normalizedGrounding = EntityGroundingNormalizationScale * ClipUnit(
					(groundingFactor-EntityUngroundedFactor)/(1.0-EntityUngroundedFactor),
				)
			}
			groundingWeightTotal += normalizedScore
			groundingQualityTotal += normalizedScore * normalizedGrounding
			evidenceCount++
		}
	}

	groundingQuality := 0.0
	if groundingWeightTotal > 0 {
		groundingQuality = groundingQualityTotal / groundingWeightTotal
	}
	channelAgreement := 0.0
	if graphScoreTotal > 0 {
		channelAgreement = agreementTotal / graphScoreTotal
	}
	supportQuantity := ClipUnit(float64(len(groundedRelations)) / float64(GraphReliabilitySupportTarget))
	supportPrecision := 0.0
	if len(relations) > 0 {
		supportPrecision = float64(len(groundedRelations)) / float64(len(relations))
	}
	supportCoverage := ClipUnit(supportQuantity * supportPrecision)
	reliability := ClipUnit(
		GraphReliabilityGroundingWeight*groundingQuality +
			GraphReliabilityAgreementWeight*channelAgreement +
			GraphReliabilitySupportWeight*supportCoverage,
	)
	return GraphReliabilityEstimate{
		Score:                 reliability,
		GroundingQuality:      groundingQuality,
		ChannelAgreement:      channelAgreement,
		SupportCoverage:       supportCoverage,
		EvidenceCount:         evidenceCount,
		GroundedRelationCount: len(groundedRelations),
		RelationCount:         len(relations),
	}
}

// CalibrateDualPoolWeights 按查询局部图可信度连续缩放图权重。
// 出处：core/retrieval/dual_path.py L1833-L1863。
func CalibrateDualPoolWeights(
	candidates []*Result,
	semanticWeight, sparseWeight, graphWeight float64,
	scanLimit int,
) (float64, float64, float64, GraphReliabilityEstimate) {
	estimate := EstimateGraphReliability(candidates, scanLimit)
	floor := minFloat(graphWeight, GraphReliabilityWeightFloor)
	weightRange := maxFloat(0, graphWeight-floor)
	independent := semanticWeight + sparseWeight
	if weightRange <= 0 || independent <= 0 {
		return semanticWeight, sparseWeight, graphWeight, estimate
	}
	effectiveGraph := floor + weightRange*pow(estimate.Score, GraphReliabilityCurveExponent)
	released := graphWeight - effectiveGraph
	effectiveSemantic := semanticWeight + released*(semanticWeight/independent)
	effectiveSparse := sparseWeight + released*(sparseWeight/independent)
	return effectiveSemantic, effectiveSparse, effectiveGraph, estimate
}

// NormalizeGraphScoresByType 按 item 类型分组做 min-max 归一化。
// 出处：core/retrieval/dual_path.py L1865-L1884。
func NormalizeGraphScoresByType(items []struct {
	Type  string
	Hash  string
	Score float64
}) []struct {
	Type            string
	Hash            string
	RawScore        float64
	NormalizedScore float64
} {
	bounds := map[string][2]float64{}
	for _, item := range items {
		cur, ok := bounds[item.Type]
		if !ok {
			bounds[item.Type] = [2]float64{item.Score, item.Score}
			continue
		}
		if item.Score < cur[0] {
			cur[0] = item.Score
		}
		if item.Score > cur[1] {
			cur[1] = item.Score
		}
		bounds[item.Type] = cur
	}
	out := make([]struct {
		Type            string
		Hash            string
		RawScore        float64
		NormalizedScore float64
	}, 0, len(items))
	for _, item := range items {
		bound := bounds[item.Type]
		normalized := 0.0
		if bound[1] > bound[0] {
			normalized = (item.Score - bound[0]) / (bound[1] - bound[0])
		} else {
			normalized = ClipUnit(item.Score)
		}
		out = append(out, struct {
			Type            string
			Hash            string
			RawScore        float64
			NormalizedScore float64
		}{item.Type, item.Hash, item.Score, normalized})
	}
	return out
}

// ScoreBreakdown 返回候选的 score_breakdown（缺失时创建）。
func scoreBreakdown(candidate *Result) map[string]any {
	if candidate.Metadata == nil {
		candidate.Metadata = map[string]any{}
	}
	meta, ok := candidate.Metadata["score_breakdown"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		candidate.Metadata["score_breakdown"] = meta
	}
	return meta
}

// ScoreBreakdownOf 是 ScoreBreakdown 的只读出口。
func ScoreBreakdownOf(candidate *Result) map[string]any { return scoreBreakdown(candidate) }

// EvidenceItems 返回候选的 evidence_items（缺失时创建）。
func evidenceItems(candidate *Result) []map[string]any {
	if candidate.Metadata == nil {
		candidate.Metadata = map[string]any{}
	}
	raw, ok := candidate.Metadata["evidence_items"].([]map[string]any)
	if !ok {
		raw = []map[string]any{}
		candidate.Metadata["evidence_items"] = raw
	}
	return raw
}

// EvidenceItemsOf 是 evidenceItems 的只读出口。
func EvidenceItemsOf(candidate *Result) []map[string]any { return evidenceItems(candidate) }

// SetEvidenceItems 覆盖候选的证据列表。
func SetEvidenceItems(candidate *Result, items []map[string]any) {
	if candidate.Metadata == nil {
		candidate.Metadata = map[string]any{}
	}
	candidate.Metadata["evidence_items"] = items
}

// AppendGraphEvidence 追加或更新一条图证据。
// 出处：core/retrieval/dual_path.py L1676-L1700。
func AppendGraphEvidence(candidate *Result, evidence map[string]any, score float64) {
	items := evidenceItems(candidate)
	payload := make(map[string]any, len(evidence)+1)
	for k, v := range evidence {
		payload[k] = v
	}
	payload["score"] = score
	keyType := stringify(payload["type"])
	keyHash := stringify(payload["hash"])
	for _, old := range items {
		if stringify(old["type"]) == keyType && stringify(old["hash"]) == keyHash {
			if score > toFloat(old["score"]) {
				for k, v := range payload {
					old[k] = v
				}
			}
			return
		}
	}
	items = append(items, payload)
	candidate.Metadata["evidence_items"] = items
}

// AddCandidateScore 记录候选某通道的分（取较大值并登记来源）。
// 出处：core/retrieval/dual_path.py L1655-L1675 与 L1645-L1653。
func AddCandidateScore(candidate *Result, scoreKey string, score float64, source string) {
	meta := scoreBreakdown(candidate)
	if score > toFloat(meta[scoreKey]) {
		meta[scoreKey] = score
	}
	if candidate.Metadata == nil {
		candidate.Metadata = map[string]any{}
	}
	sources, _ := candidate.Metadata["dual_pool_sources"].([]string)
	found := false
	for _, s := range sources {
		if s == source {
			found = true
			break
		}
	}
	if !found {
		sources = append(sources, source)
	}
	candidate.Metadata["dual_pool_sources"] = sources
}

// AppendCandidateSource 只登记来源（不记分）。
// 出处：core/retrieval/dual_path.py L1645-L1653。
func AppendCandidateSource(candidate *Result, source string) {
	if candidate.Metadata == nil {
		candidate.Metadata = map[string]any{}
	}
	sources, _ := candidate.Metadata["dual_pool_sources"].([]string)
	for _, s := range sources {
		if s == source {
			return
		}
	}
	candidate.Metadata["dual_pool_sources"] = append(sources, source)
}

// ToFloat 把 map 中的任意数值转 float64。
func ToFloat(value any) float64 { return toFloat(value) }

func toFloat(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int32:
		return float64(v)
	case int64:
		return float64(v)
	case bool:
		if v {
			return 1
		}
		return 0
	default:
		return 0
	}
}

func stringify(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func maxFloat(values ...float64) float64 {
	result := 0.0
	for _, v := range values {
		if v > result {
			result = v
		}
	}
	return result
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func pow(base, exp float64) float64 {
	if base <= 0 {
		return 0
	}
	return mathPow(base, exp)
}
