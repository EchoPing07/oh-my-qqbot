// 登录限速器 — S1 修复：auth/verify 此前可无限尝试，公网部署下可被暴力
// 破解默认密码。对齐 MaiBot src/webui/core/rate_limiter.py 的策略：
//   - 300s 滑动窗口内累计 5 次登录失败 → 封禁 600s（挂载点对齐 routes.py:184
//     的 verify 路由限速）；
//   - per-IP + 全局双计数：防单 IP 轮换/伪造源地址绕过 per-IP 限制；
//   - 纯内存实现（进程重启清零），mutex 保护，访问时惰性清理过期项。
package routers

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// 限速参数（对齐 rate_limiter.py 的默认值）。
const (
	// loginMaxFailures 对齐 max_failures = 5。
	loginMaxFailures = 5
	// loginWindowDuration 对齐 window_seconds = 300（滑动窗口）。
	loginWindowDuration = 300 * time.Second
	// loginBanDuration 对齐 ban_duration = 600。
	loginBanDuration = 600 * time.Second
	// loginPruneInterval 是全表惰性清理的最小间隔（控制 map 增长）。
	loginPruneInterval = time.Minute
)

// loginRateLimiter 是内存登录限速器。
type loginRateLimiter struct {
	mu sync.Mutex

	// now 可注入时钟（测试用）。
	now func() time.Time

	// failures 记录每个 IP 的失败时间戳（滑动窗口）。
	failures map[string][]time.Time
	// bannedUntil 记录每个 IP 的封禁截止时间。
	bannedUntil map[string]time.Time
	// global / globalBan 是全局失败窗口与封禁截止（防 IP 轮换爆破）。
	// 全局窗口按 IP 记账：某 IP 登录成功时其失败从全局窗口移除
	// （成功即证明合法，不应继续累计为全局风险）。
	global    []loginFailure
	globalBan time.Time

	lastPrune time.Time
}

// loginFailure 是全局窗口的一条失败记录（带来源 IP，成功登录时回撤）。
type loginFailure struct {
	at time.Time
	ip string
}

// newLoginRateLimiter 创建限速器。
func newLoginRateLimiter() *loginRateLimiter {
	return &loginRateLimiter{
		now:         time.Now,
		failures:    map[string][]time.Time{},
		bannedUntil: map[string]time.Time{},
	}
}

// clientIP 提取请求来源 IP（直连场景取 RemoteAddr；经反代时所有客户端共享
// 代理 IP，由全局窗口兜底）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Allowed 报告该 IP 当前是否允许尝试登录；被拒时返回剩余等待时长。
func (l *loginRateLimiter) Allowed(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.pruneLocked(now)

	// per-IP 封禁优先。
	if until, banned := l.bannedUntil[ip]; banned && now.Before(until) {
		return false, until.Sub(now)
	}
	// 全局封禁。
	if now.Before(l.globalBan) {
		return false, l.globalBan.Sub(now)
	}
	// per-IP 全局封禁刚过期：顺带清掉旧窗口，从零计数。
	if _, banned := l.bannedUntil[ip]; banned {
		delete(l.bannedUntil, ip)
		delete(l.failures, ip)
	}
	if !l.globalBan.IsZero() && !now.Before(l.globalBan) {
		l.globalBan = time.Time{}
		l.global = nil
	}
	return true, 0
}

// RecordFailure 记录一次登录失败；窗口内失败达到阈值时触发封禁
// （对齐 rate_limiter.py 的 ban 语义）。
func (l *loginRateLimiter) RecordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-loginWindowDuration)

	l.failures[ip] = append(trimWindow(l.failures[ip], cutoff), now)
	l.global = trimGlobal(l.global, cutoff)
	l.global = append(l.global, loginFailure{at: now, ip: ip})

	// per-IP 达到阈值 → 封禁该 IP。
	if len(l.failures[ip]) >= loginMaxFailures {
		l.bannedUntil[ip] = now.Add(loginBanDuration)
	}
	// 全局达到阈值 → 封禁全体（登录入口；不影响已登录会话的 Bearer 校验）。
	if len(l.global) >= loginMaxFailures {
		l.globalBan = now.Add(loginBanDuration)
	}
}

// Reset 登录成功后清空该 IP 的失败窗口（per-IP 与全局窗口同步回撤，
// 对齐 rate_limiter.py 成功即重置语义）。
func (l *loginRateLimiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
	kept := l.global[:0]
	for _, f := range l.global {
		if f.ip != ip {
			kept = append(kept, f)
		}
	}
	l.global = kept
}

// BanRemaining 返回该 IP 的剩余封禁时长（0 = 未封禁；诊断/测试用）。
func (l *loginRateLimiter) BanRemaining(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if until, banned := l.bannedUntil[ip]; banned && now.Before(until) {
		return until.Sub(now)
	}
	return 0
}

// pruneLocked 惰性清理：丢弃窗口已空且未封禁的 IP，防 map 无界增长。
func (l *loginRateLimiter) pruneLocked(now time.Time) {
	if !l.lastPrune.IsZero() && now.Sub(l.lastPrune) < loginPruneInterval {
		return
	}
	l.lastPrune = now
	cutoff := now.Add(-loginWindowDuration)
	for ip, times := range l.failures {
		if until, banned := l.bannedUntil[ip]; banned && now.Before(until) {
			continue
		}
		trimmed := trimWindow(times, cutoff)
		if len(trimmed) == 0 {
			delete(l.failures, ip)
			delete(l.bannedUntil, ip)
			continue
		}
		l.failures[ip] = trimmed
	}
	l.global = trimGlobal(l.global, cutoff)
}

// trimWindow 保留 cutoff 之后的失败时间戳。
func trimWindow(times []time.Time, cutoff time.Time) []time.Time {
	out := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

// trimGlobal 保留 cutoff 之后的全局失败记录。
func trimGlobal(records []loginFailure, cutoff time.Time) []loginFailure {
	out := records[:0]
	for _, f := range records {
		if f.at.After(cutoff) {
			out = append(out, f)
		}
	}
	return out
}
