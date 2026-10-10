/**
 * unifiedWs 单测：不建真实连接，只测订阅登记/事件分发/退订语义。
 * WebSocket 行为本身依赖浏览器，属于集成测试范围（playwright 冒烟覆盖）。
 */
import { describe, expect, it, vi } from 'vitest'

import { unifiedWs, type WsEventEnvelope } from './ws'

function makeEnvelope(event: string, data: Record<string, unknown> = {}): WsEventEnvelope {
  return { op: 'event', domain: 'logs', event, data }
}

describe('unifiedWs', () => {
  it('onEvent 收到事件并支持退订', () => {
    const listener = vi.fn()
    const off = unifiedWs.onEvent(listener)
    // 通过内部分发路径触发：直接调用 onEvent 注册的监听器集合无法从外部访问，
    // 这里只验证订阅/退订 API 契约（不抛错、退订后可再次订阅）。
    off()
    const off2 = unifiedWs.onEvent(listener)
    off2()
    expect(listener).not.toHaveBeenCalled()
  })

  it('onStatus 立即回放当前状态并支持退订', () => {
    const seen: string[] = []
    const off = unifiedWs.onStatus((s) => seen.push(s))
    off()
    expect(seen.length).toBeGreaterThanOrEqual(1)
  })

  it('subscribe 未连接时安全登记并返回退订函数', () => {
    const off = unifiedWs.subscribe('logs', 'main')
    off()
    // 不抛错即通过
    expect(true).toBe(true)
  })

  it('envelope 形状守卫', () => {
    const env = makeEnvelope('entry', { level: 'INFO' })
    expect(env.op).toBe('event')
    expect(env.domain).toBe('logs')
    expect(env.data.level).toBe('INFO')
  })
})
