import { defineStore } from 'pinia'
import { ref } from 'vue'

import { api } from '@/api/client'

/**
 * 当前登录会话。
 *
 * 登录与退出都走后端既有的表单路径（`POST /login` / `POST /logout`），
 * 前端不实现登录表单，也不新增 JSON 登录端点——鉴权契约在 P4-1 已严格验收，
 * 这里刻意不动它。
 */
export const useSessionStore = defineStore('session', () => {
  const username = ref('')
  const csrfToken = ref('')
  const loaded = ref(false)
  const error = ref('')

  /**
   * 拉取会话信息并缓存 CSRF token。
   *
   * 与其它 store 一样吞掉异常并记录 `error`：会话加载失败不能中断调用方后续的加载动作。
   * `/api/session` 在 auth 存储运行期故障时返回 `503`（不是 401），那条路径不会触发
   * 客户端的登录跳转；如果这里直接抛出，调用方后面的告警加载会被跳过，
   * 侧栏告警数会停在 0——「没有告警」和「没查到告警」在这个产品里是完全不同的两件事。
   */
  async function load(): Promise<void> {
    error.value = ''
    try {
      const info = await api.session()
      username.value = info.username
      csrfToken.value = info.csrf_token
      loaded.value = true
    } catch (cause) {
      loaded.value = false
      error.value = cause instanceof Error ? cause.message : '会话加载失败'
    }
  }

  /**
   * 退出登录。
   *
   * 后端 `POST /logout` 要求 form-urlencoded + CSRF 字段并以重定向响应，
   * 因此提交一个隐藏表单而不是发 fetch：让浏览器自己跟随重定向，
   * 也顺带清掉 SPA 的全部内存状态（包括未消费的恢复确认 token）。
   */
  function logout(): void {
    const form = document.createElement('form')
    form.method = 'post'
    form.action = '/logout'
    const field = document.createElement('input')
    field.type = 'hidden'
    field.name = 'csrf_token'
    field.value = csrfToken.value
    form.appendChild(field)
    document.body.appendChild(form)
    form.submit()
  }

  return { username, csrfToken, loaded, error, load, logout }
})
