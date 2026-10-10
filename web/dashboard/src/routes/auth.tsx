import { useCallback, useEffect, useRef, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import {
  AlertCircle,
  FileText,
  HelpCircle,
  Key,
  Terminal,
} from 'lucide-react'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

import { checkAuthStatus, getSetupStatus } from '@/lib/auth'
import { setStoredToken } from '@/lib/api-base'
import { authApi } from '@/lib/http'
import { cn } from '@/lib/utils'
import { APP_FULL_NAME } from '@/lib/version'

function resolvePostAuthTarget(): '/' | '/chat/embed' {
  if (typeof window === 'undefined') return '/'
  const redirect = new URLSearchParams(window.location.search).get('redirect')
  // 仅允许回跳站内路径，防开放重定向；外部 URL 一律回首页。
  if (redirect && redirect.startsWith('/') && !redirect.startsWith('//')) {
    return redirect === '/chat/embed' ? '/chat/embed' : '/'
  }
  return '/'
}

export function AuthPage() {
  const [token, setToken] = useState('')
  const [isValidating, setIsValidating] = useState(false)
  const [error, setError] = useState('')
  const [checkingAuth, setCheckingAuth] = useState(true)
  const navigate = useNavigate()
  const { t } = useTranslation()
  // 避免 React StrictMode 下重复触发 URL token 自动登录。
  const urlTokenHandledRef = useRef(false)
  const tokenAutofocusedRef = useRef(false)

  // 从 URL 中提取 token（支持 query 与 hash 两种位置）。
  // 允许 ?token=xxx 、&token=xxx（query）以及 #/foo?token=xxx、#token=xxx（hash）。
  const extractUrlToken = useCallback((): string => {
    if (typeof window === 'undefined') return ''
    const fromQuery = new URLSearchParams(window.location.search).get('token')
    if (fromQuery && fromQuery.trim()) return fromQuery.trim()
    // hash 中可能形如 "#token=xxx" 或 "#/path?token=xxx"
    const rawHash = window.location.hash.replace(/^#/, '')
    if (!rawHash) return ''
    const queryIdx = rawHash.indexOf('?')
    const hashQuery = queryIdx >= 0 ? rawHash.slice(queryIdx + 1) : rawHash
    const fromHash = new URLSearchParams(hashQuery).get('token')
    return fromHash && fromHash.trim() ? fromHash.trim() : ''
  }, [])

  // 从当前 URL 中移除 token 参数，避免令牌被书签/Referer/浏览器历史泄露。
  const stripTokenFromUrl = useCallback(() => {
    if (typeof window === 'undefined') return
    try {
      const url = new URL(window.location.href)
      let changed = false
      if (url.searchParams.has('token')) {
        url.searchParams.delete('token')
        changed = true
      }
      const rawHash = url.hash.replace(/^#/, '')
      if (rawHash) {
        const queryIdx = rawHash.indexOf('?')
        if (queryIdx >= 0) {
          const path = rawHash.slice(0, queryIdx)
          const hashParams = new URLSearchParams(rawHash.slice(queryIdx + 1))
          if (hashParams.has('token')) {
            hashParams.delete('token')
            const next = hashParams.toString()
            url.hash = next ? `#${path}?${next}` : `#${path}`
            changed = true
          }
        } else if (/^token=/.test(rawHash)) {
          url.hash = ''
          changed = true
        }
      }
      if (changed) {
        window.history.replaceState(null, '', url.toString())
      }
    } catch {
      // 忽略 URL 解析异常
    }
  }, [])

  const verifyToken = useCallback(
    async (rawToken: string): Promise<boolean> => {
      const trimmed = rawToken.trim()
      setError('')

      if (!trimmed) {
        setError(t('auth.tokenRequired'))
        return false
      }

      setIsValidating(true)
      try {
        // 向后端发送请求验证 token（Go 后端不下发 Cookie，验证成功后由前端保存
        // access token，后续请求以 Authorization: Bearer 头携带）。
        // 走 authApi：401（token 错误）透传后端信息，不触发整页跳转。
        const data = await authApi.post<{
          valid: boolean
          is_first_setup?: boolean
          requires_custom_token?: boolean
          message?: string
        }>('/api/webui/auth/verify', {
          body: { token: trimmed },
          errorMessage: t('auth.verifyFailed'),
        })

        if (data.valid) {
          // Token 验证成功：保存到本地，后续请求自动携带 Bearer 头
          setStoredToken(trimmed)

          // 再次检查认证状态
          await checkAuthStatus()

          // 直接使用验证响应中的 is_first_setup 字段，避免额外请求。
          // 默认密码登录即可用，只有首次配置未完成才进向导。
          if (data.is_first_setup) {
            navigate({ to: '/setup' })
          } else {
            navigate({ to: resolvePostAuthTarget() })
          }
          return true
        }

        console.error('Token 验证失败:', data.message)
        setError(data.message || t('auth.verifyFailed'))
        return false
      } catch (err) {
        console.error('Token 验证错误:', err)
        setError(err instanceof Error ? err.message : t('auth.connFailed'))
        return false
      } finally {
        setIsValidating(false)
      }
    },
    [navigate, t]
  )

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    await verifyToken(token)
  }

  // 如果已经认证，直接跳转到首页；否则尝试用 URL 中的 token 自动登录。
  useEffect(() => {
    const verifyAuth = async () => {
      try {
        const isAuth = await checkAuthStatus()
        if (isAuth) {
          // 已登录场景下，URL 中残留的 token 也清掉，避免外泄。
          stripTokenFromUrl()
          const setupStatus = await getSetupStatus()
          navigate({
            to:
              setupStatus?.is_first_setup
                ? '/setup'
                : resolvePostAuthTarget(),
          })
          return
        }

        // 未登录：检查 URL 是否带 token，带了就自动尝试登录。
        const urlToken = extractUrlToken()
        if (urlToken && !urlTokenHandledRef.current) {
          urlTokenHandledRef.current = true
          // 立即从 URL 中剥离 token，防止刷新/复制链接时再次暴露。
          stripTokenFromUrl()
          setToken(urlToken)
          // 异步触发验证；失败时错误信息会显示在表单上，token 也会保留在输入框中以便用户修正。
          void verifyToken(urlToken)
        }
      } catch {
        // 忽略错误，保持在登录页
      } finally {
        setCheckingAuth(false)
      }
    }
    verifyAuth()
  }, [navigate, extractUrlToken, stripTokenFromUrl, verifyToken])

  // 正在检查认证状态时显示加载
  if (checkingAuth) {
    return (
      <div data-auth-page="true" className="bg-background flex min-h-screen items-center justify-center p-4">
        <div className="text-muted-foreground">{t('auth.checkingAuth')}</div>
      </div>
    )
  }

  return (
    <div
      data-auth-page="true"
      className="bg-background relative flex min-h-screen items-center justify-center p-4"
    >
      {/* 背景装饰：细网格线 + 右下红色角标（线条风） */}
      <div
        aria-hidden="true"
        className="pointer-events-none absolute inset-0"
        style={{
          backgroundImage:
            'linear-gradient(hsl(var(--border) / 0.5) 1px, transparent 1px), linear-gradient(90deg, hsl(var(--border) / 0.5) 1px, transparent 1px)',
          backgroundSize: '48px 48px',
          maskImage: 'radial-gradient(ellipse 80% 60% at 50% 40%, black 30%, transparent 75%)',
          WebkitMaskImage:
            'radial-gradient(ellipse 80% 60% at 50% 40%, black 30%, transparent 75%)',
        }}
      />
      <div
        aria-hidden="true"
        className="border-primary absolute right-0 bottom-0 h-16 w-16 border-t border-l"
      />

      {/* 登录卡片 - 平面线条风 */}
      <div className="bg-card relative z-10 w-full max-w-md border shadow-sm">
        {/* 顶部红色标记条 */}
        <div className="bg-primary h-0.5 w-full" aria-hidden="true" />

        <div className="space-y-1 px-6 pt-6 pb-2 text-left">
          <div className="mb-4 flex items-center gap-2.5">
            <span
              aria-hidden="true"
              className="bg-foreground relative flex h-9 w-9 items-center justify-center"
            >
              <svg viewBox="0 0 24 24" className="h-5 w-5" fill="none" aria-hidden="true">
                <path
                  d="M5 18V6l7 7 7-7v12"
                  stroke="hsl(var(--background))"
                  strokeWidth="2.4"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              </svg>
              <span className="bg-primary absolute -right-0.5 -bottom-0.5 h-1.5 w-1.5" />
            </span>
            <span className="text-foreground text-base font-bold tracking-tight">
              oh-my-qqbot
            </span>
          </div>
          <h1 className="text-foreground text-2xl font-bold">{t('auth.welcome')}</h1>
          <p className="text-muted-foreground flex items-center gap-1.5 text-sm">
            <span aria-hidden="true" className="text-primary font-mono">
              &gt;
            </span>
            {t('auth.accessDesc')}
          </p>
        </div>

        <div className="px-6 pb-6">
          <form onSubmit={handleSubmit} className="space-y-4">
            {/* Token 输入框 */}
            <div className="space-y-2">
              <div className="relative">
                <Key
                  className="text-muted-foreground absolute top-1/2 left-3 h-4 w-4 -translate-y-1/2"
                  strokeWidth={2}
                  fill="none"
                />
                <Input
                  id="token"
                  aria-label="Access Token"
                  type="password"
                  placeholder={t('auth.tokenPlaceholder')}
                  value={token}
                  onChange={(e) => setToken(e.target.value)}
                  className={cn('pl-10', error && 'border-destructive focus-visible:ring-destructive')}
                  disabled={isValidating}
                  ref={(element) => {
                    if (element && !tokenAutofocusedRef.current) {
                      tokenAutofocusedRef.current = true
                      element.focus()
                    }
                  }}
                  autoComplete="off"
                  aria-invalid={error ? true : undefined}
                  aria-describedby={error ? 'token-error' : undefined}
                />
              </div>
            </div>

            {/* 错误提示 */}
            {error && (
              <div
                id="token-error"
                role="alert"
                className="text-destructive bg-destructive/10 border-destructive/30 flex items-center gap-2 border p-3 text-sm"
              >
                <AlertCircle className="h-4 w-4 shrink-0" strokeWidth={2} fill="none" />
                <span>{error}</span>
              </div>
            )}

            {/* 提交按钮 */}
            <Button type="submit" className="w-full" disabled={isValidating}>
              {isValidating ? (
                <>
                  <div className="mr-2 h-4 w-4 animate-spin rounded-full border-2 border-current border-t-transparent" />
                  {t('auth.verifyingLabel')}
                </>
              ) : (
                t('auth.verifyEnter')
              )}
            </Button>

            {/* 帮助文本 — 纯 CSS 折叠（禁用 Radix Dialog：其关闭路径可能残留
                body pointer-events:none，导致登录按钮整体无响应） */}
            <details data-auth-help="true" className="group">
              <summary className="text-primary hover:text-primary/80 flex w-full cursor-pointer list-none items-center justify-start gap-1 text-left text-[11px] transition-colors underline-offset-4 hover:underline [&::-webkit-details-marker]:hidden">
                <HelpCircle className="h-3 w-3" strokeWidth={2} fill="none" />
                {t('auth.helpLink')}
              </summary>
              <div className="border-border mt-3 space-y-3 border-t pt-3">
                {/* 方式一：查看控制台 */}
                <div className="bg-muted/50 space-y-2 border p-3">
                  <div className="flex items-start gap-2.5">
                    <Terminal className="text-primary mt-0.5 h-4 w-4 shrink-0" strokeWidth={2} fill="none" />
                    <div className="flex-1 space-y-1.5">
                      <h4 className="text-sm font-semibold">{t('auth.method1Title')}</h4>
                      <p className="text-muted-foreground text-xs">{t('auth.method1Desc')}</p>
                      <div className="bg-background font-mono text-[11px] p-2">
                        <code className="text-primary">admin123</code>
                      </div>
                    </div>
                  </div>
                </div>

                {/* 方式二：查看配置文件 */}
                <div className="bg-muted/50 space-y-2 border p-3">
                  <div className="flex items-start gap-2.5">
                    <FileText className="text-primary mt-0.5 h-4 w-4 shrink-0" strokeWidth={2} fill="none" />
                    <div className="flex-1 space-y-1.5">
                      <h4 className="text-sm font-semibold">{t('auth.method2Title')}</h4>
                      <p className="text-muted-foreground text-xs">{t('auth.method2Desc')}</p>
                      <div className="bg-background font-mono text-[11px] break-all p-2">
                        <code className="text-primary">data/webui.json</code>
                      </div>
                      <p className="text-muted-foreground text-[11px]">
                        {t('auth.method2FileHint')}{' '}
                        <code className="bg-background px-1 py-0.5">access_token</code>
                      </p>
                    </div>
                  </div>
                </div>

                {/* 安全提示 */}
                <div className="text-muted-foreground text-[11px]">
                  <ul className="list-inside list-disc space-y-1">
                    <li>{t('auth.securityTip1')}</li>
                    <li>{t('auth.securityTip2')}</li>
                  </ul>
                </div>
              </div>
            </details>
          </form>
        </div>
      </div>

      {/* 页脚信息 */}
      <div className="text-muted-foreground absolute right-0 bottom-4 left-0 z-10 text-center text-xs">
        <p>{APP_FULL_NAME}</p>
      </div>
    </div>
  )
}
