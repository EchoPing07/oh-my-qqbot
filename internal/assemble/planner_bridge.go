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
	"regexp"
	"strings"
	"time"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/chat/runtime"
	"oh-my-qqbot/internal/chat/tools"
	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/prompt"
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

// sessionIsGroup 解析会话的群/私属性（P0-2 修复：此前 ToolCtx 硬编码 true，
// 私聊工具行为全部按群聊处理）。取值顺序：心流 runtime（运行期真值，对齐
// Python runtime.chat_stream.is_group_session）→ ChatManager 会话记录 →
// 兜底 true（与历史行为一致，避免未知会话误判）。
func (a *App) sessionIsGroup(sessionID string) bool {
	if rt := a.heartflowRuntime(sessionID); rt != nil {
		return rt.IsGroupSession()
	}
	if a.ChatManager != nil {
		if session := a.ChatManager.GetExistingSessionBySessionID(sessionID); session != nil {
			return session.IsGroupSession()
		}
	}
	return true
}

// ToolCtx builds a RuntimeContext for the given session.
func (a *App) ToolCtx(sessionID string) *tools.RuntimeContext {
	g := a.Cfg.GetGlobalConfig()
	flags := a.ToolRegistryFlags()
	return &tools.RuntimeContext{
		SessionID:               sessionID,
		IsGroupSession:          a.sessionIsGroup(sessionID),
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
	// P1-5 修复：渲染 system 提示词并置于请求首位（对齐 chat_loop_service.py
	// `_build_request_messages` 的 system item 在历史之前）。此前只发裸历史，
	// 人设/行为风格/工具使用说明全部丢失。
	messages := plannerMessages(request)
	if systemPrompt := p.plannerSystemPrompt(p.app.sessionIsGroup(request.SessionID)); systemPrompt != "" {
		messages = append([]contracts.ChatMessage{{Role: contracts.RoleSystem, Content: systemPrompt}}, messages...)
	}
	req := contracts.ChatRequest{
		MaxTokens: 16384,
		Messages:  messages,
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

// ---------------------------------------------------------------------------
// Planner system prompt（P1-5）
// ---------------------------------------------------------------------------

// plannerChatPromptName 对齐 chat_loop_service._get_chat_prompt_name 的默认
// 模板（focus 模式模板 maisaka_chat_focus 待焦点模式接线时启用）。
const plannerChatPromptName = "maisaka_chat"

// plannerFieldPattern 匹配模板里的 {var} 占位符（与 replyer 的手动替换同一
// 形态；用手动替换而非 RenderPrompt，保证模板新增字段缺失时降级而不是报错）。
var plannerFieldPattern = regexp.MustCompile(`\{([^{}]+)\}`)

// plannerSystemPrompt 渲染 Planner 的 system 提示词。
//
// 对齐 chat_loop_service.py 的 `_build_chat_system_prompt` +
// `build_prompt_template_context`（chat_loop_service.py:760-788）：
//   - bot_name ← global_config.bot.nickname
//   - behavior_style ← global_config.personality.behavior_style
//   - group_chat_attention_block ← chat.reply_style.group_chat_prompt /
//     private_chat_prompts（按群/私取一）
//   - query_memory_rule ← 按群/私的记忆检索提示
//
// 模板来自 prompts/<locale>/maisaka_chat.prompt；模板缺失或加载失败时降级为
// 内置兜底提示词（与 replyer 的兜底策略一致）。
func (p *plannerBridge) plannerSystemPrompt(isGroup bool) string {
	botName, behaviorStyle, groupPrompt, privatePrompt := "", "", "", ""
	if p.app.Cfg != nil {
		global := p.app.Cfg.GetGlobalConfig()
		botName = strings.TrimSpace(global.Bot.Nickname)
		behaviorStyle = strings.TrimSpace(global.Personality.BehaviorStyle)
		groupPrompt = strings.TrimSpace(global.Chat.ReplyStyle.GroupChatPrompt)
		privatePrompt = strings.TrimSpace(global.Chat.ReplyStyle.PrivateChatPrompt)
	}
	fields := map[string]string{
		"bot_name":                   botName,
		"behavior_style":             behaviorStyle,
		"group_chat_attention_block": buildPlannerAttentionBlock(isGroup, groupPrompt, privatePrompt),
		"query_memory_rule":          buildPlannerQueryMemoryRule(isGroup),
	}
	if manager := p.app.promptManager(); manager != nil {
		if tpl, err := manager.GetPrompt(plannerChatPromptName); err == nil && tpl != nil {
			return renderPlannerTemplate(prompt.TemplateText(tpl), fields)
		}
	}
	return buildFallbackPlannerSystemPrompt(fields)
}

// buildPlannerAttentionBlock mirrors `_build_group_chat_attention_block`
// （chat_loop_service.py:812-829）。
func buildPlannerAttentionBlock(isGroup bool, groupPrompt string, privatePrompt string) string {
	var promptLines []string
	if isGroup {
		if groupPrompt != "" {
			promptLines = append(promptLines, "通用注意事项：\n"+groupPrompt)
		}
	} else {
		if privatePrompt != "" {
			promptLines = append(promptLines, "通用注意事项：\n"+privatePrompt)
		}
	}
	if len(promptLines) == 0 {
		return ""
	}
	return "在该聊天中的注意事项：\n" + strings.Join(promptLines, "\n\n") + "\n"
}

// buildPlannerQueryMemoryRule mirrors `_build_query_memory_rule`
// （chat_loop_service.py:847-865，zh-CN 文案照抄）。
func buildPlannerQueryMemoryRule(isGroup bool) string {
	if isGroup {
		return "- query_memory()：只有回复明显依赖群内过去对话、共同经历、公开约定、任务进展或近期线索时使用；" +
			"不要为了寒暄、即时情绪回应、轻松接话、只看最近消息就能回答的内容而检索。不要把私聊或个人隐私记忆带到群聊里。"
	}
	return "- query_memory()：当对方提到“之前”“上次”“最近”“还记得吗”“我喜欢”“我说过”等信号，" +
		"或回复依赖长期偏好、先前承诺、共同经历、人物长期信息时，可以更积极检索。"
}

// renderPlannerTemplate 手动替换模板字段（对齐 replyer.buildSystemPrompt 的
// 渲染方式）：字段值经 RenderString 保护，值中的 { } 不参与占位符解析；
// 模板中未提供的字段保持原样。
func renderPlannerTemplate(template string, fields map[string]string) string {
	out := plannerFieldPattern.ReplaceAllStringFunc(template, func(match string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(match, "{"), "}")
		if value, ok := fields[name]; ok {
			return prompt.RenderString(value)
		}
		return match
	})
	return prompt.RestoreString(out)
}

// buildFallbackPlannerSystemPrompt 构造模板不可用时的内置兜底 system 提示词
// （骨架对齐 maisaka_chat.prompt，人设与场景注意事项来自同一份 fields）。
func buildFallbackPlannerSystemPrompt(fields map[string]string) string {
	botName := fields["bot_name"]
	if botName == "" {
		botName = "bot"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "你的任务是分析聊天和聊天中的互动情况，然后做出下一步动作。\n")
	fmt.Fprintf(&builder, "你需要关注 %s 与用户的对话来为 %s 选择正确的动作和行为\n\n", botName, botName)
	if behaviorStyle := fields["behavior_style"]; behaviorStyle != "" {
		fmt.Fprintf(&builder, "%s的行为风格：%s\n\n以上 %s的行为风格可以帮助你更好地决策\n\n", botName, behaviorStyle, botName)
	}
	builder.WriteString("请你对当前场景和输出规则来进行分析。请注意，你不是 " + botName +
		" 本人，不要替 " + botName + " 发言，你需要给 " + botName +
		" 的行为做出决策，请你结合 " + botName + " 的行为风格、当前情况和可用工具做出决策。\n")
	builder.WriteString("你需要先搜集能够帮助" + botName + "进行下一步行动的信息，然后再给出思考。\n\n")
	if attentionBlock := fields["group_chat_attention_block"]; attentionBlock != "" {
		builder.WriteString(attentionBlock + "\n")
	}
	builder.WriteString("# Using your tools\n")
	builder.WriteString("- 当你判断" + botName + "现在应该正式对用户发出一条可见回复时调用 reply。调用后生成一条真正展示给用户的回复。" +
		"发言必须通过 reply 工具，不然用户无法看见；注意，你无法不调用 reply 工具直接回复。\n")
	builder.WriteString(fields["query_memory_rule"] + "\n")
	builder.WriteString("- 你可以在一次响应中调用多个工具；有依赖关系的调用必须等待前序结果，不要并行。\n")
	builder.WriteString("- 不使用工具：当没有更多操作需要做时，结束思考，不要调用任何工具。\n")
	return builder.String()
}
