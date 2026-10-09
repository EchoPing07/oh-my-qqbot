// Package database 实现 SQLite 存储层：
// 对齐 参考MaiBot/src/common/database/database.py（连接/初始化）与
// database_model.py（表结构）。SQLite 引擎为 modernc.org/sqlite（纯 Go，无 CGO），
// 连接形态对齐 Diana 的单写多读模式：单写连接 + 读写连接池，WAL 模式。
package database

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// DB 封装 SQLite 读写连接。
type DB struct {
	write *sql.DB // 单写连接（串行化写入，避免 SQLITE_BUSY）
	read  *sql.DB // 读连接池（WAL 下读写并行）
}

// openOptions 连接参数。
type openOptions struct {
	path            string
	maxReadConns    int
	busyTimeoutMS   int
	foreignKeys     bool
}

func defaultOpenOptions(path string) openOptions {
	return openOptions{
		path:          path,
		maxReadConns:  8,
		busyTimeoutMS: 5000,
		foreignKeys:   true,
	}
}

// dsn 组装 modernc.org/sqlite DSN：WAL + busy_timeout + foreign_keys。
// （对齐 database.py create_engine 的 pragmas 与 Diana sqlite_store 的连接参数。）
func (o openOptions) dsn() string {
	return fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)&_pragma=foreign_keys(%d)&_pragma=synchronous(NORMAL)",
		filepath.ToSlash(o.path), o.busyTimeoutMS, boolToInt(o.foreignKeys))
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Open 打开数据库并初始化结构（对齐 database.initialize_database）。
func Open(dbPath string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据库目录失败: %w", err)
	}
	opts := defaultOpenOptions(dbPath)

	write, err := sql.Open("sqlite", opts.dsn())
	if err != nil {
		return nil, fmt.Errorf("打开写连接失败: %w", err)
	}
	// 单写：写连接只允许 1 个连接，杜绝写锁竞争。
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0)

	read, err := sql.Open("sqlite", opts.dsn())
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("打开读连接失败: %w", err)
	}
	read.SetMaxOpenConns(opts.maxReadConns)
	read.SetMaxIdleConns(opts.maxReadConns)
	read.SetConnMaxLifetime(0)

	db := &DB{write: write, read: read}
	if err := db.verify(); err != nil {
		db.Close()
		return nil, err
	}
	if err := db.Migrate(); err != nil {
		db.Close()
		return nil, err
	}
	slog.Info("数据库初始化完成", "path", dbPath)
	return db, nil
}

// verify 检查连接可用（对齐 initialize_database 的建表前置检查）。
func (db *DB) verify() error {
	var one int
	if err := db.write.QueryRow("SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("写连接自检失败: %w", err)
	}
	if err := db.read.QueryRow("SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("读连接自检失败: %w", err)
	}
	return nil
}

// Write 暴露单写连接（调用方需自行控制事务边界）。
func (db *DB) Write() *sql.DB { return db.write }

// Read 暴露读连接池。
func (db *DB) Read() *sql.DB { return db.read }

// Close 关闭连接。
func (db *DB) Close() error {
	errW := db.write.Close()
	errR := db.read.Close()
	if errW != nil {
		return errW
	}
	return errR
}

// WithWriteTx 在写连接上执行一个事务（对齐 get_db_session(auto_commit=True)）。
func (db *DB) WithWriteTx(fn func(tx *sql.Tx) error) error {
	tx, err := db.write.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// WalCheckpointPassive 执行 PASSIVE checkpoint（供关停路径调用，截断 WAL）。
func (db *DB) WalCheckpointPassive() error {
	_, err := db.write.Exec("PRAGMA wal_checkpoint(PASSIVE);")
	return err
}

// timeLayout 是 SQLite DATETIME 的存储格式（对齐 SQLModel DateTime 列语义）。
const timeLayout = "2006-01-02 15:04:05.999999"

// formatTime / parseTime 统一 DATETIME 编解码。
func formatTime(t time.Time) string { return t.Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{timeLayout, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errParseTime(s)
}

type timeParseError string

func (e timeParseError) Error() string { return "无法解析时间: " + string(e) }

func errParseTime(s string) error { return timeParseError(s) }

// nullString 把可能为空的字符串转为 sql 可用值。
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// scanNullString 读取可空字符串列。
func scanNullString(row interface{ Scan(...any) error }, dst *string) error {
	var s sql.NullString
	if err := row.Scan(&s); err != nil {
		return err
	}
	*dst = s.String
	return nil
}

// globalOpen 防止同一路径重复打开（测试守护）。
var (
	globalOpenMu   sync.Mutex
	globalOpenPaths = map[string]bool{}
)

// OpenOnce 进程内同路径只打开一次的便捷封装（用于 main 装配）。
func OpenOnce(dbPath string) (*DB, error) {
	globalOpenMu.Lock()
	defer globalOpenMu.Unlock()
	key := filepath.Clean(dbPath)
	if globalOpenPaths[key] {
		return nil, fmt.Errorf("数据库 %s 已被打开", dbPath)
	}
	db, err := Open(dbPath)
	if err != nil {
		return nil, err
	}
	globalOpenPaths[key] = true
	return db, nil
}
