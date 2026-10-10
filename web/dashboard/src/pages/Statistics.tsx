/**
 * 统计 页 — 用量概览 + 近 N 天请求量条形图 + 模型排行 + 明细表。
 * 数据源：GET /statistics/summary、/statistics/daily、/statistics/models。
 */

import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { BarChart3, Coins, Inbox, RefreshCw, Zap } from 'lucide-react'

import {
  getStatsDaily,
  getStatsModels,
  getStatsSummary,
  type DailyStat,
} from '@/api/system'
import { Banner, Empty, Loading } from '@/components/ui'
import { compactNumber, formatNumber } from '@/lib/format'

/** 时间范围 → hours / days 映射。 */
const RANGES = [
  { key: '6h', label: '6 小时', hours: 6, days: 7 },
  { key: '24h', label: '24 小时', hours: 24, days: 7 },
  { key: '7d', label: '7 天', hours: 168, days: 7 },
] as const

type RangeKey = (typeof RANGES)[number]['key']

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

/** 近 N 天请求量条形图（纯 div，无图表库；参考首页 DailyBars）。 */
function DailyBars({ data }: { data: DailyStat[] }) {
  if (!data.length) return <Empty text="暂无用量数据" />
  const max = Math.max(...data.map((d) => d.requests), 1)
  const totalTokens = data.reduce((sum, d) => sum + d.total_tokens, 0)
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
      <div style={{ display: 'flex', alignItems: 'flex-end', gap: 4, height: 140, padding: '8px 2px 0' }}>
        {data.map((d) => (
          <div
            key={d.day}
            style={{ flex: 1, display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 4, minWidth: 0 }}
            title={`${d.day}：${d.requests} 次请求 · ${compactNumber(d.total_tokens)} tokens · 成本 ${d.cost.toFixed(4)}`}
          >
            <span className="muted-xs" style={{ fontSize: 10 }}>
              {d.requests > 0 ? compactNumber(d.requests) : ''}
            </span>
            <div
              style={{
                width: '100%',
                maxWidth: 40,
                height: `${Math.max(4, (d.requests / max) * 100)}%`,
                background: 'color-mix(in srgb, var(--fg) 55%, var(--card))',
                borderRadius: 2,
                minHeight: 3,
              }}
            />
            <span className="muted-xs" style={{ fontSize: 10 }} title={d.day}>
              {d.day.slice(5)}
            </span>
          </div>
        ))}
      </div>
      <p className="muted-xs">区间 tokens 合计 {compactNumber(totalTokens)}</p>
    </div>
  )
}

/** 模型排行：水平条形（宽度按请求数占比）。 */
function ModelRank({ models }: { models: Array<{ model_name: string; request_count: number }> }) {
  if (!models.length) return <Empty icon={<Inbox />} text="暂无模型调用数据" />
  const max = Math.max(...models.map((m) => m.request_count), 1)
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
      {models.map((m) => (
        <div key={m.model_name} style={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
          <div style={{ display: 'flex', alignItems: 'baseline', gap: 8, fontSize: 'var(--fs-xs)' }}>
            <span className="truncate" style={{ minWidth: 0, flex: 1 }} title={m.model_name}>
              {m.model_name}
            </span>
            <span className="muted-xs">{formatNumber(m.request_count)} 次</span>
          </div>
          <div className="progress">
            <div className="progress-fill" style={{ width: `${Math.max(2, (m.request_count / max) * 100)}%` }} />
          </div>
        </div>
      ))}
    </div>
  )
}

export function StatisticsPage() {
  const [range, setRange] = useState<RangeKey>('24h')
  const active = RANGES.find((r) => r.key === range) ?? RANGES[1]

  const summary = useQuery({
    queryKey: ['stats-summary', active.hours],
    queryFn: () => getStatsSummary(active.hours),
    refetchInterval: 30_000,
  })
  const daily = useQuery({
    queryKey: ['stats-daily', active.days],
    queryFn: () => getStatsDaily(active.days),
    refetchInterval: 60_000,
  })
  // 模型统计始终按与范围一致的小时窗口
  const models = useQuery({
    queryKey: ['stats-models', active.hours],
    queryFn: () => getStatsModels(active.hours),
    refetchInterval: 60_000,
  })

  const loading = summary.isPending
  const error = summary.error ?? models.error

  const s = summary.data
  const hit = s?.cache_hit_tokens ?? 0
  const miss = s?.cache_miss_tokens ?? 0
  const hitRate = hit + miss > 0 ? `${((hit / (hit + miss)) * 100).toFixed(1)}%` : '—'

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>统计</h1>
          <p className="page-sub">请求量 / tokens / 缓存 / 成本（范围：{active.label}）</p>
        </div>
        <div className="page-actions">
          <div className="tabs" style={{ borderBottom: 'none' }}>
            {RANGES.map((r) => (
              <button
                key={r.key}
                className={range === r.key ? 'tab active' : 'tab'}
                onClick={() => setRange(r.key)}
              >
                {r.label}
              </button>
            ))}
          </div>
          <button
            className="btn btn-secondary btn-sm"
            onClick={() => {
              void summary.refetch()
              void daily.refetch()
              void models.refetch()
            }}
          >
            <RefreshCw /> 刷新
          </button>
        </div>
      </div>

      {error ? <Banner kind="err">{error instanceof Error ? error.message : '统计数据获取失败'}</Banner> : null}
      {loading ? <Loading /> : null}

      {s ? (
        <div className="grid grid-metrics">
          <Metric
            label="请求总数"
            value={formatNumber(s.request_count)}
            detail={`平均耗时 ${((s.avg_response_time ?? 0) * 1000).toFixed(0)} ms`}
            icon={Zap}
          />
          <Metric
            label="总 tokens"
            value={compactNumber(s.total_tokens)}
            detail={`输入 ${compactNumber(s.prompt_tokens)} · 输出 ${compactNumber(s.completion_tokens)}`}
            icon={BarChart3}
          />
          <Metric
            label="缓存命中率"
            value={hitRate}
            detail={`命中 ${compactNumber(hit)} / 未命中 ${compactNumber(miss)}`}
            icon={Inbox}
          />
          <Metric
            label="总成本"
            value={(s.total_cost ?? 0).toFixed(4)}
            detail="按模型 price_in / price_out 累计"
            icon={Coins}
          />
        </div>
      ) : null}

      <div className="grid grid-2">
        <div className="card">
          <div className="card-head">
            <h3>近 {active.days} 天请求量</h3>
          </div>
          <div className="card-body">
            {daily.isPending ? <Loading /> : daily.error ? (
              <Banner kind="err">{daily.error instanceof Error ? daily.error.message : '日统计获取失败'}</Banner>
            ) : (
              <DailyBars data={daily.data ?? []} />
            )}
          </div>
        </div>
        <div className="card">
          <div className="card-head">
            <h3>模型排行</h3>
            <span className="spacer" />
            <span className="muted-xs">{models.data?.length ?? 0} 个模型</span>
          </div>
          <div className="card-body">
            {models.isPending ? <Loading /> : <ModelRank models={models.data ?? []} />}
          </div>
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h3>模型明细</h3>
          <span className="spacer" />
          <span className="muted-xs">范围 {active.label}</span>
        </div>
        <div className="card-body-flush">
          {models.isPending ? (
            <Loading />
          ) : (models.data?.length ?? 0) === 0 ? (
            <Empty icon={<Inbox />} text="暂无模型调用数据" />
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>模型</th>
                    <th className="num">请求数</th>
                    <th className="num">输入</th>
                    <th className="num">输出</th>
                    <th className="num">缓存命中</th>
                    <th className="num">平均耗时(ms)</th>
                    <th className="num">成本</th>
                  </tr>
                </thead>
                <tbody>
                  {(models.data ?? []).map((m) => (
                    <tr key={m.model_name}>
                      <td className="truncate" style={{ maxWidth: 220 }} title={m.model_name}>
                        {m.model_name}
                      </td>
                      <td className="num">{formatNumber(m.request_count)}</td>
                      <td className="num">{compactNumber(m.input_tokens)}</td>
                      <td className="num">{compactNumber(m.output_tokens)}</td>
                      <td className="num">{compactNumber(m.cache_hit_tokens)}</td>
                      <td className="num">{((m.avg_response_time ?? 0) * 1000).toFixed(0)}</td>
                      <td className="num">{(m.total_cost ?? 0).toFixed(4)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
