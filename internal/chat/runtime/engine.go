package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/chat/trigger"
	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/message"
)

// This file mirrors the internal round loop of
// 参考/MaiBot/src/maisaka/reasoning_engine.py:
// `run_loop`, `_prepare_turn_start_context`, `_collect_pending_messages_before_next_round`,
// `_handle_silent_turn`, `_handle_planner_response_actions`,
// `_handle_planner_interrupt`, `_build_wait_completed_message` and
// `_cycle_end_for_pause_tool` / `_cycle_end_for_max_rounds`.

// Cycle end reasons mirroring `CycleEnd(...)` reason literals used across
// reasoning_engine.py / mode_policy.py.
const (
	CycleEndContinue           = "continue"
	CycleEndToolContinue       = "tool_continue"
	CycleEndToolStopAfterExec  = "tool_stop_after_execution"
	CycleEndToolPauseWait      = "tool_pause:wait"
	CycleEndPlannerWaitRest    = "planner_wait_rest"
	CycleEndToolPause          = "tool_pause"
	CycleEndPlannerNoToolEnd   = "planner_no_tool_end"
	CycleEndMaxRounds          = "max_rounds"
	CycleEndPlannerInterrupted = "planner_interrupted"
)

// StopAfterExecutionPauseReason mirrors STOP_AFTER_EXECUTION_PAUSE_REASON.
const StopAfterExecutionPauseReason = "stop_after_execution"

// Wait completed tool-result texts, copied from `_build_wait_completed_message`.
const (
	// `requested_text` already carries its own prefix, exactly like
	// `_build_wait_completed_message` in reasoning_engine.py.
	waitCompletedWithNewMessages = "等待已结束，实际等待 %s%s，期间收到了新的用户输入。请结合这些新消息继续下一轮思考。"
	waitCompletedNoNewMessages   = "等待已超时，实际等待 %s%s，期间没有收到新的用户输入。请基于现有上下文继续下一轮思考。"
)

// CycleEnd mirrors CycleEnd(reason, detail).
type CycleEnd struct {
	Reason string
	Detail string
}

// TurnStartContext mirrors TurnStartContext.
type TurnStartContext struct {
	CachedMessages       []*message.SessionMessage
	TriggerMessage       *message.SessionMessage
	TimeoutTriggered     bool
	ProactiveTriggered   bool
	SilentReplyFrequency bool
	LogicalTurnID        string
}

// Engine mirrors MaisakaReasoningEngine.
type Engine struct {
	runtime *Runtime

	lastReasoningContent string
	activeLogicalTurnID  string
	cycleCounter         int

	running bool

	// 引擎作用域执行上下文（T5 修复）：RunLoop 启动时创建，stop 关闭或循环
	// 退出时取消，工具执行（reply 段间 sleep、渠道发送）与 planner 请求都从它
	// 派生 ctx，保证停止语义能传导到阻塞段。ctxMu 保护（RunLoop 在引擎
	// goroutine 写入，测试可能直接调 handlePlannerResponseActions）。
	ctxMu      sync.Mutex
	execCtx    context.Context
	execCancel context.CancelFunc
}

// NewEngine mirrors MaisakaReasoningEngine.__init__.
func NewEngine(runtime *Runtime) *Engine {
	return &Engine{runtime: runtime}
}

// LastReasoningContent mirrors the `last_reasoning_content` property.
func (e *Engine) LastReasoningContent() string { return e.lastReasoningContent }

// ActiveLogicalTurnID mirrors the `active_logical_turn_id` property.
func (e *Engine) ActiveLogicalTurnID() string { return e.activeLogicalTurnID }

// CycleCounter returns the started-cycle count.
func (e *Engine) CycleCounter() int { return e.cycleCounter }

// RunLoop mirrors MaisakaReasoningEngine.run_loop. It consumes triggers until
// the runtime stops or `stop` is closed.
func (e *Engine) RunLoop(stop <-chan struct{}) error {
	e.running = true
	defer func() { e.running = false }()

	// T5 修复：建立引擎作用域 ctx——stop 关闭（运行时停止/引擎循环退出）时
	// 取消，本轮 planner 请求与工具执行全部派生自它，打断/停止语义因此能
	// 传导到 reply 段间 sleep 与渠道发送。
	execCtx, cancelExec := context.WithCancel(context.Background())
	e.ctxMu.Lock()
	e.execCtx = execCtx
	e.execCancel = cancelExec
	e.ctxMu.Unlock()
	defer func() {
		cancelExec()
		e.ctxMu.Lock()
		e.execCtx = nil
		e.execCancel = nil
		e.ctxMu.Unlock()
	}()
	go func() {
		select {
		case <-stop:
			cancelExec()
		case <-execCtx.Done():
		}
	}()

	for {
		queuedTrigger, ok := e.runtime.NextTurnTrigger(stop)
		if !ok {
			return nil
		}

		// `if not focus_mode_manager.can_decide(...)`.
		if !e.runtime.CanDecide() {
			e.runtime.MarkMessageTurnUnscheduled()
			e.runtime.Logf("%s 当前不在 focus 状态，忽略已排队的 Maisaka 触发", e.runtime.LogPrefix())
			continue
		}

		turnStart, err := e.prepareTurnStartContext(queuedTrigger)
		if err != nil {
			return err
		}
		if turnStart.TriggerMessage == nil {
			continue
		}

		if turnStart.SilentReplyFrequency {
			if err := e.handleSilentTurn(turnStart); err != nil {
				return err
			}
			continue
		}

		if err := e.runCycle(turnStart); err != nil {
			if IsInterruptedError(err) {
				continue
			}
			return err
		}
	}
}

// executionContext returns the engine-scoped execution ctx（RunLoop 未运行时
// 返回 Background，保持测试直接驱动引擎路径可用）。
func (e *Engine) executionContext() context.Context {
	e.ctxMu.Lock()
	defer e.ctxMu.Unlock()
	if e.execCtx == nil {
		return context.Background()
	}
	return e.execCtx
}

// cancelExecution 取消引擎作用域执行上下文（Runtime.Stop 时调用：打断进行中
// 的 planner 请求与工具执行，保证停止语义传导到 reply 段间 sleep 与渠道发送；
// 幂等，RunLoop 未运行时为空操作）。
func (e *Engine) cancelExecution() {
	e.ctxMu.Lock()
	cancel := e.execCancel
	e.ctxMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// prepareTurnStartContext mirrors `_prepare_turn_start_context`.
func (e *Engine) prepareTurnStartContext(queuedTrigger string) (TurnStartContext, error) {
	messageTriggered, timeoutTriggered, proactiveTriggered := e.runtime.DrainReadyTurnTriggers(queuedTrigger)
	silentReplyFrequency := e.runtime.IsReplyFrequencySilent()

	if e.runtime.AgentState() == trigger.StateWait &&
		!(timeoutTriggered || proactiveTriggered) &&
		!silentReplyFrequency {
		e.runtime.MarkMessageTurnUnscheduled()
		e.runtime.Logf("%s 当前仍处于 wait 状态，忽略消息触发并继续等待超时", e.runtime.LogPrefix())
		return TurnStartContext{
			TimeoutTriggered:     timeoutTriggered,
			ProactiveTriggered:   proactiveTriggered,
			SilentReplyFrequency: silentReplyFrequency,
		}, nil
	}

	if messageTriggered {
		e.runtime.WaitForMessageQuietPeriod()
		e.runtime.MarkMessageTurnUnscheduled()
	}

	var cachedMessages []*message.SessionMessage
	if e.runtime.HasPendingMessages() {
		cachedMessages = e.runtime.CollectPendingMessages()
	}
	continuationLogicalTurnID := ""

	if len(cachedMessages) > 0 {
		e.runtime.EnterRunningState()
		if e.runtime.HasPendingWaitToolCall() {
			waitMessage, err := e.buildWaitCompletedMessage(true)
			if err != nil {
				return TurnStartContext{}, err
			}
			continuationLogicalTurnID = waitMessage.LogicalTurnID()
			e.runtime.AppendChatHistory(waitMessage)
		}
		if err := e.ingestMessages(cachedMessages); err != nil {
			return TurnStartContext{}, err
		}
		return TurnStartContext{
			CachedMessages:       cachedMessages,
			TriggerMessage:       cachedMessages[len(cachedMessages)-1],
			TimeoutTriggered:     timeoutTriggered,
			ProactiveTriggered:   proactiveTriggered,
			SilentReplyFrequency: silentReplyFrequency,
			LogicalTurnID:        continuationLogicalTurnID,
		}, nil
	}

	if proactiveTriggered {
		triggerMessage := e.runtime.consumeProactiveTriggerMessage()
		proactiveLogicalTurnID := e.runtime.consumeProactiveLogicalTurnID()
		if triggerMessage == nil {
			e.runtime.Logf("%s 主动触发缺少对应的触发消息，跳过本轮", e.runtime.LogPrefix())
			return TurnStartContext{
				TimeoutTriggered:     timeoutTriggered,
				ProactiveTriggered:   proactiveTriggered,
				SilentReplyFrequency: silentReplyFrequency,
			}, nil
		}
		if e.runtime.HasPendingWaitToolCall() {
			waitMessage, err := e.buildWaitCompletedMessage(false)
			if err != nil {
				return TurnStartContext{}, err
			}
			continuationLogicalTurnID = waitMessage.LogicalTurnID()
			e.runtime.AppendChatHistory(waitMessage)
		} else if proactiveLogicalTurnID != "" {
			continuationLogicalTurnID = proactiveLogicalTurnID
		}
		if restorer, ok := e.runtime.Planner().(interface {
			RestoreProactiveUserContext(ctx context.Context) (int, error)
		}); ok {
			if _, restoreErr := restorer.RestoreProactiveUserContext(context.Background()); restoreErr != nil {
				e.runtime.Logf("%s 主动回合回填用户消息失败: %v", e.runtime.LogPrefix(), restoreErr)
			}
		}
		return TurnStartContext{
			CachedMessages:       cachedMessages,
			TriggerMessage:       triggerMessage,
			TimeoutTriggered:     timeoutTriggered,
			ProactiveTriggered:   proactiveTriggered,
			SilentReplyFrequency: silentReplyFrequency,
			LogicalTurnID:        continuationLogicalTurnID,
		}, nil
	}

	if timeoutTriggered && e.runtime.HasPendingWaitToolCall() {
		triggerMessage := e.runtime.lastCachedMessage()
		if triggerMessage == nil {
			e.runtime.Logf("%s wait 超时后没有可复用的触发消息，跳过本轮", e.runtime.LogPrefix())
			return TurnStartContext{
				TimeoutTriggered:     timeoutTriggered,
				ProactiveTriggered:   proactiveTriggered,
				SilentReplyFrequency: silentReplyFrequency,
			}, nil
		}
		e.runtime.Logf("%s 等待结束，再看一眼！", e.runtime.LogPrefix())
		waitMessage, err := e.buildWaitCompletedMessage(false)
		if err != nil {
			return TurnStartContext{}, err
		}
		continuationLogicalTurnID = waitMessage.LogicalTurnID()
		e.runtime.AppendChatHistory(waitMessage)
		return TurnStartContext{
			CachedMessages:       cachedMessages,
			TriggerMessage:       triggerMessage,
			TimeoutTriggered:     timeoutTriggered,
			ProactiveTriggered:   proactiveTriggered,
			SilentReplyFrequency: silentReplyFrequency,
			LogicalTurnID:        continuationLogicalTurnID,
		}, nil
	}

	e.runtime.Logf("%s 消息已由当前内部轮次处理，忽略过期的轮次触发", e.runtime.LogPrefix())
	return TurnStartContext{
		TimeoutTriggered:     timeoutTriggered,
		ProactiveTriggered:   proactiveTriggered,
		SilentReplyFrequency: silentReplyFrequency,
	}, nil
}

// runCycle mirrors the body of run_loop's inner `while round_index <
// self._runtime._max_internal_rounds` loop, including the `try/except/finally`
// structure that makes `_finalize_cycle` run exactly once per round and prevents
// `round_index` from advancing after an interruption.
func (e *Engine) runCycle(turnStart TurnStartContext) error {
	defer func() {
		e.activeLogicalTurnID = ""
		if e.runtime.AgentState() == trigger.StateRunning {
			e.runtime.EnterStopState()
		}
	}()

	e.activeLogicalTurnID = turnStart.LogicalTurnID
	if e.activeLogicalTurnID == "" {
		e.activeLogicalTurnID = newLogicalTurnID()
	}
	if reason, ok := e.runtime.ConsumeForcedTurnReason(); ok {
		e.runtime.Logf("%s %s", e.runtime.LogPrefix(), reason)
	}

	planner := e.runtime.Planner()
	if planner == nil {
		return fmt.Errorf("runtime: 未注入 PlannerLLM，无法进入内部循环")
	}

	cachedMessages := turnStart.CachedMessages
	triggerMessage := turnStart.TriggerMessage
	midTermReferenceRefreshed := false
	roundIndex := 0
	maxRounds := e.runtime.options.maxInternalRounds()

	// T5 修复：轮作用域可取消 ctx——本轮 planner 请求与工具执行全部派生自它。
	// 正常轮结束时 ExecuteToolCalls 已同步返回，defer 的取消不影响已完成的
	// 发送；只有引擎停止/循环退出（父 ctx 取消）才会打断进行中的工具执行。
	cycleCtx, cancelCycle := context.WithCancel(e.executionContext())
	defer cancelCycle()

	for roundIndex < maxRounds {
		pendingRoundMessages, err := e.collectPendingMessagesBeforeNextRound(roundIndex)
		if err != nil {
			return err
		}
		if len(pendingRoundMessages) > 0 {
			cachedMessages = pendingRoundMessages
			triggerMessage = pendingRoundMessages[len(pendingRoundMessages)-1]
		}

		cycleID, roundText := e.startCycleRound(roundIndex)
		state := &cycleRuntimeState{}

		if !midTermReferenceRefreshed {
			if err := planner.RefreshMidTermMemoryReference(context.Background()); err != nil {
				e.runtime.Logf("%s 聊天回想参考刷新失败，已跳过: %v", e.runtime.LogPrefix(), err)
			}
			midTermReferenceRefreshed = true
		}

		e.runtime.StartPlannerContinuation()
		response, requestErr := e.runPlannerRequest(cycleCtx, planner, triggerMessage, cachedMessages, roundText, &state.currentStageStartedAt)
		if requestErr == nil {
			state.response = response
			var actionErr error
			state.breakAfterAction, actionErr = e.handlePlannerResponseActions(cycleCtx, planner, response, cycleID, state)
			if actionErr == nil {
				// `continue` inside the try: the finally still finalizes the round
				// and the loop advances to the next round index.
				e.finalizeCycle(planner, cycleID, roundIndex, state)
				if state.breakAfterAction {
					break
				}
				roundIndex++
				continue
			}
			requestErr = actionErr
		}

		if !IsInterruptedError(requestErr) {
			e.finalizeCycle(planner, cycleID, roundIndex, state)
			return requestErr
		}

		// `except ReqAbortException`.
		state.plannerInterrupted = true
		state.cycleEnd = CycleEnd{Reason: CycleEndPlannerInterrupted, Detail: "Planner 被新消息打断，当前轮结束。"}
		interruptResult, interruptErr := e.handlePlannerInterrupt(roundIndex, roundText, state.currentStageStartedAt, planner)
		if interruptErr != nil {
			e.finalizeCycle(planner, cycleID, roundIndex, state)
			return interruptErr
		}
		state.response = interruptResult.response
		retryMessages := interruptResult.retryMessages
		if len(retryMessages) > 0 {
			cachedMessages = retryMessages
			triggerMessage = retryMessages[len(retryMessages)-1]
			e.runtime.EnterRunningState()
		}

		// `finally`: finalize the round, and only advance the round index when the
		// round was not interrupted.
		e.finalizeCycle(planner, cycleID, roundIndex, state)
		if len(retryMessages) == 0 {
			break
		}
	}

	return nil
}

type cycleRuntimeState struct {
	currentStageStartedAt float64
	response              *PlannerResponse
	cycleEnd              CycleEnd
	plannerInterrupted    bool
	breakAfterAction      bool
	toolResults           []PlannerToolResult
}

type plannerInterruptResult struct {
	response      *PlannerResponse
	retryMessages []*message.SessionMessage
}

func (e *Engine) runPlannerRequest(
	cycleCtx context.Context,
	planner PlannerLLM,
	triggerMessage *message.SessionMessage,
	sourceMessages []*message.SessionMessage,
	roundText string,
	stageStartedAt *float64,
) (*PlannerResponse, error) {
	_ = triggerMessage
	_ = sourceMessages
	e.runtime.Logf("%s 开始思考: %s", e.runtime.LogPrefix(), roundText)
	// 请求 ctx 挂在轮作用域 ctx 之下：打断只取消本次请求（对齐 Python
	// ReqAbortException 语义），而引擎停止会经父 ctx 一并取消。
	ctx, cancel := context.WithCancel(cycleCtx)
	defer cancel()
	interruptFlag := NewInterruptFlag()
	e.runtime.BindPlannerInterrupt(interruptFlag, cancel)
	interrupted := false
	defer func() {
		e.runtime.UnbindPlannerInterruptFlag(interruptFlag, interrupted)
	}()

	toolDefinitions, deferredToolsReminder, err := planner.BuildActionToolDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	injected, err := planner.BuildInjectedUserMessages(ctx, deferredToolsReminder)
	if err != nil {
		e.runtime.Logf("%s 一次性注入消息构造失败，已跳过: %v", e.runtime.LogPrefix(), err)
		injected = nil
	}

	request := PlannerRequest{
		SessionID:            e.runtime.SessionID(),
		History:              e.runtime.ChatHistory(),
		InjectedUserMessages: injected,
		ToolDefinitions:      toolDefinitions,
		MaxContextSize:       e.runtime.options.maxContextSize(),
		LogicalTurnID:        e.activeLogicalTurnID,
		Interrupted:          interruptFlag.Done(),
	}
	response, err := planner.ChatLoopStep(ctx, request)
	if err != nil {
		if IsInterruptedError(err) {
			interrupted = true
		}
		return nil, err
	}
	return response, nil
}

// handlePlannerResponseActions mirrors `_handle_planner_response_actions`.
// It returns whether the loop must end after this action.
//
// ctx 为轮作用域执行上下文（T5 修复）：此前工具执行用 context.Background()
// 完全不可取消，运行时停止时 reply 段间 sleep 与渠道发送无法被打断。
func (e *Engine) handlePlannerResponseActions(
	ctx context.Context,
	planner PlannerLLM,
	response *PlannerResponse,
	cycleDetail int,
	state *cycleRuntimeState,
) (bool, error) {
	plannerContent := plannerContentOf(response)
	e.lastReasoningContent = plannerContent

	// `self._runtime._chat_history.extend(response.raw_messages)`
	e.runtime.AppendChatHistory(chatcontext.BuildModelOutputContextMessages(response.OutputItems, chatcontext.SourceAssistant)...)

	if len(response.ToolCalls) == 0 {
		e.endPlannerNoToolCycle()
		state.cycleEnd = CycleEnd{Reason: CycleEndPlannerNoToolEnd, Detail: "Planner本轮思考结束。"}
		state.toolResults = nil
		return true, nil
	}

	// 回填工具调用的逻辑轮次 ID 与推理文本（对齐 Python invocation 的
	// logical_turn_id/reasoning 传递）：wait 进入等待态依赖非空
	// logical_turn_id，缺失会被 _try_enter_wait_state 拒绝。
	for index := range response.ToolCalls {
		response.ToolCalls[index].LogicalTurnID = e.activeLogicalTurnID
		if strings.TrimSpace(response.ToolCalls[index].Reasoning) == "" {
			response.ToolCalls[index].Reasoning = response.Reasoning
		}
	}
	results, err := planner.ExecuteToolCalls(ctx, response.ToolCalls, plannerContent)
	if err != nil {
		if IsInterruptedError(err) {
			return false, err
		}
		return false, err
	}
	state.toolResults = results

	for _, result := range results {
		if len(result.PostHistory) == 0 {
			continue
		}
		entries := chatcontext.BuildModelOutputContextMessages(result.PostHistory, chatcontext.SourceAssistant)
		e.runtime.AppendChatHistory(entries...)
	}

	if pauseToolName, paused := pauseToolOf(results); paused {
		state.cycleEnd = cycleEndForPauseTool(pauseToolName)
		return true, nil
	}
	state.cycleEnd = CycleEnd{Reason: CycleEndToolContinue, Detail: "Planner 工具执行完成，继续下一轮内部思考。"}
	return false, nil
}

// pauseToolOf mirrors the `should_pause` / `pause_tool_name` fold of
// `_handle_tool_calls`.
func pauseToolOf(results []PlannerToolResult) (string, bool) {
	pauseToolName := ""
	for _, result := range results {
		if !result.PauseExecution {
			continue
		}
		reason := result.PauseReason
		if reason != "" {
			pauseToolName = reason
			continue
		}
		if pauseToolName == "" {
			pauseToolName = result.ToolName
		}
	}
	if pauseToolName == "" {
		return "", false
	}
	return pauseToolName, true
}

// cycleEndForPauseTool mirrors `_cycle_end_for_pause_tool`.
func cycleEndForPauseTool(pauseToolName string) CycleEnd {
	switch pauseToolName {
	case StopAfterExecutionPauseReason:
		return CycleEnd{
			Reason: CycleEndToolStopAfterExec,
			Detail: "插件工具请求在当前批次执行完成后结束 Planner，并等待新消息。",
		}
	case "wait":
		return CycleEnd{Reason: CycleEndToolPauseWait, Detail: "Planner 调用 wait，本轮暂停并在等待结束后继续判断。"}
	case "wait_rest":
		return CycleEnd{Reason: CycleEndPlannerWaitRest, Detail: "Planner 连续 wait 达到上限，本轮进入休息并等待新消息。"}
	case "":
		return CycleEnd{Reason: CycleEndToolPause, Detail: "工具要求暂停当前思考循环。"}
	default:
		return CycleEnd{
			Reason: "tool_pause:" + pauseToolName,
			Detail: fmt.Sprintf("工具 %s 要求暂停当前思考循环。", pauseToolName),
		}
	}
}

// endPlannerNoToolCycle mirrors `_end_planner_no_tool_cycle`.
func (e *Engine) endPlannerNoToolCycle() {
	e.runtime.EndPlannerContinuation()
	e.runtime.ResetConsecutiveWaitCount("planner_no_tool_end")
	e.runtime.EnterStopState()
	e.runtime.Logf("%s Planner 已结束本轮思考", e.runtime.LogPrefix())
}

// handlePlannerInterrupt mirrors `_handle_planner_interrupt`.
func (e *Engine) handlePlannerInterrupt(
	roundIndex int,
	roundText string,
	stageStartedAt float64,
	planner PlannerLLM,
) (plannerInterruptResult, error) {
	_ = roundText
	interruptedText := "Planner 收到新消息，开始重新决策"
	interruptedResponse := &PlannerResponse{
		Content:              interruptedText,
		OutputItems:          assistantOutputItems(e.activeLogicalTurnID, interruptedText),
		SelectedHistoryCount: len(e.runtime.ChatHistory()),
	}
	if !e.runtime.HasPendingMessages() || roundIndex >= e.runtime.options.maxInternalRounds() {
		return plannerInterruptResult{response: interruptedResponse}, nil
	}

	e.runtime.WaitForMessageQuietPeriod()
	e.runtime.MarkMessageTurnUnscheduled()
	interruptedMessages := e.runtime.CollectPendingMessages()
	if len(interruptedMessages) == 0 {
		return plannerInterruptResult{response: interruptedResponse}, nil
	}
	if err := e.ingestMessages(interruptedMessages); err != nil {
		return plannerInterruptResult{}, err
	}
	_ = stageStartedAt
	e.runtime.Logf("%s 保持活跃状态，直接重试 Planner: 回合=%d", e.runtime.LogPrefix(), roundIndex+2)
	return plannerInterruptResult{response: interruptedResponse, retryMessages: interruptedMessages}, nil
}

// collectPendingMessagesBeforeNextRound mirrors
// `_collect_pending_messages_before_next_round`.
func (e *Engine) collectPendingMessagesBeforeNextRound(roundIndex int) ([]*message.SessionMessage, error) {
	if roundIndex <= 0 || !e.runtime.HasPendingMessages() {
		return nil, nil
	}
	e.runtime.WaitForMessageQuietPeriod()
	pendingRoundMessages := e.runtime.CollectPendingMessages()
	if len(pendingRoundMessages) == 0 {
		return nil, nil
	}
	if err := e.ingestMessages(pendingRoundMessages); err != nil {
		return nil, err
	}
	e.runtime.Logf("%s 内部轮次开始前已合并新消息: 消息数=%d 回合=%d",
		e.runtime.LogPrefix(), len(pendingRoundMessages), roundIndex+1)
	return pendingRoundMessages, nil
}

// startCycleRound mirrors `_start_cycle_round` + `_start_cycle`.
func (e *Engine) startCycleRound(roundIndex int) (int, string) {
	e.cycleCounter++
	roundText := fmt.Sprintf("第 %d/%d 轮", roundIndex+1, e.runtime.options.maxInternalRounds())
	return e.cycleCounter, roundText
}

// finalizeCycle mirrors `_finalize_cycle` (the display/emit side effects belong
// to P4b; the runtime keeps the state transitions and reporting).
func (e *Engine) finalizeCycle(planner PlannerLLM, cycleID int, roundIndex int, state *cycleRuntimeState) {
	// `completed_cycle = await self._end_cycle(cycle_detail)` runs before the
	// max-rounds override and the monitoring snapshot in Python.
	if _, err := e.runtime.PostProcessChatHistoryAfterCycle(context.Background(), cycleInfoOf(cycleID, roundIndex, state), true); err != nil {
		e.runtime.Logf("%s 历史后处理失败: %v", e.runtime.LogPrefix(), err)
	}

	cycleEnd := state.cycleEnd
	if roundIndex+1 >= e.runtime.options.maxInternalRounds() &&
		(cycleEnd.Reason == CycleEndContinue || cycleEnd.Reason == CycleEndToolContinue) {
		cycleEnd = CycleEnd{
			Reason: CycleEndMaxRounds,
			Detail: fmt.Sprintf("已达到内部思考轮次上限 %d，本轮处理结束。", e.runtime.options.maxInternalRounds()),
		}
	}
	state.cycleEnd = cycleEnd

	plannerContent := ""
	if state.response != nil {
		plannerContent = plannerContentOf(state.response)
	}
	info := cycleInfoOf(cycleID, roundIndex, state)
	info.EndReason = cycleEnd.Reason
	info.EndDetail = cycleEnd.Detail
	planner.NotifyCycleEnd(context.Background(), info)

	if observer := e.runtime.RoundObserver(); observer != nil {
		toolCallNames := make([]string, 0)
		if state.response != nil {
			for _, call := range state.response.ToolCalls {
				toolCallNames = append(toolCallNames, call.FuncName)
			}
		}
		observer(RoundInfo{
			RoundIndex:     roundIndex,
			MaxRounds:      e.runtime.options.maxInternalRounds(),
			CycleID:        cycleID,
			PlannerContent: plannerContent,
			ToolCalls:      toolCallNames,
			Interrupted:    state.plannerInterrupted,
			EndReason:      cycleEnd.Reason,
			EndDetail:      cycleEnd.Detail,
		})
	}

	// `self._runtime._idle_backoff.record_cycle_result(cycle_end.reason)`
	e.runtime.IdleBackoff().RecordCycleResult(cycleEnd.Reason)
	e.runtime.RecordIdleCycleResult(cycleEnd.Reason)
}

func cycleInfoOf(cycleID int, roundIndex int, state *cycleRuntimeState) CycleInfo {
	info := CycleInfo{
		CycleID:     cycleID,
		RoundIndex:  roundIndex,
		Interrupted: state.plannerInterrupted,
		ToolResults: state.toolResults,
	}
	if state.response != nil {
		info.PlannerContent = state.response.Content
		info.ToolCalls = state.response.ToolCalls
		info.ModelName = state.response.ModelName
		info.PromptTokens = state.response.PromptTokens
		info.CompletionTokens = state.response.CompletionTokens
	}
	return info
}

// handleSilentTurn mirrors `_handle_silent_turn`.
func (e *Engine) handleSilentTurn(turnStart TurnStartContext) error {
	e.runtime.ClearForcedTurnState()
	if turnStart.ProactiveTriggered {
		e.runtime.consumeProactiveTriggerMessage()
		e.runtime.consumeProactiveLogicalTurnID()
	}
	// `await self._post_process_chat_history_after_cycle(cycle_detail,
	//  enable_mid_term_memory=False)` runs before `_enter_stop_state()`.
	if _, err := e.runtime.PostProcessChatHistoryAfterCycle(context.Background(), CycleInfo{
		CycleID:   e.cycleCounter,
		EndReason: "silent",
		EndDetail: "回复频率为 0，已静默接收消息",
	}, false); err != nil {
		e.runtime.Logf("%s 静默消费历史维护失败: %v", e.runtime.LogPrefix(), err)
	}
	e.runtime.EnterStopState()

	triggerLabels := make([]string, 0, 3)
	if len(turnStart.CachedMessages) > 0 {
		triggerLabels = append(triggerLabels, fmt.Sprintf("消息=%d", len(turnStart.CachedMessages)))
	}
	if turnStart.TimeoutTriggered {
		triggerLabels = append(triggerLabels, "wait_timeout")
	}
	if turnStart.ProactiveTriggered {
		triggerLabels = append(triggerLabels, "proactive")
	}
	triggerText := "无新消息"
	if len(triggerLabels) > 0 {
		triggerText = strings.Join(triggerLabels, " ")
	}
	e.runtime.Logf("%s 回复频率为 0，静默接收并完成历史维护，不进入 Planner；%s",
		e.runtime.LogPrefix(), triggerText)
	return nil
}

// buildWaitCompletedMessage mirrors `_build_wait_completed_message`.
func (e *Engine) buildWaitCompletedMessage(hasNewMessages bool) (*chatcontext.ToolResultMessage, error) {
	toolCallID, logicalTurnID, elapsedSeconds, requestedSeconds, ok := e.runtime.ConsumePendingWaitState()
	if !ok {
		return nil, fmt.Errorf("runtime: 不存在可补齐的 wait 工具调用")
	}
	requestedText := ""
	if requestedSeconds != nil {
		requestedText = fmt.Sprintf("，原计划等待 %.1f 秒", *requestedSeconds)
	}

	var content string
	if hasNewMessages {
		content = fmt.Sprintf(waitCompletedWithNewMessages,
			fmt.Sprintf("%.1f 秒", elapsedSeconds), requestedText)
	} else {
		content = fmt.Sprintf(waitCompletedNoNewMessages,
			fmt.Sprintf("%.1f 秒", elapsedSeconds), requestedText)
	}
	result, err := chatcontext.NewToolResultMessage(content, nowTime(), toolCallID, logicalTurnID, "wait", true)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ingestMessages mirrors `_ingest_messages`.
func (e *Engine) ingestMessages(messages []*message.SessionMessage) error {
	for _, sessionMessage := range messages {
		entry, err := e.BuildHistoryMessage(sessionMessage, chatcontext.SourceUser)
		if err != nil {
			return err
		}
		if entry == nil {
			continue
		}
		e.runtime.AppendChatHistory(entry)
	}
	return nil
}

// BuildHistoryMessage mirrors `_build_history_message`.
func (e *Engine) BuildHistoryMessage(sessionMessage *message.SessionMessage, sourceKind string) (chatcontext.LLMContextMessage, error) {
	visibleText := e.buildLegacyVisibleText(sessionMessage, sourceKind)
	plannerPrefix := chatcontext.BuildPlannerUserPrefixFromSessionMessage(sessionMessage, chatcontext.PlannerPrefixOptions{
		IsSelfMessage: sourceKind == chatcontext.SourceGuidedReply,
	})
	if chatcontext.ContainsComplexMessage(sessionMessage.RawMessage) {
		complexMessage, ok := chatcontext.ComplexFromSessionMessage(sessionMessage, plannerPrefix, visibleText, sourceKind)
		if !ok {
			return nil, nil
		}
		return complexMessage, nil
	}

	userSequence := chatcontext.BuildPrefixedMessageSequence(sessionMessage.RawMessage, plannerPrefix)
	if userSequence == nil || len(userSequence.Components) == 0 {
		return nil, nil
	}
	return chatcontext.SessionBackedFromSessionMessage(sessionMessage, userSequence, visibleText, sourceKind), nil
}

// buildLegacyVisibleText mirrors `_build_legacy_visible_text` for the kinds the
// runtime owns: inbound user messages and guided replies.
func (e *Engine) buildLegacyVisibleText(sessionMessage *message.SessionMessage, sourceKind string) string {
	if sourceKind == chatcontext.SourceGuidedReply {
		// 发送方消息的正文不长于普通消息，直接复用同一套渲染。
		return chatcontext.BuildSessionMessageVisibleText(sessionMessage, sessionMessage.RawMessage, false)
	}
	return chatcontext.BuildSessionMessageVisibleText(sessionMessage, sessionMessage.RawMessage, true)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func plannerContentOf(response *PlannerResponse) string {
	if response == nil {
		return ""
	}
	return response.Content
}


// assistantOutputItems mirrors the portable assistant item built for an
// interrupted request (`ChatResponse(output_items=(...))`).
func assistantOutputItems(logicalTurnID string, text string) []*llm.ContextItem {
	item, err := llm.NewContextItemBuilder().
		SetRole(llm.RoleAssistant).
		SetMeta(llm.ContextItemMeta{
			ItemID:        chatcontext.NewContextItemID(),
			LogicalTurnID: logicalTurnID,
			Timestamp:     nowTime(),
		}).
		AddTextPart(text).
		Build()
	if err != nil {
		return nil
	}
	return []*llm.ContextItem{item}
}
