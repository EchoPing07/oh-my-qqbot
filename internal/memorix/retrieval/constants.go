// Package retrieval 实现 A_memorix 检索层：BM25 稀疏召回、Aho-Corasick 实体匹配、
// Personalized PageRank 幂迭代、双路（段落/关系）融合与后验图门。
//
// 本文件集中登记从 Python 原版照抄的常数，每条都标注出处（文件 + 行号）。
// Python 原版路径：参考/MaiBot/src/A_memorix/。
package retrieval

// 图证据落地（grounding）因子。
// 出处：core/retrieval/dual_path.py L33-L37
const (
	// RelationBothEntitiesGroundingFactor subject+object 都在支撑段落中逐字出现。
	RelationBothEntitiesGroundingFactor = 0.78
	// RelationSingleEntityGroundingFactor 只有 subject 或 object 落地。
	RelationSingleEntityGroundingFactor = 0.55
	// RelationPredicateOnlyGroundingFactor 只有 predicate 落地。
	RelationPredicateOnlyGroundingFactor = 0.4
	// RelationUngroundedFactor 三元组完全不落地。
	RelationUngroundedFactor = 0.3
	// EntityUngroundedFactor 实体名未在段落中逐字出现。
	EntityUngroundedFactor = 0.4
)

// 图可靠性（graph reliability）权重。
// 出处：core/retrieval/dual_path.py L38-L43
const (
	GraphReliabilityGroundingWeight = 0.65
	GraphReliabilityAgreementWeight = 0.10
	GraphReliabilitySupportWeight   = 0.25
	GraphReliabilitySupportTarget   = 2
	GraphReliabilityWeightFloor     = 0.15
	GraphReliabilityCurveExponent   = 3.0
)

// entity 证据的归一化落地系数（实体只能证明局部落地）。
// 出处：core/retrieval/dual_path.py L1789-L1793
const EntityGroundingNormalizationScale = 0.35

// 检索器默认参数。
// 出处：core/retrieval/dual_path.py L135-L146
const (
	DefaultTopKParagraphs   = 20
	DefaultTopKRelations    = 10
	DefaultTopKFinal        = 10
	DefaultAlpha            = 0.5
	DefaultPPRAlpha         = 0.85
	DefaultPPRTimeout       = 1.5
	DefaultPPRConcurrency   = 4
	DefaultPPRLocalEnabled  = true
	DefaultPPRLocalMaxNodes = 256
	DefaultPPRLocalHops     = 2
	DefaultPPRLocalMinNodes = 128
)

// Personalized PageRank 迭代参数。
// 出处：core/retrieval/pagerank.py L24-L30（alpha/max_iter/tol/normalize/min_iterations）
const (
	DefaultPPRMaxIter       = 100
	DefaultPPRTol           = 1e-6
	DefaultPPRNormalize     = true
	DefaultPPRMinIterations = 20
)

// 时序查询选项默认值。
// 出处：core/retrieval/dual_path.py L192-L202
const (
	DefaultTemporalCandidateMultiplier = 8
	DefaultTemporalMaxScan             = 1000
)

// 稀疏 BM25 参数。
// 出处：core/retrieval/sparse_bm25.py L40-L61
const (
	DefaultSparseEnabled            = true
	DefaultSparseMode               = "auto"
	DefaultSparseTokenizerMode      = "jieba"
	DefaultSparseCharNgramN         = 2
	DefaultSparseCandidateK         = 80
	DefaultSparseMaxDocLen          = 2000
	DefaultSparseRelationCandidateK = 60
	DefaultSparseRelationMaxDocLen  = 512
	// SparseAutoMinBestScore 是 auto 模式下的回落阈值：向量最高分低于它才启用 BM25。
	// 出处：core/retrieval/dual_path.py L593（best < 0.45）
	SparseAutoMinBestScore = 0.45
	// SparseTailRelativeFloor 是稀疏候选尾部裁剪的相对分下限。
	// 出处：core/retrieval/dual_path.py L912-L937
	SparseTailRelativeFloor = 0.2
	// SparseTailMinMatchedTokens 尾部候选至少命中的 token 数。
	SparseTailMinMatchedTokens = 3
	// SparseTailMinMatchedRatio 尾部候选命中的 token 比例。
	SparseTailMinMatchedRatio = 0.12
	// SparseMatchQueryMaxTokens 是 FTS5 MATCH 的最大 token 数。
	// 出处：core/retrieval/sparse_bm25.py L449（safe_tokens[:64]）
	SparseMatchQueryMaxTokens = 64
	// SparseSubstringCoverageWeight 是 LIKE 兜底打分中的覆盖度权重。
	// 出处：core/retrieval/sparse_bm25.py L530（coverage * 0.8 + length_bonus * 0.2）
	SparseSubstringCoverageWeight = 0.8
	SparseSubstringLengthWeight   = 0.2
)

// 融合配置默认值。
// 出处：core/retrieval/dual_path.py L287-L296（method/rrf_k/vector_weight/bm25_weight）
const (
	DefaultFusionMethod       = "weighted_rrf"
	DefaultFusionRRFK         = 60
	DefaultFusionVectorWeight = 0.7
	DefaultFusionBM25Weight   = 0.3
	DefaultNormalizeScore     = true
	DefaultNormalizeMethod    = "minmax"
)

// 双向量池配置默认值。
// 出处：core/retrieval/dual_path.py L226-L245
const (
	DefaultVectorPoolMode                = "dual"
	DefaultParagraphPoolTopK             = 20
	DefaultGraphPoolTopK                 = 40
	DefaultGraphExpandParagraphK         = 80
	DefaultRelationExpandPerHit          = 5
	DefaultEntityExpandPerHit            = 8
	DefaultRelationEvidenceWeight        = 1.0
	DefaultEntityEvidenceWeight          = 0.55
	DefaultVectorPoolSemanticWeight      = 0.65
	DefaultVectorPoolSparseWeight        = 0.20
	DefaultVectorPoolGraphWeight         = 0.15
	DefaultScoreCalibrationMethod        = "none"
	DefaultScoreCalibrationRRFK          = 60
	DefaultRelationIntentGraphTopK       = 80
	DefaultRelationIntentSemanticWeight  = 0.45
	DefaultRelationIntentSparseWeight    = 0.15
	DefaultRelationIntentGraphWeight     = 0.40
	DefaultKeepRelationItemsAfterIntent  = false
	DefaultGraphReliabilityReliabilityHi = 1.0
)

// 关系意图（relation intent）配置默认值。
// 出处：core/retrieval/dual_path.py L205-L215
const (
	DefaultRelationIntentEnabled       = true
	DefaultRelationIntentAlphaOverride = 0.35
	DefaultRelationCandidateMultiplier = 4
	DefaultPreserveTopRelations        = 3
	DefaultForceRelationSparse         = true
	DefaultPairPredicateRerankEnabled  = true
	DefaultPairPredicateLimit          = 3
)

// 受控图关系召回配置。
// 出处：core/retrieval/graph_relation_recall.py L21-L28
const (
	DefaultGraphRecallEnabled        = true
	DefaultGraphRecallCandidateK     = 24
	DefaultGraphRecallMaxHop         = 1
	DefaultGraphRecallAllowTwoHop    = true
	DefaultGraphRecallMaxPaths       = 4
	DefaultGraphRecallTwoHopMinCount = 3
)

// 图候选类型权重映射。
// 出处：core/retrieval/dual_path.py L1015-L1019（direct_pair 1.0 / one_hop_seed 0.75 / two_hop_pair 0.55）
const (
	GraphScoreDirectPair  = 1.0
	GraphScoreOneHopSeed  = 0.75
	GraphScoreTwoHopPair  = 0.55
	GraphScoreUnknownType = 0.0
)

// 图增强关系合并权重与支撑度归一化。
// 出处：core/retrieval/dual_path.py L1058（evidence_score = min(1, count/3)）与 L1063
// （score = 0.60 * semantic + 0.30 * graph + 0.10 * evidence）
const (
	RelationMergeSemanticWeight = 0.60
	RelationMergeGraphWeight    = 0.30
	RelationMergeEvidenceWeight = 0.10
	RelationMergeSupportTarget  = 3.0
)

// 候选图证据聚合权重。
// 出处：core/retrieval/dual_path.py L1715-L1725
// （最高 relation 证据 + 0.35 * 其余 relation + 0.20 * 全部 entity，上限 1.0）
const (
	GraphEvidenceTopRelationWeight  = 1.0
	GraphEvidenceExtraRelationWt    = 0.35
	GraphEvidenceEntityWeight       = 0.20
	GraphEvidenceAggregateScoreClip = 1.0
)

// PPR 重排增益。
// 出处：core/retrieval/dual_path.py L2725-L2732
// （取命中实体 PPR 前 2 的均值，2 个及以上权重 0.12，否则 0.06）
const (
	PPRBoostFocusSize      = 2
	PPRBoostWeightMultiple = 0.12
	PPRBoostWeightSingle   = 0.06
)

// 分数校准方法集合。
// 出处：core/retrieval/score_calibration.py L12-L27
var ScoreCalibrationMethods = []string{
	"none",
	"minmax",
	"rank_percentile",
	"robust_sigmoid",
	"semantic_minmax",
	"semantic_graph_rrf",
	"semantic_rank_percentile",
	"semantic_robust_sigmoid",
	"semantic_rrf",
	"semantic_sparse_rrf",
	"semantic_softmax",
	"softmax",
	"weighted_rrf",
}

// RRF 方法到参与校准通道的映射（nil 表示全部通道）。
// 出处：core/retrieval/score_calibration.py L29-L34
var scoreCalibrationRRFChannels = map[string][]string{
	"semantic_graph_rrf":  {"semantic", "graph"},
	"semantic_rrf":        {"semantic"},
	"semantic_sparse_rrf": {"semantic", "sparse"},
	"weighted_rrf":        nil,
}

// 后验图门（posterior graph gate）配置默认值。
// 出处：core/retrieval/posterior_graph.py L20-L46
const (
	DefaultPosteriorEnabled              = false
	DefaultPosteriorDropRatio            = 0.15
	DefaultPosteriorMinCoreResults       = 2
	DefaultPosteriorMaxGraphSlots        = 2
	DefaultPosteriorGateScanTopK         = 5
	DefaultPosteriorGroundedConfidence   = 0.48
	DefaultPosteriorIncidentalConfidence = 0.22
	DefaultPosteriorMinQueryTokenCover   = 0.78
	DefaultPosteriorIncidentalRelevance  = 0.68
	DefaultPosteriorIncidentalOverlap    = 0.34
	DefaultPosteriorIncidentalSpecific   = 0.42
	DefaultPosteriorQueryWeight          = 0.28
	DefaultPosteriorNoveltyWeight        = 0.18
	DefaultPosteriorComplementarityWt    = 0.16
	DefaultPosteriorSpecificityWeight    = 0.12
	DefaultPosteriorGapFillWeight        = 0.26
	DefaultPosteriorMaxCandidateTokens   = 48
)

// 后验图门内部的软阈值常数。
// 出处：core/retrieval/posterior_graph.py L233（core_overlap 甜点 0.4）
// 与 L251（0.6 * entity_density + 0.4 * brevity + predicate_bonus）
const (
	PosteriorCoreOverlapSweetSpot   = 0.4
	PosteriorEntityDensityWeight    = 0.6
	PosteriorBrevityWeight          = 0.4
	PosteriorBrevityTokenBudget     = 16.0
	PosteriorBroadPredicatePenalty  = -0.25
	PosteriorRelationPredicateBonus = 0.10
	PosteriorSpecificityEntityScale = 4.0
	PosteriorEntityHitWeight        = 0.65
	PosteriorTokenHitWeight         = 0.35
)

// 动态阈值过滤默认值。
// 出处：core/retrieval/threshold.py L44-L48
const (
	DefaultThresholdMin        = 0.29
	DefaultThresholdMax        = 0.95
	DefaultThresholdPercentile = 75.0
	DefaultThresholdStdMult    = 1.5
	DefaultThresholdMinResults = 4
)

// episode 检索的双分支 RRF 融合常数。
// 出处：core/utils/episode_retrieval_service.py L18-L22
const (
	EpisodeFusionRRFK                = 60.0
	EpisodeFusionLexicalWeight       = 1.0
	EpisodeFusionParagraphWeight     = 1.0
	EpisodeFusionRelationWeight      = 0.85
	EpisodeLexicalFieldWeightDefault = 1.0
)

// aggregate 模式的 RRF 常数与分支权重。
// 出处：core/utils/aggregate_query_service.py L53-L70（rrf_k 默认 60，search/time/episode 权重 1.0）
const (
	AggregateDefaultRRFK          = 60.0
	AggregateDefaultSearchWeight  = 1.0
	AggregateDefaultTimeWeight    = 1.0
	AggregateDefaultEpisodeWeight = 1.0
)

// 关系数据支撑度上限（用于 evidence_score 计算）。
// 出处：core/retrieval/dual_path.py L1058
const RelationSupportParagraphTarget = 3.0

// RelationIntentPattern 是关系意图查询的正则（Python re.IGNORECASE）。
// 出处：core/retrieval/dual_path.py L396-L399
const RelationIntentPattern = `(什么关系|有哪些关系|和.+关系|关联|关系网|subject|predicate|object|relation|related|between.+and)`

// CJKCharClass 是中文字符的正则区间（用于分词与竞争画像）。
// 出处：core/retrieval/posterior_graph.py L11 与 sparse_bm25.py L396（[\u4e00-\u9fff]）。
const CJKCharClass = `[\x{4e00}-\x{9fff}]`
