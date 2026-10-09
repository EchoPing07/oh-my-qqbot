package transfer

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"oh-my-qqbot/internal/database"
)

// This file 把 internal/database 的各业务表适配为 transfer.Store。
//
// 导入语义（对齐 data_transfer.py 的「同名跳过、不覆盖冲突」）：
//   - overwrite=false（默认）：已存在同键记录时计入 Skipped；
//   - overwrite=true：已存在时整行更新并计入 Updated。
//
// 幂等键：messages=platform+message_id；persons=person_id；
// expressions=(situation, style, session_id)（对齐 UpsertExpression）；jargons=content；
// images=image_hash+image_type。

// DatabaseStore 是 internal/database 的 transfer.Store 适配器。
type DatabaseStore struct {
	db *database.DB
}

// NewDatabaseStore 创建全量数据导出/导入适配器。
func NewDatabaseStore(db *database.DB) *DatabaseStore { return &DatabaseStore{db: db} }

// ---------------------------------------------------------------------------
// messages
// ---------------------------------------------------------------------------

// ExportMessages implements Store（按时间升序；limit<=0 表示不限）。
func (s *DatabaseStore) ExportMessages(ctx context.Context, limit int) ([]MessageRecord, error) {
	query := `
		SELECT message_id, timestamp, platform, user_id, user_nickname, user_cardname,
			group_id, group_name, is_mentioned, is_at, session_id, reply_to,
			is_emoji, is_picture, is_command, is_notify, raw_content,
			processed_plain_text, additional_config, reply_frequency
		FROM mai_messages ORDER BY timestamp ASC, id ASC`
	args := []any{}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Read().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("导出消息失败: %w", err)
	}
	defer rows.Close()
	records := make([]MessageRecord, 0)
	for rows.Next() {
		record, err := scanMessageRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// ImportMessages implements Store.
func (s *DatabaseStore) ImportMessages(ctx context.Context, records []MessageRecord, overwrite bool) (ImportStat, error) {
	stat := ImportStat{}
	for _, record := range records {
		messageID := strings.TrimSpace(record.MessageID)
		if messageID == "" {
			return stat, fmt.Errorf("消息记录缺少 message_id")
		}
		exists, err := s.messageExists(ctx, record.Platform, messageID)
		if err != nil {
			return stat, err
		}
		if exists && !overwrite {
			stat.Skipped++
			continue
		}
		timestamp, err := parseTransferTime(record.Timestamp)
		if err != nil {
			return stat, fmt.Errorf("消息 %s 的 timestamp 非法: %w", messageID, err)
		}
		rawContent := record.RawContent
		if strings.TrimSpace(rawContent) == "" {
			rawContent = "{}"
		}
		err = s.db.WithWriteTx(func(tx *sql.Tx) error {
			if exists {
				_, err := tx.Exec(`
					UPDATE mai_messages SET timestamp = ?, user_id = ?, user_nickname = ?, user_cardname = ?,
						group_id = ?, group_name = ?, is_mentioned = ?, is_at = ?, session_id = ?, reply_to = ?,
						is_emoji = ?, is_picture = ?, is_command = ?, is_notify = ?, raw_content = ?,
						processed_plain_text = ?, additional_config = ?, reply_frequency = ?
					WHERE platform = ? AND message_id = ?`,
					formatTransferTime(timestamp), record.UserID, record.UserNickname, record.UserCardname,
					record.GroupID, record.GroupName, boolToInt(record.IsMentioned), boolToInt(record.IsAt),
					record.SessionID, record.ReplyTo, boolToInt(record.IsEmoji), boolToInt(record.IsPicture),
					boolToInt(record.IsCommand), boolToInt(record.IsNotify), rawContent,
					record.ProcessedPlainText, record.AdditionalConfig, record.ReplyFrequency,
					record.Platform, messageID)
				return err
			}
			_, err := tx.Exec(`
				INSERT INTO mai_messages (message_id, timestamp, platform, user_id, user_nickname, user_cardname,
					group_id, group_name, is_mentioned, is_at, session_id, reply_to,
					is_emoji, is_picture, is_command, is_notify, raw_content,
					processed_plain_text, additional_config, reply_frequency)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				messageID, formatTransferTime(timestamp), record.Platform, record.UserID, record.UserNickname,
				record.UserCardname, record.GroupID, record.GroupName,
				boolToInt(record.IsMentioned), boolToInt(record.IsAt), record.SessionID, record.ReplyTo,
				boolToInt(record.IsEmoji), boolToInt(record.IsPicture), boolToInt(record.IsCommand),
				boolToInt(record.IsNotify), rawContent, record.ProcessedPlainText,
				record.AdditionalConfig, record.ReplyFrequency)
			return err
		})
		if err != nil {
			return stat, fmt.Errorf("导入消息 %s 失败: %w", messageID, err)
		}
		if exists {
			stat.Updated++
		} else {
			stat.Imported++
		}
	}
	return stat, nil
}

func (s *DatabaseStore) messageExists(ctx context.Context, platform, messageID string) (bool, error) {
	var count int
	err := s.db.Read().QueryRow(
		`SELECT COUNT(*) FROM mai_messages WHERE platform = ? AND message_id = ?`, platform, messageID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("查询消息是否存在失败: %w", err)
	}
	return count > 0, nil
}

// ---------------------------------------------------------------------------
// persons
// ---------------------------------------------------------------------------

// ExportPersons implements Store.
func (s *DatabaseStore) ExportPersons(ctx context.Context, limit int) ([]PersonRecord, error) {
	query := `
		SELECT is_known, person_id, person_name, name_reason, platform, user_id, user_nickname,
			group_cardname, memory_points, know_counts, first_known_time, last_known_time
		FROM person_info ORDER BY id ASC`
	args := []any{}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Read().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("导出人物失败: %w", err)
	}
	defer rows.Close()
	records := make([]PersonRecord, 0)
	for rows.Next() {
		var record PersonRecord
		var firstKnown, lastKnown sql.NullString
		if err := rows.Scan(&record.IsKnown, &record.PersonID, &record.PersonName, &record.NameReason,
			&record.Platform, &record.UserID, &record.UserNickname, &record.GroupCardname,
			&record.MemoryPoints, &record.KnowCounts, &firstKnown, &lastKnown); err != nil {
			return nil, fmt.Errorf("导出人物扫描失败: %w", err)
		}
		if firstKnown.Valid {
			value := firstKnown.String
			record.FirstKnownAt = &value
		}
		if lastKnown.Valid {
			value := lastKnown.String
			record.LastKnownAt = &value
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// ImportPersons implements Store.
func (s *DatabaseStore) ImportPersons(ctx context.Context, records []PersonRecord, overwrite bool) (ImportStat, error) {
	stat := ImportStat{}
	for _, record := range records {
		personID := strings.TrimSpace(record.PersonID)
		if personID == "" {
			return stat, fmt.Errorf("人物记录缺少 person_id")
		}
		var existingID int64
		exists := true
		err := s.db.Read().QueryRow(`SELECT id FROM person_info WHERE person_id = ?`, personID).Scan(&existingID)
		switch {
		case err == sql.ErrNoRows:
			exists = false
		case err != nil:
			return stat, fmt.Errorf("查询人物是否存在失败: %w", err)
		}
		if exists && !overwrite {
			stat.Skipped++
			continue
		}
		firstKnown := nullableTransferTime(record.FirstKnownAt)
		lastKnown := nullableTransferTime(record.LastKnownAt)
		err = s.db.WithWriteTx(func(tx *sql.Tx) error {
			if exists {
				_, err := tx.Exec(`
					UPDATE person_info SET is_known = ?, person_name = ?, name_reason = ?, platform = ?,
						user_id = ?, user_nickname = ?, group_cardname = ?, memory_points = ?,
						know_counts = ?, first_known_time = ?, last_known_time = ?
					WHERE id = ?`,
					boolToInt(record.IsKnown), record.PersonName, record.NameReason, record.Platform,
					record.UserID, record.UserNickname, record.GroupCardname, record.MemoryPoints,
					record.KnowCounts, firstKnown, lastKnown, existingID)
				return err
			}
			_, err := tx.Exec(`
				INSERT INTO person_info (is_known, person_id, person_name, name_reason, platform, user_id,
					user_nickname, group_cardname, memory_points, know_counts, first_known_time, last_known_time)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
				boolToInt(record.IsKnown), personID, record.PersonName, record.NameReason, record.Platform,
				record.UserID, record.UserNickname, record.GroupCardname, record.MemoryPoints,
				record.KnowCounts, firstKnown, lastKnown)
			return err
		})
		if err != nil {
			return stat, fmt.Errorf("导入人物 %s 失败: %w", personID, err)
		}
		if exists {
			stat.Updated++
		} else {
			stat.Imported++
		}
	}
	return stat, nil
}

// ---------------------------------------------------------------------------
// expressions
// ---------------------------------------------------------------------------

// ExportExpressions implements Store.
func (s *DatabaseStore) ExportExpressions(ctx context.Context, limit int) ([]ExpressionRecord, error) {
	query := `
		SELECT situation, style, content_list, count, last_active_time, create_time,
			session_id, checked, modified_by
		FROM expressions ORDER BY id ASC`
	args := []any{}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Read().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("导出表达失败: %w", err)
	}
	defer rows.Close()
	records := make([]ExpressionRecord, 0)
	for rows.Next() {
		var record ExpressionRecord
		var lastActive, create string
		var checked bool
		if err := rows.Scan(&record.Situation, &record.Style, &record.ContentList, &record.Count,
			&lastActive, &create, &record.SessionID, &checked, &record.ModifiedBy); err != nil {
			return nil, fmt.Errorf("导出表达扫描失败: %w", err)
		}
		record.Checked = checked
		record.LastActiveTime = lastActive
		record.CreateTime = create
		records = append(records, record)
	}
	return records, rows.Err()
}

// ImportExpressions implements Store（幂等键 situation+style+session_id）。
func (s *DatabaseStore) ImportExpressions(ctx context.Context, records []ExpressionRecord, overwrite bool) (ImportStat, error) {
	stat := ImportStat{}
	for _, record := range records {
		if strings.TrimSpace(record.Situation) == "" && strings.TrimSpace(record.Style) == "" {
			return stat, fmt.Errorf("表达记录缺少 situation/style")
		}
		sessionKey := ""
		if record.SessionID != nil {
			sessionKey = *record.SessionID
		}
		var existingID, existingCount int64
		exists := true
		err := s.db.Read().QueryRow(
			`SELECT id, count FROM expressions WHERE situation = ? AND style = ? AND COALESCE(session_id,'') = ?`,
			record.Situation, record.Style, sessionKey).Scan(&existingID, &existingCount)
		switch {
		case err == sql.ErrNoRows:
			exists = false
		case err != nil:
			return stat, fmt.Errorf("查询表达是否存在失败: %w", err)
		}
		if exists && !overwrite {
			stat.Skipped++
			continue
		}
		lastActive := normalizeTransferTimeString(record.LastActiveTime)
		create := normalizeTransferTimeString(record.CreateTime)
		if create == "" {
			create = lastActive
		}
		err = s.db.WithWriteTx(func(tx *sql.Tx) error {
			if exists {
				_, err := tx.Exec(`
					UPDATE expressions SET content_list = ?, count = ?, last_active_time = ?, create_time = ?,
						checked = ?, modified_by = ? WHERE id = ?`,
					record.ContentList, record.Count, lastActive, create,
					boolToInt(record.Checked), record.ModifiedBy, existingID)
				return err
			}
			_, err := tx.Exec(`
				INSERT INTO expressions (situation, style, content_list, count, last_active_time, create_time,
					session_id, checked, modified_by) VALUES (?,?,?,?,?,?,?,?,?)`,
				record.Situation, record.Style, record.ContentList, record.Count, lastActive, create,
				record.SessionID, boolToInt(record.Checked), record.ModifiedBy)
			return err
		})
		if err != nil {
			return stat, fmt.Errorf("导入表达 %s/%s 失败: %w", record.Situation, record.Style, err)
		}
		if exists {
			stat.Updated++
		} else {
			stat.Imported++
		}
	}
	return stat, nil
}

// ---------------------------------------------------------------------------
// jargons
// ---------------------------------------------------------------------------

// ExportJargons implements Store.
func (s *DatabaseStore) ExportJargons(ctx context.Context, limit int) ([]JargonRecord, error) {
	query := `
		SELECT content, evidence_messages, meaning, session_id_dict, count, is_jargon, is_complete,
			is_global, last_inference_count, created_by, created_timestamp, updated_timestamp
		FROM jargons ORDER BY id ASC`
	args := []any{}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Read().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("导出黑话失败: %w", err)
	}
	defer rows.Close()
	records := make([]JargonRecord, 0)
	for rows.Next() {
		var record JargonRecord
		var isJargon sql.NullBool
		var created, updated string
		if err := rows.Scan(&record.Content, &record.EvidenceMessages, &record.Meaning,
			&record.SessionIDDict, &record.Count, &isJargon, &record.IsComplete, &record.IsGlobal,
			&record.LastInferenceCount, &record.CreatedBy, &created, &updated); err != nil {
			return nil, fmt.Errorf("导出黑话扫描失败: %w", err)
		}
		if isJargon.Valid {
			value := isJargon.Bool
			record.IsJargon = &value
		}
		record.CreatedTimestamp = created
		record.UpdatedTimestamp = updated
		records = append(records, record)
	}
	return records, rows.Err()
}

// ImportJargons implements Store（幂等键 content）。
func (s *DatabaseStore) ImportJargons(ctx context.Context, records []JargonRecord, overwrite bool) (ImportStat, error) {
	stat := ImportStat{}
	for _, record := range records {
		content := strings.TrimSpace(record.Content)
		if content == "" {
			return stat, fmt.Errorf("黑话记录缺少 content")
		}
		var existingID int64
		exists := true
		err := s.db.Read().QueryRow(`SELECT id FROM jargons WHERE content = ?`, content).Scan(&existingID)
		switch {
		case err == sql.ErrNoRows:
			exists = false
		case err != nil:
			return stat, fmt.Errorf("查询黑话是否存在失败: %w", err)
		}
		if exists && !overwrite {
			stat.Skipped++
			continue
		}
		created := normalizeTransferTimeString(record.CreatedTimestamp)
		updated := normalizeTransferTimeString(record.UpdatedTimestamp)
		if updated == "" {
			updated = created
		}
		var isJargon any
		if record.IsJargon != nil {
			isJargon = boolToInt(*record.IsJargon)
		}
		err = s.db.WithWriteTx(func(tx *sql.Tx) error {
			if exists {
				_, err := tx.Exec(`
					UPDATE jargons SET evidence_messages = ?, meaning = ?, session_id_dict = ?, count = ?,
						is_jargon = ?, is_complete = ?, is_global = ?, last_inference_count = ?,
						created_by = ?, created_timestamp = ?, updated_timestamp = ?
					WHERE id = ?`,
					record.EvidenceMessages, record.Meaning, record.SessionIDDict, record.Count,
					isJargon, boolToInt(record.IsComplete), boolToInt(record.IsGlobal),
					record.LastInferenceCount, record.CreatedBy, created, updated, existingID)
				return err
			}
			sessionDict := record.SessionIDDict
			if strings.TrimSpace(sessionDict) == "" {
				sessionDict = "{}"
			}
			createdBy := record.CreatedBy
			if strings.TrimSpace(createdBy) == "" {
				createdBy = "AI"
			}
			_, err := tx.Exec(`
				INSERT INTO jargons (content, evidence_messages, meaning, session_id_dict, count,
					is_jargon, is_complete, is_global, last_inference_count, created_by,
					created_timestamp, updated_timestamp) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
				content, record.EvidenceMessages, record.Meaning, sessionDict, record.Count,
				isJargon, boolToInt(record.IsComplete), boolToInt(record.IsGlobal),
				record.LastInferenceCount, createdBy, created, updated)
			return err
		})
		if err != nil {
			return stat, fmt.Errorf("导入黑话 %s 失败: %w", content, err)
		}
		if exists {
			stat.Updated++
		} else {
			stat.Imported++
		}
	}
	return stat, nil
}

// ---------------------------------------------------------------------------
// images
// ---------------------------------------------------------------------------

// ExportImages implements Store（含 emoji/image 两类；仅描述元数据）。
func (s *DatabaseStore) ExportImages(ctx context.Context, limit int) ([]ImageRecord, error) {
	query := `
		SELECT image_hash, description, full_path, image_type, query_count, is_registered, is_banned,
			no_file_flag, record_time, register_time, last_used_time, vlm_processed
		FROM images ORDER BY id ASC`
	args := []any{}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Read().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("导出图片描述失败: %w", err)
	}
	defer rows.Close()
	records := make([]ImageRecord, 0)
	for rows.Next() {
		var record ImageRecord
		var registerTime, lastUsed sql.NullString
		if err := rows.Scan(&record.ImageHash, &record.Description, &record.FullPath, &record.ImageType,
			&record.QueryCount, &record.IsRegistered, &record.IsBanned, &record.NoFileFlag,
			&record.RecordTime, &registerTime, &lastUsed, &record.VLMProcessed); err != nil {
			return nil, fmt.Errorf("导出图片描述扫描失败: %w", err)
		}
		if registerTime.Valid {
			value := registerTime.String
			record.RegisterTime = &value
		}
		if lastUsed.Valid {
			value := lastUsed.String
			record.LastUsedTime = &value
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// ImportImages implements Store（幂等键 image_hash+image_type）。
func (s *DatabaseStore) ImportImages(ctx context.Context, records []ImageRecord, overwrite bool) (ImportStat, error) {
	stat := ImportStat{}
	for _, record := range records {
		hash := strings.TrimSpace(record.ImageHash)
		if hash == "" {
			return stat, fmt.Errorf("图片描述记录缺少 image_hash")
		}
		imageType := record.ImageType
		if strings.TrimSpace(imageType) == "" {
			imageType = "emoji"
		}
		var existingID int64
		exists := true
		err := s.db.Read().QueryRow(
			`SELECT id FROM images WHERE image_hash = ? AND image_type = ?`, hash, imageType).Scan(&existingID)
		switch {
		case err == sql.ErrNoRows:
			exists = false
		case err != nil:
			return stat, fmt.Errorf("查询图片记录是否存在失败: %w", err)
		}
		if exists && !overwrite {
			stat.Skipped++
			continue
		}
		recordTime := normalizeTransferTimeString(record.RecordTime)
		if recordTime == "" {
			recordTime = formatTransferTime(time.Now())
		}
		registerTime := nullableTransferTime(record.RegisterTime)
		lastUsed := nullableTransferTime(record.LastUsedTime)
		err = s.db.WithWriteTx(func(tx *sql.Tx) error {
			if exists {
				_, err := tx.Exec(`
					UPDATE images SET description = ?, full_path = ?, query_count = ?, is_registered = ?,
						is_banned = ?, no_file_flag = ?, record_time = ?, register_time = ?,
						last_used_time = ?, vlm_processed = ? WHERE id = ?`,
					record.Description, record.FullPath, record.QueryCount, boolToInt(record.IsRegistered),
					boolToInt(record.IsBanned), boolToInt(record.NoFileFlag), recordTime, registerTime,
					lastUsed, boolToInt(record.VLMProcessed), existingID)
				return err
			}
			_, err := tx.Exec(`
				INSERT INTO images (image_hash, description, full_path, image_type, query_count,
					is_registered, is_banned, no_file_flag, record_time, register_time, last_used_time,
					vlm_processed) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
				hash, record.Description, record.FullPath, imageType, record.QueryCount,
				boolToInt(record.IsRegistered), boolToInt(record.IsBanned), boolToInt(record.NoFileFlag),
				recordTime, registerTime, lastUsed, boolToInt(record.VLMProcessed))
			return err
		})
		if err != nil {
			return stat, fmt.Errorf("导入图片描述 %s 失败: %w", hash, err)
		}
		if exists {
			stat.Updated++
		} else {
			stat.Imported++
		}
	}
	return stat, nil
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func scanMessageRecord(scanner interface{ Scan(...any) error }) (MessageRecord, error) {
	var record MessageRecord
	if err := scanner.Scan(&record.MessageID, &record.Timestamp, &record.Platform, &record.UserID,
		&record.UserNickname, &record.UserCardname, &record.GroupID, &record.GroupName,
		&record.IsMentioned, &record.IsAt, &record.SessionID, &record.ReplyTo,
		&record.IsEmoji, &record.IsPicture, &record.IsCommand, &record.IsNotify, &record.RawContent,
		&record.ProcessedPlainText, &record.AdditionalConfig, &record.ReplyFrequency); err != nil {
		return MessageRecord{}, fmt.Errorf("导出消息扫描失败: %w", err)
	}
	return record, nil
}

const transferTimeLayout = "2006-01-02 15:04:05.999999"

// formatTransferTime 编码为数据库 DATETIME 文本。
func formatTransferTime(t time.Time) string { return t.Format(transferTimeLayout) }

// normalizeTransferTimeString 归一化时间文本（空值原样返回）。
func normalizeTransferTimeString(value string) string {
	parsed, err := parseTransferTime(value)
	if err != nil {
		return value
	}
	if parsed.IsZero() {
		return ""
	}
	return formatTransferTime(parsed)
}

// nullableTransferTime 把可空时间文本转为 SQL 参数。
func nullableTransferTime(value *string) any {
	if value == nil {
		return nil
	}
	normalized := normalizeTransferTimeString(*value)
	if normalized == "" {
		return nil
	}
	return normalized
}

// parseTransferTime 解析导出包里的时间文本（RFC3339 或数据库格式）。
func parseTransferTime(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, nil
	}
	layouts := []string{
		time.RFC3339Nano, time.RFC3339,
		transferTimeLayout, "2006-01-02 15:04:05", "2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.ParseInLocation(layout, trimmed, time.Local); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间 %q", value)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
