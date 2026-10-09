package retrieval

import (
	"context"
	"regexp"
	"sort"
	"strings"
)

// PosteriorConfig 对齐 Python PosteriorGraphConfig（后验图补位配置）。
// 出处：core/retrieval/posterior_graph.py L20-L63。
type PosteriorConfig struct {
	Enabled                       bool
	DropRatio                     float64
	MinCoreResults                int
	MaxGraphSlots                 int
	GateScanTopK                  int
	GroundedConfidenceThreshold   float64
	IncidentalConfidenceThreshold float64
	MinQueryTokenCoverage         float64
	IncidentalQueryRelevance      float64
	IncidentalCoreOverlap         float64
	IncidentalSpecificity         float64
	QueryWeight                   float64
	NoveltyWeight                 float64
	ComplementarityWeight         float64
	SpecificityWeight             float64
	GapFillWeight                 float64
	MaxCandidateTokens            int
}

// DefaultPosteriorConfig 返回照抄 Python 的后验图门默认配置。
func DefaultPosteriorConfig() PosteriorConfig {
	return PosteriorConfig{
		Enabled:                       DefaultPosteriorEnabled,
		DropRatio:                     DefaultPosteriorDropRatio,
		MinCoreResults:                DefaultPosteriorMinCoreResults,
		MaxGraphSlots:                 DefaultPosteriorMaxGraphSlots,
		GateScanTopK:                  DefaultPosteriorGateScanTopK,
		GroundedConfidenceThreshold:   DefaultPosteriorGroundedConfidence,
		IncidentalConfidenceThreshold: DefaultPosteriorIncidentalConfidence,
		MinQueryTokenCoverage:         DefaultPosteriorMinQueryTokenCover,
		IncidentalQueryRelevance:      DefaultPosteriorIncidentalRelevance,
		IncidentalCoreOverlap:         DefaultPosteriorIncidentalOverlap,
		IncidentalSpecificity:         DefaultPosteriorIncidentalSpecific,
		QueryWeight:                   DefaultPosteriorQueryWeight,
		NoveltyWeight:                 DefaultPosteriorNoveltyWeight,
		ComplementarityWeight:         DefaultPosteriorComplementarityWt,
		SpecificityWeight:             DefaultPosteriorSpecificityWeight,
		GapFillWeight:                 DefaultPosteriorGapFillWeight,
		MaxCandidateTokens:            DefaultPosteriorMaxCandidateTokens,
	}
}

// tokenPattern 对齐 Python _TOKEN_PATTERN = [A-Za-z0-9_]+|[\x{4e00}-\x{9fff}]+。
// 出处：core/retrieval/posterior_graph.py L11。
var posteriorTokenPattern = regexp.MustCompile(`[A-Za-z0-9_]+|[\x{4e00}-\x{9fff}]+`)

// broadPredicates 对齐 Python _BROAD_PREDICATES。
// 出处：core/retrieval/posterior_graph.py L12-L21。
var broadPredicates = map[string]struct{}{
	"contains_fact": {},
	"describe":      {},
	"describes":     {},
	"description":   {},
	"mention":       {},
	"mentions":      {},
	"summary":       {},
	"summarizes":    {},
}

// CompetitionProfile 是查询/候选/核心集的竞争画像。
// 出处：core/retrieval/posterior_graph.py L64-L69。
type CompetitionProfile struct {
	Text     string
	Tokens   map[string]struct{}
	Entities map[string]struct{}
}

// PosteriorDecision 记录一次后验图门的决策（供调试与测试断言）。
// 出处：core/retrieval/posterior_graph.py L634-L781（_posterior_graph_gate_last_decision）。
type PosteriorDecision struct {
	Enabled            bool
	Bucket             string
	GroundedSeeds      []string
	IncidentalSeeds    []string
	SelectedSeedType   string
	NeedForGraph       bool
	NeedReason         string
	RAGConfidence      float64
	GraphResultCount   int
	FilteredGraphCount int
}

// ApplyPosteriorGraphGate 在融合结果之后补位图候选（默认关闭，与 Python 一致）。
// 出处：core/retrieval/posterior_graph.py L634-L781。
func (r *Retriever) ApplyPosteriorGraphGate(
	ctx context.Context,
	query string,
	baseResults []*Result,
	topK int,
	temporal *TemporalQueryOptions,
	intent RelationIntentContext,
	scope *Scope,
) ([]*Result, error) {
	cfg := r.Config.PosteriorGraph
	if !cfg.Enabled {
		return truncateResults(baseResults, topK), nil
	}
	if len(baseResults) == 0 {
		return nil, nil
	}
	if topK < 1 {
		topK = 1
	}
	alpha := r.Config.Alpha
	if intent.AlphaOverride != nil {
		alpha = *intent.AlphaOverride
	}
	ragConfidence := 0.0
	if len(baseResults) > 0 {
		ragConfidence = baseResults[0].Score
	}

	queryProfile := r.buildQueryProfile(ctx, query, cfg.MaxCandidateTokens)
	seedEvidence := r.extractSeedEvidence(ctx, queryProfile, baseResults, cfg.GateScanTopK, cfg.MaxCandidateTokens)
	groundedSeeds := groundedSeedNames(seedEvidence, 2)
	incidentalSeeds := incidentalSeedNames(seedEvidence, 2)

	cliff := findScoreCliff(truncateResults(baseResults, topK), cfg.DropRatio, cfg.MinCoreResults)
	coreResults := truncateResults(baseResults, cliff)
	coreProfile := r.buildCoreProfile(ctx, coreResults, cfg.MaxCandidateTokens)
	coreProfiles := make([]*CompetitionProfile, 0, len(coreResults))
	for _, item := range coreResults {
		coreProfiles = append(coreProfiles, r.buildCandidateProfile(ctx, item, cfg.MaxCandidateTokens))
	}
	needGraph, _ := needForGraph(queryProfile, coreProfile, coreProfiles, groundedSeeds, ragConfidence, cfg)

	seedType := "none"
	var seedNames []string
	switch {
	case len(groundedSeeds) > 0 && needGraph:
		seedType = "grounded"
		seedNames = groundedSeeds
	case len(groundedSeeds) == 0 && len(incidentalSeeds) > 0 && ragConfidence < cfg.IncidentalConfidenceThreshold:
		seedType = "incidental"
		seedNames = incidentalSeeds
	}
	if len(seedNames) == 0 {
		return truncateResults(baseResults, topK), nil
	}

	graphResults, err := r.BuildGraphResultsFromSeeds(ctx, seedNames, temporal, alpha, scope)
	if err != nil {
		return nil, err
	}
	if seedType == "incidental" {
		filtered := make([]*Result, 0, len(graphResults))
		for _, item := range graphResults {
			if r.passesIncidentalHighBar(ctx, item, queryProfile, coreProfile, cfg) {
				filtered = append(filtered, item)
			}
		}
		graphResults = filtered
	}
	if len(graphResults) == 0 {
		return truncateResults(baseResults, topK), nil
	}
	return r.competitionMerge(ctx, query, baseResults, graphResults, topK, cfg), nil
}

// findScoreCliff 找分数断崖位置（核心集边界）。
// 出处：core/retrieval/posterior_graph.py L318-L344。
func findScoreCliff(results []*Result, dropRatio float64, minCoreResults int) int {
	if len(results) == 0 {
		return 0
	}
	if len(results) <= minCoreResults {
		return len(results)
	}
	for index := 1; index < len(results); index++ {
		prev := results[index-1].Score
		if prev < 1e-8 {
			prev = 1e-8
		}
		drop := prev - results[index].Score
		if drop/prev > dropRatio {
			return maxInt(minCoreResults, index)
		}
	}
	fallback := maxInt(minCoreResults, len(results)/2)
	return minInt(len(results), fallback)
}

func truncateResults(results []*Result, topK int) []*Result {
	if topK < 0 {
		topK = 0
	}
	if len(results) > topK {
		return results[:topK]
	}
	return results
}

// tokenizeForCompetition 对齐 Python _tokenize_for_competition：
// 按正则切块，CJK 块用 jieba 搜索模式再切，过滤长度 <= 1 的 token。
// 出处：core/retrieval/posterior_graph.py L89-L112。
func (r *Retriever) tokenizeForCompetition(text string, maxTokens int, tokenizer *Tokenizer) []string {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return nil
	}
	var tokens []string
	for _, chunk := range posteriorTokenPattern.FindAllString(normalized, -1) {
		if isCJKChunk(chunk) {
			cut := chunk
			if tokenizer != nil {
				cut = strings.Join(tokenizer.Tokenize(chunk), " ")
			}
			for _, item := range strings.Fields(cut) {
				if item = strings.TrimSpace(strings.ToLower(item)); item != "" {
					tokens = append(tokens, item)
				}
			}
			continue
		}
		tokens = append(tokens, chunk)
	}
	filtered := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if len([]rune(token)) <= 1 {
			continue
		}
		filtered = append(filtered, token)
		if maxTokens > 0 && len(filtered) >= maxTokens {
			break
		}
	}
	return filtered
}

func isCJKChunk(token string) bool {
	if token == "" {
		return false
	}
	for _, ch := range token {
		if ch < 0x4E00 || ch > 0x9FFF {
			return false
		}
	}
	return true
}

// resultTextForEntityMatch 拼装候选的实体匹配文本。
// 出处：core/retrieval/posterior_graph.py L120-L130。
func resultTextForEntityMatch(result *Result) string {
	parts := []string{result.Content}
	for _, key := range []string{"subject", "object", "context_title", "benchmark_title"} {
		if v := stringify(result.Metadata[key]); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, "\n")
}

// candidateTextForCompetition 拼装候选的竞争文本。
// 出处：core/retrieval/posterior_graph.py L132-L139。
func candidateTextForCompetition(result *Result) string {
	parts := []string{resultTextForEntityMatch(result)}
	if v := stringify(result.Metadata["predicate"]); v != "" {
		parts = append(parts, v)
	}
	return strings.Join(parts, "\n")
}

// extractCandidateEntities 抽取候选的实体集合。
// 出处：core/retrieval/posterior_graph.py L141-L162。
func (r *Retriever) extractCandidateEntities(ctx context.Context, result *Result) map[string]struct{} {
	entities := map[string]struct{}{}
	found, err := r.ExtractEntities(ctx, candidateTextForCompetition(result))
	if err == nil {
		for name := range found {
			if normalized := strings.ToLower(strings.TrimSpace(name)); normalized != "" {
				entities[normalized] = struct{}{}
			}
		}
	}
	for _, key := range []string{"benchmark_title", "context_title", "object", "subject"} {
		if normalized := strings.ToLower(strings.TrimSpace(stringify(result.Metadata[key]))); normalized != "" {
			entities[normalized] = struct{}{}
		}
	}
	return entities
}

func (r *Retriever) buildQueryProfile(ctx context.Context, query string, maxTokens int) *CompetitionProfile {
	tokens := r.tokenizeForCompetition(query, maxTokens, r.Index.tokenizerOrNil())
	entities := map[string]struct{}{}
	if found, err := r.ExtractEntities(ctx, query); err == nil {
		for name := range found {
			if normalized := strings.ToLower(strings.TrimSpace(name)); normalized != "" {
				entities[normalized] = struct{}{}
			}
		}
	}
	return &CompetitionProfile{Text: query, Tokens: toSet(tokens), Entities: entities}
}

func (r *Retriever) buildCandidateProfile(ctx context.Context, result *Result, maxTokens int) *CompetitionProfile {
	text := candidateTextForCompetition(result)
	return &CompetitionProfile{
		Text:     text,
		Tokens:   toSet(r.tokenizeForCompetition(text, maxTokens, r.Index.tokenizerOrNil())),
		Entities: r.extractCandidateEntities(ctx, result),
	}
}

func (r *Retriever) buildCoreProfile(ctx context.Context, results []*Result, maxTokens int) *CompetitionProfile {
	parts := make([]string, 0, len(results))
	tokens := map[string]struct{}{}
	entities := map[string]struct{}{}
	for _, result := range results {
		profile := r.buildCandidateProfile(ctx, result, maxTokens)
		parts = append(parts, profile.Text)
		for token := range profile.Tokens {
			tokens[token] = struct{}{}
		}
		for entity := range profile.Entities {
			entities[entity] = struct{}{}
		}
	}
	return &CompetitionProfile{Text: strings.Join(parts, "\n"), Tokens: tokens, Entities: entities}
}

// SeedEvidence 是一条种子证据。
// 出处：core/retrieval/posterior_graph.py L72-L78。
type SeedEvidence struct {
	Name         string
	Strength     string
	SupportCount int
	RankHint     int
}

// extractSeedEvidence 从 top-k 结果中抽取 grounded / incidental 种子。
// 出处：core/retrieval/posterior_graph.py L347-L388。
func (r *Retriever) extractSeedEvidence(
	ctx context.Context, queryProfile *CompetitionProfile, results []*Result, scanTopK, maxTokens int,
) []SeedEvidence {
	if scanTopK < 1 {
		scanTopK = 1
	}
	top := truncateResults(results, scanTopK)
	type key struct{ name, strength string }
	scoreMap := map[key]*SeedEvidence{}
	for rank, item := range top {
		profile := r.buildCandidateProfile(ctx, item, maxTokens)
		for entity := range profile.Entities {
			strength := "incidental"
			if _, ok := queryProfile.Entities[entity]; ok {
				strength = "grounded"
			}
			k := key{name: entity, strength: strength}
			existing, ok := scoreMap[k]
			if !ok {
				scoreMap[k] = &SeedEvidence{
					Name:         entity,
					Strength:     strength,
					SupportCount: 1,
					RankHint:     rank + 1,
				}
				continue
			}
			existing.SupportCount++
			if rank+1 < existing.RankHint {
				existing.RankHint = rank + 1
			}
		}
	}
	out := make([]SeedEvidence, 0, len(scoreMap))
	for _, item := range scoreMap {
		out = append(out, *item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := 0, 0
		if out[i].Strength != "grounded" {
			si = 1
		}
		if out[j].Strength != "grounded" {
			sj = 1
		}
		if si != sj {
			return si < sj
		}
		if out[i].SupportCount != out[j].SupportCount {
			return out[i].SupportCount > out[j].SupportCount
		}
		if out[i].RankHint != out[j].RankHint {
			return out[i].RankHint < out[j].RankHint
		}
		if len([]rune(out[i].Name)) != len([]rune(out[j].Name)) {
			return len([]rune(out[i].Name)) > len([]rune(out[j].Name))
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func groundedSeedNames(evidence []SeedEvidence, limit int) []string {
	var out []string
	for _, item := range evidence {
		if item.Strength != "grounded" {
			continue
		}
		out = append(out, item.Name)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func incidentalSeedNames(evidence []SeedEvidence, limit int) []string {
	var out []string
	for _, item := range evidence {
		if item.Strength != "incidental" {
			continue
		}
		out = append(out, item.Name)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// needForGraph 判定是否需要用图补位。
// 出处：core/retrieval/posterior_graph.py L399-L425。
func needForGraph(
	queryProfile, coreProfile *CompetitionProfile,
	coreProfiles []*CompetitionProfile,
	groundedSeeds []string,
	ragConfidence float64,
	cfg PosteriorConfig,
) (bool, string) {
	uncovered := diffSet(queryProfile.Entities, coreProfile.Entities)
	if len(uncovered) > 0 {
		return true, "uncovered_query_entities"
	}
	if len(groundedSeeds) >= 2 {
		bridgeTargets := map[string]struct{}{}
		for _, name := range groundedSeeds {
			if len(bridgeTargets) >= 2 {
				break
			}
			bridgeTargets[name] = struct{}{}
		}
		sameCoreHit := false
		for _, profile := range coreProfiles {
			if len(intersectSet(profile.Entities, bridgeTargets)) >= 2 {
				sameCoreHit = true
				break
			}
		}
		if !sameCoreHit {
			return true, "grounded_bridge_gap"
		}
	}
	tokenCoverage := safeRatio(len(intersectSet(coreProfile.Tokens, queryProfile.Tokens)), len(queryProfile.Tokens))
	if len(groundedSeeds) > 0 && tokenCoverage < cfg.MinQueryTokenCoverage {
		return true, "low_core_query_coverage"
	}
	if len(groundedSeeds) > 0 && ragConfidence < cfg.GroundedConfidenceThreshold {
		return true, "low_confidence_grounded"
	}
	return false, "core_already_sufficient"
}

// passesIncidentalHighBar 判定 incidental 种子候选是否通过高门槛。
// 出处：core/retrieval/posterior_graph.py L428-L454。
func (r *Retriever) passesIncidentalHighBar(
	ctx context.Context, candidate *Result,
	queryProfile, coreProfile *CompetitionProfile, cfg PosteriorConfig,
) bool {
	profile := r.buildCandidateProfile(ctx, candidate, cfg.MaxCandidateTokens)
	if len(intersectSet(profile.Entities, diffSet(queryProfile.Entities, coreProfile.Entities))) > 0 {
		return true
	}
	queryRelevance := computeQueryRelevance(profile, queryProfile)
	specificity := computeSpecificity(profile, candidate)
	overlap := coreOverlap(profile, coreProfile)
	gapFill := computeGapFill(profile, queryProfile, coreProfile)
	return queryRelevance >= cfg.IncidentalQueryRelevance &&
		specificity >= cfg.IncidentalSpecificity &&
		overlap <= cfg.IncidentalCoreOverlap &&
		gapFill > 0
}

// computeQueryRelevance 出处：core/retrieval/posterior_graph.py L187-L193。
func computeQueryRelevance(candidate, query *CompetitionProfile) float64 {
	entityHit := safeRatio(len(intersectSet(candidate.Entities, query.Entities)), len(query.Entities))
	tokenHit := safeRatio(len(intersectSet(candidate.Tokens, query.Tokens)), len(query.Tokens))
	if len(query.Entities) > 0 {
		return ClipUnit(PosteriorEntityHitWeight*entityHit + PosteriorTokenHitWeight*tokenHit)
	}
	return ClipUnit(maxFloat(entityHit, tokenHit))
}

// computeNovelty 出处：core/retrieval/posterior_graph.py L196-L199。
func computeNovelty(candidate, core *CompetitionProfile) float64 {
	entityNovelty := safeRatio(len(diffSet(candidate.Entities, core.Entities)), len(candidate.Entities))
	tokenNovelty := safeRatio(len(diffSet(candidate.Tokens, core.Tokens)), len(candidate.Tokens))
	return ClipUnit(0.5*entityNovelty + 0.5*tokenNovelty)
}

// computeComplementarity 出处：core/retrieval/posterior_graph.py L202-L216。
func computeComplementarity(candidate, core *CompetitionProfile, queryRelevance float64) float64 {
	if len(core.Tokens) == 0 && len(core.Entities) == 0 {
		return ClipUnit(queryRelevance)
	}
	entityOverlap := safeRatio(len(intersectSet(candidate.Entities, core.Entities)), len(candidate.Entities))
	tokenOverlap := safeRatio(len(intersectSet(candidate.Tokens, core.Tokens)), len(candidate.Tokens))
	coreOverlapValue := 0.5*entityOverlap + 0.5*tokenOverlap
	sweetSpot := 1.0 - abs(coreOverlapValue-PosteriorCoreOverlapSweetSpot)/PosteriorCoreOverlapSweetSpot
	return ClipUnit(maxFloat(0, sweetSpot) * maxFloat(queryRelevance, 0.2))
}

// computeSpecificity 出处：core/retrieval/posterior_graph.py L219-L236。
func computeSpecificity(candidate *CompetitionProfile, result *Result) float64 {
	tokenCount := maxInt(1, len(candidate.Tokens))
	entityDensity := ClipUnit(safeRatio(len(candidate.Entities), tokenCount) * PosteriorSpecificityEntityScale)
	brevity := 1.0 - minFloat(1.0, maxFloat(0, float64(tokenCount)-PosteriorBrevityTokenBudget)/PosteriorBrevityTokenBudget)
	predicateBonus := 0.0
	predicate := strings.ToLower(strings.TrimSpace(stringify(result.Metadata["predicate"])))
	if predicate != "" {
		if _, broad := broadPredicates[predicate]; broad {
			predicateBonus = PosteriorBroadPredicatePenalty
		} else if result.Type == "relation" {
			predicateBonus = PosteriorRelationPredicateBonus
		}
	}
	return ClipUnit(PosteriorEntityDensityWeight*entityDensity + PosteriorBrevityWeight*brevity + predicateBonus)
}

// computeGapFill 出处：core/retrieval/posterior_graph.py L239-L251。
func computeGapFill(candidate, query, core *CompetitionProfile) float64 {
	missingEntities := diffSet(query.Entities, core.Entities)
	missingTokens := diffSet(query.Tokens, core.Tokens)
	entityFill := safeRatio(len(intersectSet(candidate.Entities, missingEntities)), len(missingEntities))
	tokenFill := safeRatio(len(intersectSet(candidate.Tokens, missingTokens)), len(missingTokens))
	if len(missingEntities) > 0 {
		return ClipUnit(0.7*entityFill + 0.3*tokenFill)
	}
	return ClipUnit(maxFloat(entityFill, tokenFill))
}

// coreOverlap 出处：core/retrieval/posterior_graph.py L254-L258。
func coreOverlap(candidate, core *CompetitionProfile) float64 {
	entityOverlap := safeRatio(len(intersectSet(candidate.Entities, core.Entities)), len(candidate.Entities))
	tokenOverlap := safeRatio(len(intersectSet(candidate.Tokens, core.Tokens)), len(candidate.Tokens))
	return ClipUnit(0.5*entityOverlap + 0.5*tokenOverlap)
}

// computeCompetitionScore 出处：core/retrieval/posterior_graph.py L261-L293。
func (r *Retriever) computeCompetitionScore(
	ctx context.Context, candidate *Result,
	queryProfile, coreProfile *CompetitionProfile, cfg PosteriorConfig,
) (float64, map[string]any) {
	profile := r.buildCandidateProfile(ctx, candidate, cfg.MaxCandidateTokens)
	queryRelevance := computeQueryRelevance(profile, queryProfile)
	novelty := computeNovelty(profile, coreProfile)
	complementarity := computeComplementarity(profile, coreProfile, queryRelevance)
	specificity := computeSpecificity(profile, candidate)
	gapFill := computeGapFill(profile, queryProfile, coreProfile)
	final := cfg.QueryWeight*queryRelevance +
		cfg.NoveltyWeight*novelty +
		cfg.ComplementarityWeight*complementarity +
		cfg.SpecificityWeight*specificity +
		cfg.GapFillWeight*gapFill
	clipped := ClipUnit(final)
	breakdown := map[string]any{
		"query_relevance":   round4(queryRelevance),
		"novelty":           round4(novelty),
		"complementarity":   round4(complementarity),
		"specificity":       round4(specificity),
		"gap_fill":          round4(gapFill),
		"competition_score": round4(clipped),
	}
	return clipped, breakdown
}

// competitionMerge 在尾部槽位竞争图候选。
// 出处：core/retrieval/posterior_graph.py L522-L632。
func (r *Retriever) competitionMerge(
	ctx context.Context, query string, baseResults, graphResults []*Result, topK int, cfg PosteriorConfig,
) []*Result {
	if topK < 1 {
		topK = 1
	}
	ranked := truncateResults(baseResults, topK)
	if len(ranked) == 0 || len(graphResults) == 0 {
		return ranked
	}
	cliff := findScoreCliff(ranked, cfg.DropRatio, cfg.MinCoreResults)
	core := truncateResults(ranked, cliff)
	remainingSlots := maxInt(0, topK-len(core))
	graphSlotLimit := minInt(remainingSlots, cfg.MaxGraphSlots)
	if remainingSlots <= 0 || graphSlotLimit <= 0 {
		return truncateResults(ranked, topK)
	}

	coreParagraphs := map[string]struct{}{}
	selected := map[string]struct{}{}
	for _, item := range core {
		selected[item.Hash] = struct{}{}
		if item.Type == "paragraph" {
			coreParagraphs[item.Hash] = struct{}{}
		}
	}

	graphCandidateIDs := map[*Result]struct{}{}
	filteredGraph := make([]*Result, 0, len(graphResults))
	for _, item := range graphResults {
		if _, ok := selected[item.Hash]; ok {
			continue
		}
		linked, err := r.Source.ParagraphsByRelation(ctx, item.Hash)
		if err == nil {
			skip := false
			for _, paragraph := range linked {
				if _, ok := coreParagraphs[paragraph.Hash]; ok {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
		}
		filteredGraph = append(filteredGraph, item)
		graphCandidateIDs[item] = struct{}{}
	}

	tail := make([]*Result, 0, len(ranked))
	for _, item := range ranked[cliff:minInt(topK, len(ranked))] {
		if _, ok := selected[item.Hash]; ok {
			continue
		}
		tail = append(tail, item)
	}
	tail = append(tail, filteredGraph...)

	queryProfile := r.buildQueryProfile(ctx, query, cfg.MaxCandidateTokens)
	coreProfile := r.buildCoreProfile(ctx, core, cfg.MaxCandidateTokens)

	type scored struct {
		item  *Result
		score float64
	}
	scoredCandidates := make([]scored, 0, len(tail))
	for _, item := range tail {
		score, breakdown := r.computeCompetitionScore(ctx, item, queryProfile, coreProfile, cfg)
		meta := item.Metadata
		if meta == nil {
			meta = map[string]any{}
			item.Metadata = meta
		}
		meta["posterior_original_score"] = round4(item.Score)
		meta["posterior_competition_breakdown"] = breakdown
		meta["posterior_competition_source"] = "posterior_graph_gate"
		scoredCandidates = append(scoredCandidates, scored{item: item, score: score})
	}
	sort.SliceStable(scoredCandidates, func(i, j int) bool {
		if scoredCandidates[i].score != scoredCandidates[j].score {
			return scoredCandidates[i].score > scoredCandidates[j].score
		}
		ri, rj := 0, 0
		if scoredCandidates[i].item.Type == "relation" {
			ri = 1
		}
		if scoredCandidates[j].item.Type == "relation" {
			rj = 1
		}
		return ri > rj
	})

	var winners []*Result
	seen := map[string]struct{}{}
	for hash := range selected {
		seen[hash] = struct{}{}
	}
	selectedGraphCount := 0
	for _, entry := range scoredCandidates {
		if _, ok := seen[entry.item.Hash]; ok {
			continue
		}
		_, isGraphCandidate := graphCandidateIDs[entry.item]
		if isGraphCandidate && selectedGraphCount >= graphSlotLimit {
			continue
		}
		winners = append(winners, entry.item)
		seen[entry.item.Hash] = struct{}{}
		if isGraphCandidate {
			selectedGraphCount++
		}
		if len(winners) >= remainingSlots {
			break
		}
	}
	// 图候选没有赢得竞争时保持原始结果（避免无意义重排尾部）。
	if selectedGraphCount == 0 {
		return truncateResults(ranked, topK)
	}
	for _, item := range ranked[cliff:minInt(topK, len(ranked))] {
		if len(winners) >= remainingSlots {
			break
		}
		if _, ok := seen[item.Hash]; ok {
			continue
		}
		winners = append(winners, item)
		seen[item.Hash] = struct{}{}
	}
	out := append(append([]*Result{}, core...), winners...)
	return truncateResults(out, topK)
}

// ---------------------------------------------------------------------------
// 集合工具
// ---------------------------------------------------------------------------

func toSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}

func intersectSet(a, b map[string]struct{}) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range a {
		if _, ok := b[k]; ok {
			out[k] = struct{}{}
		}
	}
	return out
}

func diffSet(a, b map[string]struct{}) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range a {
		if _, ok := b[k]; !ok {
			out[k] = struct{}{}
		}
	}
	return out
}

func safeRatio(numerator, denominator int) float64 {
	if denominator <= 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// tokenizerOrNil 暴露检索器内部的稀疏分词器（后验图门复用 jieba 搜索模式）。
func (s *SparseIndex) tokenizerOrNil() *Tokenizer {
	if s == nil {
		return nil
	}
	return s.tokenizer
}
