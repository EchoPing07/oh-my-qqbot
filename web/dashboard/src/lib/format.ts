/**
 * 格式化与常用工具函数（纯函数，可单测）。
 */

/** 把字节数格式化为人类可读大小（1024 进制，两位小数以内）。 */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—'
  if (bytes < 1024) return `${bytes} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = bytes
  let unit = -1
  do {
    value /= 1024
    unit++
  } while (value >= 1024 && unit < units.length - 1)
  return `${value >= 100 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`
}

/** 大数字缩写：1.2k / 3.4M / 5.6B；1000 以内原样。 */
export function compactNumber(n: number): string {
  if (!Number.isFinite(n)) return '—'
  const abs = Math.abs(n)
  if (abs < 1000) return String(Math.round(n))
  if (abs < 1_000_000) return `${(n / 1000).toFixed(abs < 10_000 ? 1 : 0)}k`
  if (abs < 1_000_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  return `${(n / 1_000_000_000).toFixed(1)}B`
}

/** 千分位（整数安全）。 */
export function formatNumber(n: number): string {
  if (!Number.isFinite(n)) return '—'
  return n.toLocaleString('zh-CN')
}

/** 秒数 → "3天2小时" / "5分钟" / "42秒" 形式的运行时长。 */
export function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '—'
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const s = Math.floor(seconds % 60)
  if (d > 0) return `${d}天${h}小时`
  if (h > 0) return `${h}小时${m}分钟`
  if (m > 0) return `${m}分钟${s}秒`
  return `${s}秒`
}

/** 解析后端时间字符串为 Date；无效返回 null。后端可能给 RFC3339 或 "YYYY-MM-DD HH:MM:SS"。 */
export function parseBackendTime(raw: string | null | undefined): Date | null {
  if (!raw) return null
  const trimmed = raw.trim()
  if (!trimmed) return null
  // RFC3339 直接解析
  let d = new Date(trimmed)
  if (!Number.isNaN(d.getTime())) return d
  // "2006-01-02 15:04:05"（Go time.ANSIC 风格 / SQLite DATETIME）按本地时区解析
  const m = trimmed.match(/^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})/)
  if (m) {
    d = new Date(
      Number(m[1]), Number(m[2]) - 1, Number(m[3]),
      Number(m[4]), Number(m[5]), Number(m[6])
    )
    if (!Number.isNaN(d.getTime())) return d
  }
  return null
}

/** 时间显示：今天内显示 HH:MM:SS，否则 MM-DD HH:mm。无效输入返回 '—'。 */
export function formatTime(raw: string | number | Date | null | undefined): string {
  const d = raw instanceof Date ? raw : parseBackendTime(raw == null ? null : String(raw))
  if (!d) return '—'
  const now = new Date()
  const sameDay =
    d.getFullYear() === now.getFullYear() &&
    d.getMonth() === now.getMonth() &&
    d.getDate() === now.getDate()
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  const ss = String(d.getSeconds()).padStart(2, '0')
  if (sameDay) return `${hh}:${mm}:${ss}`
  const M = String(d.getMonth() + 1).padStart(2, '0')
  const D = String(d.getDate()).padStart(2, '0')
  return `${M}-${D} ${hh}:${mm}`
}

/** 完整日期时间 YYYY-MM-DD HH:mm:ss。 */
export function formatDateTime(raw: string | number | Date | null | undefined): string {
  const d = raw instanceof Date ? raw : parseBackendTime(raw == null ? null : String(raw))
  if (!d) return '—'
  const Y = d.getFullYear()
  const M = String(d.getMonth() + 1).padStart(2, '0')
  const D = String(d.getDate()).padStart(2, '0')
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  const ss = String(d.getSeconds()).padStart(2, '0')
  return `${Y}-${M}-${D} ${hh}:${mm}:${ss}`
}

/** 相对时间：刚刚 / N 分钟前 / N 小时前 / N 天前 / 具体日期。 */
export function formatRelative(raw: string | null | undefined): string {
  const d = parseBackendTime(raw)
  if (!d) return '—'
  const diff = (Date.now() - d.getTime()) / 1000
  if (diff < 60) return '刚刚'
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`
  if (diff < 86400 * 30) return `${Math.floor(diff / 86400)} 天前`
  return formatDateTime(d).slice(0, 10)
}

/** 截断长文本（前端展示用；默认 max 80）。 */
export function truncate(text: string, max = 80): string {
  if (text.length <= max) return text
  return `${text.slice(0, max - 1)}…`
}

/** 深比较两个 JSON 值是否相等（配置 diff 用；对象键顺序无关）。 */
export function jsonEqual(a: unknown, b: unknown): boolean {
  if (a === b) return true
  if (typeof a !== typeof b) return false
  if (a === null || b === null) return false
  if (Array.isArray(a) && Array.isArray(b)) {
    if (a.length !== b.length) return false
    return a.every((item, i) => jsonEqual(item, b[i]))
  }
  if (typeof a === 'object' && typeof b === 'object') {
    const ka = Object.keys(a as Record<string, unknown>)
    const kb = Object.keys(b as Record<string, unknown>)
    if (ka.length !== kb.length) return false
    return ka.every(
      (k) =>
        jsonEqual(
          (a as Record<string, unknown>)[k],
          (b as Record<string, unknown>)[k]
        )
    )
  }
  return false
}

/** 会话 ID / 群名显示辅助：群聊显示群名，私聊显示昵称。 */
export function sessionTitle(s: {
  is_group?: boolean
  group_name?: string
  group_id?: string
  user_nickname?: string
  user_id?: string
  session_id?: string
}): string {
  if (s.is_group) return s.group_name || s.group_id || '群聊'
  return s.user_nickname || s.user_id || s.session_id?.slice(0, 12) || '私聊'
}
