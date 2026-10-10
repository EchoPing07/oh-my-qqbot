// data_transfer 路由 — 配置导出/导入（JSON 打包 bot_config/model_config）。
// 对齐 routers/data_transfer.py 的 export/import 面（Go 版裁剪数据库大文件打包，
// 聚焦配置双文件的可迁移性）。
package routers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	config "oh-my-qqbot/internal/config"
)

// exportPayload 是导出文件结构（data-transfer 导出 JSON）。
type exportPayload struct {
	Format      string              `json:"format"`
	Version     string              `json:"version"`
	ExportedAt  string              `json:"exported_at"`
	BotConfig   *config.Config      `json:"bot_config,omitempty"`
	ModelConfig *config.ModelConfig `json:"model_config,omitempty"`
	Extra       map[string]any      `json:"extra,omitempty"`
}

// handleExport 对齐 POST /data-transfer/export：导出配置 JSON（attachment 下载）。
// S2：默认不含密钥（app_secret/api_key 以空串导出，导入时经「留空保留」语义
// 不会洗掉目标机旧密钥）；?include_secrets=true 或 body.include_secrets=true
// 时导出明文密钥，并在响应头与 payload 中附警示标记。
func handleExport(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		var body struct {
			IncludeBot     bool `json:"include_bot"`
			IncludeModel   bool `json:"include_model"`
			IncludeSecrets bool `json:"include_secrets"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// S2：include_secrets 查询参数优先（默认 false，不含密钥）。
		if r.URL.Query().Get("include_secrets") == "true" {
			body.IncludeSecrets = true
		}
		// 缺省全包含（对齐 Python 版 export 的 scopes 默认值）。
		if !body.IncludeBot && !body.IncludeModel {
			body.IncludeBot = true
			body.IncludeModel = true
		}
		botConfig := d.Config.GetGlobalConfig()
		modelConfig := d.Config.GetModelConfig()
		if !body.IncludeSecrets {
			// 不含密钥：密钥字段置空（导入端「留空保留旧值」语义兜底）。
			botCopy := *botConfig
			botCopy.Bot.AppSecret = ""
			modelCopy := *modelConfig
			modelCopy.APIProviders = make([]config.APIProvider, len(modelConfig.APIProviders))
			for i, p := range modelConfig.APIProviders {
				p.APIKey = ""
				modelCopy.APIProviders[i] = p
			}
			botConfig, modelConfig = &botCopy, &modelCopy
		}
		payload := exportPayload{
			Format:      "oh-my-qqbot-config",
			Version:     "1",
			ExportedAt:  time.Now().Format(time.RFC3339),
			Extra:       map[string]any{},
			BotConfig:   nil,
			ModelConfig: nil,
		}
		if body.IncludeBot {
			payload.BotConfig = botConfig
		}
		if body.IncludeModel {
			payload.ModelConfig = modelConfig
		}
		// S2：含密钥导出时附警示（响应头 + payload 字段双标记）。
		if body.IncludeSecrets {
			w.Header().Set("X-Contains-Secrets", "true")
			payload.Extra["warning"] = "本文件包含明文密钥（app_secret/api_key），请妥善保管，勿经不可信渠道传输"
			payload.Extra["contains_secrets"] = true
		} else {
			payload.Extra["contains_secrets"] = false
			w.Header().Set("X-Contains-Secrets", "false")
		}
		filename := fmt.Sprintf("ohmyqqbot-config-%s.json", time.Now().Format("20060102-150405"))
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		writeJSON(w, http.StatusOK, payload)
	}
}

// handleImport 对齐 POST /data-transfer/import：导入配置 JSON（可选择性覆盖）。
// S2：与 PUT 一致的「留空保留」语义 — 导入文件里 app_secret/api_key 为空
// （默认脱敏导出）时保留本机已配置的旧密钥；含密钥导出则整体覆盖。
func handleImport(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		var payload exportPayload
		if !decodeJSON(w, r, &payload) {
			return
		}
		imported := []string{}
		if payload.BotConfig != nil {
			restoreBotSecrets(payload.BotConfig, d.Config.GetGlobalConfig())
			if err := d.Config.SaveBotConfig(payload.BotConfig); err != nil {
				fail(w, http.StatusBadRequest, "bot_config 导入失败: "+err.Error())
				return
			}
			imported = append(imported, "bot_config")
		}
		if payload.ModelConfig != nil {
			restoreModelSecrets(payload.ModelConfig, d.Config.GetModelConfig())
			if err := d.Config.SaveModelConfig(payload.ModelConfig); err != nil {
				fail(w, http.StatusBadRequest, "model_config 导入失败: "+err.Error())
				return
			}
			imported = append(imported, "model_config")
		}
		if len(imported) == 0 {
			fail(w, http.StatusBadRequest, "导入文件中没有可识别的配置（bot_config/model_config）")
			return
		}
		ok(w, map[string]any{"message": "配置导入成功", "imported": imported})
	}
}
