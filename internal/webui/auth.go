// TokenManager — 对齐 参考/MaiBot/src/webui/core/security.py 与
// routers/websocket/auth.py：
//   - access token 持久化在数据目录 webui.json（首次启动生成 64 位 hex）；
//   - temporary 来源每次启动重新生成，configured（用户固定 token）长期有效；
//   - 启动时日志打印一次（仅 temporary 来源显示完整 token）；
//   - WS 握手临时 token：60s 有效期、一次性消费、与 session token 绑定。
package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Token 来源（对齐 TOKEN_SOURCE_* 常量）。
const (
	TokenSourceTemporary  = "temporary"
	TokenSourceConfigured = "configured"
)

// DefaultWebUIPassword 是首次启动的默认登录密码（固定值，写进 README；
// 本机 127.0.0.1 部署用，公网部署必须在向导中修改）。
const DefaultWebUIPassword = "admin123"

// wsTokenExpireSeconds 对齐 _WS_TOKEN_EXPIRE_SECONDS。
const wsTokenExpireSeconds = 60

// webuiTokenFile 是 token 持久化文件名（对齐 data/webui.json）。
const webuiTokenFile = "webui.json"

// tokenConfig 对齐 webui.json 结构。
type tokenConfig struct {
	AccessToken         string `json:"access_token"`
	CreatedAt           string `json:"created_at,omitempty"`
	UpdatedAt           string `json:"updated_at,omitempty"`
	FirstSetupCompleted bool   `json:"first_setup_completed"`
	TokenSource         string `json:"token_source,omitempty"`
	SetupCompletedAt    string `json:"setup_completed_at,omitempty"`
}

// wsTempToken 对齐 {token: (expire_time, session_token)}。
type wsTempToken struct {
	expiresAt    time.Time
	sessionToken string
}

// TokenManager 实现 routers.AuthAPI。
type TokenManager struct {
	mu       sync.Mutex
	path     string
	config   tokenConfig
	wsTokens map[string]wsTempToken
}

// NewTokenManager 创建管理器并确保 token 存在（对齐 _ensure_config）。
// temporary 来源每次启动重新生成；日志打印一次。
func NewTokenManager(dataDir string) (*TokenManager, error) {
	if dataDir == "" {
		dataDir = "data"
	}
	path := filepath.Join(dataDir, webuiTokenFile)
	m := &TokenManager{path: path, wsTokens: map[string]wsTempToken{}}
	if err := m.ensureConfig(); err != nil {
		return nil, err
	}
	m.logStartupToken()
	return m, nil
}

// ensureConfig 对齐 _ensure_config：文件缺失/损坏/缺 token 时重建。
// 首次启动写入固定默认密码（DefaultWebUIPassword，来源 configured），
// 不再每次启动轮换随机 token——登录凭据跨重启稳定，消除抄错/过期问题。
func (m *TokenManager) ensureConfig() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return fmt.Errorf("webui: 创建数据目录失败: %w", err)
	}
	data, err := os.ReadFile(m.path)
	switch {
	case os.IsNotExist(err):
		slog.Info("WebUI 配置文件不存在，正在创建", "path", m.path)
		return m.createDefaultPassword()
	case err != nil:
		slog.Error("读取 WebUI 配置文件失败，正在重新创建", "err", err)
		return m.createDefaultPassword()
	}
	var cfg tokenConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Error("解析 WebUI 配置失败，正在重新创建", "err", err)
		return m.createDefaultPassword()
	}
	if cfg.AccessToken == "" {
		slog.Warn("WebUI 配置文件中缺少 access_token，正在重置为默认密码")
		return m.createDefaultPassword()
	}
	source := resolveTokenSource(cfg)
	if cfg.TokenSource != source {
		cfg.TokenSource = source
		if err := m.save(cfg); err != nil {
			return err
		}
	}
	m.config = cfg
	if source == TokenSourceConfigured {
		slog.Info(fmt.Sprintf("WebUI 登录密码已加载（用户自定义）: %s...", cfg.AccessToken[:min(4, len(cfg.AccessToken))]))
	} else {
		slog.Info(fmt.Sprintf("WebUI 登录密码已加载: %s...", cfg.AccessToken[:min(4, len(cfg.AccessToken))]))
	}
	return nil
}

// createDefaultPassword 写入固定默认密码（configured 来源，跨重启稳定）。
func (m *TokenManager) createDefaultPassword() error {
	cfg := tokenConfig{
		AccessToken: DefaultWebUIPassword,
		CreatedAt:   time.Now().Format(time.RFC3339),
		UpdatedAt:   time.Now().Format(time.RFC3339),
		TokenSource: TokenSourceConfigured,
	}
	if err := m.save(cfg); err != nil {
		return err
	}
	m.mu.Lock()
	m.config = cfg
	m.mu.Unlock()
	slog.Info(fmt.Sprintf("WebUI 已重置为默认密码: %s（登录后请在 设置→安全 中修改）", DefaultWebUIPassword))
	return nil
}

// resolveTokenSource 判定令牌来源。登录凭据已改为默认密码/用户自定义两种，
// 均为 configured（跨重启稳定）；仅兼容读取旧版数据文件中的 temporary 遗留值。
func resolveTokenSource(cfg tokenConfig) string {
	switch cfg.TokenSource {
	case TokenSourceConfigured:
		return TokenSourceConfigured
	case TokenSourceTemporary:
		return TokenSourceTemporary
	}
	if cfg.AccessToken == "" {
		return TokenSourceConfigured
	}
	// 旧版 64 位 hex 随机 token 视为临时凭据，重启时由 ensureConfig 重置为默认密码。
	if isHex64(cfg.AccessToken) {
		return TokenSourceTemporary
	}
	return TokenSourceConfigured
}

// isHex64 判断是否为 64 位十六进制串（对齐 _validate_token_format）。
func isHex64(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

// createToken 生成新 token 并落盘（对齐 _create_new_token）。
func (m *TokenManager) createToken(source string, preserved tokenConfig) error {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return fmt.Errorf("webui: 生成 token 失败: %w", err)
	}
	now := time.Now().Format(time.RFC3339)
	cfg := tokenConfig{
		AccessToken:         hex.EncodeToString(token),
		CreatedAt:           now,
		UpdatedAt:           now,
		FirstSetupCompleted: preserved.FirstSetupCompleted,
		SetupCompletedAt:    preserved.SetupCompletedAt,
		TokenSource:         source,
	}
	if err := m.save(cfg); err != nil {
		return err
	}
	m.mu.Lock()
	m.config = cfg
	m.mu.Unlock()
	slog.Info(fmt.Sprintf("新的 WebUI Token 已生成: %s...", cfg.AccessToken[:8]))
	return nil
}

// save 原子落盘（对齐 _save_config）。
func (m *TokenManager) save(cfg tokenConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("webui: 保存配置失败: %w", err)
	}
	return os.Rename(tmp, m.path)
}

// logStartupToken 登录密码已固定（默认密码或用户自定义），不再启动打印完整凭据；
// 仅在默认密码未修改时提醒一次。
func (m *TokenManager) logStartupToken() {
	if m.Token() == DefaultWebUIPassword {
		slog.Info(fmt.Sprintf("WebUI 正在使用默认密码 %s，登录后请在 设置→安全 中修改", DefaultWebUIPassword))
	}
}

// Token 返回当前访问令牌。
func (m *TokenManager) Token() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.config.AccessToken
}

// VerifyToken 常数时间校验（对齐 secrets.compare_digest）。
func (m *TokenManager) VerifyToken(token string) bool {
	if token == "" {
		return false
	}
	m.mu.Lock()
	current := m.config.AccessToken
	m.mu.Unlock()
	if current == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(current)) == 1
}

// validateCustomToken 校验新登录密码。
//
// 简化认证：只要求「非空 + 至少 6 位 + 不含空白字符」，不再强制大小写与特殊符号组合。
// 复杂度规则交给使用者自行决定（默认密码 admin123 本身也满足此规则）。
func validateCustomToken(token string) (bool, string) {
	if token == "" {
		return false, "密码不能为空"
	}
	if len(token) < 6 {
		return false, "密码长度至少为 6 位"
	}
	for _, ch := range token {
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			return false, "密码不能包含空格"
		}
	}
	return true, "密码格式正确"
}

// UpdateToken 更新为用户自定义固定 token（对齐 update_token）。
func (m *TokenManager) UpdateToken(newToken string) (bool, string) {
	if valid, msg := validateCustomToken(newToken); !valid {
		slog.Error("Token 格式无效", "reason", msg)
		return false, msg
	}
	m.mu.Lock()
	old := m.config.AccessToken
	m.config.AccessToken = newToken
	m.config.UpdatedAt = time.Now().Format(time.RFC3339)
	m.config.TokenSource = TokenSourceConfigured
	cfg := m.config
	m.mu.Unlock()
	if err := m.save(cfg); err != nil {
		m.mu.Lock()
		m.config.AccessToken = old
		m.mu.Unlock()
		return false, "更新失败: " + err.Error()
	}
	prefix := old
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	// 新密码可能是短密码（简化认证允许 6 位），截断前先判长度防越界。
	suffix := newToken
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	slog.Info(fmt.Sprintf("Token 已更新: %s... -> %s...", prefix, suffix))
	return true, "Token 更新成功"
}

// RegenerateToken 重新生成 token（对齐 regenerate_token；来源置为 configured，
// 与 Python 版语义一致：重新生成后不再按 temporary 每次启动轮换）。
func (m *TokenManager) RegenerateToken() string {
	m.mu.Lock()
	old := m.config.AccessToken
	preserved := m.config
	m.mu.Unlock()
	if err := m.createToken(TokenSourceConfigured, preserved); err != nil {
		slog.Error("重新生成 Token 失败", "err", err)
		return ""
	}
	m.mu.Lock()
	newToken := m.config.AccessToken
	m.mu.Unlock()
	prefix := old
	if prefix == "" {
		prefix = "无"
	} else if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	slog.Info(fmt.Sprintf("WebUI Token 已重新生成: %s... -> %s...", prefix, newToken[:8]))
	return newToken
}

// TokenSource 返回令牌来源。
func (m *TokenManager) TokenSource() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return resolveTokenSource(m.config)
}

// IsFirstSetup 返回首次配置向导是否未完成。
func (m *TokenManager) IsFirstSetup() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.config.FirstSetupCompleted
}

// MarkSetupCompleted 标记首次配置完成（对齐 mark_setup_completed）。
func (m *TokenManager) MarkSetupCompleted() bool {
	m.mu.Lock()
	m.config.FirstSetupCompleted = true
	m.config.SetupCompletedAt = time.Now().Format(time.RFC3339)
	cfg := m.config
	m.mu.Unlock()
	if err := m.save(cfg); err != nil {
		return false
	}
	slog.Info("首次配置已标记为完成")
	return true
}

// ResetSetup 重置首次配置状态（对齐 reset_setup_status）。
func (m *TokenManager) ResetSetup() bool {
	m.mu.Lock()
	m.config.FirstSetupCompleted = false
	m.config.SetupCompletedAt = ""
	cfg := m.config
	m.mu.Unlock()
	if err := m.save(cfg); err != nil {
		return false
	}
	slog.Info("首次配置状态已重置")
	return true
}

// ---------------------------------------------------------------------------
// WS 握手临时 token（对齐 routers/websocket/auth.py）
// ---------------------------------------------------------------------------

// GenerateWSToken 为 WS 握手签发一次性临时令牌（60s 有效期）。
func (m *TokenManager) GenerateWSToken(sessionToken string) string {
	m.cleanupExpiredWSTokens()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	temp := url.QueryEscape(hex.EncodeToString(raw))
	m.mu.Lock()
	m.wsTokens[temp] = wsTempToken{expiresAt: time.Now().Add(wsTokenExpireSeconds * time.Second), sessionToken: sessionToken}
	count := len(m.wsTokens)
	m.mu.Unlock()
	slog.Debug(fmt.Sprintf("生成 WS 临时 token: %s... 有效期 %ds", temp[:8], wsTokenExpireSeconds), "active", count)
	return temp
}

// VerifyWSToken 校验并消费 WS 临时令牌（一次性；同时校验关联 session token）。
func (m *TokenManager) VerifyWSToken(temp string) bool {
	m.cleanupExpiredWSTokens()
	m.mu.Lock()
	entry, ok := m.wsTokens[temp]
	if !ok {
		m.mu.Unlock()
		slog.Warn(fmt.Sprintf("WS token 不存在: %s...", safePrefix(temp)))
		return false
	}
	if time.Now().After(entry.expiresAt) {
		delete(m.wsTokens, temp)
		m.mu.Unlock()
		slog.Warn(fmt.Sprintf("WS token 已过期: %s...", safePrefix(temp)))
		return false
	}
	sessionToken := entry.sessionToken
	m.mu.Unlock()
	// 校验原始 session token 仍然有效（对齐 Python 版二次校验）。
	if !m.VerifyToken(sessionToken) {
		m.mu.Lock()
		delete(m.wsTokens, temp)
		m.mu.Unlock()
		slog.Warn(fmt.Sprintf("WS token 关联的 session 已失效: %s...", safePrefix(temp)))
		return false
	}
	// 一次性消费。
	m.mu.Lock()
	delete(m.wsTokens, temp)
	m.mu.Unlock()
	return true
}

// cleanupExpiredWSTokens 清理过期临时 token。
func (m *TokenManager) cleanupExpiredWSTokens() {
	now := time.Now()
	m.mu.Lock()
	for t, entry := range m.wsTokens {
		if now.After(entry.expiresAt) {
			delete(m.wsTokens, t)
		}
	}
	m.mu.Unlock()
}

// safePrefix 截断 token 前缀用于日志（避免越界）。
func safePrefix(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
