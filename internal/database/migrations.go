// 建表迁移 — 对齐 参考MaiBot/src/common/database/database_model.py 的表定义。
// 每个 CREATE TABLE 均在注释中标注对应的 Python SQLModel 类。
// 与 Python 版差异：mai_messages.raw_content 由 msgpack BLOB 改为 JSON TEXT（REWRITE_PLAN §9.6）。
package database

import (
	"fmt"
)

// schemaStatements 按依赖顺序执行的建表语句。
var schemaStatements = []string{
	// 对齐 database_model.Messages（表名 mai_messages，含 ix_mai_messages_platform_message_id 索引）。
	`CREATE TABLE IF NOT EXISTS mai_messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT NOT NULL,
		timestamp TEXT NOT NULL,
		platform TEXT NOT NULL DEFAULT '',
		user_id TEXT NOT NULL DEFAULT '',
		user_nickname TEXT NOT NULL DEFAULT '',
		user_cardname TEXT,
		group_id TEXT,
		group_name TEXT,
		is_mentioned INTEGER NOT NULL DEFAULT 0,
		is_at INTEGER NOT NULL DEFAULT 0,
		session_id TEXT NOT NULL DEFAULT '',
		reply_to TEXT,
		is_emoji INTEGER NOT NULL DEFAULT 0,
		is_picture INTEGER NOT NULL DEFAULT 0,
		is_command INTEGER NOT NULL DEFAULT 0,
		is_notify INTEGER NOT NULL DEFAULT 0,
		raw_content TEXT NOT NULL DEFAULT '{}',
		processed_plain_text TEXT,
		additional_config TEXT,
		reply_frequency REAL
	)`,
	`CREATE INDEX IF NOT EXISTS ix_mai_messages_message_id ON mai_messages(message_id)`,
	`CREATE INDEX IF NOT EXISTS ix_mai_messages_platform_message_id ON mai_messages(platform, message_id)`,
	`CREATE INDEX IF NOT EXISTS ix_mai_messages_user_id ON mai_messages(user_id)`,
	`CREATE INDEX IF NOT EXISTS ix_mai_messages_group_id ON mai_messages(group_id)`,
	`CREATE INDEX IF NOT EXISTS ix_mai_messages_session_id ON mai_messages(session_id)`,
	`CREATE INDEX IF NOT EXISTS ix_mai_messages_timestamp ON mai_messages(timestamp)`,

	// 对齐 database_model.ModelUsage（表名 llm_usage，含 prompt_cache_* 字段）。
	`CREATE TABLE IF NOT EXISTS llm_usage (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		model_name TEXT NOT NULL,
		model_assign_name TEXT,
		model_api_provider_name TEXT NOT NULL DEFAULT '',
		session_id TEXT NOT NULL DEFAULT '',
		task_name TEXT,
		request_type TEXT NOT NULL DEFAULT '',
		time_cost REAL NOT NULL DEFAULT 0,
		timestamp TEXT NOT NULL,
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		prompt_cache_enabled INTEGER NOT NULL DEFAULT 0,
		prompt_cache_hit_tokens INTEGER NOT NULL DEFAULT 0,
		prompt_cache_miss_tokens INTEGER NOT NULL DEFAULT 0,
		cost REAL NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS ix_llm_usage_model_name ON llm_usage(model_name)`,
	`CREATE INDEX IF NOT EXISTS ix_llm_usage_model_assign_name ON llm_usage(model_assign_name)`,
	`CREATE INDEX IF NOT EXISTS ix_llm_usage_model_api_provider_name ON llm_usage(model_api_provider_name)`,
	`CREATE INDEX IF NOT EXISTS ix_llm_usage_session_id ON llm_usage(session_id)`,
	`CREATE INDEX IF NOT EXISTS ix_llm_usage_task_name ON llm_usage(task_name)`,
	`CREATE INDEX IF NOT EXISTS ix_llm_usage_timestamp ON llm_usage(timestamp)`,

	// 对齐 database_model.Images（image_hash=sha256，emoji|image 共表）。
	`CREATE TABLE IF NOT EXISTS images (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		image_hash TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		full_path TEXT NOT NULL DEFAULT '',
		image_type TEXT NOT NULL DEFAULT 'emoji',
		query_count INTEGER NOT NULL DEFAULT 0,
		is_registered INTEGER NOT NULL DEFAULT 0,
		is_banned INTEGER NOT NULL DEFAULT 0,
		no_file_flag INTEGER NOT NULL DEFAULT 0,
		record_time TEXT NOT NULL,
		register_time TEXT,
		last_used_time TEXT,
		vlm_processed INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS ix_images_image_hash ON images(image_hash)`,
	`CREATE INDEX IF NOT EXISTS ix_images_record_time ON images(record_time)`,

	// 对齐 database_model.ToolRecord。
	`CREATE TABLE IF NOT EXISTS tool_records (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		tool_id TEXT NOT NULL,
		timestamp TEXT NOT NULL,
		session_id TEXT NOT NULL DEFAULT '',
		tool_name TEXT NOT NULL,
		tool_reasoning TEXT,
		tool_data TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS ix_tool_records_tool_id ON tool_records(tool_id)`,
	`CREATE INDEX IF NOT EXISTS ix_tool_records_timestamp ON tool_records(timestamp)`,
	`CREATE INDEX IF NOT EXISTS ix_tool_records_session_id ON tool_records(session_id)`,
	`CREATE INDEX IF NOT EXISTS ix_tool_records_tool_name ON tool_records(tool_name)`,

	// 对齐 database_model.Expression。
	`CREATE TABLE IF NOT EXISTS expressions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		situation TEXT NOT NULL,
		style TEXT NOT NULL,
		content_list TEXT NOT NULL DEFAULT '[]',
		count INTEGER NOT NULL DEFAULT 0,
		last_active_time TEXT NOT NULL,
		create_time TEXT NOT NULL,
		session_id TEXT,
		checked INTEGER NOT NULL DEFAULT 0,
		modified_by TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS ix_expressions_situation ON expressions(situation)`,
	`CREATE INDEX IF NOT EXISTS ix_expressions_style ON expressions(style)`,
	`CREATE INDEX IF NOT EXISTS ix_expressions_last_active_time ON expressions(last_active_time)`,

	// 对齐 database_model.Jargon。
	`CREATE TABLE IF NOT EXISTS jargons (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		content TEXT NOT NULL,
		evidence_messages TEXT,
		meaning TEXT NOT NULL,
		session_id_dict TEXT NOT NULL DEFAULT '{}',
		count INTEGER NOT NULL DEFAULT 0,
		is_jargon INTEGER DEFAULT 0,
		is_complete INTEGER NOT NULL DEFAULT 0,
		is_global INTEGER NOT NULL DEFAULT 0,
		last_inference_count INTEGER NOT NULL DEFAULT 0,
		created_by TEXT NOT NULL DEFAULT 'AI',
		created_timestamp TEXT NOT NULL,
		updated_timestamp TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS ix_jargons_content ON jargons(content)`,
	`CREATE INDEX IF NOT EXISTS ix_jargons_created_timestamp ON jargons(created_timestamp)`,
	`CREATE INDEX IF NOT EXISTS ix_jargons_updated_timestamp ON jargons(updated_timestamp)`,

	// 对齐 database_model.PersonInfo。
	`CREATE TABLE IF NOT EXISTS person_info (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		is_known INTEGER NOT NULL DEFAULT 0,
		person_id TEXT NOT NULL UNIQUE,
		person_name TEXT,
		name_reason TEXT,
		platform TEXT NOT NULL DEFAULT '',
		user_id TEXT NOT NULL DEFAULT '',
		user_nickname TEXT NOT NULL DEFAULT '',
		group_cardname TEXT,
		memory_points TEXT,
		know_counts INTEGER NOT NULL DEFAULT 0,
		first_known_time TEXT,
		last_known_time TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS ix_person_info_person_id ON person_info(person_id)`,
	`CREATE INDEX IF NOT EXISTS ix_person_info_platform ON person_info(platform)`,
	`CREATE INDEX IF NOT EXISTS ix_person_info_user_id ON person_info(user_id)`,

	// 对齐 database_model.ChatSession（表名 chat_sessions）。
	`CREATE TABLE IF NOT EXISTS chat_sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL UNIQUE,
		created_timestamp TEXT NOT NULL,
		last_active_timestamp TEXT NOT NULL,
		user_id TEXT,
		user_nickname TEXT,
		user_cardname TEXT,
		group_id TEXT,
		group_name TEXT,
		platform TEXT NOT NULL DEFAULT '',
		account_id TEXT,
		scope TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS ix_chat_sessions_session_id ON chat_sessions(session_id)`,
	`CREATE INDEX IF NOT EXISTS ix_chat_sessions_user_id ON chat_sessions(user_id)`,
	`CREATE INDEX IF NOT EXISTS ix_chat_sessions_group_id ON chat_sessions(group_id)`,
	`CREATE INDEX IF NOT EXISTS ix_chat_sessions_platform ON chat_sessions(platform)`,
	`CREATE INDEX IF NOT EXISTS ix_chat_sessions_account_id ON chat_sessions(account_id)`,
	`CREATE INDEX IF NOT EXISTS ix_chat_sessions_scope ON chat_sessions(scope)`,
	`CREATE INDEX IF NOT EXISTS ix_chat_sessions_created_timestamp ON chat_sessions(created_timestamp)`,
	`CREATE INDEX IF NOT EXISTS ix_chat_sessions_last_active_timestamp ON chat_sessions(last_active_timestamp)`,

	// 对齐 database_model.MaisakaReplyEffect（表名 maisaka_reply_effects，P11 回复效果观察）。
	// 与 Python 版差异：record_json 存完整 JSON 文本（Python 版再 gzip 一份到 record_blob）；
	// 新增 created_ms / finalized_ms 两个 epoch 毫秒列，供跨时区稳定排序。
	`CREATE TABLE IF NOT EXISTS maisaka_reply_effects (
		effect_id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		session_name TEXT NOT NULL DEFAULT '',
		chat_type TEXT NOT NULL DEFAULT 'group',
		status TEXT NOT NULL,
		created_at TEXT NOT NULL,
		created_ms INTEGER,
		finalized_at TEXT,
		finalized_ms INTEGER,
		strategy_primary TEXT NOT NULL DEFAULT 'other',
		model_name TEXT NOT NULL DEFAULT '',
		request_fingerprint TEXT NOT NULL DEFAULT '',
		prompt_fingerprint TEXT NOT NULL DEFAULT '',
		scorer_version INTEGER NOT NULL DEFAULT 2,
		response_score REAL,
		reception_score REAL,
		conversation_score REAL,
		raw_score REAL,
		relative_score REAL,
		confidence REAL NOT NULL DEFAULT 0,
		record_json TEXT NOT NULL DEFAULT '{}',
		record_blob BLOB
	)`,
	`CREATE INDEX IF NOT EXISTS ix_reply_effect_session_id ON maisaka_reply_effects(session_id)`,
	`CREATE INDEX IF NOT EXISTS ix_reply_effect_status ON maisaka_reply_effects(status)`,
	`CREATE INDEX IF NOT EXISTS ix_reply_effect_created_at ON maisaka_reply_effects(created_at)`,
	`CREATE INDEX IF NOT EXISTS ix_reply_effect_session_finalized ON maisaka_reply_effects(session_id, finalized_at)`,
	`CREATE INDEX IF NOT EXISTS ix_reply_effect_strategy_finalized ON maisaka_reply_effects(strategy_primary, finalized_at)`,
	`CREATE INDEX IF NOT EXISTS ix_reply_effect_model_prompt ON maisaka_reply_effects(model_name, prompt_fingerprint)`,
	`CREATE INDEX IF NOT EXISTS ix_reply_effect_request_fingerprint ON maisaka_reply_effects(request_fingerprint)`,
}

// Migrate 执行建表（幂等，对齐 SQLModel.metadata.create_all 的兜底建表语义）。
func (db *DB) Migrate() error {
	for _, stmt := range schemaStatements {
		if _, err := db.write.Exec(stmt); err != nil {
			return fmt.Errorf("执行建表语句失败: %w\n语句: %s", err, stmt)
		}
	}
	return nil
}
