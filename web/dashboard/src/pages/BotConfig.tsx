/**
 * 机器人设置 页 — bot_config 全节查看与编辑。
 * 对齐后端：GET /config/bot、POST /config/bot（整份保存）、GET /config/schema/bot（节清单）、
 * POST /system/reload-config（热重载）。
 *
 * 编辑模型：通用键值编辑器（递归 3 层），修改暂存本地，按「节」保存
 * （saveBotSection 内部读整份 → 覆盖该节 → 整份提交，不会丢其他节的修改）。
 */

import { useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Eye, EyeOff, RotateCcw, Save } from 'lucide-react'

import { SECRET_PLACEHOLDER, getBotConfig, getBotSchema, saveBotSection, type ConfigData } from '@/api/config'
import { reloadConfig } from '@/api/misc'
import { Banner, Loading, TextInput, Toggle } from '@/components/ui'
import { cn } from '@/lib/cn'
import { toast } from '@/lib/toast'

/** 编辑器最大递归深度：更深的嵌套以只读 JSON 展示。 */
const MAX_DEPTH = 3

/**
 * 通用键值编辑器隐藏的密钥字段（S2：GET 已脱敏为占位符，编辑器不再展示，
 * 密钥统一由凭据卡/模型页编辑；隐藏后保存时原键原值透传，后端保留旧值）。
 */
const SECRET_EDITOR_HIDDEN = new Set(['app_secret', 'api_key'])

/**
 * 节的中文名（后端 schema title 优先）。
 * 「机器人」节含 AppID / AppSecret 等首次启用必填项，后端 schema 已将其排在第一位。
 */
function sectionTitle(name: string, schemaTitle?: string): string {
  if (schemaTitle) return schemaTitle
  const fallback: Record<string, string> = {
    a_memorix: '长期记忆',
    bot: '机器人',
    chat: '聊天',
    chinese_typo: '错别字',
    database: '数据库',
    debug: '调试',
    emoji: '表情包',
    experimental: '实验性功能',
    expression: '表达',
    jargon: '黑话',
    keyword_reaction: '关键词反应',
    log: '日志',
    mcp: 'MCP',
    message_receive: '消息接收',
    personality: '人格',
    response_post_process: '回复后处理',
    response_splitter: '断句',
    visual: '视觉',
    webui: 'WebUI',
  }
  return fallback[name] ?? name
}

export function BotConfigPage() {
  const qc = useQueryClient()
  const schema = useQuery({ queryKey: ['bot-schema'], queryFn: getBotSchema, staleTime: 5 * 60_000 })
  const config = useQuery({ queryKey: ['bot-config'], queryFn: getBotConfig })

  const [active, setActive] = useState('')
  const [dirty, setDirty] = useState<Record<string, ConfigData>>({})

  // 节清单来自后端有序数组（「机器人」在第一位）；缺省时回退 bot 保证首屏可用
  const sectionNames = useMemo(() => (schema.data ?? []).map((s) => s.name).filter(Boolean), [schema.data])
  const currentSection = active || sectionNames[0] || 'bot'

  /** 当前节的中文标题（优先后端 schema）。 */
  const currentTitle = useMemo(() => {
    const fromSchema = (schema.data ?? []).find((s) => s.name === currentSection)?.title
    return sectionTitle(currentSection, fromSchema)
  }, [schema.data, currentSection])

  const sectionDirty = Boolean(dirty[currentSection])
  const originalSection = useMemo(() => {
    const v = config.data?.[currentSection]
    return v && typeof v === 'object' && !Array.isArray(v) ? (v as ConfigData) : {}
  }, [config.data, currentSection])
  const editedSection = dirty[currentSection] ?? originalSection

  const markDirty = (section: string, next: ConfigData) => {
    setDirty((prev) => ({ ...prev, [section]: next }))
  }

  const saveCurrent = async () => {
    try {
      await saveBotSection(currentSection, editedSection)
      setDirty((prev) => {
        const next = { ...prev }
        delete next[currentSection]
        return next
      })
      await qc.invalidateQueries({ queryKey: ['bot-config'] })
      toast.ok(`「${currentTitle}」已保存`)
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '保存失败')
    }
  }

  const discardCurrent = () => {
    setDirty((prev) => {
      const next = { ...prev }
      delete next[currentSection]
      return next
    })
  }

  const reloadAll = async () => {
    try {
      await reloadConfig()
      toast.ok('后端配置已热重载')
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '热重载失败')
    }
  }

  const error = schema.error ?? config.error

  return (
    <div className="page page-wide">
      <div className="page-head">
        <div>
          <h1>机器人设置</h1>
          <p className="page-sub">bot_config.toml 全部配置节 · 修改后需点「保存」生效</p>
        </div>
        <div className="page-actions">
          <button
            className="btn btn-secondary btn-sm"
            onClick={() => {
              setDirty({})
              void qc.invalidateQueries({ queryKey: ['bot-config'] })
            }}
            title="丢弃本地未保存的修改并重新拉取"
          >
            <RotateCcw /> 重置
          </button>
          <button className="btn btn-secondary btn-sm" onClick={() => void reloadAll()}>
            热重载后端
          </button>
          <button className="btn btn-sm" onClick={() => void saveCurrent()} disabled={!sectionDirty}>
            <Save /> 保存当前节
          </button>
        </div>
      </div>

      {error ? <Banner kind="err">{error instanceof Error ? error.message : '配置加载失败'}</Banner> : null}
      {schema.isPending || config.isPending ? <Loading text="加载配置…" /> : null}

      {!error && !schema.isPending && !config.isPending ? (
        <div style={{ display: 'flex', gap: 14, alignItems: 'flex-start', minWidth: 0 }}>
          {/* 节导航 */}
          <nav className="card" style={{ width: 172, flexShrink: 0, position: 'sticky', top: 68 }}>
            <div style={{ padding: 6 }}>
              {sectionNames.map((name) => {
                const isDirty = Boolean(dirty[name])
                return (
                  <button
                    key={name}
                    className={cn('nav-item', name === currentSection && 'active')}
                    style={{ width: '100%', textAlign: 'left', justifyContent: 'space-between' }}
                    onClick={() => setActive(name)}
                  >
                    <span className="truncate">
                      {sectionTitle(name, (schema.data ?? []).find((s) => s.name === name)?.title)}
                    </span>
                    {isDirty ? <span className="dot dot-warn" title="有未保存修改" /> : null}
                  </button>
                )
              })}
            </div>
          </nav>

          {/* 当前节编辑器（「机器人」节额外挂一张凭据卡） */}
          <div
            style={{
              flex: 1,
              minWidth: 0,
              display: 'flex',
              flexDirection: 'column',
              gap: 14,
            }}
          >
            {currentSection === 'bot' ? (
              <BotCredentialCard
                section={editedSection}
                onSaved={() => {
                  setDirty((prev) => {
                    const next = { ...prev }
                    delete next.bot
                    return next
                  })
                  void qc.invalidateQueries({ queryKey: ['bot-config'] })
                }}
              />
            ) : null}
            <div className="card" style={{ minWidth: 0 }}>
            <div className="card-head">
              <h3>{currentTitle}</h3>
              <span className="mono muted-xs">{currentSection}</span>
              {sectionDirty ? <span className="badge badge-warn">未保存</span> : null}
              <span className="spacer" />
              {sectionDirty ? (
                <button className="btn btn-ghost btn-sm" onClick={discardCurrent}>
                  还原本节
                </button>
              ) : null}
            </div>
            <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
              <ValueEditor
                value={editedSection}
                onChange={(next) => markDirty(currentSection, next as ConfigData)}
                depth={0}
              />
              {Object.keys(editedSection).length === 0 ? (
                <p className="muted-xs">该节为空</p>
              ) : null}
            </div>
            </div>
          </div>
        </div>
      ) : null}
    </div>
  )
}

// ══════════════════ 机器人凭据（AppID / AppSecret） ══════════════════

/**
 * 机器人凭据卡片 — 单独编辑 [bot] 节的 app_id / app_secret。
 *
 * 这两个字段是 QQ 开放平台的机器人凭据（装配层优先读 bot_config，缺失才回退
 * 环境变量），因此从通用键值编辑器里提出来做成独立卡片：带脱敏、独立校验与保存。
 * S2：后端 GET 不回传明文密钥（已配置时为 SECRET_PLACEHOLDER 占位）——密钥输入框
 * 保持「留空保留」语义（参考 Setup.tsx saveModelRoute）：留空 = 保留已配置的
 * 旧密钥，仅当输入新值时才提交 app_secret 字段。
 * 写入仍走 saveBotSection('bot', …)：读整份 → 覆盖该节 → 整份提交。
 */
function BotCredentialCard({
  section,
  onSaved,
}: {
  section: ConfigData
  onSaved: () => void
}) {
  const [appID, setAppID] = useState(String(section.app_id ?? ''))
  const [appSecret, setAppSecret] = useState('')
  const [reveal, setReveal] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  // GET 已脱敏：app_secret 为占位符即「已配置过密钥」。
  const hasSecret = String(section.app_secret ?? '') === SECRET_PLACEHOLDER

  // 外部数据刷新（首屏加载 / 保存后失效重取）时同步输入框：渲染期对齐，无 effect 级联。
  // 密钥输入框永远从空开始（不回显占位符）。
  const [lastExternal, setLastExternal] = useState('')
  const externalKey = `${String(section.app_id ?? '')}\u0000${hasSecret}`
  if (externalKey !== lastExternal) {
    setLastExternal(externalKey)
    setAppID(String(section.app_id ?? ''))
    setAppSecret('')
  }

  const configured = String(section.app_id ?? '').trim() !== ''

  const save = async () => {
    const id = appID.trim()
    const secret = appSecret.trim()
    if (!id) {
      setError('应用 ID 需要填写')
      return
    }
    if (!secret && !hasSecret) {
      setError('应用密钥需要填写（已配置过时留空即保留旧密钥）')
      return
    }
    setError('')
    setSaving(true)
    try {
      // 留空 = 保留旧密钥：不下发 app_secret 键（后端对空串/占位符均保留旧值）。
      const payload: ConfigData = { ...section, app_id: id }
      if (secret) payload.app_secret = secret
      else delete payload.app_secret
      await saveBotSection('bot', payload)
      toast.ok('机器人凭据已保存，请热重载或重启后生效')
      onSaved()
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="card" style={{ minWidth: 0 }}>
      <div className="card-head">
        <h3>机器人凭据</h3>
        {configured ? (
          <span className="badge badge-ok">ID 已配置</span>
        ) : (
          <span className="badge badge-warn">ID 未配置</span>
        )}
        {hasSecret ? (
          <span className="badge badge-ok">密钥已配置</span>
        ) : (
          <span className="badge badge-warn">密钥未配置</span>
        )}
        <span className="spacer" />
        <button className="btn btn-ghost btn-sm" onClick={() => setReveal(!reveal)} title={reveal ? '隐藏密钥' : '显示密钥'}>
          {reveal ? <EyeOff /> : <Eye />} {reveal ? '隐藏' : '显示'}
        </button>
        <button className="btn btn-sm" onClick={() => void save()} disabled={saving}>
          {saving ? <span className="spin" /> : <Save />} 保存凭据
        </button>
      </div>
      <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 12, maxWidth: 520 }}>
        {error ? <Banner kind="err">{error}</Banner> : null}
        <div>
          <label className="label" htmlFor="bot-app-id">
            应用 ID（AppID）
          </label>
          <TextInput
            id="bot-app-id"
            value={appID}
            onChange={(e) => setAppID(e.target.value)}
            placeholder="在 QQ 开放平台创建机器人后获得"
            autoComplete="off"
          />
          <p className="field-hint">
            对应 bot_config.toml 的 <code>[bot] app_id</code>
          </p>
        </div>
        <div>
          <label className="label" htmlFor="bot-app-secret">
            应用密钥（AppSecret）{hasSecret ? ' — 已配置，留空保留' : ''}
          </label>
          <TextInput
            id="bot-app-secret"
            type={reveal ? 'text' : 'password'}
            value={appSecret}
            onChange={(e) => setAppSecret(e.target.value)}
            placeholder={hasSecret ? '留空保留已配置的密钥' : '用于换取接口调用凭证，请勿外传'}
            autoComplete="new-password"
          />
          <p className="field-hint">
            对应 bot_config.toml 的 <code>[bot] app_secret</code>；密钥保存后不再回显。
          </p>
        </div>
        <p className="muted-xs">
          凭据写盘后需热重载配置或重启主程序，QQ 网关才会用新凭据重新连接。
        </p>
      </div>
    </div>
  )
}

// ── 通用值编辑器 ──

interface EditorProps {
  value: unknown
  onChange: (next: unknown) => void
  /** 字段名（顶层为空） */
  field?: string
  depth: number
}

/** 单个键值行 / 嵌套对象分组。 */
function ValueEditor({ value, onChange, field, depth }: EditorProps) {
  if (depth >= MAX_DEPTH && value !== null && typeof value === 'object') {
    return <DeepReadonly field={field} value={value} />
  }

  if (typeof value === 'boolean') {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '3px 0' }}>
        <Toggle checked={value} onChange={onChange} label={field ?? ''} />
      </div>
    )
  }

  if (typeof value === 'number') {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '3px 0' }}>
        <label className="label" style={{ width: FIELD_W, flexShrink: 0, marginBottom: 0 }} title={field}>
          {field}
        </label>
        <input
          className="input"
          style={{ width: 180 }}
          type="number"
          value={value}
          step="any"
          onChange={(e) => {
            const raw = e.target.value
            // 允许中间态（空串/-），失焦前不强制数字
            onChange(raw === '' || raw === '-' ? raw : Number(raw))
          }}
          onBlur={(e) => {
            const n = Number(e.target.value)
            if (Number.isNaN(n)) onChange(0)
            else onChange(n)
          }}
        />
      </div>
    )
  }

  if (Array.isArray(value)) {
    const isStringArray = value.every((v) => typeof v === 'string')
    return (
      <div style={{ padding: '3px 0' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <label className="label" style={{ width: FIELD_W, flexShrink: 0, marginBottom: 0 }} title={field}>
            {field} <span className="muted-xs">[{value.length}]</span>
          </label>
        </div>
        {isStringArray ? (
          <textarea
            className="textarea"
            rows={Math.min(6, Math.max(2, value.length))}
            value={value.map(String).join('\n')}
            onChange={(e) =>
              onChange(
                e.target.value
                  .split('\n')
                  // 保留空行占位避免输入中途丢项；提交由 blur 归一化
                  .map((line) => line)
              )
            }
            onBlur={(e) =>
              onChange(
                e.target.value
                  .split('\n')
                  .map((line) => line.trim())
                  .filter((line) => line !== '')
              )
            }
            style={{ marginTop: 4 }}
          />
        ) : (
          <div className="code-block" style={{ marginTop: 4 }}>
            {JSON.stringify(value, null, 2)}
          </div>
        )}
      </div>
    )
  }

  if (value !== null && typeof value === 'object') {
    const entries = Object.entries(value as Record<string, unknown>)
    return (
      <fieldset
        style={{
          border: '1px solid var(--border)',
          borderRadius: 'var(--radius-sm)',
          padding: '8px 10px 10px',
          marginLeft: field ? 0 : 0,
        }}
      >
        {field ? (
          <legend className="label" style={{ padding: '0 6px', marginBottom: 0 }} title={field}>
            {field}
          </legend>
        ) : null}
        <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
          {entries
            .filter(([key]) => !SECRET_EDITOR_HIDDEN.has(key))
            .map(([key, val]) => (
              <ValueEditor
                key={key}
                field={key}
                value={val}
                depth={depth + 1}
                onChange={(next) => onChange({ ...(value as ConfigData), [key]: next })}
              />
            ))}
        </div>
      </fieldset>
    )
  }

  // string / null / 其他 → 文本输入
  const text = value === null || value === undefined ? '' : String(value)
  const multiline = text.length > 80 || text.includes('\n')
  return (
    <div style={{ display: 'flex', alignItems: 'flex-start', gap: 8, padding: '3px 0' }}>
      <label className="label" style={{ width: FIELD_W, flexShrink: 0, marginBottom: 0, paddingTop: multiline ? 6 : 7 }} title={field}>
        {field}
      </label>
      {multiline ? (
        <textarea
          className="textarea"
          rows={Math.min(6, text.split('\n').length + 1)}
          value={text}
          onChange={(e) => onChange(e.target.value)}
          style={{ flex: 1 }}
        />
      ) : (
        <input
          className="input"
          style={{ flex: 1, maxWidth: 420 }}
          value={text}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </div>
  )
}

const FIELD_W = 220

/** 超深嵌套只读展示。 */
function DeepReadonly({ field, value }: { field?: string; value: unknown }) {
  return (
    <div style={{ padding: '3px 0' }}>
      <label className="label" style={{ marginBottom: 2 }} title={field}>
        {field} <span className="muted-xs">（深层结构，只读）</span>
      </label>
      <div className="code-block">{JSON.stringify(value, null, 2)}</div>
    </div>
  )
}

