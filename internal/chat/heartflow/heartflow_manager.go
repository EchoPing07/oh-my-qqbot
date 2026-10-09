package heartflow

import (
	"fmt"
	"sync"
	"time"

	chatmanager "oh-my-qqbot/internal/chat/manager"
	chatruntime "oh-my-qqbot/internal/chat/runtime"
)

// This file mirrors 参考/MaiBot/src/chat/heart_flow/heartflow_manager.py
// (behaviour is authoritative there): 管理 session 级别的 Maisaka 心流实例
// （LRU(100) + 24h 保活）。

// HeartflowActiveRetentionSeconds mirrors HEARTFLOW_ACTIVE_RETENTION_SECONDS.
const HeartflowActiveRetentionSeconds = 24 * 60 * 60

// HeartflowMaxActiveChats mirrors HEARTFLOW_MAX_ACTIVE_CHATS.
const HeartflowMaxActiveChats = 100

// RuntimeFactory 创建（并启动）一个 runtime 的注入边界：装配层提供
// runtime.Options 的具体填充（避免 heartflow 依赖 config）。
type RuntimeFactory func(sessionID string) (*chatruntime.Runtime, error)

// HeartflowManager mirrors HeartflowManager.
type HeartflowManager struct {
	mu sync.Mutex

	heartflowChatList map[string]*chatruntime.Runtime
	// LRU 顺序表（键与 heartflowChatList 同步维护）。
	lruOrder []string
	// chatCreateLocks mirrors _chat_create_locks（每会话创建锁）。
	chatCreateLocks map[string]*sync.Mutex
	// chatLastActiveAt mirrors _chat_last_active_at.
	chatLastActiveAt map[string]float64

	chatManager *chatmanager.ChatManager
	newRuntime  RuntimeFactory
	now         func() float64
	log         func(format string, args ...any)
}

// Options are the injected collaborators of HeartflowManager.
type Options struct {
	ChatManager *chatmanager.ChatManager
	NewRuntime  RuntimeFactory
	Now         func() float64
	Log         func(format string, args ...any)
}

// NewHeartflowManager mirrors HeartflowManager.__init__.
func NewHeartflowManager(options Options) *HeartflowManager {
	if options.Now == nil {
		options.Now = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	}
	if options.Log == nil {
		options.Log = func(string, ...any) {}
	}
	return &HeartflowManager{
		heartflowChatList: map[string]*chatruntime.Runtime{},
		chatCreateLocks:   map[string]*sync.Mutex{},
		chatLastActiveAt:  map[string]float64{},
		chatManager:       options.ChatManager,
		newRuntime:        options.NewRuntime,
		now:               options.Now,
		log:               options.Log,
	}
}

// GetOrCreateHeartflowChat mirrors get_or_create_heartflow_chat:
// 获取或创建指定会话对应的 Maisaka runtime。
func (m *HeartflowManager) GetOrCreateHeartflowChat(sessionID string) (*chatruntime.Runtime, error) {
	m.mu.Lock()
	if chat, ok := m.heartflowChatList[sessionID]; ok {
		m.touchChatLocked(sessionID)
		m.mu.Unlock()
		return chat, nil
	}
	createLock := m.chatCreateLocks[sessionID]
	if createLock == nil {
		createLock = &sync.Mutex{}
		m.chatCreateLocks[sessionID] = createLock
	}
	m.mu.Unlock()

	createLock.Lock()
	defer createLock.Unlock()

	m.mu.Lock()
	if chat, ok := m.heartflowChatList[sessionID]; ok {
		m.touchChatLocked(sessionID)
		m.mu.Unlock()
		return chat, nil
	}
	m.mu.Unlock()

	chatSession := m.chatManager.GetSessionBySessionID(sessionID)
	if chatSession == nil {
		return nil, fmt.Errorf("未找到 session_id=%s 对应的聊天流", sessionID)
	}

	newChat, err := m.newRuntime(sessionID)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.heartflowChatList[sessionID] = newChat
	m.lruOrder = append(m.lruOrder, sessionID)
	m.touchChatLocked(sessionID)
	m.mu.Unlock()
	m.evictOverLimitChats(sessionID)
	return newChat, nil
}

// touchChatLocked mirrors _touch_chat: 记录会话最近活跃时间，并维护 LRU 顺序
// （调用方须持有 m.mu）。
func (m *HeartflowManager) touchChatLocked(sessionID string) {
	m.chatLastActiveAt[sessionID] = m.now()
	for index, existing := range m.lruOrder {
		if existing == sessionID {
			m.lruOrder = append(m.lruOrder[:index], m.lruOrder[index+1:]...)
			break
		}
	}
	m.lruOrder = append(m.lruOrder, sessionID)
}

// evictOverLimitChats mirrors _evict_over_limit_chats:
// 当实例数量超过上限时，仅淘汰 24 小时内无消息的旧会话。
func (m *HeartflowManager) evictOverLimitChats(protectedSessionID string) {
	for {
		m.mu.Lock()
		if len(m.heartflowChatList) <= HeartflowMaxActiveChats {
			m.mu.Unlock()
			return
		}
		sessionID := m.findEvictableSessionIDLocked(protectedSessionID)
		m.mu.Unlock()
		if sessionID == "" {
			return
		}
		m.evictChat(sessionID, "cache_limit")
	}
}

// findEvictableSessionIDLocked mirrors _find_evictable_session_id:
// 按 LRU 查找超过活跃保护窗口的可淘汰会话（调用方须持有 m.mu）。
func (m *HeartflowManager) findEvictableSessionIDLocked(protectedSessionID string) string {
	expireBefore := m.now() - HeartflowActiveRetentionSeconds
	for _, sessionID := range m.lruOrder {
		if sessionID == protectedSessionID {
			continue
		}
		lastActiveAt := m.chatLastActiveAt[sessionID]
		if lastActiveAt <= expireBefore {
			return sessionID
		}
	}
	return ""
}

// evictChat mirrors _evict_chat: 停止并移除指定会话的心流实例。
func (m *HeartflowManager) evictChat(sessionID string, reason string) {
	m.mu.Lock()
	chat := m.heartflowChatList[sessionID]
	delete(m.heartflowChatList, sessionID)
	delete(m.chatLastActiveAt, sessionID)
	for index, existing := range m.lruOrder {
		if existing == sessionID {
			m.lruOrder = append(m.lruOrder[:index], m.lruOrder[index+1:]...)
			break
		}
	}
	lock := m.chatCreateLocks[sessionID]
	if lock != nil {
		// Go sync.Mutex 无 TryLock 语义对齐 Python `not lock.locked()` 的
		// 全量判断；创建锁仅在创建竞态中短暂持锁，直接移除安全
		//（后续 GetOrCreate 会重建）。
		delete(m.chatCreateLocks, sessionID)
	}
	m.mu.Unlock()

	if chat == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			m.log("淘汰心流聊天 %s 失败: %v", sessionID, r)
		}
	}()
	chat.Stop()
	m.log("已淘汰心流聊天 %s: reason=%s", sessionID, reason)
}

// ClearChatHistoryContext mirrors clear_chat_history_context:
// 停止并移除当前会话运行时，使其短期历史上下文立即失效。
func (m *HeartflowManager) ClearChatHistoryContext(sessionID string) bool {
	m.mu.Lock()
	_, exists := m.heartflowChatList[sessionID]
	m.mu.Unlock()
	if !exists {
		return false
	}
	m.evictChat(sessionID, "clear_context_command")
	return true
}

// AdjustTalkFrequency mirrors adjust_talk_frequency.
func (m *HeartflowManager) AdjustTalkFrequency(sessionID string, frequency float64) {
	m.mu.Lock()
	chat := m.heartflowChatList[sessionID]
	if chat != nil {
		m.touchChatLocked(sessionID)
	}
	m.mu.Unlock()
	if chat == nil {
		m.log("无法调整频率，未找到 session_id=%s 的聊天流", sessionID)
		return
	}
	chat.AdjustTalkFrequency(frequency)
	m.log("已调整聊天 %s 的说话频率为 %v", sessionID, frequency)
}

// GetChat 返回指定会话的 runtime（诊断/WebUI 用）。
func (m *HeartflowManager) GetChat(sessionID string) *chatruntime.Runtime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.heartflowChatList[sessionID]
}

// Len 返回当前实例数量（诊断用）。
func (m *HeartflowManager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.heartflowChatList)
}
