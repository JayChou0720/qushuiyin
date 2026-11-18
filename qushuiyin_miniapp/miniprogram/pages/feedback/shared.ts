import { FeedbackStatus } from '../../services/api'

export const categories = [
  { value: 'parse_failed', label: '解析失败' },
  { value: 'download_failed', label: '下载失败' },
  { value: 'album_failed', label: '相册失败' },
  { value: 'gallery_issue', label: '图集问题' },
  { value: 'suggestion', label: '建议' },
  { value: 'other', label: '其他' },
]
const states: Record<FeedbackStatus, string> = { pending: '待处理', processing: '处理中', replied: '已回复', closed: '已关闭' }
export const categoryLabel = (value: string) => categories.find(c => c.value === value)?.label || '其他'
export const statusLabel = (value: FeedbackStatus) => states[value] || '状态未知'
export const validId = (value?: string) => !!value && /^[A-Za-z0-9_-]{1,64}$/.test(value)
export function displayTime(value: string) {
  const t = new Date(value)
  if (!Number.isFinite(t.getTime())) return '—'
  const two = (n: number) => String(n).padStart(2, '0')
  return t.getFullYear() + '-' + two(t.getMonth() + 1) + '-' + two(t.getDate()) + ' ' + two(t.getHours()) + ':' + two(t.getMinutes())
}
