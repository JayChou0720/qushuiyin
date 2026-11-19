<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { DataAnalysis, Connection, Switch, Warning, ChatDotRound, Setting, Document, User, Menu, SwitchButton, Operation } from '@element-plus/icons-vue'
import { account, api, errorText, roles, sessionLost } from './api'
import { ElConfigProvider, ElMessage } from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import { routes } from './routes'
const icons = { DataAnalysis, Connection, Switch, Warning, ChatDotRound, Setting, Document, User }
const route = useRoute()
const router = useRouter()
const menu = ref(false)
const leaving = ref(false)
const links = computed(() => routes.filter(r => r.meta?.label && (!r.meta.roles || (r.meta.roles as string[]).includes(account.value?.role || ''))))
watch(() => route.path, () => { menu.value = false })
watch(sessionLost, lost => {
  if (lost && route.name !== 'login') {
    ElMessage.warning('登录已过期，请重新登录')
    void router.replace({ name: 'login', query: { next: route.fullPath } })
    sessionLost.value = false
  }
})
async function logout() {
  leaving.value = true
  try {
    await api('/sessions/current', { method: 'DELETE' })
    account.value = undefined
    await router.replace('/login')
  } catch (e) { ElMessage.error(errorText(e)) }
  finally { leaving.value = false }
}
</script>
<template>
  <el-config-provider :locale="zhCn">
  <router-view v-if="route.name === 'login'" />
  <div v-else-if="account" class="shell">
    <aside class="sidebar" :class="{ expanded: menu }">
      <router-link to="/" class="brand"><el-icon><Operation /></el-icon><span>去水印<span class="brand-sub">运营后台</span></span></router-link>
      <nav aria-label="管理导航">
        <router-link v-for="link in links" :key="link.path" :to="link.path">
          <el-icon><component :is="icons[link.meta?.icon as keyof typeof icons]" /></el-icon>
          {{ link.meta?.label }}
        </router-link>
      </nav>
    </aside>
    <div class="workspace">
      <header class="topbar">
        <el-button class="menu-trigger" :icon="Menu" aria-label="切换导航" :aria-expanded="menu" @click="menu = !menu" />
        <div class="session"><span>{{ account.username }} <span class="muted">· {{ roles[account.role] }}</span></span>
          <el-button :icon="SwitchButton" :loading="leaving" text @click="logout">退出</el-button>
        </div>
      </header>
      <main class="content">
        <h1>{{ route.meta.label }}</h1>
        <router-view :key="route.name" />
      </main>
    </div>
  </div>
  </el-config-provider>
</template>
