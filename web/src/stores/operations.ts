import { defineStore } from 'pinia'
import { ref } from 'vue'

import { ApiError, api } from '@/api/client'
import type { Operation, RestoreMode } from '@/api/types'
import { pollOperation } from '@/lib/polling'

/** 后端 confirmation token 的有效期，与 `confirmationTTL` 保持一致。 */
export const confirmationTtlMs = 10 * 60 * 1000

/**
 * 内存中的恢复确认。
 *
 * 刻意不写 localStorage：token 一旦离开内存就无法保证只被消费一次，
 * 而它代表的是「覆盖生产数据」的授权。刷新页面即作废是有意为之。
 */
export interface Confirmation {
  previewOperationId: string
  host: string
  token: string
  expiresAt: number
}

/** 手工操作：发起、轮询、历史列表与恢复确认。 */
export const useOperationsStore = defineStore('operations', () => {
  const active = ref<Operation | null>(null)
  const confirmation = ref<Confirmation | null>(null)
  const history = ref<Operation[]>([])
  const nextCursor = ref<string | null>(null)
  const loading = ref(false)
  const error = ref('')
  const busy = ref(false)

  /**
   * 判断确认是否仍然有效。
   *
   * 刻意不做成 computed：computed 只在依赖变化时重算，而这里的依赖是时间本身，
   * 缓存会让一个已经过期的 token 一直报告为有效。调用方需要按秒刷新时，
   * 应当自己持有一个 tick 并把当前时间传进来。
   * @param now 判定时刻，默认当前时间。
   */
  function isConfirmationValid(now: number = Date.now()): boolean {
    const current = confirmation.value
    return current !== null && current.expiresAt > now
  }

  /**
   * 取属于指定主机的当前操作。
   *
   * `active` 是整个应用共享的一份状态，而主机详情页是按 host 展示的。
   * 不过滤就会出现「在 web-01 发起备份，切到 db-01 看到 web-01 的结果」，
   * 让人误以为 db-01 上跑过任务——控制台的价值恰恰在于状态可信。
   * @param host 当前页面的主机名。
   * @return 属于该主机的操作；不属于或没有操作时为 null。
   */
  function activeFor(host: string): Operation | null {
    const current = active.value
    return current !== null && current.host === host ? current : null
  }

  /**
   * 把 API 错误翻译成用户能据以行动的文案。
   *
   * 这些码是后端的稳定契约，不能靠 message 判断——message 会随措辞调整。
   */
  function describe(cause: unknown): string {
    if (cause instanceof ApiError) {
      switch (cause.code) {
        case 'conflict':
          return '已有手工任务正在运行，请等它结束后再试'
        case 'confirmation_required':
          return '恢复确认无效，请重新预检'
        case 'confirmation_expired':
          return '恢复确认已过期，请重新预检'
        case 'operation_failed':
          return 'Ark 子进程启动失败，请检查 hub 上的 ark 二进制'
        case 'service_unavailable':
          return '服务暂不可用（清单可能已损坏）'
        default:
          return cause.message
      }
    }
    return cause instanceof Error ? cause.message : '操作失败'
  }

  /**
   * 记录一次操作响应，并在恢复预检成功时就地捕获确认 token。
   *
   * 后端只在预检 operation 首次变为 ok 的那一次读取里下发 token，之后永不重发。
   * 漏掉这一次就只能重新预检，因此这里不做任何条件收窄。
   */
  function absorb(operation: Operation): void {
    active.value = operation
    if (operation.confirmation_token) {
      confirmation.value = {
        previewOperationId: operation.id,
        host: operation.host,
        token: operation.confirmation_token,
        expiresAt: Date.now() + confirmationTtlMs,
      }
    }
  }

  /** 轮询直到终态，全程把每次响应交给 absorb。 */
  async function track(operationId: string): Promise<Operation | null> {
    try {
      return await pollOperation({
        fetchOnce: () => api.operation(operationId),
        onUpdate: absorb,
      })
    } catch (cause) {
      error.value = describe(cause)
      return null
    }
  }

  async function startBackup(host: string): Promise<Operation | null> {
    return start(() => api.startBackup(host))
  }

  async function startVerify(host: string, snapshot: string): Promise<Operation | null> {
    return start(() => api.startVerify(host, snapshot))
  }

  async function startRestorePreview(
    host: string,
    input: { destination_host: string; snapshot: string; mode: RestoreMode },
  ): Promise<Operation | null> {
    confirmation.value = null
    return start(() => api.startRestorePreview(host, input))
  }

  /**
   * 使用已捕获的确认执行真实恢复。
   * @param host 恢复的来源 host，必须与预检时一致。
   * @return 恢复操作的终态；确认无效时返回 null。
   */
  async function executeRestore(host: string): Promise<Operation | null> {
    const current = confirmation.value
    if (!current || !isConfirmationValid()) {
      error.value = '恢复确认已过期，请重新预检'
      return null
    }
    const started = await start(() =>
      api.executeRestore(host, {
        preview_operation_id: current.previewOperationId,
        confirmation_token: current.token,
      }),
    )
    // token 是一次性的：无论执行成功与否，本地都不再保留。
    confirmation.value = null
    return started
  }

  /** 发起一个操作并跟踪到终态。同一时刻只允许一个前台操作。 */
  async function start(action: () => Promise<Operation>): Promise<Operation | null> {
    if (busy.value) {
      error.value = '已有手工任务正在运行，请等它结束后再试'
      return null
    }
    busy.value = true
    error.value = ''
    try {
      const accepted = await action()
      absorb(accepted)
      return await track(accepted.id)
    } catch (cause) {
      error.value = describe(cause)
      return null
    } finally {
      busy.value = false
    }
  }

  /** 加载操作历史。`cursor` 为空时重新从最新一页开始。 */
  async function loadHistory(cursor?: string): Promise<void> {
    loading.value = true
    error.value = ''
    try {
      const page = await api.operations(cursor ? { cursor } : {})
      history.value = cursor ? [...history.value, ...page.items] : page.items
      nextCursor.value = page.next_cursor
    } catch (cause) {
      error.value = describe(cause)
    } finally {
      loading.value = false
    }
  }

  return {
    active,
    confirmation,
    isConfirmationValid,
    activeFor,
    history,
    nextCursor,
    loading,
    error,
    busy,
    absorb,
    describe,
    startBackup,
    startVerify,
    startRestorePreview,
    executeRestore,
    loadHistory,
  }
})
