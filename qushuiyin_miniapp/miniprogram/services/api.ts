const LOCAL_ORIGIN = 'http://127.0.0.1:18080'

type Method = 'GET' | 'POST' | 'DELETE'
interface Envelope<T> { code: string; message: string; requestId: string; data: T; error?: { details?: { candidates?: Candidate[] } } }
export interface Candidate { url: string; displayHost: string; platform: string }
export interface PublicConfig {
  privacyVersion: string
  features: { mock: boolean; devLogin: boolean }
  limits: { max_share_text_chars: number; max_image_batch_count: number; client_parse_wait_seconds: number; client_download_timeout_seconds: number; poll_interval_ms: number }
  platforms: { platform: string; status: string; videoEnabled: boolean; imageEnabled: boolean }[]
}
interface Session { userId: string; token: string; expiresAt: string }
export interface Job { jobId: string; status: string; pollAfterMs: number; deadlineAt: string; resultId?: string; error?: { message: string } }
export interface Asset { assetId: string; type: string; order: number }
export interface Result { resultId: string; platform: string; mediaType: string; title: string | null; description: string | null; assets: Asset[]; assetCount: number; cover: Asset | null; capabilities: { canPreview: boolean } }
type PreviewKind = 'video' | 'thumbnail' | 'image'
export interface BatchItem { itemId: string; order: number; prepareStatus: string; copyLinkAvailable: boolean; error: { message: string; retryable: boolean } | null }
export interface Batch { batchId: string; status: string; pollAfterMs: number; items: BatchItem[] }
interface DownloadTicket { itemId: string; deliveryMode: 'relay' | 'direct'; url: string }
interface Tickets { tickets: DownloadTicket[] }
export interface Input { shareText: string; selectedUrl: string | null; source: 'paste' | 'manual'; flowId: string }
export class APIError extends Error {
  constructor(readonly code: string, message: string, readonly candidates: Candidate[] = []) { super(message) }
}
let session: Session | undefined
let loggingIn: Promise<Session> | undefined
let cached: PublicConfig | undefined
export const id = () => Date.now().toString(36) + '-' + Math.random().toString(36).slice(2)
export const wait = (ms: number) => new Promise<void>(resolve => setTimeout(resolve, ms))
function origin() {
  if (wx.getAccountInfoSync().miniProgram.envVersion !== 'develop') throw new Error('本版本尚未开放')
  return LOCAL_ORIGIN
}
function call<T>(method: Method, path: string, data?: object, key?: string, authenticated = false): Promise<T> {
  return new Promise((resolve, reject) => {
    const header: Record<string, string> = {}
    if (authenticated && session) header.Authorization = 'Bearer ' + session.token
    if (key) header['Idempotency-Key'] = key
    wx.request<Envelope<T>>({ url: origin() + path, method, header, data,
      success(res) {
        const e = res.data
        if (e?.code === 'OK') resolve(e.data)
        else reject(new APIError(e?.code || 'SERVICE_BUSY', e?.message || '本地服务暂不可用', e?.error?.details?.candidates))
      },
      fail() { reject(new Error('无法连接本地服务，请检查服务是否启动')) },
    })
  })
}
export async function loadConfig() {
  cached = await call<PublicConfig>('GET', '/v1/config')
  return cached
}
export async function login() {
  if (session && Date.parse(session.expiresAt) > Date.now()) return
  if (!loggingIn) loggingIn = (async () => {
    const config = cached || await loadConfig()
    if (!config.features.mock || !config.features.devLogin) throw new Error('本地开发登录未开启')
    let name = wx.getStorageSync('mock-dev-user') as string
    if (!name) { name = 'device-' + id(); wx.setStorageSync('mock-dev-user', name) }
    return call<Session>('POST', '/v1/sessions/wechat', { code: 'dev:' + name, clientVersion: 'm1', privacyVersion: config.privacyVersion })
  })()
  try { session = await loggingIn } finally { loggingIn = undefined }
}
async function request<T>(method: Method, path: string, data?: object, key?: string): Promise<T> {
  await login()
  try { return await call<T>(method, path, data, key, true) } catch (e) {
    if (!(e instanceof APIError) || e.code !== 'SESSION_EXPIRED') throw e
    session = undefined
    await login()
    return call<T>(method, path, data, key, true)
  }
}
export const startParse = (input: Input, key: string) => request<Job>('POST', '/v1/parse-jobs', { ...input, forceRefresh: false }, key)
export const queryJob = (jobId: string) => request<Job>('GET', '/v1/parse-jobs/' + encodeURIComponent(jobId))
export const cancelJob = (jobId: string) => request<Job>('DELETE', '/v1/parse-jobs/' + encodeURIComponent(jobId))
export const queryResult = (resultId: string) => request<Result>('GET', '/v1/results/' + encodeURIComponent(resultId))
export async function previewAsset(resultId: string, assetId: string, kind: PreviewKind) {
  const { previews } = await request<{ previews: { assetId: string; mode: string; url: string | null; expiresAt: string | null }[] }>('POST', '/v1/results/' + encodeURIComponent(resultId) + '/preview-tickets', { assetIds: [assetId] })
  const entry = previews.find(p => p.assetId === assetId)
  const prefix = origin() + '/media/' + (kind === 'video' ? 'preview' : 'thumbnail') + '/'
  if (entry?.mode !== 'relay' || !entry.url?.startsWith(prefix) || !entry.expiresAt) throw new Error('预览暂不可用')
  const expires = Date.parse(entry.expiresAt)
  if (!Number.isFinite(expires) || expires <= Date.now()) throw new Error('预览地址已过期')
  return { url: kind === 'image' ? origin() + '/media/image/' + entry.url.slice(prefix.length) : entry.url, expires }
}
export const startBatch = (resultId: string, assetIds: string[], flowId: string, actionId: string) => request<Batch>('POST', '/v1/save-batches', { resultId, assetIds, clientActionId: actionId, flowId }, actionId)
export const queryBatch = (batchId: string) => request<Batch>('GET', '/v1/save-batches/' + encodeURIComponent(batchId))
export const cancelBatch = (batchId: string) => request<Batch>('DELETE', '/v1/save-batches/' + encodeURIComponent(batchId))
export const tickets = (batchId: string, itemIds: string[]) => request<Tickets>('POST', '/v1/save-batches/' + encodeURIComponent(batchId) + '/tickets', { itemIds })
const itemRoute = (batchId: string, itemId: string) => '/v1/save-batches/' + encodeURIComponent(batchId) + '/items/' + encodeURIComponent(itemId)
export const retryBatch = (batchId: string, itemIds: string[], key: string) => request<Batch>('POST', '/v1/save-batches/' + encodeURIComponent(batchId) + '/retry', { itemIds }, key)
export const fallback = (batchId: string, itemId: string, key: string) => request<Batch>('POST', itemRoute(batchId, itemId) + '/fallback', { reason: 'DIRECT_DOWNLOAD_REJECTED' }, key)
export async function copyDownloadURL(batchId: string, itemId: string) {
  const { url, expiresAt } = await request<{ url: string; expiresAt: string }>('POST', itemRoute(batchId, itemId) + '/copy-link')
  if (!url.startsWith(origin() + '/media/mock-source/copy/') || !(Date.parse(expiresAt) > Date.now())) throw new Error('下载地址已过期')
  return url
}
export class DirectRejected extends Error {}
export function downloadSample(ticket: DownloadTicket, timeout: number, attach: (task: WechatMiniprogram.DownloadTask) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    const direct = ticket.deliveryMode === 'direct'
    if (!ticket.url.startsWith(origin() + (direct ? '/media/mock-source/direct/' : '/media/download/'))) { reject(new Error('下载地址无效')); return }
    const task = wx.downloadFile({ url: ticket.url, timeout,
      success(res) {
        if (res.statusCode === 200 && res.tempFilePath) resolve()
        else if (direct && res.statusCode === 403) reject(new DirectRejected())
        else reject(new Error('样本下载失败，请重试'))
      },
      fail(err) {
        if (direct && /url not in domain list|domain.*not configured/i.test(err.errMsg)) reject(new DirectRejected())
        else reject(new Error('下载中断，请重试'))
      },
    })
    attach(task)
  })
}
