/**
 * 极简 UI 原语：Modal / Menu / 字段组件 / Toast 视图。
 * 全部基于 index.css 的类名，无第三方依赖。
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { CircleAlert, CircleCheck, CircleX, Info, X } from 'lucide-react'

import { toast, type ToastItem } from '@/lib/toast'
import { cn } from '@/lib/cn'

// ── Modal ──
interface ModalProps {
  open: boolean
  title: ReactNode
  description?: ReactNode
  onClose: () => void
  children?: ReactNode
  footer?: ReactNode
  width?: number
  danger?: boolean
}

export function Modal({ open, title, description, onClose, children, footer, width, danger }: ModalProps) {
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    // 焦点移入弹窗，便于键盘操作
    const timer = window.setTimeout(() => ref.current?.focus(), 0)
    const prevOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.removeEventListener('keydown', onKey)
      window.clearTimeout(timer)
      document.body.style.overflow = prevOverflow
    }
  }, [open, onClose])

  if (!open) return null
  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div
        ref={ref}
        className="modal"
        style={width ? { maxWidth: width } : undefined}
        role="alertdialog"
        aria-modal="true"
        tabIndex={-1}
      >
        <div className="modal-title">
          {title}
          {danger ? <span className="badge badge-err" style={{ marginLeft: 8 }}>危险操作</span> : null}
        </div>
        {description ? <p className="modal-msg">{description}</p> : null}
        {children ? <div className="modal-body">{children}</div> : null}
        {footer ? <div className="modal-foot">{footer}</div> : null}
        <button
          className="btn btn-ghost btn-icon"
          aria-label="关闭"
          onClick={onClose}
          style={{ position: 'absolute', top: 10, right: 10 }}
        >
          <X />
        </button>
      </div>
    </div>
  )
}

/** 确认弹窗（命令式 helper）。 */
export function ConfirmModal({
  open,
  title,
  message,
  confirmText = '确认',
  danger,
  loading,
  onConfirm,
  onClose,
}: {
  open: boolean
  title: string
  message?: ReactNode
  confirmText?: string
  danger?: boolean
  loading?: boolean
  onConfirm: () => void
  onClose: () => void
}) {
  return (
    <Modal
      open={open}
      title={title}
      description={message}
      danger={danger}
      onClose={onClose}
      width={380}
      footer={
        <>
          <button className="btn btn-secondary btn-sm" onClick={onClose} disabled={loading}>
            取消
          </button>
          <button className={cn('btn btn-sm', danger && 'btn-danger')} onClick={onConfirm} disabled={loading}>
            {loading ? <span className="spin" /> : null}
            {confirmText}
          </button>
        </>
      }
    />
  )
}

// ── Dropdown 菜单（受控挂载点） ──

interface MenuState {
  x: number
  y: number
  items: MenuItem[]
}

export interface MenuItem {
  label: string
  icon?: ReactNode
  danger?: boolean
  disabled?: boolean
  onSelect: () => void
}

const MenuContext = createContext<{
  openMenu: (e: React.MouseEvent, items: MenuItem[]) => void
}>({ openMenu: () => {} })

/** useMenu：在任意元素 onClick={openMenu(e, items)} 弹出菜单。 */
export function MenuProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<MenuState | null>(null)

  const openMenu = useCallback((e: React.MouseEvent, items: MenuItem[]) => {
    e.preventDefault()
    e.stopPropagation()
    setState({ x: e.clientX, y: e.clientY, items })
  }, [])

  useEffect(() => {
    if (!state) return
    const close = () => setState(null)
    window.addEventListener('click', close)
    window.addEventListener('resize', close)
    window.addEventListener('scroll', close, true)
    return () => {
      window.removeEventListener('click', close)
      window.removeEventListener('resize', close)
      window.removeEventListener('scroll', close, true)
    }
  }, [state])

  // 视口内收口，避免菜单溢出
  const style: React.CSSProperties = state
    ? {
        left: Math.min(state.x, window.innerWidth - 190),
        top: Math.min(state.y, window.innerHeight - state.items.length * 34 - 16),
      }
    : {}

  return (
    <MenuContext.Provider value={{ openMenu }}>
      {children}
      {state ? (
        <div className="menu" style={style} role="menu">
          {state.items.map((item, i) => (
            <button
              key={i}
              className={cn('menu-item', item.danger && 'danger')}
              disabled={item.disabled}
              role="menuitem"
              onClick={() => {
                setState(null)
                item.onSelect()
              }}
            >
              {item.icon}
              {item.label}
            </button>
          ))}
        </div>
      ) : null}
    </MenuContext.Provider>
  )
}

export function useMenu(): { openMenu: (e: React.MouseEvent, items: MenuItem[]) => void } {
  return useContext(MenuContext)
}

// ── 字段组件 ──

export function Field({
  label,
  hint,
  error,
  children,
}: {
  label: string
  hint?: string
  error?: string
  children: ReactNode
}) {
  return (
    <div>
      <label className="label">{label}</label>
      {children}
      {error ? <p className="field-hint" style={{ color: 'var(--err-fg)' }}>{error}</p> : hint ? <p className="field-hint">{hint}</p> : null}
    </div>
  )
}

export function TextInput(props: React.InputHTMLAttributes<HTMLInputElement> & { invalid?: boolean }) {
  const { invalid, className, ...rest } = props
  return <input className={cn('input', invalid && 'input-err', className)} {...rest} />
}

export function TextArea(props: React.TextareaHTMLAttributes<HTMLTextAreaElement> & { invalid?: boolean }) {
  const { invalid, className, ...rest } = props
  return <textarea className={cn('textarea', invalid && 'input-err', className)} {...rest} />
}

export function SelectInput(props: React.SelectHTMLAttributes<HTMLSelectElement>) {
  const { className, children, ...rest } = props
  return (
    <select className={cn('select', className)} {...rest}>
      {children}
    </select>
  )
}

/** 开关（受控）。 */
export function Toggle({
  checked,
  onChange,
  disabled,
  label,
}: {
  checked: boolean
  onChange: (checked: boolean) => void
  disabled?: boolean
  label?: string
}) {
  const id = useId()
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
      <button
        id={id}
        type="button"
        role="switch"
        aria-checked={checked}
        aria-label={label}
        disabled={disabled}
        onClick={() => onChange(!checked)}
        style={{
          width: 34,
          height: 18,
          borderRadius: 9,
          border: '1px solid var(--border-strong)',
          background: checked ? 'var(--fg)' : 'var(--muted)',
          position: 'relative',
          cursor: disabled ? 'not-allowed' : 'pointer',
          transition: 'background 0.12s',
          opacity: disabled ? 0.5 : 1,
        }}
      >
        <span
          style={{
            position: 'absolute',
            top: 2,
            left: checked ? 17 : 2,
            width: 12,
            height: 12,
            borderRadius: '50%',
            background: checked ? 'var(--bg)' : 'var(--muted-fg)',
            transition: 'left 0.12s',
          }}
        />
      </button>
      {label ? <label htmlFor={id} className="xs" style={{ cursor: 'pointer' }}>{label}</label> : null}
    </span>
  )
}

// ── Toast 视图 ──

const TOAST_ICONS = { ok: CircleCheck, err: CircleX, info: Info } as const

export function Toaster() {
  const [items, setItems] = useState<ToastItem[]>([])
  useEffect(() => toast.subscribe(setItems), [])
  return (
    <div className="toasts" role="region" aria-label="通知">
      {items.map((item) => {
        const Icon = item.Icon ?? TOAST_ICONS[item.kind]
        return (
          <div key={item.id} className={cn('toast', `toast-${item.kind}`)} role="status">
            <Icon />
            <span>{item.message}</span>
          </div>
        )
      })}
    </div>
  )
}

/** 行内警示条。 */
export function Banner({
  kind = 'info',
  icon,
  children,
}: {
  kind?: 'info' | 'warn' | 'err' | 'ok'
  icon?: ReactNode
  children: ReactNode
}) {
  const FallbackIcon = kind === 'err' ? CircleAlert : kind === 'warn' ? Info : CircleCheck
  return (
    <div className={cn('banner', kind !== 'info' && `banner-${kind}`)}>
      {icon ?? <FallbackIcon />}
      <div style={{ minWidth: 0 }}>{children}</div>
    </div>
  )
}

/** 分页控件。 */
export function Pagination({
  page,
  pageSize,
  total,
  onPage,
}: {
  page: number
  pageSize: number
  total: number
  onPage: (page: number) => void
}) {
  const pages = Math.max(1, Math.ceil(total / pageSize))
  if (pages <= 1) return null
  return (
    <div className="pagination">
      <span>
        第 {page} / {pages} 页 · 共 {total} 条
      </span>
      <button className="btn btn-secondary btn-sm" disabled={page <= 1} onClick={() => onPage(page - 1)}>
        上一页
      </button>
      <button className="btn btn-secondary btn-sm" disabled={page >= pages} onClick={() => onPage(page + 1)}>
        下一页
      </button>
    </div>
  )
}

/** 空态。 */
export function Empty({ icon, text }: { icon?: ReactNode; text: string }) {
  return (
    <div className="empty">
      {icon}
      <span>{text}</span>
    </div>
  )
}

/** 加载块。 */
export function Loading({ text = '加载中…' }: { text?: string }) {
  return (
    <div className="loading-block">
      <span className="spin" />
      {text}
    </div>
  )
}
