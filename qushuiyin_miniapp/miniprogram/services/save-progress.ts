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

function records(user: string) {
  const entries = wx.getStorageSync(storageKey(user)) as SaveProgress[] | undefined
  return Array.isArray(entries) ? entries.filter(entry => entry?.userId === user && opaque(entry.resultId) && Date.parse(entry.expiresAt) > Date.now()) : []
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
    return { ...saved, states }
  } catch { return undefined }
}

export function writeSaveProgress(progress: SaveProgress) {
  const others = records(progress.userId).filter(entry => entry.resultId !== progress.resultId)
  wx.setStorageSync(storageKey(progress.userId), [...others, progress])
}

export function clearSaveProgress(user: string, resultId?: string) {
  const remaining = resultId ? records(user).filter(entry => entry.resultId !== resultId) : []
  if (remaining.length) wx.setStorageSync(storageKey(user), remaining)
  else wx.removeStorageSync(storageKey(user))
}
