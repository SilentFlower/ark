<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'

import HealthBadge from '@/components/HealthBadge.vue'
import OperationResult from '@/components/OperationResult.vue'
import RestorePanel from '@/components/RestorePanel.vue'
import Sparkline from '@/components/Sparkline.vue'
import StatusPill from '@/components/StatusPill.vue'
import { formatBytes, formatDuration, formatTime } from '@/lib/format'
import { useHostsStore } from '@/stores/hosts'
import { useOperationsStore } from '@/stores/operations'

const props = defineProps<{ host: string }>()

const hosts = useHostsStore()
const operations = useOperationsStore()
const expandedRun = ref<string | null>(null)
const verifySnapshot = ref('latest')

const detail = computed(() => hosts.details[props.host] ?? null)

/** 只展示属于本主机的操作，避免切换主机时看到上一台的操作结果。 */
const hostOperation = computed(() => operations.activeFor(props.host))

onMounted(() => {
  void reload()
})

watch(
  () => props.host,
  () => {
    void reload()
  },
)

async function reload(): Promise<void> {
  if (hosts.summaries.length === 0) {
    await hosts.loadSummaries()
  }
  await hosts.loadDetail(props.host)
}

async function runBackup(): Promise<void> {
  await operations.startBackup(props.host)
  await hosts.loadDetail(props.host)
}

async function runVerify(): Promise<void> {
  await operations.startVerify(props.host, verifySnapshot.value.trim())
  await hosts.loadDetail(props.host)
}

/** 一次 run 的总字节，用于历史表格里的体积列。 */
function runBytes(targets: { bytes: number }[]): number {
  return targets.reduce((total, target) => total + target.bytes, 0)
}
</script>

<template>
  <div class="p-6">
    <RouterLink to="/" class="text-sm text-sky-700 underline">← 返回总览</RouterLink>

    <template v-if="detail">
      <header class="mt-3 mb-5 flex items-start justify-between">
        <div>
          <h1 class="flex items-center gap-2 text-xl font-semibold">
            {{ detail.summary.host }}
            <span
              v-if="detail.summary.local"
              class="rounded bg-slate-100 px-1.5 py-0.5 text-xs font-normal text-slate-600"
            >
              hub 本机
            </span>
          </h1>
          <p class="mt-1 text-sm text-slate-500">
            {{ detail.summary.project }} · 调度 {{ detail.summary.schedule }}
          </p>
        </div>
        <HealthBadge :health="detail.summary.health" />
      </header>

      <div class="grid gap-4 lg:grid-cols-3">
        <section class="rounded-lg border border-slate-200 bg-white p-4">
          <h2 class="mb-3 font-medium">概况</h2>
          <dl class="space-y-2 text-sm">
            <div class="flex justify-between">
              <dt class="text-slate-500">最近备份</dt>
              <dd><StatusPill :status="detail.summary.last_backup_status" /></dd>
            </div>
            <div class="flex justify-between">
              <dt class="text-slate-500">最近成功</dt>
              <dd>{{ formatTime(detail.summary.last_successful_backup_at) }}</dd>
            </div>
            <div class="flex justify-between">
              <dt class="text-slate-500">下次计划</dt>
              <dd>
                <span
                  v-if="detail.summary.diagnostics.includes('schedule_unavailable')"
                  class="text-amber-700"
                >
                  计划不可解析
                </span>
                <span v-else>{{ formatTime(detail.summary.next_run_at) }}</span>
              </dd>
            </div>
            <div class="flex items-center justify-between">
              <dt class="text-slate-500">最近大小</dt>
              <dd>{{ formatBytes(detail.summary.last_backup_bytes) }}</dd>
            </div>
          </dl>
          <div class="mt-3 border-t border-slate-100 pt-3">
            <Sparkline :points="detail.summary.recent_backup_sizes" :width="220" />
          </div>
        </section>

        <section class="rounded-lg border border-slate-200 bg-white p-4">
          <h2 class="mb-3 font-medium">备份目标（{{ detail.targets.length }}）</h2>
          <ul class="space-y-1.5 text-sm">
            <li v-for="target in detail.targets" :key="target.id" class="flex justify-between gap-2">
              <span class="truncate font-mono text-xs">{{ target.id }}</span>
              <span class="shrink-0 text-slate-500">{{ target.type }}</span>
            </li>
          </ul>
        </section>

        <section class="rounded-lg border border-slate-200 bg-white p-4">
          <h2 class="mb-3 font-medium">操作</h2>
          <button
            type="button"
            class="w-full rounded bg-slate-800 px-4 py-1.5 text-sm text-white hover:bg-slate-700 disabled:opacity-50"
            :disabled="operations.busy"
            @click="runBackup()"
          >
            立即备份
          </button>
          <div class="mt-3 flex gap-2">
            <input
              v-model="verifySnapshot"
              class="min-w-0 flex-1 rounded border border-slate-300 px-2 py-1.5 text-sm"
              placeholder="latest"
            >
            <button
              type="button"
              class="shrink-0 rounded border border-slate-300 px-3 py-1.5 text-sm hover:bg-slate-50 disabled:opacity-50"
              :disabled="operations.busy || verifySnapshot.trim() === ''"
              @click="runVerify()"
            >
              发起演练
            </button>
          </div>
          <p v-if="operations.busy" class="mt-2 text-xs text-slate-500">
            任务执行中，同一时刻只允许一个手工任务。
          </p>
          <div v-if="hostOperation" class="mt-3 border-t border-slate-100 pt-3">
            <div class="mb-2 flex items-center justify-between text-sm">
              <span class="text-slate-500">最近一次操作</span>
              <StatusPill :status="hostOperation.status" />
            </div>
            <OperationResult :operation="hostOperation" />
          </div>
        </section>
      </div>

      <section class="mt-4 rounded-lg border border-slate-200 bg-white p-4">
        <h2 class="mb-3 font-medium">备份历史</h2>
        <table class="w-full text-sm">
          <thead class="text-left text-xs uppercase text-slate-500">
            <tr>
              <th class="pb-2 font-medium">开始时间</th>
              <th class="pb-2 font-medium">状态</th>
              <th class="pb-2 font-medium">体积</th>
              <th class="pb-2 font-medium">耗时</th>
              <th class="pb-2" />
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-100">
            <template v-for="item in detail.runs" :key="item.run.id">
              <tr>
                <td class="py-2">{{ formatTime(item.run.started_at) }}</td>
                <td class="py-2"><StatusPill :status="item.status" /></td>
                <td class="py-2">{{ formatBytes(runBytes(item.targets)) }}</td>
                <td class="py-2 text-slate-600">{{ formatDuration(item.run.duration_ms) }}</td>
                <td class="py-2 text-right">
                  <button
                    type="button"
                    class="text-xs text-sky-700 underline"
                    @click="expandedRun = expandedRun === item.run.id ? null : item.run.id"
                  >
                    {{ expandedRun === item.run.id ? '收起' : `${item.targets.length} 个目标` }}
                  </button>
                </td>
              </tr>
              <tr v-if="expandedRun === item.run.id">
                <td colspan="5" class="bg-slate-50 px-3 py-2">
                  <table class="w-full text-xs">
                    <tbody>
                      <tr v-for="target in item.targets" :key="target.target_id">
                        <td class="py-1 font-mono">{{ target.target_id }}</td>
                        <td class="py-1"><StatusPill :status="target.status" /></td>
                        <td class="py-1">{{ formatBytes(target.bytes) }}</td>
                        <td class="py-1">{{ formatDuration(target.duration_ms) }}</td>
                        <td class="py-1 font-mono text-slate-500">
                          {{ target.snapshot_id ? target.snapshot_id.slice(0, 12) : '—' }}
                        </td>
                        <td class="py-1 text-red-700">{{ target.error }}</td>
                      </tr>
                    </tbody>
                  </table>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
        <p v-if="detail.runs.length === 0" class="py-4 text-center text-sm text-slate-500">
          还没有备份记录。
        </p>
      </section>

      <div class="mt-4 grid gap-4 lg:grid-cols-2">
        <section class="rounded-lg border border-slate-200 bg-white p-4">
          <h2 class="mb-3 flex items-center justify-between font-medium">
            <span>doctor 报告</span>
            <StatusPill v-if="detail.doctor" :status="detail.doctor.status" />
          </h2>
          <p v-if="!detail.doctor" class="text-sm text-slate-500">还没有该主机的 doctor 报告。</p>
          <template v-else>
            <p class="mb-2 text-xs text-slate-500">{{ formatTime(detail.doctor.created_at) }}</p>
            <ul class="space-y-1.5 text-sm">
              <li
                v-for="check in detail.doctor.report.checks"
                :key="check.name"
                class="flex items-start justify-between gap-3"
              >
                <div class="min-w-0">
                  <p class="truncate">{{ check.name }}</p>
                  <p class="truncate text-xs text-slate-500">{{ check.detail }}</p>
                </div>
                <StatusPill :status="check.status" />
              </li>
            </ul>
          </template>
        </section>

        <section class="rounded-lg border border-slate-200 bg-white p-4">
          <h2 class="mb-3 font-medium">恢复演练</h2>
          <p v-if="detail.verifications.length === 0" class="text-sm text-slate-500">
            还没有演练记录。
          </p>
          <ul v-else class="space-y-2 text-sm">
            <li
              v-for="verification in detail.verifications"
              :key="verification.id"
              class="flex items-center justify-between gap-3"
            >
              <div class="min-w-0">
                <p>{{ formatTime(verification.started_at) }}</p>
                <p class="truncate font-mono text-xs text-slate-500">
                  {{ verification.snapshot_id.slice(0, 12) }}
                  · {{ formatDuration(verification.duration_ms) }}
                </p>
                <p v-if="verification.error" class="text-xs text-red-700">{{ verification.error }}</p>
              </div>
              <StatusPill :status="verification.status" />
            </li>
          </ul>
        </section>
      </div>

      <div class="mt-4">
        <RestorePanel :host="props.host" :host-names="hosts.hostNames" />
      </div>
    </template>

    <p v-else-if="hosts.error" class="mt-4 rounded border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
      {{ hosts.error }}
    </p>
    <p v-else class="mt-4 text-sm text-slate-500">加载中…</p>
  </div>
</template>
