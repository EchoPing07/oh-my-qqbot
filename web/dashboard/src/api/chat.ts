/**
 * 聊天 API — 对齐 internal/webui/routers/chat.go（sessions/history，只读）。
 */

import { api } from '@/lib/http'

export interface ChatSession {
  session_id: string
  platform: string
  is_group: boolean
  group_id?: string
  group_name?: string
  user_id?: string
  user_nickname?: string
  created_at: string
  last_active_at: string
}

export async function getChatSessions(limit = 100): Promise<ChatSession[]> {
  const data = await api.get<{ sessions: ChatSession[] }>('/api/webui/chat/sessions', {
    query: { limit },
    envelope: 'raw',
  })
  return data.sessions ?? []
}

export interface ChatMessage {
  message_id: string
  time: string
  platform: string
  user_id: string
  user_nickname: string
  group_id?: string
  session_id: string
  is_emoji: boolean
  is_picture: boolean
  is_mentioned: boolean
  is_at: boolean
  reply_to?: string
  text: string
  raw_content?: unknown
}

export async function getChatHistory(sessionID: string, limit = 200): Promise<ChatMessage[]> {
  const data = await api.get<{ messages: ChatMessage[] }>('/api/webui/chat/history', {
    query: { session_id: sessionID, limit },
    envelope: 'raw',
  })
  return data.messages ?? []
}
