// .meta.toml 读取 — 对齐 参考MaiBot/src/common/prompt_i18n.py
// 的 _read_metadata_file / _coerce_metadata / _load_prompt_metadata。
//
// MaiBot 的 prompts/zh-CN/.meta.toml 是「单文件多表」形态：
// 每个 [prompt_name] 表携带 display_name / advanced / description。
package prompt

import (
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml/v2"
)

// loadMetadataFile 读取一个 .meta.toml 并抽取指定 prompt 的元数据。
func loadMetadataFile(path string) (PromptMetadata, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return PromptMetadata{}, false
	}
	var parsed map[string]PromptMetadata
	if err := toml.Unmarshal(raw, &parsed); err != nil {
		return PromptMetadata{}, false
	}
	meta, ok := parsed[filepath.Base(path)]
	return meta, ok
}

// LoadMetadataTable 读取整个 .meta.toml（prompt 名 → 元数据），供 WebUI 使用。
func LoadMetadataTable(path string) (map[string]PromptMetadata, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var parsed map[string]PromptMetadata
	if err := toml.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// MetaFileName 返回指定 prompt 的 .meta.toml 文件名。
func MetaFileName(promptName string) string {
	return promptName + ".meta.toml"
}
