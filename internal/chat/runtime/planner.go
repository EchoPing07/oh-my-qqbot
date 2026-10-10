package runtime

import (
	"context"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/llm"
)

// This file defines the planner boundary of the runtime. The actual prompt
// assembly (对齐 chat_loop_service.py / maisaka_chat.prompt) and the tool
// execution loop belong to P4b; the runtime only drives the rounds, mirrors the
// state machine and folds the model output back into the history.

// PlannerRequest mirrors the arguments of MaisakaChatLoopService.chat_loop_step:
// the runtime passes the full history and the caller assembles the provider
// request from it.
type PlannerRequest struct {
	// SessionID is the owning chat session.
	SessionID string
	// History is the runtime `_chat_history` (the planner must not mutate it).
	History []chatcontext.LLMContextMessage
	// InjectedUserMessages mirrors `injected_user_messages` (one-shot user
	// injections appended after the history).
	InjectedUserMessages []string
	// TailUserMessages mirrors `tail_user_messages` (focus-mode tail).
	TailUserMessages []string
	// ToolDefinitions mirrors the Action Loop tool definitions. The runtime does
	// not inspect them; P4b supplies them.
	ToolDefinitions []PlannerToolDefinition
	// MaxContextSize mirrors the `max_context_size` argument.
	MaxContextSize int
	// LogicalTurnID mirrors the `logical_turn_id` argument.
	LogicalTurnID string
	// Interrupted is closed (or cancelled) when the request must abort because a
	// new message arrived and the interrupt budget allows it.
	Interrupted <-chan struct{}
}

// PlannerToolDefinition is the opaque tool definition passed through to the
// planner (P4b owns the concrete shape).
type PlannerToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// PlannerResponse mirrors ChatResponse: one planner round output.
type PlannerResponse struct {
	// Content mirrors `response.content` (Planner visible text).
	Content string
	// Reasoning mirrors the provider reasoning text (never forwarded to tools).
	Reasoning string
	// ToolCalls mirrors `response.tool_calls`.
	ToolCalls []PlannerToolCall
	// OutputItems mirrors `response.output_items`, the raw model output items
	// stored item-by-item into the history (`response.raw_messages`).
	OutputItems []*llm.ContextItem
	// ModelName / token accounting are carried for monitoring only.
	ModelName            string
	PromptTokens         int
	CompletionTokens     int
	TotalTokens          int
	SelectedHistoryCount int
	ToolCount            int
}

// PlannerToolCall mirrors the ToolCall view of ChatResponse.
type PlannerToolCall struct {
	CallID   string
	FuncName string
	Args     map[string]any
	// LogicalTurnID / Reasoning 由引擎在执行前回填（对齐 Python 工具调用
	// 的 logical_turn_id / reasoning 传递；wait 进入等待态依赖非空
	// logical_turn_id，reasoning 作为 reply_reference 的兜底文案）。
	LogicalTurnID string
	Reasoning     string
}

// PlannerToolResult mirrors ToolExecutionResult (P4b).
type PlannerToolResult struct {
	CallID   string
	ToolName string
	Content  string
	Success  bool
	// PauseExecution mirrors `pause_execution` in the tool metadata: the loop
	// must end after this tool batch.
	PauseExecution bool
	// PauseReason mirrors STOP_AFTER_EXECUTION_PAUSE_REASON / "wait" /
	// "wait_rest" / any other pause tool name.
	PauseReason string
	// PostHistory mirrors `_append_tool_post_history_messages`: entries the tool
	// wants appended to the history (e.g. media placeholders).
	PostHistory []*llm.ContextItem
	// Metadata mirrors the tool result metadata.
	Metadata map[string]any
}

// PlannerLLM is the boundary implemented by P4b: it assembles the prompt, calls
// the model, executes the requested tools and returns the round result.
//
// The runtime guarantees:
//   - it appends the returned output items to the history itself, so the
//     implementation must not mutate `request.History`;
//   - `request.Interrupted` is observed for cancellation (mirrors
//     ReqAbortException) — an implementation interrupting a request returns an
//     error for which IsInterruptedError reports true.
type PlannerLLM interface {
	// ChatLoopStep runs one planner request (`chat_loop_step`).
	ChatLoopStep(ctx context.Context, request PlannerRequest) (*PlannerResponse, error)
	// ExecuteToolCalls executes the tool calls of one round
	// (`_handle_tool_calls`), returning one result per call.
	ExecuteToolCalls(ctx context.Context, calls []PlannerToolCall, planContent string) ([]PlannerToolResult, error)
	// BuildActionToolDefinitions builds the visible tool definitions and the
	// deferred-tools reminder for the current iteration
	// (`_build_action_tool_definitions`).
	BuildActionToolDefinitions(ctx context.Context) ([]PlannerToolDefinition, string, error)
	// BuildInjectedUserMessages builds the one-shot user injections of one round
	// (`_build_planner_injected_user_messages`).
	BuildInjectedUserMessages(ctx context.Context, deferredToolsReminder string) ([]string, error)
	// RefreshMidTermMemoryReference mirrors
	// `_refresh_mid_term_memory_reference_message`; the runtime calls it once at
	// the start of a continuation before the first round.
	RefreshMidTermMemoryReference(ctx context.Context) error
	// NotifyCycleEnd mirrors `_finalize_cycle`'s monitoring/reporting side
	// effects. The history post-processing itself is owned by the runtime
	// (`_post_process_chat_history_after_cycle` lives in reasoning_engine.py).
	NotifyCycleEnd(ctx context.Context, info CycleInfo)
}

// HistoryPostProcessor is the optional hook a planner implementation may add to
// observe the post-processing of one cycle (e.g. to run behaviour-reference
// refreshes or learning submissions). The context trimming and the chat-recall
// folding are always performed by the runtime itself.
type HistoryPostProcessor interface {
	AfterHistoryPostProcessed(ctx context.Context, result chatcontext.HistoryPostProcessResult, enableMidTermMemory bool)
}

// MidTermMemorySummarizer is the LLM boundary of the chat recall (mid-term
// memory) compression, implemented by P6 (`memory.mid_term`). Returning
// ok=false mirrors `build_mid_term_memory_message` returning None.
type MidTermMemorySummarizer interface {
	SummarizeMidTermMemory(ctx context.Context, removedMessages []chatcontext.LLMContextMessage) (*chatcontext.MidTermMemorySummaryPayload, bool, error)
}

// CycleInfo describes one finished internal cycle (mirrors CycleDetail plus the
// runtime state the engine reports).
type CycleInfo struct {
	// HistoryPostProcess carries the trimmed-history accounting so the planner
	// hook (HistoryPostProcessor) can decide whether to run learning.
	HistoryPostProcess *chatcontext.HistoryPostProcessResult

	CycleID          int
	RoundIndex       int
	EndReason        string
	EndDetail        string
	PlannerContent   string
	ToolCalls        []PlannerToolCall
	ToolResults      []PlannerToolResult
	Interrupted      bool
	ModelName        string
	PromptTokens     int
	CompletionTokens int
}

// InterruptedError mirrors ReqAbortException: the planner request was aborted
// because a new message arrived.
type InterruptedError struct {
	Reason string
}

// Error implements error.
func (e *InterruptedError) Error() string {
	if e.Reason == "" {
		return "runtime: planner request interrupted"
	}
	return "runtime: planner request interrupted: " + e.Reason
}

// IsInterruptedError reports whether an error is an InterruptedError (mirrors
// the `except ReqAbortException` branch of run_loop).
func IsInterruptedError(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*InterruptedError)
	return ok
}
