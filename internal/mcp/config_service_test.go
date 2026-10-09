package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 本文件覆盖 M12 的配置签名语义：稳定性、变化检测、停用草稿不触发重启。

func TestConfigSignatureStableAcrossCalls(t *testing.T) {
	config := MCPConfig{
		Enable: true,
		Client: MCPClientConfig{
			ClientName: "MaiBot", ClientVersion: "1.0.0",
			Roots:    MCPRootsConfig{Enable: true, Items: []MCPRootItemConfig{{Enabled: true, URI: "file:///a", Name: "a"}}},
			Sampling: MCPSamplingConfig{Enable: true, TaskName: "planner", ToolSupport: true},
		},
		Servers: []MCPServerItemConfig{
			{
				Name: "alpha", Enabled: true, Transport: "stdio", Command: "npx",
				Args: []string{"-y", "server"}, Env: map[string]string{"B": "2", "A": "1"},
				Authorization: MCPAuthorizationConfig{Mode: "none"},
			},
			{
				Name: "beta", Enabled: true, Transport: "streamable_http", URL: "https://example.com/mcp",
				Headers:       map[string]string{"X-Test": "1", "X-Other": "2"},
				Authorization: MCPAuthorizationConfig{Mode: "bearer", BearerToken: "tok"},
			},
		},
	}

	first := ConfigSignature(config)
	for i := 0; i < 50; i++ {
		if got := ConfigSignature(config); got != first {
			t.Fatalf("签名不稳定: #%d %s != %s", i, got, first)
		}
	}

	// map 迭代顺序不影响签名(去抖)。
	reordered := config
	reordered.Servers[0].Env = map[string]string{"A": "1", "B": "2"}
	reordered.Servers[1].Headers = map[string]string{"X-Other": "2", "X-Test": "1"}
	if got := ConfigSignature(reordered); got != first {
		t.Fatalf("map 顺序变化改变了签名: %s != %s", got, first)
	}
}

func TestConfigSignatureChanges(t *testing.T) {
	base := MCPConfig{
		Enable:  true,
		Client:  MCPClientConfig{ClientName: "MaiBot", ClientVersion: "1.0.0"},
		Servers: []MCPServerItemConfig{{Name: "alpha", Enabled: true, Transport: "stdio", Command: "npx"}},
	}
	baseline := ConfigSignature(base)

	cases := []struct {
		name   string
		mutate func(MCPConfig) MCPConfig
		// wantChange 报告该变更是否应当改变签名。
		wantChange bool
	}{
		{
			name: "启用草稿服务器",
			mutate: func(c MCPConfig) MCPConfig {
				// 停用的服务器不参与签名:enable=false → true 才算变化。
				c.Servers = append(c.Servers, MCPServerItemConfig{Name: "gamma", Enabled: false, Transport: "stdio", Command: "echo"})
				return c
			},
			wantChange: false,
		},
		{
			name: "停用服务器内容变化",
			mutate: func(c MCPConfig) MCPConfig {
				c.Servers = append(c.Servers, MCPServerItemConfig{Name: "gamma", Enabled: false, Transport: "stdio", Command: "other"})
				return c
			},
			wantChange: false,
		},
		{
			name: "启用新服务器",
			mutate: func(c MCPConfig) MCPConfig {
				c.Servers = append(c.Servers, MCPServerItemConfig{Name: "gamma", Enabled: true, Transport: "stdio", Command: "echo", Args: []string{"hi"}})
				return c
			},
			wantChange: true,
		},
		{
			name: "命令变化",
			mutate: func(c MCPConfig) MCPConfig {
				c.Servers[0].Command = "uvx"
				return c
			},
			wantChange: true,
		},
		{
			name: "参数变化",
			mutate: func(c MCPConfig) MCPConfig {
				c.Servers[0].Args = []string{"--flag"}
				return c
			},
			wantChange: true,
		},
		{
			name: "环境变量变化",
			mutate: func(c MCPConfig) MCPConfig {
				c.Servers[0].Env = map[string]string{"TOKEN": "x"}
				return c
			},
			wantChange: true,
		},
		{
			name: "超时变化",
			mutate: func(c MCPConfig) MCPConfig {
				c.Servers[0].HTTPTimeoutSeconds = 12
				return c
			},
			wantChange: true,
		},
		{
			name: "bearer token 变化",
			mutate: func(c MCPConfig) MCPConfig {
				c.Servers[0].Authorization = MCPAuthorizationConfig{Mode: "bearer", BearerToken: "a"}
				return c
			},
			wantChange: true,
		},
		{
			name: "客户端名变化",
			mutate: func(c MCPConfig) MCPConfig {
				c.Client.ClientName = "OtherBot"
				return c
			},
			wantChange: true,
		},
		{
			name: "sampling 任务名变化",
			mutate: func(c MCPConfig) MCPConfig {
				c.Client.Sampling = MCPSamplingConfig{Enable: true, TaskName: "utils"}
				return c
			},
			wantChange: true,
		},
		{
			name: "roots 变化",
			mutate: func(c MCPConfig) MCPConfig {
				c.Client.Roots = MCPRootsConfig{Enable: true, Items: []MCPRootItemConfig{{Enabled: true, URI: "file:///w"}}}
				return c
			},
			wantChange: true,
		},
		{
			name: "关闭 sampling 开关不改变签名",
			mutate: func(c MCPConfig) MCPConfig {
				c.Client.Sampling = MCPSamplingConfig{Enable: false, TaskName: "other", ToolSupport: true}
				return c
			},
			wantChange: false,
		},
		{
			name: "权限列表变化",
			mutate: func(c MCPConfig) MCPConfig {
				c.Servers[0].AllowUsers = []string{"qq:1"}
				return c
			},
			wantChange: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := ConfigSignature(testCase.mutate(cloneConfig(base)))
			if testCase.wantChange && mutated == baseline {
				t.Fatalf("期望签名变化但保持 %s", baseline)
			}
			if !testCase.wantChange && mutated != baseline {
				t.Fatalf("期望签名不变但得到 %s", mutated)
			}
		})
	}
}

func TestConfigSignatureIgnoresUnusedServerSettings(t *testing.T) {
	// 总开关关闭时签名与「无服务器」等价（对齐 build_mcp_server_runtime_configs）。
	disabled := MCPConfig{
		Enable:  false,
		Servers: []MCPServerItemConfig{{Name: "alpha", Enabled: true, Transport: "stdio", Command: "npx"}},
	}
	empty := MCPConfig{Enable: false}
	if ConfigSignature(disabled) != ConfigSignature(empty) {
		t.Fatalf("关闭 MCP 后签名应忽略服务器列表")
	}
	// 打开开关后签名应当不同。
	enabled := disabled
	enabled.Enable = true
	if ConfigSignature(enabled) == ConfigSignature(disabled) {
		t.Fatalf("启用 MCP 后签名应变化")
	}
}

// ---------------------------------------------------------------------------
// 热切换 / lease 退休
// ---------------------------------------------------------------------------

func TestServiceReloadSkipsWhenSignatureUnchanged(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()

	config := stdioTestConfig("alpha")

	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("首次初始化失败: %v", err)
	}
	firstHash := service.ConfigSignature()
	if firstHash == "" {
		t.Fatalf("签名不应为空")
	}
	firstSnapshot := service.StatusSnapshot()
	if firstSnapshot.ServerCount != 1 || firstSnapshot.ToolCount != 2 {
		t.Fatalf("快照异常: %+v", firstSnapshot)
	}

	// 同签名 reload 不得重启连接（连接对象必须保持同一指针）。
	service.reloadMu.Lock()
	originalManager := service.manager
	service.reloadMu.Unlock()
	if err := service.Reload(context.Background(), config); err != nil {
		t.Fatalf("第二次 reload 失败: %v", err)
	}
	if service.manager != originalManager {
		t.Fatalf("同签名 reload 不应重建管理器")
	}

	// 关闭总开关:manager 变 nil，快照清空。
	disabled := config
	disabled.Enable = false
	if err := service.Reload(context.Background(), disabled); err != nil {
		t.Fatalf("禁用 reload 失败: %v", err)
	}
	if service.StatusSnapshot().ServerCount != 0 {
		t.Fatalf("禁用后应清空服务器: %+v", service.StatusSnapshot())
	}
}

func TestServiceReloadRetiresOldManagerAfterLeaseRelease(t *testing.T) {
	var callCount int32
	var mu sync.Mutex
	restore := installInMemoryTransport(t, testServerOptions{
		onCall: func(name string, args map[string]any) {
			mu.Lock()
			callCount++
			mu.Unlock()
		},
	})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()

	config := stdioTestConfig("alpha")
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	// 持有旧管理器 lease → 触发配置切换 → 旧管理器必须进入 retired 而不是被立刻关闭。
	lease := service.Lease()
	oldManager := lease.Manager()
	if oldManager == nil {
		t.Fatalf("lease 应返回管理器")
	}

	changed := config
	changed.Servers[0].Args = []string{"changed"}
	if err := service.Reload(context.Background(), changed); err != nil {
		t.Fatalf("热切换失败: %v", err)
	}

	if _, retired := service.retiredManagers[oldManager]; !retired {
		t.Fatalf("有活跃 lease 时旧管理器应进入 retired 集合")
	}
	if state := oldManager.getStatusSnapshot(); !state.Initialized {
		t.Fatalf("退休中的管理器仍应可读状态")
	}

	// lease 释放 → 旧管理器被关闭（连接清空）。
	lease.Release()
	if _, retired := service.retiredManagers[oldManager]; retired {
		t.Fatalf("lease 归零后应从 retired 集合移除")
	}
	if oldManager.serverCount() != 0 {
		t.Fatalf("退休管理器应在引用归零后关闭连接, server_count=%d", oldManager.serverCount())
	}

	// 新管理器仍可正常调用工具。
	result := service.CallTool(context.Background(), ToolInvocation{
		ToolName:  "echo",
		Arguments: map[string]any{"text": "after-reload"},
	}, SessionContext{SessionID: "s1"})
	if !result.Success {
		t.Fatalf("热切换后调用失败: %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if callCount != 1 {
		t.Fatalf("期望 1 次远程调用, 实得 %d", callCount)
	}
}

func TestServiceReloadClosesOldManagerImmediatelyWithoutLease(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()

	config := stdioTestConfig("alpha")
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	service.mu.Lock()
	oldManager := service.manager
	service.mu.Unlock()

	changed := config
	changed.Servers[0].Command = "uvx"
	if err := service.Reload(context.Background(), changed); err != nil {
		t.Fatalf("热切换失败: %v", err)
	}
	if len(service.retiredManagers) != 0 {
		t.Fatalf("无 lease 时不应留下退休管理器")
	}
	if oldManager.serverCount() != 0 {
		t.Fatalf("旧管理器应被立即关闭")
	}
}

func TestServiceLeaseReleaseIsIdempotent(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	lease := service.Lease()
	manager := lease.Manager()
	lease.Release()
	lease.Release() // 幂等:重复释放不得把引用计数打成负数
	lease.Release()

	if manager != nil {
		if count, ok := service.activeLeases[manager]; ok && count != 0 {
			t.Fatalf("引用计数未归零: %d", count)
		}
	}
	if len(service.activeLeases) != 0 {
		t.Fatalf("活跃 lease 表应为空: %+v", service.activeLeases)
	}
}

func TestServiceStatusSnapshotShape(t *testing.T) {
	// 两个服务器必须提供不同工具名才能同时注册（重名会按 Python 语义去重）。
	restore := installInMemoryTransportFor(t, func(config ServerRuntimeConfig) testServerOptions {
		if config.Name == "beta" {
			return testServerOptions{toolName: "beta_echo"}
		}
		return testServerOptions{}
	})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()

	config := stdioTestConfig("alpha", "beta")
	config.Servers[1].Transport = "streamable_http"
	config.Servers[1].URL = "https://example.com/mcp"
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	snapshot := service.StatusSnapshot()
	if !snapshot.Initialized || !snapshot.Loaded {
		t.Fatalf("快照应标记 initialized/loaded: %+v", snapshot)
	}
	if snapshot.ServerCount != 2 {
		t.Fatalf("server_count = %d, want 2", snapshot.ServerCount)
	}
	if snapshot.ToolCount != 4 {
		t.Fatalf("tool_count = %d, want 4（两个服务器各 2 个工具）", snapshot.ToolCount)
	}
	if len(snapshot.Servers) != 2 {
		t.Fatalf("servers 长度 = %d", len(snapshot.Servers))
	}
	for _, server := range snapshot.Servers {
		if !server.Connected {
			t.Fatalf("服务器 %s 应已连接: %+v", server.Name, server)
		}
		if server.ToolCount != 2 {
			t.Fatalf("服务器 %s 工具数 = %d", server.Name, server.ToolCount)
		}
		if server.Error != "" {
			t.Fatalf("服务器 %s 不应有错误: %s", server.Name, server.Error)
		}
	}

	// 快照必须是深拷贝:修改返回值不得影响服务状态。
	snapshot.Servers[0].ToolCount = 99
	if service.StatusSnapshot().Servers[0].ToolCount == 99 {
		t.Fatalf("快照必须返回副本")
	}
}

func TestServiceStatusSnapshotReportsConnectionFailure(t *testing.T) {
	// 传输构造失败 → 服务器进入 connection_errors 且仍出现在快照里。
	restore := setTransportHook(func(config ServerRuntimeConfig) (mcp.Transport, error) {
		return nil, errTestTransportFailure
	})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	snapshot := service.StatusSnapshot()
	if snapshot.ServerCount != 0 {
		t.Fatalf("连接失败时 server_count 应为 0: %+v", snapshot)
	}
	if len(snapshot.Servers) != 1 {
		t.Fatalf("失败服务器仍应出现在快照: %+v", snapshot.Servers)
	}
	server := snapshot.Servers[0]
	if server.Connected {
		t.Fatalf("失败服务器不应标记 connected")
	}
	if !strings.Contains(server.Error, "transport boom") {
		t.Fatalf("应保留错误信息: %q", server.Error)
	}
}

func TestServiceCloseClearsState(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	snapshot := service.StatusSnapshot()
	if snapshot.Initialized || snapshot.Loaded {
		t.Fatalf("关闭后应标记未初始化: %+v", snapshot)
	}
	if service.ConfigSignature() != "" {
		t.Fatalf("关闭后签名应清空")
	}
	// 关闭后再 reload 应当恢复（对应 Python `self._closed = False`）。
	if err := service.Reload(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if service.StatusSnapshot().ServerCount != 1 {
		t.Fatalf("重新加载后应有一个服务器: %+v", service.StatusSnapshot())
	}
}

func TestServiceToolSummaryAndFeatureSummary(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{withPrompts: true, withResources: true})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	service.mu.Lock()
	manager := service.manager
	service.mu.Unlock()

	if summary := manager.getToolSummary(); !strings.Contains(summary, "alpha") || !strings.Contains(summary, "echo") {
		t.Fatalf("工具摘要异常: %q", summary)
	}
	if summary := manager.getFeatureSummary(); !strings.Contains(summary, "工具 2 / Prompt 1 / 资源 1 / 模板 0") {
		t.Fatalf("能力摘要异常: %q", summary)
	}
	if !manager.isPrompt("greet") || !manager.isResource("mem://greeting") || manager.isResource("mem://missing") {
		t.Fatalf("Prompt/Resource 路由表异常")
	}
	prompt, err := manager.getPrompt(context.Background(), "greet", map[string]string{"who": "麦麦"})
	if err != nil {
		t.Fatalf("读取 Prompt 失败: %v", err)
	}
	if len(prompt.Messages) != 1 || !strings.Contains(prompt.Messages[0].Content.Text, "你好 麦麦") {
		t.Fatalf("Prompt 内容异常: %+v", prompt)
	}
	resource, err := manager.readResource(context.Background(), "mem://greeting")
	if err != nil {
		t.Fatalf("读取资源失败: %v", err)
	}
	if len(resource.Contents) != 1 || resource.Contents[0].Text != "你好，世界" {
		t.Fatalf("资源内容异常: %+v", resource)
	}
	if _, err := manager.getPrompt(context.Background(), "missing", nil); err == nil {
		t.Fatalf("未知 Prompt 应报错")
	}
	if _, err := manager.readResource(context.Background(), "mem://missing"); err == nil {
		t.Fatalf("未知资源应报错")
	}
}

func TestManagerRejectsBuiltinAndDuplicateToolNames(t *testing.T) {
	// 两个服务器提供同名工具:先注册者胜出,后者跳过。
	restore := setTransportHook(func(config ServerRuntimeConfig) (mcp.Transport, error) {
		switch config.Name {
		case "first":
			_, transport, _ := newTestServer(t, testServerOptions{extraTools: []string{"reply", "dupe"}})
			return transport, nil
		default:
			_, transport, _ := newTestServer(t, testServerOptions{extraTools: []string{"dupe"}})
			return transport, nil
		}
	})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("first", "second")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	names := map[string]int{}
	for _, spec := range service.ListTools() {
		names[spec.Name]++
	}
	if names["reply"] != 0 {
		t.Fatalf("内置工具名 reply 必须被跳过")
	}
	if names["dupe"] != 1 {
		t.Fatalf("重复工具名只应注册一次, 实得 %d", names["dupe"])
	}
	// echo(每台各 1) + dupe(首台胜出) + boom(每台各 1)
	if got := len(service.ListTools()); got != 3 {
		t.Fatalf("工具总数 = %d, want 3（reply 与第二台的 dupe 已跳过）", got)
	}
	for _, spec := range service.ListTools() {
		if spec.Name == "dupe" {
			if spec.ServerName != "first" {
				t.Fatalf("重名工具应归属先注册的服务器, got %s", spec.ServerName)
			}
		}
	}
}

func TestConfigValidateMirrorsPython(t *testing.T) {
	cases := []struct {
		name    string
		config  MCPConfig
		wantErr string
	}{
		{
			name: "空名字",
			config: MCPConfig{Enable: true, Servers: []MCPServerItemConfig{
				{Name: "  ", Enabled: true, Transport: "stdio", Command: "npx"},
			}},
			wantErr: "name 不能为空",
		},
		{
			name: "重名",
			config: MCPConfig{Enable: true, Servers: []MCPServerItemConfig{
				{Name: "a", Enabled: true, Transport: "stdio", Command: "npx"},
				{Name: "a", Enabled: true, Transport: "stdio", Command: "npx"},
			}},
			wantErr: "不能重复",
		},
		{
			name: "stdio 缺 command",
			config: MCPConfig{Enable: true, Servers: []MCPServerItemConfig{
				{Name: "a", Enabled: true, Transport: "stdio"},
			}},
			wantErr: "必须填写 command",
		},
		{
			name: "http 缺 url",
			config: MCPConfig{Enable: true, Servers: []MCPServerItemConfig{
				{Name: "a", Enabled: true, Transport: "streamable_http"},
			}},
			wantErr: "必须填写 url",
		},
		{
			name: "bearer 缺 token",
			config: MCPConfig{Enable: true, Servers: []MCPServerItemConfig{
				{Name: "a", Enabled: true, Transport: "sse", URL: "https://x/sse",
					Authorization: MCPAuthorizationConfig{Mode: "bearer"}},
			}},
			wantErr: "必须填写 bearer_token",
		},
		{
			name: "停用服务器不校验",
			config: MCPConfig{Enable: true, Servers: []MCPServerItemConfig{
				{Name: "", Enabled: false, Transport: "stdio"},
			}},
			wantErr: "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.config.Validate()
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("不应报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应当报错")
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("错误文本 %q 不含 %q", err.Error(), testCase.wantErr)
			}
		})
	}
}

func TestDefaultMCPConfigMatchesPython(t *testing.T) {
	config := DefaultMCPConfig()
	if !config.Enable {
		t.Fatalf("enable 默认应为 true")
	}
	if config.Client.ClientName != "MaiBot" || config.Client.ClientVersion != "1.0.0" {
		t.Fatalf("客户端默认值异常: %+v", config.Client)
	}
	if config.Client.Sampling.TaskName != "planner" || config.Client.Sampling.Enable {
		t.Fatalf("sampling 默认值异常: %+v", config.Client.Sampling)
	}
	if !config.Client.Elicitation.AllowForm || config.Client.Elicitation.AllowURL {
		t.Fatalf("elicitation 默认值异常: %+v", config.Client.Elicitation)
	}
	if len(config.Servers) != 0 {
		t.Fatalf("servers 默认应为空")
	}
	// toml tag 必须与 official_configs 字段名一致(internal/config 直接复用本类型)。
	encoded, err := json.Marshal(ServerRuntimeConfig{})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, key := range []string{
		"name", "transport", "command", "args", "env", "url", "headers",
		"http_timeout_seconds", "read_timeout_seconds", "authorization",
		"allow_users", "allow_chats", "permission",
	} {
		if !strings.Contains(string(encoded), `"`+key+`"`) {
			t.Fatalf("运行时配置缺少字段 %s: %s", key, encoded)
		}
	}
}

func TestBuildServerRuntimeConfigNormalizes(t *testing.T) {
	config := BuildServerRuntimeConfig(MCPServerItemConfig{
		Name: "  spaced  ", Enabled: true, Transport: "STREAMABLE_HTTP",
		Command: " cmd ", Args: []string{"a", "b"}, Env: map[string]string{"K": "V"},
		URL: " https://host/mcp ", Headers: map[string]string{"H": "1"},
		HTTPTimeoutSeconds: 0, ReadTimeoutSeconds: -1,
		Authorization: MCPAuthorizationConfig{Mode: "Bearer", BearerToken: " tok "},
		AllowUsers:    []string{" qq:1 ", ""},
		AllowChats:    []string{"chat"},
		Permission:    []string{" qq:9 "},
	})
	if config.Name != "spaced" || config.Transport != TransportStreamableHTTP {
		t.Fatalf("归一化异常: %+v", config)
	}
	if config.Command != "cmd" || config.URL != "https://host/mcp" {
		t.Fatalf("空白未清理: %+v", config)
	}
	if config.HTTPTimeoutSeconds != DefaultHTTPTimeoutSeconds || config.ReadTimeoutSeconds != DefaultReadTimeoutSeconds {
		t.Fatalf("默认超时未填充: %+v", config)
	}
	if config.Authorization.Mode != "bearer" || config.Authorization.BearerToken != "tok" {
		t.Fatalf("认证归一化异常: %+v", config.Authorization)
	}
	if got := config.BuildHTTPHeaders(); got["Authorization"] != "Bearer tok" || got["H"] != "1" {
		t.Fatalf("请求头构建异常: %+v", got)
	}
	if len(config.AllowUsers) != 2 || config.AllowUsers[0] != "qq:1" {
		t.Fatalf("allow_users 归一化异常: %+v", config.AllowUsers)
	}
	// 传输完整性判定(mirrors transport_type)。
	if got := (ServerRuntimeConfig{Transport: TransportStdio}).TransportType(); got != TransportUnknown {
		t.Fatalf("缺 command 的 stdio 应为 unknown, got %s", got)
	}
	if got := (ServerRuntimeConfig{Transport: TransportSSE, URL: "u"}).TransportType(); got != TransportSSE {
		t.Fatalf("sse 应可用, got %s", got)
	}
}

func TestSamplingCapabilityNotAdvertisedWithoutCallback(t *testing.T) {
	// 未注入 sampling 回调时不声明该能力（对齐 connection.py 的告警分支）。
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()

	config := stdioTestConfig("alpha")
	config.Client.Sampling = MCPSamplingConfig{Enable: true, TaskName: "planner"}
	config.Client.Elicitation = MCPElicitationConfig{Enable: true, AllowForm: true}
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	connection := service.manager.connections["alpha"]
	options := connection.clientOptions()
	if options.CreateMessageWithToolsHandler != nil {
		t.Fatalf("没有回调时不应装配 sampling handler")
	}
	if options.ElicitationHandler != nil {
		t.Fatalf("没有回调时不应装配 elicitation handler")
	}
	if options.Capabilities.Sampling != nil {
		t.Fatalf("没有回调时不应声明 sampling 能力")
	}
}

func TestConnectionAdvertisesRoots(t *testing.T) {
	var got string
	restore := setTransportHook(func(config ServerRuntimeConfig) (mcp.Transport, error) {
		server, transport, _ := newTestServer(t, testServerOptions{
			roots: true,
			onCall: func(name string, args map[string]any) {
				got = name
			},
		})
		_ = server
		return transport, nil
	})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()

	config := stdioTestConfig("alpha")
	config.Client.Roots = MCPRootsConfig{
		Enable: true,
		Items:  []MCPRootItemConfig{{Enabled: true, URI: "file:///work", Name: "work"}},
	}
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	result := service.CallTool(context.Background(), ToolInvocation{
		ToolName:  "echo",
		Arguments: map[string]any{"text": "x"},
	}, SessionContext{})
	if !result.Success {
		t.Fatalf("调用失败: %+v", result)
	}
	if got != "echo" {
		t.Fatalf("工具未被调用: %q", got)
	}
	if !strings.Contains(result.Content, "roots=file:///work") {
		t.Fatalf("roots 未透传给服务端: %q", result.Content)
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

var errTestTransportFailure = &testTransportError{}

type testTransportError struct{}

func (e *testTransportError) Error() string { return "transport boom" }

func cloneConfig(config MCPConfig) MCPConfig {
	cloned := config
	cloned.Servers = append([]MCPServerItemConfig{}, config.Servers...)
	for index, server := range cloned.Servers {
		server.Args = append([]string{}, config.Servers[index].Args...)
		server.Env = map[string]string{}
		for key, value := range config.Servers[index].Env {
			server.Env[key] = value
		}
		server.Headers = map[string]string{}
		for key, value := range config.Servers[index].Headers {
			server.Headers[key] = value
		}
		server.AllowUsers = append([]string{}, config.Servers[index].AllowUsers...)
		server.AllowChats = append([]string{}, config.Servers[index].AllowChats...)
		server.Permission = append([]string{}, config.Servers[index].Permission...)
		cloned.Servers[index] = server
	}
	cloned.Client.Roots.Items = append([]MCPRootItemConfig{}, config.Client.Roots.Items...)
	return cloned
}

func stdioTestConfig(names ...string) MCPConfig {
	servers := make([]MCPServerItemConfig, 0, len(names))
	for _, name := range names {
		servers = append(servers, MCPServerItemConfig{
			Name:      name,
			Enabled:   true,
			Transport: "stdio",
			Command:   "noop",
		})
	}
	return MCPConfig{
		Enable:  true,
		Client:  MCPClientConfig{ClientName: "MaiBot", ClientVersion: "1.0.0"},
		Servers: servers,
	}
}

func TestWaitForHelper(t *testing.T) {
	done := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(done)
	}()
	waitFor(t, time.Second, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, "channel closed")
}

func TestServiceRebuildsConnectionsOnSignatureChange(t *testing.T) {
	// 真实 HTTP 服务端:签名变化必须重建连接（新 manager、新会话）。
	httpServer := newHTTPTestServer(t, testServerOptions{})
	defer httpServer.close()

	service := NewService()
	defer func() { _ = service.Close() }()

	config := MCPConfig{
		Enable: true,
		Client: MCPClientConfig{ClientName: "MaiBot", ClientVersion: "1.0.0"},
		Servers: []MCPServerItemConfig{{
			Name: "http", Enabled: true, Transport: "streamable_http",
			URL: httpServer.streamableURL, HTTPTimeoutSeconds: 10, ReadTimeoutSeconds: 30,
		}},
	}
	if err := service.EnsureInitialized(context.Background(), config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	service.mu.Lock()
	first := service.manager
	firstSession := first.connections["http"].sessionRef()
	service.mu.Unlock()
	if firstSession == nil {
		t.Fatalf("会话未建立")
	}

	// 同签名:连接保持。
	if err := service.Reload(context.Background(), config); err != nil {
		t.Fatalf("重复加载失败: %v", err)
	}
	service.mu.Lock()
	same := service.manager
	service.mu.Unlock()
	if same != first {
		t.Fatalf("同签名不应重建")
	}
	if first.connections["http"].sessionRef() != firstSession {
		t.Fatalf("同签名不应重建会话")
	}

	// 签名变化:重建连接（新 manager 与新会话），旧 manager 立即退休关闭。
	changed := config
	changed.Servers[0].Headers = map[string]string{"X-Round": "2"}
	if err := service.Reload(context.Background(), changed); err != nil {
		t.Fatalf("热切换失败: %v", err)
	}
	service.mu.Lock()
	second := service.manager
	retired := len(service.retiredManagers)
	service.mu.Unlock()
	if second == first {
		t.Fatalf("签名变化应重建 manager")
	}
	if retired != 0 {
		t.Fatalf("无 lease 时不应留下退休管理器")
	}
	if first.serverCount() != 0 {
		t.Fatalf("旧连接应被关闭")
	}
	if second.connections["http"].sessionRef() == nil {
		t.Fatalf("新连接应建立会话")
	}
	// 新连接仍可调用。
	result := service.CallTool(context.Background(), ToolInvocation{
		ToolName:  "echo",
		Arguments: map[string]any{"text": "round2"},
	}, SessionContext{})
	if !result.Success {
		t.Fatalf("重建后调用失败: %+v", result)
	}
}

func TestServiceConcurrentReloadAndCall(t *testing.T) {
	// 并发压力:多次热切换与并发调用不得死锁/竞态（配合 -race 更有价值）。
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	var wg sync.WaitGroup
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			for round := 0; round < 10; round++ {
				result := service.CallTool(context.Background(), ToolInvocation{
					ToolName:  "echo",
					Arguments: map[string]any{"text": "concurrent"},
				}, SessionContext{SessionID: "c"})
				if !result.Success {
					t.Errorf("并发调用失败: %+v", result)
					return
				}
			}
		}(index)
	}
	for round := 0; round < 10; round++ {
		changed := stdioTestConfig("alpha")
		changed.Servers[0].Args = []string{string(rune('a' + round))}
		if err := service.Reload(context.Background(), changed); err != nil {
			t.Fatalf("并发热切换失败: %v", err)
		}
	}
	wg.Wait()
	// 最终状态一致。
	if snapshot := service.StatusSnapshot(); snapshot.ServerCount != 1 || snapshot.ToolCount != 2 {
		t.Fatalf("最终快照异常: %+v", snapshot)
	}
	if len(service.activeLeases) != 0 || len(service.retiredManagers) != 0 {
		t.Fatalf("并发后引用计数未归零: leases=%d retired=%d",
			len(service.activeLeases), len(service.retiredManagers))
	}
}
