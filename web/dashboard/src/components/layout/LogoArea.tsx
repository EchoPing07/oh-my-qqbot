import { cn } from '@/lib/utils'

interface LogoAreaProps {
  sidebarOpen: boolean
}

/**
 * 侧栏 Logo 区 — 线条风：黑色方标 + 红点 + 无衬线字标。
 * 折叠时只保留方标。
 */
export function LogoArea({ sidebarOpen }: LogoAreaProps) {
  return (
    <div
      data-dashboard-logo-area="true"
      className="relative flex h-[var(--layout-sidebar-logo-height)] items-center border-b px-[var(--layout-sidebar-logo-padding-x)]"
    >
      <div className="relative flex w-full items-center justify-start overflow-visible lg:flex-none">
        <div
          className={cn(
            'flex shrink-0 items-center gap-2.5 transition-opacity duration-[220ms] motion-reduce:transition-none',
            !sidebarOpen && 'lg:pointer-events-none lg:opacity-0'
          )}
        >
          <span
            aria-hidden="true"
            data-dashboard-logo-mark="true"
            className="bg-foreground relative flex h-8 w-8 shrink-0 items-center justify-center"
          >
            {/* 线条风 M 字标 */}
            <svg viewBox="0 0 24 24" className="h-5 w-5" fill="none" aria-hidden="true">
              <path
                d="M5 18V6l7 7 7-7v12"
                stroke="hsl(var(--background))"
                strokeWidth="2.4"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
            <span className="bg-primary absolute -right-0.5 -bottom-0.5 h-1.5 w-1.5" />
          </span>
          <span
            data-dashboard-logo-title="true"
            className="text-foreground text-base leading-none font-bold tracking-tight whitespace-nowrap"
          >
            oh-my-qqbot
          </span>
        </div>
      </div>
      {/* 折叠态：仅方标 */}
      {!sidebarOpen && (
        <span
          aria-hidden="true"
          data-dashboard-logo-mark="true"
          className="bg-foreground pointer-events-none absolute top-1/2 left-[var(--layout-sidebar-nav-padding-collapsed)] hidden h-8 w-8 -translate-y-1/2 items-center justify-center max-lg:hidden lg:flex"
        >
          <svg viewBox="0 0 24 24" className="h-5 w-5" fill="none" aria-hidden="true">
            <path
              d="M5 18V6l7 7 7-7v12"
              stroke="hsl(var(--background))"
              strokeWidth="2.4"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
          <span className="bg-primary absolute -right-0.5 -bottom-0.5 h-1.5 w-1.5" />
        </span>
      )}
    </div>
  )
}
