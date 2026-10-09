package replyeffect

import (
	"context"
	"sync"
	"time"
)

// This file 提供任务书要求的「触发点接口」门面：
//
//	RegisterReply(sessionID, botReply, targetMsgID) → effect_id
//	EnqueueFollowUp(message)
//
// 对齐 参考/MaiBot/src/maisaka/runtime.py：
//   - runtime.track_reply_effect（reply 工具发送成功后调用）→ RegisterReply；
//   - runtime.register_message 里的
//     `asyncio.create_task(tracker.observe_user_message(message))` → EnqueueFollowUp
//     （异步投递，不阻塞消息主链路）。
//
// Manager 按 session_id 懒创建会话级 Tracker；实验性开关默认关（Enabled=false 时
// 两个入口都是零开销空操作，装配层无需分支）。

// DefaultFollowUpQueueSize 是会话随访队列容量（对齐 _prune_processed_message_cache 的
// 有界缓存思路；队列满时丢弃并记日志，不阻塞消息链路）。
const DefaultFollowUpQueueSize = 256

// ManagerConfig 汇总门面参数。
type ManagerConfig struct {
	// Tracker 是会话级 tracker 的模板配置（Enabled=false 时整个门面关闭）。
	Tracker TrackerConfig
	// RuntimeFor 提供会话的运行时信息（平台/群/会话名）；nil 时退化为仅 session_id。
	RuntimeFor func(sessionID string) Runtime
	// QueueSize 是随访消息队列容量（默认 256）。
	QueueSize int
}

// Manager 是跨会话的回复效果观察门面（装配层注入到聊天运行时）。
type Manager struct {
	cfg      ManagerConfig
	mu       sync.Mutex
	trackers map[string]*Tracker
	queues   map[string]chan *ObservedMessage
	closed   bool
	wg       sync.WaitGroup
}

// NewManager 构造门面（Enabled=false 时行为与未接入一致）。
func NewManager(cfg ManagerConfig) *Manager {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultFollowUpQueueSize
	}
	return &Manager{
		cfg:      cfg,
		trackers: map[string]*Tracker{},
		queues:   map[string]chan *ObservedMessage{},
	}
}

// Enabled 返回观察开关状态（构造参数，默认关）。
func (m *Manager) Enabled() bool { return m.cfg.Tracker.Enabled }

// RegisterReply 是 reply 工具发送成功后调用的登记入口。
// botReply 是拼接后的可见回复正文，targetMsgID 是这条回复所回应的消息 id。
// 返回 effect_id（未启用时返回空字符串）。
func (m *Manager) RegisterReply(ctx context.Context, sessionID, botReply, targetMsgID string) (string, error) {
	if !m.Enabled() {
		return "", nil
	}
	tracker := m.trackerFor(ctx, sessionID)
	record, err := tracker.RegisterReply(ctx, TrackReplyRequest{
		TargetMessageID: targetMsgID,
		ReplyText:       botReply,
		ReplySegments:   []string{botReply},
	})
	if err != nil {
		return "", err
	}
	if record == nil {
		return "", nil
	}
	return record.EffectID, nil
}

// RegisterReplyDetail 是完整登记入口（等价于 runtime.track_reply_effect 的入参面）。
func (m *Manager) RegisterReplyDetail(ctx context.Context, sessionID string, request TrackReplyRequest) (string, error) {
	if !m.Enabled() {
		return "", nil
	}
	tracker := m.trackerFor(ctx, sessionID)
	record, err := tracker.RegisterReply(ctx, request)
	if err != nil {
		return "", err
	}
	if record == nil {
		return "", nil
	}
	return record.EffectID, nil
}

// EnqueueFollowUp 把一条会话内后续用户消息投递给观察队列（异步处理，不阻塞调用方）。
// 队列满时丢弃（对齐 Python 后台任务不反压消息链路的语义）。
func (m *Manager) EnqueueFollowUp(message *ObservedMessage) {
	if !m.Enabled() || message == nil || message.SessionID == "" {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	queue, ok := m.queues[message.SessionID]
	if !ok {
		queue = make(chan *ObservedMessage, m.cfg.QueueSize)
		m.queues[message.SessionID] = queue
		m.wg.Add(1)
		go m.consume(message.SessionID, queue)
	}
	m.mu.Unlock()
	select {
	case queue <- message:
	default:
		m.cfg.Tracker.Logger.Warn("回复效果随访队列已满，丢弃本条观察消息",
			"session_id", message.SessionID, "message_id", message.MessageID)
	}
}

// FollowUpQueueLength 返回某会话随访队列的当前积压长度（测试/监控用）。
func (m *Manager) FollowUpQueueLength(sessionID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if queue, ok := m.queues[sessionID]; ok {
		return len(queue)
	}
	return 0
}

// TrackerFor 返回（或创建）会话 tracker；供 WebUI/测试读取记录。
func (m *Manager) TrackerFor(ctx context.Context, sessionID string) *Tracker {
	return m.trackerFor(ctx, sessionID)
}

// Records 返回某会话当前的观察记录（时间升序）。
func (m *Manager) Records(ctx context.Context, sessionID string) []*Record {
	if !m.Enabled() {
		return nil
	}
	return m.trackerFor(ctx, sessionID).Records()
}

// Drain 等待已投递的随访消息处理完毕（测试与停机使用）。
func (m *Manager) Drain() {
	for {
		m.mu.Lock()
		pending := 0
		for _, queue := range m.queues {
			pending += len(queue)
		}
		m.mu.Unlock()
		if pending == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// Close 停止门面：结算全部会话记录并关闭队列。
func (m *Manager) Close(ctx context.Context) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	queues := make([]chan *ObservedMessage, 0, len(m.queues))
	for _, queue := range m.queues {
		queues = append(queues, queue)
	}
	trackers := make([]*Tracker, 0, len(m.trackers))
	for _, tracker := range m.trackers {
		trackers = append(trackers, tracker)
	}
	m.mu.Unlock()
	for _, queue := range queues {
		close(queue)
	}
	m.wg.Wait()
	for _, tracker := range trackers {
		tracker.Close(ctx)
	}
}

func (m *Manager) trackerFor(ctx context.Context, sessionID string) *Tracker {
	m.mu.Lock()
	if tracker, ok := m.trackers[sessionID]; ok {
		m.mu.Unlock()
		return tracker
	}
	var runtime Runtime
	if m.cfg.RuntimeFor != nil {
		runtime = m.cfg.RuntimeFor(sessionID)
	}
	if runtime == nil {
		runtime = RuntimeFunc{SessionIDValue: sessionID}
	}
	tracker := NewTracker(runtime, m.cfg.Tracker)
	m.trackers[sessionID] = tracker
	m.mu.Unlock()
	tracker.Start(ctx)
	return tracker
}

func (m *Manager) consume(sessionID string, queue chan *ObservedMessage) {
	defer m.wg.Done()
	for message := range queue {
		m.mu.Lock()
		tracker := m.trackers[sessionID]
		m.mu.Unlock()
		if tracker != nil {
			tracker.ObserveUserMessage(context.Background(), message)
		}
	}
}
