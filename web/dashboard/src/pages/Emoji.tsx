/**
 * 表情包页 — 表情缓存列表 / 禁用或恢复 / 删除 / 详情。
 * 对齐后端：GET /emoji/list、GET /emoji/{hash}、PUT /emoji/{hash}/ban、DELETE /emoji/{hash}。
 * 注意：后端没有图片预览端点（full_path 是服务器本地路径），本页只展示文本信息，绝不加载图片。
 */

import { useState } from 'react'
import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query'
import { Ban, Eye, ImageOff, RefreshCw, RotateCcw, Smile, Trash2 } from 'lucide-react'

import {
  banEmoji,
  deleteEmoji,
  getEmojiDetail,
  getEmojis,
  type EmojiItem,
} from '@/api/resources'
import { Banner, ConfirmModal, Empty, Loading, Modal, Pagination } from '@/components/ui'
import { formatDateTime, formatNumber, truncate } from '@/lib/format'
import { toast } from '@/lib/toast'

const PAGE_SIZE = 20
/** 列表里的哈希只取前 12 位展示，完整值放在 title 中。 */
const HASH_PREVIEW = 12

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

function yesNo(v: boolean, yes: string, no: string): string {
  return v ? yes : no
}

/** 详情面板的一格键值对。 */
function KV({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return (
    <div className="kv">
      <span className="k">{k}</span>
      <span className={mono ? 'v mono' : 'v'}>{v}</span>
    </div>
  )
}

export function EmojiPage() {
  const qc = useQueryClient()
  const [page, setPage] = useState(1)
  const [busyHash, setBusyHash] = useState<string | null>(null)
  const [detailHash, setDetailHash] = useState<string | null>(null)
  const [pendingDelete, setPendingDelete] = useState<EmojiItem | null>(null)
  const [deleting, setDeleting] = useState(false)

  // 分页拉取：每页 20 条，offset 由页码换算
  const listQuery = useQuery({
    queryKey: ['emoji-list', page],
    queryFn: () => getEmojis(PAGE_SIZE, (page - 1) * PAGE_SIZE),
    placeholderData: keepPreviousData,
  })

  // 详情：按需拉取单条完整记录（GET /emoji/{hash}）
  const detailQuery = useQuery({
    queryKey: ['emoji-detail', detailHash],
    queryFn: () => getEmojiDetail(detailHash as string),
    enabled: detailHash !== null,
  })

  const items = listQuery.data ?? []
  const total = inferTotal(items.length, page)
  const detailRow = items.find((e) => e.hash === detailHash) ?? null
  // 弹窗内展示的数据：优先用详情接口结果，加载期间回退到列表行
  const detail = detailQuery.data ?? detailRow

  const refresh = () => qc.invalidateQueries({ queryKey: ['emoji-list'] })

  // 禁用 / 恢复：PUT /emoji/{hash}/ban {banned}
  const toggleBan = async (item: EmojiItem) => {
    setBusyHash(item.hash)
    try {
      await banEmoji(item.hash, !item.is_banned)
      toast.ok(item.is_banned ? '已恢复该表情' : '已禁用该表情')
      await refresh()
    } catch (err) {
      toast.err(errText(err, '更新表情状态失败'))
    } finally {
      setBusyHash(null)
    }
  }

  // 删除表情记录：确认后调用 DELETE /emoji/{hash}
  const confirmDelete = async () => {
    const target = pendingDelete
    if (!target) return
    setDeleting(true)
    try {
      await deleteEmoji(target.hash)
      toast.ok(`已删除表情 ${target.hash.slice(0, HASH_PREVIEW)}`)
      setPendingDelete(null)
      if (detailHash === target.hash) setDetailHash(null)
      if (items.length === 1 && page > 1) setPage(page - 1)
      await refresh()
    } catch (err) {
      toast.err(errText(err, '删除表情失败'))
    } finally {
      setDeleting(false)
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>表情包</h1>
          <p className="page-sub">机器人的表情缓存与 VLM 描述，可禁用或清理</p>
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

      <Banner kind="info" icon={<ImageOff />}>
        为避免额外磁盘开销，本页不加载表情图片；文件路径（full_path）仅为服务器本地路径。
      </Banner>

      {listQuery.isError ? (
        <Banner kind="err">列表加载失败：{errText(listQuery.error, '未知错误')}</Banner>
      ) : null}

      <div className="card">
        <div className="card-head">
          <h3>表情列表</h3>
          <span className="muted-xs">
            共 {total} 条 · 本页 {items.length} 条
          </span>
        </div>
        <div className="card-body-flush">
          {listQuery.isPending ? (
            <Loading text="加载表情列表…" />
          ) : (
            <>
              {items.length === 0 ? (
                <Empty icon={<Smile />} text="暂无表情数据" />
              ) : (
                <div className="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>哈希</th>
                        <th>描述</th>
                        <th className="num">查询次数</th>
                        <th>登记</th>
                        <th>状态</th>
                        <th>VLM</th>
                        <th>文件</th>
                        <th>注册时间</th>
                        <th style={{ width: 200 }} />
                      </tr>
                    </thead>
                    <tbody>
                      {items.map((e) => (
                        <tr key={e.hash}>
                          <td>
                            <span className="mono" title={e.hash}>
                              {e.hash.slice(0, HASH_PREVIEW)}
                            </span>
                          </td>
                          <td>
                            <span
                              title={e.description || undefined}
                              className={e.description ? undefined : 'muted-xs'}
                            >
                              {e.description ? truncate(e.description, 60) : '无描述'}
                            </span>
                          </td>
                          <td className="num">{formatNumber(e.query_count)}</td>
                          <td>
                            {e.is_registered ? (
                              <span className="badge badge-ok">已登记</span>
                            ) : (
                              <span className="muted-xs">未登记</span>
                            )}
                          </td>
                          <td>
                            {e.is_banned ? (
                              <span className="badge badge-err">已禁用</span>
                            ) : (
                              <span className="muted-xs">正常</span>
                            )}
                          </td>
                          <td>
                            {e.vlm_processed ? (
                              <span className="badge badge-ok">已描述</span>
                            ) : (
                              <span className="muted-xs">无描述</span>
                            )}
                          </td>
                          <td>
                            {e.no_file ? (
                              <span className="badge badge-warn">文件缺失</span>
                            ) : (
                              <span className="muted-xs">正常</span>
                            )}
                          </td>
                          <td className="muted-xs">{formatDateTime(e.register_time)}</td>
                          <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                            <button
                              className="btn btn-ghost btn-sm"
                              title="查看详情"
                              onClick={() => setDetailHash(e.hash)}
                            >
                              <Eye /> 详情
                            </button>
                            <button
                              className="btn btn-ghost btn-sm"
                              disabled={busyHash === e.hash}
                              title={e.is_banned ? '恢复使用' : '禁用该表情'}
                              onClick={() => void toggleBan(e)}
                            >
                              {busyHash === e.hash ? (
                                <span className="spin" />
                              ) : e.is_banned ? (
                                <RotateCcw />
                              ) : (
                                <Ban />
                              )}
                              {e.is_banned ? '恢复' : '禁用'}
                            </button>
                            <button
                              className="btn btn-danger btn-sm"
                              style={{ marginLeft: 6 }}
                              title="删除该表情记录"
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

      {/* 详情弹窗：全部字段以 kv-grid 展示 */}
      <Modal
        open={detailHash !== null}
        title="表情详情"
        onClose={() => setDetailHash(null)}
        width={620}
        footer={
          <button className="btn btn-secondary btn-sm" onClick={() => setDetailHash(null)}>
            关闭
          </button>
        }
      >
        {detailQuery.isPending && !detailRow ? (
          <Loading text="加载详情…" />
        ) : detailQuery.isError ? (
          <Banner kind="err">详情加载失败：{errText(detailQuery.error, '未知错误')}</Banner>
        ) : null}
        <div className="kv-grid">
          <KV k="哈希" v={detail?.hash ?? '—'} mono />
          <KV k="描述" v={detail?.description || '无描述'} />
          <KV k="查询次数" v={formatNumber(detail?.query_count ?? 0)} />
          <KV k="已登记" v={yesNo(detail?.is_registered ?? false, '是', '否')} />
          <KV k="已禁用" v={yesNo(detail?.is_banned ?? false, '是', '否')} />
          <KV k="VLM 已描述" v={yesNo(detail?.vlm_processed ?? false, '是', '否')} />
          <KV k="文件缺失" v={yesNo(detail?.no_file ?? false, '是', '否')} />
          <KV k="注册时间" v={formatDateTime(detail?.register_time)} />
          <KV k="完整路径（服务器本地）" v={detail?.full_path || '未记录'} mono />
        </div>
        {detailRow?.no_file ? (
          <Banner kind="warn" icon={<ImageOff />}>
            该表情在服务器上缺少对应文件，无法参与发送。
          </Banner>
        ) : null}
      </Modal>

      <ConfirmModal
        open={pendingDelete !== null}
        title="删除表情"
        message={
          pendingDelete
            ? `确定删除表情 ${pendingDelete.hash.slice(0, HASH_PREVIEW)}（${pendingDelete.description || '无描述'}）的记录吗？该操作不可撤销。`
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
