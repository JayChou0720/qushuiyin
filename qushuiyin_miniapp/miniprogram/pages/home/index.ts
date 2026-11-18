import { APIError, Candidate, cancelJob, id, loadConfig, queryJob, startParse, wait } from '../../services/api'
import { takeCleanupNotice } from '../../services/privacy'

const names: Record<string, string> = { douyin: '抖音', xiaohongshu: '小红书', kuaishou: '快手', bilibili: 'B 站' }
Page({
  data: { text: '', pasted: false, maxLength: 0, parseWait: 0, inputReady: false, busy: false, error: '', job: '', feedbackTrace: '', cleanupNotice: '', sequence: 0, candidates: [] as Candidate[], platforms: [] as { name: string; available: boolean }[] },
  onLoad() { this.setData({ cleanupNotice: takeCleanupNotice() }); void this.reloadConfig() },
  onUnload() { void this.cancel() },
  async reloadConfig() {
    try {
      const config = await loadConfig()
      this.setData({ inputReady: true, error: '', maxLength: config.limits.max_share_text_chars, parseWait: config.limits.client_parse_wait_seconds,
        platforms: config.platforms.map(p => ({ name: names[p.platform] || p.platform, available: p.status === 'enabled' && (p.videoEnabled || p.imageEnabled) })) })
    } catch (error) { this.setData({ error: (error as Error).message }) }
  },
  edit(e: WechatMiniprogram.CustomEvent<{ value: string }>) { this.setData({ text: e.detail.value, pasted: false, feedbackTrace: '', candidates: [], error: '' }) },
  paste() {
    wx.getClipboardData({ success: r => this.setData({ text: r.data, pasted: true, feedbackTrace: '', candidates: [], error: '' }), fail: () => this.setData({ feedbackTrace: '', error: '未读取剪贴板，可手动输入链接' }) })
  },
  example() { this.setData({ text: 'https://www.douyin.com/video/10001', pasted: false, feedbackTrace: '', candidates: [], error: '' }) },
  parse() { void this.submit(null) },
  choose(e: WechatMiniprogram.CustomEvent) { const candidate = this.data.candidates[Number(e.currentTarget.dataset.index)]; if (candidate) void this.submit(candidate.url) },
  async submit(selectedUrl: string | null) {
    if (this.data.busy) return
    if (!this.data.text.trim()) { this.setData({ error: '请粘贴作品分享链接', feedbackTrace: '' }); return }
    const sequence = this.data.sequence + 1, flowId = id()
    this.setData({ sequence, busy: true, error: '', candidates: [], job: '', feedbackTrace: '', cleanupNotice: '' })
    try {
      let job = await startParse({ shareText: this.data.text, selectedUrl, source: this.data.pasted ? 'paste' : 'manual', flowId }, id())
      if (this.data.sequence !== sequence) { await cancelJob(job.jobId); return }
      this.setData({ job: job.jobId })
      const delay = job.pollAfterMs, end = Math.min(Date.parse(job.deadlineAt), Date.now() + this.data.parseWait * 1000)
      while (job.status === 'QUEUED' || job.status === 'RUNNING') {
        await wait(delay)
        if (this.data.sequence !== sequence) return
        if (Date.now() >= end) { await cancelJob(job.jobId); throw new Error('解析等待超时，请重试') }
        job = await queryJob(job.jobId)
      }
      if (this.data.sequence !== sequence) return
      if (job.status !== 'SUCCEEDED' || !job.resultId) {
        this.setData({ feedbackTrace: job.error?.traceId || '' })
        throw new Error(job.error?.message || '未能取得解析结果')
      }
      wx.navigateTo({ url: '/pages/result/index?resultId=' + encodeURIComponent(job.resultId) + '&flowId=' + encodeURIComponent(flowId) })
    } catch (error) {
      if (this.data.sequence !== sequence) return
      this.setData({ error: (error as Error).message, candidates: error instanceof APIError ? error.candidates : [] })
    } finally {
      if (this.data.sequence === sequence) this.setData({ busy: false, job: '' })
    }
  },
  async cancel() {
    const job = this.data.job
    this.setData({ sequence: this.data.sequence + 1, busy: false, job: '' })
    if (job) { try { await cancelJob(job) } catch { /* The expired mock job will be removed by the service. */ } }
  },
})
