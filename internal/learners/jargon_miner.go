package learners

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// This file mirrors 参考/MaiBot/src/services/message_word_frequency_service.py
// 的词频/共现统计语义（黑话候选挖掘的机械前置）：
// 分词 → 停用词过滤 → 词与 n-gram 短语计数 → 排序 → 高频词表。
//
// 与 Python 差异：jieba 由装配层注入的 SegmentFunc 提供（Go 版直接复用
// internal/chat/replyer 的 gse 分词能力或退化到字符 n-gram），本包不 import
// 分词库以保持最小依赖。

// WordFrequencyItem mirrors WordFrequencyItem.
type WordFrequencyItem struct {
	Term             string
	Count            int
	MessageCount     int
	Frequency        float64
	MessageFrequency float64
}

// SegmentFunc 是分词边界（装配层用 gse 实现）。
type SegmentFunc func(text string) []string

// HighFrequencyTerm mirrors database_model.HighFrequencyTerm 的视图。
type HighFrequencyTerm struct {
	Term            string
	OccurrenceCount int
	MessageCount    int
	Rank            int
	Frequency       float64
	MessageFreq     float64
}

// WordFrequencyOptions mirrors `_build_message_word_frequency_terms` 的参数。
type WordFrequencyOptions struct {
	// Limit mirrors limit（默认 50）。
	Limit int
	// MinCount mirrors min_count（默认 2）。
	MinCount int
	// MaxPhraseSize mirrors `_iter_phrases(max_size=3)`。
	MaxPhraseSize int
	// Segment 是分词实现（nil 时退化为字符 n-gram）。
	Segment SegmentFunc
}

// BuildMessageWordFrequencyTerms mirrors `_build_message_word_frequency_terms`.
func BuildMessageWordFrequencyTerms(texts []string, options WordFrequencyOptions) []WordFrequencyItem {
	limit := options.Limit
	if limit < 1 {
		limit = 50
	}
	minCount := options.MinCount
	if minCount < 1 {
		minCount = 1
	}
	maxPhraseSize := options.MaxPhraseSize
	if maxPhraseSize < 2 {
		maxPhraseSize = 3
	}

	wordCounter := map[string]int{}
	phraseCounter := map[string]int{}
	wordMessageCounter := map[string]int{}
	phraseMessageCounter := map[string]int{}
	messageCount := 0

	for _, text := range texts {
		if strings.TrimSpace(text) == "" {
			continue
		}
		messageCount++
		tokens := TokenizeMeaningfulTerms(text, options.Segment)
		if len(tokens) == 0 {
			continue
		}
		phrases := IterPhrases(tokens, maxPhraseSize)
		for _, token := range tokens {
			wordCounter[token]++
		}
		for _, phrase := range phrases {
			phraseCounter[phrase]++
		}
		for token := range uniqueSet(tokens) {
			wordMessageCounter[token]++
		}
		for phrase := range uniqueSet(phrases) {
			phraseMessageCounter[phrase]++
		}
	}

	totalTermCount := 0
	for _, count := range wordCounter {
		totalTermCount += count
	}
	for _, count := range phraseCounter {
		totalTermCount += count
	}

	items := make([]WordFrequencyItem, 0, len(wordCounter)+len(phraseCounter))
	items = append(items, counterToItems(wordCounter, wordMessageCounter, messageCount, totalTermCount, minCount)...)
	items = append(items, counterToItems(phraseCounter, phraseMessageCounter, messageCount, totalTermCount, minCount)...)
	sort.SliceStable(items, func(i, j int) bool {
		return frequencyItemLess(items[i], items[j])
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func frequencyItemLess(left WordFrequencyItem, right WordFrequencyItem) bool {
	if left.Count != right.Count {
		return left.Count > right.Count
	}
	if left.MessageCount != right.MessageCount {
		return left.MessageCount > right.MessageCount
	}
	if len([]rune(left.Term)) != len([]rune(right.Term)) {
		return len([]rune(left.Term)) > len([]rune(right.Term))
	}
	return left.Term < right.Term
}

func counterToItems(
	counter map[string]int,
	messageCounter map[string]int,
	messageCount int,
	totalTermCount int,
	minCount int,
) []WordFrequencyItem {
	items := make([]WordFrequencyItem, 0, len(counter))
	for term, count := range counter {
		if count < minCount {
			continue
		}
		termMessageCount := messageCounter[term]
		frequency := 0.0
		if totalTermCount > 0 {
			frequency = float64(count) / float64(totalTermCount)
		}
		messageFrequency := 0.0
		if messageCount > 0 {
			messageFrequency = float64(termMessageCount) / float64(messageCount)
		}
		items = append(items, WordFrequencyItem{
			Term:             term,
			Count:            count,
			MessageCount:     termMessageCount,
			Frequency:        frequency,
			MessageFrequency: messageFrequency,
		})
	}
	return items
}

// MergeHighFrequencyTerms mirrors `_merge_high_frequency_terms` + `_rerank_…`：
// 按 term 合并增量计数，按 (count, message_count, -len, term) 重排并截断。
func MergeHighFrequencyTerms(
	existing []HighFrequencyTerm,
	terms []WordFrequencyItem,
	maxTerms int,
) []HighFrequencyTerm {
	if maxTerms < 1 {
		maxTerms = 1000
	}
	byKey := map[string]*HighFrequencyTerm{}
	order := make([]string, 0, len(existing)+len(terms))
	for _, record := range existing {
		key := NormalizeTermForMatch(record.Term)
		if key == "" {
			continue
		}
		copied := record
		if _, ok := byKey[key]; !ok {
			order = append(order, key)
		}
		byKey[key] = &copied
	}
	for _, item := range terms {
		key := NormalizeTermForMatch(item.Term)
		if key == "" {
			continue
		}
		record, ok := byKey[key]
		if !ok {
			order = append(order, key)
			record = &HighFrequencyTerm{Term: item.Term}
			byKey[key] = record
		}
		record.Term = item.Term
		record.OccurrenceCount += item.Count
		record.MessageCount += item.MessageCount
	}

	merged := make([]HighFrequencyTerm, 0, len(order))
	for _, key := range order {
		merged = append(merged, *byKey[key])
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].OccurrenceCount != merged[j].OccurrenceCount {
			return merged[i].OccurrenceCount > merged[j].OccurrenceCount
		}
		if merged[i].MessageCount != merged[j].MessageCount {
			return merged[i].MessageCount > merged[j].MessageCount
		}
		if len([]rune(merged[i].Term)) != len([]rune(merged[j].Term)) {
			return len([]rune(merged[i].Term)) > len([]rune(merged[j].Term))
		}
		return merged[i].Term < merged[j].Term
	})
	if len(merged) > maxTerms {
		merged = merged[:maxTerms]
	}
	totalOccurrence, totalMessage := 0, 0
	for _, record := range merged {
		totalOccurrence += record.OccurrenceCount
		totalMessage += record.MessageCount
	}
	for index := range merged {
		merged[index].Rank = index + 1
		if totalOccurrence > 0 {
			merged[index].Frequency = float64(merged[index].OccurrenceCount) / float64(totalOccurrence)
		}
		if totalMessage > 0 {
			merged[index].MessageFreq = float64(merged[index].MessageCount) / float64(totalMessage)
		}
	}
	return merged
}

// NormalizeTermForMatch mirrors `_normalize_term_for_match`.
func NormalizeTermForMatch(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

// ---------------------------------------------------------------------------
// 分词（正则/停用词照抄 message_word_frequency_service.py）
// ---------------------------------------------------------------------------

var (
	urlPattern       = regexp.MustCompile(`(?i)https?://\S+|www\.\S+`)
	cqCodePattern    = regexp.MustCompile(`\[CQ:[^\]]+\]`)
	mentionPattern   = regexp.MustCompile(`@\S+`)
	techTermPattern  = regexp.MustCompile(`(?:\A|[^A-Za-z0-9_])([A-Za-z][A-Za-z0-9_+./#-]+)`)
	numericPattern   = regexp.MustCompile(`^[\d._+-]+$`)
	punctOnlyPattern = regexp.MustCompile(`^[^\p{L}\p{N}]+$`)
)

// tokenTrimChars mirrors _TOKEN_TRIM_CHARS.
const tokenTrimChars = " \t\r\n.,;:!?()[]{}<>\"'`~@#$%^&*=|\\/，。！？；：、（）【】《》“”‘’…"

// cnStopWords mirrors _CN_STOP_WORDS.
var cnStopWords = []string{
	"一些", "一样", "一定", "一直", "一条", "一种", "一起", "一下", "一个", "一会", "一是", "不是", "不过",
	"不如", "不能", "不要", "不用", "不会", "但原", "为了", "为什么", "也是", "也许", "于是", "什么", "他们",
	"以后", "以前", "以及", "按照", "但是", "你们", "你的", "其实", "其它", "其他", "只是", "只要", "可以",
	"可是", "没有", "各位", "回复", "因为", "因此", "如果", "它们", "对于", "对方", "就是", "已经", "并且",
	"怎么", "怎样", "总之", "我们", "我的", "所以", "所有", "是否", "是不是", "未知", "未知用户", "有人",
	"有点", "有没有", "无法访问", "然后", "现在", "关于", "根据", "由于", "的话", "直接", "真的", "真是",
	"自己", "虽然", "这是", "通过", "这个", "这些", "这么", "这里", "这样", "那个", "那些", "那么", "那里",
	"那样", "还是", "还有", "或者", "而且", "来说", "知道", "一下子", "一大", "不了", "之前", "出来", "只能",
	"多少", "今天", "图片", "喜欢", "好像", "好看", "哈哈", "哈哈哈", "开始", "小时", "分钟", "应该", "我要",
	"时候", "时间", "消息", "确实", "聊天", "表情", "觉得", "需要", "感觉", "看看", "看到", "群友", "使用",
	"可能", "问题", "这种", "里面", "内容", "用户", "啊啊啊", "东西",
}

// enStopWords mirrors _EN_STOP_WORDS.
var enStopWords = []string{
	"a", "about", "after", "all", "also", "an", "and", "any", "are", "as", "at", "be", "because", "but", "by",
	"can", "could", "for", "from", "had", "has", "have", "how", "if", "in", "into", "is", "it", "its", "just",
	"may", "more", "not", "of", "on", "or", "our", "so", "that", "the", "their", "then", "there", "this", "to",
	"was", "we", "were", "what", "when", "where", "which", "who", "why", "will", "with", "would", "you", "your",
}

var stopWords = func() map[string]bool {
	out := map[string]bool{}
	for _, word := range cnStopWords {
		out[word] = true
	}
	for _, word := range enStopWords {
		out[word] = true
	}
	return out
}()

// TokenizeMeaningfulTerms mirrors `_tokenize_meaningful_terms`（分词器未注入时
// 使用字符 n-gram + 技术词正则的确定性回退，保证挖掘测试可复现）。
func TokenizeMeaningfulTerms(text string, segment SegmentFunc) []string {
	cleaned := RemoveTokenizationNoise(text)
	terms := make([]string, 0, 16)
	if segment != nil {
		for _, raw := range segment(cleaned) {
			token := normalizeToken(raw)
			if isMeaningfulToken(token) {
				terms = append(terms, token)
			}
		}
	} else {
		for _, token := range fallbackTokens(cleaned) {
			if isMeaningfulToken(token) {
				terms = append(terms, token)
			}
		}
	}
	existing := map[string]bool{}
	for _, term := range terms {
		existing[term] = true
	}
	for _, match := range techTermPattern.FindAllStringSubmatch(cleaned, -1) {
		if len(match) < 2 {
			continue
		}
		token := normalizeToken(match[1])
		if token == "" || existing[token] || !isMeaningfulToken(token) {
			continue
		}
		terms = append(terms, token)
		existing[token] = true
	}
	return terms
}

// fallbackTokens 是 jieba 的确定性回退：CJK 连续段按 2-gram、拉丁词按空白。
func fallbackTokens(text string) []string {
	tokens := make([]string, 0, 16)
	runes := []rune(text)
	cjkBuffer := make([]rune, 0, 8)
	latinBuffer := make([]rune, 0, 8)
	flushCJK := func() {
		if len(cjkBuffer) == 0 {
			return
		}
		for index := 0; index+2 <= len(cjkBuffer); index++ {
			tokens = append(tokens, string(cjkBuffer[index:index+2]))
		}
		if len(cjkBuffer) == 1 {
			tokens = append(tokens, string(cjkBuffer))
		}
		cjkBuffer = cjkBuffer[:0]
	}
	flushLatin := func() {
		if len(latinBuffer) == 0 {
			return
		}
		tokens = append(tokens, string(latinBuffer))
		latinBuffer = latinBuffer[:0]
	}
	for _, char := range runes {
		switch {
		case isCJK(char):
			flushLatin()
			cjkBuffer = append(cjkBuffer, char)
		case unicode.IsLetter(char) || unicode.IsDigit(char):
			flushCJK()
			latinBuffer = append(latinBuffer, char)
		default:
			flushCJK()
			flushLatin()
		}
	}
	flushCJK()
	flushLatin()
	return tokens
}

// RemoveTokenizationNoise mirrors `_remove_tokenization_noise`.
func RemoveTokenizationNoise(text string) string {
	withoutURLs := urlPattern.ReplaceAllString(text, " ")
	withoutCodes := cqCodePattern.ReplaceAllString(withoutURLs, " ")
	return mentionPattern.ReplaceAllString(withoutCodes, " ")
}

func normalizeToken(raw string) string {
	token := strings.ToLower(strings.Trim(raw, tokenTrimChars))
	return strings.Join(strings.Fields(token), " ")
}

func isMeaningfulToken(token string) bool {
	if token == "" || stopWords[token] {
		return false
	}
	if numericPattern.MatchString(token) || punctOnlyPattern.MatchString(token) {
		return false
	}
	if containsCJK(token) {
		return countCJKChars(token) >= 2
	}
	if containsLatin(token) {
		return len([]rune(token)) >= 2
	}
	return len([]rune(token)) >= 2
}

// IterPhrases mirrors `_iter_phrases`（连续 token 的 2..max size n-gram）。
func IterPhrases(tokens []string, maxSize int) []string {
	if maxSize < 2 {
		maxSize = 2
	}
	phrases := make([]string, 0, len(tokens))
	for size := 2; size <= maxSize; size++ {
		if len(tokens) < size {
			break
		}
		for index := 0; index+size <= len(tokens); index++ {
			window := tokens[index : index+size]
			phrase := joinPhrase(window)
			if isMeaningfulPhrase(phrase, window) {
				phrases = append(phrases, phrase)
			}
		}
	}
	return phrases
}

// joinPhrase mirrors `_join_phrase`（纯中文 token 直接拼接）。
func joinPhrase(tokens []string) string {
	allCJK := true
	for _, token := range tokens {
		if !containsCJK(token) || containsLatin(token) {
			allCJK = false
			break
		}
	}
	if allCJK {
		return strings.Join(tokens, "")
	}
	return strings.Join(tokens, " ")
}

// isMeaningfulPhrase mirrors `_is_meaningful_phrase`.
func isMeaningfulPhrase(phrase string, tokens []string) bool {
	if stopWords[phrase] || len(uniqueSet(tokens)) == 1 {
		return false
	}
	if containsCJK(phrase) {
		return countCJKChars(phrase) >= 4
	}
	for _, token := range tokens {
		if containsLatin(token) {
			return true
		}
	}
	return false
}

func containsCJK(text string) bool {
	for _, char := range text {
		if isCJK(char) {
			return true
		}
	}
	return false
}

func countCJKChars(text string) int {
	count := 0
	for _, char := range text {
		if isCJK(char) {
			count++
		}
	}
	return count
}

func containsLatin(text string) bool {
	for _, char := range text {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') {
			return true
		}
	}
	return false
}

func isCJK(char rune) bool { return char >= 0x4E00 && char <= 0x9FFF }

func uniqueSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// 共现挖掘（黑话候选的机械前置）
// ---------------------------------------------------------------------------

// CooccurrenceCandidate 是一次共现挖掘的结果。
type CooccurrenceCandidate struct {
	Term             string
	Count            int
	MessageCount     int
	CooccurrenceWith []string
	// Score 是 (消息覆盖数, 频次) 的排序键（越大越可疑为黑话）。
	Score float64
	// Frequency mirrors WordFrequencyItem.Frequency。
	Frequency float64
}

// CooccurrenceOptions 汇总共现挖掘参数。
type CooccurrenceOptions struct {
	// Limit mirrors message_word_frequency 的 limit。
	Limit int
	// MinCount 是最小出现次数（默认 2）。
	MinCount int
	// MinDocs 是跨消息覆盖下限（默认 1）。
	MinDocs int
	// MaxAssociations 是每条的共现词上限（默认 3）。
	MaxAssociations int
	// MaxPhraseSize mirrors `_iter_phrases(max_size=3)`。
	MaxPhraseSize int
	Segment       SegmentFunc
}

// DefaultCooccurrenceOptions 返回照抄 Python 默认值的参数。
func DefaultCooccurrenceOptions() CooccurrenceOptions {
	return CooccurrenceOptions{Limit: 50, MinCount: 2, MinDocs: 1, MaxAssociations: 3, MaxPhraseSize: 3}
}

// MineCooccurrenceCandidates 挖掘共现+频次候选。
//
// 对齐 message_word_frequency_service 的统计口径，并额外计算「同现词」：
// 与目标词出现在同一消息中的其他高频词（按同现次数、词长排序）。
func MineCooccurrenceCandidates(
	texts []string,
	options CooccurrenceOptions,
) []CooccurrenceCandidate {
	words := BuildMessageWordFrequencyTerms(texts, WordFrequencyOptions{
		Limit:         options.Limit,
		MinCount:      options.MinCount,
		MaxPhraseSize: options.MaxPhraseSize,
		Segment:       options.Segment,
	})
	if len(words) == 0 {
		return nil
	}
	frequencyByTerm := make(map[string]WordFrequencyItem, len(words))
	for _, item := range words {
		frequencyByTerm[item.Term] = item
	}

	docTerms := make([]map[string]bool, 0, len(texts))
	for _, text := range texts {
		if strings.TrimSpace(text) == "" {
			continue
		}
		tokens := TokenizeMeaningfulTerms(text, options.Segment)
		if len(tokens) == 0 {
			continue
		}
		set := map[string]bool{}
		for _, token := range tokens {
			if _, tracked := frequencyByTerm[token]; tracked {
				set[token] = true
			}
		}
		for _, phrase := range IterPhrases(tokens, options.MaxPhraseSize) {
			if _, tracked := frequencyByTerm[phrase]; tracked {
				set[phrase] = true
			}
		}
		if len(set) > 0 {
			docTerms = append(docTerms, set)
		}
	}

	minDocs := options.MinDocs
	if minDocs < 1 {
		minDocs = 1
	}
	maxAssociations := options.MaxAssociations
	if maxAssociations < 1 {
		maxAssociations = 3
	}

	candidates := make([]CooccurrenceCandidate, 0, len(words))
	for _, item := range words {
		if item.MessageCount < minDocs {
			continue
		}
		counter := map[string]int{}
		for _, set := range docTerms {
			if !set[item.Term] {
				continue
			}
			for term := range set {
				if term == item.Term {
					continue
				}
				counter[term]++
			}
		}
		associations := make([]string, 0, len(counter))
		for term := range counter {
			associations = append(associations, term)
		}
		sort.SliceStable(associations, func(i, j int) bool {
			if counter[associations[i]] != counter[associations[j]] {
				return counter[associations[i]] > counter[associations[j]]
			}
			if len([]rune(associations[i])) != len([]rune(associations[j])) {
				return len([]rune(associations[i])) > len([]rune(associations[j]))
			}
			return associations[i] < associations[j]
		})
		if len(associations) > maxAssociations {
			associations = associations[:maxAssociations]
		}
		candidates = append(candidates, CooccurrenceCandidate{
			Term:             item.Term,
			Count:            item.Count,
			MessageCount:     item.MessageCount,
			CooccurrenceWith: associations,
			Score:            float64(item.MessageCount)*1000 + float64(item.Count),
			Frequency:        item.Frequency,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].Term < candidates[j].Term
	})
	limit := options.Limit
	if limit < 1 {
		limit = 50
	}
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

// FormatCooccurrenceSummary 渲染共现候选摘要（日志/调试）。
func FormatCooccurrenceSummary(candidates []CooccurrenceCandidate, limit int) string {
	if limit < 1 {
		limit = 10
	}
	lines := make([]string, 0, limit)
	for index, candidate := range candidates {
		if index >= limit {
			break
		}
		associationText := ""
		if len(candidate.CooccurrenceWith) > 0 {
			associationText = " 同现=" + strings.Join(candidate.CooccurrenceWith, "、")
		}
		lines = append(lines, fmt.Sprintf("%s: count=%d docs=%d%s",
			candidate.Term, candidate.Count, candidate.MessageCount, associationText))
	}
	return strings.Join(lines, "\n")
}
