import { describe, expect, it } from 'vitest'

import {
  compactNumber,
  formatBytes,
  formatRelative,
  formatTime,
  formatUptime,
  jsonEqual,
  parseBackendTime,
  sessionTitle,
  truncate,
} from './format'

describe('formatBytes', () => {
  it('bytes 以下原样', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(512)).toBe('512 B')
  })
  it('KB/MB 进制', () => {
    expect(formatBytes(1024)).toBe('1.0 KB')
    expect(formatBytes(1536)).toBe('1.5 KB')
    expect(formatBytes(1024 * 1024)).toBe('1.0 MB')
  })
  it('非法输入', () => {
    expect(formatBytes(-1)).toBe('—')
    expect(formatBytes(NaN)).toBe('—')
  })
})

describe('compactNumber', () => {
  it('千以内原样', () => {
    expect(compactNumber(0)).toBe('0')
    expect(compactNumber(999)).toBe('999')
  })
  it('千/百万缩写', () => {
    expect(compactNumber(1000)).toBe('1.0k')
    expect(compactNumber(12345)).toBe('12k')
    expect(compactNumber(2_500_000)).toBe('2.5M')
  })
})

describe('formatUptime', () => {
  it('秒/分/时/天', () => {
    expect(formatUptime(42)).toBe('42秒')
    expect(formatUptime(90)).toBe('1分钟30秒')
    expect(formatUptime(3600)).toBe('1小时0分钟')
    expect(formatUptime(90061)).toBe('1天1小时')
  })
})

describe('parseBackendTime', () => {
  it('RFC3339', () => {
    expect(parseBackendTime('2026-10-10T12:00:00+08:00')).not.toBeNull()
  })
  it('SQLite DATETIME', () => {
    expect(parseBackendTime('2026-10-10 12:00:00')).not.toBeNull()
  })
  it('无效输入返回 null', () => {
    expect(parseBackendTime('')).toBeNull()
    expect(parseBackendTime('not-a-time')).toBeNull()
    expect(parseBackendTime(null)).toBeNull()
  })
})

describe('formatTime', () => {
  it('无效输入显示 —', () => {
    expect(formatTime('garbage')).toBe('—')
    expect(formatTime(null)).toBe('—')
  })
})

describe('formatRelative', () => {
  it('刚刚', () => {
    const now = new Date()
    expect(formatRelative(now.toISOString())).toBe('刚刚')
  })
  it('无效输入 —', () => {
    expect(formatRelative('bad')).toBe('—')
  })
})

describe('jsonEqual', () => {
  it('键序无关', () => {
    expect(jsonEqual({ a: 1, b: 2 }, { b: 2, a: 1 })).toBe(true)
  })
  it('嵌套数组', () => {
    expect(jsonEqual([1, [2, 3]], [1, [2, 3]])).toBe(true)
    expect(jsonEqual([1, 2], [2, 1])).toBe(false)
  })
  it('类型不同', () => {
    expect(jsonEqual('1', 1)).toBe(false)
    expect(jsonEqual(null, undefined)).toBe(false)
  })
})

describe('truncate / sessionTitle', () => {
  it('truncate', () => {
    expect(truncate('abcdef', 5)).toBe('abcd…')
    expect(truncate('abc', 5)).toBe('abc')
  })
  it('sessionTitle 群聊/私聊', () => {
    expect(sessionTitle({ is_group: true, group_name: '测试群' })).toBe('测试群')
    expect(sessionTitle({ is_group: false, user_nickname: '张三' })).toBe('张三')
    expect(sessionTitle({})).toBe('私聊')
  })
})
