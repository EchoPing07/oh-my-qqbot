package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// This file mirrors 参考/MaiBot/src/maisaka/builtin_tool/query_memory.py
// (behaviour is authoritative there): query_memory 内置工具。
// 五种检索模式 search/time/hybrid/episode/aggregate 全部保留，
// 直接映射 internal.MemoryKernel（contracts 已有）。

// allowedQueryModes mirrors _ALLOWED_QUERY_MODES.
var allowedQueryModes = map[string]bool{
	"search": true, "time": true, "hybrid": true, "episode": true, "aggregate": true,
}

// isoQueryTimePattern mirrors _ISO_QUERY_TIME_RE.
var isoQueryTimePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(?: \d{2}:\d{2})?$`)

// ReplyerMemoryReferenceMarker mirrors REPLYER_MEMORY_REFERENCE_MARKER.
const ReplyerMemoryReferenceMarker = "【长期记忆检索结果-内部参考】"

// MemorySearchResult mirrors MemorySearchResult（memory_service 门面）。
type MemorySearchResult struct {
	Summary  string            `json:"summary"`
	Hits     []MemorySearchHit `json:"hits"`
	Filtered bool              `json:"filtered"`
	Success  bool              `json:"success"`
	Error    string            `json:"error"`
}

// MemorySearchHit mirrors MemoryHit（to_dict 形态）。
type MemorySearchHit struct {
	Content   string         `json:"content"`
	Score     float64        `json:"score"`
	Type      string         `json:"type"`
	Source    string         `json:"source"`
	Hash      string         `json:"hash"`
	Metadata  map[string]any `json:"metadata"`
	EpisodeID string         `json:"episode_id"`
	Title     string         `json:"title"`
}

// ToText mirrors MemorySearchResult.to_text.
func (r MemorySearchResult) ToText(limit int) string {
	if len(r.Hits) == 0 {
		return ""
	}
	if limit < 1 {
		limit = 1
	}
	lines := make([]string, 0, limit)
	for index, hit := range r.Hits {
		if index >= limit {
			break
		}
		content := strings.ReplaceAll(strings.TrimSpace(hit.Content), "\n", " ")
		lines = append(lines, fmt.Sprintf("%d. %s", index+1, content))
	}
	return strings.Join(lines, "\n")
}

// MemorySearcher 是记忆检索的注入边界（映射 internal.MemoryKernel.SearchMemory）。
type MemorySearcher interface {
	// SearchSearch runs one memory search (mode: search/time/hybrid/episode/aggregate)。
	SearchMemory(ctx contextT, mode string, query string, chatID string, topK int) (MemorySearchResult, error)
}

// GetQueryMemoryToolSpec mirrors get_tool_spec（声明照抄；enum 为排序后的模式集合）。
func GetQueryMemoryToolSpec(enabled bool) ToolSpec {
	return ToolSpec{
		Name:        "query_memory",
		Description: "检索长期记忆。",
		ParametersSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "关键词或问题；非纯时间检索必填。",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "返回条数。",
				},
				"mode": map[string]any{
					"type": "description_placeholder",
					"description": "检索路由：search按语义检索事实、偏好等记忆；time按时间范围检索；" +
						"episode检索经历；aggregate综合检索；hybrid同时使用语义与时间范围。" +
						"time和hybrid至少提供time_start或time_end；没有时间条件时使用search。",
					"enum":    []string{"aggregate", "episode", "hybrid", "search", "time"},
					"default": "search",
				},
				"person_name": map[string]any{
					"type":        "string",
					"description": "人物名；用于定向过滤。",
				},
				"time_start": map[string]any{
					"type":        "string",
					"description": "起始时间，仅接受YYYY/MM/DD或YYYY/MM/DD HH:mm。",
				},
				"time_end": map[string]any{
					"type":        "string",
					"description": "结束时间，仅接受YYYY/MM/DD或YYYY/MM/DD HH:mm。",
				},
				"respect_filter": map[string]any{
					"type":        "boolean",
					"description": "是否遵守记忆过滤规则；默认true，模糊来源或整体印象可false。",
					"default":     true,
				},
			},
		},
		ProviderName: "maisaka_builtin",
		ProviderType: "builtin",
		Enabled:      enabled,
		Stage:        StageAction,
		Visibility:   VisibilityVisible,
	}
}

// normalizeOptionalTime mirrors _normalize_optional_time.
func normalizeOptionalTime(raw any) any {
	switch v := raw.(type) {
	case nil:
		return nil
	case string:
		timeText := strings.TrimSpace(v)
		if timeText == "" {
			return nil
		}
		if isoQueryTimePattern.MatchString(timeText) {
			return strings.Replace(timeText, "-", "/", 2)
		}
		return timeText
	case float64:
		return v
	case int:
		return float64(v)
	default:
		timeText := strings.TrimSpace(fmt.Sprintf("%v", raw))
		if timeText == "" || timeText == "<nil>" {
			return nil
		}
		return timeText
	}
}

// buildSuccessContent mirrors _build_success_content.
func buildSuccessContent(result MemorySearchResult, limit int) string {
	summary := strings.TrimSpace(result.Summary)
	snippet := result.ToText(limit)
	if len(result.Hits) > 0 {
		if snippet != "" {
			return snippet
		}
		if summary != "" {
			return summary
		}
		return "已找到匹配的长期记忆。"
	}
	if result.Filtered {
		return "当前请求被聊天过滤策略跳过，未执行长期记忆检索。"
	}
	return "未找到匹配的长期记忆。"
}

// buildReplyerMemoryReference mirrors _build_replyer_memory_reference.
func buildReplyerMemoryReference(structured map[string]any) string {
	rawHits, ok := structured["hits"].([]MemorySearchHit)
	if !ok {
		return ""
	}
	lines := []string{ReplyerMemoryReferenceMarker}
	if query := toString(structured["query"]); query != "" {
		lines = append(lines, "查询："+query)
	}
	mode := toString(structured["mode"])
	effectiveMode := toString(structured["effective_mode"])
	if mode != "" {
		modeText := mode
		if effectiveMode != "" && effectiveMode != mode {
			modeText = mode + " -> " + effectiveMode
		}
		lines = append(lines, "模式："+modeText)
	}
	hitLines := make([]string, 0)
	for index, hit := range rawHits {
		content := strings.Join(strings.Fields(hit.Content), " ")
		if content == "" {
			continue
		}
		hitType := strings.TrimSpace(hit.Type)
		title := strings.TrimSpace(hit.Title)
		label := ""
		if title != "" || hitType != "" {
			parts := make([]string, 0, 2)
			if title != "" {
				parts = append(parts, title)
			}
			if hitType != "" {
				parts = append(parts, hitType)
			}
			label = "（" + strings.Join(parts, " / ") + "）"
		}
		hitLines = append(hitLines, fmt.Sprintf("%d. %s%s", index+1, label, content))
	}
	if len(hitLines) == 0 {
		return ""
	}
	lines = append(lines, fmt.Sprintf("命中：%d 条", len(hitLines)))
	lines = append(lines, hitLines...)
	return strings.Join(lines, "\n")
}

// QueryMemoryDeps 聚合 query_memory 工具依赖。
type QueryMemoryDeps struct {
	Searcher MemorySearcher
	// SessionID / chat 上下文。
	SessionID string
	// DefaultLimit mirrors a_memorix.integration.memory_query_default_limit。
	DefaultLimit int
}

// HandleQueryMemoryTool mirrors handle_tool（query_memory）。
func HandleQueryMemoryTool(ctx contextT, deps *QueryMemoryDeps, invocation ToolInvocation) ToolResult {
	toolName := invocation.ToolName
	cleanQuery := strings.TrimSpace(toString(invocation.Arguments["query"]))
	mode := strings.ToLower(strings.TrimSpace(toString(invocation.Arguments["mode"])))
	if mode == "" {
		mode = "search"
	}
	if !allowedQueryModes[mode] {
		return BuildFailureResult(
			toolName,
			fmt.Sprintf("不支持的检索模式：%s。可选值：search/time/hybrid/episode/aggregate。", mode),
			nil, nil)
	}

	defaultLimit := deps.DefaultLimit
	if defaultLimit < 1 {
		defaultLimit = 1
	}
	limit := defaultLimit
	switch v := invocation.Arguments["limit"].(type) {
	case float64:
		limit = int(v)
	case int:
		limit = v
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 20 {
		limit = 20
	}

	timeStart := normalizeOptionalTime(invocation.Arguments["time_start"])
	timeEnd := normalizeOptionalTime(invocation.Arguments["time_end"])
	if cleanQuery == "" && timeStart == nil && timeEnd == nil {
		return BuildFailureResult(
			toolName,
			"query_memory 需要提供 query，或至少提供 time_start/time_end 中的一个。",
			nil, nil)
	}

	respectFilter := toBool(invocation.Arguments["respect_filter"], true)
	personName := strings.TrimSpace(toString(invocation.Arguments["person_name"]))

	result, err := deps.Searcher.SearchMemory(ctx, mode, cleanQuery, deps.SessionID, limit)
	if err != nil {
		return BuildFailureResult(toolName, fmt.Sprintf("长期记忆检索失败：%v", err), nil, nil)
	}
	structured := map[string]any{
		"success":        result.Success,
		"error":          result.Error,
		"summary":        result.Summary,
		"hits":           result.Hits,
		"filtered":       result.Filtered,
		"query":          cleanQuery,
		"mode":           mode,
		"effective_mode": mode,
		"limit":          limit,
		"chat_id":        deps.SessionID,
		"person_name":    personName,
		"respect_filter": respectFilter,
		"time_start":     timeStart,
		"time_end":       timeEnd,
	}

	if !result.Success {
		errorMessage := strings.TrimSpace(result.Error)
		if errorMessage == "" {
			errorMessage = "长期记忆检索失败。"
		}
		return BuildFailureResult(toolName, errorMessage, structured, nil)
	}

	content := buildSuccessContent(result, limit)
	replyerMemoryReference := buildReplyerMemoryReference(structured)
	metadata := map[string]any{}
	if replyerMemoryReference != "" {
		metadata["replyer_memory_reference"] = replyerMemoryReference
	}
	return BuildSuccessResult(toolName, content, structured, metadata, nil)
}
