/**
 * 长期记忆 页 — 记忆检索 + 人物画像。
 * 数据源：POST /memory/search、GET /memory/profiles、GET /memory/profiles/{person_id}。
 *
 * hits / profile 字段由内核动态决定，页面用 kv-grid 逐字段展示（跳过 null/undefined）。
 */

import { useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { Brain, Inbox, Search, UserRound } from 'lucide-react'

import {
  getMemoryProfile,
  getMemoryProfiles,
  searchMemory,
  type MemoryHit,
  type MemoryProfile,
} from '@/api/misc'
import { Banner, Empty, Loading } from '@/components/ui'
import { cn } from '@/lib/cn'

const TOP_K_OPTIONS = [5, 10, 20] as const

/** 动态对象 → 可展示的键值对（跳过 null/undefined/空串）。 */
function toPairs(raw: Record<string, unknown>): Array<[string, string]> {
  const out: Array<[string, string]> = []
  for (const [key, value] of Object.entries(raw)) {
    if (value === null || value === undefined || value === '') continue
    let text: string
    if (typeof value === 'object') {
      try {
        text = JSON.stringify(value)
      } catch {
        text = String(value)
      }
    } else {
      text = String(value)
    }
    out.push([key, text])
  }
  return out
}

/** 通用动态字段网格。 */
function KvGrid({ data, emptyText }: { data: Record<string, unknown>; emptyText: string }) {
  const pairs = toPairs(data)
  if (pairs.length === 0) return <Empty text={emptyText} />
  return (
    <div className="kv-grid">
      {pairs.map(([k, v]) => (
        <div className="kv" key={k}>
          <span className="k">{k}</span>
          <span className="v">{v}</span>
        </div>
      ))}
    </div>
  )
}

export function MemoryPage() {
  const [tab, setTab] = useState<'search' | 'profiles'>('search')

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>长期记忆</h1>
          <p className="page-sub">向量记忆检索与人物画像（内核未启用时后端返回 503）</p>
        </div>
      </div>

      <div className="tabs">
        <button className={cn('tab', tab === 'search' && 'active')} onClick={() => setTab('search')}>
          <Search size={14} style={{ verticalAlign: -2 }} /> 记忆检索
        </button>
        <button className={cn('tab', tab === 'profiles' && 'active')} onClick={() => setTab('profiles')}>
          <UserRound size={14} style={{ verticalAlign: -2 }} /> 人物画像
        </button>
      </div>

      {tab === 'search' ? <SearchTab /> : <ProfilesTab />}
    </div>
  )
}

// ── Tab A：记忆检索 ──

function SearchTab() {
  const [query, setQuery] = useState('')
  const [chatID, setChatID] = useState('')
  const [topK, setTopK] = useState<number>(5)

  const search = useMutation({
    mutationFn: () => searchMemory(query.trim(), chatID.trim(), topK),
  })

  const hits = search.data ?? []
  const error = search.error

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div className="card">
        <div className="card-head">
          <h3>检索条件</h3>
        </div>
        <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
          <div>
            <label className="label" htmlFor="memory-query">检索词</label>
            <input
              id="memory-query"
              className="input"
              placeholder="输入关键词或问题…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.nativeEvent.isComposing) {
                  e.preventDefault()
                  if (query.trim()) search.mutate()
                }
              }}
            />
          </div>
          <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
            <div style={{ flex: 1, minWidth: 200 }}>
              <label className="label" htmlFor="memory-chat">会话 ID（可选）</label>
              <input
                id="memory-chat"
                className="input"
                placeholder="限定群聊/私聊会话，留空为全局"
                value={chatID}
                onChange={(e) => setChatID(e.target.value)}
              />
            </div>
            <div style={{ width: 120 }}>
              <label className="label" htmlFor="memory-topk">返回条数</label>
              <select
                id="memory-topk"
                className="select"
                value={topK}
                onChange={(e) => setTopK(Number(e.target.value))}
              >
                {TOP_K_OPTIONS.map((k) => (
                  <option key={k} value={k}>
                    top {k}
                  </option>
                ))}
              </select>
            </div>
          </div>
          <div>
            <button className="btn btn-sm" onClick={() => search.mutate()} disabled={search.isPending || !query.trim()}>
              {search.isPending ? <span className="spin" /> : <Search />} 检索
            </button>
          </div>
        </div>
      </div>

      {error ? (
        <Banner kind="err">{error instanceof Error ? error.message : '记忆检索失败'}</Banner>
      ) : null}

      <div className="card">
        <div className="card-head">
          <h3>命中结果</h3>
          <span className="spacer" />
          <span className="muted-xs">{hits.length} 条</span>
        </div>
        <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
          {search.isPending ? (
            <Loading text="检索中…" />
          ) : hits.length === 0 ? (
            <Empty icon={<Inbox />} text={search.isSuccess ? '没有命中结果' : '输入检索词开始检索'} />
          ) : (
            hits.map((hit, idx) => <HitCard key={hitKey(hit, idx)} hit={hit} index={idx} />)
          )}
        </div>
      </div>
    </div>
  )
}

/** hits 无稳定 id 时用下标兜底。 */
function hitKey(hit: MemoryHit, index: number): string {
  const id = typeof hit.id === 'string' ? hit.id : ''
  return id ? `id:${id}` : `idx:${index}`
}

function HitCard({ hit, index }: { hit: MemoryHit; index: number }) {
  const score = typeof hit.score === 'number' ? hit.score.toFixed(4) : null
  const content = typeof hit.content === 'string' ? hit.content : ''
  return (
    <div style={{ border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', padding: 10 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
        <span className="badge">#{index + 1}</span>
        {score ? <span className="badge badge-solid">score {score}</span> : null}
      </div>
      {content ? (
        <p className="sm" style={{ whiteSpace: 'pre-wrap', marginBottom: 8 }}>
          {content}
        </p>
      ) : null}
      <KvGrid data={hit} emptyText="该命中无字段" />
    </div>
  )
}

// ── Tab B：人物画像 ──

function ProfilesTab() {
  const [personID, setPersonID] = useState('')

  const profiles = useQuery({
    queryKey: ['memory-profiles'],
    queryFn: getMemoryProfiles,
    staleTime: 30_000,
  })

  const detail = useQuery({
    queryKey: ['memory-profile', personID],
    queryFn: () => getMemoryProfile(personID),
    enabled: Boolean(personID),
  })

  const list = profiles.data ?? []
  const error = profiles.error

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      {error ? (
        <Banner kind="err">{error instanceof Error ? error.message : '人物画像列表获取失败'}</Banner>
      ) : null}

      <div className="card">
        <div className="card-head">
          <h3>
            <UserRound size={15} style={{ verticalAlign: -2 }} /> 已建立画像的人物
          </h3>
          <span className="spacer" />
          <span className="muted-xs">{list.length} 人</span>
        </div>
        <div className="card-body-flush">
          {profiles.isPending ? (
            <Loading />
          ) : list.length === 0 ? (
            <Empty icon={<Inbox />} text="暂无人物画像" />
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>人物标识</th>
                    <th>名称</th>
                    <th className="num">认识次数</th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((p) => (
                    <tr
                      key={p.person_id}
                      onClick={() => setPersonID(p.person_id)}
                      style={{ cursor: 'pointer' }}
                    >
                      <td className="mono truncate" style={{ maxWidth: 220 }} title={p.person_id}>
                        {p.person_id}
                      </td>
                      <td>{profileName(p)}</td>
                      <td className="num">{typeof p.know_times === 'number' ? p.know_times : '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h3>
            <Brain size={15} style={{ verticalAlign: -2 }} /> 画像详情
          </h3>
          <span className="spacer" />
          {personID ? <span className="badge">{personID}</span> : null}
        </div>
        <div className="card-body">
          {!personID ? (
            <Empty text="点击上方人物查看完整画像" />
          ) : detail.isPending ? (
            <Loading text="加载画像…" />
          ) : detail.error ? (
            <Banner kind="err">{detail.error instanceof Error ? detail.error.message : '画像获取失败'}</Banner>
          ) : (
            <KvGrid data={(detail.data ?? {}) as Record<string, unknown>} emptyText="该人物画像暂无字段" />
          )}
        </div>
      </div>
    </div>
  )
}

/** 画像列表项的名称展示（字段动态，逐个兜底）。 */
function profileName(p: MemoryProfile): string {
  const name = p.person_name
  const nickname = p.user_nickname
  if (typeof name === 'string' && name) return name
  if (typeof nickname === 'string' && nickname) return nickname
  return '—'
}
