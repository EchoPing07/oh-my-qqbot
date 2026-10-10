/**
 * HTTP 客户端（唯一出口）。
 *
 * - bearer 实例（api）：带 Bearer 头；401 时清 token 跳登录页。
 * - anon 实例（anonApi）：不带凭据、401 透传 — 用于 auth/verify、auth/check、
 *   setup/status 等登录前探测接口（未登录是正常业务结果，不能触发跳转）。
 *
 * 后端响应包络：业务接口为 { success?: boolean, message?: string, ... }，
 * success === false 视为业务失败，抛 ApiError（与 HTTP 层错误同型）。
 */
import { fireUnauthorized, getStoredToken, setStoredToken } from './token'

export type HttpMethod = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'

export interface RequestOptions {
  query?: Record<string, string | number | boolean | null | undefined | (string | number)[]>
  body?: unknown
  signal?: AbortSignal
  /** 业务上下文错误文案（后端未给出 message 时作为 ApiError.message） */
  errorMessage?: string
  /** 默认 'json'；'raw' 跳过 success 包络校验直接返回解析后的 JSON */
  envelope?: 'json' | 'raw'
}

export class ApiError extends Error {
  status?: number
  detail?: unknown

  constructor(message: string, options: { status?: number; detail?: unknown; cause?: unknown } = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = options.status
    this.detail = options.detail
    if (options.cause !== undefined) this.cause = options.cause
  }
}

export interface ApiClient {
  request<T>(method: HttpMethod, path: string, options?: RequestOptions): Promise<T>
  get<T>(path: string, options?: RequestOptions): Promise<T>
  post<T>(path: string, options?: RequestOptions): Promise<T>
  put<T>(path: string, options?: RequestOptions): Promise<T>
  delete<T>(path: string, options?: RequestOptions): Promise<T>
}

function buildQuery(query: NonNullable<RequestOptions['query']>): string {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    const values = Array.isArray(value) ? value : [value]
    for (const v of values) {
      if (v === undefined || v === null || v === '') continue
      params.append(key, String(v))
    }
  }
  const s = params.toString()
  return s ? `?${s}` : ''
}

interface ClientOptions {
  auth: 'bearer' | 'none'
  onUnauthorized?: () => void
}

/** 创建 ApiClient（导出供测试构造隔离实例；应用内统一用 api/anonApi 单例）。 */
export function createApiClient(options: ClientOptions): ApiClient {
  async function request<T>(method: HttpMethod, path: string, req: RequestOptions = {}): Promise<T> {
    const { query, body, signal, errorMessage, envelope = 'json' } = req
    const headers: Record<string, string> = {}
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    if (options.auth === 'bearer') {
      const token = getStoredToken()
      if (token) headers.Authorization = `Bearer ${token}`
    }

    let response: Response
    try {
      response = await fetch(`${path}${buildQuery(query ?? {})}`, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal,
      })
    } catch (error) {
      const reason = error instanceof Error ? error.message : String(error)
      // AbortError（组件卸载/主动取消）原样抛出，让调用方安静处理
      if (error instanceof DOMException && error.name === 'AbortError') throw error
      throw new ApiError(`网络请求失败：${reason}`, { cause: error })
    }

    if (response.status === 401 && options.auth === 'bearer') {
      options.onUnauthorized?.()
      throw new ApiError('认证失败，请重新登录', { status: 401 })
    }

    const rawText = await response.text()

    if (!response.ok) {
      let message = errorMessage ?? `请求失败（HTTP ${response.status}）`
      let detail: unknown = rawText
      try {
        const parsed = JSON.parse(rawText) as Record<string, unknown>
        detail = parsed
        if (typeof parsed.message === 'string' && parsed.message) message = parsed.message
        else if (typeof parsed.detail === 'string' && parsed.detail) message = parsed.detail
      } catch {
        // 非 JSON 错误体（如 SPA HTML）保持默认文案
      }
      throw new ApiError(message, { status: response.status, detail })
    }

    if (!rawText) throw new ApiError('接口返回了空响应', { status: response.status })

    let data: unknown
    try {
      data = JSON.parse(rawText)
    } catch {
      throw new ApiError('接口响应不是合法 JSON', { status: response.status, detail: rawText })
    }

    if (envelope === 'json' && data && typeof data === 'object' && 'success' in data) {
      const env = data as { success?: unknown; message?: unknown }
      if (env.success === false) {
        throw new ApiError(
          typeof env.message === 'string' && env.message ? env.message : (errorMessage ?? '操作失败'),
          { status: response.status, detail: data }
        )
      }
    }

    return data as T
  }

  return {
    request,
    get: (path, opts) => request('GET', path, opts),
    post: (path, opts) => request('POST', path, opts),
    put: (path, opts) => request('PUT', path, opts),
    delete: (path, opts) => request('DELETE', path, opts),
  }
}

function defaultUnauthorized(): void {
  setStoredToken('')
  fireUnauthorized()
  if (!window.location.pathname.startsWith('/auth')) {
    const redirect = encodeURIComponent(window.location.pathname + window.location.search)
    window.location.href = `/auth?redirect=${redirect}`
  }
}

/** 主后端实例：Bearer 认证 + 401 自动跳登录。 */
/** 主后端实例：Bearer 认证 + 401 自动跳登录。 */
export const api = createApiClient({ auth: 'bearer', onUnauthorized: defaultUnauthorized })

/** 匿名实例：不带凭据、401/包络透传（登录页、认证探测专用）。 */
/** 匿名实例：不带凭据、401/包络透传（登录页、认证探测专用）。 */
export const anonApi = createApiClient({ auth: 'none' })

/** 下载二进制（导出等）：带 Bearer 头，返回 Blob。 */
export async function downloadBinary(path: string, body?: unknown): Promise<Blob> {
  const headers: Record<string, string> = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const token = getStoredToken()
  if (token) headers.Authorization = `Bearer ${token}`
  const response = await fetch(path, {
    method: 'POST',
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!response.ok) throw new ApiError(`下载失败（HTTP ${response.status}）`, { status: response.status })
  return response.blob()
}
