package replyeffect

import (
	"sort"
	"sync"
	"time"
)

// 测试用假时钟：AfterFunc 登记的计时器在 Advance 到点后同步触发。
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	nextID int
}

type fakeTimer struct {
	clock   *fakeClock
	due     time.Time
	fn      func()
	stopped bool
	fired   bool
	id      int
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, fn func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	timer := &fakeTimer{clock: c, due: c.now.Add(d), fn: fn, id: c.nextID}
	c.timers = append(c.timers, timer)
	return timer
}

// Advance 推进假时钟并把到点计时器按到期顺序触发（触发时已释放锁）。
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	due := make([]*fakeTimer, 0)
	rest := make([]*fakeTimer, 0)
	for _, timer := range c.timers {
		if timer.stopped || timer.fired {
			continue
		}
		if !timer.due.After(c.now) {
			due = append(due, timer)
			continue
		}
		rest = append(rest, timer)
	}
	c.timers = rest
	c.mu.Unlock()
	sort.Slice(due, func(i, j int) bool {
		if due[i].due.Equal(due[j].due) {
			return due[i].id < due[j].id
		}
		return due[i].due.Before(due[j].due)
	})
	for _, timer := range due {
		timer.fired = true
		timer.fn()
	}
}

// TimerCount 返回尚未触发的计时器数量（断言计时器被正确取消）。
func (c *fakeClock) TimerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, timer := range c.timers {
		if !timer.stopped && !timer.fired {
			count++
		}
	}
	return count
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}
