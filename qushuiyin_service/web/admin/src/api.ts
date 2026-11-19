import { reactive, ref, shallowRef, watch } from 'vue'

export type Role = 'viewer' | 'operator' | 'tech_admin' | 'support' | 'auditor' | 'super_admin'
export type Account = { id: string; username: string; role: Role }
export type Page<T> = { items: T[]; nextCursor: string | null; hasMore: boolean }
export type Filters = Record<string, string>
export const account = shallowRef<Account>()
export const sessionLost = ref(false)
let sessionRequest: Promise<void> | undefined

const messages: Record<string, string> = {
  AUTH_REQUIRED: '请登录', SESSION_EXPIRED: '登录已过期', ADMIN_LOGIN_FAILED: '用户名或密码无效，或账户暂时锁定',
  ACCESS_DENIED: '当前账户没有此操作权限', SERVICE_BUSY: '服务繁忙，请稍后重试', VERSION_CONFLICT: '记录已被修改，请刷新后重新编辑',
  BAD_REQUEST: '请检查填写内容', RESOURCE_NOT_FOUND: '记录已删除或无法访问',
  LAST_SUPER_ADMIN: '至少保留一名启用的管理员', FEEDBACK_CLOSED: '工单已关闭', RATE_LIMITED: '请求过于频繁，请稍后重试',
}

export async function api<T>(path: string, options: { method?: string; body?: unknown; query?: Filters; signal?: AbortSignal } = {}): Promise<T> {
  const query = new URLSearchParams(Object.entries(options.query || {}).filter(([, value]) => typeof value === 'string' && value !== ''))
  const response = await fetch('/admin/v1' + path + (query.size ? '?' + query : ''), {
    method: options.method, signal: options.signal, credentials: 'same-origin', cache: 'no-store',
    headers: options.body ? { 'Content-Type': 'application/json' } : undefined,
    body: options.body ? JSON.stringify(options.body) : undefined,
  })
  const payload = await response.json().catch(() => null)
  if (!response.ok || payload?.code !== 'OK') {
    const code = payload?.code
    if (response.status === 401 && path !== '/sessions' && path !== '/me') {
      account.value = undefined
      sessionLost.value = true
    }
    const explanation = payload?.error?.details?.reason
    throw new Error((messages[code] || '请求失败（' + response.status + '）') +
      (explanation ? '。' + explanation : '') + (payload?.requestId ? ' · ' + payload.requestId : ''))
  }
  return payload.data as T
}

export function checkSession() {
  return sessionRequest ??= api<Account>('/me').then(user => { account.value = user }).catch(() => {}).finally(() => { sessionRequest = undefined })
}

export function useList<T>(endpoint: () => string, filters: Filters = {}) {
  const applied = ref<Filters>({})
  const page = ref(0)
  const cursors = ref([''])
  const reload = ref(0)
  const data = shallowRef<Page<T> | null>(null)
  const status = reactive({ loading: false, error: '' })
  watch(() => [endpoint(), applied.value, page.value, reload.value] as const, async ([path, params, index], _, cleanup) => {
    if (!path) return
    const request = new AbortController()
    cleanup(() => request.abort())
    status.loading = true
    status.error = ''
    data.value = null
    try {
      const response = await api<Page<T>>(path, { query: { ...params, cursor: cursors.value[index] || '' }, signal: request.signal })
      if (!request.signal.aborted) data.value = response
    } catch (e) {
      if (!request.signal.aborted) status.error = errorText(e)
    } finally {
      if (!request.signal.aborted) status.loading = false
    }
  }, { immediate: true })
  return { data, status, page, refresh: () => { reload.value++ }, search: () => {
    cursors.value = ['']
    page.value = 0
    applied.value = { ...filters }
  }, previous: () => { if (page.value) page.value-- }, next: () => {
    if (!data.value?.nextCursor) return
    cursors.value[page.value + 1] = data.value.nextCursor
    page.value++
  } }
}

export const errorText = (e: unknown) => e instanceof Error ? e.message : '操作失败，请重试'
export const formatDate = (value: string | null | undefined) => value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '—'
export const formatCount = (value: number) => value.toLocaleString('zh-CN')
export function formatBytes(value: number) {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++ }
  return value.toLocaleString('zh-CN', { maximumFractionDigits: 1 }) + ' ' + units[unit]
}
export const percent = (n: number, d: number) => d > 0 ? (100 * n / d).toFixed(1) + '%' : '—'
export const platforms = { douyin: '抖音', kuaishou: '快手', xiaohongshu: '小红书', bilibili: '哔哩哔哩', mock: 'Mock', unknown: '未知' } as Record<string, string>
export const registeredPlatforms = Object.fromEntries(Object.entries(platforms).filter(([key]) => key !== 'unknown'))
export const roles: Record<Role, string> = { viewer: '查看者', operator: '运营', tech_admin: '技术管理员', support: '客服', auditor: '审计', super_admin: '管理员' }
export const stages: Record<string, string> = { parse: '解析', media_prepare: '首次准备', media_retry: '重新准备', download: '完整中转下载', range_download: '分段中转下载', client_download: '客户端下载', preview: '预览', thumbnail: '缩略图', image: '图片', album: '相册', save: '保存' }
