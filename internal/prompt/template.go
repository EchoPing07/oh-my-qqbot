// 模板文件发现工具 — 对齐 参考MaiBot/src/common/prompt_i18n.py 的
// discover_prompt_locales / iter_prompt_files。
package prompt

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DiscoverLocales 对齐 discover_prompt_locales：列出 prompts 根目录下的语言目录。
func DiscoverLocales(promptsRoot string) []string {
	entries, err := os.ReadDir(promptsRoot)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// IterPromptFiles 对齐 iter_prompt_files：递归列出目录下全部 .prompt 文件。
func IterPromptFiles(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), SuffixPrompt) {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out
}
