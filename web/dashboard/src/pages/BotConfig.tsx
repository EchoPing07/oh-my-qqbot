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
import { RotateCcw, Save } from 'lucide-react'

import { getBotConfig, getBotSchema, saveBotSection, type ConfigData } from '@/api/config'
import { reloadConfig } from '@/api/misc'
import { Banner, Loading, Toggle } from '@/components/ui'
import { cn } from '@/lib/cn'
import { toast } from '@/lib/toast'

/** 编辑器最大递归深度：更深的嵌套以只读 JSON 展示。 */
const MAX_DEPTH = 3

/** 节的中文名（后端 schema title 的兜底 + 补充说明）。 */
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

  const sectionNames = useMemo(() => Object.keys(schema.data ?? {}), [schema.data])
  const currentSection = active || sectionNames[0] || ''

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
      toast.ok(`「${sectionTitle(currentSection)}」已保存`)
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
                    <span className="truncate">{sectionTitle(name, schema.data?.[name]?.title)}</span>
                    {isDirty ? <span className="dot dot-warn" title="有未保存修改" /> : null}
                  </button>
                )
              })}
            </div>
          </nav>

          {/* 当前节编辑器 */}
          <div className="card" style={{ flex: 1, minWidth: 0 }}>
            <div className="card-head">
              <h3>{sectionTitle(currentSection, schema.data?.[currentSection]?.title)}</h3>
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
      ) : null}
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
          {entries.map(([key, val]) => (
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

