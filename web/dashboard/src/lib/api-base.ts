/**
 * 集中式 API base URL 与 Bearer token 管理
 * 为全应用的 URL / 鉴权头构造提供唯一事实来源。
 *
 * 本 fork 仅保留纯 Web 形态（同源部署 + Vite dev proxy），Electron 动态后端已移除。
 */

/** 访问 token 的 localStorage 键 */
const TOKEN_STORAGE_KEY = 'ohmyqqbot-webui-token'

/** 401 未授权监听器（由 http 客户端触发，登录页/认证守卫可订阅） */
type UnauthorizedListener = () => void
const unauthorizedListeners = new Set<UnauthorizedListener>()

export function onUnauthorized(listener: UnauthorizedListener): () => void {
  unauthorizedListeners.add(listener)
  return () => {
    unauthorizedListeners.delete(listener)
  }
}

export function fireUnauthorized(): void {
  unauthorizedListeners.forEach((listener) => {
    try {
      listener()
    } catch (error) {
      console.error('401 处理器执行失败:', error)
    }
  })
}

/** 读取已保存的访问 token */
export function getStoredToken(): string {
  if (typeof window === 'undefined') return ''
  try {
    return localStorage.getItem(TOKEN_STORAGE_KEY) ?? ''
  } catch {
    return ''
  }
}

/** 保存 / 清除访问 token */
export function setStoredToken(token: string): void {
  if (typeof window === 'undefined') return
  try {
    if (token) {
      localStorage.setItem(TOKEN_STORAGE_KEY, token)
    } else {
      localStorage.removeItem(TOKEN_STORAGE_KEY)
    }
  } catch {
    // localStorage 不可用时静默忽略（隐私模式等）
  }
}

/** http 客户端取当前 Bearer token（未登录时返回空串，不注入 Authorization 头） */
export function getBearerToken(): string {
  return getStoredToken()
}

/** 登出：清除本地 token 并广播 401（供 logout 流程复用） */
export function clearTokenAndNotify(): void {
  setStoredToken('')
  fireUnauthorized()
}

/**
 * Get API base URL for HTTP/HTTPS requests
 * - Browser DEV: Empty string (Vite proxy handles /api prefix)
 * - Browser PROD: Empty string (same-origin deployment)
 */
export async function getApiBaseUrl(): Promise<string> {
  return ''
}

/**
 * Get WebSocket base URL
 * - Browser DEV: Use same-origin WS URL and let Vite proxy forward requests
 * - Browser PROD: Construct WS URL from window.location
 */
export async function getWsBaseUrl(): Promise<string> {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${protocol}//${window.location.host}`
}

/**
 * Resolve full API path by prepending base URL if needed
 * - Browser: Path remains unchanged (proxy/same-origin handling)
 */
export async function resolveApiPath(path: string): Promise<string> {
  return path
}
