// Package assemble wires every module into a runnable bot — the Go counterpart
// of 参考/MaiBot/src/main.py (MainSystem.initialize/_init_components/schedule_tasks).
//
// 装配链路（对齐 main.py 初始化顺序）:
//
//	config 双文件加载/热重载 → SQLite → LLM orchestrator → A_memorix 内核
//	→ prompt 加载 → ChatManager → memory flow workers → ToolRegistry(+MCP)
//	→ runtime/Planner（每会话）→ HeartflowManager → QQ 渠道 → WebUI
package assemble

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/chat/heartflow"
	"oh-my-qqbot/internal/chat/manager"
	"oh-my-qqbot/internal/chat/runtime"
	"oh-my-qqbot/internal/chat/tools"
	"oh-my-qqbot/internal/chat/trigger"
	"oh-my-qqbot/internal/config"
	"oh-my-qqbot/internal/database"
	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/memorix"
	"oh-my-qqbot/internal/message"
	"oh-my-qqbot/internal/monitor"
	"oh-my-qqbot/internal/qq"
)

// Options carries the assembly inputs (paths and overrides).
type Options struct {
	// RootDir is the working directory root (config/, data/, prompts/).
	RootDir string
	// Version labels the build (webui banner).
	Version string
	// Sandbox forces the QQ sandbox gateway regardless of config.
	Sandbox bool
	// Headless skips the QQ channel connection (config/webui only).
	Headless bool
	// Log is the base logger; nil → slog.Default().
	Log *slog.Logger
}

// WebUIKernel is the minimal surface main uses from internal/webui
// (satisfied by *webui.Server).
type WebUIKernel interface {
	ListenAndServe() error
	Close() error
}

// WebUIFactory builds the WebUI server late in assembly (avoids the assemble
// package importing internal/webui, which imports config/database directly).
type WebUIFactory func(app *App) (WebUIKernel, error)

// MCPStarter boots the MCP client service (satisfied by internal/mcp wiring).
type MCPStarter func(ctx context.Context, app *App) error

// App is the assembled application.
type App struct {
	opts Options
	log  *slog.Logger

	Cfg         *config.Manager
	DB          *database.DB
	LLM         *llm.Orchestrator
	Bus         *monitor.Bus
	Kernel      *memorix.Kernel
	ChatManager *manager.ChatManager
	Heartflow   *heartflow.HeartflowManager
	Channel     *qq.Channel

	planner *plannerBridge

	// Injected starters (set by cmd/bot/main.go, which owns the webui/mcp
	// imports; the core pipeline works without them).
	WebUIFactory WebUIFactory
	MCPStarter   MCPStarter

	webui     WebUIKernel
	stopChans []chan struct{}
	shutdown  []func(context.Context) error
	closeMu   sync.Mutex
}

// New assembles the application skeleton without starting background work.
func New(opts Options) (*App, error) {
	if opts.RootDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		opts.RootDir = wd
	}
	if opts.Version == "" {
		opts.Version = contracts.Version
	}
	app := &App{opts: opts, log: opts.Log}
	if app.log == nil {
		app.log = slog.Default()
	}
	return app, nil
}

// ConfigDir / DataDir / PromptsDir resolve standard sub-paths.
func (a *App) ConfigDir() string  { return filepath.Join(a.opts.RootDir, "config") }
func (a *App) DataDir() string    { return filepath.Join(a.opts.RootDir, "data") }
func (a *App) PromptsDir() string { return filepath.Join(a.opts.RootDir, "prompts") }

// Logger returns the base logger for late-stage wiring (webui/mcp starters).
func (a *App) Logger() *slog.Logger { return a.log }

// SetupPhase1 loads config + database + LLM layer (fail fast on bad config).
// Mirrors main.py: config_manager → database → llm orchestrator.
func (a *App) SetupPhase1() error {
	if _, err := config.EnsureTemplateFiles(a.ConfigDir()); err != nil {
		a.log.Warn("配置模板生成失败", "err", err)
	}
	cfg := config.NewManager(a.ConfigDir())
	if err := cfg.Initialize(); err != nil {
		return fmt.Errorf("初始化配置失败: %w", err)
	}
	a.Cfg = cfg

	if err := os.MkdirAll(a.DataDir(), 0o755); err != nil {
		return err
	}
	db, err := database.Open(filepath.Join(a.DataDir(), "mai.db"))
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	a.DB = db
	a.shutdown = append(a.shutdown, func(context.Context) error { return db.Close() })

	a.Bus = monitor.NewBus()
	a.LLM = llm.NewOrchestrator(llm.OrchestratorOptions{
		Config:   bridgeModelConfig(cfg.GetModelConfig()),
		Recorder: llm.NewUsageRecorder(dbUsageWriter{db: a.DB}),
		OnAttempt: func(attempt llm.TaskAttempt) {
			switch attempt.Status {
			case "retrying":
				a.Bus.Emit(contracts.Event{
					Kind: contracts.EventLLMRetry,
					Time: time.Now(),
					Payload: map[string]any{
						"task_name": attempt.TaskName, "model": attempt.ModelName,
						"attempt": attempt.Attempt, "reason": attempt.Reason,
					},
				})
			case "failed":
				a.Bus.Emit(contracts.Event{
					Kind: contracts.EventLLMError,
					Time: time.Now(),
					Payload: map[string]any{
						"task_name": attempt.TaskName, "model": attempt.ModelName, "reason": attempt.Reason,
					},
				})
			}
		},
	})

	cfg.RegisterReloadCallback(func(scopes []string, _ []string) {
		for _, s := range scopes {
			if s == "model" {
				a.LLM.SetConfig(bridgeModelConfig(cfg.GetModelConfig()))
				a.log.Info("模型配置已热重载")
			}
		}
	})
	return nil
}

// SetupPhase2 starts the memory kernel, chat manager and heartflow manager
// (main.py _init_components order, plugin/a_memorix parts kept).
func (a *App) SetupPhase2(ctx context.Context) error {
	global := a.Cfg.GetGlobalConfig()

	kernel, err := memorix.New(ctx, memorix.Options{
		DataDir:    global.AMemorix.Storage.DataDir,
		Embedding:  memorix.DefaultEmbeddingOptions(),
		Extraction: memorix.DefaultExtractionOptions(),
	}, memorixChatCompleter{llm: a.LLM}, a.LLM)
	if err != nil {
		return fmt.Errorf("初始化记忆内核失败: %w", err)
	}
	a.Kernel = kernel
	a.shutdown = append(a.shutdown, func(context.Context) error { return kernel.Close() })

	a.ChatManager = manager.NewChatManager(a.DB)
	if err := a.ChatManager.Initialize(); err != nil {
		return fmt.Errorf("初始化聊天管理器失败: %w", err)
	}
	if err := a.ChatManager.LoadAllSessionsFromDB(); err != nil {
		a.log.Warn("会话恢复失败", "err", err)
	}

	a.Heartflow = heartflow.NewHeartflowManager(heartflow.Options{
		ChatManager: a.ChatManager,
		NewRuntime:  a.newRuntime,
		Log: func(format string, args ...any) {
			a.log.Info(fmt.Sprintf(format, args...))
		},
	})
	return nil
}

// newRuntime builds a per-session runtime wired to the shared planner bridge.
func (a *App) newRuntime(sessionID string) (*runtime.Runtime, error) {
	global := a.Cfg.GetGlobalConfig()
	rt := global.Chat.ReplyTiming
	isGroup := !stringsHasSuffixPrivate(sessionID)
	opts := runtime.Options{
		SessionID:                          sessionID,
		IsGroupSession:                     isGroup,
		MaxContextSize:                     global.Chat.MaxContextSize,
		PlannerInterruptMaxConsecutiveCount: rt.PlannerInterruptMaxConsecutiveCount,
		MaxConsecutiveWaitCount:            rt.MaxConsecutiveWaitCount,
		EnableContextOptimization:          global.Chat.EnableContextOptimization,
		MidTermMemory:                      global.Chat.MidTermMemory,
		MidTermMemoryLength:                global.Chat.MidTermMemoryLenth,
		IsBotSelf: func(platform, userID string) bool {
			return userID != "" && userID == global.Bot.QQAccount
		},
		Log: func(format string, args ...any) {
			a.log.Info(fmt.Sprintf(format, args...), "session", sessionID)
		},
	}
	if isGroup {
		opts.BaseReplyFrequency = rt.TalkValue
	} else {
		opts.BaseReplyFrequency = rt.PrivateTalkValue
	}
	opts.DynamicReplyTriggerEnabled = rt.ReplyTriggerMode == "dynamic"
	// 空闲退避配置（照抄 chat.reply_timing 四字段）。
	opts.IdleBackoffConfig = trigger.ReplyTimingBackoffConfig{
		BaseSeconds:        rt.NoActionBackoffBaseSeconds,
		CapSeconds:         rt.NoActionBackoffCapSeconds,
		StartCount:         rt.NoActionBackoffStartCount,
		BypassPendingCount: rt.NoActionBackoffBypassPendingCount,
	}

	stop := make(chan struct{})
	a.stopChans = append(a.stopChans, stop)
	rt2 := runtime.NewRuntime(opts)
	rt2.SetPlanner(a.Planner().ForSession(sessionID))
	rt2.Start(stop)
	return rt2, nil
}

// Run starts every background service and blocks until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	global := a.Cfg.GetGlobalConfig()

	if err := a.Cfg.StartWatching(); err != nil {
		a.log.Warn("配置热重载监听失败", "err", err)
	}
	a.shutdown = append(a.shutdown, func(context.Context) error {
		a.Cfg.StopWatching()
		return nil
	})

	if global.MCP.Enable && a.MCPStarter != nil {
		if err := a.MCPStarter(ctx, a); err != nil {
			a.log.Warn("MCP 启动失败", "err", err)
		}
	}
	if global.WebUI.Enabled && a.WebUIFactory != nil {
		webui, err := a.WebUIFactory(a)
		if err != nil {
			a.log.Warn("WebUI 初始化失败", "err", err)
		} else {
			a.webui = webui
			go func() {
				if err := webui.ListenAndServe(); err != nil {
					a.log.Info("WebUI 退出", "err", err)
				}
			}()
		}
	}
	if !a.opts.Headless && global.Bot.QQAccount != "" {
		if err := a.startChannel(ctx); err != nil {
			return fmt.Errorf("QQ 渠道启动失败: %w", err)
		}
	} else if a.opts.Headless {
		a.log.Info("Headless 模式：跳过 QQ 渠道连接")
	}

	a.log.Info("oh-my-qqbot 启动完成", "version", a.opts.Version, "nickname", global.Bot.Nickname)

	<-ctx.Done()
	a.log.Info("收到退出信号，开始优雅关停")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a.Close(shutdownCtx)
	return nil
}

// Close stops background services in reverse assembly order.
func (a *App) Close(ctx context.Context) {
	a.closeMu.Lock()
	defer a.closeMu.Unlock()
	if a.webui != nil {
		_ = a.webui.Close()
		a.webui = nil
	}
	for _, ch := range a.stopChans {
		close(ch)
	}
	a.stopChans = nil
	if a.Channel != nil {
		_ = a.Channel.Close()
	}
	a.LLM.WaitForUsage()
	for i := len(a.shutdown) - 1; i >= 0; i-- {
		if err := a.shutdown[i](ctx); err != nil {
			a.log.Warn("关停组件失败", "err", err)
		}
	}
	a.shutdown = nil
}

// startChannel wires the QQ official gateway into the message pipeline.
func (a *App) startChannel(ctx context.Context) error {
	global := a.Cfg.GetGlobalConfig()
	botNames := append([]string{global.Bot.Nickname}, global.Bot.AliasNames...)
	a.Channel = qq.NewChannel(qq.Options{
		AppID:     os.Getenv("QQ_APP_ID"),
		AppSecret: os.Getenv("QQ_APP_SECRET"),
		Sandbox:   a.opts.Sandbox,
		BotNames:  botNames,
		AccountID: global.Bot.QQAccount,
		Quota:     &qq.ProactiveOptions{GroupPerMinute: 20, GroupPerDay: 1000},
		Logger:    a.log,
	})
	handler := func(_ context.Context, msg *message.SessionMessage) error {
		if msg == nil {
			return nil
		}
		// 自发消息过滤（渠道层已过滤，这里再兜底一次机器人身份）。
		if msg.MessageInfo.UserInfo.UserID == global.Bot.QQAccount {
			return nil
		}
		// 入库（对齐 MessageUtils.store_message_to_db）。
		if _, err := a.ChatManager.StoreMessageToDB(msg); err != nil {
			a.log.Warn("消息入库失败", "err", err)
		}
		// 会话登记（对齐 ChatManager.register_message + get_or_create_session）。
		if err := a.ChatManager.RegisterMessage(msg); err != nil {
			a.log.Warn("会话登记失败", "err", err)
		}
		a.Bus.Emit(contracts.Event{
			Kind:      contracts.EventMessageIngested,
			SessionID: msg.SessionID,
			Time:      time.Now(),
			Payload:   map[string]any{"message_id": msg.MessageID, "user_id": msg.UserID()},
		})
		// 心流运行时（对齐 HeartflowManager.get_or_create_heartflow_chat）。
		hf, err := a.Heartflow.GetOrCreateHeartflowChat(msg.SessionID)
		if err != nil {
			a.log.Warn("心流会话创建失败", "err", err)
			return nil
		}
		hf.RegisterMessage(msg)
		return nil
	}
	go func() {
		if err := a.Channel.Connect(ctx, handler); err != nil && !errors.Is(err, context.Canceled) {
			a.log.Error("QQ 网关退出", "err", err)
		}
	}()
	return nil
}

// ToolRegistryFlags maps config onto the builtin tool visibility gates.
func (a *App) ToolRegistryFlags() tools.FeatureFlags {
	g := a.Cfg.GetGlobalConfig()
	return tools.FeatureFlags{
		EnableMemoryQueryTool:   g.AMemorix.Integration.EnableMemoryQueryTool,
		MemoryQueryDefaultLimit: g.AMemorix.Integration.MemoryQueryDefaultLimit,
		UseNewEmojiSendLogic:    g.Emoji.UseNewSendLogic,
		FocusMode:               g.Experimental.FocusMode,
		UseVectorExpression:     g.Expression.UseVectorExpression,
		EnableReplyAt:           g.Chat.EnableReplyAt,
		EnableReplyQuote:        true,
		BotNickname:             g.Bot.Nickname,
	}
}



func stringsHasSuffixPrivate(sessionID string) bool {
	// session_id 无法直接判定群/私；以「最后一段为 private」判定。
	// CalculateSessionID 对私聊生成 "..._<user_id>_private"。
	const suffix = "_private"
	return len(sessionID) > len(suffix) && sessionID[len(sessionID)-len(suffix):] == suffix
}


// ---------------------------------------------------------------------------
// bridges (config → llm / database → llm usage / llm → memorix)
// ---------------------------------------------------------------------------

func bridgeModelConfig(src *config.ModelConfig) llm.ModelConfig {
	out := llm.ModelConfig{
		Providers: map[string]llm.APIProvider{},
		Models:    map[string]llm.ModelInfo{},
		Tasks:     map[string]llm.TaskConfig{},
	}
	if src == nil {
		return out
	}
	for _, p := range src.APIProviders {
		out.Providers[p.Name] = llm.APIProvider{
			Name:          p.Name,
			BaseURL:       p.BaseURL,
			APIKey:        p.APIKey,
			MaxRetry:      p.MaxRetry,
			Timeout:       float64(p.Timeout),
			RetryInterval: float64(p.RetryInterval),
			ClientType:    p.ClientType,
		}
	}
	for _, m := range src.Models {
		out.Models[m.Name] = llm.ModelInfo{
			ModelIdentifier: m.ModelIdentifier,
			Name:            m.Name,
			APIProvider:     m.APIProvider,
			PriceIn:         m.PriceIn,
			PriceOut:        m.PriceOut,
			SendTemperature: m.SendTemperature,
			Visual:          m.Visual,
		}
	}
	for name, tc := range src.ModelTaskConfig.TaskConfigs() {
		temperature := tc.Temperature
		out.Tasks[name] = llm.TaskConfig{
			ModelList:         tc.ModelList,
			MaxTokens:         tc.MaxTokens,
			Temperature:       &temperature,
			SelectionStrategy: tc.SelectionStrategy,
			HardTimeout:       tc.HardTimeout,
		}
	}
	return out
}

type dbUsageWriter struct{ db *database.DB }

func (w dbUsageWriter) InsertUsage(ctx context.Context, u llm.ModelUsage) error {
	taskName := u.TaskName
	assignName := u.ModelAssignName
	_, err := w.db.InsertUsage(&database.ModelUsage{
		ModelName:             u.ModelName,
		ModelAssignName:       &assignName,
		ModelAPIProviderName:  u.ProviderName,
		SessionID:             u.SessionID,
		TaskName:              &taskName,
		RequestType:           u.RequestType,
		TimeCost:              u.TimeCost,
		Timestamp:             u.Timestamp,
		PromptTokens:          u.PromptTokens,
		CompletionTokens:      u.CompletionTokens,
		TotalTokens:           u.TotalTokens,
		PromptCacheEnabled:    u.PromptCacheEnabled,
		PromptCacheHitTokens:  u.PromptCacheHitTokens,
		PromptCacheMissTokens: u.PromptCacheMissTokens,
		Cost:                  u.Cost,
	})
	return err
}

// memorixChatCompleter adapts llm.Orchestrator to memorix.ChatCompleter.
type memorixChatCompleter struct{ llm *llm.Orchestrator }

func (c memorixChatCompleter) Chat(ctx context.Context, task string, req memorix.ChatRequest) (*memorix.ChatResponse, error) {
	out := contracts.ChatRequest{Model: req.Model, MaxTokens: req.MaxTokens, Temperature: req.Temperature}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, contracts.ChatMessage{Role: contracts.Role(m.Role), Content: m.Content})
	}
	resp, err := c.llm.Chat(ctx, task, out)
	if err != nil {
		return nil, err
	}
	return &memorix.ChatResponse{Content: resp.Content}, nil
}
