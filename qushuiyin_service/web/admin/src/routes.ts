import type { RouteRecordRaw } from 'vue-router'
export const routes: RouteRecordRaw[] = [
  { path: '/', name: 'overview', component: () => import('./pages/Overview.vue'), meta: { label: '运行概览', icon: 'DataAnalysis' } },
  { path: '/download-domains', name: 'domains', component: () => import('./pages/Domains.vue'), meta: { label: '下载域名', icon: 'Connection', roles: ['tech_admin', 'super_admin'] } },
  { path: '/platforms', name: 'platforms', component: () => import('./pages/Platforms.vue'), meta: { label: '平台管理', icon: 'Switch', roles: ['operator', 'tech_admin', 'super_admin'] } },
  { path: '/failures', name: 'failures', component: () => import('./pages/Records.vue'), meta: { label: '错误记录', icon: 'Warning', roles: ['tech_admin', 'super_admin'] } },
  { path: '/feedback', name: 'feedback', component: () => import('./pages/Feedback.vue'), meta: { label: '反馈工单', icon: 'ChatDotRound', roles: ['support', 'operator', 'super_admin'] } },
  { path: '/config', name: 'config', component: () => import('./pages/Config.vue'), meta: { label: '配置管理', icon: 'Setting', roles: ['operator', 'tech_admin', 'super_admin'] } },
  { path: '/audit', name: 'audit', component: () => import('./pages/Records.vue'), meta: { label: '审计记录', icon: 'Document', roles: ['auditor', 'super_admin'] } },
  { path: '/accounts', name: 'accounts', component: () => import('./pages/Accounts.vue'), meta: { label: '管理员账户', icon: 'User', roles: ['super_admin'] } },
  { path: '/login', name: 'login', component: () => import('./pages/Login.vue') },
  { path: '/:rest(.*)*', redirect: '/' },
]
