// 数据模型与基础访问函数 — 对齐 参考MaiBot/src/common/database/database_model.py。
// raw_content 以 JSON 文本存储 MessageSequence（REWRITE_PLAN：msgpack→JSON）。
package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// ---------------------------------------------------------------------------
// Messages — 对齐 database_model.Messages
// ---------------------------------------------------------------------------

// Message 对齐 database_model.Messages（表 mai_messages）。
type Message struct {
	ID                 int64
	MessageID          string  // 消息 id
	Timestamp          time.Time
	Platform           string
	UserID             string
	UserNickname       string
	UserCardname       *string
	GroupID            *string
	GroupName          *string
	IsMentioned        bool
	IsAt               bool
	SessionID          string
	ReplyTo            *string
	IsEmoji            bool
	IsPicture          bool
	IsCommand          bool
	IsNotify           bool
	RawContent         string  // 原 msgpack BLOB → JSON 文本（MessageSequence 序列化）
	ProcessedPlainText *string
	AdditionalConfig   *string // 额外配置，JSON 格式存储
	ReplyFrequency     *float64
}

// InsertMessage 插入一条消息，返回自增 id。
func (db *DB) InsertMessage(m *Message) (int64, error) {
	res, err := db.write.Exec(`
		INSERT INTO mai_messages (
			message_id, timestamp, platform, user_id, user_nickname, user_cardname,
			group_id, group_name, is_mentioned, is_at, session_id, reply_to,
			is_emoji, is_picture, is_command, is_notify, raw_content,
			processed_plain_text, additional_config, reply_frequency
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.MessageID, formatTime(m.Timestamp), m.Platform, m.UserID, m.UserNickname,
		m.UserCardname, m.GroupID, m.GroupName, boolToInt(m.IsMentioned), boolToInt(m.IsAt),
		m.SessionID, m.ReplyTo, boolToInt(m.IsEmoji), boolToInt(m.IsPicture),
		boolToInt(m.IsCommand), boolToInt(m.IsNotify), m.RawContent,
		m.ProcessedPlainText, m.AdditionalConfig, m.ReplyFrequency,
	)
	if err != nil {
		return 0, fmt.Errorf("插入消息失败: %w", err)
	}
	return res.LastInsertId()
}

// messageColumns 是 QueryMessages/scanMessage 共用的列清单。
const messageColumns = `id, message_id, timestamp, platform, user_id, user_nickname, user_cardname,
	group_id, group_name, is_mentioned, is_at, session_id, reply_to,
	is_emoji, is_picture, is_command, is_notify, raw_content,
	processed_plain_text, additional_config, reply_frequency`

func scanMessage(scanner interface{ Scan(...any) error }) (*Message, error) {
	m := &Message{}
	var ts string
	err := scanner.Scan(
		&m.ID, &m.MessageID, &ts, &m.Platform, &m.UserID, &m.UserNickname, &m.UserCardname,
		&m.GroupID, &m.GroupName, &m.IsMentioned, &m.IsAt, &m.SessionID, &m.ReplyTo,
		&m.IsEmoji, &m.IsPicture, &m.IsCommand, &m.IsNotify, &m.RawContent,
		&m.ProcessedPlainText, &m.AdditionalConfig, &m.ReplyFrequency,
	)
	if err != nil {
		return nil, err
	}
	m.IsMentioned = intToBool(m.IsMentioned)
	m.IsAt = intToBool(m.IsAt)
	m.IsEmoji = intToBool(m.IsEmoji)
	m.IsPicture = intToBool(m.IsPicture)
	m.IsCommand = intToBool(m.IsCommand)
	m.IsNotify = intToBool(m.IsNotify)
	if m.Timestamp, err = parseTime(ts); err != nil {
		return nil, err
	}
	return m, nil
}

// intToBool 把 SQLite 整数布转为 bool（Scan 到 any 时的类型可能是 int64/int/float64）。
func intToBool(v bool) bool { return v }

// QueryMessagesBySession 按会话取最近 n 条消息（时间升序），对齐 ChatManager 历史读取。
func (db *DB) QueryMessagesBySession(sessionID string, limit int) ([]*Message, error) {
	if limit <= 0 {
		limit = 1000
	}
	// 先按时间倒序取最近 limit 条，再反转为升序（对齐 MaiBot 的「最近 N 条」语义）。
	rows, err := db.read.Query(
		`SELECT `+messageColumns+` FROM mai_messages WHERE session_id = ? ORDER BY timestamp DESC, id DESC LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("查询会话消息失败: %w", err)
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			// 逐行容错（B2）：单行坏记录（如历史脏数据/格式演化的旧时间戳）
			// 只 Warn 跳过，不让一行毁掉整段会话历史。
			slog.Warn("会话消息行解析失败，已跳过", "session_id", sessionID, "err", err)
			continue
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// GetMessageByID 按消息 id 精确查询（同 id 可能多条：不同平台），返回第一条。
func (db *DB) GetMessageByID(messageID string) (*Message, error) {
	row := db.read.QueryRow(
		`SELECT `+messageColumns+` FROM mai_messages WHERE message_id = ? ORDER BY id LIMIT 1`, messageID)
	m, err := scanMessage(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// CountMessagesBySession 统计会话消息数。
func (db *DB) CountMessagesBySession(sessionID string) (int64, error) {
	var n int64
	err := db.read.QueryRow(`SELECT COUNT(*) FROM mai_messages WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// ModelUsage — 对齐 database_model.ModelUsage
// ---------------------------------------------------------------------------

// ModelUsage 对齐 database_model.ModelUsage（表 llm_usage）。
type ModelUsage struct {
	ID                  int64
	ModelName           string  // 模型实际名称（供应商名称）
	ModelAssignName     *string // 模型分配名称（用户自定义名称）
	ModelAPIProviderName string
	SessionID           string  // 对应真实聊天流；非聊天上下文为空字符串
	TaskName            *string
	RequestType         string  // 内部请求类型
	TimeCost            float64 // 秒
	Timestamp           time.Time
	PromptTokens        int
	CompletionTokens    int
	TotalTokens         int
	PromptCacheEnabled  bool
	PromptCacheHitTokens int
	PromptCacheMissTokens int
	Cost                float64 // 元
}

// InsertUsage 写入一条模型用量记录。
func (db *DB) InsertUsage(u *ModelUsage) (int64, error) {
	res, err := db.write.Exec(`
		INSERT INTO llm_usage (
			model_name, model_assign_name, model_api_provider_name, session_id, task_name,
			request_type, time_cost, timestamp, prompt_tokens, completion_tokens, total_tokens,
			prompt_cache_enabled, prompt_cache_hit_tokens, prompt_cache_miss_tokens, cost
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.ModelName, u.ModelAssignName, u.ModelAPIProviderName, u.SessionID, u.TaskName,
		u.RequestType, u.TimeCost, formatTime(u.Timestamp), u.PromptTokens, u.CompletionTokens,
		u.TotalTokens, boolToInt(u.PromptCacheEnabled), u.PromptCacheHitTokens,
		u.PromptCacheMissTokens, u.Cost,
	)
	if err != nil {
		return 0, fmt.Errorf("插入 llm_usage 失败: %w", err)
	}
	return res.LastInsertId()
}

// SumUsageCost 汇总全部费用（供统计路由）。
func (db *DB) SumUsageCost() (float64, error) {
	var total sql.NullFloat64
	err := db.read.QueryRow(`SELECT SUM(cost) FROM llm_usage`).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Float64, nil
}

// ---------------------------------------------------------------------------
// Images — 对齐 database_model.Images
// ---------------------------------------------------------------------------

// Image 对齐 database_model.Images（表情包与图片共表）。
type Image struct {
	ID            int64
	ImageHash     string // sha256，唯一 ID
	Description   string
	FullPath      string
	ImageType     string // "emoji" | "image"
	QueryCount    int
	IsRegistered  bool
	IsBanned      bool
	NoFileFlag    bool
	RecordTime    time.Time
	RegisterTime  *time.Time
	LastUsedTime  *time.Time
	VLMProcessed  bool
}

// InsertImage 插入图片/表情记录。
func (db *DB) InsertImage(img *Image) (int64, error) {
	if img.ImageType == "" {
		img.ImageType = "emoji"
	}
	res, err := db.write.Exec(`
		INSERT INTO images (
			image_hash, description, full_path, image_type, query_count,
			is_registered, is_banned, no_file_flag, record_time, register_time,
			last_used_time, vlm_processed
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		img.ImageHash, img.Description, img.FullPath, img.ImageType, img.QueryCount,
		boolToInt(img.IsRegistered), boolToInt(img.IsBanned), boolToInt(img.NoFileFlag),
		formatTime(img.RecordTime), formatNullableTime(img.RegisterTime),
		formatNullableTime(img.LastUsedTime), boolToInt(img.VLMProcessed),
	)
	if err != nil {
		return 0, fmt.Errorf("插入 images 失败: %w", err)
	}
	return res.LastInsertId()
}

// GetImageByHash 按 sha256 查询图片记录。
func (db *DB) GetImageByHash(hash string) (*Image, error) {
	row := db.read.QueryRow(`
		SELECT id, image_hash, description, full_path, image_type, query_count,
			is_registered, is_banned, no_file_flag, record_time, register_time,
			last_used_time, vlm_processed
		FROM images WHERE image_hash = ? ORDER BY id LIMIT 1`, hash)
	img := &Image{}
	var recordTime string
	var registerTime, lastUsedTime sql.NullString
	var imageType string
	err := row.Scan(&img.ID, &img.ImageHash, &img.Description, &img.FullPath, &imageType,
		&img.QueryCount, &img.IsRegistered, &img.IsBanned, &img.NoFileFlag,
		&recordTime, &registerTime, &lastUsedTime, &img.VLMProcessed)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	img.ImageType = imageType
	if img.RecordTime, err = parseTime(recordTime); err != nil {
		return nil, err
	}
	if registerTime.Valid {
		if t, err := parseTime(registerTime.String); err == nil {
			img.RegisterTime = &t
		}
	}
	if lastUsedTime.Valid {
		if t, err := parseTime(lastUsedTime.String); err == nil {
			img.LastUsedTime = &t
		}
	}
	return img, nil
}

// TouchImageUsed 更新 last_used_time 与 query_count（表情包选择路径）。
func (db *DB) TouchImageUsed(id int64, now time.Time) error {
	_, err := db.write.Exec(
		`UPDATE images SET last_used_time = ?, query_count = query_count + 1 WHERE id = ?`,
		formatTime(now), id)
	return err
}

// CountRegisteredImages 统计已注册表情数（对齐 max_reg_num 检查）。
func (db *DB) CountRegisteredImages() (int64, error) {
	var n int64
	err := db.read.QueryRow(`SELECT COUNT(*) FROM images WHERE is_registered = 1 AND image_type = 'emoji'`).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// ToolRecord — 对齐 database_model.ToolRecord
// ---------------------------------------------------------------------------

// ToolRecord 对齐 database_model.ToolRecord。
type ToolRecord struct {
	ID            int64
	ToolID        string
	Timestamp     time.Time
	SessionID     string
	ToolName      string
	ToolReasoning *string
	ToolData      *string // JSON 格式存储
}

// InsertToolRecord 插入工具调用记录。
func (db *DB) InsertToolRecord(r *ToolRecord) (int64, error) {
	res, err := db.write.Exec(`
		INSERT INTO tool_records (tool_id, timestamp, session_id, tool_name, tool_reasoning, tool_data)
		VALUES (?,?,?,?,?,?)`,
		r.ToolID, formatTime(r.Timestamp), r.SessionID, r.ToolName, r.ToolReasoning, r.ToolData,
	)
	if err != nil {
		return 0, fmt.Errorf("插入 tool_records 失败: %w", err)
	}
	return res.LastInsertId()
}

// ---------------------------------------------------------------------------
// Expression — 对齐 database_model.Expression
// ---------------------------------------------------------------------------

// Expression 对齐 database_model.Expression。
type Expression struct {
	ID             int64
	Situation      string
	Style          string
	ContentList    string // JSON
	Count          int
	LastActiveTime time.Time
	CreateTime     time.Time
	SessionID      *string // 区分全局/会话表达
	Checked        bool
	ModifiedBy     *string // AI | USER
}

// UpsertExpression 按 (situation, style, session_id) 命中则 count+1，否则插入。
func (db *DB) UpsertExpression(e *Expression) error {
	sessionKey := ""
	if e.SessionID != nil {
		sessionKey = *e.SessionID
	}
	return db.WithWriteTx(func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRow(
			`SELECT id FROM expressions WHERE situation = ? AND style = ? AND COALESCE(session_id,'') = ?`,
			e.Situation, e.Style, sessionKey).Scan(&id)
		switch {
		case err == sql.ErrNoRows:
			_, err = tx.Exec(`
				INSERT INTO expressions (situation, style, content_list, count, last_active_time, create_time, session_id, checked, modified_by)
				VALUES (?,?,?,?,?,?,?,?,?)`,
				e.Situation, e.Style, e.ContentList, e.Count, formatTime(e.LastActiveTime),
				formatTime(e.CreateTime), e.SessionID, boolToInt(e.Checked), e.ModifiedBy)
			return err
		case err != nil:
			return err
		default:
			_, err = tx.Exec(
				`UPDATE expressions SET count = count + 1, last_active_time = ? WHERE id = ?`,
				formatTime(e.LastActiveTime), id)
			return err
		}
	})
}

// QueryExpressionsBySituation 按情境查询候选表达（表达选择器路径）。
func (db *DB) QueryExpressionsBySituation(situation string, limit int) ([]*Expression, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.read.Query(`
		SELECT id, situation, style, content_list, count, last_active_time, create_time, session_id, checked, modified_by
		FROM expressions WHERE situation = ? AND checked = 1
		ORDER BY count DESC, last_active_time DESC LIMIT ?`, situation, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Expression
	for rows.Next() {
		e := &Expression{}
		var lastActive, create string
		var checked bool
		if err := rows.Scan(&e.ID, &e.Situation, &e.Style, &e.ContentList, &e.Count,
			&lastActive, &create, &e.SessionID, &checked, &e.ModifiedBy); err != nil {
			return nil, err
		}
		e.Checked = checked
		if e.LastActiveTime, err = parseTime(lastActive); err != nil {
			return nil, err
		}
		if e.CreateTime, err = parseTime(create); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Jargon — 对齐 database_model.Jargon
// ---------------------------------------------------------------------------

// Jargon 对齐 database_model.Jargon。
type Jargon struct {
	ID                 int64
	Content            string
	EvidenceMessages   *string // [[{"platform":..,"message_id":..}]]
	Meaning            string
	SessionIDDict      string // {"session_id": count, ...}
	Count              int
	IsJargon           bool // false 表示非黑话
	IsComplete         bool // count > 100 后不再推断
	IsGlobal           bool
	LastInferenceCount int
	CreatedBy          string // AI | MANUAL
	CreatedTimestamp   time.Time
	UpdatedTimestamp   time.Time
}

// UpsertJargon 按 content 命中则合并计数，否则插入（对齐 jargon_learner 写库语义）。
func (db *DB) UpsertJargon(j *Jargon) error {
	return db.WithWriteTx(func(tx *sql.Tx) error {
		var id int64
		var count int
		err := tx.QueryRow(`SELECT id, count FROM jargons WHERE content = ?`, j.Content).Scan(&id, &count)
		switch {
		case err == sql.ErrNoRows:
			_, err = tx.Exec(`
				INSERT INTO jargons (content, evidence_messages, meaning, session_id_dict, count,
					is_jargon, is_complete, is_global, last_inference_count, created_by,
					created_timestamp, updated_timestamp)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
				j.Content, j.EvidenceMessages, j.Meaning, j.SessionIDDict, j.Count,
				boolToInt(j.IsJargon), boolToInt(j.IsComplete), boolToInt(j.IsGlobal),
				j.LastInferenceCount, j.CreatedBy,
				formatTime(j.CreatedTimestamp), formatTime(j.UpdatedTimestamp))
			return err
		case err != nil:
			return err
		default:
			_, err = tx.Exec(
				`UPDATE jargons SET count = count + ?, updated_timestamp = ? WHERE id = ?`,
				j.Count, formatTime(j.UpdatedTimestamp), id)
			return err
		}
	})
}

// GetJargonByContent 按内容查询黑话。
func (db *DB) GetJargonByContent(content string) (*Jargon, error) {
	row := db.read.QueryRow(`
		SELECT id, content, evidence_messages, meaning, session_id_dict, count,
			is_jargon, is_complete, is_global, last_inference_count, created_by,
			created_timestamp, updated_timestamp
		FROM jargons WHERE content = ? ORDER BY id LIMIT 1`, content)
	j := &Jargon{}
	var created, updated string
	var isJargon, isComplete, isGlobal bool
	var evidence sql.NullString
	err := row.Scan(&j.ID, &j.Content, &evidence, &j.Meaning, &j.SessionIDDict, &j.Count,
		&isJargon, &isComplete, &isGlobal, &j.LastInferenceCount, &j.CreatedBy, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if evidence.Valid {
		s := evidence.String
		j.EvidenceMessages = &s
	}
	j.IsJargon, j.IsComplete, j.IsGlobal = isJargon, isComplete, isGlobal
	if j.CreatedTimestamp, err = parseTime(created); err != nil {
		return nil, err
	}
	if j.UpdatedTimestamp, err = parseTime(updated); err != nil {
		return nil, err
	}
	return j, nil
}

// ---------------------------------------------------------------------------
// PersonInfo — 对齐 database_model.PersonInfo
// ---------------------------------------------------------------------------

// PersonInfo 对齐 database_model.PersonInfo。
type PersonInfo struct {
	ID            int64
	IsKnown       bool
	PersonID      string // md5 派生
	PersonName    *string
	NameReason    *string
	Platform      string
	UserID        string
	UserNickname  string
	GroupCardname *string // JSON: [{"group_id": str, "group_cardname": str}]
	MemoryPoints  *string // JSON
	KnowCounts    int
	FirstKnownTime *time.Time
	LastKnownTime  *time.Time
}

// UpsertPersonInfo 按 person_id 命中则更新 know_counts/时间，否则插入。
func (db *DB) UpsertPersonInfo(p *PersonInfo) error {
	return db.WithWriteTx(func(tx *sql.Tx) error {
		var id int64
		var knowCounts int
		err := tx.QueryRow(`SELECT id, know_counts FROM person_info WHERE person_id = ?`, p.PersonID).Scan(&id, &knowCounts)
		switch {
		case err == sql.ErrNoRows:
			_, err = tx.Exec(`
				INSERT INTO person_info (is_known, person_id, person_name, name_reason, platform,
					user_id, user_nickname, group_cardname, memory_points, know_counts,
					first_known_time, last_known_time)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
				boolToInt(p.IsKnown), p.PersonID, p.PersonName, p.NameReason, p.Platform,
				p.UserID, p.UserNickname, p.GroupCardname, p.MemoryPoints, p.KnowCounts,
				formatNullableTime(p.FirstKnownTime), formatNullableTime(p.LastKnownTime))
			return err
		case err != nil:
			return err
		default:
			_, err = tx.Exec(`
				UPDATE person_info SET is_known = ?, person_name = ?, user_nickname = ?,
					group_cardname = COALESCE(?, group_cardname), memory_points = COALESCE(?, memory_points),
					know_counts = know_counts + 1, last_known_time = ?
				WHERE id = ?`,
				boolToInt(p.IsKnown), p.PersonName, p.UserNickname,
				p.GroupCardname, p.MemoryPoints, formatNullableTime(p.LastKnownTime), id)
			return err
		}
	})
}

// GetPersonInfoByID 按 person_id 查询。
func (db *DB) GetPersonInfoByID(personID string) (*PersonInfo, error) {
	row := db.read.QueryRow(`
		SELECT id, is_known, person_id, person_name, name_reason, platform, user_id,
			user_nickname, group_cardname, memory_points, know_counts, first_known_time, last_known_time
		FROM person_info WHERE person_id = ?`, personID)
	p := &PersonInfo{}
	var isKnown bool
	var personName, nameReason, cardname, memory sql.NullString
	var firstKnown, lastKnown sql.NullString
	err := row.Scan(&p.ID, &isKnown, &p.PersonID, &personName, &nameReason, &p.Platform,
		&p.UserID, &p.UserNickname, &cardname, &memory, &p.KnowCounts, &firstKnown, &lastKnown)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.IsKnown = isKnown
	if personName.Valid {
		s := personName.String
		p.PersonName = &s
	}
	if nameReason.Valid {
		s := nameReason.String
		p.NameReason = &s
	}
	if cardname.Valid {
		s := cardname.String
		p.GroupCardname = &s
	}
	if memory.Valid {
		s := memory.String
		p.MemoryPoints = &s
	}
	if err := parseNullableTime(firstKnown, &p.FirstKnownTime); err != nil {
		return nil, err
	}
	if err := parseNullableTime(lastKnown, &p.LastKnownTime); err != nil {
		return nil, err
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// ChatSession — 对齐 database_model.ChatSession
// ---------------------------------------------------------------------------

// ChatSession 对齐 database_model.ChatSession（表 chat_sessions）。
type ChatSession struct {
	ID                 int64
	SessionID          string
	CreatedTimestamp   time.Time
	LastActiveTimestamp time.Time
	UserID             *string
	UserNickname       *string
	UserCardname       *string
	GroupID            *string
	GroupName          *string
	Platform           string
	AccountID          *string
	Scope              *string
}

// UpsertChatSession 按 session_id 命中则刷新 last_active_timestamp，否则插入。
// 对齐 ChatManager 的 get_or_create + touch 语义。
func (db *DB) UpsertChatSession(s *ChatSession) error {
	return db.WithWriteTx(func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRow(`SELECT id FROM chat_sessions WHERE session_id = ?`, s.SessionID).Scan(&id)
		switch {
		case err == sql.ErrNoRows:
			res, err := tx.Exec(`
				INSERT INTO chat_sessions (session_id, created_timestamp, last_active_timestamp,
					user_id, user_nickname, user_cardname, group_id, group_name, platform, account_id, scope)
				VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
				s.SessionID, formatTime(s.CreatedTimestamp), formatTime(s.LastActiveTimestamp),
				s.UserID, s.UserNickname, s.UserCardname, s.GroupID, s.GroupName,
				s.Platform, s.AccountID, s.Scope)
			if err != nil {
				return err
			}
			s.ID, _ = res.LastInsertId()
			return nil
		case err != nil:
			return err
		default:
			_, err := tx.Exec(
				`UPDATE chat_sessions SET last_active_timestamp = ? WHERE id = ?`,
				formatTime(s.LastActiveTimestamp), id)
			s.ID = id
			return err
		}
	})
}

// chatSessionColumns 是会话查询共用的列清单。
const chatSessionColumns = `id, session_id, created_timestamp, last_active_timestamp,
	user_id, user_nickname, user_cardname, group_id, group_name, platform, account_id, scope`

func scanChatSession(scanner interface{ Scan(...any) error }) (*ChatSession, error) {
	s := &ChatSession{}
	var created, lastActive string
	err := scanner.Scan(&s.ID, &s.SessionID, &created, &lastActive,
		&s.UserID, &s.UserNickname, &s.UserCardname, &s.GroupID, &s.GroupName,
		&s.Platform, &s.AccountID, &s.Scope)
	if err != nil {
		return nil, err
	}
	if s.CreatedTimestamp, err = parseTime(created); err != nil {
		return nil, err
	}
	if s.LastActiveTimestamp, err = parseTime(lastActive); err != nil {
		return nil, err
	}
	return s, nil
}

// GetChatSession 按 session_id 查询会话。
func (db *DB) GetChatSession(sessionID string) (*ChatSession, error) {
	row := db.read.QueryRow(`SELECT `+chatSessionColumns+` FROM chat_sessions WHERE session_id = ?`, sessionID)
	s, err := scanChatSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

// ListRecentChatSessions 按最近活跃排序的会话列表（WebUI 聊天管理页）。
func (db *DB) ListRecentChatSessions(limit int) ([]*ChatSession, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.read.Query(
		`SELECT `+chatSessionColumns+` FROM chat_sessions ORDER BY last_active_timestamp DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ChatSession
	for rows.Next() {
		s, err := scanChatSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// raw_content JSON 辅助 — 对齐 msgpack MessageSequence 的 JSON 替代
// ---------------------------------------------------------------------------

// EncodeRawContent 把任意 MessageSequence 载荷序列化为 raw_content JSON 文本。
func EncodeRawContent(v any) (string, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("序列化 raw_content 失败: %w", err)
	}
	return string(buf), nil
}

// DecodeRawContent 从 raw_content JSON 反序列化 MessageSequence 载荷。
func DecodeRawContent(raw string, v any) error {
	if raw == "" {
		return nil
	}
	return json.Unmarshal([]byte(raw), v)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func formatNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func parseNullableTime(s sql.NullString, dst **time.Time) error {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return err
	}
	*dst = &t
	return nil
}
