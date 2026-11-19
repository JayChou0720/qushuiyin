import { APIError, PublicConfig, cancelJob, verifyUserIdentity, id, isCurrentUser, loadConfig, queryResult } from '../../services/api'
import { HistoryEntry, clearHistoryRecords, deleteHistoryItem, loadHistory, stageHistory, thumbnailPath, touchHistory } from '../../services/history'
import { parseFlow } from '../../services/parse-flow'

const names: Record<string, string> = { douyin: '抖音', xiaohongshu: '小红书', kuaishou: '快手', bilibili: 'B 站' }
type Row = { localId: string; title: string; platform: string; meta: string; summary: string; thumbnail: string }
Page({
  data: { rows: [] as Row[], loading: true, busy: false, enabled: true, error: '', notice: '', ttl: 0, limit: 0 },
  owner: '', revision: 0, jobId: '', config: null as PublicConfig | null,
  entries: [] as HistoryEntry[],
  onShow() { void this.refresh() },
  onHide() { this.leave() },
  onUnload() { this.leave() },
  async onPullDownRefresh() { try { if (!this.data.busy) await this.refresh() } finally { wx.stopPullDownRefresh() } },
  leave() {
    const { jobId, owner } = this
    this.revision++; this.owner = ''; this.jobId = ''; this.entries = []
    this.setData({ rows: [], loading: true, busy: false, error: '', notice: '' })
    if (jobId && isCurrentUser(owner)) void cancelJob(jobId, owner).catch(() => {})
  },
  async refresh() {
    const n = ++this.revision
    this.owner = ''; this.entries = []
    this.setData({ rows: [], loading: true, busy: false, error: '', notice: '' })
    try {
      const [owner, config] = await Promise.all([verifyUserIdentity(), loadConfig()])
      if (this.revision !== n || !isCurrentUser(owner)) return
      this.owner = owner; this.config = config
      this.renderRows()
    } catch { if (this.revision === n) this.setData({ error: '暂时无法读取最近记录，请联网后重试' }) }
    finally { if (this.revision === n) this.setData({ loading: false }) }
  },
  renderRows() {
    const config = this.config
    if (!config || !isCurrentUser(this.owner)) return
    const { entries, enabled } = loadHistory(this.owner, config.limits)
    this.entries = entries
    this.setData({ enabled, ttl: config.limits.history_ttl_days, limit: config.limits.history_max_count,
      rows: entries.map(item => ({ localId: item.localId, title: item.title || '未命名作品', platform: names[item.platform] || '作品',
        meta: (item.mediaType === 'video' ? '视频' : '图集 · ' + item.assetCount + ' 张') + ' · ' + new Date(item.lastUsedAt).toLocaleDateString(),
        summary: item.lastSaveSummary ? '上次：' + item.lastSaveSummary : '', thumbnail: thumbnailPath(item) })) })
  },
  async open(e: WechatMiniprogram.CustomEvent) {
    const owner = this.owner, config = this.config
    if (this.data.busy || !config || !isCurrentUser(owner)) return
    const item = this.entries.find(entry => entry.localId === e.currentTarget.dataset.id)
    if (!item) return
    const n = ++this.revision, flowId = id()
    const active = () => this.revision === n && isCurrentUser(owner)
    this.setData({ busy: true, error: '', notice: '' })
    try {
      let resultId = ''
      if (item.resultExpiresAt > Date.now()) {
        try { resultId = (await queryResult(item.resultId, owner)).resultId } catch (error) {
          if (!(error instanceof APIError) || !['RESULT_EXPIRED', 'RESOURCE_NOT_FOUND'].includes(error.code)) throw error
        }
      }
      if (!active()) return
      if (!resultId) {
        const result = await parseFlow({ shareText: item.sourceUrl, selectedUrl: item.sourceUrl, source: 'history', flowId }, config.limits.client_parse_wait_seconds,
          active, job => { this.jobId = job }, owner)
        if (!result || !active()) return
        resultId = result.resultId
        stageHistory(result)
      } else {
        touchHistory(owner, item.localId)
        stageHistory({ resultId, userId: owner })
      }
      this.jobId = ''
      wx.navigateTo({ url: '/pages/result/index?resultId=' + encodeURIComponent(resultId) + '&flowId=' + encodeURIComponent(flowId),
        fail: () => { if (active()) this.setData({ error: '未能打开结果页，请重试' }) } })
    } catch (error) {
      if (this.revision !== n) return
      if (!isCurrentUser(owner)) { void this.refresh(); return }
      const fromSource = error instanceof APIError && ['UPSTREAM_UNAVAILABLE', 'LINK_INVALID', 'LINK_NOT_ALLOWED', 'CONTENT_TYPE_UNSUPPORTED'].includes(error.code)
      this.setData({ error: fromSource ? '原分享链接可能已失效，请从原平台重新复制' : (error as Error).message })
    } finally {
      if (this.revision === n) {
        this.jobId = ''
        if (isCurrentUser(owner)) this.setData({ busy: false })
        else void this.refresh()
      }
    }
  },
  remove(e: WechatMiniprogram.CustomEvent) {
    if (this.data.busy || !isCurrentUser(this.owner)) return
    const localId = this.entries.find(entry => entry.localId === e.currentTarget.dataset.id)?.localId
    if (!localId) return
    try {
      const ok = deleteHistoryItem(this.owner, localId)
      this.renderRows()
      this.setData({ error: '', notice: ok ? '已删除记录' : '记录已删除，缩略图可在本机缓存中重试清理' })
    } catch { this.setData({ error: '未能删除本机记录，请重试' }) }
  },
  clearAll() {
    const owner = this.owner, revision = this.revision
    if (this.data.busy || !isCurrentUser(owner)) return
    wx.showModal({ title: '清空最近记录？', content: '只删除此身份的本机记录和缩略图，已保存到相册的文件会保留。', confirmText: '确认清空', confirmColor: '#9d2330',
      success: result => {
        if (!result.confirm || this.revision !== revision || !isCurrentUser(owner)) return
        try {
          const ok = clearHistoryRecords(owner)
          this.renderRows()
          this.setData({ error: '', notice: ok ? '已清空最近记录' : '记录已清空，部分缩略图未能清理' })
        } catch { this.setData({ error: '未能清空本机记录，请重试' }) }
      },
    })
  },
  thumbnailError(e: WechatMiniprogram.CustomEvent) {
    const rows = this.data.rows.map(row => row.localId === e.currentTarget.dataset.id ? { ...row, thumbnail: '' } : row)
    this.setData({ rows })
  },
})
