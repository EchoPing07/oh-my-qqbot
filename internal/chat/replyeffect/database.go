package replyeffect

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"oh-my-qqbot/internal/database"
)

// This file 适配 internal/database：（对齐 参考/MaiBot/src/maisaka/reply_effect/
// storage.py._save_database_summary 写入的 maisaka_reply_effects 表）。
//
// 表名沿用 MaiBot 原表名 maisaka_reply_effects（任务书里的「mai_reply_effects 类表」
// 即此表）；建表语句在 internal/database/migrations.go 末尾追加。
//
// 与 Python 版差异：
//   - record_json 直接存完整 JSON 文本（不再 gzip 到 record_blob）；
//     record_blob / raw_score / relative_score / reception_score 列保留但不写，
//     以便与 Python 版表结构并存。
//   - 额外写 created_ms / finalized_ms（epoch 毫秒），保证跨时区/跨 DST 的排序稳定。

// DatabaseStore 把 internal/database 的 maisaka_reply_effects 表适配为 Store。
type DatabaseStore struct {
	db *database.DB
}

// NewDatabaseStore 创建回复效果持久化适配器。
func NewDatabaseStore(db *database.DB) *DatabaseStore { return &DatabaseStore{db: db} }

// Save implements Store（对齐 _save_database_summary 的 upsert 语义）。
func (s *DatabaseStore) Save(ctx context.Context, record *Record) error {
	if record == nil {
		return fmt.Errorf("回复效果记录为空")
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("序列化回复效果记录失败: %w", err)
	}
	created, err := ParseTimestamp(record.CreatedAt)
	if err != nil {
		return fmt.Errorf("解析 created_at 失败: %w", err)
	}
	var finalized any
	var finalizedMillis int64
	if record.FinalizedAt != "" {
		if parsed, err := ParseTimestamp(record.FinalizedAt); err == nil {
			finalized = parsed.Format(time.RFC3339)
			finalizedMillis = parsed.UnixMilli()
		}
	}
	confidence := 0.0
	if record.Scores != nil && record.Scores.Confidence != nil {
		confidence = *record.Scores.Confidence
	}
	return s.db.WithWriteTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO maisaka_reply_effects (
				effect_id, session_id, session_name, chat_type, status,
				created_at, created_ms, finalized_at, finalized_ms,
				strategy_primary, model_name, request_fingerprint, prompt_fingerprint,
				scorer_version, response_score, reception_score, conversation_score,
				raw_score, relative_score, confidence, record_json, record_blob
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,?,NULL,NULL,?,?,NULL)
			ON CONFLICT(effect_id) DO UPDATE SET
				session_name = excluded.session_name,
				chat_type = excluded.chat_type,
				status = excluded.status,
				finalized_at = excluded.finalized_at,
				finalized_ms = excluded.finalized_ms,
				strategy_primary = excluded.strategy_primary,
				model_name = excluded.model_name,
				request_fingerprint = excluded.request_fingerprint,
				prompt_fingerprint = excluded.prompt_fingerprint,
				scorer_version = excluded.scorer_version,
				response_score = excluded.response_score,
				conversation_score = excluded.conversation_score,
				confidence = excluded.confidence,
				record_json = excluded.record_json`,
			record.EffectID, record.Session.SessionID, record.Session.SessionName, record.Session.ChatType,
			string(record.Status), record.CreatedAt, created.UnixMilli(), finalized, finalizedMillis,
			record.Reply.StrategyPrimary, record.Reply.ModelName, record.Reply.RequestFingerprint,
			record.Reply.PromptFingerprint, record.EvaluationVersion,
			nullableScore(record, func(scores *Scores) *float64 { return &scores.ResponseScore }),
			nullableScore(record, func(scores *Scores) *float64 { return &scores.ConversationScore }),
			confidence, string(payload))
		if err != nil {
			return fmt.Errorf("写入 maisaka_reply_effects 失败: %w", err)
		}
		return nil
	})
}

// GetByID implements Store.
func (s *DatabaseStore) GetByID(ctx context.Context, effectID string) (*Record, error) {
	row := s.db.Read().QueryRow(
		`SELECT record_json FROM maisaka_reply_effects WHERE effect_id = ?`, effectID)
	return scanRecordJSON(row)
}

// LoadUnfinished implements Store（对齐 load_unfinished_records 的可恢复状态集合）。
func (s *DatabaseStore) LoadUnfinished(ctx context.Context, sessionID string) ([]*Record, error) {
	rows, err := s.db.Read().Query(`
		SELECT record_json FROM maisaka_reply_effects
		WHERE session_id = ? AND status IN ('pending','evaluating','evaluation_failed')
		ORDER BY created_at ASC, effect_id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("查询未结算回复效果失败: %w", err)
	}
	return scanRecordRows(rows)
}

// LoadByIDs implements Store.
func (s *DatabaseStore) LoadByIDs(ctx context.Context, effectIDs []string) ([]*Record, error) {
	if len(effectIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(effectIDs)), ",")
	args := make([]any, 0, len(effectIDs))
	for _, effectID := range effectIDs {
		args = append(args, effectID)
	}
	rows, err := s.db.Read().Query(
		`SELECT record_json FROM maisaka_reply_effects WHERE effect_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("查询回复效果记录失败: %w", err)
	}
	return scanRecordRows(rows)
}

// ListBySession 按会话读取全部记录（时间升序；WebUI 概览/测试用）。
func (s *DatabaseStore) ListBySession(ctx context.Context, sessionID string, limit int) ([]*Record, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Read().Query(`
		SELECT record_json FROM maisaka_reply_effects
		WHERE session_id = ? ORDER BY created_at DESC, effect_id DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("查询回复效果记录失败: %w", err)
	}
	records, err := scanRecordRows(rows)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	return records, nil
}

// CountBySession 返回会话的记录数。
func (s *DatabaseStore) CountBySession(ctx context.Context, sessionID string) (int64, error) {
	var count int64
	err := s.db.Read().QueryRow(
		`SELECT COUNT(*) FROM maisaka_reply_effects WHERE session_id = ?`, sessionID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("统计回复效果记录失败: %w", err)
	}
	return count, nil
}

func nullableScore(record *Record, pick func(*Scores) *float64) any {
	if record.Scores == nil {
		return nil
	}
	return *pick(record.Scores)
}

func scanRecordJSON(row *sql.Row) (*Record, error) {
	var payload string
	if err := row.Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("读取回复效果记录失败: %w", err)
	}
	return DecodeRecordJSON([]byte(payload))
}

func scanRecordRows(rows *sql.Rows) ([]*Record, error) {
	defer rows.Close()
	records := make([]*Record, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		record, err := DecodeRecordJSON([]byte(payload))
		if err != nil {
			continue
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
