/** 路由级错误边界：展示可读错误与重载按钮。 */

import { CircleAlert, RotateCcw } from 'lucide-react'
import { Component, type ErrorInfo, type ReactNode } from 'react'

export function RouteErrorBoundary({ error }: { error: unknown }) {
  const message = error instanceof Error ? error.message : String(error)
  return (
    <div className="page" style={{ paddingTop: 40 }}>
      <div className="banner banner-err">
        <CircleAlert />
        <div>
          <div style={{ fontWeight: 600 }}>页面加载出错</div>
          <div className="xs wrap-anywhere" style={{ marginTop: 4 }}>{message}</div>
        </div>
      </div>
      <div>
        <button className="btn btn-secondary btn-sm" onClick={() => window.location.reload()}>
          <RotateCcw /> 重新加载
        </button>
      </div>
    </div>
  )
}

/** 顶层 ErrorBoundary（兜底渲染崩溃）。 */
export class ErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state = { error: null as Error | null }

  static getDerivedStateFromError(error: Error) {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('应用崩溃:', error, info.componentStack)
  }

  render() {
    if (this.state.error) {
      return (
        <div style={{ padding: 40, maxWidth: 640, margin: '0 auto' }}>
          <div className="banner banner-err">
            <CircleAlert />
            <div>
              <div style={{ fontWeight: 600 }}>应用发生未捕获错误</div>
              <div className="xs wrap-anywhere" style={{ marginTop: 4 }}>{this.state.error.message}</div>
            </div>
          </div>
          <button
            className="btn btn-secondary btn-sm"
            style={{ marginTop: 12 }}
            onClick={() => window.location.reload()}
          >
            <RotateCcw /> 重新加载
          </button>
        </div>
      )
    }
    return this.props.children
  }
}
