// Package storage 实现 A_memorix 内核的存储层：SQLite（modernc.org/sqlite）
// 单文件库 + FTS5 虚表 + 向量表 + 内存 flat 内积索引。
//
// 表结构对齐参考/MaiBot/src/A_memorix/core/storage/metadata_schema.py：
//   - paragraphs / entities / relations / episodes（L1264-L1340、L1530-L1553）
//   - paragraph_relations / paragraph_entities 链接表（L1391-L1416）
//   - paragraphs_fts / relations_fts 虚表（core/storage/metadata_fts.py L34-L160）
//
// Go 版新增：vectors 表（Python 用 faiss 文件，Go 用 SQLite BLOB）与
// profiles / profile_facts（对齐 person_profile_snapshots 与 fact_claims 的核心面）。
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"oh-my-qqbot/internal/memorix/retrieval"
)

// SchemaVersion 是当前 schema 版本（对齐 metadata_schema.schema_migrations）。
const SchemaVersion = 1

// DefaultDBFileName 是内核库文件名（对齐 §6.14：data/a-memorix/a_memorix.db）。
const DefaultDBFileName = "a_memorix.db"

// Store 是 A_memorix 存储层：单写连接 + 读连接池（对齐 Diana/database 的单写多读形态），
// 外加内存 flat 向量索引。
type Store struct {
	write *sql.DB
	read  *sql.DB
	path  string

	vectorMu  sync.RWMutex
	vectors   *VectorIndex
	dim       int
	quantType string

	graphMu  sync.Mutex
	graph    *retrieval.Graph
	graphRev int64

	tokenizerMu sync.Mutex
	tokenizer   *retrieval.Tokenizer
}

// OpenOptions 打开存储层的参数。
type OpenOptions struct {
	// Path 是 SQLite 文件路径；空则用 Dir/a_memorix.db。
	Path string
	// Dir 是数据目录（Path 为空时使用）。
	Dir string
	// MaxReadConns 读连接池大小（默认 8）。
	MaxReadConns int
	// BusyTimeoutMS SQLite busy_timeout（默认 5000）。
	BusyTimeoutMS int
	// QuantizationType 对齐 embedding.quantization_type（默认 int8，仅记录用途）。
	QuantizationType string
	// EnableFTS 是否创建 FTS5 虚表（默认 true；不可用时自动降级）。
	EnableFTS bool
}

// DefaultOpenOptions 返回默认打开参数。
func DefaultOpenOptions() OpenOptions {
	return OpenOptions{
		MaxReadConns:     8,
		BusyTimeoutMS:    5000,
		QuantizationType: "int8",
		EnableFTS:        true,
	}
}

// dsn 组装 modernc.org/sqlite 连接串（WAL + busy_timeout + foreign_keys）。
func (o OpenOptions) dsn(path string) string {
	return fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		filepath.ToSlash(path), o.BusyTimeoutMS,
	)
}

// Open 打开（必要时创建）内核库并初始化 schema、重建内存向量索引。
func Open(ctx context.Context, options OpenOptions) (*Store, error) {
	if options.MaxReadConns <= 0 {
		options.MaxReadConns = 8
	}
	if options.BusyTimeoutMS <= 0 {
		options.BusyTimeoutMS = 5000
	}
	path := options.Path
	if path == "" {
		dir := options.Dir
		if dir == "" {
			dir = filepath.Join("data", "a-memorix")
		}
		path = filepath.Join(dir, DefaultDBFileName)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("memorix/storage: 创建数据目录失败: %w", err)
	}

	dsn := options.dsn(path)
	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("memorix/storage: 打开写连接失败: %w", err)
	}
	// 单写连接：SQLite 只允许一个写者，显式串行化避免 SQLITE_BUSY。
	write.SetMaxOpenConns(1)
	write.SetConnMaxLifetime(0)

	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("memorix/storage: 打开读连接池失败: %w", err)
	}
	read.SetMaxOpenConns(options.MaxReadConns)

	store := &Store{
		write:     write,
		read:      read,
		path:      path,
		dim:       0,
		quantType: options.QuantizationType,
	}
	if err := store.migrate(ctx, options.EnableFTS); err != nil {
		write.Close()
		read.Close()
		return nil, err
	}
	if err := store.loadVectorIndex(ctx); err != nil {
		write.Close()
		read.Close()
		return nil, err
	}
	return store, nil
}

// Path 返回库文件路径。
func (s *Store) Path() string { return s.path }

// DB 返回写连接（供上层 admin/统计使用）。
func (s *Store) DB() *sql.DB { return s.write }

// Close 关闭连接。
func (s *Store) Close() error {
	var errs []error
	if s.read != nil {
		errs = append(errs, s.read.Close())
	}
	if s.write != nil {
		errs = append(errs, s.write.Close())
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// schema
// ---------------------------------------------------------------------------

func (s *Store) migrate(ctx context.Context, enableFTS bool) error {
	stmts := []string{
		// 段落表：对齐 metadata_schema.py L1264-L1284 paragraphs 的核心列，
		// 并补 chat_id / knowledge_type 以承载聊天记忆。
		`CREATE TABLE IF NOT EXISTS paragraphs (
			hash TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT '',
			chat_id TEXT NOT NULL DEFAULT '',
			word_count INTEGER DEFAULT 0,
			knowledge_type TEXT DEFAULT 'mixed',
			created_at REAL NOT NULL,
			updated_at REAL NOT NULL,
			event_time REAL,
			event_time_start REAL,
			event_time_end REAL,
			time_granularity TEXT,
			time_confidence REAL DEFAULT 1.0,
			metadata TEXT,
			is_permanent INTEGER DEFAULT 0,
			access_count INTEGER DEFAULT 0,
			last_accessed REAL,
			is_deleted INTEGER DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_paragraphs_chat ON paragraphs(chat_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_paragraphs_event_start ON paragraphs(event_time_start)`,
		`CREATE INDEX IF NOT EXISTS idx_paragraphs_event_end ON paragraphs(event_time_end)`,
		`CREATE INDEX IF NOT EXISTS idx_paragraphs_source ON paragraphs(source)`,
		// 段落预分词索引表（对齐 Python paragraph_tokenized_fts_meta 的元信息面）。
		`CREATE TABLE IF NOT EXISTS paragraph_token_index (
			paragraph_hash TEXT PRIMARY KEY,
			tokenized TEXT NOT NULL,
			updated_at REAL NOT NULL
		)`,

		// 实体表：对齐 metadata_schema.py L1291-L1301。
		`CREATE TABLE IF NOT EXISTS entities (
			hash TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			appearance_count INTEGER DEFAULT 1,
			vector_index INTEGER,
			created_at REAL NOT NULL,
			metadata TEXT,
			is_deleted INTEGER DEFAULT 0
		)`,

		// 关系表：对齐 metadata_schema.py L1305-L1337（裁剪生命周期列）。
		`CREATE TABLE IF NOT EXISTS relations (
			hash TEXT PRIMARY KEY,
			subject TEXT NOT NULL,
			predicate TEXT NOT NULL,
			object TEXT NOT NULL,
			confidence REAL DEFAULT 1.0,
			created_at REAL NOT NULL,
			source_paragraph TEXT,
			metadata TEXT,
			is_inactive INTEGER DEFAULT 0,
			is_pinned INTEGER DEFAULT 0,
			access_count INTEGER DEFAULT 0,
			last_accessed REAL,
			UNIQUE(subject, predicate, object)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_relations_subject ON relations(subject)`,
		`CREATE INDEX IF NOT EXISTS idx_relations_object ON relations(object)`,
		`CREATE INDEX IF NOT EXISTS idx_relations_source ON relations(source_paragraph)`,

		// 段落 ↔ 关系链接表：对齐 metadata_schema.py L1391-L1397。
		`CREATE TABLE IF NOT EXISTS paragraph_links (
			paragraph_hash TEXT NOT NULL,
			relation_hash TEXT NOT NULL,
			created_at REAL NOT NULL,
			PRIMARY KEY (paragraph_hash, relation_hash)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_paragraph_links_relation ON paragraph_links(relation_hash)`,

		// 段落 ↔ 实体链接表：对齐 metadata_schema.py L1402-L1410。
		`CREATE TABLE IF NOT EXISTS entity_links (
			paragraph_hash TEXT NOT NULL,
			entity_hash TEXT NOT NULL,
			mention_count INTEGER DEFAULT 1,
			PRIMARY KEY (paragraph_hash, entity_hash)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_entity_links_entity ON entity_links(entity_hash)`,

		// episode 情景记忆：对齐 metadata_schema.py L1530-L1551。
		`CREATE TABLE IF NOT EXISTS episodes (
			episode_id TEXT PRIMARY KEY,
			source TEXT,
			title TEXT NOT NULL,
			summary TEXT NOT NULL,
			event_time_start REAL,
			event_time_end REAL,
			time_granularity TEXT,
			time_confidence REAL DEFAULT 1.0,
			participants_json TEXT,
			keywords_json TEXT,
			evidence_ids_json TEXT,
			paragraph_count INTEGER DEFAULT 0,
			llm_confidence REAL DEFAULT 0.0,
			created_at REAL NOT NULL,
			updated_at REAL NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_episodes_updated ON episodes(updated_at DESC)`,

		// episode ↔ 段落映射：对齐 metadata_schema.py L1555-L1563。
		`CREATE TABLE IF NOT EXISTS episode_paragraphs (
			episode_id TEXT NOT NULL,
			paragraph_hash TEXT NOT NULL,
			position INTEGER DEFAULT 0,
			PRIMARY KEY (episode_id, paragraph_hash)
		)`,

		// 人物画像主表：对齐 person_profile_snapshots 的核心面（L1471-L1486）。
		`CREATE TABLE IF NOT EXISTS profiles (
			person_id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			aliases_json TEXT,
			created_at REAL NOT NULL,
			updated_at REAL NOT NULL
		)`,

		// 人物事实：对齐 fact_claims 的核心面（metadata_fact.py L30-L51）。
		`CREATE TABLE IF NOT EXISTS profile_facts (
			fact_id TEXT PRIMARY KEY,
			person_id TEXT NOT NULL,
			category TEXT NOT NULL DEFAULT 'stable_facts',
			content TEXT NOT NULL,
			confidence REAL NOT NULL DEFAULT 1.0,
			source TEXT,
			created_at REAL NOT NULL,
			updated_at REAL NOT NULL,
			UNIQUE(person_id, content)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_profile_facts_person ON profile_facts(person_id, updated_at DESC)`,

		// 向量表（Go 版新增）：BLOB 存 float32，启动时重建内存 flat 内积索引。
		`CREATE TABLE IF NOT EXISTS vectors (
			id TEXT PRIMARY KEY,
			item_type TEXT NOT NULL,
			dim INTEGER NOT NULL,
			quantization TEXT NOT NULL DEFAULT 'float32',
			vector BLOB NOT NULL,
			norm REAL NOT NULL DEFAULT 0,
			created_at REAL NOT NULL,
			updated_at REAL NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_vectors_type ON vectors(item_type)`,

		// 外部幂等映射（对齐 external_memory_refs，用于 ingest 幂等）。
		`CREATE TABLE IF NOT EXISTS external_refs (
			external_id TEXT PRIMARY KEY,
			paragraph_hash TEXT NOT NULL,
			source_type TEXT NOT NULL DEFAULT '',
			metadata TEXT,
			created_at REAL NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at REAL NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.write.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("memorix/storage: 建表失败: %w", err)
		}
	}

	if enableFTS {
		// paragraphs_fts 采用 external-content 方式（对齐 metadata_fts.py L43-L83）。
		ftsStmts := []string{
			`CREATE VIRTUAL TABLE IF NOT EXISTS paragraphs_fts USING fts5(
				content,
				content='paragraphs',
				content_rowid='rowid',
				tokenize='unicode61'
			)`,
			`CREATE TRIGGER IF NOT EXISTS paragraphs_ai AFTER INSERT ON paragraphs BEGIN
				INSERT INTO paragraphs_fts(rowid, content) VALUES (new.rowid, new.content);
			END`,
			`CREATE TRIGGER IF NOT EXISTS paragraphs_ad AFTER DELETE ON paragraphs BEGIN
				INSERT INTO paragraphs_fts(paragraphs_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
			END`,
			`CREATE TRIGGER IF NOT EXISTS paragraphs_au AFTER UPDATE OF content ON paragraphs BEGIN
				INSERT INTO paragraphs_fts(paragraphs_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
				INSERT INTO paragraphs_fts(rowid, content) VALUES (new.rowid, new.content);
			END`,
			// relations_fts 是独立表（relations 无 content 列），对齐 metadata_fts.py L124-L160。
			`CREATE VIRTUAL TABLE IF NOT EXISTS relations_fts USING fts5(
				relation_hash UNINDEXED,
				content,
				tokenize='unicode61'
			)`,
			// 段落预分词 shadow index：Python 的主 BM25 路径。
			// 中文 FTS5 unicode61 会把整句当作一个 token，因此必须预分词后才能召回。
			// 出处：core/storage/metadata_fts.py L199-L218 与 core/retrieval/sparse_bm25.py L567-L580。
			`CREATE VIRTUAL TABLE IF NOT EXISTS paragraphs_tokenized_fts USING fts5(
				paragraph_hash UNINDEXED,
				tokenized,
				tokenize='unicode61'
			)`,
			`CREATE TRIGGER IF NOT EXISTS relations_ai AFTER INSERT ON relations BEGIN
				INSERT INTO relations_fts(relation_hash, content)
				VALUES (new.hash, COALESCE(new.subject,'') || ' ' || COALESCE(new.predicate,'') || ' ' || COALESCE(new.object,''));
			END`,
			`CREATE TRIGGER IF NOT EXISTS relations_ad AFTER DELETE ON relations BEGIN
				DELETE FROM relations_fts WHERE relation_hash = old.hash;
			END`,
			`CREATE TRIGGER IF NOT EXISTS relations_au AFTER UPDATE ON relations BEGIN
				DELETE FROM relations_fts WHERE relation_hash = old.hash;
				INSERT INTO relations_fts(relation_hash, content)
				VALUES (new.hash, COALESCE(new.subject,'') || ' ' || COALESCE(new.predicate,'') || ' ' || COALESCE(new.object,''));
			END`,
		}
		ftsOK := true
		for _, stmt := range ftsStmts {
			if _, err := s.write.ExecContext(ctx, stmt); err != nil {
				// FTS5 不可用时整体降级（对齐 Python ensure_fts_schema 的容错）。
				ftsOK = false
				break
			}
		}
		if ftsOK {
			if err := s.backfillFTS(ctx); err != nil {
				return err
			}
			if err := s.backfillTokenizedFTS(ctx); err != nil {
				return err
			}
		}
	}

	_, err := s.write.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (?, ?)`,
		SchemaVersion, float64(time.Now().UnixNano())/1e9)
	if err != nil {
		return fmt.Errorf("memorix/storage: 写 schema 版本失败: %w", err)
	}
	return nil
}

// backfillFTS 当 FTS 表与 paragraphs 行数不一致时执行 rebuild。
// 出处：core/storage/metadata_fts.py L91-L113。
func (s *Store) backfillFTS(ctx context.Context) error {
	var paraCount, ftsCount int
	if err := s.write.QueryRowContext(ctx, `SELECT COUNT(1) FROM paragraphs`).Scan(&paraCount); err != nil {
		return fmt.Errorf("memorix/storage: 统计 paragraphs 失败: %w", err)
	}
	if err := s.write.QueryRowContext(ctx, `SELECT COUNT(1) FROM paragraphs_fts`).Scan(&ftsCount); err != nil {
		return nil // FTS 不可用，静默降级
	}
	if paraCount > 0 && ftsCount != paraCount {
		if _, err := s.write.ExecContext(ctx, `INSERT INTO paragraphs_fts(paragraphs_fts) VALUES ('rebuild')`); err != nil {
			return fmt.Errorf("memorix/storage: FTS 回填失败: %w", err)
		}
	}
	return nil
}

// HasFTS 报告 FTS5 虚表是否可用。
func (s *Store) HasFTS(ctx context.Context) bool {
	var name string
	err := s.read.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='paragraphs_fts'`).Scan(&name)
	return err == nil && name == "paragraphs_fts"
}

// ---------------------------------------------------------------------------
// 哈希工具（对齐 core/utils/hash.py）
// ---------------------------------------------------------------------------

// UUID 生成（对齐 Python 用 uuid4 作为 episode/fact 主键）。
func newID() string { return uuid.NewString() }

// quantile 是数值裁剪工具（保留向量归一化的数值稳定）。
func safeNormalize(vec []float32) ([]float32, float64) {
	norm := 0.0
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	norm = math.Sqrt(norm)
	if norm <= 0 {
		return vec, 0
	}
	out := make([]float32, len(vec))
	for i, v := range vec {
		out[i] = float32(float64(v) / norm)
	}
	return out, norm
}

func trimSpaceLower(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
