// Package transfer 实现全量业务数据打包导出/导入（M15 数据迁移）。
//
//	对齐语义：参考/MaiBot/src/webui/routers/data_transfer.py
//	 （format="maibot-data-archive" 的清单 + 分片导出导入；Python 版打包 config/data/
//	    plugins/logs 目录，Go 版把「业务数据」收敛为 JSON 记录集）。
//
// 与 internal/webui/routers/data_transfer.go 的分工：
//   - internal/webui 的 data-transfer 路由只导出 **配置**（bot_config / model_config）；
//   - 本包导出 **全量业务数据**（消息/人物/表达/黑话/图片描述），由装配层接进
//     WebUI 的 /data-transfer/export、/data-transfer/import（任务书 P11：本包独立，
//     internal/webui 不改动）。
//
// 数据来源经 Store 接口注入（internal/database 适配在 database.go），
// 因此本包不依赖 internal/webui、internal/config、internal/llm、internal/qq。
package transfer

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 归档格式标识与版本（对齐 data_transfer.py 的 format / format_version）。
const (
	// Format mirrors "maibot-data-archive"。
	Format = "maibot-data-archive"
	// FormatVersion mirrors format_version=1。
	FormatVersion = 1
)

// 数据分片名（导出/导入范围）。
const (
	// SectionMessages 是消息分片（mai_messages）。
	SectionMessages = "messages"
	// SectionPersons 是人物分片（person_info）。
	SectionPersons = "persons"
	// SectionExpressions 是表达分片（expressions）。
	SectionExpressions = "expressions"
	// SectionJargons 是黑话分片（jargons）。
	SectionJargons = "jargons"
	// SectionImages 是图片描述分片（images 的元数据与描述，不含图片二进制）。
	SectionImages = "images"
)

// AllSections 是全部可导出分片（顺序即默认导出顺序）。
var AllSections = []string{
	SectionMessages, SectionPersons, SectionExpressions, SectionJargons, SectionImages,
}

// SectionLabel 返回分片的中文名（错误/日志文案用）。
func SectionLabel(section string) string {
	switch section {
	case SectionMessages:
		return "消息"
	case SectionPersons:
		return "人物"
	case SectionExpressions:
		return "表达"
	case SectionJargons:
		return "黑话"
	case SectionImages:
		return "图片描述"
	default:
		return section
	}
}

// validSection 判断分片名是否受支持。
func validSection(section string) bool {
	for _, known := range AllSections {
		if known == section {
			return true
		}
	}
	return false
}

// MessageRecord 是 mai_messages 的一行（raw_content 为 JSON 文本）。
type MessageRecord struct {
	MessageID          string   `json:"message_id"`
	Timestamp          string   `json:"timestamp"`
	Platform           string   `json:"platform"`
	UserID             string   `json:"user_id"`
	UserNickname       string   `json:"user_nickname"`
	UserCardname       *string  `json:"user_cardname,omitempty"`
	GroupID            *string  `json:"group_id,omitempty"`
	GroupName          *string  `json:"group_name,omitempty"`
	IsMentioned        bool     `json:"is_mentioned"`
	IsAt               bool     `json:"is_at"`
	SessionID          string   `json:"session_id"`
	ReplyTo            *string  `json:"reply_to,omitempty"`
	IsEmoji            bool     `json:"is_emoji"`
	IsPicture          bool     `json:"is_picture"`
	IsCommand          bool     `json:"is_command"`
	IsNotify           bool     `json:"is_notify"`
	RawContent         string   `json:"raw_content"`
	ProcessedPlainText *string  `json:"processed_plain_text,omitempty"`
	AdditionalConfig   *string  `json:"additional_config,omitempty"`
	ReplyFrequency     *float64 `json:"reply_frequency,omitempty"`
}

// PersonRecord 是 person_info 的一行。
type PersonRecord struct {
	IsKnown       bool    `json:"is_known"`
	PersonID      string  `json:"person_id"`
	PersonName    *string `json:"person_name,omitempty"`
	NameReason    *string `json:"name_reason,omitempty"`
	Platform      string  `json:"platform"`
	UserID        string  `json:"user_id"`
	UserNickname  string  `json:"user_nickname"`
	GroupCardname *string `json:"group_cardname,omitempty"`
	MemoryPoints  *string `json:"memory_points,omitempty"`
	KnowCounts    int     `json:"know_counts"`
	FirstKnownAt  *string `json:"first_known_time,omitempty"`
	LastKnownAt   *string `json:"last_known_time,omitempty"`
}

// ExpressionRecord 是 expressions 的一行（content_list 为 JSON 数组文本）。
type ExpressionRecord struct {
	Situation      string  `json:"situation"`
	Style          string  `json:"style"`
	ContentList    string  `json:"content_list"`
	Count          int     `json:"count"`
	LastActiveTime string  `json:"last_active_time"`
	CreateTime     string  `json:"create_time"`
	SessionID      *string `json:"session_id,omitempty"`
	Checked        bool    `json:"checked"`
	ModifiedBy     *string `json:"modified_by,omitempty"`
}

// JargonRecord 是 jargons 的一行。
type JargonRecord struct {
	Content            string  `json:"content"`
	EvidenceMessages   *string `json:"evidence_messages,omitempty"`
	Meaning            string  `json:"meaning"`
	SessionIDDict      string  `json:"session_id_dict"`
	Count              int     `json:"count"`
	IsJargon           *bool   `json:"is_jargon,omitempty"`
	IsComplete         bool    `json:"is_complete"`
	IsGlobal           bool    `json:"is_global"`
	LastInferenceCount int     `json:"last_inference_count"`
	CreatedBy          string  `json:"created_by"`
	CreatedTimestamp   string  `json:"created_timestamp"`
	UpdatedTimestamp   string  `json:"updated_timestamp"`
}

// ImageRecord 是 images 的一行描述（不含二进制；full_path 供导入方自行定位文件）。
type ImageRecord struct {
	ImageHash    string  `json:"image_hash"`
	Description  string  `json:"description"`
	FullPath     string  `json:"full_path"`
	ImageType    string  `json:"image_type"`
	QueryCount   int     `json:"query_count"`
	IsRegistered bool    `json:"is_registered"`
	IsBanned     bool    `json:"is_banned"`
	NoFileFlag   bool    `json:"no_file_flag"`
	RecordTime   string  `json:"record_time"`
	RegisterTime *string `json:"register_time,omitempty"`
	LastUsedTime *string `json:"last_used_time,omitempty"`
	VLMProcessed bool    `json:"vlm_processed"`
}

// Payload 是打包后的数据体（字段名对齐 data_transfer 的分片统计结构）。
type Payload struct {
	Messages    []MessageRecord    `json:"messages,omitempty"`
	Persons     []PersonRecord     `json:"persons,omitempty"`
	Expressions []ExpressionRecord `json:"expressions,omitempty"`
	Jargons     []JargonRecord     `json:"jargons,omitempty"`
	Images      []ImageRecord      `json:"images,omitempty"`
}

// SectionCount 返回某个分片的记录数。
func (p *Payload) SectionCount(section string) int {
	switch section {
	case SectionMessages:
		return len(p.Messages)
	case SectionPersons:
		return len(p.Persons)
	case SectionExpressions:
		return len(p.Expressions)
	case SectionJargons:
		return len(p.Jargons)
	case SectionImages:
		return len(p.Images)
	default:
		return 0
	}
}

// Counts 返回全部含数据的分片计数（导出清单用）。
func (p *Payload) Counts() map[string]int {
	counts := map[string]int{}
	for _, section := range AllSections {
		if count := p.SectionCount(section); count > 0 {
			counts[section] = count
		}
	}
	return counts
}

// Bundle 是导出文件根结构（对齐 data_transfer.py 的 manifest + 数据体）。
type Bundle struct {
	// Format = "maibot-data-archive"。
	Format string `json:"format"`
	// FormatVersion = 1。
	FormatVersion int `json:"format_version"`
	// ExportedAt mirrors manifest.created_at（RFC3339 UTC）。
	ExportedAt string `json:"exported_at"`
	// Version 是本程序版本号（主程序注入，可空）。
	Version string `json:"version,omitempty"`
	// Included 是本包实际包含的分片列表（顺序与 AllSections 一致）。
	Included []string `json:"included"`
	// Counts 是各分片记录数（对齐 manifest.parts 的 file_count）。
	Counts map[string]int `json:"counts"`
	// Data 是记录集。
	Data Payload `json:"data"`
}

// ---------------------------------------------------------------------------
// Store 边界
// ---------------------------------------------------------------------------

// Store 是导出/导入的数据边界（由 internal/database 适配，见 database.go）。
type Store interface {
	// ExportMessages 导出消息（limit<=0 表示不限）。
	ExportMessages(ctx context.Context, limit int) ([]MessageRecord, error)
	// ImportMessages 导入消息，返回新增/跳过数量。
	ImportMessages(ctx context.Context, records []MessageRecord, overwrite bool) (ImportStat, error)

	// ExportPersons 导出人物。
	ExportPersons(ctx context.Context, limit int) ([]PersonRecord, error)
	// ImportPersons 导入人物。
	ImportPersons(ctx context.Context, records []PersonRecord, overwrite bool) (ImportStat, error)

	// ExportExpressions 导出表达。
	ExportExpressions(ctx context.Context, limit int) ([]ExpressionRecord, error)
	// ImportExpressions 导入表达。
	ImportExpressions(ctx context.Context, records []ExpressionRecord, overwrite bool) (ImportStat, error)

	// ExportJargons 导出黑话。
	ExportJargons(ctx context.Context, limit int) ([]JargonRecord, error)
	// ImportJargons 导入黑话。
	ImportJargons(ctx context.Context, records []JargonRecord, overwrite bool) (ImportStat, error)

	// ExportImages 导出图片描述。
	ExportImages(ctx context.Context, limit int) ([]ImageRecord, error)
	// ImportImages 导入图片描述。
	ImportImages(ctx context.Context, records []ImageRecord, overwrite bool) (ImportStat, error)
}

// ImportStat 是单个分片的导入统计。
type ImportStat struct {
	// Imported 是新增记录数。
	Imported int `json:"imported"`
	// Updated 是被覆盖更新的记录数（overwrite=true 时可能出现）。
	Updated int `json:"updated"`
	// Skipped 是已存在且未覆盖的记录数。
	Skipped int `json:"skipped"`
}

// Total 返回该分片处理的总条数。
func (s ImportStat) Total() int { return s.Imported + s.Updated + s.Skipped }

// ---------------------------------------------------------------------------
// 选项
// ---------------------------------------------------------------------------

// ExportOptions 是导出参数。
type ExportOptions struct {
	// Sections 是要导出的分片；空表示全部（AllSections）。
	Sections []string
	// LimitPerSection 是每个分片的最大条数（<=0 表示不限）。
	LimitPerSection int
	// Version 写入 bundle.Version。
	Version string
	// Now 是导出时间（nil 时用 time.Now）。
	Now func() time.Time
}

// ImportOptions 是导入参数。
type ImportOptions struct {
	// Sections 是要导入的分片；空表示按 bundle.Included 全导入。
	Sections []string
	// Overwrite 为 true 时覆盖同名记录（默认跳过冲突）。
	Overwrite bool
	// SkipInvalidRecords 为 true 时忽略结构非法的记录（默认报错并中止）。
	SkipInvalidRecords bool
}

// ImportResult 是全量导入结果。
type ImportResult struct {
	// Sections 是各分片的统计。
	Sections map[string]ImportStat `json:"sections"`
	// Total 是全部新增记录数。
	Total int `json:"total"`
	// Updated 是全部覆盖数。
	Updated int `json:"updated"`
	// Skipped 是全部跳过数。
	Skipped int `json:"skipped"`
}

// ---------------------------------------------------------------------------
// 导出
// ---------------------------------------------------------------------------

// Export 采集各分片数据并组装 Bundle（对齐 _run_export_job 的清单+写入语义）。
func Export(ctx context.Context, store Store, opts ExportOptions) (*Bundle, error) {
	if store == nil {
		return nil, fmt.Errorf("导出需要注入数据存储")
	}
	sections, err := normalizeSections(opts.Sections)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	bundle := &Bundle{
		Format:        Format,
		FormatVersion: FormatVersion,
		ExportedAt:    now().UTC().Format(time.RFC3339),
		Version:       opts.Version,
		Included:      append([]string(nil), sections...),
		Counts:        map[string]int{},
	}
	for _, section := range sections {
		switch section {
		case SectionMessages:
			records, err := store.ExportMessages(ctx, opts.LimitPerSection)
			if err != nil {
				return nil, fmt.Errorf("导出%s失败: %w", SectionLabel(section), err)
			}
			bundle.Data.Messages = ensureMessages(records)
		case SectionPersons:
			records, err := store.ExportPersons(ctx, opts.LimitPerSection)
			if err != nil {
				return nil, fmt.Errorf("导出%s失败: %w", SectionLabel(section), err)
			}
			bundle.Data.Persons = ensurePersons(records)
		case SectionExpressions:
			records, err := store.ExportExpressions(ctx, opts.LimitPerSection)
			if err != nil {
				return nil, fmt.Errorf("导出%s失败: %w", SectionLabel(section), err)
			}
			bundle.Data.Expressions = ensureExpressions(records)
		case SectionJargons:
			records, err := store.ExportJargons(ctx, opts.LimitPerSection)
			if err != nil {
				return nil, fmt.Errorf("导出%s失败: %w", SectionLabel(section), err)
			}
			bundle.Data.Jargons = ensureJargons(records)
		case SectionImages:
			records, err := store.ExportImages(ctx, opts.LimitPerSection)
			if err != nil {
				return nil, fmt.Errorf("导出%s失败: %w", SectionLabel(section), err)
			}
			bundle.Data.Images = ensureImages(records)
		}
		if count := bundle.Data.SectionCount(section); count > 0 {
			bundle.Counts[section] = count
		} else {
			bundle.Counts[section] = 0
		}
	}
	return bundle, nil
}

// MarshalJSONBytes 序列化 Bundle（ensure_ascii=False 语义：直接输出 UTF-8）。
func MarshalJSONBytes(bundle *Bundle) ([]byte, error) {
	if bundle == nil {
		return nil, fmt.Errorf("导出包为空")
	}
	encoded, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化导出包失败: %w", err)
	}
	return encoded, nil
}

// ---------------------------------------------------------------------------
// 导入
// ---------------------------------------------------------------------------

// ParseBundle 解析并校验导出包（对齐 _load_archive_manifest 的格式校验）。
func ParseBundle(payload []byte) (*Bundle, error) {
	var bundle Bundle
	if err := json.Unmarshal(payload, &bundle); err != nil {
		return nil, fmt.Errorf("导出包不是合法 JSON: %w", err)
	}
	if bundle.Format != Format || bundle.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("不支持的导出包格式（format=%q version=%d）", bundle.Format, bundle.FormatVersion)
	}
	// 声明的计数与数据体必须一致（对齐 record_count 校验）。
	for section, count := range bundle.Counts {
		actual := bundle.Data.SectionCount(section)
		if actual != count {
			return nil, fmt.Errorf("导出包 %s 分片记录数量校验失败：声明 %d 条，实际 %d 条",
				SectionLabel(section), count, actual)
		}
	}
	return &bundle, nil
}

// Import 把 Bundle 写入 Store（对齐 _run_import_job 选择分片导入的语义）。
func Import(ctx context.Context, store Store, bundle *Bundle, opts ImportOptions) (*ImportResult, error) {
	if store == nil {
		return nil, fmt.Errorf("导入需要注入数据存储")
	}
	if bundle == nil {
		return nil, fmt.Errorf("导入包为空")
	}
	if bundle.Format != Format || bundle.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("不支持的导出包格式（format=%q version=%d）", bundle.Format, bundle.FormatVersion)
	}
	sections := opts.Sections
	if len(sections) == 0 {
		sections = bundle.Included
	}
	if len(sections) == 0 {
		sections = AllSections
	}
	normalized, err := normalizeSections(sections)
	if err != nil {
		return nil, err
	}
	support := map[string]bool{}
	for _, section := range bundle.Included {
		support[section] = true
	}
	result := &ImportResult{Sections: map[string]ImportStat{}}
	for _, section := range normalized {
		if len(bundle.Included) > 0 && !support[section] {
			// 导出包不含该分片：跳过而不是写零值（对齐 enabled_parts 过滤语义）。
			continue
		}
		var stat ImportStat
		switch section {
		case SectionMessages:
			stat, err = store.ImportMessages(ctx, ensureMessages(bundle.Data.Messages), opts.Overwrite)
		case SectionPersons:
			stat, err = store.ImportPersons(ctx, ensurePersons(bundle.Data.Persons), opts.Overwrite)
		case SectionExpressions:
			stat, err = store.ImportExpressions(ctx, ensureExpressions(bundle.Data.Expressions), opts.Overwrite)
		case SectionJargons:
			stat, err = store.ImportJargons(ctx, ensureJargons(bundle.Data.Jargons), opts.Overwrite)
		case SectionImages:
			stat, err = store.ImportImages(ctx, ensureImages(bundle.Data.Images), opts.Overwrite)
		}
		if err != nil {
			return nil, fmt.Errorf("导入%s失败: %w", SectionLabel(section), err)
		}
		result.Sections[section] = stat
		result.Total += stat.Imported
		result.Updated += stat.Updated
		result.Skipped += stat.Skipped
	}
	return result, nil
}

// normalizeSections 校验并去重分片列表（保持 AllSections 的稳定顺序）。
func normalizeSections(sections []string) ([]string, error) {
	if len(sections) == 0 {
		return append([]string(nil), AllSections...), nil
	}
	selected := map[string]bool{}
	for _, section := range sections {
		normalized := strings.TrimSpace(section)
		if !validSection(normalized) {
			return nil, fmt.Errorf("不支持的数据分片: %s", section)
		}
		selected[normalized] = true
	}
	out := make([]string, 0, len(selected))
	for _, section := range AllSections {
		if selected[section] {
			out = append(out, section)
		}
	}
	return out, nil
}

// 空切片归一为 nil 以便省略 JSON 字段，同时保证往返后 len 语义一致。
func ensureMessages(records []MessageRecord) []MessageRecord {
	if len(records) == 0 {
		return nil
	}
	return records
}

func ensurePersons(records []PersonRecord) []PersonRecord {
	if len(records) == 0 {
		return nil
	}
	return records
}

func ensureExpressions(records []ExpressionRecord) []ExpressionRecord {
	if len(records) == 0 {
		return nil
	}
	return records
}

func ensureJargons(records []JargonRecord) []JargonRecord {
	if len(records) == 0 {
		return nil
	}
	return records
}

func ensureImages(records []ImageRecord) []ImageRecord {
	if len(records) == 0 {
		return nil
	}
	return records
}

// SortSections 按固定顺序排序（供装配层/测试断言使用）。
func SortSections(sections []string) []string {
	out := append([]string(nil), sections...)
	order := map[string]int{}
	for index, section := range AllSections {
		order[section] = index
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}
