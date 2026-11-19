<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { api, formatCount, formatDate, percent, platforms, registeredPlatforms, useList } from '../api'
import FilterSelect from '../FilterSelect.vue'
import Pager from '../Pager.vue'
import ReasonDialog from '../ReasonDialog.vue'

type Domain = { id: string; platform: string; host: string; status: string; version: number; seenCount: number; directIssuedTotal: number; directFallbackTotal: number; firstSeenAt: string; lastSeenAt: string; registeredAt: string | null }
type Summary = { platform: string; totalHosts: number; registeredHosts: number; seenCount: number; unregisteredSeenCount: number }
const statuses = { observed: '已发现', registered: '已登记', review: '待复查', ignored: '已忽略' } as Record<string, string>
const filters = reactive({ platform: '', status: '', host: '' })
const { data, status, page, search, refresh, previous, next } = useList<Domain>(() => '/download-domains', filters)
const summaries = computed(() => (data.value as { platforms?: Summary[] } | null)?.platforms || [])
const target = ref<Domain>()
const selection = ref('')
const visible = ref(false)
function edit(row: Domain) {
  target.value = row
  selection.value = row.status === 'registered' ? 'review' : 'registered'
  visible.value = true
}
function save(reason: string) {
  const row = target.value!
  return api('/download-domains/' + row.id, { method: 'PATCH', body: { expectedVersion: row.version, status: selection.value, reason } })
}
</script>

<template>
  <section v-if="summaries.length" class="metrics" aria-label="平台域名登记情况">
    <div v-for="row in summaries" :key="row.platform" class="metric">
      <span class="muted">{{ platforms[row.platform] || row.platform }} · 未登记出现占比</span>
      <strong>{{ percent(row.unregisteredSeenCount, row.seenCount) }}</strong>
      <small class="muted">{{ formatCount(row.registeredHosts) }} / {{ formatCount(row.totalHosts) }} 个主机已登记</small>
    </div>
  </section>
  <section class="panel">
    <el-form class="filters" label-position="top" @submit.prevent="search">
      <FilterSelect v-model="filters.platform" label="平台" :options="registeredPlatforms" />
      <FilterSelect v-model="filters.status" label="登记状态" :options="statuses" />
      <el-form-item label="主机名前缀"><el-input v-model.trim="filters.host" placeholder="cdn.example.com" clearable maxlength="253" /></el-form-item>
      <el-button type="primary" native-type="submit">筛选</el-button>
      <el-button :disabled="status.loading" @click="refresh">刷新</el-button>
    </el-form>
    <el-alert v-if="status.error" :title="status.error" class="table-alert" type="error" :closable="false" show-icon />
    <el-table v-loading="status.loading" :data="data?.items || []" row-key="id" empty-text="暂无下载域名">
      <el-table-column label="主机" prop="host" min-width="210" sortable show-overflow-tooltip />
      <el-table-column label="平台" width="105"><template #default="{ row }">{{ platforms[row.platform] || row.platform }}</template></el-table-column>
      <el-table-column label="登记状态" width="105"><template #default="{ row }"><el-tag :type="row.status === 'registered' ? 'success' : row.status === 'review' ? 'warning' : 'info'" effect="light">{{ statuses[row.status] }}</el-tag></template></el-table-column>
      <el-table-column label="出现次数" prop="seenCount" width="110" align="right" sortable />
      <el-table-column label="直连签发 / 回退" min-width="130" align="right"><template #default="{ row }">{{ row.directIssuedTotal }} / {{ row.directFallbackTotal }}</template></el-table-column>
      <el-table-column label="首次发现" width="169"><template #default="{ row }">{{ formatDate(row.firstSeenAt) }}</template></el-table-column>
      <el-table-column label="最近发现" width="169"><template #default="{ row }">{{ formatDate(row.lastSeenAt) }}</template></el-table-column>
      <el-table-column label="登记时间" width="169"><template #default="{ row }">{{ formatDate(row.registeredAt) }}</template></el-table-column>
      <el-table-column label="操作" width="100"><template #default="{ row }"><el-button link type="primary" @click="edit(row)">更新状态</el-button></template></el-table-column>
    </el-table>
    <Pager :page="page" :has-more="data?.hasMore" :loading="status.loading" @previous="previous" @next="next" />
  </section>
  <ReasonDialog v-model="visible" title="更新下载域名" :save="save" :disabled="selection === target?.status" @saved="refresh">
    <p class="mono">{{ target?.host }}</p>
    <el-form-item label="登记状态">
      <el-select v-model="selection"><el-option v-for="(label, value) in statuses" :key="value" :label="label" :value="value" /></el-select>
    </el-form-item>
    <el-alert v-if="selection === 'registered'" title="请先确认此主机已在微信公众平台中登记为 downloadFile 合法域名。" type="info" :closable="false" show-icon />
  </ReasonDialog>
</template>
