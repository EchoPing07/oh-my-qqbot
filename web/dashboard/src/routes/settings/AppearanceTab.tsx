import { useState, useMemo, useRef, useCallback, useEffect } from 'react'
import { useTranslation } from 'react-i18next'
import {
  AlertTriangle,
  Download,
  Monitor,
  Moon,
  RotateCcw,
  Sun,
  Trash2,
  Upload,
} from 'lucide-react'

import {
  Accordion,
  AccordionContent,
  AccordionItem,
  AccordionTrigger,
} from '@/components/ui/accordion'
import { AlertDialogAction, AlertDialogCancel } from '@/components/ui/alert-dialog'
import { AlertDialog, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle, AlertDialogTrigger } from '@/components/ui/alert-dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Slider } from '@/components/ui/slider'
import { Switch } from '@/components/ui/switch'
import { useTheme } from '@/components/use-theme'
import { useToast } from '@/hooks/use-toast'
import { cn } from '@/lib/utils'
import { applyThemePipeline, getComputedTokens } from '@/lib/theme/pipeline'
import { DEFAULT_ACCENT_COLOR_HEX, DEFAULT_ACCENT_COLOR_HSL, hexToHSL } from '@/lib/theme/palette'
import { defaultLightTokens } from '@/lib/theme/tokens'
import type { ThemeTokens } from '@/lib/theme/tokens'
import { sanitizeCSS } from '@/lib/theme/sanitizer'
import { exportThemeJSON, importThemeJSON } from '@/lib/theme/storage'
import { AlertDialogFooter as _AF } from '@/components/ui/alert-dialog'

import { hslToHex } from './types'

type ThemeMode = 'light' | 'dark' | 'system'

const themeModeOptions: Array<{
  value: ThemeMode
  labelKey: string
  descriptionKey: string
  icon: typeof Monitor
}> = [
  {
    value: 'light',
    labelKey: 'settings.appearance.lightMode',
    descriptionKey: 'settings.appearance.lightModeDesc',
    icon: Sun,
  },
  {
    value: 'dark',
    labelKey: 'settings.appearance.darkMode',
    descriptionKey: 'settings.appearance.darkModeDesc',
    icon: Moon,
  },
  {
    value: 'system',
    labelKey: 'settings.appearance.systemMode',
    descriptionKey: 'settings.appearance.systemModeDesc',
    icon: Monitor,
  },
]

function getTokenValue<T>(
  overrides: Partial<ThemeTokens> | undefined,
  section: keyof ThemeTokens,
  key: string,
  fallback: T
): T {
  const sectionOverrides = overrides?.[section] as Record<string, unknown> | undefined
  const value = sectionOverrides?.[key]
  return (value as T) ?? fallback
}

function buildFontSizeTokens(basePx: number): Partial<ThemeTokens['typography']> {
  const scale: Record<number, Partial<Record<string, string>>> = {
    12: { 'font-size-xs': '0.625rem', 'font-size-sm': '0.75rem', 'font-size-base': '0.75rem', 'font-size-lg': '0.875rem', 'font-size-xl': '1rem', 'font-size-2xl': '1.25rem' },
    13: { 'font-size-xs': '0.6875rem', 'font-size-sm': '0.8125rem', 'font-size-base': '0.8125rem', 'font-size-lg': '0.9375rem', 'font-size-xl': '1.0625rem', 'font-size-2xl': '1.3125rem' },
    14: { 'font-size-xs': '0.75rem', 'font-size-sm': '0.875rem', 'font-size-base': '0.875rem', 'font-size-lg': '1rem', 'font-size-xl': '1.125rem', 'font-size-2xl': '1.375rem' },
    15: { 'font-size-xs': '0.75rem', 'font-size-sm': '0.875rem', 'font-size-base': '0.9375rem', 'font-size-lg': '1.0625rem', 'font-size-xl': '1.1875rem', 'font-size-2xl': '1.4375rem' },
    16: {},
    17: { 'font-size-xs': '0.8125rem', 'font-size-sm': '0.9375rem', 'font-size-base': '1.0625rem', 'font-size-lg': '1.1875rem', 'font-size-xl': '1.3125rem', 'font-size-2xl': '1.5625rem' },
    18: { 'font-size-xs': '0.875rem', 'font-size-sm': '1rem', 'font-size-base': '1.125rem', 'font-size-lg': '1.25rem', 'font-size-xl': '1.375rem', 'font-size-2xl': '1.625rem' },
    19: { 'font-size-xs': '0.9375rem', 'font-size-sm': '1.0625rem', 'font-size-base': '1.1875rem', 'font-size-lg': '1.3125rem', 'font-size-xl': '1.4375rem', 'font-size-2xl': '1.6875rem' },
    20: { 'font-size-xs': '1rem', 'font-size-sm': '1.125rem', 'font-size-base': '1.25rem', 'font-size-lg': '1.375rem', 'font-size-xl': '1.5rem', 'font-size-2xl': '1.75rem' },
  }
  return (scale[basePx] ?? {}) as Partial<ThemeTokens['typography']>
}

export function AppearanceTab() {
  const { theme, setTheme, themeConfig, updateThemeConfig, resolvedTheme, resetTheme } = useTheme()
  const { toast } = useToast()
  const { t } = useTranslation()
  const activeCustomCSS = themeConfig.styleCustomCSS?.modern ?? ''

  const [localCSS, setLocalCSS] = useState(activeCustomCSS)
  const [accentInputValue, setAccentInputValue] = useState(() => {
    if (themeConfig.accentColor) {
      return hslToHex(themeConfig.accentColor)
    }

    return DEFAULT_ACCENT_COLOR_HEX
  })
  const [accentPreviewHex, setAccentPreviewHex] = useState(() => {
    if (themeConfig.accentColor) {
      return hslToHex(themeConfig.accentColor)
    }

    return DEFAULT_ACCENT_COLOR_HEX
  })
  const [cssWarnings, setCssWarnings] = useState<string[]>([])
  const accentDebounceRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const cssDebounceRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)

  const activeTokenOverrides = useMemo(
    () => themeConfig.styleTokenOverrides?.modern ?? {},
    [themeConfig.styleTokenOverrides]
  )

  const isValidHexColor = useCallback((value: string) => /^#[0-9A-F]{6}$/i.test(value), [])

  const persistAccentColor = useCallback(
    (hex: string) => {
      if (accentDebounceRef.current) clearTimeout(accentDebounceRef.current)

      accentDebounceRef.current = setTimeout(() => {
        updateThemeConfig({ accentColor: hexToHSL(hex) })
      }, 160)
    },
    [updateThemeConfig]
  )

  const updateTokenSection = useCallback(
    <K extends keyof ThemeTokens>(section: K, partial: Partial<ThemeTokens[K]>) => {
      const nextStyleOverrides = {
        ...activeTokenOverrides,
        [section]: {
          ...activeTokenOverrides[section],
          ...partial,
        } as ThemeTokens[K],
      }

      updateThemeConfig({
        styleTokenOverrides: {
          ...themeConfig.styleTokenOverrides,
          modern: nextStyleOverrides,
        },
      })
    },
    [
      activeTokenOverrides,
      themeConfig.styleTokenOverrides,
      updateThemeConfig,
    ]
  )

  const resetTokenSection = useCallback(
    (section: keyof ThemeTokens) => {
      const newOverrides: Partial<ThemeTokens> = { ...activeTokenOverrides }
      delete newOverrides[section]
      updateThemeConfig({
        styleTokenOverrides: {
          ...themeConfig.styleTokenOverrides,
          modern: newOverrides,
        },
      })
    },
    [
      activeTokenOverrides,
      themeConfig.styleTokenOverrides,
      updateThemeConfig,
    ]
  )

  const handleCSSChange = useCallback(
    (val: string) => {
      setLocalCSS(val)
      const result = sanitizeCSS(val)
      setCssWarnings(result.warnings)

      if (cssDebounceRef.current) clearTimeout(cssDebounceRef.current)
      cssDebounceRef.current = setTimeout(() => {
        updateThemeConfig({
          styleCustomCSS: {
            ...themeConfig.styleCustomCSS,
            modern: val,
          },
        })
      }, 500)
    },
    [themeConfig.styleCustomCSS, updateThemeConfig]
  )

  const previewAccentHSL = useMemo(() => {
    if (isValidHexColor(accentPreviewHex)) {
      return hexToHSL(accentPreviewHex)
    }

    return themeConfig.accentColor || DEFAULT_ACCENT_COLOR_HSL
  }, [accentPreviewHex, isValidHexColor, themeConfig.accentColor])

  const previewThemeConfig = useMemo(() => {
    return {
      ...themeConfig,
      accentColor: previewAccentHSL,
    }
  }, [previewAccentHSL, themeConfig])

  useEffect(() => {
    const persistedHex = themeConfig.accentColor
      ? hslToHex(themeConfig.accentColor)
      : DEFAULT_ACCENT_COLOR_HEX

    setAccentInputValue(persistedHex)
    setAccentPreviewHex(persistedHex)
  }, [themeConfig.accentColor])

  useEffect(() => {
    setLocalCSS(activeCustomCSS)
    setCssWarnings(sanitizeCSS(activeCustomCSS).warnings)
  }, [activeCustomCSS])

  useEffect(() => {
    applyThemePipeline(previewThemeConfig, resolvedTheme === 'dark')
  }, [previewThemeConfig, resolvedTheme])

  useEffect(() => {
    return () => {
      if (accentDebounceRef.current) clearTimeout(accentDebounceRef.current)
      if (cssDebounceRef.current) clearTimeout(cssDebounceRef.current)
    }
  }, [])

  const handleAccentColorChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const hex = e.target.value
    setAccentInputValue(hex)
    setAccentPreviewHex(hex)
    persistAccentColor(hex)
  }

  const handleAccentTextChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const value = e.target.value.toUpperCase()
    setAccentInputValue(value)

    if (!isValidHexColor(value)) {
      return
    }

    setAccentPreviewHex(value)
    persistAccentColor(value)
  }

  const handleResetAccent = () => {
    if (accentDebounceRef.current) clearTimeout(accentDebounceRef.current)

    setAccentInputValue(DEFAULT_ACCENT_COLOR_HEX)
    setAccentPreviewHex(DEFAULT_ACCENT_COLOR_HEX)
    updateThemeConfig({ accentColor: DEFAULT_ACCENT_COLOR_HSL })
  }

  const handleExport = () => {
    const json = exportThemeJSON()
    const blob = new Blob([json], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `omb-theme-${Date.now()}.json`
    a.click()
    URL.revokeObjectURL(url)
  }

  const handleImport = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    const reader = new FileReader()
    reader.onload = (ev) => {
      const json = ev.target?.result as string
      const result = importThemeJSON(json)
      if (result.success) {
        // 导入成功后需要刷新页面使配置生效（因为 ThemeProvider 需要重新读取 localStorage）
        toast({
          title: t('settings.appearance.importSuccess'),
          description: t('settings.appearance.importSuccessDesc'),
        })
        setTimeout(() => window.location.reload(), 1000)
      } else {
        toast({
          title: t('settings.appearance.importFailed'),
          description: result.errors.join('; '),
          variant: 'destructive',
        })
      }
    }
    reader.readAsText(file)
    // 重置 input，允许重复选择同一文件
    e.target.value = ''
  }

  const handleResetTheme = () => {
    resetTheme()
    setLocalCSS('')
    setCssWarnings([])
    toast({
      title: t('settings.appearance.resetSuccess'),
      description: t('settings.appearance.resetSuccessDesc'),
    })
  }

  const computedTokens = useMemo(() => {
    return getComputedTokens(previewThemeConfig, resolvedTheme === 'dark')
  }, [previewThemeConfig, resolvedTheme])
  const baseFontSizePx =
    parseFloat(
      getTokenValue(
        activeTokenOverrides,
        'typography',
        'font-size-base',
        computedTokens.typography['font-size-base']
      )
    ) * 16

  const previewTokens = computedTokens.color

  return (
    <div className="space-y-6 sm:space-y-8">
      {/* 主题模式 */}
      <div>
        <h3 className="mb-3 text-base font-semibold sm:mb-4 sm:text-lg">
          {t('settings.appearance.themeMode')}
        </h3>
        <div
          role="tablist"
          aria-label={t('settings.appearance.themeMode')}
          className="bg-muted/60 text-muted-foreground grid w-full max-w-2xl grid-cols-1 gap-1 border p-1 sm:grid-cols-3"
        >
          {themeModeOptions.map((option) => {
            const selected = theme === option.value
            const Icon = option.icon

            return (
              <button
                key={option.value}
                type="button"
                role="tab"
                aria-selected={selected}
                onClick={() => setTheme(option.value)}
                className={cn(
                  'group flex min-h-16 flex-col justify-center px-3 py-2 text-left transition-all',
                  'focus-visible:ring-ring focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none',
                  selected
                    ? 'bg-background text-foreground shadow-sm'
                    : 'hover:bg-background/60 hover:text-foreground'
                )}
              >
                <span className="flex items-center gap-2 text-sm font-semibold">
                  <Icon
                    className={cn(
                      'h-4 w-4 transition-colors',
                      selected
                        ? 'text-primary'
                        : 'text-muted-foreground group-hover:text-foreground'
                    )}
                    strokeWidth={2}
                  />
                  <span>{t(option.labelKey)}</span>
                </span>
                <span className="text-muted-foreground mt-1 block max-w-full truncate text-xs">
                  {t(option.descriptionKey)}
                </span>
              </button>
            )
          })}
        </div>
      </div>

      {/* 主题色配置 */}
      <div>
        <div className="mb-3 flex items-center justify-between sm:mb-4">
          <h3 className="text-base font-semibold sm:text-lg">
            {t('settings.appearance.accentColor')}
          </h3>
          <Button
            variant="outline"
            size="sm"
            onClick={handleResetAccent}
            disabled={themeConfig.accentColor === DEFAULT_ACCENT_COLOR_HSL}
            className="h-8"
          >
            <RotateCcw className="mr-2 h-3.5 w-3.5" />
            {t('settings.appearance.resetDefault')}
          </Button>
        </div>

        <div className="space-y-6">
          {/* 颜色选择器 */}
          <div className="bg-card flex flex-col items-start gap-4 border p-4 sm:flex-row sm:items-center">
            <div className="flex items-center gap-3">
              <div className="border-border relative h-10 w-10 overflow-hidden rounded-full border-2 shadow-sm">
                <input
                  type="color"
                  value={accentPreviewHex}
                  onChange={handleAccentColorChange}
                  className="absolute inset-0 -top-1/4 -left-1/4 h-[150%] w-[150%] cursor-pointer border-0 p-0"
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="accent-color-input" className="font-medium">
                  {t('settings.appearance.accentPrimary')}
                </Label>
                <p className="text-muted-foreground text-xs">
                  {t('settings.appearance.accentHint')}
                </p>
              </div>
            </div>

            <div className="flex w-full flex-1 items-center gap-2 sm:w-auto">
              <Input
                id="accent-color-input"
                type="text"
                value={accentInputValue}
                onChange={handleAccentTextChange}
                className="w-32 font-mono uppercase"
                maxLength={7}
              />
            </div>
          </div>

          {/* 实时色板预览 */}
          <div className="space-y-3">
            <h4 className="text-muted-foreground text-sm font-medium">
              {t('settings.appearance.colorPreview')}
            </h4>
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-4 md:grid-cols-8">
              <ColorTokenPreview
                name="primary"
                value={previewTokens.primary}
                foreground={previewTokens['primary-foreground']}
              />
              <ColorTokenPreview
                name="secondary"
                value={previewTokens.secondary}
                foreground={previewTokens['secondary-foreground']}
              />
              <ColorTokenPreview
                name="muted"
                value={previewTokens.muted}
                foreground={previewTokens['muted-foreground']}
              />
              <ColorTokenPreview
                name="accent"
                value={previewTokens.accent}
                foreground={previewTokens['accent-foreground']}
              />
              <ColorTokenPreview
                name="destructive"
                value={previewTokens.destructive}
                foreground={previewTokens['destructive-foreground']}
              />
              <ColorTokenPreview
                name="background"
                value={previewTokens.background}
                foreground={previewTokens.foreground}
                border
              />
              <ColorTokenPreview
                name="card"
                value={previewTokens.card}
                foreground={previewTokens['card-foreground']}
                border
              />
              <ColorTokenPreview name="border" value={previewTokens.border} />
            </div>
          </div>
        </div>
      </div>

      {/* 样式微调 */}
      <div>
        <h3 className="mb-3 text-base font-semibold sm:mb-4 sm:text-lg">
          {t('settings.appearance.styleTweaks')}
        </h3>

        <Accordion type="single" collapsible className="w-full">
          {/* 1. 字体排版 (Typography) */}
          <AccordionItem value="typography">
            <AccordionTrigger>{t('settings.appearance.typographyGroup')}</AccordionTrigger>
            <AccordionContent>
              <div className="space-y-4 pt-2">
                <div className="flex justify-end">
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => resetTokenSection('typography')}
                    disabled={!activeTokenOverrides?.typography}
                    className="h-8 text-xs"
                  >
                    <RotateCcw className="mr-2 h-3.5 w-3.5" />
                    {t('settings.appearance.resetDefault')}
                  </Button>
                </div>

                <div className="space-y-2">
                  <Label>{t('settings.appearance.fontFamilyLabel')}</Label>
                  <Select
                    value={(() => {
                      const fontFamily = getTokenValue(
                        activeTokenOverrides,
                        'typography',
                        'font-family-base',
                        computedTokens.typography['font-family-base']
                      )
                      if (fontFamily.includes('ui-serif')) return 'serif'
                      if (fontFamily.includes('ui-monospace')) return 'mono'
                      if (fontFamily.includes('ui-sans-serif')) return 'sans'
                      return 'system'
                    })()}
                    onValueChange={(val) => {
                      let fontVal = defaultLightTokens.typography['font-family-base']
                      if (val === 'serif')
                        fontVal = 'ui-serif, Georgia, Cambria, "Times New Roman", Times, serif'
                      else if (val === 'mono')
                        fontVal =
                          'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace'
                      else if (val === 'sans')
                        fontVal =
                          'ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif'

                      updateTokenSection('typography', {
                        'font-family-base': fontVal,
                      })
                    }}
                  >
                    <SelectTrigger>
                      <SelectValue placeholder={t('settings.appearance.fontFamilyPlaceholder')} />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="system">
                        {t('settings.appearance.fontFamilySystem')}
                      </SelectItem>
                      <SelectItem value="sans">{t('settings.appearance.fontFamilySans')}</SelectItem>
                      <SelectItem value="serif">
                        {t('settings.appearance.fontFamilySerif')}
                      </SelectItem>
                      <SelectItem value="mono">{t('settings.appearance.fontFamilyMono')}</SelectItem>
                    </SelectContent>
                  </Select>
                </div>

                <div className="space-y-4">
                  <div className="flex justify-between">
                    <Label>{t('settings.appearance.baseFontSize')}</Label>
                    <span className="text-muted-foreground text-sm">{baseFontSizePx}px</span>
                  </div>
                  <Slider
                    defaultValue={[16]}
                    value={[baseFontSizePx]}
                    min={12}
                    max={20}
                    step={1}
                    onValueChange={(vals) => {
                      updateTokenSection('typography', {
                        ...buildFontSizeTokens(vals[0]),
                      })
                    }}
                  />
                </div>

                <div className="space-y-2">
                  <Label>{t('settings.appearance.lineHeight')}</Label>
                  <Select
                    value={String(
                      getTokenValue(
                        activeTokenOverrides,
                        'typography',
                        'line-height-normal',
                        computedTokens.typography['line-height-normal']
                      )
                    )}
                    onValueChange={(val) => {
                      updateTokenSection('typography', {
                        'line-height-normal': parseFloat(val),
                      })
                    }}
                  >
                    <SelectTrigger>
                      <SelectValue placeholder={t('settings.appearance.lineHeightPlaceholder')} />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="1.2">
                        {t('settings.appearance.lineHeightCompact')}
                      </SelectItem>
                      <SelectItem value="1.5">
                        {t('settings.appearance.lineHeightNormal')}
                      </SelectItem>
                      <SelectItem value="1.75">
                        {t('settings.appearance.lineHeightLoose')}
                      </SelectItem>
                    </SelectContent>
                  </Select>
                </div>
              </div>
            </AccordionContent>
          </AccordionItem>

          {/* 2. 视觉效果 (Visual) */}
          <AccordionItem value="visual">
            <AccordionTrigger>{t('settings.appearance.visualGroup')}</AccordionTrigger>
            <AccordionContent>
              <div className="space-y-4 pt-2">
                <div className="flex justify-end">
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => resetTokenSection('visual')}
                    disabled={!activeTokenOverrides?.visual}
                    className="h-8 text-xs"
                  >
                    <RotateCcw className="mr-2 h-3.5 w-3.5" />
                    {t('settings.appearance.resetDefault')}
                  </Button>
                </div>

                <div className="space-y-4">
                  <div className="flex justify-between">
                    <Label>{t('settings.appearance.borderRadiusLabel')}</Label>
                    <span className="text-muted-foreground text-sm">
                      {Math.round(
                        parseFloat(
                          getTokenValue(
                            activeTokenOverrides,
                            'visual',
                            'radius-md',
                            computedTokens.visual['radius-md']
                          )
                        ) * 16
                      )}
                      px
                    </span>
                  </div>
                  <Slider
                    defaultValue={[6]}
                    value={[
                      Math.round(
                        parseFloat(
                          getTokenValue(
                            activeTokenOverrides,
                            'visual',
                            'radius-md',
                            computedTokens.visual['radius-md']
                          )
                        ) * 16
                      ),
                    ]}
                    min={0}
                    max={24}
                    step={1}
                    onValueChange={(vals) => {
                      updateTokenSection('visual', {
                        'radius-md': `${vals[0] / 16}rem`,
                      })
                    }}
                  />
                </div>

                <div className="space-y-2">
                  <Label>{t('settings.appearance.shadowLabel')}</Label>
                  <Select
                    value={(() => {
                      const shadowMd = String(
                        getTokenValue(
                          activeTokenOverrides,
                          'visual',
                          'shadow-md',
                          computedTokens.visual['shadow-md']
                        )
                      )
                      if (shadowMd === 'none') return 'none'
                      if (shadowMd === defaultLightTokens.visual['shadow-sm']) return 'sm'
                      if (shadowMd === defaultLightTokens.visual['shadow-lg']) return 'lg'
                      if (shadowMd === defaultLightTokens.visual['shadow-xl']) return 'xl'
                      return 'md'
                    })()}
                    onValueChange={(val) => {
                      let shadowVal = defaultLightTokens.visual['shadow-md']
                      if (val === 'none') shadowVal = 'none'
                      else if (val === 'sm') shadowVal = defaultLightTokens.visual['shadow-sm']
                      else if (val === 'lg') shadowVal = defaultLightTokens.visual['shadow-lg']
                      else if (val === 'xl') shadowVal = defaultLightTokens.visual['shadow-xl']

                      updateTokenSection('visual', {
                        'shadow-md': shadowVal,
                      })
                    }}
                  >
                    <SelectTrigger>
                      <SelectValue placeholder={t('settings.appearance.shadowPlaceholder')} />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="none">{t('settings.appearance.shadowNone')}</SelectItem>
                      <SelectItem value="sm">{t('settings.appearance.shadowSm')}</SelectItem>
                      <SelectItem value="md">{t('settings.appearance.shadowMd')}</SelectItem>
                      <SelectItem value="lg">{t('settings.appearance.shadowLg')}</SelectItem>
                      <SelectItem value="xl">{t('settings.appearance.shadowXl')}</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
              </div>
            </AccordionContent>
          </AccordionItem>
        </Accordion>
      </div>

      {/* 自定义 CSS */}
      <div>
        <div className="mb-3 flex items-center justify-between sm:mb-4">
          <div>
            <h3 className="text-base font-semibold sm:text-lg">
              {t('settings.appearance.customCss')}
            </h3>
            <p className="text-muted-foreground mt-1 text-sm">
              {t('settings.appearance.cssDescription')}
            </p>
          </div>
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              setLocalCSS('')
              updateThemeConfig({
                styleCustomCSS: {
                  ...themeConfig.styleCustomCSS,
                  modern: '',
                },
              })
              setCssWarnings([])
            }}
            disabled={!activeCustomCSS}
          >
            <Trash2 className="mr-1 h-4 w-4" />
            {t('settings.appearance.clearCss')}
          </Button>
        </div>

        <CodeEditorSection
          value={localCSS}
          onChange={handleCSSChange}
          warnings={cssWarnings}
          placeholder={t('settings.appearance.cssPlaceholder')}
        />
      </div>

      {/* 动效设置 */}
      <AnimationSection />

      {/* 主题导入/导出 */}
      <div>
        <h3 className="mb-3 text-base font-semibold sm:mb-4 sm:text-lg">
          {t('settings.appearance.importExportTheme')}
        </h3>
        <div className="bg-card space-y-3 border p-3 sm:p-4">
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
            {/* 导出按钮 */}
            <Button onClick={handleExport} variant="outline" className="gap-2">
              <Download className="h-4 w-4" />
              {t('settings.appearance.exportTheme')}
            </Button>

            {/* 导入按钮 */}
            <Button onClick={() => fileInputRef.current?.click()} variant="outline" className="gap-2">
              <Upload className="h-4 w-4" />
              {t('settings.appearance.importTheme')}
            </Button>

            {/* 重置按钮 */}
            <AlertDialog>
              <AlertDialogTrigger asChild>
                <Button variant="outline" className="gap-2">
                  <RotateCcw className="h-4 w-4" />
                  {t('settings.appearance.resetTheme')}
                </Button>
              </AlertDialogTrigger>
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>{t('settings.appearance.confirmResetTheme')}</AlertDialogTitle>
                  <AlertDialogDescription>
                    {t('settings.appearance.confirmResetThemeDesc')}
                  </AlertDialogDescription>
                </AlertDialogHeader>
                <AlertDialogFooter>
                  <AlertDialogCancel>{t('common.cancel')}</AlertDialogCancel>
                  <AlertDialogAction onClick={handleResetTheme}>
                    {t('settings.appearance.confirmResetAction')}
                  </AlertDialogAction>
                </AlertDialogFooter>
              </AlertDialogContent>
            </AlertDialog>
          </div>

          {/* 隐藏的文件输入 */}
          <input ref={fileInputRef} type="file" accept=".json" onChange={handleImport} className="hidden" />

          <p className="text-muted-foreground text-xs">{t('settings.appearance.exportDesc')}</p>
        </div>
      </div>
    </div>
  )
}

function CodeEditorSection({
  value,
  onChange,
  warnings,
  placeholder,
}: {
  value: string
  onChange: (value: string) => void
  warnings: string[]
  placeholder: string
}) {
  const [CodeEditor, setCodeEditor] = useState<
    React.ComponentType<{ value: string; language: string; height: string; placeholder?: string; onChange: (value: string) => void }> | null
  >(null)

  useEffect(() => {
    let cancelled = false
    void import('@/components/CodeEditor').then((module) => {
      if (!cancelled) setCodeEditor(() => module.CodeEditor as React.ComponentType<{ value: string; language: string; height: string; placeholder?: string; onChange: (value: string) => void }>)
    })
    return () => {
      cancelled = true
    }
  }, [])

  return (
    <div className="bg-card space-y-3 border p-3 sm:p-4">
      {CodeEditor ? (
        <CodeEditor value={value} language="css" height="250px" placeholder={placeholder} onChange={onChange} />
      ) : (
        <textarea
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={placeholder}
          className="text-foreground h-[250px] w-full resize-y border bg-transparent p-2 font-mono text-sm outline-none"
        />
      )}

      {warnings.length > 0 && (
        <div className="border-yellow-200 bg-yellow-50 dark:border-yellow-800 dark:bg-yellow-950/30 border p-3">
          <div className="mb-1 flex items-center gap-2 text-sm font-medium text-yellow-800 dark:text-yellow-200">
            <AlertTriangle className="h-4 w-4" />
            CSS 警告
          </div>
          <ul className="ml-6 list-disc space-y-0.5 text-xs text-yellow-700 dark:text-yellow-300">
            {warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}

function AnimationSection() {
  const { t } = useTranslation()
  const [enableAnimations, setEnableAnimationsState] = useState(() => {
    try {
      return localStorage.getItem('omb-animations-disabled') !== 'true'
    } catch {
      return true
    }
  })

  const setEnableAnimations = (checked: boolean) => {
    setEnableAnimationsState(checked)
    try {
      localStorage.setItem('omb-animations-disabled', checked ? 'false' : 'true')
      document.documentElement.classList.toggle('no-animations', !checked)
    } catch {
      // localStorage 不可用时忽略
    }
  }

  useEffect(() => {
    try {
      const disabled = localStorage.getItem('omb-animations-disabled') === 'true'
      document.documentElement.classList.toggle('no-animations', disabled)
    } catch {
      // ignore
    }
  }, [])

  return (
    <div>
      <h3 className="mb-3 text-base font-semibold sm:mb-4 sm:text-lg">
        {t('settings.appearance.animationEffect')}
      </h3>
      <div className="bg-card border p-3 sm:p-4">
        <div className="flex items-center justify-between">
          <div className="flex-1 space-y-0.5">
            <Label htmlFor="animations" className="cursor-pointer text-base font-medium">
              {t('settings.appearance.enableAnimations')}
            </Label>
            <p className="text-muted-foreground text-sm">
              {t('settings.appearance.enableAnimationsDesc')}
            </p>
          </div>
          <Switch id="animations" checked={enableAnimations} onCheckedChange={setEnableAnimations} />
        </div>
      </div>
    </div>
  )
}

function ColorTokenPreview({
  name,
  value,
  foreground,
  border,
}: {
  name: string
  value: string
  foreground?: string
  border?: boolean
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <div
        className={cn(
          'flex h-16 items-center justify-center text-xs font-medium shadow-sm',
          border && 'border-border border'
        )}
        style={{
          backgroundColor: `hsl(${value})`,
          color: foreground ? `hsl(${foreground})` : undefined,
        }}
      >
        Aa
      </div>
      <div className="text-muted-foreground truncate text-center text-xs" title={name}>
        {name}
      </div>
    </div>
  )
}
