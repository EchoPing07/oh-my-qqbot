package image

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file mirrors 参考/MaiBot/src/chat/image_system/image_cache_cleanup.py:
// 图片缓存自动清理（三段清理，行为对齐）：
//  1. tracked files：库内记录的图片文件，超过保留期删除文件并标记 no_file_flag
//  2. orphan files：data/images 下未被库跟踪且超过保留期的孤立文件删除
//  3. no-file records：no_file_flag 记录超过保留期删除记录（识别结果）
//
// 保留期对齐 Python 语义：天数 <1 时取 1 天。

// cacheImageExtensions mirrors _CACHE_IMAGE_EXTENSIONS。
var cacheImageExtensions = map[string]bool{
	".avif": true, ".bmp": true, ".gif": true, ".jpeg": true,
	".jpg": true, ".png": true, ".webp": true,
}

// CleanupBatchSize mirrors _CLEANUP_BATCH_SIZE = 1000。
const CleanupBatchSize = 1000

// CleanupConfig 对齐 ImageCacheCleanupConfigLike。
type CleanupConfig struct {
	// Enabled 对齐 enabled（false 时 periodic 循环不执行清理）。
	Enabled bool
	// ImageFileRetentionDays 对齐 image_file_retention_days。
	ImageFileRetentionDays int
	// NoFileResultRetentionDays 对齐 no_file_result_retention_days。
	NoFileResultRetentionDays int
}

// CleanupResult mirrors ImageCacheCleanupResult。
type CleanupResult struct {
	RemovedFiles        int
	RemovedBytes        int64
	MarkedNoFileRecords int
	RemovedOrphanFiles  int
	RemovedOrphanBytes  int64
	RemovedRecords      int
	RestoredFileFlags   int
	SkippedOutsideCache int
}

// Changed mirrors the `changed` property.
func (r *CleanupResult) Changed() bool {
	return r.RemovedFiles != 0 || r.RemovedBytes != 0 || r.MarkedNoFileRecords != 0 ||
		r.RemovedOrphanFiles != 0 || r.RemovedOrphanBytes != 0 || r.RemovedRecords != 0 ||
		r.RestoredFileFlags != 0
}

// recordReferenceTime mirrors _record_reference_time: last_used_time 或 record_time。
func recordReferenceTime(record *ImageRecord, fallback time.Time) time.Time {
	if record.LastUsedTime != nil && !record.LastUsedTime.IsZero() {
		return *record.LastUsedTime
	}
	if !record.RecordTime.IsZero() {
		return record.RecordTime
	}
	return fallback
}

// resolveCacheImagePath mirrors _resolve_cache_image_cache_path：路径必须在
// 图片缓存目录内且扩展名属于图片集合。
func (m *ImageManager) resolveCacheImagePath(storedPath string) (string, bool) {
	if strings.TrimSpace(storedPath) == "" {
		return "", false
	}
	path, err := m.paths.Resolve(storedPath)
	if err != nil {
		return "", false
	}
	ext := strings.ToLower(filepath.Ext(path))
	if !cacheImageExtensions[ext] {
		return "", false
	}
	absDir, err := filepath.Abs(m.imageDir)
	if err != nil {
		return "", false
	}
	if !withinDir(absDir, path) {
		return "", false
	}
	return path, true
}

func withinDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && rel != "." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// cleanupTrackedImageFiles mirrors _cleanup_tracked_image_files。
func (m *ImageManager) cleanupTrackedImageFiles(ctx context.Context, now time.Time, cutoff time.Time) *CleanupResult {
	result := &CleanupResult{}
	if m.store == nil {
		return result
	}
	records, err := m.store.ListAll(ctx)
	if err != nil {
		return result
	}
	processed := 0
	for _, record := range records {
		if processed >= CleanupBatchSize {
			break
		}
		cachePath, isCachePath := m.resolveCacheImagePath(record.FullPath)
		path, pathErr := m.paths.Resolve(record.FullPath)
		fileExistsOnDisk := pathErr == nil && fileExists(path)

		if record.NoFileFlag {
			if fileExistsOnDisk {
				record.NoFileFlag = false
				_ = m.store.Upsert(ctx, record)
				result.RestoredFileFlags++
				processed++
			}
			continue
		}
		if !fileExistsOnDisk {
			record.NoFileFlag = true
			_ = m.store.Upsert(ctx, record)
			result.MarkedNoFileRecords++
			processed++
			continue
		}
		if recordReferenceTime(record, now).After(cutoff) {
			continue
		}
		if !isCachePath {
			result.SkippedOutsideCache++
			continue
		}
		info, statErr := os.Stat(cachePath)
		if statErr != nil {
			continue
		}
		if err := os.Remove(cachePath); err != nil {
			continue
		}
		record.NoFileFlag = true
		_ = m.store.Upsert(ctx, record)
		result.RemovedFiles++
		result.RemovedBytes += info.Size()
		result.MarkedNoFileRecords++
		processed++
	}
	return result
}

// cleanupOrphanImageFiles mirrors _cleanup_orphan_image_files。
func (m *ImageManager) cleanupOrphanImageFiles(ctx context.Context, cutoff time.Time) *CleanupResult {
	result := &CleanupResult{}
	info, err := os.Stat(m.imageDir)
	if err != nil || !info.IsDir() {
		return result
	}
	tracked := m.trackedCachePaths(ctx)
	cutoffTimestamp := cutoff.Unix()

	_ = filepath.WalkDir(m.imageDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if result.RemovedOrphanFiles >= CleanupBatchSize {
			return fs.SkipAll
		}
		if !cacheImageExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		if stat, statErr := d.Info(); statErr != nil {
			return nil
		} else if stat.ModTime().Unix() > cutoffTimestamp {
			return nil
		}
		if tracked[normalizePath(path)] {
			return nil
		}
		if stat, statErr := d.Info(); statErr == nil {
			if err := os.Remove(path); err != nil {
				return nil
			}
			result.RemovedOrphanFiles++
			result.RemovedOrphanBytes += stat.Size()
		}
		return nil
	})
	return result
}

// trackedCachePaths mirrors _get_tracked_cache_paths。
func (m *ImageManager) trackedCachePaths(ctx context.Context) map[string]bool {
	paths := map[string]bool{}
	if m.store == nil {
		return paths
	}
	records, err := m.store.ListAll(ctx)
	if err != nil {
		return paths
	}
	for _, record := range records {
		if path, ok := m.resolveCacheImagePath(record.FullPath); ok {
			paths[normalizePath(path)] = true
		}
	}
	return paths
}

func normalizePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

// cleanupNoFileResults mirrors _cleanup_no_file_results。
func (m *ImageManager) cleanupNoFileResults(ctx context.Context, now time.Time, cutoff time.Time) *CleanupResult {
	result := &CleanupResult{}
	if m.store == nil {
		return result
	}
	records, err := m.store.ListAll(ctx)
	if err != nil {
		return result
	}
	processed := 0
	for _, record := range records {
		if processed >= CleanupBatchSize {
			break
		}
		path, pathErr := m.paths.Resolve(record.FullPath)
		fileExistsOnDisk := pathErr == nil && fileExists(path)

		if fileExistsOnDisk {
			if record.NoFileFlag {
				record.NoFileFlag = false
				_ = m.store.Upsert(ctx, record)
				result.RestoredFileFlags++
				processed++
			}
			continue
		}
		if !record.NoFileFlag {
			record.NoFileFlag = true
			_ = m.store.Upsert(ctx, record)
			result.MarkedNoFileRecords++
			processed++
			continue
		}
		if recordReferenceTime(record, now).After(cutoff) {
			continue
		}
		if err := m.store.Delete(ctx, record.ImageHash); err == nil {
			result.RemovedRecords++
			processed++
		}
	}
	return result
}

// RunCacheCleanup mirrors run_image_cache_cleanup：执行一次图片缓存清理。
func (m *ImageManager) RunCacheCleanup(ctx context.Context, config CleanupConfig) *CleanupResult {
	now := m.now()
	fileRetentionDays := config.ImageFileRetentionDays
	if fileRetentionDays < 1 {
		fileRetentionDays = 1
	}
	fileCutoff := now.AddDate(0, 0, -fileRetentionDays)

	result := m.cleanupTrackedImageFiles(ctx, now, fileCutoff)
	merged := m.cleanupOrphanImageFiles(ctx, fileCutoff)
	result.RemovedOrphanFiles += merged.RemovedOrphanFiles
	result.RemovedOrphanBytes += merged.RemovedOrphanBytes

	resultRetentionDays := config.NoFileResultRetentionDays
	if resultRetentionDays < 1 {
		resultRetentionDays = 1
	}
	resultCutoff := now.AddDate(0, 0, -resultRetentionDays)
	mergedRecords := m.cleanupNoFileResults(ctx, now, resultCutoff)
	result.RemovedRecords += mergedRecords.RemovedRecords
	result.MarkedNoFileRecords += mergedRecords.MarkedNoFileRecords
	result.RestoredFileFlags += mergedRecords.RestoredFileFlags

	return result
}

// IntervalSeconds mirrors _interval_seconds：小时转秒，下限 60s。
func IntervalSeconds(checkIntervalHours float64) float64 {
	seconds := checkIntervalHours * 3600.0
	if seconds < 60.0 {
		return 60.0
	}
	return seconds
}
