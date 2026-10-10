/**
 * 首页/概览 — 系统状态 + 用量摘要 + 近 14 天请求量 + 模型调用。
 * 数据源：/system/status、/statistics/summary、/statistics/models、/statistics/daily。
 * （社区资讯与快捷入口已按需求移除。）
 */

import { useQuery } from '@tanstack/react-query'
import { Activity, Coins, Cpu, Inbox, RefreshCw, Zap } from 'lucide-react'

import { getStatsDaily, getStatsModels, getStatsSummary, getSystemStatus, type DailyStat } from '@/api/system'
import { compactNumber, formatUptime, formatNumber } from '@/lib/format'
import { Banner, Empty, Loading } from '@/components/ui'
import { APP_VERSION } from '@/lib/version'

function Metric({
  label,
  value,
  detail,
  icon: IconCmp,
}: {
  label: string
  value: string
  detail?: string
  icon: React.ComponentType<{ className?: string }>
}) {
  return (
    <div className="metric">
      <div className="metric-head">
        <span className="metric-label">{label}</span>
        <IconCmp />
      </div>
      <div className="metric-value">{value}</div>
      {detail ? <p className="metric-detail">{detail}</p> : null}
    </div>
  )
}

/** 14 天请求量迷你条形图（纯 div，无图表库）。 */
function DailyBars({ data }: { data: DailyStat[] }) {
  if (!data.length) return <Empty text="暂无用量数据" />
  const max = Math.max(...data.map((d) => d.requests), 1)
  return (
    <div style={{ display: 'flex', alignItems: 'flex-end', gap: 4, height: 120, padding: '8px 2px 0' }}>
      {data.map((d) => (
        <div key={d.day} style={{ flex: 1, display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 4, minWidth: 0 }} title={`${d.day}：${d.requests} 次请求 · ${compactNumber(d.total_tokens)} 令牌`}>
          <div
            style={{
              width: '100%',
              maxWidth: 36,
              height: `${Math.max(4, (d.requests / max) * 100)}%`,
              background: 'color-mix(in srgb, var(--fg) 55%, var(--card))',
              borderRadius: 2,
              minHeight: 4,
            }}
          />
          <span className="muted-xs" style={{ fontSize: 9, transform: 'scale(0.9)' }}>
            {d.day.slice(5)}
          </span>
        </div>
      ))}
    </div>
  )
}

export function HomePage() {
  const status = useQuery({ queryKey: ['system-status'], queryFn: getSystemStatus, refetchInterval: 15_000 })
  const summary = useQuery({ queryKey: ['stats-summary', 24], queryFn: () => getStatsSummary(24), refetchInterval: 30_000 })
  const models = useQuery({ queryKey: ['stats-models', 24], queryFn: () => getStatsModels(24), refetchInterval: 60_000 })
  const daily = useQuery({ queryKey: ['stats-daily', 14], queryFn: () => getStatsDaily(14), refetchInterval: 60_000 })

  const loading = status.isPending || summary.isPending
  const error = status.error ?? summary.error

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>概览</h1>
          <p className="page-sub">
            版本 v{APP_VERSION} · 运行 {status.data ? formatUptime(status.data.uptime) : '—'}
          </p>
        </div>
        <div className="page-actions">
          <button
            className="btn btn-secondary btn-sm"
            onClick={() => {
              void status.refetch()
              void summary.refetch()
              void daily.refetch()
            }}
          >
            <RefreshCw /> 刷新
          </button>
        </div>
      </div>

      {error ? <Banner kind="err">{error instanceof Error ? error.message : '后端状态获取失败'}</Banner> : null}
      {loading ? <Loading /> : null}

      {status.data ? (
        <div className="grid grid-metrics">
          <Metric label="运行状态" value={status.data.running ? '正常' : '停止'} detail={`运行时 ${status.data.go_version}`} icon={Activity} />
          <Metric
            label="内存"
            value={`${(status.data.memory.heap_alloc_bytes / 1024 / 1024).toFixed(0)} MB`}
            detail={`协程 ${formatNumber(status.data.memory.num_goroutine)} · 核心 ${status.data.memory.num_cpu}`}
            icon={Cpu}
          />
          <Metric
            label="模型请求（24 小时）"
            value={formatNumber(summary.data?.request_count ?? 0)}
            detail={`平均 ${((summary.data?.avg_response_time ?? 0) * 1000).toFixed(0)} ms`}
            icon={Zap}
          />
          <Metric
            label="令牌用量（24 小时）"
            value={compactNumber(summary.data?.total_tokens ?? 0)}
            detail={`缓存命中 ${compactNumber(summary.data?.cache_hit_tokens ?? 0)}`}
            icon={Coins}
          />
        </div>
      ) : null}

      <div className="grid grid-2">
        <div className="card">
          <div className="card-head">
            <h3>近 14 天请求量</h3>
          </div>
          <div className="card-body">
            {daily.isPending ? <Loading /> : <DailyBars data={daily.data ?? []} />}
          </div>
        </div>
        <div className="card">
          <div className="card-head">
            <h3>模型调用（24h）</h3>
            <span className="spacer" />
            <span className="muted-xs">{models.data?.length ?? 0} 个模型</span>
          </div>
          <div className="card-body-flush">
            {models.isPending ? (
              <Loading />
            ) : (models.data?.length ?? 0) === 0 ? (
              <Empty icon={<Inbox />} text="暂无调用数据" />
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>模型</th>
                      <th className="num">请求</th>
                      <th className="num">令牌</th>
                    </tr>
                  </thead>
                  <tbody>
                    {(models.data ?? []).slice(0, 8).map((m) => (
                      <tr key={m.model_name}>
                        <td className="truncate" style={{ maxWidth: 180 }} title={m.model_name}>{m.model_name}</td>
                        <td className="num">{formatNumber(m.request_count)}</td>
                        <td className="num">{compactNumber(m.total_tokens)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
