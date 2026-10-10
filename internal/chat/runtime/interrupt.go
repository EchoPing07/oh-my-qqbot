package runtime

import (
	"context"
	"fmt"
	"sync"

	"oh-my-qqbot/internal/chat/trigger"
	"oh-my-qqbot/internal/message"
)

// This file holds the Planner interrupt controller and the interrupt-driven
// rerun path of 参考/MaiBot/src/maisaka/runtime.py:
// `PlannerInterruptController`, `_request_planner_interrupt_for_message`,
// `_bind_planner_interrupt_flag` and the `except ReqAbortException` branch of
// reasoning_engine.run_loop.
//
// Default behaviour: `planner_interrupt_max_consecutive_count = 0` disables
// interrupting entirely (`request` immediately returns "limit"), which is the
// MaiBot default. When > 0 the current planner request is cancelled through
// `context.Context` cancellation plus the bound InterruptFlag and the loop
// re-runs with the new messages.

// Interrupt request results mirroring PlannerInterruptController.request.
const (
	InterruptRequest   = "request"
	InterruptDuplicate = "duplicate"
	InterruptLimit     = "limit"
	InterruptIdle      = "idle"
)

// PlannerInterruptController mirrors runtime.PlannerInterruptController: it
// tracks the currently interruptible planner request and the consecutive
// interrupt count used to bound the rerun storm.
type PlannerInterruptController struct {
	mu               sync.Mutex
	flag             *InterruptFlag
	cancel           context.CancelFunc
	requested        bool
	consecutiveCount int
}

// InterruptFlag is the Go stand-in for `asyncio.Event`: planner requests observe
// it through PlannerRequest.Interrupted and through the context passed to
// PlannerLLM.ChatLoopStep.
type InterruptFlag struct {
	mu        sync.Mutex
	cancelled bool
	waiters   []chan struct{}
}

// NewInterruptFlag builds an unset flag.
func NewInterruptFlag() *InterruptFlag { return &InterruptFlag{} }

// Set cancels the flag and releases every waiter (mirrors `event.set()`).
func (f *InterruptFlag) Set() {
	f.mu.Lock()
	if f.cancelled {
		f.mu.Unlock()
		return
	}
	f.cancelled = true
	waiters := f.waiters
	f.waiters = nil
	f.mu.Unlock()
	for _, waiter := range waiters {
		close(waiter)
	}
}

// IsSet mirrors `event.is_set()`.
func (f *InterruptFlag) IsSet() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled
}

// Done returns a channel closed once the flag is set.
func (f *InterruptFlag) Done() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	done := make(chan struct{})
	if f.cancelled {
		close(done)
		return done
	}
	f.waiters = append(f.waiters, done)
	return done
}

// NewPlannerInterruptController mirrors PlannerInterruptController.__init__.
func NewPlannerInterruptController() *PlannerInterruptController {
	return &PlannerInterruptController{}
}

// ConsecutiveCount mirrors the `consecutive_count` property.
func (c *PlannerInterruptController) ConsecutiveCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consecutiveCount
}

// Bind mirrors PlannerInterruptController.bind.
func (c *PlannerInterruptController) Bind(flag *InterruptFlag, cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flag = flag
	c.cancel = cancel
	c.requested = false
}

// Unbind mirrors PlannerInterruptController.unbind.
func (c *PlannerInterruptController) Unbind(flag *InterruptFlag, interrupted bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.flag == flag {
		c.flag = nil
		c.cancel = nil
	}
	c.requested = false
	if !interrupted {
		c.consecutiveCount = 0
	}
}

// Request mirrors PlannerInterruptController.request: it returns
// request/duplicate/limit/idle and, on "request", sets the bound flag plus the
// bound context cancellation.
func (c *PlannerInterruptController) Request(maxConsecutiveCount int) string {
	c.mu.Lock()
	if c.flag == nil {
		c.mu.Unlock()
		return InterruptIdle
	}
	if c.requested {
		c.mu.Unlock()
		return InterruptDuplicate
	}
	if c.consecutiveCount >= maxConsecutiveCount {
		c.mu.Unlock()
		return InterruptLimit
	}
	c.requested = true
	c.consecutiveCount++
	flag := c.flag
	cancel := c.cancel
	c.mu.Unlock()

	flag.Set()
	if cancel != nil {
		cancel()
	}
	return InterruptRequest
}

// ---------------------------------------------------------------------------
// runtime bindings
// ---------------------------------------------------------------------------

// BindPlannerInterruptFlag mirrors `_bind_planner_interrupt_flag`, extended with
// the context cancellation handle used by the Go planner boundary.
func (r *Runtime) BindPlannerInterruptFlag(flag *InterruptFlag) {
	r.interrupt.Bind(flag, nil)
}

// BindPlannerInterrupt mirrors `_bind_planner_interrupt_flag` with a cancel
// handle: cancelling the returned context aborts the request exactly like the
// Python ReqAbortException path.
func (r *Runtime) BindPlannerInterrupt(flag *InterruptFlag, cancel context.CancelFunc) {
	r.interrupt.Bind(flag, cancel)
}

// UnbindPlannerInterruptFlag mirrors `_unbind_planner_interrupt_flag`.
func (r *Runtime) UnbindPlannerInterruptFlag(flag *InterruptFlag, interrupted bool) {
	r.interrupt.Unbind(flag, interrupted)
}

// PlannerInterruptMaxConsecutiveCount mirrors
// `_planner_interrupt_max_consecutive_count` (0 = interrupting disabled).
func (r *Runtime) PlannerInterruptMaxConsecutiveCount() int {
	return maxInt(0, r.options.PlannerInterruptMaxConsecutiveCount)
}

// RequestPlannerInterruptForMessage mirrors
// `_request_planner_interrupt_for_message`.
func (r *Runtime) RequestPlannerInterruptForMessage(sessionMessage *message.SessionMessage) {
	if r.AgentState() != trigger.StateRunning {
		return
	}
	maxCount := r.PlannerInterruptMaxConsecutiveCount()
	requestResult := r.interrupt.Request(maxCount)
	if requestResult == InterruptIdle {
		return
	}
	consecutiveCount := r.interrupt.ConsecutiveCount()
	messageID := ""
	if sessionMessage != nil {
		messageID = sessionMessage.MessageID
	}
	switch requestResult {
	case InterruptDuplicate:
		r.log("%s 收到新消息，但当前请求已发起过一次规划器打断，本次不重复打断; 消息编号=%s 连续打断次数=%d/%d",
			r.LogPrefix(), messageID, consecutiveCount, maxCount)
	case InterruptLimit:
		r.log("%s 收到新消息，但已达到规划器连续打断上限，将等待当前请求自然完成; 消息编号=%s 连续打断次数=%d/%d",
			r.LogPrefix(), messageID, consecutiveCount, maxCount)
	default:
		// T2 修复：messageCache 由 r.mu 保护（RegisterMessage 引擎/渠道两侧
		// 并发追加），锁内取长度，避免与持锁写入并发。
		r.mu.Lock()
		messageCacheSize := len(r.messageCache)
		r.mu.Unlock()
		r.log("%s 收到新消息，发起规划器打断; 消息编号=%s 缓存条数=%d 时间戳=%.3f 连续打断次数=%d/%d",
			r.LogPrefix(), messageID, messageCacheSize, nowSeconds(), consecutiveCount, maxCount)
	}
}

// interruptBudgetAvailable reports whether another interrupt request would be
// accepted (mirrors the guards of `request` for the "limit" decision).
func (r *Runtime) interruptBudgetAvailable() bool {
	maxCount := r.PlannerInterruptMaxConsecutiveCount()
	if maxCount <= 0 {
		return false
	}
	return r.interrupt.ConsecutiveCount() < maxCount
}

// ResetPlannerInterruptCount mirrors the `unbind(interrupted=False)` reset used
// by `_unbind_planner_interrupt_flag`.
func (r *Runtime) ResetPlannerInterruptCount() {
	r.interrupt.mu.Lock()
	defer r.interrupt.mu.Unlock()
	r.interrupt.consecutiveCount = 0
}

// InterruptDescription renders the monitoring line for an interrupted round
// (mirrors the `extra_lines` of `_handle_planner_interrupt`).
func InterruptDescription(currentStageStartedAt float64, interruptedAt float64) []string {
	return []string{
		"状态：已被新消息打断",
		"打断位置：Planner 请求流式响应阶段",
		fmt.Sprintf("打断耗时：%.3f 秒", interruptedAt-currentStageStartedAt),
	}
}
