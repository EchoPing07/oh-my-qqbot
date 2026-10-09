package image

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func buildPNG(t *testing.T, w, h int, clr color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, clr)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fakeVLM struct {
	configured bool
	desc       string
	calls      int
}

func (f *fakeVLM) GenerateResponseForImage(ctx context.Context, req VLMRequest) (string, error) {
	f.calls++
	return f.desc, nil
}

func (f *fakeVLM) Configured() bool { return f.configured }

type memStore struct {
	records map[string]*ImageRecord
	deleted []string
}

func newMemStore() *memStore { return &memStore{records: map[string]*ImageRecord{}} }

func (s *memStore) GetByHash(ctx context.Context, hash string) (*ImageRecord, error) {
	if record := s.records[hash]; record != nil {
		cloned := *record
		return &cloned, nil
	}
	return nil, nil
}

func (s *memStore) Upsert(ctx context.Context, record *ImageRecord) error {
	cloned := *record
	s.records[record.ImageHash] = &cloned
	return nil
}

func (s *memStore) Delete(ctx context.Context, hash string) error {
	delete(s.records, hash)
	s.deleted = append(s.deleted, hash)
	return nil
}

func (s *memStore) ListAll(ctx context.Context) ([]*ImageRecord, error) {
	out := make([]*ImageRecord, 0, len(s.records))
	for _, record := range s.records {
		cloned := *record
		out = append(out, &cloned)
	}
	return out, nil
}

type passthroughResolver struct{ root string }

func (r passthroughResolver) Resolve(storedPath string) (string, error) {
	if filepath.IsAbs(storedPath) {
		return storedPath, nil
	}
	return filepath.Join(r.root, storedPath), nil
}

func (r passthroughResolver) Serialize(absolutePath string) (string, error) {
	return absolutePath, nil
}

func newTestManager(t *testing.T, vlm VLMClient) (*ImageManager, *memStore) {
	t.Helper()
	root := t.TempDir()
	imageDir := filepath.Join(root, "data", "images")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	manager := NewImageManager(Options{
		Store:     store,
		Paths:     passthroughResolver{root: root},
		VLM:       vlm,
		ImageDir:  imageDir,
		MaxSizeMB: 1,
	})
	return manager, store
}

// sha256 保存：文件以 hash 命名、记录入库、重复保存命中缓存并增加计数。
func TestEnsureImageSavedSHA256(t *testing.T) {
	manager, store := newTestManager(t, &fakeVLM{configured: true})
	raw := buildPNG(t, 32, 32, color.RGBA{9, 9, 9, 255})

	record, path, err := manager.EnsureImageSaved(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if record.ImageHash != SHA256Hex(raw) || len(record.ImageHash) != 64 {
		t.Fatalf("应计算 sha256: %s", record.ImageHash)
	}
	if !strings.HasPrefix(filepath.Base(path), record.ImageHash) {
		t.Fatalf("文件应以 hash 命名: %s", path)
	}
	if filepath.Ext(path) != ".png" {
		t.Fatalf("扩展名应修正为 png: %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("文件应存在: %v", err)
	}
	if store.records[record.ImageHash] == nil {
		t.Fatal("记录应入库")
	}

	// 重复保存命中缓存，query_count 增加。
	again, _, err := manager.EnsureImageSaved(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if again.QueryCount != 1 {
		t.Fatalf("重复保存应增加 query_count: %d", again.QueryCount)
	}
}

// VLM 描述（image_description.prompt 路径）：保存 → 描述 → vlm_processed。
func TestGetImageDescriptionWithVLM(t *testing.T) {
	vlm := &fakeVLM{configured: true, desc: "一只猫在桌子上趴着"}
	manager, _ := newTestManager(t, vlm)
	raw := buildPNG(t, 32, 32, color.RGBA{1, 2, 3, 255})

	desc, err := manager.GetImageDescription(context.Background(), "", raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if desc != "一只猫在桌子上趴着" {
		t.Fatalf("描述不符: %q", desc)
	}
	hash := SHA256Hex(raw)
	cached := manager.GetCachedImageDescription(context.Background(), hash)
	if cached != desc {
		t.Fatalf("缓存读取不符: %q", cached)
	}
	// 二次请求命中缓存，不再调用 VLM。
	calls := vlm.calls
	desc2, err := manager.GetImageDescription(context.Background(), hash, raw, true)
	if err != nil || desc2 != desc {
		t.Fatalf("缓存命中失败: %q %v", desc2, err)
	}
	if vlm.calls != calls {
		t.Fatal("缓存命中不应再次调用 VLM")
	}
}

// 未配置 VLM 时保存但不生成描述。
func TestGetImageDescriptionWithoutVLM(t *testing.T) {
	manager, _ := newTestManager(t, &fakeVLM{configured: false})
	raw := buildPNG(t, 16, 16, color.RGBA{0, 0, 0, 255})
	desc, err := manager.GetImageDescription(context.Background(), "", raw, true)
	if err != nil || desc != "" {
		t.Fatalf("未配置 VLM 应返回空描述: %q %v", desc, err)
	}
}

// 只有 hash 没有字节时返回空。
func TestDescriptionByHashOnly(t *testing.T) {
	manager, _ := newTestManager(t, &fakeVLM{configured: true})
	desc, err := manager.GetImageDescription(context.Background(), SHA256Hex([]byte("missing")), nil, true)
	if err != nil || desc != "" {
		t.Fatalf("未找到记录应返回空: %q %v", desc, err)
	}
}

// 缓存清理：过期文件删除并标记 no_file_flag；孤立文件删除；过期记录删除。
func TestCacheCleanup(t *testing.T) {
	vlm := &fakeVLM{configured: false}
	manager, store := newTestManager(t, vlm)
	now := time.Now()

	// 1. 过期的 tracked 文件（record_time 很久以前）。
	oldBytes := buildPNG(t, 8, 8, color.RGBA{1, 0, 0, 255})
	oldRecord, oldPath, err := manager.EnsureImageSaved(context.Background(), oldBytes)
	if err != nil {
		t.Fatal(err)
	}
	oldRecord.RecordTime = now.AddDate(0, 0, -40)
	_ = store.Upsert(context.Background(), oldRecord)

	// 2. 新 tracked 文件（不清理）。
	newBytes := buildPNG(t, 8, 8, color.RGBA{0, 1, 0, 255})
	newRecord, newPath, err := manager.EnsureImageSaved(context.Background(), newBytes)
	if err != nil {
		t.Fatal(err)
	}
	newRecord.RecordTime = now
	_ = store.Upsert(context.Background(), newRecord)

	// 3. 孤立文件（不在库里、mtime 很旧）。
	orphanPath := filepath.Join(manager.imageDir, "orphan.png")
	if err := os.WriteFile(orphanPath, buildPNG(t, 4, 4, color.RGBA{0, 0, 1, 255}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphanPath, now.AddDate(0, 0, -40), now.AddDate(0, 0, -40)); err != nil {
		t.Fatal(err)
	}
	// 新孤立文件不清理。
	freshOrphan := filepath.Join(manager.imageDir, "fresh.png")
	if err := os.WriteFile(freshOrphan, buildPNG(t, 4, 4, color.RGBA{0, 0, 2, 255}), 0o644); err != nil {
		t.Fatal(err)
	}

	// 4. 过期的 no_file 记录。
	staleRecord := &ImageRecord{
		ImageHash:  SHA256Hex([]byte("stale")),
		FullPath:   filepath.Join(manager.imageDir, "missing.png"),
		NoFileFlag: true,
		RecordTime: now.AddDate(0, 0, -40),
	}
	_ = store.Upsert(context.Background(), staleRecord)

	result := manager.RunCacheCleanup(context.Background(), CleanupConfig{
		Enabled:                   true,
		ImageFileRetentionDays:    30,
		NoFileResultRetentionDays: 30,
	})
	_ = now

	if result.RemovedFiles != 1 || result.RemovedBytes <= 0 {
		t.Fatalf("应删除 1 个过期文件: %+v", result)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("过期文件应被删除")
	}
	if oldRecord.ImageHash == newRecord.ImageHash {
		t.Fatal("测试数据冲突")
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatal("新文件不应被删除")
	}
	if result.RemovedOrphanFiles != 1 {
		t.Fatalf("应删除 1 个孤立文件: %+v", result)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatal("孤立文件应被删除")
	}
	if _, err := os.Stat(freshOrphan); err != nil {
		t.Fatal("新孤立文件不应被删除")
	}
	if result.RemovedRecords < 1 {
		t.Fatalf("应删除过期 no_file 记录（含过期文件记录）: %+v", result)
	}
	if _, ok := store.records[staleRecord.ImageHash]; ok {
		t.Fatal("过期记录应被删除")
	}
	// 清理后：过期文件的记录先标记 no_file_flag，再在 no-file 阶段按保留期删除
	//（对齐 Python 两阶段语义）→ 记录已被删除。
	after, _ := store.GetByHash(context.Background(), oldRecord.ImageHash)
	if after != nil && !after.NoFileFlag {
		t.Fatalf("过期文件记录应被删除或标记 no_file_flag: %+v", after)
	}
}

// 缓存清理的恢复语义：no_file 记录文件又出现时恢复。
func TestCacheCleanupRestoresFileFlags(t *testing.T) {
	manager, store := newTestManager(t, &fakeVLM{configured: false})
	raw := buildPNG(t, 8, 8, color.RGBA{2, 2, 2, 255})
	record, path, err := manager.EnsureImageSaved(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	record.NoFileFlag = true
	_ = store.Upsert(context.Background(), record)

	result := manager.RunCacheCleanup(context.Background(), CleanupConfig{
		Enabled: true, ImageFileRetentionDays: 30, NoFileResultRetentionDays: 30,
	})
	if result.RestoredFileFlags != 1 {
		t.Fatalf("应恢复 1 条记录: %+v", result)
	}
	after, _ := store.GetByHash(context.Background(), record.ImageHash)
	if after == nil || after.NoFileFlag {
		t.Fatal("记录应恢复为有文件")
	}
	_ = path
}

func TestIntervalSeconds(t *testing.T) {
	if got := IntervalSeconds(0); got != 60 {
		t.Fatalf("下限应为 60s: %v", got)
	}
	if got := IntervalSeconds(2); got != 7200 {
		t.Fatalf("2 小时应为 7200s: %v", got)
	}
}

func TestDetectImageFormat(t *testing.T) {
	if got := DetectImageFormat(buildPNG(t, 4, 4, color.RGBA{0, 0, 0, 255})); got != "png" {
		t.Fatalf("PNG 探测失败: %q", got)
	}
	if got := DetectImageFormat([]byte("not an image")); got != "" {
		t.Fatalf("非图片应返回空: %q", got)
	}
}
