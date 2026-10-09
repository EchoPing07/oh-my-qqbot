package mcp

import (
	"context"
	"sync"
)

// This file mirrors 参考/MaiBot/src/mcp_module/service.py: 进程级 MCP 运行时
// 服务。该服务在进程生命周期内统一持有 MCP 连接，聊天 Runtime 只通过轻量
// Provider 访问共享连接，避免每个聊天流重复启动 stdio 子进程和远程 HTTP 会话。
//
// 与 Python 版的差异(并发模型):
//   - Python 用 asyncio.Lock + asynccontextmanager 表达 reload/lease;Go 用
//     sync.Mutex + 引用计数,语义一一对应(_reload_lock → reloadMu,
//     _state_lock → mu, _active_leases → activeLeases,
//     _retired_managers → retiredManagers)。
//   - reload 不后台调度(Go 侧调用方自行决定是否开 goroutine),
//     但「签名相同直接返回」「旧 manager 延迟到引用归零再关闭」完全一致。

// Service mirrors MCPService: 在进程生命周期内复用 MCP 连接并支持安全热切换。
type Service struct {
	// callbacks 是宿主侧能力(Sampling 桥/elicitation);在建连时注入新 manager。
	callbacks *HostCallbacks

	reloadMu sync.Mutex
	mu       sync.Mutex

	manager         *manager
	configSignature string
	activeLeases    map[*manager]int
	retiredManagers map[*manager]struct{}
	closed          bool

	statusMu       sync.Mutex
	statusSnapshot StatusSnapshot
}

// NewService mirrors MCPService.__init__ (进程级单例由 DefaultService 提供)。
func NewService() *Service {
	return &Service{
		activeLeases:    map[*manager]int{},
		retiredManagers: map[*manager]struct{}{},
		statusSnapshot:  EmptyStatusSnapshot(false),
	}
}

var (
	defaultServiceOnce sync.Once
	defaultService     *Service
)

// DefaultService mirrors `get_mcp_service()`：获取进程级 MCP 服务单例。
func DefaultService() *Service {
	defaultServiceOnce.Do(func() {
		defaultService = NewService()
	})
	return defaultService
}

// SetCallbacks 注入宿主侧能力回调（Sampling 桥）。应在 EnsureInitialized 之前
// 调用；热重载会使用最新注入值构造新 manager。
func (s *Service) SetCallbacks(callbacks *HostCallbacks) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.callbacks = callbacks
	s.mu.Unlock()
}

func (s *Service) callbacksRef() *HostCallbacks {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callbacks
}

// EnsureInitialized mirrors MCPService.ensure_initialized：确保共享服务已按当前
// 配置初始化。
func (s *Service) EnsureInitialized(ctx context.Context, config MCPConfig) error {
	if s == nil {
		return nil
	}
	return s.Reload(ctx, config)
}

// Reload mirrors MCPService.reload：构建新一代连接并原子替换旧管理器。
// 配置签名未变化时直接返回（不重启连接）。
func (s *Service) Reload(ctx context.Context, config MCPConfig) error {
	if s == nil {
		return nil
	}
	signature := ConfigSignature(config)

	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	s.mu.Lock()
	// 签名相同则直接返回（对齐 `if not self._closed and signature == self._config_signature`）。
	unchanged := !s.closed && signature == s.configSignature
	s.closed = false
	s.mu.Unlock()
	if unchanged {
		return nil
	}

	var newManager *manager
	if len(BuildServerRuntimeConfigs(config)) > 0 {
		newManager = managerFromConfig(ctx, config, s.callbacksRef())
	}

	s.mu.Lock()
	oldManager := s.manager
	s.manager = newManager
	s.configSignature = signature
	closeImmediately := s.retireManagerLocked(oldManager)
	s.mu.Unlock()

	s.updateStatusSnapshot(newManager, signature, true)
	if newManager != nil {
		newManager.addStatusChangeCallback(func() { s.refreshManagerStatus(newManager) })
	}
	if closeImmediately != nil {
		_ = closeImmediately.close()
	}
	return nil
}

// retireManagerLocked mirrors _retire_manager_locked：在持有状态锁时退休旧管理器，
// 并决定是否可立即关闭（引用计数归零才关闭）。
func (s *Service) retireManagerLocked(target *manager) *manager {
	if target == nil {
		return nil
	}
	if s.activeLeases[target] > 0 {
		s.retiredManagers[target] = struct{}{}
		return nil
	}
	return target
}

// Lease mirrors `_lease_manager` 的语义：租用当前管理器，防止热重载关闭仍在
// 执行工具的连接。返回的 release 必须调用（Go 用 defer 表达 async contextmanager）。
func (s *Service) Lease() (result *ManagerLease) {
	if s == nil {
		return &ManagerLease{}
	}
	s.mu.Lock()
	current := s.manager
	if current != nil {
		s.activeLeases[current]++
	}
	s.mu.Unlock()
	return &ManagerLease{service: s, manager: current}
}

// ManagerLease mirrors `_lease_manager` 的 yield 结果。
type ManagerLease struct {
	service *Service
	manager *manager
	mu      sync.Mutex
	done    bool
}

// Manager 返回被租用的管理器（可能为 nil，表示当前没有启用的服务器）。
func (l *ManagerLease) Manager() *manager {
	if l == nil {
		return nil
	}
	return l.manager
}

// Release mirrors lease 的 finally 分支：引用计数归零且管理器已退休时关闭连接。
// 幂等，可安全重复调用。
func (l *ManagerLease) Release() {
	if l == nil || l.service == nil {
		return
	}
	l.mu.Lock()
	if l.done {
		l.mu.Unlock()
		return
	}
	l.done = true
	leased := l.manager
	l.mu.Unlock()
	if leased == nil {
		return
	}

	service := l.service
	var toClose *manager
	service.mu.Lock()
	leaseCount := service.activeLeases[leased] - 1
	if leaseCount > 0 {
		service.activeLeases[leased] = leaseCount
	} else {
		delete(service.activeLeases, leased)
		if _, retired := service.retiredManagers[leased]; retired {
			delete(service.retiredManagers, leased)
			toClose = leased
		}
	}
	service.mu.Unlock()

	if toClose != nil {
		_ = toClose.close()
	}
}

// ListTools mirrors MCPService.list_tools：列出共享管理器当前可用的工具。
func (s *Service) ListTools() []ToolSpec {
	if s == nil {
		return nil
	}
	lease := s.Lease()
	defer lease.Release()
	if manager := lease.Manager(); manager != nil {
		return manager.getToolSpecs()
	}
	return nil
}

// CallTool mirrors MCPService.call_tool_invocation：调用共享 MCP 工具，并保留
// 聊天执行上下文用于审计（metadata.chat_session_id / chat_stream_id）。
func (s *Service) CallTool(ctx context.Context, invocation ToolInvocation, session SessionContext) ToolResult {
	if s == nil {
		return ToolResult{
			ToolName:     invocation.ToolName,
			Success:      false,
			ErrorMessage: "MCP 共享服务不可用",
		}
	}
	lease := s.Lease()
	manager := lease.Manager()
	var result ToolResult
	if manager == nil {
		result = ToolResult{
			ToolName:     invocation.ToolName,
			Success:      false,
			ErrorMessage: "MCP 共享服务尚未连接任何服务器",
		}
	} else {
		result = manager.callTool(ctx, invocation)
		// 注意:必须在 Release 之前把 metadata 补齐(与 Python 一致：先填结果再退出 lease)。
		if session.SessionID != "" || session.StreamID != "" {
			if result.Metadata == nil {
				result.Metadata = map[string]any{}
			}
			if _, ok := result.Metadata["chat_session_id"]; !ok {
				result.Metadata["chat_session_id"] = session.SessionID
			}
			if _, ok := result.Metadata["chat_stream_id"]; !ok {
				result.Metadata["chat_stream_id"] = session.StreamID
			}
		}
	}
	lease.Release()
	return result
}

// IsTool mirrors MCPManager.is_mcp_tool 的共享入口。
func (s *Service) IsTool(toolName string) bool {
	if s == nil {
		return false
	}
	lease := s.Lease()
	defer lease.Release()
	if manager := lease.Manager(); manager != nil {
		return manager.isTool(toolName)
	}
	return false
}

// PermissionFor mirrors manager 的工具权限查询（按工具名返回所属服务器规则）。
func (s *Service) PermissionFor(toolName string) PermissionRule {
	if s == nil {
		return PermissionRule{}
	}
	lease := s.Lease()
	defer lease.Release()
	manager := lease.Manager()
	if manager == nil {
		return PermissionRule{}
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	serverName := manager.toolToServer[toolName]
	config, ok := manager.serverConfigs[serverName]
	if !ok {
		return PermissionRule{}
	}
	return permissionOf(config)
}

// StatusSnapshot mirrors MCPService.get_status_snapshot：返回不含连接对象的
// 状态副本（线程安全）。
func (s *Service) StatusSnapshot() StatusSnapshot {
	if s == nil {
		return EmptyStatusSnapshot(false)
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return cloneStatusSnapshot(s.statusSnapshot)
}

// ConfigSignature mirrors MCPService._config_signature 的读取。
func (s *Service) ConfigSignature() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configSignature
}

// refreshManagerStatus mirrors _refresh_manager_status：避免旧连接清理覆盖新一代
// 管理器快照。
func (s *Service) refreshManagerStatus(target *manager) {
	s.mu.Lock()
	isCurrent := target == s.manager
	signature := s.configSignature
	s.mu.Unlock()
	if isCurrent {
		s.updateStatusSnapshot(target, signature, true)
	}
}

// updateStatusSnapshot mirrors _update_status_snapshot。
func (s *Service) updateStatusSnapshot(target *manager, signature string, initialized bool) {
	var snapshot StatusSnapshot
	if target != nil {
		snapshot = target.getStatusSnapshot()
	} else {
		snapshot = EmptyStatusSnapshot(initialized)
	}
	snapshot.ConfigSignature = signature
	snapshot.Loaded = initialized

	s.statusMu.Lock()
	s.statusSnapshot = snapshot
	s.statusMu.Unlock()
}

// Close mirrors MCPService.close：关闭当前及已退休的全部管理器。
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	s.mu.Lock()
	current := s.manager
	retired := make([]*manager, 0, len(s.retiredManagers))
	for manager := range s.retiredManagers {
		retired = append(retired, manager)
	}
	s.manager = nil
	s.retiredManagers = map[*manager]struct{}{}
	s.activeLeases = map[*manager]int{}
	s.configSignature = ""
	s.closed = true
	s.mu.Unlock()

	var firstError error
	managers := append(retired, current)
	for _, manager := range managers {
		if manager == nil {
			continue
		}
		if err := manager.close(); err != nil && firstError == nil {
			firstError = err
		}
	}
	s.updateStatusSnapshot(nil, "", false)
	return firstError
}

func cloneStatusSnapshot(snapshot StatusSnapshot) StatusSnapshot {
	cloned := snapshot
	cloned.Servers = append([]ServerInfo{}, snapshot.Servers...)
	return cloned
}
