// Package image 实现图片系统（对照 参考MaiBot/src/chat/image_system/，
// 行为以 image_manager.py / image_cache_cleanup.py 为权威）：
//
//   - 图片保存（sha256 命名、data/images 目录、Images 表登记/回填）
//   - VLM 描述（image_description.prompt 照抄；vlm_processed 标记）
//   - 缓存清理（tracked files / orphan files / no-file records 三段清理）
//
// 依赖接口注入：不 import internal/qq、internal/config；VLM 用包内最小接口。
package image

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ImageManager 对齐 image_manager.ImageManager。
type ImageManager struct {
	imageDir     string
	store        ImageStore
	paths        StorePathResolver
	vlm          VLMClient
	prompts      PromptRenderer
	maxSizeBytes int64 // max_image_size_mb（0 不限制）
	now          func() time.Time
}

// VLMRequest 是一次视觉描述请求。
type VLMRequest struct {
	Prompt      string
	ImageBytes  []byte
	ImageFormat string
}

// VLMClient 是图片系统需要的最小视觉模型边界。
type VLMClient interface {
	// GenerateResponseForImage 对齐 LLMServiceClient.generate_response_for_image。
	GenerateResponseForImage(ctx context.Context, req VLMRequest) (string, error)
	// Configured 对齐 _is_vlm_task_configured。
	Configured() bool
}

// PromptRenderer 渲染 image_description 模板。
type PromptRenderer interface {
	Render(ctx context.Context, promptName string, vars map[string]string) (string, error)
}

// StorePathResolver 对齐 image_path.py：库内相对 posix 路径 ↔ 绝对路径。
type StorePathResolver interface {
	Resolve(storedPath string) (string, error)
	Serialize(absolutePath string) (string, error)
}

// ImageRecord 是 images 表中 image_type=image 的行。
type ImageRecord struct {
	ID           int64
	ImageHash    string
	Description  string
	FullPath     string // 存储路径（项目内相对 posix 路径）
	QueryCount   int
	NoFileFlag   bool
	RecordTime   time.Time
	LastUsedTime *time.Time
	VLMProcessed bool
}

// ImageStore 是图片持久化边界（由装配层适配 internal/database）。
type ImageStore interface {
	// GetByHash 按 sha256 查询 image 类型记录（不存在返回 nil, nil）。
	GetByHash(ctx context.Context, hash string) (*ImageRecord, error)
	// Upsert 插入或更新记录（按 image_hash + image_type=image）。
	Upsert(ctx context.Context, record *ImageRecord) error
	// Delete 删除记录。
	Delete(ctx context.Context, hash string) error
	// ListAll 列出全部 image 类型记录。
	ListAll(ctx context.Context) ([]*ImageRecord, error)
}

// Options 是 ImageManager 的注入参数。
type Options struct {
	Store    ImageStore
	Paths    StorePathResolver
	VLM      VLMClient
	Prompts  PromptRenderer
	ImageDir string
	// MaxSizeMB 对齐 visual.max_image_size_mb（0 不限制）。
	MaxSizeMB float64
	Now       func() time.Time
}

// NewImageManager mirrors ImageManager.__init__：确保 data/images 目录存在。
func NewImageManager(options Options) *ImageManager {
	if options.Now == nil {
		options.Now = time.Now
	}
	_ = os.MkdirAll(options.ImageDir, 0o755)
	var maxSize int64
	if options.MaxSizeMB > 0 {
		maxSize = int64(options.MaxSizeMB * 1024 * 1024)
	}
	return &ImageManager{
		imageDir:     options.ImageDir,
		store:        options.Store,
		paths:        options.Paths,
		vlm:          options.VLM,
		prompts:      options.Prompts,
		maxSizeBytes: maxSize,
		now:          options.Now,
	}
}

// ---------------------------------------------------------------------------
// 保存（sha256）
// ---------------------------------------------------------------------------

// SHA256Hex 计算字节内容的 sha256 十六进制串。
func SHA256Hex(data []byte) string {
	sum := sha256Sum(data)
	return sum
}

// EnsureImageSaved mirrors ensure_image_saved：按 sha256 保存图片文件与记录；
// 记录存在但文件缺失时重新保存（恢复 no_file_flag）。
func (m *ImageManager) EnsureImageSaved(ctx context.Context, imageBytes []byte) (*ImageRecord, string, error) {
	if len(imageBytes) == 0 {
		return nil, "", fmt.Errorf("image: 图片字节数据为空")
	}
	if m.maxSizeBytes > 0 && int64(len(imageBytes)) > m.maxSizeBytes {
		return nil, "", fmt.Errorf("image: 图片大小超过上限 %d 字节", m.maxSizeBytes)
	}
	hashStr := SHA256Hex(imageBytes)

	if m.store != nil {
		record, err := m.store.GetByHash(ctx, hashStr)
		if err != nil {
			return nil, "", fmt.Errorf("image: 查询图片记录时发生错误: %w", err)
		}
		if record != nil {
			path, pathErr := m.paths.Resolve(record.FullPath)
			if !record.NoFileFlag && pathErr == nil && fileExists(path) {
				// 已存在：刷新 last_used_time 与 query_count（对齐 ensure 分支）。
				record.LastUsedTime = ptrTime(m.now())
				record.QueryCount++
				_ = m.store.Upsert(ctx, record)
				return record, path, nil
			}
			// 记录存在但文件缺失 → 重新保存文件
			path, saveErr := m.writeImageFile(hashStr, imageBytes)
			if saveErr != nil {
				return nil, "", saveErr
			}
			stored, serErr := m.paths.Serialize(path)
			if serErr != nil {
				return nil, "", serErr
			}
			record.FullPath = stored
			record.NoFileFlag = false
			record.LastUsedTime = ptrTime(m.now())
			if err := m.store.Upsert(ctx, record); err != nil {
				return nil, "", fmt.Errorf("image: 保存图片记录到数据库失败: %w", err)
			}
			return record, path, nil
		}
	}

	path, err := m.writeImageFile(hashStr, imageBytes)
	if err != nil {
		return nil, "", err
	}
	stored, serErr := m.paths.Serialize(path)
	if serErr != nil {
		return nil, "", serErr
	}
	record := &ImageRecord{
		ImageHash:  hashStr,
		FullPath:   stored,
		RecordTime: m.now(),
	}
	if m.store != nil {
		if err := m.store.Upsert(ctx, record); err != nil {
			return nil, "", fmt.Errorf("image: 保存图片记录到数据库失败: %w", err)
		}
	}
	return record, path, nil
}

// writeImageFile 把图片写入 data/images/<sha256>.<format>（对齐
// tmp 文件 → calculate_hash_format 修正扩展名的流程）。
func (m *ImageManager) writeImageFile(hashStr string, imageBytes []byte) (string, error) {
	format := DetectImageFormat(imageBytes)
	ext := format
	if ext == "" {
		ext = "bin"
	}
	tmpPath := filepath.Join(m.imageDir, hashStr+".tmp")
	if err := os.WriteFile(tmpPath, imageBytes, 0o644); err != nil {
		return "", fmt.Errorf("image: 写入图片临时文件失败: %w", err)
	}
	finalPath := filepath.Join(m.imageDir, hashStr+"."+ext)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", fmt.Errorf("image: 重命名图片文件失败: %w", err)
	}
	return finalPath, nil
}

// DetectImageFormat mirrors get_image_format：从图片字节探测实际格式。
func DetectImageFormat(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	switch format {
	case "png", "jpeg", "gif", "bmp", "webp":
		if format == "jpeg" {
			return "jpg"
		}
		return format
	}
	return ""
}

// ---------------------------------------------------------------------------
// VLM 描述
// ---------------------------------------------------------------------------

// GetCachedImageDescription mirrors get_cached_image_description：只读缓存，
// 不触发模型调用。
func (m *ImageManager) GetCachedImageDescription(ctx context.Context, imageHash string) string {
	if m.store == nil {
		return ""
	}
	record, err := m.store.GetByHash(ctx, strings.TrimSpace(imageHash))
	if err != nil || record == nil || !record.VLMProcessed {
		return ""
	}
	return strings.TrimSpace(record.Description)
}

// GetImageDescription mirrors get_image_description：
// 优先数据库缓存 → 未提供字节返回空 → VLM 未配置返回空 → 构建。
func (m *ImageManager) GetImageDescription(ctx context.Context, imageHash string, imageBytes []byte, waitForBuild bool) (string, error) {
	if imageHash == "" && len(imageBytes) == 0 {
		return "", fmt.Errorf("image: 必须提供图片哈希值或图片字节数据")
	}
	if imageHash == "" {
		imageHash = SHA256Hex(imageBytes)
	}

	// 描述可以比缓存文件保留更久，命中描述不能跳过文件恢复。
	if len(imageBytes) > 0 {
		record, path, err := m.EnsureImageSaved(ctx, imageBytes)
		if err != nil {
			// 保存失败必须上抛，调用方才能保留原始字节。
			return "", err
		}
		_ = record
		_ = path
	}

	if m.store != nil {
		record, err := m.store.GetByHash(ctx, imageHash)
		if err != nil {
			return "", fmt.Errorf("image: 查询图片描述时发生错误: %w", err)
		}
		if record != nil && record.VLMProcessed && strings.TrimSpace(record.Description) != "" {
			return record.Description, nil
		}
	}
	if len(imageBytes) == 0 {
		return "", nil
	}
	if m.vlm == nil || !m.vlm.Configured() {
		return "", nil
	}
	if !waitForBuild {
		go func() {
			_, _ = m.BuildImageDescription(context.Background(), imageBytes)
		}()
		return "", nil
	}
	return m.BuildImageDescription(ctx, imageBytes)
}

// BuildImageDescription mirrors build_image_description / save_image_and_process：
// 保存 + VLM 描述 + 更新记录（vlm_processed=true）。
func (m *ImageManager) BuildImageDescription(ctx context.Context, imageBytes []byte) (string, error) {
	record, path, err := m.EnsureImageSaved(ctx, imageBytes)
	if err != nil {
		return "", err
	}
	if record.VLMProcessed && strings.TrimSpace(record.Description) != "" {
		return record.Description, nil
	}
	if m.vlm == nil || !m.vlm.Configured() {
		return "", nil
	}
	desc, err := m.generateImageDescription(ctx, imageBytes)
	if err != nil {
		return "", fmt.Errorf("image: 生成图片描述时发生错误: %w", err)
	}
	record.Description = desc
	record.VLMProcessed = true
	if m.store != nil {
		if err := m.store.Upsert(ctx, record); err != nil {
			return "", fmt.Errorf("image: 更新图片描述失败: %w", err)
		}
	}
	_ = path
	return desc, nil
}

// generateImageDescription mirrors _generate_image_description:
// image_description.prompt 照抄。
func (m *ImageManager) generateImageDescription(ctx context.Context, imageBytes []byte) (string, error) {
	prompt := "请用中文详细描述这张图片的内容。如果有文字，请把文字描述概括出来，请留意其主题、直观感受，输出为一段平文本，最多100字，请注意不要分点，就输出一段文本"
	if m.prompts != nil {
		if rendered, err := m.prompts.Render(ctx, "image_description", nil); err == nil && strings.TrimSpace(rendered) != "" {
			prompt = rendered
		}
	}
	description, err := m.vlm.GenerateResponseForImage(ctx, VLMRequest{
		Prompt:      prompt,
		ImageBytes:  imageBytes,
		ImageFormat: DetectImageFormat(imageBytes),
	})
	if err != nil {
		return "", err
	}
	return description, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func ptrTime(t time.Time) *time.Time { return &t }

func sha256Sum(data []byte) string {
	// 独立封装便于测试替换；实现就是 sha256。
	sum := sha256HexBytes(data)
	return sum
}
