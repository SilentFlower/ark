<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'

import type { Operation, RestoreMode } from '@/api/types'
import OperationResult from '@/components/OperationResult.vue'
import { useOperationsStore } from '@/stores/operations'

const props = defineProps<{ host: string; hostNames: string[] }>()

const operations = useOperationsStore()

const destination = ref(props.host)
const snapshot = ref('latest')
const mode = ref<RestoreMode>('isolate')

const previewOperation = ref<Operation | null>(null)
const restoreOperation = ref<Operation | null>(null)
const forceAcknowledgement = ref('')
const confirmDialogOpen = ref(false)

/** 每秒推进一次，用于渲染确认 token 的剩余时间。 */
const nowMs = ref(Date.now())
const ticker = window.setInterval(() => {
  nowMs.value = Date.now()
}, 1000)
onBeforeUnmount(() => window.clearInterval(ticker))

const confirmationRemainingMs = computed(() => {
  const confirmation = operations.confirmation
  return confirmation ? Math.max(0, confirmation.expiresAt - nowMs.value) : 0
})

const confirmationRemaining = computed(() => {
  const total = Math.floor(confirmationRemainingMs.value / 1000)
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, '0')}`
})

/** force 模式要求手动输入目标主机名，防止误点覆盖生产。 */
const forceUnlocked = computed(
  () => mode.value !== 'force' || forceAcknowledgement.value.trim() === destination.value,
)

const canExecute = computed(
  () =>
    previewOperation.value?.status === 'ok' &&
    operations.confirmation !== null &&
    confirmationRemainingMs.value > 0 &&
    forceUnlocked.value &&
    !operations.busy,
)

const modeDescriptions: Record<RestoreMode, string> = {
  isolate: '派生独立的 compose project、容器、卷与网络，不碰生产资源',
  normal: '恢复到目标机原位；发现同名容器或卷会直接失败，不覆盖',
  force: '覆盖目标机上的既有资源；执行前先给现状打一份安全备份',
}

async function runPreview(): Promise<void> {
  restoreOperation.value = null
  previewOperation.value = await operations.startRestorePreview(props.host, {
    destination_host: destination.value,
    snapshot: snapshot.value.trim(),
    mode: mode.value,
  })
}

function requestExecute(): void {
  if (!canExecute.value) {
    return
  }
  confirmDialogOpen.value = true
}

async function confirmExecute(): Promise<void> {
  confirmDialogOpen.value = false
  restoreOperation.value = await operations.executeRestore(props.host)
  // 确认已被消费，预检结果不再可用于第二次执行。
  previewOperation.value = null
  forceAcknowledgement.value = ''
}
</script>

<template>
  <section class="rounded-lg border border-slate-200 bg-white p-4">
    <h2 class="font-medium">恢复</h2>
    <p class="mt-1 text-sm text-slate-500">
      先做只读预检，确认冲突与计划之后才能执行。
    </p>

    <div class="mt-4 grid gap-3 sm:grid-cols-3">
      <label class="block text-sm">
        <span class="text-slate-600">恢复到</span>
        <select v-model="destination" class="mt-1 w-full rounded border border-slate-300 px-2 py-1.5">
          <option v-for="name in hostNames" :key="name" :value="name">{{ name }}</option>
        </select>
      </label>
      <label class="block text-sm">
        <span class="text-slate-600">快照</span>
        <input
          v-model="snapshot"
          class="mt-1 w-full rounded border border-slate-300 px-2 py-1.5"
          placeholder="latest"
        >
      </label>
      <label class="block text-sm">
        <span class="text-slate-600">模式</span>
        <select v-model="mode" class="mt-1 w-full rounded border border-slate-300 px-2 py-1.5">
          <option value="isolate">isolate（隔离）</option>
          <option value="normal">normal（原位，冲突即失败）</option>
          <option value="force">force（覆盖生产）</option>
        </select>
      </label>
    </div>

    <p class="mt-2 text-xs" :class="mode === 'force' ? 'text-red-700' : 'text-slate-500'">
      {{ modeDescriptions[mode] }}
    </p>

    <button
      type="button"
      class="mt-4 rounded bg-slate-800 px-4 py-1.5 text-sm text-white hover:bg-slate-700 disabled:opacity-50"
      :disabled="operations.busy || snapshot.trim() === ''"
      @click="runPreview()"
    >
      {{ operations.busy ? '执行中…' : '开始预检' }}
    </button>

    <p
      v-if="operations.error"
      class="mt-3 rounded border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700"
    >
      {{ operations.error }}
    </p>

    <div v-if="previewOperation" class="mt-5 border-t border-slate-100 pt-4">
      <h3 class="mb-2 text-sm font-medium">预检结果</h3>
      <OperationResult :operation="previewOperation" />

      <div v-if="previewOperation.status === 'ok' && operations.confirmation" class="mt-4">
        <p class="rounded border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-900">
          确认有效期剩余 <span class="font-mono">{{ confirmationRemaining }}</span>。
          <!--
            token 只存在内存且只能消费一次，刷新页面就拿不回来了。
            不写在这里，用户会以为刷新一下还能继续。
          -->
          刷新或关闭页面会作废本次确认，需要重新预检。
        </p>

        <label v-if="mode === 'force'" class="mt-3 block text-sm">
          <span class="font-medium text-red-800">
            force 会覆盖 {{ destination }} 上的生产数据。请输入目标主机名以解锁：
          </span>
          <input
            v-model="forceAcknowledgement"
            class="mt-1 w-full rounded border border-red-300 px-2 py-1.5"
            :placeholder="destination"
          >
        </label>

        <button
          type="button"
          class="mt-3 rounded px-4 py-1.5 text-sm text-white disabled:opacity-50"
          :class="mode === 'force' ? 'bg-red-700 hover:bg-red-600' : 'bg-sky-700 hover:bg-sky-600'"
          :disabled="!canExecute"
          @click="requestExecute()"
        >
          执行恢复
        </button>
      </div>
    </div>

    <div v-if="restoreOperation" class="mt-5 border-t border-slate-100 pt-4">
      <h3 class="mb-2 text-sm font-medium">恢复结果</h3>
      <OperationResult :operation="restoreOperation" />
    </div>

    <div
      v-if="confirmDialogOpen"
      class="fixed inset-0 z-10 flex items-center justify-center bg-slate-900/40 p-4"
    >
      <div class="w-full max-w-md rounded-lg bg-white p-5 shadow-lg">
        <h3 class="text-lg font-semibold" :class="mode === 'force' ? 'text-red-800' : ''">
          {{ mode === 'force' ? '确认覆盖生产数据？' : '确认执行恢复？' }}
        </h3>
        <p v-if="mode === 'force'" class="mt-2 text-sm text-red-700">
          即将<strong>覆盖 {{ destination }} 上的生产数据</strong>。恢复前会自动给现状打一份安全备份，
          但这仍是一次高风险操作。
        </p>
        <p v-else class="mt-2 text-sm text-slate-600">
          将按预检计划把 {{ host }} 的快照恢复到 {{ destination }}（{{ mode }} 模式）。
        </p>
        <div class="mt-4 flex justify-end gap-2">
          <button
            type="button"
            class="rounded border border-slate-300 px-3 py-1.5 text-sm"
            @click="confirmDialogOpen = false"
          >
            取消
          </button>
          <button
            type="button"
            class="rounded px-3 py-1.5 text-sm text-white"
            :class="mode === 'force' ? 'bg-red-700' : 'bg-sky-700'"
            @click="confirmExecute()"
          >
            确认执行
          </button>
        </div>
      </div>
    </div>
  </section>
</template>
