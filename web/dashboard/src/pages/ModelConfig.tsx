/**
 * 模型配置 页 — 模型 / 服务商 / 任务路由 三视图管理。
 * 对齐后端：GET /models/list、GET /models/providers、GET /models/task-config（查询视图）；
 * 写入统一经 GET /config/model + POST /config/model（整份保存，api/models.ts 的 saveModelSection）。
 */

import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Boxes, KeyRound, Pencil, Plus, RefreshCw, Trash2, Workflow } from 'lucide-react'

import { getModelConfig, saveModelConfig, type ConfigData } from '@/api/config'
import { getModels, getProviders, getTaskConfigs, type ModelInfo, type ProviderInfo, type TaskConfig } from '@/api/models'
import { Banner, ConfirmModal, Empty, Field, Loading, Modal, SelectInput, TextInput, Toggle } from '@/components/ui'
import { cn } from '@/lib/cn'
import { formatNumber } from '@/lib/format'
import { toast } from '@/lib/toast'

type TabKey = 'models' | 'providers' | 'tasks'

const TABS: Array<{ key: TabKey; label: string; icon: typeof Boxes }> = [
  { key: 'models', label: '模型', icon: Boxes },
  { key: 'providers', label: '服务商', icon: KeyRound },
  { key: 'tasks', label: '任务路由', icon: Workflow },
]

export function ModelConfigPage() {
  const [tab, setTab] = useState<TabKey>('models')
  const models = useQuery({ queryKey: ['models-list'], queryFn: getModels })
  const providers = useQuery({ queryKey: ['providers-list'], queryFn: getProviders })
  const tasks = useQuery({ queryKey: ['tasks-config'], queryFn: getTaskConfigs })

  const refreshing = models.isFetching || providers.isFetching || tasks.isFetching

  return (
    <div className="page page-wide">
      <div className="page-head">
        <div>
          <h1>模型配置</h1>
          <p className="page-sub">model_config.toml · 模型 / 服务商 / 任务路由（保存后即时生效）</p>
        </div>
        <div className="page-actions">
          <button
            className="btn btn-secondary btn-sm"
            disabled={refreshing}
            onClick={() => {
              void models.refetch()
              void providers.refetch()
              void tasks.refetch()
            }}
          >
            {refreshing ? <span className="spin" /> : <RefreshCw />} 刷新
          </button>
        </div>
      </div>

      <div className="tabs" role="tablist">
        {TABS.map(({ key, label, icon: Icon }) => (
          <button
            key={key}
            type="button"
            role="tab"
            aria-selected={tab === key}
            className={cn('tab', tab === key && 'active')}
            onClick={() => setTab(key)}
          >
            <Icon size={14} style={{ verticalAlign: -2, marginRight: 4 }} />
            {label}
          </button>
        ))}
      </div>

      {models.isPending || providers.isPending || tasks.isPending ? (
        <Loading text="加载模型配置…" />
      ) : tab === 'models' ? (
        <ModelsTab models={models.data ?? []} providers={providers.data ?? []} />
      ) : tab === 'providers' ? (
        <ProvidersTab providers={providers.data ?? []} />
      ) : (
        <TasksTab tasks={tasks.data ?? []} models={models.data ?? []} />
      )}
    </div>
  )
}

// ══════════════════════════ 模型 ══════════════════════════

interface ModelDraft {
  name: string
  model_identifier: string
  api_provider: string
  price_in: string
  price_out: string
  visual: boolean
  force_stream_mode: boolean
}

function draftOf(m: ModelInfo): ModelDraft {
  return {
    name: m.name,
    model_identifier: m.model_identifier,
    api_provider: m.api_provider,
    price_in: String(m.price_in ?? 0),
    price_out: String(m.price_out ?? 0),
    visual: Boolean(m.visual),
    force_stream_mode: Boolean(m.force_stream_mode),
  }
}

function ModelsTab({ models, providers }: { models: ModelInfo[]; providers: ProviderInfo[] }) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState<{ draft: ModelDraft; original: string } | null>(null)
  const [isNew, setIsNew] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<ModelInfo | null>(null)
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState('')

  const openNew = () => {
    setIsNew(true)
    setFormError('')
    setEditing({
      original: '',
      draft: {
        name: '',
        model_identifier: '',
        api_provider: providers[0]?.name ?? '',
        price_in: '0',
        price_out: '0',
        visual: false,
        force_stream_mode: false,
      },
    })
  }

  const openEdit = (m: ModelInfo) => {
    setIsNew(false)
    setFormError('')
    setEditing({ original: m.name, draft: draftOf(m) })
  }

  const validate = (d: ModelDraft): string => {
    if (!d.name.trim()) return '模型名称不能为空'
    if (!d.model_identifier.trim()) return '模型标识不能为空'
    if (!d.api_provider.trim()) return '必须选择服务商'
    if (Number.isNaN(Number(d.price_in)) || Number.isNaN(Number(d.price_out))) return '价格必须是数字'
    return ''
  }

  const save = async () => {
    if (!editing) return
    const message = validate(editing.draft)
    if (message) {
      setFormError(message)
      return
    }
    setSaving(true)
    try {
      const config = await getModelConfig()
      const list = Array.isArray(config.models) ? [...(config.models as Array<Record<string, unknown>>)] : []
      const record: Record<string, unknown> = {
        name: editing.draft.name.trim(),
        model_identifier: editing.draft.model_identifier.trim(),
        api_provider: editing.draft.api_provider,
        price_in: Number(editing.draft.price_in) || 0,
        price_out: Number(editing.draft.price_out) || 0,
        visual: editing.draft.visual,
        force_stream_mode: editing.draft.force_stream_mode,
      }
      // 保留 extra_params 等未在表单中的字段
      const existing = !isNew ? list.find((m) => m.name === editing.original) : undefined
      const merged = existing ? { ...existing, ...record } : record

      // 重命名语义：替换 original 位置的记录；若新名与其它记录冲突则报错（避免两条同名）
      const nameConflict =
        editing.draft.name.trim() !== editing.original &&
        list.some((m) => m.name === editing.draft.name.trim())
      if (nameConflict) {
        setFormError(`已存在同名模型「${editing.draft.name.trim()}」`)
        setSaving(false)
        return
      }
      const nextList = isNew
        ? [...list, merged]
        : list.map((m) => (m.name === editing.original ? merged : m))

      // 重命名后同步任务引用（model_task_config 中指向旧名的条目改指新名）
      let nextTaskConfig = config.model_task_config
      if (!isNew && editing.original !== record.name) {
        const tc =
          config.model_task_config && typeof config.model_task_config === 'object'
            ? ({ ...(config.model_task_config as ConfigData) })
            : {}
        for (const key of Object.keys(tc)) {
          const entry = tc[key]
          if (entry && typeof entry === 'object') {
            const t = entry as Record<string, unknown>
            if (Array.isArray(t.model_list)) {
              tc[key] = {
                ...t,
                model_list: (t.model_list as unknown[]).map((n) => (n === editing.original ? record.name : n)),
              }
            }
          }
        }
        nextTaskConfig = tc
      }

      await saveModelConfig({
        ...config,
        models: nextList,
        ...(nextTaskConfig ? { model_task_config: nextTaskConfig } : {}),
      })
      toast.ok(`模型「${record.name}」已保存`)
      setEditing(null)
      await qc.invalidateQueries({ queryKey: ['models-list'] })
      await qc.invalidateQueries({ queryKey: ['tasks-config'] })
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  const remove = async () => {
    const target = pendingDelete
    if (!target) return
    setSaving(true)
    try {
      const config = await getModelConfig()
      const list = Array.isArray(config.models) ? (config.models as Array<Record<string, unknown>>).filter((m) => m.name !== target.name) : []
      await saveModelConfig({ ...config, models: list })
      toast.ok(`模型「${target.name}」已删除`)
      setPendingDelete(null)
      await qc.invalidateQueries({ queryKey: ['models-list'] })
      await qc.invalidateQueries({ queryKey: ['tasks-config'] })
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '删除失败')
    } finally {
      setSaving(false)
    }
  }

  const referencedBy = (m: ModelInfo): string[] => m.tasks ?? []

  return (
    <div className="card">
      <div className="card-head">
        <h3>模型</h3>
        <span className="muted-xs">{models.length} 个模型</span>
        <span className="spacer" />
        <button className="btn btn-sm" onClick={openNew} disabled={providers.length === 0}
          title={providers.length === 0 ? '请先在「服务商」页添加服务商' : undefined}>
          <Plus /> 新增模型
        </button>
      </div>
      {providers.length === 0 ? (
        <div className="card-body">
          <Banner kind="warn">还没有服务商，请先到「服务商」页签添加 API 提供商。</Banner>
        </div>
      ) : models.length === 0 ? (
        <Empty icon={<Boxes />} text="暂无模型，点击右上角新增" />
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>名称</th>
                <th>标识</th>
                <th>服务商</th>
                <th className="num">输入价</th>
                <th className="num">输出价</th>
                <th>视觉</th>
                <th>任务引用</th>
                <th style={{ width: 110 }} />
              </tr>
            </thead>
            <tbody>
              {models.map((m) => (
                <tr key={m.name}>
                  <td style={{ fontWeight: 500 }}>{m.name}</td>
                  <td className="mono">{m.model_identifier}</td>
                  <td>{m.api_provider}</td>
                  <td className="num">{m.price_in}</td>
                  <td className="num">{m.price_out}</td>
                  <td>{m.visual ? <span className="badge badge-ok">视觉</span> : <span className="muted-xs">—</span>}</td>
                  <td>
                    {referencedBy(m).length === 0 ? (
                      <span className="muted-xs">未引用</span>
                    ) : (
                      <span className="badge">{referencedBy(m).join(' / ')}</span>
                    )}
                  </td>
                  <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                    <button className="btn btn-ghost btn-sm" title="编辑" onClick={() => openEdit(m)}>
                      <Pencil />
                    </button>
                    <button className="btn btn-ghost btn-sm" title="删除" onClick={() => setPendingDelete(m)}>
                      <Trash2 />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* 编辑/新增 Modal */}
      <Modal
        open={editing !== null}
        title={isNew ? '新增模型' : `编辑模型：${editing?.original ?? ''}`}
        onClose={() => setEditing(null)}
        footer={
          <>
            <button className="btn btn-secondary btn-sm" onClick={() => setEditing(null)} disabled={saving}>
              取消
            </button>
            <button className="btn btn-sm" onClick={() => void save()} disabled={saving}>
              {saving ? <span className="spin" /> : null} 保存
            </button>
          </>
        }
      >
        {editing ? (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
            {formError ? <Banner kind="err">{formError}</Banner> : null}
            <Field label="模型名称（配置内唯一标识）">
              <TextInput
                value={editing.draft.name}
                onChange={(e) => setEditing({ ...editing, draft: { ...editing.draft, name: e.target.value } })}
                placeholder="默认模型"
                invalid={formError.includes('名称')}
              />
            </Field>
            <Field label="模型标识（发给 API 的 model 参数）">
              <TextInput
                value={editing.draft.model_identifier}
                onChange={(e) =>
                  setEditing({ ...editing, draft: { ...editing.draft, model_identifier: e.target.value } })
                }
                placeholder="gpt-4o-mini"
              />
            </Field>
            <Field label="服务商">
              <SelectInput
                value={editing.draft.api_provider}
                onChange={(e) => setEditing({ ...editing, draft: { ...editing.draft, api_provider: e.target.value } })}
              >
                {providers.map((p) => (
                  <option key={p.name} value={p.name}>
                    {p.name}
                  </option>
                ))}
              </SelectInput>
            </Field>
            <div style={{ display: 'flex', gap: 12 }}>
              <Field label="输入价格">
                <TextInput
                  type="number"
                  step="any"
                  value={editing.draft.price_in}
                  onChange={(e) => setEditing({ ...editing, draft: { ...editing.draft, price_in: e.target.value } })}
                />
              </Field>
              <Field label="输出价格">
                <TextInput
                  type="number"
                  step="any"
                  value={editing.draft.price_out}
                  onChange={(e) => setEditing({ ...editing, draft: { ...editing.draft, price_out: e.target.value } })}
                />
              </Field>
            </div>
            <Toggle
              checked={editing.draft.visual}
              onChange={(v) => setEditing({ ...editing, draft: { ...editing.draft, visual: v } })}
              label="支持视觉（图片输入）"
            />
            <Toggle
              checked={editing.draft.force_stream_mode}
              onChange={(v) => setEditing({ ...editing, draft: { ...editing.draft, force_stream_mode: v } })}
              label="强制流式"
            />
          </div>
        ) : null}
      </Modal>

      <ConfirmModal
        open={pendingDelete !== null}
        title="删除模型"
        danger
        confirmText="删除"
        loading={saving}
        message={
          pendingDelete
            ? `确定删除「${pendingDelete.name}」吗？${
                referencedBy(pendingDelete).length > 0
                  ? `该模型正被任务 ${referencedBy(pendingDelete).join('/')} 引用，删除后对应任务将回退到链上其他模型。`
                  : '该模型未被任务引用。'
              }`
            : undefined
        }
        onConfirm={() => void remove()}
        onClose={() => {
          if (!saving) setPendingDelete(null)
        }}
      />
    </div>
  )
}

// ══════════════════════════ 服务商 ══════════════════════════

interface ProviderDraft {
  name: string
  base_url: string
  client_type: string
  max_retry: string
  timeout: string
  retry_interval: string
  api_key: string
}

function ProvidersTab({ providers }: { providers: ProviderInfo[] }) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState<{ draft: ProviderDraft; original: string } | null>(null)
  const [isNew, setIsNew] = useState(false)
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState('')

  const openNew = () => {
    setIsNew(true)
    setFormError('')
    setEditing({
      original: '',
      draft: { name: '', base_url: '', client_type: 'openai', max_retry: '3', timeout: '120', retry_interval: '5', api_key: '' },
    })
  }

  const openEdit = (p: ProviderInfo) => {
    setIsNew(false)
    setFormError('')
    setEditing({
      original: p.name,
      draft: {
        name: p.name,
        base_url: p.base_url,
        client_type: p.client_type || 'openai',
        max_retry: String(p.max_retry ?? 3),
        timeout: String(p.timeout ?? 120),
        retry_interval: String(p.retry_interval ?? 5),
        api_key: '', // 密钥不回显；留空保留旧值
      },
    })
  }

  const validate = (d: ProviderDraft): string => {
    if (!d.name.trim()) return '服务商名称不能为空'
    if (!d.base_url.trim()) return 'Base URL 不能为空'
    if (!/^https?:\/\//.test(d.base_url.trim())) return 'Base URL 需以 http(s):// 开头'
    if (Number.isNaN(Number(d.max_retry)) || Number.isNaN(Number(d.timeout)) || Number.isNaN(Number(d.retry_interval)))
      return '重试/超时必须是数字'
    return ''
  }

  const save = async () => {
    if (!editing) return
    const message = validate(editing.draft)
    if (message) {
      setFormError(message)
      return
    }
    setSaving(true)
    try {
      const config = await getModelConfig()
      const list = Array.isArray(config.api_providers)
        ? [...(config.api_providers as Array<Record<string, unknown>>)]
        : []
      const existing = !isNew ? list.find((p) => p.name === editing.original) : undefined
      const record: Record<string, unknown> = {
        name: editing.draft.name.trim(),
        base_url: editing.draft.base_url.trim(),
        client_type: editing.draft.client_type,
        max_retry: Number(editing.draft.max_retry) || 3,
        timeout: Number(editing.draft.timeout) || 120,
        retry_interval: Number(editing.draft.retry_interval) || 5,
      }
      // api_key：填写才更新；留空保留旧值；新增必须填写
      if (editing.draft.api_key.trim()) record.api_key = editing.draft.api_key.trim()
      else if (isNew) {
        setFormError('新增服务商必须填写 API Key')
        setSaving(false)
        return
      } else if (existing && typeof existing.api_key === 'string') {
        record.api_key = existing.api_key
      }
      const merged = existing ? { ...existing, ...record } : record

      // 重命名语义：替换 original 位置的记录；新名与其它记录冲突则报错。
      // 注意：重命名服务商时，引用旧名的 models.api_provider 一并同步，否则模型会指向不存在的服务商。
      const nameConflict =
        !isNew && editing.draft.name.trim() !== editing.original &&
        list.some((p) => p.name === editing.draft.name.trim())
      if (nameConflict) {
        setFormError(`已存在同名服务商「${editing.draft.name.trim()}」`)
        setSaving(false)
        return
      }
      const finalList = isNew
        ? [...list, merged]
        : list.map((p) => (p.name === editing.original ? merged : p))

      let models = config.models
      if (!isNew && editing.original !== record.name) {
        const modelList = Array.isArray(config.models) ? [...(config.models as Array<Record<string, unknown>>)] : []
        models = modelList.map((m) =>
          m.api_provider === editing.original ? { ...m, api_provider: record.name } : m
        )
      }

      await saveModelConfig({ ...config, api_providers: finalList, models })
      toast.ok(`服务商「${record.name}」已保存`)
      setEditing(null)
      await qc.invalidateQueries({ queryKey: ['providers-list'] })
      await qc.invalidateQueries({ queryKey: ['models-list'] })
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="card">
      <div className="card-head">
        <h3>服务商</h3>
        <span className="muted-xs">{providers.length} 个 API 提供商</span>
        <span className="spacer" />
        <button className="btn btn-sm" onClick={openNew}>
          <Plus /> 新增服务商
        </button>
      </div>
      {providers.length === 0 ? (
        <Empty icon={<KeyRound />} text="暂无服务商" />
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>名称</th>
                <th>Base URL</th>
                <th>类型</th>
                <th className="num">重试</th>
                <th className="num">超时(s)</th>
                <th>API Key</th>
                <th style={{ width: 60 }} />
              </tr>
            </thead>
            <tbody>
              {providers.map((p) => (
                <tr key={p.name}>
                  <td style={{ fontWeight: 500 }}>{p.name}</td>
                  <td className="mono truncate" style={{ maxWidth: 260 }} title={p.base_url}>
                    {p.base_url}
                  </td>
                  <td>{p.client_type}</td>
                  <td className="num">{p.max_retry}</td>
                  <td className="num">{p.timeout}</td>
                  <td>
                    {p.has_api_key ? <span className="badge badge-ok">已配置</span> : <span className="badge">未配置</span>}
                  </td>
                  <td style={{ textAlign: 'right' }}>
                    <button className="btn btn-ghost btn-sm" title="编辑" onClick={() => openEdit(p)}>
                      <Pencil />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <Modal
        open={editing !== null}
        title={isNew ? '新增服务商' : `编辑服务商：${editing?.original ?? ''}`}
        onClose={() => setEditing(null)}
        footer={
          <>
            <button className="btn btn-secondary btn-sm" onClick={() => setEditing(null)} disabled={saving}>
              取消
            </button>
            <button className="btn btn-sm" onClick={() => void save()} disabled={saving}>
              {saving ? <span className="spin" /> : null} 保存
            </button>
          </>
        }
      >
        {editing ? (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
            {formError ? <Banner kind="err">{formError}</Banner> : null}
            <Field label="名称（配置内唯一标识）">
              <TextInput
                value={editing.draft.name}
                onChange={(e) => setEditing({ ...editing, draft: { ...editing.draft, name: e.target.value } })}
                placeholder="my-provider"
                invalid={formError.includes('名称')}
              />
            </Field>
            <Field label="Base URL">
              <TextInput
                value={editing.draft.base_url}
                onChange={(e) => setEditing({ ...editing, draft: { ...editing.draft, base_url: e.target.value } })}
                placeholder="https://api.example.com/v1"
              />
            </Field>
            <div style={{ display: 'flex', gap: 12 }}>
              <Field label="client_type">
                <SelectInput
                  value={editing.draft.client_type}
                  onChange={(e) => setEditing({ ...editing, draft: { ...editing.draft, client_type: e.target.value } })}
                >
                  <option value="openai">openai</option>
                </SelectInput>
              </Field>
              <Field label="最大重试">
                <TextInput
                  type="number"
                  value={editing.draft.max_retry}
                  onChange={(e) => setEditing({ ...editing, draft: { ...editing.draft, max_retry: e.target.value } })}
                />
              </Field>
              <Field label="超时(秒)">
                <TextInput
                  type="number"
                  value={editing.draft.timeout}
                  onChange={(e) => setEditing({ ...editing, draft: { ...editing.draft, timeout: e.target.value } })}
                />
              </Field>
              <Field label="重试间隔(秒)">
                <TextInput
                  type="number"
                  value={editing.draft.retry_interval}
                  onChange={(e) => setEditing({ ...editing, draft: { ...editing.draft, retry_interval: e.target.value } })}
                />
              </Field>
            </div>
            <Field label="API Key" hint={isNew ? '必填' : '留空保留已配置的密钥；填写则覆盖'}>
              <TextInput
                type="password"
                autoComplete="new-password"
                value={editing.draft.api_key}
                onChange={(e) => setEditing({ ...editing, draft: { ...editing.draft, api_key: e.target.value } })}
                placeholder={isNew ? 'sk-…' : '••••••••'}
              />
            </Field>
          </div>
        ) : null}
      </Modal>
    </div>
  )
}

// ══════════════════════════ 任务路由 ══════════════════════════

function TasksTab({ tasks, models }: { tasks: TaskConfig[]; models: ModelInfo[] }) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState<{ task: string; selected: string[] } | null>(null)
  const [saving, setSaving] = useState(false)

  const openEdit = (t: TaskConfig) => {
    setEditing({ task: t.task, selected: [...t.model_list] })
  }

  const toggle = (name: string) => {
    if (!editing) return
    setEditing({
      ...editing,
      selected: editing.selected.includes(name)
        ? editing.selected.filter((n) => n !== name)
        : [...editing.selected, name],
    })
  }

  const save = async () => {
    if (!editing) return
    if (editing.selected.length === 0) {
      toast.err('至少选择一个模型')
      return
    }
    setSaving(true)
    try {
      const config = await getModelConfig()
      const taskConfig =
        config.model_task_config && typeof config.model_task_config === 'object'
          ? (config.model_task_config as ConfigData)
          : {}
      const currentTask =
        taskConfig[editing.task] && typeof taskConfig[editing.task] === 'object'
          ? (taskConfig[editing.task] as Record<string, unknown>)
          : {}
      const nextTaskConfig: ConfigData = {
        ...taskConfig,
        [editing.task]: { ...currentTask, model_list: editing.selected },
      }
      await saveModelConfig({ ...config, model_task_config: nextTaskConfig })
      toast.ok(`任务「${editing.task}」路由已更新`)
      setEditing(null)
      await qc.invalidateQueries({ queryKey: ['tasks-config'] })
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="card">
      <div className="card-head">
        <h3>任务路由</h3>
        <span className="muted-xs">每个任务按模型列表顺序尝试（随机选择 + 超时回退）</span>
      </div>
      {tasks.length === 0 ? (
        <Empty icon={<Workflow />} text="暂无任务配置" />
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>任务</th>
                <th>模型列表</th>
                <th className="num">温度</th>
                <th className="num">Max Tokens</th>
                <th>回退链</th>
                <th style={{ width: 60 }} />
              </tr>
            </thead>
            <tbody>
              {tasks.map((t) => (
                <tr key={t.task}>
                  <td style={{ fontWeight: 500 }}>{t.task}</td>
                  <td>
                    <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                      {t.model_list.length === 0 ? (
                        <span className="badge badge-warn">空（走回退链）</span>
                      ) : (
                        t.model_list.map((m) => (
                          <span key={m} className="badge">
                            {m}
                          </span>
                        ))
                      )}
                    </div>
                  </td>
                  <td className="num">{t.temperature}</td>
                  <td className="num">{formatNumber(t.max_tokens)}</td>
                  <td className="muted-xs">{t.fallback_chain?.length ? t.fallback_chain.join(' → ') : '—'}</td>
                  <td style={{ textAlign: 'right' }}>
                    <button className="btn btn-ghost btn-sm" title="编辑" onClick={() => openEdit(t)} disabled={models.length === 0}>
                      <Pencil />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <Modal
        open={editing !== null}
        title={`编辑任务路由：${editing?.task ?? ''}`}
        onClose={() => setEditing(null)}
        footer={
          <>
            <button className="btn btn-secondary btn-sm" onClick={() => setEditing(null)} disabled={saving}>
              取消
            </button>
            <button className="btn btn-sm" onClick={() => void save()} disabled={saving}>
              {saving ? <span className="spin" /> : null} 保存
            </button>
          </>
        }
      >
        {editing ? (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            <p className="field-hint">勾选该任务使用的模型（按住顺序即尝试顺序）：</p>
            {models.map((m) => {
              const idx = editing.selected.indexOf(m.name)
              return (
                <label
                  key={m.name}
                  style={{
                    display: 'flex',
                    alignItems: 'center',
                    gap: 8,
                    padding: '6px 10px',
                    border: `1px solid ${idx >= 0 ? 'var(--fg)' : 'var(--border)'}`,
                    borderRadius: 'var(--radius-sm)',
                    cursor: 'pointer',
                  }}
                >
                  <input
                    type="checkbox"
                    checked={idx >= 0}
                    onChange={() => toggle(m.name)}
                    style={{ accentColor: 'var(--fg)' }}
                  />
                  <span className="sm" style={{ fontWeight: 500 }}>{m.name}</span>
                  <span className="mono muted-xs">{m.model_identifier}</span>
                  {idx >= 0 ? <span className="spacer" /> : null}
                  {idx >= 0 ? <span className="badge">第 {idx + 1} 顺位</span> : null}
                </label>
              )
            })}
          </div>
        ) : null}
      </Modal>
    </div>
  )
}
