/**
 * 数据传输 页 — 配置导出 / 导入。
 *
 * - POST /data-transfer/export  {include_bot, include_model} → JSON 附件（api/misc.ts 的 exportConfig 触发浏览器下载）
 * - POST /data-transfer/import  payload=导出文件 JSON → 返回 imported 节清单（api/misc.ts 的 importConfig 读文件提交）
 * 导出内容为 bot_config / model_config 两个配置文件的 JSON 打包。
 */

import { useRef, useState } from 'react'
import { Download, FileJson, FolderSync, Upload } from 'lucide-react'

import { exportConfig, importConfig } from '@/api/misc'
import { toast } from '@/lib/toast'
import { Banner, ConfirmModal, Toggle } from '@/components/ui'

export function DataTransferPage() {
  // 导出选项：默认全部包含
  const [includeBot, setIncludeBot] = useState(true)
  const [includeModel, setIncludeModel] = useState(true)
  const [exporting, setExporting] = useState(false)

  // 导入流程：选文件 → ConfirmModal 确认 → importConfig
  const [pendingFile, setPendingFile] = useState<File | null>(null)
  const [importing, setImporting] = useState(false)
  const fileInputRef = useRef<HTMLInputElement>(null)

  const doExport = async () => {
    if (!includeBot && !includeModel) {
      toast.err('请至少勾选一项要导出的配置')
      return
    }
    setExporting(true)
    try {
      await exportConfig({ include_bot: includeBot, include_model: includeModel })
      toast.ok('配置已导出，浏览器开始下载')
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '导出失败')
    } finally {
      setExporting(false)
    }
  }

  const onPickFile = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0] ?? null
    // 允许重复选择同一文件：每次选完重置 input
    e.target.value = ''
    if (file) setPendingFile(file)
  }

  const doImport = async () => {
    if (!pendingFile) return
    setImporting(true)
    try {
      const imported = await importConfig(pendingFile)
      setPendingFile(null)
      toast.ok(`导入成功：${imported.length > 0 ? imported.join('、') : '配置'} 已覆盖当前配置`)
    } catch (err) {
      toast.err(err instanceof Error ? err.message : '导入失败')
    } finally {
      setImporting(false)
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>数据传输</h1>
          <p className="page-sub">打包导出 / 导入机器人与模型配置</p>
        </div>
      </div>

      <Banner kind="info" icon={<FolderSync />}>
        导出内容为 <code>bot_config</code> 与 <code>model_config</code> 两个配置文件的 JSON 打包；导入将按所选文件整体覆盖对应配置。
      </Banner>

      <div className="grid grid-2">
        {/* ── 导出卡片 ── */}
        <div className="card">
          <div className="card-head">
            <h3>导出配置</h3>
            <span className="muted-xs">POST /data-transfer/export</span>
          </div>
          <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
            <p className="sm" style={{ color: 'var(--muted-fg)' }}>
              勾选要包含的配置，导出为单个 JSON 文件并由浏览器自动下载。
            </p>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
              <Toggle checked={includeBot} onChange={setIncludeBot} label="包含 bot_config（机器人配置）" />
              <Toggle checked={includeModel} onChange={setIncludeModel} label="包含 model_config（模型配置）" />
            </div>
            <div>
              <button
                className="btn"
                onClick={() => void doExport()}
                disabled={exporting || (!includeBot && !includeModel)}
              >
                {exporting ? <span className="spin" /> : <Download />} 导出 JSON
              </button>
            </div>
          </div>
        </div>

        {/* ── 导入卡片 ── */}
        <div className="card">
          <div className="card-head">
            <h3>导入配置</h3>
            <span className="muted-xs">POST /data-transfer/import</span>
          </div>
          <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
            <p className="sm" style={{ color: 'var(--muted-fg)' }}>
              选择此前导出的 JSON 文件，确认后整体覆盖当前配置。
            </p>
            <div>
              <input
                ref={fileInputRef}
                type="file"
                accept=".json,application/json"
                onChange={onPickFile}
                style={{ display: 'none' }}
                aria-label="选择配置 JSON 文件"
              />
              <button className="btn btn-secondary" onClick={() => fileInputRef.current?.click()} disabled={importing}>
                <FileJson /> {pendingFile ? '重新选择文件' : '选择 JSON 文件'}
              </button>
              {pendingFile ? (
                <p className="sm" style={{ marginTop: 8 }}>
                  已选择：<code>{pendingFile.name}</code>
                  <span className="muted-xs">（{pendingFile.size} 字节）</span>
                </p>
              ) : (
                <p className="field-hint">尚未选择文件</p>
              )}
            </div>
            <Banner kind="warn">导入将整体覆盖当前配置，操作不可撤销，建议先导出一份备份。</Banner>
            <div>
              <button className="btn btn-danger" onClick={() => setPendingFile((f) => f)} disabled={!pendingFile || importing}>
                {importing ? <span className="spin" /> : <Upload />} 导入并覆盖
              </button>
            </div>
          </div>
        </div>
      </div>

      <ConfirmModal
        open={pendingFile !== null}
        title="确认导入配置？"
        danger
        loading={importing}
        confirmText="确认导入"
        message={
          pendingFile
            ? `导入将覆盖当前配置。将使用「${pendingFile.name}」中的内容覆盖现有 bot_config / model_config，此操作不可撤销。`
            : undefined
        }
        onConfirm={() => void doImport()}
        onClose={() => setPendingFile(null)}
      />
    </div>
  )
}
