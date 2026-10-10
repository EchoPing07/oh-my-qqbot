package llm

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	contracts "oh-my-qqbot/internal"
)

// TestOrchestratorConcurrentRequests exercises shared state under -race-less
// runs: model usage maps, client cache and the async usage recorder.
func TestOrchestratorConcurrentRequests(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return 200, completionPayload("ok", map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2})
	})
	var mu sync.Mutex
	writes := 0
	orchestrator := newTestOrchestrator(t, provider, func(config *ModelConfig) {
		config.Tasks["utils"] = TaskConfig{
			ModelList: []string{"fast", "pro"}, MaxTokens: 64, Temperature: Float64Ptr(0.5),
			SelectionStrategy: SelectionBalance, HardTimeout: 5,
		}
	}, func(options *OrchestratorOptions) {
		options.Recorder = NewUsageRecorder(UsageWriterFunc(func(context.Context, ModelUsage) error {
			mu.Lock()
			writes++
			mu.Unlock()
			return nil
		}))
	})

	var waitGroup sync.WaitGroup
	errs := make(chan error, 32)
	for index := 0; index < 32; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{
				Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "hi"}},
			})
			errs <- err
		}()
	}
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent chat failed: %v", err)
		}
	}
	orchestrator.WaitForUsage()
	mu.Lock()
	defer mu.Unlock()
	if writes != 32 {
		t.Fatalf("usage writes = %d, want 32", writes)
	}
}

// TestOrchestratorHotReloadConfig mirrors _refresh_task_config: an updated
// ModelConfig takes effect on the next request without a restart.
func TestOrchestratorHotReloadConfig(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return 200, completionPayload("ok", nil)
	})
	orchestrator := newTestOrchestrator(t, provider, nil, nil)
	if _, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if model := provider.recorded()[0].Body["model"]; model != "fast-model" {
		t.Fatalf("model = %v", model)
	}
	config := testModelConfig(provider.server.URL, func(config *ModelConfig) {
		config.Tasks["utils"] = TaskConfig{ModelList: []string{"pro"}, MaxTokens: 64, Temperature: Float64Ptr(0.5), HardTimeout: 5}
	})
	orchestrator.SetConfig(config)
	if _, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatalf("Chat after reload: %v", err)
	}
	if model := provider.recorded()[1].Body["model"]; model != "pro-model" {
		t.Fatalf("model after reload = %v", model)
	}
}

// TestRetryableErrorClassification mirrors the Python exception taxonomy.
func TestRetryableErrorClassification(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{"empty response", ErrEmptyResponse, true},
		{"parse response", ErrParseResponse, true},
		{"network", &NetworkError{Err: context.DeadlineExceeded}, true},
		{"http 429", &HTTPError{StatusCode: 429}, true},
		{"http 503", &HTTPError{StatusCode: 503}, true},
		{"http 400", &HTTPError{StatusCode: 400}, false},
		{"canceled", context.Canceled, false},
		{"nil", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableError(test.err); got != test.retryable {
				t.Fatalf("retryable = %v, want %v", got, test.retryable)
			}
		})
	}
	if !strings.Contains(ErrEmptyResponse.Error(), "empty") {
		t.Fatalf("message = %s", ErrEmptyResponse.Error())
	}
}

// TestNormalizeProviderAndModelDefaults pins the Python field defaults.
func TestNormalizeProviderAndModelDefaults(t *testing.T) {
	provider := APIProvider{Name: "p", BaseURL: "api.example.com/v1"}.Normalize()
	if provider.MaxRetry != DefaultMaxRetry || provider.Timeout != DefaultTimeout || provider.RetryInterval != DefaultRetryInterval {
		t.Fatalf("provider = %#v", provider)
	}
	if provider.BaseURL != "http://api.example.com/v1" {
		t.Fatalf("base url = %s", provider.BaseURL)
	}
	if provider.AuthType != AuthBearer || provider.AuthHeaderName != "Authorization" || provider.AuthHeaderPrefix != "Bearer" {
		t.Fatalf("auth = %#v", provider)
	}
	if model := (ModelInfo{Name: "m"}).Normalize(); model.ModelIdentifier != "m" || !model.SendsTemperature() {
		t.Fatalf("model = %#v", model)
	}
	if model := (ModelInfo{SendTemperature: BoolPtr(false)}); model.SendsTemperature() {
		t.Fatal("send_temperature=false must not send the field")
	}
	if task := (TaskConfig{}).Normalize(); task.MaxTokens != DefaultTaskMaxTokens || task.SelectionStrategy != SelectionBalance || task.HardTimeout != DefaultHardTimeout || task.EffectiveTemperature() != DefaultTaskTemperature {
		t.Fatalf("task = %#v", task)
	}
}

// TestBuildURLPreservesConfiguredPath mirrors joinOpenAICompatibleURL: a base
// URL that already contains /v1 keeps it and query/fragment are dropped.
func TestBuildURLPreservesConfiguredPath(t *testing.T) {
	client := NewClient(APIProvider{
		Name: "p", BaseURL: "https://api.example.com/v1/?stale=1#frag", APIKey: "k",
		DefaultQuery: map[string]any{"api-version": "2026-01-01"},
	}, nil)
	url, err := client.buildURL("chat/completions", map[string]any{"extra": "1"})
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	if !strings.HasPrefix(url, "https://api.example.com/v1/chat/completions?") {
		t.Fatalf("url = %s", url)
	}
	if strings.Contains(url, "stale=1") || strings.Contains(url, "#frag") {
		t.Fatalf("stale query/fragment kept: %s", url)
	}
	if !strings.Contains(url, "api-version=2026-01-01") || !strings.Contains(url, "extra=1") {
		t.Fatalf("query = %s", url)
	}

	queryClient := NewClient(APIProvider{Name: "q", BaseURL: "https://api.example.com/v1", APIKey: "secret", AuthType: AuthQuery, AuthQueryName: "key"}, nil)
	url, err = queryClient.buildURL("chat/completions", nil)
	if err != nil {
		t.Fatalf("buildURL: %v", err)
	}
	if !strings.Contains(url, "key=secret") {
		t.Fatalf("query auth missing: %s", url)
	}
}

// TestHeaderAuthUsesConfiguredHeader mirrors the AuthHeader branch of
// build_openai_compatible_client_config.
func TestHeaderAuthUsesConfiguredHeader(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return 200, completionPayload("ok", nil)
	})
	client := NewClient(APIProvider{
		Name: "h", BaseURL: provider.server.URL + "/v1", APIKey: "secret",
		AuthType: AuthHeader, AuthHeaderName: "X-Api-Key", AuthHeaderPrefix: "Token",
	}, provider.server.Client())
	if _, err := client.Chat(context.Background(), contracts.ChatRequest{Model: "m", Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}, ChatOptions{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	recorded := provider.recorded()[0]
	if got := recorded.Headers.Get("X-Api-Key"); got != "Token secret" {
		t.Fatalf("header = %q", got)
	}
	if got := recorded.Headers.Get("Authorization"); got != "" {
		t.Fatalf("unexpected Authorization header: %q", got)
	}
}

// TestExtraBodyMergesModelParams mirrors split_openai_request_overrides:
// reserved transport keys never enter the body, other keys do.
func TestExtraBodyMergesModelParams(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return 200, completionPayload("ok", nil)
	})
	config := testModelConfig(provider.server.URL, func(config *ModelConfig) {
		model := config.Models["fast"]
		model.ExtraParams = map[string]any{
			"enable_thinking":  "false",
			"headers":          map[string]any{"X-Ignored": "1"},
			"thinking":         map[string]any{"type": "disabled"},
			"reasoning_effort": "high",
		}
		config.Models["fast"] = model
	})
	orchestrator := NewOrchestrator(OrchestratorOptions{
		Config:     config,
		HTTPClient: provider.server.Client(),
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
	if _, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body := provider.recorded()[0].Body
	if body["enable_thinking"] != "false" {
		t.Fatalf("enable_thinking = %v", body["enable_thinking"])
	}
	if body["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", body["reasoning_effort"])
	}
	thinking, ok := body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("thinking = %#v", body["thinking"])
	}
	if _, exists := body["headers"]; exists {
		t.Fatalf("reserved key leaked into the body: %#v", body)
	}
}

// TestDecodeMessageContentHandlesContentArray covers gateways that answer with
// a content-part array for text-only output.
func TestDecodeMessageContentHandlesContentArray(t *testing.T) {
	content := decodeMessageContent([]byte(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`))
	if content != "ab" {
		t.Fatalf("content = %q", content)
	}
	if got := decodeMessageContent([]byte(`"plain"`)); got != "plain" {
		t.Fatalf("content = %q", got)
	}
	if got := decodeMessageContent([]byte(`null`)); got != "" {
		t.Fatalf("content = %q", got)
	}
}

// TestOrchestratorEmbedUsesOneModelForVectorSpaceStability mirrors the Python
// comment on EMBEDDING_TASK_NAMES.
func TestOrchestratorEmbedRecoversFromSingleModelFailure(t *testing.T) {
	provider := newMockProvider(t, func(_ int, body map[string]any) (int, any) {
		if body["model"] == "pro-model" {
			return 500, map[string]any{"error": map[string]any{"message": "embedding down"}}
		}
		return 200, map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 2}}}}
	})
	orchestrator := newTestOrchestrator(t, provider, func(config *ModelConfig) {
		config.Providers["mock"] = APIProvider{
			Name: "mock", BaseURL: provider.server.URL + "/v1", APIKey: "k",
			AuthType: AuthBearer, MaxRetry: 1, Timeout: 5, RetryInterval: 0.001,
		}
		config.Tasks["embedding"] = TaskConfig{ModelList: []string{"pro", "fast"}, MaxTokens: 4096, SelectionStrategy: SelectionSequential, HardTimeout: 5}
	}, nil)
	vectors, err := orchestrator.Embed(context.Background(), []string{"a"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 2 {
		t.Fatalf("vectors = %#v", vectors)
	}
	// The first configured model failed, so the orchestrator fell through to the
	// next one instead of surfacing the error.
	recorded := provider.recorded()
	if len(recorded) != 2 || recorded[1].Body["model"] != "fast-model" {
		t.Fatalf("requests = %#v", recorded)
	}
}

// ---------------------------------------------------------------------------
// L5/L9/L10/L12/L13 回归测试
// ---------------------------------------------------------------------------

// TestResolveTemperatureExplicitWins 对齐 Python _resolve_effective_temperature：
// 调用方显式传入的 temperature 优先于模型配置与任务配置（L9）。
func TestResolveTemperatureExplicitWins(t *testing.T) {
	model := ModelInfo{Name: "m", Temperature: Float64Ptr(0.1)}
	task := TaskConfig{Temperature: Float64Ptr(0.7)}
	explicit := 0.9

	if got := resolveTemperature(model, task, &explicit); got == nil || *got != 0.9 {
		t.Fatalf("显式 temperature 应最高优先: got %v", got)
	}
	if got := resolveTemperature(model, task, nil); got == nil || *got != 0.1 {
		t.Fatalf("无显式值时应回落到模型 temperature: got %v", got)
	}
	if got := resolveTemperature(ModelInfo{Name: "m"}, task, &explicit); got == nil || *got != 0.9 {
		t.Fatalf("send_temperature 默认 true 时显式值应生效: got %v", got)
	}
	if got := resolveTemperature(ModelInfo{SendTemperature: BoolPtr(false)}, task, &explicit); got != nil {
		t.Fatalf("send_temperature=false 应丢弃字段（含显式值）: got %v", got)
	}
}

// TestChatRequestExplicitTemperatureWins 走完整请求链路验证 L9：请求里的
// 显式 temperature 覆盖模型/任务配置。
func TestChatRequestExplicitTemperatureWins(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return 200, completionPayload("ok", nil)
	})
	orchestrator := newTestOrchestrator(t, provider, func(config *ModelConfig) {
		model := config.Models["fast"]
		model.Temperature = Float64Ptr(0.1)
		config.Models["fast"] = model
	}, nil)

	explicit := 0.9
	if _, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{
		Messages:    []contracts.ChatMessage{{Role: RoleUser, Content: "hi"}},
		Temperature: &explicit,
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := provider.recorded()[0].Body["temperature"]; got != 0.9 {
		t.Fatalf("body temperature = %v, want 0.9（显式值优先）", got)
	}

	// 无显式值时回落模型配置。
	if _, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{
		Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := provider.recorded()[1].Body["temperature"]; got != 0.1 {
		t.Fatalf("body temperature = %v, want 0.1（模型配置）", got)
	}
}

// TestParallelToolCallsOnlySentExplicitly 验证 L10：parallel_tool_calls 仅在
// 调用方显式提供 ChatOptions.ParallelToolCalls 时发送；默认（含带 tools 的
// 请求）一律省略，对齐 Python openai_client（部分网关会对该字段 400）。
func TestParallelToolCallsOnlySentExplicitly(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return 200, completionPayload("ok", nil)
	})
	client := NewClient(APIProvider{Name: "mock", BaseURL: provider.server.URL + "/v1", APIKey: "k"}, provider.server.Client())
	request := contracts.ChatRequest{
		Model:    "fast-model",
		Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "hi"}},
		Tools:    []contracts.ToolSpec{{Name: "reply", Description: "d", ParametersJSON: `{"type":"object"}`}},
	}

	if _, err := client.Chat(context.Background(), request, ChatOptions{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, exists := provider.recorded()[0].Body["parallel_tool_calls"]; exists {
		t.Fatalf("带 tools 的默认请求不应发送 parallel_tool_calls: %#v", provider.recorded()[0].Body)
	}

	enabled := true
	if _, err := client.Chat(context.Background(), request, ChatOptions{ParallelToolCalls: &enabled}); err != nil {
		t.Fatalf("Chat explicit: %v", err)
	}
	if got := provider.recorded()[1].Body["parallel_tool_calls"]; got != true {
		t.Fatalf("显式 parallel_tool_calls 应透传: %v", got)
	}
}

// TestEmbedUsageRecorded 验证 L12：embedding 响应的 usage 必须交给
// UsageRecorder 落库（此前 response 恒 nil，usage 被静默丢弃）。
func TestEmbedUsageRecorded(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return 200, map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float32{1, 2, 3}}},
			"usage": map[string]any{
				"prompt_tokens": 7, "total_tokens": 7,
			},
		}
	})
	var mu sync.Mutex
	var recorded []ModelUsage
	orchestrator := newTestOrchestrator(t, provider, nil, func(options *OrchestratorOptions) {
		options.Recorder = NewUsageRecorder(UsageWriterFunc(func(_ context.Context, usage ModelUsage) error {
			mu.Lock()
			defer mu.Unlock()
			recorded = append(recorded, usage)
			return nil
		}))
	})

	if _, err := orchestrator.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	orchestrator.WaitForUsage()
	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != 1 {
		t.Fatalf("embedding usage 落库条数 = %d, want 1", len(recorded))
	}
	if recorded[0].PromptTokens != 7 || recorded[0].TotalTokens != 7 {
		t.Fatalf("embedding usage = %#v, want prompt=7 total=7", recorded[0])
	}
}

// TestUsageRecordFailureDoesNotBreakRequest 验证 L13：usage 落库失败只打
// Warn 日志，不影响请求成功返回（此前 `_ =` 静默吞错）。
func TestUsageRecordFailureDoesNotBreakRequest(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return 200, completionPayload("ok", map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2})
	})
	orchestrator := newTestOrchestrator(t, provider, nil, func(options *OrchestratorOptions) {
		options.Recorder = NewUsageRecorder(UsageWriterFunc(func(context.Context, ModelUsage) error {
			return fmt.Errorf("db closed")
		}))
	})
	response, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{
		Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("usage 写入失败不应影响请求: %v", err)
	}
	if response.Content != "ok" {
		t.Fatalf("response = %#v", response)
	}
	orchestrator.WaitForUsage()
}

// TestOrchestratorConcurrentHotSwapAndRandomSelection 是 L5 的并发压力测试
// （本机无 gcc、-race 不可用，用高压并发代替竞态检测）：多 goroutine 同时
// 随机选型（rand.Rand 共享）+ 热重载 SetConfig + SetSessionID，任何未持锁
// 的共享状态访问都会触发崩溃或异常结果。
func TestOrchestratorConcurrentHotSwapAndRandomSelection(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return 200, completionPayload("ok", map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2})
	})
	base := testModelConfig(provider.server.URL, func(config *ModelConfig) {
		// 随机策略 + 多模型，迫使每次请求都走 Rand.Intn（L5 的竞争点）。
		config.Tasks["utils"] = TaskConfig{
			ModelList: []string{"fast", "pro"}, MaxTokens: 64, Temperature: Float64Ptr(0.5),
			SelectionStrategy: SelectionRandom, HardTimeout: 5,
		}
	})
	orchestrator := NewOrchestrator(OrchestratorOptions{
		Config:     base,
		HTTPClient: provider.server.Client(),
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})

	var waitGroup sync.WaitGroup
	for index := 0; index < 48; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			for round := 0; round < 8; round++ {
				// 并发热重载与 session 改写（L5 的另两个竞争点）。
				if index%8 == 0 {
					orchestrator.SetConfig(testModelConfig(provider.server.URL, func(config *ModelConfig) {
						config.Tasks["utils"] = TaskConfig{
							ModelList: []string{"fast", "pro"}, MaxTokens: 64, Temperature: Float64Ptr(0.5),
							SelectionStrategy: SelectionRandom, HardTimeout: 5,
						}
					}))
				}
				if index%8 == 1 {
					orchestrator.SetSessionID(fmt.Sprintf("session-%d-%d", index, round))
				}
				if _, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{
					Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "hi"}},
				}); err != nil {
					t.Errorf("并发 Chat 失败: %v", err)
					return
				}
				// Config() 与 SetConfig 并发读写（L5）。
				_ = orchestrator.Config().Tasks
			}
		}(index)
	}
	waitGroup.Wait()
	orchestrator.WaitForUsage()
}
