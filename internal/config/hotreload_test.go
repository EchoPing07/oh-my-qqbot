// B3 回归测试：tmp+rename 原子写后热重载仍生效（watch 配置目录）。
package config

import (
	"os"
	"testing"
	"time"
)

// TestHotReloadSurvivesTmpRenameAtomicWrite 回归 B3：编辑器/程序常用
// 「写临时文件 + rename」原子保存，逐文件 watch 在 Linux 下会指旧 inode；
// 改为 watch 配置目录后，rename 进来的新文件必须仍触发回调。
func TestHotReloadSurvivesTmpRenameAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureTemplateFiles(dir); err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir)
	if err := m.Initialize(); err != nil {
		t.Fatal(err)
	}

	reloaded := make(chan []string, 8)
	m.RegisterReloadCallback(func(scopes []string, _ []string) {
		reloaded <- scopes
	})
	if err := m.StartWatching(); err != nil {
		t.Fatalf("StartWatching: %v", err)
	}
	t.Cleanup(m.StopWatching)

	// tmp + rename 原子写 bot_config.toml（模拟编辑器保存形态）。
	path := m.BotConfigPath()
	cfg := m.GetGlobalConfig()
	cfg.WebUI.Port = 18099
	tmp := path + ".tmp"
	if err := writeConfigToPath(tmp, cfg, ConfigVersion); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	select {
	case scopes := <-reloaded:
		hit := false
		for _, s := range scopes {
			if s == "bot" {
				hit = true
			}
		}
		if !hit {
			t.Fatalf("回调应含 bot 作用域: %v", scopes)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("tmp+rename 原子写后 8s 内未触发热重载回调（B3 回归失败）")
	}

	// 重载后的内存配置应反映新端口（防抖后读盘成功）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.GetGlobalConfig().WebUI.Port == 18099 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("热重载后内存配置未更新: port=%d", m.GetGlobalConfig().WebUI.Port)
}
