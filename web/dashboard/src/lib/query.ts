/**
 * React Query 全局配置。
 * 后端为单机本地服务，全局禁用窗口聚焦重取（避免无谓刷新），失败重试 1 次。
 */

import { QueryClient } from '@tanstack/react-query'
import { ApiError } from '@/lib/http'

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: (failureCount, error) => {
        // 4xx 不重试（除 408/429）
        if (error instanceof ApiError && error.status !== undefined) {
          if (error.status === 408 || error.status === 429) return failureCount < 2
          return false
        }
        return failureCount < 1
      },
      refetchOnWindowFocus: false,
      staleTime: 15_000,
    },
  },
})
