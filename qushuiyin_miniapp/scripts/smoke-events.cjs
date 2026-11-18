const assert = require('node:assert/strict')
const { resolve } = require('node:path')
const { setTimeout: wait } = require('node:timers/promises')

const [build, port = '18080'] = process.argv.slice(2)
assert.ok(build && Number(port) > 0 && Number(port) < 65536, 'Expected compiled mini-program directory and local API port')
const client = path => require(resolve(build, path))
const key = 'qsy.client-events.v1'
const storage = new Map()
const attempts = []
let connected = false, listener, lifecycle, page, loseReceipt = false
let holdReceipt, releaseReceipt
const changeNetwork = isConnected => { connected = isConnected; listener({ isConnected }) }
const entries = () => storage.get(key)?.entries || []
global.App = app => { lifecycle = app }
global.Page = value => { page = { ...value, setData(updates, callback) { Object.assign(this.data, updates); callback?.() } } }
global.wx = {
  getStorageSync: key => structuredClone(storage.get(key)),
  setStorageSync: (key, value) => storage.set(key, structuredClone(value)),
  removeStorageSync: key => storage.delete(key),
  getAccountInfoSync: () => ({ miniProgram: { envVersion: 'develop' } }),
  getDeviceInfo: () => ({ platform: 'devtools', model: 'private-model' }),
  getNetworkType: options => options.success({ networkType: connected ? 'wifi' : 'none' }),
  onNetworkStatusChange: callback => { listener = callback },
  createIntersectionObserver: () => ({ relativeToViewport() { return this }, observe() {}, disconnect() {} }),
  getFileSystemManager: () => ({ statSync: () => ({ size: 12 }), unlink: options => options.success(), readdirSync: () => [] }),
  downloadFile(options) {
    const url = new URL(options.url)
    assert.equal(url.origin, 'http://127.0.0.1:18080')
    url.port = port
    fetch(url, { signal: AbortSignal.timeout(5000) }).then(async response => {
      await response.arrayBuffer()
      options.success({ statusCode: response.status, tempFilePath: '/private-download-path' })
    }, options.fail)
    return { abort() {} }
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
  const { data } = await api.startParse({ shareText: source, selectedUrl: source, source: 'manual', flowId }, api.id(), owner)
  const job = await api.queryJob(data.jobId, owner)
  client('services/history.js').stageHistory({ resultId: job.resultId, userId: owner, jobId: job.jobId })
  client('pages/result/index.js')
  page.onLoad({ resultId: job.resultId, flowId })
  await until(() => page.data.result, 2000)
  page.play()
  await until(() => page.data.previewURL, 2000)
  assert.deepEqual(entries().map(entry => entry.event.name), ['result_viewed', 'preview_started'])
  page.previewShown({ currentTarget: { dataset: { generation: page.data.previewGeneration } } })
  page.closePreview()
  await page.runSave(page.data.result.assets)
  const pageEvents = entries().map(entry => entry.event)
  assert.deepEqual(pageEvents.map(event => [event.name, event.outcome]), [
    ['result_viewed', 'succeeded'], ['preview_started', 'started'], ['preview_finished', 'succeeded'],
    ['save_action_started', 'started'], ['client_download_finished', 'succeeded'], ['save_action_finished', 'succeeded'],
  ])
  assert.equal(pageEvents[4].bytes, 12)
  assert.equal(pageEvents[4].batchId, pageEvents[5].batchId)
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
}

run().catch(error => { console.error(error); process.exitCode = 1 }).finally(() => lifecycle?.onHide())
