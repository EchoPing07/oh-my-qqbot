// planner_bridge.go — the PlannerLLM implementation inside assemble: it maps
// runtime.PlannerRequest onto the LLM orchestrator + tool registry, mirroring
// 参考/MaiBot/src/maisaka/chat_loop_service.py (prompt assembly is owned by the
// prompts layer; this bridge covers transport + tool execution).
package assemble

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/chat/runtime"
	"oh-my-qqbot/internal/chat/tools"
	"oh-my-qqbot/internal/llm"
)

// plannerBridge satisfies runtime.PlannerLLM for every session. One instance
// per session (currentSession pinned at construction). 工具注册表由 App 持有
// （builtin + MCP 合并后的共享实例），会话视图按 app 解析，注册表热替换对
// 所有视图生效。
type plannerBridge struct {
	app            *App
	currentSession string

	// argParseErrors 记录本轮 ChatLoopStep 中参数 JSON 解析失败的工具调用
	// （call_id → 原始 arguments 原文）。ExecuteToolCalls 据此给对应调用返回
	// 失败回执而不是把空参数静默传给工具（引擎对同一会话视图按轮次顺序调用
	// ChatLoopStep → ExecuteToolCalls，无并发访问）。
	argParseErrors map[string]string
}

// Planner exposes the shared planner bridge (lazy).
func (a *App) Planner() *plannerBridge {
	a.plannerMu.Lock()
	defer a.plannerMu.Unlock()
	if a.planner == nil {
		a.planner = &plannerBridge{app: a}
	}
	return a.planner
}

// registry resolves the shared tool registry through the app (MCP 热替换后
// 会话视图无需重建即可看到新注册表).
func (p *plannerBridge) registry() *tools.ToolRegistry { return p.app.toolRegistry() }

// Registry exposes the tool registry (webui/mcp wiring).
func (a *App) Registry() *tools.ToolRegistry { return a.toolRegistry() }

// SetToolRegistry 用合并后的注册表替换共享实例（MCP 启动/热切换时调用）。
func (a *App) SetToolRegistry(registry *tools.ToolRegistry) {
	if registry == nil {
		return
	}
	a.registryMu.Lock()
	defer a.registryMu.Unlock()
	a.toolReg = registry
}

// toolRegistry 返回共享工具注册表（惰性构建 builtin 条目；MCP 启动后由
// SetToolRegistry 替换为含 MCP 条目的实例）。
func (a *App) toolRegistry() *tools.ToolRegistry {
	a.registryMu.RLock()
	if a.toolReg != nil {
		registry := a.toolReg
		a.registryMu.RUnlock()
		return registry
	}
	a.registryMu.RUnlock()

	a.registryMu.Lock()
	defer a.registryMu.Unlock()
	if a.toolReg == nil {
		a.toolReg = tools.NewToolRegistry(tools.BuiltinEntries(a.ToolRegistryFlags(), a.builtinToolDeps()))
	}
	return a.toolReg
}

// BuiltinEntries returns the builtin tool entries with handlers wired
// (mcp provider merge input).
func (a *App) BuiltinEntries() []tools.BuiltinToolEntry {
	return tools.BuiltinEntries(a.ToolRegistryFlags(), a.builtinToolDeps())
}

// ForSession returns a session-scoped view of the shared bridge.
func (p *plannerBridge) ForSession(sessionID string) *plannerBridge {
	view := *p
	view.currentSession = sessionID
	return &view
}

// ToolCtx builds a RuntimeContext for the given session.
func (a *App) ToolCtx(sessionID string) *tools.RuntimeContext {
	g := a.Cfg.GetGlobalConfig()
	flags := a.ToolRegistryFlags()
	return &tools.RuntimeContext{
		SessionID:               sessionID,
		IsGroupSession:          true,
		LogPrefix:               "[" + sessionID + "]",
		BotName:                 g.Bot.Nickname,
		EnableReplyAt:           flags.EnableReplyAt,
		EnableReplyQuote:        flags.EnableReplyQuote,
		UseNewEmojiSendLogic:    flags.UseNewEmojiSendLogic,
		UseVectorExpression:     flags.UseVectorExpression,
		MemoryQueryEnabled:      flags.EnableMemoryQueryTool,
		MemoryQueryDefaultLimit: flags.MemoryQueryDefaultLimit,
		FocusModeActive:         flags.FocusMode,
		IsBotSelf: func(platform, userID string) bool {
			return userID != "" && userID == g.Bot.QQAccount
		},
	}
}

// ChatLoopStep implements runtime.PlannerLLM.ChatLoopStep: one planner request.
func (p *plannerBridge) ChatLoopStep(ctx context.Context, request runtime.PlannerRequest) (*runtime.PlannerResponse, error) {
	req := contracts.ChatRequest{
		MaxTokens: 16384,
		Messages:  plannerMessages(request),
	}
	for _, def := range request.ToolDefinitions {
		// L1 修复：工具定义必须携带 parameters schema（BuildActionToolDefinitions
		// 已把 registry 的 ParametersSchema 放进 def.Parameters），否则
		// openai_client.convertTools 对空 ParametersJSON 回退为空 object schema，
		// 模型不知道该传什么参数（对齐 Python _convert_tool_options 的
		// parameters 直传语义）。
		schemaJSON := ""
		if len(def.Parameters) > 0 {
			encoded, err := json.Marshal(def.Parameters)
			if err != nil {
				slog.Warn("planner 工具 parameters schema 序列化失败",
					"session", request.SessionID, "tool", def.Name, "err", err)
			} else {
				schemaJSON = string(encoded)
			}
		}
		req.Tools = append(req.Tools, contracts.ToolSpec{
			Name:           def.Name,
			Description:    def.Description,
			ParametersJSON: schemaJSON,
		})
	}
	resp, err := p.app.LLM.ChatWithSession(ctx, "planner", request.SessionID, req)
	if err != nil {
		return nil, err
	}
	out := &runtime.PlannerResponse{
		Content:          resp.Content,
		Reasoning:        resp.Reasoning,
		ModelName:        resp.Model,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}

	// L2 修复：把模型输出按 item 粒度写回会话历史（runtime 引擎在
	// handlePlannerResponseActions 里 AppendChatHistory(response.OutputItems)）。
	// 对齐 Python chat_loop_step 的 bind_output_items_to_turn：assistant 文本
	// 与 function_call items 都要进入 output_items，否则多轮工具循环中模型
	// 看不到自己上一轮的调用，等于失忆。
	p.argParseErrors = nil
	if strings.TrimSpace(resp.Content) != "" {
		out.OutputItems = append(out.OutputItems, llm.NewAssistantItem(resp.Content))
	}
	for _, call := range resp.ToolCalls {
		args := map[string]any{}
		if rawArgs := strings.TrimSpace(call.ArgsJSON); rawArgs != "" {
			// L11 修复：参数 JSON 解析失败时不静默替换为空对象——记录原文与
			// 错误，ExecuteToolCalls 会给该调用返回"参数解析失败"回执。
			// （openai_client 现在保留 arguments 原文；空串视为无参调用。）
			if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
				slog.Warn("planner 工具调用参数解析失败",
					"session", request.SessionID, "call_id", call.ID,
					"tool", call.Name, "arguments", rawArgs, "err", err)
				if p.argParseErrors == nil {
					p.argParseErrors = map[string]string{}
				}
				p.argParseErrors[call.ID] = rawArgs
			}
		}
		out.ToolCalls = append(out.ToolCalls, runtime.PlannerToolCall{
			CallID:   call.ID,
			FuncName: call.Name,
			Args:     args,
		})
		// function_call item 需要非空 logical_turn_id（对齐 Python
		// FunctionCallItem.__post_init__ 的强校验）；引擎把
		// request.LogicalTurnID（= active_logical_turn_id）原样传入。
		item, itemErr := llm.NewFunctionCallItem(request.LogicalTurnID, call.ID, call.Name, args)
		if itemErr != nil {
			slog.Warn("planner function_call item 构造失败，跳过历史回写",
				"session", request.SessionID, "call_id", call.ID, "err", itemErr)
			continue
		}
		out.OutputItems = append(out.OutputItems, item)
	}
	return out, nil
}

// plannerMessages converts the runtime history into LLM messages via the
// context layer's ContextItem conversion (P4a owns the exact rendering).
//
// L2 修复：工具协议条目必须保持配对结构进入请求——相邻的 assistant 文本与
// function_call items 折叠为一条带 tool_calls 的 assistant 消息，tool result
// 转为带 tool_call_id 的 tool 消息（镜像 Python
// openai_client._convert_messages / _convert_assistant_item_group）。此前的
// 纯文本投影会把 function_call（无文本）与 tool result 一并丢弃，多轮工具
// 循环的历史不完整。
func plannerMessages(request runtime.PlannerRequest) []contracts.ChatMessage {
	items := make([]*llm.ContextItem, 0, len(request.History))
	for _, entry := range request.History {
		if entry == nil {
			continue
		}
		item := entry.ToContextItem(false)
		if item == nil {
			continue
		}
		// 工具协议条目（function_call / tool result）必须进入请求：Python 版
		// 的 count_in_context 只用于窗口计数，不作为发送过滤器
		// （ToolResultMessage.count_in_context=False 但仍会被
		// build_context_items_from_history_entry 转换发送）。
		if item.Kind != llm.ItemKindFunctionCall && item.Kind != llm.ItemKindToolResult && !entry.CountInContext() {
			continue
		}
		items = append(items, item)
	}
	// 镜像 Python _sanitize_messages_for_toolless_request：请求不带工具时清洗
	// 历史工具调用链，避免兼容接口拒收孤儿 tool 消息。
	if len(request.ToolDefinitions) == 0 {
		filtered := make([]*llm.ContextItem, 0, len(items))
		for _, item := range items {
			if item.Kind == llm.ItemKindFunctionCall || item.Kind == llm.ItemKindToolResult {
				continue
			}
			filtered = append(filtered, item)
		}
		items = filtered
	}

	messages := make([]contracts.ChatMessage, 0, len(items))
	for index := 0; index < len(items); {
		item := items[index]
		switch item.Kind {
		case llm.ItemKindFunctionCall, llm.ItemKindAssistant:
			// 折叠相邻的 assistant/function_call items 为一条 assistant 消息
			// （_convert_assistant_item_group：text parts 拼接 + tool_calls 聚合）。
			content := ""
			var toolCalls []contracts.ToolCall
			for index < len(items) {
				next := items[index]
				if next.Kind != llm.ItemKindFunctionCall && next.Kind != llm.ItemKindAssistant {
					break
				}
				if next.Kind == llm.ItemKindFunctionCall && next.ToolCall != nil {
					toolCalls = append(toolCalls, contracts.ToolCall{
						ID:       next.ToolCall.CallID,
						Name:     next.ToolCall.FuncName,
						ArgsJSON: next.ToolCall.ArgsJSON,
					})
				} else {
					content += next.Text()
				}
				index++
			}
			if content == "" && len(toolCalls) == 0 {
				continue
			}
			message := contracts.ChatMessage{Role: llm.RoleAssistant, Content: content}
			if len(toolCalls) > 0 {
				message.ToolCalls = toolCalls
			}
			messages = append(messages, message)
		case llm.ItemKindToolResult:
			messages = append(messages, contracts.ChatMessage{
				Role:       llm.RoleTool,
				Content:    item.Text(),
				ToolCallID: item.CallID,
				Name:       item.ToolName,
			})
			index++
		default:
			if content := item.Text(); content != "" {
				messages = append(messages, contracts.ChatMessage{Role: contracts.Role(item.Role()), Content: content})
			}
			index++
		}
	}
	for _, injected := range request.InjectedUserMessages {
		if injected != "" {
			messages = append(messages, contracts.ChatMessage{Role: contracts.RoleUser, Content: injected})
		}
	}
	for _, tail := range request.TailUserMessages {
		if tail != "" {
			messages = append(messages, contracts.ChatMessage{Role: contracts.RoleUser, Content: tail})
		}
	}
	return messages
}

// ExecuteToolCalls implements runtime.PlannerLLM.ExecuteToolCalls.
// LogicalTurnID / Reasoning 由引擎在 PlannerToolCall 上回填（wait 进入等待态
// 依赖非空 logical_turn_id；reasoning 作为 reply_reference 的兜底文案）。
func (p *plannerBridge) ExecuteToolCalls(ctx context.Context, calls []runtime.PlannerToolCall, planContent string) ([]runtime.PlannerToolResult, error) {
	results := make([]runtime.PlannerToolResult, 0, len(calls))
	for _, call := range calls {
		// L11 修复：本轮参数解析失败的调用不执行工具，直接返回失败回执
		// （回执内容说明"参数解析失败"，保证 assistant tool_call → tool result
		// 协议链完整，避免下一轮请求被网关拒收）。
		if _, broken := p.argParseErrors[call.CallID]; broken {
			delete(p.argParseErrors, call.CallID)
			results = append(results, runtime.PlannerToolResult{
				CallID:   call.CallID,
				ToolName: call.FuncName,
				Content: fmt.Sprintf("工具 %s 的参数解析失败：模型返回的 arguments 不是合法 JSON，已跳过执行。"+
					"请检查参数格式后重试。", call.FuncName),
				Success: false,
			})
			continue
		}

		toolCtx := p.app.ToolCtx(p.currentSession)
		result := p.registry().Invoke(ctx, toolCtx, tools.ToolInvocation{
			ToolName:      call.FuncName,
			Arguments:     call.Args,
			CallID:        call.CallID,
			SessionID:     p.currentSession,
			Reasoning:     call.Reasoning,
			LogicalTurnID: call.LogicalTurnID,
		})
		out := runtime.PlannerToolResult{
			CallID:   call.CallID,
			ToolName: call.FuncName,
			Content:  result.Content,
			Success:  result.Success,
		}
		if pause, ok := result.Metadata["pause_execution"].(bool); ok && pause {
			out.PauseExecution = true
			if reason, ok := result.Metadata["pause_reason"].(string); ok {
				out.PauseReason = reason
			}
		}
		// L2 修复：把工具回执写进历史（runtime 引擎对 result.PostHistory 执行
		// AppendChatHistory(BuildModelOutputContextMessages(...))，见 engine.go
		// 的消费点）。镜像 Python reasoning_engine._append_tool_execution_result：
		// 回执文本取 get_history_content()，为空时回退"工具执行成功。/工具 X 执行失败。"；
		// HISTORY_DEFERRED_TOOL_RESULT_NAMES={"wait"} 的暂停回执不入历史。
		if !(call.FuncName == "wait" && result.Success && out.PauseExecution) {
			receiptContent := result.HistoryContent()
			if strings.TrimSpace(receiptContent) == "" {
				if result.Success {
					receiptContent = "工具执行成功。"
				} else {
					receiptContent = fmt.Sprintf("工具 %s 执行失败。", call.FuncName)
				}
			}
			out.PostHistory = append(out.PostHistory, llm.NewToolResultItem(call.CallID, receiptContent, call.FuncName))
		}
		results = append(results, out)
	}
	p.argParseErrors = nil
	return results, nil
}

// BuildActionToolDefinitions implements runtime.PlannerLLM.BuildActionToolDefinitions.
func (p *plannerBridge) BuildActionToolDefinitions(ctx context.Context) ([]runtime.PlannerToolDefinition, string, error) {
	specs := p.registry().VisibleToolSpecs()
	defs := make([]runtime.PlannerToolDefinition, 0, len(specs))
	for _, spec := range specs {
		defs = append(defs, runtime.PlannerToolDefinition{
			Name:        spec.Name,
			Description: spec.Description,
			Parameters:  spec.ParametersSchema,
		})
	}
	return defs, p.registry().BuildDeferredToolsReminder(), nil
}

// BuildInjectedUserMessages implements runtime.PlannerLLM.BuildInjectedUserMessages.
func (p *plannerBridge) BuildInjectedUserMessages(ctx context.Context, deferredToolsReminder string) ([]string, error) {
	if deferredToolsReminder == "" {
		return nil, nil
	}
	return []string{deferredToolsReminder}, nil
}

// RefreshMidTermMemoryReference implements runtime.PlannerLLM (chat recall
// refresh is wired when the memory layer provides the summarizer).
func (p *plannerBridge) RefreshMidTermMemoryReference(ctx context.Context) error {
	return nil
}

// NotifyCycleEnd implements runtime.PlannerLLM.NotifyCycleEnd.
func (p *plannerBridge) NotifyCycleEnd(ctx context.Context, info runtime.CycleInfo) {
	p.app.Bus.Emit(contracts.Event{
		Kind:      contracts.EventPlannerProgress,
		SessionID: p.currentSession,
		Time:      time.Now(),
		Payload: map[string]any{
			"cycle_id": info.CycleID, "round_index": info.RoundIndex,
			"end_reason": info.EndReason,
		},
	})
}

var _ runtime.PlannerLLM = (*plannerBridge)(nil)
