const assert = require('node:assert/strict')
const { resolve } = require('node:path')
const { setTimeout: wait } = require('node:timers/promises')

const [build, port = '18080'] = process.argv.slice(2)
assert.ok(build && Number(port) > 0 && Number(port) < 65536, 'Expected compiled mini-program directory and local API port')
const client = path => require(resolve(build, path))
const key = 'qsy.client-events.v1'
const storage = new Map()
const attempts = []
let connected = false, listener, lifecycle, page, createPage, loseReceipt = false
let holdReceipt, releaseReceipt
const downloaded = new Map(), albumWrites = []
let downloads = 0, failAlbumAt, holdDownload = false, downloading, holdAlbum = false, albumUnconfirmed, confirmRetry = false
let albumPermission = true, privacyAllowed = true, holdAfterAlbumAt, storageFailAt
const changeNetwork = isConnected => { connected = isConnected; listener({ isConnected }) }
const entries = () => storage.get(key)?.entries || []
global.App = app => { lifecycle = app }
global.Page = value => {
  createPage = () => ({ ...value, data: structuredClone(value.data), setData(updates, callback) { Object.assign(this.data, updates); callback?.() } })
  page = createPage()
}
const writeAlbum = options => {
  albumWrites.push(options.filePath)
  if (holdAlbum) { albumUnconfirmed = options; return }
  if (albumWrites.length === storageFailAt) { options.fail({ errMsg: 'ENOSPC' }); return }
  if (albumWrites.length === failAlbumAt) { options.fail({ errMsg: 'saveImageToPhotosAlbum:fail retryable sample' }); return }
  if (albumWrites.length === holdAfterAlbumAt) holdDownload = true
  options.success()
}
global.wx = {
  getStorageSync: key => structuredClone(storage.get(key)),
  setStorageSync: (key, value) => storage.set(key, structuredClone(value)),
  removeStorageSync: key => storage.delete(key),
  getAccountInfoSync: () => ({ miniProgram: { envVersion: 'develop' } }),
  getDeviceInfo: () => ({ platform: 'devtools', model: 'private-model' }),
  getNetworkType: options => options.success({ networkType: connected ? 'wifi' : 'none' }),
  onNetworkStatusChange: callback => { listener = callback },
  createIntersectionObserver: () => ({ relativeToViewport() { return this }, observe() {}, disconnect() {} }),
  getSetting: options => options.success({ authSetting: { 'scope.writePhotosAlbum': albumPermission } }),
  openSetting() { albumPermission = true },
  requirePrivacyAuthorize: options => privacyAllowed ? options.success() : options.fail(),
  saveImageToPhotosAlbum: writeAlbum,
  saveVideoToPhotosAlbum: writeAlbum,
  showModal: options => options.success({ confirm: confirmRetry }),
  getFileSystemManager: () => ({ statSync: path => ({ size: downloaded.get(path) || 0 }), unlink: options => { downloaded.delete(options.filePath); options.success() }, readdirSync: () => [] }),
  downloadFile(options) {
    const url = new URL(options.url)
    assert.equal(url.origin, 'http://127.0.0.1:18080')
    url.port = port
    const path = '/private-download-path-' + ++downloads
    const abort = new AbortController()
    if (holdDownload) downloading = abort
    else fetch(url, { signal: AbortSignal.any([AbortSignal.timeout(5000), abort.signal]) }).then(async response => {
      const body = await response.arrayBuffer()
      downloaded.set(path, body.byteLength)
      options.success({ statusCode: response.status, tempFilePath: path })
    }).catch(error => options.fail({ errMsg: error.message }))
    return { onProgressUpdate() {}, abort() { abort.abort(); if (downloading === abort) { downloading = undefined; options.fail({ errMsg: 'download aborted' }) } } }
  },
  request(options) {
    const url = new URL(options.url)
    assert.equal(url.origin, 'http://127.0.0.1:18080')
    url.port = port
    const report = url.pathname === '/v1/events'
    const attempt = report && { data: options.data, time: Date.now() }
    if (attempt) attempts.push(attempt)
    if (report && !connected) { options.fail(); return }
    fetch(url, { method: options.method, headers: { 'Content-Type': 'application/json', ...options.header },
      body: options.data && JSON.stringify(options.data), signal: AbortSignal.timeout(5000) })
      .then(response => response.json()).then(data => {
        if (attempt) attempt.receipt = data.data
        if (report && loseReceipt) { loseReceipt = false; options.fail(); return }
        for (const media of data.data?.previews || data.data?.tickets || []) {
          if (media.url) media.url = media.url.replace(url.origin + '/', 'http://127.0.0.1:18080/')
        }
        if (report && holdReceipt) {
          holdReceipt = false
          releaseReceipt = () => options.success({ data })
        } else options.success({ data })
      }, options.fail)
  },
}

async function parseResult(api, owner, source, flowId) {
  const { data } = await api.startParse({ shareText: source, selectedUrl: source, source: 'manual', flowId }, api.id(), owner)
  for (;;) {
    const job = await api.queryJob(data.jobId, owner)
    if (job.status === 'SUCCEEDED') return job
    assert.ok(['QUEUED', 'RUNNING'].includes(job.status) && Date.now() < Date.parse(data.deadlineAt), job.status)
    await wait(data.pollAfterMs)
  }
}
const until = async (condition, timeout) => {
  const end = Date.now() + timeout
  while (!condition()) {
    if (Date.now() >= end) throw new Error('Timed out waiting for event queue')
    await wait(10)
  }
}

async function run() {
  client('app.js')
  lifecycle.onLaunch()
  lifecycle.onShow()
  const api = client('services/api.js'), queue = client('services/events.js')
  const owner = await api.confirmUserIdentity()
  const { limits } = await api.loadConfig()
  const started = { flowId: api.id(), clientActionId: api.id() }
  queue.enqueueClientEvent(owner, 'result_viewed', 'succeeded', { flowId: 'https://private.invalid/', shareText: 'private caption' })
  queue.enqueueClientEvent(owner, 'save_action_started', 'started', started)
  queue.enqueueClientEvent(owner, 'client_download_finished', 'succeeded', { ...started, jobId: api.id(), deliveryMode: 'relay', bytes: 4 })
  const original = entries().map(entry => entry.event.eventId)
  assert.equal(original.length, 3)
  assert.ok(!/private|shareText|owner/.test(JSON.stringify(entries().map(entry => entry.event))))
  assert.equal(entries()[0].event.flowId, null)
  assert.equal(entries()[0].event.osFamily, 'devtools')
  const [entry] = entries()
  entries().unshift({ ...entry, owner: 'different-user', event: { ...entry.event, eventId: 'different-id' } },
    { ...entry, event: { ...entry.event, eventId: 'expired-id', clientTime: new Date(Date.now() - (limits.client_event_retention_hours + 1) * 3600000).toISOString() } })
  await queue.flushClientEvents()
  assert.equal(attempts.length, 0, 'does not submit while offline')
  lifecycle.onShow()
  assert.ok(!entries().some(({ event }) => event.eventId === 'expired-id'), 'prunes expired local events while offline')
  lifecycle.onHide()
  changeNetwork(true)
  await queue.flushClientEvents()
  assert.equal(attempts.length, 0, 'does not submit while in the background')

  loseReceipt = true
  lifecycle.onShow()
  const initialDelay = limits.client_event_retry_initial_seconds * 1000
  await until(() => attempts[0]?.receipt, 2000)
  assert.deepEqual(attempts[0].receipt.acceptedEventIds, original.slice(0, 2))
  assert.deepEqual(attempts[0].receipt.rejected, [{ eventId: original[2], code: 'RESOURCE_NOT_FOUND' }])
  assert.equal(entries().length, 4, 'a lost response preserves unsent and in-flight events')
  queue.enqueueClientEvent(owner, 'preview_finished', 'cancelled', started)
  await until(() => attempts[1]?.receipt, initialDelay + 2000)
  assert.ok(attempts[1].time - attempts[0].time >= initialDelay)
  assert.deepEqual(attempts[1].receipt.duplicateEventIds, original.slice(0, 2))
  await until(() => entries().length === 1, 2000)
  assert.equal(entries()[0].owner, 'different-user')

  changeNetwork(false)
  for (let i = 0; i <= limits.client_queue_max; i++) queue.enqueueClientEvent(owner, 'result_viewed', 'succeeded')
  assert.equal(entries().length, limits.client_queue_max)
  const pending = entries().map(entry => entry.event.eventId)
  changeNetwork(true)
  await until(() => !entries().length, 3000)
  const uploaded = attempts.slice(2)
  assert.ok(uploaded.every(({ data }) => data.events.length <= limits.event_batch_max))
  assert.deepEqual(uploaded.flatMap(({ receipt }) => receipt.acceptedEventIds), pending)

  lifecycle.onHide()
  const source = 'https://www.douyin.com/video/10001'
  const flowId = api.id()
  const job = await parseResult(api, owner, source, flowId)
  client('services/history.js').stageHistory({ resultId: job.resultId, userId: owner, jobId: job.jobId })
  client('pages/result/index.js')
  page.onLoad({ resultId: job.resultId, flowId })
  await until(() => page.data.result, 2000)
  page.play()
  await until(() => page.data.previewURL, 2000)
  assert.deepEqual(entries().map(entry => entry.event.name), ['result_viewed', 'preview_started'])
  page.previewShown({ currentTarget: { dataset: { generation: page.data.previewGeneration } } })
  page.closePreview()
  page.save()
  await until(() => !page.data.busy && !page.data.hasSave, 8000)
  const pageEvents = entries().map(entry => entry.event)
  assert.deepEqual(pageEvents.map(event => [event.name, event.outcome]), [
    ['result_viewed', 'succeeded'], ['preview_started', 'started'], ['preview_finished', 'succeeded'],
    ['save_action_started', 'started'], ['client_download_finished', 'succeeded'], ['album_save_finished', 'succeeded'], ['save_action_finished', 'succeeded'],
  ])
  assert.ok(pageEvents[4].bytes > 0)
  assert.equal(pageEvents[4].batchId, pageEvents[6].batchId)
  assert.equal(pageEvents[4].clientActionId, pageEvents[3].clientActionId)
  assert.ok(!JSON.stringify(pageEvents).includes('private-download-path'))
  lifecycle.onShow()
  await until(() => !entries().length, 2000)
  assert.deepEqual(attempts.at(-1).receipt.acceptedEventIds, pageEvents.map(event => event.eventId))
  holdReceipt = true
  queue.enqueueClientEvent(owner, 'result_viewed', 'succeeded')
  await until(() => releaseReceipt, 2000)
  queue.enqueueClientEvent(owner, 'preview_started', 'started')
  const lastId = entries().at(-1).event.eventId
  releaseReceipt()
  await until(() => !entries().length, 2000)
  assert.deepEqual(attempts.at(-1).receipt.acceptedEventIds, [lastId])
  changeNetwork(false)
  queue.enqueueClientEvent(owner, 'result_viewed', 'succeeded')
  entries().push({ ...entries()[0], owner: 'different-user' })
  await client('services/privacy.js').clearServiceData()
  assert.deepEqual(entries().map(entry => entry.owner), ['different-user'])
  assert.notEqual(await api.confirmUserIdentity(), owner)
  console.log('Client event smoke passed: offline/background, expiry, backoff, stable IDs, batch limit, and privacy cleanup.')

  lifecycle.onHide()
  changeNetwork(true)
  const galleryOwner = await api.confirmUserIdentity()
  const galleryFlow = api.id()
  const gallery = await parseResult(api, galleryOwner, 'https://www.xiaohongshu.com/explore/10004', galleryFlow)
  page.onUnload()
  page = createPage()
  page.onLoad({ resultId: gallery.resultId, flowId: galleryFlow })
  await until(() => page.data.result, 2000)
  assert.equal(page.data.count, 3)
  page.selectBatch()
  assert.equal(page.data.count, 0)
  page.selectBatch()
  assert.equal(page.data.count, 3)
  const actualResult = page.data.result, actualSelected = page.data.selected
  const synthetic = Array.from({ length: limits.max_image_batch_count + 1 }, (_, index) => ({ assetId: 'limit-' + index, order: index + 1 }))
  page.setData({ result: { ...actualResult, assets: synthetic, assetCount: synthetic.length }, count: 0, selected: {} })
  page.selectBatch()
  assert.equal(page.data.count, limits.max_image_batch_count)
  page.toggle({ currentTarget: { dataset: { asset: synthetic.at(-1).assetId } } })
  assert.equal(page.data.count, limits.max_image_batch_count)
  page.toggle({ currentTarget: { dataset: { asset: synthetic[0].assetId } } })
  page.toggle({ currentTarget: { dataset: { asset: synthetic.at(-1).assetId } } })
  assert.ok(page.data.selected[synthetic.at(-1).assetId], 'images beyond the first batch remain selectable')
  page.setData({ result: actualResult, count: 3, selected: actualSelected })

  albumPermission = false
  page.save()
  await until(() => !page.data.busy, 3000)
  assert.ok(page.data.settingsAvailable)
  assert.equal(page.operation.batchId, '')
  page.albumSettings()
  privacyAllowed = false
  await page.retrySave()
  await until(() => !page.data.busy, 3000)
  assert.match(page.data.error, /隐私/)
  assert.equal(page.operation.batchId, '')
  privacyAllowed = true
  failAlbumAt = albumWrites.length + 3
  await page.retrySave()
  await until(() => !page.data.busy, 8000)
  assert.match(page.data.status, /已保存 2\/3.*失败 1 项/)
  const failedPath = albumWrites.at(-1)
  const beforeRetry = downloads
  await page.retrySave()
  await until(() => !page.data.busy && !page.data.hasSave, 5000)
  assert.match(page.data.status, /已保存 3\/3/)
  assert.equal(downloads, beforeRetry, 'reuses the completed download after an album error')
  assert.equal(albumWrites.at(-1), failedPath, 'retries only the failed album write')

  holdDownload = true
  page.save()
  await until(() => downloading, 8000)
  const originalBatch = page.operation.batchId
  const beforePause = downloads
  page.onHide()
  assert.equal(downloading, undefined, 'hiding aborts the current download')
  holdDownload = false
  page.onShow()
  await until(() => page.data.paused && page.data.retryAvailable, 2000)
  assert.equal(page.operation.batchId, originalBatch)
  assert.equal(downloads, beforePause, 'showing the page never resumes writes automatically')
  await page.retrySave()
  await until(() => !page.data.busy && !page.data.hasSave, 5000)

  holdAfterAlbumAt = albumWrites.length + 1
  page.save()
  await until(() => downloading, 8000)
  await page.cancel()
  holdDownload = false
  assert.equal(albumWrites.length, holdAfterAlbumAt)
  assert.match(page.data.status, /已保存 1 项，剩余 2 项已取消/)

  changeNetwork(false)
  page.save()
  await until(() => !page.data.busy, 2000)
  assert.match(page.data.error, /网络已断开/)
  assert.equal(page.operation.batchId, '')
  changeNetwork(true)
  storageFailAt = albumWrites.length + 1
  await page.retrySave()
  await until(() => !page.data.busy, 8000)
  assert.equal(albumWrites.length, storageFailAt, 'device storage failure pauses the remaining items')
  assert.match(page.data.error, /存储空间不足/)
  await page.retrySave()
  await until(() => !page.data.busy && !page.data.hasSave, 5000)

  holdAlbum = true
  page.save()
  await until(() => albumUnconfirmed, 8000)
  const writesBeforeRestart = albumWrites.length
  const interruptedBatch = page.operation.batchId
  page.onUnload()
  holdAlbum = false
  const newResultPage = createPage
  client('pages/home/index.js')
  page.onShow()
  await until(() => page.data.resumeResultId === gallery.resultId, 2000)
  assert.equal(albumWrites.length, writesBeforeRestart, 'the home recovery entry does not start saving')
  page.onHide()
  page = newResultPage()
  page.onLoad({ resultId: gallery.resultId, flowId: galleryFlow })
  await until(() => page.data.paused && page.data.retryAvailable, 2000)
  assert.equal(page.operation.batchId, interruptedBatch)
  assert.ok(Object.values(page.operation.states).includes('UNKNOWN'))
  await page.retrySave()
  assert.equal(albumWrites.length, writesBeforeRestart, 'unknown album writes require confirmation')
  confirmRetry = true
  await page.retrySave()
  await until(() => !page.data.busy && !page.data.hasSave, 5000)
  assert.match(page.data.status, /已保存 3\/3/)
  console.log('Gallery save smoke passed: selection, consent, partial failure, file reuse, cancellation, offline/storage pause, and restart recovery.')
}

run().catch(error => { console.error(error); process.exitCode = 1 }).finally(() => lifecycle?.onHide())
