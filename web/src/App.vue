<script setup lang="ts">
import { onMounted } from 'vue'
import { RouterLink, RouterView } from 'vue-router'

import { useAlertsStore } from '@/stores/alerts'
import { useSessionStore } from '@/stores/session'

const session = useSessionStore()
const alerts = useAlertsStore()

onMounted(async () => {
  // 两个加载互不阻塞：会话失败时告警仍要加载，否则侧栏计数会停在 0，
  // 「没有告警」和「没查到告警」会被混为一谈。
  await session.load()
  await alerts.load()
})

const navigation = [
  { to: '/', label: '总览' },
  { to: '/alerts', label: '告警' },
  { to: '/operations', label: '操作' },
]
</script>

<template>
  <div class="flex min-h-full bg-slate-50 text-slate-800">
    <aside class="flex w-52 shrink-0 flex-col border-r border-slate-200 bg-white">
      <div class="border-b border-slate-200 px-5 py-4">
        <p class="text-lg font-semibold tracking-tight">ark-hub</p>
        <p class="mt-0.5 text-xs text-slate-500">备份与重建控制台</p>
      </div>

      <nav class="flex-1 space-y-1 p-3">
        <RouterLink
          v-for="item in navigation"
          :key="item.to"
          :to="item.to"
          class="flex items-center justify-between rounded-md px-3 py-2 text-sm hover:bg-slate-100"
          active-class="bg-slate-100 font-medium text-slate-900"
        >
          <span>{{ item.label }}</span>
          <span
            v-if="item.to === '/alerts' && alerts.count > 0"
            class="rounded-full bg-red-100 px-2 py-0.5 text-xs font-medium text-red-700"
          >
            {{ alerts.count }}
          </span>
        </RouterLink>
      </nav>

      <div class="border-t border-slate-200 px-5 py-3 text-xs text-slate-500">
        <p v-if="session.error" class="mb-2 rounded border border-red-200 bg-red-50 px-2 py-1.5 text-red-700">
          会话加载失败，操作按钮不可用。请刷新页面或重新登录。
        </p>
        <p class="truncate">管理员：{{ session.username || '…' }}</p>
        <button type="button" class="mt-2 text-slate-600 underline" @click="session.logout()">
          退出登录
        </button>
      </div>
    </aside>

    <main class="min-w-0 flex-1 overflow-y-auto">
      <RouterView />
    </main>
  </div>
</template>
