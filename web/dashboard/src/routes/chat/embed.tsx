import { useEffect } from 'react'

import { TooltipProvider } from '@/components/ui/tooltip'
import { useAuthGuard } from '@/hooks/use-auth'

import { ChatPage } from './index'

/**
 * 给外部程序嵌入使用的聊天页：不挂载 dashboard 顶栏和侧边栏，仅保留聊天工作区。
 */
export function ChatEmbedPage() {
  const { checking } = useAuthGuard()

  useEffect(() => {
    document.title = '聊天 — oh-my-qqbot'
  }, [])

  if (checking) {
    return (
      <div className="bg-background flex h-screen items-center justify-center">
        <div className="text-muted-foreground">正在连接…</div>
      </div>
    )
  }

  return (
    <TooltipProvider delayDuration={300}>
      <div data-dashboard-shell="embed-chat" className="bg-background relative isolate h-screen overflow-hidden">
        <main
          id="main-content"
          data-dashboard-main="true"
          tabIndex={-1}
          className="relative z-10 h-full min-h-0 outline-none"
        >
          <ChatPage />
        </main>
      </div>
    </TooltipProvider>
  )
}
