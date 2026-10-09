import { backendApi } from '@/lib/http'

export type ChatStreamType = 'group' | 'private'

export interface ChatStream {
  id: number | null
  session_id: string
  display_name: string
  chat_type: ChatStreamType
  target_id: string
  platform: string
  account_id: string | null
  scope: string | null
  user_id: string | null
  user_nickname: string | null
  user_cardname: string | null
  group_id: string | null
  group_name: string | null
  message_count: number
  expression_count: number
  jargon_count: number
  created_at: number | null
  last_active_at: number | null
  latest_message: string
  latest_message_at: number | null
}

export interface ChatConfigRule {
  platform: string
  item_id: string
  type: ChatStreamType | string
  use?: boolean
  learn?: boolean
  is_default?: boolean
  is_platform_default?: boolean
  is_wildcard?: boolean
}

export interface ChatLearningStatus {
  use: boolean
  learn: boolean
  matched_rule: ChatConfigRule | null
}

export interface ChatTalkFrequencyRule {
  platform: string
  item_id: string
  type: ChatStreamType | string
  time: string
  value: number
  value_label: string
  target_priority: number
  time_priority: number | null
  time_active: boolean
  is_effective: boolean
  is_default_target: boolean
}

export interface ChatTalkFrequencyDetail {
  enabled: boolean
  base_value: number
  base_value_label: string
  effective_value: number
  effective_value_label: string
  current_time: string
  matched_rules: ChatTalkFrequencyRule[]
}

export interface ChatPromptDetail {
  base_prompt_type: ChatStreamType | string
  base_prompt_title: string
  base_prompt: string
  chat_prompts: ChatPromptRule[]
}

export interface ChatPromptRule {
  index: number
  platform: string
  item_id: string
  rule_type: ChatStreamType | string
  prompt: string
}

export interface AdapterPolicyStatus {
  allowed: boolean
  configured: boolean
  chat_type: ChatStreamType | string
  target_id: string
  list_type: string
  source: string
  reason: string
  matched_ids: string[]
}

export interface ChatAdapterStatus {
  adapter_id: string
  plugin_id: string
  gateway_name: string
  platform: string
  account_id: string | null
  scope: string | null
  protocol: string
  route_type: string
  send_bound: boolean
  receive_bound: boolean
  routed: boolean
  policy: AdapterPolicyStatus
}

export interface AdapterPolicyDefaults {
  group: 'allow' | 'block'
  private: 'allow' | 'block'
}

export type AdapterHostDefaultAction = 'inherit' | 'allow' | 'block'

export interface AdapterHostPolicySection {
  default_action: AdapterHostDefaultAction
  allow_ids: string[]
  deny_ids: string[]
}

export interface AdapterHostPolicy {
  group: AdapterHostPolicySection
  private: AdapterHostPolicySection
  policy_groups?: AdapterHostPolicyGroup[]
  active_group?: string
}

export interface AdapterHostPolicyGroup {
  id: string
  name: string
  group: AdapterHostPolicySection
  private: AdapterHostPolicySection
}

/** 聊天流的适配器放行状态：不允许时 reason 说明被阻止的原因 */
export interface SessionAdapterStatus {
  allowed: boolean
  reason: string
}

/** 适配器当前激活身份（换账号登录后 account_id 实时变化） */
export interface AdapterActiveIdentity {
  adapter_id: string
  plugin_id: string
  gateway_name: string
  platform: string
  account_id: string | null
  scope: string | null
}

/** 同一插件名下某账号的规则条目摘要（旧账号条目换号后不再生效但保留在配置中） */
export interface AdapterAccountEntry {
  adapter_id: string
  account_id: string
  platform: string
  gateway_name: string
  scope: string
  rules: Partial<Record<ChatStreamType, AdapterHostPolicySection>>
}

interface AdapterHostPolicyResponse {
  success: boolean
  plugin_id: string
  global_defaults: AdapterPolicyDefaults
  policy: AdapterHostPolicy
  active_identity?: AdapterActiveIdentity | null
  has_entry?: boolean
  account_entries?: AdapterAccountEntry[]
}

export interface ChatStreamDetail {
  session_id: string
  display_name: string
  chat_type: ChatStreamType
  platform: string
  target_id: string
  group_id: string | null
  user_id: string | null
  expression: ChatLearningStatus
  behavior?: ChatLearningStatus
  jargon: ChatLearningStatus
  talk_frequency: ChatTalkFrequencyDetail
  prompts: ChatPromptDetail
  adapters: ChatAdapterStatus[]
}

interface ChatTargetResolveResponse {
  success: boolean
  found: boolean
  session?: ChatStream | null
}

export interface ChatTargetResolveRequest {
  platform: string
  item_id: string
  rule_type: ChatStreamType | string
}

interface ChatTargetsResolveResponse {
  success: boolean
  results: Array<{
    found: boolean
    session?: ChatStream | null
  }>
}

export interface ChatStreamDeleteItem {
  key: string
  label: string
  count: number
  unlinked?: number
}

export interface ChatStreamDeleteResult {
  success: boolean
  session_id: string
  deleted_total: number
  jargons?: {
    deleted: number
    unlinked: number
    removed_refs: number
  }
  items: ChatStreamDeleteItem[]
}

interface UpdateTalkFrequencyPayload {
  previous_time?: string | null
  time: string
  value: number
}

interface UpdateLearningPayload {
  use: boolean
  learn: boolean
}

interface UpdateChatPromptPayload {
  prompt: string
}

interface UpdateAdapterPolicyPayload {
  adapter_id: string
  action: 'allow' | 'block' | 'inherit'
}

/** 把 Go 后端 chatSessionItem 映射为前端 ChatStream 形态。 */
function mapBackendSession(raw: Record<string, unknown>): ChatStream {
  const groupId = String(raw.group_id ?? '')
  const sessionId = String(raw.session_id ?? '')
  return {
    id: null,
    session_id: sessionId,
    display_name: String(raw.group_name || raw.user_nickname || raw.user_id || sessionId),
    chat_type: groupId ? 'group' : 'private',
    target_id: groupId || String(raw.user_id ?? ''),
    platform: String(raw.platform ?? ''),
    account_id: null,
    scope: null,
    user_id: String(raw.user_id ?? '') || null,
    user_nickname: String(raw.user_nickname ?? '') || null,
    user_cardname: null,
    group_id: groupId || null,
    group_name: String(raw.group_name ?? '') || null,
    message_count: 0,
    expression_count: 0,
    jargon_count: 0,
    created_at: raw.created_at ? Date.parse(String(raw.created_at)) / 1000 : null,
    last_active_at: raw.last_active_at ? Date.parse(String(raw.last_active_at)) / 1000 : null,
    latest_message: '',
    latest_message_at: null,
  }
}

export async function getChatStreams(limit = 1000): Promise<ChatStream[]> {
  const result = await backendApi.get<{ sessions?: Array<Record<string, unknown>> }>(
    '/api/webui/chat/sessions',
    { query: { limit } }
  )
  return (result.sessions ?? []).map(mapBackendSession)
}

/** 分页读取所有已存在的聊天流，供聊天工作区展示完整列表。 */
export async function getAllChatStreams(): Promise<ChatStream[]> {
  const pageSize = 1000
  const streams: ChatStream[] = []

  for (let offset = 0; ; offset += pageSize) {
    const result = await backendApi.get<{ sessions?: Array<Record<string, unknown>> }>(
      '/api/webui/chat/sessions',
      { query: { limit: pageSize } }
    )
    const page = (result.sessions ?? []).map(mapBackendSession)
    streams.push(...page)
    // Go 后端会话列表接口未提供 offset 分页：一页取满即止
    if (page.length < pageSize) return streams
  }
}

/** 聊天流适配器放行状态的查询键：适配器规则变化后按此前缀失效缓存 */
export const CHAT_ADAPTER_STATUS_QUERY_KEY = 'chat-adapter-status'

/** 批量读取聊天流的适配器放行状态，未返回的聊天流按放行处理 */
export async function getChatSessionsAdapterStatus(
  _sessionIds: string[]
): Promise<Record<string, SessionAdapterStatus>> {
  // Go 后端无多适配器体系：全部视为放行
  return {}
}

export async function resolveChatTarget(
  platform: string,
  itemId: string,
  ruleType: ChatStreamType | string
): Promise<ChatTargetResolveResponse> {
  const [result] = await resolveChatTargets([
    {
      platform,
      item_id: itemId,
      rule_type: ruleType,
    },
  ])
  return { success: true, found: Boolean(result?.found), session: result?.session ?? null }
}

export async function resolveChatTargets(
  targets: ChatTargetResolveRequest[]
): Promise<ChatTargetsResolveResponse['results']> {
  const result = await backendApi.post<ChatTargetsResolveResponse>('/api/webui/chat/platforms', {
    body: { targets },
    errorMessage: '解析聊天流失败',
  })
  return result.results ?? []
}

export async function getChatStreamDetail(sessionId: string): Promise<ChatStreamDetail> {
  // Go 后端未提供会话详情端点：返回最小详情（仅会话标识）
  return {
    detail: {
      stream: {
        id: null,
        session_id: sessionId,
        display_name: sessionId,
        chat_type: 'group',
        target_id: sessionId,
        platform: '',
        account_id: null,
        scope: null,
        user_id: null,
        user_nickname: null,
        user_cardname: null,
        group_id: null,
        group_name: null,
        message_count: 0,
        expression_count: 0,
        jargon_count: 0,
        created_at: null,
        last_active_at: null,
        latest_message: '',
        latest_message_at: null,
      },
    },
  } as unknown as ChatStreamDetail
}

export async function updateChatStreamTalkFrequency(
  sessionId: string,
  _payload: UpdateTalkFrequencyPayload
): Promise<ChatStreamDetail> {
  // Go 后端未提供发言频率端点：本地空实现（不做持久化）
  return getChatStreamDetail(typeof sessionId === 'string' ? sessionId : '')
}


export async function deleteChatStreamTalkFrequency(
  sessionId: string,
  _time: string
): Promise<ChatStreamDetail> {
  // Go 后端未提供发言频率端点：本地空实现（不做持久化）
  return getChatStreamDetail(typeof sessionId === 'string' ? sessionId : '')
}


export async function updateChatStreamLearning(
  sessionId: string,
  _kind: 'expression' | 'jargon' | 'behavior',
  _payload: UpdateLearningPayload
): Promise<ChatStreamDetail> {
  // Go 后端未提供学习配置端点：本地空实现（不做持久化）
  return getChatStreamDetail(typeof sessionId === 'string' ? sessionId : '')
}


export async function upsertChatStreamPrompt(
  sessionId: string,
  _payload: UpdateChatPromptPayload,
  _index?: number
): Promise<ChatStreamDetail> {
  // Go 后端未提供聊天 Prompt端点：本地空实现（不做持久化）
  return getChatStreamDetail(typeof sessionId === 'string' ? sessionId : '')
}


export async function deleteChatStreamPrompt(
  sessionId: string,
  _index: number
): Promise<ChatStreamDetail> {
  // Go 后端未提供聊天 Prompt端点：本地空实现（不做持久化）
  return getChatStreamDetail(typeof sessionId === 'string' ? sessionId : '')
}


export async function updateChatStreamAdapterPolicy(
  sessionId: string,
  _payload: UpdateAdapterPolicyPayload
): Promise<ChatStreamDetail> {
  // Go 后端未提供适配器规则端点：本地空实现（不做持久化）
  return getChatStreamDetail(typeof sessionId === 'string' ? sessionId : '')
}


export async function getAdapterPolicyDefaults(): Promise<AdapterPolicyDefaults> {
  // Go 后端无多适配器体系：返回空默认值
  return {} as AdapterPolicyDefaults
}

export async function updateAdapterPolicyDefaults(
  _defaults: AdapterPolicyDefaults
): Promise<void> {
  // Go 后端无多适配器体系：空实现
}

export async function getAdapterHostPolicy(pluginId: string): Promise<AdapterHostPolicyResponse> {
  return backendApi.get<AdapterHostPolicyResponse>(
    `/api/chat/adapters/plugins/${encodeURIComponent(pluginId)}/policy`,
    { errorMessage: '获取主程序放行规则失败' }
  )
}

export async function updateAdapterHostPolicy(
  pluginId: string,
  policy: AdapterHostPolicy
): Promise<AdapterHostPolicyResponse> {
  return backendApi.put<AdapterHostPolicyResponse>(
    `/api/chat/adapters/plugins/${encodeURIComponent(pluginId)}/policy`,
    { body: policy, errorMessage: '保存主程序放行规则失败' }
  )
}

export async function deleteChatStream(sessionId: string): Promise<ChatStreamDeleteResult> {
  return backendApi.delete<ChatStreamDeleteResult>(
    `/api/chat/sessions/${encodeURIComponent(sessionId)}`,
    {
      errorMessage: '删除聊天流失败',
    }
  )
}
