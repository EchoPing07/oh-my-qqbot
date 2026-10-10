/**
 * 配置 API — 对齐 internal/webui/routers/config.go。
 * 配置以 TOML 键名字典形态返回/提交（{config: {...}}）。
 *
 * S2 脱敏语义：GET /config/bot|model 不回传明文密钥 — 已配置的
 * app_secret / api_key 以 SECRET_PLACEHOLDER（__SAVED__）占位，顶层附带
 * has_app_secret / has_api_keys 布尔；PUT 时占位符或空串由后端保留磁盘旧值
 * （前端「留空 = 保留旧密钥」）。
 */

import { api } from '@/lib/http'

/** 密钥回显占位符（后端 secretPlaceholder）。 */
export const SECRET_PLACEHOLDER = '__SAVED__'

export type ConfigData = Record<string, unknown>

/** GET /config/bot 响应（config + 密钥状态标记）。 */
export interface BotConfigResponse {
  config: ConfigData
  has_app_secret?: boolean
}

/** GET /config/model 响应（config + 各 provider 密钥状态）。 */
export interface ModelConfigResponse {
  config: ConfigData
  has_api_keys?: Record<string, boolean>
}

/** GET /config/bot：完整 bot 配置（TOML 键名字典，密钥已脱敏）。 */
export async function getBotConfig(): Promise<ConfigData> {
  const data = await api.get<BotConfigResponse>('/api/webui/config/bot', { envelope: 'raw' })
  return data.config ?? {}
}

/** GET /config/bot：连同 has_app_secret 一起返回（凭据卡展示状态用）。 */
export async function getBotConfigWithState(): Promise<BotConfigResponse> {
  const data = await api.get<BotConfigResponse>('/api/webui/config/bot', { envelope: 'raw' })
  return { config: data.config ?? {}, has_app_secret: data.has_app_secret ?? false }
}

/** POST /config/bot：整份保存 bot 配置（密钥留空/占位符即保留旧值）。 */
export async function saveBotConfig(config: ConfigData): Promise<void> {
  await api.post('/api/webui/config/bot', { body: { config } })
}

/** GET /config/model：完整模型配置（TOML 键名字典，api_key 已脱敏）。 */
export async function getModelConfig(): Promise<ConfigData> {
  const data = await api.get<ModelConfigResponse>('/api/webui/config/model', { envelope: 'raw' })
  return data.config ?? {}
}

/** POST /config/model：整份保存模型配置（api_key 留空/占位符即保留旧值）。 */
export async function saveModelConfig(config: ConfigData): Promise<void> {
  await api.post('/api/webui/config/model', { body: { config } })
}

/** GET /config/schema/bot：配置节清单（前端用于渲染节导航）。 */
export interface BotSchemaSection {
  name: string
  type: string
  title: string
}

/** GET /config/schema/bot：配置节清单（有序数组，「机器人」在第一位）。 */
export async function getBotSchema(): Promise<BotSchemaSection[]> {
  const data = await api.get<{ schema: { properties: BotSchemaSection[] } }>(
    '/api/webui/config/schema/bot',
    { envelope: 'raw' }
  )
  return data.schema?.properties ?? []
}

// ── 便捷读写：按节取/存（组装成整份配置提交，避开后端不存在的 section 端点） ──

/** 读取 bot 配置的某一节（缺省返回 {}）。 */
export async function getBotSection<T = ConfigData>(section: string): Promise<T> {
  const config = await getBotConfig()
  const value = config[section]
  return (value && typeof value === 'object' ? value : {}) as T
}

/** 只更新 bot 配置的某一节：读整份 → 覆盖该节 → 整份保存（原子性由后端保证）。 */
export async function saveBotSection(section: string, sectionData: ConfigData): Promise<void> {
  const config = await getBotConfig()
  await saveBotConfig({ ...config, [section]: sectionData })
}
