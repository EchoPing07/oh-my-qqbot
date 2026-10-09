/**
 * 认证流程工具：Bearer token 持久化、登出与认证状态探测。
 *
 * Go 后端不走 HttpOnly Cookie：登录成功后把 access token 存入 localStorage，
 * 此后所有请求经 Authorization: Bearer 头携带（见 lib/http/client.ts）。
 *
 * 走 authApi 实例（不注入 token、401 不跳转）——
 * 在这两个场景里 401 / 未认证是正常业务结果，不应触发整页跳转。
 */
import { authApi } from '@/lib/http'

import { setStoredToken } from './api-base'

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
  // 无论成功与否都跳转到登录页
  window.location.href = '/auth'
}

/**
 * 检查当前认证状态（以本地 token 调 /auth/check）
 */
export async function checkAuthStatus(): Promise<boolean> {
  return (await getAuthStatus()).authenticated
}

/**
 * 获取当前认证状态和 Token 来源
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
 * 获取首次配置状态和 Token 来源
 */
export async function getSetupStatus(): Promise<SetupStatus | null> {
  try {
    return await authApi.get<SetupStatus>('/api/webui/setup/status')
  } catch {
    return null
  }
}
