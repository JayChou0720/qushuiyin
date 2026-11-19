const assert = require('node:assert/strict')
const { resolve } = require('node:path')

const [build, port = '18080'] = process.argv.slice(2)
assert.ok(build && Number(port) > 0 && Number(port) < 65536, 'Expected compiled mini-program directory and local API port')
const client = name => require(resolve(build, name))
const storage = new Map()
const files = new Set()
const missing = () => { throw { errMsg: 'no such file' } }
let full = false, offline = false, page, route = '', actionSheet, modal, requests = []
global.Page = value => { page = { ...value, setData: updates => Object.assign(page.data, updates) } }
global.wx = {
  env: { USER_DATA_PATH: '/user' },
  getAccountInfoSync: () => ({ miniProgram: { envVersion: 'develop' } }),
  getStorageSync: key => structuredClone(storage.get(key)),
  setStorageSync(key, value) { if (full) throw new Error('quota'); storage.set(key, structuredClone(value)) },
  removeStorageSync: key => storage.delete(key),
  getFileSystemManager: () => ({
    readdirSync: () => [...files].map(path => path.split('/').at(-1)),
    unlinkSync(path) { if (!files.delete(path)) missing() },
    accessSync(path) { if (!files.has(path)) missing() },
    statSync: () => ({ size: 6, lastModifiedTime: 1 }),
    mkdirSync() {},
    saveFileSync(from, to) { if (!files.delete(from)) missing(); files.add(to) },
  }),
  downloadFile(options) { files.add('/wx-temp'); options.success({statusCode: 200, tempFilePath: '/wx-temp'}) },
  navigateTo: options => { route = options.url },
  showActionSheet: options => { actionSheet = options },
  showModal: options => { modal = options },
  request(options) {
    const original = new URL(options.url)
    assert.equal(original.origin, 'http://127.0.0.1:18080')
    if (offline) { options.fail(); return }
    requests.push({ method: options.method, path: original.pathname, data: options.data })
    original.port = port
    const headers = { 'Content-Type': 'application/json', ...options.header }
    fetch(original, { method: options.method, headers, body: options.data && JSON.stringify(options.data), signal: AbortSignal.timeout(8000) })
      .then(response => response.json()).then(data => options.success({ data }), options.fail)
  },
}

async function run() {
  const api = client('services/api.js'), history = client('services/history.js')
  const owner = await api.confirmUserIdentity()
  const limits = (await api.loadConfig()).limits
  const source = 'https://www.xiaohongshu.com/explore/10002?signature=x%2By&token=a%3Db'
  const parse = async (url = source) => {
    const { data } = await api.startParse({ shareText: url, selectedUrl: url, source: 'history', flowId: api.id() }, api.id(), owner)
    let job = await api.queryJob(data.jobId, owner)
    while (['QUEUED', 'RUNNING'].includes(job.status) && Date.now() < Date.parse(data.deadlineAt)) {
      await api.wait(data.pollAfterMs)
      job = await api.queryJob(data.jobId, owner)
    }
    assert.equal(job.status, 'SUCCEEDED')
    const result = await api.queryResult(job.resultId, owner)
    return { context: { resultId: result.resultId, userId: owner, source: job.historySource }, result }
  }
  const save = ({context, result}) => history.recordHistory(context, result, limits)
  const records = () => history.loadHistory(owner, limits).entries
  const initial = await parse(source + '#share')
  const original = save(initial)
  const second = await parse()
  assert.equal(save(second).localId, original.localId)
  assert.equal(records().length, 1)
  assert.equal(records()[0].sourceUrl, source)
  assert.equal(history.loadHistory('other-user', limits).entries.length, 0)
  assert.ok(!JSON.stringify(records()).includes('media.example.test'))

  client('pages/history/index.js')
  const historyPage = page
  const event = { currentTarget: { dataset: { id: original.localId } } }
  await page.refresh()
  await page.open(event)
  assert.ok(route.includes(second.result.resultId), 'reuses an unexpired result')
  const storageKey = 'qsy.history.' + owner
  storage.get(storageKey).entries[0].resultExpiresAt = Date.now() - 1
  await page.refresh()
  requests = []
  await page.open(event)
  assert.ok(route.includes('resultId=') && !route.includes(second.result.resultId), 'reparses an expired result')
  assert.equal(requests.find(req => req.path === '/v1/parse-jobs').data.selectedUrl, source)
  route = ''
  storage.get(storageKey).entries[0].sourceUrl = 'https://www.douyin.com/video/10003'
  await page.refresh()
  await page.open(event)
  assert.equal(route, '')
  assert.match(page.data.error, /重新复制/)
  assert.ok(save(second))
  offline = true
  await page.refresh()
  assert.equal(page.data.rows.length, 0)
  assert.ok(page.data.error)
  offline = false

  full = true
  assert.equal(save(second), undefined, 'quota errors do not break the successful parse')
  const progress = client('services/save-progress.js')
  const operation = { userId: owner, resultId: second.result.resultId, flowId: api.id(), actionId: api.id(), assetIds: [second.result.assets[0].assetId],
    batchId: api.id(), expiresAt: second.result.expiresAt, states: { 'local-item': 'SAVING' } }
  const ids = new Set(operation.assetIds)
  client('pages/result/index.js')
  page.operation = operation
  page.localFiles = new Map()
  let writes = 0
  wx.saveImageToPhotosAlbum = options => { writes++; options.success({}) }
  page.persistSave(operation)
  assert.equal(page.data.recoveryWarning, true)
  await page.writeAlbum(operation, { itemId: 'local-item' }, { path: '/test.jpg', bytes: 6, mode: 'relay' }, 'image')
  assert.equal(writes, 1, 'storage quota does not block saving to the album')
  assert.equal(progress.loadSaveProgress(owner, operation.resultId, ids).states['local-item'], 'SUCCEEDED')
  assert.equal(progress.loadSaveProgress('other-user', operation.resultId, ids), undefined)
  full = false
  page.persistSave(operation)
  assert.equal(page.data.recoveryWarning, false)
  progress.clearSaveProgress(owner, operation.resultId)

  client('pages/feedback/create.js')
  wx.pageScrollTo = () => {}
  await page.loadLimits()
  page.choose({ detail: { value: '0' } })
  page.editContent({ detail: { value: '本地验证未附带作品链接' } })
  page.toggleSource({ detail: { value: ['source'] } })
  page.editSource({ detail: { value: source } })
  page.toggleSource({ detail: { value: [] } })
  requests = []
  await page.submit()
  assert.ok(page.data.created)
  const feedback = requests.find(req => req.path === '/v1/feedbacks').data
  assert.equal(feedback.includeSource, false)
  assert.equal(Object.hasOwn(feedback, 'sourceUrl'), false)
  client('pages/privacy/index.js')
  await page.initHistory()
  const turnOff = () => page.toggleHistory({ detail: { value: false } })
  turnOff()
  actionSheet.success({ tapIndex: 0 })
  assert.equal(history.loadHistory(owner, limits).enabled, false)
  assert.equal(save(second), undefined)
  assert.equal(records().length, 1)
  page.toggleHistory({ detail: { value: true } })
  turnOff()
  actionSheet.success({ tapIndex: 1 })
  modal.success({ confirm: false })
  assert.equal(page.data.historyEnabled, true)
  assert.equal(records().length, 1)
  turnOff()
  actionSheet.success({ tapIndex: 1 })
  const dir = '/user/qsy-history-thumbnails/'
  files.add(dir + owner + '.' + original.localId + '.img')
  files.add(dir + 'other-user.some-id.img')
  modal.success({ confirm: true })
  assert.equal(records().length, 0)
  assert.deepEqual([...files], [dir + 'other-user.some-id.img'])

  files.clear()
  history.setHistoryEnabled(owner, true, false)
  const older = save(second)
  const newer = save(await parse('https://www.douyin.com/video/10001'))
  storage.get(storageKey).entries.find(entry => entry.localId === older.localId).lastUsedAt -= 1000
  const preview = async () => ({url: 'mock-preview-ticket'})
  const budget = { ...limits, thumbnail_local_budget_bytes: 6 }
  await history.cacheThumbnail(older, preview, budget)
  await history.cacheThumbnail(newer, preview, budget)
  assert.ok(!files.has(dir + owner + '.' + older.localId + '.img'))
  assert.ok(files.has(dir + owner + '.' + newer.localId + '.img'))
  assert.ok(!JSON.stringify(records()).includes('mock-preview-ticket'))
  assert.equal(history.loadHistory(owner, { ...limits, history_max_count: 1 }).entries.length, 1)
  for (const item of storage.get(storageKey).entries) item.lastUsedAt -= (limits.history_ttl_days + 1) * 86400000
  assert.equal(records().length, 0)
  assert.equal(limits.history_max_count, 100)
  let lastEntry
  for (let i = 0; i <= limits.history_max_count; i++) {
    const synthetic = { ...second.result, resultId: 'result-' + i, contentId: 'work-' + i }
    lastEntry = history.recordHistory({ ...second.context, resultId: synthetic.resultId }, synthetic, limits)
  }
  assert.equal(records().length, 100)
  assert.equal(records()[0].localId, lastEntry.localId)
  assert.equal(records().some(row => row.resultId === 'result-0'), false)
  storage.set('unrelated', 'keep')
  storage.set(storageKey, {schemaVersion: -1})
  assert.equal(records().length, 0)
  assert.equal(storage.has(storageKey), false)
  save(second)
  const backupRecords = structuredClone(storage.get(storageKey))
  await client('services/privacy.js').clearServiceData()
  assert.equal(storage.has(storageKey), false)
  assert.equal(storage.get('unrelated'), 'keep')
  assert.notEqual(await api.confirmUserIdentity(), owner)
  storage.set(storageKey, backupRecords)
  page = historyPage
  await page.refresh()
  assert.equal(page.data.rows.length, 0, 'an identity change hides records in a different partition')
  assert.ok(storage.has(storageKey))
  console.log('Mini-program data smoke passed: history retention, identity switching, album storage fallback, feedback consent, and scoped cleanup.')
}

run().catch(error => { console.error(error); process.exitCode = 1 })
