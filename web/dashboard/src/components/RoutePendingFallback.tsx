/** 路由懒加载 pending 骨架。 */

export function RoutePendingFallback() {
  return (
    <div className="page" style={{ paddingTop: 8 }}>
      <div className="loading-block">
        <span className="spin" /> 加载中…
      </div>
    </div>
  )
}
