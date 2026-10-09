package replyeffect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// This file mirrors 参考/MaiBot/src/maisaka/reply_effect/judge.py
// （行为与 prompt 文案以该文件为权威）：严格的语义归因评审 + JSON 解析校验。
//
// LLM 边界：JudgeRunner 是包内最小注入接口（不 import internal/llm 具体类型）。
// Python 版走 runtime.run_sub_agent(request_kind="reply_effect_judge",
// model_task_name="utils", response_format=JSON_OBJ)：系统提示词为
// JudgeSystemPrompt，消息体为构建出的评审 prompt。

// JudgeSystemPrompt mirrors _run_reply_effect_judge 的 system_prompt（文案照抄）。
const JudgeSystemPrompt = "你是回复效果评分器。请严格按用户给出的 JSON 格式输出，不要输出 JSON 之外的内容。"

// JudgeDisplayPrefix mirrors display_prefix="[回复效果评分任务]"。
const JudgeDisplayPrefix = "[回复效果评分任务]"

// Prompt 预算常数（照抄 judge.py）。
const (
	// MaxContextItems mirrors MAX_CONTEXT_ITEMS。
	MaxContextItems = 20
	// MaxPromptChars mirrors MAX_PROMPT_CHARS。
	MaxPromptChars = 12_000
	// judgeContextBudgetRatio mirrors 0.20（上下文配额比例）。
	judgeContextBudgetRatio = 0.20
	// judgeCandidateBudgetRatio mirrors 0.40（候选配额比例）。
	judgeCandidateBudgetRatio = 0.40
	// maxJudgeAttempts mirrors `for attempt in range(2)`。
	maxJudgeAttempts = 2
)

// JudgeRunner 是评审用的最小 LLM 边界：输入 prompt，返回模型原始文本。
// 装配层适配 internal/llm（task="utils"、request_type="reply_effect_judge"、
// JSON 对象响应格式、单条 tool_hint 参考消息 + JudgeSystemPrompt）。
type JudgeRunner func(ctx context.Context, prompt string) (string, error)

// Judgment 是一次成功解析的评审结果。
type Judgment struct {
	Primary            string
	Secondary          []string
	StrategyConfidence float64
	// Associations 按后续消息 id 索引（每条后续消息都在键集合中，可为空切片）。
	Associations map[string][]ReplyAssociation
}

// JudgeReplyEffect mirrors judge_reply_effect：严格语义归因评审，
// 失败后携带校验错误重试一次；两次都失败返回错误。
func JudgeReplyEffect(ctx context.Context, record *Record, candidates []*Record, bot BotIdentity, runner JudgeRunner) (*Judgment, error) {
	if runner == nil {
		return nil, fmt.Errorf("未提供 LLM judge runner")
	}
	prompt := BuildJudgePrompt(record, candidates, bot)
	lastError := ""
	for attempt := 0; attempt < maxJudgeAttempts; attempt++ {
		activePrompt := prompt
		if lastError != "" {
			activePrompt = prompt + "\n\n上一次输出校验失败：" + lastError + "\n请修正后重新输出完整 JSON。"
		}
		responseText, err := runner(ctx, activePrompt)
		if err != nil {
			return nil, err
		}
		payload, err := loadJSONObject(responseText)
		if err != nil {
			lastError = err.Error()
			continue
		}
		result, err := ParseJudgeResult(payload, record, candidates)
		if err != nil {
			lastError = err.Error()
			continue
		}
		return result, nil
	}
	return nil, fmt.Errorf("回复效果评审连续两次校验失败：%s", lastError)
}

// BuildJudgePrompt mirrors build_judge_prompt（prompt 文案与配额分配照抄）。
func BuildJudgePrompt(record *Record, candidates []*Record, bot BotIdentity) string {
	names := bot.Names()
	botIdentity := "（未配置昵称）"
	if len(names) > 0 {
		botIdentity = strings.Join(names, "、")
	}
	instruction := fmt.Sprintf("回复效果评估标准版本：v%d。\n", EvaluationVersion) +
		"Bot 身份名称：" + botIdentity + "。这些名称在对话中指向当前 Bot，而不是第三方；" +
		"出现名称本身不自动证明关联，仍需结合评价内容和连续语境判断。\n" +
		"请先判断每条后续用户消息是否确实承接、回应或评价了某条候选 Bot 回复；" +
		"观察窗口中的消息不一定与 Bot 有关。\n" +
		"允许候选只限定消息发生时可以选择的 Bot 回复范围，不代表这些回复与消息实际相关。\n" +
		"没有明确的引用、回复、指代、语义承接或情绪反馈证据时，必须返回 associations: []；" +
		"不要为了评价话题转移、无人回应或回复未吸引讨论而强行建立关联。\n" +
		"候选 Bot 回复一定先于与其关联的后续用户消息，请严格按照发送时间判断先后关系。\n" +
		"必须把同一用户的相邻消息作为连续表达判断：如果前一条消息已明确承接某个候选 Bot 回复，" +
		"紧邻消息继续评价 Bot 的回答、能力、人格或近期表现，应继承该对话焦点；" +
		"只有明确换题、引用第三方或出现新的指向对象时才终止继承。\n" +
		"明确称呼 Bot 并评价其回答或表现属于 bot_content 或 bot_persona 情绪反馈，" +
		"不能仅因没有显式引用而返回空关联。若评价明确覆盖多轮 Bot 表现，可以关联多个候选；" +
		"不得只因存在多个候选就机械关联全部候选。\n" +
		"每条后续消息可以关联零个、一个或多个候选 bot 回复；只能返回给出的 candidate_id。\n" +
		"messages 必须逐条覆盖给出的全部后续消息，即使无关也要返回该 message_id 和空 associations。\n" +
		"只对已经确认存在关联的消息评价两个轴：评价目标 stance_target 与讨论贡献 contribution。\n" +
		"对话题或第三方的负面喜好不等于反感 bot；指出 bot 的事实错误使用 factual_correction + advance；" +
		"针对 bot 的厌烦或攻击使用 bot_attack + wrong_push。"
	outputContract := "策略 primary 只能是 " + strings.Join(Strategies, "/") + "；" +
		"secondary 也只能使用这些值。\n" +
		"stance_target 只能是 " + strings.Join(StanceTargets, "/") + "。\n" +
		"stance 只能是 " + strings.Join(Stances, "/") + "。\n" +
		"contribution 只能是 " + strings.Join(Contributions, "/") + "；" +
		"无关消息必须使用空 associations，不存在 unrelated 关联标签。\n" +
		"confidence 与 attribution_confidence 必须是 0 到 1。\n" +
		"association 中的 candidate_id 必须使用候选 Bot 回复前的短编号。\n" +
		"严格输出；下面同时给出无关联与有关联消息的格式：\n" +
		"{\n" +
		"  \"strategy\": {\"primary\": \"answer\", \"secondary\": [], \"confidence\": 0.8},\n" +
		"  \"messages\": [\n" +
		"    {\"message_id\": \"无关联消息ID\", \"associations\": []},\n" +
		"    {\"message_id\": \"有关联消息ID\", \"associations\": [{\"candidate_id\": \"c1\", " +
		"\"attribution_confidence\": 0.8, \"stance_target\": \"bot_content\", \"stance\": \"neutral\", " +
		"\"contribution\": \"advance\", \"reason\": \"具体关联理由\", " +
		"\"evidence_spans\": [\"来自后续用户消息的证据\"], \"confidence\": 0.8}]}\n" +
		"  ]\n" +
		"}"
	sectionTemplate := "{instruction}\n\n" +
		"Bot 回复前的聊天上下文（按发送顺序排列）：\n{context}\n\n" +
		"待评估的 Bot 回复：\n{candidates}\n\n" +
		"Bot 回复后的用户消息（按发送顺序排列）：\n{followups}\n\n" +
		"{output_contract}"
	render := func(contextText, candidateText, followupText string) string {
		replaced := strings.NewReplacer(
			"{instruction}", instruction,
			"{context}", contextText,
			"{candidates}", candidateText,
			"{followups}", followupText,
			"{output_contract}", outputContract,
		)
		return replaced.Replace(sectionTemplate)
	}
	fixedLength := utf8.RuneCountInString(render("（无）", "（无）", "（无）"))
	contentBudget := MaxPromptChars - fixedLength
	if contentBudget < 0 {
		contentBudget = 0
	}
	candidateAliases := map[string]string{}
	for index, candidate := range candidates {
		candidateAliases[candidate.EffectID] = fmt.Sprintf("c%d", index+1)
	}
	candidateMinimum := candidateRequiredChars(candidates)
	followupMinimum := followupRequiredChars(record.FollowupMessages, candidateAliases)
	remainingBudget := contentBudget - candidateMinimum - followupMinimum
	if remainingBudget < 0 {
		remainingBudget = 0
	}
	contextBudget := int(float64(contentBudget) * judgeContextBudgetRatio)
	if third := remainingBudget / 3; third < contextBudget {
		contextBudget = third
	}
	remainingBudget -= contextBudget
	candidateBudget := candidateMinimum + int(float64(remainingBudget)*judgeCandidateBudgetRatio)
	followupBudget := followupMinimum + remainingBudget - int(float64(remainingBudget)*judgeCandidateBudgetRatio)

	contextText := formatContext(record.ContextSnapshot, record.Reply.TargetMessageID, contextBudget)
	if contextText == "" {
		contextText = "（无）"
	}
	candidateText := formatCandidates(candidates, candidateAliases, candidateBudget)
	if candidateText == "" {
		candidateText = "（无）"
	}
	followupText := formatFollowups(record.FollowupMessages, candidateAliases, followupBudget)
	if followupText == "" {
		followupText = "（无）"
	}
	return render(contextText, candidateText, followupText)
}

// ParseJudgeResult mirrors parse_judge_result：逐字段枚举/概率校验，
// 后续消息必须被完整覆盖，显式引用关联不得遗漏。
func ParseJudgeResult(payload map[string]any, record *Record, candidates []*Record) (*Judgment, error) {
	candidateIDs := map[string]bool{}
	effectIDsByAlias := map[string]string{}
	for index, candidate := range candidates {
		candidateIDs[candidate.EffectID] = true
		effectIDsByAlias[fmt.Sprintf("c%d", index+1)] = candidate.EffectID
	}
	followupIDs := map[string]bool{}
	allowedEffectsByMessage := map[string]map[string]bool{}
	lockedEffects := map[string]map[string]bool{}
	for index := range record.FollowupMessages {
		followup := &record.FollowupMessages[index]
		followupIDs[followup.MessageID] = true
		allowed := map[string]bool{}
		for _, effectID := range followup.CandidateEffectIDs {
			allowed[effectID] = true
		}
		allowedEffectsByMessage[followup.MessageID] = allowed
		locked := map[string]bool{}
		for _, association := range followup.Associations {
			if association.AttributionType == AttributionExplicitQuote {
				locked[association.EffectID] = true
			}
		}
		lockedEffects[followup.MessageID] = locked
	}

	strategy, err := requireDict(payload["strategy"], "strategy")
	if err != nil {
		return nil, err
	}
	primary, err := requireEnum(strategy["primary"], Strategies, "strategy.primary")
	if err != nil {
		return nil, err
	}
	rawSecondary := strategy["secondary"]
	if rawSecondary == nil {
		rawSecondary = []any{}
	}
	secondaryList, ok := rawSecondary.([]any)
	if !ok {
		return nil, fmt.Errorf("strategy.secondary 必须是数组")
	}
	secondary := make([]string, 0, len(secondaryList))
	seenSecondary := map[string]bool{}
	for _, item := range secondaryList {
		value, err := requireEnum(item, Strategies, "strategy.secondary")
		if err != nil {
			return nil, err
		}
		if seenSecondary[value] {
			continue
		}
		seenSecondary[value] = true
		secondary = append(secondary, value)
	}
	strategyConfidence, err := requireProbability(strategy["confidence"], "strategy.confidence")
	if err != nil {
		return nil, err
	}

	rawMessages, ok := payload["messages"]
	if !ok {
		return nil, fmt.Errorf("messages 必须是数组")
	}
	messages, ok := rawMessages.([]any)
	if !ok {
		return nil, fmt.Errorf("messages 必须是数组")
	}
	parsed := map[string][]ReplyAssociation{}
	seenMessages := map[string]bool{}
	for _, rawMessage := range messages {
		message, err := requireDict(rawMessage, "messages[]")
		if err != nil {
			return nil, err
		}
		messageID := trimSpace(toStringValue(message["message_id"]))
		if !followupIDs[messageID] || seenMessages[messageID] {
			return nil, fmt.Errorf("未知或重复的后续消息 ID：%s", messageID)
		}
		seenMessages[messageID] = true
		rawAssociations, ok := message["associations"].([]any)
		if !ok {
			return nil, fmt.Errorf("消息 %s 的 associations 必须是数组", messageID)
		}
		associations := make([]ReplyAssociation, 0, len(rawAssociations))
		seenEffects := map[string]bool{}
		for _, rawAssociation := range rawAssociations {
			item, err := requireDict(rawAssociation, "associations[]")
			if err != nil {
				return nil, err
			}
			candidateID := trimSpace(toStringValue(item["candidate_id"]))
			effectID := effectIDsByAlias[candidateID]
			if effectID == "" || !candidateIDs[effectID] || seenEffects[effectID] {
				return nil, fmt.Errorf("消息 %s 包含未知或重复候选：%s", messageID, candidateID)
			}
			if !allowedEffectsByMessage[messageID][effectID] {
				return nil, fmt.Errorf("消息 %s 关联了当时尚不存在或已结束观察的 Bot 回复：%s", messageID, effectID)
			}
			seenEffects[effectID] = true
			evidenceSpans := make([]string, 0)
			if rawEvidence := item["evidence_spans"]; rawEvidence != nil {
				list, ok := rawEvidence.([]any)
				if !ok {
					return nil, fmt.Errorf("evidence_spans 必须是数组")
				}
				for _, span := range list {
					if value := trimSpace(toStringValue(span)); value != "" {
						evidenceSpans = append(evidenceSpans, value)
					}
				}
			}
			attributionConfidence, err := requireProbability(item["attribution_confidence"], "attribution_confidence")
			if err != nil {
				return nil, err
			}
			stanceTarget, err := requireEnum(item["stance_target"], StanceTargets, "stance_target")
			if err != nil {
				return nil, err
			}
			stance, err := requireEnum(item["stance"], Stances, "stance")
			if err != nil {
				return nil, err
			}
			contribution, err := requireEnum(item["contribution"], Contributions, "contribution")
			if err != nil {
				return nil, err
			}
			evaluatorConfidence, err := requireProbability(item["confidence"], "confidence")
			if err != nil {
				return nil, err
			}
			associations = append(associations, ReplyAssociation{
				EffectID:              effectID,
				AttributionType:       AttributionSemantic,
				AttributionConfidence: attributionConfidence,
				StanceTarget:          stanceTarget,
				Stance:                stance,
				Contribution:          contribution,
				Reason:                trimSpace(toStringValue(item["reason"])),
				EvidenceSpans:         evidenceSpans,
				EvaluatorConfidence:   evaluatorConfidence,
			})
		}
		for locked := range lockedEffects[messageID] {
			if !seenEffects[locked] {
				return nil, fmt.Errorf("消息 %s 的显式引用关联被遗漏", messageID)
			}
		}
		parsed[messageID] = associations
	}
	missing := make([]string, 0)
	for messageID := range followupIDs {
		if !seenMessages[messageID] {
			missing = append(missing, messageID)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("评审结果未覆盖全部后续消息，缺少 message_id：%s", strings.Join(missing, "、"))
	}
	return &Judgment{
		Primary:            primary,
		Secondary:          secondary,
		StrategyConfidence: strategyConfidence,
		Associations:       parsed,
	}, nil
}

// ---------------------------------------------------------------------------
// prompt 分段格式化（照抄 judge.py 的 _format_*）
// ---------------------------------------------------------------------------

func formatContext(contextSnapshot []ContextItem, targetMessageID string, maxChars int) string {
	indexes := make([]int, 0, len(contextSnapshot))
	for index := range contextSnapshot {
		source := contextSnapshot[index].Source
		if source == "user" || source == "guided_reply" {
			indexes = append(indexes, index)
		}
	}
	if len(indexes) > MaxContextItems {
		indexes = indexes[len(indexes)-MaxContextItems:]
	}
	// 目标消息不在选中窗口时，把它插回窗口（保留最近 MAX_CONTEXT_ITEMS-1 条）。
	targetIndex := -1
	for _, index := range indexes {
		if targetMessageID != "" && contextSnapshot[index].MessageID == targetMessageID {
			targetIndex = index
			break
		}
	}
	if targetIndex < 0 && targetMessageID != "" {
		for _, index := range indexesOf(contextSnapshot, targetMessageID) {
			trimmed := indexes
			if len(trimmed) > MaxContextItems-1 {
				trimmed = trimmed[len(trimmed)-(MaxContextItems-1):]
			}
			indexes = append([]int{index}, trimmed...)
			sortInts(indexes)
			break
		}
	}
	lines := make([]string, 0, len(indexes))
	used := 0
	for _, index := range indexes {
		item := contextSnapshot[index]
		speaker := "用户"
		if item.Source == "guided_reply" {
			speaker = "Bot"
		} else if item.Sender != nil && item.Sender.DisplayName != "" {
			speaker = item.Sender.DisplayName
		}
		targetMark := ""
		if targetMessageID != "" && item.MessageID == targetMessageID {
			targetMark = "（触发当前 Bot 回复）"
		}
		line := fmt.Sprintf("- [%s] %s%s: %s", item.Timestamp, speaker, targetMark,
			NormalizeTextForPrompt(item.Text, 300))
		if used+utf8.RuneCountInString(line)+1 > maxChars {
			break
		}
		lines = append(lines, line)
		used += utf8.RuneCountInString(line) + 1
	}
	return strings.Join(lines, "\n")
}

func indexesOf(contextSnapshot []ContextItem, messageID string) []int {
	found := make([]int, 0, 1)
	for index := range contextSnapshot {
		source := contextSnapshot[index].Source
		if source != "user" && source != "guided_reply" {
			continue
		}
		if contextSnapshot[index].MessageID == messageID {
			found = append(found, index)
		}
	}
	return found
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func candidatePrefixes(candidates []*Record, aliases map[string]string) []string {
	prefixes := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		prefixes = append(prefixes, fmt.Sprintf("- candidate_id=%s time=%s Bot: ",
			aliases[candidate.EffectID], candidate.CreatedAt))
	}
	return prefixes
}

func candidateRequiredChars(candidates []*Record) int {
	aliases := map[string]string{}
	for index, candidate := range candidates {
		aliases[candidate.EffectID] = fmt.Sprintf("c%d", index+1)
	}
	total := 0
	for _, prefix := range candidatePrefixes(candidates, aliases) {
		total += utf8.RuneCountInString(prefix) + 1
	}
	return total
}

func formatCandidates(candidates []*Record, aliases map[string]string, maxChars int) string {
	if len(candidates) == 0 || maxChars <= 0 {
		return ""
	}
	prefixes := candidatePrefixes(candidates, aliases)
	fixedChars := 0
	for _, prefix := range prefixes {
		fixedChars += utf8.RuneCountInString(prefix) + 1
	}
	textLimit := (maxChars - fixedChars) / len(candidates)
	if textLimit < 0 {
		textLimit = 0
	}
	lines := make([]string, 0, len(candidates))
	for index, prefix := range prefixes {
		text := ""
		if textLimit > 0 {
			text = NormalizeTextForPrompt(candidates[index].Reply.ReplyText, textLimit)
		}
		lines = append(lines, prefix+text)
	}
	return strings.Join(lines, "\n")
}

func followupPrefixes(followups []FollowupSnapshot, aliases map[string]string) []string {
	prefixes := make([]string, 0, len(followups))
	for index := range followups {
		item := &followups[index]
		allowed := make([]string, 0, len(item.CandidateEffectIDs))
		for _, effectID := range item.CandidateEffectIDs {
			if alias, ok := aliases[effectID]; ok {
				allowed = append(allowed, alias)
			}
		}
		metadata := []string{fmt.Sprintf("允许候选（不代表相关）=[%s]", strings.Join(allowed, " "))}
		if item.ReplyTo != "" {
			metadata = append(metadata, "回复消息="+item.ReplyTo)
		}
		if len(item.QuoteTargetIDs) > 0 {
			metadata = append(metadata, "引用消息=["+strings.Join(item.QuoteTargetIDs, " ")+"]")
		}
		confirmed := make([]string, 0)
		for _, association := range item.Associations {
			if association.AttributionType == AttributionExplicitQuote {
				if alias, ok := aliases[association.EffectID]; ok {
					confirmed = append(confirmed, alias)
				}
			}
		}
		if len(confirmed) > 0 {
			metadata = append(metadata, "已确认关联=["+strings.Join(confirmed, " ")+"]")
		}
		prefixes = append(prefixes, fmt.Sprintf("- [%s] message_id=%s %s (%s): ",
			item.Timestamp, item.MessageID, item.DisplayName(), strings.Join(metadata, "，")))
	}
	return prefixes
}

func followupRequiredChars(followups []FollowupSnapshot, aliases map[string]string) int {
	total := 0
	for _, prefix := range followupPrefixes(followups, aliases) {
		total += utf8.RuneCountInString(prefix) + 1
	}
	return total
}

func formatFollowups(followups []FollowupSnapshot, aliases map[string]string, maxChars int) string {
	if len(followups) == 0 || maxChars <= 0 {
		return ""
	}
	prefixes := followupPrefixes(followups, aliases)
	fixedChars := 0
	for _, prefix := range prefixes {
		fixedChars += utf8.RuneCountInString(prefix) + 1
	}
	textLimit := (maxChars - fixedChars) / len(followups)
	if textLimit < 0 {
		textLimit = 0
	}
	lines := make([]string, 0, len(followups))
	for index, prefix := range prefixes {
		text := ""
		if textLimit > 0 {
			source := followups[index].VisibleText
			if source == "" {
				source = followups[index].PlainText
			}
			text = NormalizeTextForPrompt(source, textLimit)
		}
		lines = append(lines, prefix+text)
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// JSON 解析与校验工具
// ---------------------------------------------------------------------------

// loadJSONObject mirrors _loads_json_object：容忍 Markdown 代码块包装。
func loadJSONObject(responseText string) (map[string]any, error) {
	normalized := strings.TrimSpace(responseText)
	if strings.HasPrefix(normalized, "```") {
		normalized = strings.Trim(normalized, "`")
		if strings.HasPrefix(strings.ToLower(normalized), "json") {
			normalized = strings.TrimSpace(normalized[4:])
		}
	}
	decoder := json.NewDecoder(strings.NewReader(normalized))
	decoder.UseNumber()
	var parsed any
	if err := decoder.Decode(&parsed); err != nil {
		return nil, err
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("LLM judge 未返回 JSON 对象")
	}
	return object, nil
}

func requireDict(value any, fieldName string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是对象", fieldName)
	}
	return object, nil
}

func requireEnum(value any, choices []string, fieldName string) (string, error) {
	normalized := trimSpace(toStringValue(value))
	for _, choice := range choices {
		if choice == normalized {
			return normalized, nil
		}
	}
	return "", fmt.Errorf("%s 取值非法：%s", fieldName, normalized)
}

func requireProbability(value any, fieldName string) (float64, error) {
	switch typed := value.(type) {
	case float64:
		if typed < 0 || typed > 1 {
			return 0, fmt.Errorf("%s 必须在 0 到 1 之间", fieldName)
		}
		return typed, nil
	case int:
		if typed < 0 || typed > 1 {
			return 0, fmt.Errorf("%s 必须在 0 到 1 之间", fieldName)
		}
		return float64(typed), nil
	case int64:
		if typed < 0 || typed > 1 {
			return 0, fmt.Errorf("%s 必须在 0 到 1 之间", fieldName)
		}
		return float64(typed), nil
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, fmt.Errorf("%s 必须是数值", fieldName)
		}
		if parsed < 0 || parsed > 1 {
			return 0, fmt.Errorf("%s 必须在 0 到 1 之间", fieldName)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%s 必须是数值", fieldName)
	}
}

// toStringValue mirrors Python str() 在 JSON 标量上的输出。
func toStringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return formatFloat(typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", typed)
	default:
		return fmt.Sprintf("%v", typed)
	}
}
