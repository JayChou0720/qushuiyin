const LOCAL_ORIGIN = 'http://127.0.0.1:18080'

type Method = 'GET' | 'POST' | 'DELETE'
interface Envelope<T> { code: string; message: string; requestId: string; data: T; error?: { details?: { candidates?: Candidate[] } } }
export interface Candidate { url: string; displayHost: string; platform: string }
export interface PublicConfig {
  privacyVersion: string
  features: { mock: boolean; devLogin: boolean }
  limits: { max_share_text_chars: number; max_image_batch_count: number; client_parse_wait_seconds: number; client_download_timeout_seconds: number }
  platforms: { platform: string; status: string; videoEnabled: boolean; imageEnabled: boolean }[]
}
interface Session { userId: string; token: string; expiresAt: string }
export interface Job { jobId: string; status: string; pollAfterMs: number; deadlineAt: string; resultId?: string; error?: { message: string } }
export interface Asset { assetId: string; type: string; order: number }
export interface Result { resultId: string; platform: string; mediaType: string; title: string | null; description: string | null; assets: Asset[]; assetCount: number; cover: Asset | null }
interface Batch { batchId: string; status: string; pollAfterMs: number; items: { itemId: string; sizeBytes: number }[] }
interface Tickets { tickets: { itemId: string; url: string }[] }
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
export const startBatch = (resultId: string, assetIds: string[], flowId: string, actionId: string) => request<Batch>('POST', '/v1/save-batches', { resultId, assetIds, clientActionId: actionId, flowId }, actionId)
export const queryBatch = (batchId: string) => request<Batch>('GET', '/v1/save-batches/' + encodeURIComponent(batchId))
export const cancelBatch = (batchId: string) => request<Batch>('DELETE', '/v1/save-batches/' + encodeURIComponent(batchId))
export const tickets = (batchId: string, itemIds: string[]) => request<Tickets>('POST', '/v1/save-batches/' + encodeURIComponent(batchId) + '/tickets', { itemIds })
export function downloadSample(url: string, timeout: number, attach: (task: WechatMiniprogram.DownloadTask) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    if (!url.startsWith(origin() + '/media/download/')) { reject(new Error('下载地址无效')); return }
    const task = wx.downloadFile({ url, timeout,
      success(res) { if (res.statusCode === 200 && res.tempFilePath) resolve(); else reject(new Error('样本下载失败，请重试')) },
      fail() { reject(new Error('下载中断，请重试')) },
    })
    attach(task)
  })
}
