import { APIError, revokeMyData } from './api'
import { clearMediaCache } from './media-cache'

type PrivacyAPI = typeof wx & { openPrivacyContract?: (options: { success(): void; fail(): void }) => void }
let cleanupNotice = ''

export function openPrivacyContract() {
  return new Promise<void>((resolve, reject) => {
    const api = wx as PrivacyAPI
    const unavailable = () => reject(new Error('当前无法打开微信隐私保护指引'))
    if (!api.openPrivacyContract) { unavailable(); return }
    api.openPrivacyContract({ success: resolve, fail: unavailable })
  })
}

export function takeCleanupNotice() {
  const notice = cleanupNotice
  cleanupNotice = ''
  return notice
}

export async function clearServiceData() {
  try {
    const { receipt, localIdentityCleared } = await revokeMyData()
    const local = await clearMediaCache()
    const deadline = new Date(receipt.cleanupDeadlineAt)
    cleanupNotice = '已停止访问旧数据。' + (receipt.cleanupStatus === 'scheduled' && Number.isFinite(deadline.getTime())
      ? '预计于 ' + deadline.toLocaleString() + ' 前清理在线副本。' : '服务端清理时间暂无法确认。')
    if (local.failed || !localIdentityCleared) cleanupNotice += '部分本机数据未能清理。'
  } catch (error) {
    if (error instanceof APIError && (error.code === 'SESSION_EXPIRED' || error.code === 'AUTH_REQUIRED')) {
      throw new Error('原会话已失效，无法确认清理结果。可通过问题反馈核实。')
    }
    throw error
  }
}
