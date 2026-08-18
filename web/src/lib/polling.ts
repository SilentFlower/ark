import type { Operation } from '@/api/types'

/** 前 10 次轮询的间隔。手工操作通常几十秒内结束，前期密一点体验更好。 */
export const fastIntervalMs = 2000
/** 退避后的轮询间隔。 */
export const slowIntervalMs = 5000
/** 使用快间隔的次数。 */
export const fastAttempts = 10
/** 轮询总时长上限。超过认为前端不再跟踪，由操作列表兜底。 */
export const pollTimeoutMs = 30 * 60 * 1000

export interface PollDependencies {
  /** 拉取一次操作状态。 */
  fetchOnce: () => Promise<Operation>
  /**
   * 每次成功响应都会被调用。
   *
   * 这个回调是恢复确认 token 的唯一捕获点：后端只在预检操作**首次**变为 `ok`
   * 的那一次读取里下发 token，之后永不重发。所以调用方必须在这里就地取走，
   * 不能等轮询结束再回头找。
   */
  onUpdate: (operation: Operation) => void
  sleep?: (milliseconds: number) => Promise<void>
  now?: () => number
}

const defaultSleep = (milliseconds: number) =>
  new Promise<void>((resolve) => {
    setTimeout(resolve, milliseconds)
  })

/** 操作是否已进入终态。`interrupted` 也是终态，表示 Hub 重启时任务被中断。 */
export function isTerminal(operation: Operation): boolean {
  return operation.status !== 'running'
}

/**
 * 轮询一个手工操作直到终态。
 * @param dependencies 拉取、回调与可注入的计时器。
 * @return 终态操作。
 * @throws 超过总时长上限时抛错；拉取失败的错误直接向上传播。
 */
export async function pollOperation(dependencies: PollDependencies): Promise<Operation> {
  const sleep = dependencies.sleep ?? defaultSleep
  const now = dependencies.now ?? (() => Date.now())
  const deadline = now() + pollTimeoutMs

  for (let attempt = 0; ; attempt += 1) {
    const operation = await dependencies.fetchOnce()
    dependencies.onUpdate(operation)
    if (isTerminal(operation)) {
      return operation
    }
    if (now() >= deadline) {
      throw new Error('操作仍在运行，前端已停止跟踪；请在操作列表中查看最终结果')
    }
    await sleep(attempt < fastAttempts ? fastIntervalMs : slowIntervalMs)
  }
}
