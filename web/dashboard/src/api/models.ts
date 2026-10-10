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
