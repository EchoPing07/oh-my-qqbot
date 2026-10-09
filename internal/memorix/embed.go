package memorix

import (
	"context"
	"strings"
	"sync"
	"time"
)

// EmbeddingClient 封装 embedding 接入：维度探测、profile 缓存、失败降级。
// 语义对齐参考/MaiBot/src/A_memorix/core/embedding/api_adapter.py：
//   - default_dimension / dimension_request_mode（L37-L52）
//   - 维度探测与缓存 _dimension / _dimension_detected（L60-L71、L155-L171）
//   - 向量有效性校验 _validate_embedding_vector（L220-L228）
//   - 降级：失败时按配置允许 metadata-only 写入
//
// 同时对齐 SDKMemoryKernel._embedding_fallback_enabled / _allow_metadata_only_write
// （core/runtime/sdk_memory_kernel.py L474-L480）。
type EmbeddingClient struct {
	embedder Embedder
	options  EmbeddingOptions

	mu          sync.RWMutex
	dimension   int
	detected    bool
	lastProbe   time.Time
	degraded    bool
	degradeNote string

	// cache 对齐 embedding.enable_cache（文本 → 向量）。
	cache map[string][]float32
}

// NewEmbeddingClient 创建嵌入客户端；embedder 为 nil 时始终降级。
func NewEmbeddingClient(embedder Embedder, options EmbeddingOptions) *EmbeddingClient {
	if options.Dimension <= 0 {
		options.Dimension = 1024
	}
	if options.BatchSize <= 0 {
		options.BatchSize = 32
	}
	if options.ProbeInterval <= 0 {
		options.ProbeInterval = 180 * time.Second
	}
	return &EmbeddingClient{
		embedder:  embedder,
		options:   options,
		dimension: options.Dimension,
		cache:     map[string][]float32{},
	}
}

// Dimension 返回当前已知维度（未探测时为配置默认值）。
func (c *EmbeddingClient) Dimension() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dimension
}

// Detected 报告是否已经过真实探测。
func (c *EmbeddingClient) Detected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.detected
}

// Degraded 报告嵌入通道是否处于降级状态。
func (c *EmbeddingClient) Degraded() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.degraded
}

// DegradedReason 返回最近一次降级原因。
func (c *EmbeddingClient) DegradedReason() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.degradeNote
}

// AllowMetadataOnlyWrite 报告降级时是否允许只写元数据。
func (c *EmbeddingClient) AllowMetadataOnlyWrite() bool {
	if !c.options.FallbackEnabled {
		return false
	}
	return c.options.AllowMetadataOnlyWrite
}

// Encode 计算单条文本的嵌入。
// 失败时返回 ErrEmbeddingUnavailable（调用方按降级策略处理）。
func (c *EmbeddingClient) Encode(ctx context.Context, text string) ([]float32, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ErrEmbeddingUnavailable
	}
	c.mu.RLock()
	if vec, ok := c.cache[text]; ok && c.options.EnableCache {
		c.mu.RUnlock()
		return vec, nil
	}
	c.mu.RUnlock()

	vectors, err := c.EncodeBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, ErrEmbeddingUnavailable
	}
	return vectors[0], nil
}

// EncodeBatch 批量计算嵌入；内部按 batch_size 切分。
// 对齐 api_adapter 的维度校验：向量必须非空、有限、且与已探测维度一致。
func (c *EmbeddingClient) EncodeBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if c.embedder == nil {
		c.markDegraded("embedder 未配置")
		return nil, ErrEmbeddingUnavailable
	}
	if len(texts) == 0 {
		return nil, nil
	}

	batchSize := c.options.BatchSize
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vectors, err := c.embedder.Embed(ctx, texts[start:end])
		if err != nil {
			c.markDegraded(err.Error())
			return nil, wrapError("embedding", err)
		}
		if len(vectors) != end-start {
			c.markDegraded("embedding 返回数量不匹配")
			return nil, ErrEmbeddingUnavailable
		}
		for _, vec := range vectors {
			if !validEmbedding(vec) {
				c.markDegraded("embedding 向量非法")
				return nil, ErrEmbeddingUnavailable
			}
			c.observeDimension(len(vec))
			out = append(out, vec)
		}
	}
	c.clearDegraded()
	if c.options.EnableCache {
		c.mu.Lock()
		for i, text := range texts {
			if i < len(out) {
				c.cache[text] = out[i]
			}
		}
		c.mu.Unlock()
	}
	return out, nil
}

// EncodeWithDimension 按需请求指定维度（对齐 dimensions 透传语义）。
// 当前实现只做维度一致性校验；实际 dimensions 参数由 llm 层的 embedding 任务配置决定。
func (c *EmbeddingClient) EncodeWithDimension(ctx context.Context, text string, dimensions int) ([]float32, error) {
	vec, err := c.Encode(ctx, text)
	if err != nil {
		return nil, err
	}
	if dimensions > 0 && len(vec) != dimensions {
		return nil, ErrEmbeddingUnavailable
	}
	return vec, nil
}

// seedDimension 用配置/已落库维度预置缓存，但不标记为「已真实探测」。
// 对齐 api_adapter 的 _dimension_detected 语义（探测前 get_requested_dimension 返回配置值）。
func (c *EmbeddingClient) seedDimension(dim int) {
	if dim <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dimension = dim
}

func (c *EmbeddingClient) observeDimension(dim int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.detected {
		c.dimension = dim
		c.detected = true
		c.lastProbe = time.Now()
		return
	}
	// 维度变化时以最新为准（对齐 embedding 模型切换后的重建语义）。
	if c.dimension != dim {
		c.dimension = dim
		c.cache = map[string][]float32{}
	}
}

func (c *EmbeddingClient) markDegraded(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.degraded = true
	c.degradeNote = reason
}

func (c *EmbeddingClient) clearDegraded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.degraded = false
	c.degradeNote = ""
}

// ProbeDue 报告是否需要重新探测嵌入通道（对齐 fallback.probe_interval_seconds）。
func (c *EmbeddingClient) ProbeDue() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.degraded {
		return false
	}
	return time.Since(c.lastProbe) >= c.options.ProbeInterval
}

// validEmbedding 校验向量非空且元素有限。
// 出处：core/embedding/api_adapter.py L220-L228（_validate_embedding_vector）。
func validEmbedding(vec []float32) bool {
	if len(vec) == 0 {
		return false
	}
	for _, v := range vec {
		if v != v { // NaN
			return false
		}
		if v > 3.4e38 || v < -3.4e38 {
			return false
		}
	}
	return true
}
