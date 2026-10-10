/**
 * 应用外壳：侧边栏 + 顶栏 + 内容区。
 * 菜单即路由清单 — 每一项对应 router.tsx 中的一条受保护路由。
 */

import { useEffect, useState, type ReactNode } from 'react'
import { Link, useLocation } from '@tanstack/react-router'
import {
  Activity,
  Bot,
  Boxes,
  Database,
  FileText,
  FolderSync,
  Gauge,
  Home,
  Images,
  Info,
  Languages,
  LogOut,
  Menu,
  MessageSquare,
  Monitor,
  Moon,
  ScanFace,
  Settings,
  Sun,
  Users,
} from 'lucide-react'

import { cn } from '@/lib/cn'
import { logoutLocal } from '@/lib/token'
import { applyTheme, readStoredTheme, storeTheme, watchSystemTheme, type ThemeMode } from '@/lib/theme'
import { unifiedWs } from '@/lib/ws'

const Icon = {
  home: Home,
  chat: MessageSquare,
  bot: Bot,
  model: Boxes,
  prompt: FileText,
  emoji: Images,
  expression: Languages,
  jargon: Languages,
  person: Users,
  knowledge: Database,
  monitor: Monitor,
  logs: FileText,
  stats: Gauge,
  reasoning: Activity,
  transfer: FolderSync,
  settings: Settings,
  about: Info,
} as const

interface NavItem {
  to: string
  label: string
  icon: keyof typeof Icon
}

interface NavSection {
  title: string
  items: NavItem[]
}

export const NAV_SECTIONS: NavSection[] = [
  {
    title: '概览',
    items: [
      { to: '/', label: '首页', icon: 'home' },
      { to: '/chat', label: '聊天管理', icon: 'chat' },
    ],
  },
  {
    title: '配置',
    items: [
      { to: '/config/bot', label: '机器人设置', icon: 'bot' },
      { to: '/config/model', label: '模型配置', icon: 'model' },
    ],
  },
  {
    title: '资源',
    items: [
      { to: '/resource/emoji', label: '表情包', icon: 'emoji' },
      { to: '/resource/expression', label: '表达方式', icon: 'expression' },
      { to: '/resource/jargon', label: '黑话', icon: 'jargon' },
      { to: '/resource/person', label: '人物', icon: 'person' },
      { to: '/resource/memory', label: '长期记忆', icon: 'knowledge' },
    ],
  },
  {
    title: '运行',
    items: [
      { to: '/monitor', label: '运行监控', icon: 'monitor' },
      { to: '/reasoning-process', label: '推理过程', icon: 'reasoning' },
      { to: '/logs', label: '日志', icon: 'logs' },
      { to: '/statistics', label: '统计', icon: 'stats' },
    ],
  },
  {
    title: '系统',
    items: [
      { to: '/data-transfer', label: '数据传输', icon: 'transfer' },
      { to: '/settings', label: '设置', icon: 'settings' },
      { to: '/about', label: '关于', icon: 'about' },
    ],
  },
]

function LogoMark() {
  return (
    <span className="logo-mark" aria-hidden="true">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round">
        <path d="M5 18V6l7 7 7-7v12" />
      </svg>
    </span>
  )
}

function ThemeButton() {
  const [mode, setMode] = useState<ThemeMode>(() => readStoredTheme())
  useEffect(() => {
    applyTheme(mode)
    storeTheme(mode)
    return watchSystemTheme(mode, () => mode === 'system' && applyTheme('system'))
  }, [mode])
  const cycle = () => setMode(mode === 'light' ? 'dark' : mode === 'dark' ? 'system' : 'light')
  const label = mode === 'light' ? '亮色' : mode === 'dark' ? '暗色' : '跟随系统'
  const IconCmp = mode === 'light' ? Sun : mode === 'dark' ? Moon : ScanFace
  return (
    <button className="btn btn-outline btn-sm" onClick={cycle} title={`主题：${label}（点击切换）`}>
      <IconCmp />
      <span className="hide-sm">{label}</span>
    </button>
  )
}

export function Layout({ children }: { children: ReactNode }) {
  const pathname = useLocation({ select: (s) => s.pathname })
  const [mobileOpen, setMobileOpen] = useState(false)

  // 路由变化时收起移动端菜单：渲染期直接对齐（等价于 useEffect 重置，但无级联渲染）
  const [lastPath, setLastPath] = useState(pathname)
  if (lastPath !== pathname) {
    setLastPath(pathname)
    if (mobileOpen) setMobileOpen(false)
  }

  const title =
    NAV_SECTIONS.flatMap((s) => s.items).find((item) =>
      item.to === '/' ? pathname === '/' : pathname === item.to || pathname.startsWith(`${item.to}/`)
    )?.label ?? 'oh-my-qqbot'

  return (
    <div className="shell">
      {mobileOpen ? <div className="side-backdrop" onClick={() => setMobileOpen(false)} /> : null}
      <aside className={cn('sidebar', mobileOpen && 'open')}>
        <div className="side-head">
          <Link to="/" className="logo" title="返回首页">
            <LogoMark />
            oh-my-qqbot
          </Link>
          <span className="spacer" />
          <WsDot />
        </div>
        <nav className="sidenav" aria-label="主导航">
          {NAV_SECTIONS.map((section) => (
            <div key={section.title}>
              <div className="nav-group-title">{section.title}</div>
              {section.items.map((item) => {
                const IconCmp = Icon[item.icon]
                const active =
                  item.to === '/' ? pathname === '/' : pathname === item.to || pathname.startsWith(`${item.to}/`)
                return (
                  <Link key={item.to} to={item.to} className={cn('nav-item', active && 'active')}>
                    <IconCmp />
                    {item.label}
                  </Link>
                )
              })}
            </div>
          ))}
        </nav>
        <div className="side-foot">
          <button
            className="btn btn-ghost btn-sm btn-block"
            onClick={() => logoutLocal()}
            title="退出登录并清除本地凭据"
          >
            <LogOut /> 退出登录
          </button>
          <ThemeButton />
        </div>
      </aside>
      <div className="content">
        <header className="topbar">
          <button
            className="btn btn-ghost btn-icon show-sm-flex"
            aria-label="打开导航"
            onClick={() => setMobileOpen(true)}
          >
            <Menu />
          </button>
          <span className="topbar-title">{title}</span>
          <span className="spacer" />
          <button className="btn btn-ghost btn-sm" onClick={() => logoutLocal()} title="退出登录">
            <LogOut /> <span className="hide-sm">退出登录</span>
          </button>
          <ThemeButton />
        </header>
        <main className="main">{children}</main>
      </div>
    </div>
  )
}

/** WS 状态点：订阅 unifiedWs 状态。 */
function WsDot() {
  const [status, setStatus] = useState(unifiedWs.status)
  useEffect(() => unifiedWs.onStatus(setStatus), [])
  const label = status === 'connected' ? 'WS 已连接' : status === 'connecting' ? 'WS 连接中' : 'WS 未连接'
  return <span className="ws-dot" data-status={status} title={label} />
}
