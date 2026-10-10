package qq

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// 假 REST 服务
// ---------------------------------------------------------------------------

// fakeAPI 模拟开放平台的 REST 接口。
type fakeAPI struct {
	mu sync.Mutex

	server *httptest.Server

	// tokenCalls 记录换取 token 的次数。
	tokenCalls int
	// tokenBody 记录最近一次换取 token 的请求体。
	tokenBody map[string]string
	// tokenFailures 让前 N 次换取 token 失败。
	tokenFailures int32
	// tokenTTL 是返回的 expires_in (秒)。
	tokenTTL string

	// posts 记录每次打到 /messages 的请求体 (含被去重拒收的那次)。
	posts []map[string]any
	// paths 记录所有请求路径。
	paths []string
	// authHeaders 记录每次带上的 Authorization 头。
	authHeaders []string

	// dedup 让前 N 条 /messages 回 40054005。
	dedup int32
	// unauthorized 让前 N 条 /messages 回 401。
	unauthorized int32
	// rejectCode 非 0 时让所有 /messages 回这个业务错误码。
	rejectCode int
	// gatewayURL 是 /gateway 返回的地址。
	gatewayURL string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	api := &fakeAPI{tokenTTL: "7200", gatewayURL: "wss://gateway.example/ws"}
	mux := http.NewServeMux()

	writeJSON := func(w http.ResponseWriter, status int, payload any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}

	mux.HandleFunc("/app/getAppAccessToken", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		api.mu.Lock()
		api.tokenCalls++
		api.tokenBody = map[string]string{}
		_ = json.Unmarshal(body, &api.tokenBody)
		ttl := api.tokenTTL
		fail := api.tokenFailures > 0
		if fail {
			api.tokenFailures--
		}
		api.mu.Unlock()
		if fail {
			writeJSON(w, http.StatusOK, map[string]any{"message": "invalid secret", "code": 4001})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "TOKEN_" + itoa(api.tokenCalls), "expires_in": ttl})
	})

	mux.HandleFunc("/gateway", func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		api.paths = append(api.paths, "GET /gateway")
		api.authHeaders = append(api.authHeaders, r.Header.Get("Authorization"))
		url := api.gatewayURL
		api.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"url": url})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		api.mu.Lock()
		api.paths = append(api.paths, r.Method+" "+r.URL.Path)
		api.authHeaders = append(api.authHeaders, r.Header.Get("Authorization"))
		api.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			api.mu.Lock()
			api.posts = append(api.posts, body)
			dedup := api.dedup > 0
			if dedup {
				api.dedup--
			}
			unauthorized := api.unauthorized > 0
			if unauthorized {
				api.unauthorized--
			}
			reject := api.rejectCode
			n := len(api.posts)
			api.mu.Unlock()
			if unauthorized {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "token expired"})
				return
			}
			if dedup {
				// 官方的去重错误是 2xx 带 code (也有实现回 4xx), 这里用 200+code。
				writeJSON(w, http.StatusOK, map[string]any{"message": "消息被去重，请检查请求msgseq", "code": 40054005})
				return
			}
			if reject != 0 {
				writeJSON(w, http.StatusOK, map[string]any{"message": "主动消息失败, 无权限", "code": reject})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id":       "MSG_" + itoa(n),
				"ext_info": map[string]any{"ref_idx": "REFIDX_OUT_" + itoa(n)},
			})
		default:
			// B1: 专门提供一个返回 JSON 数组的端点。
			if strings.HasSuffix(r.URL.Path, "/array") {
				writeJSON(w, http.StatusOK, []map[string]any{{"id": "G1"}, {"id": "G2"}})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		}
	})

	api.server = httptest.NewServer(mux)
	t.Cleanup(api.server.Close)
	return api
}

func itoa(value int) string {
	if value <= 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

func newTestClient(t *testing.T, api *fakeAPI) *APIClient {
	t.Helper()
	client := NewAPIClient(APIOptions{
		AppID:            "APPID_1",
		AppSecret:        "SECRET_1",
		APIBaseOverride:  api.server.URL,
		TokenURLOverride: api.server.URL + "/app/getAppAccessToken",
	})
	return client
}

func (a *fakeAPI) postsSnapshot() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]map[string]any, len(a.posts))
	copy(out, a.posts)
	return out
}

func (a *fakeAPI) tokenCallCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokenCalls
}

func (a *fakeAPI) setDedup(n int32)        { atomic.StoreInt32(&a.dedup, n) }
func (a *fakeAPI) setUnauthorized(n int32) { atomic.StoreInt32(&a.unauthorized, n) }

func (a *fakeAPI) setReject(code int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rejectCode = code
}

// ---------------------------------------------------------------------------
// token 缓存
// ---------------------------------------------------------------------------

// TestTokenFetchAndCache 校验 token 换取一次后被缓存, 且请求体带 appId/clientSecret。
func TestTokenFetchAndCache(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)

	for i := 0; i < 5; i++ {
		header, err := client.AuthHeader(context.Background())
		if err != nil {
			t.Fatalf("换取 token 失败: %v", err)
		}
		if header != "QQBot TOKEN_1" {
			t.Fatalf("Authorization 头不符: %q", header)
		}
	}
	if got := api.tokenCallCount(); got != 1 {
		t.Fatalf("token 应只换一次, got %d", got)
	}
	api.mu.Lock()
	body := api.tokenBody
	api.mu.Unlock()
	if body["appId"] != "APPID_1" || body["clientSecret"] != "SECRET_1" {
		t.Fatalf("token 请求体不符: %+v", body)
	}
}

// TestTokenRefreshBeforeExpiry 校验 token 会在到期前 (提前 2 分钟) 刷新。
func TestTokenRefreshBeforeExpiry(t *testing.T) {
	api := newFakeAPI(t)
	clock := newFixedClock(testStart())
	client := NewAPIClient(APIOptions{
		AppID:            "APPID_1",
		AppSecret:        "SECRET_1",
		APIBaseOverride:  api.server.URL,
		TokenURLOverride: api.server.URL + "/app/getAppAccessToken",
		Now:              clock.Now,
	})
	// expires_in=125s, 扣掉 2 分钟刷新余量后只剩 5 秒寿命 → 推进 10 秒后必须换新。
	api.mu.Lock()
	api.tokenTTL = "125"
	api.mu.Unlock()

	if _, err := client.AuthHeader(context.Background()); err != nil {
		t.Fatalf("换取 token 失败: %v", err)
	}
	clock.Advance(10 * time.Second)
	if _, err := client.AuthHeader(context.Background()); err != nil {
		t.Fatalf("换取 token 失败: %v", err)
	}
	if got := api.tokenCallCount(); got != 2 {
		t.Fatalf("临近过期时应重新换取 token, got %d", got)
	}

	// 长 TTL: 剩余寿命充足 → 复用缓存。
	longLived := NewAPIClient(APIOptions{
		AppID:            "APPID_1",
		AppSecret:        "SECRET_1",
		APIBaseOverride:  api.server.URL,
		TokenURLOverride: api.server.URL + "/app/getAppAccessToken",
		Now:              clock.Now,
	})
	api.mu.Lock()
	api.tokenTTL = "7200"
	api.mu.Unlock()
	before := api.tokenCallCount()
	for i := 0; i < 3; i++ {
		if _, err := longLived.AuthHeader(context.Background()); err != nil {
			t.Fatalf("换取 token 失败: %v", err)
		}
	}
	if got := api.tokenCallCount() - before; got != 1 {
		t.Fatalf("长 TTL 应复用缓存, 新增调用 %d", got)
	}
}

// TestTokenMissingCredentials 校验缺凭据时报错。
func TestTokenMissingCredentials(t *testing.T) {
	api := newFakeAPI(t)
	client := NewAPIClient(APIOptions{
		AppID:            "",
		AppSecret:        "",
		APIBaseOverride:  api.server.URL,
		TokenURLOverride: api.server.URL + "/app/getAppAccessToken",
	})
	if _, err := client.AuthHeader(context.Background()); err == nil {
		t.Fatal("缺凭据应报错")
	}
	client.InvalidateToken()
	if _, err := client.AuthHeader(context.Background()); err == nil {
		t.Fatal("Invalidate 后仍缺凭据应报错")
	}
}

// ---------------------------------------------------------------------------
// 发送: 端点 / 双模 / msg_seq / 重试
// ---------------------------------------------------------------------------

// TestSendGroupAndUserEndpoints 校验群与单聊的端点与请求体。
func TestSendGroupAndUserEndpoints(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)

	// 群被动回复。
	_, err := client.Send(context.Background(), SendRequest{
		Target:   OutboundTarget{GroupID: "GROUP_OPENID_1"},
		Text:     "你好",
		Decision: SendDecision{Mode: ModePassive, TriggerID: "TRIGGER_1", Seq: 1},
	})
	if err != nil {
		t.Fatalf("群发送失败: %v", err)
	}
	// 单聊主动消息。
	_, err = client.Send(context.Background(), SendRequest{
		Target:   OutboundTarget{UserID: "USER_OPENID_1"},
		Text:     "你好",
		Decision: SendDecision{Mode: ModeProactive},
	})
	if err != nil {
		t.Fatalf("单聊发送失败: %v", err)
	}

	posts := api.postsSnapshot()
	if len(posts) != 2 {
		t.Fatalf("应有 2 次发送, got %d", len(posts))
	}
	if posts[0]["msg_type"] != float64(0) || posts[0]["content"] != "你好" {
		t.Fatalf("群消息体不符: %+v", posts[0])
	}
	if posts[0]["msg_id"] != "TRIGGER_1" || posts[0]["msg_seq"] != float64(1) {
		t.Fatalf("被动回复应带 msg_id + msg_seq: %+v", posts[0])
	}
	if posts[1]["msg_id"] != nil || posts[1]["msg_seq"] != nil {
		t.Fatalf("主动消息不应带 msg_id/msg_seq: %+v", posts[1])
	}
	api.mu.Lock()
	paths := append([]string(nil), api.paths...)
	api.mu.Unlock()
	want := []string{"POST /v2/groups/GROUP_OPENID_1/messages", "POST /v2/users/USER_OPENID_1/messages"}
	for _, item := range want {
		if !containsString(paths, item) {
			t.Fatalf("缺少端点 %q, got %v", item, paths)
		}
	}
}

// TestSendMsgSeqDedupRetry 校验 40054005 换 msg_seq 重试一次并通过。
func TestSendMsgSeqDedupRetry(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)
	api.setDedup(1)

	resp, err := client.Send(context.Background(), SendRequest{
		Target:   OutboundTarget{GroupID: "G1"},
		Text:     "第一句",
		Decision: SendDecision{Mode: ModePassive, TriggerID: "T1", Seq: 1},
	})
	if err != nil {
		t.Fatalf("换 seq 重试后应成功: %v", err)
	}
	posts := api.postsSnapshot()
	if len(posts) != 2 {
		t.Fatalf("应重发一次 (共 2 次请求), got %d", len(posts))
	}
	if posts[0]["msg_seq"] != float64(1) {
		t.Fatalf("首次应为 seq=1: %+v", posts[0])
	}
	if posts[1]["msg_seq"] != float64(2) {
		t.Fatalf("重试应换 seq=2: %+v", posts[1])
	}
	// 除 seq 之外的字段必须保持一致, 否则会发成两条不同的消息。
	for _, key := range []string{"content", "msg_id", "msg_type"} {
		if posts[0][key] != posts[1][key] {
			t.Fatalf("重试时 %s 不应变化: %v vs %v", key, posts[0][key], posts[1][key])
		}
	}
	if !resp.RetrySeq || resp.Seq != 2 {
		t.Fatalf("响应应标记重试并带上新 seq: %+v", resp)
	}
}

// TestSendDedupRetryStillFails 校验去重重试仍失败时保留原始错误。
func TestSendDedupRetryStillFails(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)
	api.setDedup(2) // 首次 + 重试都命中去重

	_, err := client.Send(context.Background(), SendRequest{
		Target:   OutboundTarget{GroupID: "G1"},
		Text:     "x",
		Decision: SendDecision{Mode: ModePassive, TriggerID: "T1", Seq: 1},
	})
	if err == nil {
		t.Fatal("两次都命中去重应失败")
	}
	if !IsAPIErrorCode(err, ErrCodeDedup) {
		t.Fatalf("应命中 40054005: %v", err)
	}
	if posts := api.postsSnapshot(); len(posts) != 2 {
		t.Fatalf("应只重试一次, got %d", len(posts))
	}
}

// TestSendUnauthorizedRefreshesToken 校验 401 时刷 token 重试一次。
func TestSendUnauthorizedRefreshesToken(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)
	api.setUnauthorized(1)

	resp, err := client.Send(context.Background(), SendRequest{
		Target:   OutboundTarget{GroupID: "G1"},
		Text:     "你好",
		Decision: SendDecision{Mode: ModeProactive},
	})
	if err != nil {
		t.Fatalf("401 后应刷新 token 重试成功: %v", err)
	}
	if resp.MessageID == "" {
		t.Fatal("应返回消息 id")
	}
	if got := api.tokenCallCount(); got != 2 {
		t.Fatalf("应重新换取一次 token (共 2 次), got %d", got)
	}
	posts := api.postsSnapshot()
	if len(posts) != 2 {
		t.Fatalf("应重试一次发送, got %d", len(posts))
	}
}

// TestSendProactiveRejectedByPlatform 校验平台拒绝主动消息 (40034105) 的错误语义。
func TestSendProactiveRejectedByPlatform(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)
	api.setReject(40034105)

	_, err := client.Send(context.Background(), SendRequest{
		Target:   OutboundTarget{GroupID: "G1"},
		Text:     "主动消息",
		Decision: SendDecision{Mode: ModeProactive},
	})
	if err == nil {
		t.Fatal("平台拒绝应返回错误")
	}
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("应能取出 *APIError, got %T: %v", err, apiErr)
	}
	if apiErr.Code != "40034105" || apiErr.CodeInt() != 40034105 {
		t.Fatalf("错误码不符: %+v", apiErr)
	}
	// 平台已明确处理过 → 视为已送达, 不回滚额度。
	sendErr, ok := err.(*APISendError)
	if !ok {
		t.Fatalf("应是 *APISendError, got %T", err)
	}
	if sendErr.How != SentDelivered {
		t.Fatalf("业务错误码应判定为已送达, got %v", sendErr.How)
	}
	if sendErr.Code != "40034105" {
		t.Fatalf("应带错误码: %q", sendErr.Code)
	}
}

// TestSendRefIdxAndSentLedger 校验发送响应 ext_info.ref_idx 与自发台账登记。
func TestSendRefIdxAndSentLedger(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)

	resp, err := client.Send(context.Background(), SendRequest{
		Target:   OutboundTarget{GroupID: "G1"},
		Text:     "我发的",
		Decision: SendDecision{Mode: ModeProactive},
	})
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if resp.RefIdx == "" || !IsRefKey(resp.RefIdx) {
		t.Fatalf("应返回 REFIDX_ 引用键: %q", resp.RefIdx)
	}

	// 绑定台账后, 成功发送会自动登记, 供回推识别。
	sent := NewSentLedger(0, 0)
	client.SetSentLedger(sent)
	if _, err := client.Send(context.Background(), SendRequest{
		Target:   OutboundTarget{GroupID: "G1"},
		Text:     "第二条",
		Decision: SendDecision{Mode: ModeProactive},
	}); err != nil {
		t.Fatalf("第二次发送失败: %v", err)
	}
	if !sent.Matches("G1", "MSG_2", "第二条") {
		t.Fatal("成功发送应登记进自发台账")
	}
	if sent.Matches("G1", "MSG_2", "第二条") {
		t.Fatal("同一条记录不应被认领两次")
	}
}

// TestSendEmptyPayload 校验空内容被拒。
func TestSendEmptyPayload(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)
	tests := []struct {
		name string
		req  SendRequest
	}{
		{"空目标", SendRequest{Text: "x"}},
		{"空内容", SendRequest{Target: OutboundTarget{GroupID: "G1"}}},
	}
	for _, item := range tests {
		t.Run(item.name, func(t *testing.T) {
			if _, err := client.Send(context.Background(), item.req); err == nil {
				t.Fatal("应被拒绝")
			}
		})
	}
}

// TestMessageEndpoint 校验端点生成。
func TestMessageEndpoint(t *testing.T) {
	client := NewAPIClient(APIOptions{AppID: "A", AppSecret: "S"})
	endpoint, isGroup, err := client.MessageEndpoint(OutboundTarget{GroupID: "G 1"})
	if err != nil || !isGroup || endpoint != APIBase+"/v2/groups/G%201/messages" {
		t.Fatalf("群端点不符: %q isGroup=%v err=%v", endpoint, isGroup, err)
	}
	endpoint, isGroup, err = client.MessageEndpoint(OutboundTarget{UserID: "U1"})
	if err != nil || isGroup || endpoint != APIBase+"/v2/users/U1/messages" {
		t.Fatalf("单聊端点不符: %q isGroup=%v err=%v", endpoint, isGroup, err)
	}
	if _, _, err := client.MessageEndpoint(OutboundTarget{}); err == nil {
		t.Fatal("空目标应报错")
	}
}

// TestSandboxAPIBase 校验沙箱域名切换。
func TestSandboxAPIBase(t *testing.T) {
	client := NewAPIClient(APIOptions{AppID: "A", AppSecret: "S", Sandbox: true})
	if client.APIBase() != SandboxAPIBase {
		t.Fatalf("沙箱域名不符: %q", client.APIBase())
	}
	client.SetAPIBaseOverride("http://127.0.0.1:9/")
	if client.APIBase() != "http://127.0.0.1:9" {
		t.Fatalf("覆写域名不符: %q", client.APIBase())
	}
}

// TestGatewayURLFetch 校验 GET /gateway 的解析。
func TestGatewayURLFetch(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)
	got, err := client.GatewayURL(context.Background())
	if err != nil || got != "wss://gateway.example/ws" {
		t.Fatalf("网关地址不符: %q err=%v", got, err)
	}
}

// TestCallAPI 校验透传接口的 method / path / query 处理。
func TestCallAPI(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)
	if _, err := client.CallAPI(context.Background(), "GET /users/@me/guilds", map[string]any{"limit": 10}); err != nil {
		t.Fatalf("CallAPI 失败: %v", err)
	}
	api.mu.Lock()
	paths := append([]string(nil), api.paths...)
	api.mu.Unlock()
	if !containsString(paths, "GET /users/@me/guilds") {
		t.Fatalf("路径不符: %v", paths)
	}
	if _, err := client.CallAPI(context.Background(), "", nil); err == nil {
		t.Fatal("空 action 应报错")
	}
}

// TestSendUnauthorizedTwiceReturnsError 校验 A1: 连续两次 401 必须显式报错 ——
// 旧实现循环耗尽后返回 nil 错误, 上层把失败当成功 (空 MessageID), 已扣的额度
// / 被动次数也不回滚。
func TestSendUnauthorizedTwiceReturnsError(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)
	api.setUnauthorized(2)

	_, err := client.Send(context.Background(), SendRequest{
		Target:   OutboundTarget{GroupID: "G1"},
		Text:     "你好",
		Decision: SendDecision{Mode: ModeProactive},
	})
	if err == nil {
		t.Fatal("两次 401 应返回错误, 而不是假成功")
	}
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("应返回 *APIError, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("错误应带 http 401 状态: %+v", apiErr)
	}
	// 首次请求 + 刷 token 后重试, 共 2 次; 第二次仍 401 即报错, 不再刷 token。
	if got := api.tokenCallCount(); got != 2 {
		t.Fatalf("应只多刷一次 token, got %d", got)
	}
	if posts := api.postsSnapshot(); len(posts) != 2 {
		t.Fatalf("应恰好请求 2 次, got %d", len(posts))
	}
}

// TestCallAPIArrayResponseWrapped 校验 B1: 成功响应是 JSON 数组等非对象时, 包一层
// {"result": <原始 JSON>} 返回, 不再整单报错 (对齐 Diana)。
func TestCallAPIArrayResponseWrapped(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)
	out, err := client.CallAPI(context.Background(), "GET /users/@me/guilds/array", nil)
	if err != nil {
		t.Fatalf("数组响应不应报错: %v", err)
	}
	raw, ok := out["result"].(json.RawMessage)
	if !ok {
		t.Fatalf("应包含 result 键: %v", out)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("result 应是原始数组 JSON: %v", err)
	}
	if len(items) != 2 || items[0]["id"] != "G1" || items[1]["id"] != "G2" {
		t.Fatalf("数组内容不符: %v", items)
	}
}

// TestCallRESTDoesNotPolluteOutOnRetry 校验 B5: 429/5xx 重试场景下, 失败响应体
// 不解析进出参, 重试成功后出参没有残留旧键。
func TestCallRESTDoesNotPolluteOutOnRetry(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"junk": "stale"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()

	client := NewAPIClient(APIOptions{AppID: "A", AppSecret: "S"})
	hijackToken(t, client)

	out := map[string]any{}
	if err := client.callREST(context.Background(), http.MethodGet, server.URL, nil, &out); err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if _, ok := out["junk"]; ok {
		t.Fatalf("429 响应体不应污染出参: %v", out)
	}
	if out["ok"] != true {
		t.Fatalf("成功响应应写进出参: %v", out)
	}
}

// TestTruncateRuneSafe 校验 P3b: truncate 按 rune 边界截断, 不切碎 UTF-8。
func TestTruncateRuneSafe(t *testing.T) {
	if got := truncate("你好世界", 5); got != "你…" {
		t.Fatalf("多字节截断不符: %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Fatalf("ASCII 截断不符: %q", got)
	}
	if got := truncate("ab", 5); got != "ab" {
		t.Fatalf("不超限应原样返回: %q", got)
	}
	if !utf8.ValidString(truncate("你好世界", 4)) {
		t.Fatal("截断结果必须是合法 UTF-8")
	}
}

// ---------------------------------------------------------------------------
// 引用
// ---------------------------------------------------------------------------

// TestMessageReferenceForUsesRefLedger 校验出站引用: 引用别人用入站 msg_idx,
// 引用自己用出站 ref_idx, 查不到不引用。
func TestMessageReferenceForUsesRefLedger(t *testing.T) {
	client := NewAPIClient(APIOptions{AppID: "A", AppSecret: "S"})
	refs := NewRefLedger(0, 0)
	// 别人发的消息: 入站登记的 msg_idx。
	refs.RecordInbound("REFIDX_IN_OTHER", "MSG_OTHER", "MEMBER_1", false)
	// 自己发的消息: 发送响应登记的 ref_idx。
	refs.RecordOutbound("REFIDX_OUT_SELF", "MSG_SELF", "BOT_1")
	client.SetRefLedger(refs)

	tests := []struct {
		name    string
		msgID   string
		wantNil bool
		wantKey string
	}{
		{"引用别人", "MSG_OTHER", false, "REFIDX_IN_OTHER"},
		{"引用自己", "MSG_SELF", false, "REFIDX_OUT_SELF"},
		{"空引用", "", true, ""},
		{"未登记的引用", "MSG_UNKNOWN", true, ""},
	}
	for _, item := range tests {
		t.Run(item.name, func(t *testing.T) {
			got := client.MessageReferenceFor(item.msgID)
			if item.wantNil {
				if got != nil {
					t.Fatalf("应不带引用, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("应返回 message_reference")
			}
			if got["message_id"] != item.wantKey {
				t.Fatalf("引用键不符: got %v want %v", got["message_id"], item.wantKey)
			}
			if got["ignore_get_message_error"] != true {
				t.Fatalf("ignore_get_message_error 必须为 true: %+v", got)
			}
		})
	}
}

// TestSendAttachesMessageReference 校验文本发送会挂上引用卡片 (且不影响 msg_id)。
func TestSendAttachesMessageReference(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)
	refs := NewRefLedger(0, 0)
	refs.RecordInbound("REFIDX_IN_OTHER", "MSG_OTHER", "MEMBER_1", false)
	client.SetRefLedger(refs)

	if _, err := client.Send(context.Background(), SendRequest{
		Target:     OutboundTarget{GroupID: "G1"},
		Text:       "回复你",
		Decision:   SendDecision{Mode: ModePassive, TriggerID: "MSG_OTHER", Seq: 1},
		QuoteMsgID: "MSG_OTHER",
	}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	posts := api.postsSnapshot()
	reference, ok := posts[0]["message_reference"].(map[string]any)
	if !ok {
		t.Fatalf("应带上 message_reference: %+v", posts[0])
	}
	if reference["message_id"] != "REFIDX_IN_OTHER" {
		t.Fatalf("引用键不符: %+v", reference)
	}
	// msg_id 永远指向触发本轮的消息, 与引用正交。
	if posts[0]["msg_id"] != "MSG_OTHER" {
		t.Fatalf("msg_id 应指向触发消息: %+v", posts[0])
	}
}

// TestSendMediaUsesMsgType7 校验富媒体发送用 msg_type 7 + media.file_info。
func TestSendMediaUsesMsgType7(t *testing.T) {
	api := newFakeAPI(t)
	client := newTestClient(t, api)
	if _, err := client.Send(context.Background(), SendRequest{
		Target:   OutboundTarget{GroupID: "G1"},
		Media:    "FILE_INFO_1",
		Decision: SendDecision{Mode: ModePassive, TriggerID: "T1", Seq: 1},
	}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	posts := api.postsSnapshot()
	if posts[0]["msg_type"] != float64(7) {
		t.Fatalf("富媒体应是 msg_type 7: %+v", posts[0])
	}
	media, ok := posts[0]["media"].(map[string]any)
	if !ok || media["file_info"] != "FILE_INFO_1" {
		t.Fatalf("media.file_info 不符: %+v", posts[0])
	}
	if posts[0]["message_reference"] != nil {
		t.Fatalf("富媒体不应挂引用: %+v", posts[0])
	}
}

// TestHasPayloadAndSeqValue 校验请求体判定与 seq 下界。
func TestHasPayloadAndSeqValue(t *testing.T) {
	if (SendRequest{}).HasPayload() {
		t.Fatal("空请求不应有内容")
	}
	if !(SendRequest{Media: "F"}).HasPayload() {
		t.Fatal("带 media 应算有内容")
	}
	if (SendRequest{Text: "   "}).HasPayload() {
		t.Fatal("纯空白不应算有内容")
	}
	if (SendDecision{Mode: ModePassive}).SeqValue() != 1 {
		t.Fatal("缺省 seq 应为 1")
	}
}

// ---------------------------------------------------------------------------
// 富媒体上传
// ---------------------------------------------------------------------------

// uploadAPI 模拟分片上传链路。
type uploadAPI struct {
	mu       sync.Mutex
	server   *httptest.Server
	prepares []map[string]any
	merges   []map[string]any
	finishes []map[string]any
	paths    []string
	putBody  []byte
	fileInfo string
}

func newUploadAPI(t *testing.T) *uploadAPI {
	t.Helper()
	api := &uploadAPI{fileInfo: "FILE_INFO_OK"}
	mux := http.NewServeMux()
	mux.HandleFunc("/put/1", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		api.mu.Lock()
		api.putBody = body
		api.paths = append(api.paths, "PUT "+r.URL.Path)
		api.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		api.mu.Lock()
		api.paths = append(api.paths, r.Method+" "+r.URL.Path)
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/upload_prepare"):
			api.mu.Lock()
			api.prepares = append(api.prepares, body)
			api.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"upload_id":  "UPLOAD_1",
				"block_size": body["file_size"],
				"parts": []map[string]any{{
					"index":         1,
					"presigned_url": api.server.URL + "/put/1",
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/upload_part_finish"):
			api.mu.Lock()
			api.finishes = append(api.finishes, body)
			api.mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "MEDIA_MSG_1"})
		case strings.HasSuffix(r.URL.Path, "/files"):
			api.mu.Lock()
			api.merges = append(api.merges, body)
			api.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"file_info": api.fileInfo, "ttl": 0})
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	api.server = httptest.NewServer(mux)
	t.Cleanup(api.server.Close)
	return api
}

func newUploadClient(t *testing.T, api *uploadAPI) *APIClient {
	t.Helper()
	return NewAPIClient(APIOptions{
		AppID:            "APPID_1",
		AppSecret:        "SECRET_1",
		APIBaseOverride:  api.server.URL,
		TokenURLOverride: api.server.URL + "/token",
	})
}

// hijackToken 让 token 请求返回固定 token (复用 fake token 端点不方便时)。
func hijackToken(t *testing.T, client *APIClient) {
	t.Helper()
	client.tokens.token = "TOKEN_X"
	client.tokens.expiresAt = time.Now().Add(time.Hour)
}

// TestUploadMediaFlow 校验分片上传的完整链路与 file_type / 校验值。
func TestUploadMediaFlow(t *testing.T) {
	api := newUploadAPI(t)
	client := newUploadClient(t, api)
	hijackToken(t, client)

	prefix := api.server.URL + "/v2/groups/G1"
	payload := []byte("hello-media-payload")
	fileInfo, err := client.UploadMedia(context.Background(), prefix, payload, "pic.png", FileTypeImage)
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if fileInfo != "FILE_INFO_OK" {
		t.Fatalf("file_info 不符: %q", fileInfo)
	}
	api.mu.Lock()
	paths := append([]string(nil), api.paths...)
	prepares := append([]map[string]any(nil), api.prepares...)
	finishes := append([]map[string]any(nil), api.finishes...)
	merges := append([]map[string]any(nil), api.merges...)
	putBody := append([]byte(nil), api.putBody...)
	api.mu.Unlock()

	for _, want := range []string{"POST /v2/groups/G1/upload_prepare", "PUT /put/1", "POST /v2/groups/G1/upload_part_finish", "POST /v2/groups/G1/files"} {
		if !containsString(paths, want) {
			t.Fatalf("缺少请求 %q, got %v", want, paths)
		}
	}
	if len(prepares) != 1 {
		t.Fatalf("应预上传一次, got %d", len(prepares))
	}
	if prepares[0]["file_type"] != float64(FileTypeImage) {
		t.Fatalf("file_type 应为 1 (图片): %+v", prepares[0])
	}
	if prepares[0]["file_size"] != itoa(len(payload)) {
		t.Fatalf("file_size 应为字符串: %+v", prepares[0])
	}
	if prepares[0]["file_name"] != "pic.png" {
		t.Fatalf("file_name 不符: %+v", prepares[0])
	}
	for _, key := range []string{"md5", "sha1", "md5_10m"} {
		value, ok := prepares[0][key].(string)
		if !ok || len(value) == 0 {
			t.Fatalf("缺少校验值 %s: %+v", key, prepares[0])
		}
	}
	if string(putBody) != string(payload) {
		t.Fatalf("分片内容不符: %q", putBody)
	}
	if len(finishes) != 1 || finishes[0]["upload_id"] != "UPLOAD_1" || finishes[0]["part_index"] != float64(1) {
		t.Fatalf("分片完成请求不符: %+v", finishes)
	}
	if len(merges) != 1 || merges[0]["upload_id"] != "UPLOAD_1" || merges[0]["srv_send_msg"] != false {
		t.Fatalf("合并请求不符: %+v", merges)
	}
}

// TestUploadMediaFileTypes 校验各 file_type 与默认文件名。
func TestUploadMediaFileTypes(t *testing.T) {
	api := newUploadAPI(t)
	client := newUploadClient(t, api)
	hijackToken(t, client)

	tests := []struct {
		fileType int
		name     string
		wantName string
	}{
		{FileTypeImage, "", "image.png"},
		{FileTypeVideo, "", "video.mp4"},
		{FileTypeAudio, "", "audio.mp3"},
		{FileTypeFile, "", "file.bin"},
		{FileTypeImage, "/tmp/foo.jpg", "foo.jpg"},
		{FileTypeVideo, "clip.mp4", "clip.mp4"},
	}
	for _, item := range tests {
		prefix := api.server.URL + "/v2/groups/G1"
		if _, err := client.UploadMedia(context.Background(), prefix, []byte("x"), item.name, item.fileType); err != nil {
			t.Fatalf("上传失败 (type=%d): %v", item.fileType, err)
		}
	}
	api.mu.Lock()
	prepares := append([]map[string]any(nil), api.prepares...)
	api.mu.Unlock()
	if len(prepares) != len(tests) {
		t.Fatalf("预上传次数不符: %d", len(prepares))
	}
	for i, item := range tests {
		if prepares[i]["file_name"] != item.wantName {
			t.Fatalf("第 %d 项文件名不符: got %v want %v", i, prepares[i]["file_name"], item.wantName)
		}
	}
}

// TestUploadMediaRejects 校验上传的拒绝路径。
func TestUploadMediaRejects(t *testing.T) {
	api := newUploadAPI(t)
	client := newUploadClient(t, api)
	hijackToken(t, client)

	prefix := api.server.URL + "/v2/groups/G1"
	if _, err := client.UploadMedia(context.Background(), prefix, nil, "a.png", FileTypeImage); err == nil {
		t.Fatal("空内容应被拒")
	}
	if _, err := client.UploadMedia(context.Background(), prefix, []byte("x"), "a.png", FileTypeFile); err != nil {
		t.Fatalf("普通文件类型应能上传: %v", err)
	}
	// 缺少 file_info 的合并响应。
	api.mu.Lock()
	api.fileInfo = ""
	api.mu.Unlock()
	if _, err := client.UploadMedia(context.Background(), prefix, []byte("x"), "a.png", FileTypeImage); err == nil {
		t.Fatal("缺 file_info 应被拒")
	}
}

// TestUploadPrefixAndSendMedia 校验 UploadPrefix 与 SendMedia 端到端。
func TestUploadPrefixAndSendMedia(t *testing.T) {
	api := newUploadAPI(t)
	client := newUploadClient(t, api)
	hijackToken(t, client)

	prefix, err := client.UploadPrefix(OutboundTarget{GroupID: "G1"})
	if err != nil || prefix != api.server.URL+"/v2/groups/G1" {
		t.Fatalf("群前缀不符: %q err=%v", prefix, err)
	}
	prefix, err = client.UploadPrefix(OutboundTarget{UserID: "U1"})
	if err != nil || prefix != api.server.URL+"/v2/users/U1" {
		t.Fatalf("单聊前缀不符: %q err=%v", prefix, err)
	}
	if _, err := client.UploadPrefix(OutboundTarget{}); err == nil {
		t.Fatal("空目标应报错")
	}

	// SendMedia: 上传后发 msg_type 7。
	resp, err := client.SendMedia(context.Background(), SendRequest{
		Target:   OutboundTarget{GroupID: "G1"},
		Decision: SendDecision{Mode: ModeProactive},
	}, []byte("sticker-bytes"), "sticker.png", FileTypeImage)
	if err != nil {
		t.Fatalf("SendMedia 失败: %v", err)
	}
	if resp.MessageID == "" {
		t.Fatal("应返回消息 id")
	}
	api.mu.Lock()
	paths := append([]string(nil), api.paths...)
	api.mu.Unlock()
	if !containsString(paths, "POST /v2/groups/G1/messages") {
		t.Fatalf("未发出富媒体消息: %v", paths)
	}
}

// TestFetchAttachment 校验入站附件下载与大小上限。
func TestFetchAttachment(t *testing.T) {
	payload := []byte("attachment-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	client := NewAPIClient(APIOptions{AppID: "A", AppSecret: "S"})

	got, err := client.FetchAttachment(context.Background(), Attachment{URL: server.URL, ContentType: "image/png"}, 1024)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("下载失败: %q err=%v", got, err)
	}
	if _, err := client.FetchAttachment(context.Background(), Attachment{URL: server.URL}, 4); err == nil {
		t.Fatal("超过上限应报错")
	}
	if _, err := client.FetchAttachment(context.Background(), Attachment{}, 1024); err == nil {
		t.Fatal("缺地址应报错")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
