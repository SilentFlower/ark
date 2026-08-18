<script setup lang="ts">
import { computed } from 'vue'

import type { OperationStatus, Status } from '@/api/types'

const props = defineProps<{ status: Status | OperationStatus | null }>()

const styles: Record<string, { label: string; className: string }> = {
  running: { label: '运行中', className: 'bg-sky-100 text-sky-800' },
  ok: { label: '成功', className: 'bg-emerald-100 text-emerald-800' },
  warn: { label: '警告', className: 'bg-amber-100 text-amber-800' },
  fail: { label: '失败', className: 'bg-red-100 text-red-800' },
  // Hub 重启时仍在运行的手工任务会被标记为 interrupted，不是失败也不是成功。
  interrupted: { label: '已中断', className: 'bg-orange-100 text-orange-800' },
}

const style = computed(() =>
  props.status ? styles[props.status] : { label: '无记录', className: 'bg-slate-200 text-slate-600' },
)
</script>

<template>
  <span
    class="inline-flex items-center rounded px-2 py-0.5 text-xs font-medium"
    :class="style?.className"
  >
    {{ style?.label }}
  </span>
</template>
