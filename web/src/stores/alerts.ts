import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { api } from '@/api/client'
import type { Alert } from '@/api/types'

/**
 * 当前告警。
 *
 * `GET /api/alerts` 返回的是**实时投影**而不是历史流水：每次请求都由
 * `deriveAlerts` 现算，`created_at` 是投影时刻而非告警首次出现的时间。
 * 因此这里不做去重、不做时间线，也不缓存历史。
 */
export const useAlertsStore = defineStore('alerts', () => {
  const items = ref<Alert[]>([])
  const loading = ref(false)
  const error = ref('')

  const count = computed(() => items.value.length)

  async function load(): Promise<void> {
    loading.value = true
    error.value = ''
    try {
      items.value = await api.alerts()
    } catch (cause) {
      error.value = cause instanceof Error ? cause.message : '加载告警失败'
    } finally {
      loading.value = false
    }
  }

  return { items, loading, error, count, load }
})
