/**
 * Access token 存取与 401 广播。
 *
 * Go 后端不走 Cookie：登录成功后把 access token 存 localStorage，
 * 之后所有请求经 Authorization: Bearer 头携带（见 lib/http.ts）。
 */

const TOKEN_KEY = 'ohmyqqbot-webui-token'

type Listener = () => void
const unauthorizedListeners = new Set<Listener>()

export function onUnauthorized(listener: Listener): () => void {
  unauthorizedListeners.add(listener)
  return () => {
    unauthorizedListeners.delete(listener)
  }
}

/** 广播 401（http 客户端在收到 401 时调用；页面可订阅以清理状态）。 */
export function fireUnauthorized(): void {
  for (const listener of unauthorizedListeners) {
    try {
      listener()
    } catch (error) {
      console.error('401 处理器执行失败:', error)
    }
  }
}

export function getStoredToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? ''
  } catch {
    return ''
  }
}

export function setStoredToken(token: string): void {
  try {
    if (token) {
      localStorage.setItem(TOKEN_KEY, token)
    } else {
      localStorage.removeItem(TOKEN_KEY)
    }
  } catch {
    // 隐私模式等场景下静默忽略
  }
}

/** 登出：清 token + 广播 401 + 跳登录页（带 redirect 回跳参数）。 */
export function logoutLocal(): void {
  setStoredToken('')
  fireUnauthorized()
  const path = window.location.pathname
  const redirect = path && path !== '/auth' ? `?redirect=${encodeURIComponent(path)}` : ''
  window.location.href = `/auth${redirect}`
}
