import { createRouter, createWebHistory } from 'vue-router'

import AlertsView from '@/views/AlertsView.vue'
import HostDetailView from '@/views/HostDetailView.vue'
import OperationsView from '@/views/OperationsView.vue'
import OverviewView from '@/views/OverviewView.vue'

/**
 * history 模式路由。
 *
 * 依赖后端对未知非 API 路径返回 index.html 的 SPA fallback；
 * `/api/` 前缀仍由后端返回 404 JSON，不会落到这里。
 */
export default createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'overview', component: OverviewView },
    { path: '/hosts/:host', name: 'host', component: HostDetailView, props: true },
    { path: '/alerts', name: 'alerts', component: AlertsView },
    { path: '/operations', name: 'operations', component: OperationsView },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})
