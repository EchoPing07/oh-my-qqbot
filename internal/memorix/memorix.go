// Package memorix 是 A_memorix 长期记忆内核的 Go 重写（REWRITE_PLAN §6.14）。
//
// 分层：
//   - storage/    SQLite（modernc）+ FTS5 + 向量表 + 内存 flat 内积索引
//   - retrieval/  BM25 / Aho-Corasick / Personalized PageRank / 双路融合 / 后验图门
//   - embed.go    embedding 接入（维度探测、profile 缓存、失败降级）
//   - extract.go  LLM 实体/关系抽取 + 宽松 JSON 修复 + 关系去重
//   - kernel.go   实现 internal.MemoryKernel（SDK 面向宿主的面）
package memorix

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ChatCompleter 是内核抽取链路需要的最小 LLM 接口（任务名 → 一次对话）。
// 约定：internal/llm.Orchestrator 直接满足该接口（Chat(ctx, task, req)）。
// 采用最小接口而非直接 import internal/llm，便于装配与测试。
type ChatCompleter interface {
	Chat(ctx context.Context, task string, req ChatRequest) (*ChatResponse, error)
}

// Embedder 是 embedding 接入需要的最小 LLM 接口。
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ChatRequest 是内核使用的最小对话请求（与 internal.ChatRequest 字段同形）。
type ChatRequest struct {
	Model       string
	Messages    []ChatMessage
	MaxTokens   int
	Temperature *float64
}

// ChatMessage 是最小对话消息。
type ChatMessage struct {
	Role    string
	Content string
}

// ChatResponse 是最小对话响应。
type ChatResponse struct {
	Content string
}

// ---------------------------------------------------------------------------
// 配置（由 Options 传入，不 import internal/config）
// ---------------------------------------------------------------------------

// EmbeddingOptions 对齐 AMemorixEmbeddingConfig 的核心面。
// 出处：internal/config/config.go L553-L565（字段级对齐 official_configs）。
type EmbeddingOptions struct {
	// ModelName 对齐 embedding.model_name（"auto" 时由 LLM 层路由）。
	ModelName string
	// Dimension 对齐 embedding.dimension（默认 1024）。
	Dimension int
	// DimensionRequestMode 对齐 embedding.dimension_request_mode（explicit|always|never）。
	DimensionRequestMode string
	// BatchSize 对齐 embedding.batch_size。
	BatchSize int
	// EnableCache 对齐 embedding.enable_cache。
	EnableCache bool
	// FallbackEnabled 对齐 embedding.fallback.enabled。
	FallbackEnabled bool
	// ProbeInterval 对齐 embedding.fallback.probe_interval_seconds。
	ProbeInterval time.Duration
	// AllowMetadataOnlyWrite 对齐 embedding.fallback.allow_metadata_only_write。
	AllowMetadataOnlyWrite bool
}

// DefaultEmbeddingOptions 返回与 internal/config 默认值一致的嵌入配置。
func DefaultEmbeddingOptions() EmbeddingOptions {
	return EmbeddingOptions{
		ModelName:              "auto",
		Dimension:              1024,
		DimensionRequestMode:   "explicit",
		BatchSize:              32,
		EnableCache:            false,
		FallbackEnabled:        true,
		ProbeInterval:          180 * time.Second,
		AllowMetadataOnlyWrite: true,
	}
}

// ExtractionOptions 控制 LLM 抽取行为。
type ExtractionOptions struct {
	// Enabled 关闭后 IngestText 只做段落写入与规则实体登记。
	Enabled bool
	// Task 是 llm 层的任务名（对齐 model_task_config 中的工具模型）。
	Task string
	// MaxTokens 是抽取请求的输出上限。
	MaxTokens int
	// Temperature 是抽取请求的温度。
	Temperature float64
	// MaxInputRunes 超过该长度时截断后抽取。
	MaxInputRunes int
}

// DefaultExtractionOptions 返回默认抽取配置。
func DefaultExtractionOptions() ExtractionOptions {
	return ExtractionOptions{
		Enabled:       true,
		Task:          "utils",
		MaxTokens:     1024,
		Temperature:   0.1,
		MaxInputRunes: 4000,
	}
}

// Options 是内核构造参数（配置由装配方从 internal/config 转换后传入）。
type Options struct {
	// DataDir 对齐 storage.data_dir（默认 data/a-memorix）。
	DataDir string
	// DBPath 显式指定库文件（优先于 DataDir）。
	DBPath string
	// Retrieval 是双路检索配置（默认照抄 Python 常数）。
	Retrieval RetrievalOptions
	// Embedding 是嵌入配置。
	Embedding EmbeddingOptions
	// Extraction 是抽取配置。
	Extraction ExtractionOptions
	// MaxReadConns 是 SQLite 读连接池大小。
	MaxReadConns int
	// BusyTimeout 是 SQLite busy_timeout。
	BusyTimeout time.Duration
	// EnableFTS 控制 FTS5 虚表（默认 true）。
	EnableFTS bool
}

// DefaultOptions 返回默认内核配置。
func DefaultOptions() Options {
	return Options{
		DataDir:      "data/a-memorix",
		Retrieval:    DefaultRetrievalOptions(),
		Embedding:    DefaultEmbeddingOptions(),
		Extraction:   DefaultExtractionOptions(),
		MaxReadConns: 8,
		BusyTimeout:  5 * time.Second,
		EnableFTS:    true,
	}
}

// ErrEmbeddingUnavailable 表示嵌入通道不可用（允许 metadata-only 写入）。
var ErrEmbeddingUnavailable = errors.New("memorix: 嵌入服务不可用")

// wrapError 统一包装错误信息。
func wrapError(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("memorix: %s: %w", op, err)
}
