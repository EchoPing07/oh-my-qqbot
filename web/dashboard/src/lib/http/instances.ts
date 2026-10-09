/**
 * 请求客户端的两个适配器实例。
 *
 * - backendApi：主后端（oh-my-qqbot HTTP API），Bearer 认证，401 跳转登录页
 * - statsApi：外部统计服务（保留实例形态，本 fork 未使用外部统计），无认证
 */
import { getApiBaseUrl } from '@/lib/api-base'

import { createApiClient } from './client'

/** 统计服务地址（外部服务；本 fork 不依赖，保留导出兼容既有 import） */
export const STATS_SERVICE_BASE_URL = ''

/** 主后端实例：浏览器同源 / Vite dev proxy */
export const backendApi = createApiClient({
  resolveBaseUrl: getApiBaseUrl,
  auth: 'bearer',
})

/** 统计服务实例：外部服务，不携带凭据 */
export const statsApi = createApiClient({
  resolveBaseUrl: () => STATS_SERVICE_BASE_URL,
  auth: 'none',
})

/**
 * 认证流程实例：不自动注入 token、不配置 401 跳转——
 * 登录验证、认证状态探测中 401 是正常业务结果，必须透传后端信息而不是跳转登录页。
 */
export const authApi = createApiClient({
  resolveBaseUrl: getApiBaseUrl,
  auth: 'none',
})
