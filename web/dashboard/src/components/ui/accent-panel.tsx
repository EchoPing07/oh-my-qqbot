import * as React from 'react'

import { cn } from '@/lib/utils'

interface AccentPanelProps extends React.HTMLAttributes<HTMLDivElement> {
  contentClassName?: string
  /** 兼容保留：线条风下无复古条纹，仅接受 prop 不再渲染装饰 */
  showRetroStripes?: boolean
  showRetroStripeDivider?: boolean
}

/**
 * 强调面板 — 线条风实现：左侧 2px 红色指示线，替代原红/黄/蓝三色复古条纹。
 */
const AccentPanel = React.forwardRef<HTMLDivElement, AccentPanelProps>(
  (
    {
      className,
      contentClassName,
      showRetroStripes = true,
      showRetroStripeDivider = true,
      children,
      ...props
    },
    ref
  ) => (
    <div
      ref={ref}
      data-dashboard-accent-panel="true"
      className={cn(
        'bg-card border-border text-card-foreground relative overflow-hidden border',
        className
      )}
      {...props}
    >
      <div
        aria-hidden="true"
        data-dashboard-accent-panel-stripes="true"
        className="bg-primary pointer-events-none absolute inset-y-0 left-0 hidden w-0.5"
      />
      <div
        data-dashboard-accent-panel-content="true"
        className={cn('relative min-h-full', contentClassName)}
      >
        {children}
      </div>
    </div>
  )
)
AccentPanel.displayName = 'AccentPanel'

export { AccentPanel }
