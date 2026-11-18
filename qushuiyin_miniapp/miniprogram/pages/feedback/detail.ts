import { APIError, queryFeedback, FeedbackDetail } from '../../services/api'
import { categoryLabel, statusLabel, displayTime, validId } from './shared'

type Ticket = FeedbackDetail & { categoryLabel: string; statusLabel: string; submitted: string; lastReply: string }
Page({
  data: { ticket: null as Ticket | null, busy: true, missing: false, error: '' },
  ticketId: '',
  sequence: 0,
  onLoad(query) { if (validId(query.id)) this.ticketId = query.id || '' },
  onShow() { void this.load() },
  onHide() { this.sequence++; this.setData({ ticket: null, busy: true, error: '' }) },
  onUnload() { this.sequence++ },
  async onPullDownRefresh() { try { await this.load() } finally { wx.stopPullDownRefresh() } },
  async load() {
    const sequence = ++this.sequence
    if (!this.ticketId) { this.setData({ busy: false, missing: true }); return }
    this.setData({ ticket: null, busy: true, missing: false, error: '' })
    try {
      const f = await queryFeedback(this.ticketId)
      if (sequence === this.sequence) this.setData({ ticket: { ...f, categoryLabel: categoryLabel(f.category), statusLabel: statusLabel(f.status), submitted: displayTime(f.createdAt), lastReply: f.repliedAt ? displayTime(f.repliedAt) : '' } })
    } catch (error) {
      if (sequence !== this.sequence) return
      if (error instanceof APIError && error.code === 'RESOURCE_NOT_FOUND') this.setData({ missing: true })
      else this.setData({ error: (error as Error).message })
    } finally { if (sequence === this.sequence) this.setData({ busy: false }) }
  },
})
