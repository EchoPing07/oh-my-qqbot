import { describe, afterEach, expect, it, vi } from 'vitest'

import { ApiError, createApiClient } from './http'
import { setStoredToken } from './token'

const jsonResponse = (body: unknown, status = 200): Response =>
  new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })

afterEach(() => {
  setStoredToken('')
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

function mockFetch(handler: (url: string, init: RequestInit) => Response | Promise<Response>) {
  const fetchMock = vi.fn(handler as typeof fetch)
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('ApiClient（bearer）', () => {
  it('GET 携带 Bearer 头并解析 JSON', async () => {
    setStoredToken('tok-1')
    const fetchMock = mockFetch(() => jsonResponse({ ok: true }))
    const client = createApiClient({ auth: 'bearer' })
    const data = await client.get<{ ok: boolean }>('/api/x')
    expect(data.ok).toBe(true)
    const init = fetchMock.mock.calls[0][1]!
    expect((init.headers as Record<string, string>).Authorization).toBe('Bearer tok-1')
  })

  it('无 token 时不带 Authorization 头', async () => {
    const fetchMock = mockFetch(() => jsonResponse({}))
    const client = createApiClient({ auth: 'bearer' })
    await client.get('/api/x')
    const headers = fetchMock.mock.calls[0][1]!.headers as Record<string, string>
    expect(headers.Authorization).toBeUndefined()
  })

  it('success=false 抛业务 ApiError（取后端 message）', async () => {
    mockFetch(() => jsonResponse({ success: false, message: '配置保存失败' }))
    const client = createApiClient({ auth: 'bearer' })
    await expect(client.post('/api/x', { body: {} })).rejects.toMatchObject({
      name: 'ApiError',
      message: '配置保存失败',
    })
  })

  it('HTTP 401 触发 onUnauthorized 并抛固定文案', async () => {
    setStoredToken('bad')
    mockFetch(() => jsonResponse({ message: 'Token 无效' }, 401))
    const onUnauthorized = vi.fn()
    const client = createApiClient({ auth: 'bearer', onUnauthorized })
    await expect(client.get('/api/x')).rejects.toMatchObject({ status: 401 })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('HTTP 错误透传后端 message', async () => {
    mockFetch(() => jsonResponse({ message: '数据库未启用' }, 503))
    const client = createApiClient({ auth: 'bearer' })
    await expect(client.get('/api/x')).rejects.toMatchObject({ status: 503, message: '数据库未启用' })
  })

  it('query 序列化：跳过 null/undefined/空串，数组展开', async () => {
    const fetchMock = mockFetch(() => jsonResponse({}))
    const client = createApiClient({ auth: 'bearer' })
    await client.get('/api/x', { query: { a: 1, b: 'x', c: null, d: undefined, e: '', f: ['p', 'q'] } })
    const url = fetchMock.mock.calls[0][0] as string
    expect(url).toContain('a=1')
    expect(url).toContain('b=x')
    expect(url).toContain('f=p&f=q')
    expect(url).not.toContain('c=')
    expect(url).not.toContain('e=')
  })

  it('envelope=raw 跳过 success 校验', async () => {
    mockFetch(() => jsonResponse({ success: false, authenticated: false }))
    const client = createApiClient({ auth: 'bearer' })
    const data = await client.get<{ authenticated: boolean }>('/api/x', { envelope: 'raw' })
    expect(data.authenticated).toBe(false)
  })

  it('HTML 响应给出可读诊断（非合法 JSON）', async () => {
    mockFetch(() => new Response('<!doctype html><html></html>', { status: 200 }))
    const client = createApiClient({ auth: 'bearer' })
    await expect(client.get('/api/x')).rejects.toBeInstanceOf(ApiError)
  })

  it('网络异常包装为 ApiError', async () => {
    mockFetch(() => {
      throw new TypeError('Failed to fetch')
    })
    const client = createApiClient({ auth: 'bearer' })
    await expect(client.get('/api/x')).rejects.toMatchObject({ name: 'ApiError' })
  })

  it('anon 实例语义：auth=none 不带 Bearer', async () => {
    setStoredToken('tok-1')
    const fetchMock = mockFetch(() => jsonResponse({}))
    const client = createApiClient({ auth: 'none' })
    await client.get('/api/x')
    const headers = fetchMock.mock.calls[0][1]!.headers as Record<string, string>
    expect(headers.Authorization).toBeUndefined()
  })
})
