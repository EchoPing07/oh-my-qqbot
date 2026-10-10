package memorix

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Extraction 是一次抽取的结果。
// 字段对齐 Python 内置抽取策略的 JSON 约定：
//   - factual: {"triples": [...], "entities": [...]}（core/strategies/factual.py L79-L100）
//   - narrative: {"events": [...], "relations": [...]}（core/strategies/narrative.py L124-L147）
type Extraction struct {
	Entities  []string
	Relations []ExtractedRelation
	Events    []string
}

// ExtractedRelation 是一条抽取出的三元组。
type ExtractedRelation struct {
	Subject    string
	Predicate  string
	Object     string
	Confidence float64
}

// extractionPrompt 对齐 Python factual/narrative 抽取 prompt 的中文等价形态
// （口径：只输出 JSON；triples/relations/entities/events 键名照抄）。
// 出处：core/strategies/factual.py L79-L100 与 core/strategies/narrative.py L124-L147。
const extractionPrompt = `你是知识抽取引擎。从下面的文本中抽取实体、关系（三元组）和事件。
要求：
- 实体名保留原文，不要翻译、不要改写。
- 关系三元组用动词或短语作 predicate（如「是」「喜欢」「在」「属于」）。
- 只输出 JSON，不要额外解释。
- JSON 键名必须保持为：entities, relations, events, subject, predicate, object。

返回格式：
{
  "entities": ["实体1", "实体2"],
  "relations": [
    {"subject": "实体A", "predicate": "关系", "object": "实体B"}
  ],
  "events": ["事件描述"]
}

文本：
%s`

// ExtractFromText 调用 LLM 抽取实体/关系；LLM 不可用或返回不可解析时返回空结果与 nil 错误
// （写入路径允许只在规则层面登记实体）。
func (k *Kernel) ExtractFromText(ctx context.Context, text string) (*Extraction, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return &Extraction{}, nil
	}
	if k.chat == nil || !k.options.Extraction.Enabled {
		return k.ruleBasedExtraction(text), nil
	}
	limit := k.options.Extraction.MaxInputRunes
	if limit > 0 && utf8.RuneCountInString(text) > limit {
		text = string([]rune(text)[:limit])
	}
	temp := k.options.Extraction.Temperature
	req := ChatRequest{
		Model: k.options.Embedding.ModelName,
		Messages: []ChatMessage{
			{Role: "user", Content: fmt.Sprintf(extractionPrompt, text)},
		},
		MaxTokens:   k.options.Extraction.MaxTokens,
		Temperature: &temp,
	}
	resp, err := k.chat.Chat(ctx, k.options.Extraction.Task, req)
	if err != nil || resp == nil {
		return k.ruleBasedExtraction(text), nil
	}
	payload, err := ParseLooseJSON(resp.Content)
	if err != nil {
		return k.ruleBasedExtraction(text), nil
	}
	return normalizeExtraction(payload), nil
}

// ruleBasedExtraction 在无 LLM 时的降级抽取：实体来自引号/书名号/话题标签/人物名模式。
func (k *Kernel) ruleBasedExtraction(text string) *Extraction {
	return &Extraction{Entities: RuleBasedExtractEntities(text)}
}

var (
	quotedEntityRE = regexp.MustCompile(`[“"'「『《【]([^”"'」』》】
]{1,20})[”"'」』》】]`)
	topicEntityRE = regexp.MustCompile(`#([^\s#]{1,20})#`)
	asciiWordRE   = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9_\-]{2,19}\b`)
)

// RuleBasedExtractEntities 用简单模式抽取候选实体（去重、按出现顺序）。
func RuleBasedExtractEntities(text string) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || utf8.RuneCountInString(value) > 20 {
			return
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	for _, match := range quotedEntityRE.FindAllStringSubmatch(text, -1) {
		add(match[1])
	}
	for _, match := range topicEntityRE.FindAllStringSubmatch(text, -1) {
		add(match[1])
	}
	for _, match := range asciiWordRE.FindAllString(text, -1) {
		add(match)
	}
	return out
}

// normalizeExtraction 把宽松 JSON 结果规整为 Extraction。
func normalizeExtraction(payload map[string]any) *Extraction {
	out := &Extraction{}
	out.Entities = toStringSlice(payload["entities"])
	out.Events = toStringSlice(payload["events"])
	for _, key := range []string{"relations", "triples", "relation"} {
		for _, item := range toMapSlice(payload[key]) {
			relation := ExtractedRelation{
				Subject:    strings.TrimSpace(stringValue(item["subject"])),
				Predicate:  strings.TrimSpace(stringValue(item["predicate"])),
				Object:     strings.TrimSpace(stringValue(item["object"])),
				Confidence: floatValue(item["confidence"], 1.0),
			}
			if relation.Subject == "" || relation.Predicate == "" || relation.Object == "" {
				continue
			}
			out.Relations = append(out.Relations, relation)
		}
	}
	// 关系两端也视为实体。
	for _, relation := range out.Relations {
		out.Entities = append(out.Entities, relation.Subject, relation.Object)
	}
	out.Entities = dedupeStrings(out.Entities)
	return out
}

// ParseLooseJSON 宽松解析 LLM 返回的 JSON（对齐 Python json_repair 的常用修复面）：
//   - 剥离 markdown 代码围栏与前后说明文字
//   - 截取首个 { ... } / [ ... ] 平衡块
//   - 去尾随逗号、补全未闭合括号
//   - 单引号转双引号、裸键加引号
//   - 处理 Python 字面量 True/False/None 与中文全角引号
//
// 对齐出处：core/utils/runtime_payloads.py L27-L36（safe_json_loads 用 json_repair）
// 与 core/utils/person_profile_service.py L963-L967。
func ParseLooseJSON(raw string) (map[string]any, error) {
	text := stripCodeFence(raw)
	candidates := []string{text}
	if block, ok := balancedBlock(text); ok && block != text {
		candidates = append(candidates, block)
	}
	for _, candidate := range candidates {
		if payload, err := decodeJSONObject(candidate); err == nil {
			return payload, nil
		}
	}
	// 最后一轮：应用全部修复规则后再试。
	for _, candidate := range candidates {
		repaired := repairJSON(candidate)
		if payload, err := decodeJSONObject(repaired); err == nil {
			return payload, nil
		}
	}
	return nil, fmt.Errorf("memorix: JSON 解析失败")
}

// ParseLooseJSONAny 宽松解析任意 JSON（数组或对象）。
func ParseLooseJSONAny(raw string) (any, error) {
	text := stripCodeFence(raw)
	candidates := []string{text}
	if block, ok := balancedBlock(text); ok && block != text {
		candidates = append(candidates, block)
	}
	for _, candidate := range candidates {
		var value any
		if err := json.Unmarshal([]byte(candidate), &value); err == nil {
			return value, nil
		}
	}
	for _, candidate := range candidates {
		var value any
		if err := json.Unmarshal([]byte(repairJSON(candidate)), &value); err == nil {
			return value, nil
		}
	}
	return nil, fmt.Errorf("memorix: JSON 解析失败")
}

func decodeJSONObject(text string) (map[string]any, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("empty")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, fmt.Errorf("null object")
	}
	return payload, nil
}

// stripCodeFence 剥离 markdown 代码围栏（```json ... ```）。
func stripCodeFence(raw string) string {
	text := strings.TrimSpace(raw)
	if !strings.Contains(text, "```") {
		return text
	}
	lines := strings.Split(text, "\n")
	var kept []string
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			if !inFence {
				// 围栏结束，后面的解释文字不再纳入。
				break
			}
			continue
		}
		if inFence {
			kept = append(kept, line)
		}
	}
	if len(kept) == 0 {
		return text
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// balancedBlock 截取首个花括号/方括号平衡块，跳过字符串字面量内的括号。
func balancedBlock(text string) (string, bool) {
	start := -1
	var open, close rune
	for index, r := range text {
		if r == '{' || r == '[' {
			start = index
			open = r
			if r == '{' {
				close = '}'
			} else {
				close = ']'
			}
			break
		}
	}
	if start < 0 {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for offset, r := range text[start:] {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch r {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch r {
		case '"':
			inString = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				end := start + offset + utf8.RuneLen(r)
				return strings.TrimSpace(text[start:end]), true
			}
		}
	}
	// 未闭合：返回从起点到结尾（后续 repairJSON 会补括号）。
	return strings.TrimSpace(text[start:]), true
}

var (
	// 单独的 true/false/null 之外，Python 字面量需要映射。
	pythonTrueRE    = regexp.MustCompile(`\bTrue\b`)
	pythonFalseRE   = regexp.MustCompile(`\bFalse\b`)
	pythonNoneRE    = regexp.MustCompile(`\bNone\b`)
	trailingCommaRE = regexp.MustCompile(`,\s*([}\]])`)
)

// repairJSON 应用常见修复规则（不依赖第三方库，语义覆盖 json_repair 的常用面）。
func repairJSON(text string) string {
	repaired := strings.TrimSpace(text)
	// 全角引号与逗号/冒号折叠。
	repaired = strings.NewReplacer(
		"“", `"`, "”", `"`, "‘", `'`, "’", `'`,
		"，", ",", "：", ":",
	).Replace(repaired)
	// Python 字面量。
	repaired = pythonTrueRE.ReplaceAllString(repaired, "true")
	repaired = pythonFalseRE.ReplaceAllString(repaired, "false")
	repaired = pythonNoneRE.ReplaceAllString(repaired, "null")
	// 去掉尾随逗号。
	for i := 0; i < 3; i++ {
		next := trailingCommaRE.ReplaceAllString(repaired, "$1")
		if next == repaired {
			break
		}
		repaired = next
	}
	// 单引号字符串转双引号（仅在无任何双引号时，粗暴但安全）。
	if !strings.Contains(repaired, `"`) && strings.Contains(repaired, "'") {
		repaired = strings.ReplaceAll(repaired, "'", `"`)
	}
	// 裸键加引号。
	repaired = addQuotesToBareKeys(repaired)
	// 补全未闭合的括号。
	repaired = closeBrackets(repaired)
	return repaired
}

// addQuotesToBareKeys 给未加引号的对象键补引号（跳过已在引号内的键）。
func addQuotesToBareKeys(text string) string {
	var b strings.Builder
	b.Grow(len(text) + 16)
	inString := false
	escaped := false
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if inString {
			b.WriteRune(r)
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
			} else if r == '"' {
				inString = false
			}
			continue
		}
		if r == '"' {
			inString = true
			b.WriteRune(r)
			continue
		}
		if r == '{' || r == ',' {
			b.WriteRune(r)
			// 跳过空白，判断后续是否是裸键。
			j := i + 1
			for j < len(runes) && (runes[j] == ' ' || runes[j] == '\n' || runes[j] == '\t' || runes[j] == '\r') {
				b.WriteRune(runes[j])
				j++
			}
			start := j
			for j < len(runes) && isBareKeyRune(runes[j]) {
				j++
			}
			key := string(runes[start:j])
			// 之后允许空白再判断是否紧跟 ':'
			k := j
			for k < len(runes) && (runes[k] == ' ' || runes[k] == '\n' || runes[k] == '\t' || runes[k] == '\r') {
				k++
			}
			if key != "" && k < len(runes) && runes[k] == ':' {
				b.WriteString(`"` + key + `"`)
				for m := j; m < k; m++ {
					b.WriteRune(runes[m])
				}
				b.WriteRune(':')
				i = k
				continue
			}
			i = j - 1
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isBareKeyRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '_':
		return true
	case r >= 0x4E00 && r <= 0x9FFF:
		return true
	}
	return false
}

// closeBrackets 补全未闭合的字符串/花括号/方括号。
func closeBrackets(text string) string {
	inString := false
	escaped := false
	var stack []rune
	for _, r := range text {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
			} else if r == '"' {
				inString = false
			}
			continue
		}
		switch r {
		case '"':
			inString = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) > 0 && stack[len(stack)-1] == r {
				stack = stack[:len(stack)-1]
			}
		}
	}
	var b strings.Builder
	b.WriteString(text)
	if inString {
		b.WriteString(`"`)
	}
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteRune(stack[i])
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// 类型转换工具
// ---------------------------------------------------------------------------

func toStringSlice(value any) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s := strings.TrimSpace(stringValue(item)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{strings.TrimSpace(v)}
	default:
		return nil
	}
}

func toMapSlice(value any) []map[string]any {
	switch v := value.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{v}
	default:
		return nil
	}
}

func stringValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func floatValue(value any, fallback float64) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return parsed
		}
		return fallback
	default:
		return fallback
	}
}

// ---------------------------------------------------------------------------
// 关系去重（compute_relation_hash）
// ---------------------------------------------------------------------------

// RelationHashKey 计算关系去重键（对齐 compute_relation_hash：sha256("s|p|o")）。
// 出处：core/storage/metadata_store.py L595-L609 与 core/utils/hash.py L86-L98。
func RelationHashKey(subject, predicate, object string) string {
	s := strings.ToLower(strings.TrimSpace(subject))
	p := strings.ToLower(strings.TrimSpace(predicate))
	o := strings.ToLower(strings.TrimSpace(object))
	return sha256Hex(s + "|" + p + "|" + o)
}

// DedupeRelations 按 relation hash 去重（保留首次出现，置信度取最大）。
func DedupeRelations(relations []ExtractedRelation) []ExtractedRelation {
	order := make([]string, 0, len(relations))
	byKey := map[string]ExtractedRelation{}
	for _, relation := range relations {
		if strings.TrimSpace(relation.Subject) == "" ||
			strings.TrimSpace(relation.Predicate) == "" ||
			strings.TrimSpace(relation.Object) == "" {
			continue
		}
		key := RelationHashKey(relation.Subject, relation.Predicate, relation.Object)
		existing, ok := byKey[key]
		if !ok {
			order = append(order, key)
			byKey[key] = relation
			continue
		}
		if relation.Confidence > existing.Confidence {
			existing.Confidence = relation.Confidence
			byKey[key] = existing
		}
	}
	out := make([]ExtractedRelation, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	return out
}

// SortStringsDeterministic 返回排序去重后的字符串切片（便于测试与稳定输出）。
func SortStringsDeterministic(values []string) []string {
	out := dedupeStrings(values)
	sort.Strings(out)
	return out
}
