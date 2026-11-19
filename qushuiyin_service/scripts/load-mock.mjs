import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { availableParallelism, platform, totalmem } from 'node:os'
import { performance } from 'node:perf_hooks'
import { setTimeout as delay } from 'node:timers/promises'

const port = Number(process.argv[2] || 18080)
const users = Number(process.argv[3] || 20)
assert.ok(Number.isInteger(port) && port > 0 && port < 65536, 'Expected a loopback port')
assert.ok(Number.isInteger(users) && users >= 1 && users <= 100, 'Expected 1–100 synthetic users')
const base = 'http://127.0.0.1:' + port
const stages = new Map()
let bytes = 0
const controlled = new Set(['SERVICE_BUSY', 'RATE_LIMITED', 'MEDIA_BUDGET_EXCEEDED', 'STORAGE_CAPACITY_LOW', 'DAILY_LIMIT_REACHED'])
class Outcome extends Error {
  constructor(stage, code) { super(stage + ':' + code); this.stage = stage; this.code = code }
}

async function measure(stage, work) {
  const start = performance.now()
  try { return await work() } finally {
    if (!stages.has(stage)) stages.set(stage, [])
    stages.get(stage).push(performance.now() - start)
  }
}

async function request(stage, method, path, options = {}) {
  return measure(stage, async () => {
    const headers = options.token ? { Authorization: 'Bearer ' + options.token } : {}
    if (options.data) headers['Content-Type'] = 'application/json'
    if (options.idempotent) headers['Idempotency-Key'] = randomUUID()
    let response, envelope
    try {
      response = await fetch(base + path, { method, headers, body: options.data && JSON.stringify(options.data),
        redirect: 'error', signal: AbortSignal.timeout(30000) })
      envelope = await response.json()
    } catch { throw new Outcome(stage, 'TRANSPORT_OR_RESPONSE') }
    if (!response.ok || envelope.code !== 'OK') throw new Outcome(stage, envelope.code || 'HTTP_' + response.status)
    return envelope.data
  })
}

const config = await request('config', 'GET', '/v1/config')
assert.equal(config.features.mock, true, 'Run this script only against the local fixture service')
assert.equal(config.features.devLogin, true)

async function poll(stage, path, token, active, deadline) {
  while (Date.now() < deadline) {
    const current = await request(stage, 'GET', path, { token })
    if (!active.includes(current.status)) return current
    await delay(config.limits.poll_interval_ms)
  }
  throw new Outcome(stage, 'NOT_TERMINAL_BEFORE_DEADLINE')
}

async function media(stage, raw, range) {
  return measure(stage, async () => {
    const url = new URL(raw)
    if (url.origin !== base || !url.pathname.startsWith('/media/')) throw new Outcome(stage, 'UNEXPECTED_MEDIA_ORIGIN')
    const response = await fetch(url, { headers: range ? { Range: range } : undefined,
      redirect: 'error', signal: AbortSignal.timeout(config.limits.client_download_timeout_seconds * 1000) })
    if (response.status !== (range ? 206 : 200)) {
      const envelope = await response.json().catch(() => null)
      throw new Outcome(stage, envelope?.code || 'HTTP_' + response.status)
    }
    let size = 0
    for await (const chunk of response.body) size += chunk.byteLength
    if (size <= 0 || size !== Number(response.headers.get('content-length'))) throw new Outcome(stage, 'INCOMPLETE_FILE')
    bytes += size
  })
}

const scenarios = [
  { name: 'video_relay', share: 'https://www.douyin.com/video/10001', preview: true },
  { name: 'gallery', share: 'https://www.xiaohongshu.com/explore/10004' },
  { name: 'video_fallback', share: 'https://www.douyin.com/video/10005', fallback: true },
  { name: 'source_error', share: 'https://www.douyin.com/video/10003', failure: 'UPSTREAM_UNAVAILABLE' },
]

async function exercise(scenario) {
  const flowId = randomUUID()
  const { token } = await request('login', 'POST', '/v1/sessions/wechat', {
    data: { code: 'dev:load-' + randomUUID(), clientVersion: 'load', privacyVersion: config.privacyVersion },
  })
  const job = await request('parse_create', 'POST', '/v1/parse-jobs', {
    token, data: { shareText: scenario.share, flowId, source: 'manual' }, idempotent: true,
  })
  const done = await measure('parse_until_terminal', () => poll('parse_poll', '/v1/parse-jobs/' + job.jobId, token, ['QUEUED', 'RUNNING'],
    Math.min(Date.parse(job.deadlineAt) + config.limits.poll_interval_ms, Date.now() + config.limits.client_parse_wait_seconds * 1000)))
  if (done.status !== 'SUCCEEDED') {
    const code = done.error?.code || done.status
    if (code === scenario.failure) return 'fixture_failure'
    throw new Outcome('parse', code)
  }
  if (scenario.failure) throw new Outcome('parse', 'UNEXPECTED_SUCCESS')
  const path = '/v1/results/' + done.resultId
  const result = await request('result', 'GET', path, { token })
  const assetIds = result.assets.slice(0, config.limits.max_image_batch_count).map(asset => asset.assetId)
  if (scenario.preview) {
    const { previews } = await request('preview_ticket', 'POST', path + '/preview-tickets', { token, data: { assetIds } })
    await media('preview_range', previews[0].url, 'bytes=0-11')
  }
  const batch = await request('batch_create', 'POST', '/v1/save-batches', {
    token, data: { resultId: result.resultId, assetIds, flowId, clientActionId: randomUUID() }, idempotent: true,
  })
  const batchPath = '/v1/save-batches/' + batch.batchId
  const batchDeadline = Math.min(Date.parse(batch.expiresAt), Date.now() + 240000)
  const prepared = () => measure('prepare_until_terminal', () => poll('batch_poll', batchPath, token, ['PREPARING'], batchDeadline))
  let ready = await prepared()
  if (scenario.fallback && ready.items[0]?.deliveryMode === 'direct') {
    await request('fallback', 'POST', batchPath + '/items/' + ready.items[0].itemId + '/fallback', {
      token, data: { reason: 'DIRECT_DOWNLOAD_REJECTED' }, idempotent: true,
    })
    ready = await prepared()
  }
  if (ready.status !== 'READY') throw new Outcome('prepare', ready.items.find(item => item.error)?.error.code || ready.status)
  const { tickets } = await request('tickets', 'POST', batchPath + '/tickets', { token, data: { itemIds: ready.items.map(item => item.itemId) } })
  for (const ticket of tickets) await media('download', ticket.url)
  return 'completed'
}

const started = performance.now()
const outcomes = await Promise.all(Array.from({ length: users }, async (_, i) => {
  const scenario = scenarios[i % scenarios.length]
  try { return { scenario: scenario.name, outcome: await exercise(scenario) } }
  catch (err) {
    if (err instanceof Outcome) return { scenario: scenario.name, outcome: controlled.has(err.code) ? 'controlled_rejection' : 'unexpected', stage: err.stage, code: err.code }
    return { scenario: scenario.name, outcome: 'unexpected', code: 'CLIENT_OR_TRANSPORT_ERROR' }
  }
}))
const wall = performance.now() - started
const latency = Object.fromEntries([...stages].map(([stage, data]) => {
  data.sort((a, b) => a - b)
  const p = ratio => Math.round(data[Math.ceil(data.length * ratio) - 1])
  return [stage, { count: data.length, p50Ms: p(.5), p95Ms: p(.95), maxMs: p(1) }]
}))
const totals = Object.groupBy(outcomes, row => row.outcome)
const interruptions = Object.groupBy(outcomes.filter(row => row.code), row => [row.outcome, row.stage || 'client', row.code].join('/'))
console.log(JSON.stringify({
  environment: { os: platform(), cpus: availableParallelism(), hostBytes: totalmem(), upstream: 'local fixtures' },
  users, scenarios: scenarios.map(route => route.name), wallMs: Math.round(wall), mediaBytes: bytes, averageMediaBitsPerSecond: Math.round(bytes * 8000 / wall),
  outcomes: Object.fromEntries(Object.entries(totals).map(([key, value]) => [key, value.length])),
  interruptions: Object.fromEntries(Object.entries(interruptions).map(([key, value]) => [key, value.length])), latency,
}, null, 2))
if (totals.unexpected?.length || !totals.completed?.length) process.exitCode = 1
