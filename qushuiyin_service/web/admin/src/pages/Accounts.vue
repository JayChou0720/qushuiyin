<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { account, api, formatDate, roles, useList, type Account, type Role } from '../api'
import Pager from '../Pager.vue'
import ReasonDialog from '../ReasonDialog.vue'
type User = Account & { status: string; version: number; createdAt: string; lastLoginAt: string | null; lockedUntil: string | null }
const statuses: Record<string, string> = { active: '已启用', disabled: '已停用' }
const { data, status, page, refresh, previous, next } = useList<User>(() => '/admin-users')
const visible = ref(false)
const selected = ref<User>()
const draft = reactive({ username: '', role: 'viewer' as Role, status: 'active', password: '' })
const self = computed(() => selected.value?.id === account.value?.id)
function edit(row?: User) {
  selected.value = row
  Object.assign(draft, { username: row?.username || '', role: row?.role || 'viewer', status: row?.status || 'active', password: '' })
  visible.value = true
}
function save(reason: string) {
  const row = selected.value
  if (!row) return api('/admin-users', { method: 'POST', body: { username: draft.username.trim(), role: draft.role, password: draft.password, reason } })
  return api('/admin-users/' + row.id, { method: 'PATCH', body: {
    expectedVersion: row.version, role: draft.role === row.role ? undefined : draft.role,
    status: draft.status === row.status ? undefined : draft.status, password: draft.password || undefined, reason,
  } })
}
const disabled = computed(() => selected.value ? !draft.password && selected.value.role === draft.role && selected.value.status === draft.status : !draft.username.trim() || !draft.password)
</script>
<template>
  <section class="panel">
    <div class="filters"><el-button type="primary" @click="edit()">创建账户</el-button><el-button :disabled="status.loading" @click="refresh">刷新</el-button></div>
    <el-alert v-if="status.error" :title="status.error" class="table-alert" type="error" :closable="false" show-icon />
    <el-table v-loading="status.loading" :data="data?.items || []" row-key="id" empty-text="暂无管理员">
      <el-table-column label="用户名" prop="username" min-width="145" show-overflow-tooltip />
      <el-table-column label="角色" width="135"><template #default="{ row }">{{ roles[row.role as Role] }}</template></el-table-column>
      <el-table-column label="状态" width="100"><template #default="{ row }"><el-tag :type="row.status === 'active' ? 'success' : 'info'">{{ statuses[row.status] }}</el-tag></template></el-table-column>
      <el-table-column label="最近登录" width="169"><template #default="{ row }">{{ formatDate(row.lastLoginAt) }}</template></el-table-column>
      <el-table-column label="锁定至" width="169"><template #default="{ row }">{{ formatDate(row.lockedUntil) }}</template></el-table-column>
      <el-table-column label="操作" width="100"><template #default="{ row }"><el-button link type="primary" @click="edit(row)">编辑</el-button></template></el-table-column>
    </el-table>
    <Pager :page="page" :has-more="data?.hasMore" :loading="status.loading" @previous="previous" @next="next" />
  </section>
  <ReasonDialog v-model="visible" :title="selected ? '编辑管理员账户' : '创建管理员账户'" :save="save" :disabled="disabled" @saved="refresh" @closed="draft.password = ''">
    <el-form-item label="用户名" required><el-input v-model="draft.username" :disabled="!!selected" autocomplete="off" maxlength="64" /></el-form-item>
    <el-form-item label="角色" required><el-select v-model="draft.role" :disabled="self"><el-option v-for="(label, key) in roles" :key="key" :label="label" :value="key" /></el-select></el-form-item>
    <el-form-item v-if="selected" label="状态"><el-select v-model="draft.status" :disabled="self"><el-option v-for="(label, key) in statuses" :key="key" :label="label" :value="key" /></el-select></el-form-item>
    <el-form-item :label="selected ? '新密码（留空保留）' : '初始密码'" :required="!selected"><el-input v-model="draft.password" type="password" autocomplete="new-password" maxlength="256" /></el-form-item>
  </ReasonDialog>
</template>
