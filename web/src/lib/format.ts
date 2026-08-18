/** 界面展示用的格式化函数。全部为纯函数，便于单测。 */

const byteUnits = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']

/**
 * 把字节数格式化为二进制单位。
 * @param bytes 字节数；null 表示没有数据。
 * @return 形如 `1.5 GiB` 的字符串，无数据时返回 `—`。
 */
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined || Number.isNaN(bytes)) {
    return '—'
  }
  if (bytes < 1024) {
    return `${bytes} B`
  }
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < byteUnits.length - 1) {
    value /= 1024
    unit += 1
  }
  return `${value.toFixed(value >= 100 ? 0 : 1)} ${byteUnits[unit]}`
}

/**
 * 把毫秒格式化为人类可读时长。
 * @param milliseconds 毫秒数；null 表示任务尚未结束。
 * @return 形如 `1h 12m`、`45.3s` 的字符串。
 */
export function formatDuration(milliseconds: number | null | undefined): string {
  if (milliseconds === null || milliseconds === undefined || Number.isNaN(milliseconds)) {
    return '—'
  }
  const seconds = milliseconds / 1000
  if (seconds < 60) {
    return `${seconds.toFixed(1)}s`
  }
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) {
    return `${minutes}m ${Math.round(seconds % 60)}s`
  }
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
}

/**
 * 把 RFC3339 时间戳格式化为本地时间。
 * @param value 后端返回的 UTC 时间戳；null 表示没有记录。
 * @return 形如 `2026-08-17 04:17:00` 的本地时间，无值时返回 `—`。
 */
export function formatTime(value: string | null | undefined): string {
  if (!value) {
    return '—'
  }
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) {
    return value
  }
  const pad = (part: number) => String(part).padStart(2, '0')
  return (
    `${parsed.getFullYear()}-${pad(parsed.getMonth() + 1)}-${pad(parsed.getDate())} ` +
    `${pad(parsed.getHours())}:${pad(parsed.getMinutes())}:${pad(parsed.getSeconds())}`
  )
}

/**
 * 计算相对当前时间的粗粒度描述。
 *
 * 备份场景关心的是「多久没备份了」，精确到分钟没有意义，因此按天/小时/分钟分档。
 * @param value 时间戳；null 表示从未发生。
 * @param now 当前时间，注入以便测试。
 * @return 形如 `3 天前`、`12 小时前`、`即将`；无值时返回 `从未`。
 */
export function formatRelative(value: string | null | undefined, now: Date = new Date()): string {
  if (!value) {
    return '从未'
  }
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) {
    return value
  }
  const deltaMs = parsed.getTime() - now.getTime()
  const future = deltaMs > 0
  const absMinutes = Math.abs(deltaMs) / 60000
  const suffix = future ? '后' : '前'
  if (absMinutes < 1) {
    return future ? '即将' : '刚刚'
  }
  if (absMinutes < 60) {
    return `${Math.round(absMinutes)} 分钟${suffix}`
  }
  const hours = absMinutes / 60
  if (hours < 24) {
    return `${Math.round(hours)} 小时${suffix}`
  }
  return `${Math.round(hours / 24)} 天${suffix}`
}
