// Package emoji 实现表情包系统（M9）：注册/VLM 描述/情绪标签/偷表情/
// 按情绪选择/拼图选择。对齐 参考MaiBot/src/emoji_system/（行为以该目录为权威）：
//
//   - emoji_manager.py：EmojiManager（注册、VLM 描述+情绪标签、偷表情、
//     按情绪相似度 Top10 随机选、LLM 替换决策、完整性检查）
//   - maisaka_tool.py：send_emoji_for_maisaka（出站发送路径）
//   - builtin_tool/send_emoji.py / show_emoji_list.py：拼图合成与子代理选择
//
// 依赖全部接口注入：不 import internal/qq、internal/config；LLM/VLM 用包内
// 最小接口（VLMClient / TextLLM），由装配层适配 internal/llm。
package emoji

import (
	"context"
	"time"
)

// ---------------------------------------------------------------------------
// LLM/VLM 最小接口（包内定义，装配层适配 internal/llm）
// ---------------------------------------------------------------------------

// VLMRequest 是一次视觉问答请求（图片字节 + 提示词）。
type VLMRequest struct {
	// Prompt 是渲染后的提示词。
	Prompt string
	// ImageBytes 是图片原始字节。
	ImageBytes []byte
	// ImageFormat 是图片格式（png/jpeg/gif 转静态图后为 jpg）。
	ImageFormat string
	// SessionID 是会话 id（监控/记账用）。
	SessionID string
}

// VLMClient 是表情包系统需要的最小视觉模型边界。
type VLMClient interface {
	// GenerateResponseForImage 对齐 LLMServiceClient.generate_response_for_image。
	GenerateResponseForImage(ctx context.Context, req VLMRequest) (string, error)
	// Configured 对齐 _is_vlm_task_configured：vlm 任务是否配置了模型。
	Configured() bool
}

// TextLLM 是替换决策等纯文字 LLM 调用的最小边界。
type TextLLM interface {
	// GenerateResponse 对齐 LLMServiceClient.generate_response。
	GenerateResponse(ctx context.Context, prompt string, sessionID string) (string, error)
	// EmojiTaskConfigured 对齐「emoji 任务是否配置了模型」（替换决策优先用
	// emoji 任务，未配置时用 planner）。
	EmojiTaskConfigured() bool
}

// PromptRenderer 是 emoji 相关 .prompt 模板的最小渲染边界
// （对齐 prompt_manager.get_prompt + add_context + render_prompt）。
type PromptRenderer interface {
	// Render 渲染指定 prompt 模板，ctx 为 {var} 占位符取值。
	Render(ctx context.Context, promptName string, vars map[string]string) (string, error)
}

// ---------------------------------------------------------------------------
// 表情包对象 — 对齐 MaiEmoji
// ---------------------------------------------------------------------------

// Emoji 对齐 MaiEmoji（src/common/data_models/image_data_model.py）。
type Emoji struct {
	// FileHash 是 sha256（唯一 ID）。
	FileHash string
	// FullPath 是绝对路径。
	FullPath string
	// FileName 是文件名。
	FileName string
	// ImageFormat 是实际格式（png/jpeg/gif/...）。
	ImageFormat string
	// Description 是 VLM 描述（逗号分隔的情绪标签文本）。
	Description string
	// Emotions 是规范化后的情绪标签列表。
	Emotions []string
	// QueryCount 是使用次数。
	QueryCount int
	// RegisterTime / LastUsedTime 是注册与最近使用时间。
	RegisterTime *time.Time
	LastUsedTime *time.Time
}

// StorePathResolver 对齐 src/common/utils/image_path.py 的路径解析/序列化
// （数据库存相对项目根目录的 posix 路径）。
type StorePathResolver interface {
	// Resolve 把库中存储路径解析为绝对路径。
	Resolve(storedPath string) (string, error)
	// Serialize 把绝对路径序列化为项目内相对 posix 路径。
	Serialize(absolutePath string) (string, error)
}

// RegisterStatus 对齐 EmojiRegisterStatus Literal。
type RegisterStatus string

// 注册结果（照抄 Python 字面量）。
const (
	RegisterStatusRegistered RegisterStatus = "registered"
	RegisterStatusSkipped    RegisterStatus = "skipped"
	RegisterStatusFailed     RegisterStatus = "failed"
)

// EmojiStore 是表情包持久化边界（对齐 Images 表的 emoji 用法）。
// 由装配层适配 internal/database。
type EmojiStore interface {
	// GetByHash 按 sha256 查询 emoji 记录（不存在返回 nil, nil）。
	GetByHash(ctx context.Context, hash string) (*EmojiRecord, error)
	// Upsert 插入或更新记录（按 image_hash + image_type=emoji）。
	Upsert(ctx context.Context, record *EmojiRecord) error
	// Delete 删除记录（完整性检查的破损记录清理）。
	Delete(ctx context.Context, hash string) error
	// ListAll 列出全部 emoji 类型记录。
	ListAll(ctx context.Context) ([]*EmojiRecord, error)
	// CountRegistered 统计已注册 emoji 数（max_reg_num 检查）。
	CountRegistered(ctx context.Context) (int, error)
}

// EmojiRecord 是 Images 表的 emoji 行。
type EmojiRecord struct {
	// ID 是数据库自增主键（scanEmojiRecord 使用）。
	ID           int64
	ImageHash    string
	Description  string
	FullPath     string // 存储路径（项目内相对 posix 路径）
	QueryCount   int
	IsRegistered bool
	IsBanned     bool
	NoFileFlag   bool
	RecordTime   time.Time
	RegisterTime *time.Time
	LastUsedTime *time.Time
	VLMProcessed bool
}

// MaxEmojiForPrompt mirrors MAX_EMOJI_FOR_PROMPT = 20（替换决策 prompt 中的
// 最大候选数）。
const MaxEmojiForPrompt = 20

// NormalizeEmojiTagText mirrors _normalize_emoji_tag_text:
// 把文本或标签列表转为去重的情绪标签列表（分隔符 ,，、;；\r\n\t）。
func NormalizeEmojiTagText(rawValues any) []string {
	var parts []string
	switch v := rawValues.(type) {
	case string:
		if v == "" {
			return []string{}
		}
		parts = splitTagText(v)
	case []string:
		for _, item := range v {
			parts = append(parts, splitTagText(item)...)
		}
	default:
		return []string{}
	}

	var deduped []string
	seen := map[string]bool{}
	for _, tag := range parts {
		normalized := trimSpace(tag)
		if normalized == "" {
			continue
		}
		lowered := toLower(normalized)
		if seen[lowered] {
			continue
		}
		seen[lowered] = true
		deduped = append(deduped, normalized)
	}
	return deduped
}
