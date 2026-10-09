/**
 * 黑话（俚语）管理 API
 *
 * 请求样板（认证、解析、错误格式化）由 @/lib/http 的请求客户端承担；
 * 本文件只声明 endpoint 与业务错误文案。请求失败时抛出 ApiError（throw 契约）。
 */
import { backendApi } from '@/lib/http'
import type {
  JargonChatListResponse,
  JargonCreateRequest,
  JargonCreateResponse,
  JargonDeleteResponse,
  JargonDetailResponse,
  JargonExportResponse,
  JargonImportRequest,
  JargonImportResponse,
  JargonListResponse,
  JargonStatsResponse,
  Jargon,
  JargonUpdateRequest,
  JargonUpdateResponse,
} from '@/types/jargon'

const API_BASE = '/api/webui/jargon'

/**
 * 获取聊天列表（有黑话记录的聊天）
 */
export async function getJargonChatList(
  params: { include_empty?: boolean } = {}
): Promise<JargonChatListResponse> {
  return backendApi.get<JargonChatListResponse>(`${API_BASE}/chats`, {
    query: { include_empty: params.include_empty },
    errorMessage: '获取聊天列表失败',
  })
}

/**
 * 获取黑话列表
 */
export async function getJargonList(params: {
  page?: number
  page_size?: number
  search?: string
  session_id?: string
  jargon_status?: 'confirmed_jargon' | 'confirmed_not_jargon' | 'manual_jargon' | 'pending'
  is_jargon?: boolean | null
  is_complete?: boolean
  is_global?: boolean
}): Promise<JargonListResponse> {
  // Go 后端 /jargon/list：limit/offset；响应 { success, items, count }
  const data = await backendApi.get<{
    success: boolean
    items?: Array<{
      id: number
      content: string
      meaning?: string
      count?: number
      is_jargon?: boolean
      is_global?: boolean
      created_by?: string
    }>
    count?: number
  }>(`${API_BASE}/list`, {
    query: {
      limit: params.page_size || undefined,
      offset: params.page && params.page_size ? (params.page - 1) * params.page_size : undefined,
    },
    errorMessage: '获取黑话列表失败',
  })
  const items = data.items ?? []
  const mapped: Jargon[] = items.map((item) => ({
    id: item.id,
    content: item.content,
    meaning: item.meaning ?? null,
    session_id: '',
    session_ids: [],
    chat_name: null,
    chat_names: [],
    is_global: item.is_global === true,
    count: item.count ?? 0,
    is_jargon: item.is_jargon === true,
    is_legacy_empty_meaning: false,
    is_complete: true,
    created_by: (item.created_by === 'MANUAL' ? 'manual' : 'ai') as Jargon['created_by'],
    created_timestamp: '',
    updated_timestamp: '',
  }))
  return {
    success: data.success,
    total: data.count ?? mapped.length,
    page: params.page ?? 1,
    page_size: params.page_size ?? mapped.length,
    data: mapped,
  }
}

/**
 * 获取黑话详细信息
 */
export async function getJargonDetail(jargonId: number): Promise<JargonDetailResponse> {
  return backendApi.get<JargonDetailResponse>(`${API_BASE}/${jargonId}`, {
    errorMessage: '获取黑话详情失败',
  })
}

/**
 * 导出黑话
 */
export async function exportJargons(params: {
  ids?: number[]
  include_chat_info?: boolean
}): Promise<JargonExportResponse> {
  return backendApi.post<JargonExportResponse>(`${API_BASE}/export`, {
    body: {
      ids: params.ids,
      include_chat_info: params.include_chat_info ?? false,
    },
    errorMessage: '导出黑话失败',
  })
}

/**
 * 导入黑话
 */
export async function importJargons(data: JargonImportRequest): Promise<JargonImportResponse> {
  return backendApi.post<JargonImportResponse>(`${API_BASE}/import`, {
    body: data,
    errorMessage: '导入黑话失败',
  })
}

/**
 * 创建黑话
 */
export async function createJargon(data: JargonCreateRequest): Promise<JargonCreateResponse> {
  return backendApi.post<JargonCreateResponse>(`${API_BASE}/`, {
    body: data,
    errorMessage: '创建黑话失败',
  })
}

/**
 * 更新黑话（增量更新）
 */
export async function updateJargon(
  jargonId: number,
  data: JargonUpdateRequest
): Promise<JargonUpdateResponse> {
  return backendApi.patch<JargonUpdateResponse>(`${API_BASE}/${jargonId}`, {
    body: data,
    errorMessage: '更新黑话失败',
  })
}

/**
 * 删除黑话
 */
export async function deleteJargon(jargonId: number): Promise<JargonDeleteResponse> {
  return backendApi.delete<JargonDeleteResponse>(`${API_BASE}/${jargonId}`, {
    errorMessage: '删除黑话失败',
  })
}

/**
 * 批量删除黑话
 */
export async function batchDeleteJargons(jargonIds: number[]): Promise<JargonDeleteResponse> {
  return backendApi.post<JargonDeleteResponse>(`${API_BASE}/batch/delete`, {
    body: { ids: jargonIds },
    errorMessage: '批量删除黑话失败',
  })
}

/**
 * 获取黑话统计数据
 */
export async function getJargonStats(): Promise<JargonStatsResponse> {
  return backendApi.get<JargonStatsResponse>(`${API_BASE}/stats/summary`, {
    errorMessage: '获取黑话统计失败',
  })
}

/**
 * 批量设置黑话状态
 */
export async function batchSetJargonStatus(
  jargonIds: number[],
  isJargon: boolean
): Promise<JargonUpdateResponse> {
  return backendApi.post<JargonUpdateResponse>(`${API_BASE}/batch/set-jargon`, {
    query: { ids: jargonIds, is_jargon: isJargon },
    errorMessage: '批量设置黑话状态失败',
  })
}
