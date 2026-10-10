package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// REST 客户端 (§6.12 api.go)
//
//   - token 缓存: appID/secret → POST https://bots.qq.com/app/getAppAccessToken,
//     提前 2 分钟刷新
//   - 发文本消息 msg_type 0: 群 POST /v2/groups/{group_openid}/messages,
//     单聊 POST /v2/users/{openid}/messages
//   - 被动(带 msg_id + msg_seq) / 主动(不带 msg_id) 由调用方通过 SendRequest 选择
//   - msg_seq 自增 + 40054005 换新 seq 重试一次
//   - 401 刷 token 重试一次
//   - 引用 message_reference{message_id, ignore_get_message_error:true}: 引用别人
//     用入站 msg_idx, 引用自己用 REFIDX 台账反查的 ref_idx
// ---------------------------------------------------------------------------

const (
	// TokenURL 是换取 access token 的地址。
	TokenURL = "https://bots.qq.com/app/getAppAccessToken"
	// APIBase 是正式环境域名 (群 /v2 接口)。
	APIBase = "https://api.sgroup.qq.com"
	// SandboxAPIBase 是沙箱环境域名, 机器人未上架前用于联调。
	SandboxAPIBase = "https://sandbox.api.sgroup.qq.com"

	// tokenRefreshMargin 是提前刷新的余量: token 剩不到这么多时间就重新换。
	tokenRefreshMargin = 2 * time.Minute
	// defaultTokenTTL 是 expires_in 缺失时的兜底 (官方目前固定 7200 秒)。
	defaultTokenTTL = 2 * time.Hour
	// defaultHTTPTimeout 是内置 HTTP 客户端的超时。
	defaultHTTPTimeout = 30 * time.Second
	// httpResponseLimit 限制单次响应读取体积, 避免异常响应把内存吃满。
	httpResponseLimit = 8 << 20
)

// 官方错误码 (§6.12 重试策略)。
const (
	// ErrCodeDedup 是「消息被去重, 请检查请求 msgseq」: 换 msg_seq 重发一次可通过。
	ErrCodeDedup = "40054005"
	// ErrCodePassiveQuota 是「被动回复时间或次数超过限制」: 重试无益。
	ErrCodePassiveQuota = "40034128"
	// ErrCodeNoPermission 是主动消息无权限或未开启能力。
	ErrCodeNoPermission = "40034105"
	// ErrCodeForbidden 是另一类无权限/未开启能力的拒绝。
	ErrCodeForbidden = "40034100"
	// ErrCodeTransferFailed 是富媒体转存失败, 重试上传可能救回来。
	ErrCodeTransferFailed = "40034004"
)

// APIError 是开放平台返回的业务错误 (HTTP 2xx 但 code != 0, 或非 2xx)。
type APIError struct {
	Code    string
	Message string
	Status  int
}

// Error implements error.
func (e *APIError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("qq: API 错误 %s: %s (http %d)", e.Code, e.Message, e.Status)
	}
	return fmt.Sprintf("qq: API 错误 %s: %s", e.Code, e.Message)
}

// CodeInt 返回数字错误码 (非数字返回 0)。
func (e *APIError) CodeInt() int {
	value, err := strconv.Atoi(strings.TrimSpace(e.Code))
	if err != nil {
		return 0
	}
	return value
}

// SentHow reports whether the message reached the platform (§6.12 记账语义):
// 决定发送失败后要不要回滚额度 / 被动次数。
type SentHow int

const (
	// SentUnknown 是不确定的传输错误: 是否到达平台不知道, 保守起见不回滚额度。
	SentUnknown SentHow = iota
	// SentDelivered 是消息已到达平台 (含业务错误码拒绝: 平台已明确处理过)。
	SentDelivered
	// SentNotSent 是明确没有到达平台 (本地额度不足、无触发消息等拒绝)。
	SentNotSent
)

// String implements fmt.Stringer.
func (s SentHow) String() string {
	switch s {
	case SentDelivered:
		return "delivered"
	case SentNotSent:
		return "not_sent"
	default:
		return "unknown"
	}
}

// APISendError 携带一次发送失败的可判定语义, 供上层决定是否回滚额度。
type APISendError struct {
	Err  error
	How  SentHow
	Code string
}

// Error implements error.
func (e *APISendError) Error() string {
	if e.Err == nil {
		return "qq: 发送失败"
	}
	return e.Err.Error()
}

// Unwrap 支持 errors.Is/As。
func (e *APISendError) Unwrap() error { return e.Err }

// classifySendError 判定一次失败是否到达平台:
// 业务错误码 = 平台明确处理过 (占额度/次数); 传输错误 = 不确定 (不回滚)。
func classifySendError(err error) error {
	if err == nil {
		return nil
	}
	code := ""
	how := SentUnknown
	if apiErr, ok := AsAPIError(err); ok {
		code = apiErr.Code
		how = SentDelivered
	}
	return &APISendError{Err: err, How: how, Code: code}
}

// AsAPIError 从错误链里取出业务错误详情。
func AsAPIError(err error) (*APIError, bool) {
	for current := err; current != nil; current = unwrapOnce(current) {
		if typed, ok := current.(*APIError); ok {
			return typed, true
		}
	}
	return nil, false
}

// IsAPIErrorCode 判断错误链里是否命中某个官方错误码。
func IsAPIErrorCode(err error, code string) bool {
	if err == nil {
		return false
	}
	for current := err; current != nil; current = unwrapOnce(current) {
		if typed, ok := current.(*APIError); ok && typed.Code == code {
			return true
		}
	}
	// 兜底: 未解析成 APIError 时 (例如错误信息里带着原始响应体) 按文本匹配。
	return strings.Contains(err.Error(), code)
}

func unwrapOnce(err error) error {
	type unwrapper interface{ Unwrap() error }
	typed, ok := err.(unwrapper)
	if !ok {
		return nil
	}
	return typed.Unwrap()
}

// ---------------------------------------------------------------------------
// Options / 客户端
// ---------------------------------------------------------------------------

// APIOptions 配置 REST 客户端 (包内配置, 不引入 internal/config)。
type APIOptions struct {
	// AppID 是开放平台的机器人 AppID。
	AppID string
	// AppSecret 用于换取 access token。
	AppSecret string
	// Sandbox 走沙箱环境。
	Sandbox bool
	// APIBaseOverride 覆写 API 域名 (测试指向 httptest); 空表示用内置域名。
	APIBaseOverride string
	// TokenURLOverride 覆写 token 地址 (测试用)。
	TokenURLOverride string
	// HTTPClient 复用外部客户端; nil 用内置默认客户端。
	HTTPClient *http.Client
	// Timeout 仅在 HTTPClient 为 nil 时生效 (默认 30s)。
	Timeout time.Duration
	// Now 注入时钟 (测试用)。
	Now func() time.Time
	// Logger 记录刷新 token / 重试等事件; nil 表示不记。
	Logger func(format string, args ...any)
}

func (o APIOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// APIClient 是 QQ 官方 REST 客户端: 只负责发送与鉴权, 不感知业务语义。
// 用 NewAPIClient 创建。
type APIClient struct {
	mu     sync.RWMutex
	opts   APIOptions
	client *http.Client

	tokens *tokenCache
	// refs 是 REFIDX_ 引用索引 (出站引用键反查); 可为 nil。
	refs *RefLedger
	// sent 是自发消息台账 (发送成功后登记回推识别); 可为 nil。
	sent *SentLedger
	// quota 是主动额度台账 (可选; 由上层统一调度时用)。
	quota *ProactiveLedger
}

// NewAPIClient 创建 REST 客户端。
func NewAPIClient(opts APIOptions) *APIClient {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	api := &APIClient{opts: opts, client: client}
	api.tokens = &tokenCache{now: opts.Now, fetch: api.fetchAccessToken}
	return api
}

// SetRefLedger 绑定 REFIDX 引用索引 (出站引用反查)。
func (a *APIClient) SetRefLedger(ledger *RefLedger) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refs = ledger
}

// RefLedger 返回绑定的引用索引 (可能为 nil)。
func (a *APIClient) RefLedger() *RefLedger {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.refs
}

// SetSentLedger 绑定自发消息台账。
func (a *APIClient) SetSentLedger(ledger *SentLedger) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = ledger
}

// SetQuotaLedger 绑定主动额度台账。
func (a *APIClient) SetQuotaLedger(ledger *ProactiveLedger) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.quota = ledger
}

// QuotaLedger 返回绑定的额度台账 (可能为 nil)。
func (a *APIClient) QuotaLedger() *ProactiveLedger {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.quota
}

func (a *APIClient) log(format string, args ...any) {
	a.mu.RLock()
	logger := a.opts.Logger
	a.mu.RUnlock()
	if logger != nil {
		logger(format, args...)
	}
}

// ---------------------------------------------------------------------------
// token 缓存
// ---------------------------------------------------------------------------

type tokenCache struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
	now       func() time.Time
	fetch     func(ctx context.Context) (string, time.Duration, error)
}

func (c *tokenCache) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Get 返回有效 token; 剩余有效期不足 tokenRefreshMargin 时提前换新。
func (c *tokenCache) Get(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.nowTime().Before(c.expiresAt) {
		return c.token, nil
	}
	if c.fetch == nil {
		return "", fmt.Errorf("qq: token 获取函数未配置")
	}
	token, ttl, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("qq: 平台返回空的 access token")
	}
	if ttl > tokenRefreshMargin {
		ttl -= tokenRefreshMargin
	}
	c.token = token
	c.expiresAt = c.nowTime().Add(ttl)
	return token, nil
}

// Invalidate 丢弃缓存的 token。平台返回 401 时调用, 让下一次请求重新换一个。
func (c *tokenCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
	c.expiresAt = time.Time{}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   any    `json:"expires_in"`
	Message     string `json:"message"`
	Code        int    `json:"code"`
}

func (a *APIClient) tokenURL() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if override := strings.TrimSpace(a.opts.TokenURLOverride); override != "" {
		return override
	}
	return TokenURL
}

// fetchAccessToken 用 AppID + AppSecret 换取 access token。
func (a *APIClient) fetchAccessToken(ctx context.Context) (string, time.Duration, error) {
	a.mu.RLock()
	appID := strings.TrimSpace(a.opts.AppID)
	secret := strings.TrimSpace(a.opts.AppSecret)
	a.mu.RUnlock()
	if appID == "" || secret == "" {
		return "", 0, fmt.Errorf("qq: AppID 和 AppSecret 都必须配置")
	}
	payload := map[string]string{"appId": appID, "clientSecret": secret}
	req, err := a.newRequest(ctx, http.MethodPost, a.tokenURL(), nil, payload)
	if err != nil {
		return "", 0, err
	}
	body, status, err := a.httpDo(ctx, req)
	if err != nil {
		return "", 0, fmt.Errorf("qq: 换取 access token 失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", 0, fmt.Errorf("qq: 换取 access token 失败: http %d: %s", status, truncate(string(body), 256))
	}
	var envelope tokenResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", 0, fmt.Errorf("qq: 解析 token 响应失败: %w", err)
	}
	if strings.TrimSpace(envelope.AccessToken) == "" {
		return "", 0, fmt.Errorf("qq: 换取 access token 被拒绝: %s (code %d)", envelope.Message, envelope.Code)
	}
	ttl := ttlFromExpiresIn(envelope.ExpiresIn)
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}
	return strings.TrimSpace(envelope.AccessToken), ttl, nil
}

// ttlFromExpiresIn 把官方的 expires_in (字符串或数字, 单位秒) 归一化。
func ttlFromExpiresIn(raw any) time.Duration {
	switch value := raw.(type) {
	case nil:
		return 0
	case float64:
		return time.Duration(int64(value)) * time.Second
	case int64:
		return time.Duration(value) * time.Second
	case json.Number:
		number, err := value.Float64()
		if err != nil {
			return 0
		}
		return time.Duration(int64(number)) * time.Second
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return 0
		}
		if seconds, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return time.Duration(int64(seconds)) * time.Second
		}
	}
	return 0
}

// AuthHeader 组装开放平台要求的 Authorization 头 ("QQBot <token>")。
func (a *APIClient) AuthHeader(ctx context.Context) (string, error) {
	token, err := a.tokens.Get(ctx)
	if err != nil {
		return "", err
	}
	return "QQBot " + token, nil
}

// InvalidateToken 主动丢弃缓存 token (平台明确报 token 失效时)。
func (a *APIClient) InvalidateToken() { a.tokens.Invalidate() }

// APIBase 返回生效的 REST 域名。
func (a *APIClient) APIBase() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if override := strings.TrimSpace(a.opts.APIBaseOverride); override != "" {
		return strings.TrimSuffix(override, "/")
	}
	if a.opts.Sandbox {
		return SandboxAPIBase
	}
	return APIBase
}

// SetAPIBaseOverride 覆写 API 域名 (测试指向 httptest 服务器)。
func (a *APIClient) SetAPIBaseOverride(base string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opts.APIBaseOverride = strings.TrimSpace(base)
}

// SandboxMode 返回是否走沙箱环境。
func (a *APIClient) SandboxMode() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.opts.Sandbox
}

// ---------------------------------------------------------------------------
// 传输层
// ---------------------------------------------------------------------------

func (a *APIClient) newRequest(ctx context.Context, method string, endpoint string, headers map[string]string, payload any) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("qq: 编码请求体失败: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("qq: 构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		if strings.TrimSpace(value) != "" {
			req.Header.Set(name, value)
		}
	}
	return req, nil
}

func (a *APIClient) httpDo(ctx context.Context, req *http.Request) ([]byte, int, error) {
	resp, err := a.client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpResponseLimit))
	return body, resp.StatusCode, nil
}

// decodeEnvelope 解出开放平台的响应; 带 code 的错误体即使是 2xx 也算失败
// (返回 *APIError, 调用方用 AsAPIError 判定)。
func decodeEnvelope(body []byte, status int, out any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		if status >= 200 && status < 300 {
			return nil
		}
		return &APIError{Code: strconv.Itoa(status), Message: "平台无响应体", Status: status}
	}
	var envelope struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if code := apiCodeString(envelope.Code); code != "" && !apiCodeOK(envelope.Code) {
			return &APIError{Code: code, Message: envelope.Message, Status: status}
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("qq: 解析响应失败: %w", err)
	}
	return nil
}

// apiCodeString 把 code 字段 (数字或字符串) 归一化成字符串。
func apiCodeString(raw any) string {
	switch value := raw.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(value)
	case float64:
		return strconv.Itoa(int(value))
	case json.Number:
		return value.String()
	default:
		return fmt.Sprintf("%v", value)
	}
}

// apiCodeOK 判断 code 字段是不是「成功」(缺失 / 0 / 空)。
func apiCodeOK(raw any) bool {
	switch value := raw.(type) {
	case nil:
		return true
	case string:
		trimmed := strings.TrimSpace(value)
		return trimmed == "" || trimmed == "0"
	case float64:
		return int(value) == 0
	case json.Number:
		number, err := value.Float64()
		return err != nil || int(number) == 0
	default:
		return true
	}
}

// callREST 执行一次带鉴权的 REST 调用: 401 时刷 token 重试一次, 5xx/429 重试一次。
func (a *APIClient) callREST(ctx context.Context, method string, endpoint string, payload any, out any) error {
	_, err := a.callRESTRaw(ctx, method, endpoint, payload, out)
	return err
}

// callRESTRaw 是 callREST 的原始响应版本: 成功时返回响应体 (可能是数组等非对象
// JSON), 供 CallAPI 包装 (B1); 重试与错误语义和 callREST 完全一致。
func (a *APIClient) callRESTRaw(ctx context.Context, method string, endpoint string, payload any, out any) ([]byte, error) {
	auth, err := a.AuthHeader(ctx)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := a.newRequest(ctx, method, endpoint, map[string]string{"Authorization": auth}, payload)
		if err != nil {
			return nil, err
		}
		body, status, err := a.httpDo(ctx, req)
		if err != nil {
			lastErr = fmt.Errorf("qq: 请求失败: %w", err)
			break
		}
		if status == http.StatusUnauthorized {
			// A1: 401 只重试一次 —— 第二次仍 401 必须显式报错。旧代码循环耗尽后
			// lastErr 为 nil、函数级 err 也是 nil, 上层把失败当成功 (空 MessageID),
			// 已扣的额度/被动次数也不会回滚。
			if attempt > 0 {
				return body, &APIError{
					Code:    strconv.Itoa(status),
					Message: "token 刷新后仍被拒绝: " + truncate(string(body), 256),
					Status:  status,
				}
			}
			// token 过期或被踢: 丢掉缓存换一个新的重试一次。
			a.tokens.Invalidate()
			a.log("qq: REST 401, 刷新 token 后重试 (%s %s)", method, endpoint)
			if auth, err = a.AuthHeader(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if status >= 200 && status < 300 {
			// B5: 只有 2xx 才解析进出参 —— 429/5xx 重试场景下, 失败响应体若先
			// Unmarshal 进出参 (map) 会残留旧键, 污染重试成功后的解析结果。
			return body, decodeEnvelope(body, status, out)
		}
		decoded := decodeEnvelope(body, status, nil)
		if decoded != nil {
			lastErr = decoded
		} else {
			lastErr = &APIError{Code: strconv.Itoa(status), Message: truncate(string(body), 256), Status: status}
		}
		if isRetryableStatus(status) && attempt == 0 {
			continue
		}
		break
	}
	if lastErr != nil {
		return nil, lastErr
	}
	// A1: 兜底 —— 任何非成功路径都不允许返回 nil (旧代码此处 return err 恒为 nil)。
	return nil, fmt.Errorf("qq: 请求失败 (%s %s)", method, endpoint)
}

// isRetryableStatus 判断 HTTP 状态码是否值得重试一次 (限频 / 网关抖动)。
func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// GatewayURL 取 WebSocket 网关地址 (GET /gateway)。
func (a *APIClient) GatewayURL(ctx context.Context) (string, error) {
	var payload struct {
		URL string `json:"url"`
	}
	if err := a.callREST(ctx, http.MethodGet, a.APIBase()+"/gateway", nil, &payload); err != nil {
		return "", fmt.Errorf("qq: 获取网关地址失败: %w", err)
	}
	if strings.TrimSpace(payload.URL) == "" {
		return "", fmt.Errorf("qq: 网关地址为空")
	}
	return payload.URL, nil
}

// CallAPI 透传开放平台的 REST 接口, action 形如 "GET /users/@me/guilds"。
// params 在非 GET 请求时作为 JSON body, 在 GET 请求时作为 query。
func (a *APIClient) CallAPI(ctx context.Context, action string, params map[string]any) (map[string]any, error) {
	method, path := http.MethodGet, strings.TrimSpace(action)
	if fields := strings.SplitN(path, " ", 2); len(fields) == 2 {
		method, path = strings.ToUpper(fields[0]), strings.TrimSpace(fields[1])
	}
	if path == "" {
		return nil, fmt.Errorf("qq: 缺少接口路径")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	var payload any
	if len(params) > 0 && method != http.MethodGet {
		payload = params
	}
	endpoint, err := a.requestURL(a.APIBase(), path, method, params)
	if err != nil {
		return nil, err
	}
	// B1: 成功响应可能是数组等非对象 JSON, 走原始响应版本自己解 (对齐 Diana
	// qq_official.go 的 CallAPI); 错误码 / 非 2xx 仍由 callRESTRaw 统一报错。
	raw, err := a.callRESTRaw(ctx, method, endpoint, payload, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		// 非对象响应包一层 {"result": <原始 JSON>} 返回, 不再整单报错。
		return map[string]any{"result": json.RawMessage(raw)}, nil
	}
	return out, nil
}

// requestURL 拼出完整 URL; GET 的 params 走 query string。
func (a *APIClient) requestURL(base string, path string, method string, params map[string]any) (string, error) {
	endpoint := strings.TrimSuffix(base, "/") + path
	if method != http.MethodGet || len(params) == 0 {
		return endpoint, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("qq: 接口路径无效: %w", err)
	}
	query := parsed.Query()
	for key, value := range params {
		if value == nil {
			continue
		}
		query.Set(key, fmt.Sprintf("%v", value))
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// ---------------------------------------------------------------------------
// 发送消息
// ---------------------------------------------------------------------------

// OutboundTarget 描述发送目标。
type OutboundTarget struct {
	// GroupID 是群 openid (群聊必填)。
	GroupID string
	// UserID 是单聊对方的 user openid (私聊必填)。
	UserID string
}

// Target 返回 (openid, isGroup); 目标非法返回空串。
func (t OutboundTarget) Target() (string, bool) {
	if groupID := strings.TrimSpace(t.GroupID); groupID != "" {
		return groupID, true
	}
	if userID := strings.TrimSpace(t.UserID); userID != "" {
		return userID, false
	}
	return "", false
}

// Scope 返回这笔发送对应的主动额度作用域。
func (t OutboundTarget) Scope() ProactiveScope {
	return ProactiveScope{GroupID: strings.TrimSpace(t.GroupID), UserID: strings.TrimSpace(t.UserID)}
}

// MessageEndpoint 返回 /v2 消息接口地址。
func (a *APIClient) MessageEndpoint(target OutboundTarget) (string, bool, error) {
	id, isGroup := target.Target()
	if id == "" {
		return "", false, fmt.Errorf("qq: 缺少会话标识 (group_openid 或 user openid)")
	}
	if isGroup {
		return fmt.Sprintf("%s/v2/groups/%s/messages", a.APIBase(), url.PathEscape(id)), true, nil
	}
	return fmt.Sprintf("%s/v2/users/%s/messages", a.APIBase(), url.PathEscape(id)), false, nil
}

// SendRequest 是一次出站发送请求。
type SendRequest struct {
	// Target 是发送目标。
	Target OutboundTarget
	// Text 是文本正文 (为空时必须带 Media)。
	Text string
	// Media 是已上传富媒体的 file_info (发 msg_type 7); 为空时发 msg_type 0 文本。
	Media string
	// Decision 是 DecideSendMode 的结果 (被动模式带上 TriggerID 与 Seq)。
	Decision SendDecision
	// QuoteMsgID 是要引用的平台消息 id (转 REFIDX_ 由 MessageReferenceFor 完成)。
	QuoteMsgID string
	// Reseq 在 40054005 换 seq 重试时被调用, 返回新的 msg_seq; nil 表示 old+1。
	// 由上层注入 (走 sequence 台账领全新序号, 避免与本次回复的后续段撞车)。
	Reseq func(oldSeq int) int
}

// HasPayload 判断请求有没有可发内容。
func (r SendRequest) HasPayload() bool {
	return strings.TrimSpace(r.Text) != "" || strings.TrimSpace(r.Media) != ""
}

// SeqValue returns the msg_seq to use for a passive reply (at least 1).
func (d SendDecision) SeqValue() int {
	if d.Seq > 0 {
		return d.Seq
	}
	return 1
}

// SendResponse 是发送结果。
type SendResponse struct {
	// MessageID 是平台返回的消息 id。
	MessageID string
	// RefIdx 是平台返回的引用索引键 (登记进 REFIDX 台账用于反查自发引用)。
	RefIdx string
	// Passive 标记这次发送走的是被动模式。
	Passive bool
	// Seq 是被动模式下实际使用的 msg_seq (主动模式为 0)。
	Seq int
	// RetrySeq 为真表示这次发送是 40054005 换 seq 后的重试。
	RetrySeq bool
}

type sendEnvelope struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	ExtInfo *struct {
		RefIdx string `json:"ref_idx"`
	} `json:"ext_info"`
}

// Send 发出一条消息 (文本 msg_type 0 / 富媒体 msg_type 7)。
//
// 被动模式带 msg_id + msg_seq; 主动模式不带 msg_id (占主动额度, 由上层决策)。
// 重试: 40054005(去重) 换新 msg_seq 重试一次; 401 刷 token 重试一次 (在
// callREST 内部)。返回的错误可用 AsAPIError / IsAPIErrorCode 判定。
func (a *APIClient) Send(ctx context.Context, req SendRequest) (*SendResponse, error) {
	endpoint, _, err := a.MessageEndpoint(req.Target)
	if err != nil {
		return nil, err
	}
	if !req.HasPayload() {
		return nil, fmt.Errorf("qq: 发送内容为空")
	}

	body := map[string]any{}
	if media := strings.TrimSpace(req.Media); media != "" {
		// msg_type 7 富媒体: file_info 由 UploadMedia 拿到的凭据。
		body["msg_type"] = 7
		body["media"] = map[string]any{"file_info": media}
	} else {
		// msg_type 0 纯文本。
		body["msg_type"] = 0
		body["content"] = req.Text
		// 出站引用渲染引用卡片, 与 msg_id 正交: msg_id 永远指向触发本轮的消息,
		// message_reference 指向被引用消息。引用只挂在文本消息上。
		if reference := a.MessageReferenceFor(req.QuoteMsgID); reference != nil {
			body["message_reference"] = reference
		}
	}

	passive := req.Decision.Mode == ModePassive && strings.TrimSpace(req.Decision.TriggerID) != ""
	seq := 0
	if passive {
		seq = req.Decision.SeqValue()
		body["msg_id"] = strings.TrimSpace(req.Decision.TriggerID)
		body["msg_seq"] = seq
	}

	var envelope sendEnvelope
	err = a.callREST(ctx, http.MethodPost, endpoint, body, &envelope)
	retried := false
	if err != nil && passive && IsAPIErrorCode(err, ErrCodeDedup) {
		// 40054005「消息被去重」: 换个 msg_seq 重发一次可通过, 原样重发只会再次被拒。
		retrySeq := seq + 1
		if req.Reseq != nil {
			if fresh := req.Reseq(seq); fresh > 0 {
				retrySeq = fresh
			}
		}
		a.log("qq: 命中 40054005, 换 msg_seq=%d 重试 (trigger=%s)", retrySeq, req.Decision.TriggerID)
		body["msg_seq"] = retrySeq
		var again sendEnvelope
		if retryErr := a.callREST(ctx, http.MethodPost, endpoint, body, &again); retryErr == nil {
			seq = retrySeq
			retried = true
			envelope = again
			err = nil
		} else {
			// 保留原始去重错误 (更能说明失败原因)。
			err = fmt.Errorf("%w (重试 msg_seq=%d 仍失败: %v)", err, retrySeq, retryErr)
		}
	}
	if err != nil {
		return nil, classifySendError(err)
	}

	response := &SendResponse{
		MessageID: strings.TrimSpace(envelope.ID),
		Passive:   passive,
		Seq:       seq,
		RetrySeq:  retried,
	}
	if envelope.ExtInfo != nil {
		response.RefIdx = strings.TrimSpace(envelope.ExtInfo.RefIdx)
	}
	// 登记自发消息台账: 全量模式下平台会回推自己这条发言, 靠它识别并丢弃。
	a.mu.RLock()
	sent := a.sent
	a.mu.RUnlock()
	if sent != nil && response.MessageID != "" {
		id, isGroup := req.Target.Target()
		sent.Record(SentEntry{
			Scope:     id,
			MessageID: response.MessageID,
			Text:      firstNonEmpty(req.Text, req.Media),
			Passive:   passive,
			Group:     isGroup,
		})
	}
	return response, nil
}

// MessageReferenceFor 把平台消息 id 转成官方 message_reference。
//
// 引用别人用入站登记的 msg_idx, 引用自己用发送响应登记的 ref_idx —— 两条路径都
// 走 RefLedger.RefIdxFor。查不到 (索引丢失 / 过期) 返回 nil, 调用方应不带引用照常
// 发送, 避免引用值非法被平台整条拒收。
func (a *APIClient) MessageReferenceFor(messageID string) map[string]any {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil
	}
	a.mu.RLock()
	refs := a.refs
	a.mu.RUnlock()
	key := messageID
	if refs != nil {
		if resolved := refs.RefIdxFor(messageID); resolved != "" {
			key = resolved
		}
	}
	if !IsRefKey(key) {
		// platform 需要 REFIDX_ 形态的引用键, 拿不到就宁可不引用。
		return nil
	}
	return map[string]any{
		"message_id":               key,
		"ignore_get_message_error": true,
	}
}

// truncate 截断文本用于错误信息 (避免把整段响应塞进日志)。
// P3b: 回退到 rune 边界, 避免把多字节 UTF-8 字符切成非法字节序列。
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + "…"
}
