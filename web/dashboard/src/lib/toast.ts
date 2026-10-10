/**
 * 应用级 toast 通知（极简 store，无依赖）。
 */

import type { ComponentType } from 'react'

export type ToastKind = 'ok' | 'err' | 'info'

export interface ToastItem {
  id: number
  kind: ToastKind
  message: string
  Icon: ComponentType<{ className?: string }> | null
}

type Listener = (toasts: ToastItem[]) => void

let toasts: ToastItem[] = []
const listeners = new Set<Listener>()
let nextId = 1

function notify(): void {
  for (const listener of listeners) listener(toasts)
}

function push(kind: ToastKind, message: string, Icon: ToastItem['Icon'], durationMs: number): void {
  const item: ToastItem = { id: nextId++, kind, message, Icon }
  toasts = [...toasts, item]
  notify()
  window.setTimeout(() => {
    toasts = toasts.filter((t) => t.id !== item.id)
    notify()
  }, durationMs)
}

export const toast = {
  ok(message: string, durationMs = 2600): void {
    push('ok', message, null, durationMs)
  },
  err(message: string, durationMs = 4200): void {
    push('err', message, null, durationMs)
  },
  info(message: string, durationMs = 2600): void {
    push('info', message, null, durationMs)
  },
  subscribe(listener: Listener): () => void {
    listeners.add(listener)
    listener(toasts)
    return () => {
      listeners.delete(listener)
    }
  },
}
