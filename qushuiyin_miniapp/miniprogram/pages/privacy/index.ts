import { clearMediaCache } from '../../services/media-cache'
import { clearServiceData, openPrivacyContract } from '../../services/privacy'

Page({
  data: { busy: '', legalMessage: '', localMessage: '', remoteError: '' },
  disposed: false,
  onUnload() { this.disposed = true },
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
    if (this.disposed) return
    this.setData({ busy: '', localMessage: result.failed ? '部分临时文件未能清理，可稍后重试' : result.hadFiles ? '本机临时文件已清理' : '没有可清理的本机临时文件' })
  },
  confirmRemote() {
    if (this.data.busy) return
    this.setData({ busy: 'confirm', remoteError: '' })
    wx.showModal({ title: '清除服务端数据？',
      content: '解析结果、反馈和旧会话将立即失效，在线副本会随后清理。已保存到相册的文件不受影响。',
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
