// chat 路由 — 会话列表/消息查询（只读）。
// 对齐 routers/chat/routes.py 的 sessions/history 面（Go 版裁剪 adapter/policy 面；
// WEBUI_CHAT_PLATFORM 发言注入从未接线，已随轻量化移除）。
package routers

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

// chatSessionItem 对齐 sessions 列表行。
type chatSessionItem struct {
	SessionID    string `json:"session_id"`
	Platform     string `json:"platform"`
	IsGroup      bool   `json:"is_group"`
	GroupID      string `json:"group_id,omitempty"`
	GroupName    string `json:"group_name,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	UserNickname string `json:"user_nickname,omitempty"`
	CreatedAt    string `json:"created_at"`
	LastActive   string `json:"last_active_at"`
}

// handleChatSessions 对齐 GET /api/chat/sessions（最近活跃会话列表）。
func handleChatSessions(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		limit := queryInt(r, "limit", 100)
		rows, err := d.DB.Read().Query(`
			SELECT session_id, platform, user_id, user_nickname, group_id, group_name,
				created_timestamp, last_active_timestamp
			FROM chat_sessions ORDER BY last_active_timestamp DESC LIMIT ?`, limit)
		if err != nil {
			fail(w, http.StatusInternalServerError, "查询会话失败: "+err.Error())
			return
		}
		defer rows.Close()
		items := []chatSessionItem{}
		for rows.Next() {
			var s chatSessionItem
			var userID, nickname, groupID, groupName sql.NullString
			var created, lastActive string
			if err := rows.Scan(&s.SessionID, &s.Platform, &userID, &nickname,
				&groupID, &groupName, &created, &lastActive); err != nil {
				continue
			}
			s.UserID = userID.String
			s.UserNickname = nickname.String
			s.GroupID = groupID.String
			s.GroupName = groupName.String
			s.IsGroup = groupID.String != ""
			s.CreatedAt = created
			s.LastActive = lastActive
			items = append(items, s)
		}
		ok(w, map[string]any{"sessions": items, "count": len(items)})
	}
}

// chatMessageItem 对齐 history 消息行（raw_content JSON 序列化的组件序列）。
type chatMessageItem struct {
	MessageID    string          `json:"message_id"`
	Timestamp    string          `json:"time"`
	Platform     string          `json:"platform"`
	UserID       string          `json:"user_id"`
	UserNickname string          `json:"user_nickname"`
	GroupID      string          `json:"group_id,omitempty"`
	SessionID    string          `json:"session_id"`
	IsEmoji      bool            `json:"is_emoji"`
	IsPicture    bool            `json:"is_picture"`
	IsMentioned  bool            `json:"is_mentioned"`
	IsAt         bool            `json:"is_at"`
	ReplyTo      string          `json:"reply_to,omitempty"`
	Text         string          `json:"text"`
	RawContent   json.RawMessage `json:"raw_content,omitempty"`
}

// handleChatHistory 对齐 GET /api/chat/history（session_id 必填，最近 limit 条升序）。
func handleChatHistory(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.DB == nil {
			fail(w, http.StatusServiceUnavailable, "数据库未启用")
			return
		}
		sessionID := r.URL.Query().Get("session_id")
		if sessionID == "" {
			fail(w, http.StatusBadRequest, "缺少 session_id 参数")
			return
		}
		limit := queryInt(r, "limit", 200)
		if limit <= 0 || limit > 2000 {
			limit = 200
		}
		rows, err := d.DB.Read().Query(`
			SELECT message_id, timestamp, platform, user_id, user_nickname, group_id,
				session_id, is_emoji, is_picture, is_mentioned, is_at, reply_to,
				processed_plain_text, raw_content
			FROM (SELECT * FROM mai_messages WHERE session_id = ?
				ORDER BY timestamp DESC, id DESC LIMIT ?)
			ORDER BY timestamp ASC, id ASC`, sessionID, limit)
		if err != nil {
			fail(w, http.StatusInternalServerError, "查询消息失败: "+err.Error())
			return
		}
		defer rows.Close()
		items := []chatMessageItem{}
		for rows.Next() {
			var m chatMessageItem
			var groupID, replyTo, plain, raw sql.NullString
			if err := rows.Scan(&m.MessageID, &m.Timestamp, &m.Platform, &m.UserID,
				&m.UserNickname, &groupID, &m.SessionID, &m.IsEmoji, &m.IsPicture,
				&m.IsMentioned, &m.IsAt, &replyTo, &plain, &raw); err != nil {
				continue
			}
			m.GroupID = groupID.String
			m.ReplyTo = replyTo.String
			m.Text = plain.String
			if raw.Valid && raw.String != "" && raw.String != "{}" {
				m.RawContent = json.RawMessage(raw.String)
			}
			items = append(items, m)
		}
		ok(w, map[string]any{"messages": items, "count": len(items), "session_id": sessionID})
	}
}
