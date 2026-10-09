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
func handleExport(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		var body struct {
			IncludeBot   bool `json:"include_bot"`
			IncludeModel bool `json:"include_model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// 缺省全包含（对齐 Python 版 export 的 scopes 默认值）。
		if !body.IncludeBot && !body.IncludeModel {
			body.IncludeBot = true
			body.IncludeModel = true
		}
		payload := exportPayload{
			Format:     "oh-my-qqbot-config",
			Version:    "1",
			ExportedAt: time.Now().Format(time.RFC3339),
		}
		if body.IncludeBot {
			payload.BotConfig = d.Config.GetGlobalConfig()
		}
		if body.IncludeModel {
			payload.ModelConfig = d.Config.GetModelConfig()
		}
		filename := fmt.Sprintf("ohmyqqbot-config-%s.json", time.Now().Format("20060102-150405"))
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		writeJSON(w, http.StatusOK, payload)
	}
}

// handleImport 对齐 POST /data-transfer/import：导入配置 JSON（可选择性覆盖）。
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
			if err := d.Config.SaveBotConfig(payload.BotConfig); err != nil {
				fail(w, http.StatusBadRequest, "bot_config 导入失败: "+err.Error())
				return
			}
			imported = append(imported, "bot_config")
		}
		if payload.ModelConfig != nil {
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
