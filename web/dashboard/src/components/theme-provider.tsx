import { useCallback, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'

import { ThemeProviderContext } from '@/lib/theme-context'
import { getBotConfig, updateBotConfigSection } from '@/lib/config-api'
import { DEFAULT_DASHBOARD_STYLE } from '@/lib/theme/tokens'
import type { DashboardStyle, UserThemeConfig } from '@/lib/theme/tokens'
import {
  THEME_STORAGE_KEYS,
  loadThemeConfig,
  resetThemeToDefault,
  saveThemePartial,
} from '@/lib/theme/storage'
import { applyThemePipeline, removeCustomCSS } from '@/lib/theme/pipeline'

type Theme = 'dark' | 'light' | 'system'

type ThemeProviderProps = {
  children: ReactNode
  defaultTheme?: Theme
  storageKey?: string
}

type WebUIStyleConfigValue = 0 | 1 | 2

// bot_config 的 webui.webui_style 用整数保存界面风格：0 黑白灰线条风。
// 1/2 是历史值（未来复古/千禧），读取时归一化为 0。
const DASHBOARD_STYLE_CONFIG_VALUES: Record<DashboardStyle, WebUIStyleConfigValue> = {
  modern: 0,
}

function dashboardStyleToConfigValue(style: DashboardStyle): WebUIStyleConfigValue {
  return DASHBOARD_STYLE_CONFIG_VALUES[style] ?? 0
}

function configValueToDashboardStyle(value: unknown): DashboardStyle {
  // 单一风格：任何历史值都归一化为 modern。
  void value
  return DEFAULT_DASHBOARD_STYLE
}

function shouldSyncRemoteWebUIStyle(): boolean {
  return !window.location.pathname.startsWith('/auth')
}

export function ThemeProvider({
  children,
  defaultTheme = 'system',
}: ThemeProviderProps) {
  const [themeMode, setThemeMode] = useState<Theme>(() => {
    const saved = localStorage.getItem(THEME_STORAGE_KEYS.MODE) as Theme | null
    return saved || defaultTheme
  })
  const [themeConfig, setThemeConfig] = useState<UserThemeConfig>(() => loadThemeConfig())
  const [systemThemeTick, setSystemThemeTick] = useState(0)
  const [remoteStyleLoaded, setRemoteStyleLoaded] = useState(false)

  const resolvedTheme = useMemo<'dark' | 'light'>(() => {
    void systemThemeTick
    if (themeMode !== 'system') return themeMode
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  }, [themeMode, systemThemeTick])

  useEffect(() => {
    const mediaQuery = window.matchMedia('(prefers-color-scheme: dark)')
    const handleChange = () => {
      if (themeMode === 'system') {
        setSystemThemeTick((prev) => prev + 1)
      }
    }
    mediaQuery.addEventListener('change', handleChange)
    return () => {
      mediaQuery.removeEventListener('change', handleChange)
    }
  }, [themeMode])

  useEffect(() => {
    const root = document.documentElement
    root.classList.remove('light', 'dark')
    root.classList.add(resolvedTheme)
    root.dataset.dashboardStyle = DEFAULT_DASHBOARD_STYLE

    applyThemePipeline(themeConfig, resolvedTheme === 'dark')
  }, [resolvedTheme, themeConfig])

  // 后端 webui_style 只作历史值归一化，不再驱动本地风格切换。
  useEffect(() => {
    if (remoteStyleLoaded || !shouldSyncRemoteWebUIStyle()) {
      return
    }
    let cancelled = false
    void (async () => {
      try {
        const result = await getBotConfig()
        if (cancelled) return
        const webuiConfig = result.webui as Record<string, unknown> | undefined
        if (webuiConfig && 'webui_style' in webuiConfig) {
          setThemeConfig((prev) => ({
            ...prev,
            dashboardStyle: configValueToDashboardStyle(webuiConfig.webui_style),
          }))
        }
      } catch (error) {
        console.debug('同步 WebUI 风格配置失败:', error)
      } finally {
        if (!cancelled) setRemoteStyleLoaded(true)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [remoteStyleLoaded])

  const persistRemoteWebUIStyle = useCallback(async (_style: DashboardStyle) => {
    if (!shouldSyncRemoteWebUIStyle()) {
      return
    }
    try {
      await updateBotConfigSection('webui', { webui_style: dashboardStyleToConfigValue(_style) })
    } catch (error) {
      console.warn('保存 WebUI 风格配置失败:', error)
    }
  }, [])

  const setTheme = useCallback((mode: Theme) => {
    localStorage.setItem(THEME_STORAGE_KEYS.MODE, mode)
    setThemeMode(mode)
  }, [])

  const updateThemeConfig = useCallback(
    (partial: Partial<UserThemeConfig>) => {
      saveThemePartial(partial)
      setThemeConfig((prev) => ({ ...prev, ...partial }))

      if (partial.dashboardStyle) {
        void persistRemoteWebUIStyle(partial.dashboardStyle)
      }
    },
    [persistRemoteWebUIStyle]
  )

  const resetTheme = useCallback(() => {
    resetThemeToDefault()
    removeCustomCSS()
    const defaultThemeConfig = loadThemeConfig()
    setThemeConfig(defaultThemeConfig)
    void persistRemoteWebUIStyle(defaultThemeConfig.dashboardStyle)
  }, [persistRemoteWebUIStyle])

  const value = useMemo(
    () => ({
      theme: themeMode,
      resolvedTheme,
      setTheme,
      themeConfig,
      updateThemeConfig,
      resetTheme,
    }),
    [themeMode, resolvedTheme, setTheme, themeConfig, updateThemeConfig, resetTheme]
  )

  return <ThemeProviderContext value={value}>{children}</ThemeProviderContext>
}
