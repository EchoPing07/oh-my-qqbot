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
func handleGetBotConfig(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		cfg := d.Config.GetGlobalConfig()
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "config": structToTOMLMap(cfg),
		})
	}
}

// handlePutBotConfig 对齐 POST /config/bot：接收完整 bot 配置（TOML 键名字典），
// 解析校验后保存（保存即生效，对齐 _reload_model_config_after_save 语义）。
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
		if err := d.Config.SaveBotConfig(cfg); err != nil {
			fail(w, http.StatusBadRequest, "配置保存失败: "+err.Error())
			return
		}
		ok(w, map[string]any{"message": "配置已保存"})
	}
}

// handleGetModelConfig 对齐 GET /config/model。
func handleGetModelConfig(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		cfg := d.Config.GetModelConfig()
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "config": structToTOMLMap(cfg),
		})
	}
}

// handlePutModelConfig 对齐 POST /config/model：解析校验 + 保存 + 内存热替换。
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
		if err := d.Config.SaveModelConfig(cfg); err != nil {
			fail(w, http.StatusBadRequest, "配置数据验证失败: "+err.Error())
			return
		}
		ok(w, map[string]any{"message": "配置已保存"})
	}
}

// handleGetBotSchema 对齐 GET /config/schema/bot：返回配置节清单与类型。
// （Go 版以节名枚举替代 JSON Schema；前端用于渲染配置表单骨架。）
func handleGetBotSchema(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"bot":                   map[string]any{"type": "object", "title": "机器人"},
					"personality":           map[string]any{"type": "object", "title": "人格"},
					"chat":                  map[string]any{"type": "object", "title": "聊天"},
					"experimental":          map[string]any{"type": "object", "title": "实验性功能"},
					"visual":                map[string]any{"type": "object", "title": "视觉"},
					"expression":            map[string]any{"type": "object", "title": "表达"},
					"jargon":                map[string]any{"type": "object", "title": "黑话"},
					"a_memorix":             map[string]any{"type": "object", "title": "长期记忆"},
					"message_receive":       map[string]any{"type": "object", "title": "消息接收"},
					"emoji":                 map[string]any{"type": "object", "title": "表情包"},
					"keyword_reaction":      map[string]any{"type": "object", "title": "关键词反应"},
					"response_post_process": map[string]any{"type": "object", "title": "回复后处理"},
					"chinese_typo":          map[string]any{"type": "object", "title": "错别字"},
					"response_splitter":     map[string]any{"type": "object", "title": "断句"},
					"log":                   map[string]any{"type": "object", "title": "日志"},
					"debug":                 map[string]any{"type": "object", "title": "调试"},
					"mcp":                   map[string]any{"type": "object", "title": "MCP"},
					"webui":                 map[string]any{"type": "object", "title": "WebUI"},
					"database":              map[string]any{"type": "object", "title": "数据库"},
				},
			},
		})
	}
}
