package image

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"oh-my-qqbot/internal/database"
)

// DatabaseStore 把 internal/database 的 images 表适配为 ImageStore
// （image_type=image 行）。
type DatabaseStore struct {
	db *database.DB
}

// NewDatabaseStore 创建图片持久化适配器。
func NewDatabaseStore(db *database.DB) *DatabaseStore { return &DatabaseStore{db: db} }

const imageRecordColumns = `id, image_hash, description, full_path, query_count,
	no_file_flag, record_time, last_used_time, vlm_processed`

// GetByHash implements ImageStore.
func (s *DatabaseStore) GetByHash(ctx context.Context, hash string) (*ImageRecord, error) {
	row := s.db.Read().QueryRow(
		`SELECT `+imageRecordColumns+` FROM images WHERE image_hash = ? AND image_type = 'image' ORDER BY id LIMIT 1`, hash)
	record, err := scanImageRecord(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return record, err
}

// Upsert implements ImageStore.
func (s *DatabaseStore) Upsert(ctx context.Context, record *ImageRecord) error {
	if record.RecordTime.IsZero() {
		record.RecordTime = time.Now()
	}
	return s.db.WithWriteTx(func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRow(
			`SELECT id FROM images WHERE image_hash = ? AND image_type = 'image' ORDER BY id LIMIT 1`,
			record.ImageHash).Scan(&id)
		switch {
		case err == sql.ErrNoRows:
			_, err = tx.Exec(`
				INSERT INTO images (
					image_hash, description, full_path, image_type, query_count,
					is_registered, is_banned, no_file_flag, record_time, last_used_time, vlm_processed
				) VALUES (?,?,?,'image',?,?,0,0,?,?,?)`,
				record.ImageHash, record.Description, record.FullPath, record.QueryCount,
				false, false, record.NoFileFlag, formatTime(record.RecordTime),
				formatNullableTime(record.LastUsedTime), record.VLMProcessed)
			return err
		case err != nil:
			return err
		default:
			_, err = tx.Exec(`
				UPDATE images SET description = ?, full_path = ?, query_count = ?,
					no_file_flag = ?, last_used_time = ?, vlm_processed = ?
				WHERE id = ?`,
				record.Description, record.FullPath, record.QueryCount,
				record.NoFileFlag, formatNullableTime(record.LastUsedTime),
				record.VLMProcessed, id)
			return err
		}
	})
}

// Delete implements ImageStore.
func (s *DatabaseStore) Delete(ctx context.Context, hash string) error {
	_, err := s.db.Write().Exec(`DELETE FROM images WHERE image_hash = ? AND image_type = 'image'`, hash)
	if err != nil {
		return fmt.Errorf("image: 删除记录失败: %w", err)
	}
	return nil
}

// ListAll implements ImageStore.
func (s *DatabaseStore) ListAll(ctx context.Context) ([]*ImageRecord, error) {
	rows, err := s.db.Read().Query(
		`SELECT ` + imageRecordColumns + ` FROM images WHERE image_type = 'image' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("image: 查询记录失败: %w", err)
	}
	defer rows.Close()
	var out []*ImageRecord
	for rows.Next() {
		record, err := scanImageRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func scanImageRecord(row rowScanner) (*ImageRecord, error) {
	r := &ImageRecord{}
	var recordTime string
	var lastUsedTime sql.NullString
	err := row.Scan(&r.ID, &r.ImageHash, &r.Description, &r.FullPath, &r.QueryCount,
		&r.NoFileFlag, &recordTime, &lastUsedTime, &r.VLMProcessed)
	if err != nil {
		return nil, err
	}
	if r.RecordTime, err = parseStoredTime(recordTime); err != nil {
		return nil, err
	}
	if lastUsedTime.Valid && lastUsedTime.String != "" {
		if t, err := parseStoredTime(lastUsedTime.String); err == nil {
			r.LastUsedTime = &t
		}
	}
	return r, nil
}

// ProjectPathResolver 把 image_path.py 语义适配为 StorePathResolver。
type ProjectPathResolver struct {
	root string
}

// NewProjectPathResolver 创建路径解析器；root 为项目根目录绝对路径。
func NewProjectPathResolver(root string) *ProjectPathResolver {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return &ProjectPathResolver{root: abs}
}

// Resolve implements StorePathResolver.
func (r *ProjectPathResolver) Resolve(storedPath string) (string, error) {
	if strings.TrimSpace(storedPath) == "" {
		return "", fmt.Errorf("image: 图片路径不能为空")
	}
	clean := filepath.FromSlash(storedPath)
	absolute := clean
	if !filepath.IsAbs(clean) {
		absolute = filepath.Join(r.root, clean)
	}
	abs, err := filepath.Abs(absolute)
	if err != nil {
		return "", err
	}
	if !withinRoot(r.root, abs) {
		return "", fmt.Errorf("image: 图片路径不在项目目录内: %s", storedPath)
	}
	return abs, nil
}

// Serialize implements StorePathResolver.
func (r *ProjectPathResolver) Serialize(absolutePath string) (string, error) {
	if strings.TrimSpace(absolutePath) == "" {
		return "", fmt.Errorf("image: 图片路径不能为空")
	}
	abs, err := filepath.Abs(absolutePath)
	if err != nil {
		return "", err
	}
	if !withinRoot(r.root, abs) {
		return "", fmt.Errorf("image: 图片路径不在项目目录内: %s", absolutePath)
	}
	rel, err := filepath.Rel(r.root, abs)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func formatTime(t time.Time) string { return t.Format("2006-01-02 15:04:05.999999") }

func formatNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
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
	return time.Time{}, fmt.Errorf("image: 无法解析时间: %s", s)
}
