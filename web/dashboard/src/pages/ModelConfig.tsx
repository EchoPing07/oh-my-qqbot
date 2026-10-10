/**
 * 模型配置 页 — 以「服务商」为单位管理模型（服务商不可删除）。
 *
 * 对齐后端：
 * - GET /models/providers   服务商清单（含各自名下的模型，服务商不可删除）
 * - GET /models/task-config 任务路由（保留，供查看/调整各任务用哪些模型）
 * - 写入统一经 GET /config/model + POST /config/model（整份保存）
 *
 * 结构变化：服务商卡片即管理单元 —— 卡内直接增删该服务商的模型；
 * 服务商只可编辑不可删除（保证 models[].api_provider 永不悬空）。
 */

import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Boxes,
  Eye,
  EyeOff,
  KeyRound,
  Pencil,
  Plus,
  RefreshCw,
  Trash2,
  Workflow,
} from 'lucide-react'

import { getModelConfig, saveModelConfig, type ConfigData } from '@/api/config'
import {
  getProviders,
  getTaskConfigs,
  type ProviderInfo,
  type ProviderModelInfo,
  type TaskConfig,
} from '@/api/models'
import { Banner, ConfirmModal, Empty, Field, Loading, Modal, SelectInput, TextInput, Toggle } from '@/components/ui'
import { cn } from '@/lib/cn'
import { formatNumber } from '@/lib/format'
import { toast } from '@/lib/toast'

/** 新增服务商的默认参数（对齐 config.defaultAPIProviderFields）。 */
const DEFAULT_PROVIDER = {
  client_type: 'openai',
  max_retry: '3',
  timeout: '120',
  retry_interval: '4',
}

export function ModelConfigPage() {
  const providers = useQuery({ queryKey: ['providers-list'], queryFn: getProviders })
  const tasks = useQuery({ queryKey: ['tasks-config'], queryFn: getTaskConfigs })

  const refreshing = providers.isFetching || tasks.isFetching
  const error = providers.error ?? tasks.error

  return (
    <div className="page page-wide">
      <div className="page-head">
        <div>
          <h1>模型配置</h1>
          <p className="page-sub">以服务商为单位管理模型与密钥 · 服务商不可删除</p>
        </div>
        <div className="page-actions">
          <button
            className="btn btn-secondary btn-sm"
            disabled={refreshing}
            onClick={() => {
              void providers.refetch()
              void tasks.refetch()
            }}
          >
            {refreshing ? <span className="spin" /> : <RefreshCw />} 刷新
          </button>
        </div>
      </div>

      {error ? (
        <Banner kind="err">{error instanceof Error ? error.message : '模型配置加载失败'}</Banner>
      ) : null}

      {providers.isPending || tasks.isPending ? (
        <Loading text="加载模型配置…" />
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <ProvidersPanel providers={providers.data ?? []} />
          <TasksPanel tasks={tasks.data ?? []} providers={providers.data ?? []} />
        </div>
      )}
    </div>
  )
}

// ══════════════════════════ 服务商（管理单元） ══════════════════════════

function ProvidersPanel({ providers }: { providers: ProviderInfo[] }) {
  if (providers.length === 0) {
    return <Empty icon={<KeyRound />} text="暂无服务商（配置可能未生成）" />
  }
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      {providers.map((p) => (
        <ProviderCard key={p.name} provider={p} />
      ))}
    </div>
  )
}

/** 单个服务商卡片：头部信息 + 卡内模型表 + 新增/编辑/删除模型。 */
function ProviderCard({ provider }: { provider: ProviderInfo }) {
  const qc = useQueryClient()
  const [editingModel, setEditingModel] = useState<{ draft: ModelDraft; original: string } | null>(null)
  const [isNewModel, setIsNewModel] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<ProviderModelInfo | null>(null)
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState('')

  // ── 服务商编辑 ──
  const [editingProvider, setEditingProvider] = useState(false)
  const [providerDraft, setProviderDraft] = useState<ProviderDraft>(() => draftOfProvider(provider))
  const [providerError, setProviderError] = useState('')
  const [revealKey, setRevealKey] = useState(false)

  const openEditProvider = () => {
    setProviderError('')
    setProviderDraft(draftOfProvider(provider))
    setEditingProvider(true)
  }

  const saveProvider = async () => {
    const message = validateProvider(providerDraft)
    if (message) {
      setProviderError(message)
      return
    }
    setSaving(true)
    try {
      const config = await getModelConfig()
      const list = Array.isArray(config.api_providers)
        ? [...(config.api_providers as Array<Record<string, unknown>>)]
        : []
      const idx = list.findIndex((p) => p.name === provider.name)
      if (idx < 0) {
        setProviderError('配置中找不到该服务商，请刷新后重试')
        setSaving(false)
        return
      }
      const record: Record<string, unknown> = {
        name: providerDraft.name.trim(),
        base_url: providerDraft.base_url.trim(),
        client_type: providerDraft.client_type,
        max_retry: Number(providerDraft.max_retry) || 3,
        timeout: Number(providerDraft.timeout) || 120,
        retry_interval: Number(providerDraft.retry_interval) || 4,
      }
      // 密钥：填写才覆盖，留空保留原值
      if (providerDraft.api_key.trim()) record.api_key = providerDraft.api_key.trim()
      else if (typeof list[idx].api_key === 'string') record.api_key = list[idx].api_key

      const newName = providerDraft.name.trim()
      if (newName !== provider.name && list.some((p) => p.name === newName)) {
        setProviderError(`已存在同名服务商「${newName}」`)
        setSaving(false)
        return
      }
      list[idx] = { ...list[idx], ...record }

      // 改名时同步 models[].api_provider，避免模型指向不存在的服务商
      let models = config.models
      if (newName !== provider.name) {
        const modelList = Array.isArray(config.models)
          ? [...(config.models as Array<Record<string, unknown>>)]
          : []
        models = modelList.map((m) => (m.api_provider === provider.name ? { ...m, api_provider: newName } : m))
      }

      await saveModelConfig({ ...config, api_providers: list, models })
      toast.ok(`服务商「${record.name}」已保存`)
      setEditingProvider(false)
      await qc.invalidateQueries({ queryKey: ['providers-list'] })
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  // ── 卡内模型 ──

  const openNewModel = () => {
    setIsNewModel(true)
    setFormError('')
    setEditingModel({
      original: '',
      draft: { name: '', model_identifier: '', price_in: '0', price_out: '0', visual: false },
    })
  }

  const openEditModel = (m: ProviderModelInfo) => {
    setIsNewModel(false)
    setFormError('')
    setEditingModel({
      original: m.name,
      draft: {
        name: m.name,
        model_identifier: m.model_identifier,
        price_in: String(m.price_in ?? 0),
        price_out: String(m.price_out ?? 0),
        visual: Boolean(m.visual),
      },
    })
  }

  const validateModel = (d: ModelDraft): string => {
    if (!d.name.trim()) return '模型名称不能为空'
    if (!d.model_identifier.trim()) return '模型标识不能为空'
    if (Number.isNaN(Number(d.price_in)) || Number.isNaN(Number(d.price_out))) return '价格必须是数字'
    return ''
  }

  const saveModel = async () => {
    if (!editingModel) return
    const message = validateModel(editingModel.draft)
    if (message) {
      setFormError(message)
      return
    }
    setSaving(true)
    try {
      const config = await getModelConfig()
      const list = Array.isArray(config.models)
        ? [...(config.models as Array<Record<string, unknown>>)]
        : []
      const record: Record<string, unknown> = {
        name: editingModel.draft.name.trim(),
        model_identifier: editingModel.draft.model_identifier.trim(),
        api_provider: provider.name,
        price_in: Number(editingModel.draft.price_in) || 0,
        price_out: Number(editingModel.draft.price_out) || 0,
        visual: editingModel.draft.visual,
      }
      const existing = !isNewModel ? list.find((m) => m.name === editingModel.original) : undefined
      const merged = existing ? { ...existing, ...record } : record

      const nameConflict =
        editingModel.draft.name.trim() !== editingModel.original &&
        list.some((m) => m.name === editingModel.draft.name.trim())
      if (nameConflict) {
        setFormError(`已存在同名模型「${editingModel.draft.name.trim()}」`)
        setSaving(false)
        return
      }
      const nextList = isNewModel
        ? [...list, merged]
        : list.map((m) => (m.name === editingModel.original ? merged : m))

      // 改名同步任务引用，避免 model_task_config 指向不存在的模型名
      let nextTaskConfig = config.model_task_config
      if (!isNewModel && editingModel.original !== record.name) {
        const tc =
          config.model_task_config && typeof config.model_task_config === 'object'
            ? { ...(config.model_task_config as ConfigData) }
            : {}
        for (const key of Object.keys(tc)) {
          const entry = tc[key]
          if (entry && typeof entry === 'object') {
            const t = entry as Record<string, unknown>
            if (Array.isArray(t.model_list)) {
              tc[key] = {
                ...t,
                model_list: (t.model_list as unknown[]).map((n) =>
                  n === editingModel.original ? record.name : n
                ),
              }
            }
          }
        }
        nextTaskConfig = tc
      }

      await saveModelConfig({ ...config, models: nextList, ...(nextTaskConfig ? { model_task_config: nextTaskConfig } : {}) })
      toast.ok(`模型「${record.name}」已保存`)
      setEditingModel(null)
      await qc.invalidateQueries({ queryKey: ['providers-list'] })
      await qc.invalidateQueries({ queryKey: ['tasks-config'] })
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  const removeModel = async () => {
    const target = pendingDelete
    if (!target) return
    setSaving(true)
    try {
      const config = await getModelConfig()
      const list = Array.isArray(config.models)
        ? (config.models as Array<Record<string, unknown>>).filter((m) => m.name !== target.name)
        : []
      await saveModelConfig({ ...config, models: list })
      toast.ok(`模型「${target.name}」已删除`)
      setPendingDelete(null)
      await qc.invalidateQueries({ queryKey: ['providers-list'] })
      await qc.invalidateQueries({ queryKey: ['tasks-config'] })
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '删除失败')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="card">
      <div className="card-head">
        <h3>{provider.name}</h3>
        <span className={cn('badge', provider.has_api_key ? 'badge-ok' : 'badge-warn')}>
          {provider.has_api_key ? '密钥已配置' : '密钥未配置'}
        </span>
        <span className="muted-xs truncate" style={{ maxWidth: 320 }} title={provider.base_url}>
          {provider.base_url}
        </span>
        <span className="spacer" />
        <span className="muted-xs">
          {provider.models.length} 个模型 · 重试 {provider.max_retry} · 超时 {provider.timeout}s
        </span>
        <button className="btn btn-secondary btn-sm" onClick={openEditProvider} title="编辑服务商">
          <Pencil /> 编辑服务商
        </button>
        <button className="btn btn-sm" onClick={openNewModel}>
          <Plus /> 新增模型
        </button>
      </div>

      {provider.models.length === 0 ? (
        <div className="card-body">
          <Empty icon={<Boxes />} text="该服务商下还没有模型，点「新增模型」添加" />
        </div>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>模型名称</th>
                <th>模型标识</th>
                <th className="num">输入价格</th>
                <th className="num">输出价格</th>
                <th>能力</th>
                <th>任务引用</th>
                <th style={{ width: 120 }} />
              </tr>
            </thead>
            <tbody>
              {provider.models.map((m) => (
                <tr key={m.name}>
                  <td style={{ fontWeight: 500 }}>{m.name}</td>
                  <td className="mono truncate" style={{ maxWidth: 220 }} title={m.model_identifier}>
                    {m.model_identifier}
                  </td>
                  <td className="num">{m.price_in}</td>
                  <td className="num">{m.price_out}</td>
                  <td>{m.visual ? <span className="badge">视觉</span> : <span className="muted-xs">文本</span>}</td>
                  <td>
                    {m.tasks.length === 0 ? (
                      <span className="muted-xs">未引用</span>
                    ) : (
                      <span className="badge" title={m.tasks.join('、')}>
                        {m.tasks.length} 个任务
                      </span>
                    )}
                  </td>
                  <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                    <button className="btn btn-ghost btn-sm" title="编辑模型" onClick={() => openEditModel(m)}>
                      <Pencil />
                    </button>
                    <button className="btn btn-ghost btn-sm" title="删除模型" onClick={() => setPendingDelete(m)}>
                      <Trash2 />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* ── 服务商编辑弹窗 ── */}
      <Modal
        open={editingProvider}
        title={`编辑服务商：${provider.name}`}
        onClose={() => setEditingProvider(false)}
        footer={
          <>
            <button className="btn btn-secondary btn-sm" onClick={() => setEditingProvider(false)} disabled={saving}>
              取消
            </button>
            <button className="btn btn-sm" onClick={() => void saveProvider()} disabled={saving}>
              {saving ? <span className="spin" /> : null} 保存
            </button>
          </>
        }
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          {providerError ? <Banner kind="err">{providerError}</Banner> : null}
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
            <p className="field-hint">服务商不可删除 —— 删除会让引用它的模型失去归属，只能改名或改配置。</p>
          </div>
          <Field label="服务商名称（配置内唯一标识）">
            <TextInput
              value={providerDraft.name}
              onChange={(e) => setProviderDraft({ ...providerDraft, name: e.target.value })}
              invalid={providerError.includes('名称')}
            />
          </Field>
          <Field label="接口地址（Base URL）">
            <TextInput
              value={providerDraft.base_url}
              onChange={(e) => setProviderDraft({ ...providerDraft, base_url: e.target.value })}
              placeholder="https://api.example.com/v1"
            />
          </Field>
          <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap' }}>
            <Field label="客户端类型">
              <SelectInput
                value={providerDraft.client_type}
                onChange={(e) => setProviderDraft({ ...providerDraft, client_type: e.target.value })}
              >
                <option value="openai">OpenAI 兼容</option>
              </SelectInput>
            </Field>
            <Field label="最大重试">
              <TextInput
                type="number"
                value={providerDraft.max_retry}
                onChange={(e) => setProviderDraft({ ...providerDraft, max_retry: e.target.value })}
              />
            </Field>
            <Field label="超时（秒）">
              <TextInput
                type="number"
                value={providerDraft.timeout}
                onChange={(e) => setProviderDraft({ ...providerDraft, timeout: e.target.value })}
              />
            </Field>
            <Field label="重试间隔（秒）">
              <TextInput
                type="number"
                value={providerDraft.retry_interval}
                onChange={(e) => setProviderDraft({ ...providerDraft, retry_interval: e.target.value })}
              />
            </Field>
          </div>
          <Field label="密钥（API Key）" hint="留空保留已配置的密钥；填写则覆盖">
            <div style={{ display: 'flex', gap: 8 }}>
              <TextInput
                type={revealKey ? 'text' : 'password'}
                autoComplete="new-password"
                value={providerDraft.api_key}
                onChange={(e) => setProviderDraft({ ...providerDraft, api_key: e.target.value })}
                placeholder="••••••••"
              />
              <button className="btn btn-secondary btn-sm" onClick={() => setRevealKey(!revealKey)} title={revealKey ? '隐藏' : '显示'}>
                {revealKey ? <EyeOff /> : <Eye />}
              </button>
            </div>
          </Field>
        </div>
      </Modal>

      {/* ── 模型新增/编辑弹窗 ── */}
      <Modal
        open={editingModel !== null}
        title={isNewModel ? `新增模型（服务商：${provider.name}）` : `编辑模型：${editingModel?.original ?? ''}`}
        onClose={() => setEditingModel(null)}
        footer={
          <>
            <button className="btn btn-secondary btn-sm" onClick={() => setEditingModel(null)} disabled={saving}>
              取消
            </button>
            <button className="btn btn-sm" onClick={() => void saveModel()} disabled={saving}>
              {saving ? <span className="spin" /> : null} 保存
            </button>
          </>
        }
      >
        {editingModel ? (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
            {formError ? <Banner kind="err">{formError}</Banner> : null}
            <Field label="模型名称（任务路由引用的标识）">
              <TextInput
                value={editingModel.draft.name}
                onChange={(e) =>
                  setEditingModel({ ...editingModel, draft: { ...editingModel.draft, name: e.target.value } })
                }
                placeholder="例如：对话主力模型"
                invalid={formError.includes('名称')}
              />
            </Field>
            <Field label="模型标识（发给接口的 model 参数）">
              <TextInput
                value={editingModel.draft.model_identifier}
                onChange={(e) =>
                  setEditingModel({
                    ...editingModel,
                    draft: { ...editingModel.draft, model_identifier: e.target.value },
                  })
                }
                placeholder="例如：deepseek-v4-flash"
              />
            </Field>
            <div style={{ display: 'flex', gap: 12 }}>
              <Field label="输入价格（每百万令牌）">
                <TextInput
                  type="number"
                  step="any"
                  value={editingModel.draft.price_in}
                  onChange={(e) =>
                    setEditingModel({ ...editingModel, draft: { ...editingModel.draft, price_in: e.target.value } })
                  }
                />
              </Field>
              <Field label="输出价格（每百万令牌）">
                <TextInput
                  type="number"
                  step="any"
                  value={editingModel.draft.price_out}
                  onChange={(e) =>
                    setEditingModel({ ...editingModel, draft: { ...editingModel.draft, price_out: e.target.value } })
                  }
                />
              </Field>
            </div>
            <Toggle
              checked={editingModel.draft.visual}
              onChange={(v) => setEditingModel({ ...editingModel, draft: { ...editingModel.draft, visual: v } })}
              label="支持视觉（可接收图片输入）"
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
                pendingDelete.tasks.length > 0
                  ? `正被 ${pendingDelete.tasks.length} 个任务引用，删除后相关任务会回退到链上其他模型。`
                  : '该模型未被任务引用。'
              }`
            : undefined
        }
        onConfirm={() => void removeModel()}
        onClose={() => {
          if (!saving) setPendingDelete(null)
        }}
      />
    </div>
  )
}

// ══════════════════════════ 任务路由 ══════════════════════════

/** 任务中文名（后端字段名为英文，页面做汉化映射）。 */
const TASK_LABELS: Record<string, string> = {
  replyer: '回复生成',
  planner: '决策规划',
  memory: '长期记忆',
  mid_memory: '聊天回想',
  utils: '通用小任务',
  learner: '学习任务',
  fast_model: '快速任务',
  emoji: '表情选择',
  vlm: '识图',
  voice: '语音',
  embedding: '文本向量',
  image_embedding: '图片向量',
}

function taskLabel(task: string): string {
  return TASK_LABELS[task] ?? task
}

function TasksPanel({ tasks, providers }: { tasks: TaskConfig[]; providers: ProviderInfo[] }) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState<{ task: string; selected: string[] } | null>(null)
  const [saving, setSaving] = useState(false)

  const allModels = providers.flatMap((p) => p.models)

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
      toast.ok(`任务「${taskLabel(editing.task)}」路由已更新`)
      setEditing(null)
      await qc.invalidateQueries({ queryKey: ['tasks-config'] })
      await qc.invalidateQueries({ queryKey: ['providers-list'] })
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="card">
      <div className="card-head">
        <h3>
          <Workflow size={15} style={{ verticalAlign: -2 }} /> 任务路由
        </h3>
        <span className="muted-xs">每个任务按所选模型顺序尝试（均衡 / 随机 + 超时回退）</span>
      </div>
      {tasks.length === 0 ? (
        <Empty icon={<Workflow />} text="暂无任务配置" />
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>任务</th>
                <th>使用模型</th>
                <th className="num">温度</th>
                <th className="num">最大令牌</th>
                <th>回退链</th>
                <th style={{ width: 60 }} />
              </tr>
            </thead>
            <tbody>
              {tasks.map((t) => (
                <tr key={t.task}>
                  <td style={{ fontWeight: 500 }}>{taskLabel(t.task)}</td>
                  <td>
                    {t.model_list.length === 0 ? (
                      <span className="badge badge-warn">空（走回退链）</span>
                    ) : (
                      <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                        {t.model_list.map((m) => (
                          <span key={m} className="badge">
                            {m}
                          </span>
                        ))}
                      </div>
                    )}
                  </td>
                  <td className="num">{t.temperature}</td>
                  <td className="num">{formatNumber(t.max_tokens)}</td>
                  <td className="muted-xs">
                    {t.fallback_chain?.length ? t.fallback_chain.map(taskLabel).join(' → ') : '—'}
                  </td>
                  <td style={{ textAlign: 'right' }}>
                    <button
                      className="btn btn-ghost btn-sm"
                      title="编辑任务使用的模型"
                      onClick={() => openEdit(t)}
                      disabled={allModels.length === 0}
                    >
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
        title={`编辑任务路由：${editing ? taskLabel(editing.task) : ''}`}
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
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
            <p className="field-hint">勾选该任务使用的模型（勾选顺序即尝试顺序）：</p>
            {providers.map((p) => (
              <div key={p.name} style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
                <span className="label" style={{ marginBottom: 0 }}>
                  {p.name}
                </span>
                {p.models.length === 0 ? (
                  <span className="muted-xs">该服务商下没有模型</span>
                ) : (
                  p.models.map((m) => {
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
                        <span className="sm" style={{ fontWeight: 500 }}>
                          {m.name}
                        </span>
                        <span className="mono muted-xs">{m.model_identifier}</span>
                        <span className="spacer" />
                        {idx >= 0 ? <span className="badge">第 {idx + 1} 顺位</span> : null}
                      </label>
                    )
                  })
                )}
              </div>
            ))}
            {allModels.length === 0 ? <Empty icon={<Boxes />} text="还没有任何模型，请先新增模型" /> : null}
          </div>
        ) : null}
      </Modal>
    </div>
  )
}

// ══════════════════════════ 表单类型与校验 ══════════════════════════

interface ModelDraft {
  name: string
  model_identifier: string
  price_in: string
  price_out: string
  visual: boolean
}

interface ProviderDraft {
  name: string
  base_url: string
  client_type: string
  max_retry: string
  timeout: string
  retry_interval: string
  api_key: string
}

function draftOfProvider(p: ProviderInfo): ProviderDraft {
  return {
    name: p.name,
    base_url: p.base_url,
    client_type: p.client_type || DEFAULT_PROVIDER.client_type,
    max_retry: String(p.max_retry ?? DEFAULT_PROVIDER.max_retry),
    timeout: String(p.timeout ?? DEFAULT_PROVIDER.timeout),
    retry_interval: String(p.retry_interval ?? DEFAULT_PROVIDER.retry_interval),
    api_key: '', // 密钥不回显；留空保留旧值
  }
}

function validateProvider(d: ProviderDraft): string {
  if (!d.name.trim()) return '服务商名称不能为空'
  if (!d.base_url.trim()) return '接口地址不能为空'
  if (!/^https?:\/\//.test(d.base_url.trim())) return '接口地址需以 http(s):// 开头'
  if (
    Number.isNaN(Number(d.max_retry)) ||
    Number.isNaN(Number(d.timeout)) ||
    Number.isNaN(Number(d.retry_interval))
  ) {
    return '重试 / 超时 / 间隔必须是数字'
  }
  return ''
}
