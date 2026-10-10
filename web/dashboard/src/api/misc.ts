/**
 * 日志 / 记忆 / MCP / 数据传输 / 推理过程 API。
 */

import { api } from '@/lib/http'

// ── 日志（internal/webui/routers/logs.go） ──

export interface LogEntry {
  id: string
  timestamp: string
  level: string
  module: string
  message: string
}

export async function getRecentLogs(limit = 200, level = ''): Promise<{ logs: LogEntry[]; total: number }> {
  const data = await api.get<{ logs: LogEntry[]; total: number }>('/api/webui/logs/recent', {
    query: { limit, level: level || undefined },
    envelope: 'raw',
  })
  return { logs: data.logs ?? [], total: data.total ?? 0 }
}

export async function getLogLevel(): Promise<string> {
  const data = await api.get<{ level: string }>('/api/webui/logs/level', { envelope: 'raw' })
  return data.level
}

export async function setLogLevel(level: string): Promise<void> {
  await api.put('/api/webui/logs/level', { body: { level } })
}

// ── 记忆（internal/webui/routers/memory.go） ──

export interface MemoryHit {
  id?: string
  content?: string
  score?: number
  [key: string]: unknown
}

export async function searchMemory(
  query: string,
  chatID = '',
  topK = 5
): Promise<MemoryHit[]> {
  const data = await api.post<{ hits: MemoryHit[] }>('/api/webui/memory/search', {
    body: { query, chat_id: chatID, top_k: topK },
    errorMessage: '记忆检索失败',
  })
  return data.hits ?? []
}

export interface MemoryProfile {
  person_id: string
  [key: string]: unknown
}

export async function getMemoryProfiles(): Promise<MemoryProfile[]> {
  const data = await api.get<{ profiles?: MemoryProfile[]; items?: MemoryProfile[] }>(
    '/api/webui/memory/profiles',
    { envelope: 'raw' }
  )
  return data.profiles ?? data.items ?? []
}

export async function getMemoryProfile(personID: string): Promise<MemoryProfile> {
  return api.get<MemoryProfile>(
    `/api/webui/memory/profiles/${encodeURIComponent(personID)}`,
    { envelope: 'raw' }
  )
}

// ── MCP（internal/webui/routers/mcp.go） ──

export interface McpStatus {
  enabled: boolean
  initialized: boolean
  server_count: number
  tool_count: number
  servers: Array<Record<string, unknown>>
  [key: string]: unknown
}

export async function getMcpStatus(): Promise<McpStatus> {
  return api.get<McpStatus>('/api/webui/mcp/status', { envelope: 'raw' })
}

// ── 数据传输（internal/webui/routers/data_transfer.go） ──

export interface ExportOptions {
  include_bot: boolean
  include_model: boolean
}

/** 导出配置 JSON（浏览器触发下载）。 */
export async function exportConfig(options: ExportOptions): Promise<void> {
  const { downloadBinary } = await import('@/lib/http')
  const blob = await downloadBinary('/api/webui/data-transfer/export', options)
  const url = URL.createObjectURL(blob)
  try {
    const a = document.createElement('a')
    a.href = url
    // Content-Disposition 文件名不可读时用本地时间兜底
    a.download = `ohmyqqbot-config-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')}.json`
    a.click()
  } finally {
    URL.revokeObjectURL(url)
  }
}

/** 导入配置 JSON（读文件 → 提交）。返回导入的节清单。 */
export async function importConfig(file: File): Promise<string[]> {
  const text = await file.text()
  let payload: unknown
  try {
    payload = JSON.parse(text)
  } catch {
    throw new Error('文件不是合法 JSON')
  }
  const data = await api.post<{ imported?: string[] }>('/api/webui/data-transfer/import', {
    body: payload,
    errorMessage: '导入失败',
  })
  return data.imported ?? []
}

// ── 推理过程（internal/webui/routers/reasoning_process.go） ──

export interface ReasoningSnapshot {
  seq: number
  kind: string
  session_id: string
  time: string
  payload: Record<string, unknown>
}

export async function getSnapshots(sessionID = '', limit = 50): Promise<ReasoningSnapshot[]> {
  const data = await api.get<{ snapshots: ReasoningSnapshot[] }>(
    '/api/webui/reasoning-process/snapshots',
    { query: { session_id: sessionID || undefined, limit }, envelope: 'raw' }
  )
  return data.snapshots ?? []
}

// ── 系统操作 ──

/** POST /system/reload-config：热重载配置。 */
export async function reloadConfig(): Promise<void> {
  await api.post('/api/webui/system/reload-config')
}
