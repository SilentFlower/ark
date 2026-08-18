import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { ApiError } from '@/api/client'

import { useSessionStore } from './session'

const apiMock = vi.hoisted(() => ({ session: vi.fn() }))

vi.mock('@/api/client', async (importOriginal) => {
  const original = await importOriginal<typeof import('@/api/client')>()
  return { ...original, api: apiMock }
})

describe('session store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    apiMock.session.mockReset()
  })

  it('加载成功时记录用户名与 CSRF token', async () => {
    apiMock.session.mockResolvedValue({
      authenticated: true,
      username: 'admin',
      csrf_token: 'tok',
    })

    const store = useSessionStore()
    await store.load()

    expect(store.username).toBe('admin')
    expect(store.csrfToken).toBe('tok')
    expect(store.loaded).toBe(true)
    expect(store.error).toBe('')
  })

  // /api/session 在 auth 存储运行期故障时返回 503，那条路径不触发登录跳转。
  // 若 load() 直接抛出，调用方后续的告警加载会被跳过，侧栏告警数会停在 0——
  // 「没有告警」和「没查到告警」在这个产品里是完全不同的两件事。
  it('加载失败时吞掉异常并记录错误，不中断调用方', async () => {
    apiMock.session.mockRejectedValue(new ApiError(503, 'service_unavailable', '服务暂不可用'))

    const store = useSessionStore()
    await expect(store.load()).resolves.toBeUndefined()

    expect(store.loaded).toBe(false)
    expect(store.error).toBe('服务暂不可用')
    expect(store.csrfToken).toBe('')
  })
})
