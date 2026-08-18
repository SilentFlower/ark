<script setup lang="ts">
import { computed } from 'vue'

import type {
  BackupResult,
  Operation,
  RestorePreviewResult,
  RestoreResult,
  VerifyResult,
} from '@/api/types'

const props = defineProps<{ operation: Operation }>()

/**
 * 按 kind 取出结构化结果。
 *
 * 后端对每种 kind 都做了字段白名单校验（operationResultFields），
 * 所以这里可以安全地按 kind 断言，而不需要逐字段防御。
 */
const backup = computed(() =>
  props.operation.kind === 'backup' ? (props.operation.result as BackupResult | null) : null,
)
const verify = computed(() =>
  props.operation.kind === 'verify' ? (props.operation.result as VerifyResult | null) : null,
)
const preview = computed(() =>
  props.operation.kind === 'restore_preview'
    ? (props.operation.result as RestorePreviewResult | null)
    : null,
)
const restore = computed(() =>
  props.operation.kind === 'restore' ? (props.operation.result as RestoreResult | null) : null,
)
</script>

<template>
  <div class="space-y-3 text-sm">
    <p v-if="operation.error" class="rounded border border-red-200 bg-red-50 px-3 py-2 text-red-700">
      {{ operation.error }}
    </p>

    <dl v-if="backup" class="grid grid-cols-[8rem_1fr] gap-y-1">
      <dt class="text-slate-500">run id</dt>
      <dd class="font-mono text-xs">{{ backup.run_id }}</dd>
      <dt class="text-slate-500">manifest 快照</dt>
      <dd class="font-mono text-xs">{{ backup.manifest_snapshot_id || '—' }}</dd>
    </dl>

    <dl v-else-if="verify" class="grid grid-cols-[8rem_1fr] gap-y-1">
      <dt class="text-slate-500">manifest 快照</dt>
      <dd class="font-mono text-xs">{{ verify.manifest_snapshot_id || '—' }}</dd>
      <dt class="text-slate-500">检查项</dt>
      <dd>{{ verify.results?.length ?? 0 }} 项</dd>
    </dl>

    <div v-else-if="preview" class="space-y-3">
      <dl class="grid grid-cols-[8rem_1fr] gap-y-1">
        <dt class="text-slate-500">来源 → 目标</dt>
        <dd>{{ preview.plan.source_host }} → {{ preview.plan.destination_host }}</dd>
        <dt class="text-slate-500">manifest 快照</dt>
        <dd class="font-mono text-xs">{{ preview.plan.manifest_snapshot_id }}</dd>
        <dt class="text-slate-500">步骤数</dt>
        <dd>{{ preview.plan.steps?.length ?? 0 }}</dd>
        <dt class="text-slate-500">写入生产</dt>
        <dd :class="preview.destructive ? 'font-medium text-red-700' : 'text-emerald-700'">
          {{ preview.destructive ? '是（会写入非隔离资源）' : '否（隔离恢复）' }}
        </dd>
        <dt v-if="preview.resume" class="text-slate-500">续跑</dt>
        <dd v-if="preview.resume">目标机存在同一计划的可恢复状态</dd>
      </dl>

      <div v-if="preview.conflicts?.length">
        <p class="mb-1 font-medium text-amber-800">目标冲突（{{ preview.conflicts.length }}）</p>
        <ul class="space-y-1">
          <li
            v-for="conflict in preview.conflicts"
            :key="conflict.resource"
            class="rounded border border-amber-200 bg-amber-50 px-3 py-2"
          >
            <p class="font-mono text-xs">{{ conflict.resource }}</p>
            <p class="text-xs text-slate-600">{{ conflict.detail }}</p>
            <p class="text-xs" :class="conflict.force_allowed ? 'text-amber-700' : 'text-red-700'">
              {{ conflict.force_allowed ? 'force 模式可在安全备份后处理' : 'force 也无法处理' }}
            </p>
          </li>
        </ul>
      </div>

      <div v-if="preview.plan.manual_checks?.length">
        <p class="mb-1 font-medium">人工确认清单</p>
        <ul class="list-disc space-y-0.5 pl-5 text-slate-700">
          <li v-for="check in preview.plan.manual_checks" :key="check">{{ check }}</li>
        </ul>
      </div>
    </div>

    <div v-else-if="restore" class="space-y-3">
      <dl class="grid grid-cols-[8rem_1fr] gap-y-1">
        <dt class="text-slate-500">来源 → 目标</dt>
        <dd>{{ restore.source_host }} → {{ restore.destination_host }}</dd>
        <dt class="text-slate-500">manifest 快照</dt>
        <dd class="font-mono text-xs">{{ restore.manifest_snapshot_id }}</dd>
        <dt class="text-slate-500">步骤数</dt>
        <dd>{{ restore.steps?.length ?? 0 }}</dd>
      </dl>
      <!--
        人工确认清单是恢复流程的真正终点：DNS、证书、防火墙和 .env 不会自动就位。
        因此它必须直接展示，而不是折叠进原始 JSON。
      -->
      <div v-if="restore.manual_checks?.length" class="rounded border border-sky-200 bg-sky-50 px-3 py-2">
        <p class="mb-1 font-medium text-sky-900">恢复后仍需人工处理</p>
        <ul class="list-disc space-y-0.5 pl-5 text-slate-700">
          <li v-for="check in restore.manual_checks" :key="check">{{ check }}</li>
        </ul>
      </div>
    </div>

    <p v-else-if="operation.status === 'running'" class="text-slate-500">任务仍在运行…</p>
    <p v-else class="text-slate-500">没有结构化结果。</p>

    <p v-if="operation.exit_code !== null" class="text-xs text-slate-400">
      ark 退出码：{{ operation.exit_code }}
    </p>
    <p v-if="backup && backup.status" class="text-xs text-slate-400">
      备份状态：{{ backup.status }}<span v-if="backup.manifest"> · manifest 已写入</span>
    </p>
    <p v-if="preview" class="text-xs text-slate-400">
      预检摘要 digest：<span class="font-mono">{{ preview.digest.slice(0, 16) }}…</span>
    </p>
  </div>
</template>
