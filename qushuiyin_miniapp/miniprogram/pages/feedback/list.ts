import { listFeedback, FeedbackSummary } from '../../services/api'
import { categoryLabel, displayTime, statusLabel } from './shared'

type Row = FeedbackSummary & { categoryLabel: string; statusLabel: string; submitted: string }
Page({
  data: { items: [] as Row[], cursor: '', busy: true, loadingMore: false, error: '' },
  version: 0,
  owner: '',
  onShow() { void this.refresh() },
  onHide() { this.version++; this.owner = ''; this.setData({ items: [], cursor: '', busy: true, loadingMore: false, error: '' }) },
  onUnload() { this.version++ },
  async onPullDownRefresh() { try { await this.refresh() } finally { wx.stopPullDownRefresh() } },
  refresh() { return this.load(false) },
  more() { if (!this.data.busy && !this.data.loadingMore && this.data.cursor) void this.load(true) },
  async load(append: boolean) {
    const version = ++this.version
    const cursor = append ? this.data.cursor : '', owner = append ? this.owner : ''
    if (append) this.setData({ loadingMore: true })
    else {
      this.owner = ''
      this.setData({ items: [], cursor: '', busy: true, loadingMore: false, error: '' })
    }
    try {
      const { data, userId } = await listFeedback(cursor, owner)
      if (version !== this.version) return
      this.owner = userId
      const items = data.items.map(item => ({ ...item, categoryLabel: categoryLabel(item.category), statusLabel: statusLabel(item.status), submitted: displayTime(item.createdAt) }))
      this.setData({ items: append ? this.data.items.concat(items) : items, cursor: data.hasMore ? data.nextCursor || '' : '' })
    } catch (error) {
      if (version !== this.version) return
      this.owner = ''
      // A failed fetch may follow reauthentication. Never keep rows from the previous identity.
      this.setData({ items: [], cursor: '', error: (error as Error).message })
    } finally { if (version === this.version) this.setData({ busy: false, loadingMore: false }) }
  },
})
