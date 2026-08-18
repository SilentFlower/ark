<script setup lang="ts">
import { computed, onMounted } from 'vue'

import StatusPill from '@/components/StatusPill.vue'
import { formatRelative, formatTime } from '@/lib/format'
import { useAlertsStore } from '@/stores/alerts'

const alerts = useAlertsStore()

onMounted(() => {
  void alerts.load()
})

const kindLabels: Record<string, string> = {
  backup_overdue: '超时未备份',
  backup_consecutive_failures: '连续两次备份失败',
  verification_failed: '最近一次演练失败',
}

const grouped = computed(() => {
  const map = new Map<string, typeof alerts.items>()
  for (const alert of alerts.items) {
    const existing = map.get(alert.host) ?? []
    existing.push(alert)
    map.set(alert.host, existing)
  }
  return [...map.entries()]
})
</script>

<template>
  <div class="p-6">
    <header class="mb-5">
      <h1 class="text-xl font-semibold">告警</h1>
      <!--
        这里的措辞是刻意的：后端每次请求都用 deriveAlerts 现算，
        created_at 是投影时刻而不是告警首次出现的时间。不说清楚会让人
        误以为这是一份可以回溯的历史记录。
      -->
      <p class="mt-1 text-sm text-slate-500">
        当前生效的告警（每次刷新实时计算，不是历史流水）
      </p>
    </header>

    <p v-if="alerts.error" class="mb-4 rounded border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
      {{ alerts.error }}
    </p>

    <div
      v-if="!alerts.loading && alerts.items.length === 0"
      class="rounded-md border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm text-emerald-800"
    >
      当前没有告警。
    </div>

    <section v-for="[host, hostAlerts] in grouped" :key="host" class="mb-5">
      <h2 class="mb-2 text-sm font-medium text-slate-700">{{ host }}</h2>
      <ul class="space-y-2">
        <li
          v-for="alert in hostAlerts"
          :key="alert.id"
          class="rounded-md border border-red-200 bg-white px-4 py-3"
        >
          <div class="flex items-center justify-between">
            <p class="font-medium text-red-800">{{ kindLabels[alert.kind] ?? alert.kind }}</p>
            <StatusPill status="fail" />
          </div>
          <p class="mt-1 text-sm text-slate-600">{{ alert.message }}</p>
          <p class="mt-1 text-xs text-slate-400">
            投影于 {{ formatTime(alert.created_at) }}（{{ formatRelative(alert.created_at) }}）
          </p>
        </li>
      </ul>
    </section>
  </div>
</template>
