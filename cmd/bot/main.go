// cmd/bot/main.go — entrypoint: assembly, startup and graceful shutdown
// (Go counterpart of 参考/MaiBot/src/main.py main()).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"oh-my-qqbot/internal"
	"oh-my-qqbot/internal/assemble"
	"oh-my-qqbot/internal/mcp"
	"oh-my-qqbot/internal/webui"
)

func main() {
	root := flag.String("root", ".", "工作目录根（config/ data/ prompts/ 所在处）")
	headless := flag.Bool("headless", false, "跳过 QQ 渠道连接（仅配置/WebUI）")
	sandbox := flag.Bool("sandbox", false, "使用 QQ 沙箱环境")
	verbose := flag.Bool("v", false, "调试日志")
	showVersion := flag.Bool("version", false, "打印版本号")
	flag.Parse()

	if *showVersion {
		fmt.Println("oh-my-qqbot", internal.Version)
		return
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	// 先建 WebUI 日志桥（环形缓冲 + WS 广播），主 logger 用 MultiHandler 同时写 stderr 与桥。
	// LogService 实例后续经 webui.Options.Logs 注入 Server（共享同一份数据源）。
	levelVar.Set(level)
	logger := slog.New(slog.NewMultiHandler(
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: levelVar}),
		logService,
	))
	slog.SetDefault(logger)

	app, err := assemble.New(assemble.Options{
		RootDir:  *root,
		Version:  internal.Version,
		Sandbox:  *sandbox,
		Headless: *headless,
		Log:      logger,
	})
	if err != nil {
		logger.Error("装配失败", "err", err)
		os.Exit(1)
	}
	app.WebUIFactory = newWebUI
	app.MCPStarter = startMCP

	if err := app.SetupPhase1(); err != nil {
		logger.Error("阶段一装配失败（配置/数据库/LLM）", "err", err)
		os.Exit(1)
	}
	if err := app.SetupPhase2(context.Background()); err != nil {
		logger.Error("阶段二装配失败（内核/会话）", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx); err != nil {
		logger.Error("运行退出", "err", err)
		os.Exit(1)
	}
	logger.Info("oh-my-qqbot 已优雅关停")
}

// webUIKernel adapts *webui.Server (Start/Handler) to assemble.WebUIKernel.
type webUIKernel struct{ s *webui.Server }

func (w webUIKernel) ListenAndServe() error { return w.s.Start() }
func (w webUIKernel) Close() error          { return nil } // Server 生命周期由 http.Server 承载

// logService/levelVar 是主程序 slog 与 WebUI Server 之间共享的日志桥（main 里初始化）。
var (
	logService = webui.NewLogBridge(nil)
	levelVar   = new(slog.LevelVar)
)

// newWebUI builds the WebUI server from the assembled app.
func newWebUI(app *assemble.App) (assemble.WebUIKernel, error) {
	cfg := app.Cfg.GetGlobalConfig()
	server, err := webui.NewServer(webui.Options{
		ConfigDir:   app.ConfigDir(),
		DataDir:     app.DataDir(),
		DistDir:     "web/dashboard/dist",
		Hosts:       cfg.WebUI.Host,
		Port:        cfg.WebUI.Port,
		Version:     internal.Version,
		Memory:      app.Kernel,
		DB:          app.DB,
		Bus:         app.Bus,
		Logs:        logService,
		LogLevelVar: levelVar,
	})
	if err != nil {
		return nil, err
	}
	return webUIKernel{s: server}, nil
}

// startMCP boots the MCP client service with the config snapshot and merges
// MCP tools into the unified tool registry (deferred, tool_search-discoverable).
func startMCP(ctx context.Context, app *assemble.App) error {
	cfg := app.Cfg.GetGlobalConfig()
	service := mcp.NewService()
	mcpCfg := mcp.MCPConfig{Enable: cfg.MCP.Enable}
	if err := service.EnsureInitialized(ctx, mcpCfg); err != nil {
		return err
	}
	registry, _ := mcp.NewProvider(service).Register(app.BuiltinEntries())
	_ = registry // 新 registry 已包含 MCP 条目；planner 惰性读取时使用共享实例
	return nil
}
