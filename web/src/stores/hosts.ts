import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { api } from '@/api/client'
import type { HostDetail, HostSummary } from '@/api/types'

/** 主机摘要与详情。摘要用于总览，详情按 host 名缓存。 */
export const useHostsStore = defineStore('hosts', () => {
  const summaries = ref<HostSummary[]>([])
  const details = ref<Record<string, HostDetail>>({})
  const loading = ref(false)
  const error = ref('')

  /** 可作为恢复目标的主机名列表。 */
  const hostNames = computed(() => summaries.value.map((item) => item.host))

  async function loadSummaries(): Promise<void> {
    loading.value = true
    error.value = ''
    try {
      summaries.value = await api.hosts()
    } catch (cause) {
      error.value = cause instanceof Error ? cause.message : '加载主机列表失败'
    } finally {
      loading.value = false
    }
  }

  async function loadDetail(host: string): Promise<void> {
    loading.value = true
    error.value = ''
    try {
      details.value = { ...details.value, [host]: await api.host(host) }
    } catch (cause) {
      error.value = cause instanceof Error ? cause.message : '加载主机详情失败'
    } finally {
      loading.value = false
    }
  }

  return { summaries, details, loading, error, hostNames, loadSummaries, loadDetail }
})
