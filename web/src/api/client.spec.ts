import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { ApiError, api, setCsrfToken, setUnauthorizedHandler } from './client'

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

describe('api client', () => {
  const fetchMock = vi.fn()

  beforeEach(() => {
    fetchMock.mockReset()
    vi.stubGlobal('fetch', fetchMock)
    setUnauthorizedHandler(() => {})
    // csrfToken 是模块级状态：写请求在它为空时会被前置拦截，
    // 因此每个用例都从"会话已就绪"这个默认前提开始。
    setCsrfToken('test-csrf')
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('401 触发跳转登录，并抛出可识别的错误', async () => {
    const onUnauthorized = vi.fn()
    setUnauthorizedHandler(onUnauthorized)
    fetchMock.mockResolvedValue(jsonResponse(401, { authenticated: false }))

    await expect(api.hosts()).rejects.toBeInstanceOf(ApiError)
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('把结构化错误体映射为稳定错误码', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(409, { error: { code: 'conflict', message: '已有手工任务正在运行' } }),
    )

    const error = await api.startBackup('web-01').catch((cause: unknown) => cause)

    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).code).toBe('conflict')
    expect((error as ApiError).httpStatus).toBe(409)
    expect((error as ApiError).message).toBe('已有手工任务正在运行')
  })

  it('未知错误码归入 unknown，不把后端新码当成已知语义', async () => {
    fetchMock.mockResolvedValue(jsonResponse(400, { error: { code: 'brand_new', message: '?' } }))
    const error = (await api.hosts().catch((cause: unknown) => cause)) as ApiError
    expect(error.code).toBe('unknown')
  })

  it('非 JSON 错误响应不会让客户端崩溃', async () => {
    fetchMock.mockResolvedValue(new Response('boom', { status: 500 }))
    const error = (await api.hosts().catch((cause: unknown) => cause)) as ApiError
    expect(error.code).toBe('unknown')
    expect(error.message).toContain('500')
  })

  it('写操作带上 CSRF 头与 JSON Content-Type', async () => {
    setCsrfToken('token-abc')
    fetchMock.mockResolvedValue(jsonResponse(202, { id: 'op-1' }))

    await api.startVerify('web-01', 'latest')

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    const headers = init.headers as Record<string, string>
    expect(headers['X-CSRF-Token']).toBe('token-abc')
    expect(headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body as string)).toEqual({ snapshot: 'latest' })
  })

  it('恢复预检与执行使用后端要求的 action 判别字段', async () => {
    // 每次调用都要新建 Response：Response 的 body 只能被消费一次。
    fetchMock.mockImplementation(() => Promise.resolve(jsonResponse(202, { id: 'op-2' })))

    await api.startRestorePreview('web-01', {
      destination_host: 'web-02',
      snapshot: 'latest',
      mode: 'isolate',
    })
    expect(JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string)).toEqual({
      action: 'preview',
      destination_host: 'web-02',
      snapshot: 'latest',
      mode: 'isolate',
    })

    await api.executeRestore('web-01', {
      preview_operation_id: 'op-2',
      confirmation_token: 'tok',
    })
    expect(JSON.parse((fetchMock.mock.calls[1]![1] as RequestInit).body as string)).toEqual({
      action: 'execute',
      preview_operation_id: 'op-2',
      confirmation_token: 'tok',
    })
  })

  it('session 会顺带刷新客户端持有的 CSRF token', async () => {
    fetchMock
      .mockResolvedValueOnce(
        jsonResponse(200, { authenticated: true, username: 'admin', csrf_token: 'fresh' }),
      )
      .mockResolvedValueOnce(jsonResponse(202, { id: 'op-3' }))

    await api.session()
    await api.startBackup('web-01')

    const headers = (fetchMock.mock.calls[1]![1] as RequestInit).headers as Record<string, string>
    expect(headers['X-CSRF-Token']).toBe('fresh')
  })

  it('空查询参数不会拼进 URL，避免触发后端的未知参数拒绝', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { items: [], next_cursor: null }))
    await api.operations({})
    expect(fetchMock.mock.calls[0]![0]).toBe('/api/operations')
  })

  // 带空 CSRF 头发出去只会换来后端的 403 invalid_request，界面显示「请求参数无效」，
  // 把会话问题伪装成参数问题。必须在发请求前就拦住并说清真实原因。
  it('CSRF token 未就绪时拒绝发出写请求', async () => {
    setCsrfToken('')

    const error = (await api.startBackup('web-01').catch((cause: unknown) => cause)) as ApiError

    expect(error).toBeInstanceOf(ApiError)
    expect(error.message).toContain('会话未就绪')
    expect(fetchMock).not.toHaveBeenCalled()
  })
})
