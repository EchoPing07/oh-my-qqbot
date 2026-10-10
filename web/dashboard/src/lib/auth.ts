/**
 * 认证流程工具：Bearer token 持久化、登出与认证状态探测。
 *
 * Go 后端不走 HttpOnly Cookie：登录成功后把 access token 存入 localStorage，
 * 此后所有请求经 Authorization: Bearer 头携带（见 lib/http/client.ts）。
 *
 * 认证探测分两类：
 * - authApi（不注入 token、401 不跳转）：auth/check、auth/verify、登出。
 *   未认证是正常业务结果，必须透传后端信息而不是跳转登录页。
 * - backendApi（携带 Bearer、401 跳转）：setup/status 自校验探测，用于
 *   登录后确认凭据仍有效并获取首次配置状态；401 时回退匿名请求。
 */
import { authApi, backendApi } from '@/lib/http'
import { ApiError } from '@/lib/http/errors'

import { fireUnauthorized, setStoredToken } from './api-base'

export interface AuthStatus {
  authenticated: boolean
  token_source?: string
  requires_custom_token?: boolean
}

export interface SetupStatus {
  is_first_setup: boolean
  token_source: string
  requires_custom_token: boolean
  message?: string
}

/**
 * 调用登出接口、清除本地 token 并跳转到登录页
 */
export async function logout(): Promise<void> {
  try {
    await authApi.post('/api/webui/auth/logout', { parse: 'response' })
  } catch (error) {
    console.error('登出请求失败:', error)
  }
  setStoredToken('')
  // 广播 401，让已挂载的页面/WS 客户端清状态；随后整页跳转登录页。
  fireUnauthorized()
  window.location.href = '/auth'
}

/**
 * 检查当前认证状态（以本地 token 调 /auth/check）
 */
export async function checkAuthStatus(): Promise<boolean> {
  return (await getAuthStatus()).authenticated
}

/**
 * 获取当前认证状态和 Token 来源。
 *
 * 走 authApi（无 Bearer）模拟 Cookie 会话语义：后端 Go 版以 Authorization 头鉴权、
 * 不下发 Cookie，未登录时请求头里自然没有 token，返回 authenticated=false。
 * 登录后的状态判断一律走 getSetupStatus（带 Bearer 自校验）。
 */
export async function getAuthStatus(): Promise<AuthStatus> {
  try {
    const data = await authApi.get<AuthStatus>('/api/webui/auth/check')
    return {
      authenticated: data.authenticated === true,
      token_source: data.token_source,
      requires_custom_token: data.requires_custom_token === true,
    }
  } catch {
    return { authenticated: false }
  }
}

/**
 * 获取首次配置状态和 Token 来源。
 *
 * 优先走 backendApi（携带已存的 Bearer，后端自校验并回传 authenticated）；
 * 401 时回退 authApi（后端允许匿名访问 setup/status，匿名响应仍含 setup 状态）。
 * 登录页在无 token 场景依赖匿名响应分流，已登录场景依赖 Bearer 响应确认凭据有效。
 */
export async function getSetupStatus(): Promise<SetupStatus | null> {
  try {
    return await backendApi.get<SetupStatus>('/api/webui/setup/status')
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) {
      try {
        return await authApi.get<SetupStatus>('/api/webui/setup/status')
      } catch {
        return null
      }
    }
    return null
  }
}
