/**
 * 表达方式页 — 表达样本列表 / 人工审核 / 删除。
 * 对齐后端：GET /expression/list、PUT /expression/{id}/check、DELETE /expression/{id}。
 */

import { useState } from 'react'
import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query'
import { Check, ClipboardList, RefreshCw, ShieldCheck, Trash2 } from 'lucide-react'

import { checkExpression, deleteExpression, getExpressions, type ExpressionItem } from '@/api/resources'
import { Banner, ConfirmModal, Empty, Loading, Pagination } from '@/components/ui'
import { cn } from '@/lib/cn'
import { formatNumber, truncate } from '@/lib/format'
import { toast } from '@/lib/toast'

const PAGE_SIZE = 20

/** 审核筛选三态。 */
type CheckFilter = 'all' | 'pending' | 'checked'

const FILTER_TABS: Array<{ key: CheckFilter; label: string }> = [
  { key: 'all', label: '全部' },
  { key: 'pending', label: '待审核' },
  { key: 'checked', label: '已审核' },
]

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

/** 后端实际返回 modified_by（前端类型声明为 created_by），二者取先出现的值。 */
function expressionAuthor(e: ExpressionItem): string {
  return e.created_by || e.modified_by || '—'
}

export function ExpressionPage() {
  const qc = useQueryClient()
  const [page, setPage] = useState(1)
  const [filter, setFilter] = useState<CheckFilter>('all')
  const [checkingId, setCheckingId] = useState<number | null>(null)
  const [pendingDelete, setPendingDelete] = useState<ExpressionItem | null>(null)
  const [deleting, setDeleting] = useState(false)

  // 分页拉取：每页 20 条，offset 由页码换算
  const listQuery = useQuery({
    queryKey: ['expression-list', page],
    queryFn: () => getExpressions(PAGE_SIZE, (page - 1) * PAGE_SIZE),
    placeholderData: keepPreviousData,
  })

  const items = listQuery.data ?? []
  // 后端未提供 checked_only 之外的筛选参数，三态在本页内即时过滤
  const filtered = items.filter((e) => {
    if (filter === 'pending') return !e.checked
    if (filter === 'checked') return e.checked
    return true
  })

  const total = inferTotal(items.length, page)

  // 审核切换：PUT /expression/{id}/check（checked + modified_by=USER）
  const toggleCheck = async (item: ExpressionItem) => {
    setCheckingId(item.id)
    try {
      await checkExpression(item.id, !item.checked)
      toast.ok(item.checked ? `已取消审核 #${item.id}` : `已审核 #${item.id}`)
      await qc.invalidateQueries({ queryKey: ['expression-list'] })
    } catch (err) {
      toast.err(errText(err, '更新审核状态失败'))
    } finally {
      setCheckingId(null)
    }
  }

  // 删除表达：确认后调用 DELETE /expression/{id}
  const confirmDelete = async () => {
    const target = pendingDelete
    if (!target) return
    setDeleting(true)
    try {
      await deleteExpression(target.id)
      toast.ok(`已删除表达 #${target.id}`)
      setPendingDelete(null)
      if (items.length === 1 && page > 1) setPage(page - 1)
      await qc.invalidateQueries({ queryKey: ['expression-list'] })
    } catch (err) {
      toast.err(errText(err, '删除表达失败'))
    } finally {
      setDeleting(false)
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>表达方式</h1>
          <p className="page-sub">机器人在不同情境下学到的表达，可人工审核或清理</p>
        </div>
        <div className="page-actions">
          <button
            className="btn btn-secondary btn-sm"
            disabled={listQuery.isFetching}
            onClick={() => void listQuery.refetch()}
          >
            {listQuery.isFetching ? <span className="spin" /> : <RefreshCw />} 刷新
          </button>
        </div>
      </div>

      {listQuery.isError ? (
        <Banner kind="err">列表加载失败：{errText(listQuery.error, '未知错误')}</Banner>
      ) : null}

      <div className="card">
        <div className="card-head">
          <h3>表达列表</h3>
          <span className="muted-xs">
            共 {total} 条 · 本页 {items.length} 条
            {filter !== 'all' ? ` · 筛出 ${filtered.length} 条` : ''}
          </span>
        </div>

        <div className="tabs" style={{ padding: '0 8px' }}>
          {FILTER_TABS.map((t) => (
            <button
              key={t.key}
              className={cn('tab', filter === t.key && 'active')}
              onClick={() => setFilter(t.key)}
            >
              {t.label}
            </button>
          ))}
        </div>

        <div className="card-body-flush">
          {listQuery.isPending ? (
            <Loading text="加载表达列表…" />
          ) : (
            <>
              {filtered.length === 0 ? (
                <Empty
                  icon={<ClipboardList />}
                  text={filter === 'all' ? '暂无表达数据' : '本页没有符合筛选条件的表达'}
                />
              ) : (
                <div className="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>情境</th>
                        <th>表达</th>
                        <th>风格</th>
                        <th className="num">使用次数</th>
                        <th>审核状态</th>
                        <th>创建者</th>
                        <th style={{ width: 150 }} />
                      </tr>
                    </thead>
                    <tbody>
                      {filtered.map((e) => (
                        <tr key={e.id}>
                          <td>
                            <span
                              className="truncate"
                              style={{ display: 'inline-block', maxWidth: 200, verticalAlign: 'bottom' }}
                              title={e.situation || undefined}
                            >
                              {e.situation || '—'}
                            </span>
                          </td>
                          <td>
                            <span title={e.expression || undefined}>{truncate(e.expression || '—', 60)}</span>
                          </td>
                          <td>{e.style || '—'}</td>
                          <td className="num">{formatNumber(e.count)}</td>
                          <td>
                            {e.checked ? (
                              <span className="badge badge-ok">已审核</span>
                            ) : (
                              <span className="badge badge-warn">待审核</span>
                            )}
                          </td>
                          <td className="muted-xs">{expressionAuthor(e)}</td>
                          <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                            <button
                              className="btn btn-ghost btn-sm"
                              disabled={checkingId === e.id}
                              title={e.checked ? '取消审核标记' : '标记为已审核'}
                              onClick={() => void toggleCheck(e)}
                            >
                              {checkingId === e.id ? (
                                <span className="spin" />
                              ) : e.checked ? (
                                <ShieldCheck />
                              ) : (
                                <Check />
                              )}
                              {e.checked ? '取消审核' : '通过审核'}
                            </button>
                            <button
                              className="btn btn-danger btn-sm"
                              style={{ marginLeft: 6 }}
                              title="删除该表达"
                              onClick={() => setPendingDelete(e)}
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
        title="删除表达"
        message={
          pendingDelete
            ? `确定删除表达 #${pendingDelete.id}（${truncate(pendingDelete.expression || pendingDelete.situation || '无内容', 40)}）吗？该操作不可撤销。`
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
