package assemble

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	chatcontext "oh-my-qqbot/internal/chat/context"
	"oh-my-qqbot/internal/chat/heartflow"
	"oh-my-qqbot/internal/chat/manager"
	"oh-my-qqbot/internal/chat/runtime"
	"oh-my-qqbot/internal/chat/tools"
	"oh-my-qqbot/internal/chat/trigger"
	"oh-my-qqbot/internal/config"
	"oh-my-qqbot/internal/database"
	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/message"
	"oh-my-qqbot/internal/monitor"
)

// newPipelineApp 构造一个只包含消息管线所需组件的 App（config/database/
// ChatManager/Heartflow），不含 LLM 内核与渠道连接。
func newPipelineApp(t *testing.T) *App {
	t.Helper()
	root := t.TempDir()
	db, err := database.Open(filepath.Join(root, "data", "mai.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// 提前生成配置模板，避免 Manager.Initialize 与模板写入竞争。
	if _, err := config.EnsureTemplateFiles(filepath.Join(root, "config")); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewManager(filepath.Join(root, "config"))
	if err := cfg.Initialize(); err != nil {
		t.Fatal(err)
	}

	chatManager := manager.NewChatManager(db)
	if err := chatManager.Initialize(); err != nil {
		t.Fatal(err)
	}
	app := &App{
		log:         slog.New(slog.DiscardHandler),
		Cfg:         cfg,
		DB:          db,
		Bus:         monitor.NewBus(),
		ChatManager: chatManager,
		Heartflow: newHeartflowManager(t, chatManager, func(sessionID string, isGroupSession bool) (*runtime.Runtime, error) {
			return runtime.NewRuntime(runtime.Options{SessionID: sessionID, IsGroupSession: isGroupSession}), nil
		}),
	}
	return app
}

// newHeartflowManager mirrors SetupPhase2 的 Heartflow 装配（NewRuntime 可注入）。
func newHeartflowManager(t *testing.T, chatManager *manager.ChatManager, newRuntime func(sessionID string, isGroupSession bool) (*runtime.Runtime, error)) *heartflow.HeartflowManager {
	t.Helper()
	return heartflow.NewHeartflowManager(heartflow.Options{
		ChatManager: chatManager,
		NewRuntime:  newRuntime,
		Log:         func(string, ...any) {},
	})
}

// makeQQGroupMessage 构造一条 QQ 群消息（对齐渠道层转换产物：SessionID 已由
// EnsureSessionID 重算，AdditionalConfig 带 account_id）。
func makeQQGroupMessage(userID, groupID string) *message.SessionMessage {
	msg := &message.SessionMessage{
		MessageID: "msg_" + userID,
		Timestamp: time.Unix(1700000000, 0),
		Platform:  "qq",
		MessageInfo: message.MessageInfo{
			UserInfo: message.NewUserInfo(userID, "昵称_"+userID, ""),
			GroupInfo: &message.GroupInfo{
				GroupID:   groupID,
				GroupName: "测试群",
			},
			AdditionalConfig: map[string]any{"account_id": "acc1"},
		},
		RawMessage: message.NewMessageSequence(message.NewTextComponent("你好")),
	}
	if err := msg.EnsureSessionID(); err != nil {
		panic(err) // helper 保证 userID/groupID 至少有一个，不会走到这里
	}
	return msg
}

// TestHandleMessageCreatesSessionAndHeartflow 回归 P0：消息进来必须先确保
// session 存在，否则 GetOrCreateHeartflowChat 报「未找到 session 对应的聊天流」，
// 心流 runtime 永远建不出来（docs/DEBUG_REPORT_2026-10-10_link_failures.md §3）。
func TestHandleMessageCreatesSessionAndHeartflow(t *testing.T) {
	app := newPipelineApp(t)
	msg := makeQQGroupMessage("u1", "g1")

	if err := app.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// session 必须已创建（内存 + DB）。
	session := app.ChatManager.GetSessionBySessionID(msg.SessionID)
	if session == nil {
		t.Fatal("消息处理后 session 仍不存在：GetOrCreateSession 未被调用")
	}
	if !session.IsGroupSession() || session.GroupID == nil || *session.GroupID != "g1" {
		t.Fatalf("session 路由字段错误: %+v", session)
	}
	if session.AccountID == nil || *session.AccountID != "acc1" {
		t.Fatalf("session.AccountID 应为 acc1: %+v", session)
	}

	// 心流 runtime 必须能由同一 sessionID 建出。
	hf, err := app.Heartflow.GetOrCreateHeartflowChat(msg.SessionID)
	if err != nil {
		t.Fatalf("心流会话创建失败（回归 P0）: %v", err)
	}
	if hf == nil {
		t.Fatal("心流 runtime 不应为空")
	}

	// 同一 sessionID 重复投递应幂等。
	if err := app.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("重复投递应幂等: %v", err)
	}
	if app.ChatManager.SessionCount() != 1 {
		t.Fatalf("重复投递不应新建会话: %d", app.ChatManager.SessionCount())
	}
}

// TestHandleMessagePrivateChatCreatesSession 私聊消息同样走通会话创建。
func TestHandleMessagePrivateChatCreatesSession(t *testing.T) {
	app := newPipelineApp(t)
	msg := makeQQGroupMessage("u9", "")
	msg.MessageInfo.GroupInfo = nil // 私聊

	if err := app.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	session := app.ChatManager.GetSessionBySessionID(msg.SessionID)
	if session == nil {
		t.Fatal("私聊消息处理后 session 不存在")
	}
	if session.IsGroupSession() {
		t.Fatalf("私聊会话被误判为群聊: %+v", session)
	}
}

// TestHandleMessageWithoutSessionIdentityIsRejected 确认缺身份的消息在会话
// 创建处被拒绝（返回 nil），不再流入心流层产生「心流会话创建失败」。
func TestHandleMessageWithoutSessionIdentityIsRejected(t *testing.T) {
	app := newPipelineApp(t)
	msg := makeQQGroupMessage("u2", "g2")
	msg.Platform = ""
	msg.MessageInfo.UserInfo.UserID = ""

	if err := app.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("缺身份消息应返回 nil 而非错误: %v", err)
	}
	if s := app.ChatManager.GetExistingSessionBySessionID(msg.SessionID); s != nil {
		t.Fatal("缺身份消息不应创建会话")
	}
}

// TestRegisterMessageAloneCannotSatisfyHeartflow 验证历史缺陷形态：
// 只调 RegisterMessage（修复前的行为）时，心流创建必然失败 ——
// 保证回归测试覆盖的是真实故障路径。
func TestRegisterMessageAloneCannotSatisfyHeartflow(t *testing.T) {
	app := newPipelineApp(t)
	msg := makeQQGroupMessage("u3", "g3")

	if _, err := app.ChatManager.StoreMessageToDB(msg); err != nil {
		t.Fatal(err)
	}
	if err := app.ChatManager.RegisterMessage(msg); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Heartflow.GetOrCreateHeartflowChat(msg.SessionID); err == nil {
		t.Fatal("只 RegisterMessage 不建会话时，心流创建应失败（历史缺陷）")
	}
}

// TestMessageBanFilterDropsBeforePersist 接线回归：message_receive.ban_words /
// ban_msgs_regex 必须在登记前过滤（命中消息不入库、不建会话）；坏正则只跳过
// 不 panic；无纯文本的富媒体消息不受影响；私聊同样生效。
func TestMessageBanFilterDropsBeforePersist(t *testing.T) {
	app := newPipelineApp(t)
	global := app.Cfg.GetGlobalConfig()
	global.MessageReceive.BanWords = []string{"违禁词", ""}
	global.MessageReceive.BanMsgsRegex = []string{"^广告.*$", "[[坏正则"}

	// ban_words 包含匹配 → 丢弃（群聊）。
	banned := makeQQGroupMessage("u-ban", "g-ban")
	banned.ProcessedPlainText = message.StringPtr("这条含违禁词的内容应被丢弃")
	if err := app.handleMessage(context.Background(), banned); err != nil {
		t.Fatalf("handleMessage(banned): %v", err)
	}
	if s := app.ChatManager.GetExistingSessionBySessionID(banned.SessionID); s != nil {
		t.Fatal("命中 ban_words 的消息不应创建会话（应在登记前丢弃）")
	}

	// ban_msgs_regex 正则匹配 → 丢弃（私聊同样生效）。
	bannedRegex := makeQQGroupMessage("u-ban2", "")
	bannedRegex.MessageInfo.GroupInfo = nil
	bannedRegex.ProcessedPlainText = message.StringPtr("广告位招租")
	if err := app.handleMessage(context.Background(), bannedRegex); err != nil {
		t.Fatalf("handleMessage(bannedRegex): %v", err)
	}
	if s := app.ChatManager.GetExistingSessionBySessionID(bannedRegex.SessionID); s != nil {
		t.Fatal("命中 ban_msgs_regex 的消息不应创建会话（私聊也应生效）")
	}

	// 坏正则只跳过不 panic；未命中的正常消息放行。
	okMsg := makeQQGroupMessage("u-ok", "g-ok")
	okMsg.ProcessedPlainText = message.StringPtr("正常聊天内容")
	if err := app.handleMessage(context.Background(), okMsg); err != nil {
		t.Fatalf("handleMessage(ok): %v", err)
	}
	if s := app.ChatManager.GetExistingSessionBySessionID(okMsg.SessionID); s == nil {
		t.Fatal("未命中过滤规则的消息应正常建会话")
	}

	// 纯图片消息无纯文本 → 不受 ban 过滤影响。
	picMsg := makeQQGroupMessage("u-pic", "g-pic")
	picMsg.RawMessage = message.NewMessageSequence(message.NewImageComponent([]byte("img"), "pic.png"))
	if err := app.handleMessage(context.Background(), picMsg); err != nil {
		t.Fatalf("handleMessage(pic): %v", err)
	}
	if s := app.ChatManager.GetExistingSessionBySessionID(picMsg.SessionID); s == nil {
		t.Fatal("无纯文本的富媒体消息不应被 ban 过滤丢弃")
	}
}

// TestMessageBannedHitPatterns 直接覆盖 messageBanned 的命中语义。
func TestMessageBannedHitPatterns(t *testing.T) {
	app := newPipelineApp(t)
	global := app.Cfg.GetGlobalConfig()
	global.MessageReceive.BanWords = []string{"敏感"}
	global.MessageReceive.BanMsgsRegex = []string{"\\d{4}-\\d{2}-\\d{2}"}

	if hit := app.messageBanned("含敏感词"); hit != "敏感" {
		t.Fatalf("应命中 ban_words, got %q", hit)
	}
	if hit := app.messageBanned("日期 2026-10-11 在正文里"); hit != `\d{4}-\d{2}-\d{2}` {
		t.Fatalf("应命中 ban_msgs_regex, got %q", hit)
	}
	if hit := app.messageBanned("正常内容"); hit != "" {
		t.Fatalf("未命中应返回空, got %q", hit)
	}
	if hit := app.messageBanned(""); hit != "" {
		t.Fatalf("空文本不过滤, got %q", hit)
	}
}

// TestResolveChannelCredentials 校验 QQ 渠道凭据的优先级：
// bot_config 的 [bot] app_id/app_secret 优先，缺省才回退环境变量。
func TestResolveChannelCredentials(t *testing.T) {
	app := newPipelineApp(t)

	// 1) 配置未填写 → 回退环境变量
	t.Setenv("QQ_APP_ID", "env-id")
	t.Setenv("QQ_APP_SECRET", "env-secret")
	if got, want := joinCreds(app.resolveChannelCredentials()), [2]string{"env-id", "env-secret"}; got != want {
		t.Fatalf("配置为空时应回退环境变量, got %v want %v", got, want)
	}

	// 2) 配置已填写 → 优先配置（环境变量不再生效）
	setBotCredentials(t, app, "cfg-id", "cfg-secret")
	if got, want := joinCreds(app.resolveChannelCredentials()), [2]string{"cfg-id", "cfg-secret"}; got != want {
		t.Fatalf("配置优先于环境变量, got %v want %v", got, want)
	}

	// 3) 只填了 app_id → secret 仍回退环境变量
	setBotCredentials(t, app, "cfg-id", "")
	if got, want := joinCreds(app.resolveChannelCredentials()), [2]string{"cfg-id", "env-secret"}; got != want {
		t.Fatalf("逐字段回退, got %v want %v", got, want)
	}

	// 4) 带空白的配置值需被裁剪
	setBotCredentials(t, app, "  cfg-id  ", " cfg-secret ")
	if got, want := joinCreds(app.resolveChannelCredentials()), [2]string{"cfg-id", "cfg-secret"}; got != want {
		t.Fatalf("空白应被裁剪, got %v want %v", got, want)
	}
}

// setBotCredentials 把凭据写进内存中的 bot 配置（测试用，不落盘）。
func setBotCredentials(t *testing.T, app *App, id, secret string) {
	t.Helper()
	cfg := app.Cfg.GetGlobalConfig()
	cfg.Bot.AppID = id
	cfg.Bot.AppSecret = secret
}

// joinCreds 把 resolveChannelCredentials 的双返回值打包成可比较的数组。
func joinCreds(id, secret string) [2]string { return [2]string{id, secret} }

// 回归 P0（根因 1/2）：共享注册表的 builtin handler 必须全部装配；未注册
// 会话的工具调用应失败回执而不是 panic（nil handler 与 nil runtime 两条
// 历史崩溃路径）。
func TestToolRegistryHandlersWiredAndGraceful(t *testing.T) {
	app := newPipelineApp(t)
	registry := app.Registry()
	if registry == nil {
		t.Fatal("共享工具注册表不应为空")
	}
	for _, name := range registry.Names() {
		if registry.HandlerOf(name) == nil {
			t.Fatalf("工具 %s handler 未装配", name)
		}
	}
	// 未注册会话：wait 依赖解析为 nil → 失败回执。
	result := registry.Invoke(context.Background(), app.ToolCtx("unknown_session"), tools.ToolInvocation{
		ToolName:  "wait",
		CallID:    "call_1",
		Arguments: map[string]any{"seconds": float64(1)},
	})
	if result.Success {
		t.Fatalf("未注册会话的 wait 应失败回执: %+v", result)
	}
}

// 回归 P0（根因 2）：已注册会话的 reply 依赖必须完整装配
// （runtime/sender/generator/postprocessor 任一为 nil 都会在执行期 panic）。
func TestReplyDependenciesWiredForSession(t *testing.T) {
	app := newPipelineApp(t)
	msg := makeQQGroupMessage("u7", "g7")
	if err := app.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	hf, err := app.Heartflow.GetOrCreateHeartflowChat(msg.SessionID)
	if err != nil {
		t.Fatalf("心流会话创建失败: %v", err)
	}

	deps := app.replyDependencies(msg.SessionID)
	if deps == nil {
		t.Fatal("已注册会话的 reply 依赖不应为 nil")
	}
	if deps.Runtime == nil || deps.Sender == nil || deps.Generator == nil || deps.PostProcessor == nil {
		t.Fatalf("reply 依赖不完整: runtime=%v sender=%v generator=%v postprocessor=%v",
			deps.Runtime != nil, deps.Sender != nil, deps.Generator != nil, deps.PostProcessor != nil)
	}
	if deps.SegmentDelay == nil {
		t.Fatal("SegmentDelay 应已装配")
	}
	_ = hf
}

// 回归（修复 5）：ExecuteToolCalls 必须把非空 LogicalTurnID 传给 wait 工具，
// 否则进入等待态会被 runtime 拒绝（wait 永远失败）。
func TestExecuteToolCallsCarriesLogicalTurnID(t *testing.T) {
	app := newPipelineApp(t)
	msg := makeQQGroupMessage("u8", "g8")
	if err := app.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	hf, err := app.Heartflow.GetOrCreateHeartflowChat(msg.SessionID)
	if err != nil {
		t.Fatalf("心流会话创建失败: %v", err)
	}

	bridge := app.Planner().ForSession(msg.SessionID)
	results, err := bridge.ExecuteToolCalls(context.Background(), []runtime.PlannerToolCall{{
		CallID:        "call_1",
		FuncName:      "wait",
		LogicalTurnID: "turn_1",
		Args:          map[string]any{"seconds": float64(1)},
	}}, "")
	if err != nil {
		t.Fatalf("ExecuteToolCalls: %v", err)
	}
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("wait 应成功执行: %+v err=%v", results, err)
	}
	if hf.AgentState() != trigger.StateWait {
		t.Fatalf("wait 应使 runtime 进入等待态: %s", hf.AgentState())
	}
}

// MCP 合并路径：SetToolRegistry 替换共享注册表后，planner 会话视图应立即
// 使用新注册表（此前合并结果被丢弃，MCP 工具从未接入）。
func TestSetToolRegistrySwapsSharedRegistry(t *testing.T) {
	app := newPipelineApp(t)
	original := app.Registry()
	if original == nil {
		t.Fatal("共享工具注册表不应为空")
	}

	merged := tools.NewToolRegistry(append(
		tools.BuiltinEntries(app.ToolRegistryFlags(), app.builtinToolDeps()),
		tools.BuiltinToolEntry{
			Name:    "mcp_stub_tool",
			GetSpec: tools.GetWaitToolSpec,
			HandleTool: func(context.Context, *tools.RuntimeContext, tools.ToolInvocation) tools.ToolResult {
				return tools.BuildSuccessResult("mcp_stub_tool", "ok", nil, nil, nil)
			},
			Stage:           tools.StageAction,
			Visibility:      tools.VisibilityDeferred,
			EnabledByConfig: func() bool { return true },
		},
	))
	app.SetToolRegistry(merged)

	view := app.Planner().ForSession("s1")
	if view.registry() != merged {
		t.Fatal("会话视图应解析到替换后的共享注册表")
	}
}

// ---------------------------------------------------------------------------
// planner bridge 回归测试（L1/L2/L11/L6）
// ---------------------------------------------------------------------------

// newLLMBackedApp 构造带 LLM（指向 httptest 假 provider）的最小 App，并记录
// 每个 POST 请求体，供 planner bridge 测试断言。
func newLLMBackedApp(t *testing.T, respond func(index int, body map[string]any) (int, any)) (*App, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		mu.Lock()
		index := len(bodies)
		bodies = append(bodies, body)
		mu.Unlock()
		status, payload := respond(index, body)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_ = json.NewEncoder(writer).Encode(payload)
	}))
	t.Cleanup(server.Close)

	app := &App{log: slog.New(slog.DiscardHandler), Bus: monitor.NewBus()}
	app.LLM = llm.NewOrchestrator(llm.OrchestratorOptions{
		Config: llm.ModelConfig{
			Providers: map[string]llm.APIProvider{
				"mock": {Name: "mock", BaseURL: server.URL + "/v1", APIKey: "k", AuthType: llm.AuthBearer, MaxRetry: 1, Timeout: 5, RetryInterval: 0.001},
			},
			Models: map[string]llm.ModelInfo{
				"planner-model": {ModelIdentifier: "planner-model", Name: "planner-model", APIProvider: "mock"},
			},
			Tasks: map[string]llm.TaskConfig{
				"planner": {ModelList: []string{"planner-model"}, MaxTokens: 128, Temperature: llm.Float64Ptr(0.5), HardTimeout: 5},
			},
		},
		HTTPClient: server.Client(),
	})
	return app, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}
}

// plannerChatLoopPayload 构造带 assistant 文本 + 单个 tool_call 的响应。
func plannerChatLoopPayload(content string, callID string, funcName string, arguments string) map[string]any {
	return map[string]any{
		"id": "chatcmpl-1", "model": "test-model",
		"choices": []map[string]any{{
			"index": 0, "finish_reason": "tool_calls",
			"message": map[string]any{
				"role":    "assistant",
				"content": content,
				"tool_calls": []map[string]any{{
					"id": callID, "type": "function",
					"function": map[string]any{"name": funcName, "arguments": arguments},
				}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
	}
}

// TestChatLoopStepSendsToolParametersSchema 验证 L1：工具定义的 parameters
// schema 必须进入请求体（此前只有 Name/Description，convertTools 对空
// ParametersJSON 回退空 object schema，模型不知道该传什么参数）。
func TestChatLoopStepSendsToolParametersSchema(t *testing.T) {
	app, recordedBodies := newLLMBackedApp(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, plannerChatLoopPayload("", "call-1", "wait", `{"seconds":1}`)
	})
	bridge := app.Planner().ForSession("s1")
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"seconds": map[string]any{"type": "number"}},
		"required":   []string{"seconds"},
	}
	_, err := bridge.ChatLoopStep(context.Background(), runtime.PlannerRequest{
		SessionID:     "s1",
		LogicalTurnID: "turn-1",
		ToolDefinitions: []runtime.PlannerToolDefinition{
			{Name: "wait", Description: "等待一段时间", Parameters: schema},
		},
	})
	if err != nil {
		t.Fatalf("ChatLoopStep: %v", err)
	}
	body := recordedBodies()[0]
	toolList, ok := body["tools"].([]any)
	if !ok || len(toolList) != 1 {
		t.Fatalf("tools = %#v", body["tools"])
	}
	function := toolList[0].(map[string]any)["function"].(map[string]any)
	parameters, ok := function["parameters"].(map[string]any)
	if !ok || parameters["type"] != "object" {
		t.Fatalf("parameters schema 丢失: %#v", function["parameters"])
	}
	if _, ok := parameters["properties"].(map[string]any)["seconds"]; !ok {
		t.Fatalf("parameters.properties.seconds 丢失: %#v", parameters)
	}
}

// TestChatLoopStepOutputItemsCarryToolCalls 验证 L2：assistant 文本与
// function_call items 必须写回 PlannerResponse.OutputItems（引擎将其
// AppendChatHistory），否则多轮工具循环模型失忆。
func TestChatLoopStepOutputItemsCarryToolCalls(t *testing.T) {
	app, _ := newLLMBackedApp(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, plannerChatLoopPayload("我先等待一下", "call-9", "wait", `{"seconds":2}`)
	})
	bridge := app.Planner().ForSession("s1")
	response, err := bridge.ChatLoopStep(context.Background(), runtime.PlannerRequest{
		SessionID:     "s1",
		LogicalTurnID: "turn-9",
	})
	if err != nil {
		t.Fatalf("ChatLoopStep: %v", err)
	}
	if len(response.OutputItems) != 2 {
		t.Fatalf("OutputItems 条数 = %d, want 2: %#v", len(response.OutputItems), response.OutputItems)
	}
	assistant := response.OutputItems[0]
	if assistant.Kind != llm.ItemKindAssistant || assistant.Text() != "我先等待一下" {
		t.Fatalf("第 1 项应为 assistant 文本: %#v", assistant)
	}
	call := response.OutputItems[1]
	if call.Kind != llm.ItemKindFunctionCall || call.ToolCall == nil {
		t.Fatalf("第 2 项应为 function_call: %#v", call)
	}
	if call.ToolCall.CallID != "call-9" || call.ToolCall.FuncName != "wait" {
		t.Fatalf("function_call 内容不符: %#v", call.ToolCall)
	}
	if call.Meta.LogicalTurnID != "turn-9" {
		t.Fatalf("function_call 应绑定本轮 logical_turn_id: %q", call.Meta.LogicalTurnID)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Args["seconds"] != float64(2) {
		t.Fatalf("ToolCalls 参数解析不符: %#v", response.ToolCalls)
	}
}

// TestPlannerMessagesPreservesToolChain 验证 L2 的历史投影：相邻 function_call
// 折叠为带 tool_calls 的 assistant 消息、tool result 转为带 tool_call_id 的
// tool 消息（镜像 Python openai_client._convert_messages）；请求不带工具时
// 清洗整条工具链（_sanitize_messages_for_toolless_request）。
func TestPlannerMessagesPreservesToolChain(t *testing.T) {
	callItem, err := llm.NewFunctionCallItem("turn-1", "call-1", "wait", map[string]any{"seconds": float64(1)})
	if err != nil {
		t.Fatalf("NewFunctionCallItem: %v", err)
	}
	receipt := llm.NewToolResultItem("call-1", "等待已结束。", "wait")
	history := chatcontext.BuildModelOutputContextMessages([]*llm.ContextItem{callItem, receipt}, chatcontext.SourceAssistant)

	request := runtime.PlannerRequest{
		History:         history,
		ToolDefinitions: []runtime.PlannerToolDefinition{{Name: "wait"}},
	}
	messages := plannerMessages(request)
	if len(messages) != 2 {
		t.Fatalf("messages 条数 = %d, want 2: %#v", len(messages), messages)
	}
	assistant := messages[0]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call-1" {
		t.Fatalf("第 1 条应为带 tool_calls 的 assistant: %#v", assistant)
	}
	if assistant.ToolCalls[0].ArgsJSON == "" {
		t.Fatalf("tool_calls.arguments 不能为空: %#v", assistant.ToolCalls[0])
	}
	toolMessage := messages[1]
	if toolMessage.Role != "tool" || toolMessage.ToolCallID != "call-1" || toolMessage.Content != "等待已结束。" {
		t.Fatalf("第 2 条应为 tool result: %#v", toolMessage)
	}

	// 无工具请求：整条工具链被清洗。
	sanitized := plannerMessages(runtime.PlannerRequest{History: history})
	if len(sanitized) != 0 {
		t.Fatalf("无工具时应清洗工具链: %#v", sanitized)
	}
}

// TestExecuteToolCallsReturnsParseFailureReceipt 验证 L11：模型返回的
// arguments 不是合法 JSON 时，工具不被执行，回执明确说明"参数解析失败"，
// 保证 assistant tool_call → tool result 协议链完整。
func TestExecuteToolCallsReturnsParseFailureReceipt(t *testing.T) {
	app, _ := newLLMBackedApp(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, plannerChatLoopPayload("", "call-bad", "wait", `{"seconds": not-json}`)
	})
	bridge := app.Planner().ForSession("s1")
	response, err := bridge.ChatLoopStep(context.Background(), runtime.PlannerRequest{
		SessionID:     "s1",
		LogicalTurnID: "turn-1",
	})
	if err != nil {
		t.Fatalf("ChatLoopStep: %v", err)
	}
	if len(response.ToolCalls) != 1 || len(response.ToolCalls[0].Args) != 0 {
		t.Fatalf("解析失败时应得到空参数: %#v", response.ToolCalls)
	}
	results, err := bridge.ExecuteToolCalls(context.Background(), response.ToolCalls, "")
	if err != nil {
		t.Fatalf("ExecuteToolCalls: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %#v", results)
	}
	if results[0].Success {
		t.Fatalf("参数解析失败的调用应返回失败回执: %#v", results[0])
	}
	if !strings.Contains(results[0].Content, "参数解析失败") {
		t.Fatalf("回执应说明参数解析失败: %q", results[0].Content)
	}
}

// TestExecuteToolCallsAppendsToolReceipt 验证 L2 的工具回执路径：普通工具的
// 回执以 tool result item 的形态进入 PlannerToolResult.PostHistory（引擎会
// AppendChatHistory）；wait 暂停回执不入历史（对齐 Python
// HISTORY_DEFERRED_TOOL_RESULT_NAMES={"wait"}）。
func TestExecuteToolCallsAppendsToolReceipt(t *testing.T) {
	app := newPipelineApp(t)
	invoked := false
	// 内置条目 + stub：wait 回执断言依赖内置 wait 工具仍然可用。
	app.SetToolRegistry(tools.NewToolRegistry(append(app.BuiltinEntries(), tools.BuiltinToolEntry{
		Name: "stub_ok",
		GetSpec: func() tools.ToolSpec {
			return tools.ToolSpec{Name: "stub_ok", Description: "测试工具", Stage: tools.StageAction, Visibility: tools.VisibilityVisible}
		},
		HandleTool: func(context.Context, *tools.RuntimeContext, tools.ToolInvocation) tools.ToolResult {
			invoked = true
			return tools.BuildSuccessResult("stub_ok", "测试完成", nil, nil, nil)
		},
		Stage:           tools.StageAction,
		Visibility:      tools.VisibilityVisible,
		EnabledByConfig: func() bool { return true },
	})))

	bridge := app.Planner().ForSession("s1")
	results, err := bridge.ExecuteToolCalls(context.Background(), []runtime.PlannerToolCall{{
		CallID:        "call-ok",
		FuncName:      "stub_ok",
		LogicalTurnID: "turn-1",
	}}, "")
	if err != nil {
		t.Fatalf("ExecuteToolCalls: %v", err)
	}
	if !invoked || !results[0].Success {
		t.Fatalf("stub 工具应被执行且成功: invoked=%v results=%#v", invoked, results)
	}
	if len(results[0].PostHistory) != 1 {
		t.Fatalf("普通工具回执应进入 PostHistory: %#v", results[0].PostHistory)
	}
	receipt := results[0].PostHistory[0]
	if receipt.Kind != llm.ItemKindToolResult || receipt.CallID != "call-ok" || receipt.Output != "测试完成" {
		t.Fatalf("回执 item 内容不符: %#v", receipt)
	}

	// wait 暂停回执：不写历史（等待结束后由引擎补 wait 完成消息）。
	// wait 工具依赖已登记的会话，先走一遍消息管线（对齐
	// TestExecuteToolCallsCarriesLogicalTurnID 的准备步骤）。
	msg := makeQQGroupMessage("u_receipt", "g_receipt")
	if err := app.handleMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	hf, err := app.Heartflow.GetOrCreateHeartflowChat(msg.SessionID)
	if err != nil {
		t.Fatalf("心流会话创建失败: %v", err)
	}
	waitResults, err := app.Planner().ForSession(msg.SessionID).ExecuteToolCalls(context.Background(), []runtime.PlannerToolCall{{
		CallID:        "call-wait",
		FuncName:      "wait",
		LogicalTurnID: "turn-1",
		Args:          map[string]any{"seconds": float64(1)},
	}}, "")
	if err != nil {
		t.Fatalf("ExecuteToolCalls(wait): %v", err)
	}
	if !waitResults[0].PauseExecution {
		t.Fatalf("wait 应触发暂停: %#v", waitResults[0])
	}
	if hf.AgentState() != trigger.StateWait {
		t.Fatalf("wait 应使 runtime 进入等待态: %s", hf.AgentState())
	}
	if len(waitResults[0].PostHistory) != 0 {
		t.Fatalf("wait 暂停回执不应入历史: %#v", waitResults[0].PostHistory)
	}
}

// TestPlannerSingletonConcurrentAccess 验证 L6：并发首次访问 Planner() 必须
// 返回同一实例（plannerMu 保护；并发压力测试，本机 -race 不可用）。
func TestPlannerSingletonConcurrentAccess(t *testing.T) {
	app := &App{}
	const goroutines = 64
	results := make(chan *plannerBridge, goroutines)
	var waitGroup sync.WaitGroup
	for index := 0; index < goroutines; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			results <- app.Planner()
		}()
	}
	waitGroup.Wait()
	close(results)
	first := <-results
	for view := range results {
		if view != first {
			t.Fatal("并发 Planner() 应返回同一单例")
		}
	}
}

// TestChatLoopStepRequestCarriesSessionTasks sanity：ChatWithSession 的任务名
// 必须是 planner（工具循环走 planner 任务配置）。防止后续重构悄悄改掉。
func TestChatLoopStepUsesPlannerTask(t *testing.T) {
	app, recordedBodies := newLLMBackedApp(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, plannerChatLoopPayload("", "call-1", "wait", "{}")
	})
	bridge := app.Planner().ForSession("s1")
	if _, err := bridge.ChatLoopStep(context.Background(), runtime.PlannerRequest{SessionID: "s1"}); err != nil {
		t.Fatalf("ChatLoopStep: %v", err)
	}
	if body := recordedBodies()[0]; body["model"] != "planner-model" {
		t.Fatalf("planner 任务应解析到 planner-model: %#v", body["model"])
	}
}

// ---------------------------------------------------------------------------
// P0-2 回归：群/私判定必须来自 BotChatSession.IsGroupSession
// ---------------------------------------------------------------------------

// TestNewRuntimeResolvesSessionKindFromChatStream 验证：私聊会话的 runtime
// IsGroupSession=false 且基础频率走 private_talk_value；群聊走 talk_value。
// 此前 isGroup 由 session_id 字符串后缀猜测（md5 hex 永不含 _private），
// 所有会话恒判为群聊。
func TestNewRuntimeResolvesSessionKindFromChatStream(t *testing.T) {
	app := newPipelineApp(t)
	// 把群/私频率配置改成不同值，验证取值来源。
	global := app.Cfg.GetGlobalConfig()
	global.Chat.ReplyTiming.TalkValue = 0.9
	global.Chat.ReplyTiming.PrivateTalkValue = 0.3

	// 用真实 a.newRuntime 重建 Heartflow（newPipelineApp 注入的是轻量工厂）。
	app.Heartflow = heartflow.NewHeartflowManager(heartflow.Options{
		ChatManager: app.ChatManager,
		NewRuntime:  app.newRuntime,
		Log:         func(string, ...any) {},
	})

	privateMsg := makeQQGroupMessage("u-private", "")
	privateMsg.MessageInfo.GroupInfo = nil // 私聊
	if err := app.handleMessage(context.Background(), privateMsg); err != nil {
		t.Fatalf("handleMessage(private): %v", err)
	}
	privateRuntime, err := app.Heartflow.GetOrCreateHeartflowChat(privateMsg.SessionID)
	if err != nil {
		t.Fatalf("私聊心流创建失败: %v", err)
	}
	privateOptions := privateRuntime.Options()
	if privateOptions.IsGroupSession {
		t.Fatalf("私聊会话被误判为群聊（P0-2 回归）: %+v", privateOptions)
	}
	if privateOptions.BaseReplyFrequency != 0.3 {
		t.Fatalf("私聊基础频率应取 private_talk_value=0.3: %v", privateOptions.BaseReplyFrequency)
	}
	// ToolCtx 必须取到真实群/私值（此前硬编码 true）。
	if ctx := app.ToolCtx(privateMsg.SessionID); ctx.IsGroupSession {
		t.Fatalf("私聊 ToolCtx.IsGroupSession 应为 false: %v", ctx.IsGroupSession)
	}

	groupMsg := makeQQGroupMessage("u-group", "g-kind")
	if err := app.handleMessage(context.Background(), groupMsg); err != nil {
		t.Fatalf("handleMessage(group): %v", err)
	}
	groupRuntime, err := app.Heartflow.GetOrCreateHeartflowChat(groupMsg.SessionID)
	if err != nil {
		t.Fatalf("群聊心流创建失败: %v", err)
	}
	groupOptions := groupRuntime.Options()
	if !groupOptions.IsGroupSession {
		t.Fatalf("群聊会话应判为群聊: %+v", groupOptions)
	}
	if groupOptions.BaseReplyFrequency != 0.9 {
		t.Fatalf("群聊基础频率应取 talk_value=0.9: %v", groupOptions.BaseReplyFrequency)
	}
}

// TestPrivateChatSkipsDynamicGate 验证私聊动态门控被跳过（对齐 gates.py
// 私聊不设门控分支）：群聊里低概率消息会被动态门控拦下，私聊必须直接放行。
func TestPrivateChatSkipsDynamicGate(t *testing.T) {
	app := newPipelineApp(t)
	global := app.Cfg.GetGlobalConfig()
	global.Chat.ReplyTiming.ReplyTriggerMode = "dynamic"
	global.Chat.ReplyTiming.PrivateTalkValue = 0.5

	app.Heartflow = heartflow.NewHeartflowManager(heartflow.Options{
		ChatManager: app.ChatManager,
		NewRuntime:  app.newRuntime,
		Log:         func(string, ...any) {},
	})

	privateMsg := makeQQGroupMessage("u-gate", "")
	privateMsg.MessageInfo.GroupInfo = nil
	if err := app.handleMessage(context.Background(), privateMsg); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	privateRuntime, err := app.Heartflow.GetOrCreateHeartflowChat(privateMsg.SessionID)
	if err != nil {
		t.Fatalf("私聊心流创建失败: %v", err)
	}
	privateRuntime.Start(make(chan struct{}))
	defer privateRuntime.Stop()

	// 占位符消息（[表情包]）概率低于静态阈值，群聊动态门控会拦下；私聊直接放行。
	placeholder := privateMsg
	placeholder.ProcessedPlainText = message.StringPtr("[表情包]")
	privateRuntime.RegisterMessage(placeholder)
	if !privateRuntime.MessageTurnScheduled() {
		t.Fatal("私聊不设动态门控，低概率消息也应直接进入 Planner")
	}
}

// ---------------------------------------------------------------------------
// P1-5 回归：Planner 请求必须携带 system 提示词
// ---------------------------------------------------------------------------

// TestChatLoopStepPrependsSystemMessage 验证 planner 请求首条消息为 system
// （模板/配置缺失时使用内置兜底提示词，不允许再发裸历史）。
func TestChatLoopStepPrependsSystemMessage(t *testing.T) {
	app, recordedBodies := newLLMBackedApp(t, func(int, map[string]any) (int, any) {
		return http.StatusOK, plannerChatLoopPayload("", "call-1", "wait", "{}")
	})
	bridge := app.Planner().ForSession("s1")
	if _, err := bridge.ChatLoopStep(context.Background(), runtime.PlannerRequest{SessionID: "s1"}); err != nil {
		t.Fatalf("ChatLoopStep: %v", err)
	}
	messages, ok := recordedBodies()[0]["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("messages = %#v", recordedBodies()[0]["messages"])
	}
	system := messages[0].(map[string]any)
	if system["role"] != "system" {
		t.Fatalf("首条消息应为 system: %#v", system)
	}
	content, _ := system["content"].(string)
	if strings.TrimSpace(content) == "" {
		t.Fatal("system 提示词不应为空")
	}
	// system 只允许出现在首条（历史/注入消息不得插入新的 system）。
	for index, raw := range messages[1:] {
		entry := raw.(map[string]any)
		if entry["role"] == "system" {
			t.Fatalf("第 %d 条又出现 system 消息: %#v", index+1, entry)
		}
	}
}

// TestPlannerSystemPromptRendersMaisakaTemplate 验证模板路径：仓库自带
// prompts/zh-CN/maisaka_chat.prompt 应被人设与场景注意事项填充。
func TestPlannerSystemPromptRendersMaisakaTemplate(t *testing.T) {
	app := newPipelineApp(t)
	// 指向仓库 prompts 目录（测试运行于 internal/assemble，仓库根在 ../..）。
	// 必须在 promptManager 首次加载前设置（sync.Once 缓存）。
	app.opts.RootDir = "../.."
	global := app.Cfg.GetGlobalConfig()
	global.Personality.BehaviorStyle = "测试行为风格ABCD"
	global.Chat.ReplyStyle.GroupChatPrompt = "群聊注意事项XYZ"
	global.Chat.ReplyStyle.PrivateChatPrompt = "私聊注意事项XYZ"

	bridge := app.Planner().ForSession("s1")
	groupPrompt := bridge.plannerSystemPrompt(true)
	if !strings.Contains(groupPrompt, "麦麦") {
		t.Fatalf("群聊 system 提示词应含 bot 昵称: %q", groupPrompt)
	}
	if !strings.Contains(groupPrompt, "测试行为风格ABCD") {
		t.Fatalf("群聊 system 提示词应渲染 behavior_style: %q", groupPrompt)
	}
	if !strings.Contains(groupPrompt, "群聊注意事项XYZ") {
		t.Fatalf("群聊 system 提示词应渲染群聊注意事项: %q", groupPrompt)
	}
	if strings.Contains(groupPrompt, "私聊注意事项XYZ") {
		t.Fatalf("群聊 system 提示词不应混入私聊注意事项: %q", groupPrompt)
	}
	if !strings.Contains(groupPrompt, "query_memory()") {
		t.Fatalf("system 提示词应含 query_memory 使用规则: %q", groupPrompt)
	}

	privatePrompt := bridge.plannerSystemPrompt(false)
	if !strings.Contains(privatePrompt, "私聊注意事项XYZ") {
		t.Fatalf("私聊 system 提示词应渲染私聊注意事项: %q", privatePrompt)
	}
	if strings.Contains(privatePrompt, "群聊注意事项XYZ") {
		t.Fatalf("私聊 system 提示词不应混入群聊注意事项: %q", privatePrompt)
	}
}
