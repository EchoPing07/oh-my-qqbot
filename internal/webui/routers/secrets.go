// 密钥脱敏辅助 — S2 修复：GET /config/bot、/config/model 与导出接口此前
// 明文回显 bot.app_secret 与各 provider 的 api_key（与 model.go ProviderView
// 的脱敏行为不一致，同一信息两处暴露面不同）。
//
// 策略（对齐前端 Setup.tsx「留空保留旧值」语义）：
//   - GET 回显：密钥字段非空时替换为固定占位符 secretPlaceholder，
//     前端据此判断「已配置」；
//   - PUT 保存：密钥字段为占位符或空串时保留磁盘旧值（不会因整份提交把
//     旧密钥洗成空）。
package routers

import (
	config "oh-my-qqbot/internal/config"
)

// secretPlaceholder 是密钥回显占位符（非空密钥 → __SAVED__）。
const secretPlaceholder = "__SAVED__"

// secretFieldNames 是需要脱敏的配置字段名（TOML 键名）。
var secretFieldNames = map[string]bool{
	"app_secret": true, // bot.app_secret：QQ 开放平台应用密钥
	"api_key":    true, // api_providers[*].api_key：模型服务商密钥
}

// maskSecretsMap 递归遍历 TOML 键名字典（structToTOMLMap 的产物），把非空的
// 密钥字段替换为占位符。返回深拷贝，不修改原 map。
func maskSecretsMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch tv := v.(type) {
		case map[string]any:
			out[k] = maskSecretsMap(tv)
		case []any:
			out[k] = maskSecretsSlice(tv)
		default:
			if secretFieldNames[k] {
				if s, isStr := v.(string); isStr && s != "" {
					out[k] = secretPlaceholder
					continue
				}
			}
			out[k] = v
		}
	}
	return out
}

// maskSecretsSlice 处理数组形态（toml.Unmarshal 把 tables 数组还原为 []any）。
func maskSecretsSlice(items []any) []any {
	out := make([]any, len(items))
	for i, v := range items {
		if m, ok := v.(map[string]any); ok {
			out[i] = maskSecretsMap(m)
			continue
		}
		if arr, ok := v.([]any); ok {
			out[i] = maskSecretsSlice(arr)
			continue
		}
		out[i] = v
	}
	return out
}

// isSecretUnset 报告 PUT 上来的密钥值是否应保留旧值（占位符或空串）。
func isSecretUnset(value string) bool {
	return value == "" || value == secretPlaceholder
}

// restoreBotSecrets 把磁盘旧配置中的密钥回填进新解析的配置
// （新值为占位符/空串时保留旧值，S2 兼容整份提交）。
func restoreBotSecrets(next, old *config.Config) {
	if isSecretUnset(next.Bot.AppSecret) {
		next.Bot.AppSecret = old.Bot.AppSecret
	}
}

// restoreModelSecrets 按服务商名匹配旧配置，回填未修改的 api_key
// （新增 provider 无旧值时保留空串）。
func restoreModelSecrets(next, old *config.ModelConfig) {
	for i := range next.APIProviders {
		p := &next.APIProviders[i]
		if !isSecretUnset(p.APIKey) {
			continue
		}
		for _, op := range old.APIProviders {
			if op.Name == p.Name {
				p.APIKey = op.APIKey
				break
			}
		}
	}
}
