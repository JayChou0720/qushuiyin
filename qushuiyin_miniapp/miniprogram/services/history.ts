import { HistorySource, PublicConfig, Result, id, isCurrentUser } from './api'

const version = 1
const day = 24 * 60 * 60 * 1000
type Limits = PublicConfig['limits']
export interface HistoryEntry {
  localId: string; userId: string; platform: string; contentId: string | null
  sourceUrl: string; sourceKey: string; title: string | null; mediaType: string; assetCount: number
  parsedAt: number; lastUsedAt: number; resultId: string; resultExpiresAt: number
  lastSaveSummary: string | null; thumbnailLocalPath: string | null
}
interface History { schemaVersion: number; enabled: boolean; entries: HistoryEntry[] }
interface Seed { resultId: string; userId: string; source?: HistorySource | null; jobId?: string }
let seed: Seed | undefined
export function stageHistory(next: Seed) { seed = next }
export function takeHistory(resultId: string) {
  if (seed?.resultId !== resultId) return undefined
  const next = seed
  seed = undefined
  return next
}
const keyFor = (userId: string) => 'qsy.history.' + userId
const noHistory = (): History => ({ schemaVersion: version, enabled: true, entries: [] })
const plainId = /^[a-zA-Z0-9_-]+$/
function thumbnailName(userId: string, localId: string) { return userId + '.' + localId + '.img' }
function validEntry(item: HistoryEntry, userId: string) {
  return item && item.userId === userId && typeof item.localId === 'string' && plainId.test(item.localId) &&
    typeof item.sourceUrl === 'string' && item.sourceUrl.startsWith('https://') && /^[a-f0-9]{64}$/.test(item.sourceKey) &&
    typeof item.platform === 'string' && (item.contentId === null || typeof item.contentId === 'string') &&
    (item.title === null || typeof item.title === 'string') && (item.mediaType === 'video' || item.mediaType === 'image') &&
    Number.isFinite(item.assetCount) && Number.isFinite(item.parsedAt) && Number.isFinite(item.lastUsedAt) &&
    typeof item.resultId === 'string' && plainId.test(item.resultId) && Number.isFinite(item.resultExpiresAt) &&
    (item.lastSaveSummary === null || typeof item.lastSaveSummary === 'string') &&
    (item.thumbnailLocalPath === null || item.thumbnailLocalPath === thumbnailName(userId, item.localId))
}
function read(userId: string): History {
  const key = keyFor(userId)
  const stored = wx.getStorageSync(key) as History | undefined
  if (!stored) return noHistory()
  if (stored.schemaVersion === version && typeof stored.enabled === 'boolean' && Array.isArray(stored.entries)) {
    return { ...stored, entries: stored.entries.filter(item => validEntry(item, userId)) }
  }
  // An unsupported schema may contain old paths; purge only this partition and its owned files.
  wx.removeStorageSync(key)
  clearHistoryThumbnails(userId)
  return noHistory()
}
function write(userId: string, store: History) { wx.setStorageSync(keyFor(userId), store) }
function dateValue(iso: string) { return Date.parse(iso) }
function trim(entries: HistoryEntry[], limits: Limits) {
  const count = limits.history_max_count, days = limits.history_ttl_days
  if (!Number.isSafeInteger(count) || count < 1 || !Number.isSafeInteger(days) || days < 1) return []
  const minimum = Date.now() - days * day
  return entries.filter(item => item.lastUsedAt > minimum).sort((a, b) => b.lastUsedAt - a.lastUsedAt).slice(0, count)
}
function dropThumbnails(previous: HistoryEntry[], next: HistoryEntry[]) {
  const remaining = new Set(next.map(item => item.localId))
  for (const item of previous) if (!remaining.has(item.localId)) removeThumbnail(item.userId, item.localId)
}
export function loadHistory(userId: string, limits: Limits) {
  const store = read(userId)
  const entries = trim(store.entries, limits)
  if (entries.length < store.entries.length && limits.history_max_count > 0 && limits.history_ttl_days > 0) {
    try { write(userId, { ...store, entries }); dropThumbnails(store.entries, entries) } catch { /* Expired records stay hidden if storage is full. */ }
  }
  return { ...store, entries }
}
export function setHistoryEnabled(userId: string, enabled: boolean, clear: boolean) {
  const store = read(userId)
  write(userId, { ...store, enabled, entries: clear ? [] : store.entries })
  if (clear) return clearHistoryThumbnails(userId)
  return true
}
export function deleteHistoryItem(userId: string, localId: string) {
  const store = read(userId)
  write(userId, { ...store, entries: store.entries.filter(item => item.localId !== localId) })
  return removeThumbnail(userId, localId)
}
export function clearHistoryRecords(userId: string, forgetPreference = false) {
  if (forgetPreference) {
    if (seed?.userId === userId) seed = undefined
    const thumbnailsCleared = clearHistoryThumbnails(userId)
    wx.removeStorageSync(keyFor(userId))
    return thumbnailsCleared
  }
  return setHistoryEnabled(userId, read(userId).enabled, true)
}
export function recordHistory(context: Seed, result: Result, limits: Limits) {
  const { userId, source } = context
  if (!source || !isCurrentUser(userId) || result.resultId !== context.resultId || !source.url.startsWith('https://') || !/^[a-f0-9]{64}$/.test(source.key)) return undefined
  try {
    const store = read(userId)
    if (!store.enabled) return undefined
    const parsedAt = dateValue(result.createdAt), resultExpiresAt = dateValue(result.expiresAt)
    if (!Number.isFinite(parsedAt) || !Number.isFinite(resultExpiresAt)) return undefined
    const same = (item: HistoryEntry) => item.platform === result.platform && (result.contentId && item.contentId
      ? item.contentId === result.contentId : item.sourceKey === source.key)
    const old = store.entries.find(same)
    const item: HistoryEntry = { localId: old?.localId || id(), userId, platform: result.platform,
      contentId: result.contentId, sourceUrl: source.url, sourceKey: source.key, title: result.title, mediaType: result.mediaType,
      assetCount: result.assetCount, parsedAt, lastUsedAt: Date.now(), resultId: result.resultId, resultExpiresAt,
      lastSaveSummary: old?.lastSaveSummary ?? null, thumbnailLocalPath: old?.thumbnailLocalPath ?? null }
    if (!validEntry(item, userId)) return undefined
    const entries = trim([item, ...store.entries.filter(existing => !same(existing))], limits)
    if (!entries.length) return undefined
    write(userId, { ...store, entries })
    dropThumbnails(store.entries, entries)
    return item
  } catch { return undefined }
}
function updateHistory(userId: string, localId: string, change: Partial<HistoryEntry>, onlyEnabled = false) {
  if (!isCurrentUser(userId)) return false
  try {
    const store = read(userId)
    if ((onlyEnabled && !store.enabled) || !store.entries.some(item => item.localId === localId)) return false
    write(userId, { ...store, entries: store.entries.map(item => item.localId === localId ? { ...item, ...change } : item) })
    return true
  } catch { return false }
}
export const touchHistory = (userId: string, localId: string) => updateHistory(userId, localId, { lastUsedAt: Date.now() })
export function noteHistorySave(userId: string, resultId: string, lastSaveSummary: string) {
  if (!isCurrentUser(userId)) return
  try {
    const item = read(userId).entries.find(entry => entry.resultId === resultId)
    if (item) updateHistory(userId, item.localId, { lastSaveSummary, lastUsedAt: Date.now() }, true)
  } catch { /* Local storage cannot affect saving. */ }
}

const cacheDir = () => wx.env.USER_DATA_PATH + '/qsy-history-thumbnails'
const ownedFile = /^[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.img$/
let cacheEpoch = 0
const missingFile = (error: unknown) => /no such file|not exist|enoent/i.test(
  String((error as { errMsg?: string } | null)?.errMsg || error))
function removeFile(name: string) {
  if (!ownedFile.test(name)) return false
  try { wx.getFileSystemManager().unlinkSync(cacheDir() + '/' + name); return true } catch (error) { return missingFile(error) }
}
function removeThumbnail(userId: string, localId: string) { return removeFile(thumbnailName(userId, localId)) }
export function clearHistoryThumbnails(userId?: string) {
  cacheEpoch++
  try {
    const files = wx.getFileSystemManager().readdirSync(cacheDir())
      .filter(name => ownedFile.test(name) && (!userId || name.startsWith(userId + '.')))
    return files.map(removeFile).every(Boolean)
  } catch (error) { return missingFile(error) }
}
export function thumbnailPath(item: HistoryEntry) {
  if (!item.thumbnailLocalPath || item.thumbnailLocalPath !== thumbnailName(item.userId, item.localId)) return ''
  const path = cacheDir() + '/' + item.thumbnailLocalPath
  try { wx.getFileSystemManager().accessSync(path); return path } catch { return '' }
}
let thumbnailBusy = false
export async function cacheThumbnail(item: HistoryEntry, getURL: () => Promise<{ url: string }>, limits: Limits) {
  const budget = limits.thumbnail_local_budget_bytes
  if (thumbnailBusy || !isCurrentUser(item.userId) || !Number.isSafeInteger(budget) || budget <= 0 || thumbnailPath(item)) return
  thumbnailBusy = true
  const epoch = cacheEpoch
  let temp = ''
  try {
    const ticket = await getURL()
    if (epoch !== cacheEpoch || !isCurrentUser(item.userId)) return
    const reply = await new Promise<WechatMiniprogram.DownloadFileSuccessCallbackResult>((resolve, reject) => {
      wx.downloadFile({ url: ticket.url, timeout: limits.client_download_timeout_seconds * 1000, success: resolve, fail: reject })
    })
    temp = reply.tempFilePath
    if (reply.statusCode !== 200 || !temp || epoch !== cacheEpoch || !isCurrentUser(item.userId)) return
    const store = read(item.userId)
    if (!store.enabled || !store.entries.some(entry => entry.localId === item.localId && entry.resultId === item.resultId)) return
    const fs = wx.getFileSystemManager()
    const size = (fs.statSync(temp) as WechatMiniprogram.Stats).size
    if (!Number.isFinite(size) || size <= 0 || size > budget) return
    try { fs.mkdirSync(cacheDir(), true) } catch { /* An existing cache directory is fine. */ }
    const uses = new Map(store.entries.map(entry => [thumbnailName(entry.userId, entry.localId), entry.lastUsedAt]))
    const files = fs.readdirSync(cacheDir()).filter(name => ownedFile.test(name)).map(name => {
      const stat = fs.statSync(cacheDir() + '/' + name) as WechatMiniprogram.Stats
      return { name, size: stat.size, used: uses.get(name) ?? stat.lastModifiedTime * 1000 }
    }).sort((a, b) => a.used - b.used)
    let total = size + files.reduce((sum, file) => sum + file.size, 0)
    for (const file of files) {
      if (total <= budget) break
      if (removeFile(file.name)) total -= file.size
    }
    if (total > budget) return
    const name = thumbnailName(item.userId, item.localId)
    fs.saveFileSync(temp, cacheDir() + '/' + name)
    temp = ''
    if (!updateHistory(item.userId, item.localId, { thumbnailLocalPath: name }, true)) removeFile(name)
  } catch { /* Missing or oversized images use the platform placeholder. */ }
  finally {
    if (temp) { try { wx.getFileSystemManager().unlinkSync(temp) } catch { /* WeChat owns temporary files. */ } }
    thumbnailBusy = false
  }
}
