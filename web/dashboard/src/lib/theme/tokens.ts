/**
 * Design Token Schema — oh-my-qqbot 设计系统
 *
 * 设计语言：主黑白灰、辅红、线条风。
 *   - 所有界面由中性灰阶承载（背景/卡片/边框/文字）；
 *   - 唯一强调色为红（primary），仅用于主操作、激活态、关键数据；
 *   - 视觉以 1px 边框与细分隔线构成层次，弱化阴影与圆角。
 */

// ============================================================================
// Color Tokens 类型定义
// ============================================================================

export type ColorTokens = {
  primary: string
  'primary-foreground': string
  'primary-gradient': string
  secondary: string
  'secondary-foreground': string
  muted: string
  'muted-foreground': string
  accent: string
  'accent-foreground': string
  destructive: string
  'destructive-foreground': string
  background: string
  foreground: string
  card: string
  'card-foreground': string
  popover: string
  'popover-foreground': string
  border: string
  input: string
  ring: string
  'chart-1': string
  'chart-2': string
  'chart-3': string
  'chart-4': string
  'chart-5': string
}

// ============================================================================
// Typography Tokens 类型定义
// ============================================================================

export type TypographyTokens = {
  'font-family-base': string
  'font-family-code': string
  'font-size-xs': string
  'font-size-sm': string
  'font-size-base': string
  'font-size-lg': string
  'font-size-xl': string
  'font-size-2xl': string
  'font-weight-normal': number
  'font-weight-medium': number
  'font-weight-semibold': number
  'font-weight-bold': number
  'line-height-tight': number
  'line-height-normal': number
  'line-height-relaxed': number
  'letter-spacing-tight': string
  'letter-spacing-normal': string
  'letter-spacing-wide': string
}

// ============================================================================
// Visual Tokens 类型定义
// ============================================================================

export type VisualTokens = {
  'radius-sm': string
  'radius-md': string
  'radius-lg': string
  'radius-xl': string
  'radius-full': string
  'shadow-sm': string
  'shadow-md': string
  'shadow-lg': string
  'shadow-xl': string
  'blur-sm': string
  'blur-md': string
  'blur-lg': string
  'opacity-disabled': number
  'opacity-hover': number
  'opacity-overlay': number
}

// ============================================================================
// Layout Tokens 类型定义
// ============================================================================

export type LayoutTokens = {
  'space-xs': string
  'space-sm': string
  'space-md': string
  'space-lg': string
  'space-xl': string
  'space-2xl': string
  'sidebar-width': string
  'sidebar-logo-height': string
  'sidebar-logo-padding-x': string
  'sidebar-nav-padding': string
  'sidebar-nav-padding-collapsed': string
  'sidebar-section-gap': string
  'sidebar-section-title-height': string
  'sidebar-section-title-margin-bottom': string
  'sidebar-section-title-margin-bottom-collapsed': string
  'sidebar-nav-item-gap': string
  'sidebar-collapsed-width': string
  'sidebar-nav-item-height': string
  'sidebar-nav-item-padding-x': string
  'sidebar-nav-item-collapsed-width': string
  'header-height': string
}

// ============================================================================
// Animation Tokens 类型定义
// ============================================================================

export type AnimationTokens = {
  'anim-duration-fast': string
  'anim-duration-normal': string
  'anim-duration-slow': string
  'anim-easing-default': string
  'anim-easing-in': string
  'anim-easing-out': string
  'anim-easing-in-out': string
  'transition-colors': string
  'transition-transform': string
  'transition-opacity': string
}

export type ThemeTokens = {
  color: ColorTokens
  typography: TypographyTokens
  visual: VisualTokens
  layout: LayoutTokens
  animation: AnimationTokens
}

// ============================================================================
// Theme Preset & Config Types
// ============================================================================

export type ThemePreset = {
  id: string
  name: string
  description: string
  tokens: ThemeTokens
  isDark: boolean
}

/**
 * 界面风格只保留一种（黑白灰线条风）。
 * 保留类型与字段形态以兼容既有存储/同步逻辑。
 */
export type DashboardStyle = 'modern'

export const DASHBOARD_STYLES: readonly DashboardStyle[] = ['modern']

export type StyleTokenOverrides = Partial<Record<DashboardStyle, Partial<ThemeTokens>>>
export type StyleCustomCSS = Partial<Record<DashboardStyle, string>>

export const DEFAULT_DASHBOARD_STYLE: DashboardStyle = 'modern'

export type UserThemeConfig = {
  selectedPreset: string
  accentColor: string
  styleTokenOverrides: StyleTokenOverrides
  styleCustomCSS: StyleCustomCSS
  dashboardStyle: DashboardStyle
}

// ============================================================================
// Default Light Tokens（亮色：纸白 + 墨黑 + 红）
// ============================================================================

const sharedTypography: TypographyTokens = {
  'font-family-base':
    '-apple-system, BlinkMacSystemFont, "Segoe UI", "Microsoft YaHei UI", "PingFang SC", Roboto, "Helvetica Neue", Arial, sans-serif',
  'font-family-code': '"JetBrains Mono", "Cascadia Mono", Consolas, "Courier New", monospace',
  'font-size-xs': '0.75rem',
  'font-size-sm': '0.875rem',
  'font-size-base': '1rem',
  'font-size-lg': '1.125rem',
  'font-size-xl': '1.25rem',
  'font-size-2xl': '1.5rem',
  'font-weight-normal': 400,
  'font-weight-medium': 500,
  'font-weight-semibold': 600,
  'font-weight-bold': 700,
  'line-height-tight': 1.2,
  'line-height-normal': 1.5,
  'line-height-relaxed': 1.75,
  'letter-spacing-tight': '-0.01em',
  'letter-spacing-normal': '0em',
  'letter-spacing-wide': '0.02em',
}

/** 线条风视觉：小圆角、极弱阴影（层次靠边框与分隔线）。 */
const sharedVisual: VisualTokens = {
  'radius-sm': '2px',
  'radius-md': '4px',
  'radius-lg': '6px',
  'radius-xl': '8px',
  'radius-full': '9999px',
  'shadow-sm': '0 1px 2px 0 rgba(0, 0, 0, 0.05)',
  'shadow-md': '0 2px 6px -1px rgba(0, 0, 0, 0.07)',
  'shadow-lg': '0 4px 12px -3px rgba(0, 0, 0, 0.09)',
  'shadow-xl': '0 8px 24px -6px rgba(0, 0, 0, 0.1)',
  'blur-sm': '4px',
  'blur-md': '12px',
  'blur-lg': '24px',
  'opacity-disabled': 0.5,
  'opacity-hover': 0.8,
  'opacity-overlay': 0.75,
}

const sharedLayout: LayoutTokens = {
  'space-xs': '0.5rem',
  'space-sm': '0.75rem',
  'space-md': '1rem',
  'space-lg': '1.5rem',
  'space-xl': '2rem',
  'space-2xl': '3rem',
  'sidebar-width': '13rem',
  'sidebar-logo-height': '4.5rem',
  'sidebar-logo-padding-x': '1rem',
  'sidebar-nav-padding': '0.75rem',
  'sidebar-nav-padding-collapsed': '0.5rem',
  'sidebar-section-gap': '0.75rem',
  'sidebar-section-title-height': '1.25rem',
  'sidebar-section-title-margin-bottom': '0.5rem',
  'sidebar-section-title-margin-bottom-collapsed': '0.25rem',
  'sidebar-nav-item-gap': '0.125rem',
  'sidebar-collapsed-width': '4rem',
  'sidebar-nav-item-height': '2.375rem',
  'sidebar-nav-item-padding-x': '0.75rem',
  'sidebar-nav-item-collapsed-width': '3rem',
  'header-height': '3.25rem',
}

const sharedAnimation: AnimationTokens = {
  'anim-duration-fast': '120ms',
  'anim-duration-normal': '200ms',
  'anim-duration-slow': '320ms',
  'anim-easing-default': 'cubic-bezier(0.4, 0, 0.2, 1)',
  'anim-easing-in': 'cubic-bezier(0.4, 0, 1, 1)',
  'anim-easing-out': 'cubic-bezier(0, 0, 0.2, 1)',
  'anim-easing-in-out': 'cubic-bezier(0.4, 0, 0.2, 1)',
  'transition-colors': 'color 160ms cubic-bezier(0.4, 0, 0.2, 1)',
  'transition-transform': 'transform 160ms cubic-bezier(0.4, 0, 0.2, 1)',
  'transition-opacity': 'opacity 160ms cubic-bezier(0.4, 0, 0.2, 1)',
}

export const defaultLightTokens: ThemeTokens = {
  color: {
    primary: '0 72% 46%',
    'primary-foreground': '0 0% 100%',
    'primary-gradient': 'none',
    secondary: '0 0% 94%',
    'secondary-foreground': '0 0% 20%',
    muted: '0 0% 95%',
    'muted-foreground': '0 0% 42%',
    accent: '0 0% 93%',
    'accent-foreground': '0 0% 10%',
    destructive: '0 74% 46%',
    'destructive-foreground': '0 0% 100%',
    background: '0 0% 97%',
    foreground: '0 0% 10%',
    card: '0 0% 100%',
    'card-foreground': '0 0% 10%',
    popover: '0 0% 100%',
    'popover-foreground': '0 0% 10%',
    border: '0 0% 87%',
    input: '0 0% 84%',
    ring: '0 72% 46%',
    'chart-1': '0 72% 46%',
    'chart-2': '0 0% 30%',
    'chart-3': '0 0% 55%',
    'chart-4': '0 0% 74%',
    'chart-5': '0 42% 62%',
  },
  typography: sharedTypography,
  visual: sharedVisual,
  layout: sharedLayout,
  animation: sharedAnimation,
}

// ============================================================================
// Default Dark Tokens（暗色：墨黑 + 纸白 + 红）
// ============================================================================

export const defaultDarkTokens: ThemeTokens = {
  color: {
    primary: '0 70% 56%',
    'primary-foreground': '0 0% 100%',
    'primary-gradient': 'none',
    secondary: '0 0% 17%',
    'secondary-foreground': '0 0% 90%',
    muted: '0 0% 15%',
    'muted-foreground': '0 0% 62%',
    accent: '0 0% 19%',
    'accent-foreground': '0 0% 92%',
    destructive: '0 68% 52%',
    'destructive-foreground': '0 0% 100%',
    background: '0 0% 7%',
    foreground: '0 0% 92%',
    card: '0 0% 11%',
    'card-foreground': '0 0% 92%',
    popover: '0 0% 12%',
    'popover-foreground': '0 0% 92%',
    border: '0 0% 22%',
    input: '0 0% 25%',
    ring: '0 70% 56%',
    'chart-1': '0 70% 56%',
    'chart-2': '0 0% 70%',
    'chart-3': '0 0% 48%',
    'chart-4': '0 0% 30%',
    'chart-5': '0 36% 58%',
  },
  typography: sharedTypography,
  visual: sharedVisual,
  layout: sharedLayout,
  animation: sharedAnimation,
}

// ============================================================================
// Token Utility Functions
// ============================================================================

/**
 * 将 Token 类别和 key 转换为 CSS 变量名
 * @example tokenToCSSVarName('color', 'primary') => '--color-primary'
 */
export function tokenToCSSVarName(
  category: keyof ThemeTokens | 'color' | 'typography' | 'visual' | 'layout' | 'animation',
  key: string
): string {
  return `--${category}-${key}`
}
