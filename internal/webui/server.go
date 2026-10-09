// Package webui 实现 WebUI 后端（REWRITE_PLAN §6.16，对齐
// 参考/MaiBot/src/webui/ 的 FastAPI 版）：
//   - server.go   HTTP 服务器 + 静态托管（SPA fallback）+ 默认 127.0.0.1
//   - auth.go     TokenManager（access token 持久化 + WS 临时令牌）
//   - wshub.go    WS 统一通道（域/主题订阅 + 广播）
//   - monitor.go  EventBus → WS maisaka_monitor 域的桥 + Planner 快照缓存
//
// REST 路由在 internal/webui/routers 子包（每路由一个文件，路径/字段对齐
// Python 版），本包只做装配（Server 组合 ServerMux + 路由注册 + 鉴权注入）。
package webui

import (
	"log/slog"
	"net/http"
	"time"

	contracts "oh-my-qqbot/internal"
	config "oh-my-qqbot/internal/config"
	database "oh-my-qqbot/internal/database"
	"oh-my-qqbot/internal/webui/routers"
)

// EventBus 是观察事件总线接口（= internal.EventBus，语义对齐 contracts）。
type EventBus = contracts.EventBus

// routersDepsDB 是 *database.DB 的类型别名（routers.Deps.DB 字段类型）。
type routersDepsDB = *database.DB

// slogLevelVar 是 *slog.LevelVar 的类型别名。
type slogLevelVar = slog.LevelVar

// configNewManager / mgrInitialize 包装 config.Manager 构造（避免本文件
// 直接耦合初始化细节）。
func configNewManager(configDir string) *config.Manager { return config.NewManager(configDir) }

func mgrInitialize(mgr *config.Manager) error { return mgr.Initialize() }

// Options 是 Server 的装配参数。
type Options struct {
	// ConfigDir 配置目录（bot_config.toml / model_config.toml，config.Manager 用）。
	// 为空时配置路由返回 503。
	ConfigDir string
	// DataDir 数据目录（webui.json token 文件、用户表情包）。
	// 为空时取 "data"。
	DataDir string
	// DistDir 前端静态目录（web/dashboard/dist）。为空或不存在时仅提供 API。
	DistDir string
	// Hosts 监听地址；为空时默认 ["127.0.0.1"]（安全默认，对齐 §9.9）。
	Hosts []string
	// Port 监听端口；为 0 时取 8001（对齐 WebUIConfig.port 默认值）。
	Port int
	// Version 主程序版本号。
	Version string
	// CORS 允许的跨域来源；非空时启用 CORS（默认关闭，仅本机部署）。
	CORS []string
	// NewsURL 首页资讯上游 URL（空值取默认源）。
	NewsURL string
	// Memory 记忆内核（routers.MemoryAPI，注入后 /memory 路由可用）。
	Memory routers.MemoryAPI
	// MCP MCP 状态提供者（注入后 /mcp/status 返回真实快照）。
	MCP routers.StatusProvider
	// ChatInjector WebUI 虚拟平台发言注入器（注入后 /chat/send 可用）。
	ChatInjector routers.ChatInjector
	// DB 数据库句柄（nil 时会话/统计/资源路由 503）。
	DB routersDepsDB
	// Bus 观察事件 bus（nil 时 monitor 桥不订阅）。
	Bus EventBus
	// BusBuffer monitor 桥的订阅缓冲容量（0 取 256）。
	BusBuffer int
	// SnapshotHistory Planner 快照缓存条数上限（0 取 200）。
	SnapshotHistory int
	// LogBuffer 日志环形缓冲条数（0 取 500）。
	LogBuffer int
	// LogLevelVar slog 级别变量（与宿主日志系统共享时注入）。
	LogLevelVar *slogLevelVar
}

// Server 是 WebUI 后端宿主：组合鉴权/WS hub/monitor 桥/REST 路由。
type Server struct {
	// TokenManager 鉴权（对齐 core/security.py）。
	TokenManager *TokenManager
	// Hub WS 统一通道（对齐 websocket/unified.py + manager.py）。
	Hub *WSHub
	// Logs 日志服务（环形缓冲 + slog handler + 广播）。
	Logs *routers.LogService
	// Monitor 观察事件桥（EventBus → maisaka_monitor 域）。
	Monitor *MonitorBridge
	// Deps 路由依赖集。
	Deps *routers.Deps

	mux   *http.ServeMux
	opts  Options
	port  int
	hosts []string
}

// logsService 让 WSHub 能访问日志环形缓冲（回放快照用）。
func (s *Server) wireHubLogs() { s.Hub.logs = s.Logs }

// NewServer 装配 Server（不监听；Start 才监听）。
func NewServer(opts Options) (*Server, error) {
	if opts.DataDir == "" {
		opts.DataDir = "data"
	}
	if opts.Port == 0 {
		opts.Port = 8001
	}
	if len(opts.Hosts) == 0 {
		opts.Hosts = []string{"127.0.0.1"}
	}

	// 鉴权：启动生成/读取 data/webui.json 的 access token。
	tm, err := NewTokenManager(opts.DataDir)
	if err != nil {
		return nil, err
	}

	hub := NewWSHub(tm)
	logs := routers.NewLogService(opts.LogBuffer, opts.LogLevelVar, hub)
	monitor := NewMonitorBridge(opts.Bus, hub, opts.SnapshotHistory)

	deps := &routers.Deps{
		Auth:      tm,
		Hub:       hub,
		Logs:      logs,
		Memory:    opts.Memory,
		MCP:       opts.MCP,
		Chat:      opts.ChatInjector,
		Snapshots: monitor,
		Version:   opts.Version,
		StartedAt: time.Now(),
		DataDir:   opts.DataDir,
		NewsURL:   opts.NewsURL,
	}

	// 配置管理器（config 目录存在时才装配）。
	if opts.ConfigDir != "" {
		mgr := configNewManager(opts.ConfigDir)
		if err := mgrInitialize(mgr); err == nil {
			deps.Config = mgr
		}
	}

	s := &Server{
		TokenManager: tm,
		Hub:          hub,
		Logs:         logs,
		Monitor:      monitor,
		Deps:         deps,
		mux:          http.NewServeMux(),
		opts:         opts,
		port:         opts.Port,
		hosts:        opts.Hosts,
	}

	// REST 路由（routers.Register 对齐 Python 路由清单）。
	routers.Register(s.mux, deps)
	// WS hub 挂接日志服务（logs 快照回放）。
	s.wireHubLogs()

	// WS 统一通道端点（/api/webui/ws，对齐 unified.py 的 router.websocket("/ws")）。
	s.mux.HandleFunc("GET /api/webui/ws", hub.HandleWS)
	// 独立日志 WS 端点（对齐 logs_ws.py 的 /ws/logs，统一通道 logs 域的别名）。
	s.mux.HandleFunc("GET /api/webui/ws/logs", hub.HandleWSLogs)

	// 静态托管 + SPA fallback（磁盘目录 web/dashboard/dist）。
	s.registerStatic()

	// 健康检查（根级，无鉴权；对齐 /api/webui/health 已在 routers 中注册）。
	return s, nil
}

// Handler 返回根 HTTP handler（鉴权中间件在路由层按需包裹；
// CORS 可选注入）。测试可直接复用。
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	if len(s.opts.CORS) > 0 {
		h = corsMiddleware(h, s.opts.CORS)
	}
	return h
}

// Addrs 返回监听地址列表（host:port）。
func (s *Server) Addrs() []string {
	out := make([]string, 0, len(s.hosts))
	for _, host := range s.hosts {
		out = append(out, host+":"+itoa(s.port))
	}
	return out
}

// Start 启动 HTTP 服务器（阻塞；每个 host:port 一个 goroutine 监听）。
// 对齐 webui_server.py 的启动语义：配置/静态目录就绪后监听，token 已打印。
func (s *Server) Start() error {
	h := s.Handler()
	addrs := s.Addrs()
	if len(addrs) == 1 {
		srv := &http.Server{Addr: addrs[0], Handler: h}
		slog.Info("WebUI 服务器启动", "addr", addrs[0], "version", s.Deps.Version)
		return srv.ListenAndServe()
	}
	errCh := make(chan error, len(addrs))
	for _, addr := range addrs {
		addr := addr
		srv := &http.Server{Addr: addr, Handler: h}
		slog.Info("WebUI 服务器启动", "addr", addr, "version", s.Deps.Version)
		go func() { errCh <- srv.ListenAndServe() }()
	}
	return <-errCh
}

// corsMiddleware 可选 CORS（对齐 FastAPI CORSMiddleware 的最小面）。
func corsMiddleware(next http.Handler, origins []string) http.Handler {
	allow := map[string]bool{}
	for _, o := range origins {
		allow[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (allow[origin] || allow["*"]) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
