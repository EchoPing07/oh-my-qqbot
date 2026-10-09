/**
 * useQuickShortcuts —— 首页快捷菜单领域 hook（页面逻辑下沉）。
 *
 * 收编 index.tsx 的快捷菜单状态机：
 * - quickShortcutIds（localStorage 持久化）、dialog 开关、搜索关键字
 * - quickShortcutOptions / map / selected / filtered 派生态，toggle / reset 操作
 *
 * 设计判断：
 * - 快捷菜单内置项依赖外部交互态（重启、审核器、未审核数量），故由调用方注入
 *   handleRestart / isRestarting / uncheckedCount / onOpenReviewer，hook 负责编排其余全部逻辑。
 * - 本 fork 无插件系统，插件快捷入口分支已移除。
 */
import { BookOpen, ClipboardCheck, FileText, HardDrive, MessageSquare, RotateCcw, Settings } from 'lucide-react'
import { useQuery } from '@tanstack/react-query'
import { useCallback, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { getBotConfigSchema } from '@/lib/config-api'
import { buildTabGroupsFromSchema } from '@/lib/config-tab-groups'
import type { QuickShortcutDefinition } from '../types'

const QUICK_SHORTCUT_STORAGE_KEY = 'omb-home-quick-shortcuts'
const DEFAULT_QUICK_SHORTCUT_IDS = [
  'route:logs',
  'route:settings-appearance',
  'route:model-list',
  'route:chat',
  'route:logs:replyer',
  'route:logs:reasoning',
]
const SIDEBAR_REDUNDANT_SHORTCUT_IDS = new Set([
  'route:model-providers',
  'route:bot-config',
  'route:emoji',
  'route:expression',
])

function loadQuickShortcutIds(): string[] {
  const fallback = [...DEFAULT_QUICK_SHORTCUT_IDS]
  if (typeof window === 'undefined') {
    return fallback
  }

  const stored = localStorage.getItem(QUICK_SHORTCUT_STORAGE_KEY)
  if (!stored) {
    return fallback
  }

  try {
    const parsed = JSON.parse(stored)
    if (Array.isArray(parsed)) {
      const ids = parsed.filter(
        (item): item is string => typeof item === 'string' && item.length > 0
      )
      const uniqueIds = Array.from(new Set(ids))
      const filteredIds = uniqueIds.filter((id) => !SIDEBAR_REDUNDANT_SHORTCUT_IDS.has(id))
      const normalizedIds = filteredIds.length > 0 ? filteredIds : fallback
      if (
        normalizedIds.length !== uniqueIds.length ||
        normalizedIds.some((id, index) => id !== uniqueIds[index])
      ) {
        saveQuickShortcutIds(normalizedIds)
      }
      return normalizedIds
    }
  } catch {
    return fallback
  }

  return fallback
}

function saveQuickShortcutIds(ids: string[]): void {
  localStorage.setItem(QUICK_SHORTCUT_STORAGE_KEY, JSON.stringify(Array.from(new Set(ids))))
}

interface UseQuickShortcutsParams {
  isRestarting: boolean
  handleRestart: () => void | Promise<void>
  uncheckedCount: number
  onOpenReviewer: () => void
}

export function useQuickShortcuts({
  isRestarting,
  handleRestart,
  uncheckedCount,
  onOpenReviewer,
}: UseQuickShortcutsParams) {
  const { t } = useTranslation()
  const [quickShortcutIds, setQuickShortcutIds] = useState<string[]>(loadQuickShortcutIds)
  const [quickShortcutDialogOpen, setQuickShortcutDialogOpen] = useState(false)
  const [quickShortcutSearch, setQuickShortcutSearch] = useState('')
  const configSchemaQuery = useQuery({
    queryKey: ['bot-config', 'schema'],
    queryFn: getBotConfigSchema,
    staleTime: Infinity,
  })
  const configShortcuts = useMemo<QuickShortcutDefinition[]>(
    () =>
      configSchemaQuery.data
        ? buildTabGroupsFromSchema(configSchemaQuery.data).map((tab) => ({
            id: `route:bot-config:${tab.id}`,
            category: 'config' as const,
            label: tab.label,
            description: t('home.quickActions.descriptions.configSection', { section: tab.label }),
            icon: Settings,
            href: `/config/bot?${new URLSearchParams({ tab: tab.id }).toString()}`,
          }))
        : [],
    [configSchemaQuery.data, t]
  )

  const quickShortcutOptions = useMemo<QuickShortcutDefinition[]>(
    () => [
      {
        id: 'action:restart',
        category: 'system',
        label: isRestarting ? t('home.quickActions.restarting') : t('home.quickActions.restart'),
        description: t('home.quickActions.descriptions.restart'),
        icon: RotateCcw,
        action: handleRestart,
        disabled: isRestarting,
      },
      {
        id: 'action:expression-review',
        category: 'resource',
        label: t('home.quickActions.expressionReview'),
        description: t('home.quickActions.descriptions.expressionReview'),
        icon: ClipboardCheck,
        action: onOpenReviewer,
        badge:
          uncheckedCount > 0 ? (uncheckedCount > 99 ? '99+' : String(uncheckedCount)) : undefined,
      },
      {
        id: 'route:logs',
        category: 'monitor',
        label: t('home.quickActions.viewLogs'),
        description: t('home.quickActions.descriptions.viewLogs'),
        icon: FileText,
        href: '/logs',
      },
      {
        id: 'route:chat',
        category: 'system',
        label: t('home.quickActions.chat'),
        description: t('home.quickActions.descriptions.chat'),
        icon: MessageSquare,
        href: '/chat',
      },
      {
        id: 'external:docs',
        category: 'external',
        label: t('home.quickActions.docs'),
        description: t('home.quickActions.descriptions.docs'),
        icon: BookOpen,
        href: 'https://github.com/user/oh-my-qqbot',
        external: true,
      },
      ...(['replyer', 'planner', 'reasoning'] as const).map((stage) => ({
        id: `route:logs:${stage}`,
        category: 'monitor' as const,
        label: t(`home.quickActions.${stage}Logs`),
        description: t(`home.quickActions.descriptions.${stage}Logs`),
        icon: FileText,
        href: stage === 'reasoning' ? '/reasoning-process' : `/reasoning-process?stage=${stage}`,
      })),
      ...configShortcuts,
      {
        id: 'route:settings-appearance',
        category: 'system',
        label: t('home.quickActions.appearanceSettings'),
        description: t('home.quickActions.descriptions.appearanceSettings'),
        icon: Settings,
        href: '/settings?tab=appearance',
      },
      {
        id: 'route:settings-local-cache',
        category: 'system',
        label: t('home.quickActions.localCache'),
        description: t('home.quickActions.descriptions.localCache'),
        icon: HardDrive,
        href: '/settings?tab=local-cache',
      },
      {
        id: 'route:model-list',
        category: 'config',
        label: t('home.quickActions.modelList'),
        description: t('home.quickActions.descriptions.modelList'),
        icon: Settings,
        href: '/config/model?tab=models',
      },
      {
        id: 'route:model-tasks',
        category: 'config',
        label: t('home.quickActions.modelTasks'),
        description: t('home.quickActions.descriptions.modelTasks'),
        icon: Settings,
        href: '/config/model?tab=tasks',
      },
    ],
    [configShortcuts, handleRestart, isRestarting, onOpenReviewer, t, uncheckedCount]
  )

  const quickShortcutMap = useMemo(
    () => new Map(quickShortcutOptions.map((shortcut) => [shortcut.id, shortcut])),
    [quickShortcutOptions]
  )

  const selectedQuickShortcuts = useMemo(
    () =>
      quickShortcutIds
        .map((id) => quickShortcutMap.get(id))
        .filter((shortcut): shortcut is QuickShortcutDefinition => Boolean(shortcut)),
    [quickShortcutIds, quickShortcutMap]
  )

  const filteredQuickShortcutOptions = useMemo(() => {
    const query = quickShortcutSearch.trim().toLowerCase()
    if (!query) {
      return quickShortcutOptions
    }

    return quickShortcutOptions.filter((shortcut) =>
      `${shortcut.label} ${shortcut.description}`.toLowerCase().includes(query)
    )
  }, [quickShortcutOptions, quickShortcutSearch])

  const updateQuickShortcutIds = useCallback((nextIds: string[]) => {
    const normalizedIds = Array.from(new Set(nextIds))
    setQuickShortcutIds(normalizedIds)
    saveQuickShortcutIds(normalizedIds)
  }, [])

  const toggleQuickShortcut = useCallback(
    (id: string, checked: boolean) => {
      updateQuickShortcutIds(
        checked
          ? [...quickShortcutIds, id]
          : quickShortcutIds.filter((shortcutId) => shortcutId !== id)
      )
    },
    [quickShortcutIds, updateQuickShortcutIds]
  )

  const resetQuickShortcuts = useCallback(() => {
    updateQuickShortcutIds([...DEFAULT_QUICK_SHORTCUT_IDS])
  }, [updateQuickShortcutIds])

  return {
    quickShortcutIds,
    quickShortcutDialogOpen,
    setQuickShortcutDialogOpen,
    quickShortcutSearch,
    setQuickShortcutSearch,
    selectedQuickShortcuts,
    filteredQuickShortcutOptions,
    toggleQuickShortcut,
    resetQuickShortcuts,
  }
}
