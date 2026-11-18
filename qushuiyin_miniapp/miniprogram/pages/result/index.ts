import { APIError, Asset, Batch, DirectRejected, Result, cancelBatch, copyDownloadURL, downloadSample, fallback, id, isCurrentUser, loadConfig, previewAsset, queryBatch, queryOwnedResult, retryBatch, startBatch, tickets, wait } from '../../services/api'
import { cacheThumbnail, noteHistorySave, recordHistory, takeHistory } from '../../services/history'
import { enqueueClientEvent } from '../../services/events'

type SaveOutcome = 'succeeded' | 'partial' | 'failed' | 'cancelled' | 'unknown'
interface SaveEvent { actionId: string; started: number; batchId?: string }

Page({
  data: { result: null as Result | null, flow: '', resultId: '', selected: {} as Record<string, boolean>, count: 0, max: 0, busy: false, error: '', status: '', batchId: '', sequence: 0,
    retryAvailable: false, copyItems: [] as { itemId: string; order: number }[],
    thumbnails: {} as Record<string, string>, previewKind: '', previewURL: '', previewBusy: false, previewError: '', previewOrder: 0, previewGeneration: 0 },
  downloadTask: null as WechatMiniprogram.DownloadTask | null,
  downloaded: null as unknown as Set<string>,
  observers: [] as WechatMiniprogram.IntersectionObserver[],
  previewSequence: 0,
  previewExpires: 0,
  previewRefreshed: false,
  previewEvent: null as { started: number; rendered: boolean } | null,
  disposed: false,
  historyOwner: '',
  jobId: undefined as string | undefined,
  actionId: '',
  activeSave: null as SaveEvent | null,
  onLoad(query) { this.downloaded = new Set(); this.setData({ resultId: query.resultId || '', flow: query.flowId || id() }); void this.load() },
  onHide() { this.closePreview() },
  onUnload() { this.disposed = true; this.finishPreview(this.previewEvent?.rendered ? 'succeeded' : 'cancelled'); this.previewSequence++; for (const o of this.observers) o.disconnect(); void this.cancel() },
  async load() {
    try {
      const history = takeHistory(this.data.resultId)
      const [{ data: result, userId }, config] = await Promise.all([queryOwnedResult(this.data.resultId, history?.userId), loadConfig()])
      const selected: Record<string, boolean> = {}
      const max = config.limits.max_image_batch_count
      for (const asset of result.assets.slice(0, max)) selected[asset.assetId] = true
      if (this.disposed || !isCurrentUser(userId)) return
      this.historyOwner = userId
      this.jobId = history?.jobId
      if (history) {
        const entry = recordHistory(history, result, config.limits)
        const thumbnail = result.mediaType === 'video' ? result.cover : result.assets[0]
        if (entry && thumbnail && result.capabilities.canPreview) {
          void cacheThumbnail(entry, () => previewAsset(result.resultId, thumbnail.assetId, 'thumbnail'), config.limits)
        }
      }
      this.setData({ result, selected, max, count: Object.keys(selected).length, error: '' }, () => {
        if (this.disposed) return
        enqueueClientEvent(userId, 'result_viewed', 'succeeded', { flowId: this.data.flow, jobId: this.jobId })
        this.observeThumbnails(result)
      })
    } catch (error) { if (!this.disposed) this.setData({ error: (error as Error).message }) }
  },
  observeThumbnails(result: Result) {
    if (this.disposed || !result.capabilities.canPreview) return
    for (const o of this.observers) o.disconnect()
    this.observers = []
    const assets = result.mediaType === 'video' ? (result.cover ? [result.cover] : []) : result.assets
    for (const asset of assets) {
      const observer = wx.createIntersectionObserver(this).relativeToViewport()
      observer.observe('#thumb-' + asset.assetId, entry => {
        if (entry.intersectionRatio <= 0) return
        observer.disconnect()
        void this.loadThumbnail(asset.assetId)
      })
      this.observers.push(observer)
    }
  },
  async loadThumbnail(assetId: string) {
    try {
      const ticket = await previewAsset(this.data.resultId, assetId, 'thumbnail')
      if (!this.disposed) this.setData({ ['thumbnails.' + assetId]: ticket.url })
    } catch { /* An unavailable thumbnail leaves its labeled placeholder in place. */ }
  },
  play() {
    const video = this.data.result?.assets[0]
    if (video) this.openPreview(video, 'video')
  },
  viewImage(e: WechatMiniprogram.CustomEvent) {
    const asset = this.data.result?.assets.find(a => a.assetId === e.currentTarget.dataset.asset)
    if (asset) this.openPreview(asset, 'image')
  },
  openPreview(asset: Asset, kind: 'video' | 'image') {
    this.finishPreview(this.previewEvent?.rendered ? 'succeeded' : 'cancelled')
    this.previewEvent = { started: Date.now(), rendered: false }
    enqueueClientEvent(this.historyOwner, 'preview_started', 'started', { flowId: this.data.flow, jobId: this.jobId })
    this.previewRefreshed = false
    this.setData({ previewOrder: asset.order })
    void this.loadPreview(asset, kind)
  },
  async loadPreview(asset: Asset, kind: 'video' | 'image') {
    const sequence = ++this.previewSequence
    this.setData({ previewKind: kind, previewURL: '', previewBusy: true, previewError: '', previewGeneration: sequence })
    try {
      const ticket = await previewAsset(this.data.resultId, asset.assetId, kind)
      if (sequence !== this.previewSequence) return
      this.previewExpires = ticket.expires
      this.setData({ previewURL: ticket.url, previewBusy: false })
    } catch (error) {
      if (sequence !== this.previewSequence) return
      this.finishPreview('failed')
      const expired = error instanceof APIError && error.code === 'RESULT_EXPIRED'
      this.setData({ previewBusy: false, previewError: expired ? '解析结果已过期，请返回首页重新解析' : '预览暂不可用，可尝试保存' })
    }
  },
  previewShown(e: WechatMiniprogram.CustomEvent) {
    if (!this.previewEvent || e.currentTarget.dataset.generation !== this.previewSequence) return
    this.previewEvent.rendered = true
    if (this.data.previewKind === 'image') this.finishPreview('succeeded')
  },
  previewEnded(e: WechatMiniprogram.CustomEvent) {
    if (e.currentTarget.dataset.generation === this.previewSequence) this.finishPreview('succeeded')
  },
  previewFailed(e: WechatMiniprogram.CustomEvent) {
    if (e.currentTarget.dataset.generation !== this.previewSequence || !this.data.previewURL) return
    const kind = this.data.previewKind === 'video' ? 'video' : 'image'
    const asset = this.data.result?.assets.find(a => a.order === this.data.previewOrder)
    if (asset && !this.previewRefreshed && Date.now() >= this.previewExpires) {
      this.previewRefreshed = true
      void this.loadPreview(asset, kind)
      return
    }
    this.finishPreview('failed')
    this.setData({ previewURL: '', previewError: '预览暂不可用，可尝试保存' })
  },
  finishPreview(outcome: 'succeeded' | 'failed' | 'cancelled') {
    const event = this.previewEvent
    if (!event) return
    this.previewEvent = null
    enqueueClientEvent(this.historyOwner, 'preview_finished', outcome,
      { flowId: this.data.flow, jobId: this.jobId, durationMs: Math.max(0, Date.now() - event.started) })
  },
  closePreview() {
    this.finishPreview(this.previewEvent?.rendered ? 'succeeded' : 'cancelled')
    this.previewSequence++
    if (this.data.previewKind) this.setData({ previewKind: '', previewURL: '', previewBusy: false, previewError: '' })
  },
  toggle(e: WechatMiniprogram.CustomEvent) {
    if (this.data.busy) return
    const asset = String(e.currentTarget.dataset.asset)
    const selected = { ...this.data.selected }
    if (selected[asset]) delete selected[asset]
    else if (this.data.count < this.data.max) selected[asset] = true
    else { this.setData({ error: '一次最多选择 ' + this.data.max + ' 张图片' }); return }
    this.setData({ selected, count: Object.keys(selected).length, error: '' })
  },
  copy() { const text = this.data.result?.description; if (text) wx.setClipboardData({ data: text }) },
  save() { const assets = this.data.result?.assets.filter(a => this.data.selected[a.assetId]); if (assets?.length) void this.runSave(assets) },
  saveCover() { const cover = this.data.result?.cover; if (cover) void this.runSave([cover]) },
  async runSave(assets?: Asset[]) {
    if (this.data.busy) return
    const sequence = this.data.sequence + 1
    const previous = this.data.batchId
    const event: SaveEvent = { actionId: assets ? id() : this.actionId, started: Date.now(), batchId: assets ? undefined : previous }
    this.activeSave = event
    enqueueClientEvent(this.historyOwner, 'save_action_started', 'started',
      { flowId: this.data.flow, jobId: this.jobId, clientActionId: event.actionId, batchId: event.batchId })
    this.setData({ sequence, busy: true, error: '', status: '正在准备样本', retryAvailable: false, copyItems: [] })
    let outcome: SaveOutcome = 'unknown'
    try {
      const config = await loadConfig()
      if (this.data.sequence !== sequence) return
      let batch: Batch
      if (assets) {
        this.downloaded.clear()
        this.setData({ batchId: '' })
        if (previous) void cancelBatch(previous).catch(() => {})
        batch = await startBatch(this.data.resultId, assets.map(a => a.assetId), this.data.flow, event.actionId)
        if (this.data.sequence !== sequence) { await cancelBatch(batch.batchId); return }
        this.actionId = event.actionId
        event.batchId = batch.batchId
        this.setData({ batchId: batch.batchId })
      } else {
        batch = await queryBatch(previous)
        if (this.data.sequence !== sequence) return
        const ids = batch.items.filter(x => !this.downloaded.has(x.itemId) && (x.prepareStatus === 'EXPIRED' || (x.prepareStatus === 'FAILED' && x.error?.retryable))).map(x => x.itemId)
        if (ids.length) batch = await retryBatch(previous, ids, id())
      }
      outcome = await this.consumeBatch(batch, config.limits.poll_interval_ms, config.limits.client_download_timeout_seconds * 1000, sequence, event)
    } catch (error) {
      if (this.data.sequence === sequence) {
        outcome = this.downloaded.size ? 'partial' : 'failed'
        const permanent = error instanceof APIError && ['FILE_NOT_READY', 'RETRY_LIMIT_REACHED', 'RESULT_EXPIRED', 'BATCH_EXPIRED', 'RESOURCE_NOT_FOUND'].includes(error.code)
        this.setData({ error: (error as Error).message, status: '演练未完成', retryAvailable: Boolean(this.data.batchId) && !permanent })
      }
    } finally {
      if (this.activeSave === event) this.finishSave(outcome)
      if (this.data.sequence === sequence) { this.downloadTask = null; this.setData({ busy: false }) }
    }
  },
  finishSave(outcome: SaveOutcome) {
    const event = this.activeSave
    if (!event) return
    this.activeSave = null
    enqueueClientEvent(this.historyOwner, 'save_action_finished', outcome, { flowId: this.data.flow, jobId: this.jobId,
      clientActionId: event.actionId, batchId: event.batchId, durationMs: Math.max(0, Date.now() - event.started),
      errorCode: outcome === 'cancelled' ? 'USER_CANCELLED' : outcome === 'unknown' ? 'SAVE_OUTCOME_UNKNOWN' : undefined })
  },
  async consumeBatch(initial: Batch, pollMs: number, downloadMs: number, sequence: number, event: SaveEvent): Promise<SaveOutcome> {
    let batch = initial
    let downloading = true
    while (downloading && this.data.sequence === sequence) {
      while (batch.status === 'PREPARING') {
        await wait(batch.pollAfterMs || pollMs)
        if (this.data.sequence !== sequence) return 'cancelled'
        batch = await queryBatch(batch.batchId)
      }
      if (this.data.sequence !== sequence) return 'cancelled'
      downloading = false
      let failed = false
      for (const item of batch.items) {
        if (this.data.sequence !== sequence) return 'cancelled'
        if (item.prepareStatus !== 'READY' || this.downloaded.has(item.itemId)) continue
        this.setData({ status: '正在下载样本 ' + (this.downloaded.size + 1) + ' / ' + batch.items.length })
        try {
          const response = await tickets(batch.batchId, [item.itemId])
          if (this.data.sequence !== sequence) return 'cancelled'
          const ticket = response.tickets.find(entry => entry.itemId === item.itemId)
          if (!ticket) throw new Error('下载票据不完整')
          const started = Date.now()
          const details = { flowId: this.data.flow, jobId: this.jobId, clientActionId: event.actionId, batchId: batch.batchId, itemId: item.itemId, deliveryMode: ticket.deliveryMode }
          try {
            const bytes = await downloadSample(ticket, downloadMs, task => { this.downloadTask = task })
            enqueueClientEvent(this.historyOwner, 'client_download_finished', 'succeeded', { ...details, bytes: bytes ?? undefined, durationMs: Math.max(0, Date.now() - started) })
          } catch (error) {
            const cancelled = this.data.sequence !== sequence
            enqueueClientEvent(this.historyOwner, 'client_download_finished', cancelled ? 'cancelled' : 'failed', { ...details,
              durationMs: Math.max(0, Date.now() - started), errorCode: cancelled ? 'USER_CANCELLED' : error instanceof DirectRejected ? 'DIRECT_DOWNLOAD_REJECTED' : 'CLIENT_DOWNLOAD_FAILED' })
            throw error
          }
          if (this.data.sequence !== sequence) return 'cancelled'
          this.downloaded.add(item.itemId)
        } catch (error) {
          if (this.data.sequence !== sequence) return 'cancelled'
          if (error instanceof DirectRejected) {
            this.setData({ status: '直连不可用，正在切换下载方式' })
            batch = await fallback(batch.batchId, item.itemId, id())
            downloading = true
            break
          }
          failed = true
        }
      }
      if (!downloading && this.data.sequence === sequence) {
        const incomplete = batch.items.filter(x => !this.downloaded.has(x.itemId))
        const copyItems = incomplete.filter(x => x.copyLinkAvailable).map(x => ({ itemId: x.itemId, order: x.order }))
        const retryAvailable = failed || incomplete.some(x => x.prepareStatus === 'EXPIRED' || x.error?.retryable)
        const error = incomplete.find(x => !x.copyLinkAvailable && x.error)?.error?.message || (failed ? '部分样本未能下载，请重试' : '')
        const status = incomplete.length === 0 ? '样本已下载，未写入相册' : this.downloaded.size ? '已下载 ' + this.downloaded.size + ' 项，未写入相册' : copyItems.length ? '可复制下载链接' : '演练未完成'
        if (this.historyOwner) noteHistorySave(this.historyOwner, this.data.resultId, status)
        this.setData({ error, status, copyItems, retryAvailable })
        return incomplete.length === 0 ? 'succeeded' : this.downloaded.size ? 'partial' : 'failed'
      }
    }
    return 'cancelled'
  },
  retrySave() { if (this.data.batchId && this.data.retryAvailable) void this.runSave() },
  async copyLink(e: WechatMiniprogram.CustomEvent) {
    const batchId = this.data.batchId
    try {
      const url = await copyDownloadURL(batchId, String(e.currentTarget.dataset.item))
      if (this.disposed || this.data.batchId !== batchId) return
      wx.setClipboardData({ data: url, success: () => this.setData({ status: '已复制，链接很快会失效' }), fail: () => this.setData({ error: '无法写入剪贴板' }) })
    } catch (error) { if (!this.disposed) this.setData({ error: (error as Error).message }) }
  },
  async cancel() {
    const batch = this.data.batchId
    const wasBusy = this.data.busy
    this.finishSave('cancelled')
    this.setData({ sequence: this.data.sequence + 1, busy: false, batchId: '', retryAvailable: false, copyItems: [], status: wasBusy ? '已取消演练' : this.data.status })
    this.downloadTask?.abort(); this.downloadTask = null
    if (batch) { try { await cancelBatch(batch) } catch { /* The expired mock batch will be removed by the service. */ } }
  },
})
