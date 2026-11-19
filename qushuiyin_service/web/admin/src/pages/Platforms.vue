<script setup lang="ts">
import { computed, ref } from 'vue'
import { account, api, formatDate, platforms, useList } from '../api'
import ReasonDialog from '../ReasonDialog.vue'

type Kind = 'video' | 'image' | 'cover'
type Capabilities = { videoEnabled: boolean; imageEnabled: boolean; coverEnabled: boolean; textEnabled: boolean; deliveryPolicy: Record<Kind, string> }
type Platform = { platform: string; status: string; capabilities: Capabilities; maintenanceMessage: string; adapterName: string; adapterVersion: string; shareDomains: string[]; credentialConfigured: boolean; lastVerifiedAt: string | null; version: number; circuit: string }
const statuses: Record<string, string> = { enabled: '已启用', disabled: '已停用', maintenance: '维护中' }
const circuits: Record<string, string> = { closed: '正常', open: '已熔断', half_open: '半开', unavailable: '未运行' }
const kinds = { video: '视频', image: '图集', cover: '封面' } as const
const modes = { direct_preferred: '直连优先', relay_only: '仅中转', copy_only: '仅复制链接' }
const canManage = computed(() => account.value?.role !== 'operator')
const { data, status, refresh } = useList<Platform>(() => '/platforms')
const selected = ref<Platform>()
const draft = ref<Platform>()
const visible = ref(false)
const verification = ref(false)
const domains = ref('')
function edit(row: Platform) {
  selected.value = row
  draft.value = { ...row, capabilities: { ...row.capabilities, deliveryPolicy: { ...row.capabilities.deliveryPolicy } } }
  domains.value = row.shareDomains.join('\n')
  verification.value = false
  visible.value = true
}
function save(reason: string) {
  const row = draft.value!
  return api('/platforms/' + row.platform, { method: 'PATCH', body: {
    expectedVersion: row.version, maintenanceMessage: row.maintenanceMessage, reason,
    ...(canManage.value ? { status: row.status, capabilities: row.capabilities, shareDomains: domains.value.split(/[\s,，]+/).filter(Boolean), verifiedAt: verification.value ? new Date().toISOString() : undefined } : {}),
  } })
}
</script>

<template>
  <section class="panel">
    <div class="filters"><el-button :disabled="status.loading" @click="refresh">刷新</el-button></div>
    <el-alert v-if="status.error" :title="status.error" class="table-alert" type="error" :closable="false" show-icon />
    <el-table v-loading="status.loading" :data="data?.items || []" row-key="platform" empty-text="暂无平台">
      <el-table-column label="平台" width="110"><template #default="{ row }">{{ platforms[row.platform] || row.platform }}</template></el-table-column>
      <el-table-column label="状态" width="100"><template #default="{ row }"><el-tag :type="row.status === 'enabled' ? 'success' : row.status === 'maintenance' ? 'warning' : 'info'">{{ statuses[row.status] }}</el-tag></template></el-table-column>
      <el-table-column label="已启用能力" min-width="160"><template #default="{ row }">{{ ([row.capabilities.videoEnabled && '视频', row.capabilities.imageEnabled && '图集', row.capabilities.coverEnabled && '封面', row.capabilities.textEnabled && '文案'].filter(Boolean).join('、')) || '—' }}</template></el-table-column>
      <el-table-column label="维护文案" prop="maintenanceMessage" min-width="140" show-overflow-tooltip />
      <el-table-column label="适配器 / 版本" min-width="165"><template #default="{ row }">{{ row.adapterName }} · {{ row.adapterVersion }}</template></el-table-column>
      <el-table-column label="熔断" width="100"><template #default="{ row }">{{ circuits[row.circuit] || row.circuit || '—' }}</template></el-table-column>
      <el-table-column label="凭证" width="100"><template #default="{ row }">{{ row.credentialConfigured ? '已配置' : '未配置' }}</template></el-table-column>
      <el-table-column label="最近验证" width="169"><template #default="{ row }">{{ formatDate(row.lastVerifiedAt) }}</template></el-table-column>
      <el-table-column label="配置版本" prop="version" width="100" />
      <el-table-column label="操作" width="110"><template #default="{ row }"><el-button link type="primary" @click="edit(row)">{{ canManage ? '编辑平台' : '编辑文案' }}</el-button></template></el-table-column>
    </el-table>
  </section>
  <ReasonDialog v-model="visible" :title="'编辑 · ' + (platforms[selected?.platform || ''] || '')" :save="save" @saved="refresh">
    <template v-if="draft">
      <el-form-item v-if="canManage" label="平台状态"><el-select v-model="draft.status"><el-option v-for="(label, key) in statuses" :key="key" :label="label" :value="key" /></el-select></el-form-item>
      <el-form-item label="维护文案"><el-input v-model="draft.maintenanceMessage" type="textarea" :rows="2" maxlength="200" show-word-limit /></el-form-item>
      <template v-if="canManage">
        <div class="dialog-grid">
          <template v-for="(label, kind) in kinds" :key="kind">
            <el-form-item :label="label"><el-switch v-model="draft.capabilities[`${kind}Enabled`]" :disabled="selected?.platform === 'bilibili' && kind === 'image'" /></el-form-item>
            <el-form-item :label="label + '交付策略'"><el-select v-model="draft.capabilities.deliveryPolicy[kind]"><el-option v-for="(name, mode) in modes" :key="mode" :label="name" :value="mode" /></el-select></el-form-item>
          </template>
        </div>
        <el-form-item label="文案"><el-switch v-model="draft.capabilities.textEnabled" /></el-form-item>
        <el-form-item label="允许的分享域名（每行一个）"><el-input v-model="domains" type="textarea" :rows="3" /></el-form-item>
        <el-checkbox v-model="verification">将最近验证时间更新为当前时间</el-checkbox>
      </template>
    </template>
  </ReasonDialog>
</template>
