// Version 常量 — 对齐 参考MaiBot/src/config/config.py 的 CONFIG_VERSION / MODEL_CONFIG_VERSION。
// MMC_VERSION（程序版本）在 Go 版由 internal.Version 承担。
package config

// ConfigVersion 对齐 config.py: CONFIG_VERSION = "8.14.62"。
const ConfigVersion = "8.14.62"

// ModelConfigVersion 对齐 config.py: MODEL_CONFIG_VERSION = "1.17.15"。
const ModelConfigVersion = "1.17.15"

// CompareVersions 对齐 config_utils.compare_versions：
// 逐段转 int 比较，返回 new > old 是否成立（用于判断是否需要回写升级后的配置）。
func CompareVersions(oldVer, newVer string) bool {
	oldParts, okOld := parseVersionParts(oldVer)
	newParts, okNew := parseVersionParts(newVer)
	if !okOld || !okNew {
		return false
	}
	n := len(oldParts)
	if len(newParts) < n {
		n = len(newParts)
	}
	for i := 0; i < n; i++ {
		if newParts[i] != oldParts[i] {
			return newParts[i] > oldParts[i]
		}
	}
	return len(newParts) > len(oldParts)
}

func parseVersionParts(ver string) ([]int, bool) {
	if ver == "" {
		return nil, false
	}
	parts := splitNonEmpty(ver, '.')
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		v := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				return nil, false
			}
			v = v*10 + int(r-'0')
		}
		out = append(out, v)
	}
	return out, true
}

func splitNonEmpty(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == sep {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}
