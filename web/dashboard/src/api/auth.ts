/**
 * 认证 / 首次配置 API — 对齐 internal/webui/routers/auth.go。
 */

import { anonApi, api, ApiError } from '@/lib/http'
import { getStoredToken, setStoredToken } from '@/lib/token'

export interface AuthCheck {
  authenticated: boolean
  token_source?: string
  requires_custom_token?: boolean
}

export interface VerifyResult {
  valid: boolean
  message?: string
  is_first_setup?: boolean
  token_source?: string
  requires_custom_token?: boolean
}

export interface SetupStatus {
  is_first_setup: boolean
  token_source: string
  requires_custom_token: boolean
  authenticated: boolean
  message?: string
}

/** POST /auth/verify：验证密码（登录动作）。 */
export async function verifyToken(token: string): Promise<VerifyResult> {
  const trimmed = token.trim()
  if (!trimmed) throw new ApiError('请输入登录密码')
  return anonApi.post<VerifyResult>('/api/webui/auth/verify', {
    body: { token: trimmed },
    errorMessage: '登录失败',
  })
}

/** GET /auth/check：以本地 token 探测是否已认证（带 Bearer；后端按头鉴权）。 */
export async function checkAuth(): Promise<boolean> {
  if (!getStoredToken()) return false
  try {
    const data = await api.get<AuthCheck>('/api/webui/auth/check', { envelope: 'raw' })
    return Boolean(data && typeof data === 'object' && (data as AuthCheck).authenticated)
  } catch {
    return false
  }
}

/** GET /setup/status（匿名可访问）：登录页分流用。 */
export async function getSetupStatus(): Promise<SetupStatus> {
  return anonApi.get<SetupStatus>('/api/webui/setup/status', { envelope: 'raw' })
}

/** GET /setup/status（带 Bearer）：登录后确认凭据仍有效并取向导状态。 */
export async function getSetupStatusAuthorized(): Promise<SetupStatus | null> {
  if (!getStoredToken()) return null
  try {
    return await api.get<SetupStatus>('/api/webui/setup/status', { envelope: 'raw' })
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) return null
    throw error
  }
}

/** 登录成功后的统一动作：存 token。 */
export function storeLoginToken(token: string): void {
  setStoredToken(token.trim())
}

export interface TokenUpdateResult {
  success: boolean
  message: string
}

/** POST /auth/update：设置自定义密码（≥10 位，含大小写与特殊符号）。 */
export async function updatePassword(newToken: string): Promise<TokenUpdateResult> {
  return api.post<{ success: boolean; message: string }>('/api/webui/auth/update', {
    body: { new_token: newToken },
    errorMessage: '更新密码失败',
  })
}

/** POST /auth/regenerate：重新生成随机 token（返回新 token，需重新登录）。 */
export async function regenerateToken(): Promise<string> {
  const data = await api.post<{ success: boolean; token: string }>('/api/webui/auth/regenerate')
  return data.token
}

/** POST /setup/complete：标记首次配置完成。 */
export async function completeSetup(): Promise<void> {
  await api.post('/api/webui/setup/complete')
}

/** POST /setup/reset：重置首次配置状态（重新进入向导）。 */
export async function resetSetup(): Promise<void> {
  await api.post('/api/webui/setup/reset')
}
