package person

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"oh-my-qqbot/internal/database"
)

// DatabaseStore 把 internal/database 的 person_info 表适配为 PersonStore。
type DatabaseStore struct {
	db *database.DB
}

// NewDatabaseStore 创建人物持久化适配器。
func NewDatabaseStore(db *database.DB) *DatabaseStore { return &DatabaseStore{db: db} }

// GetByID implements PersonStore.
func (s *DatabaseStore) GetByID(ctx context.Context, personID string) (*PersonRecord, error) {
	row := s.db.Read().QueryRow(`
		SELECT id, is_known, person_id, person_name, name_reason, platform, user_id,
			user_nickname, group_cardname, memory_points, know_counts, first_known_time, last_known_time
		FROM person_info WHERE person_id = ?`, personID)
	record, err := scanPersonRecord(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return record, err
}

// GetIDByName implements PersonStore.
func (s *DatabaseStore) GetIDByName(ctx context.Context, personName string) (string, error) {
	var personID sql.NullString
	err := s.db.Read().QueryRow(
		`SELECT person_id FROM person_info WHERE person_name = ? ORDER BY id LIMIT 1`, personName).Scan(&personID)
	if err == sql.ErrNoRows || !personID.Valid {
		return "", nil
	}
	return personID.String, err
}

// Upsert implements PersonStore.
func (s *DatabaseStore) Upsert(ctx context.Context, record *PersonRecord) error {
	return s.db.WithWriteTx(func(tx *sql.Tx) error {
		var id int64
		var knowCounts int
		err := tx.QueryRow(`SELECT id, know_counts FROM person_info WHERE person_id = ?`,
			record.PersonID).Scan(&id, &knowCounts)
		switch {
		case err == sql.ErrNoRows:
			_, err = tx.Exec(`
				INSERT INTO person_info (is_known, person_id, person_name, name_reason, platform,
					user_id, user_nickname, group_cardname, memory_points, know_counts,
					first_known_time, last_known_time)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
				boolToInt(record.IsKnown), record.PersonID, record.PersonName, record.NameReason,
				record.Platform, record.UserID, record.UserNickname, record.GroupCardname,
				record.MemoryPoints, record.KnowCounts,
				formatNullableTime(record.FirstKnownTime), formatNullableTime(record.LastKnownTime))
			return err
		case err != nil:
			return err
		default:
			// 全量字段更新（Person.SyncToDatabase 是权威写入路径）。
			_, err = tx.Exec(`
				UPDATE person_info SET is_known = ?, person_name = ?, name_reason = ?,
					platform = ?, user_id = ?, user_nickname = ?, group_cardname = ?,
					memory_points = ?, know_counts = ?, first_known_time = ?, last_known_time = ?
				WHERE id = ?`,
				boolToInt(record.IsKnown), record.PersonName, record.NameReason,
				record.Platform, record.UserID, record.UserNickname, record.GroupCardname,
				record.MemoryPoints, record.KnowCounts,
				formatNullableTime(record.FirstKnownTime), formatNullableTime(record.LastKnownTime), id)
			return err
		}
	})
}

type rowScanner interface{ Scan(...any) error }

func scanPersonRecord(row rowScanner) (*PersonRecord, error) {
	r := &PersonRecord{}
	var isKnown bool
	var personName, nameReason, cardname, memory sql.NullString
	var firstKnown, lastKnown sql.NullString
	err := row.Scan(&r.ID, &isKnown, &r.PersonID, &personName, &nameReason, &r.Platform,
		&r.UserID, &r.UserNickname, &cardname, &memory, &r.KnowCounts, &firstKnown, &lastKnown)
	if err != nil {
		return nil, err
	}
	r.IsKnown = isKnown
	if personName.Valid {
		r.PersonName = &personName.String
	}
	if nameReason.Valid {
		r.NameReason = &nameReason.String
	}
	if cardname.Valid {
		r.GroupCardname = &cardname.String
	}
	if memory.Valid {
		r.MemoryPoints = &memory.String
	}
	if firstKnown.Valid && firstKnown.String != "" {
		if t, err := parseStoredTime(firstKnown.String); err == nil {
			r.FirstKnownTime = &t
		}
	}
	if lastKnown.Valid && lastKnown.String != "" {
		if t, err := parseStoredTime(lastKnown.String); err == nil {
			r.LastKnownTime = &t
		}
	}
	return r, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func formatNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format("2006-01-02 15:04:05.999999")
}

func parseStoredTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("person: 无法解析时间: %s", s)
}
