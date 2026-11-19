<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { useRoute } from 'vue-router'
import { formatDate, platforms, stages, useList } from '../api'
import FilterSelect from '../FilterSelect.vue'
import Pager from '../Pager.vue'

type Audit = { id: string; operatorId: string; action: string; targetType: string; targetId: string; before: unknown; after: unknown; reason: string; requestId: string; createdAt: string }
type Failure = { id: string; traceId: string; platform: string; stage: string; adapterVersion: string; errorCode: string; durationMs: number | null; createdAt: string }
const route = useRoute()
const audit = computed(() => route.name === 'audit')
const filters = reactive({ traceId: '', platform: '', stage: '', from: '', to: '', targetType: '', targetId: '', action: '' })
const { data, status, page, search, refresh, previous, next } = useList<Audit | Failure>(() => audit.value ? '/audit-logs' : '/failures', filters)
const selected = ref<Audit>()
const visible = ref(false)
const actions = { domain_update: '更新下载域名', platform_update: '更新平台', feedback_update: '处理工单', feedback_private_view: '查看工单敏感信息', bootstrap: '初始化管理员', config_publish: '发布配置', admin_create: '创建管理员', admin_update: '更新管理员' } as Record<string, string>
const targets = { download_domain: '下载域名', platform: '平台', feedback: '反馈', system_config: '配置', admin_user: '管理员' } as Record<string, string>
</script>
<template>
  <section class="panel">
    <el-form class="filters" label-position="top" @submit.prevent="search">
      <template v-if="audit">
        <FilterSelect v-model="filters.targetType" label="对象类型" :options="targets" />
        <FilterSelect v-model="filters.action" label="变更动作" :options="actions" />
        <el-form-item label="对象 ID"><el-input v-model.trim="filters.targetId" clearable /></el-form-item>
      </template>
      <template v-else>
        <FilterSelect v-model="filters.platform" label="平台" :options="platforms" />
        <FilterSelect v-model="filters.stage" label="阶段" :options="stages" />
        <el-form-item label="Trace ID"><el-input v-model.trim="filters.traceId" clearable /></el-form-item>
        <el-form-item label="开始日期"><el-date-picker v-model="filters.from" type="date" value-format="YYYY-MM-DD" placeholder="选择日期" /></el-form-item>
        <el-form-item label="结束日期"><el-date-picker v-model="filters.to" type="date" value-format="YYYY-MM-DD" placeholder="选择日期" /></el-form-item>
      </template>
      <el-button type="primary" native-type="submit">筛选</el-button>
      <el-button :disabled="status.loading" @click="refresh">刷新</el-button>
    </el-form>
    <el-alert v-if="status.error" :title="status.error" class="table-alert" type="error" :closable="false" show-icon />
    <el-table v-loading="status.loading" :data="data?.items || []" row-key="id" empty-text="暂无记录">
      <el-table-column label="时间" width="172"><template #default="{ row }">{{ formatDate(row.createdAt) }}</template></el-table-column>
      <template v-if="audit">
        <el-table-column label="动作" min-width="135"><template #default="{ row }">{{ actions[row.action] || row.action }}</template></el-table-column>
        <el-table-column label="对象" min-width="150" show-overflow-tooltip><template #default="{ row }">{{ targets[row.targetType] || row.targetType }} · {{ row.targetId }}</template></el-table-column>
        <el-table-column label="操作者 ID" prop="operatorId" min-width="130" show-overflow-tooltip />
        <el-table-column label="原因" prop="reason" min-width="170" show-overflow-tooltip />
        <el-table-column label="操作" width="95"><template #default="{ row }"><el-button link type="primary" @click="selected = row; visible = true">查看详情</el-button></template></el-table-column>
      </template>
      <template v-else>
        <el-table-column label="Trace ID" prop="traceId" min-width="220" show-overflow-tooltip />
        <el-table-column label="平台" width="105"><template #default="{ row }">{{ platforms[row.platform] || row.platform }}</template></el-table-column>
        <el-table-column label="阶段" min-width="100"><template #default="{ row }">{{ stages[row.stage] || row.stage }}</template></el-table-column>
        <el-table-column label="适配器版本" prop="adapterVersion" min-width="125" />
        <el-table-column label="错误码" prop="errorCode" min-width="205" show-overflow-tooltip />
        <el-table-column label="耗时（毫秒）" min-width="125" align="right"><template #default="{ row }">{{ row.durationMs ?? '—' }}</template></el-table-column>
      </template>
    </el-table>
    <Pager :page="page" :has-more="data?.hasMore" :loading="status.loading" @previous="previous" @next="next" />
  </section>
  <el-dialog v-model="visible" title="审计详情" width="min(700px, 94vw)">
    <template v-if="selected">
      <el-descriptions :column="1" border>
        <el-descriptions-item label="操作者 ID">{{ selected.operatorId }}</el-descriptions-item>
        <el-descriptions-item label="对象 ID">{{ selected.targetId }}</el-descriptions-item>
        <el-descriptions-item label="请求 ID">{{ selected.requestId }}</el-descriptions-item>
        <el-descriptions-item label="原因"><span class="readable">{{ selected.reason }}</span></el-descriptions-item>
        <el-descriptions-item label="变更前"><pre>{{ JSON.stringify(selected.before, null, 2) }}</pre></el-descriptions-item>
        <el-descriptions-item label="变更后"><pre>{{ JSON.stringify(selected.after, null, 2) }}</pre></el-descriptions-item>
      </el-descriptions>
    </template>
  </el-dialog>
</template>
