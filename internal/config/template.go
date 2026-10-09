// 模板文件生成 — 对齐 参考MaiBot/src/config/config.py generate_new_config_file：
// 在配置目录缺失 bot_config.toml / model_config.toml 时生成默认模板。
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnsureTemplateFiles 确保配置目录存在两个配置模板文件（已存在则跳过）。
// 返回是否发生了生成。等价于 generate_new_config_file(Config/ModelConfig, ...)。
func EnsureTemplateFiles(configDir string) (bool, error) {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return false, err
	}
	generated := false
	botPath := filepath.Join(configDir, "bot_config.toml")
	if _, err := os.Stat(botPath); os.IsNotExist(err) {
		if err := writeConfigToPath(botPath, DefaultConfig(), ConfigVersion); err != nil {
			return generated, fmt.Errorf("生成 bot_config.toml 失败: %w", err)
		}
		generated = true
	}
	modelPath := filepath.Join(configDir, "model_config.toml")
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		if err := writeConfigToPath(modelPath, DefaultModelConfig(), ModelConfigVersion); err != nil {
			return generated, fmt.Errorf("生成 model_config.toml 失败: %w", err)
		}
		generated = true
	}
	return generated, nil
}
