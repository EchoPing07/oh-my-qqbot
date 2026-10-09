package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"oh-my-qqbot/internal/memorix/retrieval"
)

// nowTS 返回 Unix 秒（浮点），对齐 Python datetime.now().timestamp()。
func nowTS() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func tsToTime(ts float64) time.Time {
	if ts <= 0 {
		return time.Time{}
	}
	sec := int64(ts)
	nsec := int64((ts - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

func timeToTS(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// ---------------------------------------------------------------------------
// 哈希：对齐 core/utils/hash.py
// ---------------------------------------------------------------------------

// ComputeHash 计算 sha256 十六进制摘要。
// 出处：core/utils/hash.py L10-L26。
func ComputeHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

var whitespaceRE = regexp.MustCompile(`[^\S\n]+`)

// NormalizeText 规范化文本用于哈希：去首尾空白、统一换行、压缩连续空格。
// 出处：core/utils/hash.py L29-L48。
func NormalizeText(text string) string {
	text = strings.TrimSpace(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return whitespaceRE.ReplaceAllString(text, " ")
}

// ComputeParagraphHash 计算段落 hash。
// 出处：core/utils/hash.py L51-L63。
func ComputeParagraphHash(paragraph string) string {
	return ComputeHash(NormalizeText(paragraph))
}

// ComputeEntityHash 计算实体 hash（strip + lower）。
// 出处：core/utils/hash.py L66-L78。
func ComputeEntityHash(entity string) string {
	return ComputeHash(strings.ToLower(strings.TrimSpace(entity)))
}

// CanonicalizeName 规范化名称（strip + lower），对齐 MetadataStore._canonicalize_name。
// 出处：core/storage/metadata_store.py L264-L276。
func CanonicalizeName(name string) string { return trimSpaceLower(name) }

// ComputeRelationHash 计算关系 hash：sha256("subject|predicate|object")（全部规范化）。
// 出处：core/storage/metadata_store.py L595-L609。
func ComputeRelationHash(subject, predicate, object string) (string, error) {
	s := CanonicalizeName(subject)
	p := CanonicalizeName(predicate)
	o := CanonicalizeName(object)
	if s == "" || p == "" || o == "" {
		return "", fmt.Errorf("memorix/storage: 关系三元组不能为空")
	}
	return ComputeHash(s + "|" + p + "|" + o), nil
}

// ---------------------------------------------------------------------------
// 段落
// ---------------------------------------------------------------------------

// ParagraphInput 是写入段落的参数。
type ParagraphInput struct {
	Content   string
	Source    string
	ChatID    string
	EventTime *float64
	// KnowledgeType 对齐 paragraphs.knowledge_type（mixed/factual/chat_log/...）。
	KnowledgeType string
	Metadata      map[string]any
}

// AddParagraph 幂等写入段落，返回段落 hash。
// 出处：core/storage/metadata_store.py add_paragraph（同 hash 时仅更新 metadata）。
func (s *Store) AddParagraph(ctx context.Context, input ParagraphInput) (string, error) {
	content := NormalizeText(input.Content)
	if content == "" {
		return "", fmt.Errorf("memorix/storage: 段落内容为空")
	}
	hash := ComputeParagraphHash(content)
	knowledgeType := strings.TrimSpace(input.KnowledgeType)
	if knowledgeType == "" {
		knowledgeType = "mixed"
	}
	metaJSON, err := encodeJSON(input.Metadata)
	if err != nil {
		return "", err
	}
	now := nowTS()
	eventTime := input.EventTime
	_, err = s.write.ExecContext(ctx, `
		INSERT INTO paragraphs(hash, content, source, chat_id, word_count, knowledge_type,
			created_at, updated_at, event_time, event_time_start, event_time_end, metadata, is_deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(hash) DO UPDATE SET
			updated_at=excluded.updated_at,
			metadata=excluded.metadata,
			source=CASE WHEN excluded.source <> '' THEN excluded.source ELSE paragraphs.source END,
			chat_id=CASE WHEN excluded.chat_id <> '' THEN excluded.chat_id ELSE paragraphs.chat_id END,
			event_time=COALESCE(excluded.event_time, paragraphs.event_time),
			event_time_start=COALESCE(excluded.event_time_start, paragraphs.event_time_start),
			event_time_end=COALESCE(excluded.event_time_end, paragraphs.event_time_end),
			is_deleted=0`,
		hash, content, input.Source, input.ChatID, CountRunes(content), knowledgeType,
		now, now, nullableFloat(eventTime), nullableFloat(eventTime), nullableFloat(eventTime), metaJSON)
	if err != nil {
		return "", fmt.Errorf("memorix/storage: 写 paragraphs 失败: %w", err)
	}
	if err := s.upsertTokenizedParagraph(ctx, hash, content); err != nil {
		return "", err
	}
	return hash, nil
}

// AddParagraphWithTimeRange 写入带事件时间区间的段落（跨时间窗的记忆）。
func (s *Store) AddParagraphWithTimeRange(
	ctx context.Context, input ParagraphInput, start, end *float64,
) (string, error) {
	content := NormalizeText(input.Content)
	if content == "" {
		return "", fmt.Errorf("memorix/storage: 段落内容为空")
	}
	hash := ComputeParagraphHash(content)
	knowledgeType := strings.TrimSpace(input.KnowledgeType)
	if knowledgeType == "" {
		knowledgeType = "mixed"
	}
	metaJSON, err := encodeJSON(input.Metadata)
	if err != nil {
		return "", err
	}
	now := nowTS()
	_, err = s.write.ExecContext(ctx, `
		INSERT INTO paragraphs(hash, content, source, chat_id, word_count, knowledge_type,
			created_at, updated_at, event_time, event_time_start, event_time_end, metadata, is_deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(hash) DO UPDATE SET
			updated_at=excluded.updated_at,
			metadata=excluded.metadata,
			event_time_start=COALESCE(excluded.event_time_start, paragraphs.event_time_start),
			event_time_end=COALESCE(excluded.event_time_end, paragraphs.event_time_end),
			is_deleted=0`,
		hash, content, input.Source, input.ChatID, CountRunes(content), knowledgeType,
		now, now, nullableFloat(input.EventTime), nullableFloat(start), nullableFloat(end), metaJSON)
	if err != nil {
		return "", fmt.Errorf("memorix/storage: 写 paragraphs(range) 失败: %w", err)
	}
	if err := s.upsertTokenizedParagraph(ctx, hash, content); err != nil {
		return "", err
	}
	return hash, nil
}

// GetParagraph 读取单个段落。
func (s *Store) GetParagraph(ctx context.Context, hash string) (*retrieval.Paragraph, error) {
	rows, err := s.queryParagraphs(ctx, `WHERE hash = ? AND (is_deleted IS NULL OR is_deleted = 0)`, hash)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// ParagraphsByHashes 批量读取段落（保持输入顺序在调用方）。
func (s *Store) ParagraphsByHashes(ctx context.Context, hashes []string) (map[string]*retrieval.Paragraph, error) {
	out := map[string]*retrieval.Paragraph{}
	if len(hashes) == 0 {
		return out, nil
	}
	for _, chunk := range chunkStrings(hashes, 400) {
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, h := range chunk {
			placeholders[i] = "?"
			args[i] = h
		}
		rows, err := s.queryParagraphs(ctx,
			`WHERE hash IN (`+strings.Join(placeholders, ",")+`) AND (is_deleted IS NULL OR is_deleted = 0)`,
			args...)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			out[row.Hash] = row
		}
	}
	return out, nil
}

// ParagraphsByRelation 返回关系的支撑段落。
func (s *Store) ParagraphsByRelation(ctx context.Context, relationHash string) ([]*retrieval.Paragraph, error) {
	return s.queryParagraphs(ctx, `
		WHERE hash IN (SELECT paragraph_hash FROM paragraph_links WHERE relation_hash = ?)
		  AND (is_deleted IS NULL OR is_deleted = 0)`, relationHash)
}

// ParagraphsByEntity 返回实体的支撑段落。
func (s *Store) ParagraphsByEntity(ctx context.Context, entityHash string) ([]*retrieval.Paragraph, error) {
	return s.queryParagraphs(ctx, `
		WHERE hash IN (SELECT paragraph_hash FROM entity_links WHERE entity_hash = ?)
		  AND (is_deleted IS NULL OR is_deleted = 0)`, entityHash)
}

// ParagraphHashesByRelationHashes 批量返回关系 → 支撑段落 hash 列表。
// 出处：core/storage/metadata_store.py get_paragraph_hashes_by_relation_hashes。
func (s *Store) ParagraphHashesByRelationHashes(
	ctx context.Context, relationHashes []string,
) (map[string][]string, error) {
	out := map[string][]string{}
	if len(relationHashes) == 0 {
		return out, nil
	}
	for _, chunk := range chunkStrings(relationHashes, 400) {
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, h := range chunk {
			placeholders[i] = "?"
			args[i] = h
		}
		rows, err := s.read.QueryContext(ctx,
			`SELECT relation_hash, paragraph_hash FROM paragraph_links WHERE relation_hash IN (`+
				strings.Join(placeholders, ",")+`) ORDER BY paragraph_hash`,
			args...)
		if err != nil {
			return nil, fmt.Errorf("memorix/storage: 查询 paragraph_links 失败: %w", err)
		}
		for rows.Next() {
			var relHash, paraHash string
			if err := rows.Scan(&relHash, &paraHash); err != nil {
				rows.Close()
				return nil, err
			}
			out[relHash] = append(out[relHash], paraHash)
		}
		rows.Close()
	}
	return out, nil
}

// TemporalParagraphs 按时序条件查询段落（对齐 query_paragraphs_temporal）。
// 出处：core/storage/metadata_store.py query_paragraphs_temporal。
func (s *Store) TemporalParagraphs(
	ctx context.Context, opts retrieval.TemporalQueryOptions, limit int,
) ([]*retrieval.Paragraph, error) {
	conditions := []string{"(is_deleted IS NULL OR is_deleted = 0)"}
	args := []any{}
	allowFallback := opts.AllowCreatedFallback
	// 有效时间优先取 event_time_range，其次 event_time，最后 created_at 兜底。
	// CASE 的 THEN/ELSE 必须是字面量（绑定参数会被 SQLite 推断为 TEXT 导致类型不匹配）。
	fallbackExpr := "NULL"
	if allowFallback {
		fallbackExpr = "created_at"
	}
	effectiveStart := "COALESCE(event_time_start, event_time, " + fallbackExpr + ")"
	effectiveEnd := "COALESCE(event_time_end, event_time, " + fallbackExpr + ")"
	if opts.TimeFrom != nil && opts.TimeTo != nil {
		conditions = append(conditions, effectiveEnd+" >= ?", effectiveStart+" <= ?")
		args = append(args, *opts.TimeFrom, *opts.TimeTo)
	} else if opts.TimeFrom != nil {
		conditions = append(conditions, effectiveEnd+" >= ?")
		args = append(args, *opts.TimeFrom)
	} else if opts.TimeTo != nil {
		conditions = append(conditions, effectiveStart+" <= ?")
		args = append(args, *opts.TimeTo)
	}
	if opts.Source != "" {
		conditions = append(conditions, "source = ?")
		args = append(args, opts.Source)
	}
	if opts.Person != "" {
		conditions = append(conditions, "(metadata LIKE ? OR metadata LIKE ?)")
		args = append(args, `%"`+opts.Person+`"%`, `%"`+strings.ToLower(opts.Person)+`"%`)
	}
	sqlText := `SELECT hash, content, source, chat_id, word_count, knowledge_type,
		created_at, updated_at, event_time, event_time_start, event_time_end, metadata
		FROM paragraphs WHERE ` + strings.Join(conditions, " AND ") +
		` ORDER BY COALESCE(event_time_end, event_time, created_at) DESC LIMIT ?`
	args = append(args, maxIntStorage(1, limit))
	rows, err := s.read.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: 时序查询失败: %w", err)
	}
	defer rows.Close()
	var out []*retrieval.Paragraph
	for rows.Next() {
		paragraph, err := scanParagraph(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, paragraph)
	}
	return out, rows.Err()
}

func (s *Store) queryParagraphs(ctx context.Context, where string, args ...any) ([]*retrieval.Paragraph, error) {
	sqlText := `SELECT hash, content, source, chat_id, word_count, knowledge_type,
		created_at, updated_at, event_time, event_time_start, event_time_end, metadata
		FROM paragraphs ` + where
	rows, err := s.read.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: 查询段落失败: %w", err)
	}
	defer rows.Close()
	var out []*retrieval.Paragraph
	for rows.Next() {
		paragraph, err := scanParagraph(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, paragraph)
	}
	return out, rows.Err()
}

func scanParagraph(rows *sql.Rows) (*retrieval.Paragraph, error) {
	var (
		hash, content, source, chatID, knowledgeType string
		wordCount                                    int
		createdAt, updatedAt                         float64
		eventTime, eventStart, eventEnd              sql.NullFloat64
		metaJSON                                     sql.NullString
	)
	if err := rows.Scan(&hash, &content, &source, &chatID, &wordCount, &knowledgeType,
		&createdAt, &updatedAt, &eventTime, &eventStart, &eventEnd, &metaJSON); err != nil {
		return nil, fmt.Errorf("memorix/storage: 解析段落后失败: %w", err)
	}
	paragraph := &retrieval.Paragraph{
		Hash:          hash,
		Content:       content,
		Source:        source,
		ChatID:        chatID,
		WordCount:     wordCount,
		KnowledgeType: knowledgeType,
		CreatedAt:     tsToTime(createdAt),
		UpdatedAt:     tsToTime(updatedAt),
		Metadata:      decodeJSONMap(metaJSON),
	}
	if eventTime.Valid {
		v := eventTime.Float64
		paragraph.EventTime = &v
	}
	if eventStart.Valid {
		v := eventStart.Float64
		paragraph.EventStart = &v
	}
	if eventEnd.Valid {
		v := eventEnd.Float64
		paragraph.EventEnd = &v
	}
	if paragraph.EventTime == nil && paragraph.EventStart == nil && paragraph.EventEnd == nil {
		// created_at 兜底由 retrieval.EffectiveTime 处理（allow_created_fallback）。
		_ = createdAt
	}
	return paragraph, nil
}

// TouchParagraph 增加访问计数（对齐 access_count / last_accessed）。
func (s *Store) TouchParagraph(ctx context.Context, hash string) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE paragraphs SET access_count = access_count + 1, last_accessed = ? WHERE hash = ?`,
		nowTS(), hash)
	return err
}

// ---------------------------------------------------------------------------
// 实体与链接
// ---------------------------------------------------------------------------

// AddEntity 幂等写入实体并返回其 hash。
// 出处：core/storage/metadata_store.py add_entity。
func (s *Store) AddEntity(ctx context.Context, name string) (string, error) {
	canonical := CanonicalizeName(name)
	if canonical == "" {
		return "", fmt.Errorf("memorix/storage: 实体名不能为空")
	}
	hash := ComputeEntityHash(canonical)
	now := nowTS()
	_, err := s.write.ExecContext(ctx, `
		INSERT INTO entities(hash, name, appearance_count, created_at, is_deleted)
		VALUES (?, ?, 1, ?, 0)
		ON CONFLICT(hash) DO UPDATE SET
			appearance_count = entities.appearance_count + 1,
			is_deleted = 0`,
		hash, canonical, now)
	if err != nil {
		return "", fmt.Errorf("memorix/storage: 写 entities 失败: %w", err)
	}
	return hash, nil
}

// LinkParagraphEntity 关联段落与实体（mention_count 累加）。
func (s *Store) LinkParagraphEntity(ctx context.Context, paragraphHash, entityHash string) error {
	_, err := s.write.ExecContext(ctx, `
		INSERT INTO entity_links(paragraph_hash, entity_hash, mention_count)
		VALUES (?, ?, 1)
		ON CONFLICT(paragraph_hash, entity_hash) DO UPDATE SET
			mention_count = entity_links.mention_count + 1`,
		paragraphHash, entityHash)
	if err != nil {
		return fmt.Errorf("memorix/storage: 写 entity_links 失败: %w", err)
	}
	return nil
}

// EntityByHash 读取单个实体。
func (s *Store) EntityByHash(ctx context.Context, hash string) (*retrieval.Entity, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT hash, name, appearance_count FROM entities WHERE hash = ? AND (is_deleted IS NULL OR is_deleted = 0)`,
		hash)
	var entity retrieval.Entity
	if err := row.Scan(&entity.Hash, &entity.Name, &entity.AppearanceCount); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("memorix/storage: 查询实体失败: %w", err)
	}
	return &entity, nil
}

// EntityByName 按规范化名解析实体。
func (s *Store) EntityByName(ctx context.Context, name string) (*retrieval.Entity, error) {
	canonical := canonicalEntityName(name)
	if canonical == "" {
		return nil, nil
	}
	row := s.read.QueryRowContext(ctx,
		`SELECT hash, name, appearance_count FROM entities WHERE name = ? AND (is_deleted IS NULL OR is_deleted = 0)`,
		canonical)
	var entity retrieval.Entity
	if err := row.Scan(&entity.Hash, &entity.Name, &entity.AppearanceCount); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("memorix/storage: 按名查询实体失败: %w", err)
	}
	return &entity, nil
}

// EntitiesByHashes 批量读取实体。
func (s *Store) EntitiesByHashes(ctx context.Context, hashes []string) (map[string]*retrieval.Entity, error) {
	out := map[string]*retrieval.Entity{}
	if len(hashes) == 0 {
		return out, nil
	}
	for _, chunk := range chunkStrings(hashes, 400) {
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, h := range chunk {
			placeholders[i] = "?"
			args[i] = h
		}
		rows, err := s.read.QueryContext(ctx,
			`SELECT hash, name, appearance_count FROM entities WHERE hash IN (`+
				strings.Join(placeholders, ",")+`) AND (is_deleted IS NULL OR is_deleted = 0)`,
			args...)
		if err != nil {
			return nil, fmt.Errorf("memorix/storage: 批量查询实体失败: %w", err)
		}
		for rows.Next() {
			var entity retrieval.Entity
			if err := rows.Scan(&entity.Hash, &entity.Name, &entity.AppearanceCount); err != nil {
				rows.Close()
				return nil, err
			}
			out[entity.Hash] = &entity
		}
		rows.Close()
	}
	return out, nil
}

// EntityNamesByParagraphs 返回段落关联的实体名（供 PPR 增益与后验图门）。
func (s *Store) EntityNamesByParagraphs(ctx context.Context, paragraphHashes []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(paragraphHashes) == 0 {
		return out, nil
	}
	for _, chunk := range chunkStrings(paragraphHashes, 300) {
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, h := range chunk {
			placeholders[i] = "?"
			args[i] = h
		}
		rows, err := s.read.QueryContext(ctx, `
			SELECT el.paragraph_hash, e.name
			FROM entity_links el JOIN entities e ON e.hash = el.entity_hash
			WHERE el.paragraph_hash IN (`+strings.Join(placeholders, ",")+`)
			ORDER BY el.paragraph_hash, e.name`, args...)
		if err != nil {
			return nil, fmt.Errorf("memorix/storage: 查询段落实体失败: %w", err)
		}
		for rows.Next() {
			var paragraphHash, name string
			if err := rows.Scan(&paragraphHash, &name); err != nil {
				rows.Close()
				return nil, err
			}
			out[paragraphHash] = append(out[paragraphHash], name)
		}
		rows.Close()
	}
	return out, nil
}

// EntityNames 返回全部实体名（供 Aho-Corasick 建库）。
func (s *Store) EntityNames(ctx context.Context) ([]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT name FROM entities WHERE (is_deleted IS NULL OR is_deleted = 0) ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: 查询实体名失败: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 关系
// ---------------------------------------------------------------------------

// UpsertRelation 幂等写入关系，返回 hash 与是否新建。
// 出处：core/storage/metadata_store.py add_relation。
func (s *Store) UpsertRelation(
	ctx context.Context, subject, predicate, object string, confidence float64, sourceParagraph string, metadata map[string]any,
) (string, bool, error) {
	hash, err := ComputeRelationHash(subject, predicate, object)
	if err != nil {
		return "", false, err
	}
	if confidence <= 0 {
		confidence = 1.0
	}
	metaJSON, err := encodeJSON(metadata)
	if err != nil {
		return "", false, err
	}
	now := nowTS()
	canonSubject := CanonicalizeName(subject)
	canonPredicate := CanonicalizeName(predicate)
	canonObject := CanonicalizeName(object)
	result, err := s.write.ExecContext(ctx, `
		INSERT INTO relations(hash, subject, predicate, object, confidence, created_at, source_paragraph, metadata, is_inactive)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(hash) DO UPDATE SET
			confidence = MAX(relations.confidence, excluded.confidence),
			metadata = CASE WHEN excluded.metadata IS NOT NULL THEN excluded.metadata ELSE relations.metadata END,
			is_inactive = 0`,
		hash, canonSubject, canonPredicate, canonObject, confidence, now, sourceParagraph, metaJSON)
	if err != nil {
		return "", false, fmt.Errorf("memorix/storage: 写 relations 失败: %w", err)
	}
	affected, _ := result.RowsAffected()
	_ = affected
	// 判断是否新建：插入前不存在即视为新建。
	var count int
	_ = s.read.QueryRowContext(ctx, `SELECT COUNT(1) FROM paragraph_links WHERE relation_hash = ?`, hash).Scan(&count)
	return hash, count == 0, nil
}

// LinkParagraphRelation 关联段落与关系（幂等）。
// 出处：core/storage/metadata_store.py link_paragraph_relation。
func (s *Store) LinkParagraphRelation(ctx context.Context, paragraphHash, relationHash string) error {
	_, err := s.write.ExecContext(ctx, `
		INSERT OR IGNORE INTO paragraph_links(paragraph_hash, relation_hash, created_at)
		VALUES (?, ?, ?)`, paragraphHash, relationHash, nowTS())
	if err != nil {
		return fmt.Errorf("memorix/storage: 写 paragraph_links 失败: %w", err)
	}
	return nil
}

// RelationByHash 读取单个关系。
func (s *Store) RelationByHash(ctx context.Context, hash string) (*retrieval.Relation, error) {
	rows, err := s.queryRelations(ctx,
		`WHERE hash = ? AND (is_inactive IS NULL OR is_inactive = 0)`, hash)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// RelationsByHashes 批量读取关系。
func (s *Store) RelationsByHashes(ctx context.Context, hashes []string) (map[string]*retrieval.Relation, error) {
	out := map[string]*retrieval.Relation{}
	if len(hashes) == 0 {
		return out, nil
	}
	for _, chunk := range chunkStrings(hashes, 400) {
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, h := range chunk {
			placeholders[i] = "?"
			args[i] = h
		}
		rows, err := s.queryRelations(ctx,
			`WHERE hash IN (`+strings.Join(placeholders, ",")+`) AND (is_inactive IS NULL OR is_inactive = 0)`,
			args...)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			out[row.Hash] = row
		}
	}
	return out, nil
}

func (s *Store) queryRelations(ctx context.Context, where string, args ...any) ([]*retrieval.Relation, error) {
	sqlText := `SELECT hash, subject, predicate, object, confidence, created_at, metadata FROM relations ` + where
	rows, err := s.read.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: 查询关系失败: %w", err)
	}
	defer rows.Close()
	var out []*retrieval.Relation
	for rows.Next() {
		var (
			relation  retrieval.Relation
			createdAt float64
			metaJSON  sql.NullString
		)
		if err := rows.Scan(&relation.Hash, &relation.Subject, &relation.Predicate, &relation.Object,
			&relation.Confidence, &createdAt, &metaJSON); err != nil {
			return nil, err
		}
		relation.CreatedAt = tsToTime(createdAt)
		relation.Metadata = decodeJSONMap(metaJSON)
		out = append(out, &relation)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 外部幂等映射
// ---------------------------------------------------------------------------

// GetExternalRef 读取外部 id 的幂等映射，未命中返回 (nil, nil)。
func (s *Store) GetExternalRef(ctx context.Context, externalID string) (map[string]any, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT paragraph_hash, source_type, metadata FROM external_refs WHERE external_id = ?`, externalID)
	var (
		paragraphHash string
		sourceType    string
		metaJSON      sql.NullString
	)
	if err := row.Scan(&paragraphHash, &sourceType, &metaJSON); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("memorix/storage: 查询 external_refs 失败: %w", err)
	}
	return map[string]any{
		"paragraph_hash": paragraphHash,
		"source_type":    sourceType,
		"metadata":       decodeJSONMap(metaJSON),
	}, nil
}

// UpsertExternalRef 写入外部 id → 段落映射。
func (s *Store) UpsertExternalRef(
	ctx context.Context, externalID, paragraphHash, sourceType string, metadata map[string]any,
) error {
	metaJSON, err := encodeJSON(metadata)
	if err != nil {
		return err
	}
	_, err = s.write.ExecContext(ctx, `
		INSERT INTO external_refs(external_id, paragraph_hash, source_type, metadata, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(external_id) DO UPDATE SET
			paragraph_hash=excluded.paragraph_hash,
			source_type=excluded.source_type,
			metadata=excluded.metadata`,
		externalID, paragraphHash, sourceType, metaJSON, nowTS())
	if err != nil {
		return fmt.Errorf("memorix/storage: 写 external_refs 失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// profiles / facts
// ---------------------------------------------------------------------------

// ProfileFact 是人物事实（对齐 contracts.ProfileFact 的持久化形态）。
type ProfileFact struct {
	ID         string
	PersonID   string
	Category   string
	Content    string
	Confidence float64
	Source     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// UpsertProfileFact 幂等写入人物事实（同 person + content 覆盖置信度）。
func (s *Store) UpsertProfileFact(ctx context.Context, fact ProfileFact) error {
	if strings.TrimSpace(fact.PersonID) == "" {
		return fmt.Errorf("memorix/storage: person_id 不能为空")
	}
	content := strings.TrimSpace(fact.Content)
	if content == "" {
		return fmt.Errorf("memorix/storage: fact 内容不能为空")
	}
	if fact.ID == "" {
		fact.ID = ComputeHash(fact.PersonID + "|" + content)
	}
	category := strings.TrimSpace(fact.Category)
	if category == "" {
		category = "stable_facts"
	}
	confidence := fact.Confidence
	if confidence <= 0 {
		confidence = 1.0
	}
	now := nowTS()
	_, err := s.write.ExecContext(ctx, `
		INSERT INTO profile_facts(fact_id, person_id, category, content, confidence, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(person_id, content) DO UPDATE SET
			category=excluded.category,
			confidence=MAX(profile_facts.confidence, excluded.confidence),
			source=excluded.source,
			updated_at=excluded.updated_at`,
		fact.ID, fact.PersonID, category, content, confidence, fact.Source, now, now)
	if err != nil {
		return fmt.Errorf("memorix/storage: 写 profile_facts 失败: %w", err)
	}
	_, err = s.write.ExecContext(ctx, `
		INSERT INTO profiles(person_id, name, summary, created_at, updated_at)
		VALUES (?, '', '', ?, ?)
		ON CONFLICT(person_id) DO UPDATE SET updated_at=excluded.updated_at`,
		fact.PersonID, now, now)
	if err != nil {
		return fmt.Errorf("memorix/storage: 写 profiles 失败: %w", err)
	}
	return nil
}

// ProfileFacts 读取某人物的全部事实（更新倒序）。
func (s *Store) ProfileFacts(ctx context.Context, personID string, limit int) ([]ProfileFact, error) {
	sqlText := `SELECT fact_id, person_id, category, content, confidence, source, created_at, updated_at
		FROM profile_facts WHERE person_id = ? ORDER BY updated_at DESC, fact_id`
	args := []any{personID}
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.read.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: 查询 profile_facts 失败: %w", err)
	}
	defer rows.Close()
	var out []ProfileFact
	for rows.Next() {
		var (
			fact        ProfileFact
			createdAt   float64
			updatedAt   float64
			sourceValue sql.NullString
		)
		if err := rows.Scan(&fact.ID, &fact.PersonID, &fact.Category, &fact.Content, &fact.Confidence,
			&sourceValue, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		fact.Source = scanNullString(sourceValue)
		fact.CreatedAt = tsToTime(createdAt)
		fact.UpdatedAt = tsToTime(updatedAt)
		out = append(out, fact)
	}
	return out, rows.Err()
}

// UpsertProfile 写入人物画像是主表信息（姓名/摘要）。
func (s *Store) UpsertProfile(ctx context.Context, personID, name, summary string) error {
	if strings.TrimSpace(personID) == "" {
		return fmt.Errorf("memorix/storage: person_id 不能为空")
	}
	now := nowTS()
	_, err := s.write.ExecContext(ctx, `
		INSERT INTO profiles(person_id, name, summary, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(person_id) DO UPDATE SET
			name=CASE WHEN excluded.name <> '' THEN excluded.name ELSE profiles.name END,
			summary=CASE WHEN excluded.summary <> '' THEN excluded.summary ELSE profiles.summary END,
			updated_at=excluded.updated_at`,
		personID, strings.TrimSpace(name), strings.TrimSpace(summary), now, now)
	if err != nil {
		return fmt.Errorf("memorix/storage: 写 profiles 失败: %w", err)
	}
	return nil
}

// Profile 读取人物画像主表，未命中返回 (nil,nil,nil)。
func (s *Store) Profile(ctx context.Context, personID string) (name, summary string, updatedAt time.Time, err error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT name, summary, updated_at FROM profiles WHERE person_id = ?`, personID)
	var ts float64
	if scanErr := row.Scan(&name, &summary, &ts); scanErr != nil {
		if scanErr == sql.ErrNoRows {
			return "", "", time.Time{}, nil
		}
		return "", "", time.Time{}, fmt.Errorf("memorix/storage: 查询 profiles 失败: %w", scanErr)
	}
	return name, summary, tsToTime(ts), nil
}

// RelationsForEntity 返回以实体名为 subject 或 object 的活跃关系。
// 出处：core/storage/metadata_store.py get_relations(subject=..., object=...)。
func (s *Store) RelationsForEntity(ctx context.Context, name string) ([]*retrieval.Relation, error) {
	canonical := canonicalEntityName(name)
	if canonical == "" {
		return nil, nil
	}
	return s.queryRelations(ctx,
		`WHERE (subject = ? OR object = ?) AND (is_inactive IS NULL OR is_inactive = 0)
		 ORDER BY confidence DESC, hash`, canonical, canonical)
}

// RelationsForPerson 返回以某人物（实体名）为 subject/object 的关系。
func (s *Store) RelationsForPerson(ctx context.Context, personName string, limit int) ([]*retrieval.Relation, error) {
	canonical := canonicalEntityName(personName)
	if limit <= 0 {
		limit = 50
	}
	return s.queryRelations(ctx,
		`WHERE (subject = ? OR object = ?) AND (is_inactive IS NULL OR is_inactive = 0)
		 ORDER BY confidence DESC, hash LIMIT ?`,
		canonical, canonical, limit)
}

// ---------------------------------------------------------------------------
// episodes
// ---------------------------------------------------------------------------

// Episode 是情景记忆。
type Episode struct {
	ID             string
	Source         string
	Title          string
	Summary        string
	EventStart     *float64
	EventEnd       *float64
	Participants   []string
	Keywords       []string
	EvidenceIDs    []string
	ParagraphCount int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UpsertEpisode 幂等写入 episode（同 id 覆盖）。
func (s *Store) UpsertEpisode(ctx context.Context, episode Episode) (string, error) {
	if strings.TrimSpace(episode.Title) == "" && strings.TrimSpace(episode.Summary) == "" {
		return "", fmt.Errorf("memorix/storage: episode 标题与摘要均为空")
	}
	if episode.ID == "" {
		episode.ID = ComputeHash(episode.Source + "|" + episode.Title + "|" + episode.Summary)
	}
	participants, _ := encodeJSON(episode.Participants)
	keywords, _ := encodeJSON(episode.Keywords)
	evidence, _ := encodeJSON(episode.EvidenceIDs)
	now := nowTS()
	_, err := s.write.ExecContext(ctx, `
		INSERT INTO episodes(episode_id, source, title, summary, event_time_start, event_time_end,
			participants_json, keywords_json, evidence_ids_json, paragraph_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(episode_id) DO UPDATE SET
			title=excluded.title, summary=excluded.summary,
			event_time_start=COALESCE(excluded.event_time_start, episodes.event_time_start),
			event_time_end=COALESCE(excluded.event_time_end, episodes.event_time_end),
			participants_json=excluded.participants_json,
			keywords_json=excluded.keywords_json,
			evidence_ids_json=excluded.evidence_ids_json,
			paragraph_count=excluded.paragraph_count,
			updated_at=excluded.updated_at`,
		episode.ID, episode.Source, episode.Title, episode.Summary,
		nullableFloat(episode.EventStart), nullableFloat(episode.EventEnd),
		participants, keywords, evidence, episode.ParagraphCount, now, now)
	if err != nil {
		return "", fmt.Errorf("memorix/storage: 写 episodes 失败: %w", err)
	}
	for position, hash := range episode.EvidenceIDs {
		if _, err := s.write.ExecContext(ctx, `
			INSERT OR IGNORE INTO episode_paragraphs(episode_id, paragraph_hash, position)
			VALUES (?, ?, ?)`, episode.ID, hash, position); err != nil {
			return "", fmt.Errorf("memorix/storage: 写 episode_paragraphs 失败: %w", err)
		}
	}
	return episode.ID, nil
}

// SearchEpisodes 按词法融合检索 episode（对齐 EpisodeRetrievalService.query 的
// lexical 分支：title/summary/keywords/participants 的 token 覆盖度累加打分）。
// 出处：core/storage/metadata_episode.py L1505-L1557。
func (s *Store) SearchEpisodes(ctx context.Context, tokens []string, limit int) ([]Episode, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.read.QueryContext(ctx, `
		SELECT episode_id, source, title, summary, event_time_start, event_time_end,
			participants_json, keywords_json, evidence_ids_json, paragraph_count, created_at, updated_at
		FROM episodes ORDER BY COALESCE(event_time_end, updated_at) DESC, updated_at DESC LIMIT ?`,
		maxIntStorage(limit*8, 200))
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: 查询 episodes 失败: %w", err)
	}
	defer rows.Close()
	type scored struct {
		episode Episode
		score   float64
	}
	var all []scored
	for rows.Next() {
		var (
			episode      Episode
			eventStart   sql.NullFloat64
			eventEnd     sql.NullFloat64
			participants sql.NullString
			keywords     sql.NullString
			evidence     sql.NullString
			createdAt    float64
			updatedAt    float64
		)
		if err := rows.Scan(&episode.ID, &episode.Source, &episode.Title, &episode.Summary,
			&eventStart, &eventEnd, &participants, &keywords, &evidence,
			&episode.ParagraphCount, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if eventStart.Valid {
			v := eventStart.Float64
			episode.EventStart = &v
		}
		if eventEnd.Valid {
			v := eventEnd.Float64
			episode.EventEnd = &v
		}
		episode.Participants = decodeJSONStrings(participants)
		episode.Keywords = decodeJSONStrings(keywords)
		episode.EvidenceIDs = decodeJSONStrings(evidence)
		episode.CreatedAt = tsToTime(createdAt)
		episode.UpdatedAt = tsToTime(updatedAt)
		all = append(all, scored{episode: episode, score: lexicalEpisodeScore(episode, tokens)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		// 无 query token 时按时间倒序返回。
		out := make([]Episode, 0, minIntStorage(limit, len(all)))
		for i := 0; i < len(all) && i < limit; i++ {
			out = append(out, all[i].episode)
		}
		return out, nil
	}
	// 稳定排序：词法分数降序，其次更新时间倒序。
	for i := 1; i < len(all); i++ {
		for j := i; j > 0; j-- {
			if all[j].score > all[j-1].score {
				all[j], all[j-1] = all[j-1], all[j]
				continue
			}
			break
		}
	}
	out := make([]Episode, 0, minIntStorage(limit, len(all)))
	for _, item := range all {
		if item.score <= 0 {
			continue
		}
		out = append(out, item.episode)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// lexicalEpisodeScore 计算 episode 的词法覆盖度得分（对齐 field_exprs 打分子集）。
func lexicalEpisodeScore(episode Episode, tokens []string) float64 {
	if len(tokens) == 0 {
		return 0
	}
	lowerTitle := strings.ToLower(episode.Title)
	lowerSummary := strings.ToLower(episode.Summary)
	fields := []string{lowerTitle, lowerSummary}
	for _, k := range episode.Keywords {
		fields = append(fields, strings.ToLower(k))
	}
	for _, p := range episode.Participants {
		fields = append(fields, strings.ToLower(p))
	}
	score := 0.0
	for _, token := range tokens {
		token = strings.ToLower(strings.TrimSpace(token))
		if token == "" {
			continue
		}
		for index, field := range fields {
			if !strings.Contains(field, token) {
				continue
			}
			// title/summary 为主字段，keywords/participants 权重略低。
			weight := 1.0
			if index >= 2 {
				weight = 0.6
			}
			score += weight
		}
	}
	return score
}

// EpisodesByIDs 批量读取 episode。
func (s *Store) EpisodesByIDs(ctx context.Context, ids []string) (map[string]Episode, error) {
	out := map[string]Episode{}
	if len(ids) == 0 {
		return out, nil
	}
	for _, chunk := range chunkStrings(ids, 300) {
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := s.read.QueryContext(ctx, `
			SELECT episode_id, source, title, summary, event_time_start, event_time_end,
				participants_json, keywords_json, evidence_ids_json, paragraph_count, created_at, updated_at
			FROM episodes WHERE episode_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("memorix/storage: 批量查询 episodes 失败: %w", err)
		}
		for rows.Next() {
			var (
				episode      Episode
				eventStart   sql.NullFloat64
				eventEnd     sql.NullFloat64
				participants sql.NullString
				keywords     sql.NullString
				evidence     sql.NullString
				createdAt    float64
				updatedAt    float64
			)
			if err := rows.Scan(&episode.ID, &episode.Source, &episode.Title, &episode.Summary,
				&eventStart, &eventEnd, &participants, &keywords, &evidence,
				&episode.ParagraphCount, &createdAt, &updatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			if eventStart.Valid {
				v := eventStart.Float64
				episode.EventStart = &v
			}
			if eventEnd.Valid {
				v := eventEnd.Float64
				episode.EventEnd = &v
			}
			episode.Participants = decodeJSONStrings(participants)
			episode.Keywords = decodeJSONStrings(keywords)
			episode.EvidenceIDs = decodeJSONStrings(evidence)
			episode.CreatedAt = tsToTime(createdAt)
			episode.UpdatedAt = tsToTime(updatedAt)
			out[episode.ID] = episode
		}
		rows.Close()
	}
	return out, nil
}

// EpisodesByParagraph 返回包含指定段落的 episode id 列表。
func (s *Store) EpisodesByParagraph(ctx context.Context, paragraphHash string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT episode_id FROM episode_paragraphs WHERE paragraph_hash = ?`, paragraphHash)
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: 查询 episode_paragraphs 失败: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func encodeJSON(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			return nil, nil
		}
	case []string:
		if len(v) == 0 {
			return nil, nil
		}
	}
	buf, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: JSON 编码失败: %w", err)
	}
	return string(buf), nil
}

func decodeJSONMap(raw sql.NullString) map[string]any {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return map[string]any{}
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return map[string]any{}
	}
	return out
}

func decodeJSONStrings(raw sql.NullString) []string {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return nil
	}
	return out
}

func nullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// CountRunes 返回文本的 rune 数（对齐 Python len()）。
func CountRunes(text string) int { return len([]rune(text)) }

func canonicalEntityName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func chunkStrings(values []string, size int) [][]string {
	if size <= 0 {
		size = 100
	}
	var out [][]string
	seen := map[string]struct{}{}
	unique := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		unique = append(unique, v)
	}
	for start := 0; start < len(unique); start += size {
		end := start + size
		if end > len(unique) {
			end = len(unique)
		}
		out = append(out, unique[start:end])
	}
	return out
}

func maxIntStorage(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minIntStorage(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = sql.ErrNoRows
