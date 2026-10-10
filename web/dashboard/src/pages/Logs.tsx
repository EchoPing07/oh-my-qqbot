/**
 * 实时日志 页 — 历史 REST 拉取 + WS logs 域实时追加。
 * 数据源：GET /logs/recent、GET|PUT /logs/level、WS 域 logs:main（entry / snapshot）。
 *
 * 渲染模型：历史（REST）作为基线，WS 事件作为增量；两者在渲染期合并，
 * 「清屏」只隐藏基线（不影响后端），新事件继续追加。
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Copy, Eraser, Pause, Play, RefreshCw, ScrollText } from 'lucide-react'

import { getLogLevel, getRecentLogs, setLogLevel, type LogEntry } from '@/api/misc'
import { Banner, Empty, Loading } from '@/components/ui'
import { cn } from '@/lib/cn'
import { formatDateTime } from '@/lib/format'
import { toast } from '@/lib/toast'
import { unifiedWs, type ConnectionStatus } from '@/lib/ws'

/** 本地保留上限（超出裁头，防止长跑内存膨胀）。 */
const MAX_LOCAL = 500
/** REST 首次拉取的历史条数。 */
const HISTORY_LIMIT = 300
/** 滚动跟随阈值（px）：距底小于该值视为贴底。 */
const STICK_THRESHOLD = 60

const LEVEL_TABS = [
  { key: '', label: '全部' },
  { key: 'DEBUG', label: 'DEBUG' },
  { key: 'INFO', label: 'INFO' },
  { key: 'WARN', label: 'WARN' },
  { key: 'ERROR', label: 'ERROR' },
] as const

const LEVEL_OPTIONS = ['DEBUG', 'INFO', 'WARN', 'ERROR'] as const

/** 本地行需要稳定唯一 key：后端 id 可能缺失，用自增序号兜底。 */
interface LocalEntry extends LogEntry {
  key: string
}

let keySeed = 0

function toLocal(entry: LogEntry): LocalEntry {
  keySeed += 1
  const id = entry.id && entry.id.trim() ? entry.id : ''
  return { ...entry, key: id ? `id:${id}` : `seq:${keySeed}` }
}

/** 尝试把未知对象解析成日志条目（WS 载荷是动态结构）。 */
function parseEntry(raw: unknown): LogEntry | null {
  if (!raw || typeof raw !== 'object') return null
  const obj = raw as Record<string, unknown>
  const level = typeof obj.level === 'string' ? obj.level : ''
  const message = typeof obj.message === 'string' ? obj.message : ''
  if (!level && !message) return null
  return {
    id: typeof obj.id === 'string' ? obj.id : '',
    timestamp: typeof obj.timestamp === 'string' ? obj.timestamp : '',
    level: level || 'INFO',
    module: typeof obj.module === 'string' ? obj.module : '',
    message,
  }
}

/** 连接状态徽章。 */
function StatusBadge({ status }: { status: ConnectionStatus }) {
  if (status === 'connected') return <span className="badge badge-ok">已连接</span>
  if (status === 'connecting') return <span className="badge badge-warn">连接中…</span>
  return <span className="badge">未连接</span>
}

export function LogsPage() {
  const qc = useQueryClient()
  const [level, setLevel] = useState('')
  // WS 增量（entry 追加）
  const [live, setLive] = useState<LocalEntry[]>([])
  // WS 整体快照（snapshot 事件覆盖 REST 基线）
  const [snapshot, setSnapshot] = useState<LocalEntry[] | null>(null)
  // 清屏标记：仅隐藏本地视图，不影响后端
  const [baseHidden, setBaseHidden] = useState(false)
  const [status, setStatus] = useState<ConnectionStatus>(unifiedWs.status)
  const [follow, setFollow] = useState(true)
  const listRef = useRef<HTMLDivElement>(null)

  // 历史日志（切换级别过滤时重新拉取，对齐 GET /logs/recent?limit&level）
  const history = useQuery({
    queryKey: ['logs-recent', HISTORY_LIMIT, level],
    queryFn: () => getRecentLogs(HISTORY_LIMIT, level),
  })

  const scrollToBottom = useCallback(() => {
    const el = listRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [])

  // WS：订阅 logs:main，entry 追加、snapshot 整体替换（回调内 setState，非渲染期）
  useEffect(() => {
    const unsubscribe = unifiedWs.subscribe('logs', 'main')
    const offStatus = unifiedWs.onStatus(setStatus)
    const offEvent = unifiedWs.onEvent((env) => {
      if (env.domain !== 'logs') return
      if (env.event === 'entry') {
        const entry = parseEntry(env.data.entry ?? env.data)
        if (!entry) return
        setLive((prev) => {
          const next = [...prev, toLocal(entry)]
          return next.length > MAX_LOCAL ? next.slice(next.length - MAX_LOCAL) : next
        })
      } else if (env.event === 'snapshot') {
        const raw = env.data.entries
        const list = Array.isArray(raw) ? raw : []
        const parsed = list.map(parseEntry).filter((e): e is LogEntry => e !== null).map(toLocal)
        setSnapshot(parsed)
        setLive([])
        setBaseHidden(false)
      }
    })
    return () => {
      offEvent()
      offStatus()
      unsubscribe()
    }
  }, [])

  // 跟随底部（暂停时不滚）
  useEffect(() => {
    if (follow) scrollToBottom()
  }, [live, snapshot, baseHidden, follow, scrollToBottom])

  const onScroll = useCallback(() => {
    const el = listRef.current
    if (!el) return
    setFollow(el.scrollHeight - el.scrollTop - el.clientHeight < STICK_THRESHOLD)
  }, [])

  // 日志级别：GET|PUT /logs/level
  const levelQuery = useQuery({
    queryKey: ['logs-level'],
    queryFn: getLogLevel,
    staleTime: 30_000,
  })
  const levelMutation = useMutation({
    mutationFn: async (next: string) => {
      await setLogLevel(next)
    },
    onSuccess: (_data, next) => {
      toast.ok(`日志级别已切换为 ${next}`)
      void qc.invalidateQueries({ queryKey: ['logs-level'] })
    },
    onError: (err) => {
      toast.err(err instanceof Error ? err.message : '日志级别修改失败')
    },
  })
  // 后端可能返回小写级别，统一大写后再回填下拉
  const currentLevel = (levelQuery.data ?? '').toUpperCase()

  // 渲染期合并：基线（快照 > REST 历史）+ WS 增量，按级别过滤后裁到上限
  const entries = useMemo(() => {
    const base = snapshot ?? (baseHidden ? [] : (history.data?.logs ?? []).map(toLocal))
    const merged = baseHidden && !snapshot ? [...live] : [...base, ...live]
    const filtered = level ? merged.filter((e) => e.level.toUpperCase() === level) : merged
    return filtered.length > MAX_LOCAL ? filtered.slice(filtered.length - MAX_LOCAL) : filtered
  }, [snapshot, baseHidden, history.data, live, level])

  const clearLocal = useCallback(() => {
    setLive([])
    setBaseHidden(true)
    setFollow(true)
    toast.info('已清屏（仅本地视图）')
  }, [])

  const copyAll = useCallback(() => {
    if (entries.length === 0) {
      toast.info('没有可复制的日志')
      return
    }
    const text = entries
      .map((e) => `${formatDateTime(e.timestamp)} ${e.level} [${e.module}] ${e.message}`)
      .join('\n')
    navigator.clipboard
      .writeText(text)
      .then(() => toast.ok(`已复制 ${entries.length} 条日志`))
      .catch(() => toast.err('复制失败：浏览器拒绝了剪贴板访问'))
  }, [entries])

  const counts = useMemo(() => {
    const stat = { DEBUG: 0, INFO: 0, WARN: 0, ERROR: 0, OTHER: 0 }
    for (const e of entries) {
      const key = e.level.toUpperCase() as keyof typeof stat
      if (key in stat) stat[key] += 1
      else stat.OTHER += 1
    }
    return stat
  }, [entries])

  const error = history.error

  return (
    <div className="page page-wide">
      <div className="page-head">
        <div>
          <h1>实时日志</h1>
          <p className="page-sub">
            当前视图 {entries.length} 条（本地上限 {MAX_LOCAL}） · WARN {counts.WARN} / ERROR {counts.ERROR}
          </p>
        </div>
        <div className="page-actions">
          <StatusBadge status={status} />
          <button
            className="btn btn-secondary btn-sm"
            disabled={history.isFetching}
            onClick={() => {
              setBaseHidden(false)
              setLive([])
              void history.refetch()
            }}
          >
            {history.isFetching ? <span className="spin" /> : <RefreshCw />} 刷新
          </button>
          <button className="btn btn-secondary btn-sm" onClick={() => setFollow((v) => !v)}>
            {follow ? <Pause /> : <Play />} {follow ? '暂停滚动' : '恢复滚动'}
          </button>
          <button className="btn btn-secondary btn-sm" onClick={clearLocal}>
            <Eraser /> 清屏
          </button>
          <button className="btn btn-secondary btn-sm" onClick={copyAll}>
            <Copy /> 复制全部
          </button>
        </div>
      </div>

      {error ? <Banner kind="err">{error instanceof Error ? error.message : '日志拉取失败'}</Banner> : null}
      {status !== 'connected' ? (
        <Banner kind="warn">实时通道未连接，页面仅显示 REST 拉取到的历史日志。</Banner>
      ) : null}

      {/* 工具条：级别过滤 tabs + 级别修改 */}
      <div className="card">
        <div className="card-head" style={{ gap: 12 }}>
          <div className="tabs" style={{ borderBottom: 'none', flex: 1 }}>
            {LEVEL_TABS.map((tab) => (
              <button
                key={tab.key}
                className={cn('tab', level === tab.key && 'active')}
                onClick={() => {
                  setLevel(tab.key)
                  setBaseHidden(false)
                }}
              >
                {tab.label}
              </button>
            ))}
          </div>
          <span className="spacer" />
          <span className="muted-xs">日志级别</span>
          <select
            className="select"
            style={{ width: 110 }}
            value={currentLevel}
            disabled={levelQuery.isPending || levelMutation.isPending}
            onChange={(e) => levelMutation.mutate(e.target.value)}
            aria-label="日志级别"
          >
            {levelQuery.isPending ? <option value="">读取中…</option> : null}
            {LEVEL_OPTIONS.map((opt) => (
              <option key={opt} value={opt}>
                {opt}
              </option>
            ))}
          </select>
          {levelMutation.isPending ? <span className="spin" style={{ width: 12, height: 12 }} /> : null}
        </div>
        <div className="card-body-flush">
          {history.isPending && !snapshot ? (
            <Loading text="正在加载日志…" />
          ) : entries.length === 0 ? (
            <Empty icon={<ScrollText />} text="暂无日志" />
          ) : (
            <div
              ref={listRef}
              onScroll={onScroll}
              style={{ height: 'calc(100vh - 260px)', minHeight: 320, overflowY: 'auto', padding: '6px 0' }}
            >
              {entries.map((e) => (
                <LogRow key={e.key} entry={e} />
              ))}
            </div>
          )}
        </div>
        {!follow ? (
          <div style={{ borderTop: '1px solid var(--border)', padding: '6px 14px' }}>
            <button
              className="btn btn-ghost btn-sm"
              onClick={() => {
                setFollow(true)
                scrollToBottom()
              }}
            >
              已暂停跟随，点击回到最新
            </button>
          </div>
        ) : null}
      </div>
    </div>
  )
}

function LogRow({ entry }: { entry: LogEntry }) {
  const level = entry.level.toUpperCase()
  return (
    <div className="log-line" title={formatDateTime(entry.timestamp)}>
      <span className="log-time">{formatDateTime(entry.timestamp)}</span>
      <span className={cn('log-level', `log-${level}`)}>{level}</span>
      <span className="log-module" title={entry.module}>
        {entry.module || '-'}
      </span>
      <span className="log-msg">{entry.message}</span>
    </div>
  )
}
