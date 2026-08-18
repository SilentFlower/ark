<script setup lang="ts">
import { onMounted, ref } from 'vue'

import OperationResult from '@/components/OperationResult.vue'
import StatusPill from '@/components/StatusPill.vue'
import { formatDuration, formatTime } from '@/lib/format'
import { useOperationsStore } from '@/stores/operations'

const operations = useOperationsStore()
const expanded = ref<string | null>(null)

onMounted(() => {
  void operations.loadHistory()
})

const kindLabels: Record<string, string> = {
  backup: '备份',
  verify: '演练',
  restore_preview: '恢复预检',
  restore: '恢复',
}

function toggle(id: string): void {
  expanded.value = expanded.value === id ? null : id
}
</script>

<template>
  <div class="p-6">
    <header class="mb-5 flex items-baseline justify-between">
      <div>
        <h1 class="text-xl font-semibold">操作</h1>
        <p class="mt-1 text-sm text-slate-500">从页面发起的手工任务记录</p>
      </div>
      <button
        type="button"
        class="rounded border border-slate-300 bg-white px-3 py-1.5 text-sm hover:bg-slate-50"
        :disabled="operations.loading"
        @click="operations.loadHistory()"
      >
        刷新
      </button>
    </header>

    <p
      v-if="operations.error"
      class="mb-4 rounded border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700"
    >
      {{ operations.error }}
    </p>

    <div class="overflow-hidden rounded-lg border border-slate-200 bg-white">
      <table class="w-full text-sm">
        <thead class="bg-slate-50 text-left text-xs uppercase text-slate-500">
          <tr>
            <th class="px-4 py-2 font-medium">类型</th>
            <th class="px-4 py-2 font-medium">主机</th>
            <th class="px-4 py-2 font-medium">状态</th>
            <th class="px-4 py-2 font-medium">开始时间</th>
            <th class="px-4 py-2 font-medium">耗时</th>
            <th class="px-4 py-2" />
          </tr>
        </thead>
        <tbody class="divide-y divide-slate-100">
          <template v-for="operation in operations.history" :key="operation.id">
            <tr class="hover:bg-slate-50">
              <td class="px-4 py-2">{{ kindLabels[operation.kind] ?? operation.kind }}</td>
              <td class="px-4 py-2">{{ operation.host }}</td>
              <td class="px-4 py-2"><StatusPill :status="operation.status" /></td>
              <td class="px-4 py-2 text-slate-600">{{ formatTime(operation.started_at) }}</td>
              <td class="px-4 py-2 text-slate-600">{{ formatDuration(operation.duration_ms) }}</td>
              <td class="px-4 py-2 text-right">
                <button
                  type="button"
                  class="text-xs text-sky-700 underline"
                  @click="toggle(operation.id)"
                >
                  {{ expanded === operation.id ? '收起' : '详情' }}
                </button>
              </td>
            </tr>
            <tr v-if="expanded === operation.id">
              <td colspan="6" class="bg-slate-50 px-4 py-3">
                <OperationResult :operation="operation" />
              </td>
            </tr>
          </template>
        </tbody>
      </table>

      <p
        v-if="!operations.loading && operations.history.length === 0"
        class="px-4 py-6 text-center text-sm text-slate-500"
      >
        还没有手工操作记录。
      </p>
    </div>

    <div v-if="operations.nextCursor" class="mt-4 text-center">
      <button
        type="button"
        class="rounded border border-slate-300 bg-white px-4 py-1.5 text-sm hover:bg-slate-50"
        :disabled="operations.loading"
        @click="operations.loadHistory(operations.nextCursor ?? undefined)"
      >
        加载更多
      </button>
    </div>
  </div>
</template>
