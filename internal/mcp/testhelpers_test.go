package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 测试辅助:内存/进程内 MCP 服务器与传输注入。
//
// 说明:go-sdk 的三种传输全部通过 `mcp.Transport` 接口接入,因此单测用三条路径
// 覆盖:
//  1. `mcp.NewInMemoryTransports()` 直连进程内服务器(覆盖 Manager/Service/
//     Provider/Sampling 的全部语义);
//  2. 真子进程 + `tolerantStdioTransport`(覆盖 stdio 传输与 stdout 噪声清洗);
//  3. `httptest.NewServer(mcp.NewStreamableHTTPHandler/NewSSEHandler)` +
//     `mcp.StreamableClientTransport` / `mcp.SSEClientTransport`(覆盖两种 HTTP
//     传输的真实建连、Authorization 头透传)。
//
// 注意:go-sdk 在协议版本 >= 2026-07-28 时禁止服务端在处理器内直接发起
// sampling/roots 请求(SEP-2322),必须返回 InputRequests 并由客户端的
// multi-round-trip 中间件自动补全后重试。测试服务器按此规范实现,因此它同时
// 验证了「服务端采样请求 → 宿主桥 → 客户端回包」的完整往返。

const (
	testToolSchema = `{"type":"object","properties":{"text":{"type":"string","description":"要回显的文本"},"count":{"type":"integer"}},"required":["text"]}`
	testServerName = "testserver"
)

// testServerOptions 控制测试服务器的形状。
type testServerOptions struct {
	// name 是服务端实现名。
	name string
	// extraTools 是额外工具名(tools/list 中暴露),默认含 echo/boom。
	extraTools []string
	// toolName 覆盖主回显工具名（多服务器场景避免重名冲突）。
	toolName string
	// withPrompts / withResources 控制能力声明与列表内容。
	withPrompts   bool
	withResources bool
	// onCall 在工具被调用时执行(用于断言参数透传)。
	onCall func(name string, args map[string]any)
	// sampling 为 true 时主工具会向客户端发起 sampling/createMessage。
	sampling bool
	// samplingTools 为 true 时主工具用 CreateMessageWithTools 发起采样。
	samplingTools bool
	// roots 为 true 时主工具会请求客户端 roots 列表。
	roots bool
}

func (o testServerOptions) echoName() string {
	if o.toolName != "" {
		return o.toolName
	}
	return "echo"
}

func (o testServerOptions) nameOrDefault() string {
	if o.name != "" {
		return o.name
	}
	return testServerName
}

// newTestServer 构造进程内 MCP 服务器与其客户端传输。
func newTestServer(t *testing.T, opts testServerOptions) (*mcp.Server, *mcp.InMemoryTransport, func()) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: opts.nameOrDefault(), Version: "1.0.0"}, nil)

	echoInputs := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text":  map[string]any{"type": "string", "description": "要回显的文本"},
			"count": map[string]any{"type": "integer"},
		},
		"required": []string{"text"},
	}

	// 主回显工具：支持 sampling / roots 的服务端发起请求（multi-round-trip）。
	mcp.AddTool(server, &mcp.Tool{
		Name:        opts.echoName(),
		Title:       "回显工具",
		Description: "回显输入文本",
		InputSchema: echoInputs,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		text, _ := args["text"].(string)
		if opts.onCall != nil {
			opts.onCall(opts.echoName(), args)
		}

		switch {
		case opts.sampling:
			return serverSamplingRoundTrip(ctx, req, text, false)
		case opts.samplingTools:
			return serverSamplingRoundTrip(ctx, req, text, true)
		case opts.roots:
			return serverRootsRoundTrip(ctx, req)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "echo:" + text},
			&mcp.ImageContent{MIMEType: "image/png", Data: []byte("aGVsbG8=")},
		}}, nil, nil
	})

	// 错误工具（名字可覆盖）。
	boomName := "boom"
	if opts.toolName != "" {
		boomName = opts.toolName + "_boom"
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        boomName,
		Description: "总是报错",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "出错了"}},
		}, nil, nil
	})

	for _, name := range opts.extraTools {
		extraName := name
		mcp.AddTool(server, &mcp.Tool{
			Name:        extraName,
			Description: "额外工具 " + extraName,
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"value": map[string]any{"type": "string"}},
			},
		}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			if opts.onCall != nil {
				opts.onCall(extraName, args)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: extraName + ":ok"},
			}}, nil, nil
		})
	}

	if opts.withPrompts {
		server.AddPrompt(&mcp.Prompt{
			Name:        "greet",
			Description: "打招呼",
			Arguments:   []*mcp.PromptArgument{{Name: "who", Description: "对象", Required: true}},
		}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			who := req.Params.Arguments["who"]
			return &mcp.GetPromptResult{
				Description: "问候提示词",
				Messages: []*mcp.PromptMessage{
					{Role: "user", Content: &mcp.TextContent{Text: "你好 " + who}},
				},
			}, nil
		})
	}

	if opts.withResources {
		server.AddResource(&mcp.Resource{
			URI:         "mem://greeting",
			Name:        "问候",
			Description: "内存资源",
			MIMEType:    "text/plain",
		}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
				{URI: req.Params.URI, MIMEType: "text/plain", Text: "你好，世界"},
			}}, nil
		})
	}

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("连接测试服务器失败: %v", err)
	}
	return server, clientTransport, func() { _ = serverSession.Close() }
}

// serverSamplingRoundTrip 以 multi-round-trip 形态发起 sampling 请求:
// 首轮返回 InputRequests,客户端中间件补全后重试,第二轮读取 InputResponses。
func serverSamplingRoundTrip(
	ctx context.Context,
	req *mcp.CallToolRequest,
	text string,
	withTools bool,
) (*mcp.CallToolResult, any, error) {
	key := "sampling-1"
	if responses := req.Params.InputResponses; len(responses) > 0 {
		response, ok := responses[key]
		if !ok {
			return nil, nil, fmt.Errorf("缺少 sampling 回包: %v", responses)
		}
		sampled := ""
		model := ""
		stop := ""
		role := ""
		switch typed := response.(type) {
		case *mcp.CreateMessageResult:
			if content, ok := typed.Content.(*mcp.TextContent); ok {
				sampled = content.Text
			}
			model, stop, role = typed.Model, typed.StopReason, string(typed.Role)
		case *mcp.CreateMessageWithToolsResult:
			for _, content := range typed.Content {
				switch block := content.(type) {
				case *mcp.TextContent:
					sampled += block.Text
				case *mcp.ToolUseContent:
					sampled += fmt.Sprintf("[call %s %v]", block.Name, block.Input)
				}
			}
			model, stop, role = typed.Model, typed.StopReason, string(typed.Role)
		default:
			return nil, nil, fmt.Errorf("未知 sampling 回包类型 %T", response)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("echo=%s sampled=%s model=%s stop=%s role=%s", text, sampled, model, stop, role)},
		}}, nil, nil
	}

	if withTools {
		return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{
			key: &mcp.CreateMessageWithToolsParams{
				MaxTokens: 128,
				Messages: []*mcp.SamplingMessageV2{
					{Role: "user", Content: []mcp.Content{&mcp.TextContent{Text: text}}},
				},
				SystemPrompt: "服务端系统提示",
				Temperature:  0.5,
				Tools: []*mcp.Tool{{
					Name:        "server_side_tool",
					Description: "服务端工具",
					InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
				}},
				ToolChoice: &mcp.ToolChoice{Mode: "auto"},
			},
		}}, nil, nil
	}
	return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{
		key: &mcp.CreateMessageParams{
			MaxTokens:    128,
			Messages:     []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}},
			SystemPrompt: "服务端系统提示",
			Temperature:  0.5,
		},
	}}, nil, nil
}

// serverRootsRoundTrip 以 multi-round-trip 形态请求客户端 roots 列表。
func serverRootsRoundTrip(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, any, error) {
	key := "roots-1"
	responses := req.Params.InputResponses
	if len(responses) == 0 {
		return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{
			key: &mcp.ListRootsParams{},
		}}, nil, nil
	}
	response, ok := responses[key].(*mcp.ListRootsResult)
	if !ok {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("roots-unexpected:%T", responses[key])},
		}}, nil, nil
	}
	uris := make([]string, 0, len(response.Roots))
	for _, root := range response.Roots {
		uris = append(uris, root.URI)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "roots=" + strings.Join(uris, ",")},
	}}, nil, nil
}

// installInMemoryTransport 让所有服务器都连到「每服务器一个新测试服务器」的
// 内存传输上，并返回 restore。
func installInMemoryTransport(t *testing.T, opts testServerOptions) func() {
	t.Helper()
	return installInMemoryTransportFor(t, func(ServerRuntimeConfig) testServerOptions { return opts })
}

// installInMemoryTransportFor 允许按服务器定制测试服务器形状。
func installInMemoryTransportFor(t *testing.T, optsFor func(ServerRuntimeConfig) testServerOptions) func() {
	t.Helper()
	servers := &sync.Map{} // config.Name -> *mcp.Server（保持引用防止被 GC）
	return setTransportHook(func(config ServerRuntimeConfig) (mcp.Transport, error) {
		server, clientTransport, _ := newTestServer(t, optsFor(config))
		servers.Store(config.Name, server)
		return clientTransport, nil
	})
}

// ---------------------------------------------------------------------------
// 真 stdio 子进程
// ---------------------------------------------------------------------------

// testStdioServerEnv 是「以子进程方式启动测试 MCP 服务器」的环境变量开关。
const testStdioServerEnv = "OHMYQQBOT_MCP_TEST_STDIO_SERVER"

// TestMain 让同一个 test binary 可以充当 stdio MCP 服务器
// (go test 编译出的可执行文件由 -test.run 驱动)。
func TestMain(m *testing.M) {
	if os.Getenv(testStdioServerEnv) == "1" {
		runStdioTestServer()
		return
	}
	os.Exit(m.Run())
}

// runStdioTestServer 以 stdio 传输运行最小 MCP 服务器（真实子进程场景）。
func runStdioTestServer() {
	server := mcp.NewServer(&mcp.Implementation{Name: "stdio-test-server", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "echo",
		Description: "回显输入文本",
		InputSchema: json.RawMessage(testToolSchema),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		text, _ := args["text"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "stdio-echo:" + text},
		}}, nil, nil
	})
	// 故意往 stdout 打一行噪声,验证 tolerantStdioTransport 的清洗
	//（对齐 stdio_filter.py 的动机:第三方服务器启动横幅污染协议流）。
	fmt.Fprintln(os.Stdout, "starting stdio-test-server v1 ...")
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "stdio test server failed:", err)
		os.Exit(1)
	}
}

// stdioTestConfig 构造指向「本测试二进制自身」的 stdio 服务器配置。
func stdioServerItemConfig(name string) MCPServerItemConfig {
	return MCPServerItemConfig{
		Name:      name,
		Enabled:   true,
		Transport: "stdio",
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestMain"},
		Env:       map[string]string{testStdioServerEnv: "1"},
	}
}

// stdioCommandForTest 返回等价的 exec.Cmd（供 tolerantStdioTransport 直连测试）。
func stdioCommandForTest() *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append(os.Environ(), testStdioServerEnv+"=1")
	return cmd
}

// ---------------------------------------------------------------------------
// HTTP 传输（真建连）
// ---------------------------------------------------------------------------

// httpTestServer 启动真实的 streamable_http / sse 端点。
type httpTestServer struct {
	streamableURL string
	sseURL        string
	close         func()
	// headers 记录每次请求收到的 Header（用于断言 authorization 透传）。
	mu      sync.Mutex
	headers []http.Header
}

func newHTTPTestServer(t *testing.T, opts testServerOptions) *httpTestServer {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "http-test-server", Version: "1.0.0"}, nil)
	echoName := opts.echoName()
	mcp.AddTool(server, &mcp.Tool{
		Name:        echoName,
		Description: "回显输入文本",
		InputSchema: json.RawMessage(testToolSchema),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		if opts.onCall != nil {
			opts.onCall(echoName, args)
		}
		text, _ := args["text"].(string)
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "http-echo:" + text},
		}}, nil, nil
	})

	streamableHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	sseHandler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil)

	result := &httpTestServer{}
	mux := http.NewServeMux()
	mux.Handle("/mcp", streamableHandler)
	mux.Handle("/sse", sseHandler)
	recorder := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result.mu.Lock()
		result.headers = append(result.headers, r.Header.Clone())
		result.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
	httpServer := httptest.NewServer(recorder)
	result.streamableURL = httpServer.URL + "/mcp"
	result.sseURL = httpServer.URL + "/sse"
	result.close = httpServer.Close
	return result
}

func (s *httpTestServer) sawAuthorization() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, header := range s.headers {
		if value := header.Get("Authorization"); value != "" {
			return value
		}
	}
	return ""
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", message)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}
