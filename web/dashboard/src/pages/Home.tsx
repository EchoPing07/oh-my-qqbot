/**
 * 首页/概览 — 系统状态 + 用量摘要 + 快捷入口。
 * 数据源：/system/status、/statistics/summary、/statistics/models、/news（降级可选）。
 */

import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Activity, Coins, Cpu, Inbox, Newspaper, RefreshCw, Zap } from 'lucide-react'

import { getStatsModels, getStatsSummary, getSystemStatus, type DailyStat } from '@/api/system'
import { getStatsDaily } from '@/api/system'
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
        <div key={d.day} style={{ flex: 1, display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 4, minWidth: 0 }} title={`${d.day}：${d.requests} 次请求 · ${compactNumber(d.total_tokens)} tokens`}>
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
          <Metric label="运行状态" value={status.data.running ? '正常' : '停止'} detail={`Go ${status.data.go_version}`} icon={Activity} />
          <Metric
            label="内存"
            value={`${(status.data.memory.heap_alloc_bytes / 1024 / 1024).toFixed(0)} MB`}
            detail={`${status.data.memory.num_goroutine} goroutines · ${status.data.memory.num_cpu} CPU`}
            icon={Cpu}
          />
          <Metric
            label="LLM 请求（24h）"
            value={formatNumber(summary.data?.request_count ?? 0)}
            detail={`平均 ${((summary.data?.avg_response_time ?? 0) * 1000).toFixed(0)} ms`}
            icon={Zap}
          />
          <Metric
            label="Tokens（24h）"
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
                      <th className="num">Tokens</th>
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

      <div className="page-head" style={{ marginTop: 6 }}>
        <h2 style={{ fontSize: 'var(--fs-lg)', fontWeight: 600 }}>快捷入口</h2>
      </div>
      <div className="grid grid-metrics">
        {[
          { to: '/chat', label: '和机器人聊天', desc: 'WebUI 内置聊天' },
          { to: '/config/bot', label: '机器人设置', desc: '触发器 / 表达 / 后处理' },
          { to: '/config/model', label: '模型配置', desc: '服务商与任务路由' },
          { to: '/logs', label: '实时日志', desc: '运行排障第一现场' },
        ].map((item) => (
          <Link key={item.to} to={item.to} className="metric" style={{ display: 'block' }}>
            <div className="metric-head">
              <span style={{ fontWeight: 600, fontSize: 'var(--fs-sm)' }}>{item.label}</span>
            </div>
            <p className="metric-detail">{item.desc}</p>
          </Link>
        ))}
      </div>

      <NewsCard />
    </div>
  )
}

/** 资讯卡片：接口失败时静默隐藏（外部源不保证可用）。 */
function NewsCard() {
  const news = useQuery({
    queryKey: ['news'],
    queryFn: async () => {
      const { getNews } = await import('@/api/system')
      return getNews()
    },
    retry: false,
    staleTime: 10 * 60_000,
  })
  if (news.isError || (news.data?.length ?? 0) === 0) return null
  return (
    <div className="card">
      <div className="card-head">
        <h3><Newspaper size={15} style={{ verticalAlign: -2 }} /> 社区资讯</h3>
      </div>
      <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
        {(news.data ?? []).slice(0, 5).map((item) => (
          <div key={item.id} style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
            <span className="sm" style={{ fontWeight: 500 }}>
              {item.url ? (
                <a href={item.url} target="_blank" rel="noreferrer" style={{ borderBottom: '1px solid var(--border-strong)' }}>
                  {item.title}
                </a>
              ) : (
                item.title
              )}
            </span>
            {item.content ? <span className="muted-xs">{item.content.slice(0, 140)}</span> : null}
          </div>
        ))}
      </div>
    </div>
  )
}
