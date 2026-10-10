/**
 * 路由清单 — 页面与 Go 后端路由一一对应。
 * 受保护路由经 rootRoute.beforeLoad 统一鉴权（本地 token → /auth/check）。
 */

import { createRootRoute, createRoute, createRouter, Outlet, redirect } from '@tanstack/react-router'

import { checkAuth } from '@/api/auth'
import { getStoredToken } from '@/lib/token'
import { Layout } from '@/components/Layout'
import { NotFoundPage } from '@/pages/NotFound'
import { RouteErrorBoundary } from '@/components/ErrorBoundary'
import { RoutePendingFallback } from '@/components/RoutePendingFallback'

const rootRoute = createRootRoute({
  component: Outlet,
  beforeLoad: ({ location }) => {
    const publicPaths = ['/auth', '/setup', '/chat/embed']
    if (publicPaths.some((p) => location.pathname === p || location.pathname.startsWith(`${p}/`))) {
      return
    }
    return checkAuth().then((authenticated) => {
      if (!authenticated) {
        throw redirect({
          to: '/auth',
          search: location.pathname !== '/' ? { redirect: location.pathname } : undefined,
        })
      }
    })
  },
})

// ── 公开路由 ──

const authRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/auth',
  component: lazyPage(() => import('@/pages/Auth'), 'AuthPage'),
})

const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/setup',
  component: lazyPage(() => import('@/pages/Setup'), 'SetupPage'),
})

const chatEmbedRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/chat/embed',
  component: lazyPage(() => import('@/pages/ChatEmbed'), 'ChatEmbedPage'),
})

// ── 受保护路由（带 Layout） ──

const protectedRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: 'protected',
  component: () => (
    <Layout>
      <Outlet />
    </Layout>
  ),
  errorComponent: ({ error }) => <RouteErrorBoundary error={error} />,
})

const indexRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/',
  component: lazyPage(() => import('@/pages/Home'), 'HomePage'),
})

const chatRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/chat',
  component: lazyPage(() => import('@/pages/Chat'), 'ChatPage'),
})

const botConfigRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/config/bot',
  component: lazyPage(() => import('@/pages/BotConfig'), 'BotConfigPage'),
})

const modelConfigRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/config/model',
  component: lazyPage(() => import('@/pages/ModelConfig'), 'ModelConfigPage'),
})

const emojiRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/resource/emoji',
  component: lazyPage(() => import('@/pages/Emoji'), 'EmojiPage'),
})

const expressionRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/resource/expression',
  component: lazyPage(() => import('@/pages/Expression'), 'ExpressionPage'),
})

const jargonRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/resource/jargon',
  component: lazyPage(() => import('@/pages/Jargon'), 'JargonPage'),
})

const personRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/resource/person',
  component: lazyPage(() => import('@/pages/Person'), 'PersonPage'),
})

const memoryRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/resource/memory',
  component: lazyPage(() => import('@/pages/Memory'), 'MemoryPage'),
})

const monitorRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/monitor',
  component: lazyPage(() => import('@/pages/Monitor'), 'MonitorPage'),
})

const reasoningRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/reasoning-process',
  component: lazyPage(() => import('@/pages/ReasoningProcess'), 'ReasoningProcessPage'),
})

const logsRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/logs',
  component: lazyPage(() => import('@/pages/Logs'), 'LogsPage'),
})

const statisticsRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/statistics',
  component: lazyPage(() => import('@/pages/Statistics'), 'StatisticsPage'),
})

const dataTransferRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/data-transfer',
  component: lazyPage(() => import('@/pages/DataTransfer'), 'DataTransferPage'),
})

const settingsRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/settings',
  component: lazyPage(() => import('@/pages/Settings'), 'SettingsPage'),
})

const notFoundRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '*',
  component: NotFoundPage,
})

function lazyPage(loader: () => Promise<{ [key: string]: unknown }>, exportName: string) {
  // TanStack lazyRouteComponent 需要 default 导出；这里包一层显式映射
  return createLazyComponent(loader, exportName)
}

import { lazyRouteComponent } from '@tanstack/react-router'

function createLazyComponent(
  loader: () => Promise<{ [key: string]: unknown }>,
  exportName: string
) {
  return lazyRouteComponent(async () => {
    const module = await loader()
    return { default: module[exportName] as React.ComponentType }
  })
}

const routeTree = rootRoute.addChildren([
  authRoute,
  setupRoute,
  chatEmbedRoute,
  protectedRoute.addChildren([
    indexRoute,
    chatRoute,
    botConfigRoute,
    modelConfigRoute,
    emojiRoute,
    expressionRoute,
    jargonRoute,
    personRoute,
    memoryRoute,
    monitorRoute,
    reasoningRoute,
    logsRoute,
    statisticsRoute,
    dataTransferRoute,
    settingsRoute,
  ]),
  notFoundRoute,
])

export const router = createRouter({
  routeTree,
  defaultNotFoundComponent: NotFoundPage,
  defaultErrorComponent: ({ error }) => <RouteErrorBoundary error={error} />,
  defaultPendingComponent: RoutePendingFallback,
  defaultPendingMs: 120,
  defaultPendingMinMs: 120,
  defaultPreload: 'intent',
  defaultPreloadDelay: 80,
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

// 供守卫/登录页复用：登录成功后的落地页
export function postAuthTarget(): string {
  if (typeof window === 'undefined') return '/'
  const redirect = new URLSearchParams(window.location.search).get('redirect')
  // 仅允许站内路径，防开放重定向
  if (redirect && redirect.startsWith('/') && !redirect.startsWith('//')) return redirect
  return '/'
}

// 未登录访问受保护路由时，beforeLoad 依赖 checkAuth；
// checkAuth 失败（后端不可达）时按未登录处理，由登录页给出明确错误。
export function hasLocalToken(): boolean {
  return getStoredToken() !== ''
}
