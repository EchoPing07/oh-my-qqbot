package trigger

import (
	"math"
	"strings"
	"sync"
	"time"
)

// This file mirrors 参考/MaiBot/src/maisaka/turn_trigger/idle_backoff.py and the
// idle-cycle classification of 参考/MaiBot/src/maisaka/mode_policy.py.

// IdleCycleReasons mirrors mode_policy.IDLE_CYCLE_REASONS: the whole-cycle end
// reasons that count as "idle" for the backoff.
var IdleCycleReasons = map[string]bool{
	"planner_no_tool_end":       true,
	"planner_wait_rest":         true,
	"tool_pause:wait":           true,
	"tool_stop_after_execution": true,
}

// IsIdleCycleReason mirrors mode_policy.is_idle_cycle_reason.
func IsIdleCycleReason(cycleEndReason string) bool {
	return IdleCycleReasons[trimSpaceString(cycleEndReason)]
}

// ReplyTimingBackoffConfig mirrors the `chat.reply_timing` fields read by
// idle_backoff.py. Values are supplied by the caller (the config package is not
// imported by this package).
type ReplyTimingBackoffConfig struct {
	// BaseSeconds mirrors no_action_backoff_base_seconds (default 15).
	BaseSeconds float64
	// CapSeconds mirrors no_action_backoff_cap_seconds (default 300).
	CapSeconds float64
	// StartCount mirrors no_action_backoff_start_count (default 2).
	StartCount int
	// BypassPendingCount mirrors no_action_backoff_bypass_pending_count
	// (default 6, 0 disables the bypass).
	BypassPendingCount int
}

// DefaultReplyTimingBackoffConfig mirrors the MaiBot defaults.
func DefaultReplyTimingBackoffConfig() ReplyTimingBackoffConfig {
	return ReplyTimingBackoffConfig{
		BaseSeconds:        15,
		CapSeconds:         300,
		StartCount:         2,
		BypassPendingCount: 6,
	}
}

// IdleBackoffRuntime is the runtime surface IdleBackoffController needs.
type IdleBackoffRuntime interface {
	// IsGroupSession mirrors `runtime.chat_stream.is_group_session` — backoff
	// only applies to group chats.
	IsGroupSession() bool
	// FocusModeActiveForCurrentChat mirrors
	// `_is_focus_mode_active_for_current_chat`.
	FocusModeActiveForCurrentChat() bool
	// DeferMessageTurnCheck mirrors `_defer_message_turn_check`.
	DeferMessageTurnCheck(delaySeconds float64)
	// LogPrefix mirrors `runtime.log_prefix`.
	LogPrefix() string
}

// IdleBackoffController mirrors idle_backoff.IdleBackoffController.
//
// T1 修复：count/until 由引擎 goroutine（RecordCycleResult）与渠道读循环
// goroutine（ShouldDelay）并发读写，Python 靠 asyncio 串行，Go 显式加锁。
type IdleBackoffController struct {
	mu      sync.Mutex
	runtime IdleBackoffRuntime
	config  ReplyTimingBackoffConfig
	now     func() float64
	log     func(format string, args ...any)

	count int
	until float64
}

// NewIdleBackoffController builds the controller.
func NewIdleBackoffController(
	runtime IdleBackoffRuntime,
	config ReplyTimingBackoffConfig,
	now func() float64,
	log func(format string, args ...any),
) *IdleBackoffController {
	if now == nil {
		now = defaultNow
	}
	return &IdleBackoffController{runtime: runtime, config: config, now: now, log: log}
}

// BackoffSeconds mirrors `_get_backoff_seconds`:
// `min(cap, base * 2^(count - start))`, 0 before `start_count`.
func (c *IdleBackoffController) BackoffSeconds() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.backoffSecondsLocked()
}

func (c *IdleBackoffController) backoffSecondsLocked() float64 {
	baseSeconds := maxFloat(0.0, c.config.BaseSeconds)
	capSeconds := maxFloat(0.0, c.config.CapSeconds)
	if baseSeconds <= 0 || capSeconds <= 0 {
		return 0.0
	}
	startCount := c.config.StartCount
	if startCount < 1 {
		startCount = 1
	}
	if c.count < startCount {
		return 0.0
	}
	exponent := maxInt(0, c.count-startCount)
	return minFloat(capSeconds, baseSeconds*exp2(exponent))
}

// Count returns the consecutive idle count (diagnostics/tests).
func (c *IdleBackoffController) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// Until returns the current backoff deadline (0 when inactive).
func (c *IdleBackoffController) Until() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.until
}

// Reset mirrors IdleBackoffController.reset.
func (c *IdleBackoffController) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetLocked()
}

func (c *IdleBackoffController) resetLocked() {
	c.count = 0
	c.until = 0.0
}

// RecordCycleResult mirrors IdleBackoffController.record_cycle_result.
func (c *IdleBackoffController) RecordCycleResult(cycleEndReason string) {
	normalizedReason := trimSpaceString(cycleEndReason)
	if !IsIdleCycleReason(normalizedReason) {
		c.Reset()
		return
	}
	if !c.runtime.IsGroupSession() {
		c.Reset()
		return
	}

	c.mu.Lock()
	c.count++
	backoffSeconds := c.backoffSecondsLocked()
	if backoffSeconds <= 0 {
		c.until = 0.0
		c.mu.Unlock()
		return
	}
	c.until = c.now() + backoffSeconds
	count := c.count
	c.mu.Unlock()
	c.logf("%s 连续空闲退避已更新: 来源=planner 连续次数=%d 退避=%.2f 秒",
		c.runtime.LogPrefix(), count, backoffSeconds)
}

// ShouldDelay mirrors IdleBackoffController.should_delay.
func (c *IdleBackoffController) ShouldDelay(pendingCount int) bool {
	if c.runtime.FocusModeActiveForCurrentChat() {
		c.Reset()
		return false
	}
	if !c.runtime.IsGroupSession() {
		return false
	}

	c.mu.Lock()
	if c.until <= 0 {
		c.mu.Unlock()
		return false
	}
	remainingSeconds := c.until - c.now()
	if remainingSeconds <= 0 {
		c.until = 0.0
		c.mu.Unlock()
		return false
	}
	c.mu.Unlock()

	bypassPendingCount := maxInt(0, c.config.BypassPendingCount)
	if bypassPendingCount > 0 && pendingCount >= bypassPendingCount {
		c.logf("%s 空闲退避被待处理消息数绕过: 待处理=%d 阈值=%d",
			c.runtime.LogPrefix(), pendingCount, bypassPendingCount)
		return false
	}

	c.logf("%s 空闲退避中，延迟 %.2f 秒后再检查", c.runtime.LogPrefix(), remainingSeconds)
	// 对齐 Python should_delay 末尾的 `_defer_message_turn_check(remaining)`：
	// 睡够后重跑调度（P1-3 定时器在 runtime 侧实现）。
	c.runtime.DeferMessageTurnCheck(remainingSeconds)
	return true
}

func (c *IdleBackoffController) logf(format string, args ...any) {
	if c.log == nil {
		return
	}
	c.log(format, args...)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func defaultNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func exp2(exponent int) float64 { return math.Ldexp(1, exponent) }

func trimSpaceString(value string) string { return strings.TrimSpace(value) }

func maxFloat(left float64, right float64) float64 {
	if left > right {
		return left
	}
	return right
}

func minFloat(left float64, right float64) float64 {
	if left < right {
		return left
	}
	return right
}

func timeFromUnixSeconds(seconds float64) time.Time {
	whole := int64(seconds)
	nanos := int64((seconds - float64(whole)) * 1e9)
	return time.Unix(whole, nanos)
}
