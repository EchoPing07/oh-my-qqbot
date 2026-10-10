/**
 * 统一 WebSocket 客户端 — 对接 Go 后端 internal/webui/wshub.go 协议。
 *
 * 协议：
 * - 握手：GET /api/webui/ws?token={一次性 ws-token}（经 /api/webui/ws-token 换取）
 * - 客户端：{op:'ping'|'subscribe'|'unsubscribe', id?, domain, topic?, data?}
 * - 服务端：{op:'event', domain, event, topic, data} / {op:'response', id, ok, data|error} / {op:'pong', ts}
 *
 * 自动重连（指数退避，上限 30s）、30s 心跳、订阅在重连后自动恢复。
 */

export interface WsEventEnvelope {
  op: 'event'
  domain: string
  event: string
  topic?: string
  data: Record<string, unknown>
}

export type ConnectionStatus = 'idle' | 'connecting' | 'connected'

type EventListener = (message: WsEventEnvelope) => void
type StatusListener = (status: ConnectionStatus) => void

interface SubscriptionKey {
  domain: string
  topic: string
}

function keyOf(sub: SubscriptionKey): string {
  return `${sub.domain}:${sub.topic}`
}

async function fetchWsToken(): Promise<string | null> {
  try {
    const { api } = await import('./http')
    const data = await api.get<{ success?: boolean; token?: string }>(
      '/api/webui/ws-token',
      { envelope: 'raw' }
    )
    if (data && typeof data === 'object' && data.success === true && typeof data.token === 'string') {
      return data.token
    }
    return null
  } catch {
    return null
  }
}

class UnifiedWs {
  private ws: WebSocket | null = null
  private statusValue: ConnectionStatus = 'idle'
  private subscriptions = new Map<string, SubscriptionKey>()
  private eventListeners = new Set<EventListener>()
  private statusListeners = new Set<StatusListener>()
  private reconnectAttempts = 0
  private reconnectTimer: number | null = null
  private heartbeatTimer: number | null = null
  private disposed = false
  private connectGeneration = 0

  get status(): ConnectionStatus {
    return this.statusValue
  }

  onEvent(listener: EventListener): () => void {
    this.eventListeners.add(listener)
    return () => {
      this.eventListeners.delete(listener)
    }
  }

  onStatus(listener: StatusListener): () => void {
    this.statusListeners.add(listener)
    listener(this.statusValue)
    return () => {
      this.statusListeners.delete(listener)
    }
  }

  /** 登记订阅（若已连接会立即下发 subscribe）。 */
  subscribe(domain: string, topic = 'main'): () => void {
    const sub = { domain, topic }
    const key = keyOf(sub)
    this.subscriptions.set(key, sub)
    if (this.statusValue === 'connected') {
      this.send({ op: 'subscribe', domain, topic })
    } else {
      this.ensureConnected()
    }
    return () => {
      this.subscriptions.delete(key)
      if (this.statusValue === 'connected') {
        this.send({ op: 'unsubscribe', domain, topic })
      }
    }
  }

  /** 主动关闭（登出时调用）：停止重连。 */
  dispose(): void {
    this.disposed = true
    this.connectGeneration++
    if (this.reconnectTimer !== null) window.clearTimeout(this.reconnectTimer)
    if (this.heartbeatTimer !== null) window.clearInterval(this.heartbeatTimer)
    this.ws?.close()
    this.ws = null
    this.setStatus('idle')
  }

  /** 重置后重新连接（重新登录后调用）。 */
  reset(): void {
    this.dispose()
    this.disposed = false
    this.reconnectAttempts = 0
  }

  private setStatus(status: ConnectionStatus): void {
    if (this.statusValue === status) return
    this.statusValue = status
    for (const listener of this.statusListeners) {
      try {
        listener(status)
      } catch (error) {
        console.error('WS 状态监听器执行失败:', error)
      }
    }
  }

  private ensureConnected(): void {
    if (this.disposed) return
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) return
    void this.connect()
  }

  private async connect(): Promise<void> {
    const generation = ++this.connectGeneration
    this.setStatus('connecting')
    const token = await fetchWsToken()
    if (this.disposed || generation !== this.connectGeneration) return
    if (!token) {
      // token 换取失败（未登录/过期）：退避重试（http 层 401 会跳登录页）
      this.scheduleReconnect()
      return
    }
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    let ws: WebSocket
    try {
      ws = new WebSocket(`${proto}//${window.location.host}/api/webui/ws?token=${encodeURIComponent(token)}`)
    } catch {
      this.scheduleReconnect()
      return
    }
    this.ws = ws

    ws.onopen = () => {
      if (generation !== this.connectGeneration) return
      this.reconnectAttempts = 0
      this.setStatus('connected')
      // 恢复全部订阅
      for (const sub of this.subscriptions.values()) {
        this.send({ op: 'subscribe', domain: sub.domain, topic: sub.topic })
      }
      this.heartbeatTimer = window.setInterval(() => {
        this.send({ op: 'ping' })
      }, 30_000)
    }

    ws.onmessage = (event) => {
      let parsed: unknown
      try {
        parsed = JSON.parse(String(event.data))
      } catch {
        return
      }
      if (!parsed || typeof parsed !== 'object') return
      const msg = parsed as Record<string, unknown>
      if (msg.op === 'event' && typeof msg.domain === 'string' && typeof msg.event === 'string') {
        const envelope: WsEventEnvelope = {
          op: 'event',
          domain: msg.domain,
          event: msg.event,
          topic: typeof msg.topic === 'string' ? msg.topic : undefined,
          data: (msg.data && typeof msg.data === 'object' ? msg.data : {}) as Record<string, unknown>,
        }
        for (const listener of this.eventListeners) {
          try {
            listener(envelope)
          } catch (error) {
            console.error('WS 事件监听器执行失败:', error)
          }
        }
      }
      // response / pong 当前无需处理（无 call 语义）
    }

    ws.onclose = () => {
      if (generation !== this.connectGeneration) return
      if (this.heartbeatTimer !== null) {
        window.clearInterval(this.heartbeatTimer)
        this.heartbeatTimer = null
      }
      this.ws = null
      this.setStatus('idle')
      this.scheduleReconnect()
    }

    ws.onerror = () => {
      // onclose 会随后触发，这里不重复处理
    }
  }

  private scheduleReconnect(): void {
    if (this.disposed) return
    if (this.reconnectTimer !== null) return
    const delay = Math.min(30_000, 1000 * 2 ** Math.min(this.reconnectAttempts, 5))
    this.reconnectAttempts++
    this.reconnectTimer = window.setTimeout(() => {
      this.reconnectTimer = null
      this.ensureConnected()
    }, delay)
  }

  private send(payload: Record<string, unknown>): void {
    if (this.ws?.readyState === WebSocket.OPEN) {
      try {
        this.ws.send(JSON.stringify(payload))
      } catch (error) {
        console.warn('WS 发送失败:', error)
      }
    }
  }
}

export const unifiedWs = new UnifiedWs()
