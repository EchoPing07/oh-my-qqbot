/**
 * 嵌入式聊天 页 — 供外部 iframe 引用的极简聊天视图（/chat/embed，公开路由）。
 *
 * 布局：100vh 纵向 flex — 顶部窄条（logo + 状态点）/ 中间消息区 / 底部输入行，无 sidebar。
 * 数据：GET /chat/sessions 取第一个 platform==='webui' 会话 → GET /chat/history 轮询 3s；
 *       POST /chat/send 发言（后端未接线时返回 503 → 顶部 Banner 提示并禁用输入）。
 * 消息归属：本页发言经 InjectWebUIMessage 以固定 user_id='webui_user' 入库（对齐装配层约定）。
 */

import { useCallback, useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { SendHorizontal } from 'lucide-react'

import { getChatHistory, getChatSessions, sendChatMessage, type ChatMessage } from '@/api/chat'
import { formatTime } from '@/lib/format'
import { Banner, Empty, Loading } from '@/components/ui'
import { cn } from '@/lib/cn'

const WEBUI_PLATFORM = 'webui'
/** 与主聊天页一致的轮询节奏。 */
const POLL_MS = 3000
/** 本页发言的用户名（/chat/send 的 user_name 字段）。 */
const EMBED_USER_NAME = 'WebUI用户'
/** 装配层 InjectWebUIMessage 写库时使用的固定 user_id，用于判断消息归属。 */
const EMBED_USER_ID = 'webui_user'

export function ChatEmbedPage() {
  const qc = useQueryClient()

  // 会话：轮询会话列表，取第一个 webui 会话；发送消息会自动建会话
  const sessions = useQuery({
    queryKey: ['chat-embed-sessions'],
    queryFn: () => getChatSessions(50),
    refetchInterval: POLL_MS,
    select: (list) => list.filter((s) => s.platform === WEBUI_PLATFORM),
  })
  const sessionID = sessions.data?.[0]?.session_id ?? ''

  // 历史：跟随会话轮询
  const history = useQuery({
    queryKey: ['chat-embed-history', sessionID],
    queryFn: () => getChatHistory(sessionID, 200),
    enabled: Boolean(sessionID),
    refetchInterval: POLL_MS,
  })

  // 平台接线状态（ChatInjector 未注入时 /chat/send 返回 503）
  const [platformDown, setPlatformDown] = useState(false)

  const [draft, setDraft] = useState('')
  const [sending, setSending] = useState(false)
  const listRef = useRef<HTMLDivElement>(null)
  const stickBottomRef = useRef(true)

  // 新消息到达且用户未上翻时自动滚到底部
  useEffect(() => {
    if (stickBottomRef.current) {
      listRef.current?.scrollTo({ top: listRef.current.scrollHeight })
    }
  }, [history.data])

  // 上翻（距底 > 60px）暂停自动跟随，回到底部恢复
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
      await sendChatMessage(content, EMBED_USER_NAME)
      setPlatformDown(false)
      setDraft('')
      stickBottomRef.current = true
      // 立即刷新历史与会话（发送可能新建会话）
      await qc.invalidateQueries({ queryKey: ['chat-embed-history'] })
      await qc.invalidateQueries({ queryKey: ['chat-embed-sessions'] })
    } catch (err) {
      if (err instanceof Error && err.message.includes('未接线')) {
        setPlatformDown(true)
      }
      // 非接线类错误仍可重试，不置灰输入
      toastErr(err)
    } finally {
      setSending(false)
    }
  }

  const inputDisabled = sending || platformDown
  const messages = history.data ?? []

  return (
    <div style={{ height: '100vh', display: 'flex', flexDirection: 'column', background: 'var(--bg)' }}>
      {/* ── 顶部窄条：logo + 状态点 ── */}
      <header
        style={{
          flexShrink: 0,
          display: 'flex',
          alignItems: 'center',
          gap: 8,
          padding: '0 12px',
          height: 40,
          borderBottom: '1px solid var(--border)',
          background: 'var(--card)',
        }}
      >
        <span className="logo-mark" aria-hidden="true">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round">
            <path d="M5 18V6l7 7 7-7v12" />
          </svg>
        </span>
        <span className="sm" style={{ fontWeight: 600 }}>
          oh-my-qqbot
        </span>
        <span className="spacer" />
        <span
          className={cn('dot', sessions.isFetching || history.isFetching ? 'dot-warn' : 'dot-ok')}
          title={platformDown ? '聊天平台未接线' : history.isError ? '连接异常' : '正常'}
        />
      </header>

      {/* ── 中间消息区 ── */}
      <div
        ref={listRef}
        onScroll={onScroll}
        style={{ flex: 1, minHeight: 0, overflowY: 'auto', padding: 14, display: 'flex', flexDirection: 'column' }}
      >
        {platformDown ? (
          <div style={{ marginBottom: 10 }}>
            <Banner kind="err">聊天平台未接线：后端装配层未提供 ChatInjector，暂时无法收发消息。</Banner>
          </div>
        ) : null}

        {sessions.isPending ? (
          <Loading text="连接聊天服务…" />
        ) : sessions.isError ? (
          <Banner kind="err">会话加载失败：{errText(sessions.error)}</Banner>
        ) : messages.length === 0 ? (
          <Empty text={sessionID ? '还没有消息' : '发第一条消息开始对话'} />
        ) : (
          <div className="chat-messages">
            {messages.map((m) => (
              <MessageRow key={m.message_id} msg={m} />
            ))}
          </div>
        )}
      </div>

      {/* ── 底部输入行 ── */}
      <form
        className="chat-composer"
        onSubmit={(e) => {
          e.preventDefault()
          void send()
        }}
        style={{ flexShrink: 0, background: 'var(--card)' }}
      >
        <input
          className="input chat-input"
          placeholder={platformDown ? '聊天平台未接线，暂时无法发送' : '输入消息，Enter 发送…'}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault()
              void send()
            }
          }}
          disabled={inputDisabled}
          aria-label="消息内容"
        />
        <button className="btn" type="submit" disabled={inputDisabled || !draft.trim()}>
          {sending ? <span className="spin" /> : <SendHorizontal />} 发送
        </button>
      </form>
    </div>
  )
}

/** 单条消息：装配层固定 user_id='webui_user' 视为本页用户发言（靠右）。 */
function MessageRow({ msg }: { msg: ChatMessage }) {
  const mine = msg.user_id === EMBED_USER_ID
  return (
    <div className={cn('chat-bubble', mine && 'mine')}>
      <div className="chat-meta">
        <span>{msg.user_nickname || msg.user_id || '未知'}</span>
        <span>{formatTime(msg.time)}</span>
      </div>
      <div className="chat-text">{msg.text || '[非文本消息]'}</div>
    </div>
  )
}

/** 行内错误提示（不打断界面）。 */
function toastErr(err: unknown): void {
  console.error('发送失败:', err instanceof Error ? err.message : err)
}

/** 统一错误文案提取。 */
function errText(err: unknown): string {
  return err instanceof Error ? err.message : '未知错误'
}
