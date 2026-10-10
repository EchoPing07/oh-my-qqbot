/**
 * 首次配置向导 — 两步：基础与人格 → 模型路由。
 * 只读写后端真实存在的配置节（bot / personality / 模型三件套）。
 */

import { useCallback, useEffect, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { CircleCheck } from 'lucide-react'

import { completeSetup, getSetupStatus } from '@/api/auth'
import { getBotSection, saveBotSection } from '@/api/config'
import { getModelConfig, saveModelConfig } from '@/api/config'
import { postAuthTarget } from '@/router'
import { toast } from '@/lib/toast'
import { Loading, SelectInput, TextArea, TextInput } from '@/components/ui'

interface BotBasic {
  nickname: string
  alias_names: string
  platform: string
  qq_account: string
}

interface Personality {
  personality: string
  reply_style: string
}

interface ModelRoute {
  provider_name: string
  base_url: string
  api_key: string
  model_identifier: string
}

const TASK_ORDER = ['replyer', 'planner', 'utils', 'memory', 'vlm', 'embedding'] as const

export function SetupPage() {
  return <SetupWizard />
}

function SetupWizard() {
  const navigate = useNavigate()
  const [step, setStep] = useState(0)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  const [basic, setBasic] = useState<BotBasic>({ nickname: '', alias_names: '', platform: 'qq', qq_account: '' })
  const [personality, setPersonality] = useState<Personality>({ personality: '', reply_style: '' })
  const [model, setModel] = useState<ModelRoute>({ provider_name: '', base_url: '', api_key: '', model_identifier: '' })
  const [skipConfirm, setSkipConfirm] = useState(false)

  useEffect(() => {
    let cancelled = false
    const run = async () => {
      try {
        const status = await getSetupStatus()
        if (!status.authenticated) {
          navigate({ to: '/auth' })
          return
        }
        const [bot, persona] = await Promise.all([getBotSection<Record<string, unknown>>('bot'), getBotSection<Record<string, unknown>>('personality')])
        if (cancelled) return
        setBasic({
          nickname: String(bot.nickname ?? ''),
          alias_names: Array.isArray(bot.alias_names) ? (bot.alias_names as string[]).join(', ') : '',
          platform: String(bot.platform ?? 'qq'),
          qq_account: String(bot.qq_account ?? ''),
        })
        setPersonality({
          personality: String(persona.personality ?? ''),
          reply_style: String(persona.reply_style ?? ''),
        })
        const modelCfg = await getModelConfig()
        if (cancelled) return
        const providers = (modelCfg.api_providers as Array<Record<string, unknown>> | undefined) ?? []
        const first = providers[0] ?? {}
        const models = (modelCfg.models as Array<Record<string, unknown>> | undefined) ?? []
        setModel({
          provider_name: String(first.name ?? ''),
          base_url: String(first.base_url ?? ''),
          api_key: '',
          model_identifier: String(models[0]?.model_identifier ?? ''),
        })
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : '读取配置失败')
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    void run()
    return () => {
      cancelled = true
    }
  }, [navigate])

  const validateStep0 = useCallback((): string => {
    if (!basic.nickname.trim()) return '昵称不能为空'
    if (!personality.personality.trim()) return '人格特征不能为空'
    return ''
  }, [basic.nickname, personality.personality])

  const validateStep1 = useCallback((): string => {
    if (!model.provider_name.trim()) return '服务商名称不能为空'
    if (!model.base_url.trim()) return 'Base URL 不能为空'
    if (!model.model_identifier.trim()) return '模型标识不能为空'
    return ''
  }, [model.provider_name, model.base_url, model.model_identifier])

  const saveStep = useCallback(
    async (target: number): Promise<boolean> => {
      const message = target === 0 ? validateStep0() : validateStep1()
      if (message) {
        setError(message)
        return false
      }
      setError('')
      setSaving(true)
      try {
        if (target === 0) {
          await saveBotSection('bot', {
            nickname: basic.nickname.trim(),
            alias_names: basic.alias_names.split(/[,，]/).map((s) => s.trim()).filter(Boolean),
            platform: basic.platform.trim() || 'qq',
            qq_account: basic.qq_account.trim(),
          })
          await saveBotSection('personality', {
            personality: personality.personality.trim(),
            reply_style: personality.reply_style.trim(),
          })
        } else {
          await saveModelRoute(model)
        }
        return true
      } catch (err) {
        setError(err instanceof Error ? err.message : '保存失败')
        return false
      } finally {
        setSaving(false)
      }
    },
    [basic, personality, model, validateStep0, validateStep1]
  )

  const goNext = async () => {
    const okStep = await saveStep(step)
    if (okStep) setStep(1)
  }

  const finish = async () => {
    const okStep = await saveStep(1)
    if (!okStep) return
    try {
      await completeSetup()
      toast.ok('配置完成')
      navigate({ to: postAuthTarget() })
    } catch (err) {
      setError(err instanceof Error ? err.message : '标记完成失败')
    }
  }

  const skip = async () => {
    try {
      await completeSetup()
      navigate({ to: postAuthTarget() })
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '操作失败')
    }
  }

  if (loading) return <Loading text="读取配置…" />

  return (
    <div className="page" style={{ maxWidth: 720, margin: '0 auto', paddingTop: 24 }}>
      <div className="page-head">
        <h1>首次配置向导</h1>
        <span className="page-actions">
          <button className="btn btn-ghost btn-sm" onClick={() => setSkipConfirm(true)} disabled={saving}>
            跳过向导
          </button>
        </span>
      </div>
      <div className="tabs">
        <button className={`tab ${step === 0 ? 'active' : ''}`} onClick={() => step === 1 && setStep(0)}>
          1 · 基础与人格
        </button>
        <button className={`tab ${step === 1 ? 'active' : ''}`} onClick={() => step === 0 && setStep(1)}>
          2 · 模型配置
        </button>
      </div>

      {error ? <div className="banner banner-err">{error}</div> : null}

      {step === 0 ? (
        <div className="card">
          <div className="card-head"><h3>基础与人格</h3></div>
          <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
            <div className="grid grid-2">
              <div>
                <label className="label">机器人昵称 *</label>
                <TextInput
                  value={basic.nickname}
                  onChange={(e) => setBasic({ ...basic, nickname: e.target.value })}
                  placeholder="麦麦"
                  invalid={!basic.nickname.trim()}
                />
                <p className="field-hint">用于判断是否被 @ / 提及</p>
              </div>
              <div>
                <label className="label">别名（逗号分隔）</label>
                <TextInput
                  value={basic.alias_names}
                  onChange={(e) => setBasic({ ...basic, alias_names: e.target.value })}
                  placeholder="小麦,麦麦酱"
                />
              </div>
              <div>
                <label className="label">平台</label>
                <SelectInput value={basic.platform} onChange={(e) => setBasic({ ...basic, platform: e.target.value })}>
                  <option value="qq">qq</option>
                </SelectInput>
              </div>
              <div>
                <label className="label">QQ 账号（机器人）</label>
                <TextInput
                  value={basic.qq_account}
                  onChange={(e) => setBasic({ ...basic, qq_account: e.target.value })}
                  placeholder="机器人 QQ 号"
                />
              </div>
            </div>
            <div>
              <label className="label">人格特征 *</label>
              <TextArea
                rows={4}
                value={personality.personality}
                onChange={(e) => setPersonality({ ...personality, personality: e.target.value })}
                placeholder="是一个大二女大学生，正在上网和群友聊天…"
                invalid={!personality.personality.trim()}
              />
            </div>
            <div>
              <label className="label">表达风格</label>
              <TextArea
                rows={3}
                value={personality.reply_style}
                onChange={(e) => setPersonality({ ...personality, reply_style: e.target.value })}
                placeholder="风格平淡简短，参考贴吧回复风格…"
              />
            </div>
            <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
              <button className="btn" onClick={() => void goNext()} disabled={saving}>
                {saving ? <span className="spin" /> : null} 下一步
              </button>
            </div>
          </div>
        </div>
      ) : (
        <div className="card">
          <div className="card-head"><h3>模型配置</h3></div>
          <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
            <div className="grid grid-2">
              <div>
                <label className="label">服务商名称 *</label>
                <TextInput
                  value={model.provider_name}
                  onChange={(e) => setModel({ ...model, provider_name: e.target.value })}
                  placeholder="my-provider"
                />
              </div>
              <div>
                <label className="label">Base URL *</label>
                <TextInput
                  value={model.base_url}
                  onChange={(e) => setModel({ ...model, base_url: e.target.value })}
                  placeholder="https://api.example.com/v1"
                />
              </div>
              <div style={{ gridColumn: '1 / -1' }}>
                <label className="label">API Key</label>
                <TextInput
                  type="password"
                  value={model.api_key}
                  onChange={(e) => setModel({ ...model, api_key: e.target.value })}
                  placeholder="留空则保留已配置的 Key"
                />
              </div>
              <div style={{ gridColumn: '1 / -1' }}>
                <label className="label">模型标识 *</label>
                <TextInput
                  value={model.model_identifier}
                  onChange={(e) => setModel({ ...model, model_identifier: e.target.value })}
                  placeholder="gpt-4o-mini"
                />
                <p className="field-hint">replyer / planner / utils 任务都将路由到该模型</p>
              </div>
            </div>
            <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8 }}>
              <button className="btn btn-secondary" onClick={() => setStep(0)} disabled={saving}>
                上一步
              </button>
              <button className="btn" onClick={() => void finish()} disabled={saving}>
                {saving ? <span className="spin" /> : <CircleCheck size={14} />} 完成配置
              </button>
            </div>
          </div>
        </div>
      )}

      {skipConfirm ? (
        <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && setSkipConfirm(false)}>
          <div className="modal" style={{ maxWidth: 380 }} role="alertdialog" aria-modal="true">
            <div className="modal-title">跳过首次配置？</div>
            <p className="modal-msg">可以稍后在「机器人设置」「模型配置」页完成配置。</p>
            <div className="modal-foot">
              <button className="btn btn-secondary btn-sm" onClick={() => setSkipConfirm(false)}>取消</button>
              <button className="btn btn-sm" onClick={() => void skip()}>跳过</button>
            </div>
          </div>
        </div>
      ) : null}
    </div>
  )
}

/** 把向导里的服务商/模型/任务路由写进 model_config（合并语义）。 */
async function saveModelRoute(route: ModelRoute): Promise<void> {
  const config = await getModelConfig()
  const providers = Array.isArray(config.api_providers) ? [...(config.api_providers as Array<Record<string, unknown>>)] : []
  const models = Array.isArray(config.models) ? [...(config.models as Array<Record<string, unknown>>)] : []
  const taskConfig = (config.model_task_config && typeof config.model_task_config === 'object'
    ? config.model_task_config
    : {}) as Record<string, Record<string, unknown>>

  const providerName = route.provider_name.trim()
  const idx = providers.findIndex((p) => p.name === providerName)
  const provider: Record<string, unknown> = {
    name: providerName,
    base_url: route.base_url.trim(),
    client_type: 'openai',
    max_retry: 3,
    timeout: 120,
    retry_interval: 5,
  }
  if (route.api_key.trim()) provider.api_key = route.api_key.trim()
  if (idx >= 0) {
    // 保留已有 api_key（表单留空时）
    if (!route.api_key.trim() && providers[idx].api_key) provider.api_key = providers[idx].api_key
    providers[idx] = { ...providers[idx], ...provider }
  } else {
    providers.push(provider)
  }

  const modelName = route.model_identifier.trim()
  const modelIdx = models.findIndex((m) => m.name === modelName)
  const model: Record<string, unknown> = {
    name: modelName,
    model_identifier: modelName,
    api_provider: providerName,
    price_in: 0,
    price_out: 0,
    force_stream_mode: false,
    visual: false,
  }
  if (modelIdx >= 0) {
    models[modelIdx] = { ...models[modelIdx], ...model }
  } else {
    models.push(model)
  }

  for (const task of TASK_ORDER) {
    taskConfig[task] = { ...(taskConfig[task] ?? {}), model_list: [modelName] }
  }

  await saveModelConfig({ ...config, api_providers: providers, models, model_task_config: taskConfig })
}
