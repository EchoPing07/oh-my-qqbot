/**
 * 主题配置的 localStorage 存储管理模块。
 *
 * 单一风格（黑白灰线条风）后仅保留：模式/预设/强调色/token 覆盖/自定义 CSS。
 */

import { DEFAULT_ACCENT_COLOR_HSL, normalizeAccentColor } from './palette'
import { DEFAULT_DASHBOARD_STYLE } from './tokens'
import type { DashboardStyle, StyleCustomCSS, StyleTokenOverrides, UserThemeConfig } from './tokens'

export const THEME_STORAGE_KEYS = {
  MODE: 'omb-theme-mode',
  PRESET: 'omb-theme-preset',
  ACCENT: 'omb-theme-accent',
  STYLE_OVERRIDES: 'omb-theme-style-overrides',
  STYLE_CUSTOM_CSS: 'omb-theme-style-custom-css',
  DASHBOARD_STYLE: 'omb-theme-dashboard-style',
} as const

const DEFAULT_THEME_CONFIG: UserThemeConfig = {
  selectedPreset: 'light',
  accentColor: DEFAULT_ACCENT_COLOR_HSL,
  styleTokenOverrides: {},
  styleCustomCSS: {},
  dashboardStyle: DEFAULT_DASHBOARD_STYLE,
}

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null

type ImportThemeConfigRecord = Record<string, unknown> &
  Pick<UserThemeConfig, 'selectedPreset' | 'accentColor'>

function hasRequiredImportThemeFields(
  config: Record<string, unknown>,
  errors: string[]
): config is ImportThemeConfigRecord {
  let valid = true

  if (typeof config.selectedPreset !== 'string') {
    errors.push('selectedPreset must be a string')
    valid = false
  }
  if (typeof config.accentColor !== 'string') {
    errors.push('accentColor must be a string')
    valid = false
  }

  return valid
}

function normalizeDashboardStyle(value: unknown): DashboardStyle {
  // 单一风格：无论存储值是什么都归一化为 modern。
  void value
  return DEFAULT_DASHBOARD_STYLE
}
void normalizeDashboardStyle

function normalizeStyleTokenOverrides(value: unknown): StyleTokenOverrides {
  if (!isRecord(value)) {
    return {}
  }

  const nextOverrides: StyleTokenOverrides = {}
  for (const style of DASHBOARD_STYLES_KEYS) {
    if (isRecord(value[style])) {
      nextOverrides[style] = value[style] as StyleTokenOverrides[typeof style]
    }
  }

  return nextOverrides
}

function normalizeStyleCustomCSS(value: unknown): StyleCustomCSS {
  if (!isRecord(value)) {
    return {}
  }

  const nextCustomCSS: StyleCustomCSS = {}
  if (typeof value.modern === 'string') {
    nextCustomCSS.modern = value.modern
  }

  return nextCustomCSS
}

const DASHBOARD_STYLES_KEYS = ['modern'] as const

function parseJSONStorage(key: string): unknown {
  const value = localStorage.getItem(key)
  if (!value) {
    return undefined
  }

  return JSON.parse(value)
}

export function loadThemeConfig(): UserThemeConfig {
  const preset = localStorage.getItem(THEME_STORAGE_KEYS.PRESET)
  const accent = localStorage.getItem(THEME_STORAGE_KEYS.ACCENT)

  let styleTokenOverrides: StyleTokenOverrides = {}
  let styleCustomCSS: StyleCustomCSS = {}

  try {
    styleTokenOverrides = normalizeStyleTokenOverrides(
      parseJSONStorage(THEME_STORAGE_KEYS.STYLE_OVERRIDES)
    )
  } catch {
    styleTokenOverrides = {}
  }

  try {
    styleCustomCSS = normalizeStyleCustomCSS(parseJSONStorage(THEME_STORAGE_KEYS.STYLE_CUSTOM_CSS))
  } catch {
    styleCustomCSS = {}
  }

  return {
    selectedPreset: preset || DEFAULT_THEME_CONFIG.selectedPreset,
    accentColor: normalizeAccentColor(accent),
    styleTokenOverrides,
    styleCustomCSS,
    dashboardStyle: DEFAULT_THEME_CONFIG.dashboardStyle,
  }
}

export function saveThemeConfig(config: UserThemeConfig): void {
  localStorage.setItem(THEME_STORAGE_KEYS.PRESET, config.selectedPreset)
  localStorage.setItem(THEME_STORAGE_KEYS.ACCENT, normalizeAccentColor(config.accentColor))
  localStorage.setItem(
    THEME_STORAGE_KEYS.STYLE_OVERRIDES,
    JSON.stringify(normalizeStyleTokenOverrides(config.styleTokenOverrides))
  )
  localStorage.setItem(
    THEME_STORAGE_KEYS.STYLE_CUSTOM_CSS,
    JSON.stringify(normalizeStyleCustomCSS(config.styleCustomCSS))
  )
}

export function saveThemePartial(partial: Partial<UserThemeConfig>): void {
  const current = loadThemeConfig()
  saveThemeConfig({
    ...current,
    ...partial,
  })
}

export function exportThemeJSON(): string {
  const config = loadThemeConfig()
  return JSON.stringify(config, null, 2)
}

export function importThemeJSON(json: string): { success: boolean; errors: string[] } {
  const errors: string[] = []

  let config: unknown
  try {
    config = JSON.parse(json)
  } catch (error) {
    return {
      success: false,
      errors: [`Invalid JSON format: ${error instanceof Error ? error.message : 'Unknown error'}`],
    }
  }

  if (!isRecord(config)) {
    return {
      success: false,
      errors: ['Configuration must be a JSON object'],
    }
  }

  const hasRequiredFields = hasRequiredImportThemeFields(config, errors)
  if (
    config.styleTokenOverrides !== undefined &&
    (typeof config.styleTokenOverrides !== 'object' || config.styleTokenOverrides === null)
  ) {
    errors.push('styleTokenOverrides must be an object')
  }
  if (
    config.styleCustomCSS !== undefined &&
    (typeof config.styleCustomCSS !== 'object' || config.styleCustomCSS === null)
  ) {
    errors.push('styleCustomCSS must be an object')
  }

  if (errors.length > 0) {
    return { success: false, errors }
  }
  const selectedPreset = config.selectedPreset
  const accentColor = config.accentColor
  if (!hasRequiredFields || typeof selectedPreset !== 'string' || typeof accentColor !== 'string') {
    throw new Error('Theme import required fields were not narrowed after validation')
  }

  saveThemeConfig({
    selectedPreset,
    accentColor,
    styleTokenOverrides: normalizeStyleTokenOverrides(config.styleTokenOverrides),
    styleCustomCSS: normalizeStyleCustomCSS(config.styleCustomCSS),
    dashboardStyle: DEFAULT_DASHBOARD_STYLE,
  })

  return { success: true, errors: [] }
}

export function resetThemeToDefault(): void {
  Object.values(THEME_STORAGE_KEYS).forEach((key) => {
    localStorage.removeItem(key)
  })
}
