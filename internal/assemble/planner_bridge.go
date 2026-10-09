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
// per session (currentSession pinned at construction).
type plannerBridge struct {
	app            *App
	registry       *tools.ToolRegistry
	currentSession string
}

// newPlannerBridge builds the shared tool registry once (per app).
func (a *App) newPlannerBridge() *plannerBridge {
	registry := tools.NewToolRegistry(tools.BuiltinEntries(a.ToolRegistryFlags()))
	return &plannerBridge{app: a, registry: registry}
}

// Planner exposes the shared planner bridge (lazy).
func (a *App) Planner() *plannerBridge {
	if a.planner == nil {
		a.planner = a.newPlannerBridge()
	}
	return a.planner
}

// Registry exposes the tool registry (webui/mcp wiring).
func (a *App) Registry() *tools.ToolRegistry { return a.Planner().registry }

// BuiltinEntries returns the builtin tool entries (mcp provider merge input).
func (a *App) BuiltinEntries() []tools.BuiltinToolEntry {
	return tools.BuiltinEntries(a.ToolRegistryFlags())
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
func (p *plannerBridge) ExecuteToolCalls(ctx context.Context, calls []runtime.PlannerToolCall, planContent string) ([]runtime.PlannerToolResult, error) {
	results := make([]runtime.PlannerToolResult, 0, len(calls))
	for _, call := range calls {
		toolCtx := p.app.ToolCtx(p.currentSession)
		result := p.registry.Invoke(ctx, toolCtx, tools.ToolInvocation{
			ToolName:  call.FuncName,
			Arguments: call.Args,
			CallID:    call.CallID,
			SessionID: p.currentSession,
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
	specs := p.registry.VisibleToolSpecs()
	defs := make([]runtime.PlannerToolDefinition, 0, len(specs))
	for _, spec := range specs {
		defs = append(defs, runtime.PlannerToolDefinition{
			Name:        spec.Name,
			Description: spec.Description,
			Parameters:  spec.ParametersSchema,
		})
	}
	return defs, p.registry.BuildDeferredToolsReminder(), nil
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
