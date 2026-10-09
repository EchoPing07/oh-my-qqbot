package emoji

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file mirrors 参考/MaiBot/src/emoji_system/emoji_manager.py（行为以该
// 文件为权威）：EmojiManager 的注册/描述/情绪标签/偷表情/替换/选择/维护逻辑。

// Manager 对齐 EmojiManager：内存可发送池 + Images 表登记。
type Manager struct {
	mu sync.Mutex

	emojis      []*Emoji
	emojiNum    int
	maxRegNum   int
	emojiDir    string
	projectRoot string

	store          EmojiStore
	paths          StorePathResolver
	vlm            VLMClient
	textLLM        TextLLM
	prompts        PromptRenderer
	rng            *rand.Rand
	maxEmojiSizeMB float64
	now            func() time.Time

	// outbound 是表情出站发送通道（SetOutbound 注入）。
	outbound OutboundSender
	// candidateCount 是新逻辑子代理候选数量（SetCandidateCount 注入）。
	candidateCount int
}

// Options 是 Manager 的注入参数。
type Options struct {
	// Store 是 Images 表的持久化边界（必填）。
	Store EmojiStore
	// Paths 是项目内路径解析边界（必填）。
	Paths StorePathResolver
	// VLM 是视觉模型边界（可 nil：跳过识别/审核）。
	VLM VLMClient
	// TextLLM 是替换决策的文字模型边界（可 nil）。
	TextLLM TextLLM
	// Prompts 渲染 emoji_content_analysis / emoji_content_filtration /
	// emoji_replace / emoji_selection 模板（可 nil：使用内置回退文案）。
	Prompts PromptRenderer
	// EmojiDir 是 data/emoji 目录。
	EmojiDir string
	// ProjectRoot 是项目根目录（路径越界保护）。
	ProjectRoot string
	// MaxRegNum 对齐 emoji.max_reg_num（默认 64）。
	MaxRegNum int
	// MaxEmojiSizeMB 对齐 emoji.max_emoji_size_mb（默认 5；0 不限制）。
	MaxEmojiSizeMB float64
	// Rng / Now 可注入（测试用）。
	Rng *rand.Rand
	Now func() time.Time
	// Log 是结构化日志；nil 用 slog.Default()。
	Log *slog.Logger
}

// NewManager mirrors EmojiManager.__init__：确保目录存在。
func NewManager(options Options) *Manager {
	if options.MaxRegNum <= 0 {
		options.MaxRegNum = 64
	}
	if options.MaxEmojiSizeMB < 0 {
		options.MaxEmojiSizeMB = 0
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Rng == nil {
		options.Rng = rand.New(rand.NewSource(options.Now().UnixNano()))
	}
	_ = os.MkdirAll(options.EmojiDir, 0o755)
	return &Manager{
		emojis:         []*Emoji{},
		maxRegNum:      options.MaxRegNum,
		emojiDir:       options.EmojiDir,
		projectRoot:    options.ProjectRoot,
		store:          options.Store,
		paths:          options.Paths,
		vlm:            options.VLM,
		textLLM:        options.TextLLM,
		prompts:        options.Prompts,
		rng:            options.Rng,
		maxEmojiSizeMB: options.MaxEmojiSizeMB,
		now:            options.Now,
	}
}

// Rng 暴露随机源（子代理候选采样共用，保证可测试性）。
func (m *Manager) Rng() *rand.Rand { return m.rng }

// Emojis 返回内存可发送池快照（调用方不得修改）。
func (m *Manager) Emojis() []*Emoji {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Emoji{}, m.emojis...)
}

// EmojiNum 返回可发送池大小。
func (m *Manager) EmojiNum() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.emojiNum
}

// ---------------------------------------------------------------------------
// 缓存与描述
// ---------------------------------------------------------------------------

// isCollectedEmojiSizeAllowed mirrors _is_collected_emoji_size_allowed
// （max_emoji_size_mb<=0 时不限制）。
func (m *Manager) isCollectedEmojiSizeAllowed(sizeBytes int) bool {
	if m.maxEmojiSizeMB <= 0 {
		return true
	}
	return sizeBytes <= int(m.maxEmojiSizeMB*1024*1024)
}

// EnsureEmojiSaved mirrors ensure_emoji_saved：先缓存表情文件与数据库记录，
// 确保后续可按 hash 回填。达 max_emoji_size_mb 时报错跳过。
func (m *Manager) EnsureEmojiSaved(ctx context.Context, emojiBytes []byte, emojiHash string) (*Emoji, error) {
	if !m.isCollectedEmojiSizeAllowed(len(emojiBytes)) {
		return nil, fmt.Errorf("表情包大小超过收集上限 %g MB，跳过缓存", m.maxEmojiSizeMB)
	}
	if emojiHash == "" {
		emojiHash = sha256Hex(emojiBytes)
	}

	// 已有可用记录直接返回（对齐 _ensure_emoji_saved_locked）。
	if m.store != nil {
		record, err := m.store.GetByHash(ctx, emojiHash)
		if err != nil {
			return nil, fmt.Errorf("缓存表情包前查询数据库时出错: %w", err)
		}
		if record != nil && !record.NoFileFlag {
			if path, pathErr := m.paths.Resolve(record.FullPath); pathErr == nil {
				if fileExists(path) {
					return m.emojiFromRecord(record, path), nil
				}
			}
		}
	}

	// 写临时文件 → 修正扩展名 → 登记/回填记录。
	tmpPath := filepath.Join(m.emojiDir, emojiHash+".tmp")
	if err := os.WriteFile(tmpPath, emojiBytes, 0o644); err != nil {
		return nil, fmt.Errorf("写入表情包临时文件失败: %w", err)
	}
	format := detectImageFormat(emojiBytes)
	if format != "" {
		finalPath := filepath.Join(m.emojiDir, emojiHash+"."+format)
		if err := os.Rename(tmpPath, finalPath); err != nil {
			return nil, fmt.Errorf("重命名表情包文件失败: %w", err)
		}
		tmpPath = finalPath
	}
	storedPath, err := m.paths.Serialize(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("序列化表情包路径失败: %w", err)
	}
	emoji := &Emoji{
		FileHash:    emojiHash,
		FullPath:    tmpPath,
		FileName:    filepath.Base(tmpPath),
		ImageFormat: format,
	}

	if m.store != nil {
		existing, err := m.store.GetByHash(ctx, emojiHash)
		if err != nil {
			return nil, fmt.Errorf("缓存表情包记录到数据库时出错: %w", err)
		}
		if existing != nil {
			existing.FullPath = storedPath
			existing.NoFileFlag = false
			if err := m.store.Upsert(ctx, existing); err != nil {
				return nil, fmt.Errorf("缓存表情包记录到数据库时出错: %w", err)
			}
			return m.emojiFromRecord(existing, tmpPath), nil
		}
		now := m.now()
		record := &EmojiRecord{
			ImageHash:    emojiHash,
			FullPath:     storedPath,
			RecordTime:   now,
			IsRegistered: false,
			IsBanned:     false,
			NoFileFlag:   false,
			QueryCount:   0,
		}
		if err := m.store.Upsert(ctx, record); err != nil {
			return nil, fmt.Errorf("缓存表情包记录到数据库时出错: %w", err)
		}
	}
	return emoji, nil
}

// GetEmojiDescription mirrors get_emoji_description：优先内存 → 数据库 →
// VLM 构建描述。返回 (描述, 情绪标签, 是否找到/构建成功)。
func (m *Manager) GetEmojiDescription(
	ctx context.Context,
	emojiBytes []byte,
	emojiHash string,
	sessionID string,
	waitForBuild bool,
) (string, []string, bool) {
	if emojiHash == "" {
		if emojiBytes == nil {
			return "", nil, false
		}
		emojiHash = sha256Hex(emojiBytes)
	}
	// 先查内存
	if emoji := m.getEmojiByHash(emojiHash); emoji != nil {
		if emojiBytes != nil && !fileExists(emoji.FullPath) {
			if restored, err := m.EnsureEmojiSaved(ctx, emojiBytes, emojiHash); err == nil {
				emoji.FullPath = restored.FullPath
				emoji.FileName = restored.FileName
			}
		}
		return emoji.Description, NormalizeEmojiTagText(emoji.Description), true
	}
	// 再查数据库
	if m.store != nil {
		record, err := m.store.GetByHash(ctx, emojiHash)
		if err == nil && record != nil {
			description := record.Description
			emotions := NormalizeEmojiTagText(description)
			if emojiBytes != nil && (record.NoFileFlag || !m.recordFileExists(record)) {
				if restored, err := m.EnsureEmojiSaved(ctx, emojiBytes, emojiHash); err == nil {
					_ = restored
				}
			}
			return description, emotions, true
		}
	}
	// 数据库未找到：无字节无法构建
	if emojiBytes == nil {
		return "", nil, false
	}
	if m.vlm == nil || !m.vlm.Configured() {
		if _, err := m.EnsureEmojiSaved(ctx, emojiBytes, emojiHash); err != nil {
			slog.Warn("缓存表情包失败", "err", err)
		}
		slog.Info("未配置 VLM 模型，跳过表情包识别、打标签和审核")
		return "", nil, false
	}
	if !waitForBuild {
		if _, err := m.EnsureEmojiSaved(ctx, emojiBytes, emojiHash); err != nil {
			slog.Warn("缓存表情包失败", "err", err)
			return "", nil, false
		}
		go m.buildAndCacheEmojiDescription(context.Background(), emojiHash, emojiBytes, sessionID)
		return "", nil, false
	}
	if desc, _, ok := m.buildAndCacheEmojiDescription(ctx, emojiHash, emojiBytes, sessionID); ok {
		return desc, NormalizeEmojiTagText(desc), true
	}
	return "", nil, false
}

// buildAndCacheEmojiDescription mirrors _build_and_cache_emoji_description。
func (m *Manager) buildAndCacheEmojiDescription(ctx context.Context, emojiHash string, emojiBytes []byte, sessionID string) (string, *Emoji, bool) {
	newEmoji, err := m.EnsureEmojiSaved(ctx, emojiBytes, emojiHash)
	if err != nil {
		slog.Warn("构建表情包描述前缓存失败", "hash", emojiHash, "err", err)
		return "", nil, false
	}
	successDesc, updatedEmoji := m.BuildEmojiDescription(ctx, newEmoji, sessionID)
	if successDesc == "" {
		slog.Error("Build emoji description failed")
		return "", nil, false
	}
	if m.store != nil {
		record, err := m.store.GetByHash(ctx, updatedEmoji.FileHash)
		if err == nil && record != nil {
			record.Description = updatedEmoji.Description
			record.NoFileFlag = false
			record.VLMProcessed = true
			if err := m.store.Upsert(ctx, record); err != nil {
				slog.Error("Update cached emoji description failed", "err", err)
			}
		}
	}
	return updatedEmoji.Description, updatedEmoji, true
}

// BuildEmojiDescription mirrors build_emoji_description：VLM 生成描述 +
// 情绪标签（emoji_content_analysis.prompt 照抄）。返回 (描述文本, emoji, 成功)。
// 未配置 VLM 时返回空描述。
func (m *Manager) BuildEmojiDescription(ctx context.Context, target *Emoji, sessionID string) (string, *Emoji) {
	if m.vlm == nil || !m.vlm.Configured() {
		slog.Info("[构建描述] 未配置 VLM 模型，跳过表情包识别和打标签", "file", target.FileName)
		return "", target
	}
	if target.FileHash == "" || target.ImageFormat == "" {
		if target.ImageFormat == "" {
			target.ImageFormat = detectImageFormat(emojiBytesOf(m, target))
		}
	}
	imageBytes, err := os.ReadFile(target.FullPath)
	if err != nil {
		slog.Error("[构建描述] 读取表情包文件失败", "path", target.FullPath, "err", err)
		return "", target
	}
	requestFormat := target.ImageFormat
	if requestFormat == "gif" {
		// GIF → 抽取首帧静态图（对齐 ImageUtils.gif_2_static_image 的意图：
		// 转静态图请求；Go 版取首帧）。
		if staticBytes, staticFormat, ok := gifFirstFrame(imageBytes); ok {
			imageBytes = staticBytes
			requestFormat = staticFormat
		} else {
			slog.Error("[构建描述] 转换 GIF 图片时出错", "file", target.FileName)
			return "", target
		}
	}

	imageType := "STATIC"
	if target.ImageFormat == "gif" {
		imageType = "GIF"
	}
	analysisPrompt := m.renderPromptOrDefault(ctx, "emoji_content_analysis",
		map[string]string{"image_type": imageType}, "emoji_content_analysis")
	description, err := m.vlm.GenerateResponseForImage(ctx, VLMRequest{
		Prompt:      analysisPrompt,
		ImageBytes:  imageBytes,
		ImageFormat: requestFormat,
		SessionID:   sessionID,
	})
	if err != nil {
		slog.Error("[构建描述] 调用视觉模型生成表情包描述时出错", "err", err)
		return "", target
	}
	normalizedDescription := strings.TrimSpace(description)
	if normalizedDescription == "" {
		slog.Warn("[构建描述] 视觉模型返回空描述，跳过注册", "file", target.FileName)
		return "", target
	}
	normalizedEmotions := NormalizeEmojiTagText(normalizedDescription)
	if len(normalizedEmotions) == 0 {
		slog.Warn("[构建描述] 视觉模型返回空标签，跳过注册", "file", target.FileName)
		return "", target
	}
	target.Description = strings.Join(normalizedEmotions, ",")
	target.Emotions = normalizedEmotions
	slog.Info("理解表情包情绪", "hash", target.FileHash, "desc", target.Description)
	return target.Description, target
}

// ReviewEmojiForRegistration mirrors review_emoji_for_registration：
// content_filtration 关闭时直接通过；emoji_content_filtration.prompt 照抄。
func (m *Manager) ReviewEmojiForRegistration(ctx context.Context, target *Emoji, sessionID string) bool {
	// content_filtration 开关由装配层决定是否注入审核（这里以渲染审核 prompt
	// 的能力承载：关闭时不传 filtration prompt vars）。为保持注入面最小，
	// 以「装配层在 Options.FiltrationEnabled 之上调用本方法与否」承载开关；
	// Manager 提供默认通过 + VLM 审核两个路径。
	if m.vlm == nil || !m.vlm.Configured() {
		slog.Warn("[表情包审查] 已启用内容过滤但未配置 VLM 模型，拒绝注册", "file", target.FileName)
		return false
	}
	imageBytes, err := os.ReadFile(target.FullPath)
	if err != nil {
		slog.Error("[表情包审查] 读取表情包文件失败", "err", err)
		return false
	}
	requestFormat := target.ImageFormat
	if requestFormat == "gif" {
		if staticBytes, staticFormat, ok := gifFirstFrame(imageBytes); ok {
			imageBytes = staticBytes
			requestFormat = staticFormat
		} else {
			return false
		}
	}
	reviewPrompt := m.renderPromptOrDefault(ctx, "emoji_content_filtration", nil, "emoji_content_filtration")
	response, err := m.vlm.GenerateResponseForImage(ctx, VLMRequest{
		Prompt:      reviewPrompt,
		ImageBytes:  imageBytes,
		ImageFormat: requestFormat,
		SessionID:   sessionID,
	})
	if err != nil {
		slog.Error("[表情包审查] 调用视觉模型审查表情包时出错", "err", err)
		return false
	}
	if strings.Contains(response, "否") {
		slog.Warn("[表情包审查] 表情包内容不符合要求，拒绝注册", "file", target.FileName)
		return false
	}
	return true
}

// renderPromptOrDefault 渲染 prompt 模板；未注入 PromptRenderer 时回退到
// prompts/zh-CN 原文的内联副本（保持提示词语义不缺失）。
func (m *Manager) renderPromptOrDefault(ctx context.Context, name string, vars map[string]string, fallback string) string {
	if m.prompts != nil {
		if rendered, err := m.prompts.Render(ctx, name, vars); err == nil {
			return rendered
		}
	}
	return fallbackText(name, vars, fallback)
}

// ---------------------------------------------------------------------------
// 注册 / 删除 / 封禁 / 使用记录
// ---------------------------------------------------------------------------

// LoadEmojisFromDB mirrors load_emojis_from_db：加载已注册表情包到内存，
// 清理缺少文件的破损注册记录。
func (m *Manager) LoadEmojisFromDB(ctx context.Context) error {
	if m.store == nil {
		return nil
	}
	records, err := m.store.ListAll(ctx)
	if err != nil {
		m.mu.Lock()
		m.emojis = []*Emoji{}
		m.emojiNum = 0
		m.mu.Unlock()
		return fmt.Errorf("加载数据库表情包记录时发生不可恢复错误: %w", err)
	}
	loaded := make([]*Emoji, 0)
	removed := 0
	for _, record := range records {
		if !record.IsRegistered || record.IsBanned {
			continue
		}
		path, err := m.paths.Resolve(record.FullPath)
		if err != nil || record.NoFileFlag || !fileExists(path) {
			slog.Warn("[数据库] 已注册表情包缺少实际文件，删除破损注册记录", "path", record.FullPath)
			_ = m.store.Delete(ctx, record.ImageHash)
			removed++
			continue
		}
		loaded = append(loaded, m.emojiFromRecord(record, path))
	}
	m.mu.Lock()
	m.emojis = loaded
	m.emojiNum = len(loaded)
	m.mu.Unlock()
	slog.Info("[数据库] 加载已注册表情包完成", "count", len(loaded), "removed", removed)
	return nil
}

// RegisterEmojiToDB mirrors register_emoji_to_db：只登记数据库，不移动源文件。
func (m *Manager) RegisterEmojiToDB(ctx context.Context, emoji *Emoji) RegisterStatus {
	if emoji == nil || emoji.FileHash == "" {
		return RegisterStatusFailed
	}
	if !fileExists(emoji.FullPath) {
		slog.Error("[register_emoji] Emoji file does not exist", "path", emoji.FullPath)
		return RegisterStatusFailed
	}
	if m.store == nil {
		return RegisterStatusFailed
	}
	existing, err := m.store.GetByHash(ctx, emoji.FileHash)
	if err != nil {
		slog.Error("[register_emoji] Database query failed", "err", err)
		return RegisterStatusFailed
	}
	if existing != nil {
		if existing.IsBanned {
			slog.Info("[register_emoji] Emoji is banned, skipping", "hash", emoji.FileHash)
			return RegisterStatusSkipped
		}
		if existing.IsRegistered && !existing.NoFileFlag {
			if path, pathErr := m.paths.Resolve(existing.FullPath); pathErr == nil && fileExists(path) {
				return RegisterStatusSkipped
			}
		}
		normalizedDescription := strings.TrimSpace(orDefault(emoji.Description, existing.Description))
		normalizedEmotions := NormalizeEmojiTagText(normalizedDescription)
		registerTime := orTime(existing.RegisterTime, m.now())
		storedPath, pathErr := m.paths.Serialize(emoji.FullPath)
		if pathErr != nil {
			slog.Error("[register_emoji] serialize path failed", "err", pathErr)
			return RegisterStatusFailed
		}
		existing.FullPath = storedPath
		existing.Description = normalizedDescription
		existing.RegisterTime = &registerTime
		existing.IsRegistered = true
		existing.IsBanned = false
		existing.NoFileFlag = false
		if err := m.store.Upsert(ctx, existing); err != nil {
			return RegisterStatusFailed
		}
		emoji.Description = normalizedDescription
		emoji.Emotions = normalizedEmotions
		emoji.RegisterTime = &registerTime
		slog.Info("[register_emoji] Updated existing record and registered emoji", "hash", emoji.FileHash)
		return RegisterStatusRegistered
	}

	now := m.now()
	storedPath, pathErr := m.paths.Serialize(emoji.FullPath)
	if pathErr != nil {
		return RegisterStatusFailed
	}
	record := &EmojiRecord{
		ImageHash:    emoji.FileHash,
		Description:  emoji.Description,
		FullPath:     storedPath,
		RecordTime:   now,
		RegisterTime: &now,
		IsRegistered: true,
	}
	if err := m.store.Upsert(ctx, record); err != nil {
		slog.Error("[register_emoji] Failed to write database record", "err", err)
		return RegisterStatusFailed
	}
	emoji.RegisterTime = &now
	slog.Info("[register_emoji] Registered emoji to database", "hash", emoji.FileHash)
	return RegisterStatusRegistered
}

// DeleteEmoji mirrors delete_emoji：删除文件 +（keepDesc 时标记 no_file_flag）
// 记录。
func (m *Manager) DeleteEmoji(ctx context.Context, emoji *Emoji, keepDesc bool) bool {
	if emoji == nil {
		return false
	}
	if err := os.Remove(emoji.FullPath); err != nil && !os.IsNotExist(err) {
		slog.Error("[删除表情包] 删除表情包文件时出错", "err", err)
		return false
	}
	if m.store == nil {
		return true
	}
	record, err := m.store.GetByHash(ctx, emoji.FileHash)
	if err != nil {
		return false
	}
	if record == nil {
		slog.Warn("[删除表情包] 数据库中未找到表情包记录", "hash", emoji.FileHash)
		return true
	}
	if keepDesc {
		record.NoFileFlag = true
		if err := m.store.Upsert(ctx, record); err != nil {
			return false
		}
	} else if err := m.store.Delete(ctx, emoji.FileHash); err != nil {
		return false
	}
	m.removeFromPool(emoji.FileHash)
	return true
}

// BanEmoji mirrors ban_emoji：is_banned=True 并从内存列表移除。
func (m *Manager) BanEmoji(ctx context.Context, emoji *Emoji) bool {
	if emoji == nil || m.store == nil {
		return false
	}
	record, err := m.store.GetByHash(ctx, emoji.FileHash)
	if err != nil || record == nil {
		slog.Warn("[封禁表情包] 未找到表情包记录", "hash", emoji.FileHash)
		return false
	}
	record.IsBanned = true
	if err := m.store.Upsert(ctx, record); err != nil {
		return false
	}
	m.removeFromPool(emoji.FileHash)
	slog.Info("[封禁表情包] 成功封禁表情包", "hash", emoji.FileHash)
	return true
}

// UnregisterEmoji mirrors unregister_emoji：取消注册但保留文件和识别结果。
func (m *Manager) UnregisterEmoji(ctx context.Context, emoji *Emoji) bool {
	if emoji == nil || emoji.FileHash == "" || m.store == nil {
		return false
	}
	record, err := m.store.GetByHash(ctx, emoji.FileHash)
	if err != nil {
		return false
	}
	if record == nil {
		slog.Warn("[取消注册表情包] 未找到表情包记录", "hash", emoji.FileHash)
		return false
	}
	record.IsRegistered = false
	if err := m.store.Upsert(ctx, record); err != nil {
		return false
	}
	m.removeFromPool(emoji.FileHash)
	slog.Info("[取消注册表情包] 成功取消注册表情包", "hash", emoji.FileHash)
	return true
}

// UpdateEmojiUsageByHash mirrors update_emoji_usage_by_hash：记录一次真实发送。
func (m *Manager) UpdateEmojiUsageByHash(ctx context.Context, emojiHash string) bool {
	normalized := strings.TrimSpace(emojiHash)
	if normalized == "" || m.store == nil {
		return false
	}
	record, err := m.store.GetByHash(ctx, normalized)
	if err != nil || record == nil {
		slog.Error("[记录表情包使用] 未找到表情包记录", "hash", normalized)
		return false
	}
	record.QueryCount++
	now := m.now()
	record.LastUsedTime = &now
	if err := m.store.Upsert(ctx, record); err != nil {
		return false
	}
	m.mu.Lock()
	for _, emoji := range m.emojis {
		if emoji.FileHash == normalized {
			emoji.QueryCount = record.QueryCount
			emoji.LastUsedTime = &now
		}
	}
	m.mu.Unlock()
	return true
}

// UpdateEmoji mirrors update_emoji：更新情绪标签和描述。
func (m *Manager) UpdateEmoji(ctx context.Context, emoji *Emoji) bool {
	if emoji == nil || emoji.FileHash == "" || m.store == nil {
		return false
	}
	record, err := m.store.GetByHash(ctx, emoji.FileHash)
	if err != nil || record == nil {
		return false
	}
	record.Description = emoji.Description
	if err := m.store.Upsert(ctx, record); err != nil {
		return false
	}
	return true
}

// GetEmojiByHash mirrors get_emoji_by_hash（内存池查找）。
func (m *Manager) getEmojiByHash(emojiHash string) *Emoji {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, emoji := range m.emojis {
		if emoji.FileHash == emojiHash {
			return emoji
		}
	}
	return nil
}

// GetEmojiByHash 是公开形态。
func (m *Manager) GetEmojiByHash(emojiHash string) *Emoji { return m.getEmojiByHash(emojiHash) }

// GetEmojiByHashFromDB mirrors get_emoji_by_hash_from_db。
func (m *Manager) GetEmojiByHashFromDB(ctx context.Context, emojiHash string) *Emoji {
	if m.store == nil {
		return nil
	}
	record, err := m.store.GetByHash(ctx, emojiHash)
	if err != nil || record == nil || record.IsBanned {
		return nil
	}
	if record.NoFileFlag {
		slog.Warn("[数据库] 表情包记录标记为文件不存在", "hash", emojiHash)
		return nil
	}
	path, pathErr := m.paths.Resolve(record.FullPath)
	if pathErr != nil || !fileExists(path) {
		return nil
	}
	return m.emojiFromRecord(record, path)
}

func (m *Manager) removeFromPool(emojiHash string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	filtered := m.emojis[:0]
	for _, emoji := range m.emojis {
		if emoji.FileHash != emojiHash {
			filtered = append(filtered, emoji)
		}
	}
	m.emojis = filtered
	m.emojiNum = len(filtered)
}

// emojiFromRecord mirrors MaiEmoji.from_db_instance。
func (m *Manager) emojiFromRecord(record *EmojiRecord, absolutePath string) *Emoji {
	emotions := NormalizeEmojiTagText(record.Description)
	return &Emoji{
		FileHash:     record.ImageHash,
		FullPath:     absolutePath,
		FileName:     filepath.Base(absolutePath),
		ImageFormat:  strings.TrimPrefix(filepath.Ext(absolutePath), "."),
		Description:  record.Description,
		Emotions:     emotions,
		QueryCount:   record.QueryCount,
		RegisterTime: record.RegisterTime,
		LastUsedTime: record.LastUsedTime,
	}
}

func (m *Manager) recordFileExists(record *EmojiRecord) bool {
	if record.NoFileFlag {
		return false
	}
	path, err := m.paths.Resolve(record.FullPath)
	if err != nil {
		return false
	}
	return fileExists(path)
}

// ---------------------------------------------------------------------------
// 按情绪选择（旧逻辑）与替换决策
// ---------------------------------------------------------------------------

// CalculateEmotionSimilarity mirrors Levenshtein 归一化相似度
// （1 - distance / max(len)）。
func CalculateEmotionSimilarity(textEmotion, candidate string) float64 {
	normalized := toLower(trimSpace(textEmotion))
	candidateLower := toLower(trimSpace(candidate))
	if normalized == "" || candidateLower == "" {
		return 0
	}
	distance := levenshteinDistance(normalized, candidateLower)
	maxLen := len([]rune(normalized))
	if cl := len([]rune(candidateLower)); cl > maxLen {
		maxLen = cl
	}
	if maxLen == 0 {
		return 0
	}
	return 1 - float64(distance)/float64(maxLen)
}

// levenshteinDistance 是 Levenshtein 距离（对齐 rapidfuzz.distance.Levenshtein）。
func levenshteinDistance(s1, s2 string) int {
	r1, r2 := []rune(s1), []rune(s2)
	previous := make([]int, len(r2)+1)
	current := make([]int, len(r2)+1)
	for j := 0; j <= len(r2); j++ {
		previous[j] = j
	}
	for i := 1; i <= len(r1); i++ {
		current[0] = i
		for j := 1; j <= len(r2); j++ {
			insertions := previous[j] + 1
			deletions := current[j-1] + 1
			substitutions := previous[j-1]
			if r1[i-1] != r2[j-1] {
				substitutions++
			}
			current[j] = min3(insertions, deletions, substitutions)
		}
		previous, current = current, previous
	}
	return previous[len(r2)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// calculateEmotionSimilarityList mirrors _calculate_emotion_similarity_list。
func (m *Manager) calculateEmotionSimilarityList(textEmotion string) []*emojiSimilarity {
	normalized := toLower(trimSpace(textEmotion))
	if normalized == "" {
		return nil
	}
	m.mu.Lock()
	emojis := append([]*Emoji{}, m.emojis...)
	m.mu.Unlock()

	var result []*emojiSimilarity
	for _, emoji := range emojis {
		candidateEmotions := NormalizeEmojiTagText(emoji.Description)
		if len(candidateEmotions) == 0 {
			continue
		}
		best := 0.0
		hasCandidate := false
		for _, emotion := range candidateEmotions {
			if trimSpace(emotion) == "" {
				continue
			}
			similarity := CalculateEmotionSimilarity(normalized, emotion)
			if !hasCandidate || similarity > best {
				best = similarity
				hasCandidate = true
			}
		}
		if hasCandidate {
			result = append(result, &emojiSimilarity{emoji: emoji, similarity: best})
		}
	}
	return result
}

type emojiSimilarity struct {
	emoji      *Emoji
	similarity float64
}

// GetEmojiForEmotion mirrors get_emoji_for_emotion：按情绪标签相似度 Top10
// 随机选一个（旧逻辑出站路径）。
func (m *Manager) GetEmojiForEmotion(emotionLabel string) *Emoji {
	if m.EmojiNum() == 0 {
		slog.Warn("[获取表情包] 表情包列表为空")
		return nil
	}
	similarities := m.calculateEmotionSimilarityList(emotionLabel)
	if len(similarities) == 0 {
		slog.Info("[获取表情包] 未找到匹配的表情包")
		return nil
	}
	// heapq.nlargest(10, ...) 语义
	top := topN(similarities, 10, func(a, b *emojiSimilarity) bool { return a.similarity > b.similarity })
	selected := top[m.rng.Intn(len(top))]
	return selected.emoji
}

// topN 是 heapq.nlargest 的等价实现。
func topN[T any](items []T, n int, less func(a, b T) bool) []T {
	if n > len(items) {
		n = len(items)
	}
	result := append([]T{}, items...)
	// 简单部分排序：取前 n 大。
	for i := 0; i < n; i++ {
		best := i
		for j := i + 1; j < len(result); j++ {
			if less(result[j], result[best]) {
				best = j
			}
		}
		result[i], result[best] = result[best], result[i]
	}
	return result[:n]
}

// ReplaceAnEmojiByLLM mirrors replace_an_emoji_by_llm（emoji_replace.prompt
// 照抄；解析「不取消注册」/「取消注册编号X」）。
func (m *Manager) ReplaceAnEmojiByLLM(ctx context.Context, newEmoji *Emoji, sessionID string) bool {
	m.mu.Lock()
	emojis := append([]*Emoji{}, m.emojis...)
	emojiNum := m.emojiNum
	m.mu.Unlock()

	// random.choices(weights=1/(query_count+1), k=min(20, len))
	probabilities := make([]float64, len(emojis))
	for i, emoji := range emojis {
		probabilities[i] = 1.0 / float64(emoji.QueryCount+1)
	}
	total := 0.0
	for _, p := range probabilities {
		total += p
	}
	k := MaxEmojiForPrompt
	if k > len(emojis) {
		k = len(emojis)
	}
	selected := make([]*Emoji, 0, k)
	for i := 0; i < k && total > 0; i++ {
		pick := m.rng.Float64() * total
		acc := 0.0
		for j, p := range probabilities {
			acc += p
			if acc >= pick {
				selected = append(selected, emojis[j])
				probabilities[j] = 0
				total -= p
				break
			}
		}
	}
	if len(selected) == 0 {
		return false
	}

	var infoList []string
	for i, emoji := range selected {
		timeStr := "未知时间"
		if emoji.RegisterTime != nil {
			timeStr = emoji.RegisterTime.Format("2006-01-02 15:04:05")
		}
		infoList = append(infoList, fmt.Sprintf(
			"编号: %d\n描述: %s\n使用次数: %d\n添加时间: %s\n",
			i+1, emoji.Description, emoji.QueryCount, timeStr))
	}

	prompt := m.renderPromptOrDefault(ctx, "emoji_replace", map[string]string{
		"nickname":      "麦麦",
		"emoji_num":     strconv.Itoa(emojiNum),
		"emoji_num_max": strconv.Itoa(m.maxRegNum),
		"emoji_list":    strings.Join(infoList, ""),
		"description":   orDefault(newEmoji.Description, "无描述"),
	}, "emoji_replace")
	// nickname 覆盖：由装配层注入 bot 昵称；这里保持模板渲染语义。
	if m.textLLM == nil {
		return false
	}
	decision, err := m.textLLM.GenerateResponse(ctx, prompt, sessionID)
	if err != nil {
		slog.Error("[决策] 调用替换决策模型失败", "err", err)
		return false
	}
	slog.Info("[决策] 结果", "decision", decision)

	skipDecisions := []string{"不取消注册", "不删除", "do not unregister", "do not delete", "解除しない", "削除しない"}
	for _, skip := range skipDecisions {
		if strings.Contains(decision, skip) {
			slog.Info("[决策] 不取消注册任何表情包")
			return false
		}
	}
	pattern := regexp.MustCompile(`(?i)(?:取消注册|删除)编号(\d+)|(?:unregister|delete)\s+number\s+(\d+)|番号(\d+)を(?:解除|削除)`)
	match := pattern.FindStringSubmatch(decision)
	if match == nil {
		slog.Error("[决策] 未能解析取消注册编号")
		return false
	}
	matchedNumber := ""
	for _, group := range match[1:] {
		if group != "" {
			matchedNumber = group
			break
		}
	}
	emojiIndex, _ := strconv.Atoi(matchedNumber)
	emojiIndex-- // 0-based
	if emojiIndex < 0 || emojiIndex >= len(selected) {
		slog.Error("[决策] 无效的表情包编号", "index", emojiIndex+1)
		return false
	}
	toUnregister := selected[emojiIndex]
	slog.Info("[决策] 取消注册表情包", "desc", toUnregister.Description)
	if !m.UnregisterEmoji(ctx, toUnregister) {
		slog.Error("[错误] 取消注册表情包失败，无法完成替换")
		return false
	}
	registerStatus := m.RegisterEmojiToDB(ctx, newEmoji)
	if registerStatus == RegisterStatusRegistered {
		m.mu.Lock()
		m.emojis = append(m.emojis, newEmoji)
		m.emojiNum = len(m.emojis)
		m.mu.Unlock()
		slog.Info("[register_emoji] 成功替换并注册新表情包", "desc", newEmoji.Description)
		return true
	}
	if registerStatus == RegisterStatusSkipped {
		slog.Info("[register_emoji] Replacement emoji was already registered", "desc", newEmoji.Description)
	} else {
		slog.Error("[register_emoji] Failed to register replacement emoji", "desc", newEmoji.Description)
	}
	return false
}

// RegisterEmojiByFilename mirrors register_emoji_by_filename：注册 data/emoji
// 下的表情文件（VLM 描述 → 审核 → 达 max_reg_num 且 do_replace 时替换）。
// doReplace / needReview 由装配层按配置传入（steal_emoji 路径）。
func (m *Manager) RegisterEmojiByFilename(ctx context.Context, filename string, doReplace bool, needReview bool) RegisterStatus {
	absolutePath, err := filepath.Abs(filename)
	if err != nil || !fileExists(absolutePath) {
		slog.Error("[register_emoji] Emoji file does not exist", "path", filename)
		return RegisterStatusFailed
	}
	rawBytes, err := os.ReadFile(absolutePath)
	if err != nil {
		slog.Error("[register_emoji] Failed to read emoji file", "err", err)
		return RegisterStatusFailed
	}
	if !m.isCollectedEmojiSizeAllowed(len(rawBytes)) {
		slog.Info("[register_emoji] Emoji file exceeds size limit, skipping", "name", filepath.Base(absolutePath))
		return RegisterStatusFailed
	}
	hash := sha256Hex(rawBytes)

	if m.store != nil {
		existing, err := m.store.GetByHash(ctx, hash)
		if err != nil {
			return RegisterStatusFailed
		}
		if existing != nil {
			if existing.IsBanned {
				slog.Info("[register_emoji] Emoji is banned, skipping", "hash", hash)
				return RegisterStatusSkipped
			}
			if existing.IsRegistered && !existing.NoFileFlag {
				if path, pathErr := m.paths.Resolve(existing.FullPath); pathErr == nil && fileExists(path) {
					return RegisterStatusSkipped
				}
			}
		}
		if m.getEmojiByHash(hash) != nil {
			return RegisterStatusSkipped
		}
	}

	target := &Emoji{
		FileHash:    hash,
		FullPath:    absolutePath,
		FileName:    filepath.Base(absolutePath),
		ImageFormat: strings.TrimPrefix(filepath.Ext(absolutePath), "."),
	}

	// 已有描述缓存则复用
	if m.store != nil {
		if existing, _ := m.store.GetByHash(ctx, hash); existing != nil && strings.TrimSpace(existing.Description) != "" {
			normalized := NormalizeEmojiTagText(existing.Description)
			target.Description = strings.Join(normalized, ",")
			target.Emotions = normalized
			target.QueryCount = existing.QueryCount
			target.LastUsedTime = existing.LastUsedTime
			target.RegisterTime = existing.RegisterTime
		}
	}
	if m.getEmojiByHash(hash) != nil {
		return RegisterStatusSkipped
	}

	if target.Description == "" {
		description, updated := m.BuildEmojiDescription(ctx, target, "")
		if description == "" {
			slog.Error("[register_emoji] Failed to build emoji description", "file", absolutePath)
			return RegisterStatusFailed
		}
		target = updated
		if m.store != nil {
			if record, _ := m.store.GetByHash(ctx, hash); record != nil {
				record.VLMProcessed = true
				record.NoFileFlag = false
				_ = m.store.Upsert(ctx, record)
			}
		}
	}
	if needReview && !m.ReviewEmojiForRegistration(ctx, target, "") {
		if m.store != nil {
			if record, _ := m.store.GetByHash(ctx, hash); record != nil {
				record.VLMProcessed = true
				_ = m.store.Upsert(ctx, record)
			}
		}
		slog.Error("[register_emoji] Emoji did not pass content review", "file", absolutePath)
		return RegisterStatusFailed
	}

	if m.EmojiNum() >= m.maxRegNum && doReplace {
		slog.Warn("[register_emoji] Emoji limit reached, trying replacement", "max", m.maxRegNum)
		if !m.ReplaceAnEmojiByLLM(ctx, target, "") {
			return RegisterStatusFailed
		}
		return RegisterStatusRegistered
	}

	registerStatus := m.RegisterEmojiToDB(ctx, target)
	if registerStatus == RegisterStatusRegistered {
		m.mu.Lock()
		m.emojis = append(m.emojis, target)
		m.emojiNum = len(m.emojis)
		m.mu.Unlock()
		slog.Info("[register_emoji] Registered new emoji", "file", target.FileName)
	}
	return registerStatus
}

// CheckEmojiFileIntegrity mirrors check_emoji_file_integrity：维护数据库记录
// 与内存可发送池，不主动删除 data/emoji 下的图片文件。
func (m *Manager) CheckEmojiFileIntegrity(ctx context.Context) {
	if m.store == nil {
		return
	}
	records, err := m.store.ListAll(ctx)
	if err != nil {
		slog.Error("[完整性检查] 读取记录失败", "err", err)
		return
	}
	available := make([]*Emoji, 0)
	removed := 0
	for _, record := range records {
		path, pathErr := m.paths.Resolve(record.FullPath)
		if record.NoFileFlag || pathErr != nil || !fileExists(path) {
			if record.IsRegistered {
				slog.Warn("[完整性检查] 已注册表情包缺少实际文件，删除破损注册记录", "path", record.FullPath)
				_ = m.store.Delete(ctx, record.ImageHash)
				removed++
			}
			continue
		}
		if !record.IsRegistered || record.IsBanned {
			continue
		}
		available = append(available, m.emojiFromRecord(record, path))
	}
	m.mu.Lock()
	m.emojis = available
	m.emojiNum = len(available)
	m.mu.Unlock()
	slog.Info("[完整性检查] 完成", "removed", removed, "available", len(available))
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func orTime(value *time.Time, fallback time.Time) time.Time {
	if value != nil {
		return *value
	}
	return fallback
}

func emojiBytesOf(m *Manager, target *Emoji) []byte {
	raw, err := os.ReadFile(target.FullPath)
	if err != nil {
		return nil
	}
	return raw
}
