package tools

import (
	"fmt"
	"strings"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/message"
)

// This file mirrors 参考/MaiBot/src/maisaka/builtin_tool/view_forward_message.py +
// fetch_history.py + tool_search.py (behaviour is authoritative there).

// ForwardViewRuntime 是 view_forward_message 需要的 runtime 面。
type ForwardViewRuntime interface {
	// ChatHistory mirrors runtime._chat_history.
	ChatHistory() []chatcontext.LLMContextMessage
	// FindSourceMessageByID mirrors find_source_message_by_id.
	FindSourceMessageByID(messageID string) *message.SessionMessage
}

// GetViewForwardMessageToolSpec mirrors get_tool_spec（声明照抄）。
func GetViewForwardMessageToolSpec() ToolSpec {
	return ToolSpec{
		Name: "view_forward_message",
		Description: "根据 msg_id 逐层查看合并转发消息。首次调用只展开顶层；遇到嵌套转发时，" +
			"使用返回内容中的 path 再次调用以继续展开。",
		ParametersSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"msg_id": map[string]any{
					"type":        "string",
					"description": "转发消息的 msg_id。",
				},
				"path": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "integer", "minimum": 0},
					"description": "可选的嵌套转发路径。首次查看时省略，后续使用工具返回内容中的 path 逐层展开。",
				},
			},
			"required": []string{"msg_id"},
		},
		ProviderName: "maisaka_builtin",
		ProviderType: "builtin",
		Enabled:      true,
		Stage:        StageAction,
		Visibility:   VisibilityDeferred,
	}
}

// findContextMessageByID mirrors _find_context_message_by_id.
func findContextMessageByID(runtime ForwardViewRuntime, messageID string) *chatcontext.SessionBackedMessage {
	normalizedMessageID := strings.TrimSpace(messageID)
	if normalizedMessageID == "" {
		return nil
	}
	history := runtime.ChatHistory()
	for index := len(history) - 1; index >= 0; index-- {
		if strings.TrimSpace(history[index].MessageID()) != normalizedMessageID {
			continue
		}
		if sessionBacked, ok := history[index].(*chatcontext.SessionBackedMessage); ok {
			return sessionBacked
		}
	}
	return nil
}

// HandleViewForwardMessageTool mirrors handle_tool（view_forward_message 骨架：
// 展开逻辑委托 chatcontext.BuildFullComplexMessageContent）。
func HandleViewForwardMessageTool(runtime ForwardViewRuntime, invocation ToolInvocation) ToolResult {
	toolName := invocation.ToolName
	targetMessageID := strings.TrimSpace(toString(invocation.Arguments["msg_id"]))
	if targetMessageID == "" {
		return BuildFailureResult(toolName, "查看转发消息工具需要提供有效的 `msg_id` 参数。", nil, nil)
	}

	rawPath, _ := invocation.Arguments["path"].([]any)
	path := make([]int, 0, len(rawPath))
	for _, item := range rawPath {
		value, ok := item.(float64)
		if !ok || value < 0 || value != float64(int(value)) {
			return BuildFailureResult(toolName, "查看转发消息工具的 `path` 必须是由非负整数组成的数组。", nil, nil)
		}
		path = append(path, int(value))
	}

	targetContextMessage := findContextMessageByID(runtime, targetMessageID)
	var targetSourceMessage *message.SessionMessage
	if targetContextMessage == nil {
		targetSourceMessage = runtime.FindSourceMessageByID(targetMessageID)
	}
	if targetContextMessage == nil && targetSourceMessage == nil {
		return BuildFailureResult(
			toolName,
			fmt.Sprintf("未找到目标转发消息，msg_id=%s", chatcontext.ToDisplayMessageID(targetMessageID)),
			nil, nil)
	}

	var targetSequence *message.MessageSequence
	if targetContextMessage != nil {
		targetSequence = targetContextMessage.RawMessage
	} else {
		targetSequence = targetSourceMessage.RawMessage
	}
	if !chatcontext.ContainsComplexMessage(targetSequence) {
		return BuildFailureResult(
			toolName,
			fmt.Sprintf("目标消息不是可展开查看的转发消息，msg_id=%s", chatcontext.ToDisplayMessageID(targetMessageID)),
			nil, nil)
	}

	// 展开逻辑：Go 版 chatcontext 只提供按序列展开的入口
	//（BuildFullComplexMessageContent）；原始消息与上下文序列在此等价
	//（_build_full_content_for_context_message 的两个分支统一为一个调用）。
	fullContent := ""
	if targetContextMessage != nil {
		content, err := chatcontext.BuildFullComplexMessageContent(targetContextMessage.RawMessage, path)
		if err != nil {
			return BuildFailureResult(toolName, err.Error(), nil, nil)
		}
		fullContent = content
	} else {
		content, err := chatcontext.BuildFullComplexMessageContent(targetSourceMessage.RawMessage, path)
		if err != nil {
			return BuildFailureResult(toolName, err.Error(), nil, nil)
		}
		fullContent = content
	}

	if fullContent == "" {
		return BuildFailureResult(
			toolName,
			fmt.Sprintf("转发消息内容为空，msg_id=%s", chatcontext.ToDisplayMessageID(targetMessageID)),
			nil, nil)
	}

	return BuildSuccessResult(toolName, fullContent, map[string]any{
		"msg_id":       targetMessageID,
		"message_type": "forward",
		"path":         path,
		"full_content": fullContent,
	}, nil, nil)
}

// ---------------------------------------------------------------------------
// fetch_history（focus 模式工具）
// ---------------------------------------------------------------------------

// FetchHistoryRuntime 是 fetch_history 需要的 runtime 面。
type FetchHistoryRuntime interface {
	// FocusModeActiveForCurrentChat mirrors _is_focus_mode_active_for_current_chat.
	FocusModeActiveForCurrentChat() bool
	// BuildFocusFetchHistoryResult mirrors build_focus_fetch_history_result.
	BuildFocusFetchHistoryResult(num int) (content string, structured map[string]any, postHistory []PostHistoryEntry, err error)
}

// GetFetchHistoryToolSpec mirrors get_tool_spec（声明照抄）。
func GetFetchHistoryToolSpec() ToolSpec {
	return ToolSpec{
		Name: "fetch_history",
		Description: "获取当前聊天流中已经存在、但尚未进入 Maisaka 上下文的消息。" +
			"按从新到旧最多返回 num 条；不能获取其他聊天的信息。",
		ParametersSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"num": map[string]any{
					"type":        "integer",
					"description": "获取消息数量；不填默认 10，最多 50。",
					"minimum":     1,
					"maximum":     50,
				},
			},
		},
		ProviderName: "maisaka_builtin",
		ProviderType: "builtin",
		Enabled:      true,
		Stage:        StageAction,
		Visibility:   VisibilityVisible,
	}
}

// coercePositiveInt mirrors _coerce_positive_int.
func coercePositiveInt(value any, fallback int) int {
	switch v := value.(type) {
	case float64:
		if int(v) >= 1 {
			return int(v)
		}
	case int:
		if v >= 1 {
			return v
		}
	}
	return fallback
}

// HandleFetchHistoryTool mirrors handle_tool（fetch_history）。
func HandleFetchHistoryTool(runtime FetchHistoryRuntime, invocation ToolInvocation) ToolResult {
	toolName := invocation.ToolName
	if !runtime.FocusModeActiveForCurrentChat() {
		return BuildFailureResult(toolName, "focus_mode 未启用，fetch_history 不可用。", nil, nil)
	}
	num := minInt(50, coercePositiveInt(invocation.Arguments["num"], 10))
	content, structured, postHistory, err := runtime.BuildFocusFetchHistoryResult(num)
	if err != nil {
		return BuildFailureResult(toolName, err.Error(), nil, nil)
	}
	return BuildSuccessResult(toolName, content, structured, nil, postHistory)
}

// ---------------------------------------------------------------------------
// tool_search（deferred 工具发现）
// ---------------------------------------------------------------------------

// GetToolSearchToolSpec mirrors get_tool_spec（声明照抄）。
func GetToolSearchToolSpec() ToolSpec {
	return ToolSpec{
		Name:        "tool_search",
		Description: "在 deferred tools 列表中按名称或关键词搜索工具，并将命中的工具加入后续轮次的可用工具列表。",
		ParametersSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "要搜索的工具名、前缀或关键词。",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "最多返回多少个工具。",
					"minimum":     1,
				},
			},
			"required": []string{"query"},
		},
		ProviderName: "maisaka_builtin",
		ProviderType: "builtin",
		Enabled:      true,
		Stage:        StageAction,
		Visibility:   VisibilityVisible,
	}
}

// HandleToolSearchTool mirrors handle_tool（tool_search）：
// runtime.search_deferred_tool_specs / discover_deferred_tools 由注册表承载。
func HandleToolSearchTool(registry *ToolRegistry, invocation ToolInvocation) ToolResult {
	toolName := invocation.ToolName
	rawQuery := toString(invocation.Arguments["query"])
	if strings.TrimSpace(rawQuery) == "" {
		return BuildFailureResult(toolName, "tool_search 需要提供非空的 `query` 字符串参数。", nil, nil)
	}

	limit := 5
	switch v := invocation.Arguments["limit"].(type) {
	case float64:
		limit = int(v)
	case int:
		limit = v
	}
	if limit < 1 {
		limit = 1
	}

	matchedToolSpecs := registry.SearchDeferredToolSpecs(rawQuery, limit)
	matchedToolNames := make([]string, 0, len(matchedToolSpecs))
	for _, spec := range matchedToolSpecs {
		matchedToolNames = append(matchedToolNames, spec.Name)
	}
	newlyDiscovered := registry.DiscoverDeferredTools(matchedToolNames)

	structured := map[string]any{
		"query":                       strings.TrimSpace(rawQuery),
		"matched_tool_names":          matchedToolNames,
		"newly_discovered_tool_names": newlyDiscovered,
	}

	if len(matchedToolNames) == 0 {
		return BuildSuccessResult(
			toolName,
			"未找到匹配的 deferred tools，请尝试更完整的工具名、前缀或其他关键词。",
			structured, nil, nil)
	}

	newlyDiscoveredSet := map[string]bool{}
	for _, name := range newlyDiscovered {
		newlyDiscoveredSet[name] = true
	}
	contentLines := []string{
		fmt.Sprintf("已找到 %d 个 deferred tools，它们会在后续轮次中加入可用工具列表：", len(matchedToolNames)),
	}
	for _, toolNameFound := range matchedToolNames {
		suffix := "（此前已发现）"
		if newlyDiscoveredSet[toolNameFound] {
			suffix = "（本次新发现）"
		}
		contentLines = append(contentLines, "- "+toolNameFound+suffix)
	}
	return BuildSuccessResult(toolName, strings.Join(contentLines, "\n"), structured,
		map[string]any{
			"matched_tool_names":          matchedToolNames,
			"newly_discovered_tool_names": newlyDiscovered,
		}, nil)
}

func minInt(a int, b int) int {
	if a < b {
		return a
	}
	return b
}
