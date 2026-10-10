package replyer

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"

	"github.com/go-ego/gse"
	"github.com/mozillazg/go-pinyin"
)

// This file mirrors 参考/MaiBot/src/chat/utils/typo_generator.py
// (behaviour is authoritative there): 基于拼音和字频的中文错别字生成工具。
//
// jieba → gse、pypinyin → go-pinyin（对齐重写计划 §3）；
// 字频表直接复用 depends-data/char_frequency.json。

//go:embed charfreq_data.json
var charFrequencyJSON []byte

// pinyinDict mirrors _create_pinyin_dict: 拼音(TONE3) -> 汉字列表（常用汉字范围）。
var (
	pinyinDictOnce sync.Once
	pinyinDict     map[string][]string
	pinyinDictErr  error
)

// segmenterOnce mirrors the shared jieba initialisation (进程内共享)。
var (
	segmenterOnce sync.Once
	segmenter     gse.Segmenter
	segmenterErr  error
)

// wordFrequencies mirrors _load_word_frequencies: gse 内置词典的词 -> 词频。
var (
	wordFreqOnce    sync.Once
	wordFrequencies map[string]float64
)

func getSegmenter() (gse.Segmenter, error) {
	segmenterOnce.Do(func() {
		seg, err := gse.NewEmbed("zh")
		if err != nil {
			segmenterErr = err
			return
		}
		seg.SkipLog = true
		segmenter = seg
	})
	return segmenter, segmenterErr
}

// createPinyinDict mirrors _create_pinyin_dict（0x4E00..0x9FFF 常用汉字范围）。
func createPinyinDict() (map[string][]string, error) {
	pinyinDictOnce.Do(func() {
		dict := map[string][]string{}
		for code := 0x4E00; code < 0x9FFF; code++ {
			char := string(rune(code))
			pys := pinyin.SinglePinyin(rune(code), pinyin.Args{Style: pinyin.Tone3})
			if len(pys) == 0 {
				continue
			}
			dict[pys[0]] = append(dict[pys[0]], char)
		}
		pinyinDict = dict
	})
	return pinyinDict, pinyinDictErr
}

// loadCharFrequency mirrors _load_or_create_char_frequency:
// 直接加载 depends-data/char_frequency.json（构建期 embed，等价 Python 缓存文件）。
func loadCharFrequency() (map[string]float64, error) {
	freq := map[string]float64{}
	if err := json.Unmarshal(charFrequencyJSON, &freq); err != nil {
		return nil, fmt.Errorf("解析字频表失败: %w", err)
	}
	return freq, nil
}

// loadWordFrequencies mirrors _load_word_frequencies（gse 内置词典词频）。
func loadWordFrequencies() map[string]float64 {
	wordFreqOnce.Do(func() {
		words := map[string]float64{}
		if _, err := getSegmenter(); err != nil {
			wordFrequencies = words
			return
		}
		// gse 内置词典无直接全量导出；通过分词常用字词集合近似不可取，
		// 这里退而求其次：按需查询词频（Find），构建查询缓存。
		wordFrequencies = words
	})
	return wordFrequencies
}

// wordFreqCache caches Find() results (词频查询缓存)。
var (
	wordFreqCacheMu sync.RWMutex
	wordFreqCache   = map[string]float64{}
)

// lookupWordFrequency mirrors valid_words.get(word, 0) via the gse dictionary.
func lookupWordFrequency(word string) float64 {
	wordFreqCacheMu.RLock()
	if freq, ok := wordFreqCache[word]; ok {
		wordFreqCacheMu.RUnlock()
		return freq
	}
	wordFreqCacheMu.RUnlock()

	freq := 0.0
	if seg, err := getSegmenter(); err == nil {
		if value, _, ok := seg.Find(word); ok {
			freq = value
		}
	}
	wordFreqCacheMu.Lock()
	wordFreqCache[word] = freq
	wordFreqCacheMu.Unlock()
	return freq
}

// ChineseTypoGenerator mirrors ChineseTypoGenerator.
type ChineseTypoGenerator struct {
	// error_rate: 单字替换概率
	errorRate float64
	// min_freq: 最小字频阈值
	minFreq int
	// tone_error_rate: 声调错误概率
	toneErrorRate float64
	// word_replace_rate: 整词替换概率
	wordReplaceRate float64
	// max_freq_diff: 最大允许的频率差异
	maxFreqDiff float64

	pinyinDict    map[string][]string
	charFrequency map[string]float64
}

// DefaultMaxFreqDiff mirrors max_freq_diff=200.
const DefaultMaxFreqDiff = 200.0

// NewChineseTypoGenerator mirrors __init__(error_rate=0.3, min_freq=5,
// tone_error_rate=0.2, word_replace_rate=0.3, max_freq_diff=200)。
func NewChineseTypoGenerator(errorRate float64, minFreq int, toneErrorRate float64, wordReplaceRate float64) *ChineseTypoGenerator {
	generator := &ChineseTypoGenerator{
		errorRate:       errorRate,
		minFreq:         minFreq,
		toneErrorRate:   toneErrorRate,
		wordReplaceRate: wordReplaceRate,
		maxFreqDiff:     DefaultMaxFreqDiff,
	}
	generator.pinyinDict, _ = createPinyinDict()
	generator.charFrequency, _ = loadCharFrequency()
	loadWordFrequencies()
	return generator
}

// isChineseChar mirrors _is_chinese_char.
func isChineseCharRune(char rune) bool {
	return '\u4e00' <= char && char <= '\u9fff'
}

// containsChineseChar mirrors `all(not self._is_chinese_char(c) for c in word)`
// negated.
func containsChineseChar(word string) bool {
	for _, c := range word {
		if isChineseCharRune(c) {
			return true
		}
	}
	return false
}


// getSimilarTonePinyin mirrors _get_similar_tone_pinyin（static）。
func getSimilarTonePinyin(py string) string {
	if py == "" {
		return py
	}
	lastRune := py[len(py)-1]
	if lastRune < '0' || lastRune > '9' {
		// 非数字结尾的拼音添加数字声调1
		return py + "1"
	}
	base := py[:len(py)-1]
	tone := int(lastRune - '0')
	if tone < 1 || tone > 4 {
		return base + itoa(1+rand.Intn(4))
	}
	possibleTones := []int{1, 2, 3, 4}
	filtered := possibleTones[:0]
	for _, t := range possibleTones {
		if t != tone {
			filtered = append(filtered, t)
		}
	}
	return base + itoa(filtered[rand.Intn(len(filtered))])
}

// calculateReplacementProbability mirrors _calculate_replacement_probability:
// 根据频率差计算替换概率。
func (g *ChineseTypoGenerator) calculateReplacementProbability(origFreq float64, targetFreq float64) float64 {
	if targetFreq > origFreq {
		return 1.0 // 如果替换字频率更高，保持原有概率
	}
	freqDiff := origFreq - targetFreq
	if freqDiff > g.maxFreqDiff {
		return 0.0 // 频率差太大，不替换
	}
	// 频率差为0时概率为1，频率差为max_freq_diff时概率接近0
	return math.Exp(-3 * freqDiff / g.maxFreqDiff)
}

// getSimilarFrequencyChars mirrors _get_similar_frequency_chars:
// 获取与给定字频率相近的同音字，可能包含声调错误（返回概率最高的 5 个）。
func (g *ChineseTypoGenerator) getSimilarFrequencyChars(char string, py string, numCandidates int) []string {
	if numCandidates <= 0 {
		numCandidates = 5
	}
	var homophones []string

	// 有一定概率使用错误声调
	if rand.Float64() < g.toneErrorRate {
		wrongTonePy := getSimilarTonePinyin(py)
		homophones = append(homophones, g.pinyinDict[wrongTonePy]...)
	}
	// 添加正确声调的同音字
	homophones = append(homophones, g.pinyinDict[py]...)

	if len(homophones) == 0 {
		return nil
	}

	origFreq := g.charFrequency[char]

	type candidate struct {
		char string
		freq float64
	}
	var candidatesWithProb []candidate
	for _, h := range homophones {
		if h == char {
			continue
		}
		freq := g.charFrequency[h]
		if freq < float64(g.minFreq) {
			continue
		}
		prob := g.calculateReplacementProbability(origFreq, freq)
		if prob > 0 {
			candidatesWithProb = append(candidatesWithProb, candidate{char: h, freq: freq})
		}
	}
	if len(candidatesWithProb) == 0 {
		return nil
	}

	// 根据概率排序（稳定降序，照抄 `sort(key=lambda x: x[1], reverse=True)`）
	for i := 1; i < len(candidatesWithProb); i++ {
		for j := i; j > 0 && candidatesWithProb[j].freq > candidatesWithProb[j-1].freq; j-- {
			candidatesWithProb[j], candidatesWithProb[j-1] = candidatesWithProb[j-1], candidatesWithProb[j]
		}
	}
	result := make([]string, 0, numCandidates)
	for i, candidate := range candidatesWithProb {
		if i >= numCandidates {
			break
		}
		result = append(result, candidate.char)
	}
	return result
}

// getWordPinyin mirrors _get_word_pinyin（static）。
func getWordPinyin(word string) []string {
	var result []string
	for _, char := range word {
		pys := pinyin.SinglePinyin(char, pinyin.Args{Style: pinyin.Tone3})
		if len(pys) == 0 {
			result = append(result, "")
			continue
		}
		result = append(result, pys[0])
	}
	return result
}

// segmentSentence mirrors _segment_sentence（jieba.cut → gse.Cut HMM 模式）。
func (g *ChineseTypoGenerator) segmentSentence(sentence string) []string {
	seg, err := getSegmenter()
	if err != nil {
		return []string{sentence}
	}
	return seg.Cut(sentence, true)
}

// getWordHomophones mirrors _get_word_homophones:
// 获取整个词的同音词，只返回高频的有意义词语（综合评分前 5）。
func (g *ChineseTypoGenerator) getWordHomophones(word string) []string {
	if len([]rune(word)) == 1 {
		return []string{}
	}

	wordPinyin := getWordPinyin(word)

	// 遍历所有可能的同音字组合
	candidates := make([][]string, 0, len(wordPinyin))
	for _, py := range wordPinyin {
		chars := g.pinyinDict[py]
		if len(chars) == 0 {
			return []string{}
		}
		candidates = append(candidates, chars)
	}

	originalWordFreq := lookupWordFrequency(word)
	minWordFreq := originalWordFreq * 0.1 // 最小词频为原词频的10%

	var homophones []struct {
		word  string
		score float64
	}
	for _, combo := range product(candidates) {
		newWord := strings.Join(combo, "")
		if newWord == word {
			continue
		}
		newWordFreq := lookupWordFrequency(newWord)
		if newWordFreq <= 0 {
			continue
		}
		if newWordFreq < minWordFreq {
			continue
		}
		// 计算词的平均字频（考虑字频和词频）
		charAvgFreq := 0.0
		for _, c := range newWord {
			charAvgFreq += g.charFrequency[string(c)]
		}
		charAvgFreq /= float64(len([]rune(newWord)))
		// 综合评分：结合词频和字频
		combinedScore := newWordFreq*0.7 + charAvgFreq*0.3
		if combinedScore >= float64(g.minFreq) {
			homophones = append(homophones, struct {
				word  string
				score float64
			}{newWord, combinedScore})
		}
	}

	// 按综合分数排序并限制返回数量（前 5）
	for i := 1; i < len(homophones); i++ {
		for j := i; j > 0 && homophones[j].score > homophones[j-1].score; j-- {
			homophones[j], homophones[j-1] = homophones[j-1], homophones[j]
		}
	}
	result := make([]string, 0, 5)
	for i, h := range homophones {
		if i >= 5 {
			break
		}
		result = append(result, h.word)
	}
	return result
}

// product mirrors itertools.product（笛卡尔积）。
func product(sets [][]string) [][]string {
	total := 1
	for _, set := range sets {
		total *= len(set)
	}
	result := make([][]string, 0, total)
	indexes := make([]int, len(sets))
	for {
		combo := make([]string, len(sets))
		for i, set := range sets {
			combo[i] = set[indexes[i]]
		}
		result = append(result, combo)
		i := len(sets) - 1
		for i >= 0 {
			indexes[i]++
			if indexes[i] < len(sets[i]) {
				break
			}
			indexes[i] = 0
			i--
		}
		if i < 0 {
			break
		}
	}
	return result
}

// typoInfoItem mirrors one entry of typo_info.
type typoInfoItem struct {
	Orig   string
	Typo   string
	OrigPy string
	TypoPy string
	OrigFq float64
	TypoFq float64
}

// FormatTypoInfo mirrors format_typo_info（static）。
func FormatTypoInfo(typoInfo []typoInfoItem) string {
	if len(typoInfo) == 0 {
		return "未生成错别字"
	}
	var result []string
	for _, info := range typoInfo {
		isWord := strings.Contains(info.OrigPy, " ")
		errorType := "同音字替换"
		if isWord {
			errorType = "整词替换"
		} else {
			// orig_py[:-1] == typo_py[:-1] and orig_py[-1] != typo_py[-1]
			origBase := info.OrigPy[:len(info.OrigPy)-1]
			typoBase := info.TypoPy[:len(info.TypoPy)-1]
			if origBase == typoBase && info.OrigPy[len(info.OrigPy)-1] != info.TypoPy[len(info.TypoPy)-1] {
				errorType = "声调错误"
			}
		}
		result = append(result, fmt.Sprintf(
			"原文：%s(%s) [频率：%.2f] -> 替换：%s(%s) [频率：%.2f] [%s]",
			info.Orig, info.OrigPy, info.OrigFq, info.Typo, info.TypoPy, info.TypoFq, errorType,
		))
	}
	return strings.Join(result, "\n")
}

// CreateTypoSentence mirrors create_typo_sentence:
// 创建包含同音字错误的句子，支持词语级别和字级别的替换。
//
// 返回 (typo_sentence, correction_suggestion)：纠正建议为 50% 概率随机选出的
// 一个正确字/词（照抄 Python 分支）。
func (g *ChineseTypoGenerator) CreateTypoSentence(sentence string) (string, string) {
	var result []string
	var wordTypos [][2]string // (错词, 正确词)
	var charTypos [][2]string // (错字, 正确字)

	// 分词
	words := g.segmentSentence(sentence)

	for _, word := range words {
		if !containsChineseChar(word) {
			// 如果是标点符号或空格，直接添加
			result = append(result, word)
			continue
		}

		wordPinyin := getWordPinyin(word)

		// 尝试整词替换
		if len([]rune(word)) > 1 && rand.Float64() < g.wordReplaceRate {
			wordHomophones := g.getWordHomophones(word)
			if len(wordHomophones) > 0 {
				typoWord := wordHomophones[rand.Intn(len(wordHomophones))]
				// 计算词的平均频率
				origFreq := 0.0
				for _, c := range word {
					origFreq += g.charFrequency[string(c)]
				}
				origFreq /= float64(len([]rune(word)))
				typoFreq := 0.0
				for _, c := range typoWord {
					typoFreq += g.charFrequency[string(c)]
				}
				typoFreq /= float64(len([]rune(typoWord)))

				result = append(result, typoWord)
				wordTypos = append(wordTypos, [2]string{typoWord, word})
				continue
			}
		}

		// 如果不进行整词替换，则进行单字替换
		runes := []rune(word)
		if len(runes) == 1 {
			char := string(runes[0])
			py := wordPinyin[0]
			if rand.Float64() < g.errorRate {
				similarChars := g.getSimilarFrequencyChars(char, py, 5)
				if len(similarChars) > 0 {
					typoChar := similarChars[rand.Intn(len(similarChars))]
					typoFreq := g.charFrequency[typoChar]
					origFreq := g.charFrequency[char]
					replaceProb := g.calculateReplacementProbability(origFreq, typoFreq)
					if rand.Float64() < replaceProb {
						result = append(result, typoChar)
						charTypos = append(charTypos, [2]string{typoChar, char})
						continue
					}
				}
			}
			result = append(result, char)
		} else {
			// 处理多字词的单字替换
			wordResult := make([]string, 0, len(runes))
			for index, charRune := range runes {
				char := string(charRune)
				py := ""
				if index < len(wordPinyin) {
					py = wordPinyin[index]
				}
				// 词中的字替换概率降低
				wordErrorRate := g.errorRate * math.Pow(0.7, float64(len(runes)-1))

				if rand.Float64() < wordErrorRate {
					similarChars := g.getSimilarFrequencyChars(char, py, 5)
					if len(similarChars) > 0 {
						typoChar := similarChars[rand.Intn(len(similarChars))]
						typoFreq := g.charFrequency[typoChar]
						origFreq := g.charFrequency[char]
						replaceProb := g.calculateReplacementProbability(origFreq, typoFreq)
						if rand.Float64() < replaceProb {
							wordResult = append(wordResult, typoChar)
							charTypos = append(charTypos, [2]string{typoChar, char})
							continue
						}
					}
				}
				wordResult = append(wordResult, char)
			}
			result = append(result, strings.Join(wordResult, ""))
		}
	}

	// 优先从词语错误中选择，如果没有则从单字错误中选择；50%概率返回纠正建议
	correctionSuggestion := ""
	if rand.Float64() < 0.5 {
		if len(wordTypos) > 0 {
			pair := wordTypos[rand.Intn(len(wordTypos))]
			correctionSuggestion = pair[1]
		} else if len(charTypos) > 0 {
			pair := charTypos[rand.Intn(len(charTypos))]
			correctionSuggestion = pair[1]
		}
	}

	return strings.Join(result, ""), correctionSuggestion
}

// compile-time assertion that the embedded font table is non-empty.
var _ = func() bool {
	if len(charFrequencyJSON) == 0 {
		panic("replyer: char_frequency.json 为空")
	}
	return true
}()
