package retrieval

import (
	"math"
	"testing"
)

// TestConstantsMatchPythonSource 对照 Python 源码逐条断言常数（防走样）。
// 出处见 retrieval/constants.go 的逐条注释。
func TestConstantsMatchPythonSource(t *testing.T) {
	cases := []struct {
		name   string
		got    float64
		want   float64
		source string
	}{
		{"RelationBothEntitiesGroundingFactor", RelationBothEntitiesGroundingFactor, 0.78, "dual_path.py L33"},
		{"RelationSingleEntityGroundingFactor", RelationSingleEntityGroundingFactor, 0.55, "dual_path.py L34"},
		{"RelationPredicateOnlyGroundingFactor", RelationPredicateOnlyGroundingFactor, 0.4, "dual_path.py L35"},
		{"RelationUngroundedFactor", RelationUngroundedFactor, 0.3, "dual_path.py L36"},
		{"EntityUngroundedFactor", EntityUngroundedFactor, 0.4, "dual_path.py L37"},
		{"GraphReliabilityGroundingWeight", GraphReliabilityGroundingWeight, 0.65, "dual_path.py L38"},
		{"GraphReliabilityAgreementWeight", GraphReliabilityAgreementWeight, 0.10, "dual_path.py L39"},
		{"GraphReliabilitySupportWeight", GraphReliabilitySupportWeight, 0.25, "dual_path.py L40"},
		{"GraphReliabilitySupportTarget", GraphReliabilitySupportTarget, 2, "dual_path.py L41"},
		{"GraphReliabilityWeightFloor", GraphReliabilityWeightFloor, 0.15, "dual_path.py L42"},
		{"GraphReliabilityCurveExponent", GraphReliabilityCurveExponent, 3.0, "dual_path.py L43"},
		{"DefaultAlpha", DefaultAlpha, 0.5, "dual_path.py L138"},
		{"DefaultPPRAlpha", DefaultPPRAlpha, 0.85, "dual_path.py L140 / pagerank.py L25"},
		{"DefaultPPRTol", DefaultPPRTol, 1e-6, "pagerank.py L27"},
		{"DefaultPPRMinIterations", float64(DefaultPPRMinIterations), 20, "pagerank.py L30"},
		{"SparseAutoMinBestScore", SparseAutoMinBestScore, 0.45, "dual_path.py L593"},
		{"DefaultFusionVectorWeight", DefaultFusionVectorWeight, 0.7, "dual_path.py L293"},
		{"DefaultFusionBM25Weight", DefaultFusionBM25Weight, 0.3, "dual_path.py L294"},
		{"DefaultVectorPoolSemanticWeight", DefaultVectorPoolSemanticWeight, 0.65, "dual_path.py L237"},
		{"DefaultVectorPoolSparseWeight", DefaultVectorPoolSparseWeight, 0.20, "dual_path.py L238"},
		{"DefaultVectorPoolGraphWeight", DefaultVectorPoolGraphWeight, 0.15, "dual_path.py L239"},
		{"DefaultEntityEvidenceWeight", DefaultEntityEvidenceWeight, 0.55, "dual_path.py L236"},
		{"DefaultRelationIntentAlphaOverride", DefaultRelationIntentAlphaOverride, 0.35, "dual_path.py L209"},
		{"DefaultRelationIntentSemanticWeight", DefaultRelationIntentSemanticWeight, 0.45, "dual_path.py L243"},
		{"DefaultRelationIntentSparseWeight", DefaultRelationIntentSparseWeight, 0.15, "dual_path.py L244"},
		{"DefaultRelationIntentGraphWeight", DefaultRelationIntentGraphWeight, 0.40, "dual_path.py L245"},
		{"GraphScoreDirectPair", GraphScoreDirectPair, 1.0, "dual_path.py L1016"},
		{"GraphScoreOneHopSeed", GraphScoreOneHopSeed, 0.75, "dual_path.py L1017"},
		{"GraphScoreTwoHopPair", GraphScoreTwoHopPair, 0.55, "dual_path.py L1017"},
		{"RelationMergeSemanticWeight", RelationMergeSemanticWeight, 0.60, "dual_path.py L1063"},
		{"RelationMergeGraphWeight", RelationMergeGraphWeight, 0.30, "dual_path.py L1063"},
		{"RelationMergeEvidenceWeight", RelationMergeEvidenceWeight, 0.10, "dual_path.py L1063"},
		{"GraphEvidenceExtraRelationWt", GraphEvidenceExtraRelationWt, 0.35, "dual_path.py L1721"},
		{"GraphEvidenceEntityWeight", GraphEvidenceEntityWeight, 0.20, "dual_path.py L1723"},
		{"PPRBoostWeightMultiple", PPRBoostWeightMultiple, 0.12, "dual_path.py L2729"},
		{"PPRBoostWeightSingle", PPRBoostWeightSingle, 0.06, "dual_path.py L2729"},
		{"EntityGroundingNormalizationScale", EntityGroundingNormalizationScale, 0.35, "dual_path.py L1791"},
		{"DefaultPosteriorDropRatio", DefaultPosteriorDropRatio, 0.15, "posterior_graph.py L23"},
		{"DefaultPosteriorMinQueryTokenCover", DefaultPosteriorMinQueryTokenCover, 0.78, "posterior_graph.py L38"},
		{"DefaultPosteriorGroundedConfidence", DefaultPosteriorGroundedConfidence, 0.48, "posterior_graph.py L35"},
		{"DefaultPosteriorIncidentalConfidence", DefaultPosteriorIncidentalConfidence, 0.22, "posterior_graph.py L36"},
		{"DefaultPosteriorIncidentalRelevance", DefaultPosteriorIncidentalRelevance, 0.68, "posterior_graph.py L39"},
		{"DefaultPosteriorIncidentalOverlap", DefaultPosteriorIncidentalOverlap, 0.34, "posterior_graph.py L40"},
		{"DefaultPosteriorIncidentalSpecific", DefaultPosteriorIncidentalSpecific, 0.42, "posterior_graph.py L41"},
		{"DefaultPosteriorQueryWeight", DefaultPosteriorQueryWeight, 0.28, "posterior_graph.py L42"},
		{"DefaultPosteriorNoveltyWeight", DefaultPosteriorNoveltyWeight, 0.18, "posterior_graph.py L43"},
		{"DefaultPosteriorComplementarityWt", DefaultPosteriorComplementarityWt, 0.16, "posterior_graph.py L44"},
		{"DefaultPosteriorSpecificityWeight", DefaultPosteriorSpecificityWeight, 0.12, "posterior_graph.py L45"},
		{"DefaultPosteriorGapFillWeight", DefaultPosteriorGapFillWeight, 0.26, "posterior_graph.py L46"},
		{"DefaultThresholdMin", DefaultThresholdMin, 0.29, "threshold.py L44"},
		{"DefaultThresholdMax", DefaultThresholdMax, 0.95, "threshold.py L45"},
		{"DefaultThresholdPercentile", DefaultThresholdPercentile, 75.0, "threshold.py L46"},
		{"DefaultThresholdStdMult", DefaultThresholdStdMult, 1.5, "threshold.py L47"},
		{"EpisodeFusionRelationWeight", EpisodeFusionRelationWeight, 0.85, "episode_retrieval_service.py L21"},
	}
	for _, tc := range cases {
		if math.Abs(tc.got-tc.want) > 1e-12 {
			t.Errorf("%s: got %v, want %v (%s)", tc.name, tc.got, tc.want, tc.source)
		}
	}

	if DefaultPPRMaxIter != 100 {
		t.Errorf("DefaultPPRMaxIter: got %d, want 100 (pagerank.py L26)", DefaultPPRMaxIter)
	}
	if GraphReliabilitySupportTarget != 2 {
		t.Errorf("GraphReliabilitySupportTarget: got %d, want 2 (dual_path.py L41)", GraphReliabilitySupportTarget)
	}
	if DefaultSparseCandidateK != 80 {
		t.Errorf("DefaultSparseCandidateK: got %d, want 80 (sparse_bm25.py L54)", DefaultSparseCandidateK)
	}
	if DefaultSparseRelationCandidateK != 60 {
		t.Errorf("DefaultSparseRelationCandidateK: got %d, want 60 (sparse_bm25.py L57)", DefaultSparseRelationCandidateK)
	}
	if DefaultFusionRRFK != 60 {
		t.Errorf("DefaultFusionRRFK: got %d, want 60 (dual_path.py L291)", DefaultFusionRRFK)
	}
	if EpisodeFusionRRFK != 60.0 {
		t.Errorf("EpisodeFusionRRFK: got %v, want 60 (episode_retrieval_service.py L18)", EpisodeFusionRRFK)
	}
	if AggregateDefaultRRFK != 60.0 {
		t.Errorf("AggregateDefaultRRFK: got %v, want 60 (aggregate_query_service.py L53)", AggregateDefaultRRFK)
	}
}

// TestGraphEvidenceGroundingFactor 覆盖四级 grounding 衰减常数。
// 出处：core/retrieval/dual_path.py L1886-L1916。
func TestGraphEvidenceGroundingFactor(t *testing.T) {
	paragraph := "小明喜欢在上海喝咖啡"
	cases := []struct {
		name     string
		evidence map[string]any
		want     float64
	}{
		{
			name:     "三段全落地",
			evidence: map[string]any{"type": "relation", "subject": "小明", "predicate": "喜欢", "object": "咖啡"},
			want:     1.0,
		},
		{
			name:     "双实体落地",
			evidence: map[string]any{"type": "relation", "subject": "小明", "predicate": "讨厌", "object": "咖啡"},
			want:     RelationBothEntitiesGroundingFactor,
		},
		{
			name:     "单实体落地",
			evidence: map[string]any{"type": "relation", "subject": "小明", "predicate": "讨厌", "object": "牛奶"},
			want:     RelationSingleEntityGroundingFactor,
		},
		{
			name:     "仅谓词落地",
			evidence: map[string]any{"type": "relation", "subject": "小红", "predicate": "喜欢", "object": "牛奶"},
			want:     RelationPredicateOnlyGroundingFactor,
		},
		{
			name:     "全不落地",
			evidence: map[string]any{"type": "relation", "subject": "小红", "predicate": "讨厌", "object": "牛奶"},
			want:     RelationUngroundedFactor,
		},
		{
			name:     "实体落地",
			evidence: map[string]any{"type": "entity", "name": "咖啡"},
			want:     1.0,
		},
		{
			name:     "实体未落地",
			evidence: map[string]any{"type": "entity", "name": "牛奶"},
			want:     EntityUngroundedFactor,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GraphEvidenceGroundingFactor(tc.evidence, paragraph); math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("grounding factor: got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAggregateGraphEvidenceScore 覆盖图证据聚合权重。
// 出处：core/retrieval/dual_path.py L1701-L1725。
func TestAggregateGraphEvidenceScore(t *testing.T) {
	items := []map[string]any{
		{"type": "relation", "score": 0.5},
		{"type": "relation", "score": 0.2},
		{"type": "entity", "score": 0.3},
	}
	// 0.5 + 0.35*0.2 + 0.20*0.3 = 0.63
	want := 0.5 + GraphEvidenceExtraRelationWt*0.2 + GraphEvidenceEntityWeight*0.3
	got := AggregateGraphEvidenceScore(items)
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("aggregate: got %v, want %v", got, want)
	}
	// 上限裁剪到 1.0
	clipped := AggregateGraphEvidenceScore([]map[string]any{
		{"type": "relation", "score": 0.9},
		{"type": "relation", "score": 0.9},
		{"type": "entity", "score": 0.9},
	})
	if clipped != 1.0 {
		t.Errorf("aggregate clip: got %v, want 1.0", clipped)
	}
}

// TestEstimateGraphReliability 覆盖图可靠性三项加权。
// 出处：core/retrieval/dual_path.py L1730-L1831。
func TestEstimateGraphReliability(t *testing.T) {
	// 两个关系都完全落地、语义一致 → reliability 应接近上界。
	candidate := &Result{
		Hash: "p1", Content: "小明 喜欢 咖啡", Type: "paragraph",
		Score:    0.9,
		Metadata: map[string]any{"score_breakdown": map[string]any{"semantic": 0.9}},
	}
	AppendGraphEvidence(candidate, map[string]any{
		"type": "relation", "hash": "r1", "normalized_score": 0.9, "grounding_factor": 1.0,
	}, 0.9)
	AppendGraphEvidence(candidate, map[string]any{
		"type": "relation", "hash": "r2", "normalized_score": 0.8, "grounding_factor": 1.0,
	}, 0.8)

	estimate := EstimateGraphReliability([]*Result{candidate}, 10)
	if math.Abs(estimate.GroundingQuality-1.0) > 1e-9 {
		t.Errorf("grounding_quality: got %v, want 1.0", estimate.GroundingQuality)
	}
	if math.Abs(estimate.ChannelAgreement-1.0) > 1e-9 {
		t.Errorf("channel_agreement: got %v, want 1.0", estimate.ChannelAgreement)
	}
	if math.Abs(estimate.SupportCoverage-1.0) > 1e-9 {
		t.Errorf("support_coverage: got %v, want 1.0", estimate.SupportCoverage)
	}
	want := GraphReliabilityGroundingWeight + GraphReliabilityAgreementWeight + GraphReliabilitySupportWeight
	if math.Abs(estimate.Score-want) > 1e-9 {
		t.Errorf("reliability: got %v, want %v", estimate.Score, want)
	}
	if estimate.GroundedRelationCount != 2 || estimate.RelationCount != 2 {
		t.Errorf("relation counts: grounded=%d relations=%d", estimate.GroundedRelationCount, estimate.RelationCount)
	}
}

// TestCalibrateDualPoolWeights 覆盖图权重的可靠性缩放（曲线指数 3.0）。
// 出处：core/retrieval/dual_path.py L1833-L1863。
func TestCalibrateDualPoolWeights(t *testing.T) {
	// 无图证据 → reliability=0 → 图权重压到 floor 0.15，释放的权重按比例分给语义/稀疏。
	candidate := &Result{
		Hash: "p1", Content: "无证据", Type: "paragraph", Score: 0.5,
		Metadata: map[string]any{"score_breakdown": map[string]any{"semantic": 0.5}},
	}
	semantic, sparse, graph, estimate := CalibrateDualPoolWeights(
		[]*Result{candidate}, 0.65, 0.20, 0.15, 10)
	if estimate.Score != 0 {
		t.Errorf("reliability: got %v, want 0", estimate.Score)
	}
	if math.Abs(graph-0.15) > 1e-12 {
		t.Errorf("graph weight floor: got %v, want 0.15", graph)
	}
	if math.Abs(semantic-0.65) > 1e-12 || math.Abs(sparse-0.20) > 1e-12 {
		t.Errorf("independent weights changed without range: semantic=%v sparse=%v", semantic, sparse)
	}

	// 图权重高于 floor 时按 reliability^3 缩放。
	withGraph := &Result{
		Hash: "p2", Content: "小明 喜欢 咖啡", Type: "paragraph", Score: 0.9,
		Metadata: map[string]any{"score_breakdown": map[string]any{"semantic": 0.9}},
	}
	AppendGraphEvidence(withGraph, map[string]any{
		"type": "relation", "hash": "r1", "normalized_score": 0.9, "grounding_factor": 1.0,
	}, 0.9)
	semantic2, sparse2, graph2, estimate2 := CalibrateDualPoolWeights(
		[]*Result{withGraph}, 0.65, 0.20, 0.50, 10)
	if estimate2.Score <= 0 {
		t.Fatalf("expected positive reliability, got %v", estimate2.Score)
	}
	wantGraph := GraphReliabilityWeightFloor +
		(0.50-GraphReliabilityWeightFloor)*math.Pow(estimate2.Score, GraphReliabilityCurveExponent)
	if math.Abs(graph2-wantGraph) > 1e-9 {
		t.Errorf("effective graph weight: got %v, want %v", graph2, wantGraph)
	}
	released := 0.50 - graph2
	independent := 0.65 + 0.20
	if math.Abs(semantic2-(0.65+released*(0.65/independent))) > 1e-9 {
		t.Errorf("effective semantic weight: got %v", semantic2)
	}
	if math.Abs(sparse2-(0.20+released*(0.20/independent))) > 1e-9 {
		t.Errorf("effective sparse weight: got %v", sparse2)
	}
}

// TestNormalizeGraphScoresByType 覆盖按类型分组的 min-max 归一化。
// 出处：core/retrieval/dual_path.py L1865-L1884。
func TestNormalizeGraphScoresByType(t *testing.T) {
	input := []struct {
		Type  string
		Hash  string
		Score float64
	}{
		{"relation", "r1", 0.9},
		{"relation", "r2", 0.5},
		{"entity", "e1", 0.3},
		{"entity", "e2", 0.3},
	}
	got := NormalizeGraphScoresByType(input)
	if len(got) != 4 {
		t.Fatalf("length: got %d, want 4", len(got))
	}
	if math.Abs(got[0].NormalizedScore-1.0) > 1e-12 || math.Abs(got[1].NormalizedScore-0.0) > 1e-12 {
		t.Errorf("relation normalization: %v %v", got[0].NormalizedScore, got[1].NormalizedScore)
	}
	// 同分时退化为 clip(raw)（Python: max(0, min(1, raw))）
	if math.Abs(got[2].NormalizedScore-0.3) > 1e-12 {
		t.Errorf("entity tie normalization: got %v, want 0.3", got[2].NormalizedScore)
	}
}
