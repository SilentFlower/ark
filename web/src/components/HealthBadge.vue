<script setup lang="ts">
import { computed } from 'vue'

import type { Health } from '@/api/types'

const props = defineProps<{ health: Health }>()

/** unknown 刻意用中性灰而不是绿色：无法判断不等于通过。 */
const styles: Record<Health, { label: string; className: string }> = {
  ok: { label: '正常', className: 'bg-emerald-100 text-emerald-800' },
  warn: { label: '警告', className: 'bg-amber-100 text-amber-800' },
  fail: { label: '故障', className: 'bg-red-100 text-red-800' },
  unknown: { label: '未知', className: 'bg-slate-200 text-slate-700' },
}

const style = computed(() => styles[props.health] ?? styles.unknown)
</script>

<template>
  <span
    class="inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-medium"
    :class="style.className"
  >
    {{ style.label }}
  </span>
</template>
