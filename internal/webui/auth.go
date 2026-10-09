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

// ensureConfig 对齐 _ensure_config：文件缺失/损坏/缺 token 时重建；
// temporary 来源每次启动重新生成。
func (m *TokenManager) ensureConfig() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return fmt.Errorf("webui: 创建数据目录失败: %w", err)
	}
	data, err := os.ReadFile(m.path)
	switch {
	case os.IsNotExist(err):
		slog.Info("WebUI 配置文件不存在，正在创建", "path", m.path)
		return m.createToken(TokenSourceTemporary, tokenConfig{})
	case err != nil:
		slog.Error("读取 WebUI 配置文件失败，正在重新创建", "err", err)
		return m.createToken(TokenSourceTemporary, tokenConfig{})
	}
	var cfg tokenConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Error("解析 WebUI 配置失败，正在重新创建", "err", err)
		return m.createToken(TokenSourceTemporary, tokenConfig{})
	}
	if cfg.AccessToken == "" {
		slog.Warn("WebUI 配置文件中缺少 access_token，正在重新生成")
		return m.createToken(TokenSourceTemporary, cfg)
	}
	source := resolveTokenSource(cfg)
	if cfg.TokenSource != source {
		cfg.TokenSource = source
		if err := m.save(cfg); err != nil {
			return err
		}
	}
	if source == TokenSourceTemporary {
		slog.Info("WebUI 尚未配置固定 Token，正在重新生成本次启动临时 Token")
		return m.createToken(TokenSourceTemporary, cfg)
	}
	m.config = cfg
	slog.Info(fmt.Sprintf("WebUI Token 已加载: %s...", cfg.AccessToken[:8]))
	return nil
}

// resolveTokenSource 对齐 _resolve_token_source。
func resolveTokenSource(cfg tokenConfig) string {
	switch cfg.TokenSource {
	case TokenSourceTemporary, TokenSourceConfigured:
		return cfg.TokenSource
	}
	if cfg.AccessToken == "" {
		return TokenSourceTemporary
	}
	// 64 位 hex 是系统生成的临时 token；其余视为用户配置。
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

// logStartupToken 对齐 show_access_token：temporary 来源启动打印完整 token 一次。
func (m *TokenManager) logStartupToken() {
	if m.TokenSource() == TokenSourceTemporary {
		slog.Info(fmt.Sprintf("WebUI Access Token: %s", m.Token()))
		slog.Info("请使用该 Token 登录 WebUI（配置固定 Token 后将不再打印）")
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

// validateCustomToken 对齐 _validate_custom_token：≥10 位且含大小写字母与特殊符号。
func validateCustomToken(token string) (bool, string) {
	if token == "" {
		return false, "Token 不能为空"
	}
	if len(token) < 10 {
		return false, "Token 长度至少为 10 位"
	}
	var hasUpper, hasLower, hasSpecial bool
	const specials = "!@#$%^&*()_+-=[]{}|;:,.<>?/"
	for _, ch := range token {
		switch {
		case ch >= 'A' && ch <= 'Z':
			hasUpper = true
		case ch >= 'a' && ch <= 'z':
			hasLower = true
		case stringsContainsByte(specials, byte(ch)):
			hasSpecial = true
		}
	}
	if !hasUpper {
		return false, "Token 必须包含大写字母"
	}
	if !hasLower {
		return false, "Token 必须包含小写字母"
	}
	if !hasSpecial {
		return false, "Token 必须包含特殊符号 (" + specials + ")"
	}
	return true, "Token 格式正确"
}

func stringsContainsByte(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
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
	slog.Info(fmt.Sprintf("Token 已更新: %s... -> %s...", prefix, newToken[:8]))
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
