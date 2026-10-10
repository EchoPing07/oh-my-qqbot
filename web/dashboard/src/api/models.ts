/**
 * 模型 API — 对齐 internal/webui/routers/model.go。
 */

import { api } from '@/lib/http'

export interface ModelInfo {
  name: string
  model_identifier: string
  api_provider: string
  base_url?: string
  price_in: number
  price_out: number
  visual: boolean
  force_stream_mode: boolean
  extra_params?: Record<string, unknown>
  tasks: string[]
}

export async function getModels(): Promise<ModelInfo[]> {
  const data = await api.get<{ models: ModelInfo[] }>('/api/webui/models/list', { envelope: 'raw' })
  return data.models ?? []
}

export interface ProviderInfo {
  name: string
  base_url: string
  client_type: string
  max_retry: number
  timeout: number
  retry_interval: number
  has_api_key: boolean
  /** 该服务商下的模型清单（模型配置以服务商为单位管理） */
  models: ProviderModelInfo[]
}

/** 服务商卡片内的单行模型摘要。 */
export interface ProviderModelInfo {
  name: string
  model_identifier: string
  visual: boolean
  price_in: number
  price_out: number
  tasks: string[]
}

export async function getProviders(): Promise<ProviderInfo[]> {
  const data = await api.get<{ providers: ProviderInfo[] }>('/api/webui/models/providers', {
    envelope: 'raw',
  })
  return data.providers ?? []
}

export interface TaskConfig {
  task: string
  model_list: string[]
  max_tokens: number
  temperature: number
  selection_strategy: string
  hard_timeout: number
  fallback_chain: string[]
}

export async function getTaskConfigs(): Promise<TaskConfig[]> {
  const data = await api.get<{ tasks: TaskConfig[] }>('/api/webui/models/task-config', {
    envelope: 'raw',
  })
  return data.tasks ?? []
}

/** 模型配置写入辅助：读整份 model 配置 → 深合并 → 整份保存。 */
export async function saveModelSection(section: 'models' | 'api_providers' | 'model_task_config', value: unknown): Promise<void> {
  const { getModelConfig, saveModelConfig } = await import('./config')
  const config = await getModelConfig()
  await saveModelConfig({ ...config, [section]: value })
}
