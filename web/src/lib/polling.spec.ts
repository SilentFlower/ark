import { describe, expect, it, vi } from 'vitest'

import type { Operation } from '@/api/types'

import { fastAttempts, fastIntervalMs, pollOperation, slowIntervalMs } from './polling'

function operation(status: Operation['status'], overrides: Partial<Operation> = {}): Operation {
  return {
    id: 'op-1',
    kind: 'restore_preview',
    host: 'web-01',
    status,
    started_at: '2026-08-17T04:17:00Z',
    finished_at: null,
    duration_ms: null,
    request: {},
    result: null,
    error: '',
    exit_code: null,
    parent_operation_id: null,
    ...overrides,
  }
}

describe('pollOperation', () => {
  it('拿到终态立即返回，不再多轮询一次', async () => {
    const fetchOnce = vi.fn().mockResolvedValue(operation('ok'))
    const sleep = vi.fn().mockResolvedValue(undefined)

    const result = await pollOperation({ fetchOnce, onUpdate: () => {}, sleep })

    expect(result.status).toBe('ok')
    expect(fetchOnce).toHaveBeenCalledTimes(1)
    expect(sleep).not.toHaveBeenCalled()
  })

  it('interrupted 也是终态', async () => {
    const fetchOnce = vi.fn().mockResolvedValue(operation('interrupted'))
    const result = await pollOperation({ fetchOnce, onUpdate: () => {}, sleep: vi.fn() })
    expect(result.status).toBe('interrupted')
  })

  it('每次响应都会回调，包括仍在运行的中间态', async () => {
    const responses = [operation('running'), operation('running'), operation('ok')]
    const fetchOnce = vi.fn().mockImplementation(() => Promise.resolve(responses.shift()!))
    const seen: string[] = []

    await pollOperation({
      fetchOnce,
      onUpdate: (value) => seen.push(value.status),
      sleep: vi.fn().mockResolvedValue(undefined),
    })

    expect(seen).toEqual(['running', 'running', 'ok'])
  })

  it('前 10 次用快间隔，之后退避到慢间隔', async () => {
    let remaining = fastAttempts + 2
    const fetchOnce = vi.fn().mockImplementation(() => {
      remaining -= 1
      return Promise.resolve(operation(remaining > 0 ? 'running' : 'ok'))
    })
    const intervals: number[] = []
    const sleep = vi.fn().mockImplementation((ms: number) => {
      intervals.push(ms)
      return Promise.resolve()
    })

    await pollOperation({ fetchOnce, onUpdate: () => {}, sleep })

    expect(intervals.slice(0, fastAttempts).every((value) => value === fastIntervalMs)).toBe(true)
    expect(intervals[fastAttempts]).toBe(slowIntervalMs)
  })

  it('超过总时长上限后放弃跟踪并给出可行动的提示', async () => {
    const fetchOnce = vi.fn().mockResolvedValue(operation('running'))
    // 第二次读取 now 时直接跨过 deadline，模拟长时间运行。
    let calls = 0
    const now = () => {
      calls += 1
      return calls === 1 ? 0 : Number.MAX_SAFE_INTEGER
    }

    await expect(
      pollOperation({ fetchOnce, onUpdate: () => {}, sleep: vi.fn(), now }),
    ).rejects.toThrow('操作列表')
  })

  it('拉取失败的错误向上传播，不吞掉', async () => {
    const fetchOnce = vi.fn().mockRejectedValue(new Error('网络中断'))
    await expect(
      pollOperation({ fetchOnce, onUpdate: () => {}, sleep: vi.fn() }),
    ).rejects.toThrow('网络中断')
  })
})
