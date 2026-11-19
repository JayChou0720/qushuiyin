<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api, errorText, formatBytes, formatCount, percent, platforms, stages } from '../api'
import FilterSelect from '../FilterSelect.vue'

type Metric = { date: string; platform: string; stage: string; outcome: string; sourceKind: string; deliveryMode: string; count: number; bytes: number; p50Ms: number | null; p95Ms: number | null }
type Overview = { items: Metric[]; queuedParses: number; queuedMedia: number; storageBytes: number; storageBudgetBytes: number; from: string; to: string }
const data = ref<Overview>()
const loading = ref(false)
const error = ref('')
const filters = reactive({ platform: '', from: '', to: '' })
const outcomes: Record<string, string> = { succeeded: '成功', failed: '失败', cancelled: '已取消', unknown: '未知', partial: '部分完成', issued: '已签发' }
const delivery: Record<string, string> = { direct: '直连', relay: '中转', copy: '复制链接', copy_only: '仅复制链接', direct_preview: '直连预览' }
const sources: Record<string, string> = { video: '视频', image: '图片', thumbnail: '缩略图', cover: '封面' }
const stageTotals = computed(() => Object.entries(stages).map(([stage, label]) => {
  const rows = data.value?.items.filter(row => row.stage === stage) || []
  const count = (outcome: string) => rows.reduce((sum, row) => sum + (row.outcome === outcome ? row.count : 0), 0)
  const succeeded = count('succeeded'), failed = count('failed')
  const total = rows.reduce((sum, row) => sum + row.count, 0)
  return { label, total, succeeded, failed, other: total - succeeded - failed, denominator: succeeded + failed + (stage === 'save' ? count('partial') + count('unknown') : 0) }
}).filter(row => row.total))
const parseTotal = computed(() => data.value?.items.filter(row => row.stage === 'parse').reduce((n, row) => n + row.count, 0) || 0)
const distribution = computed(() => {
  const totals = new Map<string, { platform: string; source: string; direct: number; relay: number; copy: number }>()
  for (const row of data.value?.items || []) {
    if (row.stage !== 'media_prepare' || (row.deliveryMode !== 'direct' && row.deliveryMode !== 'relay' && row.deliveryMode !== 'copy')) continue
    const key = row.platform + ':' + row.sourceKind
    const item = totals.get(key) || { platform: row.platform, source: row.sourceKind, direct: 0, relay: 0, copy: 0 }
    item[row.deliveryMode] += row.count
    totals.set(key, item)
  }
  return [...totals.values()]
})
const traffic = computed(() => data.value?.items.reduce((n, row) => n + row.bytes, 0) || 0)
async function load() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try {
    data.value = await api<Overview>('/overview', { query: filters })
    filters.from = data.value.from
    filters.to = data.value.to
  } catch (e) { error.value = errorText(e) }
  finally { loading.value = false }
}
onMounted(load)
</script>
<template>
  <section class="panel">
    <el-form label-position="top" class="filters" @submit.prevent="load">
      <FilterSelect v-model="filters.platform" label="平台" :options="platforms" />
      <el-form-item label="开始日期"><el-date-picker v-model="filters.from" type="date" value-format="YYYY-MM-DD" placeholder="选择日期" /></el-form-item>
      <el-form-item label="结束日期"><el-date-picker v-model="filters.to" type="date" value-format="YYYY-MM-DD" placeholder="选择日期" /></el-form-item>
      <el-button type="primary" native-type="submit" :loading="loading">查询</el-button>
    </el-form>
  </section>
  <el-alert v-if="error" :title="error" class="table-alert" type="error" :closable="false" show-icon />
  <section v-if="data" class="metrics" aria-label="服务指标">
    <div class="metric"><span class="muted">解析量</span><strong>{{ formatCount(parseTotal) }}</strong></div>
    <div class="metric"><span class="muted">服务端交付流量</span><strong>{{ formatBytes(traffic) }}</strong></div>
    <div class="metric"><span class="muted">解析排队 / 媒体排队</span><strong>{{ data.queuedParses }} / {{ data.queuedMedia }}</strong></div>
    <div class="metric"><span class="muted">缓存占用</span><strong>{{ formatBytes(data.storageBytes) }}</strong><small class="muted">总预算 {{ formatBytes(data.storageBudgetBytes) }}</small></div>
  </section>
  <section v-if="stageTotals.length" class="panel">
    <h2>各阶段成功率</h2>
    <el-table :data="stageTotals">
      <el-table-column label="阶段" prop="label" />
      <el-table-column label="总量" prop="total" align="right" />
      <el-table-column label="成功" prop="succeeded" align="right" />
      <el-table-column label="失败" prop="failed" align="right" />
      <el-table-column label="其他结果" prop="other" align="right" />
      <el-table-column label="成功率" align="right"><template #default="{ row }">{{ percent(row.succeeded, row.denominator) }}</template></el-table-column>
    </el-table>
  </section>
  <section v-if="distribution.length" class="panel">
    <h2>首次准备的下载方式</h2>
    <el-table :data="distribution">
      <el-table-column label="平台"><template #default="{ row }">{{ platforms[row.platform] || row.platform }}</template></el-table-column>
      <el-table-column label="内容"><template #default="{ row }">{{ sources[row.source] || row.source }}</template></el-table-column>
      <el-table-column v-for="mode in ['direct', 'relay', 'copy']" :key="mode" :label="delivery[mode]" align="right">
        <template #default="{ row }">{{ formatCount(row[mode]) }} · {{ percent(row[mode], row.direct + row.relay + row.copy) }}</template>
      </el-table-column>
    </el-table>
  </section>
  <section class="panel">
    <h2>按日统计</h2>
    <el-table v-loading="loading" :data="data?.items || []" empty-text="暂无运行数据">
      <el-table-column label="日期" prop="date" width="115" />
      <el-table-column label="平台" width="110"><template #default="{ row }">{{ platforms[row.platform] || row.platform }}</template></el-table-column>
      <el-table-column label="阶段" width="110"><template #default="{ row }">{{ stages[row.stage] || row.stage }}</template></el-table-column>
      <el-table-column label="结果" width="92"><template #default="{ row }">{{ outcomes[row.outcome] || row.outcome }}</template></el-table-column>
      <el-table-column label="内容 / 交付" min-width="125"><template #default="{ row }">{{ sources[row.sourceKind] || row.sourceKind || '—' }} / {{ delivery[row.deliveryMode] || row.deliveryMode || '—' }}</template></el-table-column>
      <el-table-column label="次数" prop="count" width="90" align="right" />
      <el-table-column label="流量" width="105" align="right"><template #default="{ row }">{{ formatBytes(row.bytes) }}</template></el-table-column>
      <el-table-column label="P50 / P95（毫秒）" min-width="150" align="right"><template #default="{ row }">{{ row.p50Ms ?? '—' }} / {{ row.p95Ms ?? '—' }}</template></el-table-column>
    </el-table>
  </section>
</template>
