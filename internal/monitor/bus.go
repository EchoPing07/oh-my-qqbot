// Package monitor 实现观察事件 bus（对齐 参考MaiBot/src/maisaka/monitor/events.py
// 的 emit_* 事件面与 WebSocket 广播语义）。
//
// 设计要点（REWRITE_PLAN §6.16）：
//   - 进程级观察事件总线，聊天运行时调用 emit 辅助函数，WebUI WS hub 订阅；
//   - 无订阅者时零开销（原子计数快速路径，不加锁、不构造切片）；
//   - 非阻塞发送：慢消费者（订阅 channel 满）直接丢弃并计数，不阻塞聊天主链路。
package monitor

import (
	"sync"
	"sync/atomic"

	contracts "oh-my-qqbot/internal"
)

// Bus 是 internal.EventBus 的默认实现。
type Bus struct {
	// subCount 为当前订阅者数量（快速路径：为 0 时 Emit 零开销直接返回）。
	subCount atomic.Int64

	mu     sync.RWMutex
	subs   map[int64]*subscriber
	nextID int64

	// dropped 累计因背压被丢弃的事件数（诊断用）。
	dropped atomic.Int64
}

type subscriber struct {
	ch chan contracts.Event
}

// NewBus 创建事件 bus。
func NewBus() *Bus {
	return &Bus{subs: map[int64]*subscriber{}}
}

// Emit 发布一个事件。零订阅时为无锁快速路径；有订阅者时对每个订阅者做
// 非阻塞发送，channel 满即丢弃（防慢消费者拖垮聊天主链路）。
//
// 并发安全（回归 P0）：发送必须在 RLock 持锁期间完成。cancel 持写锁先
// delete 再 close，Emit 持读锁时二者互斥——凡是在锁内遍历到的订阅者都
// 尚未被 close，从而杜绝「send on closed channel」panic。若在锁外发送，
// Emit 复制 channel 与 cancel 关闭 channel 之间没有同步点，必然竞态。
func (b *Bus) Emit(ev contracts.Event) {
	if b == nil {
		return
	}
	if b.subCount.Load() == 0 {
		return // 零订阅零开销
	}
	b.mu.RLock()
	if len(b.subs) == 0 {
		b.mu.RUnlock()
		return
	}
	dropped := 0
	for _, sub := range b.subs {
		select {
		case sub.ch <- ev:
		default:
			// 慢消费者：丢弃本次事件，保证 Emit 永不阻塞。
			dropped++
		}
	}
	b.mu.RUnlock()
	if dropped > 0 {
		b.dropped.Add(int64(dropped))
	}
}

// Subscribe 注册一个订阅者，返回只读 channel 与取消函数。
// buffer 为 channel 容量（<=0 时取 1）。
func (b *Bus) Subscribe(buffer int) (<-chan contracts.Event, func()) {
	if buffer <= 0 {
		buffer = 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	sub := &subscriber{ch: make(chan contracts.Event, buffer)}
	b.subs[id] = sub
	b.subCount.Store(int64(len(b.subs)))

	cancel := func() {
		// 写锁与 Emit 的读锁互斥，且必须先 delete 再 close：保证 Emit 在
		// 读锁内遍历到的订阅者 channel 都还未关闭（发送不会落在已关闭
		// channel 上），订阅方仍可用 range + close 语义正常退出。
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[id]; !ok {
			return
		}
		delete(b.subs, id)
		close(sub.ch)
		b.subCount.Store(int64(len(b.subs)))
	}
	return sub.ch, cancel
}

// Dropped 返回因背压被丢弃的事件总数（诊断用）。
func (b *Bus) Dropped() int64 { return b.dropped.Load() }

// SubscriberCount 返回当前订阅者数量（诊断用）。
func (b *Bus) SubscriberCount() int { return int(b.subCount.Load()) }
