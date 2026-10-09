package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 本文件覆盖 host_llm_bridge.py 的语义:服务端 sampling 请求 → 任务 LLM →
// MCP 结果(含 tool_use / 必填工具校验 / 错误回包)。

// fakeSamplingLLM 是最小 LLM 接口的测试替身。
type fakeSamplingLLM struct {
	mu       sync.Mutex
	calls    []samplingCall
	response SamplingLLMResponse
	err      error
}

type samplingCall struct {
	taskName string
	messages []SamplingMessage
	options  SamplingGenerationOptions
}

func (f *fakeSamplingLLM) GenerateWithSamplingMessages(
	ctx context.Context,
	taskName string,
	messages []SamplingMessage,
	options SamplingGenerationOptions,
) (*SamplingLLMResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, samplingCall{taskName: taskName, messages: messages, options: options})
	if f.err != nil {
		return nil, f.err
	}
	response := f.response
	return &response, nil
}

func (f *fakeSamplingLLM) lastCall(t *testing.T) samplingCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatalf("LLM 未被调用")
	}
	return f.calls[len(f.calls)-1]
}

func TestHostLLMBridgeForwardsSamplingRequest(t *testing.T) {
	llm := &fakeSamplingLLM{response: SamplingLLMResponse{
		Response:  "模型回答",
		ModelName: "fake-model",
	}}
	bridge := NewHostLLMBridge(llm, "planner")

	restore := installInMemoryTransport(t, testServerOptions{sampling: true})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()

	service.SetCallbacks(bridge.BuildCallbacks())
	config := stdioTestConfig("alpha")
	config.Client.Sampling = MCPSamplingConfig{Enable: true, TaskName: "planner"}
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	// 服务端发起 sampling → 桥转发 → 结果回包。
	result := service.CallTool(context.Background(), ToolInvocation{
		ToolName:  "echo",
		Arguments: map[string]any{"text": "你好"},
	}, SessionContext{SessionID: "s1"})
	if !result.Success {
		t.Fatalf("调用失败: %+v", result)
	}
	if !strings.Contains(result.Content, "sampled=模型回答") {
		t.Fatalf("采样结果未回包: %q", result.Content)
	}
	if !strings.Contains(result.Content, "model=fake-model") {
		t.Fatalf("模型名未回包: %q", result.Content)
	}

	call := llm.lastCall(t)
	if call.taskName != "planner" {
		t.Fatalf("任务名应为 planner, got %q", call.taskName)
	}
	// 系统提示词：服务端 systemPrompt + user 消息。
	if len(call.messages) != 2 {
		t.Fatalf("消息数异常: %+v", call.messages)
	}
	if call.messages[0].Role != SamplingRoleSystem || call.messages[0].Text != "服务端系统提示" {
		t.Fatalf("系统提示词转换异常: %+v", call.messages[0])
	}
	if call.messages[1].Role != SamplingRoleUser || call.messages[1].Text != "你好" {
		t.Fatalf("用户消息转换异常: %+v", call.messages[1])
	}
	// 生成选项：maxTokens / temperature。
	if call.options.MaxTokens != 128 {
		t.Fatalf("maxTokens 未透传: %d", call.options.MaxTokens)
	}
	if call.options.Temperature == nil || *call.options.Temperature != 0.5 {
		t.Fatalf("temperature 未透传: %+v", call.options.Temperature)
	}
	if bridge.TaskName() != "planner" {
		t.Fatalf("TaskName 异常: %q", bridge.TaskName())
	}
}

func TestHostLLMBridgeDefaultTaskName(t *testing.T) {
	bridge := NewHostLLMBridge(&fakeSamplingLLM{}, "   ")
	if bridge.TaskName() != DefaultSamplingTaskName {
		t.Fatalf("空任务名应回落到 planner, got %q", bridge.TaskName())
	}
	callbacks := bridge.BuildCallbacks()
	if callbacks.Sampling == nil {
		t.Fatalf("BuildCallbacks 应包含 sampling")
	}
}

func TestHostLLMBridgeToolSupport(t *testing.T) {
	llm := &fakeSamplingLLM{response: SamplingLLMResponse{
		Response:  "先调用工具",
		ModelName: "fake-model",
		ToolCalls: []SamplingToolCall{
			{CallID: "call_1", FuncName: "server_side_tool", Args: map[string]any{"x": 1}},
			{CallID: "call_2", FuncName: "server_side_tool", Args: nil},
		},
	}}
	bridge := NewHostLLMBridge(llm, "planner")

	restore := installInMemoryTransport(t, testServerOptions{samplingTools: true})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	service.SetCallbacks(bridge.BuildCallbacks())

	config := stdioTestConfig("alpha")
	config.Client.Sampling = MCPSamplingConfig{Enable: true, TaskName: "planner", ToolSupport: true}
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	result := service.CallTool(context.Background(), ToolInvocation{
		ToolName:  "echo",
		Arguments: map[string]any{"text": "用工具"},
	}, SessionContext{})
	if !result.Success {
		t.Fatalf("调用失败: %+v", result)
	}
	// 服务端收到的应当是 2 个 tool_use 内容块 + toolUse stopReason。
	if !strings.Contains(result.Content, "stop=toolUse") {
		t.Fatalf("stopReason 异常: %q", result.Content)
	}
	if !strings.Contains(result.Content, "[call server_side_tool map[x:1]]") {
		t.Fatalf("工具调用未回包: %q", result.Content)
	}
	if !strings.Contains(result.Content, "model=fake-model") {
		t.Fatalf("模型名未回包: %q", result.Content)
	}

	call := llm.lastCall(t)
	if len(call.options.ToolOptions) != 1 {
		t.Fatalf("工具定义未透传: %+v", call.options.ToolOptions)
	}
	tool := call.options.ToolOptions[0]
	if tool.Name != "server_side_tool" || tool.Description != "服务端工具" {
		t.Fatalf("工具定义异常: %+v", tool)
	}
	if tool.ParametersSchema["type"] != "object" {
		t.Fatalf("工具 schema 未透传: %+v", tool.ParametersSchema)
	}
}

func TestHostLLMBridgeRequiredToolChoiceWithoutToolCall(t *testing.T) {
	llm := &fakeSamplingLLM{response: SamplingLLMResponse{Response: "我只想聊天", ModelName: "m"}}
	bridge := NewHostLLMBridge(llm, "planner")

	// 直接驱动处理器，不走传输（toolChoice=required + 无工具调用 → 报错）。
	_, err := bridge.HandleWithTools(context.Background(), &mcp.CreateMessageWithToolsRequest{
		Params: &mcp.CreateMessageWithToolsParams{
			MaxTokens: 32,
			Messages: []*mcp.SamplingMessageV2{
				{Role: "user", Content: []mcp.Content{&mcp.TextContent{Text: "必须用工具"}}},
			},
			Tools:      []*mcp.Tool{{Name: "t", InputSchema: map[string]any{"type": "object"}}},
			ToolChoice: &mcp.ToolChoice{Mode: "required"},
		},
	})
	if err == nil {
		t.Fatalf("required 模式下无工具调用应报错")
	}
	if !strings.Contains(err.Error(), "必须调用工具") {
		t.Fatalf("错误文案异常: %v", err)
	}

	call := llm.lastCall(t)
	// required 模式追加系统提示（文案照抄）。
	found := false
	for _, message := range call.messages {
		if message.Role == SamplingRoleSystem && strings.Contains(message.Text, "本轮回答必须至少调用一个工具；不要直接结束回答。") {
			found = true
		}
	}
	if !found {
		t.Fatalf("required 模式缺少系统提示: %+v", call.messages)
	}
}

func TestHostLLMBridgeToolChoiceNoneDropsTools(t *testing.T) {
	llm := &fakeSamplingLLM{response: SamplingLLMResponse{Response: "ok", ModelName: "m"}}
	bridge := NewHostLLMBridge(llm, "planner")

	result, err := bridge.HandleWithTools(context.Background(), &mcp.CreateMessageWithToolsRequest{
		Params: &mcp.CreateMessageWithToolsParams{
			MaxTokens: 32,
			Messages: []*mcp.SamplingMessageV2{
				{Role: "user", Content: []mcp.Content{&mcp.TextContent{Text: "别用工具"}}},
			},
			Tools:      []*mcp.Tool{{Name: "t", InputSchema: map[string]any{"type": "object"}}},
			ToolChoice: &mcp.ToolChoice{Mode: "none"},
		},
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(llm.lastCall(t).options.ToolOptions) != 0 {
		t.Fatalf("mode=none 时应丢弃工具定义")
	}
	// tools_enabled=false → 单块文本结果 + endTurn。
	if len(result.Content) != 1 || result.StopReason != "endTurn" {
		t.Fatalf("结果形态异常: %+v", result)
	}
	if text, ok := result.Content[0].(*mcp.TextContent); !ok || text.Text != "ok" {
		t.Fatalf("文本内容异常: %+v", result.Content[0])
	}
	if result.Role != "assistant" {
		t.Fatalf("role 异常: %q", result.Role)
	}
}

func TestHostLLMBridgeLLMFailureReturnsProtocolError(t *testing.T) {
	llm := &fakeSamplingLLM{err: errors.New("模型超时")}
	bridge := NewHostLLMBridge(llm, "planner")

	_, err := bridge.HandleWithTools(context.Background(), &mcp.CreateMessageWithToolsRequest{
		Params: &mcp.CreateMessageWithToolsParams{
			MaxTokens: 16,
			Messages: []*mcp.SamplingMessageV2{
				{Role: "user", Content: []mcp.Content{&mcp.TextContent{Text: "hi"}}},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "MCP Sampling 调用失败") || !strings.Contains(err.Error(), "模型超时") {
		t.Fatalf("错误未包装: %v", err)
	}
}

func TestHostLLMBridgeWithoutLLMErrors(t *testing.T) {
	bridge := NewHostLLMBridge(nil, "planner")
	_, err := bridge.HandleWithTools(context.Background(), &mcp.CreateMessageWithToolsRequest{
		Params: &mcp.CreateMessageWithToolsParams{},
	})
	if err == nil || !strings.Contains(err.Error(), "未注入采样模型") {
		t.Fatalf("未注入 LLM 时应报错: %v", err)
	}
}

func TestHostLLMBridgeRejectsUnknownRole(t *testing.T) {
	bridge := NewHostLLMBridge(&fakeSamplingLLM{}, "planner")
	_, err := bridge.HandleWithTools(context.Background(), &mcp.CreateMessageWithToolsRequest{
		Params: &mcp.CreateMessageWithToolsParams{
			Messages: []*mcp.SamplingMessageV2{
				{Role: "system", Content: []mcp.Content{&mcp.TextContent{Text: "x"}}},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "不支持的 MCP Sampling 消息角色") {
		t.Fatalf("未知角色应报错: %v", err)
	}
}

func TestHostLLMBridgeConvertsAssistantAndToolMessages(t *testing.T) {
	llm := &fakeSamplingLLM{response: SamplingLLMResponse{Response: "done", ModelName: "m"}}
	bridge := NewHostLLMBridge(llm, "planner")

	_, err := bridge.HandleWithTools(context.Background(), &mcp.CreateMessageWithToolsRequest{
		Params: &mcp.CreateMessageWithToolsParams{
			MaxTokens: 64,
			Messages: []*mcp.SamplingMessageV2{
				{Role: "user", Content: []mcp.Content{&mcp.TextContent{Text: "第一次提问"}}},
				{Role: "assistant", Content: []mcp.Content{
					&mcp.TextContent{Text: "我来调用工具"},
					&mcp.ToolUseContent{ID: "c1", Name: "calc", Input: map[string]any{"a": 1}},
				}},
				{Role: "user", Content: []mcp.Content{
					&mcp.ToolResultContent{
						ToolUseID: "c1",
						Content:   []mcp.Content{&mcp.TextContent{Text: "结果 2"}},
					},
					&mcp.TextContent{Text: "继续"},
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	call := llm.lastCall(t)
	// 期望: user / assistant(text) / assistant(tool_call) / tool / user
	roles := make([]SamplingRole, 0, len(call.messages))
	for _, message := range call.messages {
		roles = append(roles, message.Role)
	}
	want := []SamplingRole{SamplingRoleUser, SamplingRoleAssistant, SamplingRoleAssistant, SamplingRoleTool, SamplingRoleUser}
	if len(roles) != len(want) {
		t.Fatalf("消息序列异常: %+v", call.messages)
	}
	for index := range want {
		if roles[index] != want[index] {
			t.Fatalf("消息 %d 角色 = %s, want %s", index, roles[index], want[index])
		}
	}
	if len(call.messages[1].ToolCalls) != 0 || call.messages[1].Text != "我来调用工具" {
		t.Fatalf("assistant 文本消息异常: %+v", call.messages[1])
	}
	if len(call.messages[2].ToolCalls) != 1 || call.messages[2].ToolCalls[0].FuncName != "calc" {
		t.Fatalf("assistant 工具调用异常: %+v", call.messages[2])
	}
	// 同一 logical_turn_id 归组工具调用与文本。
	if call.messages[1].LogicalTurnID == "" || call.messages[1].LogicalTurnID != call.messages[2].LogicalTurnID {
		t.Fatalf("logical_turn_id 未归组: %q vs %q", call.messages[1].LogicalTurnID, call.messages[2].LogicalTurnID)
	}
	if call.messages[3].ToolCallID != "c1" || call.messages[3].Text != "结果 2" {
		t.Fatalf("tool_result 转换异常: %+v", call.messages[3])
	}
	if call.messages[3].LogicalTurnID != call.messages[2].LogicalTurnID {
		t.Fatalf("tool_result 应继承 logical_turn_id")
	}
	if call.messages[4].Text != "继续" {
		t.Fatalf("第二条 user 消息异常: %+v", call.messages[4])
	}
}

func TestHostLLMBridgeImageAndAudioContent(t *testing.T) {
	llm := &fakeSamplingLLM{response: SamplingLLMResponse{Response: "ok", ModelName: "m"}}
	bridge := NewHostLLMBridge(llm, "planner")

	_, err := bridge.HandleWithTools(context.Background(), &mcp.CreateMessageWithToolsRequest{
		Params: &mcp.CreateMessageWithToolsParams{
			MaxTokens: 32,
			Messages: []*mcp.SamplingMessageV2{
				{Role: "user", Content: []mcp.Content{
					&mcp.ImageContent{MIMEType: "image/png", Data: []byte("QUJD")},
					&mcp.ImageContent{MIMEType: "image/tiff", Data: []byte("QUJD")},
					&mcp.AudioContent{MIMEType: "audio/wav", Data: []byte("QUJD")},
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	message := llm.lastCall(t).messages[0]
	if len(message.Images) != 1 || message.Images[0].Format != "png" || message.Images[0].Base64 != "QUJD" {
		t.Fatalf("图片转换异常: %+v", message.Images)
	}
	if !strings.Contains(message.Text, "mime_type=image/tiff") {
		t.Fatalf("不支持的图片应降级为文本: %q", message.Text)
	}
	if !strings.Contains(message.Text, "[音频内容：mime_type=audio/wav]") {
		t.Fatalf("音频应降级为文本: %q", message.Text)
	}
}

func TestHostLLMBridgeToolDescriptionFallbacks(t *testing.T) {
	llm := &fakeSamplingLLM{}
	bridge := NewHostLLMBridge(llm, "planner")

	definitions := bridge.buildToolDefinitions([]*mcp.Tool{
		{Name: "with_desc", Description: " 描述 ", InputSchema: map[string]any{"$schema": "x", "type": "object"}},
		{Name: "title_only", Title: "标题", InputSchema: map[string]any{}},
		{Name: "no_meta"},
		{Name: "   "},
		nil,
	}, "auto")
	if len(definitions) != 3 {
		t.Fatalf("工具定义数量异常: %+v", definitions)
	}
	if definitions[0].Description != "描述" {
		t.Fatalf("描述异常: %q", definitions[0].Description)
	}
	if _, exists := definitions[0].ParametersSchema["$schema"]; exists {
		t.Fatalf("$schema 应被剥离")
	}
	if definitions[1].Description != "标题" {
		t.Fatalf("标题回落异常: %q", definitions[1].Description)
	}
	if definitions[1].ParametersSchema["type"] != "object" {
		t.Fatalf("空 schema 应补齐: %+v", definitions[1].ParametersSchema)
	}
	if definitions[2].Description != "工具 no_meta" {
		t.Fatalf("默认描述异常: %q", definitions[2].Description)
	}
}

func TestHostLLMBridgeSystemPromptMerge(t *testing.T) {
	bridge := NewHostLLMBridge(&fakeSamplingLLM{}, "planner")
	definitions := []SamplingToolDefinition{{Name: "t"}}

	if got := bridge.buildSystemPrompt("  原始提示  ", "auto", definitions); got != "原始提示" {
		t.Fatalf("提示词未清理: %q", got)
	}
	got := bridge.buildSystemPrompt("原始提示", "required", definitions)
	if got != "原始提示\n\n本轮回答必须至少调用一个工具；不要直接结束回答。" {
		t.Fatalf("required 提示词拼接异常: %q", got)
	}
	if got := bridge.buildSystemPrompt("", "required", nil); got != "" {
		t.Fatalf("无工具时不应追加提示: %q", got)
	}
}

func TestToolChoiceModeDefaultsToAuto(t *testing.T) {
	if got := toolChoiceModeOf(nil); got != "auto" {
		t.Fatalf("nil 应为 auto: %s", got)
	}
	if got := toolChoiceModeOf(&mcp.ToolChoice{Mode: ""}); got != "auto" {
		t.Fatalf("空模式应为 auto: %s", got)
	}
	if got := toolChoiceModeOf(&mcp.ToolChoice{Mode: "REQUIRED"}); got != "required" {
		t.Fatalf("大小写不敏感: %s", got)
	}
	if got := toolChoiceModeOf(&mcp.ToolChoice{Mode: "none"}); got != "none" {
		t.Fatalf("none 应保留: %s", got)
	}
}

func TestMaxTokensAndTemperatureDefaults(t *testing.T) {
	if got := maxTokensOf(0); got != 1024 {
		t.Fatalf("maxTokens 默认应为 1024: %d", got)
	}
	if got := maxTokensOf(256); got != 256 {
		t.Fatalf("maxTokens 透传异常: %d", got)
	}
	if temperatureOf(0) != nil {
		t.Fatalf("temperature=0 视作未设置")
	}
	value := temperatureOf(1.2)
	if value == nil || *value != 1.2 {
		t.Fatalf("temperature 透传异常: %v", value)
	}
}

func TestSamplingToolsCapabilityAdvertisedWithCallback(t *testing.T) {
	llm := &fakeSamplingLLM{}
	bridge := NewHostLLMBridge(llm, "planner")

	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	service.SetCallbacks(bridge.BuildCallbacks())

	config := stdioTestConfig("alpha")
	config.Client.Sampling = MCPSamplingConfig{Enable: true, TaskName: "planner", ToolSupport: true}
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	options := service.manager.connections["alpha"].clientOptions()
	if options.CreateMessageWithToolsHandler == nil {
		t.Fatalf("有回调时应装配 sampling handler")
	}
	if options.Capabilities.Sampling == nil || options.Capabilities.Sampling.Tools == nil {
		t.Fatalf("tool_support=true 时应声明 sampling.tools: %+v", options.Capabilities.Sampling)
	}
	if options.Capabilities.Sampling.Context != nil {
		t.Fatalf("include_context_support 未实现,不应声明 sampling.context")
	}
}

func TestSamplingEndToEndThroughInMemoryTransport(t *testing.T) {
	// 端到端:真实 MCP 往返(服务端 → 客户端 → 桥 → 假 LLM → 客户端 → 服务端)。
	llm := &fakeSamplingLLM{response: SamplingLLMResponse{
		Response:  "端到端回答",
		ModelName: "e2e-model",
		ToolCalls: []SamplingToolCall{{CallID: "c9", FuncName: "server_side_tool", Args: map[string]any{"k": "v"}}},
	}}
	bridge := NewHostLLMBridge(llm, "planner")

	restore := installInMemoryTransport(t, testServerOptions{samplingTools: true})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	service.SetCallbacks(bridge.BuildCallbacks())

	config := stdioTestConfig("alpha")
	config.Client.Sampling = MCPSamplingConfig{Enable: true, TaskName: "planner", ToolSupport: true}
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	result := service.CallTool(context.Background(), ToolInvocation{
		ToolName:  "echo",
		Arguments: map[string]any{"text": "端到端"},
	}, SessionContext{SessionID: "e2e"})
	if !result.Success {
		t.Fatalf("端到端调用失败: %+v", result)
	}
	if !strings.Contains(result.Content, "端到端回答") || !strings.Contains(result.Content, "stop=toolUse") {
		t.Fatalf("端到端结果异常: %q", result.Content)
	}
	if !strings.Contains(result.Content, "role=assistant") {
		t.Fatalf("role 异常: %q", result.Content)
	}
}
