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
	token := tm.Token()
	if len(token) != 64 {
		t.Fatalf("token 长度应为 64 hex, got %d: %q", len(token), token)
	}
	// 文件存在且包含 token。
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
	if cfg.TokenSource != TokenSourceTemporary {
		t.Fatalf("默认来源应为 temporary, got %q", cfg.TokenSource)
	}
}

// temporary 来源每次启动重新生成（对齐 _ensure_config 语义）。
func TestTokenManagerTemporaryRegeneratedEachStart(t *testing.T) {
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
	if token1 == token2 {
		t.Fatalf("temporary token 应每次启动重新生成")
	}
	if !second.VerifyToken(token2) || second.VerifyToken(token1) {
		t.Fatalf("旧 token 应失效")
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
		{"short", false},
		{"alllowercase#123", false}, // 缺大写
		{"ALLUPPERCASE#123", false}, // 缺小写
		{"NoSpecial123Abc", false},  // 缺特殊符号
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
