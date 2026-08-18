import { describe, expect, it } from 'vitest'

import { buildSparkline } from './sparkline'

describe('buildSparkline', () => {
  it('空数据不产生路径', () => {
    expect(buildSparkline([], 100, 20)).toEqual({ path: '', last: null })
  })

  it('单点画在中线，不假装有趋势', () => {
    const geometry = buildSparkline([42], 100, 20)
    expect(geometry.last).not.toBeNull()
    expect(geometry.last!.x).toBe(50)
    expect(geometry.last!.y).toBeCloseTo(10, 1)
  })

  it('全部数值相同时落在中线而不是顶部（避免除零）', () => {
    const geometry = buildSparkline([7, 7, 7], 100, 20)
    const yValues = geometry.path.match(/,(\d+\.\d+)/g)
    expect(new Set(yValues).size).toBe(1)
    expect(geometry.last!.y).toBeCloseTo(10, 1)
  })

  it('14 个点铺满整个宽度，最大值贴上沿', () => {
    const values = Array.from({ length: 14 }, (_, index) => index + 1)
    const geometry = buildSparkline(values, 140, 20)
    expect(geometry.path.startsWith('M0.00,')).toBe(true)
    expect(geometry.last!.x).toBe(140)
    expect(geometry.last!.y).toBeCloseTo(1, 1)
  })

  it('最小值贴下沿', () => {
    const geometry = buildSparkline([10, 20], 100, 20)
    expect(geometry.path).toContain('M0.00,19.00')
  })
})
