/**
 * 登录页 — 密码即 access token（默认 admin123）。
 *
 * 流程：verifyToken 成功 → 存 token → is_first_setup 时进向导 → 否则回跳 redirect 目标。
 * 已登录访问 /auth 时自动跳走；URL 带 ?token= 时自动登录（用后即从地址栏剥离防泄露）。
 */

import { useCallback, useEffect, useRef, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { CircleAlert, KeyRound } from 'lucide-react'

import { checkAuth, getSetupStatus, storeLoginToken, verifyToken } from '@/api/auth'
import { postAuthTarget } from '@/router'
import { APP_NAME } from '@/lib/version'
import { cn } from '@/lib/cn'

function extractUrlToken(): string {
  const fromQuery = new URLSearchParams(window.location.search).get('token')
  if (fromQuery?.trim()) return fromQuery.trim()
  const rawHash = window.location.hash.replace(/^#/, '')
  if (!rawHash) return ''
  const queryIdx = rawHash.indexOf('?')
  const hashQuery = queryIdx >= 0 ? rawHash.slice(queryIdx + 1) : rawHash
  const fromHash = new URLSearchParams(hashQuery).get('token')
  return fromHash?.trim() ? fromHash.trim() : ''
}

function stripTokenFromUrl(): void {
  try {
    const url = new URL(window.location.href)
    let changed = false
    if (url.searchParams.has('token')) {
      url.searchParams.delete('token')
      changed = true
    }
    if (changed) window.history.replaceState(null, '', url.toString())
  } catch {
    // 忽略 URL 解析异常
  }
}

export function AuthPage() {
  const navigate = useNavigate()
  const [password, setPassword] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')
  const [checking, setChecking] = useState(true)
  const urlTokenHandledRef = useRef(false)
  const inputRef = useRef<HTMLInputElement>(null)

  const doLogin = useCallback(
    async (rawToken: string): Promise<boolean> => {
      const token = rawToken.trim()
      setError('')
      if (!token) {
        setError('请输入登录密码')
        return false
      }
      setSubmitting(true)
      try {
        const result = await verifyToken(token)
        if (!result.valid) {
          setError(result.message || '密码错误')
          return false
        }
        storeLoginToken(token)
        if (result.is_first_setup) {
          navigate({ to: '/setup' })
        } else {
          navigate({ to: postAuthTarget() })
        }
        return true
      } catch (err) {
        setError(err instanceof Error ? err.message : '连接失败，请确认后端已启动')
        return false
      } finally {
        setSubmitting(false)
      }
    },
    [navigate]
  )

  // 进入页面：已登录直接跳走；否则尝试 URL token 自动登录
  useEffect(() => {
    let cancelled = false
    const run = async () => {
      try {
        if (await checkAuth()) {
          if (!cancelled) {
            stripTokenFromUrl()
            const setup = await getSetupStatus()
            navigate({ to: setup.is_first_setup ? '/setup' : postAuthTarget() })
          }
          return
        }
        const urlToken = extractUrlToken()
        if (urlToken && !urlTokenHandledRef.current) {
          urlTokenHandledRef.current = true
          stripTokenFromUrl()
          setPassword(urlToken)
          await doLogin(urlToken)
        }
      } catch {
        // 保持登录页
      } finally {
        if (!cancelled) setChecking(false)
      }
    }
    void run()
    return () => {
      cancelled = true
    }
  }, [navigate, doLogin])

  // 未登录且无自动登录时聚焦输入框
  useEffect(() => {
    if (!checking) inputRef.current?.focus()
  }, [checking])

  if (checking) {
    return (
      <div className="login-page">
        <div className="loading-block">
          <span className="spin" /> 正在检查登录状态…
        </div>
      </div>
    )
  }

  return (
    <div className="login-page">
      <div className="login-card">
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span className="logo-mark" aria-hidden="true">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round">
              <path d="M5 18V6l7 7 7-7v12" />
            </svg>
          </span>
          <span style={{ fontWeight: 700, fontSize: 'var(--fs-md)' }}>{APP_NAME}</span>
        </div>
        <div>
          <h1 className="login-title">登录</h1>
          <p className="login-sub">输入管理密码进入控制台</p>
        </div>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            void doLogin(password)
          }}
          style={{ display: 'flex', flexDirection: 'column', gap: 12 }}
        >
          <div>
            <label className="label" htmlFor="password">管理密码</label>
            <div style={{ position: 'relative' }}>
              <KeyRound
                size={15}
                style={{ position: 'absolute', left: 10, top: '50%', transform: 'translateY(-50%)', color: 'var(--muted-fg)' }}
              />
              <input
                id="password"
                ref={inputRef}
                type="password"
                className={cn('input', error && 'input-err')}
                style={{ paddingLeft: 32 }}
                placeholder="••••••••"
                autoComplete="current-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                disabled={submitting}
                aria-invalid={Boolean(error)}
                aria-describedby={error ? 'auth-error' : undefined}
              />
            </div>
            {error ? (
              <p id="auth-error" role="alert" className="field-hint" style={{ color: 'var(--err-fg)', display: 'flex', alignItems: 'center', gap: 4 }}>
                <CircleAlert size={13} /> {error}
              </p>
            ) : null}
          </div>
          <button type="submit" className="btn btn-lg btn-block" disabled={submitting}>
            {submitting ? (
              <>
                <span className="spin" /> 登录中…
              </>
            ) : (
              '登 录'
            )}
          </button>
        </form>
        <details className="disclosure">
          <summary>没有密码？</summary>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8, marginTop: 6 }}>
            <div className="code-block">
              默认密码：<code>admin123</code>
              <br />
              登录后请在 设置 → 安全 中修改。
            </div>
            <div className="code-block">
              忘记密码：删除 <code>data/webui.json</code> 后重启主程序即可重置为默认密码。
            </div>
            <p className="muted-xs">公网部署请务必修改默认密码，并通过反向代理 + HTTPS 访问。</p>
          </div>
        </details>
      </div>
      <div className="login-foot" style={{ position: 'fixed', bottom: 14, left: 0, right: 0 }}>
        {APP_NAME}
      </div>
    </div>
  )
}
