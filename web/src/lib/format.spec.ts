import { describe, expect, it } from 'vitest'

import { formatBytes, formatDuration, formatRelative, formatTime } from './format'

describe('formatBytes', () => {
  it('无数据时返回占位符', () => {
    expect(formatBytes(null)).toBe('—')
    expect(formatBytes(undefined)).toBe('—')
  })

  it('小于 1 KiB 时保留原始字节', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(512)).toBe('512 B')
  })

  it('按二进制单位进位', () => {
    expect(formatBytes(1024)).toBe('1.0 KiB')
    expect(formatBytes(1536)).toBe('1.5 KiB')
    expect(formatBytes(1024 * 1024 * 3)).toBe('3.0 MiB')
  })

  it('数值较大时省略小数，避免卡片里数字过长', () => {
    expect(formatBytes(1024 * 200)).toBe('200 KiB')
  })
})

describe('formatDuration', () => {
  it('无数据时返回占位符', () => {
    expect(formatDuration(null)).toBe('—')
  })

  it('分档展示秒、分、时', () => {
    expect(formatDuration(1500)).toBe('1.5s')
    expect(formatDuration(90_000)).toBe('1m 30s')
    expect(formatDuration(3_930_000)).toBe('1h 5m')
  })
})

describe('formatTime', () => {
  it('无值时返回占位符', () => {
    expect(formatTime(null)).toBe('—')
  })

  it('无法解析时原样返回，不伪造时间', () => {
    expect(formatTime('not-a-time')).toBe('not-a-time')
  })

  it('输出固定宽度的本地时间', () => {
    expect(formatTime('2026-08-17T04:17:00Z')).toMatch(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/)
  })
})

describe('formatRelative', () => {
  const now = new Date('2026-08-17T12:00:00Z')

  it('从未发生时明确说「从未」，而不是显示 0', () => {
    expect(formatRelative(null, now)).toBe('从未')
  })

  it('按分钟、小时、天分档', () => {
    expect(formatRelative('2026-08-17T11:30:00Z', now)).toBe('30 分钟前')
    expect(formatRelative('2026-08-17T00:00:00Z', now)).toBe('12 小时前')
    expect(formatRelative('2026-08-14T12:00:00Z', now)).toBe('3 天前')
  })

  it('未来时间用「后」，用于下次计划', () => {
    expect(formatRelative('2026-08-17T16:00:00Z', now)).toBe('4 小时后')
  })
})
