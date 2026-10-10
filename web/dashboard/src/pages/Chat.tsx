/**
 * 聊天管理 页 — 全平台会话台账浏览 + 会话消息历史查看（只读）。
 *
 * 与旧版「WebUI 内置聊天」的区别：本页不再提供发言入口（不发消息、不注入虚拟平台），
 * 只做「已发生对话」的检索与审计 —— 对齐后端 GET /chat/sessions、GET /chat/history、
 * GET /chat/platforms（均为只读查询面）。
 *
 * 交互：左侧会话列表（平台筛选 + 关键字搜索 + 刷新），右侧消息历史（按时间正序）。
 */

import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Inbox, MessageSquare, RefreshCw, Search, UserRound, Users } from 'lucide-react'

import { getChatHistory, getChatSessions, type ChatMessage, type ChatSession } from '@/api/chat'
import { Banner, Empty, Loading, TextInput } from '@/components/ui'
import { cn } from '@/lib/cn'
import { formatDateTime, formatNumber, sessionTitle } from '@/lib/format'

/** 会话列表拉取上限（后端 SQLite 全表，前端取最近活跃的一段）。 */
const SESSION_LIMIT = 200
/** 单个会话的历史消息拉取上限。 */
const HISTORY_LIMIT = 300

/** 平台筛选选项：全部 + 后端上报的平台名。 */
type PlatformFilter = 'all' | string

/** 会话身份展示：群聊显示群名/群号，私聊显示昵称/ID。 */
function sessionSubtitle(s: ChatSession): string {
  if (s.is_group) return s.group_id ? `群号 ${s.group_id}` : '群聊'
  return s.user_id ? `用户 ${s.user_id}` : '私聊'
}

/** 消息作者展示：机器人消息用统一署名，其余用昵称兜底用户 ID。 */
function authorLabel(msg: ChatMessage): string {
  const nickname = msg.user_nickname?.trim()
  if (nickname) return nickname
  const uid = msg.user_id?.trim()
  return uid ? uid.slice(0, 12) : '未知用户'
}

/** 一条消息是否为机器人发出（昵称/user_id 命中 bot 关键词）。 */
function isBotMessage(msg: ChatMessage): boolean {
  const nickname = (msg.user_nickname ?? '').toLowerCase()
  const uid = (msg.user_id ?? '').toLowerCase()
  return uid === 'bot' || nickname.includes('bot')
}

export function ChatPage() {
  const [activeID, setActiveID] = useState('')
  const [keyword, setKeyword] = useState('')
  const [platform, setPlatform] = useState<PlatformFilter>('all')

  const sessionsQuery = useQuery({
    queryKey: ['chat-sessions', SESSION_LIMIT],
    queryFn: () => getChatSessions(SESSION_LIMIT),
  })

  const sessions = useMemo(() => sessionsQuery.data ?? [], [sessionsQuery.data])

  // 平台下拉项：实际会话里出现过的平台
  const platformOptions = useMemo(() => {
    const seen = sessions.map((s) => s.platform).filter(Boolean)
    return Array.from(new Set(seen)).sort()
  }, [sessions])

  // 前端过滤：平台 + 关键字（会话标题 / 群名 / 用户 ID / 会话 ID）
  const filtered = useMemo(() => {
    const kw = keyword.trim().toLowerCase()
    return sessions.filter((s) => {
      if (platform !== 'all' && s.platform !== platform) return false
      if (!kw) return true
      return [sessionTitle(s), s.group_name, s.group_id, s.user_id, s.user_nickname, s.session_id]
        .some((v) => v?.toLowerCase().includes(kw))
    })
  }, [sessions, keyword, platform])

  // 默认选中：过滤结果的首条（列表变化时自动跟随，避免停在已隐藏的会话）
  const resolvedID = filtered.some((s) => s.session_id === activeID)
    ? activeID
    : (filtered[0]?.session_id ?? '')
  const activeSession = sessions.find((s) => s.session_id === resolvedID) ?? null

  const historyQuery = useQuery({
    queryKey: ['chat-history', resolvedID, HISTORY_LIMIT],
    queryFn: () => getChatHistory(resolvedID, HISTORY_LIMIT),
    enabled: Boolean(resolvedID),
  })

  const messages = historyQuery.data ?? []
  const [selectedMessage, setSelectedMessage] = useState<ChatMessage | null>(null)

  return (
    <div className="page page-wide">
      <div className="page-head">
        <div>
          <h1>聊天管理</h1>
          <p className="page-sub">
            会话台账与消息历史（只读）· 共 {formatNumber(sessions.length)} 个会话
          </p>
        </div>
        <div className="page-actions">
          <button
            className="btn btn-secondary btn-sm"
            disabled={sessionsQuery.isFetching}
            onClick={() => void sessionsQuery.refetch()}
          >
            {sessionsQuery.isFetching ? <span className="spin" /> : <RefreshCw />} 刷新
          </button>
        </div>
      </div>

      {sessionsQuery.isError ? (
        <Banner kind="err">
          会话列表加载失败：{sessionsQuery.error instanceof Error ? sessionsQuery.error.message : '未知错误'}
        </Banner>
      ) : null}

      <div className="grid grid-2" style={{ gridTemplateColumns: '300px 1fr', alignItems: 'start' }}>
        {/* 左：会话列表 */}
        <div className="card" style={{ minWidth: 0 }}>
          <div className="card-head">
            <h3>会话</h3>
            <span className="spacer" />
            <span className="muted-xs">{filtered.length} / {sessions.length}</span>
          </div>
          <div style={{ padding: '8px 10px', display: 'flex', flexDirection: 'column', gap: 8 }}>
            <div style={{ position: 'relative' }}>
              <Search size={13} style={{ position: 'absolute', left: 9, top: 9, color: 'var(--muted-fg)' }} />
              <TextInput
                aria-label="搜索会话"
                placeholder="搜索昵称 / 群名 / ID"
                value={keyword}
                onChange={(e) => setKeyword(e.target.value)}
                style={{ paddingLeft: 27 }}
              />
            </div>
            <select
              className="select"
              value={platform}
              onChange={(e) => setPlatform(e.target.value)}
              aria-label="按平台筛选"
            >
              <option value="all">全部平台</option>
              {platformOptions.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
          </div>
          <div className="card-body-flush" style={{ maxHeight: 'calc(100vh - 300px)', overflowY: 'auto' }}>
            {sessionsQuery.isPending ? (
              <Loading text="加载会话…" />
            ) : filtered.length === 0 ? (
              <Empty icon={<Inbox />} text={keyword || platform !== 'all' ? '没有匹配的会话' : '暂无会话记录'} />
            ) : (
              filtered.map((s) => (
                <button
                  key={s.session_id}
                  className={cn('nav-item', s.session_id === resolvedID && 'active')}
                  style={{ width: '100%', textAlign: 'left', alignItems: 'flex-start', borderRadius: 0 }}
                  onClick={() => {
                    setActiveID(s.session_id)
                    setSelectedMessage(null)
                  }}
                  title={s.session_id}
                >
                  <span style={{ minWidth: 0, flex: 1, display: 'flex', flexDirection: 'column', gap: 1 }}>
                    <span className="truncate sm" style={{ fontWeight: 500 }}>
                      {sessionTitle(s)}
                    </span>
                    <span className="muted-xs">
                      {s.platform} · {sessionSubtitle(s)}
                    </span>
                    <span className="muted-xs">{formatDateTime(s.last_active_at)}</span>
                  </span>
                  {s.is_group ? <Users size={13} /> : <UserRound size={13} />}
                </button>
              ))
            )}
          </div>
        </div>

        {/* 右：消息历史 */}
        <div className="card" style={{ minWidth: 0 }}>
          <div className="card-head">
            <h3>消息历史</h3>
            {activeSession ? (
              <>
                <span className="badge">{activeSession.platform}</span>
                <span className="muted-xs truncate" style={{ maxWidth: 200 }} title={activeSession.session_id}>
                  {sessionTitle(activeSession)}
                </span>
              </>
            ) : null}
            <span className="spacer" />
            <span className="muted-xs">
              {historyQuery.isFetching ? <span className="spin" style={{ width: 12, height: 12 }} /> : `${messages.length} 条`}
            </span>
          </div>
          <div className="card-body-flush">
            {!resolvedID ? (
              <Empty icon={<MessageSquare />} text="从左侧选择一个会话查看消息" />
            ) : historyQuery.isPending ? (
              <Loading text="加载消息…" />
            ) : historyQuery.isError ? (
              <Banner kind="err">
                消息加载失败：{historyQuery.error instanceof Error ? historyQuery.error.message : '未知错误'}
              </Banner>
            ) : messages.length === 0 ? (
              <Empty icon={<MessageSquare />} text="该会话暂无消息" />
            ) : (
              <div style={{ maxHeight: 'calc(100vh - 300px)', overflowY: 'auto', padding: 10, display: 'flex', flexDirection: 'column', gap: 6 }}>
                {messages.map((m) => (
                  <button
                    key={m.message_id || `${m.session_id}-${m.time}`}
                    className="nav-item"
                    style={{
                      width: '100%',
                      textAlign: 'left',
                      alignItems: 'flex-start',
                      borderRadius: 'var(--radius-sm)',
                      cursor: 'default',
                    }}
                    onClick={() => setSelectedMessage(m)}
                  >
                    <span style={{ minWidth: 0, flex: 1, display: 'flex', flexDirection: 'column', gap: 2 }}>
                      <span style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
                        <span className="sm" style={{ fontWeight: 600 }}>
                          {authorLabel(m)}
                        </span>
                        {isBotMessage(m) ? <span className="badge">机器人</span> : null}
                        {m.is_at ? <span className="badge badge-warn">@</span> : null}
                        {m.is_emoji ? <span className="badge">表情</span> : null}
                        {m.is_picture ? <span className="badge">图片</span> : null}
                        <span className="spacer" />
                        <span className="muted-xs">{formatDateTime(m.time)}</span>
                      </span>
                      <span className="sm" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
                        {m.text || '[非文本消息]'}
                      </span>
                    </span>
                  </button>
                ))}
              </div>
            )}
          </div>
        </div>
      </div>

      {selectedMessage ? (
        <div className="card" style={{ marginTop: 14 }}>
          <div className="card-head">
            <h3>消息详情</h3>
            <span className="spacer" />
            <span className="muted-xs">{selectedMessage.message_id || '无消息 ID'}</span>
          </div>
          <div className="card-body">
            <div className="kv-grid">
              <div className="kv">
                <span className="k">发送者</span>
                <span className="v">{authorLabel(selectedMessage)}</span>
              </div>
              <div className="kv">
                <span className="k">用户 ID</span>
                <span className="v">{selectedMessage.user_id || '—'}</span>
              </div>
              <div className="kv">
                <span className="k">平台</span>
                <span className="v">{selectedMessage.platform || '—'}</span>
              </div>
              <div className="kv">
                <span className="k">时间</span>
                <span className="v">{formatDateTime(selectedMessage.time)}</span>
              </div>
              <div className="kv">
                <span className="k">所属群</span>
                <span className="v">{selectedMessage.group_id || '—'}</span>
              </div>
              <div className="kv">
                <span className="k">回复目标</span>
                <span className="v">{selectedMessage.reply_to || '—'}</span>
              </div>
            </div>
            <p className="label" style={{ marginTop: 8 }}>原始内容</p>
            <pre className="code-block" style={{ margin: 0, maxHeight: 260, overflowY: 'auto', whiteSpace: 'pre-wrap' }}>
              {selectedMessage.raw_content !== undefined && selectedMessage.raw_content !== null
                ? JSON.stringify(selectedMessage.raw_content, null, 2)
                : (selectedMessage.text || '[空]')}
            </pre>
          </div>
        </div>
      ) : null}
    </div>
  )
}
