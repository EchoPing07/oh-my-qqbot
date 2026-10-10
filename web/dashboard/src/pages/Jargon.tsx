/**
 * 黑话页 — 黑话候选列表 / 人工确认或否认 / 含义编辑 / 删除。
 * 对齐后端：GET /jargon/list、PUT /jargon/{id}、DELETE /jargon/{id}。
 */

import { useState } from 'react'
import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query'
import { Ban, Check, MessagesSquare, Pencil, RefreshCw, Trash2 } from 'lucide-react'

import { deleteJargon, getJargons, updateJargon, type JargonItem } from '@/api/resources'
import {
  Banner,
  ConfirmModal,
  Empty,
  Loading,
  Modal,
  Pagination,
  TextArea,
} from '@/components/ui'
import { formatNumber, truncate } from '@/lib/format'
import { toast } from '@/lib/toast'

const PAGE_SIZE = 20

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

/** 后端实际返回 content（前端类型声明为 jargon），二者取先出现的值。 */
function jargonText(j: JargonItem): string {
  const raw = j.jargon ?? (j as JargonItem & { content?: string }).content
  return raw?.trim() || '—'
}

/** 含义优先取 meaning，退化到 explanation。 */
function jargonMeaning(j: JargonItem): string {
  return (j.meaning ?? j.explanation ?? '').trim()
}

export function JargonPage() {
  const qc = useQueryClient()
  const [page, setPage] = useState(1)
  const [busyId, setBusyId] = useState<number | null>(null)
  const [editing, setEditing] = useState<JargonItem | null>(null)
  const [meaningDraft, setMeaningDraft] = useState('')
  const [saving, setSaving] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<JargonItem | null>(null)
  const [deleting, setDeleting] = useState(false)

  // 分页拉取：每页 20 条，offset 由页码换算
  const listQuery = useQuery({
    queryKey: ['jargon-list', page],
    queryFn: () => getJargons(PAGE_SIZE, (page - 1) * PAGE_SIZE),
    placeholderData: keepPreviousData,
  })

  const items = listQuery.data ?? []
  const total = inferTotal(items.length, page)

  const refresh = () => qc.invalidateQueries({ queryKey: ['jargon-list'] })

  // 确认 / 否认：PUT /jargon/{id} {is_jargon, meaning}
  const setJargonFlag = async (item: JargonItem, isJargon: boolean) => {
    setBusyId(item.id)
    try {
      const meaning = jargonMeaning(item)
      await updateJargon(item.id, {
        is_jargon: isJargon,
        ...(meaning ? { meaning } : {}),
      })
      toast.ok(isJargon ? `已确认「${jargonText(item)}」为黑话` : `已否认「${jargonText(item)}」为黑话`)
      await refresh()
    } catch (err) {
      toast.err(errText(err, '更新黑话状态失败'))
    } finally {
      setBusyId(null)
    }
  }

  // 打开含义编辑弹窗（保存时仍带上原 is_jargon，避免误改状态）
  const openEditor = (item: JargonItem) => {
    setEditing(item)
    setMeaningDraft(jargonMeaning(item))
  }

  const saveMeaning = async () => {
    const target = editing
    if (!target) return
    setSaving(true)
    try {
      await updateJargon(target.id, { is_jargon: target.is_jargon, meaning: meaningDraft.trim() })
      toast.ok('含义已保存')
      setEditing(null)
      await refresh()
    } catch (err) {
      toast.err(errText(err, '保存含义失败'))
    } finally {
      setSaving(false)
    }
  }

  // 删除黑话：确认后调用 DELETE /jargon/{id}
  const confirmDelete = async () => {
    const target = pendingDelete
    if (!target) return
    setDeleting(true)
    try {
      await deleteJargon(target.id)
      toast.ok(`已删除「${jargonText(target)}」`)
      setPendingDelete(null)
      if (items.length === 1 && page > 1) setPage(page - 1)
      await refresh()
    } catch (err) {
      toast.err(errText(err, '删除黑话失败'))
    } finally {
      setDeleting(false)
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>黑话</h1>
          <p className="page-sub">机器人收集的群聊黑话候选，可人工确认、修订含义或清理</p>
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
          <h3>黑话列表</h3>
          <span className="muted-xs">
            共 {total} 条 · 本页 {items.length} 条
          </span>
        </div>
        <div className="card-body-flush">
          {listQuery.isPending ? (
            <Loading text="加载黑话列表…" />
          ) : (
            <>
              {items.length === 0 ? (
                <Empty icon={<MessagesSquare />} text="暂无黑话数据" />
              ) : (
                <div className="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>黑话</th>
                        <th>含义</th>
                        <th className="num">出现次数</th>
                        <th>判定</th>
                        <th>来源</th>
                        <th style={{ width: 210 }} />
                      </tr>
                    </thead>
                    <tbody>
                      {items.map((j) => (
                        <tr key={j.id}>
                          <td>
                            <span
                              className="truncate"
                              style={{ display: 'inline-block', maxWidth: 160, verticalAlign: 'bottom', fontWeight: 500 }}
                              title={jargonText(j)}
                            >
                              {jargonText(j)}
                            </span>
                          </td>
                          <td>
                            <span
                              title={jargonMeaning(j) || undefined}
                              className={jargonMeaning(j) ? undefined : 'muted-xs'}
                            >
                              {jargonMeaning(j) ? truncate(jargonMeaning(j), 60) : '暂无含义'}
                            </span>
                          </td>
                          <td className="num">{formatNumber(j.count)}</td>
                          <td>
                            {j.is_jargon ? (
                              <span className="badge badge-ok">已确认黑话</span>
                            ) : (
                              <span className="badge">候选</span>
                            )}
                          </td>
                          <td className="muted-xs">{j.created_by || '—'}</td>
                          <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                            {j.is_jargon ? (
                              <button
                                className="btn btn-ghost btn-sm"
                                disabled={busyId === j.id}
                                title="否认这是一条黑话"
                                onClick={() => void setJargonFlag(j, false)}
                              >
                                {busyId === j.id ? <span className="spin" /> : <Ban />} 否认
                              </button>
                            ) : (
                              <button
                                className="btn btn-ghost btn-sm"
                                disabled={busyId === j.id}
                                title="确认这是一条黑话"
                                onClick={() => void setJargonFlag(j, true)}
                              >
                                {busyId === j.id ? <span className="spin" /> : <Check />} 确认
                              </button>
                            )}
                            <button
                              className="btn btn-ghost btn-sm"
                              title="编辑含义"
                              onClick={() => openEditor(j)}
                            >
                              <Pencil /> 含义
                            </button>
                            <button
                              className="btn btn-danger btn-sm"
                              style={{ marginLeft: 6 }}
                              title="删除该黑话"
                              onClick={() => setPendingDelete(j)}
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

      {/* 含义行内编辑弹窗：保存走 PUT /jargon/{id} {meaning} */}
      <Modal
        open={editing !== null}
        title={`编辑含义 · ${editing ? jargonText(editing) : ''}`}
        description="保存后会标记来源为 MANUAL。"
        onClose={() => {
          if (!saving) setEditing(null)
        }}
        footer={
          <>
            <button
              className="btn btn-secondary btn-sm"
              disabled={saving}
              onClick={() => setEditing(null)}
            >
              取消
            </button>
            <button className="btn btn-sm" disabled={saving} onClick={() => void saveMeaning()}>
              {saving ? <span className="spin" /> : null} 保存
            </button>
          </>
        }
      >
        <label className="label" htmlFor="jargon-meaning">
          含义
        </label>
        <TextArea
          id="jargon-meaning"
          placeholder="填写这条黑话的含义…"
          value={meaningDraft}
          onChange={(e) => setMeaningDraft(e.target.value)}
          style={{ minHeight: 96 }}
        />
      </Modal>

      <ConfirmModal
        open={pendingDelete !== null}
        title="删除黑话"
        message={
          pendingDelete
            ? `确定删除「${jargonText(pendingDelete)}」吗？该操作不可撤销。`
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
