// S10 .tmp 残留清理测试 — 格式识别失败不留 .tmp；完整性检查清历史残留。
package emoji

import (
	"context"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// emojiDirOf 从测试管理器推导 emoji 目录（newTestManager 约定 root/data/emoji）。
func emojiDirOf(root string) string {
	return filepath.Join(root, "data", "emoji")
}

// listTmpFiles 列出 emoji 目录下的 .tmp 文件。
func listTmpFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".tmp" {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestEnsureEmojiSavedUndecodableNoTmpResidue 格式识别失败：报错、不落 .tmp、不入库。
func TestEnsureEmojiSavedUndecodableNoTmpResidue(t *testing.T) {
	manager, store, root := newTestManager(t, nil, nil)
	_, err := manager.EnsureEmojiSaved(context.Background(), []byte("not-an-image"), "deadbeefdeadbeef")
	if err == nil {
		t.Fatalf("无法识别的字节应报错")
	}
	if files := listTmpFiles(t, emojiDirOf(root)); len(files) != 0 {
		t.Fatalf("失败后不应残留 .tmp 文件: %v", files)
	}
	if _, ok := store.records["deadbeefdeadbeef"]; ok {
		t.Fatalf("失败后不应入库记录")
	}
}

// TestEnsureEmojiSavedTmpNameHasRandomSuffix 成功路径：临时文件名带随机后缀
// （防同 hash 并发互踩），最终落盘为 hash.<ext>。
func TestEnsureEmojiSavedTmpNameHasRandomSuffix(t *testing.T) {
	manager, _, root := newTestManager(t, nil, nil)
	dir := emojiDirOf(root)
	// 直接观察两次随机后缀不同（randomHexSuffix 的基本性质）。
	a, b := randomHexSuffix(), randomHexSuffix()
	if a == b || len(a) != 8 {
		t.Fatalf("随机后缀应 8 位且不重复: %q %q", a, b)
	}
	if _, err := manager.EnsureEmojiSaved(context.Background(), buildTestPNG(t, 8, 8, color.RGBA{10, 20, 30, 255}), ""); err != nil {
		t.Fatalf("正常保存应成功: %v", err)
	}
	if files := listTmpFiles(t, dir); len(files) != 0 {
		t.Fatalf("成功保存后不应有 .tmp: %v", files)
	}
}

// TestCheckEmojiFileIntegrityRemovesTmpRecords 完整性检查删除历史 .tmp 记录与文件。
func TestCheckEmojiFileIntegrityRemovesTmpRecords(t *testing.T) {
	manager, store, root := newTestManager(t, nil, nil)
	dir := emojiDirOf(root)
	tmpPath := filepath.Join(dir, "deadbeefdeadbeef.tmp")
	if err := os.WriteFile(tmpPath, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	store.records["deadbeefdeadbeef"] = &EmojiRecord{
		ImageHash: "deadbeefdeadbeef", FullPath: tmpPath, IsRegistered: false,
	}
	manager.CheckEmojiFileIntegrity(context.Background())
	if _, ok := store.records["deadbeefdeadbeef"]; ok {
		t.Fatalf(".tmp 记录应被删除")
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf(".tmp 文件应被删除")
	}
}

// TestCheckEmojiFileIntegrityRemovesOrphanTmpFiles 完整性检查清理孤儿 .tmp
// （仅 ModTime 超过 1 小时的，避开进行中的写入）。
func TestCheckEmojiFileIntegrityRemovesOrphanTmpFiles(t *testing.T) {
	manager, _, root := newTestManager(t, nil, nil)
	dir := emojiDirOf(root)

	stale := filepath.Join(dir, "cafebabe.abcdef01.tmp")
	if err := os.WriteFile(stale, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "cafebabe.abcdef02.tmp")
	if err := os.WriteFile(fresh, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	manager.CheckEmojiFileIntegrity(context.Background())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("过期孤儿 .tmp 应被删除")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("新鲜 .tmp（可能正在写入）不应被删除")
	}
}
