package replyer

import (
	"fmt"
	"strings"
	"testing"
)

// 断句边界：引号内不切 / 冒号两侧不切 / 数字英文间空格不切 / 换行强制切

func setDeterministicRandom(t *testing.T, value float64) {
	t.Helper()
	old := randFloat
	randFloat = func() float64 { return value }
	t.Cleanup(func() { randFloat = old })
}

func TestSplitNoMergeShortText(t *testing.T) {
	setDeterministicRandom(t, 1.0)
	segments := splitIntoSentenceSegments("今天天气。真的不错，我们一起去吧。")
	var sentences []string
	for _, pair := range segments {
		sentences = append(sentences, pair[0])
	}
	got := strings.Join(sentences, "|")
	// 句子内容不含分隔符；分隔符保存在 pair[1] 供压缩恢复
	want := "今天天气|真的不错|我们一起去吧"
	if got != want {
		t.Fatalf("断句结果不符: got=%q want=%q", got, want)
	}
	// 尾随分隔符语义：段分隔符依次为 。 ， 。（文本以句号结尾，末段带句号分隔符）
	if segments[0][1] != "。" || segments[1][1] != "，" || segments[2][1] != "。" {
		t.Fatalf("段尾分隔符应保留: %v", segments)
	}
}

func TestSplitKeepsQuoteContentWhole(t *testing.T) {
	setDeterministicRandom(t, 1.0)
	segments := splitIntoSentenceSegments("他说“你好。今天。很好”然后。就走了")
	var sentences []string
	for _, pair := range segments {
		sentences = append(sentences, pair[0])
	}
	joined := strings.Join(sentences, "|")
	// 引号内的两个句号都不是分割点
	if !strings.Contains(joined, "“你好。今天。很好”然后。") {
		t.Fatalf("引号内容应保持完整，got=%q", joined)
	}
}

func TestSplitSkipsAroundColon(t *testing.T) {
	setDeterministicRandom(t, 1.0)
	// 分隔符左右紧邻冒号（中英文）时不切；URL 场景 "//e" 邻冒号，句号后正常切
	segments := splitIntoSentenceSegments("地址: https://example.com。请查收")
	var sentences []string
	for _, pair := range segments {
		sentences = append(sentences, pair[0])
	}
	joined := strings.Join(sentences, "|")
	if !strings.Contains(joined, "https://example.com") {
		t.Fatalf("冒号两侧分隔符不应切分: got=%q", joined)
	}
}

func TestSplitSkipsAlnumSpaces(t *testing.T) {
	setDeterministicRandom(t, 1.0)
	segments := splitIntoSentenceSegments("test 123 abc。中文")
	var sentences []string
	for _, pair := range segments {
		sentences = append(sentences, pair[0])
	}
	joined := strings.Join(sentences, "|")
	if !strings.Contains(joined, "test 123 abc") {
		t.Fatalf("数字/英文字母间空格不应切分: got=%q", joined)
	}
}

func TestSplitForcesNewline(t *testing.T) {
	setDeterministicRandom(t, 1.0)
	segments := splitIntoSentenceSegments("第一句\n第二句\n\n第三句")
	var sentences []string
	for _, pair := range segments {
		sentences = append(sentences, pair[0])
	}
	if len(sentences) != 3 || strings.Join(sentences, "|") != "第一句|第二句|第三句" {
		t.Fatalf("换行应强制分割: got=%q", strings.Join(sentences, "|"))
	}
}

func TestSplitDashSpaceNotSplit(t *testing.T) {
	setDeterministicRandom(t, 1.0)
	segments := splitIntoSentenceSegments("well - known东西。好")
	var sentences []string
	for _, pair := range segments {
		sentences = append(sentences, pair[0])
	}
	if !strings.Contains(strings.Join(sentences, "|"), "well - known东西") {
		t.Fatalf("破折号两侧空格不应切分: got=%q", strings.Join(sentences, "|"))
	}
}

// 合并概率分级：<12 → 0.2、<32 → 0.6、≥32 → 0.7（合并概率 = 1 - split_strength）

func TestMergeProbabilityGrading(t *testing.T) {
	// randFloat=0.1 → 任何 <0.8 的合并概率都触发合并
	setDeterministicRandom(t, 0.1)

	short := "一。二。"                              // len=4 <12 → merge prob 0.8
	segments := splitIntoSentenceSegments(short) // 段: (一,。)(二,。)
	if len(segments) != 1 {
		t.Fatalf("短文本应大概率合并为 1 段: got %d 段 %v", len(segments), segments)
	}

	mid := "一二三四五六七。八九十甲乙丙丁戊。" // len=16 <32 → merge prob 0.4
	segments = splitIntoSentenceSegments(mid)
	if len(segments) != 1 {
		t.Fatalf("中文本应合并为 1 段: got %d 段", len(segments))
	}

	// 长文本 split_strength=0.7，merge 概率 0.3；randFloat=0.5 > 0.3 → 不合并
	setDeterministicRandom(t, 0.5)
	long := strings.Repeat("甲乙丙丁。", 10) // 40 字 ≥32
	segments = splitIntoSentenceSegments(long)
	if len(segments) != 10 {
		t.Fatalf("长文本在 merge 概率 0.3 下不应合并: got %d 段", len(segments))
	}
}

// random_remove_punctuation 概率语义

func TestRandomRemovePunctuation(t *testing.T) {
	// 照抄 Python：结尾句号在 rand > 0.1 时删除（90% 区间）；逗号 rand<0.05 删、
	// 0.05<=rand<0.25 转空格。
	setDeterministicRandom(t, 0.0)
	// rand=0.0：句号不删（0.0>0.1 为假 → 保留）；逗号删除（<0.05）
	if got := RandomRemovePunctuation("你好。好，呀。"); got != "你好。好呀。" {
		t.Fatalf("rand=0 应保留句号、删除逗号: got=%q", got)
	}

	// rand=0.1：句号不删（0.1>0.1 为假）；逗号转空格（0.05<=r<0.25）
	setDeterministicRandom(t, 0.1)
	if got := RandomRemovePunctuation("你好。好，呀。"); got != "你好。好 呀。" {
		t.Fatalf("rand=0.1 句号应保留、逗号应转空格: got=%q", got)
	}

	// rand=0.15：句号删除（0.15>0.1 为真）；逗号转空格（0.05<=r<0.25）
	setDeterministicRandom(t, 0.15)
	if got := RandomRemovePunctuation("你好。好，呀。"); got != "你好。好 呀" {
		t.Fatalf("rand=0.15 结尾句号应删除: got=%q", got)
	}

	// rand=0.5：句号删除（0.5>0.1 为真）；逗号保留
	setDeterministicRandom(t, 0.5)
	if got := RandomRemovePunctuation("你好。好，呀。"); got != "你好。好，呀" {
		t.Fatalf("rand=0.5 结尾句号应删除、逗号应保留: got=%q", got)
	}
}

// 默认回复（7 条照抄）与呃呃兜底、超长降级

func TestDefaultRepliesCopy(t *testing.T) {
	setDeterministicRandom(t, 0.0)
	nickname := "麦麦"
	expected := map[string]bool{
		nickname + "不知道哦": true,
		nickname + "不知道":  true,
		"不知道哦":            true,
		"不知道":             true,
		"不晓得":             true,
		"懒得说":             true,
		"()":              true,
	}
	for i := 0; i < 200; i++ {
		reply := RandomDefaultReply(nickname)
		if !expected[reply] {
			t.Fatalf("默认回复不在照抄清单内: %q", reply)
		}
	}
}

func TestProcessLLMResponseSegmentsBypass(t *testing.T) {
	setDeterministicRandom(t, 1.0)
	options := PostProcessOptions{EnableResponsePostProcess: false}
	segments := ProcessLLMResponseSegments("原样输出", true, true, options)
	if len(segments) != 1 || segments[0].Text != "原样输出" {
		t.Fatalf("关闭后处理应原样返回: %+v", segments)
	}
}

func TestProcessLLMResponseSegmentsEhEhFallback(t *testing.T) {
	setDeterministicRandom(t, 1.0)
	options := PostProcessOptions{
		EnableResponsePostProcess: true,
		SplitterEnable:            true,
		MaxLength:                 512,
		MaxSentenceNum:            8,
		MaxSplitNum:               3,
	}
	// 纯括号中文内容被剥离后为空 → "呃呃" 兜底
	segments := ProcessLLMResponseSegments("(哈哈哈哈哈)", true, true, options)
	if len(segments) != 1 || segments[0].Text != "呃呃" {
		t.Fatalf("空文本应兜底为「呃呃」: %+v", segments)
	}
}

func TestProcessLLMResponseSegmentsOverlongFallback(t *testing.T) {
	setDeterministicRandom(t, 1.0)
	options := PostProcessOptions{
		EnableResponsePostProcess: true,
		SplitterEnable:            true,
		MaxLength:                 16, // 2x = 32
		MaxSentenceNum:            8,
		MaxSplitNum:               3,
		BotNickname:               "麦麦",
	}
	longChinese := strings.Repeat("长", 40) // 中文占比 1.0 > 0.1 且 > 32
	segments := ProcessLLMResponseSegments(longChinese, true, true, options)
	if len(segments) != 1 {
		t.Fatalf("超长应返回单条默认回复: got %d", len(segments))
	}
	expected := map[string]bool{
		"麦麦不知道哦": true, "麦麦不知道": true, "不知道哦": true, "不知道": true,
		"不晓得": true, "懒得说": true, "()": true,
	}
	if !expected[segments[0].Text] {
		t.Fatalf("超长降级应返回默认回复: %q", segments[0].Text)
	}

	// 英文占比高（≥0.1）不降级
	setDeterministicRandom(t, 1.0)
	longEnglish := strings.Repeat("abcdefghij klmnop", 4)
	segments = ProcessLLMResponseSegments(longEnglish, true, true, options)
	if len(segments) < 1 || expected[segments[0].Text] {
		t.Fatalf("英文长文本不应触发默认回复降级: %+v", segments)
	}
}

func TestProcessLLMResponseSegmentsMaxSentenceNum(t *testing.T) {
	setDeterministicRandom(t, 1.0)
	options := PostProcessOptions{
		EnableResponsePostProcess: true,
		SplitterEnable:            true,
		MaxLength:                 512,
		MaxSentenceNum:            2,
		MaxSplitNum:               3,
		BotNickname:               "麦麦",
	}
	text := "一。二。三。四。"
	// 溢出不返回全部 → 默认回复
	segments := ProcessLLMResponseSegments(text, true, true, options)
	expected := map[string]bool{
		"麦麦不知道哦": true, "麦麦不知道": true, "不知道哦": true, "不知道": true,
		"不晓得": true, "懒得说": true, "()": true,
	}
	if !expected[segments[0].Text] {
		t.Fatalf("超出 max_sentence_num 且不回退应返回默认回复: %+v", segments)
	}

	// enable_overflow_return_all → 返回原文
	setDeterministicRandom(t, 1.0)
	options.EnableOverflowReturnAll = true
	segments = ProcessLLMResponseSegments(text, true, true, options)
	if len(segments) != 1 || segments[0].Text != text {
		t.Fatalf("enable_overflow_return_all 应返回原文: %+v", segments)
	}
}

func TestMergeProcessedSegmentsToMaxCountRestoresSeparators(t *testing.T) {
	segments := []ProcessedResponseSegment{
		{Text: "甲", Separator: "，"},
		{Text: "乙", Separator: "。"},
		{Text: "丙", Separator: "，"},
		{Text: "丁", Separator: ""},
	}
	merged := mergeProcessedSegmentsToMaxCount(segments, 2)
	if len(merged) != 2 {
		t.Fatalf("应压缩为 2 条: got %d", len(merged))
	}
	// 组内非末位段补回其尾随分隔符；组 separator 取组末段的 separator（照抄 Python）
	if merged[0].Text != "甲，乙" {
		t.Fatalf("组内应补回分隔符: got %q", merged[0].Text)
	}
	if merged[0].Separator != "。" {
		t.Fatalf("组 separator 应取组末段 separator: %q", merged[0].Separator)
	}
}

func TestQuotePreviousSurvivesCompression(t *testing.T) {
	segments := []ProcessedResponseSegment{
		{Text: "甲", Separator: "，"},
		{Text: "乙", Separator: "。"},
		{Text: "丙", QuotePrevious: true, Separator: "，"},
		{Text: "丁", Separator: ""},
	}
	merged := mergeProcessedSegmentsToMaxCount(segments, 3)
	// 纠正段（index=2）必须保持为组起点
	found := false
	for _, segment := range merged {
		if segment.QuotePrevious && strings.HasPrefix(segment.Text, "丙") {
			found = true
		}
	}
	if !found {
		t.Fatalf("quote_previous 段应保持为组起点: %+v", merged)
	}
}

// 颜文字保护

func TestKaomojiProtectAndRecover(t *testing.T) {
	text := "你好(￣▽￣)今天天气不错(¬_¬)"
	protected, mapping := ProtectKaomoji(text)
	if strings.Contains(protected, "￣▽￣") {
		t.Fatalf("颜文字应被替换为占位符: %q", protected)
	}
	if len(mapping) != 2 {
		t.Fatalf("应保护 2 个颜文字: got %d (%v)", len(mapping), mapping)
	}
	recovered := RecoverKaomoji([]string{protected}, mapping)
	if recovered[0] != text {
		t.Fatalf("恢复应还原原文: got %q want %q", recovered[0], text)
	}
}

func TestKaomojiSkipsStickerPlaceholder(t *testing.T) {
	protected, mapping := ProtectKaomoji("[表情包]描述")
	if len(mapping) != 0 || !strings.Contains(protected, "[表情包]") {
		t.Fatalf("「[表情包…]」占位不应被保护: %q %v", protected, mapping)
	}
}

// get_western_ratio

func TestWesternRatio(t *testing.T) {
	if got := getWesternRatio("你好世界"); got != 0.0 {
		t.Fatalf("纯中文比例应为 0: %v", got)
	}
	// alnum=[h,e,l,l,o,你,好] → western 5/7≈0.714
	if got := getWesternRatio("hello你好"); got < 0.71 || got > 0.72 {
		t.Fatalf("hello你好 西文比例应约 0.714（5/7）: %v", got)
	}
	if got := getWesternRatio("。。。"); got != 0.0 {
		t.Fatalf("无字母数字字符应为 0: %v", got)
	}
}

// typing_time

func TestCalculateTypingTime(t *testing.T) {
	// 纯中文：0.3/字
	if got := CalculateTypingTime("你好", 1.0, false); got != 0.6 {
		t.Fatalf("两个中文字应 0.6s: %v", got)
	}
	// 纯英文：0.15/字
	if got := CalculateTypingTime("hi", 1.0, false); got != 0.3 {
		t.Fatalf("两个英文字应 0.3s: %v", got)
	}
	// 单个中文字符（strip 后长度 1）：3 倍 + 0.3 回车
	if got := CalculateTypingTime("好", 1.0, false); got != 1.2 {
		t.Fatalf("单中文字应 3*0.3+0.3=1.2s: %v", got)
	}
	// emoji 固定 1 秒
	if got := CalculateTypingTime("[表情]", 1.0, true); got != 1 {
		t.Fatalf("is_emoji 应固定 1s: %v", got)
	}
	// typing_speed 缩放
	if got := CalculateTypingTime("你好", 2.0, false); got != 1.2 {
		t.Fatalf("typing_speed=2 应加倍: %v", got)
	}
	// typing_speed<=0 → 0
	if got := CalculateTypingTime("你好", 0, false); got != 0 {
		t.Fatalf("typing_speed=0 应返回 0: %v", got)
	}
}

// LLM 断句（splitter_llm.go）

func TestSplitTextWithLLMNoSplit(t *testing.T) {
	ctx := testContext()
	chatFunc := func(request ReplyerChatRequest) (string, error) { panic("unused") }
	_ = chatFunc
	result, err := SplitTextWithLLM(ctx, "一句话。不拆分。", func(_ contextT, request ReplyerChatRequest) (string, error) {
		return "no_split", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0][0] != "一句话。不拆分。" {
		t.Fatalf("no_split 应返回原文单段: %+v", result)
	}
}

func TestSplitTextWithLLMValidatesPreservation(t *testing.T) {
	result, err := SplitTextWithLLM(testContext(), "第一句。第二句。", func(_ contextT, request ReplyerChatRequest) (string, error) {
		return `["第一句。第二句。"]`, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("原文应保持完整: %+v", result)
	}

	// 篡改原文必须报错
	_, err = SplitTextWithLLM(testContext(), "第一句。第二句。", func(_ contextT, request ReplyerChatRequest) (string, error) {
		return `["第一句。 第二句。"]`, nil
	})
	if err == nil || !strings.Contains(err.Error(), "未完整保留原文") {
		t.Fatalf("篡改原文应报未保留错误: %v", err)
	}
}

func TestSplitTextWithLLMMarkdownFence(t *testing.T) {
	result, err := SplitTextWithLLM(testContext(), "甲，乙。", func(_ contextT, request ReplyerChatRequest) (string, error) {
		return "```json\n[\"甲，\", \"乙。\"]\n```", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result[0][0] != "甲" || result[0][1] != "，" {
		t.Fatalf("段尾逗号应移入分隔符: %+v", result)
	}
}

// 错别字：错误率语义（确定性驱动，不用真实随机路径）

func TestTypoGeneratorDeterministicNoTypo(t *testing.T) {
	// rand.Float64 无法直接注入；但 error_rate=0 时单字替换必不触发，
	// word_replace_rate=0 时整词替换必不触发 → 输出必须等于输入。
	generator := NewChineseTypoGenerator(0, 9, 0, 0)
	typoed, correction := generator.CreateTypoSentence("今天天气真的不错，我们一起去吃饭吧。")
	if typoed != "今天天气真的不错，我们一起去吃饭吧。" {
		t.Fatalf("error_rate=0 时输出应与原文一致: %q", typoed)
	}
	if correction != "" {
		t.Fatalf("无错别字时不应有纠正建议: %q", correction)
	}
}

func TestTypoGeneratorHomophoneCandidateSemantics(t *testing.T) {
	generator := NewChineseTypoGenerator(0, 9, 0, 0)
	// 错字候选必须与原字同音（正确声调或错声调）
	similar := generator.getSimilarFrequencyChars("吃", "chi1", 5)
	for _, candidate := range similar {
		pys := pinyinOf(candidate)
		if pys == "" {
			t.Fatalf("候选字应有拼音: %q", candidate)
		}
		base := pys[:len(pys)-1]
		origBase := "chi1"[:len("chi1")-1]
		if base != origBase {
			t.Fatalf("候选 %q(%s) 与 吃(chi1) 不同音: %q", candidate, pys, base)
		}
	}
	// 高频原字（一）的替换概率高于低频字
	if len(similar) > 5 {
		t.Fatalf("候选数量应 ≤5: %d", len(similar))
	}
}

func TestTypoGeneratorReplacementProbability(t *testing.T) {
	generator := NewChineseTypoGenerator(0.01, 9, 0.1, 0.006)
	generator.maxFreqDiff = 200
	// 目标频率更高 → 1.0
	if got := generator.calculateReplacementProbability(100, 200); got != 1.0 {
		t.Fatalf("目标频率更高时应为 1.0: %v", got)
	}
	// 频率差超上限 → 0
	if got := generator.calculateReplacementProbability(500, 100); got != 0.0 {
		t.Fatalf("频率差>max_freq_diff 应为 0: %v", got)
	}
	// 指数衰减：freq_diff=0 → 1
	if got := generator.calculateReplacementProbability(100, 100); got <= 0.99 || got > 1.0 {
		t.Fatalf("频率差为 0 应接近 1: %v", got)
	}
}

func TestTypoSentenceEndsWithCorrectionFromTypos(t *testing.T) {
	// error_rate=1、tone_error_rate=0：每个汉字都替换成正确声调的同音字；
	// 纠正建议 50% 概率出现，出现时必须是原文中的一个字。
	generator := NewChineseTypoGenerator(1.0, 1, 0, 0)
	original := "天气不错"
	for range 30 {
		typoed, correction := generator.CreateTypoSentence(original)
		if typoed == original {
			continue // 全部候选耗尽时可能不变
		}
		if correction != "" {
			if !strings.Contains(original, correction) && !strings.Contains(typoed, correction) {
				// 纠正建议应为原文/错字句中的字
				t.Fatalf("纠正建议 %q 不在原句中", correction)
			}
		}
		return
	}
}

func TestTypoSegmenterChinese(t *testing.T) {
	generator := NewChineseTypoGenerator(0, 9, 0, 0)
	words := generator.segmentSentence("今天天气不错")
	if len(words) < 2 {
		t.Fatalf("gse 分词应拆出多个词: %v", words)
	}
}

func TestTypoWordHomophonesRequireDictWords(t *testing.T) {
	generator := NewChineseTypoGenerator(0, 9, 0, 0)
	homophones := generator.getWordHomophones("吃饭")
	for _, word := range homophones {
		if lookupWordFrequency(word) <= 0 {
			t.Fatalf("整词候选必须是真实词条: %q", word)
		}
	}
	if len(homophones) > 5 {
		t.Fatalf("整词候选应 ≤5: %d", len(homophones))
	}
}

// 表达选择器

func TestWeightedSampleFallbackUniform(t *testing.T) {
	population := make([]ExpressionCandidate, 12)
	for i := range population {
		population[i] = ExpressionCandidate{ID: int64(i + 1), Situation: "s", Style: "t", Count: 1}
	}
	sampled := WeightedSample(population, 5)
	if len(sampled) != 5 {
		t.Fatalf("应抽取 5 条: %d", len(sampled))
	}
	seen := map[int64]bool{}
	for _, item := range sampled {
		if seen[item.ID] {
			t.Fatalf("抽样不应重复: %v", sampled)
		}
		seen[item.ID] = true
	}
}

func TestSampleLegacyExpressionCandidates(t *testing.T) {
	population := make([]ExpressionCandidate, 12)
	for i := range population {
		population[i] = ExpressionCandidate{ID: int64(i + 1), Situation: "s", Style: "t", Count: i + 1}
	}
	pool := SampleLegacyExpressionCandidates(population)
	if len(pool) == 0 || len(pool) > 10 {
		t.Fatalf("随手候选应 ≤10 条: %d", len(pool))
	}
}

func TestExpressionSelectorInsufficientCandidates(t *testing.T) {
	selector := &MaisakaExpressionSelector{
		Source: &fakeExpressionSource{candidates: make([]ExpressionCandidate, 5)},
	}
	result := selector.SelectForReply(testContext(), SelectionInput{SessionID: "s"})
	if result.ExpressionHabits != "" || len(result.SelectedExpressionIDs) != 0 {
		t.Fatalf("候选不足 10 应返回空结果: %+v", result)
	}
}

func TestExpressionSelectorDirectInjectionWithoutSubAgent(t *testing.T) {
	selector := &MaisakaExpressionSelector{
		Source: &fakeExpressionSource{candidates: fakeCandidates(12)},
	}
	result := selector.SelectForReply(testContext(), SelectionInput{SessionID: "s"})
	// 随手候选池：高频 5 + 随机 5 去重（≤10）；直接注入返回池内全部候选
	if len(result.SelectedExpressionIDs) == 0 || len(result.SelectedExpressionIDs) > 10 {
		t.Fatalf("直接注入应返回随手候选池: %d", len(result.SelectedExpressionIDs))
	}
	if !strings.Contains(result.ExpressionHabits, "【表达习惯参考，请视情况自然的使用】") {
		t.Fatalf("表达习惯块文案应照抄: %q", result.ExpressionHabits)
	}
}

func TestExpressionSelectorSubAgentParsesJSON(t *testing.T) {
	selector := &MaisakaExpressionSelector{
		Source: &fakeExpressionSource{candidates: fakeCandidates(12)},
		SubAgent: func(_ contextT, systemPrompt string) (string, error) {
			if !strings.Contains(systemPrompt, "selected_ids") {
				return "", fmt.Errorf("selector prompt 应包含 selected_ids 约定")
			}
			// 从 prompt 的候选行中提取前两个 id，模拟模型只选合法 id
			ids := parseCandidateIDsFromPrompt(systemPrompt)
			if len(ids) < 2 {
				return "", fmt.Errorf("prompt 候选行解析失败")
			}
			return fmt.Sprintf(`{"selected_ids":[%d,%d]}`, ids[0], ids[1]), nil
		},
	}
	result := selector.SelectForReply(testContext(), SelectionInput{SessionID: "s"})
	if len(result.SelectedExpressionIDs) != 2 {
		t.Fatalf("子代理选择应生效: %v", result.SelectedExpressionIDs)
	}
	if len(result.SelectedExpressions) != 2 {
		t.Fatalf("已选表达详情应同步返回: %v", result.SelectedExpressions)
	}
}

func TestExpressionSelectorSubAgentFailureFallsBack(t *testing.T) {
	selector := &MaisakaExpressionSelector{
		Source: &fakeExpressionSource{candidates: fakeCandidates(12)},
		SubAgent: func(_ contextT, _ string) (string, error) {
			return "", fmt.Errorf("boom")
		},
	}
	result := selector.SelectForReply(testContext(), SelectionInput{SessionID: "s"})
	if len(result.SelectedExpressionIDs) == 0 {
		t.Fatalf("子代理失败应回退直接注入")
	}
}

func TestExpressionHabitsBlockFormat(t *testing.T) {
	block := BuildExpressionHabitsBlock([]ExpressionCandidate{
		{ID: 1, Situation: "开心时", Style: "哈哈哈"},
	})
	want := "【表达习惯参考，请视情况自然的使用】\n- 当“开心时”时，可以用“哈哈哈”来表达。"
	if block != want {
		t.Fatalf("表达习惯块文案应照抄:\n got=%q\nwant=%q", block, want)
	}
}

// generator 人设拼接

func TestBuildPersonalityPrompt(t *testing.T) {
	generator := &BaseMaisakaReplyGenerator{
		Config: GeneratorConfig{
			BotNickname:  "麦麦",
			AliasNames:   []string{"小麦", "MM"},
			Personality:  "是一个善于思考的 AI。",
			EmotionTrait: "rational_calm",
		},
	}
	got := generator.BuildPersonalityPrompt()
	want := "你的名字是麦麦，也有人叫你小麦,MM。\n是一个善于思考的 AI。\n你整体理性冷静，回应时更偏向客观、克制和清晰判断，少用强烈情绪表达。"
	if got != want {
		t.Fatalf("人设拼接应照抄:\n got=%q\nwant=%q", got, want)
	}

	// 空人格回退 "是人类。"；neutral 情绪后缀为空
	generator2 := &BaseMaisakaReplyGenerator{
		Config: GeneratorConfig{BotNickname: "麦麦", EmotionTrait: "neutral"},
	}
	got2 := generator2.BuildPersonalityPrompt()
	if got2 != "你的名字是麦麦。\n是人类。" {
		t.Fatalf("空人格应回退: %q", got2)
	}
}

func TestPersonalityEmotionSuffixesCopy(t *testing.T) {
	if buildPersonalityEmotionSuffix("neutral") != "" {
		t.Fatal("neutral 后缀应为空")
	}
	if buildPersonalityEmotionSuffix("sentimental") !=
		"你更敏感细腻，容易被聊天氛围触动，会自然表现出一点惆怅、共情或情绪波动，但不要过度煽情。" {
		t.Fatal("sentimental 后缀文案应照抄")
	}
}

func TestSelectTemporaryReplyStyle(t *testing.T) {
	generator := &BaseMaisakaReplyGenerator{
		Config: GeneratorConfig{
			MultipleReplyStyle:  []string{"风格A", "风格B"},
			MultipleProbability: 0, // 概率为 0 → 永不选择
		},
	}
	if got := generator.SelectTemporaryReplyStyle(); got != "" {
		t.Fatalf("multiple_probability=0 应返回空: %q", got)
	}
}

func TestRequestedReplyStyleMessages(t *testing.T) {
	if ReplyStyleMessages["简短表达"] != "请简短的回复，允许句子残缺，奇怪表达，倒装，省略，符合口语习惯，符合省力随意回复习惯" {
		t.Fatal("简短表达文案应照抄")
	}
	if ReplyStyleMessages["长回复"] != "可以针对问题做出较为详细的评论和说明" {
		t.Fatal("长回复文案应照抄")
	}
	if ReplyStyleMessages["正常回复"] != "" {
		t.Fatal("正常回复应为空")
	}
}

func TestDuplicateTargetReplyReminderTemplate(t *testing.T) {
	rendered := strings.ReplaceAll(DuplicateTargetReplyReminderTemplate, "{previous_reply}", "之前的回复")
	want := "你刚刚已经回复过这条消息，你刚刚的发言是：“之前的回复”\n" +
		"你现在想再次回复这条消息，进行补充，注意请不要和之前你的发言重复。"
	if rendered != want {
		t.Fatalf("重复回复提醒文案应照抄:\n got=%q\nwant=%q", rendered, want)
	}
}

func TestBuildTargetMessageBlockSelfAndOther(t *testing.T) {
	generator := &BaseMaisakaReplyGenerator{Config: GeneratorConfig{
		BotNickname: "麦麦",
		IsBotSelf:   func(platform string, userID string) bool { return userID == "bot1" },
	}}
	other := makeSessionMessage("user1", "张三", "你好啊", false)
	block := generator.buildTargetMessageBlock(other)
	if !strings.Contains(block, "你想要回复的消息是 张三 发送的 msg_id为") {
		t.Fatalf("目标消息块文案应照抄: %q", block)
	}
	self := makeSessionMessage("bot1", "麦麦", "我之前说的话", true)
	selfBlock := generator.buildTargetMessageBlock(self)
	if !strings.Contains(selfBlock, "你想要补充说明你自己（麦麦） 发送的 msg_id为") {
		t.Fatalf("自身消息块文案应照抄: %q", selfBlock)
	}
}

// parseCandidateIDsFromPrompt extracts candidate ids from the selector prompt
// ("123: 情景=... | 风格=..." lines).
func parseCandidateIDsFromPrompt(prompt string) []int64 {
	var ids []int64
	for _, line := range strings.Split(prompt, "\n") {
		idx := strings.Index(line, ": 情景=")
		if idx <= 0 {
			continue
		}
		var id int64
		if _, err := fmt.Sscanf(line[:idx], "%d", &id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}
