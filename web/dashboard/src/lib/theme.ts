/**
 * 主题（亮/暗/跟随系统）— 极简实现。
 * 只有 class 策略：<html class="dark">，与 index.html 的首屏防闪烁脚本一致。
 */

export type ThemeMode = 'light' | 'dark' | 'system'

const STORAGE_KEY = 'omb-theme-mode'

export function readStoredTheme(): ThemeMode {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (v === 'light' || v === 'dark' || v === 'system') return v
  } catch {
    // localStorage 不可用（隐私模式）时走默认值
  }
  return 'system'
}

export function applyTheme(mode: ThemeMode): void {
  const dark =
    mode === 'dark' ||
    (mode === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches)
  document.documentElement.classList.toggle('dark', dark)
}

export function storeTheme(mode: ThemeMode): void {
  try {
    localStorage.setItem(STORAGE_KEY, mode)
  } catch {
    // 忽略写入失败
  }
}

/** 订阅系统主题变化（mode === 'system' 时跟随）。返回清理函数。 */
export function watchSystemTheme(mode: ThemeMode, onChange: () => void): () => void {
  if (mode !== 'system') return () => {}
  const mql = window.matchMedia('(prefers-color-scheme: dark)')
  mql.addEventListener('change', onChange)
  return () => mql.removeEventListener('change', onChange)
}
