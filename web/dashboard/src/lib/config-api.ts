/**
 * 配置API客户端
 *
 * 请求样板（认证、解析、错误格式化）由 @/lib/http 的请求客户端承担；
 * 本文件只声明 endpoint、业务错误文案、响应解包规则与配置数据缓存。
 * 公开函数遵循 throw 契约：成功返回数据，失败抛 ApiError。
 */

import { ApiError, backendApi } from '@/lib/http'
import type { ConfigSchema } from '@/types/config-schema'

const API_BASE = '/api/webui/config'
export const BOT_CONFIG_UPDATED_EVENT = 'omb:bot-config-updated'
const schemaRequestCache = new Map<string, Promise<ConfigSchema>>()
const configDataCache = new Map<
  string,
  { timestamp: number; request: Promise<Record<string, unknown>> }
>()
const CONFIG_DATA_CACHE_TTL = 30_000

function unwrapConfigResponse(data: unknown): Record<string, unknown> {
  if (
    data &&
    typeof data === 'object' &&
    'config' in data &&
    (data as Record<string, unknown>).config &&
    typeof (data as Record<string, unknown>).config === 'object'
  ) {
    return (data as { config: Record<string, unknown> }).config
  }

  if (data && typeof data === 'object') {
    return data as Record<string, unknown>
  }

  return {}
}

function getCachedSchema(key: string, url: string): Promise<ConfigSchema> {
  const cachedRequest = schemaRequestCache.get(key)
  if (cachedRequest) {
    return cachedRequest
  }

  const request = backendApi
    .get<ConfigSchema>(url, { cache: 'no-store', errorMessage: '获取配置架构失败' })
    .catch((error: unknown) => {
      // HTTP 层失败（带 status 的 ApiError）保留被拒绝的 Promise 在缓存中，
      // 避免对失败的配置接口反复发起请求；请求未到达服务器等瞬时异常则剔除缓存后重抛
      if (!(error instanceof ApiError) || error.status === undefined) {
        schemaRequestCache.delete(key)
      }
      throw error
    })

  schemaRequestCache.set(key, request)
  return request
}

function getCachedConfigData(key: string, url: string): Promise<Record<string, unknown>> {
  const cachedRequest = configDataCache.get(key)
  if (cachedRequest && Date.now() - cachedRequest.timestamp < CONFIG_DATA_CACHE_TTL) {
    return cachedRequest.request
  }

  const request = backendApi
    .get<unknown>(url, { cache: 'no-store', errorMessage: '获取配置失败' })
    .then((data) => unwrapConfigResponse(data))
    .catch((error: unknown) => {
      // HTTP 层失败保留被拒绝的 Promise 在缓存中，避免反复请求失败接口；瞬时异常则剔除缓存后重抛
      if (!(error instanceof ApiError) || error.status === undefined) {
        configDataCache.delete(key)
      }
      throw error
    })

  configDataCache.set(key, { timestamp: Date.now(), request })
  return request
}

function invalidateConfigDataCache(key?: string): void {
  if (key) {
    configDataCache.delete(key)
    return
  }
  configDataCache.clear()
}

function notifyBotConfigUpdated(): void {
  if (typeof window !== 'undefined') {
    window.dispatchEvent(new Event(BOT_CONFIG_UPDATED_EVENT))
  }
}

/**
 * 获取机器人配置架构（Go 后端：返回配置节清单的 schema 形态）
 */
export async function getBotConfigSchema(): Promise<ConfigSchema> {
  return getCachedSchema('bot', `${API_BASE}/schema/bot`)
}

/**
 * 获取模型配置架构
 */
export async function getModelConfigSchema(): Promise<ConfigSchema> {
  return getCachedSchema('model', `${API_BASE}/schema/model`)
}

/**
 * 获取指定配置节的架构
 */
export async function getConfigSectionSchema(sectionName: string): Promise<ConfigSchema> {
  return getCachedSchema(`section:${sectionName}`, `${API_BASE}/schema/section/${sectionName}`)
}

/**
 * 获取机器人配置数据（Go 后端返回 { success, config }，TOML 键名字典形态）
 */
export async function getBotConfig(): Promise<Record<string, unknown>> {
  const data = await backendApi.get<unknown>(`${API_BASE}/bot`, {
    cache: 'no-store',
    errorMessage: '获取配置失败',
  })
  return unwrapConfigResponse(data)
}

/** Cached config data for lightweight status summaries. */
export async function getBotConfigCached(): Promise<Record<string, unknown>> {
  return getCachedConfigData('bot', `${API_BASE}/bot`)
}

/**
 * 获取模型配置数据
 */
export async function getModelConfig(): Promise<Record<string, unknown>> {
  const data = await backendApi.get<unknown>(`${API_BASE}/model`, {
    cache: 'no-store',
    errorMessage: '获取配置失败',
  })
  return unwrapConfigResponse(data)
}

/** Cached model config data for lightweight status summaries. */
export async function getModelConfigCached(): Promise<Record<string, unknown>> {
  return getCachedConfigData('model', `${API_BASE}/model`)
}

/**
 * 更新麦麦主程序配置
 */
export async function updateBotConfig(
  config: Record<string, unknown>
): Promise<Record<string, unknown>> {
  const result = await backendApi.post<Record<string, unknown>>(`${API_BASE}/bot`, {
    body: config,
    errorMessage: '更新配置失败',
  })
  invalidateConfigDataCache('bot')
  notifyBotConfigUpdated()
  return result
}

/**
 * 获取麦麦主程序配置的原始 TOML 内容
 */
export async function getBotConfigRaw(): Promise<string> {
  return backendApi.get<string>(`${API_BASE}/bot/raw`, {
    cache: 'no-store',
    errorMessage: '获取原始配置失败',
  })
}

/**
 * 更新麦麦主程序配置（原始 TOML 内容）
 */
export async function updateBotConfigRaw(rawContent: string): Promise<Record<string, unknown>> {
  const result = await backendApi.post<Record<string, unknown>>(`${API_BASE}/bot/raw`, {
    body: { raw_content: rawContent },
    errorMessage: '更新配置失败',
  })
  invalidateConfigDataCache('bot')
  notifyBotConfigUpdated()
  return result
}

/**
 * 更新模型配置
 */
export async function updateModelConfig(
  config: Record<string, unknown>
): Promise<Record<string, unknown>> {
  const result = await backendApi.post<Record<string, unknown>>(`${API_BASE}/model`, {
    body: config,
    errorMessage: '更新配置失败',
  })
  invalidateConfigDataCache('model')
  return result
}

/**
 * 模型配置文件副本信息
 */
export interface ModelConfigVersionInfo {
  id: string
  label: string
  created_at: number
  modified_at: number
  size: number
  active: boolean
  inner_config_version: string | null
  valid: boolean
  error: string | null
}

/**
 * 模型配置文件副本列表
 */
export interface ModelConfigVersionListResponse {
  success: boolean
  active_version: ModelConfigVersionInfo
  versions: ModelConfigVersionInfo[]
}

/**
 * 获取模型配置文件副本列表
 */
export async function getModelConfigVersions(): Promise<ModelConfigVersionListResponse> {
  return backendApi.get<ModelConfigVersionListResponse>(`${API_BASE}/model/versions`, {
    cache: 'no-store',
    errorMessage: '获取模型配置副本失败',
  })
}

/**
 * 将当前模型配置保存为副本
 */
export async function createModelConfigVersion(label: string): Promise<ModelConfigVersionInfo> {
  const result = await backendApi.post<{ version: ModelConfigVersionInfo }>(
    `${API_BASE}/model/versions`,
    {
      body: { label },
      errorMessage: '创建模型配置副本失败',
    }
  )
  return result.version
}

/**
 * 切换当前启用的模型配置文件副本
 */
export async function switchModelConfigVersion(versionId: string): Promise<ModelConfigVersionInfo> {
  const result = await backendApi.post<{ version: ModelConfigVersionInfo }>(
    `${API_BASE}/model/versions/${encodeURIComponent(versionId)}/activate`,
    {
      body: { archive_current: true },
      errorMessage: '切换模型配置副本失败',
    }
  )
  invalidateConfigDataCache('model')
  return result.version
}

/**
 * 更新模型配置副本名称
 */
export async function updateModelConfigVersionLabel(
  versionId: string,
  label: string
): Promise<ModelConfigVersionInfo> {
  const result = await backendApi.patch<{ version: ModelConfigVersionInfo }>(
    `${API_BASE}/model/versions/${encodeURIComponent(versionId)}`,
    {
      body: { label },
      errorMessage: '更新模型配置副本失败',
    }
  )
  return result.version
}

/**
 * 删除模型配置副本
 */
export async function deleteModelConfigVersion(versionId: string): Promise<void> {
  await backendApi.delete(`${API_BASE}/model/versions/${encodeURIComponent(versionId)}`, {
    errorMessage: '删除模型配置副本失败',
  })
}

/**
 * 更新麦麦主程序配置的指定节
 */
export async function updateBotConfigSection(
  sectionName: string,
  sectionData: unknown
): Promise<Record<string, unknown>> {
  const result = await backendApi.post<Record<string, unknown>>(
    `${API_BASE}/bot/section/${sectionName}`,
    {
      body: sectionData,
      errorMessage: '更新配置失败',
    }
  )
  invalidateConfigDataCache('bot')
  notifyBotConfigUpdated()
  return result
}

/**
 * 更新模型配置的指定节
 */
export async function updateModelConfigSection(
  sectionName: string,
  sectionData: unknown
): Promise<Record<string, unknown>> {
  const result = await backendApi.post<Record<string, unknown>>(
    `${API_BASE}/model/section/${sectionName}`,
    {
      body: sectionData,
      errorMessage: '更新配置失败',
    }
  )
  invalidateConfigDataCache('model')
  return result
}

/**
 * 模型信息
 */
export interface ModelListItem {
  id: string
  name: string
  owned_by?: string
}

/**
 * 获取模型列表响应
 */
export interface FetchModelsResponse {
  success: boolean
  models: ModelListItem[]
  provider?: string
  count: number
}

/**
 * 已注册的模型客户端类型
 */
export interface ModelClientType {
  client_type: string
  owner_plugin_id: string | null
  version: string
  description: string
  builtin: boolean
}

/**
 * 获取可用的模型客户端类型（Go 后端仅支持 openai 兼容客户端）
 */
export async function fetchModelClientTypes(): Promise<ModelClientType[]> {
  return [
    {
      client_type: 'openai',
      owner_plugin_id: null,
      version: 'builtin',
      description: 'OpenAI 兼容客户端',
      builtin: true,
    },
  ]
}

/**
 * 获取指定提供商的可用模型列表
 * （Go 后端 /models/list 返回配置内全量模型；这里按 provider 在前端过滤）
 * @param providerName 提供商名称（在 model_config.toml 中配置的名称）
 */
export async function fetchProviderModels(providerName: string): Promise<ModelListItem[]> {
  const body = await backendApi.get<{ models?: ModelListItem[] } | ModelListItem[]>(
    '/api/webui/models/list',
    { errorMessage: '获取模型列表失败' }
  )
  const models = Array.isArray(body) ? body : Array.isArray(body?.models) ? body.models : []
  return models.filter(
    (model) => (model as { api_provider?: string }).api_provider === providerName
  )
}

/**
 * 测试提供商连接结果（success 标记来自后端包络层）
 */
export interface TestConnectionResult {
  success: boolean
  message?: string
  network_ok: boolean
  api_key_valid: boolean | null
  latency_ms: number | null
  error: string | null
  http_status: number | null
}

/**
 * 单个模型测试返回的工具调用摘要
 */
export interface ModelTestToolCall {
  id: string
  name: string
  arguments: Record<string, unknown>
}

/**
 * 嵌入对比测试的单组相似度结果
 */
export interface ModelTestEmbeddingPair {
  /** 对比说明，例如「相近文本」「不同图片」 */
  label: string
  /** 两组向量的余弦相似度 */
  similarity: number
}

/**
 * 单个模型测试结果
 */
export interface ModelTestResult {
  success: boolean
  model_name: string
  visual_tested: boolean
  tool_call_ok: boolean
  response: string
  reasoning: string
  tool_calls: ModelTestToolCall[]
  latency_ms: number | null
  error: string | null
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  /** 测试类型：chat / text_embedding / image_embedding */
  test_kind?: string
  /** 嵌入向量维度；仅嵌入测试返回 */
  embedding_dimension?: number | null
  /** 嵌入对比相似度结果；仅嵌入测试返回 */
  embedding_pairs?: ModelTestEmbeddingPair[]
}

/**
 * 测试提供商连接状态（通过提供商名称）
 * @param providerName 提供商名称
 */
export async function testProviderConnection(providerName: string): Promise<TestConnectionResult> {
  // Go 后端裁剪了网络探测端点；返回明确的未实现结果供 UI 提示
  return Promise.resolve({
    success: false,
    message: `Go 后端暂不支持连接测试（${providerName}）`,
    network_ok: false,
    api_key_valid: false,
    latency_ms: 0,
    error: 'not_implemented',
    http_status: 0,
  })
}

/**
 * 测试单个模型的文本、tool call 与可选视觉能力
 * @param modelName 模型名称
 */
export async function testModelCapability(modelName: string): Promise<ModelTestResult> {
  // Go 后端裁剪了网络探测端点；返回明确的未实现结果供 UI 提示
  return Promise.resolve({
    success: false,
    model_name: modelName,
    visual_tested: false,
    tool_call_ok: false,
    response: '',
    message: `Go 后端暂不支持模型能力测试（${modelName}）`,
    error: 'not_implemented',
  } as unknown as ModelTestResult)
}
