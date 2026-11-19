import { APIError, Asset, Batch, BatchItem, ConnectionError, PublicConfig, Result, cancelBatch, copyDownloadURL, fallback, id, isCurrentUser, isKnownUser, loadConfig, previewAsset, queryBatch, queryOwnedResult, retryBatch, startBatch, tickets, wait } from '../../services/api'
import { cacheThumbnail, noteHistorySave, recordHistory, takeHistory } from '../../services/history'
import { enqueueClientEvent } from '../../services/events'
import { LocalMedia, SaveMediaError, downloadForAlbum, ensureAlbumAccess, ensureSaveNetwork, fileSize, openAlbumSettings, respondToSavePrivacy, saveToAlbum } from '../../services/save-media'
import { SaveProgress, clearSaveProgress, loadSaveProgress, writeSaveProgress } from '../../services/save-progress'
import { releaseTemporaryMedia } from '../../services/media-cache'
import { openPrivacyContract } from '../../services/privacy'

type SaveOutcome = 'succeeded' | 'partial' | 'failed' | 'cancelled' | 'unknown'
interface SaveEvent { operation: SaveProgress; started: number }
const expiredBatch = (error: unknown) => error instanceof APIError && ['BATCH_EXPIRED', 'RESULT_EXPIRED', 'RESOURCE_NOT_FOUND', 'IDENTITY_CHANGED'].includes(error.code)
const retryablePreparation = (item: BatchItem) => item.prepareStatus === 'EXPIRED' || item.prepareStatus === 'CANCELLED' || item.prepareStatus === 'FAILED' && item.error?.retryable

Page({
  data: { result: null as Result | null, flow: '', resultId: '', selected: {} as Record<string, boolean>, count: 0, max: 0, busy: false, error: '', status: '', sequence: 0,
    retryAvailable: false, paused: false, hasSave: false, albumPending: false, privacyVisible: false, settingsAvailable: false,
    staticOnly: false, truncated: false, copyText: '', saveItems: [] as { itemId: string; label: string }[], copyItems: [] as { itemId: string; order: number }[],
    thumbnails: {} as Record<string, string>, previewKind: '', previewURL: '', previewBusy: false, previewError: '', previewOrder: 0, previewGeneration: 0 },
  downloadTask: null as WechatMiniprogram.DownloadTask | null,
  localFiles: null as unknown as Map<string, LocalMedia>,
  operation: null as SaveProgress | null,
  albumTask: null as Promise<void> | null,
  retryBlocked: null as unknown as Set<string>,
  cancelling: false,
  foreground: false,
  checkingRecovery: 0,
  observers: [] as WechatMiniprogram.IntersectionObserver[],
  previewSequence: 0,
  previewExpires: 0,
  previewRefreshed: false,
  previewEvent: null as { started: number; rendered: boolean } | null,
  disposed: false,
  historyOwner: '',
  jobId: undefined as string | undefined,
  activeSave: null as SaveEvent | null,
  onLoad(query) { this.localFiles = new Map(); this.retryBlocked = new Set(); this.foreground = true; this.setData({ resultId: query.resultId || '', flow: query.flowId || id() }); void this.load() },
  onShow() { this.foreground = true; if (this.operation && this.data.paused) void this.inspectRecovery() },
  onHide() { this.foreground = false; this.closePreview(); this.pauseSave() },
  onUnload() {
    this.disposed = true
    this.foreground = false
    this.finishPreview(this.previewEvent?.rendered ? 'succeeded' : 'cancelled')
    this.previewSequence++
    for (const observer of this.observers) observer.disconnect()
    this.pauseSave()
    if (this.albumTask) void this.albumTask.then(() => this.releaseLocalFiles())
    else this.releaseLocalFiles()
  },
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
      const ids = new Set(result.assets.map(asset => asset.assetId))
      if (result.cover) ids.add(result.cover.assetId)
      const restored = loadSaveProgress(userId, result.resultId, ids)
      if (restored) this.operation = restored
      this.setData({ result, selected, max, count: Object.keys(selected).length, error: '', copyText: result.description || result.title || '',
        staticOnly: result.warnings?.includes('LIVE_PHOTO_AS_STATIC') ?? false, truncated: result.warnings?.includes('TEXT_TRUNCATED') ?? false,
        hasSave: !!restored, paused: !!restored, retryAvailable: !!restored }, () => {
        if (this.disposed) return
        enqueueClientEvent(userId, 'result_viewed', 'succeeded', { flowId: this.data.flow, jobId: this.jobId })
        this.observeThumbnails(result)
        if (restored && this.foreground) void this.inspectRecovery()
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
    if (this.operation) return
    const asset = String(e.currentTarget.dataset.asset)
    const selected = { ...this.data.selected }
    if (selected[asset]) delete selected[asset]
    else if (this.data.count < this.data.max) selected[asset] = true
    else { this.setData({ error: '一次最多选择 ' + this.data.max + ' 张图片' }); return }
    this.setData({ selected, count: Object.keys(selected).length, error: '' })
  },
  selectBatch() {
    if (this.operation || !this.data.result) return
    const first = this.data.result.assets.slice(0, this.data.max)
    const selected: Record<string, boolean> = {}
    if (this.data.count !== first.length || this.data.result.assets.length > this.data.max) {
      for (const asset of first) selected[asset.assetId] = true
    }
    this.setData({ selected, count: Object.keys(selected).length, error: '' })
  },
  copy() { if (this.data.copyText) wx.setClipboardData({ data: this.data.copyText }) },
  save() {
    const assets = this.data.result?.assets.filter(asset => this.data.selected[asset.assetId])
    if (assets?.length) this.beginSave(assets)
  },
  saveCover() { const cover = this.data.result?.cover; if (cover) this.beginSave([cover]) },
  beginSave(assets: Asset[]) {
    const result = this.data.result
    if (!result || this.operation || this.data.busy || !this.foreground) return
    const operation: SaveProgress = { userId: this.historyOwner, resultId: result.resultId, flowId: this.data.flow,
      actionId: id(), assetIds: assets.map(asset => asset.assetId), batchId: '', expiresAt: result.expiresAt, states: {} }
    this.operation = operation
    this.setData({ hasSave: true, saveItems: [], copyItems: [] })
    void this.runSave(operation, false)
  },
  persistSave(operation: SaveProgress) {
    if (this.operation !== operation || !isKnownUser(operation.userId)) throw new Error('使用身份已变更，请重新打开结果')
    try { writeSaveProgress(operation) }
    catch { throw new SaveMediaError('CLIENT_STORAGE_LOW', '无法记录保存进度，请清理本机空间后继续') }
  },
  async inspectRecovery() {
    const operation = this.operation
    if (!operation || !this.foreground || this.data.busy || this.albumTask) return
    const check = ++this.checkingRecovery
    const current = () => this.foreground && !this.disposed && this.operation === operation && this.checkingRecovery === check && !this.data.busy
    this.setData({ paused: true, retryAvailable: true, status: '已暂停，点击继续保存' })
    if (!operation.batchId) return
    try {
      await ensureSaveNetwork()
      if (!current()) return
      const batch = await queryBatch(operation.batchId, operation.userId)
      if (!current()) return
      if (batch.status === 'CANCELLED') { this.finishSave('cancelled'); this.endOperation('本次保存已取消'); return }
      if (this.summarize(batch, operation) === 'succeeded') {
        this.finishSave('succeeded')
        this.endOperation()
      }
    } catch (err) {
      if (!current()) return
      if (expiredBatch(err)) { this.finishSave('failed'); this.endOperation() }
      this.setData({ error: (err as Error).message })
    }
  },
  async retrySave() {
    const operation = this.operation
    if (!operation || !this.data.retryAvailable || this.data.busy || this.albumTask || !this.foreground) return
    if (Object.values(operation.states).includes('UNKNOWN')) {
      const confirmed = await new Promise<boolean>(resolve => wx.showModal({
        title: '保存结果未确认', content: '有文件可能已保存到相册，继续重试可能产生重复文件。', confirmText: '继续重试',
        success: result => resolve(result.confirm), fail: () => resolve(false),
      }))
      if (!confirmed || !this.foreground || this.operation !== operation) return
    }
    void this.runSave(operation, true)
  },
  async runSave(operation: SaveProgress, retry: boolean) {
    if (this.data.busy || this.albumTask || !this.foreground || operation !== this.operation) return
    const sequence = this.data.sequence + 1
    const current = () => this.foreground && !this.disposed && this.data.sequence === sequence && this.operation === operation && isKnownUser(operation.userId)
    this.checkingRecovery++
    this.setData({ sequence, busy: true, paused: false, error: '', status: '正在检查保存权限', settingsAvailable: false, retryAvailable: false })
    wx.enableAlertBeforeUnload?.({ message: '返回将暂停本次保存，已保存的文件会保留。' })
    let outcome: SaveOutcome = 'unknown'
    let batch: Batch | undefined
    try {
      await ensureSaveNetwork()
      if (!current()) return
      await ensureAlbumAccess(() => { if (current()) this.setData({ privacyVisible: true }); else respondToSavePrivacy(false) }, current)
      if (!current()) return
      this.setData({ privacyVisible: false, status: '正在准备文件' })
      const config = await loadConfig()
      if (!current()) return
      this.persistSave(operation)
      if (!operation.batchId) {
        const receipt = await startBatch(operation.resultId, operation.assetIds, operation.flowId, operation.actionId, operation.userId)
        if (operation !== this.operation) { void cancelBatch(receipt.batchId, operation.userId).catch(() => {}); return }
        operation.batchId = receipt.batchId
        operation.expiresAt = receipt.expiresAt
        this.persistSave(operation)
        if (!current()) return
      }
      if (!this.activeSave) {
        this.activeSave = { operation, started: Date.now() }
        enqueueClientEvent(operation.userId, 'save_action_started', 'started', this.eventDetails(operation))
      }
      batch = await this.waitForBatch(operation, config.limits.poll_interval_ms, current)
      if (!batch) return
      if (batch.status === 'CANCELLED') { outcome = 'cancelled'; this.endOperation('本次保存已取消'); return }
      if (retry) {
        const pending = batch.items.filter(item => operation.states[item.itemId] !== 'SUCCEEDED' && !this.localFiles.has(item.itemId) && !this.retryBlocked.has(item.itemId) && retryablePreparation(item))
        if (pending.length) {
          try { await retryBatch(batch.batchId, pending.map(item => item.itemId), id(), operation.userId) }
          catch (error) {
            if (!(error instanceof APIError) || error.code !== 'RETRY_LIMIT_REACHED') throw error
            // An exhausted item must not prevent the rest of the batch from retrying.
            for (const item of pending) {
              if (!current()) return
              try { await retryBatch(batch.batchId, [item.itemId], id(), operation.userId) }
              catch (individual) {
                if (!(individual instanceof APIError) || individual.code !== 'RETRY_LIMIT_REACHED') throw individual
                this.retryBlocked.add(item.itemId)
              }
            }
          }
          batch = await this.waitForBatch(operation, config.limits.poll_interval_ms, current)
          if (!batch) return
        }
      }
      for (const item of batch.items) {
        if (!current()) return
        if (operation.states[item.itemId] !== 'SUCCEEDED') await this.saveBatchItem(item, operation, config, current)
      }
      if (!current()) return
      const final = await queryBatch(operation.batchId, operation.userId)
      if (!current()) return
      outcome = this.summarize(final, operation)
      if (outcome === 'succeeded' || !this.data.retryAvailable && this.data.copyItems.length === 0) this.endOperation()
    } catch (error) {
      if (!current()) return
      outcome = Object.values(operation.states).includes('SUCCEEDED') ? 'partial' : 'failed'
      if (batch) this.summarize(batch, operation)
      const permission = error instanceof SaveMediaError && ['ALBUM_PERMISSION_DENIED', 'SYSTEM_PERMISSION_DENIED'].includes(error.code)
      if (expiredBatch(error)) this.endOperation()
      this.setData({ error: (error as Error).message, privacyVisible: false, settingsAvailable: permission, retryAvailable: !!this.operation })
    } finally {
      if (this.activeSave?.operation === operation && this.data.sequence === sequence) this.finishSave(outcome)
      if (this.data.sequence === sequence) {
        this.downloadTask = null
        this.setData({ busy: false })
        wx.disableAlertBeforeUnload?.({})
      }
    }
  },
  eventDetails(operation: SaveProgress) {
    return { flowId: operation.flowId, jobId: this.jobId, clientActionId: operation.actionId, batchId: operation.batchId }
  },
  finishSave(outcome: SaveOutcome) {
    const event = this.activeSave
    if (!event) return
    this.activeSave = null
    enqueueClientEvent(event.operation.userId, 'save_action_finished', outcome, { ...this.eventDetails(event.operation),
      durationMs: Math.max(0, Date.now() - event.started),
      errorCode: outcome === 'cancelled' ? 'USER_CANCELLED' : outcome === 'unknown' ? 'SAVE_OUTCOME_UNKNOWN' : undefined })
  },
  async waitForBatch(operation: SaveProgress, pollMs: number, current: () => boolean): Promise<Batch | undefined> {
    while (current()) {
      const batch = await queryBatch(operation.batchId, operation.userId)
      if (!current()) return undefined
      if (batch.status !== 'PREPARING') return batch
      await wait(pollMs)
    }
    return undefined
  },
  async saveBatchItem(original: BatchItem, operation: SaveProgress, config: PublicConfig, current: () => boolean) {
    const result = this.data.result
    const kind = result?.assets.find(asset => asset.assetId === original.assetId)?.type || (result?.cover?.assetId === original.assetId ? result.cover.type : undefined)
    if (!kind) return
    let item = original
    let fellBack = false
    while (current()) {
      let file = this.localFiles.get(item.itemId)
      if (file && fileSize(file.path) !== file.bytes) { this.dropLocalFile(item.itemId); file = undefined }
      if (!file && item.prepareStatus !== 'READY') return
      try {
        if (!file) {
          this.setData({ status: '正在下载第 ' + item.order + ' 项' })
          file = await this.downloadItem(item, kind, operation, config, current)
          if (!file) return
          if (!current()) { void releaseTemporaryMedia(file.path); return }
          this.localFiles.set(item.itemId, file)
        }
        if (!current()) return
        operation.states[item.itemId] = 'SAVING'
        try { this.persistSave(operation) }
        catch (error) { operation.states[item.itemId] = 'FAILED'; throw error }
        this.setData({ status: '正在写入相册 · 第 ' + item.order + ' 项', albumPending: true })
        await this.writeAlbum(operation, item, file, kind)
        if (current()) {
          const saved = Object.values(operation.states).filter(state => state === 'SUCCEEDED').length
          this.setData({ status: '已保存 ' + saved + '/' + operation.assetIds.length + ' 项' })
        }
        return
      } catch (error) {
        if (!current()) return
        if (expiredBatch(error) || error instanceof ConnectionError || error instanceof SaveMediaError && error.stopsBatch) throw error
        if (!fellBack && error instanceof SaveMediaError && (error.code === 'DIRECT_DOWNLOAD_REJECTED' || error.code === 'DIRECT_CONTENT_INVALID')) {
          fellBack = true
          this.dropLocalFile(item.itemId)
          this.setData({ status: '正在切换下载方式' })
          try { await fallback(operation.batchId, item.itemId, error.code, operation.userId) }
          catch (fallbackError) {
            if (!(fallbackError instanceof APIError) || fallbackError.code !== 'FALLBACK_NOT_ALLOWED') throw fallbackError
          }
          const refreshed = await this.waitForBatch(operation, config.limits.poll_interval_ms, current)
          const updated = refreshed?.items.find(next => next.itemId === item.itemId)
          if (!updated) return
          item = updated
          continue
        }
        if (error instanceof SaveMediaError && error.code === 'CLIENT_DOWNLOAD_FAILED') await ensureSaveNetwork()
        if (!current()) return
        operation.states[item.itemId] = 'FAILED'
        this.persistSave(operation)
        return
      }
    }
  },
  async downloadItem(item: BatchItem, kind: string, operation: SaveProgress, config: PublicConfig, current: () => boolean) {
    const response = await tickets(operation.batchId, [item.itemId], operation.userId)
    if (!current()) return undefined
    const ticket = response.tickets.find(entry => entry.itemId === item.itemId)
    if (!ticket) throw new Error('下载票据不完整')
    const maximum = kind === 'video' ? config.limits.max_video_bytes : config.limits.max_image_bytes
    const started = Date.now()
    const details = { ...this.eventDetails(operation), itemId: item.itemId, deliveryMode: ticket.deliveryMode }
    let task: WechatMiniprogram.DownloadTask | null = null
    try {
      const file = await downloadForAlbum(ticket, config.limits.client_download_timeout_seconds * 1000, maximum, item.sizeBytes, download => {
        task = download
        if (current()) this.downloadTask = download
        else download.abort()
      })
      if (current()) enqueueClientEvent(operation.userId, 'client_download_finished', 'succeeded',
        { ...details, bytes: file.bytes, durationMs: Date.now() - started })
      return file
    } catch (error) {
      const active = current()
      enqueueClientEvent(operation.userId, 'client_download_finished', active ? 'failed' : 'cancelled',
        { ...details, durationMs: Date.now() - started, errorCode: active ? error instanceof SaveMediaError ? error.code : 'CLIENT_DOWNLOAD_FAILED' : 'USER_CANCELLED' })
      throw error
    } finally { if (this.downloadTask === task) this.downloadTask = null }
  },
  async writeAlbum(operation: SaveProgress, item: BatchItem, file: LocalMedia, kind: string) {
    const started = Date.now()
    const details = { ...this.eventDetails(operation), itemId: item.itemId, deliveryMode: file.mode, bytes: file.bytes }
    const result = saveToAlbum(file, kind).then(() => {
      operation.states[item.itemId] = 'SUCCEEDED'
      this.dropLocalFile(item.itemId)
      enqueueClientEvent(operation.userId, 'album_save_finished', 'succeeded', { ...details, durationMs: Date.now() - started })
    }, error => {
      operation.states[item.itemId] = 'FAILED'
      enqueueClientEvent(operation.userId, 'album_save_finished', 'failed', { ...details, durationMs: Date.now() - started,
        errorCode: error instanceof SaveMediaError ? error.code : 'ALBUM_SAVE_FAILED' })
      throw error
    }).finally(() => {
      // Native album callbacks can complete after the page is hidden.
      try { this.persistSave(operation) } catch { /* The SAVING checkpoint will recover as UNKNOWN. */ }
      this.albumTask = null
      if (!this.disposed) {
        this.setData({ albumPending: false })
        if (this.data.paused && this.foreground && !this.cancelling) void this.inspectRecovery()
      }
    })
    this.albumTask = result.catch(() => {})
    return result
  },
  summarize(batch: Batch, operation: SaveProgress): SaveOutcome {
    let saved = 0, failed = 0, unknown = 0
    const saveItems = batch.items.map(item => {
      const state = operation.states[item.itemId]
      let label = '未完成'
      if (state === 'SUCCEEDED') { saved++; label = '已保存' }
      else if (state === 'UNKNOWN' || state === 'SAVING') { unknown++; label = '结果未确认' }
      else if (state === 'FAILED' || item.prepareStatus === 'FAILED') { failed++; label = this.retryBlocked.has(item.itemId) ? '重试次数已用完' : item.error?.message || '保存失败' }
      else if (item.prepareStatus === 'CANCELLED') label = '已取消'
      return { itemId: item.itemId, label: '第 ' + item.order + ' 项 · ' + label }
    })
    const remaining = batch.items.filter(item => operation.states[item.itemId] !== 'SUCCEEDED')
    const copyItems = remaining.filter(item => item.copyLinkAvailable).map(item => ({ itemId: item.itemId, order: item.order }))
    const retryAvailable = remaining.some(item => item.prepareStatus === 'READY' || item.prepareStatus === 'PREPARING' || this.localFiles.has(item.itemId) || !this.retryBlocked.has(item.itemId) && retryablePreparation(item))
    const status = '已保存 ' + saved + '/' + batch.items.length + ' 项 · 失败 ' + failed + ' 项 · 未完成 ' + (batch.items.length - saved - failed) + ' 项'
    noteHistorySave(operation.userId, operation.resultId, status)
    this.setData({ status, saveItems, retryAvailable, copyItems })
    return saved === batch.items.length ? 'succeeded' : unknown ? 'unknown' : saved ? 'partial' : 'failed'
  },
  privacyAgree() { this.setData({ privacyVisible: false }); respondToSavePrivacy(true) },
  privacyDecline() { this.setData({ privacyVisible: false }); respondToSavePrivacy(false) },
  showPrivacyContract() { void openPrivacyContract().catch(error => this.setData({ error: error.message })) },
  albumSettings() { openAlbumSettings(() => this.setData({ error: '无法打开设置，请在微信或系统设置中开启相册权限' })) },
  async copyLink(e: WechatMiniprogram.CustomEvent) {
    const operation = this.operation
    if (!operation?.batchId) return
    try {
      const url = await copyDownloadURL(operation.batchId, String(e.currentTarget.dataset.item), operation.userId)
      if (!this.foreground || this.operation !== operation || !isKnownUser(operation.userId)) return
      wx.setClipboardData({ data: url, success: () => {
        if (!this.disposed && this.operation === operation) this.setData({ status: '已复制，链接很快会失效' })
      }, fail: () => {
        if (!this.disposed && this.operation === operation) this.setData({ error: '无法写入剪贴板' })
      } })
    } catch (error) { if (!this.disposed && this.operation === operation) this.setData({ error: (error as Error).message }) }
  },
  pauseSave() {
    this.checkingRecovery++
    respondToSavePrivacy(false)
    const download = this.downloadTask
    this.downloadTask = null
    if (!this.disposed) this.setData({ sequence: this.data.sequence + 1, busy: this.cancelling, privacyVisible: false,
      paused: !!this.operation, retryAvailable: !!this.operation, status: this.operation ? '已暂停，点击继续保存' : this.data.status })
    download?.abort()
    wx.disableAlertBeforeUnload?.({})
  },
  dropLocalFile(itemId: string) {
    const file = this.localFiles.get(itemId)
    if (file) { this.localFiles.delete(itemId); void releaseTemporaryMedia(file.path) }
  },
  releaseLocalFiles() { for (const itemId of this.localFiles.keys()) this.dropLocalFile(itemId) },
  endOperation(status?: string) {
    const operation = this.operation
    this.operation = null
    this.releaseLocalFiles()
    this.retryBlocked.clear()
    if (operation && isKnownUser(operation.userId)) {
      try { clearSaveProgress(operation.userId, operation.resultId) } catch { /* Recovery skips any recorded successful items. */ }
    }
    this.setData({ hasSave: false, paused: false, retryAvailable: false, copyItems: [], settingsAvailable: false,
      status: status || this.data.status })
  },
  async cancel() {
    const operation = this.operation
    if (!operation || this.cancelling) return
    this.cancelling = true
    this.finishSave('cancelled')
    this.pauseSave()
    this.setData({ busy: true, status: '正在取消剩余项目' })
    await this.albumTask
    if (operation.batchId) {
      try { await cancelBatch(operation.batchId, operation.userId) } catch { /* Local saving has stopped. */ }
    }
    this.cancelling = false
    if (!this.disposed && this.operation === operation) {
      const saved = Object.values(operation.states).filter(state => state === 'SUCCEEDED').length
      this.endOperation('已保存 ' + saved + ' 项，剩余 ' + (operation.assetIds.length - saved) + ' 项已取消')
      this.setData({ busy: false, error: '', saveItems: [] })
    }
  },
})
