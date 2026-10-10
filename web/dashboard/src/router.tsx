import {
  createRootRoute,
  createRoute,
  createRouter,
  lazyRouteComponent,
  Outlet,
  redirect,
} from '@tanstack/react-router'
import { NotFoundPage } from './routes/404'
import { Layout } from './components/layout'
import { RoutePendingFallback } from './components/route-pending-fallback'
import { checkAuth } from './hooks/use-auth'
import { RouteErrorBoundary } from './components/error-boundary'

// Root 路由：所有非公开路由（protected 分支）在进入前统一鉴权；
// /auth 与 /setup 自行处理未认证态，不做根层跳转。
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

// 认证路由（无 Layout）
const authRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/auth',
  component: lazyRouteComponent(() => import('./routes/auth'), 'AuthPage'),
})

// 首次配置路由（无 Layout）
const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/setup',
  component: lazyRouteComponent(() => import('./routes/setup/index.tsx'), 'SetupPage'),
})

// 受保护的路由 Root（带 Layout）
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

// 首页路由
const indexRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/',
  component: lazyRouteComponent(() => import('./routes/index'), 'IndexPage'),
})

// 详细统计路由
const statisticsRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/statistics',
  component: lazyRouteComponent(() => import('./routes/logs'), 'StatisticsLogViewerPage'),
})

const replyEffectsRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/reply-effects',
  component: lazyRouteComponent(() => import('./routes/reply-effects'), 'ReplyEffectsPage'),
})

// 配置路由 - 机器人主程序配置
const botConfigRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/config/bot',
  beforeLoad: ({ location }) => {
    const params = new URLSearchParams(location.searchStr)
    if (params.get('mode') !== 'webui') return
    params.delete('mode')
    const search = params.size ? `?${params.toString()}` : ''
    // 兼容此前内嵌 WebUI 设置的链接，保留页签参数和 hash。
    const hash = location.hash ? `#${location.hash.replace(/^#/, '')}` : ''
    throw redirect({ href: `/settings${search}${hash}`, replace: true })
  },
  component: lazyRouteComponent(() => import('./routes/config/bot'), 'BotConfigPage'),
})

// 配置路由 - 机器人模型配置
const modelConfigRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/config/model',
  component: lazyRouteComponent(() => import('./routes/config/model'), 'ModelConfigPage'),
})

// 配置路由 - Prompt 管理
const promptManagementRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/config/prompts',
  component: lazyRouteComponent(() => import('./routes/config/prompts'), 'PromptManagementPage'),
})

// 资源管理路由 - 表情包管理
const emojiManagementRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/resource/emoji',
  component: lazyRouteComponent(
    () => import('./routes/resource/emoji/index.tsx'),
    'EmojiManagementPage'
  ),
})

// 资源管理路由 - 表达方式管理
const expressionManagementRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/resource/expression',
  component: lazyRouteComponent(
    () => import('./routes/resource/expression/index.tsx'),
    'ExpressionManagementPage'
  ),
})

// 资源管理路由 - 人物信息管理
const personManagementRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/resource/person',
  component: lazyRouteComponent(() => import('./routes/person'), 'PersonManagementPage'),
})

// 资源管理路由 - 黑话管理
const jargonManagementRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/resource/jargon',
  component: lazyRouteComponent(
    () => import('./routes/resource/jargon/index.tsx'),
    'JargonManagementPage'
  ),
})

// 资源管理路由 - 知识库图谱可视化
const behaviorLearningRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/resource/behavior',
  component: lazyRouteComponent(
    () => import('./routes/resource/behavior/index.tsx'),
    'BehaviorLearningPage'
  ),
})

const knowledgeGraphRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/resource/knowledge-graph',
  component: lazyRouteComponent(
    () => import('./routes/resource/knowledge-graph/index.tsx'),
    'KnowledgeGraphPage'
  ),
})

// 资源管理路由 - 机器人知识库管理
const knowledgeBaseRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/resource/knowledge-base',
  component: lazyRouteComponent(
    () => import('./routes/resource/knowledge-base'),
    'KnowledgeBasePage'
  ),
})

// 日志查看器路由
const logsRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/logs',
  component: lazyRouteComponent(() => import('./routes/logs'), 'LogViewerPage'),
})

const reasoningProcessRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/reasoning-process',
  component: lazyRouteComponent(() => import('./routes/logs'), 'ReasoningLogViewerPage'),
})

// 运行时聊天流监控路由
const plannerMonitorRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/planner-monitor',
  component: lazyRouteComponent(() => import('./routes/monitor/index.tsx'), 'PlannerMonitorPage'),
})

// 本地聊天室路由
const chatRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/chat',
  component: lazyRouteComponent(() => import('./routes/chat/index'), 'ChatPage'),
})

// 聊天管理路由
const chatManagementRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/chat-management',
  component: lazyRouteComponent(() => import('./routes/chat-management'), 'ChatManagementPage'),
})

// 外部程序嵌入用聊天室路由，不挂载 dashboard 顶栏
const chatEmbedRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/chat/embed',
  component: lazyRouteComponent(() => import('./routes/chat/embed'), 'ChatEmbedPage'),
})

// 数据迁移与备份路由
const dataTransferRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/data-transfer',
  component: lazyRouteComponent(() => import('./routes/data-transfer'), 'DataTransferPage'),
})

const settingsRoute = createRoute({
  getParentRoute: () => protectedRoute,
  path: '/settings',
  component: lazyRouteComponent(() => import('./routes/settings/index.tsx'), 'SettingsPage'),
})

// 问卷调查路由、配置模板市场路由已随 MaiBot 品牌特色系统移除

// 404 路由
const notFoundRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '*',
  component: NotFoundPage,
})

// 路由树
const routeTree = rootRoute.addChildren([
  authRoute,
  setupRoute,
  chatEmbedRoute,
  protectedRoute.addChildren([
    indexRoute,
    statisticsRoute,
    replyEffectsRoute,
    botConfigRoute,
    modelConfigRoute,
    promptManagementRoute,
    emojiManagementRoute,
    expressionManagementRoute,
    jargonManagementRoute,
    behaviorLearningRoute,
    personManagementRoute,
    knowledgeGraphRoute,
    knowledgeBaseRoute,
    dataTransferRoute,
    logsRoute,
    reasoningProcessRoute,
    plannerMonitorRoute,
    chatManagementRoute,
    chatRoute,
    settingsRoute,
  // 问卷调查路由、配置模板市场路由已随 MaiBot 品牌特色系统移除
  ]),
  notFoundRoute,
])

type RouteNode = {
  fullPath?: string
  children?: RouteNode[]
}

function collectRoutePaths(node: RouteNode): string[] {
  const currentPath = node.fullPath ? [node.fullPath] : []
  const childPaths = node.children?.flatMap(collectRoutePaths) ?? []
  return [...currentPath, ...childPaths]
}

export const registeredRoutePaths = new Set(collectRoutePaths(routeTree as RouteNode))

// 创建路由器
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

// 类型声明
declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
