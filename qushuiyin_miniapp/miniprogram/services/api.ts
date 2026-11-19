import { LOCAL_API_ORIGIN, RELEASE_API_ORIGIN } from '../config/env'
const mockIdentityKey = 'mock-dev-user'
const usedWeChatKey = 'qsy.wechat-session-used'
export const clientVersion = 'm1'

export interface EventLimits { event_batch_max: number; client_queue_max: number; client_event_retention_hours: number; client_event_retry_initial_seconds: number; client_event_retry_max_seconds: number }

type Method = 'GET' | 'POST' | 'DELETE'
interface Envelope<T> { code: string; message: string; requestId: string; data: T; error?: { details?: { candidates?: Candidate[] } } }
export interface Candidate { url: string; displayHost: string; platform: string }
export interface PublicConfig {
  privacyVersion: string
  notice?: string
  help?: { message: string }
  features: { mock: boolean; devLogin: boolean }
  limits: EventLimits & { config_client_cache_seconds: number; max_share_text_chars: number; max_image_batch_count: number; client_parse_wait_seconds: number; client_download_timeout_seconds: number; max_video_bytes: number; max_image_bytes: number; poll_interval_ms: number; feedback_min_chars: number; feedback_max_chars: number; feedback_contact_max_chars: number; history_max_count: number; history_ttl_days: number; thumbnail_local_budget_bytes: number }
  platforms: { platform: string; status: string; videoEnabled: boolean; imageEnabled: boolean; maintenanceMessage?: string }[]
}
interface Session { userId: string; token: string; expiresAt: string }
interface CleanupReceipt { accessRevoked: true; cleanupStatus: 'scheduled'; cleanupDeadlineAt: string }
export interface HistorySource { url: string; key: string }
export interface Job { jobId: string; status: string; pollAfterMs: number; deadlineAt: string; resultId?: string; historySource?: HistorySource | null; error?: { message: string; traceId?: string; code?: string } }
export type FeedbackStatus = 'pending' | 'processing' | 'replied' | 'closed'
export interface FeedbackRequest { category: string; content: string; contact?: string; traceId?: string; includeSource: boolean; sourceUrl?: string }
export interface FeedbackCreated { feedbackId: string; status: FeedbackStatus; createdAt: string }
export interface FeedbackSummary { id: string; category: string; summary: string; status: FeedbackStatus; createdAt: string; updatedAt: string }
export interface FeedbackDetail extends Omit<FeedbackSummary, 'summary'> { content: string; contact: string | null; reply: string | null; repliedAt: string | null }
interface FeedbackPage { items: FeedbackSummary[]; nextCursor: string | null; hasMore: boolean }
export interface Asset { assetId: string; type: string; order: number }
export interface Result { resultId: string; platform: string; contentId: string | null; mediaType: string; title: string | null; description: string | null; assets: Asset[]; assetCount: number; cover: Asset | null; createdAt: string; expiresAt: string; warnings: string[]; capabilities: { canPreview: boolean } }
type PreviewKind = 'video' | 'thumbnail' | 'image'
export interface BatchItem { itemId: string; assetId: string; order: number; prepareStatus: string; sizeBytes: number | null; copyLinkAvailable: boolean; error: { message: string; retryable: boolean } | null }
export interface Batch { batchId: string; status: string; expiresAt: string; items: BatchItem[] }
interface BatchAccepted { batchId: string; status: string; pollAfterMs: number; expiresAt: string }
export interface DownloadTicket { itemId: string; deliveryMode: 'relay' | 'direct'; url: string; expiresAt: string }
interface Tickets { tickets: DownloadTicket[] }
export interface Input { shareText: string; selectedUrl: string | null; source: 'paste' | 'manual' | 'history'; flowId: string }
export class APIError extends Error {
  constructor(readonly code: string, message: string, readonly candidates: Candidate[] = []) { super(message) }
}
export class ConnectionError extends Error {}
let session: Session | undefined
let loggingIn: Promise<Session> | undefined
let cached: PublicConfig | undefined
let configLoadedAt = 0
let loadingConfig: Promise<PublicConfig> | undefined
let identityEpoch = 0
let deleting = false
let deletionTarget: Session | undefined
export const id = () => Date.now().toString(36) + '-' + Math.random().toString(36).slice(2)
export const wait = (ms: number) => new Promise<void>(resolve => setTimeout(resolve, ms))
function origin() {
  if (wx.getAccountInfoSync().miniProgram.envVersion === 'develop') return LOCAL_API_ORIGIN
  if (!/^https:\/\/[a-zA-Z0-9.-]+(?::443)?$/.test(RELEASE_API_ORIGIN)) throw new Error('本版本尚未配置服务地址')
  return RELEASE_API_ORIGIN
}
function call<T>(method: Method, path: string, data?: object, key?: string, credential?: Session): Promise<T> {
  return new Promise((resolve, reject) => {
    const header: Record<string, string> = {}
    if (credential) header.Authorization = 'Bearer ' + credential.token
    if (key) header['Idempotency-Key'] = key
    wx.request<Envelope<T>>({ url: origin() + path, method, header, data,
      success(res) {
        const e = res.data
        if (e?.code === 'OK') resolve(e.data)
        else reject(new APIError(e?.code || 'SERVICE_BUSY', e?.message || '服务暂不可用', e?.error?.details?.candidates))
      },
      fail() { reject(new ConnectionError('无法连接服务，请检查网络后重试')) },
    })
  })
}
export function loadConfig() {
  if (cached && Date.now() - configLoadedAt < cached.limits.config_client_cache_seconds * 1000) return Promise.resolve(cached)
  return loadingConfig ??= call<PublicConfig>('GET', '/v1/config').then(config => {
    cached = config
    configLoadedAt = Date.now()
    return config
  }).catch(error => {
    if (cached && error instanceof ConnectionError) return cached
    throw error
  }).finally(() => { loadingConfig = undefined })
}
export const feedbackConfig = () => cached ? Promise.resolve(cached) : loadConfig()
export const cachedEventLimits = () => cached?.limits
async function login(allowNew = true): Promise<Session> {
  if (session && Date.parse(session.expiresAt) > Date.now()) return session
  if (!loggingIn) {
    const epoch = identityEpoch
    loggingIn = (async () => {
      const config = cached || await loadConfig()
      const development = config.features.devLogin
      let code: string
      if (development) {
        let name = wx.getStorageSync(mockIdentityKey) as string
        if (!name) {
          if (!allowNew) throw new Error('此设备还没有使用过服务，无需清除')
          name = 'device-' + id()
          wx.setStorageSync(mockIdentityKey, name)
        }
        code = 'dev:' + name
      } else {
        if (!allowNew && !hasHistoryIdentity()) throw new Error('此设备还没有使用过服务，无需清除')
        code = await new Promise<string>((resolve, reject) => wx.login({
          success: result => result.code ? resolve(result.code) : reject(new Error('未能取得微信登录凭证，请重试')),
          fail: () => reject(new Error('微信登录暂不可用，请稍后重试')),
        }))
      }
      const next = await call<Session>('POST', '/v1/sessions/wechat', { code, clientVersion, privacyVersion: config.privacyVersion })
      if (epoch !== identityEpoch) throw new Error('使用身份已变更，请重新操作')
      if (!development) {
        try { wx.setStorageSync(usedWeChatKey, true) } catch { /* Keep the authenticated session usable in this run. */ }
      }
      session = next
      return next
    })().finally(() => { loggingIn = undefined })
  }
  return loggingIn
}
async function authenticatedRequest<T>(method: Method, path: string, data?: object, key?: string, owner?: string, allowNew = true) {
  const epoch = identityEpoch
  const verifyIdentity = () => {
    if (deleting || epoch !== identityEpoch) throw new Error('正在清除数据或使用身份已变更，请重新操作')
  }
  verifyIdentity()
  const send = async (credential: Session) => {
    verifyIdentity()
    if (owner && credential.userId !== owner) throw new APIError('IDENTITY_CHANGED', '当前使用身份已变更，请刷新列表')
    const response = await call<T>(method, path, data, key, credential)
    verifyIdentity()
    return { data: response, userId: credential.userId }
  }
  const credential = await login(allowNew)
  try { return await send(credential) } catch (e) {
    if (!(e instanceof APIError) || e.code !== 'SESSION_EXPIRED') throw e
    verifyIdentity()
    if (session === credential) session = undefined
    return send(await login(allowNew))
  }
}
async function request<T>(method: Method, path: string, data?: object, key?: string, owner?: string) {
  return (await authenticatedRequest<T>(method, path, data, key, owner)).data
}
export const isCurrentUser = (userId: string) => !deleting && session?.userId === userId && Date.parse(session.expiresAt) > Date.now()
export const isKnownUser = (userId: string) => !deleting && session?.userId === userId
export function hasHistoryIdentity() {
  if (session) return true
  try { return !!wx.getStorageSync(mockIdentityKey) || wx.getStorageSync(usedWeChatKey) === true } catch { return false }
}
export async function confirmUserIdentity(allowNew = true) {
  const epoch = identityEpoch
  if (deleting) throw new Error('正在清除数据，请稍后再试')
  const { userId } = await login(allowNew)
  if (epoch !== identityEpoch || !isCurrentUser(userId)) throw new Error('当前使用身份已变更，请重试')
  return userId
}
export async function verifyUserIdentity(allowNew = true) {
  const { data, userId } = await authenticatedRequest<{ userId: string }>('GET', '/v1/me', undefined, undefined, undefined, allowNew)
  if (data?.userId !== userId || !isCurrentUser(userId)) throw new Error('当前使用身份已变更，请重试')
  return userId
}
export async function revokeMyData() {
  if (deleting) throw new Error('正在提交清除请求')
  deleting = true
  identityEpoch++
  try {
    const pendingLogin = loggingIn
    if (pendingLogin) { try { await pendingLogin } catch { /* Any earlier request belongs to the previous epoch. */ } }
    const credential = deletionTarget || await login(false)
    deletionTarget = credential
    // A replay must keep the same credential, even after a network error or a 401.
    const receipt = await call<CleanupReceipt>('DELETE', '/v1/me/data', undefined, undefined, credential)
    if (receipt?.accessRevoked !== true) throw new Error('未能确认服务端已停止访问')
    session = undefined
    deletionTarget = undefined
    let localIdentityCleared = true
    for (const key of [mockIdentityKey, usedWeChatKey]) {
      try { wx.removeStorageSync(key) } catch { localIdentityCleared = false }
    }
    return { receipt, localIdentityCleared, userId: credential.userId }
  } catch (error) {
    if (deletionTarget && error instanceof ConnectionError) throw new Error('未收到清除结果，可联网后重试')
    throw error
  } finally { deleting = false }
}
export const startParse = (input: Input, key: string, owner?: string) => authenticatedRequest<Job>('POST', '/v1/parse-jobs', { ...input, forceRefresh: false }, key, owner)
export const queryJob = (jobId: string, owner: string) => request<Job>('GET', '/v1/parse-jobs/' + encodeURIComponent(jobId), undefined, undefined, owner)
export const cancelJob = (jobId: string, owner: string) => request<Job>('DELETE', '/v1/parse-jobs/' + encodeURIComponent(jobId), undefined, undefined, owner)
export const queryResult = (resultId: string, owner?: string) => request<Result>('GET', '/v1/results/' + encodeURIComponent(resultId), undefined, undefined, owner)
export const queryOwnedResult = (resultId: string, owner?: string) => authenticatedRequest<Result>('GET', '/v1/results/' + encodeURIComponent(resultId), undefined, undefined, owner)
interface EventReceipt { acceptedEventIds: string[]; duplicateEventIds: string[]; rejected: { eventId: string | null; code: string }[] }
export async function reportClientEvents(events: readonly object[], owner: string) {
  return (await authenticatedRequest<EventReceipt>('POST', '/v1/events', { events }, undefined, owner, false)).data
}
export const submitFeedback = (data: FeedbackRequest, key: string) => request<FeedbackCreated>('POST', '/v1/feedbacks', data, key)
export const queryFeedback = (feedbackId: string) => request<FeedbackDetail>('GET', '/v1/feedbacks/' + encodeURIComponent(feedbackId))
export const listFeedback = (cursor: string, owner: string) => authenticatedRequest<FeedbackPage>('GET', '/v1/feedbacks' + (cursor ? '?cursor=' + encodeURIComponent(cursor) : ''), undefined, undefined, owner)
export async function previewAsset(resultId: string, assetId: string, kind: PreviewKind) {
  const { previews } = await request<{ previews: { assetId: string; mode: string; url: string | null; expiresAt: string | null }[] }>('POST', '/v1/results/' + encodeURIComponent(resultId) + '/preview-tickets', { assetIds: [assetId] })
  const entry = previews.find(p => p.assetId === assetId)
  const prefix = origin() + '/media/' + (kind === 'video' ? 'preview' : 'thumbnail') + '/'
  if (!entry?.url || !entry.expiresAt) throw new Error('预览暂不可用')
  const direct = entry.mode === 'direct' && /^https:\/\/[^/@?#]+\//.test(entry.url)
  if (!direct && (entry.mode !== 'relay' || !entry.url.startsWith(prefix))) throw new Error('预览暂不可用')
  const expires = Date.parse(entry.expiresAt)
  if (!Number.isFinite(expires) || expires <= Date.now()) throw new Error('预览地址已过期')
  return { url: !direct && kind === 'image' ? origin() + '/media/image/' + entry.url.slice(prefix.length) : entry.url, expires }
}
export const startBatch = (resultId: string, assetIds: string[], flowId: string, actionId: string, owner: string) => request<BatchAccepted>('POST', '/v1/save-batches', { resultId, assetIds, clientActionId: actionId, flowId }, actionId, owner)
export const queryBatch = (batchId: string, owner: string) => request<Batch>('GET', '/v1/save-batches/' + encodeURIComponent(batchId), undefined, undefined, owner)
export const cancelBatch = (batchId: string, owner: string) => request<Batch>('DELETE', '/v1/save-batches/' + encodeURIComponent(batchId), undefined, undefined, owner)
export const tickets = (batchId: string, itemIds: string[], owner: string) => request<Tickets>('POST', '/v1/save-batches/' + encodeURIComponent(batchId) + '/tickets', { itemIds }, undefined, owner)
const itemRoute = (batchId: string, itemId: string) => '/v1/save-batches/' + encodeURIComponent(batchId) + '/items/' + encodeURIComponent(itemId)
export const retryBatch = (batchId: string, itemIds: string[], key: string, owner: string) => request<BatchAccepted>('POST', '/v1/save-batches/' + encodeURIComponent(batchId) + '/retry', { itemIds }, key, owner)
export type FallbackReason = 'DIRECT_DOWNLOAD_REJECTED' | 'DIRECT_CONTENT_INVALID'
export const fallback = (batchId: string, itemId: string, reason: FallbackReason, owner: string) => request<BatchAccepted>('POST', itemRoute(batchId, itemId) + '/fallback', { reason }, itemId, owner)
const httpsMediaURL = (address: string) => /^https:\/\/[^/@?#\s]+\//i.test(address)
export async function copyDownloadURL(batchId: string, itemId: string, owner: string) {
  const { url, expiresAt } = await request<{ url: string; expiresAt: string }>('POST', itemRoute(batchId, itemId) + '/copy-link', undefined, undefined, owner)
  if ((!httpsMediaURL(url) && !url.startsWith(origin() + '/media/mock-source/copy/')) || !(Date.parse(expiresAt) > Date.now())) throw new Error('下载地址已过期')
  return url
}
export function validDownloadTicket(ticket: DownloadTicket) {
  return ticket.deliveryMode === 'direct'
    ? httpsMediaURL(ticket.url) || ticket.url.startsWith(origin() + '/media/mock-source/direct/')
    : ticket.deliveryMode === 'relay' && ticket.url.startsWith(origin() + '/media/download/')
}
