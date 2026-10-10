import { AlertTriangle, Trash2 } from 'lucide-react'

import { Button } from '@/components/ui/button'
import { CodeEditor } from '@/components/CodeEditor'
import { Label } from '@/components/ui/label'
import { sanitizeCSS } from '@/lib/theme/sanitizer'

export type ComponentCSSEditorProps = {
  /** 组件唯一标识符 */
  componentId: string
  /** 当前 CSS 内容 */
  value: string
  /** CSS 内容变更回调 */
  onChange: (css: string) => void
  /** 编辑器标签文字 */
  label?: string
  /** 编辑器高度，默认 200px */
  height?: string
  disabled?: boolean
}

/**
 * 组件级 CSS 编辑器
 * 提供 CSS 代码编辑、语法高亮和安全过滤警告功能
 */
export function ComponentCSSEditor({
  componentId,
  value,
  onChange,
  label,
  height = '200px',
  disabled = false,
}: ComponentCSSEditorProps) {
  // 实时计算 CSS 警告
  const { warnings } = sanitizeCSS(value)

  return (
    <div className={disabled ? 'space-y-2 opacity-50' : 'space-y-2'}>
      <div className="flex items-center justify-between">
        <Label className="text-sm font-medium">
          {label || '自定义 CSS'}
        </Label>
        
        <Button
          variant="ghost"
          size="sm"
          onClick={() => onChange('')}
          disabled={disabled || !value}
          className="h-7 px-2 text-xs text-muted-foreground hover:text-destructive"
          title="清除所有 CSS"
        >
          <Trash2 className="mr-1.5 h-3.5 w-3.5" />
          清除
        </Button>
      </div>

      <div className="rounded-md border bg-card overflow-hidden">
        <CodeEditor
          value={value}
          onChange={disabled ? undefined : onChange}
          language="css"
          readOnly={disabled}
          height={height}
          placeholder={`/* 为 ${componentId} 组件编写自定义 CSS */\n\n/* 示例: */\n/* .custom-class { background: red; } */`}
        />

        {warnings.length > 0 && (
          <div className="border-destructive/30 bg-destructive/5 border-t p-3">
            <div className="text-destructive mb-1 flex items-center gap-2 text-xs font-medium">
              <AlertTriangle className="h-3.5 w-3.5" />
              检测到不安全的 CSS 规则：
            </div>
            <ul className="text-destructive/80 ml-5 list-disc space-y-0.5 text-[10px] sm:text-xs">
              {warnings.map((w, i) => (
                <li key={i}>{w}</li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </div>
  )
}
