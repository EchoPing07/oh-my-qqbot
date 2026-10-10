import { Link } from '@tanstack/react-router'
import { Home } from 'lucide-react'

export function NotFoundPage() {
  return (
    <div className="page" style={{ paddingTop: 56, alignItems: 'center' }}>
      <div className="mono" style={{ fontSize: 40, fontWeight: 700, color: 'var(--muted-fg)' }}>404</div>
      <p className="muted">页面不存在或已被移除</p>
      <Link to="/" className="btn btn-secondary btn-sm" style={{ marginTop: 8 }}>
        <Home /> 返回首页
      </Link>
    </div>
  )
}
