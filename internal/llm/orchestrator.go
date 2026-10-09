package llm

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	contracts "oh-my-qqbot/internal"
)

// TaskAttempt describes one model attempt of a logical request (used by tests
// and by the monitor layer to emit retry events).
type TaskAttempt struct {
	TaskName    string
	ModelName   string
	Attempt     int
	MaxAttempts int
	Status      string // "succeeded" | "retrying" | "failed"
	Reason      string
	Duration    time.Duration
}

// AttemptHook receives every attempt outcome; used for monitor events
// (internal.EventLLMRetry / internal.EventLLMError later on).
type AttemptHook func(attempt TaskAttempt)

// OrchestratorOptions configures an Orchestrator.
type OrchestratorOptions struct {
	// Config is the model configuration (providers/models/task configs).
	Config ModelConfig
	// PrefixMessages are prepended to every request (system prompt).
	PrefixMessages []contracts.ChatMessage
	// SessionID is the default session id used for usage accounting.
	SessionID string
	// Recorder writes llm_usage rows; nil disables accounting.
	Recorder *UsageRecorder
	// HTTPClient overrides the underlying transport (tests inject httptest clients).
	HTTPClient *http.Client
	// Rand overrides model selection randomness.
	Rand *rand.Rand
	// Sleep overrides the retry backoff sleep (tests make it instant).
	Sleep func(ctx context.Context, delay time.Duration) error
	// OnAttempt receives attempt outcomes.
	OnAttempt AttemptHook
	// RequestType labels the usage rows (defaults to the task name when empty).
	RequestType string
	// MaxRetryOverride overrides APIProvider.MaxRetry when > 0.
	MaxRetryOverride int
}

// Orchestrator mirrors LLMOrchestrator in
// 参考/MaiBot/src/llm_models/utils_model.py:
//
//	task name → TaskConfig.model_list → selection strategy → per-model retries
//	(max_retry) → hard_timeout per attempt → next model on failure.
type Orchestrator struct {
	options OrchestratorOptions

	mu sync.Mutex
	// modelUsage mirrors `model_usage`: total_tokens, penalty, usage_penalty.
	modelUsage map[string][3]int
	// explicitUsage mirrors `_explicit_model_usage`.
	explicitUsage map[string][3]int
	// clients caches one Client per provider name.
	clients map[string]*Client
	// usageWrites tracks in-flight llm_usage writes so shutdown can flush them.
	usageWrites sync.WaitGroup
}

// NewOrchestrator creates a task router.
func NewOrchestrator(options OrchestratorOptions) *Orchestrator {
	if options.Sleep == nil {
		options.Sleep = sleepWithContext
	}
	if options.Rand == nil {
		options.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Orchestrator{
		options:       options,
		modelUsage:    map[string][3]int{},
		explicitUsage: map[string][3]int{},
		clients:       map[string]*Client{},
	}
}

// Config returns the live model configuration (re-read on every request so hot
// reloads take effect, mirroring _refresh_task_config).
func (o *Orchestrator) Config() ModelConfig { return o.options.Config }

// SetConfig swaps the model configuration (hot reload entry point).
func (o *Orchestrator) SetConfig(config ModelConfig) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.options.Config = config
}

// WaitForUsage blocks until in-flight llm_usage writes have been handed to the
// UsageWriter. Call it on graceful shutdown so accounting rows are not lost.
func (o *Orchestrator) WaitForUsage() { o.usageWrites.Wait() }

// SetSessionID updates the default accounting session id.
func (o *Orchestrator) SetSessionID(sessionID string) { o.options.SessionID = sessionID }

// ---------------------------------------------------------------------------
// internal.LLMLayer implementation
// ---------------------------------------------------------------------------

// Chat implements internal.LLMLayer: one logical chat request routed to a task.
func (o *Orchestrator) Chat(ctx context.Context, task string, request contracts.ChatRequest) (*contracts.ChatResponse, error) {
	result, err := o.execute(ctx, executeParams{
		Task:        task,
		RequestType: "response",
		Request:     request,
	})
	if err != nil {
		return nil, err
	}
	return result.Response, nil
}

// Embed implements internal.LLMLayer: batch embeddings through the `embedding`
// task (sequentially embedded when the provider caps batch size at one).
func (o *Orchestrator) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	result, err := o.execute(ctx, executeParams{
		Task:        "embedding",
		RequestType: "embedding",
		Embedding:   texts,
	})
	if err != nil {
		return nil, err
	}
	return result.Embeddings, nil
}

// ChatForTask is Chat with an explicit request type label (for usage rows).
func (o *Orchestrator) ChatForTask(ctx context.Context, task string, requestType string, request contracts.ChatRequest) (*contracts.ChatResponse, error) {
	result, err := o.execute(ctx, executeParams{
		Task:        task,
		RequestType: requestType,
		Request:     request,
	})
	if err != nil {
		return nil, err
	}
	return result.Response, nil
}

// ChatWithSession is Chat with a per-call session id override.
func (o *Orchestrator) ChatWithSession(ctx context.Context, task string, sessionID string, request contracts.ChatRequest) (*contracts.ChatResponse, error) {
	result, err := o.execute(ctx, executeParams{
		Task:        task,
		RequestType: "response",
		SessionID:   sessionID,
		Request:     request,
	})
	if err != nil {
		return nil, err
	}
	return result.Response, nil
}

// ---------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------

type executeParams struct {
	Task        string
	RequestType string
	SessionID   string
	ModelName   string
	Request     contracts.ChatRequest
	Embedding   []string
}

// ExecutionResult mirrors LLMExecutionResult (api_response + model_info +
// request_started_at).
type ExecutionResult struct {
	Response         *contracts.ChatResponse
	Embeddings       [][]float32
	Model            ModelInfo
	Provider         APIProvider
	RequestStartedAt time.Time
	TimeCost         time.Duration
}

func (o *Orchestrator) execute(ctx context.Context, params executeParams) (*ExecutionResult, error) {
	if strings.TrimSpace(params.Task) == "" {
		return nil, fmt.Errorf("llm: 任务配置名称不能为空")
	}
	config := o.Config()
	taskConfig, _, err := config.TaskConfigFor(params.Task)
	if err != nil {
		return nil, err
	}
	if !hasAnyModel(taskConfig.ModelList) {
		return nil, fmt.Errorf("llm: 任务 '%s' 未配置任何模型", params.Task)
	}

	failedModels := map[string]bool{}
	maxAttempts := len(taskConfig.ModelList)
	explicit := strings.TrimSpace(params.ModelName) != ""
	if explicit {
		maxAttempts = 1
	}

	var lastErr error
	for modelIndex := 0; modelIndex < maxAttempts; modelIndex++ {
		modelName, model, provider, err := o.selectModel(config, taskConfig, params.Task, params.ModelName, failedModels)
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		client := o.clientFor(provider)

		startedAt := time.Now()
		result, err := o.attemptWithTimeout(ctx, client, model, provider, taskConfig, params, startedAt)
		if err == nil {
			o.adjustModelUsage(modelName, resultTokens(result), 0, -1)
			return result, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			o.adjustModelUsage(modelName, 0, 0, -1)
			return nil, err
		}

		lastErr = err
		o.adjustModelUsage(modelName, 0, 1, -1)
		failedModels[modelName] = true
		o.emitAttempt(TaskAttempt{
			TaskName:    params.Task,
			ModelName:   modelName,
			Attempt:     modelIndex + 1,
			MaxAttempts: maxAttempts,
			Status:      "failed",
			Reason:      err.Error(),
			Duration:    time.Since(startedAt),
		})
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("llm: 请求失败，所有可用模型均已尝试失败。")
	}
	return nil, lastErr
}

// attemptWithTimeout mirrors _attempt_request_on_model_with_timeout: the task
// level hard_timeout cancels the provider request and the orchestrator moves on
// to the next model.
func (o *Orchestrator) attemptWithTimeout(
	ctx context.Context,
	client *Client,
	model ModelInfo,
	provider APIProvider,
	taskConfig TaskConfig,
	params executeParams,
	startedAt time.Time,
) (*ExecutionResult, error) {
	attemptCtx := ctx
	cancel := func() {}
	if taskConfig.HardTimeout > 0 {
		attemptCtx, cancel = context.WithTimeout(ctx, time.Duration(taskConfig.HardTimeout*float64(time.Second)))
	}
	defer cancel()

	var (
		response   *contracts.ChatResponse
		embeddings [][]float32
		err        error
	)
	if params.Embedding != nil {
		options := o.embeddingOptions(model, params)
		embeddings, err = client.Embeddings(attemptCtx, model, params.Embedding, options)
	} else {
		response, err = o.attemptChat(attemptCtx, client, model, taskConfig, params)
	}
	if err != nil {
		if attemptCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return nil, fmt.Errorf("llm: 任务 '%s' 的模型 '%s' 硬超时(%ss)", params.Task, model.Name, formatSeconds(taskConfig.HardTimeout))
		}
		return nil, err
	}

	timeCost := time.Since(startedAt)
	result := &ExecutionResult{
		Response:         response,
		Embeddings:       embeddings,
		Model:            model,
		Provider:         provider,
		RequestStartedAt: startedAt,
		TimeCost:         timeCost,
	}
	if response != nil {
		o.recordUsage(ctx, params, result, response.Usage, timeCost)
	}
	return result, nil
}

// attemptChat mirrors _attempt_request_on_model: retries inside one model with
// `api_provider.max_retry` (floored at 1) attempts and retry_interval backoff.
func (o *Orchestrator) attemptChat(
	ctx context.Context,
	client *Client,
	model ModelInfo,
	taskConfig TaskConfig,
	params executeParams,
) (*contracts.ChatResponse, error) {
	maxAttempts := client.provider.MaxRetry
	if o.options.MaxRetryOverride > 0 {
		maxAttempts = o.options.MaxRetryOverride
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	request := params.Request
	request.Model = model.ModelIdentifier
	// Effective request parameters follow _resolve_effective_max_tokens /
	// _resolve_effective_temperature (explicit → model → extra_params → task).
	request.MaxTokens = resolveMaxTokens(model, taskConfig, params.Request.MaxTokens)
	request.Temperature = resolveTemperature(model, taskConfig)
	messages := request.Messages
	if len(o.options.PrefixMessages) > 0 {
		// Prefix messages are stored once and reused; copy the slice so
		// concurrent requests cannot mutate each other's view.
		prefix := append([]contracts.ChatMessage(nil), o.options.PrefixMessages...)
		messages = append(prefix, messages...)
	}
	request.Messages = messages

	extraHeaders, extraQuery, extraBody := splitExtraParams(model.ExtraParams)
	options := ChatOptions{
		ExtraBody:      extraBody,
		ExtraHeaders:   extraHeaders,
		ExtraQuery:     extraQuery,
		PromptCacheKey: o.resolveSessionID(params.SessionID),
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attemptStarted := time.Now()
		response, err := client.Chat(ctx, request, options)
		if err == nil {
			o.emitAttempt(TaskAttempt{
				TaskName:    params.Task,
				ModelName:   model.Name,
				Attempt:     attempt,
				MaxAttempts: maxAttempts,
				Status:      "succeeded",
				Duration:    time.Since(attemptStarted),
			})
			return response, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryableError(err) || attempt == maxAttempts {
			break
		}
		o.emitAttempt(TaskAttempt{
			TaskName:    params.Task,
			ModelName:   model.Name,
			Attempt:     attempt,
			MaxAttempts: maxAttempts,
			Status:      "retrying",
			Reason:      err.Error(),
			Duration:    time.Since(attemptStarted),
		})
		delay := time.Duration(client.provider.RetryInterval * float64(time.Second))
		if httpErr := (*HTTPError)(nil); errors.As(err, &httpErr) && httpErr.RetryAfter > 0 {
			delay = httpErr.RetryAfter
		}
		if sleepErr := o.options.Sleep(ctx, delay); sleepErr != nil {
			return nil, sleepErr
		}
	}
	return nil, lastErr
}

func (o *Orchestrator) embeddingOptions(model ModelInfo, params executeParams) EmbeddingOptions {
	extraHeaders, extraQuery, extraBody := splitExtraParams(model.ExtraParams)
	return EmbeddingOptions{ExtraBody: extraBody, ExtraHeaders: extraHeaders, ExtraQuery: extraQuery}
}

// splitExtraParams mirrors split_openai_request_overrides: the `headers` /
// `query` / `body` sub-maps are transport-level, every other key is merged into
// the request body.
func splitExtraParams(extraParams map[string]any) (headers map[string]string, query map[string]any, body map[string]any) {
	headers = map[string]string{}
	query = map[string]any{}
	body = map[string]any{}
	for key, value := range extraParams {
		switch key {
		case "headers":
			for name, headerValue := range asAnyMap(value) {
				headers[name] = toStringValue(headerValue)
			}
		case "query":
			for name, queryValue := range asAnyMap(value) {
				query[name] = queryValue
			}
		case "body":
			for name, bodyValue := range asAnyMap(value) {
				body[name] = bodyValue
			}
		default:
			body[key] = value
		}
	}
	return headers, query, body
}

func asAnyMap(value any) map[string]any {
	if mapping, ok := value.(map[string]any); ok {
		return mapping
	}
	return map[string]any{}
}

// recordUsage mirrors the recorder call sites in utils_model.py.
func (o *Orchestrator) recordUsage(ctx context.Context, params executeParams, result *ExecutionResult, usage contracts.Usage, timeCost time.Duration) {
	if o.options.Recorder == nil {
		return
	}
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 && !usage.CacheEnabled {
		return
	}
	requestType := firstNonEmpty(params.RequestType, o.options.RequestType, params.Task)
	o.usageWrites.Add(1)
	go func() {
		defer o.usageWrites.Done()
		// Usage accounting must never fail or delay a chat request.
		_ = o.options.Recorder.Record(context.WithoutCancel(ctx), RecordParams{
			Model:            result.Model,
			Provider:         result.Provider,
			Usage:            usage,
			RequestType:      requestType,
			TaskName:         params.Task,
			SessionID:        o.resolveSessionID(params.SessionID),
			TimeCost:         timeCost.Seconds(),
			RequestStartedAt: result.RequestStartedAt,
			Success:          true,
		})
	}()
}

// ---------------------------------------------------------------------------
// model selection (mirrors LLMOrchestrator._select_model)
// ---------------------------------------------------------------------------

func (o *Orchestrator) selectModel(
	config ModelConfig,
	taskConfig TaskConfig,
	taskName string,
	explicitModel string,
	excluded map[string]bool,
) (string, ModelInfo, APIProvider, error) {
	available := map[string][3]int{}
	o.mu.Lock()
	for _, name := range taskConfig.ModelList {
		if excluded[name] {
			continue
		}
		if _, ok := o.modelUsage[name]; !ok {
			o.modelUsage[name] = [3]int{}
		}
		available[name] = o.modelUsage[name]
	}
	o.mu.Unlock()

	requested := strings.TrimSpace(explicitModel)
	if requested != "" {
		if excluded[requested] {
			return "", ModelInfo{}, APIProvider{}, fmt.Errorf("llm: 指定模型 '%s' 已在本次请求中尝试失败", requested)
		}
		if _, _, err := config.ModelFor(requested); err != nil {
			return "", ModelInfo{}, APIProvider{}, err
		}
		if _, inList := available[requested]; !inList {
			// Explicitly chosen models outside the task list keep a separate
			// usage store, mirroring `_explicit_model_usage`.
			o.mu.Lock()
			if _, ok := o.explicitUsage[requested]; !ok {
				o.explicitUsage[requested] = [3]int{}
			}
			usage := o.explicitUsage[requested]
			o.mu.Unlock()
			available = map[string][3]int{requested: usage}
		} else {
			available = map[string][3]int{requested: available[requested]}
		}
	}
	if len(available) == 0 {
		return "", ModelInfo{}, APIProvider{}, fmt.Errorf("llm: 没有可用的模型可供选择。所有模型均已尝试失败。")
	}

	strategy := strings.ToLower(strings.TrimSpace(taskConfig.SelectionStrategy))
	if embeddingTaskNames[taskName] {
		// Embedding vectors must stay in one vector space; ignore the configured
		// strategy and pick the first configured model (mirrors utils_model.py).
		strategy = SelectionSequential
	}

	var selected string
	switch {
	case requested != "":
		selected = requested
	case strategy == SelectionRandom:
		candidates := sortedKeys(available)
		selected = candidates[o.options.Rand.Intn(len(candidates))]
	case strategy == SelectionSequential:
		for _, name := range taskConfig.ModelList {
			if _, ok := available[name]; ok {
				selected = name
				break
			}
		}
	default: // balance (and unknown strategies, which warn and fall back)
		selected = minBalance(available, taskConfig.ModelList)
	}
	if selected == "" {
		return "", ModelInfo{}, APIProvider{}, fmt.Errorf("llm: 没有可用的模型可供选择。所有模型均已尝试失败。")
	}

	model, provider, err := config.ModelFor(selected)
	if err != nil {
		return "", ModelInfo{}, APIProvider{}, err
	}
	o.adjustModelUsage(selected, 0, 0, 1)
	return selected, model, provider, nil
}

func (o *Orchestrator) adjustModelUsage(modelName string, tokensDelta int, penaltyDelta int, usagePenaltyDelta int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	store := o.modelUsage
	if _, ok := store[modelName]; !ok {
		store = o.explicitUsage
		if _, ok := store[modelName]; !ok {
			store[modelName] = [3]int{}
		}
	}
	current := store[modelName]
	store[modelName] = [3]int{
		current[0] + tokensDelta,
		current[1] + penaltyDelta,
		current[2] + usagePenaltyDelta,
	}
}

// minBalance mirrors the balance key
// `total_tokens + penalty*300 + usage_penalty*1000`.
func minBalance(available map[string][3]int, ordered []string) string {
	best := ""
	bestScore := 0
	for _, name := range ordered {
		usage, ok := available[name]
		if !ok {
			continue
		}
		score := usage[0] + usage[1]*300 + usage[2]*1000
		if best == "" || score < bestScore {
			best, bestScore = name, score
		}
	}
	if best != "" {
		return best
	}
	for _, name := range sortedKeys(available) {
		usage := available[name]
		score := usage[0] + usage[1]*300 + usage[2]*1000
		if best == "" || score < bestScore {
			best, bestScore = name, score
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (o *Orchestrator) clientFor(provider APIProvider) *Client {
	o.mu.Lock()
	defer o.mu.Unlock()
	if client, ok := o.clients[provider.Name]; ok {
		return client
	}
	client := NewClient(provider, o.options.HTTPClient)
	o.clients[provider.Name] = client
	return client
}

func (o *Orchestrator) resolveSessionID(override string) string {
	return strings.TrimSpace(firstNonEmpty(override, o.options.SessionID))
}

func (o *Orchestrator) emitAttempt(attempt TaskAttempt) {
	if o.options.OnAttempt != nil {
		o.options.OnAttempt(attempt)
	}
}

// resolveMaxTokens mirrors _resolve_effective_max_tokens.
func resolveMaxTokens(model ModelInfo, taskConfig TaskConfig, explicit int) int {
	if explicit > 0 {
		return explicit
	}
	if model.MaxTokens != nil && *model.MaxTokens > 0 {
		return *model.MaxTokens
	}
	if raw, ok := model.ExtraParams["max_tokens"]; ok {
		if value, ok := toInt(raw); ok && value > 0 {
			return value
		}
	}
	return taskConfig.MaxTokens
}

// resolveTemperature mirrors _resolve_effective_temperature, including the
// `send_temperature = false` escape hatch (returns nil → field omitted).
func resolveTemperature(model ModelInfo, taskConfig TaskConfig) *float64 {
	if !model.SendsTemperature() {
		return nil
	}
	if model.Temperature != nil {
		return model.Temperature
	}
	if raw, ok := model.ExtraParams["temperature"]; ok {
		if value, ok := toFloat(raw); ok {
			return &value
		}
	}
	value := taskConfig.EffectiveTemperature()
	return &value
}

// retryableError mirrors the Python retry classification.
func retryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrEmptyResponse) || errors.Is(err, ErrParseResponse) {
		return true
	}
	var network *NetworkError
	if errors.As(err, &network) {
		return true
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Retryable()
	}
	return false
}

func resultTokens(result *ExecutionResult) int {
	if result == nil || result.Response == nil {
		return 0
	}
	return result.Response.Usage.TotalTokens
}

func sortedKeys(values map[string][3]int) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func formatSeconds(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d", int64(value))
	}
	return fmt.Sprintf("%g", value)
}
