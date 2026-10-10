package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	contracts "oh-my-qqbot/internal"
)

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

type recordedRequest struct {
	Path    string
	Headers http.Header
	Body    map[string]any
}

type mockProvider struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
	handler  func(index int, body map[string]any) (int, any)
	counter  int
}

func newMockProvider(t *testing.T, handler func(index int, body map[string]any) (int, any)) *mockProvider {
	t.Helper()
	provider := &mockProvider{handler: handler}
	provider.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
		var body map[string]any
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		provider.mu.Lock()
		index := provider.counter
		provider.counter++
		provider.requests = append(provider.requests, recordedRequest{Path: request.URL.Path, Headers: request.Header.Clone(), Body: body})
		provider.mu.Unlock()

		status, payload := provider.handler(index, body)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		if payload != nil {
			_ = json.NewEncoder(writer).Encode(payload)
		}
	}))
	t.Cleanup(provider.server.Close)
	return provider
}

func (p *mockProvider) recorded() []recordedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedRequest(nil), p.requests...)
}

func completionPayload(content string, usage map[string]any) map[string]any {
	return map[string]any{
		"id":    "chatcmpl-1",
		"model": "test-model",
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
			"message": map[string]any{
				"role":    "assistant",
				"content": content,
			},
		}},
		"usage": usage,
	}
}

func toolCallPayload(name string, args string, usage map[string]any) map[string]any {
	return map[string]any{
		"id":    "chatcmpl-2",
		"model": "test-model",
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []map[string]any{{
					"id":   "call-1",
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args,
					},
				}},
			},
		}},
		"usage": usage,
	}
}

func testModelConfig(providerURL string, handlerCfg func(config *ModelConfig)) ModelConfig {
	config := ModelConfig{
		Providers: map[string]APIProvider{
			"mock": {
				Name:          "mock",
				BaseURL:       providerURL + "/v1",
				APIKey:        "secret",
				AuthType:      AuthBearer,
				MaxRetry:      2,
				Timeout:       5,
				RetryInterval: 0.001,
			},
		},
		Models: map[string]ModelInfo{
			"fast": {ModelIdentifier: "fast-model", Name: "fast", APIProvider: "mock", PriceIn: 1, PriceOut: 2},
			"pro":  {ModelIdentifier: "pro-model", Name: "pro", APIProvider: "mock", PriceIn: 12, PriceOut: 24, CachePriceIn: 1.2},
		},
		Tasks: map[string]TaskConfig{
			"utils":      {ModelList: []string{"fast"}, MaxTokens: 512, Temperature: Float64Ptr(0.5), SelectionStrategy: SelectionRandom, HardTimeout: 5},
			"planner":    {ModelList: []string{"fast", "pro"}, MaxTokens: 1024, Temperature: Float64Ptr(0.7), SelectionStrategy: SelectionSequential, HardTimeout: 5},
			"replyer":    {ModelList: []string{"fast", "pro"}, MaxTokens: 1024, Temperature: Float64Ptr(1), SelectionStrategy: SelectionRandom, HardTimeout: 5},
			"embedding":  {ModelList: []string{"pro", "fast"}, MaxTokens: 4096, SelectionStrategy: SelectionSequential, HardTimeout: 5},
			"fast_model": {ModelList: []string{}, HardTimeout: 5},
			"mid_memory": {ModelList: []string{}, HardTimeout: 5},
		},
	}
	if handlerCfg != nil {
		handlerCfg(&config)
	}
	return config
}

func newTestOrchestrator(t *testing.T, provider *mockProvider, mutate func(*ModelConfig), options func(*OrchestratorOptions)) *Orchestrator {
	t.Helper()
	cfg := testModelConfig(provider.server.URL, mutate)
	opts := OrchestratorOptions{
		Config:     cfg,
		HTTPClient: provider.server.Client(),
		Sleep:      func(context.Context, time.Duration) error { return nil },
		Rand:       rand.New(rand.NewSource(1)),
	}
	if options != nil {
		options(&opts)
	}
	return NewOrchestrator(opts)
}

// ---------------------------------------------------------------------------
// client tests
// ---------------------------------------------------------------------------

func TestChatSendsRequestAndParsesResponse(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, completionPayload("hello there", map[string]any{
			"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120,
			"prompt_cache_hit_tokens": 64, "prompt_cache_miss_tokens": 36,
		})
	})
	client := NewClient(APIProvider{Name: "mock", BaseURL: provider.server.URL + "/v1", APIKey: "k"}, provider.server.Client())
	temperature := 0.42
	response, err := client.Chat(context.Background(), contracts.ChatRequest{
		Model:       "fast-model",
		Messages:    []contracts.ChatMessage{{Role: RoleSystem, Content: "sys"}, {Role: RoleUser, Content: "hi"}},
		Temperature: &temperature,
		MaxTokens:   256,
		Tools: []contracts.ToolSpec{{
			Name:           "reply",
			Description:    "send a reply",
			ParametersJSON: `{"type":"object","properties":{"msg_id":{"type":"string"}},"required":["msg_id"]}`,
		}},
	}, ChatOptions{PromptCacheKey: "session-abc"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if response.Content != "hello there" || response.Model != "test-model" || response.FinishReason != "stop" {
		t.Fatalf("response = %#v", response)
	}
	if response.Usage.PromptTokens != 100 || response.Usage.CompletionTokens != 20 || response.Usage.TotalTokens != 120 {
		t.Fatalf("usage = %#v", response.Usage)
	}
	if !response.Usage.CacheEnabled || response.Usage.CacheHitTokens != 64 || response.Usage.CacheMissTokens != 36 {
		t.Fatalf("cache usage = %#v", response.Usage)
	}

	recorded := provider.recorded()[0]
	if recorded.Path != "/v1/chat/completions" {
		t.Fatalf("path = %s", recorded.Path)
	}
	if got := recorded.Headers.Get("Authorization"); got != "Bearer k" {
		t.Fatalf("authorization = %q", got)
	}
	if got := recorded.Headers.Get("X-Session-Affinity"); got != "session-abc" {
		t.Fatalf("session affinity = %q", got)
	}
	if recorded.Body["model"] != "fast-model" {
		t.Fatalf("model = %v", recorded.Body["model"])
	}
	if recorded.Body["temperature"] != 0.42 {
		t.Fatalf("temperature = %v", recorded.Body["temperature"])
	}
	if recorded.Body["max_tokens"] != float64(256) {
		t.Fatalf("max_tokens = %v", recorded.Body["max_tokens"])
	}
	// L10 修复后：parallel_tool_calls 仅在调用方显式提供时发送（Python 版
	// 从不发送该字段），带 tools 的默认请求不应出现。
	if _, exists := recorded.Body["parallel_tool_calls"]; exists {
		t.Fatalf("parallel_tool_calls must be omitted by default, got %v", recorded.Body["parallel_tool_calls"])
	}
	tools := recorded.Body["tools"].([]any)
	function := tools[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "reply" {
		t.Fatalf("tool = %#v", function)
	}
	parameters := function["parameters"].(map[string]any)
	if parameters["type"] != "object" {
		t.Fatalf("parameters = %#v", parameters)
	}
}

func TestChatToolCallRoundTrip(t *testing.T) {
	provider := newMockProvider(t, func(index int, body map[string]any) (int, any) {
		if index == 0 {
			return http.StatusOK, toolCallPayload("reply", `{"msg_id":"m12","set_quote":true}`, map[string]any{
				"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
			})
		}
		// Second round: the tool result must arrive as a `tool` role message
		// carrying the tool_call_id, and the assistant tool call must be echoed.
		messages := body["messages"].([]any)
		last := messages[len(messages)-1].(map[string]any)
		if last["role"] != "tool" || last["tool_call_id"] != "call-1" {
			return http.StatusBadRequest, map[string]any{"error": map[string]any{"message": fmt.Sprintf("bad tool message: %#v", last)}}
		}
		assistant := messages[len(messages)-2].(map[string]any)
		calls := assistant["tool_calls"].([]any)
		call := calls[0].(map[string]any)["function"].(map[string]any)
		if call["name"] != "reply" || !strings.Contains(call["arguments"].(string), "m12") {
			return http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "assistant tool call not echoed"}}
		}
		return http.StatusOK, completionPayload("done", map[string]any{"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15})
	})
	client := NewClient(APIProvider{Name: "mock", BaseURL: provider.server.URL + "/v1", APIKey: "k"}, provider.server.Client())
	ctx := context.Background()

	first, err := client.Chat(ctx, contracts.ChatRequest{
		Model:    "pro-model",
		Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "say hi"}},
		Tools:    []contracts.ToolSpec{{Name: "reply", ParametersJSON: `{"type":"object"}`}},
	}, ChatOptions{})
	if err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != "reply" || first.ToolCalls[0].ID != "call-1" {
		t.Fatalf("tool calls = %#v", first.ToolCalls)
	}
	if first.ToolCalls[0].ArgsJSON != `{"msg_id":"m12","set_quote":true}` {
		t.Fatalf("args = %s", first.ToolCalls[0].ArgsJSON)
	}
	if first.Content != "" || first.FinishReason != "tool_calls" {
		t.Fatalf("content = %q finish = %s", first.Content, first.FinishReason)
	}

	second, err := client.Chat(ctx, contracts.ChatRequest{
		Model: "pro-model",
		Messages: []contracts.ChatMessage{
			{Role: RoleUser, Content: "say hi"},
			{Role: RoleAssistant, ToolCalls: first.ToolCalls},
			{Role: RoleTool, ToolCallID: "call-1", Name: "reply", Content: `{"success":true}`},
		},
		Tools: []contracts.ToolSpec{{Name: "reply", ParametersJSON: `{"type":"object"}`}},
	}, ChatOptions{})
	if err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if second.Content != "done" {
		t.Fatalf("second content = %q", second.Content)
	}
}

func TestChatEmptyResponseIsRetryableError(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, completionPayload("   ", map[string]any{"prompt_tokens": 1})
	})
	client := NewClient(APIProvider{Name: "mock", BaseURL: provider.server.URL + "/v1"}, provider.server.Client())
	_, err := client.Chat(context.Background(), contracts.ChatRequest{Model: "m", Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}, ChatOptions{})
	if err == nil || !retryableError(err) {
		t.Fatalf("err = %v (retryable=%v)", err, retryableError(err))
	}
}

func TestChatHTTPErrors(t *testing.T) {
	tests := []struct {
		status    int
		retryable bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("status %d", test.status), func(t *testing.T) {
			provider := newMockProvider(t, func(int, map[string]any) (int, any) {
				return test.status, map[string]any{"error": map[string]any{"message": "boom"}}
			})
			client := NewClient(APIProvider{Name: "mock", BaseURL: provider.server.URL + "/v1"}, provider.server.Client())
			_, err := client.Chat(context.Background(), contracts.ChatRequest{Model: "m", Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}, ChatOptions{})
			if err == nil {
				t.Fatal("expected error")
			}
			var httpErr *HTTPError
			if !asHTTPError(err, &httpErr) {
				t.Fatalf("err type = %T (%v)", err, err)
			}
			if httpErr.StatusCode != test.status {
				t.Fatalf("status = %d", httpErr.StatusCode)
			}
			if httpErr.Retryable() != test.retryable {
				t.Fatalf("retryable = %v, want %v", httpErr.Retryable(), test.retryable)
			}
		})
	}
}

func TestChatTimeoutPropagatesAsDeadline(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		time.Sleep(150 * time.Millisecond)
		return http.StatusOK, completionPayload("late", nil)
	})
	client := NewClient(APIProvider{Name: "mock", BaseURL: provider.server.URL + "/v1"}, provider.server.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := client.Chat(ctx, contracts.ChatRequest{Model: "m", Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}, ChatOptions{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if ctx.Err() == nil {
		t.Fatalf("context should be done: %v", err)
	}
}

// ---------------------------------------------------------------------------
// usage parsing
// ---------------------------------------------------------------------------

func TestParseUsageCacheFields(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		prompt     int
		completion int
		total      int
		cacheOn    bool
		hit        int
		miss       int
	}{
		{
			name: "deepseek style hit/miss", prompt: 100, completion: 10, total: 110,
			cacheOn: true, hit: 80, miss: 20,
			raw: `{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}`,
		},
		{
			name: "openai style cached_tokens", prompt: 100, completion: 10, total: 110,
			cacheOn: true, hit: 30, miss: 70,
			raw: `{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_tokens_details":{"cached_tokens":30}}`,
		},
		{
			name: "hit only derives misses", prompt: 100, completion: 1, total: 101,
			cacheOn: true, hit: 25, miss: 75,
			raw: `{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101,"prompt_cache_hit_tokens":25}`,
		},
		{
			name: "no cache fields", prompt: 12, completion: 2, total: 14,
			cacheOn: false, hit: 0, miss: 0,
			raw: `{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}`,
		},
		{
			name: "missing usage", raw: ``,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := parseUsage(json.RawMessage(test.raw))
			if usage.PromptTokens != test.prompt || usage.CompletionTokens != test.completion || usage.TotalTokens != test.total {
				t.Fatalf("usage = %#v", usage)
			}
			if usage.CacheEnabled != test.cacheOn || usage.CacheHitTokens != test.hit || usage.CacheMissTokens != test.miss {
				t.Fatalf("cache = %#v", usage)
			}
		})
	}
}

func TestUsagePricingMirrorsPython(t *testing.T) {
	period := ModelPricePeriod{PriceIn: 12, PriceOut: 24, CachePriceIn: 1.2}
	// 1M prompt tokens全部命中 + 1M completion: 1.2 + 24
	usage := contracts.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000, TotalTokens: 2_000_000, CacheHitTokens: 1_000_000, CacheMissTokens: 0}
	cost := calculateCost(usage, period, ModelInfo{PriceIn: 12, PriceOut: 24})
	if diff := cost - (1.2 + 24); diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %v", cost)
	}
	// Without any cache accounting everything is priced as a miss.
	plain := contracts.Usage{PromptTokens: 500_000, CompletionTokens: 0}
	plainCost := CalculateInputCost(plain, period)
	if diff := plainCost - 6; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("plain cost = %v", plainCost)
	}
}

func TestResolvePricePeriodCrossesMidnight(t *testing.T) {
	model := ModelInfo{
		PriceIn: 1, PriceOut: 2, CachePriceIn: 0.1,
		PricePeriods: []ModelPricePeriod{
			{StartTime: "22:00", EndTime: "04:00", PriceIn: 5, PriceOut: 9, CachePriceIn: 0.5},
			{StartTime: "09:00", EndTime: "18:00", PriceIn: 3, PriceOut: 6, CachePriceIn: 0.3},
		},
	}
	tests := []struct {
		hour     int
		minute   int
		expectIn float64
	}{
		{23, 30, 5},
		{2, 0, 5},
		{4, 0, 1},
		{10, 0, 3},
		{17, 59, 3},
		{20, 0, 1},
	}
	for _, test := range tests {
		period := resolvePricePeriod(model, time.Date(2026, 2, 3, test.hour, test.minute, 0, 0, time.Local))
		if period.PriceIn != test.expectIn {
			t.Fatalf("hour %02d:%02d price_in = %v, want %v", test.hour, test.minute, period.PriceIn, test.expectIn)
		}
	}
}

func TestUsageRecorderWritesRow(t *testing.T) {
	var captured []ModelUsage
	recorder := NewUsageRecorder(UsageWriterFunc(func(_ context.Context, usage ModelUsage) error {
		captured = append(captured, usage)
		return nil
	}))
	started := time.Date(2026, 2, 3, 12, 0, 0, 0, time.Local)
	err := recorder.Record(context.Background(), RecordParams{
		Model:            ModelInfo{ModelIdentifier: "deepseek-v4-flash", Name: "ds-flash", PriceIn: 1, PriceOut: 2},
		Provider:         APIProvider{Name: "DeepSeek"},
		Usage:            contracts.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, CacheEnabled: true, CacheHitTokens: 800, CacheMissTokens: 200},
		RequestType:      "response",
		TaskName:         "replyer",
		SessionID:        "sid-1",
		TimeCost:         1.23456,
		RequestStartedAt: started,
		Success:          true,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("rows = %d", len(captured))
	}
	row := captured[0]
	if row.ModelName != "deepseek-v4-flash" || row.ModelAssignName != "ds-flash" || row.ProviderName != "DeepSeek" {
		t.Fatalf("row = %#v", row)
	}
	if row.SessionID != "sid-1" || row.TaskName != "replyer" || row.RequestType != "response" {
		t.Fatalf("row = %#v", row)
	}
	if !row.PromptCacheEnabled || row.PromptCacheHitTokens != 800 || row.PromptCacheMissTokens != 200 {
		t.Fatalf("cache row = %#v", row)
	}
	// (800/1e6)*0 + (200/1e6)*1 + (500/1e6)*2 = 0.0002 + 0.001 = 0.0012
	if diff := row.Cost - 0.0012; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %v", row.Cost)
	}
	if row.TimeCost != 1.235 {
		t.Fatalf("time cost = %v", row.TimeCost)
	}
}

func TestUsageRecorderRemembersCacheReporting(t *testing.T) {
	recorder := NewUsageRecorder(nil)
	recorder.Record(context.Background(), RecordParams{}) // nil writer is a no-op
	var rows []ModelUsage
	recorder.Writer = UsageWriterFunc(func(_ context.Context, usage ModelUsage) error {
		rows = append(rows, usage)
		return nil
	})
	params := RecordParams{
		Model:    ModelInfo{ModelIdentifier: "stepfun-x", Name: "sf"},
		Provider: APIProvider{Name: "StepFun"},
		Usage:    contracts.Usage{PromptTokens: 10, CacheEnabled: true, CacheHitTokens: 4, CacheMissTokens: 6},
	}
	if err := recorder.Record(context.Background(), params); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// A later response omits the cache fields; the model is remembered as
	// cache-reporting so statistics stay consistent (mirrors _CACHE_REPORTING_MODELS).
	params.Usage = contracts.Usage{PromptTokens: 10, TotalTokens: 10}
	if err := recorder.Record(context.Background(), params); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	if !rows[0].PromptCacheEnabled || !rows[1].PromptCacheEnabled {
		t.Fatalf("cache enabled flags = %v %v", rows[0].PromptCacheEnabled, rows[1].PromptCacheEnabled)
	}
}

// ---------------------------------------------------------------------------
// orchestrator tests
// ---------------------------------------------------------------------------

func TestOrchestratorSatisfiesLLMLayer(t *testing.T) {
	var layer contracts.LLMLayer = NewOrchestrator(OrchestratorOptions{})
	if layer == nil {
		t.Fatal("nil layer")
	}
}

func TestOrchestratorRoutesTaskToConfiguredModel(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, completionPayload("ok", map[string]any{"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6})
	})
	orchestrator := newTestOrchestrator(t, provider, nil, nil)
	response, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{
		Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if response.Content != "ok" {
		t.Fatalf("content = %q", response.Content)
	}
	body := provider.recorded()[0].Body
	if body["model"] != "fast-model" {
		t.Fatalf("model = %v", body["model"])
	}
	// Task defaults are applied (max_tokens 512 / temperature 0.5).
	if body["max_tokens"] != float64(512) {
		t.Fatalf("max_tokens = %v", body["max_tokens"])
	}
	if body["temperature"] != 0.5 {
		t.Fatalf("temperature = %v", body["temperature"])
	}
}

// TestOrchestratorRecordsUsageAsync covers the recorder wiring: the orchestrator
// hands the response usage to the UsageWriter (async, flushed by WaitForUsage).
func TestOrchestratorRecordsUsageAsync(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, completionPayload("ok", map[string]any{
			"prompt_tokens": 40, "completion_tokens": 10, "total_tokens": 50,
			"prompt_cache_hit_tokens": 30, "prompt_cache_miss_tokens": 10,
		})
	})
	var mu sync.Mutex
	var rows []ModelUsage
	orchestrator := newTestOrchestrator(t, provider, nil, func(options *OrchestratorOptions) {
		options.SessionID = "session-x"
		options.Recorder = NewUsageRecorder(UsageWriterFunc(func(_ context.Context, usage ModelUsage) error {
			mu.Lock()
			rows = append(rows, usage)
			mu.Unlock()
			return nil
		}))
	})
	if _, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	orchestrator.WaitForUsage()
	mu.Lock()
	defer mu.Unlock()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	row := rows[0]
	if row.ModelName != "fast-model" || row.ModelAssignName != "fast" || row.TaskName != "utils" {
		t.Fatalf("row = %#v", row)
	}
	if row.SessionID != "session-x" || row.RequestType != "response" {
		t.Fatalf("row = %#v", row)
	}
	if row.TotalTokens != 50 || !row.PromptCacheEnabled || row.PromptCacheHitTokens != 30 || row.PromptCacheMissTokens != 10 {
		t.Fatalf("row = %#v", row)
	}
}

func TestOrchestratorUsesFallbackTaskChain(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, completionPayload("ok", nil)
	})
	// fast_model/mid_memory have empty model lists → fall back to utils.
	orchestrator := newTestOrchestrator(t, provider, nil, nil)
	if _, err := orchestrator.Chat(context.Background(), "mid_memory", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatalf("mid_memory fallback failed: %v", err)
	}
	if model := provider.recorded()[0].Body["model"]; model != "fast-model" {
		t.Fatalf("model = %v", model)
	}
	if _, err := orchestrator.Chat(context.Background(), "fast_model", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatalf("fast_model fallback failed: %v", err)
	}
}

func TestOrchestratorRetriesWithinModelThenSwitches(t *testing.T) {
	var fastAttempts int32
	provider := newMockProvider(t, func(_ int, body map[string]any) (int, any) {
		if body["model"] == "fast-model" {
			atomic.AddInt32(&fastAttempts, 1)
			return http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": "fast down"}}
		}
		return http.StatusOK, completionPayload("from pro", map[string]any{"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9})
	})
	var attempts []TaskAttempt
	orchestrator := newTestOrchestrator(t, provider, nil, func(options *OrchestratorOptions) {
		options.OnAttempt = func(attempt TaskAttempt) { attempts = append(attempts, attempt) }
	})
	response, err := orchestrator.Chat(context.Background(), "planner", contracts.ChatRequest{
		Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if response.Content != "from pro" {
		t.Fatalf("content = %q", response.Content)
	}
	// max_retry=2 → two attempts on `fast` before moving to `pro`.
	if got := atomic.LoadInt32(&fastAttempts); got != 2 {
		t.Fatalf("fast attempts = %d, want 2", got)
	}
	recorded := provider.recorded()
	if len(recorded) != 3 {
		t.Fatalf("requests = %d", len(recorded))
	}
	if recorded[2].Body["model"] != "pro-model" {
		t.Fatalf("third request model = %v", recorded[2].Body["model"])
	}
	statuses := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		statuses = append(statuses, attempt.ModelName+":"+attempt.Status)
	}
	want := []string{"fast:retrying", "fast:failed", "pro:succeeded"}
	if strings.Join(statuses, ",") != strings.Join(want, ",") {
		t.Fatalf("attempts = %v, want %v", statuses, want)
	}
}

func TestOrchestratorDoesNotRetryHardHTTPErrors(t *testing.T) {
	var attempts int32
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		atomic.AddInt32(&attempts, 1)
		return http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "bad request"}}
	})
	orchestrator := newTestOrchestrator(t, provider, nil, nil)
	_, err := orchestrator.Chat(context.Background(), "planner", contracts.ChatRequest{
		Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	// Two models × one attempt each (400 is a hard error for the model).
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestOrchestratorRetriesEmptyResponses(t *testing.T) {
	var attempts int32
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		atomic.AddInt32(&attempts, 1)
		return http.StatusOK, completionPayload("", map[string]any{"prompt_tokens": 1})
	})
	orchestrator := newTestOrchestrator(t, provider, func(config *ModelConfig) {
		config.Tasks["utils"] = TaskConfig{ModelList: []string{"fast"}, MaxTokens: 100, Temperature: Float64Ptr(0.5), SelectionStrategy: SelectionRandom, HardTimeout: 5}
	}, nil)
	_, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (max_retry)", got)
	}
}

func TestOrchestratorHardTimeoutSwitchesModel(t *testing.T) {
	var slowAttempts int32
	provider := newMockProvider(t, func(_ int, body map[string]any) (int, any) {
		if body["model"] == "fast-model" {
			atomic.AddInt32(&slowAttempts, 1)
			time.Sleep(300 * time.Millisecond)
			return http.StatusOK, completionPayload("late", nil)
		}
		return http.StatusOK, completionPayload("from pro", map[string]any{"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4})
	})
	orchestrator := newTestOrchestrator(t, provider, func(config *ModelConfig) {
		config.Tasks["planner"] = TaskConfig{
			ModelList: []string{"fast", "pro"}, MaxTokens: 100, Temperature: Float64Ptr(0.5),
			SelectionStrategy: SelectionSequential, HardTimeout: 0.05,
		}
	}, nil)
	started := time.Now()
	response, err := orchestrator.Chat(context.Background(), "planner", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if response.Content != "from pro" {
		t.Fatalf("content = %q", response.Content)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("hard timeout did not cancel promptly: %v", elapsed)
	}
	if got := atomic.LoadInt32(&slowAttempts); got != 1 {
		t.Fatalf("slow attempts = %d", got)
	}
}

func TestOrchestratorRespectsContextCancellation(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		time.Sleep(200 * time.Millisecond)
		return http.StatusOK, completionPayload("late", nil)
	})
	orchestrator := newTestOrchestrator(t, provider, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := orchestrator.Chat(ctx, "utils", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errorsIs(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestOrchestratorEmbeddingUsesSequentialStrategy(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, map[string]any{
			"model": "pro-model",
			"data": []map[string]any{
				{"index": 0, "embedding": []float32{0.1, 0.2, 0.3}},
				{"index": 1, "embedding": []float32{0.4, 0.5, 0.6}},
			},
			"usage": map[string]any{"prompt_tokens": 8, "total_tokens": 8},
		}
	})
	var rows []ModelUsage
	orchestrator := newTestOrchestrator(t, provider, nil, func(options *OrchestratorOptions) {
		options.Recorder = NewUsageRecorder(UsageWriterFunc(func(_ context.Context, usage ModelUsage) error {
			rows = append(rows, usage)
			return nil
		}))
	})
	vectors, err := orchestrator.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 2 || len(vectors[0]) != 3 || vectors[1][0] != 0.4 {
		t.Fatalf("vectors = %#v", vectors)
	}
	// Embedding tasks always take the first configured model ("pro"), even
	// though the task strategy is sequential and pro is first in the list.
	recorded := provider.recorded()[0]
	if recorded.Path != "/v1/embeddings" {
		t.Fatalf("path = %s", recorded.Path)
	}
	if recorded.Body["model"] != "pro-model" || recorded.Body["encoding_format"] != "float" {
		t.Fatalf("body = %#v", recorded.Body)
	}
	inputs := recorded.Body["input"].([]any)
	if len(inputs) != 2 || inputs[0] != "a" {
		t.Fatalf("input = %#v", inputs)
	}
}

func TestEmbeddingsSortByIndex(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"index": 1, "embedding": []float32{2}},
				{"index": 0, "embedding": []float32{1}},
			},
		}
	})
	client := NewClient(APIProvider{Name: "mock", BaseURL: provider.server.URL + "/v1"}, provider.server.Client())
	vectors, err := client.Embeddings(context.Background(), ModelInfo{ModelIdentifier: "emb"}, []string{"a", "b"}, EmbeddingOptions{})
	if err != nil {
		t.Fatalf("Embeddings: %v", err)
	}
	if len(vectors) != 2 || vectors[0][0] != 1 || vectors[1][0] != 2 {
		t.Fatalf("vectors = %#v", vectors)
	}
}

func TestEmbeddingsCountMismatchIsParseError(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1}}}}
	})
	client := NewClient(APIProvider{Name: "mock", BaseURL: provider.server.URL + "/v1"}, provider.server.Client())
	_, err := client.Embeddings(context.Background(), ModelInfo{ModelIdentifier: "emb"}, []string{"a", "b"}, EmbeddingOptions{})
	if err == nil || !retryableError(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestOrchestratorBalanceSelectionPrefersCheapestHistory(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, completionPayload("ok", map[string]any{"prompt_tokens": 1, "total_tokens": 1})
	})
	orchestrator := newTestOrchestrator(t, provider, func(config *ModelConfig) {
		config.Tasks["utils"] = TaskConfig{ModelList: []string{"fast", "pro"}, MaxTokens: 100, Temperature: Float64Ptr(0.5), SelectionStrategy: SelectionBalance, HardTimeout: 5}
	}, nil)
	// First call takes "fast" (both scores are 0 → first in list wins).
	if _, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	// "fast" now has tokens but a negative usage_penalty, "pro" a positive
	// usage_penalty → the balance key favours "pro".
	if _, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	recorded := provider.recorded()
	if recorded[0].Body["model"] != "fast-model" {
		t.Fatalf("first model = %v", recorded[0].Body["model"])
	}
	if recorded[1].Body["model"] != "pro-model" {
		t.Fatalf("second model = %v (balance key not applied)", recorded[1].Body["model"])
	}
}

func TestOrchestratorExplicitModelOverridesRouting(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, completionPayload("ok", nil)
	})
	orchestrator := newTestOrchestrator(t, provider, nil, nil)
	response, err := orchestrator.execute(context.Background(), executeParams{
		Task:        "utils",
		RequestType: "response",
		ModelName:   "pro",
		Request:     contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if response.Response.Content != "ok" {
		t.Fatalf("content = %q", response.Response.Content)
	}
	if model := provider.recorded()[0].Body["model"]; model != "pro-model" {
		t.Fatalf("model = %v", model)
	}
}

func TestOrchestratorPrefixMessagesArePrepended(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, completionPayload("ok", nil)
	})
	orchestrator := newTestOrchestrator(t, provider, nil, func(options *OrchestratorOptions) {
		options.PrefixMessages = []contracts.ChatMessage{{Role: RoleSystem, Content: "persona"}}
	})
	if _, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	messages := provider.recorded()[0].Body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	first := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "persona" {
		t.Fatalf("prefix = %#v", first)
	}
}

func TestOrchestratorUnknownTaskFails(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, completionPayload("ok", nil)
	})
	orchestrator := newTestOrchestrator(t, provider, nil, nil)
	if _, err := orchestrator.Chat(context.Background(), "nope", contracts.ChatRequest{}); err == nil {
		t.Fatal("expected error for unknown task")
	}
	if _, err := orchestrator.Chat(context.Background(), "", contracts.ChatRequest{}); err == nil {
		t.Fatal("expected error for empty task")
	}
}

func TestOrchestratorSendTemperatureDisabledOmitsField(t *testing.T) {
	provider := newMockProvider(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, completionPayload("ok", nil)
	})
	orchestrator := newTestOrchestrator(t, provider, func(config *ModelConfig) {
		model := config.Models["fast"]
		model.SendTemperature = BoolPtr(false)
		config.Models["fast"] = model
	}, nil)
	if _, err := orchestrator.Chat(context.Background(), "utils", contracts.ChatRequest{Messages: []contracts.ChatMessage{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, present := provider.recorded()[0].Body["temperature"]; present {
		t.Fatalf("temperature should be omitted: %#v", provider.recorded()[0].Body)
	}
}

// ---------------------------------------------------------------------------
// context item tests
// ---------------------------------------------------------------------------

func TestContextItemBuilderMirrorsPython(t *testing.T) {
	item, err := NewContextItemBuilder().AddTextPart("hello").AddTextPart(" world").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if item.Kind != ItemKindUser || item.Role() != RoleUser {
		t.Fatalf("item = %#v", item)
	}
	if item.Text() != "hello world" {
		t.Fatalf("text = %q", item.Text())
	}
	if item.Meta.ItemID == "" {
		t.Fatal("item id must be generated")
	}

	if _, err := NewContextItemBuilder().SetRole(RoleSystem).Build(); err == nil {
		t.Fatal("empty parts should fail")
	}
	if _, err := NewContextItemBuilder().AddTextPart("x").SetToolCallID("call").Build(); err == nil {
		t.Fatal("tool call id requires the tool role")
	}
	toolItem, err := NewContextItemBuilder().SetRole(RoleTool).AddTextPart("result").SetToolCallID("call-1").SetToolName("reply").Build()
	if err != nil {
		t.Fatalf("tool Build: %v", err)
	}
	if toolItem.Kind != ItemKindToolResult || toolItem.CallID != "call-1" || toolItem.Output != "result" || !toolItem.Success {
		t.Fatalf("tool item = %#v", toolItem)
	}
	if toolItem.Role() != RoleTool {
		t.Fatalf("tool role = %s", toolItem.Role())
	}

	imageItem, err := NewContextItemBuilder().AddImagePart("png", "AAAA").Build()
	if err != nil {
		t.Fatalf("image Build: %v", err)
	}
	if imageItem.Parts[0].Image.DataURL() != "data:image/png;base64,AAAA" {
		t.Fatalf("data url = %s", imageItem.Parts[0].Image.DataURL())
	}
	if _, err := NewContextImagePart("bmp", "AAAA"); err == nil {
		t.Fatal("unsupported image format must fail")
	}
	if format := (ContextImagePart{ImageFormat: "jpg"}).NormalizedImageFormat(); format != "jpeg" {
		t.Fatalf("normalized format = %s", format)
	}
}

func TestItemsToMessagesProjection(t *testing.T) {
	call, err := NewFunctionCallItem("turn-1", "call-7", "query_memory", map[string]any{"q": "x"})
	if err != nil {
		t.Fatalf("NewFunctionCallItem: %v", err)
	}
	items := []*ContextItem{
		NewSystemItem("sys"),
		NewUserItem("hi"),
		call,
		NewToolResultItem("call-7", "memory result", "query_memory"),
	}
	messages := ItemsToMessages(items)
	if len(messages) != 4 {
		t.Fatalf("messages = %d", len(messages))
	}
	if messages[2].Role != RoleAssistant || len(messages[2].ToolCalls) != 1 || messages[2].ToolCalls[0].Name != "query_memory" {
		t.Fatalf("assistant projection = %#v", messages[2])
	}
	if messages[2].ToolCalls[0].ArgsJSON != `{"q":"x"}` {
		t.Fatalf("args = %s", messages[2].ToolCalls[0].ArgsJSON)
	}
	if messages[3].Role != RoleTool || messages[3].ToolCallID != "call-7" || messages[3].Content != "memory result" {
		t.Fatalf("tool projection = %#v", messages[3])
	}
	if _, err := NewFunctionCallItem("", "call-1", "f", nil); err == nil {
		t.Fatal("function call requires a logical turn id")
	}
}

func TestMessagesToItemsRoundTrip(t *testing.T) {
	messages := []contracts.ChatMessage{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "hi", Images: []contracts.ImagePart{{Base64: "data:image/jpeg;base64,AAAA"}}},
		{Role: RoleAssistant, ToolCalls: []contracts.ToolCall{{ID: "c1", Name: "reply", ArgsJSON: `{"a":1}`}}},
		{Role: RoleTool, ToolCallID: "c1", Name: "reply", Content: "done"},
	}
	items := MessagesToItems(messages)
	if len(items) != 4 {
		t.Fatalf("items = %d", len(items))
	}
	if items[1].Kind != ItemKindUser || len(items[1].Parts) != 2 {
		t.Fatalf("user item = %#v", items[1])
	}
	if items[1].Parts[1].Image.ImageFormat != "jpeg" {
		t.Fatalf("image format = %s", items[1].Parts[1].Image.ImageFormat)
	}
	if items[2].Kind != ItemKindFunctionCall || items[2].ToolCall.FuncName != "reply" {
		t.Fatalf("function call item = %#v", items[2])
	}
	if items[3].Kind != ItemKindToolResult || items[3].CallID != "c1" || items[3].Output != "done" {
		t.Fatalf("tool item = %#v", items[3])
	}
}

func TestTaskConfigFallbackAndNormalize(t *testing.T) {
	config := ModelConfig{
		Tasks: map[string]TaskConfig{
			"utils":           {ModelList: []string{"a"}},
			"fast_model":      {},
			"mid_memory":      {},
			"learner":         {},
			"image_embedding": {},
		},
	}
	task, effective, err := config.TaskConfigFor("mid_memory")
	if err != nil {
		t.Fatalf("mid_memory: %v", err)
	}
	if effective != "utils" || task.MaxTokens != DefaultTaskMaxTokens || task.EffectiveTemperature() != DefaultTaskTemperature {
		t.Fatalf("task = %#v effective = %s", task, effective)
	}
	if _, _, err := config.TaskConfigFor("image_embedding"); err == nil {
		t.Fatal("image_embedding without models must fail")
	}
	if _, _, err := config.TaskConfigFor(""); err == nil {
		t.Fatal("empty task name must fail")
	}
}

// asHTTPError is a tiny errors.As wrapper keeping the test imports lean.
func asHTTPError(err error, target **HTTPError) bool {
	for err != nil {
		if typed, ok := err.(*HTTPError); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func errorsIs(err error, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
