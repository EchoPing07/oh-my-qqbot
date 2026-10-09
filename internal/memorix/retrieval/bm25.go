package retrieval

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
)

// SparseConfig 对齐 Python SparseBM25Config 的核心面。
// 出处：core/retrieval/sparse_bm25.py L40-L61。
type SparseConfig struct {
	Enabled               bool
	Mode                  string // auto | fallback_only | hybrid
	TokenizerMode         string // jieba | mixed | char_2gram
	CharNgramN            int
	CandidateK            int
	MaxDocLen             int
	EnableRelationSparse  bool
	RelationCandidateK    int
	RelationMaxDocLen     int
	EnableTokenizedShadow bool
	EnableNgramFallback   bool
	EnableLikeFallback    bool
}

// DefaultSparseConfig 返回与 Python 版一致的默认稀疏配置。
func DefaultSparseConfig() SparseConfig {
	return SparseConfig{
		Enabled:               DefaultSparseEnabled,
		Mode:                  DefaultSparseMode,
		TokenizerMode:         DefaultSparseTokenizerMode,
		CharNgramN:            DefaultSparseCharNgramN,
		CandidateK:            DefaultSparseCandidateK,
		MaxDocLen:             DefaultSparseMaxDocLen,
		EnableRelationSparse:  true,
		RelationCandidateK:    DefaultSparseRelationCandidateK,
		RelationMaxDocLen:     DefaultSparseRelationMaxDocLen,
		EnableTokenizedShadow: true,
		EnableNgramFallback:   true,
		EnableLikeFallback:    false,
	}
}

// SparseRow 是稀疏检索的一行命中。
// score 为 BM25 的取反（越大越相关），与 Python 版 `score = -bm25_score` 一致。
//
// 出处：core/retrieval/sparse_bm25.py L598-L615。
type SparseRow struct {
	Hash              string
	Content           string
	Rank              int
	BM25Score         float64
	Score             float64
	MatchedTokenCount int
	MatchedTokenRatio float64
	// 关系行专有字段。
	Subject   string
	Predicate string
	Object    string
}

// BM25Config 是 FTS5 BM25 打分器的可调权重。
// FTS5 默认 bm25(fts, 1.0, 1.0) 已在 SQLite 内实现 k1=1.2/b=0.75；
// Go 侧只暴露列权重以对齐多列场景。
type BM25Config struct {
	// ContentColumnWeight 段落/关系 content 列的权重。
	ContentColumnWeight float64
	// ExtraColumnWeight 其他列的权重（段落 FTS 只有一列时忽略）。
	ExtraColumnWeight float64
}

// DefaultBM25Config 返回 FTS5 默认列权重。
func DefaultBM25Config() BM25Config {
	return BM25Config{ContentColumnWeight: 1.0, ExtraColumnWeight: 1.0}
}

// BM25Scorer 在内存中复现 FTS5 bm25() 的打分（k1=1.2, b=0.75）。
// 用于无 FTS5 环境或需要对候选做二次打分的场景；
// 正式检索优先走 SQLite FTS5（见 FTSStore）。
type BM25Scorer struct {
	// K1 与 B 对齐 SQLite FTS5 内置 bm25 实现。
	K1 float64
	B  float64

	docs      []map[string]int // 每篇文档的 term frequency
	docLen    []int
	df        map[string]int
	avgDocLen float64
}

// NewBM25Scorer 创建空打分器。
func NewBM25Scorer() *BM25Scorer {
	return &BM25Scorer{K1: 1.2, B: 0.75, df: map[string]int{}}
}

// Add 加入一篇文档（tokens 为该文档的分词序列）。
func (s *BM25Scorer) Add(tokens []string) int {
	tf := make(map[string]int, len(tokens))
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		tf[tok]++
	}
	s.docs = append(s.docs, tf)
	s.docLen = append(s.docLen, len(tokens))
	for term := range tf {
		s.df[term]++
	}
	total := 0
	for _, l := range s.docLen {
		total += l
	}
	if len(s.docLen) > 0 {
		s.avgDocLen = float64(total) / float64(len(s.docLen))
	}
	return len(s.docs) - 1
}

// Score 对单个文档计算 BM25（分数越大越相关，与 FTS5 的负值约定相反）。
// 公式：sum over q terms of IDF(t) * (tf * (k1+1)) / (tf + k1*(1 - b + b*dl/avgdl))，
// IDF 使用 FTS5 的 ln((N - df + 0.5) / (df + 0.5)) 形式并做下界保护。
func (s *BM25Scorer) Score(docIndex int, queryTokens []string) float64 {
	if docIndex < 0 || docIndex >= len(s.docs) || len(queryTokens) == 0 {
		return 0
	}
	tf := s.docs[docIndex]
	dl := float64(s.docLen[docIndex])
	avg := s.avgDocLen
	if avg <= 0 {
		avg = 1
	}
	n := float64(len(s.docs))
	score := 0.0
	seen := map[string]struct{}{}
	for _, term := range queryTokens {
		if term == "" {
			continue
		}
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		f := float64(tf[term])
		if f == 0 {
			continue
		}
		df := float64(s.df[term])
		idf := 0.0
		if n > 0 {
			idf = ln((n - df + 0.5) / (df + 0.5))
			if idf < 0 {
				idf = 0
			}
		}
		denom := f + s.K1*(1-s.B+s.B*dl/avg)
		if denom <= 0 {
			continue
		}
		score += idf * (f * (s.K1 + 1)) / denom
	}
	return score
}

// ln 是自然对数（对非正值做 0 保护）。
func ln(x float64) float64 {
	if x <= 0 {
		return 0
	}
	return math.Log(x)
}

// FTSStore 抽象 FTS5 查询能力（由 storage 层实现），避免 retrieval 直接依赖 SQL 连接。
// 出处：core/storage/metadata_fts.py L758-L860。
type FTSStore interface {
	// SearchParagraphsFTS 用 MATCH 表达式检索段落 FTS5 表，返回 BM25 升序结果。
	SearchParagraphsFTS(ctx context.Context, matchQuery string, limit int, allowedIDs []string) ([]SparseRow, error)
	// SearchRelationsFTS 检索关系 FTS5 表。
	SearchRelationsFTS(ctx context.Context, matchQuery string, limit int, allowedIDs []string) ([]SparseRow, error)
}

// SparseIndex 是 BM25 稀疏检索入口，语义对齐 Python SparseBM25Index。
type SparseIndex struct {
	config    SparseConfig
	tokenizer *Tokenizer
	store     FTSStore
}

// NewSparseIndex 构造稀疏索引。
func NewSparseIndex(store FTSStore, config SparseConfig) *SparseIndex {
	if config.TokenizerMode == "" {
		config.TokenizerMode = DefaultSparseTokenizerMode
	}
	if config.CharNgramN < 1 {
		config.CharNgramN = DefaultSparseCharNgramN
	}
	if config.CandidateK < 1 {
		config.CandidateK = DefaultSparseCandidateK
	}
	if config.RelationCandidateK < 1 {
		config.RelationCandidateK = DefaultSparseRelationCandidateK
	}
	return &SparseIndex{
		config:    config,
		tokenizer: NewTokenizer(config.TokenizerMode, config.CharNgramN),
		store:     store,
	}
}

// Config 返回配置副本。
func (s *SparseIndex) Config() SparseConfig { return s.config }

// Tokenizer 暴露分词器（供测试与上层复用）。
func (s *SparseIndex) Tokenizer() *Tokenizer { return s.tokenizer }

// Search 执行段落 BM25 检索（FTS5 + 尾部裁剪 + LIKE 兜底）。
// 出处：core/retrieval/sparse_bm25.py L541-L616 与 dual_path.py L863-L890。
func (s *SparseIndex) Search(ctx context.Context, query string, k int, allowedIDs []string) ([]SparseRow, error) {
	if !s.config.Enabled || s.store == nil {
		return nil, nil
	}
	tokens := s.tokenizer.Tokenize(query)
	matchTokens := MatchTokens(tokens)
	matchQuery := BuildMatchQuery(matchTokens)
	if matchQuery == "" {
		return nil, nil
	}
	limit := maxInt(1, k)
	if allowedIDs != nil && len(allowedIDs) == 0 {
		return nil, nil
	}
	rows, err := s.store.SearchParagraphsFTS(ctx, matchQuery, limit, allowedIDs)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		rows = substringFallback(matchTokens, limit, allowedIDs, s.config.MaxDocLen)
	}
	return s.rankRows(rows, matchTokens, s.config.MaxDocLen), nil
}

// SearchRelations 执行关系 BM25 检索。
// 出处：core/retrieval/sparse_bm25.py L618-L664。
func (s *SparseIndex) SearchRelations(ctx context.Context, query string, k int, allowedIDs []string) ([]SparseRow, error) {
	if !s.config.Enabled || !s.config.EnableRelationSparse || s.store == nil {
		return nil, nil
	}
	tokens := s.tokenizer.Tokenize(query)
	matchTokens := MatchTokens(tokens)
	matchQuery := BuildMatchQuery(matchTokens)
	if matchQuery == "" {
		return nil, nil
	}
	limit := maxInt(1, k)
	if allowedIDs != nil && len(allowedIDs) == 0 {
		return nil, nil
	}
	rows, err := s.store.SearchRelationsFTS(ctx, matchQuery, limit, allowedIDs)
	if err != nil {
		return nil, err
	}
	return s.rankRows(rows, matchTokens, s.config.RelationMaxDocLen), nil
}

// rankRows 填充 rank/score/matched_token 统计并按 BM25 升序稳定排序。
func (s *SparseIndex) rankRows(rows []SparseRow, matchTokens []string, maxDocLen int) []SparseRow {
	out := make([]SparseRow, 0, len(rows))
	tokenCount := maxInt(1, len(matchTokens))
	for _, row := range rows {
		content := row.Content
		if maxDocLen > 0 && len([]rune(content)) > maxDocLen {
			content = string([]rune(content)[:maxDocLen])
		}
		lowered := strings.ToLower(content)
		seen := map[string]struct{}{}
		matched := 0
		for _, tok := range matchTokens {
			if strings.Contains(lowered, tok) {
				if _, ok := seen[tok]; ok {
					continue
				}
				seen[tok] = struct{}{}
				matched++
			}
		}
		row.Content = content
		row.Score = -row.BM25Score
		row.MatchedTokenCount = matched
		row.MatchedTokenRatio = float64(matched) / float64(tokenCount)
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].BM25Score != out[j].BM25Score {
			return out[i].BM25Score < out[j].BM25Score
		}
		return out[i].Hash < out[j].Hash
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

// TrimSparseTail 裁剪稀疏候选尾部：避免只命中一个弱 token 的结果在 RRF 中拿到过高 rank。
// 出处：core/retrieval/dual_path.py L901-L946。
func TrimSparseTail(rows []SparseRow) []SparseRow {
	if len(rows) <= 2 {
		return rows
	}
	top := rows[0].Score
	if top <= 0 {
		return rows[:2]
	}
	floor := top * SparseTailRelativeFloor
	kept := make([]SparseRow, 0, len(rows))
	for index, row := range rows {
		if index < 2 {
			kept = append(kept, row)
			continue
		}
		if row.Score >= floor ||
			row.MatchedTokenCount >= SparseTailMinMatchedTokens ||
			(row.MatchedTokenCount >= 2 && row.MatchedTokenRatio >= SparseTailMinMatchedRatio) {
			kept = append(kept, row)
		}
	}
	return kept
}

// ShouldUseSparse 判定 auto 模式下是否需要 BM25 回落。
// 出处：core/retrieval/dual_path.py L573-L600。
func (s *SparseIndex) ShouldUseSparse(embeddingOK bool, vectorScores []float64) bool {
	if !s.config.Enabled || s.store == nil {
		return false
	}
	switch s.config.Mode {
	case "hybrid", "fallback_only":
		return true
	}
	if !embeddingOK {
		return true
	}
	if len(vectorScores) == 0 {
		return true
	}
	best := vectorScores[0]
	for _, score := range vectorScores[1:] {
		if score > best {
			best = score
		}
	}
	return best < SparseAutoMinBestScore
}

// substringFallback 在 FTS5 因分词不一致召回为空时退化为子串匹配。
// 出处：core/retrieval/sparse_bm25.py L453-L539（enable_like_fallback 关闭时返回空）。
func substringFallback(tokens []string, limit int, allowedIDs []string, maxDocLen int) []SparseRow {
	// Python 版默认 enable_like_fallback=False 且 ngram 倒排表未迁移，因此这里保持空返回。
	_ = tokens
	_ = limit
	_ = allowedIDs
	_ = maxDocLen
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ErrFTSTable 表示 FTS5 虚表不可用（此时稀疏检索会静默退化为空结果，
// 与 Python SparseBM25Index.search 在 backend 未就绪时返回 [] 的行为一致）。
var ErrFTSTable = errors.New("memorix: FTS5 虚表不可用")

// FTSUnavailable 判定错误是否为「FTS 不可用」。
func FTSUnavailable(err error) bool { return errors.Is(err, ErrFTSTable) }
