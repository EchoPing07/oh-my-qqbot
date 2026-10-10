/**
 * 资源 API（人物 / 表达 / 黑话 / 表情包）— 对齐 internal/webui/routers/resources.go。
 */

import { api } from '@/lib/http'

// ── 人物 ──

export interface PersonItem {
  person_id: string
  person_name?: string
  user_nickname?: string
  platform?: string
  user_id?: string
  know_times: number
  memory_points: number
  is_known: boolean
  last_known?: string
}

export async function getPersons(limit = 100, offset = 0): Promise<{ items: PersonItem[]; total: number | null }> {
  const data = await api.get<{ items: PersonItem[] }>('/api/webui/person/list', {
    query: { limit, offset },
    envelope: 'raw',
  })
  return { items: data.items ?? [], total: null }
}

export interface PersonStats {
  total: number
  known: number
}

export async function getPersonStats(): Promise<PersonStats> {
  return api.get<PersonStats>('/api/webui/person/stats/summary', { envelope: 'raw' })
}

export async function deletePerson(personID: string): Promise<void> {
  await api.delete(`/api/webui/person/${encodeURIComponent(personID)}`)
}

// ── 表达方式 ──

export interface ExpressionItem {
  id: number
  situation?: string
  expression?: string
  style?: string
  count: number
  checked: boolean
  modified_by?: string
  created_by?: string
}

export async function getExpressions(limit = 100, offset = 0): Promise<ExpressionItem[]> {
  const data = await api.get<{ items: ExpressionItem[] }>('/api/webui/expression/list', {
    query: { limit, offset },
    envelope: 'raw',
  })
  return data.items ?? []
}

export async function deleteExpression(id: number): Promise<void> {
  await api.delete(`/api/webui/expression/${id}`)
}

/** 人工审核标记（checked + modified_by=USER）。 */
export async function checkExpression(id: number, checked: boolean): Promise<void> {
  await api.put(`/api/webui/expression/${id}/check`, { body: { checked } })
}

// ── 黑话 ──

export interface JargonItem {
  id: number
  jargon?: string
  meaning?: string
  explanation?: string
  count: number
  is_jargon: boolean
  created_by?: string
}

export async function getJargons(limit = 100, offset = 0): Promise<JargonItem[]> {
  const data = await api.get<{ items: JargonItem[] }>('/api/webui/jargon/list', {
    query: { limit, offset },
    envelope: 'raw',
  })
  return data.items ?? []
}

export async function updateJargon(
  id: number,
  body: { is_jargon: boolean; meaning?: string }
): Promise<void> {
  await api.put(`/api/webui/jargon/${id}`, { body })
}

export async function deleteJargon(id: number): Promise<void> {
  await api.delete(`/api/webui/jargon/${id}`)
}

// ── 表情包 ──

export interface EmojiItem {
  hash: string
  description?: string
  full_path?: string
  query_count: number
  is_registered: boolean
  is_banned: boolean
  no_file: boolean
  vlm_processed: boolean
  register_time?: string
}

export async function getEmojis(limit = 100, offset = 0): Promise<EmojiItem[]> {
  const data = await api.get<{ items: EmojiItem[] }>('/api/webui/emoji/list', {
    query: { limit, offset },
    envelope: 'raw',
  })
  return data.items ?? []
}

export async function getEmojiDetail(hash: string): Promise<EmojiItem> {
  const data = await api.get<{ emoji: EmojiItem }>(
    `/api/webui/emoji/${encodeURIComponent(hash)}`,
    { envelope: 'raw' }
  )
  return data.emoji
}

export async function deleteEmoji(hash: string): Promise<void> {
  await api.delete(`/api/webui/emoji/${encodeURIComponent(hash)}`)
}

export async function banEmoji(hash: string, banned: boolean): Promise<void> {
  await api.put(`/api/webui/emoji/${encodeURIComponent(hash)}/ban`, { body: { banned } })
}
