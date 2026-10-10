/**
 * 运行监控 页 — WS maisaka_monitor:main 事件流 + 快照历史回填。
 * 数据源：WS 域 maisaka_monitor（事件名即 Event.Kind）、GET /reasoning-process/snapshots。
 *
 * 事件载荷是内核动态结构（planner.progress / message.ingested / llm.retry …），
 * 页面只做「kind + session + 时间 + payload 摘要」的透传展示，不假设具体字段。
 *
 * 渲染模型：历史快照（REST）作为基线按 seq 倒序，WS 实时事件作为头部增量，
 * 两者在渲染期合并去重（by seq）；暂停时事件仍接收但写入暂停缓冲区，
 * 恢复后一次性并入列表。
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Activity, Eraser, Inbox, Pause, Play } from 'lucide-react'

import { getSnapshots } from '@/api/misc'
import { Banner, Empty, Loading } from '@/components/ui'
import { cn } from '@/lib/cn'
import { formatTime } from '@/lib/format'
import { unifiedWs, type ConnectionStatus } from '@/lib/ws'

/** 事件列表上限（倒序展示，超出裁尾）。 */
const MAX_EVENTS = 200
/** 摘要最大字符数。 */
const SUMMARY_MAX = 120
/** 未连接降级提示的等待阈值。 */
const CONNECT_TIMEOUT_MS = 10_000
/** 历史快照回填条数（与推理过程页同源）。 */
const HISTORY_LIMIT = 100

interface MonitorEvent {
  /** 唯一键：优先后端 seq，否则用自增序号兜底。 */
  key: string
  kind: string
  session: string
  time: string
  payload: Record<string, unknown>
}

let fallbackSeed = 0

function makeEvent(kind: string, payload: Record<string, unknown>): MonitorEvent {
  const rawSeq = payload.seq
  const session = typeof payload.session_id === 'string' ? payload.session_id : ''
  const time = typeof payload.time === 'string' ? payload.time : new Date().toISOString()
  const seq =
    typeof rawSeq === 'number' ? String(rawSeq) : typeof rawSeq === 'string' ? rawSeq : ''
  fallbackSeed += 1
  return {
    key: seq ? `seq:${seq}` : `gen:${fallbackSeed}`,
    kind,
    session,
    time,
    payload,
  }
}

/** payload 摘要：JSON 紧凑串截断（title 挂全量）。 */
function summarize(payload: Record<string, unknown>): string {
  let text = ''
  try {
    text = JSON.stringify(payload)
  } catch {
    text = '[无法序列化的载荷]'
  }
  if (!text) text = '{}'
  return text.length > SUMMARY_MAX ? `${text.slice(0, SUMMARY_MAX - 1)}…` : text
}

function StatusBadge({ status }: { status: ConnectionStatus }) {
  if (status === 'connected') return <span className="badge badge-ok">已连接</span>
  if (status === 'connecting') return <span className="badge badge-warn">连接中…</span>
  return <span className="badge">未连接</span>
}

/** kind → 徽章样式（仅 llm.error 等少数有语义色，其余中性）。 */
function kindClass(kind: string): string {
  if (kind.includes('error')) return 'badge badge-err'
  if (kind.includes('retry') || kind.includes('warn')) return 'badge badge-warn'
  if (kind.includes('sent') || kind.includes('start')) return 'badge badge-ok'
  return 'badge'
}

export function MonitorPage() {
  // 实时事件（最新在前）
  const [live, setLive] = useState<MonitorEvent[]>([])
  // 暂停期间仍接收的事件：存 ref（纯缓冲区，不参与渲染），恢复时由点击处理器并入
  const pendingRef = useRef<MonitorEvent[]>([])
  const [bufferedCount, setBufferedCount] = useState(0)
  const [status, setStatus] = useState<ConnectionStatus>(unifiedWs.status)
  const [paused, setPaused] = useState(false)
  const [selected, setSelected] = useState<MonitorEvent | null>(null)
  const [degraded, setDegraded] = useState(false)
  // 暂停时事件仍接收但 UI 不追加：用 ref 标记避免闭包重订阅
  const pausedRef = useRef(false)

  // 历史快照回填（planner.progress 缓存）
  const history = useQuery({
    queryKey: ['reasoning-snapshots', '', HISTORY_LIMIT],
    queryFn: () => getSnapshots('', HISTORY_LIMIT),
  })

  // 暂停标记只在事件回调里读，写入放到 effect（避免渲染期改 ref）
  useEffect(() => {
    pausedRef.current = paused
  }, [paused])

  // 恢复：把暂停期间的缓冲事件一次性并入列表（事件处理器内改 state）
  const resume = useCallback(() => {
    setPaused(false)
    pausedRef.current = false
    const pending = pendingRef.current
    if (pending.length === 0) return
    pendingRef.current = []
    setBufferedCount(0)
    setLive((prev) => [...pending, ...prev].slice(0, MAX_EVENTS))
  }, [])

  // WS 订阅：事件倒序插入头部
  useEffect(() => {
    const unsubscribe = unifiedWs.subscribe('maisaka_monitor', 'main')
    const offStatus = unifiedWs.onStatus(setStatus)
    const offEvent = unifiedWs.onEvent((env) => {
      if (env.domain !== 'maisaka_monitor') return
      const item = makeEvent(env.event, env.data)
      if (pausedRef.current) {
        pendingRef.current = [item, ...pendingRef.current].slice(0, MAX_EVENTS)
        setBufferedCount(pendingRef.current.length)
        return
      }
      setLive((prev) => [item, ...prev].slice(0, MAX_EVENTS))
    })
    return () => {
      offEvent()
      offStatus()
      unsubscribe()
    }
  }, [])

  // 10s 内未连接 → 降级提示
  useEffect(() => {
    const timer = window.setTimeout(() => {
      setDegraded(unifiedWs.status !== 'connected')
    }, CONNECT_TIMEOUT_MS)
    return () => window.clearTimeout(timer)
  }, [])

  // 渲染期合并：实时增量 + 历史快照（按 key 去重，最新在前）
  const events = useMemo(() => {
    const seen = new Set(live.map((e) => e.key))
    const merged = [...live]
    const fromHistory = (history.data ?? []).map((s) =>
      makeEvent(s.kind, { ...(s.payload ?? {}), seq: s.seq, session_id: s.session_id, time: s.time })
    )
    for (const item of fromHistory) {
      if (seen.has(item.key)) continue
      seen.add(item.key)
      merged.push(item)
    }
    return merged.slice(0, MAX_EVENTS)
  }, [live, history.data])

  const clear = useCallback(() => {
    setLive([])
    pendingRef.current = []
    setBufferedCount(0)
    setSelected(null)
  }, [])

  const loading = history.isPending && events.length === 0
  const error = history.error

  return (
    <div className="page page-wide">
      <div className="page-head">
        <div>
          <h1>运行监控</h1>
          <p className="page-sub">
            观察事件总线实时流（planner.progress / message.* / llm.* / session.start）· 保留最近 {MAX_EVENTS} 条
          </p>
        </div>
        <div className="page-actions">
          <StatusBadge status={status} />
          <button
            className="btn btn-secondary btn-sm"
            onClick={() => {
              if (paused) resume()
              else setPaused(true)
            }}
          >
            {paused ? <Play /> : <Pause />} {paused ? '恢复' : '暂停'}
          </button>
          <button className="btn btn-secondary btn-sm" onClick={clear}>
            <Eraser /> 清空
          </button>
        </div>
      </div>

      {error ? <Banner kind="err">{error instanceof Error ? error.message : '历史快照获取失败'}</Banner> : null}
      {degraded && status !== 'connected' ? (
        <Banner kind="warn">实时通道未连接，仅显示历史快照。</Banner>
      ) : null}

      <div className="grid grid-2">
        {/* 左：事件列表 */}
        <div className="card" style={{ minWidth: 0 }}>
          <div className="card-head">
            <h3>
              <Activity size={15} style={{ verticalAlign: -2 }} /> 事件流
            </h3>
            <span className="spacer" />
            <span className="muted-xs">
              {events.length} 条{paused ? ` · 已暂停（缓冲 ${bufferedCount}）` : ''}
            </span>
          </div>
          <div className="card-body-flush">
            {loading ? (
              <Loading />
            ) : events.length === 0 ? (
              <Empty icon={<Inbox />} text="暂无事件" />
            ) : (
              <div style={{ maxHeight: 'calc(100vh - 260px)', minHeight: 320, overflowY: 'auto' }}>
                {events.map((e) => (
                  <button
                    key={e.key}
                    className={cn('nav-item', selected?.key === e.key && 'active')}
                    style={{ width: '100%', textAlign: 'left', alignItems: 'flex-start', borderRadius: 0 }}
                    onClick={() => setSelected(e)}
                  >
                    <span className={kindClass(e.kind)} style={{ flexShrink: 0 }}>
                      {e.kind}
                    </span>
                    <span style={{ minWidth: 0, flex: 1, display: 'flex', flexDirection: 'column', gap: 1 }}>
                      <span className="muted-xs">
                        {e.session ? e.session.slice(0, 8) : '—'} · {formatTime(e.time)}
                      </span>
                      <span className="xs truncate" title={JSON.stringify(e.payload)}>
                        {summarize(e.payload)}
                      </span>
                    </span>
                  </button>
                ))}
              </div>
            )}
          </div>
        </div>

        {/* 右：事件详情 */}
        <div className="card" style={{ minWidth: 0 }}>
          <div className="card-head">
            <h3>事件详情</h3>
            <span className="spacer" />
            {selected ? <span className={kindClass(selected.kind)}>{selected.kind}</span> : null}
          </div>
          <div className="card-body">
            {!selected ? (
              <Empty text="点击左侧事件查看完整载荷" />
            ) : (
              <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
                <div className="kv-grid">
                  <div className="kv">
                    <span className="k">事件类型</span>
                    <span className="v">{selected.kind}</span>
                  </div>
                  <div className="kv">
                    <span className="k">会话 ID</span>
                    <span className="v">{selected.session || '—'}</span>
                  </div>
                  <div className="kv">
                    <span className="k">时间</span>
                    <span className="v">{formatTime(selected.time)}</span>
                  </div>
                  <div className="kv">
                    <span className="k">事件键</span>
                    <span className="v">{selected.key}</span>
                  </div>
                </div>
                <div>
                  <p className="label">载荷</p>
                  <pre className="code-block" style={{ margin: 0, maxHeight: 'calc(100vh - 380px)', overflowY: 'auto' }}>
                    {JSON.stringify(selected.payload, null, 2)}
                  </pre>
                </div>
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
