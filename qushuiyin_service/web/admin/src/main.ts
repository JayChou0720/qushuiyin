import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import { ElAlert, ElButton, ElCheckbox, ElDatePicker, ElDescriptions, ElDescriptionsItem, ElDialog, ElForm, ElFormItem, ElIcon, ElInput, ElInputNumber, ElLoading, ElOption, ElSelect, ElSwitch, ElTable, ElTableColumn, ElTag } from 'element-plus'
import 'element-plus/dist/index.css'
import './style.css'
import App from './App.vue'
import { account, checkSession } from './api'
import { routes } from './routes'

const router = createRouter({ history: createWebHistory(import.meta.env.BASE_URL), routes })
router.beforeEach(async to => {
  if (!account.value) await checkSession()
  if (to.name === 'login') return account.value ? '/' : true
  if (!account.value) return { name: 'login', query: { next: to.fullPath } }
  const roles = to.meta.roles as string[] | undefined
  if (roles && !roles.includes(account.value.role)) return '/'
})
const app = createApp(App).use(router).use(ElLoading)
for (const component of [ElAlert, ElButton, ElCheckbox, ElDatePicker, ElDescriptions, ElDescriptionsItem, ElDialog, ElForm, ElFormItem,
  ElIcon, ElInput, ElInputNumber, ElOption, ElSelect, ElSwitch, ElTable, ElTableColumn, ElTag]) {
  app.use(component)
}
app.mount('#app')
