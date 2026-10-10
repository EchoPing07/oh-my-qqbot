// TokenManager 测试 — 生成/读取/校验/更新/重生成/过期 + WS 临时 token 一次性语义。
package webui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestTokenManager(t *testing.T) *TokenManager {
	t.Helper()
	tm, err := NewTokenManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewTokenManager: %v", err)
	}
	return tm
}

func TestTokenManagerGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	tm, err := NewTokenManager(dir)
	if err != nil {
		t.Fatalf("NewTokenManager: %v", err)
	}
	// 首次启动写入固定默认密码（configured 来源），跨重启稳定。
	token := tm.Token()
	if token != DefaultWebUIPassword {
		t.Fatalf("默认密码应为 %q, got %q", DefaultWebUIPassword, token)
	}
	// 文件存在且包含密码。
	data, err := os.ReadFile(filepath.Join(dir, "webui.json"))
	if err != nil {
		t.Fatalf("token 文件未创建: %v", err)
	}
	var cfg tokenConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("token 文件不是有效 JSON: %v", err)
	}
	if cfg.AccessToken != token {
		t.Fatalf("文件中的 token 与内存不一致")
	}
	if cfg.TokenSource != TokenSourceConfigured {
		t.Fatalf("默认密码来源应为 configured, got %q", cfg.TokenSource)
	}
}

// 默认密码跨重启保持稳定（不再按次启动轮换）。
func TestTokenManagerStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	first, err := NewTokenManager(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	token1 := first.Token()
	second, err := NewTokenManager(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	token2 := second.Token()
	if token1 != token2 {
		t.Fatalf("默认密码应跨重启稳定, got %q vs %q", token1, token2)
	}
	if !second.VerifyToken(token2) {
		t.Fatalf("默认密码应可登录")
	}
}

// 损坏的 token 文件在重启时重置为默认密码。
func TestTokenManagerCorruptedFileResetsToDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "webui.json"), []byte("{invalid"), 0o600); err != nil {
		t.Fatalf("写坏文件: %v", err)
	}
	tm, err := NewTokenManager(dir)
	if err != nil {
		t.Fatalf("NewTokenManager: %v", err)
	}
	if tm.Token() != DefaultWebUIPassword {
		t.Fatalf("损坏文件应重置为默认密码, got %q", tm.Token())
	}
}

// configured（固定 token）来源重启保持不变。
func TestTokenManagerConfiguredPersists(t *testing.T) {
	dir := t.TempDir()
	tm, err := NewTokenManager(dir)
	if err != nil {
		t.Fatalf("NewTokenManager: %v", err)
	}
	ok, msg := tm.UpdateToken("My#Fixed2024Token")
	if !ok {
		t.Fatalf("UpdateToken 失败: %s", msg)
	}
	fixed := tm.Token()
	if tm.TokenSource() != TokenSourceConfigured {
		t.Fatalf("来源应为 configured, got %q", tm.TokenSource())
	}
	again, err := NewTokenManager(dir)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if again.Token() != fixed {
		t.Fatalf("configured token 重启后应保持不变")
	}
}

func TestTokenManagerVerify(t *testing.T) {
	tm := newTestTokenManager(t)
	if !tm.VerifyToken(tm.Token()) {
		t.Fatalf("正确 token 应通过")
	}
	if tm.VerifyToken("") || tm.VerifyToken(strings.Repeat("0", 64)) {
		t.Fatalf("错误/空 token 应拒绝")
	}
}

func TestTokenManagerUpdateValidation(t *testing.T) {
	tm := newTestTokenManager(t)
	cases := []struct {
		token string
		want  bool
	}{
		{"", false},
		{"short", false},   // 长度不足 6 位
		{"a b c d", false}, // 含空格
		{"admin123", true}, // 简单密码（简化认证允许）
		{"Good#Token123", true},
	}
	for _, c := range cases {
		ok, _ := tm.UpdateToken(c.token)
		if ok != c.want {
			t.Fatalf("UpdateToken(%q) = %v, want %v", c.token, ok, c.want)
		}
	}
}

func TestTokenManagerRegenerate(t *testing.T) {
	tm := newTestTokenManager(t)
	old := tm.Token()
	tm.MarkSetupCompleted()
	if tm.IsFirstSetup() {
		t.Fatalf("MarkSetupCompleted 后 IsFirstSetup 应为 false")
	}
	newToken := tm.RegenerateToken()
	if newToken == "" || newToken == old {
		t.Fatalf("RegenerateToken 应返回新的非空 token")
	}
	if !tm.VerifyToken(newToken) || tm.VerifyToken(old) {
		t.Fatalf("重生成后旧 token 应失效")
	}
	if tm.IsFirstSetup() {
		t.Fatalf("重生成应保留 first_setup_completed 状态")
	}
	// 重生成后 reset 回首启状态。
	if !tm.ResetSetup() || !tm.IsFirstSetup() {
		t.Fatalf("ResetSetup 应回到未完成状态")
	}
}

// WS 临时 token：有效期内一次性消费。
func TestWSTokenOneShot(t *testing.T) {
	tm := newTestTokenManager(t)
	session := tm.Token()
	wsToken := tm.GenerateWSToken(session)
	if wsToken == "" {
		t.Fatalf("GenerateWSToken 不应返回空")
	}
	if !tm.VerifyWSToken(wsToken) {
		t.Fatalf("首次校验应通过")
	}
	if tm.VerifyWSToken(wsToken) {
		t.Fatalf("WS 临时 token 应一次性消费")
	}
}

// WS 临时 token 绑定 session token：session 失效后临时 token 拒绝。
func TestWSTokenBoundToSession(t *testing.T) {
	tm := newTestTokenManager(t)
	session := tm.Token()
	wsToken := tm.GenerateWSToken(session)
	// session 变化（模拟重新生成）后临时 token 拒绝。
	tm.RegenerateToken()
	if tm.VerifyWSToken(wsToken) {
		t.Fatalf("session 失效后 WS 临时 token 应拒绝")
	}
}

// WS 临时 token 与 session 不匹配时拒绝（错误 session 签发）。
func TestWSTokenRejectsBadSession(t *testing.T) {
	tm := newTestTokenManager(t)
	wsToken := tm.GenerateWSToken("not-a-valid-session-token")
	if tm.VerifyWSToken(wsToken) {
		t.Fatalf("无效 session 签发的临时 token 应拒绝")
	}
}

// 简化认证：默认密码本身即可通过校验（不再要求大小写 + 特殊符号组合）。
func TestValidateCustomTokenSimplePolicy(t *testing.T) {
	cases := []struct {
		token string
		want  bool
	}{
		{DefaultWebUIPassword, true}, // admin123
		{"123456", true},             // 6 位纯数字即可
		{"abc", false},               // 不足 6 位
		{"pass word", false},         // 含空格
		{"密码密码密码", true},             // 中文密码（按字节长度 ≥6）
	}
	for _, c := range cases {
		valid, msg := validateCustomToken(c.token)
		if valid != c.want {
			t.Fatalf("validateCustomToken(%q) = %v (%s), want %v", c.token, valid, msg, c.want)
		}
	}
}

// 短密码（简化认证允许 6 位）不得让 UpdateToken 在日志截断处越界 panic。
func TestUpdateTokenShortPasswordNoPanic(t *testing.T) {
	tm := newTestTokenManager(t)
	ok, msg := tm.UpdateToken("abc123")
	if !ok {
		t.Fatalf("6 位简单密码应允许: %s", msg)
	}
	if !tm.VerifyToken("abc123") {
		t.Fatal("新密码应可登录")
	}
}
