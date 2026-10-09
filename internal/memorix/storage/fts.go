package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"oh-my-qqbot/internal/memorix/retrieval"
)

// SearchParagraphsFTS 检索段落 BM25。
// 对齐 Python SparseBM25Index.search 的三级回落（core/retrieval/sparse_bm25.py L541-L607）：
//  1. paragraphs_tokenized_fts（预分词 shadow index，中文可召回）
//  2. paragraphs_fts（external-content unicode61 全文索引）
//  3. 子串兜底（enable_like_fallback 默认关闭 → 返回空）
func (s *Store) SearchParagraphsFTS(
	ctx context.Context, matchQuery string, limit int, allowedIDs []string,
) ([]retrieval.SparseRow, error) {
	if strings.TrimSpace(matchQuery) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if allowedIDs != nil && len(allowedIDs) == 0 {
		return nil, nil
	}
	if rows, err := s.searchTokenizedParagraphsFTS(ctx, matchQuery, limit, allowedIDs); err != nil {
		return nil, err
	} else if len(rows) > 0 {
		return rows, nil
	}
	return s.searchRawParagraphsFTS(ctx, matchQuery, limit, allowedIDs)
}

// searchTokenizedParagraphsFTS 查预分词 shadow index。
// 出处：core/storage/metadata_fts.py L376-L420。
func (s *Store) searchTokenizedParagraphsFTS(
	ctx context.Context, matchQuery string, limit int, allowedIDs []string,
) ([]retrieval.SparseRow, error) {
	scopeClause, args := scopeFilter("p.hash", matchQuery, allowedIDs)
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, `
		SELECT p.hash, p.content, bm25(paragraphs_tokenized_fts) AS bm25_score
		FROM paragraphs_tokenized_fts
		JOIN paragraphs p ON p.hash = paragraphs_tokenized_fts.paragraph_hash
		WHERE paragraphs_tokenized_fts MATCH ?
		  AND (p.is_deleted IS NULL OR p.is_deleted = 0)`+scopeClause+`
		ORDER BY bm25_score ASC, p.hash ASC
		LIMIT ?`, args...)
	if err != nil {
		// tokenized 表不可用（未建或 FTS5 缺失）→ 交由上一级回落。
		return nil, nil
	}
	return scanSparseRows(rows)
}

// searchRawParagraphsFTS 查 external-content 全文索引。
// 出处：core/storage/metadata_fts.py L758-L806。
func (s *Store) searchRawParagraphsFTS(
	ctx context.Context, matchQuery string, limit int, allowedIDs []string,
) ([]retrieval.SparseRow, error) {
	scopeClause, args := scopeFilter("p.hash", matchQuery, allowedIDs)
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, `
		SELECT p.hash, p.content, bm25(paragraphs_fts) AS bm25_score
		FROM paragraphs_fts
		JOIN paragraphs p ON p.rowid = paragraphs_fts.rowid
		WHERE paragraphs_fts MATCH ?
		  AND (p.is_deleted IS NULL OR p.is_deleted = 0)`+scopeClause+`
		ORDER BY bm25_score ASC, p.hash ASC
		LIMIT ?`, args...)
	if err != nil {
		return nil, nil
	}
	return scanSparseRows(rows)
}

// SearchRelationsFTS 检索关系 BM25。
// 出处：core/storage/metadata_fts.py L807-L860。
func (s *Store) SearchRelationsFTS(
	ctx context.Context, matchQuery string, limit int, allowedIDs []string,
) ([]retrieval.SparseRow, error) {
	if strings.TrimSpace(matchQuery) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if allowedIDs != nil && len(allowedIDs) == 0 {
		return nil, nil
	}
	scopeClause, args := scopeFilter("r.hash", matchQuery, allowedIDs)
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, `
		SELECT r.hash, r.subject, r.predicate, r.object, bm25(relations_fts) AS bm25_score
		FROM relations_fts
		JOIN relations r ON r.hash = relations_fts.relation_hash
		WHERE relations_fts MATCH ?
		  AND (r.is_inactive IS NULL OR r.is_inactive = 0)`+scopeClause+`
		ORDER BY bm25_score ASC, r.hash ASC
		LIMIT ?`, args...)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	out := make([]retrieval.SparseRow, 0, limit)
	for rows.Next() {
		var row retrieval.SparseRow
		if err := rows.Scan(&row.Hash, &row.Subject, &row.Predicate, &row.Object, &row.BM25Score); err != nil {
			return nil, fmt.Errorf("memorix/storage: 解析 FTS 关系行失败: %w", err)
		}
		row.Content = strings.TrimSpace(row.Subject + " " + row.Predicate + " " + row.Object)
		out = append(out, row)
	}
	return out, rows.Err()
}

// scopeFilter 构造 IN (...) 范围过滤片段。
func scopeFilter(column, matchQuery string, allowedIDs []string) (string, []any) {
	args := []any{matchQuery}
	if allowedIDs == nil {
		return "", args
	}
	placeholders := make([]string, len(allowedIDs))
	for i, id := range allowedIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	return " AND " + column + " IN (" + strings.Join(placeholders, ",") + ")", args
}

func scanSparseRows(rows *sql.Rows) ([]retrieval.SparseRow, error) {
	defer rows.Close()
	var out []retrieval.SparseRow
	for rows.Next() {
		var row retrieval.SparseRow
		if err := rows.Scan(&row.Hash, &row.Content, &row.BM25Score); err != nil {
			return nil, fmt.Errorf("memorix/storage: 解析 FTS 行失败: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// upsertTokenizedParagraph 增量维护预分词 shadow index（写入段落时调用）。
// 出处：core/storage/metadata_fts.py L313-L360（fts_upsert_tokenized_paragraph）。
func (s *Store) upsertTokenizedParagraph(ctx context.Context, hash, content string) error {
	tokenized := s.tokenizeForFTS(content)
	if hasTable(ctx, s, "paragraphs_tokenized_fts") {
		if _, err := s.write.ExecContext(ctx,
			`DELETE FROM paragraphs_tokenized_fts WHERE paragraph_hash = ?`, hash); err != nil {
			return nil // shadow index 不可用时静默跳过
		}
		if _, err := s.write.ExecContext(ctx,
			`INSERT INTO paragraphs_tokenized_fts(paragraph_hash, tokenized) VALUES (?, ?)`,
			hash, tokenized); err != nil {
			return nil
		}
	}
	// 无条件写段落 hash 索引表（供 admin 面与 stats 使用）。
	if hasTable(ctx, s, "paragraph_token_index") {
		if _, err := s.write.ExecContext(ctx,
			`INSERT INTO paragraph_token_index(paragraph_hash, tokenized, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT(paragraph_hash) DO UPDATE SET tokenized=excluded.tokenized, updated_at=excluded.updated_at`,
			hash, tokenized, nowTS()); err != nil {
			return nil
		}
	}
	return nil
}

// tokenizeForFTS 复现 Python _tokenize_paragraph_for_fts：
// jieba cut_for_search（Go 用 gse）+ 短语正则 + 去重。
// 出处：core/storage/metadata_fts.py L226-L240。
func (s *Store) tokenizeForFTS(text string) string {
	s.tokenizerMu.Lock()
	defer s.tokenizerMu.Unlock()
	if s.tokenizer == nil {
		s.tokenizer = retrieval.NewTokenizer(retrieval.DefaultSparseTokenizerMode, retrieval.DefaultSparseCharNgramN)
	}
	return strings.Join(s.tokenizer.Tokenize(text), " ")
}

// backfillTokenizedFTS 回填预分词 shadow index（启动或数据修复时调用）。
// 出处：core/storage/metadata_fts.py L263-L311。
func (s *Store) backfillTokenizedFTS(ctx context.Context) error {
	if !hasTable(ctx, s, "paragraphs_tokenized_fts") {
		return nil
	}
	var paraCount int
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM paragraphs WHERE (is_deleted IS NULL OR is_deleted = 0)`).Scan(&paraCount); err != nil {
		return fmt.Errorf("memorix/storage: 统计段落失败: %w", err)
	}
	var indexedCount int
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT paragraph_hash) FROM paragraphs_tokenized_fts`).Scan(&indexedCount); err != nil {
		return nil
	}
	if indexedCount == paraCount {
		return nil
	}
	if _, err := s.write.ExecContext(ctx, `DELETE FROM paragraphs_tokenized_fts`); err != nil {
		return fmt.Errorf("memorix/storage: 清空预分词索引失败: %w", err)
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT hash, content FROM paragraphs WHERE (is_deleted IS NULL OR is_deleted = 0)`)
	if err != nil {
		return fmt.Errorf("memorix/storage: 读取段落失败: %w", err)
	}
	var batch []struct{ hash, tokenized string }
	for rows.Next() {
		var hash, content string
		if err := rows.Scan(&hash, &content); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, struct{ hash, tokenized string }{hash: hash, tokenized: s.tokenizeForFTS(content)})
		if len(batch) >= 1000 {
			if err := s.insertTokenizedBatch(ctx, batch); err != nil {
				rows.Close()
				return err
			}
			batch = batch[:0]
		}
	}
	rows.Close()
	if len(batch) > 0 {
		if err := s.insertTokenizedBatch(ctx, batch); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) insertTokenizedBatch(ctx context.Context, batch []struct{ hash, tokenized string }) error {
	for _, item := range batch {
		if _, err := s.write.ExecContext(ctx,
			`INSERT INTO paragraphs_tokenized_fts(paragraph_hash, tokenized) VALUES (?, ?)`,
			item.hash, item.tokenized); err != nil {
			return fmt.Errorf("memorix/storage: 回填预分词索引失败: %w", err)
		}
	}
	return nil
}

// hasTable 判定表/虚表是否存在。
func hasTable(ctx context.Context, s *Store, name string) bool {
	var found string
	err := s.read.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE name = ?`, name).Scan(&found)
	return err == nil && found == name
}

// FTSSearchable 报告 BM25 检索路径是否可用（任一级索引存在即可）。
func (s *Store) FTSSearchable(ctx context.Context) bool {
	return hasTable(ctx, s, "paragraphs_fts") || hasTable(ctx, s, "paragraphs_tokenized_fts")
}

// RebuildFTS 重建全部全文索引（数据修复/admin 动作）。
func (s *Store) RebuildFTS(ctx context.Context) error {
	if hasTable(ctx, s, "paragraphs_fts") {
		if _, err := s.write.ExecContext(ctx, `INSERT INTO paragraphs_fts(paragraphs_fts) VALUES ('rebuild')`); err != nil {
			return fmt.Errorf("memorix/storage: paragraphs_fts rebuild 失败: %w", err)
		}
	}
	if hasTable(ctx, s, "relations_fts") {
		if _, err := s.write.ExecContext(ctx, `DELETE FROM relations_fts`); err != nil {
			return fmt.Errorf("memorix/storage: relations_fts 清空失败: %w", err)
		}
		if _, err := s.write.ExecContext(ctx, `
			INSERT INTO relations_fts(relation_hash, content)
			SELECT hash, COALESCE(subject,'') || ' ' || COALESCE(predicate,'') || ' ' || COALESCE(object,'')
			FROM relations`); err != nil {
			return fmt.Errorf("memorix/storage: relations_fts 回填失败: %w", err)
		}
	}
	return s.backfillTokenizedFTS(ctx)
}

// EncodeVectorForTest 暴露向量编码（供测试断言 BLOB 布局）。
func EncodeVectorForTest(vec []float32) []byte { return encodeVector(vec) }

// DecodeVectorForTest 暴露向量解码（供测试断言）。
func DecodeVectorForTest(buf []byte, dim int) []float32 { return decodeVector(buf, dim) }
