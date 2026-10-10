// S2 密钥脱敏测试 — GET 回显占位符、PUT 占位符/空串保留旧值、导出默认不含密钥。
package routers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	config "oh-my-qqbot/internal/config"
)

// setTestBotSecret 直接落一份带密钥的 bot 配置。
func setTestBotSecret(t *testing.T, env *testEnv, secret string) {
	t.Helper()
	cfg := env.deps.Config.GetGlobalConfig()
	cfg.Bot.AppSecret = secret
	if err := env.deps.Config.SaveBotConfig(cfg); err != nil {
		t.Fatalf("保存 bot 配置: %v", err)
	}
}

// TestGetBotConfigMasksAppSecret GET 回显脱敏 + has_app_secret 标记。
func TestGetBotConfigMasksAppSecret(t *testing.T) {
	env := newTestEnv(t, nil)
	setTestBotSecret(t, env, "real-secret-do-not-leak")

	req := httptest.NewRequest("GET", "/api/webui/config/bot", nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /config/bot = %d", rec.Code)
	}
	// 明文绝不能出现在响应体里。
	if strings.Contains(rec.Body.String(), "real-secret-do-not-leak") {
		t.Fatalf("GET /config/bot 明文回显了 app_secret")
	}
	var out struct {
		Success      bool           `json:"success"`
		Config       map[string]any `json:"config"`
		HasAppSecret bool           `json:"has_app_secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if !out.HasAppSecret {
		t.Fatalf("has_app_secret 应为 true")
	}
	bot, _ := out.Config["bot"].(map[string]any)
	if bot == nil {
		t.Fatalf("config.bot 缺失")
	}
	if got, _ := bot["app_secret"].(string); got != secretPlaceholder {
		t.Fatalf("app_secret 应为占位符 %q, got %q", secretPlaceholder, got)
	}
}

// TestPutBotConfigPlaceholderKeepsOldSecret PUT 收到占位符/空串时保留旧值。
func TestPutBotConfigPlaceholderKeepsOldSecret(t *testing.T) {
	env := newTestEnv(t, nil)
	setTestBotSecret(t, env, "real-secret-do-not-leak")

	// 模拟前端流程：GET（脱敏）→ 原样 PUT 回去。
	masked := env.do(t, "GET", "/api/webui/config/bot", nil, http.StatusOK)
	cfgAny := masked["config"]
	env.do(t, "PUT", "/api/webui/config/bot", map[string]any{"config": cfgAny}, http.StatusOK)
	if got := env.deps.Config.GetGlobalConfig().Bot.AppSecret; got != "real-secret-do-not-leak" {
		t.Fatalf("占位符整份提交不应洗掉旧密钥, got %q", got)
	}

	// 空串同样保留旧值。
	blank := map[string]any{}
	raw, _ := json.Marshal(masked["config"])
	_ = json.Unmarshal(raw, &blank)
	if bot, ok := blank["bot"].(map[string]any); ok {
		bot["app_secret"] = ""
	}
	env.do(t, "PUT", "/api/webui/config/bot", map[string]any{"config": blank}, http.StatusOK)
	if got := env.deps.Config.GetGlobalConfig().Bot.AppSecret; got != "real-secret-do-not-leak" {
		t.Fatalf("空串提交不应洗掉旧密钥, got %q", got)
	}

	// 显式新密钥则更新。
	if bot, ok := blank["bot"].(map[string]any); ok {
		bot["app_secret"] = "brand-new-secret"
	}
	env.do(t, "PUT", "/api/webui/config/bot", map[string]any{"config": blank}, http.StatusOK)
	if got := env.deps.Config.GetGlobalConfig().Bot.AppSecret; got != "brand-new-secret" {
		t.Fatalf("显式新密钥应被保存, got %q", got)
	}
}

// TestGetModelConfigMasksAPIKeys GET /config/model 各 provider 密钥脱敏。
func TestGetModelConfigMasksAPIKeys(t *testing.T) {
	env := newTestEnv(t, nil)
	mc := env.deps.Config.GetModelConfig()
	mc.APIProviders = append(mc.APIProviders, config.APIProvider{
		Name: "test-provider", APIKey: "sk-real-do-not-leak",
	})
	if err := env.deps.Config.SaveModelConfig(mc); err != nil {
		t.Fatalf("保存 model 配置: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/webui/config/model", nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /config/model = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sk-real-do-not-leak") {
		t.Fatalf("GET /config/model 明文回显了 api_key")
	}
	var loose map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &loose); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	hasKeys, _ := loose["has_api_keys"].(map[string]any)
	if hasKeys["test-provider"] != true {
		t.Fatalf("has_api_keys[test-provider] 应为 true: %v", hasKeys)
	}
	cfgMap, _ := loose["config"].(map[string]any)
	providers, _ := cfgMap["api_providers"].([]any)
	if len(providers) == 0 {
		t.Fatalf("config.api_providers 缺失")
	}
	p0, _ := providers[0].(map[string]any)
	if got, _ := p0["api_key"].(string); got != secretPlaceholder {
		t.Fatalf("api_key 应为占位符 %q, got %q", secretPlaceholder, got)
	}
}

// TestPutModelConfigPlaceholderKeepsOldKey PUT 占位符/空串按名保留旧 api_key。
func TestPutModelConfigPlaceholderKeepsOldKey(t *testing.T) {
	env := newTestEnv(t, nil)
	mc := env.deps.Config.GetModelConfig()
	mc.APIProviders = append(mc.APIProviders, config.APIProvider{
		Name: "test-provider", APIKey: "sk-real-do-not-leak",
	})
	if err := env.deps.Config.SaveModelConfig(mc); err != nil {
		t.Fatalf("保存 model 配置: %v", err)
	}

	// GET（脱敏）→ 原样 PUT 回去 → 旧密钥保留。
	masked := env.do(t, "GET", "/api/webui/config/model", nil, http.StatusOK)
	env.do(t, "PUT", "/api/webui/config/model", map[string]any{"config": masked["config"]}, http.StatusOK)
	for _, p := range env.deps.Config.GetModelConfig().APIProviders {
		if p.Name == "test-provider" && p.APIKey != "sk-real-do-not-leak" {
			t.Fatalf("占位符整份提交不应洗掉旧 api_key, got %q", p.APIKey)
		}
	}

	// 显式新密钥则更新（按 name 定位 test-provider）。
	var blank map[string]any
	raw, _ := json.Marshal(masked["config"])
	_ = json.Unmarshal(raw, &blank)
	if providers, ok := blank["api_providers"].([]any); ok {
		for _, item := range providers {
			if p, ok := item.(map[string]any); ok && p["name"] == "test-provider" {
				p["api_key"] = "sk-updated"
			}
		}
	}
	env.do(t, "PUT", "/api/webui/config/model", map[string]any{"config": blank}, http.StatusOK)
	for _, p := range env.deps.Config.GetModelConfig().APIProviders {
		if p.Name == "test-provider" && p.APIKey != "sk-updated" {
			t.Fatalf("显式新 api_key 应被保存, got %q", p.APIKey)
		}
	}
}

// TestExportSecretsGating 导出默认不含密钥；include_secrets=true 时含密钥并带警示。
func TestExportSecretsGating(t *testing.T) {
	env := newTestEnv(t, nil)
	setTestBotSecret(t, env, "real-secret-do-not-leak")

	post := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"include_bot": true}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+env.token)
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, req)
		return rec
	}

	// 默认（不含密钥）。
	rec := post("/api/webui/data-transfer/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Contains-Secrets"); got != "false" {
		t.Fatalf("默认导出 X-Contains-Secrets 应为 false, got %q", got)
	}
	body := rec.Body.String()
	if strings.Contains(body, "real-secret-do-not-leak") {
		t.Fatalf("默认导出不应包含明文密钥")
	}
	var payload struct {
		BotConfig map[string]any `json:"bot_config"`
		Extra     map[string]any `json:"extra"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析导出: %v", err)
	}
	// config.Config 只有 toml 标签：JSON 键为 Go 字段名（Bot → AppSecret），
	// 大小写不敏感地取 bot.app_secret 复核为空。
	if got, ok := lookupStringCI(payload.BotConfig, []string{"bot", "app_secret"}); ok && got != "" {
		t.Fatalf("默认导出 app_secret 应为空, got %q", got)
	} else if !ok {
		t.Fatalf("bot_config.bot.app_secret 缺失")
	}

	// include_secrets=true：含明文密钥 + 警示。
	rec = post("/api/webui/data-transfer/export?include_secrets=true")
	if got := rec.Header().Get("X-Contains-Secrets"); got != "true" {
		t.Fatalf("含密钥导出 X-Contains-Secrets 应为 true, got %q", got)
	}
	if !strings.Contains(rec.Body.String(), "real-secret-do-not-leak") {
		t.Fatalf("include_secrets=true 应导出明文密钥")
	}
	var payload2 struct {
		Extra map[string]any `json:"extra"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload2); err != nil {
		t.Fatalf("解析导出: %v", err)
	}
	if payload2.Extra["contains_secrets"] != true || payload2.Extra["warning"] == "" {
		t.Fatalf("含密钥导出应附警示标记: %v", payload2.Extra)
	}
}

// TestImportMaskedExportKeepsOldSecret 导入默认脱敏导出（密钥为空）不洗掉本机旧密钥。
func TestImportMaskedExportKeepsOldSecret(t *testing.T) {
	env := newTestEnv(t, nil)
	setTestBotSecret(t, env, "real-secret-do-not-leak")

	// 默认导出（不含密钥）→ 导入回本机。
	exported := env.do(t, "POST", "/api/webui/data-transfer/export", map[string]any{"include_bot": true}, http.StatusOK)
	env.do(t, "POST", "/api/webui/data-transfer/import", exported, http.StatusOK)
	if got := env.deps.Config.GetGlobalConfig().Bot.AppSecret; got != "real-secret-do-not-leak" {
		t.Fatalf("导入脱敏导出不应洗掉旧密钥, got %q", got)
	}
}

// lookupStringCI 大小写/下划线不敏感地沿路径取字符串（config 结构体无 json
// 标签，导出 JSON 键为 Go 字段名 AppSecret，而 toml 语义键为 app_secret）。
func lookupStringCI(m map[string]any, path []string) (string, bool) {
	norm := func(k string) string {
		return strings.ToLower(strings.ReplaceAll(k, "_", ""))
	}
	cur := m
	for i, key := range path {
		v, ok := cur[key]
		if !ok {
			// 大小写/下划线归一化兜底（AppSecret ↔ app_secret）。
			for k, v2 := range cur {
				if norm(k) == norm(key) {
					v, ok = v2, true
					break
				}
			}
		}
		if !ok {
			return "", false
		}
		if i == len(path)-1 {
			s, isStr := v.(string)
			return s, isStr
		}
		next, isMap := v.(map[string]any)
		if !isMap {
			return "", false
		}
		cur = next
	}
	return "", false
}
