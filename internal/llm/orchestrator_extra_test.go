package llm

import (
	"context"
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
