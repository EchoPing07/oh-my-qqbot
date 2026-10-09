package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file mirrors 参考/MaiBot/src/mcp_module/host_llm_bridge.py: 把 MCP
// Sampling 请求桥接到主程序大模型调用链(默认任务名 planner)。
//
// 依赖边界:本包不 import internal/llm 的具体类型;宿主通过 SamplingLLM 最小
// 接口注入(装配层用 planner 任务的 LLM 实现)。

// ---------------------------------------------------------------------------
// 最小 LLM 接口（对齐 LLMServiceClient 的调用面）
// ---------------------------------------------------------------------------

// SamplingRole mirrors RoleType 的 Sampling 子集。
type SamplingRole string

// Sampling roles.
const (
	SamplingRoleSystem    SamplingRole = "system"
	SamplingRoleUser      SamplingRole = "user"
	SamplingRoleAssistant SamplingRole = "assistant"
	SamplingRoleTool      SamplingRole = "tool"
)

// SamplingMessage mirrors 桥接层内部消息形态(user/assistant/tool)。
type SamplingMessage struct {
	Role SamplingRole
	// Text 是可见文本内容。
	Text string
	// Images 是 base64 图片(格式: png/jpeg/webp/gif)。
	Images []SamplingImage
	// ToolCalls 是 assistant 消息中的工具调用。
	ToolCalls []SamplingToolCall
	// ToolCallID 是 tool 角色消息对应的 call_id。
	ToolCallID string
	// LogicalTurnID 对齐 ContextItemMeta.logical_turn_id(同一轮工具调用归组)。
	LogicalTurnID string
}

// SamplingImage mirrors add_image_content 的入参。
type SamplingImage struct {
	Base64 string
	Format string
}

// SamplingToolCall mirrors ToolCall。
type SamplingToolCall struct {
	CallID   string
	FuncName string
	Args     map[string]any
}

// SamplingToolDefinition mirrors ToolDefinitionInput。
type SamplingToolDefinition struct {
	Name        string
	Description string
	// ParametersSchema 是 JSON Schema 对象。
	ParametersSchema map[string]any
}

// SamplingGenerationOptions mirrors LLMGenerationOptions 的 Sampling 子集。
type SamplingGenerationOptions struct {
	MaxTokens   int
	Temperature *float64
	ToolOptions []SamplingToolDefinition
}

// SamplingLLMResponse mirrors LLMResponseResult 的 Sampling 子集。
type SamplingLLMResponse struct {
	Response  string
	ToolCalls []SamplingToolCall
	// ModelName 是实际使用的模型名；为空时回落到任务名。
	ModelName string
}

// SamplingLLM 是宿主注入的最小 LLM 接口（对齐
// LLMServiceClient.generate_response_with_context）。
type SamplingLLM interface {
	// GenerateWithSamplingMessages 用 Sampling 消息与生成选项调用任务模型。
	GenerateWithSamplingMessages(
		ctx context.Context,
		taskName string,
		messages []SamplingMessage,
		options SamplingGenerationOptions,
	) (*SamplingLLMResponse, error)
}

// ---------------------------------------------------------------------------
// 桥实现
// ---------------------------------------------------------------------------

// HostLLMBridge mirrors MCPHostLLMBridge。
type HostLLMBridge struct {
	taskName string
	llm      SamplingLLM
}

// NewHostLLMBridge mirrors MCPHostLLMBridge.__init__。
func NewHostLLMBridge(llm SamplingLLM, samplingTaskName string) *HostLLMBridge {
	taskName := strings.TrimSpace(samplingTaskName)
	if taskName == "" {
		taskName = DefaultSamplingTaskName
	}
	return &HostLLMBridge{taskName: taskName, llm: llm}
}

// DefaultSamplingTaskName mirrors sampling_task_name 的默认值。
const DefaultSamplingTaskName = "planner"

// TaskName 返回桥使用的任务名（供状态探测/日志）。
func (b *HostLLMBridge) TaskName() string {
	if b == nil {
		return ""
	}
	return b.taskName
}

// BuildCallbacks mirrors MCPHostLLMBridge.build_callbacks。
func (b *HostLLMBridge) BuildCallbacks() *HostCallbacks {
	return &HostCallbacks{Sampling: b}
}

// HandleWithTools mirrors handle_sampling_request: 把 `sampling/createMessage`
// 请求转发到任务 LLM 并转换回 MCP 结果。
//
// 与 Python 版的差异:go-sdk 在协议版本 >= 2026-07-28 时禁止服务端在处理器内
// 发起服务端请求,因此只保留 CreateMessageWithToolsHandler 一个入口;基础
// Sampling 请求(sampling/createMessage,无 tools)由 go-sdk 下转为
// CreateMessageWithToolsParams 后进入同一处理器,行为等价。
func (b *HostLLMBridge) HandleWithTools(
	ctx context.Context,
	request *mcp.CreateMessageWithToolsRequest,
) (*mcp.CreateMessageWithToolsResult, error) {
	if request == nil || request.Params == nil {
		return nil, fmt.Errorf("MCP Sampling 请求缺少参数")
	}
	if b == nil || b.llm == nil {
		return nil, fmt.Errorf("MCP Sampling 调用失败: 宿主未注入采样模型")
	}

	params := request.Params
	toolChoiceMode := toolChoiceModeOf(params.ToolChoice)
	toolDefinitions := b.buildToolDefinitions(params.Tools, toolChoiceMode)

	messages, err := b.convertMessages(params.Messages)
	if err != nil {
		return nil, fmt.Errorf("MCP Sampling 调用失败: %w", err)
	}
	if systemPrompt := b.buildSystemPrompt(params.SystemPrompt, toolChoiceMode, toolDefinitions); systemPrompt != "" {
		messages = append([]SamplingMessage{{Role: SamplingRoleSystem, Text: systemPrompt}}, messages...)
	}

	generation, err := b.llm.GenerateWithSamplingMessages(ctx, b.taskName, messages, SamplingGenerationOptions{
		MaxTokens:   maxTokensOf(params.MaxTokens),
		Temperature: temperatureOf(params.Temperature),
		ToolOptions: toolDefinitions,
	})
	if err != nil {
		return nil, fmt.Errorf("MCP Sampling 调用失败: %w", err)
	}
	if generation == nil {
		return nil, fmt.Errorf("MCP Sampling 调用失败: 模型层返回空结果")
	}

	if toolChoiceMode == "required" && len(toolDefinitions) > 0 && len(generation.ToolCalls) == 0 {
		return nil, fmt.Errorf("Sampling 要求必须调用工具，但模型未返回任何工具调用")
	}

	return b.buildSamplingResult(generation, len(toolDefinitions) > 0), nil
}

// toolChoiceModeOf mirrors _get_tool_choice_mode。
func toolChoiceModeOf(choice *mcp.ToolChoice) string {
	if choice == nil {
		return "auto"
	}
	switch strings.ToLower(strings.TrimSpace(choice.Mode)) {
	case "required":
		return "required"
	case "none":
		return "none"
	default:
		return "auto"
	}
}

// buildSystemPrompt mirrors _build_system_prompt（文案照抄）。
func (b *HostLLMBridge) buildSystemPrompt(
	rawSystemPrompt string,
	toolChoiceMode string,
	toolDefinitions []SamplingToolDefinition,
) string {
	parts := make([]string, 0, 2)
	if trimmed := strings.TrimSpace(rawSystemPrompt); trimmed != "" {
		parts = append(parts, trimmed)
	}
	if toolChoiceMode == "required" && len(toolDefinitions) > 0 {
		parts = append(parts, "本轮回答必须至少调用一个工具；不要直接结束回答。")
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// buildToolDefinitions mirrors _build_tool_definitions。
func (b *HostLLMBridge) buildToolDefinitions(rawTools []*mcp.Tool, toolChoiceMode string) []SamplingToolDefinition {
	if toolChoiceMode == "none" || len(rawTools) == 0 {
		return nil
	}
	definitions := make([]SamplingToolDefinition, 0, len(rawTools))
	for _, rawTool := range rawTools {
		if rawTool == nil {
			continue
		}
		toolName := strings.TrimSpace(rawTool.Name)
		if toolName == "" {
			continue
		}
		parameters := schemaMap(rawTool.InputSchema)
		delete(parameters, "$schema")

		description := strings.TrimSpace(rawTool.Description)
		if description == "" {
			description = strings.TrimSpace(rawTool.Title)
		}
		if description == "" {
			description = "工具 " + toolName
		}
		if len(parameters) == 0 {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		definitions = append(definitions, SamplingToolDefinition{
			Name:             toolName,
			Description:      description,
			ParametersSchema: parameters,
		})
	}
	if len(definitions) == 0 {
		return nil
	}
	return definitions
}

// convertMessages mirrors _build_context_factory + _convert_sampling_message:
// 把 MCP Sampling 消息转换为内部消息序列，tool_use/tool_result 归组到同一
// logical_turn_id。
func (b *HostLLMBridge) convertMessages(rawMessages []*mcp.SamplingMessageV2) ([]SamplingMessage, error) {
	messages := make([]SamplingMessage, 0, len(rawMessages))
	logicalTurnByCallID := map[string]string{}

	for _, rawMessage := range rawMessages {
		if rawMessage == nil {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(string(rawMessage.Role)))
		switch role {
		case "assistant":
			converted := b.convertAssistantMessage(rawMessage.Content)
			for _, item := range converted {
				if item.ToolCallID != "" {
					logicalTurnByCallID[item.ToolCallID] = item.LogicalTurnID
				}
			}
			messages = append(messages, converted...)
		case "user":
			converted, err := b.convertUserMessage(rawMessage.Content, logicalTurnByCallID)
			if err != nil {
				return nil, err
			}
			messages = append(messages, converted...)
		default:
			return nil, fmt.Errorf("不支持的 MCP Sampling 消息角色: %s", role)
		}
	}
	return messages, nil
}

// convertAssistantMessage mirrors _build_assistant_items。
func (b *HostLLMBridge) convertAssistantMessage(content []mcp.Content) []SamplingMessage {
	text := strings.Builder{}
	images := make([]SamplingImage, 0)
	toolCalls := make([]SamplingToolCall, 0)
	hasVisible := false

	for _, block := range content {
		switch typed := block.(type) {
		case *mcp.TextContent:
			if trimmed := strings.TrimSpace(typed.Text); trimmed != "" {
				text.WriteString(typed.Text)
				hasVisible = true
			}
		case *mcp.ImageContent:
			if image, ok := b.appendImage(typed); ok {
				images = append(images, image)
				hasVisible = true
				continue
			}
			text.WriteString("[图片内容：mime_type=" + defaultString(typed.MIMEType, "unknown") + "，当前客户端无法直接透传]")
			hasVisible = true
		case *mcp.AudioContent:
			text.WriteString("[音频内容：mime_type=" + defaultString(typed.MIMEType, "unknown") + "]")
			hasVisible = true
		case *mcp.ToolUseContent:
			toolCalls = append(toolCalls, SamplingToolCall{
				CallID:   strings.TrimSpace(typed.ID),
				FuncName: strings.TrimSpace(typed.Name),
				Args:     normalizeToolCallArguments(typed.Input),
			})
		}
	}

	if !hasVisible && len(toolCalls) == 0 {
		return nil
	}
	logicalTurnID := newLogicalTurnID()
	messages := make([]SamplingMessage, 0, len(toolCalls)+1)
	if hasVisible {
		messages = append(messages, SamplingMessage{
			Role:          SamplingRoleAssistant,
			Text:          text.String(),
			Images:        images,
			LogicalTurnID: logicalTurnID,
		})
	}
	for _, toolCall := range toolCalls {
		messages = append(messages, SamplingMessage{
			Role:          SamplingRoleAssistant,
			ToolCalls:     []SamplingToolCall{toolCall},
			ToolCallID:    toolCall.CallID,
			LogicalTurnID: logicalTurnID,
		})
	}
	return messages
}

// convertUserMessage mirrors _build_user_messages: user 可见内容与 tool_result
// 交错时按顺序切成独立消息。
func (b *HostLLMBridge) convertUserMessage(
	content []mcp.Content,
	logicalTurnByCallID map[string]string,
) ([]SamplingMessage, error) {
	messages := make([]SamplingMessage, 0, len(content))
	pending := SamplingMessage{Role: SamplingRoleUser}
	hasUserContent := false

	flush := func() {
		if !hasUserContent {
			return
		}
		messages = append(messages, pending)
		pending = SamplingMessage{Role: SamplingRoleUser}
		hasUserContent = false
	}

	for _, block := range content {
		switch typed := block.(type) {
		case *mcp.ToolResultContent:
			flush()
			toolCallID := firstNonEmpty(typed.ToolUseID, "tool_result")
			messages = append(messages, SamplingMessage{
				Role:          SamplingRoleTool,
				Text:          b.summarizeToolResult(typed),
				ToolCallID:    toolCallID,
				LogicalTurnID: logicalTurnByCallID[toolCallID],
			})
		case *mcp.TextContent:
			if strings.TrimSpace(typed.Text) != "" {
				pending.Text += typed.Text
				hasUserContent = true
			}
		case *mcp.ImageContent:
			if image, ok := b.appendImage(typed); ok {
				pending.Images = append(pending.Images, image)
			} else {
				pending.Text += "[图片内容：mime_type=" + defaultString(typed.MIMEType, "unknown") + "，当前客户端无法直接透传]"
			}
			hasUserContent = true
		case *mcp.AudioContent:
			pending.Text += "[音频内容：mime_type=" + defaultString(typed.MIMEType, "unknown") + "]"
			hasUserContent = true
		}
	}
	flush()
	return messages, nil
}

// summarizeToolResult mirrors _summarize_tool_result_content（Go 版复用
// models.SummarizeToolResultContent 以避免重复实现）。
func (b *HostLLMBridge) summarizeToolResult(content *mcp.ToolResultContent) string {
	return SummarizeToolResultContent(content)
}

// appendImage mirrors _append_sampling_content_to_builder 的 image 分支。
func (b *HostLLMBridge) appendImage(content *mcp.ImageContent) (SamplingImage, bool) {
	format := normalizeImageFormat(content.MIMEType)
	if len(content.Data) == 0 || format == "" {
		return SamplingImage{}, false
	}
	return SamplingImage{Base64: string(content.Data), Format: format}, true
}

// normalizeImageFormat mirrors _normalize_image_format。
func normalizeImageFormat(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpeg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return ""
	}
}

// normalizeToolCallArguments mirrors _normalize_tool_call_arguments。
func normalizeToolCallArguments(rawInput map[string]any) map[string]any {
	if rawInput == nil {
		return map[string]any{}
	}
	normalized := make(map[string]any, len(rawInput))
	for key, value := range rawInput {
		normalized[key] = value
	}
	return normalized
}

// buildSamplingResult mirrors _build_sampling_result。
func (b *HostLLMBridge) buildSamplingResult(
	generation *SamplingLLMResponse,
	toolsEnabled bool,
) *mcp.CreateMessageWithToolsResult {
	textContent := generation.Response
	toolCalls := generation.ToolCalls
	modelName := strings.TrimSpace(generation.ModelName)
	if modelName == "" {
		modelName = b.taskName
	}

	result := &mcp.CreateMessageWithToolsResult{
		Role:  "assistant",
		Model: modelName,
	}
	if toolsEnabled {
		blocks := make([]mcp.Content, 0, len(toolCalls)+1)
		if strings.TrimSpace(textContent) != "" {
			blocks = append(blocks, &mcp.TextContent{Text: textContent})
		}
		for _, toolCall := range toolCalls {
			blocks = append(blocks, &mcp.ToolUseContent{
				ID:    toolCall.CallID,
				Name:  toolCall.FuncName,
				Input: normalizeToolCallArguments(toolCall.Args),
			})
		}
		if len(blocks) == 0 {
			blocks = append(blocks, &mcp.TextContent{Text: ""})
		}
		result.Content = blocks
		if len(toolCalls) > 0 {
			result.StopReason = "toolUse"
		} else {
			result.StopReason = "endTurn"
		}
		return result
	}

	result.Content = []mcp.Content{&mcp.TextContent{Text: textContent}}
	result.StopReason = "endTurn"
	return result
}

// maxTokensOf mirrors `int(getattr(params, "maxTokens", 1024) or 1024)`。
func maxTokensOf(value int64) int {
	if value <= 0 {
		return 1024
	}
	return int(value)
}

// temperatureOf mirrors _coerce_float: Go SDK 的 Temperature 为 float64，
// 0 表示「未设置」，两种语义等价(默认温度)。
func temperatureOf(value float64) *float64 {
	if value == 0 {
		return nil
	}
	copied := value
	return &copied
}

// schemaMap 把 SDK 的 InputSchema(any) 归一化为 map。
func schemaMap(schema any) map[string]any {
	if schema == nil {
		return map[string]any{}
	}
	switch typed := schema.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, value := range typed {
			copied[key] = value
		}
		return copied
	case json.RawMessage:
		decoded := map[string]any{}
		if err := json.Unmarshal(typed, &decoded); err == nil {
			return decoded
		}
	case []byte:
		decoded := map[string]any{}
		if err := json.Unmarshal(typed, &decoded); err == nil {
			return decoded
		}
	case string:
		decoded := map[string]any{}
		if err := json.Unmarshal([]byte(typed), &decoded); err == nil {
			return decoded
		}
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return map[string]any{}
		}
		decoded := map[string]any{}
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return map[string]any{}
		}
		return decoded
	}
	return map[string]any{}
}
