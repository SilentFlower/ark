/**
 * ark-hub API 客户端。
 *
 * 所有请求都同源发出，靠 `HttpOnly` + `SameSite=Strict` 的会话 Cookie 鉴权，
 * 前端不持有也不存储任何凭证。写操作必须带 `X-CSRF-Token`，值来自
 * `GET /api/session`。
 */

import type {
  Alert,
  HostDetail,
  HostSummary,
  Operation,
  Page,
  RestoreMode,
  Run,
  SessionInfo,
  ApiErrorCode,
} from './types'

/** 后端返回的结构化错误。`code` 是稳定契约，`message` 面向用户。 */
export class ApiError extends Error {
  readonly code: ApiErrorCode
  readonly httpStatus: number

  constructor(httpStatus: number, code: ApiErrorCode, message: string) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.httpStatus = httpStatus
  }
}

/** 会话失效时的跳转动作。抽成变量是为了让单测能在无导航的环境里断言。 */
let unauthorizedHandler: () => void = () => {
  window.location.assign('/login')
}

/**
 * 覆盖会话失效处理。仅供测试注入使用。
 * @param handler 收到 401 时执行的动作。
 */
export function setUnauthorizedHandler(handler: () => void): void {
  unauthorizedHandler = handler
}

let csrfToken = ''

/**
 * 记录当前会话的 CSRF token，供后续写操作使用。
 * @param token `GET /api/session` 返回的 csrf_token。
 */
export function setCsrfToken(token: string): void {
  csrfToken = token
}

/** 已知错误码集合，用于把后端未来新增的码安全地归入 unknown。 */
const knownCodes: ReadonlySet<string> = new Set<ApiErrorCode>([
  'invalid_request',
  'not_found',
  'conflict',
  'confirmation_required',
  'confirmation_expired',
  'operation_failed',
  'service_unavailable',
])

/**
 * 把响应体解析为稳定的 ApiError。
 *
 * 后端对未认证请求返回的是 `{"authenticated":false}` 而不是标准错误体，
 * 因此这里不能假设错误体一定存在。
 */
async function toApiError(response: Response): Promise<ApiError> {
  let code = 'unknown'
  let message = `请求失败（HTTP ${response.status}）`
  try {
    const body = (await response.json()) as { error?: { code?: string; message?: string } }
    if (body?.error?.code) {
      code = body.error.code
    }
    if (body?.error?.message) {
      message = body.error.message
    }
  } catch {
    // 非 JSON 响应保持默认文案。这里刻意不回显响应体，避免把后端细节泄露到界面。
  }
  return new ApiError(
    response.status,
    knownCodes.has(code) ? (code as ApiErrorCode) : 'unknown',
    message,
  )
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    credentials: 'same-origin',
    headers: {
      Accept: 'application/json',
      ...(init?.headers ?? {}),
    },
  })
  if (response.status === 401) {
    unauthorizedHandler()
    throw new ApiError(401, 'unknown', '会话已失效，请重新登录')
  }
  if (!response.ok) {
    throw await toApiError(response)
  }
  return (await response.json()) as T
}

/** 发送带 CSRF 与 JSON body 的写请求。 */
async function post<T>(path: string, body: unknown): Promise<T> {
  // 会话没加载成功时 csrfToken 是空串。带着空头发出去只会换来后端的
  // 403 invalid_request，界面显示「请求参数无效」——把会话问题伪装成参数问题，
  // 用户会一直重试而不知道该刷新。这里提前拦住并说清真实原因。
  if (csrfToken === '') {
    throw new ApiError(403, 'unknown', '会话未就绪，请刷新页面后重试')
  }
  return request<T>(path, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-CSRF-Token': csrfToken,
    },
    body: JSON.stringify(body),
  })
}

/** 把查询参数拼进 URL，跳过空值，避免触发后端的未知参数拒绝。 */
function withQuery(path: string, params: Record<string, string | number | undefined>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== '') {
      search.set(key, String(value))
    }
  }
  const query = search.toString()
  return query ? `${path}?${query}` : path
}

export const api = {
  /** 读取当前会话，同时刷新客户端持有的 CSRF token。 */
  async session(): Promise<SessionInfo> {
    const info = await request<SessionInfo>('/api/session')
    setCsrfToken(info.csrf_token)
    return info
  },

  async hosts(): Promise<HostSummary[]> {
    const page = await request<{ items: HostSummary[] }>('/api/hosts')
    return page.items
  },

  async host(name: string): Promise<HostDetail> {
    return request<HostDetail>(`/api/hosts/${encodeURIComponent(name)}`)
  },

  async alerts(): Promise<Alert[]> {
    const page = await request<{ items: Alert[] }>('/api/alerts')
    return page.items
  },

  /** 全局运行历史。当前没有页面消费，保留给后续的跨机时间线。 */
  async runs(params: { host?: string; status?: string; limit?: number; cursor?: string } = {}) {
    return request<Page<Run>>(withQuery('/api/runs', params))
  },

  async operations(
    params: { host?: string; kind?: string; status?: string; limit?: number; cursor?: string } = {},
  ): Promise<Page<Operation>> {
    return request<Page<Operation>>(withQuery('/api/operations', params))
  },

  async operation(id: string): Promise<Operation> {
    return request<Operation>(`/api/operations/${encodeURIComponent(id)}`)
  },

  async startBackup(host: string): Promise<Operation> {
    return post<Operation>(`/api/hosts/${encodeURIComponent(host)}/backup`, {})
  },

  async startVerify(host: string, snapshot: string): Promise<Operation> {
    return post<Operation>(`/api/hosts/${encodeURIComponent(host)}/verify`, { snapshot })
  },

  async startRestorePreview(
    host: string,
    input: { destination_host: string; snapshot: string; mode: RestoreMode },
  ): Promise<Operation> {
    return post<Operation>(`/api/hosts/${encodeURIComponent(host)}/restore`, {
      action: 'preview',
      ...input,
    })
  },

  async executeRestore(
    host: string,
    input: { preview_operation_id: string; confirmation_token: string },
  ): Promise<Operation> {
    return post<Operation>(`/api/hosts/${encodeURIComponent(host)}/restore`, {
      action: 'execute',
      ...input,
    })
  },
}
