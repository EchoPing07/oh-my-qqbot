package replyer

import (
	"fmt"
	"math/rand"
	"regexp"
	"strings"
)

// This file mirrors the post-processing algorithms of
// 参考/MaiBot/src/chat/utils/utils.py (behaviour is authoritative there):
//
//   - _split_into_sentence_segments（断句 + 概率合并）
//   - _merge_processed_segments_to_max_count（压缩到 max_split_num 条）
//   - random_remove_punctuation（结尾句号 90% 删、逗号 5% 删 20% 转空格）
//   - _get_random_default_reply（超长降级默认回复 7 条照抄）
//   - process_llm_response_segments（完整管线）
//
// get_western_ratio 与颜文字保护也在本文件收口。

// ProcessedResponseSegment mirrors ProcessedResponseSegment:
// 回复后处理产生的单条消息及其发送提示。
type ProcessedResponseSegment struct {
	// Text 是该段可见文本。
	Text string
	// QuotePrevious 表示该条消息需要引用上一条（错别字纠正段使用）。
	QuotePrevious bool
	// Separator 是原始文本中紧跟本段的分隔符；多条消息被压缩合并为一条时，
	// 用它恢复句间的停顿。
	Separator string
}

// PostProcessOptions mirrors the global_config fields the Python pipeline reads
// (response_post_process / response_splitter / chinese_typo). The tool layer
// fills them from internal/config; the replyer package never imports config.
type PostProcessOptions struct {
	// EnableResponsePostProcess mirrors response_post_process.enable_response_post_process.
	EnableResponsePostProcess bool
	// Splitter fields mirror response_splitter.*.
	SplitterEnable          bool
	SplitterMode            string // "rule" | "llm"
	MaxLength               int
	MaxSentenceNum          int
	MaxSplitNum             int
	EnableOverflowReturnAll bool
	EnableKaomojiProtection bool
	// Typo mirrors chinese_typo.*（nil 表示不注入错别字）。
	Typo *TypoOptions
	// BotNickname mirrors global_config.bot.nickname（默认回复文案使用）。
	BotNickname string
	// SplitterSplitter overrides the LLM splitter for SplitterMode == "llm"
	// (injected; nil makes the LLM path fall back to rule splitting).
	LLMSplitter func(text string) ([][2]string, error)
}

// TypoOptions mirrors the constructor arguments of ChineseTypoGenerator plus the
// correction-quote switches of chinese_typo config.
type TypoOptions struct {
	ErrorRate                  float64
	MinFreq                    int
	ToneErrorRate              float64
	WordReplaceRate            float64
	EnableCorrectionQuote      bool
	CorrectionQuoteProbability float64
}

// randFloat mirrors random.random() (injectable for deterministic tests).
var randFloat = rand.Float64

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// isEnglishLetter mirrors is_english_letter: 检查字符是否为英文字母（忽略大小写）。
func isEnglishLetter(char string) bool {
	for _, r := range char {
		lower := r
		if 'A' <= lower && lower <= 'Z' {
			lower += 'a' - 'A'
		}
		return 'a' <= lower && lower <= 'z'
	}
	return false
}

// isDigitString mirrors str.isdigit() for single characters.
func isDigitString(char string) bool {
	for _, r := range char {
		if r < '0' || r > '9' {
			return false
		}
		return true
	}
	return false
}

// getWesternRatio mirrors get_western_ratio: 段落中字母数字字符的西文比例。
func getWesternRatio(paragraph string) float64 {
	var alnumChars []rune
	for _, char := range paragraph {
		if unicodeIsAlnum(char) {
			alnumChars = append(alnumChars, char)
		}
	}
	if len(alnumChars) == 0 {
		return 0.0
	}
	westernCount := 0
	for _, char := range alnumChars {
		if isEnglishLetterRune(char) {
			westernCount++
		}
	}
	return float64(westernCount) / float64(len(alnumChars))
}

// unicodeIsAlnum mirrors Python str.isalnum() for the characters we care about
// (letters and digits; Python also counts other Unicode letters, so mirror the
// broad definition).
func unicodeIsAlnum(char rune) bool {
	return ('a' <= char && char <= 'z') || ('A' <= char && char <= 'Z') ||
		('0' <= char && char <= '9') || isChineseChar(char) || unicodeIsLetterOther(char)
}

// unicodeIsLetterOther covers the remaining Unicode letter/digit categories
// Python's str.isalnum() accepts (kept coarse on purpose).
func unicodeIsLetterOther(char rune) bool {
	switch {
	case char >= 0x3040 && char <= 0x30ff: // kana
		return true
	case char >= 0xac00 && char <= 0xd7af: // hangul
		return true
	case char >= 0xff00 && char <= 0xffef: // fullwidth forms
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// _split_into_sentence_segments
// ---------------------------------------------------------------------------

// quoteChars mirrors quote_chars（中英文单/双引号和常见中文引号）。
var quoteChars = map[string]bool{
	`"`: true, `'`: true, "“": true, "”": true, "‘": true, "’": true,
	"「": true, "」": true, "『": true, "』": true,
}

var (
	// 预处理：连续换行合并、换行与分隔符组合清理（照抄 Python 三个 re.sub）。
	newlineFoldPattern  = regexp.MustCompile(`\n\s*\n+`)
	newlineAfterPattern = regexp.MustCompile(`\n\s*([，,。;\s])`)
	newlineBeforeRegep  = regexp.MustCompile(`([，,。;\s])\s*\n`)
	// 段内换行归一化（合并阶段末尾的 re.sub）。
	innerNewlinePattern = regexp.MustCompile(`[^\S\r\n]*[\r\n]+[^\S\r\n]*`)
)

// splitIntoSentenceSegments mirrors _split_into_sentence_segments:
// 将文本分割成 (句子, 尾随分隔符) 元组，并根据概率合并。
//
// 注意：此函数假定颜文字已在上层被保护（照抄 Python 前置条件）。
func splitIntoSentenceSegments(text string) [][2]string {
	// 预处理：处理多余的换行符
	text = newlineFoldPattern.ReplaceAllString(text, "\n")
	text = newlineAfterPattern.ReplaceAllString(text, "\n$1")
	text = newlineBeforeRegep.ReplaceAllString(text, "$1\n")

	lenText := len([]rune(text))
	if lenText < 3 {
		if randFloat() < 0.01 {
			pairs := make([][2]string, 0, lenText)
			for _, char := range text {
				pairs = append(pairs, [2]string{string(char), ""})
			}
			return pairs
		}
		return [][2]string{{text, ""}}
	}

	// 先标记哪些位置位于成对引号内部，避免在引号内部进行句子分割
	runes := []rune(text)
	insideQuote := make([]bool, lenText)
	inQuote := false
	currentQuoteChar := ""
	{
		for i, ch := range runes {
			charStr := string(ch)
			if quoteChars[charStr] {
				if !inQuote {
					inQuote = true
					currentQuoteChar = charStr
					insideQuote[i] = false
				} else {
					// 只有遇到同一类引号才视为关闭（英文引号互相兼容，照抄 Python 条件）
					if charStr == currentQuoteChar ||
						((charStr == `"` || charStr == `'`) && (currentQuoteChar == `"` || currentQuoteChar == `'`)) {
						inQuote = false
						currentQuoteChar = ""
					}
					insideQuote[i] = false
				}
			} else {
				insideQuote[i] = inQuote
			}
			i++
		}
	}

	// 定义分隔符（包含换行符）
	separators := map[string]bool{"，": true, ",": true, " ": true, "。": true, ";": true, "\n": true}
	type segment struct {
		content    string
		separator  string
		hasContent bool
	}
	segments := []segment{}
	var currentSegment strings.Builder
	currentSegmentEmpty := true

	// 1. 分割成 (内容, 分隔符) 元组
	for i, ch := range runes {
		charStr := string(ch)
		if separators[charStr] {
			canSplit := false
			if insideQuote[i] {
				// 引号内部一律不作为分割点（包括换行）
				canSplit = false
			} else if charStr == "\n" {
				// 换行符在不在引号内时都强制分割
				canSplit = true
			} else {
				canSplit = true
				// 检查分隔符左右是否有冒号（中英文），如果有则不分割
				if i > 0 {
					prevChar := string(runes[i-1])
					if prevChar == ":" || prevChar == "：" {
						canSplit = false
					}
				}
				if i < lenText-1 {
					nextChar := string(runes[i+1])
					if nextChar == ":" || nextChar == "：" {
						canSplit = false
					}
				}

				// 如果左右没有冒号，再检查空格的特殊情况
				if canSplit && charStr == " " && i > 0 && i < lenText-1 {
					prevChar := string(runes[i-1])
					nextChar := string(runes[i+1])
					dashChars := map[string]bool{"-": true, "—": true}
					if dashChars[prevChar] || dashChars[nextChar] {
						canSplit = false
					} else {
						// 不分割数字和数字、数字和英文、英文和数字、英文和英文之间的空格
						prevIsAlnum := isDigitString(prevChar) || isEnglishLetter(prevChar)
						nextIsAlnum := isDigitString(nextChar) || isEnglishLetter(nextChar)
						if prevIsAlnum && nextIsAlnum {
							canSplit = false
						}
					}
				}
			}

			if canSplit {
				if !currentSegmentEmpty {
					segments = append(segments, segment{content: currentSegment.String(), separator: charStr, hasContent: true})
				} else if charStr == " " || charStr == "\n" {
					// 如果当前段为空，但分隔符是空格或换行符，则也添加一个空段（保留分隔符）
					segments = append(segments, segment{separator: charStr, hasContent: false})
				}
				currentSegment.Reset()
				currentSegmentEmpty = true
			} else {
				// 不分割，将分隔符加入当前段
				currentSegment.WriteString(charStr)
				currentSegmentEmpty = false
			}
		} else {
			currentSegment.WriteString(charStr)
			currentSegmentEmpty = false
		}
		i++
	}

	// 添加最后一个段（没有后续分隔符）
	if !currentSegmentEmpty {
		segments = append(segments, segment{content: currentSegment.String(), hasContent: true})
	}

	// 过滤掉完全空的段（内容和分隔符都为空）
	filtered := make([]segment, 0, len(segments))
	for _, seg := range segments {
		if seg.content != "" || seg.separator != "" {
			filtered = append(filtered, seg)
		}
	}
	segments = filtered

	if len(segments) == 0 {
		if text != "" {
			return [][2]string{{text, ""}}
		}
		return nil
	}

	// 2. 概率合并
	var splitStrength float64
	switch {
	case lenText < 12:
		splitStrength = 0.2
	case lenText < 32:
		splitStrength = 0.6
	default:
		splitStrength = 0.7
	}
	// 合并概率与分割强度相反
	mergeProbability := 1.0 - splitStrength

	mergedSegments := []segment{}
	idx := 0
	for idx < len(segments) {
		currentContent := segments[idx].content
		currentSep := segments[idx].separator

		// 检查是否可以与下一段合并：
		// 不是最后一段，且随机数小于合并概率，且当前段有内容（避免合并空段）
		if idx+1 < len(segments) && currentContent != "" && currentSep != "\n" && randFloat() < mergeProbability {
			nextContent := segments[idx+1].content
			nextSep := segments[idx+1].separator
			if nextContent != "" {
				mergedSegments = append(mergedSegments, segment{
					content:    currentContent + currentSep + nextContent,
					separator:  nextSep,
					hasContent: true,
				})
			} else {
				// 下一段内容为空，只保留当前内容和下一段的分隔符
				mergedSegments = append(mergedSegments, segment{content: currentContent, separator: nextSep, hasContent: true})
			}
			idx += 2
		} else {
			mergedSegments = append(mergedSegments, segments[idx])
			idx++
		}
	}

	// 提取最终的句子内容（保留尾随分隔符，供压缩拼接时恢复停顿）
	sentenceSegments := make([][2]string, 0, len(mergedSegments))
	for _, seg := range mergedSegments {
		if seg.content != "" {
			sentenceSegments = append(sentenceSegments, [2]string{seg.content, seg.separator})
		}
	}

	// 清理可能引入的空字符串和仅包含空白的字符串
	cleaned := make([][2]string, 0, len(sentenceSegments))
	for _, pair := range sentenceSegments {
		if strings.TrimSpace(pair[0]) == "" {
			continue
		}
		cleaned = append(cleaned, pair)
	}
	final := make([][2]string, 0, len(cleaned))
	for _, pair := range cleaned {
		normalized := strings.TrimSpace(innerNewlinePattern.ReplaceAllString(pair[0], " "))
		if normalized == "" {
			continue
		}
		final = append(final, [2]string{normalized, pair[1]})
	}
	return final
}

// SplitIntoSentenceSegments is the exported form of _split_into_sentence_segments.
func SplitIntoSentenceSegments(text string) [][2]string {
	return splitIntoSentenceSegments(text)
}

// ---------------------------------------------------------------------------
// random_remove_punctuation / 默认回复
// ---------------------------------------------------------------------------

// RandomRemovePunctuation mirrors random_remove_punctuation:
// 随机处理标点符号，模拟人类打字习惯（结尾句号 90% 删、逗号 5% 删 20% 转空格）。
func RandomRemovePunctuation(text string) string {
	var result strings.Builder
	runes := []rune(text)
	textLen := len(runes)

	for i, char := range runes {
		charStr := string(char)
		if charStr == "。" && i == textLen-1 { // 结尾的句号
			if randFloat() > 0.1 { // 90%概率删除结尾句号
				continue
			}
		} else if charStr == "，" {
			r := randFloat()
			if r < 0.05 { // 5%概率删除逗号
				continue
			} else if r < 0.25 { // 20%概率把逗号变成空格
				result.WriteString(" ")
				continue
			}
		}
		result.WriteString(charStr)
	}
	return result.String()
}

// getRandomDefaultReply mirrors _get_random_default_reply（7 条文案照抄）。
func getRandomDefaultReply(botNickname string) string {
	defaultReplies := []string{
		botNickname + "不知道哦",
		botNickname + "不知道",
		"不知道哦",
		"不知道",
		"不晓得",
		"懒得说",
		"()",
	}
	return defaultReplies[rand.Intn(len(defaultReplies))]
}

// RandomDefaultReply is the exported form of _get_random_default_reply.
func RandomDefaultReply(botNickname string) string {
	return getRandomDefaultReply(botNickname)
}

// ---------------------------------------------------------------------------
// _apply_response_post_processing / _merge_processed_segments_to_max_count
// ---------------------------------------------------------------------------

// applyResponsePostProcessing mirrors _apply_response_post_processing:
// 对已完成断句的文本执行错别字注入和纠正段处理。
func applyResponsePostProcessing(splitSentences [][2]string, typoOptions *TypoOptions) []ProcessedResponseSegment {
	var typoGenerator *ChineseTypoGenerator
	if typoOptions != nil {
		typoGenerator = NewChineseTypoGenerator(
			typoOptions.ErrorRate,
			typoOptions.MinFreq,
			typoOptions.ToneErrorRate,
			typoOptions.WordReplaceRate,
		)
	}
	segments := make([]ProcessedResponseSegment, 0, len(splitSentences))
	for _, pair := range splitSentences {
		sentence, sentenceSeparator := pair[0], pair[1]
		if typoGenerator != nil {
			typoedText, typoCorrections := typoGenerator.CreateTypoSentence(sentence)
			if typoCorrections != "" {
				if randFloat() < 0.5 {
					quotePrevious := typoOptions.EnableCorrectionQuote &&
						randFloat() < typoOptions.CorrectionQuoteProbability
					segments = append(segments, ProcessedResponseSegment{Text: typoedText, Separator: sentenceSeparator})
					segments = append(segments, ProcessedResponseSegment{Text: typoCorrections, QuotePrevious: quotePrevious})
				} else {
					segments = append(segments, ProcessedResponseSegment{Text: sentence, Separator: sentenceSeparator})
				}
			} else {
				segments = append(segments, ProcessedResponseSegment{Text: typoedText, Separator: sentenceSeparator})
			}
		} else {
			segments = append(segments, ProcessedResponseSegment{Text: sentence, Separator: sentenceSeparator})
		}
	}
	return segments
}

// mergeProcessedSegmentsToMaxCount mirrors _merge_processed_segments_to_max_count:
// 压缩回复段数量，并优先让需要引用的纠正内容保持在消息开头。
func mergeProcessedSegmentsToMaxCount(segments []ProcessedResponseSegment, maxCount int) []ProcessedResponseSegment {
	if len(segments) <= maxCount {
		return segments
	}
	if maxCount <= 0 {
		return []ProcessedResponseSegment{}
	}

	segmentCount := len(segments)
	requiredStarts := make([]int, 0)
	for index, segment := range segments {
		if index > 0 && segment.QuotePrevious {
			requiredStarts = append(requiredStarts, index)
		}
	}
	if len(requiredStarts) > maxCount-1 {
		requiredStarts = requiredStarts[:maxCount-1]
	}
	groupStarts := map[int]bool{0: true}
	for _, start := range requiredStarts {
		groupStarts[start] = true
	}

	// 在保留纠正消息边界后，沿用原来的均匀分组策略填充其余可用分组。
	evenlySpacedStarts := make([]int, 0, maxCount)
	startIndex := 0
	for groupIndex := 0; groupIndex < maxCount; groupIndex++ {
		remainingSegments := segmentCount - startIndex
		remainingGroups := maxCount - groupIndex
		groupSize := (remainingSegments + remainingGroups - 1) / remainingGroups
		evenlySpacedStarts = append(evenlySpacedStarts, startIndex)
		startIndex += groupSize
	}

	for _, candidateStart := range evenlySpacedStarts {
		if len(groupStarts) >= maxCount {
			break
		}
		groupStarts[candidateStart] = true
	}

	sortedStarts := sortedIntKeys(groupStarts)
	mergedSegments := make([]ProcessedResponseSegment, 0, len(sortedStarts))
	for groupIndex, groupStart := range sortedStarts {
		groupEnd := segmentCount
		if groupIndex+1 < len(sortedStarts) {
			groupEnd = sortedStarts[groupIndex+1]
		}
		group := segments[groupStart:groupEnd]
		// 组内多段被压缩为一条消息时，补回各段尾随的分隔符，避免多句硬拼接成无标点的一整坨
		mergedTextParts := make([]string, 0, len(group))
		for position, segment := range group {
			mergedTextParts = append(mergedTextParts, segment.Text)
			if position < len(group)-1 {
				mergedTextParts = append(mergedTextParts, segment.Separator)
			}
		}
		mergedSegments = append(mergedSegments, ProcessedResponseSegment{
			Text:          strings.Join(mergedTextParts, ""),
			QuotePrevious: group[0].QuotePrevious,
			Separator:     group[len(group)-1].Separator,
		})
	}

	return mergedSegments
}

func sortedIntKeys(values map[int]bool) []int {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// ---------------------------------------------------------------------------
// process_llm_response_segments（完整管线）
// ---------------------------------------------------------------------------

// bracketPattern mirrors `[(\[（](?=.*[一-鿿]).*?[)\]）]`（括号含中文剥离）。
var bracketPattern = regexp.MustCompile(
	`[(\[\x{ff08}][^\)\]\x{ff09}]*[\x{4e00}-\x{9fff}][^\)\]\x{ff09}]*[)\]\x{ff09}]`)

// normalizeReplyStyleMessages mirrors _build_requested_reply_style_message's
// style_messages table (used by the tools package; kept beside the pipeline for
// single-source parity).
var ReplyStyleMessages = map[string]string{
	"简短表达": "请简短的回复，允许句子残缺，奇怪表达，倒装，省略，符合口语习惯，符合省力随意回复习惯",
	"正常回复": "",
	"长回复":  "可以针对问题做出较为详细的评论和说明",
}

// ProcessLLMResponseSegments mirrors process_llm_response_segments:
// 处理回复文本，并保留错别字纠正消息的引用提示。
func ProcessLLMResponseSegments(text string, enableSplitter bool, enableChineseTypo bool, options PostProcessOptions) []ProcessedResponseSegment {
	if !options.EnableResponsePostProcess {
		return []ProcessedResponseSegment{{Text: text}}
	}

	botNickname := options.BotNickname
	if botNickname == "" {
		botNickname = "麦麦"
	}

	// 先保护颜文字
	var protectedText string
	var kaomojiMapping KaomojiMapping
	if options.EnableKaomojiProtection {
		protectedText, kaomojiMapping = ProtectKaomoji(text)
	} else {
		protectedText = text
		kaomojiMapping = KaomojiMapping{}
	}
	// 提取被 () 或 [] 或 （）包裹且包含中文的内容，并去除
	cleanedText := bracketPattern.ReplaceAllString(protectedText, "")

	if cleanedText == "" {
		return []ProcessedResponseSegment{{Text: "呃呃"}}
	}

	// 如果基本上是中文，则进行长度过滤（超长降级默认回复）
	maxLength := options.MaxLength * 2
	if getWesternRatio(cleanedText) < 0.1 && runeLen(cleanedText) > maxLength {
		return []ProcessedResponseSegment{{Text: getRandomDefaultReply(botNickname)}}
	}

	var splitSentences [][2]string
	if options.SplitterMode == "llm" && options.LLMSplitter != nil {
		// LLM 断句路径：失败时照抄 Python 抛错语义（由调用方决定降级）。
		pairs, err := options.LLMSplitter(cleanedText)
		if err != nil {
			return []ProcessedResponseSegment{{Text: getRandomDefaultReply(botNickname)}}
		}
		splitSentences = pairs
	} else if options.SplitterEnable && enableSplitter {
		splitSentences = splitIntoSentenceSegments(cleanedText)
	} else {
		splitSentences = [][2]string{{cleanedText, ""}}
	}

	var typoOptions *TypoOptions
	if enableChineseTypo {
		typoOptions = options.Typo
	}
	segments := applyResponsePostProcessing(splitSentences, typoOptions)

	if len(segments) > options.MaxSentenceNum {
		if options.EnableOverflowReturnAll {
			segments = []ProcessedResponseSegment{{Text: cleanedText}}
		} else {
			return []ProcessedResponseSegment{{Text: getRandomDefaultReply(botNickname)}}
		}
	}

	segments = mergeProcessedSegmentsToMaxCount(segments, options.MaxSplitNum)

	// 在所有句子处理完毕后，对包含占位符的列表进行恢复
	if options.EnableKaomojiProtection {
		texts := make([]string, 0, len(segments))
		for _, segment := range segments {
			texts = append(texts, segment.Text)
		}
		recovered := RecoverKaomoji(texts, kaomojiMapping)
		merged := make([]ProcessedResponseSegment, 0, len(segments))
		for i, segment := range segments {
			merged = append(merged, ProcessedResponseSegment{
				Text:          recovered[i],
				QuotePrevious: segment.QuotePrevious,
				Separator:     segment.Separator,
			})
		}
		segments = merged
	}

	return segments
}

// ProcessLLMResponseTexts mirrors process_llm_response（纯文本列表兼容旧调用链）。
func ProcessLLMResponseTexts(text string, enableSplitter bool, enableChineseTypo bool, options PostProcessOptions) []string {
	segments := ProcessLLMResponseSegments(text, enableSplitter, enableChineseTypo, options)
	texts := make([]string, 0, len(segments))
	for _, segment := range segments {
		texts = append(texts, segment.Text)
	}
	return texts
}

// PostProcessReplySegments mirrors BuiltinToolRuntimeContext
// .post_process_reply_segments（skip_post_process 分支 + 空段过滤 + 回退原文）。
func PostProcessReplySegments(replyText string, skipPostProcess bool, enableSplitter bool, enableChineseTypo bool, options PostProcessOptions) []ProcessedResponseSegment {
	var processedSegments []ProcessedResponseSegment
	if skipPostProcess {
		processedSegments = []ProcessedResponseSegment{{Text: strings.TrimSpace(replyText)}}
	} else {
		for _, segment := range ProcessLLMResponseSegments(replyText, enableSplitter, enableChineseTypo, options) {
			normalizedText := strings.TrimSpace(segment.Text)
			if normalizedText == "" {
				continue
			}
			processedSegments = append(processedSegments, ProcessedResponseSegment{
				Text:          normalizedText,
				QuotePrevious: segment.QuotePrevious,
			})
		}
	}
	if len(processedSegments) > 0 {
		return processedSegments
	}
	return []ProcessedResponseSegment{{Text: strings.TrimSpace(replyText)}}
}

// runeLen is len(str) in Python (rune count).
func runeLen(text string) int { return len([]rune(text)) }

// ErrSplitterMustRaise is reserved for callers that need to distinguish a
// splitter failure from a normal degradation.
var ErrSplitterMustRaise = fmt.Errorf("llm splitter failed")
