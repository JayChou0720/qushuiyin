export type SaveState = 'SAVING' | 'SUCCEEDED' | 'FAILED' | 'UNKNOWN'

export interface SaveProgress {
  userId: string
  resultId: string
  flowId: string
  actionId: string
  assetIds: string[]
  batchId: string
  expiresAt: string
  states: Record<string, SaveState>
}

const storageKey = (user: string) => 'qsy.save-progress.v1.' + user
const opaque = (value: unknown): value is string => typeof value === 'string' && /^[A-Za-z0-9_-]{1,64}$/.test(value)

const volatile = new Map<string, Map<string, { expiresAt: string; progress?: SaveProgress }>>()

function storedRecords(user: string) {
  const entries = wx.getStorageSync(storageKey(user)) as SaveProgress[] | undefined
  return Array.isArray(entries) ? entries.filter(entry => entry?.userId === user && opaque(entry.resultId) && Date.parse(entry.expiresAt) > Date.now()) : []
}

function records(user: string, requireStorage = false) {
  let entries: SaveProgress[] = []
  try { entries = storedRecords(user) }
  catch (error) { if (requireStorage) throw error }
  const pending = volatile.get(user)
  if (!pending) return entries
  for (const [result, entry] of pending) {
    if (Date.parse(entry.expiresAt) <= Date.now()) { pending.delete(result); continue }
    entries = entries.filter(saved => saved.resultId !== result)
    if (entry.progress) entries.push(entry.progress)
  }
  if (!pending.size) volatile.delete(user)
  return entries
}

export function latestSaveResult(user: string) {
  try { return records(user).pop()?.resultId || '' } catch { return '' }
}

export function loadSaveProgress(user: string, resultId: string, assets: ReadonlySet<string>): SaveProgress | undefined {
  try {
    const saved = records(user).find(entry => entry.resultId === resultId)
    if (!saved || saved.userId !== user || saved.resultId !== resultId || !opaque(saved.flowId) || !opaque(saved.actionId) ||
      (saved.batchId !== '' && !opaque(saved.batchId)) || !(Date.parse(saved.expiresAt) > Date.now()) || !Array.isArray(saved.assetIds) ||
      saved.assetIds.length === 0 || !saved.assetIds.every(asset => assets.has(asset)) || !saved.states || typeof saved.states !== 'object') return
    const states: Record<string, SaveState> = {}
    for (const [item, state] of Object.entries(saved.states)) {
      if (!opaque(item)) continue
      if (state === 'SAVING' || state === 'UNKNOWN') states[item] = 'UNKNOWN'
      else if (state === 'SUCCEEDED' || state === 'FAILED') states[item] = state
    }
    return { ...saved, assetIds: [...saved.assetIds], states }
  } catch { return undefined }
}

export const isVolatileSave = (user: string, result: string) => !!volatile.get(user)?.get(result)?.progress

export function writeSaveProgress(progress: SaveProgress) {
  const { userId, resultId, expiresAt } = progress
  const key = storageKey(userId)
  try {
    const others = records(userId, true).filter(entry => entry.resultId !== resultId)
    wx.setStorageSync(key, [...others, progress])
    volatile.delete(userId)
    return true
  } catch {
    const pending = volatile.get(userId) || new Map()
    pending.set(resultId, { expiresAt, progress: { ...progress, states: { ...progress.states } } })
    volatile.set(userId, pending)
    // A stale checkpoint could replay items the in-memory operation already saved.
    try {
      const stored = storedRecords(userId)
      const remaining = stored.filter(entry => entry.resultId !== resultId)
      if (remaining.length === stored.length) return false
      if (remaining.length) wx.setStorageSync(key, remaining)
      else wx.removeStorageSync(key)
    } catch { /* This run still tracks every completed item. */ }
    return false
  }
}

export function clearSaveProgress(user: string, resultId?: string) {
  try {
    const remaining = resultId ? records(user, true).filter(entry => entry.resultId !== resultId) : []
    if (remaining.length) wx.setStorageSync(storageKey(user), remaining)
    else wx.removeStorageSync(storageKey(user))
    volatile.delete(user)
  } catch (error) {
    const pending = volatile.get(user) || new Map()
    for (const entry of records(user)) {
      if (!resultId || entry.resultId === resultId) pending.set(entry.resultId, { expiresAt: entry.expiresAt })
    }
    if (pending.size) volatile.set(user, pending)
    throw error
  }
}
