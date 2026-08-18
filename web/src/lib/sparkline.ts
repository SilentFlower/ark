/** 大小趋势用的极简 sparkline 几何计算。抽成纯函数以便单测边界情况。 */

export interface SparklineGeometry {
  /** SVG path 的 d 属性；无数据时为空字符串。 */
  path: string
  /** 最后一个点的坐标，用于画高亮圆点；无数据时为 null。 */
  last: { x: number; y: number } | null
}

/**
 * 计算 sparkline 的折线路径。
 * @param values 采样值，按时间正序。
 * @param width SVG 宽度。
 * @param height SVG 高度。
 * @return 路径与末点坐标。
 *
 * 三种退化情况都必须安全：空数组返回空路径；单点画在中线（没有斜率可言）；
 * 所有值相同也画在中线，避免除零把整条线顶到顶部。
 */
export function buildSparkline(values: number[], width: number, height: number): SparklineGeometry {
  if (values.length === 0) {
    return { path: '', last: null }
  }
  const padding = 1
  const usableHeight = Math.max(height - padding * 2, 1)
  const maximum = Math.max(...values)
  const minimum = Math.min(...values)
  const span = maximum - minimum

  const pointAt = (index: number) => {
    const x = values.length === 1 ? width / 2 : (index / (values.length - 1)) * width
    // span 为 0 时全部落在中线：这条线代表「体积稳定」，不是「体积最高」。
    const ratio = span === 0 ? 0.5 : (values[index]! - minimum) / span
    const y = padding + (1 - ratio) * usableHeight
    return { x, y }
  }

  const points = values.map((_, index) => pointAt(index))
  const path = points
    .map((point, index) => `${index === 0 ? 'M' : 'L'}${point.x.toFixed(2)},${point.y.toFixed(2)}`)
    .join(' ')
  return { path, last: points[points.length - 1] ?? null }
}
