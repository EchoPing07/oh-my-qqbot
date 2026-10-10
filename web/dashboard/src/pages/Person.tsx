/**
 * 人物页 — 人物档案列表 / 认识度统计 / 删除。
 * 对齐后端：GET /person/stats/summary、GET /person/list、DELETE /person/{person_id}。
 */

import { useState } from 'react'
import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query'
import { RefreshCw, Search, Trash2, UserCheck, Users } from 'lucide-react'

import { deletePerson, getPersonStats, getPersons, type PersonItem } from '@/api/resources'
import { Banner, ConfirmModal, Empty, Loading, Pagination, TextInput } from '@/components/ui'
import { formatNumber, formatRelative } from '@/lib/format'
import { toast } from '@/lib/toast'

const PAGE_SIZE = 20

/** 统一提取错误文案（ApiError 继承自 Error）。 */
function errText(err: unknown, fallback: string): string {
  return err instanceof Error && err.message ? err.message : fallback
}

/**
 * 列表接口不返回 total，用当前页条数推断分页总数：
 * 满页 → 视为可能还有下一页；空页 → 保留一条以便回退上一页。
 */
function inferTotal(count: number, page: number): number {
  if (count === 0 && page > 1) return (page - 1) * PAGE_SIZE + 1
  if (count < PAGE_SIZE) return (page - 1) * PAGE_SIZE + count
  return page * PAGE_SIZE + 1
}

/** 兼容后端实际字段 last_known_time（前端类型声明为 last_known）。 */
function personLastKnown(p: PersonItem): string | undefined {
  return (p as PersonItem & { last_known_time?: string }).last_known_time ?? p.last_known
}

/** person_name / user_nickname 合并展示，均为空时退化为 person_id 前 8 位。 */
function personLabel(p: PersonItem): string {
  const name = p.person_name?.trim()
  const nick = p.user_nickname?.trim()
  if (name && nick && name !== nick) return `${name}（${nick}）`
  return name || nick || p.person_id.slice(0, 8)
}

export function PersonPage() {
  const qc = useQueryClient()
  const [page, setPage] = useState(1)
  const [keyword, setKeyword] = useState('')
  const [pendingDelete, setPendingDelete] = useState<PersonItem | null>(null)
  const [deleting, setDeleting] = useState(false)

  // 分页拉取：每页 20 条，offset 由页码换算
  const listQuery = useQuery({
    queryKey: ['person-list', page],
    queryFn: () => getPersons(PAGE_SIZE, (page - 1) * PAGE_SIZE),
    placeholderData: keepPreviousData,
  })
  const statsQuery = useQuery({ queryKey: ['person-stats'], queryFn: getPersonStats })

  const items = listQuery.data?.items ?? []
  // 前端即时过滤：名称 / 昵称 / 用户 ID 包含匹配（不区分大小写）
  const kw = keyword.trim().toLowerCase()
  const filtered = kw
    ? items.filter((p) =>
        [p.person_name, p.user_nickname, p.user_id].some((v) => v?.toLowerCase().includes(kw))
      )
    : items

  // 分页总数优先用统计接口真实值，缺失时退化为本页推断
  const total = statsQuery.data?.total ?? inferTotal(items.length, page)
  const knownRate =
    statsQuery.data && statsQuery.data.total > 0
      ? Math.round((statsQuery.data.known / statsQuery.data.total) * 100)
      : 0

  // 删除人物：确认后调用 DELETE /person/{person_id}，成功后刷新列表与统计
  const confirmDelete = async () => {
    const target = pendingDelete
    if (!target) return
    setDeleting(true)
    try {
      await deletePerson(target.person_id)
      toast.ok(`已删除「${personLabel(target)}」`)
      setPendingDelete(null)
      // 当前页删空时回退一页，避免停留在空页
      if (items.length === 1 && page > 1) setPage(page - 1)
      await Promise.all([
        qc.invalidateQueries({ queryKey: ['person-list'] }),
        qc.invalidateQueries({ queryKey: ['person-stats'] }),
      ])
    } catch (err) {
      toast.err(errText(err, '删除人物失败'))
    } finally {
      setDeleting(false)
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>人物</h1>
          <p className="page-sub">机器人认识的人、认识次数与记忆点统计</p>
        </div>
        <div className="page-actions">
          <button
            className="btn btn-secondary btn-sm"
            disabled={listQuery.isFetching || statsQuery.isFetching}
            onClick={() => {
              void listQuery.refetch()
              void statsQuery.refetch()
            }}
          >
            {listQuery.isFetching ? <span className="spin" /> : <RefreshCw />} 刷新
          </button>
        </div>
      </div>

      {/* 指标卡：GET /person/stats/summary */}
      <div className="grid grid-metrics">
        <div className="metric">
          <div className="metric-head">
            <span className="metric-label">人物总数</span>
            <Users />
          </div>
          <div className="metric-value">
            {statsQuery.isPending ? '—' : formatNumber(statsQuery.data?.total ?? 0)}
          </div>
          <p className="metric-detail">已建档的平台用户数</p>
        </div>
        <div className="metric">
          <div className="metric-head">
            <span className="metric-label">已认识</span>
            <UserCheck />
          </div>
          <div className="metric-value">
            {statsQuery.isPending ? '—' : formatNumber(statsQuery.data?.known ?? 0)}
          </div>
          <p className="metric-detail">占人物总数 {knownRate}%</p>
        </div>
      </div>

      {statsQuery.isError ? (
        <Banner kind="err">统计加载失败：{errText(statsQuery.error, '未知错误')}</Banner>
      ) : null}
      {listQuery.isError ? (
        <Banner kind="err">列表加载失败：{errText(listQuery.error, '未知错误')}</Banner>
      ) : null}

      <div className="card">
        <div className="card-head">
          <h3>人物列表</h3>
          <span className="muted-xs">
            {kw ? `匹配 ${filtered.length} / 本页 ${items.length} 条` : `本页 ${items.length} 条`}
          </span>
          <span className="spacer" />
          <div style={{ width: 230, maxWidth: '100%', position: 'relative' }}>
            <Search
              size={13}
              style={{ position: 'absolute', left: 9, top: 9, color: 'var(--muted-fg)' }}
            />
            <TextInput
              aria-label="搜索人物"
              placeholder="搜索名称 / 昵称 / 用户 ID"
              value={keyword}
              onChange={(e) => setKeyword(e.target.value)}
              style={{ paddingLeft: 27 }}
            />
          </div>
        </div>
        <div className="card-body-flush">
          {listQuery.isPending ? (
            <Loading text="加载人物列表…" />
          ) : (
            <>
              {filtered.length === 0 ? (
                <Empty icon={<Users />} text={kw ? '本页没有匹配的人物' : '暂无人物数据'} />
              ) : (
                <div className="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>人物</th>
                        <th>平台</th>
                        <th>用户 ID</th>
                        <th className="num">认识次数</th>
                        <th className="num">记忆点数</th>
                        <th>状态</th>
                        <th>最近认识</th>
                        <th style={{ width: 84 }} />
                      </tr>
                    </thead>
                    <tbody>
                      {filtered.map((p) => (
                        <tr key={p.person_id}>
                          <td>
                            <span
                              className="truncate"
                              style={{ display: 'inline-block', maxWidth: 220, verticalAlign: 'bottom' }}
                              title={personLabel(p)}
                            >
                              {personLabel(p)}
                            </span>
                          </td>
                          <td>{p.platform || '—'}</td>
                          <td>
                            <span className="mono" title={p.user_id || undefined}>
                              {p.user_id || '—'}
                            </span>
                          </td>
                          <td className="num">{formatNumber(p.know_times)}</td>
                          <td className="num">{formatNumber(p.memory_points)}</td>
                          <td>
                            {p.is_known ? (
                              <span className="badge badge-ok">已认识</span>
                            ) : (
                              <span className="badge">陌生</span>
                            )}
                          </td>
                          <td className="muted-xs" title={personLastKnown(p)}>
                            {formatRelative(personLastKnown(p))}
                          </td>
                          <td style={{ textAlign: 'right' }}>
                            <button
                              className="btn btn-danger btn-sm"
                              title="删除该人物"
                              onClick={() => setPendingDelete(p)}
                            >
                              <Trash2 /> 删除
                            </button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
              {total > PAGE_SIZE ? (
                <div style={{ padding: '8px 12px', borderTop: '1px solid var(--border)' }}>
                  <Pagination page={page} pageSize={PAGE_SIZE} total={total} onPage={setPage} />
                </div>
              ) : null}
            </>
          )}
        </div>
      </div>

      <ConfirmModal
        open={pendingDelete !== null}
        title="删除人物"
        message={
          pendingDelete
            ? `确定删除「${personLabel(pendingDelete)}」吗？该操作不可撤销。`
            : undefined
        }
        confirmText="删除"
        danger
        loading={deleting}
        onConfirm={() => void confirmDelete()}
        onClose={() => {
          if (!deleting) setPendingDelete(null)
        }}
      />
    </div>
  )
}
