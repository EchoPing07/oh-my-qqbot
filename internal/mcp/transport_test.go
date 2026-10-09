package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// roundTripFunc 是 http.RoundTripper 的函数式替身。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// 本文件覆盖三种传输的真实建连:
//   - stdio:真子进程(本测试二进制以 -test.run=TestMain 自举为 MCP 服务器),
//     并故意在 stdout 打印噪声行验证 tolerantStdioTransport 的清洗;
//   - streamable_http / sse:httptest 起真 HTTP 服务端,走 go-sdk 的两种客户端
//     传输,并断言 Authorization 头与超时配置透传。

func TestStdioTransportConnectsRealSubprocess(t *testing.T) {
	command := stdioCommandForTest()
	transport := &tolerantStdioTransport{Command: command, Logger: quietLogger()}

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{
		Logger: quietLogger(),
	}).Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("stdio 传输连接失败(噪声行应被清洗而非中断协商): %v", err)
	}
	defer func() { _ = session.Close() }()

	// 工具发现。
	tools := make([]*mcp.Tool, 0)
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("列举工具失败: %v", err)
		}
		tools = append(tools, tool)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("stdout 子进程工具列表异常: %+v", tools)
	}

	// 真实调用。
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": "子进程"},
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("内容块数量异常: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "stdio-echo:子进程" {
		t.Fatalf("结果异常: %+v", result.Content[0])
	}
}

func TestServiceStdioTransportEndToEnd(t *testing.T) {
	// 通过配置走完整链路:command/args/env → 子进程 → 工具注册 → 调用。
	config := MCPConfig{
		Enable: true,
		Client: MCPClientConfig{ClientName: "MaiBot", ClientVersion: "1.0.0"},
		Servers: []MCPServerItemConfig{
			stdioServerItemConfig("stdio-e2e"),
		},
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("配置校验失败: %v", err)
	}

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	snapshot := service.StatusSnapshot()
	if snapshot.ServerCount != 1 || snapshot.ToolCount != 1 {
		t.Fatalf("快照异常: %+v", snapshot)
	}
	if snapshot.Servers[0].Transport != "stdio" || !snapshot.Servers[0].Connected {
		t.Fatalf("stdio 服务器状态异常: %+v", snapshot.Servers[0])
	}
	if snapshot.Servers[0].ProtocolVersion == "" {
		t.Fatalf("协议版本未记录: %+v", snapshot.Servers[0])
	}

	result := service.CallTool(context.Background(), ToolInvocation{
		ToolName:  "echo",
		Arguments: map[string]any{"text": "经配置链接"},
	}, SessionContext{SessionID: "stdio"})
	if !result.Success {
		t.Fatalf("调用失败: %+v", result)
	}
	if !strings.Contains(result.Content, "stdio-echo:经配置链接") {
		t.Fatalf("结果异常: %q", result.Content)
	}
	if result.Metadata["server_name"] != "stdio-e2e" {
		t.Fatalf("元数据异常: %+v", result.Metadata)
	}
	if result.Metadata["chat_session_id"] != "stdio" {
		t.Fatalf("会话审计元数据缺失: %+v", result.Metadata)
	}

	// 关闭后子进程连接释放（再次调用失败）。
	if err := service.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	after := service.CallTool(context.Background(), ToolInvocation{ToolName: "echo"}, SessionContext{})
	if after.Success {
		t.Fatalf("关闭后不应可调用: %+v", after)
	}
}

func TestTolerantReaderDropsNonJSONLines(t *testing.T) {
	// 直接验证清洗逻辑:非 JSON 行被丢弃,合法 JSON-RPC 帧原样通过。
	logger := quietLogger()
	reader := newTolerantReader(strings.NewReader(strings.Join([]string{
		"npm warn something",
		"",
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		"  banner with spaces  ",
		`[{"jsonrpc":"2.0"}]`,
		"{not json but starts with brace}",
		`{"jsonrpc":"2.0","id":2,"result":{}}`,
	}, "\n")+"\n"), logger)

	buffer := make([]byte, 1024)
	first, err := reader.Read(buffer)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got := strings.TrimSpace(string(buffer[:first])); got != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Fatalf("第一帧异常: %q", got)
	}

	second, err := reader.Read(buffer)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got := strings.TrimSpace(string(buffer[:second])); got != `[{"jsonrpc":"2.0"}]` {
		t.Fatalf("第二帧异常: %q", got)
	}

	third, err := reader.Read(buffer)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got := strings.TrimSpace(string(buffer[:third])); got != `{"jsonrpc":"2.0","id":2,"result":{}}` {
		t.Fatalf("第三帧异常（非法 JSON 行应被丢弃）: %q", got)
	}

	if _, err := reader.Read(buffer); err == nil {
		t.Fatalf("应当读到 EOF")
	}
}

func TestStdioTransportMissingCommandErrors(t *testing.T) {
	transport := &tolerantStdioTransport{Logger: quietLogger()}
	if _, err := transport.Connect(context.Background()); err == nil {
		t.Fatalf("缺少 command 应当报错")
	}

	connection := newConnection(ServerRuntimeConfig{
		Name:      "bad",
		Transport: TransportStdio,
	}, ClientRuntimeConfig{}, &HostCallbacks{})
	if connection.connect(context.Background()) {
		t.Fatalf("缺少 command 的连接应当失败")
	}
	if !strings.Contains(connection.lastErrorText(), "缺少 command 配置") {
		t.Fatalf("错误信息异常: %q", connection.lastErrorText())
	}
}

func TestStdioFailingCommandReportsError(t *testing.T) {
	// 起一个立刻退出的命令:连接必须失败且错误信息非空（对齐 _connect_all 的
	// `connection_errors` 语义）。
	connection := newConnection(ServerRuntimeConfig{
		Name:      "failing",
		Transport: TransportStdio,
		Command:   "definitely-not-an-existing-binary-xyz",
	}, ClientRuntimeConfig{}, &HostCallbacks{})
	if connection.connect(context.Background()) {
		t.Fatalf("不存在的命令应当连接失败")
	}
	if strings.TrimSpace(connection.lastErrorText()) == "" {
		t.Fatalf("应记录失败原因")
	}
}

func TestStdioEnvIsPassedToSubprocess(t *testing.T) {
	// env 追加到默认环境（cmd.Env = os.Environ() + 配置项）。
	pairs := envPairs(map[string]string{"B": "2", "A": "1"})
	if len(pairs) != 2 || pairs[0] != "A=1" || pairs[1] != "B=2" {
		t.Fatalf("env 转换异常: %+v", pairs)
	}
}

// ---------------------------------------------------------------------------
// streamable_http / sse 真建连
// ---------------------------------------------------------------------------

func TestStreamableHTTPTransportEndToEnd(t *testing.T) {
	callCount := 0
	httpServer := newHTTPTestServer(t, testServerOptions{
		onCall: func(name string, args map[string]any) { callCount++ },
	})
	defer httpServer.close()

	config := MCPConfig{
		Enable: true,
		Client: MCPClientConfig{ClientName: "MaiBot", ClientVersion: "1.0.0"},
		Servers: []MCPServerItemConfig{{
			Name:               "http-server",
			Enabled:            true,
			Transport:          "streamable_http",
			URL:                httpServer.streamableURL,
			Headers:            map[string]string{"X-Custom": "yes"},
			HTTPTimeoutSeconds: 10,
			ReadTimeoutSeconds: 30,
			Authorization:      MCPAuthorizationConfig{Mode: "bearer", BearerToken: "secret-token"},
		}},
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("配置校验失败: %v", err)
	}

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	snapshot := service.StatusSnapshot()
	if snapshot.ServerCount != 1 || snapshot.ToolCount != 1 {
		t.Fatalf("HTTP 服务器快照异常: %+v", snapshot)
	}
	if snapshot.Servers[0].Transport != "streamable_http" {
		t.Fatalf("传输类型异常: %+v", snapshot.Servers[0])
	}

	result := service.CallTool(context.Background(), ToolInvocation{
		ToolName:  "echo",
		Arguments: map[string]any{"text": "http"},
	}, SessionContext{})
	if !result.Success {
		t.Fatalf("HTTP 调用失败: %+v", result)
	}
	if !strings.Contains(result.Content, "http-echo:http") {
		t.Fatalf("结果异常: %q", result.Content)
	}
	if callCount != 1 {
		t.Fatalf("远程调用次数 = %d", callCount)
	}

	// authorization 与自定义 headers 必须真实出现在请求上。
	if got := httpServer.sawAuthorization(); got != "Bearer secret-token" {
		t.Fatalf("Authorization 未透传: %q", got)
	}
}

func TestSSETransportEndToEnd(t *testing.T) {
	httpServer := newHTTPTestServer(t, testServerOptions{})
	defer httpServer.close()

	config := MCPConfig{
		Enable: true,
		Client: MCPClientConfig{ClientName: "MaiBot", ClientVersion: "1.0.0"},
		Servers: []MCPServerItemConfig{{
			Name:               "sse-server",
			Enabled:            true,
			Transport:          "sse",
			URL:                httpServer.sseURL,
			HTTPTimeoutSeconds: 10,
			ReadTimeoutSeconds: 30,
		}},
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("配置校验失败: %v", err)
	}

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	snapshot := service.StatusSnapshot()
	if snapshot.ServerCount != 1 || snapshot.ToolCount != 1 {
		t.Fatalf("SSE 服务器快照异常: %+v", snapshot)
	}
	if snapshot.Servers[0].Transport != "sse" {
		t.Fatalf("传输类型异常: %+v", snapshot.Servers[0])
	}

	result := service.CallTool(context.Background(), ToolInvocation{
		ToolName:  "echo",
		Arguments: map[string]any{"text": "sse"},
	}, SessionContext{})
	if !result.Success {
		t.Fatalf("SSE 调用失败: %+v", result)
	}
	if !strings.Contains(result.Content, "http-echo:sse") {
		t.Fatalf("结果异常: %q", result.Content)
	}
}

func TestHTTPTransportFailureIsReported(t *testing.T) {
	// 指向未监听的端口:连接失败但不 panic，错误进入快照。
	config := MCPConfig{
		Enable: true,
		Servers: []MCPServerItemConfig{{
			Name:               "dead",
			Enabled:            true,
			Transport:          "streamable_http",
			URL:                "http://127.0.0.1:1/mcp",
			HTTPTimeoutSeconds: 0.5,
			ReadTimeoutSeconds: 1,
		}},
	}
	service := NewService()
	defer func() { _ = service.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = service.EnsureInitialized(context.Background(), config)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("连接失败场景不应无限阻塞")
	}

	snapshot := service.StatusSnapshot()
	if snapshot.ServerCount != 0 || len(snapshot.Servers) != 1 || snapshot.Servers[0].Connected {
		t.Fatalf("失败快照异常: %+v", snapshot)
	}
	if strings.TrimSpace(snapshot.Servers[0].Error) == "" {
		t.Fatalf("应记录连接错误: %+v", snapshot.Servers[0])
	}
}

func TestConnectTimeoutBounds(t *testing.T) {
	// 非 stdio:min(60, max(5, http_timeout + 5))；stdio 固定 30s。
	httpConn := newConnection(ServerRuntimeConfig{
		Transport:          TransportStreamableHTTP,
		URL:                "http://x",
		HTTPTimeoutSeconds: 0.5,
	}, ClientRuntimeConfig{}, &HostCallbacks{})
	// 0.5 + 5 = 5.5s 已高于下限 5s，故保持 5.5s。
	if got := httpConn.connectTimeout(); got != 5500*time.Millisecond {
		t.Fatalf("超时应为 http_timeout + 5s, got %s", got)
	}
	// 超时缺省时优先取默认值 30s (BuildServerRuntimeConfig 负责填充)。
	httpConn = newConnection(BuildServerRuntimeConfig(MCPServerItemConfig{
		Name: "x", Enabled: true, Transport: "streamable_http", URL: "http://x",
	}), ClientRuntimeConfig{}, &HostCallbacks{})
	if got := httpConn.connectTimeout(); got != 35*time.Second {
		t.Fatalf("默认超时 30s + 5s 应为 35s, got %s", got)
	}
	httpConn = newConnection(ServerRuntimeConfig{
		Transport:          TransportStreamableHTTP,
		URL:                "http://x",
		HTTPTimeoutSeconds: 120,
	}, ClientRuntimeConfig{}, &HostCallbacks{})
	if got := httpConn.connectTimeout(); got != maxConnectTimeout {
		t.Fatalf("大超时应压到 60s, got %s", got)
	}
	httpConn = newConnection(ServerRuntimeConfig{
		Transport:          TransportStreamableHTTP,
		URL:                "http://x",
		HTTPTimeoutSeconds: 20,
	}, ClientRuntimeConfig{}, &HostCallbacks{})
	if got := httpConn.connectTimeout(); got != 25*time.Second {
		t.Fatalf("应等于 http_timeout + 5s, got %s", got)
	}
	stdioConn := newConnection(ServerRuntimeConfig{Transport: TransportStdio, Command: "x"}, ClientRuntimeConfig{}, &HostCallbacks{})
	if got := stdioConn.connectTimeout(); got != stdioConnectTimeout {
		t.Fatalf("stdio 超时应固定 30s, got %s", got)
	}
}

func TestBuildTransportRejectsUnknownAndIncomplete(t *testing.T) {
	cases := []struct {
		name    string
		config  ServerRuntimeConfig
		wantErr string
	}{
		{
			name:    "未知传输",
			config:  ServerRuntimeConfig{Name: "a", Transport: Transport("carrier-pigeon")},
			wantErr: "未知传输类型",
		},
		{
			name:    "http 缺 url",
			config:  ServerRuntimeConfig{Name: "a", Transport: TransportStreamableHTTP},
			wantErr: "缺少 url 配置",
		},
		{
			name:    "sse 缺 url",
			config:  ServerRuntimeConfig{Name: "a", Transport: TransportSSE},
			wantErr: "缺少 url 配置",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			connection := newConnection(testCase.config, ClientRuntimeConfig{}, &HostCallbacks{})
			_, err := connection.buildTransport()
			if err == nil {
				t.Fatalf("应当报错")
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("错误文本 %q 不含 %q", err.Error(), testCase.wantErr)
			}
		})
	}
}

func TestHTTPClientHeaderTransportOverrides(t *testing.T) {
	// headerAddingTransport 显式配置覆盖 SDK 生成的同名头。
	var seen http.Header
	transport := &headerAddingTransport{
		base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			seen = request.Header
			return nil, nil
		}),
		headers: map[string]string{"Accept": "application/json", "Authorization": "Bearer x"},
	}
	request, err := http.NewRequest("GET", "http://example.com", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Accept", "text/event-stream")
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}
	// headerAddingTransport 在克隆出来的请求上覆盖头（避免修改调用方请求），
	// 因此校验替身观察到的那份克隆。
	if seen.Get("Accept") != "application/json" {
		t.Fatalf("配置头未覆盖: %q", seen.Get("Accept"))
	}
	if seen.Get("Authorization") != "Bearer x" {
		t.Fatalf("Authorization 未附加: %q", seen.Get("Authorization"))
	}
	// 调用方原始请求不被修改（http.Request.Clone 语义）。
	if request.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("不应修改调用方请求: %q", request.Header.Get("Accept"))
	}
}

func TestBuildHTTPClientAppliesTimeouts(t *testing.T) {
	connection := newConnection(ServerRuntimeConfig{
		Transport:          TransportStreamableHTTP,
		URL:                "http://x",
		HTTPTimeoutSeconds: 7,
		ReadTimeoutSeconds: 90,
		Headers:            map[string]string{"X-A": "1"},
	}, ClientRuntimeConfig{}, &HostCallbacks{})

	client := connection.buildHTTPClient()
	if client.Timeout != 90*time.Second {
		t.Fatalf("读取超时未应用: %s", client.Timeout)
	}
	if _, ok := client.Transport.(*headerAddingTransport); !ok {
		t.Fatalf("应为 headerAddingTransport: %T", client.Transport)
	}
}
