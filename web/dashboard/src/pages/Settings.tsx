/**
 * 设置 页 — 安全 / 外观 / 关于 三个分组。
 *
 * 安全（对齐 internal/webui/routers/auth.go 与 system.go）：
 * - POST /auth/update            修改密码（后端 UpdateToken 即时生效，当前 Bearer 已是新值，无需重登）
 * - POST /auth/regenerate        重新生成 token（所有已登录会话立即失效，必须重新登录）
 * - GET  /setup/status           首次配置状态（带 Bearer，is_first_setup / token_source）
 * - POST /setup/reset | /setup/complete   重置 / 完成首次配置向导
 * - POST /system/reload-config   热重载配置
 * 外观：localStorage 'omb-theme-mode'（经 @/lib/theme 的 readStoredTheme/storeTheme/applyTheme）。
 * 关于：@/lib/version（前端版本）+ GET /system/status（后端版本 / Go 版本 / 启动时间）。
 */

import { useEffect, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Check,
  Copy,
  KeyRound,
  LogOut,
  Monitor,
  Moon,
  RefreshCw,
  RotateCcw,
  Sun,
  TriangleAlert,
} from 'lucide-react'

import {
  completeSetup,
  getSetupStatusAuthorized,
  regenerateToken,
  resetSetup,
  updatePassword,
} from '@/api/auth'
import { getSystemStatus } from '@/api/system'
import { reloadConfig } from '@/api/misc'
import { APP_NAME, APP_VERSION } from '@/lib/version'
import {
  applyTheme,
  readStoredTheme,
  storeTheme,
  watchSystemTheme,
  type ThemeMode,
} from '@/lib/theme'
import { logoutLocal } from '@/lib/token'
import { toast } from '@/lib/toast'
import { formatDateTime, formatUptime } from '@/lib/format'
import { Banner, ConfirmModal, Field, Loading, Modal, TextInput } from '@/components/ui'
import { cn } from '@/lib/cn'

type SettingsTab = 'security' | 'appearance' | 'about'

const TABS: Array<{ key: SettingsTab; label: string }> = [
  { key: 'security', label: '安全' },
  { key: 'appearance', label: '外观' },
  { key: 'about', label: '关于' },
]

/** 与后端 validateCustomToken（internal/webui/auth.go）一致的特殊符号集合。 */
const SPECIAL_CHARS = '!@#$%^&*()_+-=[]{}|;:,.<>?/'

/** 客户端预校验：返回问题文案，空串表示通过（规则对齐后端 validateCustomToken）。 */
function passwordProblem(v: string): string {
  if (!v) return '请输入新密码'
  if (v.length < 10) return '密码长度至少 10 位'
  let hasUpper = false
  let hasLower = false
  let hasSpecial = false
  for (const ch of v) {
    if (ch >= 'A' && ch <= 'Z') hasUpper = true
    else if (ch >= 'a' && ch <= 'z') hasLower = true
    else if (SPECIAL_CHARS.includes(ch)) hasSpecial = true
  }
  if (!hasUpper) return '必须包含大写字母'
  if (!hasLower) return '必须包含小写字母'
  if (!hasSpecial) return '必须包含特殊符号（如 !@#$%^&* 等）'
  return ''
}

/** 复制文本到剪贴板（结果用 toast 反馈）。 */
async function copyText(text: string, okMsg: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    toast.ok(okMsg)
  } catch {
    toast.err('复制失败，请手动选择文本复制')
  }
}

export function SettingsPage() {
  const [tab, setTab] = useState<SettingsTab>('security')

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>设置</h1>
          <p className="page-sub">安全凭据、界面外观与应用信息</p>
        </div>
        <div className="page-actions">
          <button className="btn btn-ghost" onClick={() => logoutLocal()} title="清除本地 token 并回到登录页">
            <LogOut /> 登出
          </button>
        </div>
      </div>

      <div className="tabs" role="tablist">
        {TABS.map((t) => (
          <button
            key={t.key}
            type="button"
            role="tab"
            aria-selected={tab === t.key}
            className={cn('tab', tab === t.key && 'active')}
            onClick={() => setTab(t.key)}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'security' ? <SecurityPanel /> : null}
      {tab === 'appearance' ? <AppearancePanel /> : null}
      {tab === 'about' ? <AboutPanel /> : null}
    </div>
  )
}

// ══════════════════════════════ 安全 ══════════════════════════════

function SecurityPanel() {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <PasswordCard />
      <RegenerateCard />
      <SetupCard />
      <ReloadCard />
    </div>
  )
}

/** 卡片一：修改密码（POST /auth/update）。 */
function PasswordCard() {
  const [pwd, setPwd] = useState('')
  const [confirm, setConfirm] = useState('')
  const [errors, setErrors] = useState<{ pwd?: string; confirm?: string }>({})
  const [saving, setSaving] = useState(false)

  const submit = async () => {
    const p = passwordProblem(pwd)
    const c = confirm !== pwd ? '两次输入的密码不一致' : ''
    setErrors({ pwd: p || undefined, confirm: c || undefined })
    if (p || c) return
    setSaving(true)
    try {
      await updatePassword(pwd)
      // 后端 UpdateToken 即时生效：本地 Bearer 已是新值，无需重新登录
      toast.ok('密码已更新，请牢记')
      setPwd('')
      setConfirm('')
      setErrors({})
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '密码更新失败')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="card">
      <div className="card-head">
        <h3>修改密码</h3>
        <span className="muted-xs">POST /auth/update · 即时生效</span>
      </div>
      <div className="card-body">
        <form
          onSubmit={(e) => {
            e.preventDefault()
            void submit()
          }}
          style={{ display: 'flex', flexDirection: 'column', gap: 12, maxWidth: 420 }}
        >
          <Field
            label="新密码"
            error={errors.pwd}
            hint="至少 10 位，需同时包含大写字母、小写字母与特殊符号"
          >
            <TextInput
              type="password"
              autoComplete="new-password"
              placeholder="••••••••••"
              value={pwd}
              invalid={Boolean(errors.pwd)}
              disabled={saving}
              onChange={(e) => {
                const v = e.target.value
                setPwd(v)
                // 已有错误时随输入实时重校验，便于即时修正
                if (errors.pwd) setErrors((prev) => ({ ...prev, pwd: passwordProblem(v) || undefined }))
                if (errors.confirm && v === confirm) setErrors((prev) => ({ ...prev, confirm: undefined }))
              }}
            />
          </Field>
          <Field label="确认新密码" error={errors.confirm}>
            <TextInput
              type="password"
              autoComplete="new-password"
              placeholder="再次输入新密码"
              value={confirm}
              invalid={Boolean(errors.confirm)}
              disabled={saving}
              onChange={(e) => {
                const v = e.target.value
                setConfirm(v)
                if (errors.confirm && v === pwd) setErrors((prev) => ({ ...prev, confirm: undefined }))
              }}
            />
          </Field>
          <div>
            <button type="submit" className="btn" disabled={saving}>
              {saving ? <span className="spin" /> : <KeyRound />} 修改密码
            </button>
          </div>
          <p className="muted-xs">修改即时生效：当前浏览器会话将继续使用新密码，无需重新登录。</p>
        </form>
      </div>
    </div>
  )
}

/** 卡片二：重新生成 token（POST /auth/regenerate，双重确认 + 新 token 展示）。 */
function RegenerateCard() {
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [regenerating, setRegenerating] = useState(false)
  const [resultOpen, setResultOpen] = useState(false)
  const [newToken, setNewToken] = useState('')

  const doRegenerate = async () => {
    setRegenerating(true)
    try {
      const token = await regenerateToken()
      setNewToken(token)
      setConfirmOpen(false)
      setResultOpen(true)
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '重新生成失败')
    } finally {
      setRegenerating(false)
    }
  }

  return (
    <div className="card">
      <div className="card-head">
        <h3>重新生成 Token</h3>
        <span className="muted-xs">POST /auth/regenerate</span>
      </div>
      <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        <p className="sm" style={{ color: 'var(--muted-fg)' }}>
          生成全新的随机登录凭据并立即启用。旧 token 将马上作废。
        </p>
        <div>
          <button className="btn btn-danger" onClick={() => setConfirmOpen(true)} disabled={regenerating}>
            <RotateCcw /> 重新生成 Token
          </button>
        </div>
      </div>

      {/* 第一重：危险操作确认 */}
      <ConfirmModal
        open={confirmOpen}
        title="确认重新生成 Token？"
        danger
        loading={regenerating}
        confirmText="确认重新生成"
        message="所有已登录会话将失效，请立即保存新 token。此操作不可撤销。"
        onConfirm={() => void doRegenerate()}
        onClose={() => setConfirmOpen(false)}
      />

      {/* 第二重：展示新 token（仅此一次，关闭后不可再看） */}
      <Modal
        open={resultOpen}
        title="Token 已重新生成"
        width={480}
        onClose={() => setResultOpen(false)}
        footer={
          <>
            <button className="btn btn-secondary btn-sm" onClick={() => setResultOpen(false)}>
              留在本页
            </button>
            <button className="btn btn-danger btn-sm" onClick={() => logoutLocal()}>
              去登录
            </button>
          </>
        }
      >
        <Banner kind="warn">所有已登录会话已失效，请立即保存新 token；关闭此弹窗后将无法再次查看。</Banner>
        <div style={{ display: 'flex', gap: 8, alignItems: 'stretch' }}>
          <div className="code-block" style={{ flex: 1 }}>
            {newToken}
          </div>
          <button
            className="btn btn-secondary btn-sm"
            onClick={() => void copyText(newToken, '新 token 已复制到剪贴板')}
          >
            <Copy /> 复制
          </button>
        </div>
        <p className="muted-xs">保存后请点击「去登录」，使用新 token 重新登录。</p>
      </Modal>
    </div>
  )
}

/** 卡片三：首次配置状态（GET /setup/status + POST /setup/reset|complete）。 */
function SetupCard() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const status = useQuery({
    queryKey: ['settings-setup-status'],
    queryFn: getSetupStatusAuthorized,
    staleTime: 10_000,
  })
  const [confirm, setConfirm] = useState<null | 'reset' | 'complete'>(null)
  const [acting, setActing] = useState(false)

  const act = async (which: 'reset' | 'complete') => {
    setActing(true)
    try {
      if (which === 'reset') {
        await resetSetup()
        toast.info('已重置向导状态，正在进入向导…')
        setConfirm(null)
        navigate({ to: '/setup' })
      } else {
        await completeSetup()
        setConfirm(null)
        toast.ok('已标记首次配置完成')
        void qc.invalidateQueries({ queryKey: ['settings-setup-status'] })
      }
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '操作失败')
    } finally {
      setActing(false)
    }
  }

  const st = status.data
  const sourceLabel =
    st?.token_source === 'configured' ? '自定义密码' : st?.token_source === 'temporary' ? '默认密码' : st?.token_source || '—'

  return (
    <div className="card">
      <div className="card-head">
        <h3>首次配置状态</h3>
        <span className="muted-xs">GET /setup/status</span>
      </div>
      <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        {status.isPending ? (
          <Loading text="读取向导状态…" />
        ) : status.isError || !st ? (
          <Banner kind="err">向导状态读取失败：{errText(status.error)}</Banner>
        ) : (
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
            <span className="muted-xs">向导状态</span>
            <span className={cn('badge', st.is_first_setup ? 'badge-warn' : 'badge-ok')}>
              {st.is_first_setup ? '未完成' : '已完成'}
            </span>
            <span className="muted-xs" style={{ marginLeft: 12 }}>
              凭据来源
            </span>
            <span className="badge">{sourceLabel}</span>
          </div>
        )}
        <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
          <button className="btn btn-danger" onClick={() => setConfirm('reset')} disabled={acting}>
            <RotateCcw /> 重新进入向导
          </button>
          <button
            className="btn btn-secondary"
            onClick={() => setConfirm('complete')}
            disabled={acting || !st?.is_first_setup}
            title={st?.is_first_setup ? undefined : '向导已完成，无需重复标记'}
          >
            <Check /> 完成向导
          </button>
        </div>
      </div>

      <ConfirmModal
        open={confirm !== null}
        title={confirm === 'reset' ? '重新进入首次配置向导？' : '标记向导为已完成？'}
        danger={confirm === 'reset'}
        loading={acting}
        confirmText={confirm === 'reset' ? '重置并进入向导' : '标记完成'}
        message={
          confirm === 'reset'
            ? '将重置首次配置状态并跳转到 /setup 向导页；已保存的配置不会被清除。'
            : '后端将记录「首次配置已完成」，之后不再引导进入向导。'
        }
        onConfirm={() => void act(confirm ?? 'complete')}
        onClose={() => setConfirm(null)}
      />
    </div>
  )
}

/** 卡片四：热重载配置（POST /system/reload-config）。 */
function ReloadCard() {
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [reloading, setReloading] = useState(false)

  const doReload = async () => {
    setReloading(true)
    try {
      await reloadConfig()
      toast.ok('配置已热重载')
      setConfirmOpen(false)
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '热重载失败')
    } finally {
      setReloading(false)
    }
  }

  return (
    <div className="card">
      <div className="card-head">
        <h3>热重载配置</h3>
        <span className="muted-xs">POST /system/reload-config</span>
      </div>
      <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        <p className="sm" style={{ color: 'var(--muted-fg)' }}>
          让后端重新读取 bot_config / model_config 等配置文件，无需重启进程。部分配置项可能需要重启才能生效。
        </p>
        <div>
          <button className="btn btn-danger" onClick={() => setConfirmOpen(true)} disabled={reloading}>
            <RefreshCw /> 立即热重载
          </button>
        </div>
      </div>
      <ConfirmModal
        open={confirmOpen}
        title="确认热重载配置？"
        danger
        loading={reloading}
        confirmText="确认重载"
        message="后端将重新加载磁盘上的配置文件；正在运行中的对话可能受到短暂影响。"
        onConfirm={() => void doReload()}
        onClose={() => setConfirmOpen(false)}
      />
    </div>
  )
}

// ══════════════════════════════ 外观 ══════════════════════════════

const THEME_MODES: Array<{ key: ThemeMode; label: string; desc: string; Icon: typeof Sun }> = [
  { key: 'light', label: '亮色', desc: '纸白底色，适合明亮环境', Icon: Sun },
  { key: 'dark', label: '暗色', desc: '深色底色，适合夜间使用', Icon: Moon },
  { key: 'system', label: '跟随系统', desc: '自动跟随操作系统的深浅色偏好', Icon: Monitor },
]

/** 外观面板：主题三选（读写 localStorage 'omb-theme-mode'）。 */
function AppearancePanel() {
  const [mode, setMode] = useState<ThemeMode>(() => readStoredTheme())

  // 选择即时生效；system 模式下订阅系统深浅色变化
  useEffect(() => {
    applyTheme(mode)
    return watchSystemTheme(mode, () => applyTheme('system'))
  }, [mode])

  const select = (m: ThemeMode) => {
    setMode(m)
    storeTheme(m)
  }

  return (
    <div className="card">
      <div className="card-head">
        <h3>主题</h3>
        <span className="muted-xs">即时生效，保存在本地（omb-theme-mode）</span>
      </div>
      <div className="card-body">
        <div
          role="radiogroup"
          aria-label="主题模式"
          style={{
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))',
            gap: 10,
            maxWidth: 640,
          }}
        >
          {THEME_MODES.map(({ key, label, desc, Icon }) => {
            const active = mode === key
            return (
              <button
                key={key}
                type="button"
                role="radio"
                aria-checked={active}
                onClick={() => select(key)}
                style={{
                  textAlign: 'left',
                  padding: 12,
                  background: 'var(--card)',
                  color: 'var(--fg)',
                  border: `1px solid ${active ? 'var(--fg)' : 'var(--border)'}`,
                  boxShadow: active ? 'inset 0 0 0 1px var(--fg)' : 'none',
                  borderRadius: 'var(--radius)',
                  cursor: 'pointer',
                  display: 'flex',
                  flexDirection: 'column',
                  gap: 4,
                }}
              >
                <span style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  <Icon />
                  <span className="sm" style={{ fontWeight: 600 }}>
                    {label}
                  </span>
                  <span className="spacer" />
                  {active ? <Check /> : null}
                </span>
                <span className="muted-xs">{desc}</span>
              </button>
            )
          })}
        </div>
      </div>
    </div>
  )
}

// ══════════════════════════════ 关于 ══════════════════════════════

/** 关于面板：前端版本（@/lib/version）+ 后端状态（GET /system/status）+ 开源说明。 */
function AboutPanel() {
  const sys = useQuery({
    queryKey: ['about-system-status'],
    queryFn: getSystemStatus,
    staleTime: 30_000,
  })

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div className="card">
        <div className="card-head">
          <h3>应用信息</h3>
        </div>
        <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
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
          <p className="muted-xs">
            <TriangleAlert
              size={12}
              style={{ display: 'inline', verticalAlign: '-2px', marginRight: 4 }}
            />
            分发本项目（含修改版）时请一并附上 GPLv3 协议文本与来源声明。
          </p>
        </div>
      </div>
    </div>
  )
}

/** 统一错误文案提取。 */
function errText(err: unknown): string {
  return err instanceof Error ? err.message : '未知错误'
}
