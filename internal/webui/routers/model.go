// 模型路由 — 对齐 参考/MaiBot/src/webui/routers/model.py 的配置查询面
// （Go 版裁剪「远端拉取模型列表/test-connection」网络探测，返回配置视图）。
package routers

import (
	"net/http"

	config "oh-my-qqbot/internal/config"
)

// ModelInfoView 对齐 GET /models/list 的单行（模型 + 所属 provider 摘要）。
type ModelInfoView struct {
	Name            string         `json:"name"`
	ModelIdentifier string         `json:"model_identifier"`
	APIProvider     string         `json:"api_provider"`
	BaseURL         string         `json:"base_url,omitempty"`
	PriceIn         float64        `json:"price_in"`
	PriceOut        float64        `json:"price_out"`
	Visual          bool           `json:"visual"`
	ForceStreamMode bool           `json:"force_stream_mode"`
	ExtraParams     map[string]any `json:"extra_params,omitempty"`
	Tasks           []string       `json:"tasks"`
}

// handleModelsList 对齐 GET /models/list：模型清单（含被哪些任务引用）。
func handleModelsList(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		mc := d.Config.GetModelConfig()
		taskUsers := map[string][]string{}
		for task, tc := range mc.ModelTaskConfig.TaskConfigs() {
			for _, name := range tc.ModelList {
				taskUsers[name] = append(taskUsers[name], task)
			}
		}
		baseURLByProvider := map[string]string{}
		for _, p := range mc.APIProviders {
			baseURLByProvider[p.Name] = p.BaseURL
		}
		models := make([]ModelInfoView, 0, len(mc.Models))
		for _, m := range mc.Models {
			tasks := taskUsers[m.Name]
			if tasks == nil {
				tasks = []string{}
			}
			models = append(models, ModelInfoView{
				Name:            m.Name,
				ModelIdentifier: m.ModelIdentifier,
				APIProvider:     m.APIProvider,
				BaseURL:         baseURLByProvider[m.APIProvider],
				PriceIn:         m.PriceIn,
				PriceOut:        m.PriceOut,
				Visual:          m.Visual,
				ForceStreamMode: m.ForceStreamMode,
				ExtraParams:     m.ExtraParams,
				Tasks:           tasks,
			})
		}
		ok(w, map[string]any{"models": models, "count": len(models)})
	}
}

// ProviderView 对齐 providers 视图（python 版经 /models/list-by-url 间接暴露）。
type ProviderView struct {
	Name          string `json:"name"`
	BaseURL       string `json:"base_url"`
	ClientType    string `json:"client_type"`
	MaxRetry      int    `json:"max_retry"`
	Timeout       int    `json:"timeout"`
	RetryInterval int    `json:"retry_interval"`
	// HasAPIKey 隐藏密钥本身，只暴露是否存在。
	HasAPIKey bool `json:"has_api_key"`
}

// handleProvidersList 返回 API 提供商清单（密钥脱敏）。
func handleProvidersList(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		mc := d.Config.GetModelConfig()
		providers := make([]ProviderView, 0, len(mc.APIProviders))
		for _, p := range mc.APIProviders {
			providers = append(providers, ProviderView{
				Name: p.Name, BaseURL: p.BaseURL, ClientType: p.ClientType,
				MaxRetry: p.MaxRetry, Timeout: p.Timeout, RetryInterval: p.RetryInterval,
				HasAPIKey: p.APIKey != "" && p.APIKey != "your-api-key",
			})
		}
		ok(w, map[string]any{"providers": providers, "count": len(providers)})
	}
}

// TaskConfigView 对齐 model_task_config 的任务视图。
type TaskConfigView struct {
	Task              string   `json:"task"`
	ModelList         []string `json:"model_list"`
	MaxTokens         int      `json:"max_tokens"`
	Temperature       float64  `json:"temperature"`
	SelectionStrategy string   `json:"selection_strategy"`
	HardTimeout       float64  `json:"hard_timeout"`
	// FallbackChain 是空模型列表时的回退任务链（对齐 TaskFallbackChain）。
	FallbackChain []string `json:"fallback_chain"`
}

// handleTaskConfig 返回任务配置查询（对齐 model_task_config 面板）。
func handleTaskConfig(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Config == nil {
			fail(w, http.StatusServiceUnavailable, "配置服务未启用")
			return
		}
		mc := d.Config.GetModelConfig()
		// 固定字段顺序：对齐 ModelTaskConfig.model_fields 顺序。
		order := []string{"replyer", "planner", "memory", "mid_memory", "utils", "learner",
			"fast_model", "emoji", "vlm", "voice", "embedding", "image_embedding"}
		all := mc.ModelTaskConfig.TaskConfigs()
		tasks := make([]TaskConfigView, 0, len(order))
		for _, name := range order {
			tc, ok := all[name]
			if !ok {
				continue
			}
			modelList := tc.ModelList
			if modelList == nil {
				modelList = []string{}
			}
			fallback := config.TaskFallbackChain[name]
			if fallback == nil {
				fallback = []string{}
			}
			tasks = append(tasks, TaskConfigView{
				Task: name, ModelList: modelList, MaxTokens: tc.MaxTokens,
				Temperature: tc.Temperature, SelectionStrategy: tc.SelectionStrategy,
				HardTimeout: tc.HardTimeout, FallbackChain: fallback,
			})
		}
		ok(w, map[string]any{"tasks": tasks})
	}
}
