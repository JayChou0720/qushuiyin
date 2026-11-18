import { APIError, Asset, Batch, DirectRejected, Result, cancelBatch, copyDownloadURL, downloadSample, fallback, id, loadConfig, previewAsset, queryBatch, queryResult, retryBatch, startBatch, tickets, wait } from '../../services/api'

Page({
  data: { result: null as Result | null, flow: '', resultId: '', selected: {} as Record<string, boolean>, count: 0, max: 0, busy: false, error: '', status: '', batchId: '', sequence: 0,
    retryAvailable: false, copyItems: [] as { itemId: string; order: number }[],
    thumbnails: {} as Record<string, string>, previewKind: '', previewURL: '', previewBusy: false, previewError: '', previewOrder: 0 },
  downloadTask: null as WechatMiniprogram.DownloadTask | null,
  downloaded: null as unknown as Set<string>,
  observers: [] as WechatMiniprogram.IntersectionObserver[],
  previewSequence: 0,
  previewExpires: 0,
  previewRefreshed: false,
  disposed: false,
  onLoad(query) { this.downloaded = new Set(); this.setData({ resultId: query.resultId || '', flow: query.flowId || id() }); void this.load() },
  onHide() { this.closePreview() },
  onUnload() { this.disposed = true; this.previewSequence++; for (const o of this.observers) o.disconnect(); void this.cancel() },
  async load() {
    try {
      const [result, config] = await Promise.all([queryResult(this.data.resultId), loadConfig()])
      const selected: Record<string, boolean> = {}
      const max = config.limits.max_image_batch_count
      for (const asset of result.assets.slice(0, max)) selected[asset.assetId] = true
      if (this.disposed) return
      this.setData({ result, selected, max, count: Object.keys(selected).length, error: '' }, () => this.observeThumbnails(result))
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
    this.previewRefreshed = false
    this.setData({ previewOrder: asset.order })
    void this.loadPreview(asset, kind)
  },
  async loadPreview(asset: Asset, kind: 'video' | 'image') {
    const sequence = ++this.previewSequence
    this.setData({ previewKind: kind, previewURL: '', previewBusy: true, previewError: '' })
    try {
      const ticket = await previewAsset(this.data.resultId, asset.assetId, kind)
      if (sequence !== this.previewSequence) return
      this.previewExpires = ticket.expires
      this.setData({ previewURL: ticket.url, previewBusy: false })
    } catch (error) {
      if (sequence !== this.previewSequence) return
      const expired = error instanceof APIError && error.code === 'RESULT_EXPIRED'
      this.setData({ previewBusy: false, previewError: expired ? '解析结果已过期，请返回首页重新解析' : '预览暂不可用，可尝试保存' })
    }
  },
  previewFailed() {
    if (!this.data.previewURL) return
    const kind = this.data.previewKind === 'video' ? 'video' : 'image'
    const asset = this.data.result?.assets.find(a => a.order === this.data.previewOrder)
    if (asset && !this.previewRefreshed && Date.now() >= this.previewExpires) {
      this.previewRefreshed = true
      void this.loadPreview(asset, kind)
      return
    }
    this.setData({ previewURL: '', previewError: '预览暂不可用，可尝试保存' })
  },
  closePreview() {
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
    this.setData({ sequence, busy: true, error: '', status: '正在准备样本', retryAvailable: false, copyItems: [] })
    try {
      const config = await loadConfig()
      if (this.data.sequence !== sequence) return
      let batch: Batch
      if (assets) {
        this.downloaded.clear()
        this.setData({ batchId: '' })
        if (previous) void cancelBatch(previous).catch(() => {})
        batch = await startBatch(this.data.resultId, assets.map(a => a.assetId), this.data.flow, id())
        if (this.data.sequence !== sequence) { await cancelBatch(batch.batchId); return }
        this.setData({ batchId: batch.batchId })
      } else {
        batch = await queryBatch(previous)
        if (this.data.sequence !== sequence) return
        const ids = batch.items.filter(x => !this.downloaded.has(x.itemId) && (x.prepareStatus === 'EXPIRED' || (x.prepareStatus === 'FAILED' && x.error?.retryable))).map(x => x.itemId)
        if (ids.length) batch = await retryBatch(previous, ids, id())
      }
      await this.consumeBatch(batch, config.limits.poll_interval_ms, config.limits.client_download_timeout_seconds * 1000, sequence)
    } catch (error) {
      if (this.data.sequence === sequence) {
        const permanent = error instanceof APIError && ['FILE_NOT_READY', 'RETRY_LIMIT_REACHED', 'RESULT_EXPIRED', 'BATCH_EXPIRED', 'RESOURCE_NOT_FOUND'].includes(error.code)
        this.setData({ error: (error as Error).message, status: '演练未完成', retryAvailable: Boolean(this.data.batchId) && !permanent })
      }
    } finally {
      if (this.data.sequence === sequence) { this.downloadTask = null; this.setData({ busy: false }) }
    }
  },
  async consumeBatch(initial: Batch, pollMs: number, downloadMs: number, sequence: number) {
    let batch = initial
    let downloading = true
    while (downloading && this.data.sequence === sequence) {
      while (batch.status === 'PREPARING') {
        await wait(batch.pollAfterMs || pollMs)
        if (this.data.sequence !== sequence) return
        batch = await queryBatch(batch.batchId)
      }
      if (this.data.sequence !== sequence) return
      downloading = false
      let failed = false
      for (const item of batch.items) {
        if (this.data.sequence !== sequence) return
        if (item.prepareStatus !== 'READY' || this.downloaded.has(item.itemId)) continue
        this.setData({ status: '正在下载样本 ' + (this.downloaded.size + 1) + ' / ' + batch.items.length })
        try {
          const response = await tickets(batch.batchId, [item.itemId])
          if (this.data.sequence !== sequence) return
          const ticket = response.tickets.find(entry => entry.itemId === item.itemId)
          if (!ticket) throw new Error('下载票据不完整')
          await downloadSample(ticket, downloadMs, task => { this.downloadTask = task })
          if (this.data.sequence !== sequence) return
          this.downloaded.add(item.itemId)
        } catch (error) {
          if (this.data.sequence !== sequence) return
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
        this.setData({ error, status, copyItems, retryAvailable })
      }
    }
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
    this.setData({ sequence: this.data.sequence + 1, busy: false, batchId: '', retryAvailable: false, copyItems: [], status: wasBusy ? '已取消演练' : this.data.status })
    this.downloadTask?.abort(); this.downloadTask = null
    if (batch) { try { await cancelBatch(batch) } catch { /* The expired mock batch will be removed by the service. */ } }
  },
})
