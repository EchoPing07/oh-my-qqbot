package retrieval

import (
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/go-ego/gse"
)

// Tokenizer 对齐 Python 版 SparseBM25Index._tokenize：
// jieba 分词（Go 用 gse 等价）+ 短语正则 + char n-gram 兜底。
//
// 出处：core/retrieval/sparse_bm25.py L393-L426
type Tokenizer struct {
	mu      sync.Mutex
	seg     gse.Segmenter
	loaded  bool
	loadErr error
	ngramN  int
	mode    string
}

var (
	phrasePattern     = regexp.MustCompile(`[A-Za-z0-9_]+|[\x{4e00}-\x{9fff}]{2,}`)
	whitespacePattern = regexp.MustCompile(`\s+`)
	cjkSinglePattern  = regexp.MustCompile(`^[\x{4e00}-\x{9fff}]$`)
)

// NewTokenizer 创建分词器；mode 取 jieba|mixed|char_2gram（对齐 Python tokenizer_mode）。
func NewTokenizer(mode string, ngramN int) *Tokenizer {
	if ngramN < 1 {
		ngramN = DefaultSparseCharNgramN
	}
	normalized := strings.ToLower(strings.TrimSpace(mode))
	switch normalized {
	case "jieba", "mixed", "char_2gram":
	default:
		normalized = DefaultSparseTokenizerMode
	}
	return &Tokenizer{ngramN: ngramN, mode: normalized}
}

// ensureSegmenter 懒加载 gse 内置词典（等价 jieba 初始化）。
func (t *Tokenizer) ensureSegmenter() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loaded {
		return t.loadErr == nil
	}
	t.loaded = true
	seg, err := gse.NewEmbed("zh")
	if err != nil {
		t.loadErr = err
		return false
	}
	seg.SkipLog = true
	t.seg = seg
	return true
}

// Tokenize 返回去重后的 token 序列（对齐 Python `list(dict.fromkeys(...))`）。
func (t *Tokenizer) Tokenize(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var tokens []string
	switch t.mode {
	case "char_2gram":
		tokens = charNgram(text, t.ngramN)
	default:
		tokens = t.tokenizeJieba(text)
		if t.mode == "mixed" {
			tokens = append(tokens, charNgram(text, t.ngramN)...)
		}
		tokens = append(tokens, tokenizePhrases(text)...)
		if len(tokens) == 0 {
			tokens = append(charNgram(text, t.ngramN), tokenizePhrases(text)...)
		}
	}
	return dedupe(tokens)
}

func (t *Tokenizer) tokenizeJieba(text string) []string {
	if !t.ensureSegmenter() {
		return nil
	}
	t.mu.Lock()
	seg := t.seg
	t.mu.Unlock()
	out := seg.CutSearch(text, true)
	tokens := make([]string, 0, len(out))
	for _, tok := range out {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok != "" {
			tokens = append(tokens, tok)
		}
	}
	return tokens
}

func tokenizePhrases(text string) []string {
	lowered := strings.ToLower(text)
	matches := phrasePattern.FindAllString(lowered, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

func charNgram(text string, n int) []string {
	compact := whitespacePattern.ReplaceAllString(strings.ToLower(text), "")
	if compact == "" {
		return nil
	}
	runes := []rune(compact)
	if len(runes) < n {
		return []string{compact}
	}
	out := make([]string, 0, len(runes)-n+1)
	for i := 0; i+n <= len(runes); i++ {
		out = append(out, string(runes[i:i+n]))
	}
	return out
}

func dedupe(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// isLowSignalQueryToken 识别长查询中容易造成 OR 误召回的中文单字。
// 出处：core/retrieval/sparse_bm25.py L421-L425
func isLowSignalQueryToken(token string) bool {
	return len([]rune(token)) == 1 && cjkSinglePattern.MatchString(token)
}

// MatchTokens 生成实际参与匹配的 token；若过滤后为空则回退全量。
// 出处：core/retrieval/sparse_bm25.py L427-L436
func MatchTokens(tokens []string) []string {
	normalized := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if tok = strings.TrimSpace(tok); tok != "" {
			normalized = append(normalized, tok)
		}
	}
	if len(normalized) == 0 {
		return nil
	}
	informative := make([]string, 0, len(normalized))
	for _, tok := range normalized {
		if !isLowSignalQueryToken(tok) {
			informative = append(informative, tok)
		}
	}
	if len(informative) == 0 {
		return dedupe(normalized)
	}
	return dedupe(informative)
}

// BuildMatchQuery 生成 FTS5 MATCH 表达式（OR 提升召回，最多 64 个 token）。
// 出处：core/retrieval/sparse_bm25.py L438-L451
func BuildMatchQuery(tokens []string) string {
	safe := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		t := strings.ReplaceAll(tok, `"`, `""`)
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		safe = append(safe, `"`+t+`"`)
		if len(safe) >= SparseMatchQueryMaxTokens {
			break
		}
	}
	return strings.Join(safe, " OR ")
}

// CountCJK 返回文本中日韩统一表意文字的数量。
// 用于中文占比判定（对齐 Python 版对中文文本选择 jieba 分词而非 char n-gram 的口径）。
func CountCJK(text string) int {
	count := 0
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			count++
		}
	}
	return count
}
