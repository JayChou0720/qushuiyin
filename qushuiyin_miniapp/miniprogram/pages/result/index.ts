import { Asset, Result, cancelBatch, downloadSample, id, loadConfig, queryBatch, queryResult, startBatch, tickets, wait } from '../../services/api'

Page({
  data: { result: null as Result | null, flow: '', resultId: '', selected: {} as Record<string, boolean>, count: 0, max: 0, busy: false, error: '', status: '', batchId: '', sequence: 0 },
  downloadTask: null as WechatMiniprogram.DownloadTask | null,
  onLoad(query) { this.setData({ resultId: query.resultId || '', flow: query.flowId || id() }); void this.load() },
  onUnload() { void this.cancel() },
  async load() {
    try {
      const [result, config] = await Promise.all([queryResult(this.data.resultId), loadConfig()])
      const selected: Record<string, boolean> = {}
      const max = config.limits.max_image_batch_count
      for (const asset of result.assets.slice(0, max)) selected[asset.assetId] = true
      this.setData({ result, selected, max, count: Object.keys(selected).length, error: '' })
    } catch (error) { this.setData({ error: (error as Error).message }) }
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
  async runSave(assets: Asset[]) {
    if (this.data.busy) return
    const sequence = this.data.sequence + 1
    this.setData({ sequence, busy: true, error: '', status: '正在准备样本' })
    try {
      const config = await loadConfig()
      if (this.data.sequence !== sequence) return
      let b = await startBatch(this.data.resultId, assets.map(a => a.assetId), this.data.flow, id())
      if (this.data.sequence !== sequence) { await cancelBatch(b.batchId); return }
      this.setData({ batchId: b.batchId })
      const delay = b.pollAfterMs
      while (b.status === 'PREPARING') { await wait(delay); if (this.data.sequence !== sequence) return; b = await queryBatch(b.batchId) }
      if (b.status !== 'READY') throw new Error('样本未准备完成，请重试')
      if (this.data.sequence !== sequence) return
      const response = await tickets(b.batchId, b.items.map(x => x.itemId))
      if (response.tickets.length !== assets.length) throw new Error('下载票据不完整')
      for (let n = 0; n < response.tickets.length; n++) {
        if (this.data.sequence !== sequence) return
        this.setData({ status: '正在下载样本 ' + (n + 1) + ' / ' + response.tickets.length })
        await downloadSample(response.tickets[n].url, config.limits.client_download_timeout_seconds * 1000, task => { this.downloadTask = task })
      }
      if (this.data.sequence === sequence) this.setData({ status: '样本已下载，未写入相册' })
    } catch (error) {
      if (this.data.sequence === sequence) this.setData({ error: (error as Error).message, status: '演练未完成' })
    } finally {
      if (this.data.sequence === sequence) { this.downloadTask = null; this.setData({ busy: false }) }
    }
  },
  async cancel() {
    const batch = this.data.batchId
    const wasBusy = this.data.busy
    this.setData({ sequence: this.data.sequence + 1, busy: false, batchId: '', status: wasBusy ? '已取消演练' : this.data.status })
    this.downloadTask?.abort(); this.downloadTask = null
    if (batch) { try { await cancelBatch(batch) } catch { /* The expired mock batch will be removed by the service. */ } }
  },
})
