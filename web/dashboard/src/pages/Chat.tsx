/**
 * 聊天页 — WebUI 内置聊天（虚拟平台 webui）。
 * REST 轮询历史（3s）+ 发送走 /chat/send；机器人回复经轮询自然带出。
 */

import { useCallback, useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { SendHorizontal } from 'lucide-react'

import { getChatHistory, getChatSessions, sendChatMessage, type ChatMessage } from '@/api/chat'
import { formatTime, sessionTitle } from '@/lib/format'
import { Banner, Empty, Loading } from '@/components/ui'
import { cn } from '@/lib/cn'

const WEBUI_PLATFORM = 'webui'
const POLL_MS = 3000

export function ChatPage() {
  const qc = useQueryClient()
  const sessions = useQuery({
    queryKey: ['chat-sessions'],
    queryFn: () => getChatSessions(50),
    refetchInterval: POLL_MS,
    select: (list) => list.filter((s) => s.platform === WEBUI_PLATFORM),
  })

  const [activeID, setActiveID] = useState('')
  // 首个 webui 会话自动选中：渲染期对齐（数据到达后无 effect 级联）
  const firstSessionID = sessions.data?.[0]?.session_id ?? ''
  if (!activeID && firstSessionID) {
    setActiveID(firstSessionID)
  }

  const history = useQuery({
    queryKey: ['chat-history', activeID],
    queryFn: () => getChatHistory(activeID, 200),
    enabled: Boolean(activeID),
    refetchInterval: POLL_MS,
  })

  const [draft, setDraft] = useState('')
  const [sending, setSending] = useState(false)
  const listRef = useRef<HTMLDivElement>(null)
  const stickBottomRef = useRef(true)

  // 新消息到达时滚到底部（用户上翻时暂停跟随）
  useEffect(() => {
    if (stickBottomRef.current) listRef.current?.scrollTo({ top: listRef.current.scrollHeight })
  }, [history.data])

  const onScroll = useCallback(() => {
    const el = listRef.current
    if (!el) return
    stickBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 60
  }, [])

  const send = async () => {
    const content = draft.trim()
    if (!content || sending) return
    setSending(true)
    try {
      await sendChatMessage(content, 'WebUI用户')
      setDraft('')
      stickBottomRef.current = true
      await qc.invalidateQueries({ queryKey: ['chat-history', activeID] })
      await qc.invalidateQueries({ queryKey: ['chat-sessions'] })
    } catch (err) {
      console.error('发送失败:', err)
    } finally {
      setSending(false)
    }
  }

  return (
    <div className="page page-wide" style={{ height: 'calc(100vh - var(--topbar-h) - 36px)' }}>
      <div className="card" style={{ flex: 1, display: 'flex', flexDirection: 'column', minHeight: 0 }}>
        <div className="card-head">
          <h3>聊天</h3>
          <span className="muted-xs">虚拟平台 {WEBUI_PLATFORM} · {POLL_MS / 1000}s 轮询</span>
          <span className="spacer" />
          {sessions.isFetching ? <span className="spin" style={{ width: 12, height: 12 }} /> : null}
        </div>
        <div style={{ flex: 1, display: 'flex', minHeight: 0 }}>
          {/* 会话侧栏 */}
          <div style={{ width: 200, flexShrink: 0, borderRight: '1px solid var(--border)', overflowY: 'auto', padding: 6 }}>
            {sessions.isPending ? (
              <Loading />
            ) : (sessions.data?.length ?? 0) === 0 ? (
              <p className="muted-xs" style={{ padding: 10 }}>暂无会话，发第一条消息开始对话</p>
            ) : (
              (sessions.data ?? []).map((s) => (
                <button
                  key={s.session_id}
                  className={cn('nav-item', s.session_id === activeID && 'active')}
                  style={{ width: '100%', textAlign: 'left' }}
                  onClick={() => setActiveID(s.session_id)}
                  title={s.session_id}
                >
                  <span className="truncate">{sessionTitle(s)}</span>
                </button>
              ))
            )}
          </div>
          {/* 消息区 */}
          <div style={{ flex: 1, display: 'flex', flexDirection: 'column', minWidth: 0 }}>
            <div ref={listRef} onScroll={onScroll} style={{ flex: 1, overflowY: 'auto', padding: 14 }}>
              {history.isPending ? (
                <Loading />
              ) : (history.data?.length ?? 0) === 0 ? (
                <Empty text="还没有消息" />
              ) : (
                <div className="chat-messages">
                  {(history.data ?? []).map((m) => (
                    <MessageRow key={m.message_id} msg={m} />
                  ))}
                </div>
              )}
            </div>
            <div className="chat-composer">
              <input
                className="input chat-input"
                placeholder="输入消息，Enter 发送…"
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                    e.preventDefault()
                    void send()
                  }
                }}
                disabled={sending}
              />
              <button className="btn" onClick={() => void send()} disabled={sending || !draft.trim()}>
                {sending ? <span className="spin" /> : <SendHorizontal />} 发送
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}

function MessageRow({ msg }: { msg: ChatMessage }) {
  const isBot = msg.user_id === 'bot' || msg.user_nickname.includes('bot') || msg.user_nickname.includes('麦麦')
  const mine = !isBot && msg.platform === WEBUI_PLATFORM
  return (
    <div className={cn('chat-bubble', mine && 'mine')}>
      <div className="chat-meta">
        <span>{msg.user_nickname || msg.user_id || '未知'}</span>
        <span>{formatTime(msg.time)}</span>
        {msg.is_at ? <span className="badge">@</span> : null}
      </div>
      <div className="chat-text">{msg.text || '[非文本消息]'}</div>
    </div>
  )
}

export function ChatEmbedPage() {
  return (
    <div style={{ height: '100vh', display: 'flex', flexDirection: 'column' }}>
      <ChatBody />
    </div>
  )
}

function ChatBody() {
  return (
    <div style={{ flex: 1, minHeight: 0 }}>
      <Banner>嵌入式聊天视图</Banner>
    </div>
  )
}
