package manager

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"oh-my-qqbot/internal/database"
	"oh-my-qqbot/internal/message"
)

// This file mirrors 参考/MaiBot/src/chat/message_receive/chat_manager.py
// (behaviour is authoritative there): ChatManager 负责管理所有聊天会话，
// BotChatSession 对齐 MaiChatSession + SessionContext。

// SessionContext mirrors SessionContext: 会话上下文（当前模板等）。
type SessionContext struct {
	// Message 是触发当前上下文的消息。
	Message *message.SessionMessage
	// TemplateName 是当前使用的回复模板。
	TemplateName string
}

// UpdateTemplate mirrors SessionContext.update_template.
func (c *SessionContext) UpdateTemplate(templateName string) {
	c.TemplateName = templateName
}

// BotChatSession mirrors BotChatSession（MaiChatSession + context/accept_format）。
type BotChatSession struct {
	SessionID           string
	Platform            string
	UserID              *string
	UserNickname        *string
	UserCardname        *string
	GroupID             *string
	GroupName           *string
	AccountID           *string
	Scope               *string
	CreatedTimestamp    time.Time
	LastActiveTimestamp time.Time

	// Context mirrors BotChatSession.context.
	Context *SessionContext
	// AcceptFormat mirrors accept_format.
	AcceptFormat []string
}

// IsGroupSession mirrors MaiChatSession.is_group_session: bool(group_id)。
func (s *BotChatSession) IsGroupSession() bool {
	return s.GroupID != nil && strings.TrimSpace(*s.GroupID) != ""
}

// CheckTypes mirrors check_types: 检查消息是否符合可接受类型列表。
func (s *BotChatSession) CheckTypes(types []string) bool {
	for _, t := range types {
		found := false
		for _, accepted := range s.AcceptFormat {
			if accepted == t {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// UpdateActiveTime mirrors update_active_time.
func (s *BotChatSession) UpdateActiveTime() {
	s.LastActiveTimestamp = time.Now()
}

// SetContext mirrors set_context.
func (s *BotChatSession) SetContext(sessionMessage *message.SessionMessage) {
	s.Context = &SessionContext{Message: sessionMessage}
}

// FromDBChatSession mirrors BotChatSession.from_db_instance.
func FromDBChatSession(record *database.ChatSession) *BotChatSession {
	return &BotChatSession{
		SessionID:           record.SessionID,
		Platform:            record.Platform,
		UserID:              record.UserID,
		UserNickname:        record.UserNickname,
		UserCardname:        record.UserCardname,
		GroupID:             record.GroupID,
		GroupName:           record.GroupName,
		AccountID:           record.AccountID,
		Scope:               record.Scope,
		CreatedTimestamp:    record.CreatedTimestamp,
		LastActiveTimestamp: record.LastActiveTimestamp,
	}
}

// ToDBChatSession mirrors BotChatSession.to_db_instance.
func (s *BotChatSession) ToDBChatSession() *database.ChatSession {
	return &database.ChatSession{
		SessionID:           s.SessionID,
		CreatedTimestamp:    s.CreatedTimestamp,
		LastActiveTimestamp: s.LastActiveTimestamp,
		UserID:              s.UserID,
		UserNickname:        s.UserNickname,
		UserCardname:        s.UserCardname,
		GroupID:             s.GroupID,
		GroupName:           s.GroupName,
		Platform:            s.Platform,
		AccountID:           s.AccountID,
		Scope:               s.Scope,
	}
}

// ChatManager mirrors ChatManager: 聊天管理器，负责管理所有聊天会话。
type ChatManager struct {
	mu                     sync.Mutex
	db                     *database.DB
	sessions               map[string]*BotChatSession
	lastMessages           map[string]*message.SessionMessage
	replyFrequencyProvider ReplyFrequencyProvider
}

// SetReplyFrequencyProvider injects fill_reply_frequency_if_available 的频率来源。
func (m *ChatManager) SetReplyFrequencyProvider(provider ReplyFrequencyProvider) {
	m.replyFrequencyProvider = provider
}

// NewChatManager mirrors ChatManager.__init__.
func NewChatManager(db *database.DB) *ChatManager {
	return &ChatManager{
		db:           db,
		sessions:     map[string]*BotChatSession{},
		lastMessages: map[string]*message.SessionMessage{},
	}
}

// Initialize mirrors initialize: 从数据库加载全部会话记录到内存中。
func (m *ChatManager) Initialize() error {
	if err := m.loadAllSessionsFromDB(); err != nil {
		return fmt.Errorf("初始化聊天管理器出现错误: %w", err)
	}
	return nil
}

// GetOrCreateSession mirrors get_or_create_session:
// 获取会话，如果不存在则创建一个新会话；一个封装方法。
func (m *ChatManager) GetOrCreateSession(
	platform string,
	userID string,
	groupID string,
	accountID string,
	scope string,
) (*BotChatSession, error) {
	sessionID, err := message.CalculateSessionIDStrings(platform, userID, groupID, accountID, scope)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if ok {
		routeMetadataChanged := applyRouteMetadata(session, accountID, scope)
		session.UpdateActiveTime()
		identityChanged := m.updateSessionIdentityFromLastMessage(sessionID, session)
		if routeMetadataChanged || identityChanged {
			m.saveSession(session)
		}
		return session, nil
	}

	// 内存没有就找 db
	if m.db != nil {
		record, dbErr := m.db.GetChatSession(sessionID)
		if dbErr != nil {
			return nil, fmt.Errorf("从数据库获取会话时发生错误: %w", dbErr)
		}
		if record != nil {
			session = FromDBChatSession(record)
			routeMetadataChanged := applyRouteMetadata(session, accountID, scope)
			identityChanged := false
			m.mu.Lock()
			if last, ok := m.lastMessages[sessionID]; ok {
				session.SetContext(last)
				identityChanged = updateSessionIdentity(session, last)
			}
			m.sessions[session.SessionID] = session
			m.mu.Unlock()
			if routeMetadataChanged || identityChanged {
				m.saveSession(session)
			}
			return session, nil
		}
	}

	// 都没有就创建新的
	newSession := &BotChatSession{
		SessionID:           sessionID,
		Platform:            platform,
		UserID:              stringPtrOrNull(userID),
		GroupID:             stringPtrOrNull(groupID),
		AccountID:           stringPtrOrNull(accountID),
		Scope:               stringPtrOrNull(scope),
		CreatedTimestamp:    time.Now(),
		LastActiveTimestamp: time.Now(),
	}
	m.mu.Lock()
	m.sessions[newSession.SessionID] = newSession
	if last, ok := m.lastMessages[newSession.SessionID]; ok {
		newSession.SetContext(last)
		updateSessionIdentity(newSession, last)
	}
	m.mu.Unlock()
	m.saveSession(newSession)
	return newSession, nil
}

// RegisterMessage mirrors register_message: 缓存最后一条消息并刷新会话身份。
func (m *ChatManager) RegisterMessage(sessionMessage *message.SessionMessage) error {
	platform := strings.TrimSpace(sessionMessage.Platform)
	if platform == "" {
		return fmt.Errorf("消息缺少平台信息")
	}
	userID := sessionMessage.MessageInfo.UserInfo.UserID
	var groupID string
	if sessionMessage.MessageInfo.GroupInfo != nil {
		groupID = sessionMessage.MessageInfo.GroupInfo.GroupID
	}
	accountID, scope := message.SessionRouteComponents(sessionMessage.MessageInfo.AdditionalConfig)
	sessionID, err := message.CalculateSessionIDStrings(platform, userID, groupID, accountID, scope)
	if err != nil {
		return err
	}
	sessionMessage.SessionID = sessionID // 确保消息的session_id正确设置

	m.mu.Lock()
	m.lastMessages[sessionID] = sessionMessage
	session := m.sessions[sessionID]
	m.mu.Unlock()

	if session != nil && updateSessionIdentity(session, sessionMessage) {
		m.saveSession(session)
	}
	return nil
}

// updateSessionIdentityFromLastMessage is the last-messages variant of
// _update_session_identity (get_or_create_session path).
func (m *ChatManager) updateSessionIdentityFromLastMessage(sessionID string, session *BotChatSession) bool {
	m.mu.Lock()
	last, ok := m.lastMessages[sessionID]
	m.mu.Unlock()
	if !ok {
		return false
	}
	session.SetContext(last)
	return updateSessionIdentity(session, last)
}

// normalizeIdentityText mirrors _normalize_identity_text.
func normalizeIdentityText(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil
	}
	return &normalized
}

// updateSessionIdentity mirrors _update_session_identity:
// 用真实入站消息补齐聊天流展示身份，群聊不保存最近发言人的用户信息。
func updateSessionIdentity(session *BotChatSession, sessionMessage *message.SessionMessage) bool {
	changed := false
	groupInfo := sessionMessage.MessageInfo.GroupInfo
	userInfo := sessionMessage.MessageInfo.UserInfo
	if groupInfo != nil {
		// 群名为空表示适配器未知（显式语义），不覆盖聊天流已记录的名称
		groupName := normalizeIdentityText(&groupInfo.GroupName)
		if groupName != nil && (session.GroupName == nil || *session.GroupName != *groupName) {
			session.GroupName = groupName
			changed = true
		}
		if session.UserID != nil {
			session.UserID = nil
			changed = true
		}
		if session.UserNickname != nil {
			session.UserNickname = nil
			changed = true
		}
		if session.UserCardname != nil {
			session.UserCardname = nil
			changed = true
		}
		return changed
	}

	userNickname := normalizeIdentityText(&userInfo.UserNickname)
	var cardname string
	if userInfo.UserCardname != nil {
		cardname = *userInfo.UserCardname
	}
	userCardname := normalizeIdentityText(&cardname)
	if userNickname != nil && (session.UserNickname == nil || *session.UserNickname != *userNickname) {
		session.UserNickname = userNickname
		changed = true
	}
	sessionCardname := ""
	if session.UserCardname != nil {
		sessionCardname = *session.UserCardname
	}
	if cardname != sessionCardname {
		session.UserCardname = userCardname
		changed = true
	}
	return changed
}

// applyRouteMetadata mirrors _apply_route_metadata.
func applyRouteMetadata(session *BotChatSession, accountID string, scope string) bool {
	changed := false
	normalizedAccountID := strings.TrimSpace(accountID)
	normalizedScope := strings.TrimSpace(scope)

	if normalizedAccountID != "" && (session.AccountID == nil || strings.TrimSpace(*session.AccountID) == "") {
		session.AccountID = &normalizedAccountID
		changed = true
	}
	if normalizedScope != "" && (session.Scope == nil || strings.TrimSpace(*session.Scope) == "") {
		session.Scope = &normalizedScope
		changed = true
	}
	return changed
}

// LoadAllSessionsFromDB mirrors load_all_sessions_from_db（公开形态）。
func (m *ChatManager) LoadAllSessionsFromDB() error {
	return m.loadAllSessionsFromDB()
}

// loadAllSessionsFromDB mirrors _load_sessions_from_db.
func (m *ChatManager) loadAllSessionsFromDB() error {
	if m.db == nil {
		return nil
	}
	records, err := m.db.ListRecentChatSessions(0)
	if err != nil {
		m.mu.Lock()
		m.sessions = map[string]*BotChatSession{}
		m.mu.Unlock()
		return fmt.Errorf("从数据库加载会话记录时发生错误: %w", err)
	}
	m.mu.Lock()
	m.sessions = map[string]*BotChatSession{}
	for _, record := range records {
		m.sessions[record.SessionID] = FromDBChatSession(record)
		if last, ok := m.lastMessages[record.SessionID]; ok {
			m.sessions[record.SessionID].SetContext(last)
		}
	}
	m.mu.Unlock()
	return nil
}

// SaveAllSessions mirrors save_all_sessions: 将内存中的全部会话记录保存到数据库。
func (m *ChatManager) SaveAllSessions() error {
	m.mu.Lock()
	all := make([]*BotChatSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		all = append(all, session)
	}
	m.mu.Unlock()
	for _, session := range all {
		if err := m.saveSession(session); err != nil {
			return err
		}
	}
	return nil
}

// RegularlySaveSessions mirrors regularly_save_sessions（调用方用 ticker 驱动）。
func (m *ChatManager) saveSession(session *BotChatSession) error {
	if m.db == nil {
		return nil
	}
	if err := m.db.UpsertChatSession(session.ToDBChatSession()); err != nil {
		return fmt.Errorf("保存会话记录到数据库时发生错误: %w", err)
	}
	return nil
}

// GetSessionName mirrors get_session_name.
func (m *ChatManager) GetSessionName(sessionID string) string {
	m.mu.Lock()
	session := m.sessions[sessionID]
	var contextMessage *message.SessionMessage
	if session != nil && session.Context != nil {
		contextMessage = session.Context.Message
	}
	m.mu.Unlock()

	if session == nil {
		return ""
	}
	if session.IsGroupSession() {
		if session.GroupName != nil && strings.TrimSpace(*session.GroupName) != "" {
			return *session.GroupName
		}
		if contextMessage != nil && contextMessage.MessageInfo.GroupInfo != nil {
			return contextMessage.MessageInfo.GroupInfo.GroupName
		}
		return ""
	}
	if session.UserNickname != nil && strings.TrimSpace(*session.UserNickname) != "" {
		return *session.UserNickname + "的私聊"
	}
	if contextMessage != nil {
		return contextMessage.MessageInfo.UserInfo.UserNickname + "的私聊"
	}
	return ""
}

// GetSessionBySessionID mirrors get_session_by_session_id.
func (m *ChatManager) GetSessionBySessionID(sessionID string) *BotChatSession {
	m.mu.Lock()
	session := m.sessions[sessionID]
	m.mu.Unlock()
	if session == nil {
		return nil
	}
	m.mu.Lock()
	last, ok := m.lastMessages[sessionID]
	m.mu.Unlock()
	if ok {
		session.SetContext(last)
	}
	return session
}

// GetExistingSessionBySessionID mirrors get_existing_session_by_session_id:
// 内存未命中时从数据库加载。
func (m *ChatManager) GetExistingSessionBySessionID(sessionID string) *BotChatSession {
	normalized := strings.TrimSpace(sessionID)
	if normalized == "" {
		return nil
	}
	if session := m.GetSessionBySessionID(normalized); session != nil {
		return session
	}
	if m.db == nil {
		return nil
	}
	record, err := m.db.GetChatSession(normalized)
	if err != nil || record == nil {
		return nil
	}
	session := FromDBChatSession(record)
	m.mu.Lock()
	m.sessions[session.SessionID] = session
	if last, ok := m.lastMessages[session.SessionID]; ok {
		session.SetContext(last)
	}
	m.mu.Unlock()
	return session
}

// SessionCount 返回内存中的会话数量（诊断用）。
func (m *ChatManager) SessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// stringPtrOrNull maps "" → nil。
func stringPtrOrNull(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}
