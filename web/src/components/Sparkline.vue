<script setup lang="ts">
import { computed } from 'vue'

import type { BackupSizePoint } from '@/api/types'
import { formatBytes, formatTime } from '@/lib/format'
import { buildSparkline } from '@/lib/sparkline'

const props = withDefaults(defineProps<{ points: BackupSizePoint[]; width?: number; height?: number }>(), {
  width: 132,
  height: 28,
})

const geometry = computed(() =>
  buildSparkline(
    props.points.map((point) => point.bytes),
    props.width,
    props.height,
  ),
)

/** 单点时没有趋势可言，用文字说明而不是画一条误导性的直线。 */
const singlePoint = computed(() => props.points.length === 1)

const tooltip = computed(() =>
  props.points
    .map((point) => `${formatTime(point.finished_at)}  ${formatBytes(point.bytes)}`)
    .join('\n'),
)
</script>

<template>
  <div v-if="points.length === 0" class="text-xs text-slate-400">暂无成功备份</div>
  <div v-else class="flex items-center gap-2" :title="tooltip">
    <svg
      :width="width"
      :height="height"
      :viewBox="`0 0 ${width} ${height}`"
      class="overflow-visible"
      role="img"
      aria-label="备份大小趋势"
    >
      <path
        v-if="!singlePoint"
        :d="geometry.path"
        fill="none"
        stroke="currentColor"
        stroke-width="1.5"
        class="text-sky-500"
      />
      <circle
        v-if="geometry.last"
        :cx="geometry.last.x"
        :cy="geometry.last.y"
        r="2.5"
        class="fill-sky-600"
      />
    </svg>
    <span v-if="singlePoint" class="text-xs text-slate-400">仅一次成功备份</span>
  </div>
</template>
