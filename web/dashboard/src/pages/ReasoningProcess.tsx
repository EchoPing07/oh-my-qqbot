/**
 * 推理过程 页 — Planner 快照历史（可展开完整 payload）。
 * 数据源：GET /reasoning-process/snapshots?session_id=&limit=（10s 轮询）。
 */

import { Fragment, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Brain, ChevronDown, ChevronRight, Inbox, RefreshCw } from 'lucide-react'

import { getSnapshots } from '@/api/misc'
import { Banner, Empty, Loading } from '@/components/ui'
import { formatDateTime } from '@/lib/format'

/** 默认拉取条数（后端上限 500，取 100 兼顾实时与体积）。 */
const SNAPSHOT_LIMIT = 100
/** 轮询间隔。 */
const POLL_MS = 10_000

/** kind → 徽章样式。 */
function kindClass(kind: string): string {
  if (kind.includes('error')) return 'badge badge-err'
  if (kind.includes('retry') || kind.includes('warn')) return 'badge badge-warn'
  if (kind.includes('done') || kind.includes('finish')) return 'badge badge-ok'
  return 'badge'
}

export function ReasoningProcessPage() {
  // draft：输入框值；filter：回车后生效的查询条件
  const [draft, setDraft] = useState('')
  const [filter, setFilter] = useState('')
  const [openSeq, setOpenSeq] = useState<number | null>(null)

  const snapshots = useQuery({
    queryKey: ['reasoning-snapshots', filter, SNAPSHOT_LIMIT],
    queryFn: () => getSnapshots(filter, SNAPSHOT_LIMIT),
    refetchInterval: POLL_MS,
  })

  const list = snapshots.data ?? []
  const error = snapshots.error

  return (
    <div className="page page-wide">
      <div className="page-head">
        <div>
          <h1>推理过程</h1>
          <p className="page-sub">
            Planner 进度快照（后端缓存最近 {SNAPSHOT_LIMIT} 条）· {POLL_MS / 1000}s 轮询
          </p>
        </div>
        <div className="page-actions">
          <input
            className="input"
            style={{ width: 260 }}
            placeholder="按 session_id 过滤（回车生效）"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.nativeEvent.isComposing) {
                e.preventDefault()
                setFilter(draft.trim())
              }
            }}
            aria-label="会话 ID 过滤"
          />
          {filter ? (
            <button
              className="btn btn-ghost btn-sm"
              onClick={() => {
                setDraft('')
                setFilter('')
              }}
            >
              清除
            </button>
          ) : null}
          <button className="btn btn-secondary btn-sm" onClick={() => void snapshots.refetch()} disabled={snapshots.isFetching}>
            {snapshots.isFetching ? <span className="spin" /> : <RefreshCw />} 刷新
          </button>
        </div>
      </div>

      {error ? <Banner kind="err">{error instanceof Error ? error.message : '快照获取失败'}</Banner> : null}
      {filter ? (
        <Banner kind="info">当前按会话 <code>{filter}</code> 过滤，共 {list.length} 条快照。</Banner>
      ) : null}

      <div className="card">
        <div className="card-head">
          <h3>
            <Brain size={15} style={{ verticalAlign: -2 }} /> 快照列表
          </h3>
          <span className="spacer" />
          <span className="muted-xs">点击行展开完整 payload</span>
        </div>
        <div className="card-body-flush">
          {snapshots.isPending ? (
            <Loading />
          ) : list.length === 0 ? (
            <Empty icon={<Inbox />} text={filter ? '该会话暂无快照' : '暂无快照（等待 Planner 产生进度事件）'} />
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th style={{ width: 28 }} />
                    <th className="num" style={{ width: 70 }}>seq</th>
                    <th style={{ width: 160 }}>kind</th>
                    <th>session_id</th>
                    <th style={{ width: 180 }}>time</th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((s) => {
                    const open = openSeq === s.seq
                    return (
                      <Fragment key={s.seq}>
                        <tr
                          onClick={() => setOpenSeq(open ? null : s.seq)}
                          style={{ cursor: 'pointer' }}
                        >
                          <td style={{ color: 'var(--muted-fg)' }}>
                            {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                          </td>
                          <td className="num">{s.seq}</td>
                          <td>
                            <span className={kindClass(s.kind)}>{s.kind}</span>
                          </td>
                          <td className="mono truncate" style={{ maxWidth: 260 }} title={s.session_id}>
                            {s.session_id ? s.session_id.slice(0, 12) : '—'}
                          </td>
                          <td className="muted-xs">{formatDateTime(s.time)}</td>
                        </tr>
                        {open ? (
                          <tr>
                            <td colSpan={5} style={{ background: 'var(--muted)' }}>
                              <pre
                                className="code-block"
                                style={{ margin: 0, maxHeight: 320, overflowY: 'auto', whiteSpace: 'pre-wrap' }}
                              >
                                {JSON.stringify(s.payload ?? {}, null, 2)}
                              </pre>
                            </td>
                          </tr>
                        ) : null}
                      </Fragment>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
