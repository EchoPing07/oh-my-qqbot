// ModelConfig 定义 — 对齐 参考MaiBot/src/config/model_configs.py
// (APIProvider/ModelInfo/TaskConfig/ModelTaskConfig) 与 default_model_config.py 的默认模板。
package config

// ---------------------------------------------------------------------------
// APIProvider — aligned with model_configs.APIProvider
// ---------------------------------------------------------------------------

// APIProvider 对齐 model_configs.APIProvider（API 提供商配置）。
// 裁剪面：organization/project/model_list_endpoint 等次要字段保留，
// reasoning/tool 解析模式保留字符串枚举默认值。
type APIProvider struct {
	Name        string `toml:"name"`         // 服务商名称（models 中引用）
	BaseURL     string `toml:"base_url"`     // 服务商 BaseURL
	APIKey      string `toml:"api_key"`      // API 密钥
	ClientType  string `toml:"client_type"`  // openai（内置）
	AuthType    string `toml:"auth_type"`    // bearer|header|query|none
	AuthHeaderName   string `toml:"auth_header_name"`
	AuthHeaderPrefix string `toml:"auth_header_prefix"`
	AuthQueryName    string `toml:"auth_query_name"`
	DefaultHeaders   map[string]string `toml:"default_headers"`
	DefaultQuery     map[string]string `toml:"default_query"`
	ReasoningParseMode     string `toml:"reasoning_parse_mode"`     // auto|native|think_tag|none
	ToolArgumentParseMode  string `toml:"tool_argument_parse_mode"` // auto|strict|repair|double_decode
	MaxRetry      int `toml:"max_retry"`      // 含首次请求在内的最大请求总数
	Timeout       int `toml:"timeout"`        // 请求超时（秒）
	RetryInterval int `toml:"retry_interval"` // 重试间隔（秒）
}

func defaultAPIProviderFields(p *APIProvider) {
	if p.ClientType == "" {
		p.ClientType = "openai"
	}
	if p.AuthType == "" {
		p.AuthType = "bearer"
	}
	if p.AuthHeaderName == "" {
		p.AuthHeaderName = "Authorization"
	}
	if p.AuthHeaderPrefix == "" {
		p.AuthHeaderPrefix = "Bearer"
	}
	if p.AuthQueryName == "" {
		p.AuthQueryName = "api_key"
	}
	if p.ReasoningParseMode == "" {
		p.ReasoningParseMode = "auto"
	}
	if p.ToolArgumentParseMode == "" {
		p.ToolArgumentParseMode = "auto"
	}
	if p.MaxRetry == 0 {
		p.MaxRetry = 3
	}
	if p.Timeout == 0 {
		p.Timeout = 120
	}
	if p.RetryInterval == 0 {
		p.RetryInterval = 4
	}
	if p.DefaultHeaders == nil {
		p.DefaultHeaders = map[string]string{}
	}
	if p.DefaultQuery == nil {
		p.DefaultQuery = map[string]string{}
	}
}

// ---------------------------------------------------------------------------
// ModelPricePeriod / ModelInfo — aligned with model_configs
// ---------------------------------------------------------------------------

// ModelPricePeriod 对齐 model_configs.ModelPricePeriod（每天重复的价格时段）。
type ModelPricePeriod struct {
	StartTime    string  `toml:"start_time"` // HH:MM（含）
	EndTime      string  `toml:"end_time"`   // HH:MM（不含）；早于开始表示跨午夜
	PriceIn      float64 `toml:"price_in"`
	PriceOut     float64 `toml:"price_out"`
	CachePriceIn float64 `toml:"cache_price_in"`
}

// ModelInfo 对齐 model_configs.ModelInfo（单个模型信息）。
type ModelInfo struct {
	ModelIdentifier string             `toml:"model_identifier"` // 服务商模型标识符
	Name            string             `toml:"name"`             // 自定义名称（model_task_config 引用）
	APIProvider     string             `toml:"api_provider"`
	PriceIn         float64            `toml:"price_in"`
	Cache           bool               `toml:"cache"`           // 遗留兼容字段，不参与逻辑
	CachePriceIn    float64            `toml:"cache_price_in"`
	PriceOut        float64            `toml:"price_out"`
	PricePeriods    []ModelPricePeriod `toml:"price_periods"`
	Temperature     *float64           `toml:"temperature"`      // 模型级温度，覆盖任务配置
	SendTemperature *bool              `toml:"send_temperature"` // 是否发送 MaiBot 管理的 temperature；缺省 true
	MaxTokens       *int               `toml:"max_tokens"`       // 模型级 max_tokens，覆盖任务配置
	ForceStreamMode bool               `toml:"force_stream_mode"`
	Visual          bool               `toml:"visual"`
	ExtraParams     map[string]any     `toml:"extra_params"`
}

// SendTemperatureOrDefault 返回 send_temperature 的有效值（缺省 true，对齐 pydantic 默认值）。
func (m *ModelInfo) SendTemperatureOrDefault() bool {
	return m.SendTemperature == nil || *m.SendTemperature
}

// BoolPtr 返回 bool 指针（模板构造用）。
func BoolPtr(b bool) *bool { return &b }


// ---------------------------------------------------------------------------
// TaskConfig — aligned with model_configs.TaskConfig
// ---------------------------------------------------------------------------

// TaskConfig 对齐 model_configs.TaskConfig（任务配置类）。
type TaskConfig struct {
	ModelList         []string `toml:"model_list"` // 使用的模型名（对应 models[].name）
	MaxTokens         int      `toml:"max_tokens"`
	Temperature       float64  `toml:"temperature"`
	SelectionStrategy string   `toml:"selection_strategy"` // balance|random|sequential
	HardTimeout       float64  `toml:"hard_timeout"`       // 任务硬超时（秒）
}

func defaultTaskConfig() TaskConfig {
	return TaskConfig{
		ModelList:         []string{},
		MaxTokens:         4096,
		Temperature:       0.7,
		SelectionStrategy: "balance",
		HardTimeout:       240.0,
	}
}

// ---------------------------------------------------------------------------
// ModelTaskConfig — aligned with model_configs.ModelTaskConfig
// ---------------------------------------------------------------------------

// ModelTaskConfig 对齐 model_configs.ModelTaskConfig。
// voice 任务保留（Python 版有该任务，本项目暂不使用但不裁掉字段）。
type ModelTaskConfig struct {
	Replyer        TaskConfig `toml:"replyer"`        // 回复模型
	Planner        TaskConfig `toml:"planner"`        // 规划模型
	Memory         TaskConfig `toml:"memory"`         // 长期记忆总结/抽取/写回
	MidMemory      TaskConfig `toml:"mid_memory"`     // 聊天回想；留空回退 fast_model→utils
	Utils          TaskConfig `toml:"utils"`          // 文本概括等小任务（必需）
	Learner        TaskConfig `toml:"learner"`        // 表达/黑话学习；留空回退 utils
	FastModel      TaskConfig `toml:"fast_model"`     // 快速小任务；留空回退 utils
	Emoji          TaskConfig `toml:"emoji"`          // 表情包选择；留空回退 planner/vlm
	VLM            TaskConfig `toml:"vlm"`            // 识图模型
	Voice          TaskConfig `toml:"voice"`          // 语音识别
	Embedding      TaskConfig `toml:"embedding"`      // 嵌入模型
	ImageEmbedding TaskConfig `toml:"image_embedding"` // 图片嵌入
}

// TaskConfigs 返回任务名 → TaskConfig 的映射（对齐 ModelTaskConfig.model_fields 顺序）。
func (m *ModelTaskConfig) TaskConfigs() map[string]*TaskConfig {
	return map[string]*TaskConfig{
		"replyer":        &m.Replyer,
		"planner":        &m.Planner,
		"memory":         &m.Memory,
		"mid_memory":     &m.MidMemory,
		"utils":          &m.Utils,
		"learner":        &m.Learner,
		"fast_model":     &m.FastModel,
		"emoji":          &m.Emoji,
		"vlm":            &m.VLM,
		"voice":          &m.Voice,
		"embedding":      &m.Embedding,
		"image_embedding": &m.ImageEmbedding,
	}
}

// TaskFallbackChain 对齐 default_model_config.py / 调用方的回退语义：
// mid_memory→fast_model→utils、learner→utils、fast_model→utils、emoji→planner/vlm。
var TaskFallbackChain = map[string][]string{
	"mid_memory": {"fast_model", "utils"},
	"learner":    {"utils"},
	"fast_model": {"utils"},
	"memory":     {"utils"},
	"emoji":      {"planner"},
}

// ResolveTask 返回任务对应的模型列表；空列表时按回退链合成（对齐「留空时由调用方按需回退」）。
func (m *ModelTaskConfig) ResolveTask(task string) []string {
	tasks := m.TaskConfigs()
	if tc, ok := tasks[task]; ok && len(tc.ModelList) > 0 {
		return tc.ModelList
	}
	for _, fallback := range TaskFallbackChain[task] {
		if tc, ok := tasks[fallback]; ok && len(tc.ModelList) > 0 {
			return tc.ModelList
		}
	}
	if tc, ok := tasks["utils"]; ok {
		return tc.ModelList
	}
	return nil
}

// ---------------------------------------------------------------------------
// ModelConfig — aligned with config.py class ModelConfig
// ---------------------------------------------------------------------------

// ModelConfig 对齐 config.py class ModelConfig（model_config.toml 顶层结构）。
type ModelConfig struct {
	Models         []ModelInfo     `toml:"models"`
	ModelTaskConfig ModelTaskConfig `toml:"model_task_config"`
	APIProviders   []APIProvider   `toml:"api_providers"`
}

// Validate 对齐 ModelConfig.model_post_init：非空、名称唯一、模型引用的 provider 必须存在。
func (c *ModelConfig) Validate() error {
	if len(c.Models) == 0 {
		return &ValidationError{Message: "models 不能为空"}
	}
	if len(c.APIProviders) == 0 {
		return &ValidationError{Message: "api_providers 不能为空"}
	}
	providerNames := map[string]bool{}
	for _, p := range c.APIProviders {
		if providerNames[p.Name] {
			return &ValidationError{Message: "api_providers 名称重复: " + p.Name}
		}
		providerNames[p.Name] = true
	}
	modelNames := map[string]bool{}
	providerSet := map[string]bool{}
	for _, p := range c.APIProviders {
		providerSet[p.Name] = true
	}
	for _, m := range c.Models {
		if modelNames[m.Name] {
			return &ValidationError{Message: "models 名称重复: " + m.Name}
		}
		modelNames[m.Name] = true
		if m.ModelIdentifier == "" {
			return &ValidationError{Message: "模型 " + m.Name + " 的 model_identifier 不能为空"}
		}
		if m.APIProvider == "" || !providerSet[m.APIProvider] {
			return &ValidationError{Message: "模型 " + m.Name + " 引用了不存在的 api_provider: " + m.APIProvider}
		}
	}
	return nil
}

// ValidationError 表示配置校验失败（对齐 pydantic 校验异常语义）。
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return "config: " + e.Message }

// ---------------------------------------------------------------------------
// 默认模板 — 照抄 default_model_config.py
// ---------------------------------------------------------------------------

// DefaultProviderTemplates 对齐 DEFAULT_PROVIDER_TEMPLATES。
var DefaultProviderTemplates = []APIProvider{
	{
		Name: "DeepSeek", BaseURL: "https://api.deepseek.com", APIKey: "your-api-key",
		AuthType: "bearer", MaxRetry: 3, Timeout: 120, RetryInterval: 4,
	},
}
// defaultTaskConfigTemplates 对齐 DEFAULT_TASK_CONFIG_TEMPLATES（缺省字段由 TaskConfig 默认值补齐）。
var defaultTaskConfigTemplates = map[string]TaskConfig{
	"utils":      {ModelList: []string{"deepseek-v4-flash"}, MaxTokens: 8192, Temperature: 0.5, SelectionStrategy: "random", HardTimeout: 120.0},
	"memory":     {ModelList: []string{}, MaxTokens: 8192, Temperature: 0.3, SelectionStrategy: "random", HardTimeout: 240.0},
	"mid_memory": {ModelList: []string{}, MaxTokens: 8192, Temperature: 0.7, SelectionStrategy: "random", HardTimeout: 180.0},
	"replyer":    {ModelList: []string{"deepseek-v4-pro-think", "deepseek-v4-pro-nonthink"}, MaxTokens: 8192, Temperature: 1, SelectionStrategy: "random", HardTimeout: 240.0},
	"planner":    {ModelList: []string{"deepseek-v4-flash"}, MaxTokens: 16384, Temperature: 0.7, SelectionStrategy: "random", HardTimeout: 180.0},
	"learner":    {ModelList: []string{}, MaxTokens: 8192, Temperature: 0.7, SelectionStrategy: "balance", HardTimeout: 120.0},
	"fast_model": {ModelList: []string{}, MaxTokens: 8192, Temperature: 0.3, SelectionStrategy: "balance", HardTimeout: 120.0},
	"emoji":      {ModelList: []string{}, MaxTokens: 8192, Temperature: 0.7, SelectionStrategy: "balance", HardTimeout: 120.0},
	"vlm":        {ModelList: []string{}, MaxTokens: 8192, Temperature: 0.7, SelectionStrategy: "balance", HardTimeout: 240.0},
	"voice":      {ModelList: []string{}, MaxTokens: 8192, Temperature: 0.7, SelectionStrategy: "balance", HardTimeout: 120.0},
	"embedding":  {ModelList: []string{}, MaxTokens: 4096, Temperature: 0.7, SelectionStrategy: "balance", HardTimeout: 60.0},
	"image_embedding": {ModelList: []string{}, MaxTokens: 4096, Temperature: 0.7, SelectionStrategy: "balance", HardTimeout: 60.0},
}

// DefaultModelTemplates 对齐 DEFAULT_MODEL_TEMPLATES。
var DefaultModelTemplates = []ModelInfo{
	{
		ModelIdentifier: "deepseek-v4-pro", Name: "deepseek-v4-pro-think", APIProvider: "DeepSeek",
		PriceIn: 12.0, PriceOut: 24.0, SendTemperature: BoolPtr(true), Visual: false,
		ExtraParams: map[string]any{"thinking": map[string]any{"type": "enabled"}, "reasoning_effort": "high"},
	},
	{
		ModelIdentifier: "deepseek-v4-pro", Name: "deepseek-v4-pro-nonthink", APIProvider: "DeepSeek",
		PriceIn: 12.0, PriceOut: 24.0, SendTemperature: BoolPtr(true), Visual: false,
		ExtraParams: map[string]any{"thinking": map[string]any{"type": "disabled"}},
	},
	{
		ModelIdentifier: "deepseek-v4-flash", Name: "deepseek-v4-flash", APIProvider: "DeepSeek",
		PriceIn: 1.0, PriceOut: 2.0, SendTemperature: BoolPtr(true), Visual: false,
		ExtraParams: map[string]any{"thinking": map[string]any{"type": "disabled"}},
	},
}

// BuildDefaultModelTemplates 对齐 build_default_model_templates：
// 只保留任务分配中实际用到的模型模板。
func BuildDefaultModelTemplates() []ModelInfo {
	used := map[string]bool{}
	for _, tc := range defaultTaskConfigTemplates {
		for _, name := range tc.ModelList {
			used[name] = true
		}
	}
	var out []ModelInfo
	for _, m := range DefaultModelTemplates {
		if used[m.Name] {
			out = append(out, m)
		}
	}
	return out
}

// DefaultModelConfig 对齐 create_default_model_config：生成可通过校验的默认模型配置。
func DefaultModelConfig() *ModelConfig {
	mtc := ModelTaskConfig{
		Replyer:        defaultTaskConfigTemplates["replyer"],
		Planner:        defaultTaskConfigTemplates["planner"],
		Memory:         defaultTaskConfigTemplates["memory"],
		MidMemory:      defaultTaskConfigTemplates["mid_memory"],
		Utils:          defaultTaskConfigTemplates["utils"],
		Learner:        defaultTaskConfigTemplates["learner"],
		FastModel:      defaultTaskConfigTemplates["fast_model"],
		Emoji:          defaultTaskConfigTemplates["emoji"],
		VLM:            defaultTaskConfigTemplates["vlm"],
		Voice:          defaultTaskConfigTemplates["voice"],
		Embedding:      defaultTaskConfigTemplates["embedding"],
		ImageEmbedding: defaultTaskConfigTemplates["image_embedding"],
	}
	providers := make([]APIProvider, len(DefaultProviderTemplates))
	copy(providers, DefaultProviderTemplates)
	models := BuildDefaultModelTemplates()
	if models == nil {
		models = []ModelInfo{}
	}
	return &ModelConfig{Models: models, ModelTaskConfig: mtc, APIProviders: providers}
}
