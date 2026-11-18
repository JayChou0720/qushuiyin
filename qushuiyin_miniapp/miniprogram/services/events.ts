import { APIError, EventLimits, cachedEventLimits, clientVersion, confirmUserIdentity, hasHistoryIdentity, id, isCurrentUser, isKnownUser, loadConfig, reportClientEvents } from './api'

const key = 'qsy.client-events.v1'
const hour = 3600000
const stages = { result_viewed: 'result', preview_started: 'preview', preview_finished: 'preview',
  save_action_started: 'save', save_action_finished: 'save', client_download_finished: 'download', album_save_finished: 'album' } as const
type Name = keyof typeof stages
type Outcome = 'started' | 'succeeded' | 'failed' | 'cancelled' | 'partial' | 'unknown'
type Code = 'CLIENT_DOWNLOAD_FAILED' | 'DIRECT_DOWNLOAD_REJECTED' | 'USER_CANCELLED' | 'SAVE_OUTCOME_UNKNOWN'
interface Details {
  flowId?: string; clientActionId?: string; jobId?: string; batchId?: string; itemId?: string
  deliveryMode?: 'direct' | 'relay'; errorCode?: Code; durationMs?: number; bytes?: number
}
interface Payload extends Required<{ [K in keyof Details]: Details[K] | null }> {
  eventId: string; name: Name; stage: typeof stages[Name]; outcome: Outcome; clientTime: string; clientVersion: string; osFamily: string
}
interface Entry { owner: string; event: Payload }
interface Queue { entries: Entry[]; limits?: EventLimits }

const opaque = (s: unknown) => typeof s === 'string' && /^[A-Za-z0-9_-]{1,64}$/.test(s) ? s : null
const metric = (n: unknown) => typeof n === 'number' && Number.isSafeInteger(n) && n >= 0 ? n : null
const outcomes = ['started', 'succeeded', 'failed', 'cancelled', 'partial', 'unknown']
const codes = ['CLIENT_DOWNLOAD_FAILED', 'DIRECT_DOWNLOAD_REJECTED', 'USER_CANCELLED', 'SAVE_OUTCOME_UNKNOWN']
const osFamilies = ['ios', 'android', 'windows', 'macos', 'devtools', 'unknown']
function payload(name: Name, outcome: Outcome, details: Details | Payload, eventId: string, clientTime: string, osFamily: string): Payload {
  const code = details.errorCode
  return { eventId, name, stage: stages[name], outcome, clientVersion, clientTime, osFamily: osFamilies.includes(osFamily) ? osFamily : 'unknown',
    flowId: opaque(details.flowId), clientActionId: opaque(details.clientActionId), jobId: opaque(details.jobId),
    batchId: opaque(details.batchId), itemId: opaque(details.batchId) ? opaque(details.itemId) : null,
    deliveryMode: details.deliveryMode === 'direct' || details.deliveryMode === 'relay' ? details.deliveryMode : null,
    errorCode: code && codes.includes(code) ? code : null,
    durationMs: metric(details.durationMs), bytes: metric(details.bytes) }
}
function validLimits(limits?: EventLimits): limits is EventLimits {
  return !!limits && [limits.event_batch_max, limits.client_queue_max, limits.client_event_retention_hours,
    limits.client_event_retry_initial_seconds, limits.client_event_retry_max_seconds].every(n => Number.isSafeInteger(n) && n > 0) &&
    limits.client_event_retry_max_seconds >= limits.client_event_retry_initial_seconds && limits.client_event_retry_max_seconds * 1000 < limits.client_event_retention_hours * hour
}
function read(): Queue {
  const store = wx.getStorageSync(key) as Partial<Queue> | undefined
  return { entries: Array.isArray(store?.entries) ? store.entries : [], limits: store?.limits }
}
function trim(entries: Entry[], limits: EventLimits): Entry[] {
  const cutoff = Date.now() - limits.client_event_retention_hours * hour
  return entries.filter(entry => entry?.event && opaque(entry.owner) && opaque(entry.event.eventId) &&
    Object.prototype.hasOwnProperty.call(stages, entry.event.name) && outcomes.includes(entry.event.outcome) &&
    Date.parse(entry.event.clientTime) > cutoff).slice(-limits.client_queue_max)
}
function write(entries: Entry[], limits?: EventLimits) {
  if (!entries.length) { wx.removeStorageSync(key); return }
  // Store just the event limits so a restart can retry before config is reachable.
  const snapshot = limits && {
    event_batch_max: limits.event_batch_max, client_queue_max: limits.client_queue_max,
    client_event_retention_hours: limits.client_event_retention_hours,
    client_event_retry_initial_seconds: limits.client_event_retry_initial_seconds,
    client_event_retry_max_seconds: limits.client_event_retry_max_seconds,
  }
  wx.setStorageSync(key, { entries, limits: snapshot })
}
function osFamily() {
  try {
    const device = wx as typeof wx & { getDeviceInfo?: () => { platform: string } }
    return device.getDeviceInfo?.().platform || 'unknown'
  } catch { return 'unknown' }
}
export function enqueueClientEvent(owner: string, name: Name, outcome: Outcome, details: Details = {}) {
  try {
    const limits = cachedEventLimits()
    if (!isKnownUser(owner) || !validLimits(limits)) return
    const event = payload(name, outcome, details, id(), new Date().toISOString(), osFamily())
    write(trim([...read().entries, { owner, event }], limits), limits)
    schedule()
  } catch { /* Telemetry must never affect a user action. */ }
}
export function clearClientEvents(owner: string) {
  const { entries, limits } = read()
  write(entries.filter(entry => entry?.owner !== owner), limits)
}

let foreground = false, online = false, sending = false, retryMs = 0
let timer: ReturnType<typeof setTimeout> | undefined
function cancelTimer() { if (timer !== undefined) clearTimeout(timer); timer = undefined }
function schedule(delay = retryMs) {
  if (!foreground || !online || sending || timer !== undefined) return
  timer = setTimeout(() => { timer = undefined; void flushClientEvents() }, delay)
}
export function setEventForeground(visible: boolean) {
  foreground = visible
  if (!visible) { cancelTimer(); return }
  try {
    const store = read()
    const limits = cachedEventLimits() || store.limits
    if (validLimits(limits)) {
      const pending = trim(store.entries, limits)
      if (pending.length !== store.entries.length) write(pending, limits)
    }
  } catch { /* Local storage is best effort, including expiry when offline. */ }
  schedule()
}
export function watchEventNetwork() {
  let revision = 0
  const update = (connected: boolean) => {
    const wasOnline = online
    online = connected
    if (!online) cancelTimer()
    else if (!wasOnline) { retryMs = 0; schedule() }
  }
  wx.onNetworkStatusChange(({ isConnected }) => {
    revision++
    update(isConnected)
  })
  wx.getNetworkType({
    success: ({ networkType }) => { if (!revision) update(networkType !== 'none') },
    fail: () => { if (!revision) update(true) },
  })
}
export async function flushClientEvents() {
  if (!foreground || !online || sending) return
  sending = true
  let again = false, limits: EventLimits | undefined
  try {
    const store = read()
    limits = cachedEventLimits() || store.limits
    if (!validLimits(limits) || !store.entries.length) return
    const entries = trim(store.entries, limits)
    if (entries.length !== store.entries.length) write(entries, limits)
    if (!entries.length || !hasHistoryIdentity()) { retryMs = 0; return }
    const owner = await confirmUserIdentity(false)
    if (!foreground || !online || !isCurrentUser(owner)) return
    const batch = trim(read().entries, limits).filter(entry => entry.owner === owner).slice(0, limits.event_batch_max)
    if (!batch.length) { retryMs = 0; return }
    const receipt = await reportClientEvents(batch.map(({ event }) => payload(event.name, event.outcome,
      event, event.eventId, event.clientTime, event.osFamily)), owner)
    const delivered = new Set([...receipt.acceptedEventIds, ...receipt.duplicateEventIds, ...receipt.rejected.map(r => r.eventId)])
    // Re-read to keep events recorded during the request, and only remove acknowledged IDs from this batch.
    const done = new Set(batch.filter(({ event }) => delivered.has(event.eventId)).map(({ event }) => event.eventId))
    const pending = read().entries.filter(entry => entry.owner !== owner || !done.has(entry.event.eventId))
    if (done.size) write(pending, limits)
    if (done.size < batch.length) throw new Error('Incomplete event receipt')
    retryMs = 0
    again = pending.some(entry => entry.owner === owner)
  } catch (error) {
    if (error instanceof APIError && error.code === 'BAD_REQUEST' && online && foreground) {
      await loadConfig().catch(() => {})
    }
    if (validLimits(limits)) {
      retryMs = Math.min(limits.client_event_retry_max_seconds * 1000, retryMs * 2 || limits.client_event_retry_initial_seconds * 1000)
      again = true
    }
  } finally { sending = false; if (again) schedule() }
}
