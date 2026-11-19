<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { account, api, formatDate, useList } from '../api'
import Pager from '../Pager.vue'
import ReasonDialog from '../ReasonDialog.vue'
type Value = string | number | boolean
type Entry = { key: string; value: Value; version: number; publishedAt: string; reason: string; default: Value; unit: string; editable: boolean; restartRequired: boolean; reserved: boolean }
type Version = { id: string; value: Value; version: number; publishedAt: string; operatorId: string; reason: string }
const { data, status, refresh } = useList<Entry>(() => '/config')
const searchText = ref('')
const entries = computed(() => (data.value?.items || []).filter(row => (row.key + ' ' + label(row.key)).toLowerCase().includes(searchText.value.trim().toLowerCase())))
const drafts = reactive<Record<string, { entry: Entry; value: Value }>>({})
const staged = computed(() => Object.values(drafts))
const selected = ref<Entry>()
const editValue = ref<Value>('')
const editing = ref(false)
const publishing = ref(false)
const confirmRange = ref(false)
const history = ref(false)
const historyEntry = ref<Entry>()
const versions = useList<Version>(() => history.value && historyEntry.value ? '/config/' + historyEntry.value.key + '/versions' : '')
const label = (key: string) => ({ public_notice: '小程序公告', public_help: '小程序帮助', direct_preview_enabled: '直连预览灰度开关' } as Record<string, string>)[key] || key
function edit(entry: Entry, value = drafts[entry.key]?.value ?? entry.value) {
  selected.value = entry
  editValue.value = value
  editing.value = true
}
function stage() {
  const entry = selected.value!
  if (editValue.value === entry.value) delete drafts[entry.key]
  else drafts[entry.key] = { entry, value: editValue.value }
  editing.value = false
}
function openHistory(row: Entry) {
  historyEntry.value = row
  versions.search()
  history.value = true
}
function rollBack(row: Version) {
  history.value = false
  edit(historyEntry.value!, row.value)
}
function publish() { confirmRange.value = false; publishing.value = true }
function save(reason: string) {
  return api('/config', { method: 'PATCH', body: { reason, confirmRange: confirmRange.value,
    changes: staged.value.map(({ entry, value }) => ({ key: entry.key, expectedVersion: entry.version, value })),
  } })
}
function clearDrafts() { for (const key of Object.keys(drafts)) delete drafts[key] }
function reload() { clearDrafts(); refresh() }
</script>
<template>
  <section class="panel">
    <el-form class="filters" label-position="top" @submit.prevent>
      <el-form-item label="搜索配置"><el-input v-model="searchText" clearable placeholder="配置名称或键" /></el-form-item>
      <el-button :disabled="status.loading || !!staged.length" @click="refresh">刷新</el-button>
      <el-button v-if="staged.length" @click="reload">清除草稿并刷新</el-button>
      <el-button type="primary" :disabled="!staged.length" @click="publish">发布变更{{ staged.length ? '（' + staged.length + '）' : '' }}</el-button>
    </el-form>
    <el-alert v-if="status.error" :title="status.error" class="table-alert" type="error" :closable="false" show-icon />
    <el-table v-loading="status.loading" :data="entries" row-key="key" empty-text="暂无配置">
      <el-table-column label="配置" min-width="225" show-overflow-tooltip><template #default="{ row }">{{ label(row.key) }}</template></el-table-column>
      <el-table-column label="当前值" min-width="150" show-overflow-tooltip><template #default="{ row }">{{ JSON.stringify(row.value) }}</template></el-table-column>
      <el-table-column label="待发布" min-width="130" show-overflow-tooltip><template #default="{ row }">{{ drafts[row.key] ? JSON.stringify(drafts[row.key].value) : '—' }}</template></el-table-column>
      <el-table-column label="单位" prop="unit" width="80" />
      <el-table-column label="版本" prop="version" width="70" />
      <el-table-column label="生效方式" width="100"><template #default="{ row }">{{ row.reserved ? '预留' : row.restartRequired ? '重启后' : '发布后' }}</template></el-table-column>
      <el-table-column label="发布时间" width="169"><template #default="{ row }">{{ formatDate(row.publishedAt) }}</template></el-table-column>
      <el-table-column label="操作" width="125"><template #default="{ row }">
        <el-button v-if="row.editable" link type="primary" @click="edit(row)">编辑</el-button>
        <el-button link type="primary" @click="openHistory(row)">版本</el-button>
      </template></el-table-column>
    </el-table>
  </section>
  <el-dialog v-model="editing" title="编辑配置草稿" width="min(550px, 94vw)">
    <el-form v-if="selected" label-position="top" @submit.prevent="stage">
      <p class="mono">{{ selected.key }}</p>
      <el-form-item :label="'配置值（' + selected.unit + '）'">
        <el-input-number v-if="typeof selected.default === 'number'" v-model="editValue" :min="0" />
        <el-switch v-else-if="typeof selected.default === 'boolean'" v-model="editValue" />
        <el-input v-else v-model="editValue" type="textarea" :rows="5" />
      </el-form-item>
      <p class="muted">默认值 {{ JSON.stringify(selected.default) }} · 当前版本 {{ selected.version }}</p>
      <p v-if="typeof selected.default === 'number' && selected.default > 0" class="muted">
        建议范围 {{ Math.ceil(selected.default / 10) }}–{{ selected.unit === '%' ? Math.min(100, selected.default * 10) : selected.default * 10 }} {{ selected.unit }}
      </p>
      <p class="readable muted">上次发布原因 {{ selected.reason }}</p>
      <div class="dialog-actions">
        <el-button @click="editing = false">取消</el-button>
        <el-button type="primary" native-type="submit" :disabled="editValue == null">加入草稿</el-button>
      </div>
    </el-form>
  </el-dialog>
  <ReasonDialog v-model="publishing" title="发布配置变更" :save="save" success="已发布新版本" @saved="reload">
    <ul class="readable"><li v-for="change in staged" :key="change.entry.key">{{ label(change.entry.key) }} · v{{ change.entry.version }} → v{{ change.entry.version + 1 }}<br /><code>{{ JSON.stringify(change.value) }}</code></li></ul>
    <el-alert v-if="staged.some(change => change.entry.restartRequired)" title="部分参数需要重启服务后生效。" type="warning" :closable="false" show-icon />
    <el-alert v-if="staged.some(change => change.entry.reserved)" title="部分参数为预留配置，当前版本未读取这些值。" type="info" :closable="false" show-icon />
    <el-checkbox v-if="account?.role === 'super_admin'" v-model="confirmRange">确认发布超出建议范围的值</el-checkbox>
  </ReasonDialog>
  <el-dialog v-model="history" :title="'配置版本 · ' + (historyEntry ? label(historyEntry.key) : '')" width="min(810px, 94vw)">
    <el-alert v-if="versions.status.error" :title="versions.status.error" type="error" :closable="false" show-icon />
    <el-table v-loading="versions.status.loading" :data="versions.data.value?.items || []" row-key="id" empty-text="暂无版本">
      <el-table-column label="版本" prop="version" width="70" />
      <el-table-column label="值" min-width="120" show-overflow-tooltip><template #default="{ row }">{{ JSON.stringify(row.value) }}</template></el-table-column>
      <el-table-column label="原因" prop="reason" min-width="125" show-overflow-tooltip />
      <el-table-column label="操作者 ID" prop="operatorId" min-width="130" show-overflow-tooltip />
      <el-table-column label="发布时间" width="169"><template #default="{ row }">{{ formatDate(row.publishedAt) }}</template></el-table-column>
      <el-table-column v-if="historyEntry?.editable" label="操作" width="110"><template #default="{ row }">
        <el-button v-if="row.version !== historyEntry?.version" link type="primary" @click="rollBack(row)">采用此版本</el-button>
      </template></el-table-column>
    </el-table>
    <Pager :page="versions.page.value" :has-more="versions.data.value?.hasMore" :loading="versions.status.loading" @previous="versions.previous" @next="versions.next" />
  </el-dialog>
</template>
