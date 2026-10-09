package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	contracts "oh-my-qqbot/internal"
)

// Now is the injectable clock used across this package.
var Now = time.Now

// randomItemID mirrors ContextItemMeta.create's uuid4().hex.
func randomItemID() string {
	const hexDigits = "0123456789abcdef"
	buf := make([]byte, 32)
	for index := range buf {
		buf[index] = hexDigits[rand.IntN(len(hexDigits))]
	}
	return string(buf)
}

// ---------------------------------------------------------------------------
// Provider / model configuration (aligned with src/config/model_configs.py)
// ---------------------------------------------------------------------------

// AuthKind mirrors OpenAICompatibleAuthType.
type AuthKind string

const (
	AuthNone   AuthKind = "none"
	AuthBearer AuthKind = "bearer"
	AuthHeader AuthKind = "header"
	AuthQuery  AuthKind = "query"
)

// APIProvider mirrors the裁剪后的 APIProvider of model_configs.py.
type APIProvider struct {
	Name             string            `toml:"name"`
	BaseURL          string            `toml:"base_url"`
	APIKey           string            `toml:"api_key"`
	AuthType         AuthKind          `toml:"auth_type"`
	AuthHeaderName   string            `toml:"auth_header_name"`
	AuthHeaderPrefix string            `toml:"auth_header_prefix"`
	AuthQueryName    string            `toml:"auth_query_name"`
	MaxRetry         int               `toml:"max_retry"`
	Timeout          float64           `toml:"timeout"`
	RetryInterval    float64           `toml:"retry_interval"`
	ClientType       string            `toml:"client_type"`
	DefaultHeaders   map[string]string `toml:"default_headers"`
	DefaultQuery     map[string]any    `toml:"default_query"`
}

// ProviderDefaults mirrors the field defaults of APIProvider (max_retry=3,
// timeout=120, retry_interval=4 per src/config/model_configs.py).
const (
	DefaultMaxRetry      = 3
	DefaultTimeout       = 120.0
	DefaultRetryInterval = 4.0
)

// Normalize fills in the Python defaults for unset fields.
func (p APIProvider) Normalize() APIProvider {
	if p.AuthType == "" {
		p.AuthType = AuthBearer
	}
	if p.AuthHeaderName == "" {
		p.AuthHeaderName = "Authorization"
	}
	if p.AuthHeaderPrefix == "" && p.AuthType == AuthBearer {
		p.AuthHeaderPrefix = "Bearer"
	}
	if p.AuthQueryName == "" {
		p.AuthQueryName = "api_key"
	}
	if p.MaxRetry <= 0 {
		p.MaxRetry = DefaultMaxRetry
	}
	if p.Timeout <= 0 {
		p.Timeout = DefaultTimeout
	}
	if p.RetryInterval <= 0 {
		p.RetryInterval = DefaultRetryInterval
	}
	if p.ClientType == "" {
		p.ClientType = "openai"
	}
	p.BaseURL = normalizeBaseURL(p.BaseURL)
	return p
}

// ModelPricePeriod mirrors ModelPricePeriod (daily recurring price window,
// prices are元 / M token).
type ModelPricePeriod struct {
	StartTime    string  `toml:"start_time"`
	EndTime      string  `toml:"end_time"`
	PriceIn      float64 `toml:"price_in"`
	PriceOut     float64 `toml:"price_out"`
	CachePriceIn float64 `toml:"cache_price_in"`
}

// ModelInfo mirrors the裁剪后的 ModelInfo of model_configs.py.
type ModelInfo struct {
	ModelIdentifier string             `toml:"model_identifier"`
	Name            string             `toml:"name"`
	APIProvider     string             `toml:"api_provider"`
	PriceIn         float64            `toml:"price_in"`
	CachePriceIn    float64            `toml:"cache_price_in"`
	PriceOut        float64            `toml:"price_out"`
	PricePeriods    []ModelPricePeriod `toml:"price_periods"`
	Temperature     *float64           `toml:"temperature"`
	// SendTemperature mirrors `send_temperature`: nil keeps the Python default
	// (true), i.e. temperature managed by MaiBot is sent to the model.
	SendTemperature *bool          `toml:"send_temperature"`
	MaxTokens       *int           `toml:"max_tokens"`
	ForceStreamMode bool           `toml:"force_stream_mode"`
	Visual          bool           `toml:"visual"`
	ExtraParams     map[string]any `toml:"extra_params"`
}

// Normalize fills defaults that cannot be expressed by the zero value
// (SendTemperature defaults to true in Python).
func (m ModelInfo) Normalize() ModelInfo {
	if m.ModelIdentifier == "" {
		m.ModelIdentifier = m.Name
	}
	if m.ExtraParams == nil {
		m.ExtraParams = map[string]any{}
	}
	return m
}

// SendsTemperature mirrors the `send_temperature` field default (True).
func (m ModelInfo) SendsTemperature() bool {
	if m.SendTemperature == nil {
		return true
	}
	return *m.SendTemperature
}

// Selection strategies mirroring TaskConfig.selection_strategy.
const (
	SelectionBalance    = "balance"
	SelectionRandom     = "random"
	SelectionSequential = "sequential"
)

// TaskConfig mirrors TaskConfig of model_configs.py.
type TaskConfig struct {
	ModelList         []string `toml:"model_list"`
	MaxTokens         int      `toml:"max_tokens"`
	Temperature       *float64 `toml:"temperature"`
	SelectionStrategy string   `toml:"selection_strategy"`
	HardTimeout       float64  `toml:"hard_timeout"`
}

// Defaults mirror the Python field defaults of TaskConfig.
const (
	DefaultTaskMaxTokens   = 4096
	DefaultTaskTemperature = 0.7
	DefaultHardTimeout     = 240.0
)

// Normalize fills the Python defaults for unset fields.
func (t TaskConfig) Normalize() TaskConfig {
	if t.MaxTokens <= 0 {
		t.MaxTokens = DefaultTaskMaxTokens
	}
	if t.SelectionStrategy == "" {
		t.SelectionStrategy = SelectionBalance
	}
	if t.HardTimeout <= 0 {
		t.HardTimeout = DefaultHardTimeout
	}
	return t
}

// EffectiveTemperature mirrors the `temperature` field default (0.7).
func (t TaskConfig) EffectiveTemperature() float64 {
	if t.Temperature == nil {
		return DefaultTaskTemperature
	}
	return *t.Temperature
}

// TaskNames mirrors ModelTaskConfig fields of model_configs.py.
var TaskNames = []string{
	"replyer", "planner", "memory", "mid_memory", "utils", "learner",
	"fast_model", "emoji", "vlm", "voice", "embedding", "image_embedding",
}

// emptyTaskFallbacks mirrors EMPTY_TASK_FALLBACKS in utils_model.py.
var emptyTaskFallbacks = map[string]string{
	"fast_model": "utils",
	"learner":    "utils",
	"mid_memory": "fast_model",
}

// EmbeddingTaskNames mirrors EMBEDDING_TASK_NAMES in utils_model.py.
var embeddingTaskNames = map[string]bool{"embedding": true, "image_embedding": true}

// ModelConfig mirrors ModelTaskConfig: one task config per logical task.
type ModelConfig struct {
	Providers map[string]APIProvider `toml:"api_providers"`
	Models    map[string]ModelInfo   `toml:"models"`
	Tasks     map[string]TaskConfig  `toml:"model_task_config"`
}

// TaskConfigFor resolves a task name into its effective TaskConfig, applying
// the EMPTY_TASK_FALLBACKS chain exactly like
// LLMOrchestrator._get_task_config_or_raise.
func (c ModelConfig) TaskConfigFor(taskName string) (TaskConfig, string, error) {
	taskName = strings.TrimSpace(taskName)
	if taskName == "" {
		return TaskConfig{}, "", fmt.Errorf("llm: 任务配置名称不能为空")
	}
	task, ok := c.Tasks[taskName]
	if !ok {
		return TaskConfig{}, "", fmt.Errorf("llm: 未找到名为 '%s' 的任务配置", taskName)
	}
	effective := taskName
	for !hasAnyModel(task.ModelList) {
		fallback, ok := emptyTaskFallbacks[effective]
		if !ok {
			if taskName == "image_embedding" {
				return TaskConfig{}, "", fmt.Errorf("llm: 图片嵌入任务未配置模型，请在 image_embedding 中指定支持图片嵌入的模型")
			}
			break
		}
		fallbackTask, ok := c.Tasks[fallback]
		if !ok {
			break
		}
		task = fallbackTask
		effective = fallback
	}
	return task.Normalize(), effective, nil
}

// ModelFor resolves a model assignment name into its ModelInfo together with
// its provider (mirrors TempMethodsLLMUtils.get_model_info_by_name + provider).
func (c ModelConfig) ModelFor(name string) (ModelInfo, APIProvider, error) {
	model, ok := c.Models[name]
	if !ok {
		return ModelInfo{}, APIProvider{}, fmt.Errorf("llm: 未找到名为 '%s' 的模型配置", name)
	}
	model = model.Normalize()
	provider, ok := c.Providers[model.APIProvider]
	if !ok {
		return ModelInfo{}, APIProvider{}, fmt.Errorf("llm: 未找到名为 '%s' 的 API 供应商配置", model.APIProvider)
	}
	return model, provider.Normalize(), nil
}

// Models returns every configured model assignment name (sorted stable order).
func (c ModelConfig) ModelsList() []string {
	names := make([]string, 0, len(c.Models))
	for name := range c.Models {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

func hasAnyModel(modelList []string) bool {
	for _, name := range modelList {
		if strings.TrimSpace(name) != "" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// HTTP client
// ---------------------------------------------------------------------------

// HTTPError mirrors RespNotOkException: a non-2xx provider response.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
	URL        string
	RetryAfter time.Duration
}

// Error implements error.
func (e *HTTPError) Error() string {
	body := e.Body
	if len(body) > 512 {
		body = body[:512] + "…"
	}
	return fmt.Sprintf("llm: %s 请求失败: HTTP %d %s: %s", e.URL, e.StatusCode, e.Status, strings.TrimSpace(body))
}

// Retryable mirrors the Python classification: 429 and >=500 are retried,
// everything else is a hard error for the current model.
func (e *HTTPError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// Errors mirroring the Python exception taxonomy
// (src/llm_models/exceptions.py).
var (
	// ErrEmptyResponse mirrors EmptyResponseException (retryable within a model).
	ErrEmptyResponse = errors.New("llm: provider output is empty (no visible text or tool calls)")
	// ErrParseResponse mirrors RespParseException (retryable within a model).
	ErrParseResponse = errors.New("llm: 响应解析失败")
)

// NetworkError wraps a transport-level failure (mirrors NetworkConnectionError).
type NetworkError struct {
	Err error
}

// Error implements error.
func (e *NetworkError) Error() string { return "llm: 网络错误: " + e.Err.Error() }

// Unwrap supports errors.Is/As.
func (e *NetworkError) Unwrap() error { return e.Err }

// Client is an OpenAI-compatible chat-completions client built on the standard
// library (the wire structures are aligned with
// 参考/MaiBot/src/llm_models/model_client/openai_client.py, engineering layout
// inspired by 参考/Diana/model/llm/openai_compatible.go + types.go).
type Client struct {
	provider APIProvider
	http     *http.Client
}

// NewClient builds a client for one provider configuration.
func NewClient(provider APIProvider, httpClient *http.Client) *Client {
	provider = provider.Normalize()
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{provider: provider, http: httpClient}
}

// Provider exposes the normalized provider configuration.
func (c *Client) Provider() APIProvider { return c.provider }

// ---------------------------------------------------------------------------
// Wire structures (chat completions)
// ---------------------------------------------------------------------------

type chatCompletionRequest struct {
	Model               string                  `json:"model"`
	Messages            []chatCompletionMessage `json:"messages"`
	Temperature         *float64                `json:"temperature,omitempty"`
	MaxTokens           int                     `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                     `json:"max_completion_tokens,omitempty"`
	Tools               []chatCompletionTool    `json:"tools,omitempty"`
	ToolChoice          string                  `json:"tool_choice,omitempty"`
	Stream              bool                    `json:"stream,omitempty"`
	ReasoningEffort     string                  `json:"reasoning_effort,omitempty"`
	Thinking            *chatThinking           `json:"thinking,omitempty"`
	ParallelToolCalls   *bool                   `json:"parallel_tool_calls,omitempty"`
	PromptCacheKey      string                  `json:"prompt_cache_key,omitempty"`
}

type chatThinking struct {
	Type string `json:"type"`
}

type chatCompletionMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"`
	Name             string         `json:"name,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
}

type chatToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function chatToolCallFunction `json:"function"`
}

type chatToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatCompletionTool struct {
	Type     string                   `json:"type"`
	Function chatCompletionToolSchema `json:"function"`
}

type chatCompletionToolSchema struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
	Refusal  string        `json:"refusal,omitempty"`
}

type chatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// chatCompletionResponse mirrors the parts of the OpenAI response payload this
// port consumes. Unknown fields are ignored on purpose.
type chatCompletionResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			Reasoning        string          `json:"reasoning"`
			Refusal          string          `json:"refusal"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// ---------------------------------------------------------------------------
// Chat completion
// ---------------------------------------------------------------------------

// ChatOptions carries per-call overrides applied on top of the provider config.
type ChatOptions struct {
	// Temperature overrides the model/task temperature when non-nil.
	Temperature *float64
	// MaxTokens overrides the model/task max output tokens when > 0.
	MaxTokens int
	// ReasoningEffort maps to the OpenAI `reasoning_effort` field.
	ReasoningEffort string
	// ExtraBody merges model extra_params into the request body.
	ExtraBody map[string]any
	// ExtraHeaders merges provider extra headers.
	ExtraHeaders map[string]string
	// ExtraQuery merges provider extra query parameters.
	ExtraQuery map[string]any
	// PromptCacheKey is the stable session-affinity key (also forwarded via the
	// X-Session-Affinity-style headers used by relay gateways).
	PromptCacheKey string
	// ParallelToolCalls forces the parallel_tool_calls field when non-nil.
	ParallelToolCalls *bool
}

// Chat performs one non-streaming chat-completions call and normalizes the
// result into contracts.ChatResponse.
func (c *Client) Chat(ctx context.Context, req contracts.ChatRequest, options ChatOptions) (*contracts.ChatResponse, error) {
	model := req.Model
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("llm: 模型标识符不能为空")
	}
	body := chatCompletionRequest{
		Model:    model,
		Messages: convertMessages(req.Messages),
		Tools:    convertTools(req.Tools),
	}
	if options.Temperature != nil {
		body.Temperature = options.Temperature
	} else if req.Temperature != nil {
		body.Temperature = req.Temperature
	}
	maxTokens := req.MaxTokens
	if options.MaxTokens > 0 {
		maxTokens = options.MaxTokens
	}
	if maxTokens > 0 {
		body.MaxTokens = maxTokens
	}
	if options.ReasoningEffort != "" {
		body.ReasoningEffort = options.ReasoningEffort
	}
	if options.PromptCacheKey != "" {
		body.PromptCacheKey = options.PromptCacheKey
	}
	if options.ParallelToolCalls != nil {
		body.ParallelToolCalls = options.ParallelToolCalls
	} else if len(req.Tools) > 0 {
		parallel := false
		body.ParallelToolCalls = &parallel
	}
	for key, value := range req.ExtraParams {
		c.applyExtraBody(&body, key, value)
	}
	for key, value := range options.ExtraBody {
		c.applyExtraBody(&body, key, value)
	}

	payload, err := c.marshalWithExtras(&body, options)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
	}
	for key, value := range options.ExtraHeaders {
		headers[key] = value
	}
	query := map[string]any{}
	for key, value := range options.ExtraQuery {
		query[key] = value
	}

	endpoint := "chat/completions"
	raw, err := c.do(ctx, endpoint, payload, headers, query, options.PromptCacheKey)
	if err != nil {
		return nil, err
	}

	var parsed chatCompletionResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseResponse, err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("llm: provider error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("%w: choices 为空", ErrParseResponse)
	}
	choice := parsed.Choices[0]

	usage := parseUsage(parsed.Usage)
	response := &contracts.ChatResponse{
		Content:      decodeMessageContent(choice.Message.Content),
		Reasoning:    firstNonEmpty(choice.Message.ReasoningContent, choice.Message.Reasoning),
		FinishReason: choice.FinishReason,
		Usage:        usage,
		Model:        firstNonEmpty(parsed.Model, model),
	}
	for _, call := range choice.Message.ToolCalls {
		response.ToolCalls = append(response.ToolCalls, contracts.ToolCall{
			ID:       call.ID,
			Name:     call.Function.Name,
			ArgsJSON: defaultJSONObject(call.Function.Arguments),
		})
	}
	if strings.TrimSpace(response.Content) == "" && len(response.ToolCalls) == 0 && strings.TrimSpace(choice.Message.Refusal) == "" {
		return nil, ErrEmptyResponse
	}
	return response, nil
}

// marshalWithExtras merges the caller-supplied body extensions and returns the
// request body JSON. `body` keeps the typed request, `options.ExtraBody` the
// flattened caller overrides (see Orchestrator.splitExtraParams).
func (c *Client) marshalWithExtras(body *chatCompletionRequest, options ChatOptions) ([]byte, error) {
	base, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: encode request: %w", err)
	}
	if len(options.ExtraBody) == 0 {
		return base, nil
	}
	var merged map[string]any
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, fmt.Errorf("llm: normalize request: %w", err)
	}
	for key, value := range options.ExtraBody {
		if nested, ok := value.(map[string]any); ok && (key == "body") {
			for nestedKey, nestedValue := range nested {
				merged[nestedKey] = nestedValue
			}
			continue
		}
		merged[key] = value
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("llm: encode request: %w", err)
	}
	return encoded, nil
}

// applyExtraBody mirrors split_openai_request_overrides: reserved transport keys
// (headers/query/body) are handled separately, everything else lands in the body.
// Typed fields that would otherwise collide with the JSON struct tags are folded
// back onto the typed request.
func (c *Client) applyExtraBody(body *chatCompletionRequest, key string, value any) {
	switch key {
	case "headers", "query":
		return
	case "body":
		for nestedKey, nestedValue := range asAnyMap(value) {
			c.applyExtraBody(body, nestedKey, nestedValue)
		}
	case "temperature":
		if number, ok := toFloat(value); ok {
			body.Temperature = &number
		}
	case "max_tokens":
		if number, ok := toInt(value); ok {
			body.MaxTokens = number
		}
	case "reasoning_effort":
		if text := strings.TrimSpace(toStringValue(value)); text != "" {
			body.ReasoningEffort = text
		}
	case "thinking":
		if mapping, ok := value.(map[string]any); ok {
			body.Thinking = &chatThinking{Type: strings.TrimSpace(toStringValue(mapping["type"]))}
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

func (c *Client) do(ctx context.Context, endpoint string, payload []byte, headers map[string]string, query map[string]any, promptCacheKey string) ([]byte, error) {
	requestURL, err := c.buildURL(endpoint, query)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	c.applyAuth(request)
	for key, value := range c.provider.DefaultHeaders {
		request.Header.Set(key, value)
	}
	if strings.TrimSpace(promptCacheKey) != "" {
		// Session affinity headers used by relay gateways (see Diana
		// sessionAffinityHeaders); a configured header with the same name wins.
		for _, name := range []string{"X-Session-Affinity", "X-Session-Id", "X-Conversation-Id"} {
			if _, configured := c.provider.DefaultHeaders[name]; configured {
				continue
			}
			request.Header.Set(name, promptCacheKey)
		}
	}

	client := *c.http
	if client.Timeout == 0 && c.provider.Timeout > 0 {
		client.Timeout = time.Duration(c.provider.Timeout * float64(time.Second))
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &NetworkError{Err: err}
	}
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, &NetworkError{Err: err}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, &HTTPError{
			StatusCode: response.StatusCode,
			Status:     response.Status,
			Body:       string(raw),
			URL:        requestURL,
			RetryAfter: parseRetryAfter(response.Header.Get("Retry-After")),
		}
	}
	return raw, nil
}

func (c *Client) buildURL(endpoint string, query map[string]any) (string, error) {
	base := c.provider.BaseURL
	if strings.TrimSpace(base) == "" {
		base = "https://api.openai.com/v1"
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("llm: invalid base url %q", base)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.TrimLeft(endpoint, "/")
	// The base URL's own query/fragment are dropped, matching
	// joinOpenAICompatibleURL in Diana and the Python SDK's base_url handling.
	parsed.RawQuery = ""
	parsed.Fragment = ""
	values := parsed.Query()
	for key, value := range c.provider.DefaultQuery {
		values.Set(key, toStringValue(value))
	}
	for key, value := range query {
		values.Set(key, toStringValue(value))
	}
	if c.provider.AuthType == AuthQuery && c.provider.APIKey != "" {
		values.Set(c.provider.AuthQueryName, c.provider.APIKey)
	}
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

// applyAuth mirrors build_openai_compatible_client_config.
func (c *Client) applyAuth(request *http.Request) {
	provider := c.provider
	switch provider.AuthType {
	case AuthNone:
		return
	case AuthQuery:
		// already applied while building the URL
		return
	case AuthHeader:
		request.Header.Set(provider.AuthHeaderName, joinAuthValue(provider.AuthHeaderPrefix, provider.APIKey))
	default: // AuthBearer
		if provider.AuthHeaderName == "Authorization" && provider.AuthHeaderPrefix == "Bearer" {
			if provider.APIKey != "" {
				request.Header.Set("Authorization", "Bearer "+provider.APIKey)
			}
			return
		}
		request.Header.Set(provider.AuthHeaderName, joinAuthValue(provider.AuthHeaderPrefix, provider.APIKey))
	}
}

func joinAuthValue(prefix string, apiKey string) string {
	if strings.TrimSpace(prefix) == "" {
		return apiKey
	}
	return prefix + " " + apiKey
}

// ---------------------------------------------------------------------------
// Conversion helpers
// ---------------------------------------------------------------------------

func convertMessages(messages []contracts.ChatMessage) []chatCompletionMessage {
	out := make([]chatCompletionMessage, 0, len(messages))
	for _, message := range messages {
		converted := chatCompletionMessage{
			Role:       string(message.Role),
			Name:       message.Name,
			ToolCallID: message.ToolCallID,
		}
		if converted.Role == "" {
			converted.Role = string(RoleUser)
		}
		converted.Content = convertContent(message)
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, chatToolCall{
				ID:   call.ID,
				Type: "function",
				Function: chatToolCallFunction{
					Name:      call.Name,
					Arguments: defaultJSONObject(call.ArgsJSON),
				},
			})
		}
		out = append(out, converted)
	}
	return out
}

func convertContent(message contracts.ChatMessage) any {
	if len(message.Images) == 0 {
		return message.Content
	}
	parts := make([]chatContentPart, 0, len(message.Images)+1)
	if text := strings.TrimSpace(message.Content); text != "" {
		parts = append(parts, chatContentPart{Type: ContentPartText, Text: message.Content})
	}
	for _, image := range message.Images {
		if strings.TrimSpace(image.URL) == "" && strings.TrimSpace(image.Base64) == "" {
			continue
		}
		detail := strings.ToLower(strings.TrimSpace(image.Detail))
		if detail != "low" && detail != "high" {
			detail = "auto"
		}
		parts = append(parts, chatContentPart{
			Type:     "image_url",
			ImageURL: &chatImageURL{URL: firstNonEmpty(image.URL, image.Base64), Detail: detail},
		})
	}
	if len(parts) == 0 {
		return message.Content
	}
	return parts
}

func convertTools(tools []contracts.ToolSpec) []chatCompletionTool {
	out := make([]chatCompletionTool, 0, len(tools))
	for _, tool := range tools {
		var parameters any = map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}}
		if strings.TrimSpace(tool.ParametersJSON) != "" {
			var decoded any
			if err := json.Unmarshal([]byte(tool.ParametersJSON), &decoded); err == nil && decoded != nil {
				parameters = decoded
			}
		}
		out = append(out, chatCompletionTool{
			Type: "function",
			Function: chatCompletionToolSchema{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  parameters,
			},
		})
	}
	return out
}

// decodeMessageContent accepts both the plain-string and the content-part-array
// forms of `message.content` (some gateways return arrays for text-only output).
func decodeMessageContent(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return text
		}
		return ""
	}
	var parts []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		switch part.Type {
		case ContentPartText, "output_text":
			builder.WriteString(part.Text)
		case ContentPartRefusal:
			builder.WriteString(part.Refusal)
		}
	}
	return builder.String()
}

// ---------------------------------------------------------------------------
// usage parsing (mirrors openai_client._extract_usage_record)
// ---------------------------------------------------------------------------

type rawUsage struct {
	PromptTokens          *int `json:"prompt_tokens"`
	CompletionTokens      *int `json:"completion_tokens"`
	TotalTokens           *int `json:"total_tokens"`
	PromptCacheHitTokens  *int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens *int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails   *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails json.RawMessage `json:"completion_tokens_details"`
}

// parseUsage mirrors `_extract_usage_record`: prompt_cache_hit_tokens wins over
// prompt_tokens_details.cached_tokens, and a missing miss-count is derived from
// prompt_tokens - hit_tokens. CacheEnabled records whether the provider reported
// any cache field at all (None sentinel semantics in Python).
func parseUsage(raw json.RawMessage) contracts.Usage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return contracts.Usage{}
	}
	var parsed rawUsage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return contracts.Usage{}
	}
	usage := contracts.Usage{}
	if parsed.PromptTokens != nil {
		usage.PromptTokens = *parsed.PromptTokens
	}
	if parsed.CompletionTokens != nil {
		usage.CompletionTokens = *parsed.CompletionTokens
	}
	if parsed.TotalTokens != nil {
		usage.TotalTokens = *parsed.TotalTokens
	}

	var cachedTokens *int
	if parsed.PromptTokensDetails != nil {
		cachedTokens = parsed.PromptTokensDetails.CachedTokens
	}
	usage.CacheEnabled = parsed.PromptCacheHitTokens != nil || parsed.PromptCacheMissTokens != nil || cachedTokens != nil

	hitTokens := 0
	switch {
	case parsed.PromptCacheHitTokens != nil:
		hitTokens = *parsed.PromptCacheHitTokens
	case cachedTokens != nil:
		hitTokens = *cachedTokens
	}
	missTokens := 0
	if parsed.PromptCacheMissTokens != nil {
		missTokens = *parsed.PromptCacheMissTokens
	}
	if missTokens == 0 && hitTokens > 0 {
		if derived := usage.PromptTokens - hitTokens; derived > 0 {
			missTokens = derived
		}
	}
	usage.CacheHitTokens = hitTokens
	usage.CacheMissTokens = missTokens
	return usage
}

// parseRetryAfter decodes the Retry-After header (seconds or HTTP date).
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delta := time.Until(when); delta > 0 {
			return delta
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func normalizeBaseURL(value string) string {
	value = strings.TrimSpace(value)
	if value != "" && !strings.Contains(value, "://") {
		value = "http://" + value
	}
	return strings.TrimRight(value, "/")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && values[cursor] < values[cursor-1]; cursor-- {
			values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
		}
	}
}

// toStringValue mirrors Python's `str(value)` used for TOML/JSON configuration
// values that may arrive as any JSON scalar.
func toStringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func toFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// Float64Ptr is a helper for optional numeric configuration fields.
func Float64Ptr(value float64) *float64 { return &value }

// IntPtr is a helper for optional integer configuration fields.
func IntPtr(value int) *int { return &value }

// BoolPtr is a helper for optional boolean configuration fields.
func BoolPtr(value bool) *bool { return &value }

func toInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		return parsed, err == nil
	default:
		return 0, false
	}
}
