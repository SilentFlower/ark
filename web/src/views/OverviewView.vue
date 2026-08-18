<script setup lang="ts">
import { onMounted } from 'vue'
import { RouterLink } from 'vue-router'

import HealthBadge from '@/components/HealthBadge.vue'
import Sparkline from '@/components/Sparkline.vue'
import StatusPill from '@/components/StatusPill.vue'
import { formatBytes, formatRelative, formatTime } from '@/lib/format'
import { useAlertsStore } from '@/stores/alerts'
import { useHostsStore } from '@/stores/hosts'

const hosts = useHostsStore()
const alerts = useAlertsStore()

onMounted(() => {
  void hosts.loadSummaries()
})

/** 调度不可解析时后端把健康度置为 unknown，页面必须说清原因而不是显示一个假的下次时间。 */
function scheduleUnavailable(diagnostics: string[]): boolean {
  return diagnostics.includes('schedule_unavailable')
}
</script>

<template>
  <div class="p-6">
    <header class="mb-5 flex items-baseline justify-between">
      <div>
        <h1 class="text-xl font-semibold">总览</h1>
        <p class="mt-1 text-sm text-slate-500">共 {{ hosts.summaries.length }} 台机器</p>
      </div>
      <button
        type="button"
        class="rounded border border-slate-300 bg-white px-3 py-1.5 text-sm hover:bg-slate-50"
        :disabled="hosts.loading"
        @click="hosts.loadSummaries()"
      >
        刷新
      </button>
    </header>

    <RouterLink
      v-if="alerts.count > 0"
      to="/alerts"
      class="mb-5 flex items-center gap-2 rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-800"
    >
      <span class="font-medium">当前有 {{ alerts.count }} 条告警</span>
      <span class="text-red-600">查看详情 →</span>
    </RouterLink>

    <p v-if="hosts.error" class="mb-4 rounded border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
      {{ hosts.error }}
    </p>

    <div class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
      <RouterLink
        v-for="host in hosts.summaries"
        :key="host.host"
        :to="`/hosts/${host.host}`"
        class="block rounded-lg border border-slate-200 bg-white p-4 transition hover:border-slate-300 hover:shadow-sm"
      >
        <div class="flex items-start justify-between">
          <div class="min-w-0">
            <p class="flex items-center gap-2 font-medium">
              <span class="truncate">{{ host.host }}</span>
              <span
                v-if="host.local"
                class="rounded bg-slate-100 px-1.5 py-0.5 text-xs font-normal text-slate-600"
              >
                hub 本机
              </span>
            </p>
            <p class="mt-0.5 truncate text-xs text-slate-500">
              {{ host.project }} · {{ host.target_count }} 个目标
            </p>
          </div>
          <HealthBadge :health="host.health" />
        </div>

        <dl class="mt-4 space-y-2 text-sm">
          <div class="flex items-center justify-between">
            <dt class="text-slate-500">最近备份</dt>
            <dd class="flex items-center gap-2">
              <StatusPill :status="host.last_backup_status" />
              <span class="text-slate-600">{{ formatRelative(host.last_successful_backup_at) }}</span>
            </dd>
          </div>
          <div class="flex items-center justify-between">
            <dt class="text-slate-500">最近演练</dt>
            <dd><StatusPill :status="host.last_verification_status" /></dd>
          </div>
          <div class="flex items-center justify-between">
            <dt class="text-slate-500">下次计划</dt>
            <dd class="text-slate-600">
              <span v-if="scheduleUnavailable(host.diagnostics)" class="text-amber-700">
                计划不可解析
              </span>
              <span v-else>{{ formatTime(host.next_run_at) }}</span>
            </dd>
          </div>
        </dl>

        <div class="mt-4 border-t border-slate-100 pt-3">
          <div class="flex items-end justify-between">
            <div>
              <p class="text-xs text-slate-500">最近备份大小</p>
              <p class="text-sm font-medium">{{ formatBytes(host.last_backup_bytes) }}</p>
            </div>
            <Sparkline :points="host.recent_backup_sizes" />
          </div>
        </div>
      </RouterLink>
    </div>

    <p v-if="!hosts.loading && hosts.summaries.length === 0" class="text-sm text-slate-500">
      清单里还没有配置任何主机。
    </p>
  </div>
</template>
