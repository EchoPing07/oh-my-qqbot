/**
 * 系统 / 统计 / 概览 API — 对齐 internal/webui/routers/system.go、statistics.go。
 */

import { api } from '@/lib/http'

export interface SystemStatus {
  running: boolean
  uptime: number
  version: string
  start_time: string
  go_version: string
  memory: {
    heap_alloc_bytes: number
    heap_sys_bytes: number
    sys_bytes: number
    num_goroutine: number
    num_cpu: number
    gc_count: number
  }
}

export async function getSystemStatus(): Promise<SystemStatus> {
  return api.get<SystemStatus>('/api/webui/system/status', { envelope: 'raw' })
}

export interface StatsSummary {
  hours: number
  request_count: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cache_hit_tokens: number
  cache_miss_tokens: number
  total_cost: number
  avg_response_time: number
}

export async function getStatsSummary(hours = 24): Promise<StatsSummary> {
  return api.get<StatsSummary>('/api/webui/statistics/summary', {
    query: { hours },
    envelope: 'raw',
  })
}

export interface ModelStat {
  model_name: string
  request_count: number
  total_cost: number
  input_tokens: number
  output_tokens: number
  total_tokens: number
  cache_hit_tokens: number
  cache_miss_tokens: number
  avg_response_time: number
}

export async function getStatsModels(hours = 24): Promise<ModelStat[]> {
  const data = await api.get<{ models: ModelStat[] }>('/api/webui/statistics/models', {
    query: { hours },
    envelope: 'raw',
  })
  return data.models ?? []
}

export interface DailyStat {
  day: string
  requests: number
  total_tokens: number
  cost: number
}

export async function getStatsDaily(days = 7): Promise<DailyStat[]> {
  const data = await api.get<{ daily: Array<{ day: string; request_count: number; total_tokens: number; total_cost: number }> }>(
    '/api/webui/statistics/daily',
    { query: { days }, envelope: 'raw' }
  )
  return (data.daily ?? []).map((d) => ({
    day: d.day,
    requests: Number(d.request_count) || 0,
    total_tokens: Number(d.total_tokens) || 0,
    cost: Number(d.total_cost) || 0,
  }))
}
