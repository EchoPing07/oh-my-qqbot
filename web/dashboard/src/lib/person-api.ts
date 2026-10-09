/**
 * 人物信息管理 API
 *
 * 请求样板（认证、解析、错误格式化）由 @/lib/http 的请求客户端承担；
 * 本文件只声明 endpoint、业务错误文案与响应体 success 标记的解包规则。
 * 已切换为 throw 契约：成功直接返回数据，失败抛出 ApiError（配合 TanStack Query 使用）。
 */
import { ApiError, backendApi, requireSuccess } from '@/lib/http'
import type {
  PersonDeleteResponse,
  PersonDetailResponse,
  PersonInfo,
  PersonStats,
  PersonUpdateRequest,
  PersonUpdateResponse,
} from '@/types/person'

const API_BASE = '/api/webui/person'

/**
 * Person list response with pagination info
 */
export interface PersonListData {
  data: PersonInfo[]
  total: number
  page: number
  page_size: number
}

/**
 * 获取人物信息列表
 */
export async function getPersonList(params: {
  page?: number
  page_size?: number
  search?: string
  is_known?: boolean
  platform?: string
  user_id?: string
}): Promise<PersonListData> {
  // Go 后端 /person/list：limit/offset；响应 { success, items, count }
  const data = await backendApi.get<{
    success: boolean
    items?: Array<{
      person_id: string
      person_name?: string
      user_nickname?: string
      platform?: string
      user_id?: string
      know_times?: number
      memory_points?: number
      is_known?: boolean
      last_known_time?: string | null
    }>
    count?: number
  }>(`${API_BASE}/list`, {
    query: {
      limit: params.page_size || undefined,
      offset: params.page && params.page_size ? (params.page - 1) * params.page_size : undefined,
    },
    errorMessage: '获取人物列表失败',
  })
  const items = data.items ?? []
  const mapped: PersonInfo[] = items.map((item, index) => ({
    id: index,
    is_known: item.is_known === true,
    person_id: item.person_id,
    person_name: item.person_name ?? null,
    name_reason: null,
    platform: item.platform ?? '',
    user_id: item.user_id ?? '',
    nickname: item.user_nickname ?? null,
    group_nick_name: null,
    memory_points: item.memory_points != null ? String(item.memory_points) : null,
    know_times: item.know_times ?? 0,
    know_since: null,
    last_know: item.last_known_time ? Date.parse(item.last_known_time) : null,
  }))
  return {
    data: mapped,
    total: data.count ?? mapped.length,
    page: params.page ?? 1,
    page_size: params.page_size ?? mapped.length,
  }
}

/**
 * 获取人物详细信息
 */
export async function getPersonDetail(personId: string): Promise<PersonInfo> {
  const data = await backendApi.get<PersonDetailResponse>(`${API_BASE}/${personId}`, {
    errorMessage: '获取人物详情失败',
  })
  return requireSuccess(data, '获取人物详情失败').data
}

/**
 * 更新人物信息（增量更新）
 */
export async function updatePerson(
  personId: string,
  data: PersonUpdateRequest
): Promise<PersonInfo> {
  const responseData = await backendApi.patch<PersonUpdateResponse>(`${API_BASE}/${personId}`, {
    body: data,
    errorMessage: '更新人物信息失败',
  })
  const checked = requireSuccess(responseData, '更新人物信息失败')
  if (!checked.data) {
    throw new ApiError(checked.message || '更新人物信息失败', { detail: checked })
  }
  return checked.data
}

/**
 * 删除人物信息
 */
export async function deletePerson(personId: string): Promise<void> {
  const data = await backendApi.delete<PersonDeleteResponse>(`${API_BASE}/${personId}`, {
    errorMessage: '删除人物信息失败',
  })
  requireSuccess(data, '删除人物信息失败')
}

/**
 * 获取人物统计数据
 */
export async function getPersonStats(): Promise<PersonStats> {
  // Go 后端 /person/stats/summary：{ success, total, known }
  const data = await backendApi.get<{ success: boolean; total?: number; known?: number }>(
    `${API_BASE}/stats/summary`,
    { errorMessage: '获取统计数据失败' }
  )
  const total = data.total ?? 0
  const known = data.known ?? 0
  return { total, known, unknown: Math.max(0, total - known), platforms: {} }
}

/**
 * 批量删除人物信息
 */
export async function batchDeletePersons(personIds: string[]): Promise<{
  message: string
  deleted_count: number
  failed_count: number
  failed_ids: string[]
}> {
  const data = await backendApi.post<{
    success: boolean
    message: string
    deleted_count: number
    failed_count: number
    failed_ids: string[]
  }>(`${API_BASE}/batch/delete`, {
    body: { person_ids: personIds },
    errorMessage: '批量删除失败',
  })
  const checked = requireSuccess(data, '批量删除失败')
  return {
    message: checked.message,
    deleted_count: checked.deleted_count,
    failed_count: checked.failed_count,
    failed_ids: checked.failed_ids,
  }
}
