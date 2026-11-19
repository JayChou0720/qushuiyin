<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Operation } from '@element-plus/icons-vue'
import { account, api, errorText, type Account } from '../api'
const router = useRouter()
const route = useRoute()
const username = ref('')
const password = ref('')
const working = ref(false)
const error = ref('')
async function login() {
  if (working.value || !username.value || !password.value) return
  working.value = true
  error.value = ''
  try {
    account.value = await api<Account>('/sessions', { method: 'POST', body: { username: username.value.trim(), password: password.value } })
    const next = route.query.next
    await router.replace(typeof next === 'string' && next.startsWith('/') && !next.startsWith('//') ? next : '/')
  } catch (e) { error.value = errorText(e) }
  finally { password.value = ''; working.value = false }
}
</script>
<template>
  <main class="login">
    <section class="panel login-card">
      <div class="brand"><el-icon><Operation /></el-icon><span>去水印</span></div>
      <h1>登录运营后台</h1>
      <el-form label-position="top" @submit.prevent="login">
        <el-form-item label="用户名"><el-input v-model="username" autocomplete="username" maxlength="64" /></el-form-item>
        <el-form-item label="密码"><el-input v-model="password" type="password" autocomplete="current-password" maxlength="256" /></el-form-item>
        <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon />
        <el-button type="primary" native-type="submit" :loading="working" :disabled="!username.trim() || !password" class="login-submit">登录</el-button>
      </el-form>
    </section>
  </main>
</template>
