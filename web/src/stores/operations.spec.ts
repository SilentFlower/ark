import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { ApiError } from '@/api/client'
import type { Operation } from '@/api/types'

import { confirmationTtlMs, useOperationsStore } from './operations'

// vi.mock 的工厂会被提升到文件顶部，因此 mock 集合必须用 vi.hoisted 一起提升。
const apiMock = vi.hoisted(() => ({
  operation: vi.fn(),
  startBackup: vi.fn(),
  startVerify: vi.fn(),
  startRestorePreview: vi.fn(),
  executeRestore: vi.fn(),
  operations: vi.fn(),
}))

vi.mock('@/api/client', async (importOriginal) => {
  const original = await importOriginal<typeof import('@/api/client')>()
  return { ...original, api: apiMock }
})

// 轮询在测试里必须立刻推进，否则每个用例都要等真实的 2 秒。
vi.mock('@/lib/polling', async (importOriginal) => {
  const original = await importOriginal<typeof import('@/lib/polling')>()
  return {
    ...original,
    pollOperation: (dependencies: Parameters<typeof original.pollOperation>[0]) =>
      original.pollOperation({ ...dependencies, sleep: () => Promise.resolve() }),
  }
})

function operation(overrides: Partial<Operation> = {}): Operation {
  return {
    id: 'preview-1',
    kind: 'restore_preview',
    host: 'web-01',
    status: 'ok',
    started_at: '2026-08-17T04:17:00Z',
    finished_at: '2026-08-17T04:18:00Z',
    duration_ms: 60_000,
    request: {},
    result: null,
    error: '',
    exit_code: 0,
    parent_operation_id: null,
    ...overrides,
  }
}

describe('operations store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    for (const mock of Object.values(apiMock)) {
      mock.mockReset()
    }
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('在预检首次变为 ok 的那一次响应里就地捕获确认 token', async () => {
    const store = useOperationsStore()
    apiMock.startRestorePreview.mockResolvedValue(operation({ status: 'running' }))
    // 后端只在这一次响应里下发 token，之后永不重发。
    apiMock.operation
      .mockResolvedValueOnce(operation({ status: 'running' }))
      .mockResolvedValueOnce(operation({ status: 'ok', confirmation_token: 'tok-1' }))
      .mockResolvedValue(operation({ status: 'ok' }))

    await store.startRestorePreview('web-01', {
      destination_host: 'web-01',
      snapshot: 'latest',
      mode: 'isolate',
    })

    expect(store.confirmation).not.toBeNull()
    expect(store.confirmation!.token).toBe('tok-1')
    expect(store.confirmation!.previewOperationId).toBe('preview-1')
    expect(store.isConfirmationValid()).toBe(true)
  })

  it('token 过期后拒绝执行并要求重新预检', async () => {
    vi.useFakeTimers()
    const store = useOperationsStore()
    store.absorb(operation({ confirmation_token: 'tok-2' }))
    expect(store.isConfirmationValid()).toBe(true)

    vi.advanceTimersByTime(confirmationTtlMs + 1000)

    expect(store.isConfirmationValid()).toBe(false)
    const result = await store.executeRestore('web-01')
    expect(result).toBeNull()
    expect(store.error).toContain('重新预检')
    expect(apiMock.executeRestore).not.toHaveBeenCalled()
  })

  it('确认是一次性的：执行后本地立即丢弃', async () => {
    const store = useOperationsStore()
    store.absorb(operation({ confirmation_token: 'tok-3' }))
    apiMock.executeRestore.mockResolvedValue(operation({ id: 'restore-1', kind: 'restore' }))
    apiMock.operation.mockResolvedValue(operation({ id: 'restore-1', kind: 'restore' }))

    await store.executeRestore('web-01')

    expect(apiMock.executeRestore).toHaveBeenCalledWith('web-01', {
      preview_operation_id: 'preview-1',
      confirmation_token: 'tok-3',
    })
    expect(store.confirmation).toBeNull()
  })

  it('发起新预检会先作废旧确认，避免拿旧 token 执行新计划', async () => {
    const store = useOperationsStore()
    store.absorb(operation({ confirmation_token: 'stale' }))
    apiMock.startRestorePreview.mockResolvedValue(operation({ status: 'ok' }))
    apiMock.operation.mockResolvedValue(operation({ status: 'ok' }))

    await store.startRestorePreview('web-01', {
      destination_host: 'web-01',
      snapshot: 'latest',
      mode: 'normal',
    })

    expect(store.confirmation).toBeNull()
  })

  it('409 冲突翻译成可行动的提示', async () => {
    const store = useOperationsStore()
    apiMock.startBackup.mockRejectedValue(new ApiError(409, 'conflict', '已有手工任务正在运行'))

    const result = await store.startBackup('web-01')

    expect(result).toBeNull()
    expect(store.error).toContain('已有手工任务正在运行')
    expect(store.busy).toBe(false)
  })

  it('确认过期错误码引导重新预检而不是重试', async () => {
    const store = useOperationsStore()
    expect(store.describe(new ApiError(422, 'confirmation_expired', '恢复确认已过期'))).toContain(
      '重新预检',
    )
    expect(store.describe(new ApiError(409, 'confirmation_required', '恢复确认无效'))).toContain(
      '重新预检',
    )
    expect(store.describe(new ApiError(503, 'service_unavailable', ''))).toContain('清单')
    expect(store.describe(new ApiError(500, 'operation_failed', ''))).toContain('子进程')
  })

  // active 是全局状态，主机详情页按 host 展示。不过滤就会出现
  // 「在 web-01 发起备份，切到 db-01 看到 web-01 的结果」。
  it('activeFor 只返回属于该主机的操作', () => {
    const store = useOperationsStore()
    store.absorb(operation({ id: 'op-web', kind: 'backup', host: 'web-01' }))

    expect(store.activeFor('web-01')?.id).toBe('op-web')
    expect(store.activeFor('db-01')).toBeNull()
  })

  it('没有活动操作时 activeFor 返回 null', () => {
    const store = useOperationsStore()
    expect(store.activeFor('web-01')).toBeNull()
  })

  it('操作历史分页会追加而不是覆盖', async () => {
    const store = useOperationsStore()
    apiMock.operations
      .mockResolvedValueOnce({ items: [operation({ id: 'a' })], next_cursor: 'cursor-1' })
      .mockResolvedValueOnce({ items: [operation({ id: 'b' })], next_cursor: null })

    await store.loadHistory()
    expect(store.history).toHaveLength(1)

    await store.loadHistory('cursor-1')
    expect(store.history.map((item) => item.id)).toEqual(['a', 'b'])
    expect(store.nextCursor).toBeNull()
  })
})
