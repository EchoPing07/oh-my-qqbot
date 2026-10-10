package assemble

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"oh-my-qqbot/internal/chat/heartflow"
	"oh-my-qqbot/internal/chat/manager"
	"oh-my-qqbot/internal/chat/runtime"
	"oh-my-qqbot/internal/chat/tools"
	"oh-my-qqbot/internal/chat/trigger"
	"oh-my-qqbot/internal/config"
	"oh-my-qqbot/internal/database"
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
		Heartflow: newHeartflowManager(t, chatManager, func(sessionID string) (*runtime.Runtime, error) {
			return runtime.NewRuntime(runtime.Options{SessionID: sessionID, IsGroupSession: true}), nil
		}),
	}
	return app
}

// newHeartflowManager mirrors SetupPhase2 的 Heartflow 装配（NewRuntime 可注入）。
func newHeartflowManager(t *testing.T, chatManager *manager.ChatManager, newRuntime func(sessionID string) (*runtime.Runtime, error)) *heartflow.HeartflowManager {
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
