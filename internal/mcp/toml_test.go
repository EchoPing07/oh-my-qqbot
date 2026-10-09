package mcp

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// 本文件保证 MCPConfig 的 toml tag 与 official_configs.py 的字段名一一对应
// （internal/config 直接复用本包类型，配置模板必须能被同一份 toml 反序列化）。

func TestMCPConfigTomlTagsMatchPythonFieldNames(t *testing.T) {
	expected := map[reflect.Type]map[string]string{
		reflect.TypeOf(MCPConfig{}): {
			"Enable": "enable", "Client": "client", "Servers": "servers",
		},
		reflect.TypeOf(MCPClientConfig{}): {
			"ClientName": "client_name", "ClientVersion": "client_version",
			"Roots": "roots", "Sampling": "sampling", "Elicitation": "elicitation",
		},
		reflect.TypeOf(MCPRootsConfig{}): {"Enable": "enable", "Items": "items"},
		reflect.TypeOf(MCPRootItemConfig{}): {
			"Enabled": "enabled", "URI": "uri", "Name": "name",
		},
		reflect.TypeOf(MCPSamplingConfig{}): {
			"Enable": "enable", "TaskName": "task_name",
			"IncludeContextSupport": "include_context_support", "ToolSupport": "tool_support",
		},
		reflect.TypeOf(MCPElicitationConfig{}): {
			"Enable": "enable", "AllowForm": "allow_form", "AllowURL": "allow_url",
		},
		reflect.TypeOf(MCPServerItemConfig{}): {
			"Name": "name", "Enabled": "enabled", "Transport": "transport",
			"Command": "command", "Args": "args", "Env": "env", "URL": "url",
			"Headers": "headers", "HTTPTimeoutSeconds": "http_timeout_seconds",
			"ReadTimeoutSeconds": "read_timeout_seconds", "Authorization": "authorization",
			"AllowUsers": "allow_users", "AllowChats": "allow_chats", "Permission": "permission",
		},
		reflect.TypeOf(MCPAuthorizationConfig{}): {
			"Mode": "mode", "BearerToken": "bearer_token",
		},
	}

	for structType, fields := range expected {
		for index := 0; index < structType.NumField(); index++ {
			field := structType.Field(index)
			want, ok := fields[field.Name]
			if !ok {
				t.Fatalf("%s.%s 未在期望表中（新增字段需同步 official_configs 语义）",
					structType.Name(), field.Name)
			}
			if got := field.Tag.Get("toml"); got != want {
				t.Fatalf("%s.%s toml tag = %q, want %q", structType.Name(), field.Name, got, want)
			}
		}
		if len(fields) != structType.NumField() {
			t.Fatalf("%s 字段数 %d != 期望表 %d", structType.Name(), structType.NumField(), len(fields))
		}
	}
}

func TestMCPConfigUnmarshalsPythonStyleTOML(t *testing.T) {
	// 取自官方配置模板形态（对齐 config/official_configs.MCPConfig 的 toml 表达）。
	document := `
enable = true

[client]
client_name = "MaiBot"
client_version = "1.0.0"

[client.roots]
enable = true

[[client.roots.items]]
enabled = true
uri = "file:///work"
name = "工作目录"

[client.sampling]
enable = true
task_name = "planner"
include_context_support = false
tool_support = true

[client.elicitation]
enable = false
allow_form = true
allow_url = false

[[servers]]
name = "local"
enabled = true
transport = "stdio"
command = "npx"
args = ["-y", "some-mcp"]
env = { TOKEN = "abc" }
http_timeout_seconds = 15.0
read_timeout_seconds = 120.0
allow_users = ["qq:123456789"]
allow_chats = ["session-group"]
permission = ["qq:999"]

[servers.authorization]
mode = "none"
bearer_token = ""

[[servers]]
name = "remote"
enabled = true
transport = "streamable_http"
url = "https://example.com/mcp"
headers = { "X-Token" = "t" }

[servers.authorization]
mode = "bearer"
bearer_token = "secret"
`
	var config MCPConfig
	if err := toml.Unmarshal([]byte(document), &config); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if !config.Enable || len(config.Servers) != 2 {
		t.Fatalf("顶层字段异常: %+v", config)
	}
	if config.Client.ClientName != "MaiBot" || !config.Client.Roots.Enable {
		t.Fatalf("client 字段异常: %+v", config.Client)
	}
	if len(config.Client.Roots.Items) != 1 || config.Client.Roots.Items[0].URI != "file:///work" {
		t.Fatalf("roots.items 异常: %+v", config.Client.Roots.Items)
	}
	if !config.Client.Sampling.Enable || config.Client.Sampling.TaskName != "planner" || !config.Client.Sampling.ToolSupport {
		t.Fatalf("sampling 异常: %+v", config.Client.Sampling)
	}
	local := config.Servers[0]
	if local.Command != "npx" || len(local.Args) != 2 || local.Env["TOKEN"] != "abc" {
		t.Fatalf("stdio 服务器异常: %+v", local)
	}
	if local.HTTPTimeoutSeconds != 15 || local.ReadTimeoutSeconds != 120 {
		t.Fatalf("超时字段异常: %+v", local)
	}
	if len(local.AllowUsers) != 1 || len(local.AllowChats) != 1 || len(local.Permission) != 1 {
		t.Fatalf("权限字段异常: %+v", local)
	}
	remote := config.Servers[1]
	if remote.URL != "https://example.com/mcp" || remote.Headers["X-Token"] != "t" {
		t.Fatalf("http 服务器异常: %+v", remote)
	}
	if remote.Authorization.Mode != "bearer" || remote.Authorization.BearerToken != "secret" {
		t.Fatalf("authorization 异常: %+v", remote.Authorization)
	}

	// 配置必须通过校验，并能转换为运行时配置。
	if err := config.Validate(); err != nil {
		t.Fatalf("配置校验失败: %v", err)
	}
	runtime := BuildServerRuntimeConfigs(config)
	if len(runtime) != 2 {
		t.Fatalf("运行时配置数量异常: %d", len(runtime))
	}
	if headers := runtime[1].BuildHTTPHeaders(); headers["Authorization"] != "Bearer secret" {
		t.Fatalf("bearer 头未生成: %+v", headers)
	}
	client := BuildClientRuntimeConfig(config)
	if !client.EnableRoots || len(client.Roots) != 1 || !client.EnableSampling || client.SamplingTaskName != "planner" {
		t.Fatalf("客户端运行时配置异常: %+v", client)
	}

	// 往返序列化保持稳定（WebUI 配置写回场景）。
	encoded, err := toml.Marshal(config)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(encoded), "client_name = 'MaiBot'") &&
		!strings.Contains(string(encoded), `client_name = "MaiBot"`) {
		t.Fatalf("序列化缺少 client_name:\n%s", encoded)
	}
	var roundTrip MCPConfig
	if err := toml.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatalf("往返反序列化失败: %v", err)
	}
	if ConfigSignature(roundTrip) != ConfigSignature(config) {
		t.Fatalf("往返后签名变化（toml 序列化丢字段）")
	}
}
