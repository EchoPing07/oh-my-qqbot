// 配置路由 — 对齐 参考/MaiBot/src/webui/routers/config.py 的读写面：
// GET/POST /config/bot、GET/POST /config/model（Go 版同时接受 PUT 语义）。
//
// 传输形态对齐 Python 版 tomlkit.load：配置以 TOML 键名（小写节名）的字典
// 返回/接收；写入经 TOML 再解析进结构体校验后由 Manager 保存（保存即生效）。
package routers

import (
	"encoding/json"
	"net/http"

	toml "github.com/pelletier/go-toml/v2"

	config "oh-my-qqbot/internal/config"
)

// structToTOMLMap 把配置结构体转成 TOML 键名字典（对齐 tomlkit.load 的形态）。
func structToTOMLMap(cfg any) map[string]any {
	buf, err := toml.Marshal(cfg)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := toml.Unmarshal(buf, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// jsonToConfig 把请求体里的 TOML 形字典解析进配置结构体（缺省字段保留默认值）。
// JSON 数字统一为 float64，TOML 编解码会拒绝 float→int 赋值，先做数值归一。
func jsonToConfig(raw any, dst any) error {
	buf, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err != nil {
		return err
	}
	normalizeNumbers(m)
	tomlBuf, err := toml.Marshal(m)
	if err != nil {
		return err
	}
	return toml.Unmarshal(tomlBuf, dst)
}

// normalizeNumbers 递归把整值 float64 转为 int（toml.Marshal 的 int/float 严格性）。
func normalizeNumbers(v any) {
	switch tv := v.(type) {
	case map[string]any:
		for k, val := range tv {
			tv[k] = normalizeValue(val)
		}
	case []any:
		for i, val := range tv {
			tv[i] = normalizeValue(val)
		}
	}
}

func normalizeValue(val any) any {
	switch f := val.(type) {
	case float64:
		if f == float64(int64(f)) && f < 1e15 && f > -1e15 {
			return int64(f)
		}
		return f
	case map[string]any:
		normalizeNumbers(f)
		return f
	case []any:
		normalizeNumbers(f)
		return f
	default:
		return val
	}
}

// handleGetBotConfig 对齐 GET /config/bot：返回完整 bot 配置（TOML 键名形态）。
// S2：app_secret 不明文回显 — 非空时返回占位符 __SAVED__，顶层附带
// has_app_secret 供前端展示「已配置」状态。
func handleGetBotConfig(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		cfg := d.Config.GetGlobalConfig()
		writeJSON(w, http.StatusOK, map[string]any{
			"success":        true,
			"config":         maskSecretsMap(structToTOMLMap(cfg)),
			"has_app_secret": cfg.Bot.AppSecret != "",
		})
	}
}

// handlePutBotConfig 对齐 POST /config/bot：接收完整 bot 配置（TOML 键名字典），
// 解析校验后保存（保存即生效，对齐 _reload_model_config_after_save 语义）。
// S2：app_secret 为占位符/空串时保留磁盘旧值（前端「留空保留」语义）。
func handlePutBotConfig(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		var body struct {
			Config any `json:"config"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Config == nil {
			fail(w, http.StatusBadRequest, "缺少 config 字段")
			return
		}
		cfg := config.DefaultConfig()
		if err := jsonToConfig(body.Config, cfg); err != nil {
			fail(w, http.StatusBadRequest, "配置数据验证失败: "+err.Error())
			return
		}
		restoreBotSecrets(cfg, d.Config.GetGlobalConfig())
		if err := d.Config.SaveBotConfig(cfg); err != nil {
			fail(w, http.StatusBadRequest, "配置保存失败: "+err.Error())
			return
		}
		ok(w, map[string]any{"message": "配置已保存"})
	}
}

// handleGetModelConfig 对齐 GET /config/model。
// S2：各 provider 的 api_key 不明文回显 — 非空时返回占位符 __SAVED__，
// 顶层附带 has_api_keys（provider 名 → 是否已配置）。
func handleGetModelConfig(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		cfg := d.Config.GetModelConfig()
		hasAPIKeys := make(map[string]bool, len(cfg.APIProviders))
		for _, p := range cfg.APIProviders {
			hasAPIKeys[p.Name] = p.APIKey != ""
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success":      true,
			"config":       maskSecretsMap(structToTOMLMap(cfg)),
			"has_api_keys": hasAPIKeys,
		})
	}
}

// handlePutModelConfig 对齐 POST /config/model：解析校验 + 保存 + 内存热替换。
// S2：api_key 为占位符/空串时按服务商名保留磁盘旧值。
func handlePutModelConfig(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		var body struct {
			Config any `json:"config"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Config == nil {
			fail(w, http.StatusBadRequest, "缺少 config 字段")
			return
		}
		cfg := config.DefaultModelConfig()
		if err := jsonToConfig(body.Config, cfg); err != nil {
			fail(w, http.StatusBadRequest, "配置数据验证失败: "+err.Error())
			return
		}
		restoreModelSecrets(cfg, d.Config.GetModelConfig())
		if err := d.Config.SaveModelConfig(cfg); err != nil {
			fail(w, http.StatusBadRequest, "配置数据验证失败: "+err.Error())
			return
		}
		ok(w, map[string]any{"message": "配置已保存"})
	}
}

// botSchemaSections 定义配置节的展示顺序与中文标题。
// 「机器人」放第一位：内含 AppID / AppSecret 等首次使用必填的凭据字段。
// 顺序敏感 — 前端节导航按此 slice 顺序渲染（服务端保持有序输出）。
var botSchemaSections = []struct {
	Name  string
	Title string
}{
	{Name: "bot", Title: "机器人"},
	{Name: "personality", Title: "人格"},
	{Name: "chat", Title: "聊天"},
	{Name: "experimental", Title: "实验性功能"},
	{Name: "visual", Title: "视觉"},
	{Name: "expression", Title: "表达"},
	{Name: "jargon", Title: "黑话"},
	{Name: "a_memorix", Title: "长期记忆"},
	{Name: "message_receive", Title: "消息接收"},
	{Name: "emoji", Title: "表情包"},
	{Name: "keyword_reaction", Title: "关键词反应"},
	{Name: "response_post_process", Title: "回复后处理"},
	{Name: "chinese_typo", Title: "错别字"},
	{Name: "response_splitter", Title: "断句"},
	{Name: "log", Title: "日志"},
	{Name: "debug", Title: "调试"},
	{Name: "mcp", Title: "MCP"},
	{Name: "webui", Title: "WebUI"},
	{Name: "database", Title: "数据库"},
}

// handleGetBotSchema 对齐 GET /config/schema/bot：返回配置节清单与类型。
// （Go 版以节名枚举替代 JSON Schema；前端用于渲染节导航与中文标题。）
// 用有序数组 + json.Encoder 输出，避免 Go map 无序导致节导航乱序。
func handleGetBotSchema(_ *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		properties := make([]map[string]any, 0, len(botSchemaSections))
		for _, s := range botSchemaSections {
			properties = append(properties, map[string]any{
				"name":  s.Name,
				"type":  "object",
				"title": s.Title,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"schema": map[string]any{
				"type":       "object",
				"properties": properties,
			},
		})
	}
}
