package mcp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file mirrors 参考/MaiBot/src/mcp_module/connection.py: 单个 MCP 服务器的
// 连接生命周期(连接 → 发现能力 → 调用工具/读取资源 → 断开)。
//
// Go SDK 的 Client.Connect 已经在同一个调用栈里完成 transport 建立、initialize
// 协商与资源清理,因此 Go 版不需要 Python 的 `_lifecycle_task` + AsyncExitStack
// 结构;取而代之的是一个连接对象由互斥锁保护、Close 幂等。

// 连接超时参考 manager.py 的 connect_one:非 stdio 为
// min(60s, max(5s, http_timeout + 5s)),stdio 固定 30s。
const (
	minConnectTimeout     = 5 * time.Second
	maxConnectTimeout     = 60 * time.Second
	stdioConnectTimeout   = 30 * time.Second
	connectTimeoutPadding = 5 * time.Second

	// loadServerFeaturesTimeout 是能力发现（tools/prompts/resources 列表拉取）
	// 的整体超时（L14：此前直接继承调用方 context——connect 分支里是
	// detachContext 产物、无 deadline，服务端对 tools/list 不响应时会无限
	// 阻塞启动。30s 与 stdio 建连超时对齐）。
	loadServerFeaturesTimeout = 30 * time.Second
)

// errNilCommand 表示 stdio 传输缺少可执行命令。
var errNilCommand = errors.New("stdio 传输缺少 command")

// transportHook 是传输构造的测试接缝:非 nil 时替代 buildTransport 的结果。
// 生产代码永远不设置它;单测用它注入 go-sdk 的内存传输(或包装过的传输)。
var (
	transportHookMu sync.Mutex
	transportHook   func(config ServerRuntimeConfig) (mcp.Transport, error)
)

// setTransportHook installs a transport factory for the duration of a test and
// returns the restore function.
func setTransportHook(hook func(config ServerRuntimeConfig) (mcp.Transport, error)) func() {
	transportHookMu.Lock()
	previous := transportHook
	transportHook = hook
	transportHookMu.Unlock()
	return func() {
		transportHookMu.Lock()
		transportHook = previous
		transportHookMu.Unlock()
	}
}

func currentTransportHook() func(config ServerRuntimeConfig) (mcp.Transport, error) {
	transportHookMu.Lock()
	defer transportHookMu.Unlock()
	return transportHook
}

// ServerInfo 是单个服务器的状态快照项（对齐 manager.get_status_snapshot 的
// servers[] 条目）。
type ServerInfo struct {
	Name            string `json:"name"`
	Transport       string `json:"transport"`
	Connected       bool   `json:"connected"`
	ProtocolVersion string `json:"protocol_version"`
	ToolCount       int    `json:"tool_count"`
	Error           string `json:"error"`
}

// connection 管理单个 MCP 服务器的连接生命周期。
type connection struct {
	config       ServerRuntimeConfig
	clientConfig ClientRuntimeConfig
	callbacks    *HostCallbacks

	mu            sync.RWMutex
	session       *mcp.ClientSession
	cancelSession context.CancelFunc
	// onToolsChanged 在服务端宣告 tools/list_changed 后触发，用于重建工具路由表。
	onToolsChanged  func()
	tools           []*mcp.Tool
	prompts         []*mcp.Prompt
	resources       []*mcp.Resource
	resourceTmpl    []*mcp.ResourceTemplate
	protocolVersion string
	serverCaps      *mcp.ServerCapabilities
	lastError       string
	connected       bool
	closed          bool
}

func newConnection(
	config ServerRuntimeConfig,
	clientConfig ClientRuntimeConfig,
	callbacks *HostCallbacks,
) *connection {
	return &connection{
		config:       config,
		clientConfig: clientConfig,
		callbacks:    callbacks,
	}
}

// connect mirrors MCPConnection.connect: 建立连接并发现服务端能力，
// 失败时返回 false 并把错误写入 lastError。
//
// 生命周期：Python 版把 SDK 的异步作用域放在独立的 lifecycle task 里持有，
// 因此在建连超时/调用方取消后连接依然存活。Go 版用「分离上下文 + 显式 cancel」
// 达到同一效果：transport/会话的生命周期只由 connection.close() 结束，
// 建连超时只取消本次等待。
func (c *connection) connect(ctx context.Context) bool {
	timeout := c.connectTimeout()
	sessionCtx, cancelSession := detachContext(ctx)

	type connectResult struct {
		session *mcp.ClientSession
		err     error
	}
	results := make(chan connectResult, 1)
	go func() {
		session, err := c.openSession(sessionCtx)
		results <- connectResult{session: session, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case result := <-results:
		if result.err != nil {
			cancelSession()
			c.mu.Lock()
			c.lastError = connectErrorMessage(result.err, timeout)
			c.connected = false
			c.mu.Unlock()
			return false
		}
		c.mu.Lock()
		c.session = result.session
		c.cancelSession = cancelSession
		c.connected = true
		c.lastError = ""
		if initResult := result.session.InitializeResult(); initResult != nil {
			c.protocolVersion = initResult.ProtocolVersion
			c.serverCaps = initResult.Capabilities
		}
		c.mu.Unlock()

		if err := c.loadServerFeatures(sessionCtx); err != nil {
			c.mu.Lock()
			c.lastError = err.Error()
			c.connected = false
			c.mu.Unlock()
			_ = c.closeSession()
			return false
		}
		return true

	case <-timer.C:
		cancelSession()
		go func() { <-results }()
		c.mu.Lock()
		c.lastError = connectErrorMessage(context.DeadlineExceeded, timeout)
		c.connected = false
		c.mu.Unlock()
		return false

	case <-ctx.Done():
		cancelSession()
		go func() { <-results }()
		c.mu.Lock()
		c.lastError = connectErrorMessage(ctx.Err(), timeout)
		c.connected = false
		c.mu.Unlock()
		return false
	}
}

// detachContext 返回一个继承 ctx 值但不受其取消/超时影响的可取消上下文
// （对齐 go-sdk 内部 xcontext.Detach 的语义）。
type detachedContext struct {
	context.Context // 提供 Value
}

func (detachedContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (detachedContext) Done() <-chan struct{}       { return nil }
func (detachedContext) Err() error                  { return nil }

func detachContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(detachedContext{Context: ctx})
}

// connectTimeout mirrors manager.connect_one 的超时计算:
// 非 stdio 为 min(60s, max(5s, http_timeout + 5s)),stdio 固定 30s。
func (c *connection) connectTimeout() time.Duration {
	if c.config.TransportType() == TransportStdio {
		return stdioConnectTimeout
	}
	timeout := c.config.HTTPTimeoutSeconds + connectTimeoutPadding.Seconds()
	if timeout < minConnectTimeout.Seconds() {
		timeout = minConnectTimeout.Seconds()
	}
	if timeout > maxConnectTimeout.Seconds() {
		timeout = maxConnectTimeout.Seconds()
	}
	return time.Duration(timeout * float64(time.Second))
}

func connectErrorMessage(err error, timeout time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("连接超过 %g 秒", timeout.Seconds())
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "连接失败"
	}
	return message
}

// openSession 按配置建立传输并创建已初始化的 ClientSession。
func (c *connection) openSession(ctx context.Context) (*mcp.ClientSession, error) {
	transport, err := c.buildTransport()
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    c.clientConfig.ClientName,
		Version: c.clientConfig.ClientVersion,
	}, c.clientOptions())
	if roots := c.clientConfig.Roots; c.clientConfig.EnableRoots && len(roots) > 0 {
		sdkRoots := make([]*mcp.Root, 0, len(roots))
		for _, root := range roots {
			sdkRoots = append(sdkRoots, &mcp.Root{URI: root.URI, Name: root.Name})
		}
		client.AddRoots(sdkRoots...)
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// buildTransport 按 transport 类型构建 go-sdk transport。
func (c *connection) buildTransport() (mcp.Transport, error) {
	if hook := currentTransportHook(); hook != nil {
		return hook(c.config)
	}
	switch c.config.TransportType() {
	case TransportStdio:
		// #nosec G204 -- command/args 来自用户自己的配置文件（与 Python 版一致）。
		cmd := exec.Command(c.config.Command, c.config.Args...)
		if len(c.config.Env) > 0 {
			// 对齐 StdioServerParameters(env=...) 的语义:在默认环境之上追加
			// 配置的环境变量(而不是替换整个环境)。
			cmd.Env = append(os.Environ(), envPairs(c.config.Env)...)
		}
		return &tolerantStdioTransport{
			Command: cmd,
			Logger:  loggerOrDefault(nil).With("server", c.config.Name),
		}, nil

	case TransportStreamableHTTP:
		return &mcp.StreamableClientTransport{
			Endpoint:   c.config.URL,
			HTTPClient: c.buildHTTPClient(),
		}, nil

	case TransportSSE:
		return &mcp.SSEClientTransport{
			Endpoint:   c.config.URL,
			HTTPClient: c.buildHTTPClient(),
		}, nil

	default:
		return nil, c.unknownTransportError()
	}
}

// unknownTransportError mirrors `使用了未知传输类型` 分支，并补上具体缺失项，
// 便于用户定位（Python 版在配置不完整时也落到同一个 unknown 分支）。
func (c *connection) unknownTransportError() error {
	hint := ""
	switch c.config.Transport {
	case TransportStdio:
		hint = "（缺少 command 配置）"
	case TransportStreamableHTTP, TransportSSE:
		hint = "（缺少 url 配置）"
	}
	return fmt.Errorf("MCP 服务器 '%s' 使用了未知传输类型: %s%s", c.config.Name, c.config.Transport, hint)
}

// buildHTTPClient mirrors MCPConnection._build_http_client: 请求头(含 bearer)
// 与 connect/read/write 超时配置。
func (c *connection) buildHTTPClient() *http.Client {
	headers := c.config.BuildHTTPHeaders()
	readTimeout := c.config.ReadTimeoutSeconds
	roundTripper := http.DefaultTransport.(*http.Transport).Clone()
	// #nosec G402 -- 对齐 Python 版:远程 MCP 服务器可能使用自签证书,
	// 是否校验由用户环境决定(httpx 默认校验,此处保留系统默认值)。
	roundTripper.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{
		Transport: &headerAddingTransport{base: roundTripper, headers: headers},
		Timeout:   time.Duration(readTimeout * float64(time.Second)),
	}
}

// envPairs 把配置 env 转换为 exec.Cmd.Env 需要的 KEY=VALUE 形式。
func envPairs(env map[string]string) []string {
	pairs := make([]string, 0, len(env))
	for key, value := range env {
		pairs = append(pairs, key+"="+value)
	}
	sort.Strings(pairs)
	return pairs
}

// headerAddingTransport 把配置 headers 附加到每个出站请求。
type headerAddingTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerAddingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 {
		request = request.Clone(request.Context())
		for key, value := range t.headers {
			// 显式配置覆盖 SDK 生成的头(如 Accept)。
			request.Header.Set(key, value)
		}
	}
	return t.base.RoundTrip(request)
}

// clientOptions mirrors MCPConnection._create_client_session 的能力装配:
// sampling / elicitation / roots 回调按配置与回调可用性决定是否声明。
func (c *connection) clientOptions() *mcp.ClientOptions {
	options := &mcp.ClientOptions{
		Logger: loggerOrDefault(nil).With("server", c.config.Name),
		Capabilities: &mcp.ClientCapabilities{
			// Roots 能力由 AddRoots + RootsV2 显式声明(禁用默认的 listChanged)。
			RootsV2: &mcp.RootCapabilities{},
		},
	}

	sampling := c.callbacks.Sampling
	if c.clientConfig.EnableSampling && sampling != nil {
		options.CreateMessageWithToolsHandler = sampling.HandleWithTools
		if options.Capabilities.Sampling == nil {
			options.Capabilities.Sampling = &mcp.SamplingCapabilities{}
			if c.clientConfig.SamplingToolSupport {
				options.Capabilities.Sampling.Tools = &mcp.SamplingToolsCapabilities{}
			}
			if c.clientConfig.SamplingIncludeContextSupport {
				options.Capabilities.Sampling.Context = &mcp.SamplingContextCapabilities{}
			}
		}
	}

	if c.clientConfig.EnableElicitation && c.callbacks.Elicitation != nil {
		options.ElicitationHandler = c.callbacks.Elicitation
	}

	if c.clientConfig.EnableRoots && len(c.clientConfig.Roots) > 0 {
		options.Capabilities.RootsV2 = &mcp.RootCapabilities{}
	}

	if logging := c.callbacks.Logging; logging != nil {
		options.LoggingMessageHandler = logging
	}
	// tools/list_changed 仅在宿主显式订阅时安装：go-sdk 在该回调非空时会打开
	// subscriptions/listen 流，对不支持该能力的服务端会导致建连失败；
	// Python 版同样只在建连时发现工具，因此默认保持行为一致。
	if notifyChanged := c.callbacks.ToolListChanged; notifyChanged != nil {
		options.ToolListChangedHandler = func(ctx context.Context, request *mcp.ToolListChangedRequest) {
			notifyChanged(ctx, request)
			if err := c.loadServerFeatures(context.WithoutCancel(ctx)); err != nil {
				loggerOrDefault(nil).Warn("MCP 工具列表刷新失败",
					"server", c.config.Name, "error", err)
				return
			}
			if onChanged := c.toolsChanged(); onChanged != nil {
				onChanged()
			}
		}
	}

	return options
}

// toolsChanged 返回工具目录变更回调（由 manager 在注册时注入）。
func (c *connection) toolsChanged() func() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.onToolsChanged
}

// loadServerFeatures mirrors _load_server_features: 按服务端能力声明拉取
// 工具/提示/资源/资源模板(分页由 go-sdk 迭代器完成)。
func (c *connection) loadServerFeatures(ctx context.Context) error {
	session := c.sessionRef()
	if session == nil {
		return errors.New("MCP 会话未建立")
	}
	// L14 修复：包一层整体超时，服务端列表接口挂起时不会无限阻塞建连流程
	// 与启动（超时按连接失败处理，错误写入 lastError）。
	featuresCtx, cancel := context.WithTimeout(ctx, loadServerFeaturesTimeout)
	defer cancel()

	tools := make([]*mcp.Tool, 0)
	if c.supportsTools() {
		for tool, err := range session.Tools(featuresCtx, nil) {
			if err != nil {
				return fmt.Errorf("加载工具失败: %w", err)
			}
			tools = append(tools, tool)
		}
	}

	prompts := make([]*mcp.Prompt, 0)
	resources := make([]*mcp.Resource, 0)
	resourceTemplates := make([]*mcp.ResourceTemplate, 0)

	if c.supportsPrompts() {
		for prompt, err := range session.Prompts(featuresCtx, nil) {
			if err != nil {
				return fmt.Errorf("加载 Prompt 失败: %w", err)
			}
			prompts = append(prompts, prompt)
		}
	}
	if c.supportsResources() {
		for resource, err := range session.Resources(featuresCtx, nil) {
			if err != nil {
				return fmt.Errorf("加载资源失败: %w", err)
			}
			resources = append(resources, resource)
		}
		// list_resource_templates 是 OPTIONAL 能力:未实现时按空集合处理，
		// 不能毁掉整个连接(对齐 _load_resource_templates 的 METHOD_NOT_FOUND 分支)。
		for template, err := range session.ResourceTemplates(featuresCtx, nil) {
			if err != nil {
				break
			}
			resourceTemplates = append(resourceTemplates, template)
		}
	}

	c.mu.Lock()
	c.tools = tools
	c.prompts = prompts
	c.resources = resources
	c.resourceTmpl = resourceTemplates
	c.mu.Unlock()
	return nil
}

func (c *connection) sessionRef() *mcp.ClientSession {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.session
}

func (c *connection) supportsTools() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serverCaps != nil && c.serverCaps.Tools != nil
}

func (c *connection) supportsPrompts() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serverCaps != nil && c.serverCaps.Prompts != nil
}

func (c *connection) supportsResources() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serverCaps != nil && c.serverCaps.Resources != nil
}

// toolList 返回已发现的工具副本。
func (c *connection) toolList() []*mcp.Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]*mcp.Tool{}, c.tools...)
}

// protocolVersionValue 返回协商得到的协议版本。
func (c *connection) protocolVersionValue() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.protocolVersion
}

// isConnected mirrors `connection.session is not None`。
func (c *connection) isConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected && c.session != nil
}

// sessionID 返回当前连接协商得到的 MCP 会话标识。
func (c *connection) sessionID() string {
	session := c.sessionRef()
	if session == nil {
		return ""
	}
	return session.ID()
}

// lastErrorText mirrors connection.last_error。
func (c *connection) lastErrorText() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastError
}

// callTool mirrors MCPConnection.call_tool: 调用工具并把 MCP 内容块转换为统一
// 执行结果。
func (c *connection) callTool(ctx context.Context, toolName string, arguments map[string]any) ToolResult {
	session := c.sessionRef()
	if session == nil {
		return ToolResult{
			ToolName:     toolName,
			Success:      false,
			ErrorMessage: fmt.Sprintf("MCP 服务器 '%s' 未连接", c.config.Name),
			Metadata:     map[string]any{"server_name": c.config.Name},
		}
	}
	if arguments == nil {
		arguments = map[string]any{}
	}

	callCtx := ctx
	if timeout := c.config.ReadTimeoutSeconds; timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
		defer cancel()
	}

	result, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: toolName, Arguments: arguments})
	if err != nil {
		return ToolResult{
			ToolName:     toolName,
			Success:      false,
			ErrorMessage: fmt.Sprintf("MCP 工具 '%s' 执行失败: %v", toolName, err),
			Metadata:     map[string]any{"server_name": c.config.Name},
		}
	}

	items := BuildToolContentItems(result.Content)
	text := JoinTextParts(items)
	isError := result.IsError
	errorMessage := ""
	if isError {
		errorMessage = text
		text = ""
	}
	return ToolResult{
		ToolName:          toolName,
		Success:           !isError,
		Content:           text,
		ErrorMessage:      errorMessage,
		StructuredContent: result.StructuredContent,
		ContentItems:      items,
		Metadata: map[string]any{
			"server_name":      c.config.Name,
			"protocol_version": c.protocolVersionValue(),
			"session_id":       c.sessionID(),
		},
	}
}

// getPrompt mirrors MCPConnection.get_prompt。
func (c *connection) getPrompt(ctx context.Context, promptName string, arguments map[string]string) (*PromptResult, error) {
	session := c.sessionRef()
	if session == nil {
		return nil, fmt.Errorf("MCP 服务器 '%s' 未连接", c.config.Name)
	}
	result, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: promptName, Arguments: arguments})
	if err != nil {
		return nil, err
	}
	prompt := &PromptResult{
		PromptName:  promptName,
		ServerName:  c.config.Name,
		Description: result.Description,
		Messages:    make([]PromptMessage, 0, len(result.Messages)),
		Metadata:    metaToMap(result.Meta),
	}
	for _, message := range result.Messages {
		prompt.Messages = append(prompt.Messages, PromptMessage{
			Role:    string(message.Role),
			Content: BuildToolContentItem(message.Content),
		})
	}
	return prompt, nil
}

// readResource mirrors MCPConnection.read_resource。
func (c *connection) readResource(ctx context.Context, uri string) (*ResourceReadResult, error) {
	session := c.sessionRef()
	if session == nil {
		return nil, fmt.Errorf("MCP 服务器 '%s' 未连接", c.config.Name)
	}
	result, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, err
	}
	readResult := &ResourceReadResult{
		URI:        uri,
		ServerName: c.config.Name,
		Metadata:   metaToMap(result.Meta),
	}
	for _, content := range result.Contents {
		if content == nil {
			continue
		}
		item := ToolContentItem{
			ContentType: ContentResource,
			URI:         firstNonEmpty(content.URI, uri),
			MIMEType:    content.MIMEType,
			Text:        string(content.Text),
			Data:        string(content.Blob),
			Metadata:    metaToMap(content.Meta),
		}
		readResult.Contents = append(readResult.Contents, item)
	}
	return readResult, nil
}

// close mirrors MCPConnection.close: 幂等关闭，允许重复调用。
func (c *connection) close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	session := c.session
	cancel := c.cancelSession
	c.session = nil
	c.cancelSession = nil
	c.connected = false
	c.tools = nil
	c.prompts = nil
	c.resources = nil
	c.resourceTmpl = nil
	c.serverCaps = nil
	c.mu.Unlock()

	var firstError error
	if session != nil {
		firstError = session.Close()
	}
	if cancel != nil {
		cancel()
	}
	return firstError
}

// closeSession 关闭底层会话但保留连接对象(供 connect 失败回滚)。
func (c *connection) closeSession() error {
	c.mu.Lock()
	session := c.session
	cancel := c.cancelSession
	c.session = nil
	c.cancelSession = nil
	c.mu.Unlock()
	var firstError error
	if session != nil {
		firstError = session.Close()
	}
	if cancel != nil {
		cancel()
	}
	return firstError
}
