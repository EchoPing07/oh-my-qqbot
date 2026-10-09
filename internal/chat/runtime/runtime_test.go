package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/chat/trigger"
	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/message"
)

// ---------------------------------------------------------------------------
// test doubles
// ---------------------------------------------------------------------------

// stubPlanner records every planner interaction so the engine loop can be
// asserted without any prompt assembly (P4b).
type stubPlanner struct {
	mu sync.Mutex

	// responses is consumed round by round; the last one repeats.
	responses []*PlannerResponse
	// errors, when set for a round index, is returned instead of a response.
	errors map[int]error

	requests  []PlannerRequest
	executed  [][]PlannerToolCall
	injected  [][]string
	cycles    []CycleInfo
	posted    []CycleInfo
	restores  int
	refreshes int

	// toolResults is returned by ExecuteToolCalls per round index.
	toolResults map[int][]PlannerToolResult
	// beforeExecute runs before the round is recorded, so tests can inject a
	// message that arrives while the tools are running.
	beforeExecute func()
}

func newStubPlanner(responses ...*PlannerResponse) *stubPlanner {
	return &stubPlanner{
		responses:   responses,
		errors:      map[int]error{},
		toolResults: map[int][]PlannerToolResult{},
	}
}

func (s *stubPlanner) ChatLoopStep(_ context.Context, request PlannerRequest) (*PlannerResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	round := len(s.requests)
	s.requests = append(s.requests, request)
	if err := s.errors[round]; err != nil {
		return nil, err
	}
	if len(s.responses) == 0 {
		return &PlannerResponse{Content: "空响应"}, nil
	}
	index := round
	if index >= len(s.responses) {
		index = len(s.responses) - 1
	}
	response := s.responses[index]
	if response != nil && len(response.OutputItems) == 0 && response.Content != "" {
		response.OutputItems = assistantItems(response.Content, "")
	}
	return response, nil
}

func (s *stubPlanner) ExecuteToolCalls(_ context.Context, calls []PlannerToolCall, _ string) ([]PlannerToolResult, error) {
	if s.beforeExecute != nil {
		s.beforeExecute()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	round := len(s.executed)
	s.executed = append(s.executed, calls)
	if results, ok := s.toolResults[round]; ok {
		return results, nil
	}
	results := make([]PlannerToolResult, 0, len(calls))
	for _, call := range calls {
		results = append(results, PlannerToolResult{
			CallID: call.CallID, ToolName: call.FuncName, Content: "ok", Success: true,
		})
	}
	return results, nil
}

func (s *stubPlanner) BuildActionToolDefinitions(context.Context) ([]PlannerToolDefinition, string, error) {
	return []PlannerToolDefinition{{Name: "reply"}}, "", nil
}

func (s *stubPlanner) BuildInjectedUserMessages(_ context.Context, reminder string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	injected := []string{}
	if reminder != "" {
		injected = append(injected, reminder)
	}
	s.injected = append(s.injected, injected)
	return injected, nil
}

func (s *stubPlanner) RefreshMidTermMemoryReference(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshes++
	return nil
}

func (s *stubPlanner) AfterHistoryPostProcessed(_ context.Context, _ chatcontext.HistoryPostProcessResult, _ bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posted = append(s.posted, CycleInfo{})
}

func (s *stubPlanner) NotifyCycleEnd(_ context.Context, info CycleInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cycles = append(s.cycles, info)
}

func (s *stubPlanner) RestoreProactiveUserContext(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restores++
	return 1, nil
}

// stubSummarizer records the removed messages handed to the chat-recall
// compression (P6 boundary).
type stubSummarizer struct {
	calls   int
	summary *chatcontext.MidTermMemorySummaryPayload
	ok      bool
	err     error
	seen    int
}

func (s *stubSummarizer) SummarizeMidTermMemory(_ context.Context, removed []chatcontext.LLMContextMessage) (*chatcontext.MidTermMemorySummaryPayload, bool, error) {
	s.calls++
	s.seen = len(removed)
	if s.err != nil {
		return nil, false, s.err
	}
	return s.summary, s.ok, nil
}

func (s *stubPlanner) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *stubPlanner) cycleEnds() []CycleInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]CycleInfo{}, s.cycles...)
}

// assistantItems builds N assistant output items.
func assistantItems(texts ...string) []*llm.ContextItem {
	items := make([]*llm.ContextItem, 0, len(texts))
	for _, text := range texts {
		item, err := llm.NewContextItemBuilder().SetRole(llm.RoleAssistant).AddTextPart(text).Build()
		if err != nil {
			continue
		}
		items = append(items, item)
	}
	return items
}

// functionCallResponse builds a planner response with one tool call.
func functionCallResponse(funcName string, callID string) *PlannerResponse {
	return &PlannerResponse{
		Content:     "调用 " + funcName,
		ToolCalls:   []PlannerToolCall{{CallID: callID, FuncName: funcName, Args: map[string]any{}}},
		OutputItems: assistantItems("调用 " + funcName),
	}
}

func runtimeMessage(messageID string, at time.Time) *message.SessionMessage {
	sessionMessage := message.NewSessionMessage(messageID, at, "qq")
	sessionMessage.SessionID = "session"
	sessionMessage.ProcessedPlainText = message.StringPtr("内容 " + messageID)
	sessionMessage.RawMessage = message.NewMessageSequence(message.NewTextComponent("内容 " + messageID))
	sessionMessage.MessageInfo.UserInfo = message.NewUserInfo("10001", "小明", "小明同学")
	return sessionMessage
}

func newTestRuntime(options Options) *Runtime {
	if options.SessionID == "" {
		options.SessionID = "session"
	}
	if options.MaxContextSize == 0 {
		options.MaxContextSize = 40
	}
	if options.BaseReplyFrequency == 0 {
		options.BaseReplyFrequency = 1
	}
	if options.MaxConsecutiveWaitCount == 0 {
		options.MaxConsecutiveWaitCount = 3
	}
	options.Log = func(string, ...any) {}
	return NewRuntime(options)
}

// ---------------------------------------------------------------------------
// state machine
// ---------------------------------------------------------------------------

// TestStateConstants pins `_STATE_RUNNING / _STATE_WAIT / _STATE_STOP`.
func TestStateConstants(t *testing.T) {
	if trigger.StateRunning != "running" || trigger.StateWait != "wait" || trigger.StateStop != "stop" {
		t.Fatalf("state constants changed: %s/%s/%s",
			trigger.StateRunning, trigger.StateWait, trigger.StateStop)
	}
	runtime := newTestRuntime(Options{IsGroupSession: true})
	if runtime.AgentState() != trigger.StateStop {
		t.Fatalf("initial state = %q, want stop", runtime.AgentState())
	}
	runtime.EnterRunningState()
	if runtime.AgentState() != trigger.StateRunning {
		t.Fatalf("state = %q, want running", runtime.AgentState())
	}
	runtime.EnterStopState()
	if runtime.AgentState() != trigger.StateStop {
		t.Fatalf("state = %q, want stop", runtime.AgentState())
	}
}

// TestMaxInternalRoundsConstant pins MAX_INTERNAL_ROUNDS = 10.
func TestMaxInternalRoundsConstant(t *testing.T) {
	if MaxInternalRounds != 10 {
		t.Fatalf("MaxInternalRounds = %d, want 10", MaxInternalRounds)
	}
	runtime := newTestRuntime(Options{})
	if got := runtime.Options().maxInternalRounds(); got != 10 {
		t.Fatalf("default maxInternalRounds = %d, want 10", got)
	}
}

// TestTryEnterWaitStateLimit pins `_try_enter_wait_state` /
// `max_consecutive_wait_count` (default 3).
func TestTryEnterWaitStateLimit(t *testing.T) {
	runtime := newTestRuntime(Options{IsGroupSession: true})
	seconds := 30.0
	for attempt := 1; attempt <= 3; attempt++ {
		entered, count, max := runtime.TryEnterWaitState(&seconds, fmt.Sprintf("call-%d", attempt), "turn-1")
		if !entered || count != attempt || max != 3 {
			t.Fatalf("attempt %d: entered=%v count=%d max=%d", attempt, entered, count, max)
		}
		if runtime.AgentState() != trigger.StateWait {
			t.Fatalf("state after wait = %q", runtime.AgentState())
		}
	}
	entered, count, max := runtime.TryEnterWaitState(&seconds, "call-4", "turn-1")
	if entered || count != 3 || max != 3 {
		t.Fatalf("4th wait: entered=%v count=%d max=%d, want false/3/3", entered, count, max)
	}
	if runtime.ConsecutiveWaitCount() != 3 {
		t.Fatalf("consecutive wait count = %d, want 3", runtime.ConsecutiveWaitCount())
	}

	// The count resets on a non-wait cycle end.
	runtime.ResetConsecutiveWaitCount("planner_no_tool_end")
	if runtime.ConsecutiveWaitCount() != 0 {
		t.Fatalf("after reset = %d, want 0", runtime.ConsecutiveWaitCount())
	}

	// max_consecutive_wait_count is clamped to >= 1 like `max(1, int(...))`.
	single := newTestRuntime(Options{MaxConsecutiveWaitCount: 0, MaxInternalRounds: 0})
	entered, count, max = single.TryEnterWaitState(nil, "call-1", "turn-1")
	if !entered || count != 1 || max != 3 {
		t.Fatalf("default wait config: entered=%v count=%d max=%d", entered, count, max)
	}
	limited := newTestRuntime(Options{MaxConsecutiveWaitCount: 1})
	if _, _, max = limited.TryEnterWaitState(nil, "c1", "t1"); max != 1 {
		t.Fatalf("max = %d, want 1", max)
	}
	if entered, _, _ = limited.TryEnterWaitState(nil, "c2", "t1"); entered {
		t.Fatalf("wait limit must be enforced")
	}
}

// TestEnterWaitStateRequiresIDs pins the Python ValueError.
func TestEnterWaitStateRequiresIDs(t *testing.T) {
	runtime := newTestRuntime(Options{})
	if entered, _, _ := runtime.TryEnterWaitState(nil, "", "turn-1"); entered {
		t.Fatalf("empty tool_call_id must be rejected")
	}
	runtime2 := newTestRuntime(Options{})
	if entered, _, _ := runtime2.TryEnterWaitState(nil, "call-1", "  "); entered {
		t.Fatalf("empty logical_turn_id must be rejected")
	}
	if runtime2.AgentState() != trigger.StateStop {
		t.Fatalf("state = %q, want stop after a rejected wait", runtime2.AgentState())
	}
}

// TestWaitTimeoutAndNewMessageWakeups pins
// `_schedule_wait_timeout` / `schedule_message_turn` wake-up semantics.
func TestWaitTimeoutAndNewMessageWakeups(t *testing.T) {
	runtime := newTestRuntime(Options{IsGroupSession: true})
	runtime.Start(nil)
	defer runtime.Stop()
	seconds := 5.0
	if entered, _, _ := runtime.TryEnterWaitState(&seconds, "call-1", "turn-1"); !entered {
		t.Fatalf("wait not entered")
	}

	// A timeout for a different call id must be ignored.
	if runtime.HandleWaitTimeout("other") {
		t.Fatalf("timeout for a different call must be ignored")
	}
	if runtime.AgentState() != trigger.StateWait {
		t.Fatalf("state = %q, want wait", runtime.AgentState())
	}
	// The matching timeout wakes the runtime and queues a timeout trigger.
	if !runtime.HandleWaitTimeout("call-1") {
		t.Fatalf("timeout not handled")
	}
	if runtime.AgentState() != trigger.StateRunning {
		t.Fatalf("state = %q, want running after the timeout", runtime.AgentState())
	}
	triggerKind, ok := runtime.NextTurnTrigger(nil)
	if !ok || triggerKind != TriggerTimeout {
		t.Fatalf("queued trigger = %q/%v, want timeout", triggerKind, ok)
	}

	// New private-chat messages wake from wait via the scheduler.
	privateRuntime := newTestRuntime(Options{IsGroupSession: false})
	privateRuntime.Start(nil)
	defer privateRuntime.Stop()
	if entered, _, _ := privateRuntime.TryEnterWaitState(nil, "call-2", "turn-2"); !entered {
		t.Fatalf("wait not entered")
	}
	privateRuntime.RegisterMessage(runtimeMessage("m1", time.Now()))
	if privateRuntime.AgentState() != trigger.StateRunning {
		t.Fatalf("private chat message must wake the wait, state = %q", privateRuntime.AgentState())
	}
}

// TestConsumePendingWaitState pins `_consume_pending_wait_state`.
func TestConsumePendingWaitState(t *testing.T) {
	runtime := newTestRuntime(Options{})
	if _, _, _, _, ok := runtime.ConsumePendingWaitState(); ok {
		t.Fatalf("consuming without a wait must report ok=false")
	}
	seconds := 12.0
	if entered, _, _ := runtime.TryEnterWaitState(&seconds, "call-1", "turn-1"); !entered {
		t.Fatalf("wait not entered")
	}
	callID, logicalTurnID, elapsed, requested, ok := runtime.ConsumePendingWaitState()
	if !ok || callID != "call-1" || logicalTurnID != "turn-1" {
		t.Fatalf("consume = %q/%q/%v", callID, logicalTurnID, ok)
	}
	if requested == nil || *requested != 12 {
		t.Fatalf("requested seconds = %v, want 12", requested)
	}
	if elapsed < 0 {
		t.Fatalf("elapsed = %v", elapsed)
	}
	if runtime.HasPendingWaitToolCall() {
		t.Fatalf("wait state must be cleared after consuming")
	}
}

// TestGetPendingWaitToolCallIDs pins `_get_pending_wait_tool_call_ids`.
func TestGetPendingWaitToolCallIDs(t *testing.T) {
	runtime := newTestRuntime(Options{})
	if ids := runtime.GetPendingWaitToolCallIDs(); len(ids) != 0 {
		t.Fatalf("ids = %v, want empty", ids)
	}
	if entered, _, _ := runtime.TryEnterWaitState(nil, "call-1", "turn-1"); !entered {
		t.Fatalf("wait not entered")
	}
	ids := runtime.GetPendingWaitToolCallIDs()
	if !ids["call-1"] || len(ids) != 1 {
		t.Fatalf("ids = %v, want {call-1}", ids)
	}
}

// TestMessageTriggerThreshold pins `_get_message_trigger_threshold`
// (ceil(1 / frequency)).
func TestMessageTriggerThreshold(t *testing.T) {
	cases := []struct {
		frequency float64
		want      int
	}{
		{1.0, 1},
		{0.5, 2},
		{0.34, 3},
		{0.25, 4},
		{0.2, 5},
	}
	for _, testCase := range cases {
		runtime := newTestRuntime(Options{BaseReplyFrequency: testCase.frequency})
		if got := runtime.MessageTriggerThreshold(); got != testCase.want {
			t.Errorf("frequency=%v threshold=%d, want %d", testCase.frequency, got, testCase.want)
		}
	}

	runtime := newTestRuntime(Options{BaseReplyFrequency: 1})
	runtime.AdjustTalkFrequency(0.5)
	if got := runtime.MessageTriggerThreshold(); got != 2 {
		t.Errorf("after adjust threshold=%d, want 2", got)
	}
	if got := runtime.EffectiveReplyFrequency(); got != 0.5 {
		t.Errorf("effective frequency = %v, want 0.5", got)
	}
	if runtime.IsReplyFrequencySilent() {
		t.Errorf("0.5 must not be silent")
	}
	// A frequency of 0 is the silent mode: threshold 0 and `_is_reply_frequency_silent`.
	silent := NewRuntime(Options{
		SessionID:          "session",
		IsGroupSession:     true,
		MaxContextSize:     40,
		BaseReplyFrequency: 0,
		Log:                func(string, ...any) {},
	})
	if threshold := silent.MessageTriggerThreshold(); threshold != 0 {
		t.Errorf("silent threshold = %d, want 0", threshold)
	}
	if !silent.IsReplyFrequencySilent() {
		t.Errorf("frequency 0 must be silent")
	}
}

// TestTalkValueProviderOverridesBaseFrequency pins the
// `ChatConfigUtils.get_talk_value` injection point (dynamic talk-value rules).
func TestTalkValueProviderOverridesBaseFrequency(t *testing.T) {
	value := 0.4
	runtime := newTestRuntime(Options{
		BaseReplyFrequency: 1.0,
		TalkValueProvider:  func() float64 { return value },
	})
	if got := runtime.EffectiveReplyFrequency(); math.Abs(got-0.4) > 1e-12 {
		t.Fatalf("frequency = %v, want 0.4 from the provider", got)
	}
	if got := runtime.MessageTriggerThreshold(); got != 3 {
		t.Fatalf("threshold = %d, want ceil(1/0.4) = 3", got)
	}
	runtime.AdjustTalkFrequency(0.5)
	if got := runtime.EffectiveReplyFrequency(); math.Abs(got-0.2) > 1e-12 {
		t.Fatalf("frequency after adjust = %v, want 0.2", got)
	}
	// A provider returning 0 makes the runtime silent.
	value = 0
	if !runtime.IsReplyFrequencySilent() || runtime.MessageTriggerThreshold() != 0 {
		t.Fatalf("provider 0 must be silent")
	}
}

// TestForcedTurnStateLifecycle pins `_arm_forced_turn_state` /
// `_consume_forced_turn_reason`.
func TestForcedTurnStateLifecycle(t *testing.T) {
	runtime := newTestRuntime(Options{IsGroupSession: true})
	if runtime.HasForcedTurnTrigger() {
		t.Fatalf("forced turn armed initially")
	}
	atMessage := runtimeMessage("m1", time.Now())
	atMessage.IsAt = true
	runtime.RegisterMessage(atMessage)
	if !runtime.HasForcedTurnTrigger() {
		t.Fatalf("@ message must arm the forced turn")
	}
	reason, ok := runtime.ConsumeForcedTurnReason()
	if !ok || !strings.Contains(reason, "检测到新的@消息") || !strings.Contains(reason, "消息编号=m1") {
		t.Fatalf("reason = %q/%v", reason, ok)
	}
	if runtime.HasForcedTurnTrigger() {
		t.Fatalf("forced turn must be consumed")
	}
	if _, ok := runtime.ConsumeForcedTurnReason(); ok {
		t.Fatalf("second consume must report ok=false")
	}

	// A mention-only message uses the 提及消息 reason.
	mentionMessage := runtimeMessage("m2", time.Now())
	mentionMessage.IsMentioned = true
	runtime.RegisterMessage(mentionMessage)
	reason, _ = runtime.ConsumeForcedTurnReason()
	if !strings.Contains(reason, "提及消息") {
		t.Fatalf("reason = %q, want 提及消息", reason)
	}

	// A plain message does not arm anything.
	runtime.RegisterMessage(runtimeMessage("m3", time.Now()))
	if runtime.HasForcedTurnTrigger() {
		t.Fatalf("plain message must not arm the forced turn")
	}
}

// TestRegisterMessageSchedulesTurn pins `register_message` →
// `_schedule_message_turn`.
func TestRegisterMessageSchedulesTurn(t *testing.T) {
	runtime := newTestRuntime(Options{IsGroupSession: true, BaseReplyFrequency: 1})
	runtime.Start(nil)
	defer runtime.Stop()

	runtime.RegisterMessage(runtimeMessage("m1", time.Now()))
	if runtime.PendingMessageCount() != 1 {
		t.Fatalf("pending = %d, want 1", runtime.PendingMessageCount())
	}
	if !runtime.MessageTurnScheduled() {
		t.Fatalf("the message turn must be scheduled at frequency 1 (threshold 1)")
	}
	triggerKind, ok := runtime.NextTurnTrigger(nil)
	if !ok || triggerKind != TriggerMessage {
		t.Fatalf("trigger = %q/%v, want message", triggerKind, ok)
	}
}

// TestFocusEntryGateBlocksDecision mirrors `_should_continue_after_focus_gate`.
func TestFocusEntryGateBlocksDecision(t *testing.T) {
	gated := newTestRuntime(Options{
		IsGroupSession:  true,
		FocusModeActive: func() bool { return true },
		FocusEntryGate:  func(string, bool) bool { return false },
	})
	gated.Start(nil)
	defer gated.Stop()

	gated.RegisterMessage(runtimeMessage("m1", time.Now()))
	if gated.MessageTurnScheduled() {
		t.Fatalf("a chat outside the focus slot must not schedule a turn")
	}
	if gated.PendingMessageCount() != 1 {
		t.Fatalf("the message must still be cached, pending = %d", gated.PendingMessageCount())
	}

	// An @ message bypasses the gate (`message.is_at and
	// focus_mode_manager.unblock_focus_entry`).
	atRuntime := newTestRuntime(Options{
		IsGroupSession:  true,
		FocusModeActive: func() bool { return true },
		FocusEntryGate:  func(_ string, isAt bool) bool { return isAt },
	})
	atRuntime.Start(nil)
	defer atRuntime.Stop()
	atMessage := runtimeMessage("m2", time.Now())
	atMessage.IsAt = true
	atRuntime.RegisterMessage(atMessage)
	if !atRuntime.MessageTurnScheduled() {
		t.Fatalf("@ messages must pass the focus gate")
	}

	// Focus mode off → no gating at all.
	off := newTestRuntime(Options{
		IsGroupSession: true,
		FocusEntryGate: func(string, bool) bool { return false },
	})
	off.Start(nil)
	defer off.Stop()
	off.RegisterMessage(runtimeMessage("m3", time.Now()))
	if !off.MessageTurnScheduled() {
		t.Fatalf("with focus mode off the gate must not apply")
	}
}

// TestPendingMessageCachePruning pins `_prune_processed_message_cache`.
func TestPendingMessageCachePruning(t *testing.T) {
	runtime := newTestRuntime(Options{})
	runtime.mu.Lock()
	for index := 0; index < MaxRetainedMessageCacheSize+20; index++ {
		runtime.messageCache = append(runtime.messageCache, runtimeMessage(fmt.Sprintf("m%d", index), time.Now()))
	}
	runtime.lastProcessedIndex = MaxRetainedMessageCacheSize + 20
	runtime.mu.Unlock()

	runtime.PruneProcessedMessageCache()
	runtime.mu.Lock()
	retained := len(runtime.messageCache)
	lastProcessed := runtime.lastProcessedIndex
	runtime.mu.Unlock()
	if retained != MaxRetainedMessageCacheSize {
		t.Fatalf("retained = %d, want %d", retained, MaxRetainedMessageCacheSize)
	}
	if lastProcessed != MaxRetainedMessageCacheSize {
		t.Fatalf("lastProcessedIndex = %d, want %d", lastProcessed, MaxRetainedMessageCacheSize)
	}

	// Unprocessed messages are never dropped.
	fresh := newTestRuntime(Options{})
	fresh.mu.Lock()
	for index := 0; index < MaxRetainedMessageCacheSize+10; index++ {
		fresh.messageCache = append(fresh.messageCache, runtimeMessage(fmt.Sprintf("f%d", index), time.Now()))
	}
	fresh.mu.Unlock()
	fresh.PruneProcessedMessageCache()
	if got := fresh.PendingMessageCount(); got != MaxRetainedMessageCacheSize+10 {
		t.Fatalf("unprocessed messages were dropped: pending = %d", got)
	}
}

// TestCollectPendingMessages pins `_collect_pending_messages`.
func TestCollectPendingMessages(t *testing.T) {
	runtime := newTestRuntime(Options{})
	runtime.mu.Lock()
	runtime.messageCache = append(runtime.messageCache,
		runtimeMessage("m1", time.Now()), runtimeMessage("m2", time.Now()), runtimeMessage("m3", time.Now()))
	runtime.mu.Unlock()
	if runtime.HasPendingMessages() != true {
		t.Fatalf("HasPendingMessages = false")
	}
	pending := runtime.CollectPendingMessages()
	if len(pending) != 3 {
		t.Fatalf("pending = %d, want 3", len(pending))
	}
	if runtime.HasPendingMessages() || runtime.PendingMessageCount() != 0 {
		t.Fatalf("cache index was not advanced")
	}
	if again := runtime.CollectPendingMessages(); len(again) != 0 {
		t.Fatalf("second collect = %v, want empty", again)
	}
}

// TestExternalMessageIntervalStats pins
// `_record_external_message_interval` / `_get_recent_average_external_message_interval`.
//
// The Python implementation samples with `time.time()`, so this test uses wall
// clock offsets instead of fake absolute timestamps.
func TestExternalMessageIntervalStats(t *testing.T) {
	now := float64(time.Now().UnixNano()) / 1e9
	runtime := newTestRuntime(Options{IsGroupSession: true})
	if _, ok := runtime.RecentAverageExternalMessageInterval(); ok {
		t.Fatalf("no samples must report ok=false before any external message")
	}

	runtime.recordExternalMessageInterval(runtimeMessage("m1", time.Now()), now)
	if average, ok := runtime.RecentAverageExternalMessageInterval(); !ok ||
		average != IdleCompensationMinAverageIntervalSeconds {
		t.Fatalf("after the first sample: average=%v ok=%v, want the 30s floor", average, ok)
	}

	// A burst (< 5s) is filtered out and does not become a sample.
	runtime.recordExternalMessageInterval(runtimeMessage("m2", time.Now()), now+2)
	if average, _ := runtime.RecentAverageExternalMessageInterval(); average != IdleCompensationMinAverageIntervalSeconds {
		t.Fatalf("burst interval leaked into the average: %v", average)
	}
	runtime.mu.Lock()
	samples := len(runtime.recentExternalMessageIntervals)
	runtime.mu.Unlock()
	if samples != 0 {
		t.Fatalf("burst samples = %d, want 0", samples)
	}

	// A real gap (measured from the burst message) is recorded.
	runtime.recordExternalMessageInterval(runtimeMessage("m3", time.Now()), now+62)
	average, ok := runtime.RecentAverageExternalMessageInterval()
	if !ok || average != 60 {
		t.Fatalf("average = %v/%v, want 60", average, ok)
	}
	if ExternalMessageBurstIntervalSeconds != 5.0 ||
		IdleCompensationMinAverageIntervalSeconds != 30.0 ||
		ExternalMessageIntervalSampleWindowSeconds != 1800.0 {
		t.Fatalf("interval constants changed")
	}

	// Samples older than 30 minutes are pruned and the 30s floor replaces them
	// (the "seen an external message but no usable sample" fallback).
	runtime.mu.Lock()
	runtime.recentExternalMessageIntervals[0].at = now - 3600
	runtime.mu.Unlock()
	average, ok = runtime.RecentAverageExternalMessageInterval()
	if !ok || average != IdleCompensationMinAverageIntervalSeconds {
		t.Fatalf("expired samples: average=%v ok=%v, want the 30s floor", average, ok)
	}

	// A never-seen-external-message runtime keeps reporting ok=false.
	fresh := newTestRuntime(Options{})
	if _, ok := fresh.RecentAverageExternalMessageInterval(); ok {
		t.Fatalf("fresh runtime must report ok=false")
	}
}

// TestExpandMessageIDAliasesUsesNewestFirst pins
// `expand_message_id_aliases`.
func TestExpandMessageIDAliasesUsesNewestFirst(t *testing.T) {
	longID := "ROBOT1.0_abcdefghijklmnopqrstuvwxyz0123456789"
	alias := chatcontext.ToDisplayMessageID(longID)
	runtime := newTestRuntime(Options{})
	entry := chatcontext.NewSessionBackedMessage(nil, "可见文本", time.Now(), longID, nil, chatcontext.SourceUser)
	runtime.AppendChatHistory(entry)

	expanded := runtime.ExpandMessageIDAliases(map[string]any{"msg_id": alias})
	if expanded["msg_id"] != longID {
		t.Fatalf("expanded = %v, want %v", expanded["msg_id"], longID)
	}
	if source := runtime.FindSourceMessageByID(longID); source != nil {
		t.Fatalf("source = %v, want nil (no original SessionMessage)", source)
	}
}

// TestFocusModeDefaults pins `_is_focus_mode_active_for_current_chat`: the
// experimental switch defaults to OFF (`focus_mode = false`), which makes
// `can_decide` always return true.
func TestFocusModeDefaults(t *testing.T) {
	runtime := newTestRuntime(Options{})
	if runtime.FocusModeActive() {
		t.Fatalf("focus mode must default to off")
	}
	runtime2 := newTestRuntime(Options{FocusModeActive: func() bool { return true }})
	if !runtime2.FocusModeActive() {
		t.Fatalf("focus probe not honoured")
	}
	// The idle backoff resets when focus mode is active for the chat.
	runtime2.IdleBackoff().RecordCycleResult("planner_no_tool_end")
	runtime2.IdleBackoff().RecordCycleResult("planner_no_tool_end")
	if runtime2.IdleBackoff().ShouldDelay(1) {
		t.Fatalf("focus mode must reset the idle backoff")
	}
}

// ---------------------------------------------------------------------------
// interrupt controller
// ---------------------------------------------------------------------------

// TestPlannerInterruptDefaultDisabled pins
// `planner_interrupt_max_consecutive_count = 0` disabling interrupts.
func TestPlannerInterruptDefaultDisabled(t *testing.T) {
	runtime := newTestRuntime(Options{IsGroupSession: true, PlannerInterruptMaxConsecutiveCount: 0})
	if runtime.PlannerInterruptMaxConsecutiveCount() != 0 {
		t.Fatalf("max count = %d, want 0", runtime.PlannerInterruptMaxConsecutiveCount())
	}
	runtime.EnterRunningState()
	flag := NewInterruptFlag()
	runtime.BindPlannerInterruptFlag(flag)
	runtime.RequestPlannerInterruptForMessage(runtimeMessage("m1", time.Now()))
	if flag.IsSet() {
		t.Fatalf("interrupt must stay disabled when the limit is 0")
	}
	if runtime.interrupt.ConsecutiveCount() != 0 {
		t.Fatalf("consecutive count = %d, want 0", runtime.interrupt.ConsecutiveCount())
	}
}

// TestPlannerInterruptRequestLifecycle pins
// request / duplicate / limit / idle.
func TestPlannerInterruptRequestLifecycle(t *testing.T) {
	controller := NewPlannerInterruptController()
	if got := controller.Request(2); got != InterruptIdle {
		t.Fatalf("request without a bound flag = %q, want idle", got)
	}

	flag := NewInterruptFlag()
	controller.Bind(flag, nil)
	if got := controller.Request(2); got != InterruptRequest {
		t.Fatalf("first request = %q, want request", got)
	}
	if !flag.IsSet() {
		t.Fatalf("flag was not set")
	}
	if got := controller.Request(2); got != InterruptDuplicate {
		t.Fatalf("second request = %q, want duplicate", got)
	}
	if controller.ConsecutiveCount() != 1 {
		t.Fatalf("consecutive count = %d, want 1", controller.ConsecutiveCount())
	}

	// An interrupted unbind keeps the count; a clean unbind resets it.
	controller.Unbind(flag, true)
	flag2 := NewInterruptFlag()
	controller.Bind(flag2, nil)
	if controller.ConsecutiveCount() != 1 {
		t.Fatalf("interrupted unbind must keep the count, got %d", controller.ConsecutiveCount())
	}
	if got := controller.Request(2); got != InterruptRequest {
		t.Fatalf("second interrupt = %q, want request", got)
	}
	controller.Unbind(flag2, true)
	flag3 := NewInterruptFlag()
	controller.Bind(flag3, nil)
	if got := controller.Request(2); got != InterruptLimit {
		t.Fatalf("request at the limit = %q, want limit", got)
	}
	controller.Unbind(flag3, false)
	if controller.ConsecutiveCount() != 0 {
		t.Fatalf("clean unbind must reset the count, got %d", controller.ConsecutiveCount())
	}
}

// TestPlannerInterruptCancelsContext pins the context cancellation hook.
func TestPlannerInterruptCancelsContext(t *testing.T) {
	controller := NewPlannerInterruptController()
	flag := NewInterruptFlag()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller.Bind(flag, cancel)
	if got := controller.Request(3); got != InterruptRequest {
		t.Fatalf("request = %q", got)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatalf("context was not cancelled")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err = %v", ctx.Err())
	}
}

// TestInterruptFlagWaiter pins the `asyncio.Event` stand-in.
func TestInterruptFlagWaiter(t *testing.T) {
	flag := NewInterruptFlag()
	done := flag.Done()
	select {
	case <-done:
		t.Fatalf("flag is not set yet")
	default:
	}
	flag.Set()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("waiter was not released")
	}
	if !flag.IsSet() {
		t.Fatalf("IsSet = false")
	}
	// A Done() call after Set returns a closed channel immediately.
	select {
	case <-flag.Done():
	default:
		t.Fatalf("Done() after Set must be closed")
	}
}

// TestInterruptDescription pins the monitoring lines.
func TestInterruptDescription(t *testing.T) {
	lines := InterruptDescription(10.0, 10.5)
	if len(lines) != 3 || lines[0] != "状态：已被新消息打断" ||
		lines[1] != "打断位置：Planner 请求流式响应阶段" || lines[2] != "打断耗时：0.500 秒" {
		t.Fatalf("lines = %v", lines)
	}
}
