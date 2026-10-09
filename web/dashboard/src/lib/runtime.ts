/**
 * Runtime environment detection and information
 * 本 fork 仅支持纯 Web 运行时，Electron 分支已移除；保留导出兼容既有 import。
 */

/**
 * Type of runtime environment
 */
export type RuntimeKind = 'browser'

/**
 * Runtime information object
 */
export interface RuntimeInfo {
  /** Type of runtime (electron or browser) */
  kind: RuntimeKind
  /** Version information (electron versions, etc) */
  versions?: Record<string, string>
  /** User agent string */
  userAgent?: string
  /** Source of runtime detection */
  source: 'browser'
}

/**
 * Get current runtime information
 */
export function getRuntime(): RuntimeInfo {
  return {
    kind: 'browser',
    userAgent: typeof navigator !== 'undefined' ? navigator.userAgent : undefined,
    source: 'browser',
  }
}

/**
 * Check if running in Electron environment — 本 fork 恒为 false
 */
export function isElectron(): boolean {
  return false
}

/**
 * Get platform information
 */
export function getPlatform(): string {
  return 'browser'
}
