// S1 登录限速测试 — 单元级（注入时钟）+ 路由级（verify 连续失败被封禁）。
package routers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLoginRateLimiterBansAfterFiveFailures 对齐 rate_limiter.py：
// 300s 窗口内第 5 次失败触发 600s 封禁。
func TestLoginRateLimiterBansAfterFiveFailures(t *testing.T) {
	l := newLoginRateLimiter()
	base := time.Unix(1700000000, 0)
	now := base
	l.now = func() time.Time { return now }

	if allowed, _ := l.Allowed("1.2.3.4"); !allowed {
		t.Fatalf("初始状态应允许登录")
	}
	for i := 0; i < loginMaxFailures-1; i++ {
		l.RecordFailure("1.2.3.4")
		if allowed, _ := l.Allowed("1.2.3.4"); !allowed {
			t.Fatalf("第 %d 次失败后仍应允许", i+1)
		}
	}
	l.RecordFailure("1.2.3.4")
	if allowed, wait := l.Allowed("1.2.3.4"); allowed {
		t.Fatalf("第 5 次失败后应封禁")
	} else if wait <= 0 || wait > loginBanDuration {
		t.Fatalf("封禁剩余时长异常: %v", wait)
	}

	// 封禁期内推进 600s → 解封，可重新尝试。
	now = base.Add(loginBanDuration + time.Second)
	if allowed, _ := l.Allowed("1.2.3.4"); !allowed {
		t.Fatalf("封禁过期后应恢复允许")
	}
}

// TestLoginRateLimiterWindowExpiry 滑动窗口过期后失败计数清零。
func TestLoginRateLimiterWindowExpiry(t *testing.T) {
	l := newLoginRateLimiter()
	base := time.Unix(1700000000, 0)
	now := base
	l.now = func() time.Time { return now }

	for i := 0; i < loginMaxFailures-1; i++ {
		l.RecordFailure("5.6.7.8")
	}
	// 推进到窗口外：旧失败全部过期，未封禁状态下重新计数。
	now = base.Add(loginWindowDuration + time.Second)
	if allowed, _ := l.Allowed("5.6.7.8"); !allowed {
		t.Fatalf("窗口过期后应恢复允许")
	}
	// 再失败 1 次仍不应封禁（旧失败已滑出窗口）。
	l.RecordFailure("5.6.7.8")
	if allowed, _ := l.Allowed("5.6.7.8"); !allowed {
		t.Fatalf("过期失败不应计入新窗口")
	}
}

// TestLoginRateLimiterGlobalWindow 全局窗口：不同 IP 轮换爆破也会触发封禁。
func TestLoginRateLimiterGlobalWindow(t *testing.T) {
	l := newLoginRateLimiter()
	for i := 0; i < loginMaxFailures; i++ {
		ip := "10.0.0." + itoa(i+1)
		if allowed, _ := l.Allowed(ip); !allowed {
			t.Fatalf("不同 IP 未达 per-IP 阈值前应允许: %s", ip)
		}
		l.RecordFailure(ip)
	}
	// 第 5 个 IP 触发全局封禁后，新 IP 也被拒。
	if allowed, wait := l.Allowed("10.0.0.99"); allowed {
		t.Fatalf("全局封禁后应拒绝新 IP")
	} else if wait <= 0 {
		t.Fatalf("全局封禁剩余时长异常")
	}
}

// TestLoginRateLimiterResetOnSuccess 登录成功重置该 IP 失败窗口。
func TestLoginRateLimiterResetOnSuccess(t *testing.T) {
	l := newLoginRateLimiter()
	for i := 0; i < loginMaxFailures-2; i++ {
		l.RecordFailure("7.7.7.7")
	}
	l.Reset("7.7.7.7")
	for i := 0; i < loginMaxFailures-2; i++ {
		l.RecordFailure("7.7.7.7")
	}
	if allowed, _ := l.Allowed("7.7.7.7"); !allowed {
		t.Fatalf("成功重置后累计 %d 次失败不应封禁", loginMaxFailures-2)
	}
}

// TestAuthVerifyRateLimited 路由级：verify 连续 5 次失败后返回 429。
func TestAuthVerifyRateLimited(t *testing.T) {
	env := newTestEnv(t, nil)
	verify := func(token string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]string{"token": token})
		req := httptest.NewRequest("POST", "/api/webui/auth/verify", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// 前 loginMaxFailures-1 次失败：200 + valid=false。
	for i := 0; i < loginMaxFailures-1; i++ {
		code, out := verify("wrong-token")
		if code != http.StatusOK {
			t.Fatalf("第 %d 次失败应 200, got %d", i+1, code)
		}
		if out["valid"] == true {
			t.Fatalf("错误 token 不应验证通过")
		}
	}
	// 第 loginMaxFailures 次：记录失败即触发封禁 → 429。
	code, _ := verify("wrong-token")
	if code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次失败应 429, got %d", loginMaxFailures, code)
	}
	// 封禁期内即使密码正确也 429。
	code, _ = verify(env.token)
	if code != http.StatusTooManyRequests {
		t.Fatalf("封禁期内正确密码也应 429, got %d", code)
	}
}

// TestAuthVerifySuccessResetsFailures 成功登录后失败窗口清零，不误伤后续尝试。
func TestAuthVerifySuccessResetsFailures(t *testing.T) {
	env := newTestEnv(t, nil)
	verify := func(token string) int {
		body, _ := json.Marshal(map[string]string{"token": token})
		req := httptest.NewRequest("POST", "/api/webui/auth/verify", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < loginMaxFailures-2; i++ {
		if code := verify("wrong"); code != http.StatusOK {
			t.Fatalf("失败尝试应 200, got %d", code)
		}
	}
	if code := verify(env.token); code != http.StatusOK {
		t.Fatalf("正确 token 应 200, got %d", code)
	}
	// 成功重置后再失败 loginMaxFailures-2 次仍不应封禁。
	for i := 0; i < loginMaxFailures-2; i++ {
		if code := verify("wrong"); code != http.StatusOK {
			t.Fatalf("重置后累计失败不应触发 429, got %d", code)
		}
	}
}
