/**
 * 模型配置页引导 Hook — Tour 系统已移除，保留空实现以兼容调用方。
 */
export interface UseModelTourReturn {
  startTour: () => void
  isRunning: boolean
  stepIndex: number
}

export function useModelTour(_options: Record<string, unknown> = {}): UseModelTourReturn {
  void _options
  return {
    startTour: () => {},
    isRunning: false,
    stepIndex: 0,
  }
}
