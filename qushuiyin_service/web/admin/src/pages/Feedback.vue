<script setup lang="ts">
import { onBeforeUnmount, reactive, ref } from 'vue'
import { account, api, errorText, formatDate, useList } from '../api'
import FilterSelect from '../FilterSelect.vue'
import Pager from '../Pager.vue'
import ReasonDialog from '../ReasonDialog.vue'

type Summary = { id: string; category: string; summary: string; status: string; version: number; createdAt: string; updatedAt: string }
type Ticket = Omit<Summary, 'summary'> & { content: string; reply: string | null; repliedAt: string | null }
const statuses = { pending: '待处理', processing: '处理中', replied: '已回复', closed: '已关闭' } as Record<string, string>
const categories = { parse_failed: '解析失败', download_failed: '下载失败', album_failed: '保存失败', gallery_issue: '图集问题', suggestion: '建议', other: '其他' } as Record<string, string>
const filters = reactive({ status: '', category: '' })
const { data, status, page, search, refresh, previous, next } = useList<Summary>(() => '/feedbacks', filters)
const visible = ref(false)
const editing = ref(false)
const revealing = ref(false)
const privateData = ref<{ contact: string | null; sourceUrl: string | null; traceId: string | null }>()
async function reveal(reason: string) {
  privateData.value = await api('/feedbacks/' + ticketId.value + '/private', { method: 'POST', body: { reason } })
}
function openReveal() {
  visible.value = false
  revealing.value = true
}
const loading = ref(false)
const detailError = ref('')
const selected = ref<Ticket>()
const ticketId = ref('')
const draft = reactive({ status: '', reply: '' })
let request: AbortController | undefined
onBeforeUnmount(() => request?.abort())

async function detail(id: string) {
  request?.abort()
  const controller = new AbortController()
  request = controller
  privateData.value = undefined
  ticketId.value = id
  visible.value = true
  selected.value = undefined
  loading.value = true
  detailError.value = ''
  try { selected.value = await api<Ticket>('/feedbacks/' + id, { signal: controller.signal }) }
  catch (e) { if (!controller.signal.aborted) detailError.value = errorText(e) }
  finally { if (!controller.signal.aborted) loading.value = false }
}
function edit() {
  draft.status = selected.value!.status
  draft.reply = selected.value!.reply || ''
  visible.value = false
  editing.value = true
}
async function save(reason: string) {
  const row = selected.value!
  await api('/feedbacks/' + row.id, { method: 'PATCH', body: {
    expectedVersion: row.version, reason, status: draft.status,
    reply: draft.reply.trim() && draft.reply.trim() !== row.reply ? draft.reply.trim() : undefined,
  } })
}
function saved() { refresh(); void detail(ticketId.value) }
</script>
<template>
  <section class="panel">
    <el-form class="filters" label-position="top" @submit.prevent="search">
      <FilterSelect v-model="filters.status" label="状态" :options="statuses" />
      <FilterSelect v-model="filters.category" label="分类" :options="categories" />
      <el-button native-type="submit" type="primary">筛选</el-button>
      <el-button :disabled="status.loading" @click="refresh">刷新</el-button>
    </el-form>
    <el-alert v-if="status.error" :title="status.error" class="table-alert" type="error" :closable="false" show-icon />
    <el-table v-loading="status.loading" :data="data?.items || []" row-key="id" empty-text="暂无工单">
      <el-table-column label="分类" min-width="105"><template #default="{ row }">{{ categories[row.category] }}</template></el-table-column>
      <el-table-column label="反馈摘要" prop="summary" min-width="200" show-overflow-tooltip />
      <el-table-column label="状态" width="105"><template #default="{ row }"><el-tag effect="light">{{ statuses[row.status] }}</el-tag></template></el-table-column>
      <el-table-column label="提交时间" width="169"><template #default="{ row }">{{ formatDate(row.createdAt) }}</template></el-table-column>
      <el-table-column label="最近更新" width="169"><template #default="{ row }">{{ formatDate(row.updatedAt) }}</template></el-table-column>
      <el-table-column label="操作" width="98"><template #default="{ row }"><el-button link type="primary" @click="detail(row.id)">查看详情</el-button></template></el-table-column>
    </el-table>
    <Pager :page="page" :has-more="data?.hasMore" :loading="status.loading" @previous="previous" @next="next" />
  </section>
  <el-dialog v-model="visible" title="反馈详情" width="min(680px, 94vw)">
    <div v-loading="loading" style="min-height: 48px">
      <el-alert v-if="detailError" :title="detailError" class="table-alert" type="error" :closable="false" show-icon />
      <el-descriptions v-if="selected" border :column="1">
        <el-descriptions-item label="工单 ID">{{ selected.id }}</el-descriptions-item>
        <el-descriptions-item label="分类 / 状态">{{ categories[selected.category] }} / {{ statuses[selected.status] }}</el-descriptions-item>
        <el-descriptions-item label="内容"><span class="readable">{{ selected.content }}</span></el-descriptions-item>
        <el-descriptions-item label="回复"><span class="readable">{{ selected.reply || '—' }}</span></el-descriptions-item>
        <el-descriptions-item label="回复时间">{{ formatDate(selected.repliedAt) }}</el-descriptions-item>
        <template v-if="privateData">
          <el-descriptions-item label="联系方式"><span class="readable">{{ privateData.contact || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="附带来源"><span class="readable">{{ privateData.sourceUrl || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="Trace ID">{{ privateData.traceId || '—' }}</el-descriptions-item>
        </template>
      </el-descriptions>
    </div>
    <template #footer>
      <el-button @click="visible = false; privateData = undefined">关闭</el-button>
      <el-button v-if="selected && !privateData && account?.role === 'super_admin'" @click="openReveal">查看敏感附加信息</el-button>
      <el-button v-if="selected && selected.status !== 'closed'" type="primary" @click="edit">处理工单</el-button>
    </template>
  </el-dialog>
  <ReasonDialog v-model="revealing" title="查看敏感附加信息" :save="reveal" success="已记录访问审计" @saved="visible = true" />
  <ReasonDialog v-model="editing" title="处理反馈工单" :save="save" :disabled="draft.status === 'replied' && !draft.reply.trim()" @saved="saved">
    <el-form-item label="处理状态"><el-select v-model="draft.status"><el-option v-for="(label, key) in statuses" :key="key" :label="label" :value="key" /></el-select></el-form-item>
    <el-form-item label="回复"><el-input v-model="draft.reply" type="textarea" :rows="4" /></el-form-item>
    <el-alert v-if="draft.status === 'closed'" title="关闭后无法继续处理此工单。" type="warning" :closable="false" show-icon />
  </ReasonDialog>
</template>
