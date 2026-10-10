package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// This file mirrors the ToolSpec/ToolInvocation/ToolExecutionResult surfaces of
// 参考/MaiBot/src/core/tooling.py plus the builtin-tool catalogue of
// 参考/MaiBot/src/maisaka/builtin_tool/__init__.py (behaviour is authoritative
// there). 工具 schema 以 OpenAI function 形态暴露。

// ToolVisibility mirrors BuiltinToolVisibility.
type ToolVisibility string

// Visibility values.
const (
	VisibilityVisible  ToolVisibility = "visible"
	VisibilityDeferred ToolVisibility = "deferred"
	VisibilityHidden   ToolVisibility = "hidden"
)

// ToolStage mirrors BuiltinToolStage.
type ToolStage string

// Stage values.
const (
	StageAction ToolStage = "action"
	StageBoth   ToolStage = "both"
)

// ToolSpec mirrors core.tooling.ToolSpec: 统一工具声明。
type ToolSpec struct {
	// Name 是工具名（OpenAI function.name）。
	Name string
	// Description 是供 LLM 使用的描述（照抄 Python 文案）。
	Description string
	// ParametersSchema 是 OpenAI function schema 的 parameters 对象。
	ParametersSchema map[string]any
	// ProviderName / ProviderType 标记来源（内置工具为 maisaka_builtin/builtin）。
	ProviderName string
	ProviderType string
	// Enabled 对齐 ToolSpec.enabled。
	Enabled bool
	// Stage / Visibility 对齐 BuiltinToolEntry 元数据。
	Stage      ToolStage
	Visibility ToolVisibility
}

// FunctionDefinition 是 OpenAI function schema 形态的工具定义（对齐
// ToolDefinitionInput：{type:"function", function:{name,description,parameters}}）。
type FunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToLLMDefinition mirrors ToolSpec.to_llm_definition。
func (s ToolSpec) ToLLMDefinition() FunctionDefinition {
	return FunctionDefinition{
		Name:        s.Name,
		Description: strings.TrimSpace(s.Description),
		Parameters:  s.ParametersSchema,
	}
}

// MarshalJSON renders the OpenAI function-schema wire shape.
func (s ToolSpec) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        s.Name,
			"description": strings.TrimSpace(s.Description),
			"parameters":  s.ParametersSchema,
		},
	})
}

// ToolInvocation mirrors core.tooling.ToolInvocation: 统一工具调用请求。
type ToolInvocation struct {
	ToolName      string
	Arguments     map[string]any
	CallID        string
	SessionID     string
	Reasoning     string
	LogicalTurnID string
	Metadata      map[string]any
}

// ToolResult mirrors core.tooling.ToolExecutionResult: 统一工具执行结果。
type ToolResult struct {
	ToolName          string
	Success           bool
	Content           string
	ErrorMessage      string
	StructuredContent map[string]any
	// PostHistory 是工具要求追加到历史的 LLM ContextItems（如表情候选拼图）。
	PostHistory []PostHistoryEntry
	Metadata    map[string]any
}

// PostHistoryEntry carries one ContextItem for post-history appending. The
// concrete llm.ContextItem is wrapped to avoid importing internal/llm here —
// the planner assembles the final items.
type PostHistoryEntry struct {
	// ItemID 是去重 ID。
	ItemID string
	// VisibleText 是监控可见文本。
	VisibleText string
	// PayloadJSON 是 llm.ContextItem 的 JSON 序列化（由装配层反序列化或构造）。
	PayloadJSON []byte
}

// HistoryContent mirrors ToolExecutionResult.get_history_content:
// 优先文本内容，其次错误信息。
func (r ToolResult) HistoryContent() string {
	if strings.TrimSpace(r.Content) != "" {
		return strings.TrimSpace(r.Content)
	}
	if strings.TrimSpace(r.ErrorMessage) != "" {
		return strings.TrimSpace(r.ErrorMessage)
	}
	if r.StructuredContent != nil {
		encoded, err := json.Marshal(r.StructuredContent)
		if err == nil {
			return string(encoded)
		}
	}
	return ""
}

// Handler is the builtin-tool raw handler signature
// (mirrors BuiltinToolRawHandler with the runtime context injected).
type Handler func(ctx context.Context, toolCtx *RuntimeContext, invocation ToolInvocation) ToolResult

// RuntimeContext mirrors BuiltinToolRuntimeContext: 为拆分后的内置工具提供
// 统一运行时能力。All collaborators are injected (不 import qq/config)。
type RuntimeContext struct {
	// SessionID 是 runtime.session_id。
	SessionID string
	// IsGroupSession mirrors chat_stream.is_group_session.
	IsGroupSession bool
	// LogPrefix mirrors runtime.log_prefix.
	LogPrefix string
	// BotName mirrors global_config.bot.nickname（回执文案使用）。
	BotName string
	// Feature flags (照抄 config 开关读取结果，由装配层填充)。
	EnableReplyAt           bool
	EnableReplyQuote        bool
	UseNewEmojiSendLogic    bool
	UseVectorExpression     bool
	MemoryQueryEnabled      bool
	MemoryQueryDefaultLimit int
	FocusModeActive         bool
	// FindSourceMessage mirrors runtime.find_source_message_by_id.
	FindSourceMessage func(messageID string) any
	// HistoryEntries gives the planner-visible history (guided-reply dedup etc.);
	// the concrete type is interface{} to keep this package independent — the
	// reply tool narrows via HistoryAccessor instead.
	// IsBotSelf mirrors utils.is_bot_self.
	IsBotSelf func(platform string, userID string) bool
}

// BuildSuccessResult mirrors BuiltinToolRuntimeContext.build_success_result.
func BuildSuccessResult(toolName string, content string, structured map[string]any, metadata map[string]any, postHistory []PostHistoryEntry) ToolResult {
	result := ToolResult{
		ToolName:    toolName,
		Success:     true,
		Content:     content,
		PostHistory: postHistory,
		Metadata:    map[string]any{},
	}
	if structured != nil {
		result.StructuredContent = structured
	}
	for key, value := range metadata {
		result.Metadata[key] = value
	}
	return result
}

// BuildFailureResult mirrors BuiltinToolRuntimeContext.build_failure_result.
func BuildFailureResult(toolName string, errorMessage string, structured map[string]any, metadata map[string]any) ToolResult {
	result := ToolResult{
		ToolName:     toolName,
		Success:      false,
		ErrorMessage: errorMessage,
		Metadata:     map[string]any{},
	}
	if structured != nil {
		result.StructuredContent = structured
	}
	for key, value := range metadata {
		result.Metadata[key] = value
	}
	return result
}

// BuiltinToolEntry mirrors BuiltinToolEntry: 内置工具目录项。
type BuiltinToolEntry struct {
	Name       string
	GetSpec    func() ToolSpec
	HandleTool Handler
	Stage      ToolStage
	Visibility ToolVisibility
	// EnabledByConfig mirrors _is_builtin_tool_enabled_by_config.
	EnabledByConfig func() bool
}

// ToolRegistry mirrors the builtin part of the unified tool registry: 工具目录 +
// 发现 + 激活（deferred tools）。
type ToolRegistry struct {
	entries        []BuiltinToolEntry
	handlers       map[string]Handler
	deferredByName map[string]ToolSpec
	discovered     map[string]bool
}

// NewToolRegistry builds a registry from the builtin entries.
func NewToolRegistry(entries []BuiltinToolEntry) *ToolRegistry {
	registry := &ToolRegistry{
		entries:        append([]BuiltinToolEntry{}, entries...),
		handlers:       map[string]Handler{},
		deferredByName: map[string]ToolSpec{},
		discovered:     map[string]bool{},
	}
	for _, entry := range registry.entries {
		registry.handlers[entry.Name] = entry.HandleTool
		if entry.Visibility == VisibilityDeferred {
			registry.deferredByName[entry.Name] = entry.GetSpec()
		}
	}
	// tool_search 的 handler 需要引用注册表自身（搜索/发现 deferred tools），
	// 目录闭包在注册表构建完成前拿不到实例；这里在构建时回填，保证
	// Invoke("tool_search") 永远可用（此前 handler 为 nil，调用即 nil 函数 panic）。
	if handler, ok := registry.handlers["tool_search"]; ok && handler == nil {
		registry.handlers["tool_search"] = func(_ context.Context, _ *RuntimeContext, invocation ToolInvocation) ToolResult {
			return HandleToolSearchTool(registry, invocation)
		}
	}
	return registry
}

// VisibleToolSpecs returns the visible + action-stage tool specs
// (mirrors get_builtin_tools).
func (r *ToolRegistry) VisibleToolSpecs() []ToolSpec {
	specs := make([]ToolSpec, 0)
	for _, entry := range r.entries {
		if entry.Stage != StageAction && entry.Stage != StageBoth {
			continue
		}
		if entry.Visibility != VisibilityVisible {
			continue
		}
		if entry.EnabledByConfig != nil && !entry.EnabledByConfig() {
			continue
		}
		specs = append(specs, entry.GetSpec())
	}
	return specs
}

// DeferredToolSpecs 返回未发现的 deferred 工具声明（build_deferred_tools_reminder 用）。
func (r *ToolRegistry) DeferredToolSpecs() []ToolSpec {
	specs := make([]ToolSpec, 0, len(r.deferredByName))
	for name := range r.deferredByName {
		specs = append(specs, r.deferredByName[name])
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
}

// DiscoveredToolSpecs mirrors get_discovered_deferred_tool_specs.
func (r *ToolRegistry) DiscoveredToolSpecs() []ToolSpec {
	specs := make([]ToolSpec, 0)
	for name, spec := range r.deferredByName {
		if r.discovered[name] {
			specs = append(specs, spec)
		}
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
}

// BuildDeferredToolsReminder mirrors runtime.build_deferred_tools_reminder
// （文案照抄）。
func (r *ToolRegistry) BuildDeferredToolsReminder() string {
	var undiscovered []ToolSpec
	for name, spec := range r.deferredByName {
		if !r.discovered[name] {
			undiscovered = append(undiscovered, spec)
		}
	}
	if len(undiscovered) == 0 {
		return ""
	}
	sort.Slice(undiscovered, func(i, j int) bool { return undiscovered[i].Name < undiscovered[j].Name })

	toolLines := make([]string, 0, len(undiscovered))
	for index, spec := range undiscovered {
		toolName := strings.TrimSpace(spec.Name)
		toolDescription := strings.TrimSpace(spec.Description)
		if toolDescription != "" {
			toolLines = append(toolLines, fmt.Sprintf("%d. %s: %s", index+1, toolName, toolDescription))
		} else {
			toolLines = append(toolLines, fmt.Sprintf("%d. %s", index+1, toolName))
		}
	}
	reminderLines := append([]string{
		"<system-reminder>",
		"以下工具当前未直接暴露给你，但可以通过 tool_search 工具发现并在后续轮次中使用：",
	}, toolLines...)
	reminderLines = append(reminderLines,
		"",
		"如需其中某个工具，请先调用 tool_search。tool_search 只负责发现工具，不直接执行。",
		"</system-reminder>",
	)
	return strings.Join(reminderLines, "\n")
}

// SearchDeferredToolSpecs mirrors runtime.search_deferred_tool_specs:
// 按名称或简要描述搜索 deferred tools（打分规则照抄）。
func (r *ToolRegistry) SearchDeferredToolSpecs(query string, limit int) []ToolSpec {
	normalizedQuery := strings.Join(strings.Fields(strings.ToLower(query)), "")
	if normalizedQuery == "" {
		return nil
	}
	if limit < 1 {
		limit = 1
	}

	queryTerms := strings.Fields(
		strings.ReplaceAll(strings.ReplaceAll(normalizedQuery, "_", " "), "-", " "))

	type scoredMatch struct {
		score float64
		name  string
		spec  ToolSpec
	}
	matches := make([]scoredMatch, 0)
	for toolName, toolSpec := range r.deferredByName {
		lowerName := strings.ToLower(toolName)
		lowerDescription := strings.ToLower(toolSpec.Description)
		score := 0

		if normalizedQuery == lowerName {
			score += 1000
		}
		if strings.HasPrefix(lowerName, normalizedQuery) {
			score += 300
		}
		if strings.Contains(lowerName, normalizedQuery) {
			score += 200
		}
		if strings.Contains(lowerDescription, normalizedQuery) {
			score += 100
		}
		for _, term := range queryTerms {
			if strings.Contains(lowerName, term) {
				score += 25
			}
			if strings.Contains(lowerDescription, term) {
				score += 10
			}
		}
		if score <= 0 {
			continue
		}
		matches = append(matches, scoredMatch{score: float64(score), name: toolName, spec: toolSpec})
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].name < matches[j].name
	})
	result := make([]ToolSpec, 0, limit)
	for index, match := range matches {
		if index >= limit {
			break
		}
		result = append(result, match.spec)
	}
	return result
}

// DiscoverDeferredTools mirrors runtime.discover_deferred_tools:
// 将指定 deferred tools 标记为已发现，并返回本次新发现的工具名。
func (r *ToolRegistry) DiscoverDeferredTools(toolNames []string) []string {
	newlyDiscovered := make([]string, 0)
	for _, rawToolName := range toolNames {
		normalized := strings.TrimSpace(rawToolName)
		if normalized == "" {
			continue
		}
		if _, ok := r.deferredByName[normalized]; !ok {
			continue
		}
		if r.discovered[normalized] {
			continue
		}
		r.discovered[normalized] = true
		newlyDiscovered = append(newlyDiscovered, normalized)
	}
	return newlyDiscovered
}

// Invoke executes one tool call (mirrors ToolRegistry.invoke 的内置部分)。
func (r *ToolRegistry) Invoke(ctx context.Context, toolCtx *RuntimeContext, invocation ToolInvocation) ToolResult {
	handler, ok := r.handlers[invocation.ToolName]
	if !ok {
		return BuildFailureResult(invocation.ToolName, fmt.Sprintf("未找到工具 %s。", invocation.ToolName), nil, nil)
	}
	if handler == nil {
		// 防御：目录项未携带 handler 时显式失败回执，而不是调用 nil 函数
		// 崩溃进程（P0 崩溃链根因 1 的最后一道防线）。
		return BuildFailureResult(invocation.ToolName, fmt.Sprintf("工具 %s 没有可用的处理器。", invocation.ToolName), nil, nil)
	}
	return handler(ctx, toolCtx, invocation)
}

// HandlerOf returns the registered handler (nil when absent).
func (r *ToolRegistry) HandlerOf(name string) Handler {
	return r.handlers[name]
}

// Names returns all registered tool names (diagnostics).
func (r *ToolRegistry) Names() []string {
	names := make([]string, 0, len(r.entries))
	for _, entry := range r.entries {
		names = append(names, entry.Name)
	}
	sort.Strings(names)
	return names
}
