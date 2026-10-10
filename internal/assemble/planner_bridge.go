// planner_bridge.go — the PlannerLLM implementation inside assemble: it maps
// runtime.PlannerRequest onto the LLM orchestrator + tool registry, mirroring
// 参考/MaiBot/src/maisaka/chat_loop_service.py (prompt assembly is owned by the
// prompts layer; this bridge covers transport + tool execution).
package assemble

import (
	"context"
	"encoding/json"
	"time"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/chat/runtime"
	"oh-my-qqbot/internal/chat/tools"
)

// plannerBridge satisfies runtime.PlannerLLM for every session. One instance
// per session (currentSession pinned at construction). 工具注册表由 App 持有
// （builtin + MCP 合并后的共享实例），会话视图按 app 解析，注册表热替换对
// 所有视图生效。
type plannerBridge struct {
	app            *App
	currentSession string
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
		req.Tools = append(req.Tools, contracts.ToolSpec{
			Name:        def.Name,
			Description: def.Description,
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
	for _, call := range resp.ToolCalls {
		args := map[string]any{}
		_ = json.Unmarshal([]byte(call.ArgsJSON), &args)
		out.ToolCalls = append(out.ToolCalls, runtime.PlannerToolCall{
			CallID:   call.ID,
			FuncName: call.Name,
			Args:     args,
		})
	}
	return out, nil
}

// plannerMessages converts the runtime history into LLM messages via the
// context layer's ContextItem conversion (P4a owns the exact rendering).
func plannerMessages(request runtime.PlannerRequest) []contracts.ChatMessage {
	messages := make([]contracts.ChatMessage, 0, len(request.History)+len(request.InjectedUserMessages)+1)
	for _, item := range request.History {
		if item == nil || !item.CountInContext() {
			continue
		}
		content := item.ProcessedPlainText()
		if content == "" {
			continue
		}
		role := contracts.Role(item.Role())
		messages = append(messages, contracts.ChatMessage{Role: role, Content: content})
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
		results = append(results, out)
	}
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
