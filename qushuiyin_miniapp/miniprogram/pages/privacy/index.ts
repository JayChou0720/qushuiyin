import { clearMediaCache } from '../../services/media-cache'
import { clearServiceData, openPrivacyContract } from '../../services/privacy'
import { verifyUserIdentity, hasHistoryIdentity, isCurrentUser, loadConfig, PublicConfig } from '../../services/api'
import { clearHistoryThumbnails, loadHistory, setHistoryEnabled } from '../../services/history'

Page({
  data: { busy: '', legalMessage: '', localMessage: '', remoteError: '', historyReady: false, historyEnabled: true, historyMessage: '' },
  disposed: false,
  historyOwner: '',
  limits: null as PublicConfig['limits'] | null,
  onShow() { if (hasHistoryIdentity() && !this.data.busy) void this.initHistory(false) },
  onUnload() { this.disposed = true },
  async initHistory(allowNew = true) {
    if (this.data.busy) return
    this.historyOwner = ''
    this.setData({ busy: 'history', historyReady: false, historyMessage: '' })
    try {
      const [owner, config] = await Promise.all([verifyUserIdentity(allowNew), loadConfig()])
      if (this.disposed || !isCurrentUser(owner)) return
      this.historyOwner = owner; this.limits = config.limits
      this.setData({ historyReady: true, historyEnabled: loadHistory(owner, config.limits).enabled })
    } catch { if (!this.disposed) this.setData({ historyMessage: '暂时无法读取记录设置，请联网后重试' }) }
    finally { if (!this.disposed) this.setData({ busy: '' }) }
  },
  configureHistory() { void this.initHistory() },
  toggleHistory(e: WechatMiniprogram.SwitchChange) {
    const owner = this.historyOwner, limits = this.limits
    if (this.data.busy || !limits) return
    if (!isCurrentUser(owner)) { void this.initHistory(false); return }
    const enabled = e.detail.value, previous = this.data.historyEnabled
    this.setData({ busy: 'history', historyEnabled: enabled, historyMessage: '' })
    const finish = (clear?: boolean) => {
      if (this.disposed) return
      if (!isCurrentUser(owner)) {
        this.setData({ busy: '', historyReady: false, historyMessage: '当前使用身份已变更，请重新读取设置' })
        return
      }
      let value = previous, message = ''
      if (clear !== undefined) {
        try {
          const ok = setHistoryEnabled(owner, enabled, clear)
          value = enabled
          if (!ok) message = '已清空记录，部分缩略图可稍后重试清理'
        } catch { message = '未能保存记录设置，请重试' }
      }
      this.setData({ busy: '', historyEnabled: value, historyMessage: message })
    }
    try {
      if (enabled || !loadHistory(owner, limits).entries.length) { finish(false); return }
      wx.showActionSheet({ itemList: ['关闭并保留记录', '关闭并清空记录'],
        success: ({ tapIndex }) => {
          if (tapIndex === 0) { finish(false); return }
          wx.showModal({ title: '关闭并清空记录？', content: '只删除此身份的本机记录和缩略图，已保存到相册的文件会保留。',
            confirmText: '确认清空', confirmColor: '#9d2330', success: ({ confirm }) => finish(confirm ? true : undefined), fail: () => finish() })
        }, fail: () => finish() })
    } catch { finish(); this.setData({ historyMessage: '暂时无法读取本机记录，请重试' }) }
  },
  async showContract() {
    this.setData({ legalMessage: '' })
    try { await openPrivacyContract() } catch (error) {
      if (!this.disposed) this.setData({ legalMessage: (error as Error).message })
    }
  },
  async clearLocal() {
    if (this.data.busy) return
    this.setData({ busy: 'local', localMessage: '' })
    const result = await clearMediaCache()
    const historyCacheCleared = clearHistoryThumbnails()
    if (this.disposed) return
    this.setData({ busy: '', localMessage: result.failed || !historyCacheCleared ? '部分缓存未能清理，可稍后重试' : '本机缓存已清理' })
  },
  confirmRemote() {
    if (this.data.busy) return
    this.setData({ busy: 'confirm', remoteError: '' })
    wx.showModal({ title: '清除服务端数据？',
      content: '解析结果、反馈和旧会话将立即失效，在线副本会随后清理。此身份的本机记录也会清除，相册不受影响。',
      confirmText: '确认清除', confirmColor: '#9d2330',
      success: result => {
        if (this.disposed) return
        if (result.confirm) void this.clearRemote()
        else this.setData({ busy: '' })
      },
      fail: () => { if (!this.disposed) this.setData({ busy: '', remoteError: '未能打开确认窗口，请重试' }) },
    })
  },
  async clearRemote() {
    this.setData({ busy: 'remote' })
    try {
      await clearServiceData()
      // Discard any screens still holding the revoked user's content.
      wx.reLaunch({ url: '/pages/home/index', fail: () => {
        if (!this.disposed) this.setData({ busy: '', remoteError: '已停止访问旧数据，请重新打开小程序' })
      } })
    } catch (error) {
      if (!this.disposed) this.setData({ busy: '', remoteError: (error as Error).message })
    }
  },
})
