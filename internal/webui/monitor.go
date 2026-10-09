// MonitorBridge — 观察事件 bus → WS maisaka_monitor 域的桥。
// 订阅 internal.EventBus，按 Event.Kind 路由（对齐 events.py 的
// emit_* 事件名），并缓存最近 N 条 planner.progress 快照供
// /reasoning-process/snapshots 查询。
package webui

import (
	"sync"
	"sync/atomic"
	"time"

	contracts "oh-my-qqbot/internal"

	"oh-my-qqbot/internal/webui/routers"
)

// defaultBusBuffer 对齐订阅缓冲默认容量（事件丢弃语义由 bus 决定）。
const defaultBusBuffer = 256

// defaultSnapshotHistory 缓存的 planner.progress 快照条数。
const defaultSnapshotHistory = 200

// MonitorBridge 实现 routers.SnapshotSource 与 Broadcaster 的组合消费端。
type MonitorBridge struct {
	hub     routers.Broadcaster
	history int

	mu        sync.Mutex
	snapshots []routers.ReasoningSnapshot
	nextSeq   int64
	cancel    func()
}

// NewMonitorBridge 订阅事件 bus 并把事件路由到 WS 域（bus 为 nil 时空转）。
func NewMonitorBridge(bus contracts.EventBus, hub routers.Broadcaster, history int) *MonitorBridge {
	if history <= 0 {
		history = defaultSnapshotHistory
	}
	b := &MonitorBridge{hub: hub, history: history}
	if bus != nil {
		buffer := defaultBusBuffer
		ch, cancel := bus.Subscribe(buffer)
		b.cancel = cancel
		go b.consume(ch)
	}
	return b
}

// consume 消费事件循环：按 Kind 路由到 WS 域。
func (b *MonitorBridge) consume(ch <-chan contracts.Event) {
	for ev := range ch {
		b.handleEvent(ev)
	}
}

// handleEvent 按 Kind 路由（对齐 events.py 的 emit_* → WS 事件名映射）。
func (b *MonitorBridge) handleEvent(ev contracts.Event) {
	eventName := string(ev.Kind)
	switch ev.Kind {
	case contracts.EventPlannerProgress:
		b.cacheSnapshot(ev, eventName)
	case contracts.EventSessionStart:
		// session.start 同时驱动 stage.snapshot 的会话列表（简化面：直接透传）。
	default:
	}
	// 全部事件透传到 maisaka_monitor:main（对齐 broadcast_to_topic）。
	if b.hub != nil {
		b.hub.Broadcast(DomainMaisakaMonitor, TopicMain, eventName, ev.Payload)
	}
}

// cacheSnapshot 缓存 planner.progress 快照（环形上限 history）。
func (b *MonitorBridge) cacheSnapshot(ev contracts.Event, eventName string) {
	seq := atomic.AddInt64(&b.nextSeq, 1)
	snap := routers.ReasoningSnapshot{
		Seq:       seq,
		Kind:      eventName,
		SessionID: ev.SessionID,
		Time:      ev.Time,
		Payload:   ev.Payload,
	}
	if snap.Time.IsZero() {
		snap.Time = time.Now()
	}
	b.mu.Lock()
	b.snapshots = append(b.snapshots, snap)
	if len(b.snapshots) > b.history {
		b.snapshots = b.snapshots[len(b.snapshots)-b.history:]
	}
	b.mu.Unlock()
}

// RecentPlannerSnapshots 实现 routers.SnapshotSource（sessionID 为空返回全部）。
func (b *MonitorBridge) RecentPlannerSnapshots(sessionID string, limit int) []routers.ReasoningSnapshot {
	b.mu.Lock()
	all := make([]routers.ReasoningSnapshot, len(b.snapshots))
	copy(all, b.snapshots)
	b.mu.Unlock()
	var out []routers.ReasoningSnapshot
	for _, s := range all {
		if sessionID == "" || s.SessionID == sessionID {
			out = append(out, s)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Close 取消 bus 订阅。
func (b *MonitorBridge) Close() {
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
}
