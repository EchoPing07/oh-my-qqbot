/**
 * 关于 页 — 应用信息（版本 / 运行时）与开源说明。
 *
 * 已独立于「设置」页：侧边栏「系统」分组单独入口。
 * 数据源：@/lib/version（前端版本）+ GET /system/status（后端版本 / Go 版本 / 启动时间 / 运行时长）。
 */

import { useQuery } from '@tanstack/react-query'
import { CircleCheck, RefreshCw, TriangleAlert } from 'lucide-react'

import { getSystemStatus } from '@/api/system'
import { Banner, Loading } from '@/components/ui'
import { APP_NAME, APP_VERSION } from '@/lib/version'
import { formatDateTime, formatUptime } from '@/lib/format'

/** 统一错误文案提取。 */
function errText(err: unknown): string {
  return err instanceof Error ? err.message : '未知错误'
}

export function AboutPage() {
  const sys = useQuery({
    queryKey: ['about-system-status'],
    queryFn: getSystemStatus,
    staleTime: 30_000,
  })

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>关于</h1>
          <p className="page-sub">版本信息、运行状态与开源许可说明</p>
        </div>
        <div className="page-actions">
          <button
            className="btn btn-secondary btn-sm"
            disabled={sys.isFetching}
            onClick={() => void sys.refetch()}
          >
            {sys.isFetching ? <span className="spin" /> : <RefreshCw />} 刷新
          </button>
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h3>应用信息</h3>
          <span className="spacer" />
          {sys.data ? <span className="badge badge-ok">运行中</span> : null}
        </div>
        <div className="card-body">
          {sys.isPending ? (
            <Loading text="读取后端状态…" />
          ) : (
            <div className="kv-grid">
              <div className="kv">
                <span className="k">应用名</span>
                <span className="v">{APP_NAME}</span>
              </div>
              <div className="kv">
                <span className="k">WebUI 版本</span>
                <span className="v">{APP_VERSION}</span>
              </div>
              <div className="kv">
                <span className="k">后端版本</span>
                <span className="v">{sys.data?.version || '—'}</span>
              </div>
              <div className="kv">
                <span className="k">Go 版本</span>
                <span className="v">{sys.data?.go_version || '—'}</span>
              </div>
              <div className="kv">
                <span className="k">后端启动时间</span>
                <span className="v">{sys.data ? formatDateTime(sys.data.start_time) : '—'}</span>
              </div>
              <div className="kv">
                <span className="k">运行时长</span>
                <span className="v">{sys.data ? formatUptime(sys.data.uptime) : '—'}</span>
              </div>
            </div>
          )}
          {sys.isError ? <Banner kind="err">后端状态获取失败：{errText(sys.error)}</Banner> : null}
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h3>开源说明</h3>
        </div>
        <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
          <p className="sm">
            本项目以 <strong>GPLv3</strong> 协议开源，遵循协议自由使用、修改与分发。
          </p>
          <p className="sm">
            项目衍生自 <strong>MaiBot</strong>，在此感谢原项目作者的贡献。
          </p>
          <p className="sm">
            WebUI 前端为全新实现（React + Go 后端原生路由），不依赖 MaiBot 原前端代码。
          </p>
          <p className="muted-xs" style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
            <TriangleAlert size={12} />
            分发本项目（含修改版）时请一并附上 GPLv3 协议文本与来源声明。
          </p>
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h3>运行提示</h3>
        </div>
        <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
          <p className="sm" style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
            <CircleCheck size={13} /> 配置改动后可在「机器人设置」页点「热重载后端」即时生效。
          </p>
          <p className="sm" style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
            <CircleCheck size={13} /> 排查问题优先看「日志」页；观察决策过程看「运行监控」页。
          </p>
          <p className="muted-xs">
            忘记登录密码时，删除 <code>data/webui.json</code> 后重启主程序即可恢复默认密码。
          </p>
        </div>
      </div>
    </div>
  )
}
