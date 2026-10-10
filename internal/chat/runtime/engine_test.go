package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/chat/trigger"
	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/message"
)

// waitFor polls cond until it returns true or the deadline expires.
// 截止时间给足余量：全量套件并行跑时（本机还有其他构建负载），goroutine
// 调度延迟可能远超 3s，过短的等待会间歇误报。
func waitFor(t *testing.T, label string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", label)
}

// ---------------------------------------------------------------------------
// engine: internal rounds
// ---------------------------------------------------------------------------

// TestEngineRunsInternalRoundsUpToMax pins the `while round_index <
// self._runtime._max_internal_rounds` loop: tool rounds continue until the
// limit, which then closes with `max_rounds`.
func TestEngineRunsInternalRoundsUpToMax(t *testing.T) {
	planner := newStubPlanner()
	// Every round asks for another tool call → the loop must stop on the limit.
	for round := 0; round < 40; round++ {
		planner.responses = append(planner.responses, functionCallResponse("reply", fmt.Sprintf("call-%d", round)))
	}
	runtime := newTestRuntime(Options{IsGroupSession: true, MaxInternalRounds: 3, MaxContextSize: 40})
	planner.toolResults = map[int][]PlannerToolResult{}
	for round := 0; round < 5; round++ {
		planner.toolResults[round] = []PlannerToolResult{{
			CallID: fmt.Sprintf("call-%d", round), ToolName: "reply", Content: "ok", Success: true,
		}}
	}
	runtime.SetPlanner(planner)
	runtime.Start(nil)
	defer runtime.Stop()

	runtime.RegisterMessage(runtimeMessage("m1", time.Now()))
	waitFor(t, "three rounds", func() bool { return planner.requestCount() >= 3 })

	// No 4th round may run.
	time.Sleep(50 * time.Millisecond)
	if got := planner.requestCount(); got != 3 {
		t.Fatalf("planner rounds = %d, want exactly MaxInternalRounds=3", got)
	}
	ends := planner.cycleEnds()
	if len(ends) != 3 {
		t.Fatalf("cycle ends = %d, want 3", len(ends))
	}
	if ends[len(ends)-1].EndReason != CycleEndMaxRounds {
		t.Fatalf("last end reason = %q, want %q", ends[len(ends)-1].EndReason, CycleEndMaxRounds)
	}
	if ends[len(ends)-1].EndDetail != "已达到内部思考轮次上限 3，本轮处理结束。" {
		t.Fatalf("end detail = %q", ends[len(ends)-1].EndDetail)
	}
	// Every round keeps the runtime running, and the cycle end returns to stop.
	if state := runtime.AgentState(); state != trigger.StateStop {
		t.Fatalf("state after the cycle = %q, want stop", state)
	}
}

// TestEngineNoToolEndsCycle pins `_handle_planner_no_tool_retry`: a response
// without tool calls terminates the loop with `planner_no_tool_end`.
func TestEngineNoToolEndsCycle(t *testing.T) {
	planner := newStubPlanner(&PlannerResponse{Content: "我只是在思考", OutputItems: assistantItems("我只是在思考")})
	runtime := newTestRuntime(Options{IsGroupSession: true, MaxInternalRounds: 5})
	runtime.SetPlanner(planner)
	runtime.Start(nil)
	defer runtime.Stop()

	runtime.RegisterMessage(runtimeMessage("m1", time.Now()))
	waitFor(t, "one round", func() bool { return planner.requestCount() >= 1 })
	waitFor(t, "cycle end", func() bool { return len(planner.cycleEnds()) >= 1 })

	ends := planner.cycleEnds()
	if ends[0].EndReason != CycleEndPlannerNoToolEnd {
		t.Fatalf("end reason = %q, want %q", ends[0].EndReason, CycleEndPlannerNoToolEnd)
	}
	if ends[0].EndDetail != "Planner本轮思考结束。" {
		t.Fatalf("end detail = %q", ends[0].EndDetail)
	}
	time.Sleep(30 * time.Millisecond)
	if got := planner.requestCount(); got != 1 {
		t.Fatalf("planner rounds = %d, want 1 (no-tool output ends the cycle)", got)
	}
	if runtime.AgentState() != trigger.StateStop {
		t.Fatalf("state = %q, want stop", runtime.AgentState())
	}
	if runtime.IsPlannerContinuationActive() {
		t.Fatalf("planner continuation must be ended")
	}
}

// TestEnginePauseToolEndsCycle pins `_cycle_end_for_pause_tool`.
func TestEnginePauseToolEndsCycle(t *testing.T) {
	cases := []struct {
		pauseReason string
		toolName    string
		wantReason  string
	}{
		{"wait", "wait", CycleEndToolPauseWait},
		{"wait_rest", "wait", CycleEndPlannerWaitRest},
		{StopAfterExecutionPauseReason, "some_tool", CycleEndToolStopAfterExec},
		{"other_tool", "other_tool", "tool_pause:other_tool"},
	}
	for _, testCase := range cases {
		t.Run(testCase.wantReason, func(t *testing.T) {
			planner := newStubPlanner(functionCallResponse(testCase.toolName, "call-1"))
			planner.toolResults[0] = []PlannerToolResult{{
				CallID: "call-1", ToolName: testCase.toolName, Content: "paused", Success: true,
				PauseExecution: true, PauseReason: testCase.pauseReason,
			}}
			runtime := newTestRuntime(Options{IsGroupSession: true, MaxInternalRounds: 5})
			runtime.SetPlanner(planner)
			runtime.Start(nil)
			defer runtime.Stop()

			runtime.RegisterMessage(runtimeMessage("m1", time.Now()))
			waitFor(t, "cycle end", func() bool { return len(planner.cycleEnds()) >= 1 })
			ends := planner.cycleEnds()
			if ends[0].EndReason != testCase.wantReason {
				t.Fatalf("end reason = %q, want %q", ends[0].EndReason, testCase.wantReason)
			}
			time.Sleep(30 * time.Millisecond)
			if got := planner.requestCount(); got != 1 {
				t.Fatalf("planner rounds = %d, want 1 (pause ends the cycle)", got)
			}
		})
	}

	// The pause reason mapping itself is pinned directly too.
	ends := map[string]string{
		StopAfterExecutionPauseReason: CycleEndToolStopAfterExec,
		"wait":                        CycleEndToolPauseWait,
		"wait_rest":                   CycleEndPlannerWaitRest,
		"":                            CycleEndToolPause,
		"plugin_pause":                "tool_pause:plugin_pause",
	}
	for pauseTool, wantReason := range ends {
		if got := cycleEndForPauseTool(pauseTool); got.Reason != wantReason {
			t.Errorf("cycleEndForPauseTool(%q) = %q, want %q", pauseTool, got.Reason, wantReason)
		}
	}
}

// TestEngineCollectsNewMessagesBetweenRounds pins
// `_collect_pending_messages_before_next_round`: messages arriving during a tool
// round are merged into the next round.
func TestEngineCollectsNewMessagesBetweenRounds(t *testing.T) {
	planner := newStubPlanner(functionCallResponse("query_memory", "call-1"), &PlannerResponse{
		Content: "结束", OutputItems: assistantItems("结束"),
	})
	runtime := newTestRuntime(Options{IsGroupSession: true, MaxInternalRounds: 5})
	planner.toolResults[0] = []PlannerToolResult{{
		CallID: "call-1", ToolName: "query_memory", Content: "ok", Success: true,
	}}
	// The tool execution registers a new message, exactly like a user typing
	// while the planner is working.
	plannerSink := runtime
	planner.toolResults[0][0].Metadata = map[string]any{}
	planner.beforeExecute = func() {
		plannerSink.RegisterMessage(runtimeMessage("m-during-round", time.Now()))
	}
	runtime.SetPlanner(planner)
	runtime.Start(nil)
	defer runtime.Stop()

	runtime.RegisterMessage(runtimeMessage("m1", time.Now()))
	waitFor(t, "two rounds", func() bool { return planner.requestCount() >= 2 })

	history := runtime.ChatHistory()
	found := false
	for _, entry := range history {
		if strings.Contains(entry.ProcessedPlainText(), "m-during-round") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the new message must be merged into the history before round 2; history=%v", dumpHistory(history))
	}
	second := planner.requests[1]
	if len(second.History) <= len(planner.requests[0].History) && !historyContains(second.History, "m-during-round") {
		t.Fatalf("round 2 request must include the merged message")
	}
}

// TestEngineSilentBranchConsumesWithoutPlanner pins `_handle_silent_turn`: with a
// reply frequency of 0 the batch is consumed, the history maintained and the
// Planner never called.
func TestEngineSilentBranchConsumesWithoutPlanner(t *testing.T) {
	planner := newStubPlanner()
	runtime := NewRuntime(Options{
		SessionID:          "session",
		IsGroupSession:     true,
		MaxContextSize:     40,
		BaseReplyFrequency: 0,
		Log:                func(string, ...any) {},
	})
	runtime.SetPlanner(planner)
	runtime.Start(nil)
	defer runtime.Stop()

	runtime.RegisterMessage(runtimeMessage("m1", time.Now()))
	// The silent branch consumes the batch and folds it into the history without
	// ever calling the planner.
	waitFor(t, "silent consumption", func() bool { return runtime.PendingMessageCount() == 0 })
	time.Sleep(30 * time.Millisecond)
	if got := planner.requestCount(); got != 0 {
		t.Fatalf("planner rounds = %d, want 0 in the silent branch", got)
	}
	if got := runtime.PendingMessageCount(); got != 0 {
		t.Fatalf("pending messages = %d, want 0 after the silent consumption", got)
	}
	if history := runtime.ChatHistory(); len(history) != 1 {
		t.Fatalf("history = %d entries, want the consumed message", len(history))
	}
	if runtime.AgentState() != trigger.StateStop {
		t.Fatalf("state = %q, want stop", runtime.AgentState())
	}
	if runtime.ConsecutiveWaitCount() != 0 {
		t.Fatalf("consecutive wait count = %d, want 0", runtime.ConsecutiveWaitCount())
	}
}

// TestEngineWaitStateIgnoresMessageTrigger pins the guard that a group-chat wait
// ignores message triggers until the timeout/proactive trigger arrives.
func TestEngineWaitStateIgnoresMessageTrigger(t *testing.T) {
	planner := newStubPlanner(&PlannerResponse{Content: "思考", OutputItems: assistantItems("思考")})
	runtime := newTestRuntime(Options{IsGroupSession: true})
	runtime.SetPlanner(planner)
	runtime.Start(nil)
	defer runtime.Stop()

	seconds := 60.0
	if entered, _, _ := runtime.TryEnterWaitState(&seconds, "call-1", "turn-1"); !entered {
		t.Fatalf("wait not entered")
	}
	// A group-chat message arriving while waiting is cached but must not wake the
	// Planner (`if runtime.chat_stream.is_group_session: return`).
	runtime.RegisterMessage(runtimeMessage("m-waiting", time.Now()))
	if runtime.MessageTurnScheduled() {
		t.Fatalf("group chat must not schedule a turn while waiting")
	}
	if runtime.AgentState() != trigger.StateWait {
		t.Fatalf("state = %q, want wait", runtime.AgentState())
	}
	// A queued message trigger while waiting must be drained and ignored.
	runtime.EnqueueMessageTurn()
	time.Sleep(50 * time.Millisecond)
	if got := planner.requestCount(); got != 0 {
		t.Fatalf("planner rounds = %d, want 0 while waiting", got)
	}
	if !runtime.HasPendingMessages() {
		t.Fatalf("the ignored message must stay pending")
	}

	// The timeout wakes the loop; the pending wait yields the wait-completed
	// tool result that is appended before the round runs.
	if !runtime.HandleWaitTimeout("call-1") {
		t.Fatalf("timeout not handled")
	}
	waitFor(t, "planner round after the timeout", func() bool { return planner.requestCount() >= 1 })
	if runtime.HasPendingWaitToolCall() {
		t.Fatalf("the wait state must be consumed when the wait result is built")
	}
	// The message registered while waiting counts as a new input, so the wait
	// result reports the "等待已结束" wording (mirrors `has_new_messages`).
	foundWaitResult := false
	for _, entry := range planner.requests[0].History {
		if result, ok := entry.(*chatcontext.ToolResultMessage); ok && result.ToolName == "wait" {
			foundWaitResult = true
			if !strings.Contains(result.Content, "等待已结束，实际等待") ||
				!strings.Contains(result.Content, "原计划等待 60.0 秒") {
				t.Fatalf("wait result = %q", result.Content)
			}
		}
	}
	if !foundWaitResult {
		t.Fatalf("the wait-completed tool result must be in the round-1 history: %v", dumpHistory(planner.requests[0].History))
	}
}

// blockingPlanner blocks the first request until the bound interrupt flag fires
// (or the timeout expires) and then reports ReqAbortException, mirroring the
// streaming planner path.
type blockingPlanner struct {
	*stubPlanner
	interrupted int
	// flagObserved records whether PlannerRequest.Interrupted was already closed
	// when the context cancellation was observed (the Go stand-in for
	// `asyncio.Event.set()`).
	flagObserved bool
}

func (b *blockingPlanner) ChatLoopStep(ctx context.Context, request PlannerRequest) (*PlannerResponse, error) {
	b.mu.Lock()
	round := len(b.requests)
	b.requests = append(b.requests, request)
	b.mu.Unlock()
	if round == 0 {
		select {
		case <-ctx.Done():
			flagSet := false
			select {
			case <-request.Interrupted:
				flagSet = true
			default:
			}
			b.mu.Lock()
			b.interrupted++
			b.flagObserved = flagSet
			b.mu.Unlock()
			return nil, &InterruptedError{Reason: "收到新消息"}
		case <-time.After(2 * time.Second):
			return nil, fmt.Errorf("planner was never interrupted")
		}
	}
	return &PlannerResponse{Content: "重跑完成", OutputItems: assistantItems("重跑完成")}, nil
}

// TestEngineInterruptRerunsWithNewMessages pins the `except ReqAbortException`
// branch: when `planner_interrupt_max_consecutive_count > 0` a new message
// cancels the running request (via context) and the round re-runs with the new
// message merged in.
func TestEngineInterruptRerunsWithNewMessages(t *testing.T) {
	base := newStubPlanner()
	planner := &blockingPlanner{stubPlanner: base}
	runtime := newTestRuntime(Options{
		IsGroupSession:                      true,
		MaxInternalRounds:                   5,
		PlannerInterruptMaxConsecutiveCount: 3,
	})
	runtime.SetPlanner(planner)
	runtime.Start(nil)
	defer runtime.Stop()

	runtime.RegisterMessage(runtimeMessage("m1", time.Now()))
	// Wait until the first request is in flight, then send the interrupting
	// message.
	waitFor(t, "first request", func() bool {
		base.mu.Lock()
		defer base.mu.Unlock()
		return len(base.requests) >= 1
	})
	runtime.RegisterMessage(runtimeMessage("m2-interrupt", time.Now()))

	waitFor(t, "rerun", func() bool {
		base.mu.Lock()
		defer base.mu.Unlock()
		return len(base.requests) >= 2
	})
	base.mu.Lock()
	requests := append([]PlannerRequest{}, base.requests...)
	interrupted := planner.interrupted
	flagObserved := planner.flagObserved
	base.mu.Unlock()
	if interrupted != 1 {
		t.Fatalf("interrupted requests = %d, want 1", interrupted)
	}
	if !flagObserved {
		t.Fatalf("the interrupt flag must be set before the context cancellation")
	}
	if !historyContains(requests[1].History, "m2-interrupt") {
		t.Fatalf("the retry must contain the interrupting message: %v", dumpHistory(requests[1].History))
	}
	// The successful retry is a non-interrupted unbind, which resets the
	// consecutive count exactly like `unbind(interrupted=False)`.
	if runtime.interrupt.ConsecutiveCount() != 0 {
		t.Fatalf("consecutive interrupt count after the retry = %d, want 0", runtime.interrupt.ConsecutiveCount())
	}
	// The cycle eventually ends with the no-tool response.
	waitFor(t, "cycle end", func() bool {
		base.mu.Lock()
		defer base.mu.Unlock()
		return len(base.cycles) >= 1
	})
}

// TestBuildWaitCompletedMessage pins `_build_wait_completed_message` texts.
func TestBuildWaitCompletedMessage(t *testing.T) {
	seconds := 30.0
	runtime := newTestRuntime(Options{})
	if entered, _, _ := runtime.TryEnterWaitState(&seconds, "call-1", "turn-1"); !entered {
		t.Fatalf("wait not entered")
	}
	entry, err := runtime.Engine().buildWaitCompletedMessage(true)
	if err != nil {
		t.Fatalf("buildWaitCompletedMessage: %v", err)
	}
	if entry.ToolCallID != "call-1" || entry.LogicalTurnID() != "turn-1" || entry.ToolName != "wait" {
		t.Fatalf("entry = %+v", entry)
	}
	if !strings.Contains(entry.Content, "等待已结束，实际等待") ||
		!strings.Contains(entry.Content, "原计划等待 30.0 秒") ||
		!strings.Contains(entry.Content, "期间收到了新的用户输入") {
		t.Fatalf("content = %q", entry.Content)
	}

	seconds2 := 5.0
	if entered, _, _ := runtime.TryEnterWaitState(&seconds2, "call-2", "turn-2"); !entered {
		t.Fatalf("wait not entered")
	}
	entry, err = runtime.Engine().buildWaitCompletedMessage(false)
	if err != nil {
		t.Fatalf("buildWaitCompletedMessage: %v", err)
	}
	if !strings.Contains(entry.Content, "等待已超时，实际等待") ||
		!strings.Contains(entry.Content, "期间没有收到新的用户输入") {
		t.Fatalf("content = %q", entry.Content)
	}

	// Without a pending wait the Python code raises a RuntimeError.
	empty := newTestRuntime(Options{})
	if _, err := empty.Engine().buildWaitCompletedMessage(true); err == nil {
		t.Fatalf("missing wait state must return an error")
	}
}

// TestDrainReadyTurnTriggers pins `_drain_ready_turn_triggers`.
func TestDrainReadyTurnTriggers(t *testing.T) {
	runtime := newTestRuntime(Options{})
	runtime.mu.Lock()
	runtime.internalTurnQueue = []string{TriggerTimeout, TriggerProactive, TriggerMessage}
	runtime.mu.Unlock()

	messageTriggered, timeoutTriggered, proactiveTriggered := runtime.DrainReadyTurnTriggers(TriggerMessage)
	if !messageTriggered || !timeoutTriggered || !proactiveTriggered {
		t.Fatalf("drain = %v/%v/%v, want all true", messageTriggered, timeoutTriggered, proactiveTriggered)
	}
	if runtime.PendingMessageCount() != 0 || len(runtime.internalTurnQueue) != 0 {
		t.Fatalf("queue not drained")
	}
	messageTriggered, timeoutTriggered, proactiveTriggered = runtime.DrainReadyTurnTriggers(TriggerProactive)
	if messageTriggered || timeoutTriggered || !proactiveTriggered {
		t.Fatalf("single drain = %v/%v/%v", messageTriggered, timeoutTriggered, proactiveTriggered)
	}
}

// TestSandboxStyleDirectRound drives the engine half manually to pin the
// response-actions behaviour without the background loop.
func TestReasoningEngineResponseActions(t *testing.T) {
	planner := newStubPlanner()
	runtime := newTestRuntime(Options{MaxContextSize: 40})
	runtime.SetPlanner(planner)
	engine := runtime.Engine()

	// A tool-call response extends the history with the model output items and
	// returns shouldBreak=false.
	response := functionCallResponse("query_memory", "call-1")
	state := &cycleRuntimeState{response: response}
	shouldBreak, err := engine.handlePlannerResponseActions(context.Background(), planner, response, 1, state)
	if err != nil {
		t.Fatalf("handlePlannerResponseActions: %v", err)
	}
	if shouldBreak {
		t.Fatalf("a tool round must continue the loop")
	}
	if state.cycleEnd.Reason != CycleEndToolContinue {
		t.Fatalf("cycle end = %q, want %q", state.cycleEnd.Reason, CycleEndToolContinue)
	}
	if len(runtime.ChatHistory()) != 1 {
		t.Fatalf("history = %d, want 1 model output item", len(runtime.ChatHistory()))
	}
	if engine.LastReasoningContent() != "调用 query_memory" {
		t.Fatalf("last reasoning = %q", engine.LastReasoningContent())
	}

	// A no-tool response ends the cycle.
	noTool := &PlannerResponse{Content: "结束", OutputItems: assistantItems("结束")}
	state = &cycleRuntimeState{response: noTool}
	shouldBreak, err = engine.handlePlannerResponseActions(context.Background(), planner, noTool, 2, state)
	if err != nil {
		t.Fatalf("handlePlannerResponseActions: %v", err)
	}
	if !shouldBreak {
		t.Fatalf("a no-tool round must end the cycle")
	}
	if state.cycleEnd.Reason != CycleEndPlannerNoToolEnd {
		t.Fatalf("cycle end = %q, want %q", state.cycleEnd.Reason, CycleEndPlannerNoToolEnd)
	}
	if runtime.AgentState() != trigger.StateStop {
		t.Fatalf("state = %q, want stop", runtime.AgentState())
	}
}

// TestEngineInterruptDoesNotAdvanceRoundIndex pins the `finally:
// if not state.planner_interrupted: round_index += 1` rule: an interrupt retries
// the *same* round slot, so the max-rounds budget is not consumed by aborts.
func TestEngineInterruptDoesNotAdvanceRoundIndex(t *testing.T) {
	planner := &alwaysInterruptedPlanner{stubPlanner: newStubPlanner()}
	runtime := newTestRuntime(Options{
		IsGroupSession:                      true,
		MaxInternalRounds:                   2,
		PlannerInterruptMaxConsecutiveCount: 10,
	})
	runtime.SetPlanner(planner)
	runtime.Start(nil)
	defer runtime.Stop()

	// Register a new message on every request so there is always something to
	// retry with (mirrors `_has_pending_messages()` being true).
	runtime.RegisterMessage(runtimeMessage("m0", time.Now()))
	planner.onRequest = func() {
		runtime.RegisterMessage(runtimeMessage(fmt.Sprintf("m-%d", planner.requestCount()), time.Now()))
	}

	waitFor(t, "three interrupted requests", func() bool { return planner.requestCount() >= 3 })
	planner.mu2.Lock()
	requests := planner.interrupted
	planner.mu2.Unlock()
	base := planner.stubPlanner
	base.mu.Lock()
	cycles := append([]CycleInfo{}, base.cycles...)
	base.mu.Unlock()

	if requests < 3 {
		t.Fatalf("interrupted requests = %d, want >= 3", requests)
	}
	// Every finalize must report the same round index (0) — the interrupt never
	// advances it — and MaxInternalRounds limits the loop.
	for _, cycle := range cycles {
		if cycle.RoundIndex != 0 {
			t.Fatalf("finalized round index = %d, want 0 for interrupted rounds", cycle.RoundIndex)
		}
		if cycle.EndReason != CycleEndPlannerInterrupted {
			t.Fatalf("end reason = %q, want %q", cycle.EndReason, CycleEndPlannerInterrupted)
		}
	}
	if len(cycles) > 2 {
		t.Fatalf("finalized cycles = %d, want at most MaxInternalRounds=2", len(cycles))
	}
}

// alwaysInterruptedPlanner aborts every request with ReqAbortException.
type alwaysInterruptedPlanner struct {
	*stubPlanner
	interrupted int
	onRequest   func()
	mu2         sync.Mutex
}

func (p *alwaysInterruptedPlanner) ChatLoopStep(_ context.Context, request PlannerRequest) (*PlannerResponse, error) {
	p.stubPlanner.mu.Lock()
	p.stubPlanner.requests = append(p.stubPlanner.requests, request)
	p.stubPlanner.mu.Unlock()
	p.mu2.Lock()
	p.interrupted++
	p.mu2.Unlock()
	if p.onRequest != nil {
		p.onRequest()
	}
	return nil, &InterruptedError{Reason: "持续打断"}
}

// TestPostProcessFoldsRemovedHistoryIntoRecall pins
// `_post_process_chat_history_after_cycle` + `insert_mid_term_memory_message`:
// the trimmed prefix is compressed into a single chat-recall entry.
func TestPostProcessFoldsRemovedHistoryIntoRecall(t *testing.T) {
	summarizer := &stubSummarizer{
		ok: true,
		summary: &chatcontext.MidTermMemorySummaryPayload{
			TimeRange:    "2025-10-09 12:00:00 ~ 2025-10-09 12:05:00",
			Participants: []string{"小明"},
			Summary:      "聊了天气",
		},
	}
	runtime := newTestRuntime(Options{
		IsGroupSession:      true,
		MaxContextSize:      2,
		MidTermMemory:       true,
		MidTermMemoryLength: 10,
		Summarizer:          summarizer,
	})
	history := make([]chatcontext.LLMContextMessage, 0, 12)
	for index := 0; index < 12; index++ {
		history = append(history, chatcontext.NewSessionBackedMessage(
			message.NewMessageSequence(message.NewTextComponent(fmt.Sprintf("m%d", index))),
			fmt.Sprintf("m%d", index), time.Now(), fmt.Sprintf("id%d", index), nil, chatcontext.SourceUser,
		))
	}
	runtime.SetChatHistory(history)

	result, err := runtime.PostProcessChatHistoryAfterCycle(context.Background(), CycleInfo{CycleID: 1}, true)
	if err != nil {
		t.Fatalf("PostProcessChatHistoryAfterCycle: %v", err)
	}
	if summarizer.calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1", summarizer.calls)
	}
	if summarizer.seen == 0 || len(result.RemovedMessages) != summarizer.seen {
		t.Fatalf("summarizer saw %d of %d removed messages", summarizer.seen, len(result.RemovedMessages))
	}
	recalls := 0
	for _, entry := range runtime.ChatHistory() {
		if chatcontext.IsMidTermMemoryMessage(entry) {
			recalls++
		}
	}
	if recalls != 1 {
		t.Fatalf("recalls = %d, want 1 (折叠为单条 mid_term_memory)", recalls)
	}
	if result.RemainingContextCount > 2 {
		t.Fatalf("remaining context = %d, want <= 2", result.RemainingContextCount)
	}
}

// TestPostProcessMidTermMemoryDisabled pins
// `enable_mid_term_memory=False` (the silent branch) and
// `chat.mid_term_memory = false`.
func TestPostProcessMidTermMemoryDisabled(t *testing.T) {
	summarizer := &stubSummarizer{ok: true, summary: &chatcontext.MidTermMemorySummaryPayload{Summary: "x"}}
	runtime := newTestRuntime(Options{
		IsGroupSession: true, MaxContextSize: 2, MidTermMemory: true, Summarizer: summarizer,
	})
	history := make([]chatcontext.LLMContextMessage, 0, 12)
	for index := 0; index < 12; index++ {
		history = append(history, chatcontext.NewSessionBackedMessage(
			message.NewMessageSequence(message.NewTextComponent(fmt.Sprintf("m%d", index))),
			fmt.Sprintf("m%d", index), time.Now(), fmt.Sprintf("id%d", index), nil, chatcontext.SourceUser,
		))
	}
	runtime.SetChatHistory(history)
	// enableMidTermMemory=false skips the compression while the trimming runs.
	if _, err := runtime.PostProcessChatHistoryAfterCycle(context.Background(), CycleInfo{}, false); err != nil {
		t.Fatalf("PostProcessChatHistoryAfterCycle: %v", err)
	}
	if summarizer.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0 when mid-term memory is disabled", summarizer.calls)
	}

	// The config switch also disables it.
	configOff := newTestRuntime(Options{
		IsGroupSession: true, MaxContextSize: 2, MidTermMemory: false, Summarizer: summarizer,
	})
	configOff.SetChatHistory(history)
	if _, err := configOff.PostProcessChatHistoryAfterCycle(context.Background(), CycleInfo{}, true); err != nil {
		t.Fatalf("PostProcessChatHistoryAfterCycle: %v", err)
	}
	if summarizer.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0 with mid_term_memory = false", summarizer.calls)
	}

	// Without a summarizer the trimming still happens.
	noSummarizer := newTestRuntime(Options{IsGroupSession: true, MaxContextSize: 2, MidTermMemory: true})
	noSummarizer.SetChatHistory(history)
	result, err := noSummarizer.PostProcessChatHistoryAfterCycle(context.Background(), CycleInfo{}, true)
	if err != nil {
		t.Fatalf("PostProcessChatHistoryAfterCycle: %v", err)
	}
	if result.RemainingContextCount > 2 {
		t.Fatalf("remaining = %d, want <= 2", result.RemainingContextCount)
	}
}

// TestAssistantOutputItems pins the portable interrupted-response item.
func TestAssistantOutputItems(t *testing.T) {
	items := assistantOutputItems("turn-1", "被新消息打断")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].Meta.LogicalTurnID != "turn-1" || items[0].Text() != "被新消息打断" {
		t.Fatalf("item = %+v", items[0])
	}
	if items[0].Role() != llm.RoleAssistant {
		t.Fatalf("role = %q", items[0].Role())
	}
}

// TestBuildHistoryMessagePaths pins `_build_history_message`.
func TestBuildHistoryMessagePaths(t *testing.T) {
	runtime := newTestRuntime(Options{MaxContextSize: 40})
	engine := runtime.Engine()

	sessionMessage := runtimeMessage("m1", time.Now())
	entry, err := engine.BuildHistoryMessage(sessionMessage, chatcontext.SourceUser)
	if err != nil {
		t.Fatalf("BuildHistoryMessage: %v", err)
	}
	sessionBacked, ok := entry.(*chatcontext.SessionBackedMessage)
	if !ok {
		t.Fatalf("entry = %T, want *SessionBackedMessage", entry)
	}
	if sessionBacked.MsgID != "m1" || sessionBacked.SourceKind != chatcontext.SourceUser {
		t.Fatalf("entry = %+v", sessionBacked)
	}
	prefix := sessionBacked.RawMessage.Components[0].(*message.TextComponent).Text
	if !strings.HasPrefix(prefix, "<message ") || !strings.Contains(prefix, "msg_id=\"m1\"") {
		t.Fatalf("raw prefix = %q", prefix)
	}
	if sessionBacked.ProcessedPlainText() == "" {
		t.Fatalf("visible text is empty")
	}

	// Guided replies are flagged as self messages.
	guided, err := engine.BuildHistoryMessage(runtimeMessage("m2", time.Now()), chatcontext.SourceGuidedReply)
	if err != nil {
		t.Fatalf("BuildHistoryMessage: %v", err)
	}
	if !strings.Contains(guided.(*chatcontext.SessionBackedMessage).RawMessage.Components[0].(*message.TextComponent).Text,
		`is_self_message="true"`) {
		t.Fatalf("guided reply prefix = %q", guided.ProcessedPlainText())
	}

	// A complex (forward) message becomes a ComplexSessionMessage.
	forwardNode, err := message.NewForwardNodeComponent([]*message.ForwardComponent{{
		UserNickname: "甲", MessageID: "f1",
		Content: []message.Component{message.NewTextComponent("转发正文")},
	}})
	if err != nil {
		t.Fatalf("NewForwardNodeComponent: %v", err)
	}
	complexMessage := runtimeMessage("m3", time.Now())
	complexMessage.RawMessage = message.NewMessageSequence(message.NewTextComponent("看"), forwardNode)
	entry, err = engine.BuildHistoryMessage(complexMessage, chatcontext.SourceUser)
	if err != nil {
		t.Fatalf("BuildHistoryMessage: %v", err)
	}
	complexEntry, ok := entry.(*chatcontext.ComplexSessionMessage)
	if !ok {
		t.Fatalf("entry = %T, want *ComplexSessionMessage", entry)
	}
	if complexEntry.ComplexMessageType != "forward" {
		t.Fatalf("complex type = %q", complexEntry.ComplexMessageType)
	}
}

func dumpHistory(history []chatcontext.LLMContextMessage) []string {
	out := make([]string, 0, len(history))
	for _, entry := range history {
		out = append(out, fmt.Sprintf("%T:%s", entry, entry.ProcessedPlainText()))
	}
	return out
}

func historyContains(history []chatcontext.LLMContextMessage, needle string) bool {
	for _, entry := range history {
		if strings.Contains(entry.ProcessedPlainText(), needle) {
			return true
		}
	}
	return false
}

// blockingToolPlanner 在工具执行阶段阻塞在 ctx 上，用于验证取消语义能传导
// 到 ExecuteToolCalls（T5）。
type blockingToolPlanner struct {
	*stubPlanner
	mu        sync.Mutex
	inFlight  bool
	cancelled bool
}

func (b *blockingToolPlanner) ExecuteToolCalls(ctx context.Context, calls []PlannerToolCall, _ string) ([]PlannerToolResult, error) {
	b.mu.Lock()
	b.inFlight = true
	b.cancelled = ctx.Err() != nil
	b.mu.Unlock()
	select {
	case <-ctx.Done():
		b.mu.Lock()
		b.cancelled = true
		b.mu.Unlock()
		// 工具实现把取消折算为失败回执（对齐 reply 工具 ctx.Done 分支）。
		results := make([]PlannerToolResult, 0, len(calls))
		for _, call := range calls {
			results = append(results, PlannerToolResult{
				CallID: call.CallID, ToolName: call.FuncName, Content: "已取消", Success: false,
			})
		}
		return results, nil
	case <-time.After(3 * time.Second):
		return nil, fmt.Errorf("tool execution was never cancelled")
	}
}

// TestEngineStopCancelsToolExecution 验证 T5：runtime.Stop() 会取消进行中的
// ExecuteToolCalls（此前用 context.Background()，工具执行完全不可取消）。
func TestEngineStopCancelsToolExecution(t *testing.T) {
	planner := &blockingToolPlanner{stubPlanner: newStubPlanner(functionCallResponse("reply", "call-1"))}
	runtime := newTestRuntime(Options{IsGroupSession: true, MaxInternalRounds: 5})
	runtime.SetPlanner(planner)
	runtime.Start(nil)
	defer runtime.Stop()

	runtime.RegisterMessage(runtimeMessage("m1", time.Now()))
	// 等工具执行进入阻塞，然后 Stop → ctx 取消 → ExecuteToolCalls 返回。
	waitFor(t, "tool execution in flight", func() bool {
		planner.mu.Lock()
		defer planner.mu.Unlock()
		return planner.inFlight
	})
	runtime.Stop()
	waitFor(t, "tool execution observed cancellation", func() bool {
		planner.mu.Lock()
		defer planner.mu.Unlock()
		return planner.cancelled
	})
}
