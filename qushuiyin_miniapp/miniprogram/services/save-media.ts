import { DownloadTicket, FallbackReason, validDownloadTicket } from './api'
import { releaseTemporaryMedia } from './media-cache'

type SaveErrorCode = FallbackReason | 'CLIENT_NETWORK_ERROR' | 'CLIENT_STORAGE_LOW' | 'CLIENT_DOWNLOAD_FAILED' |
  'PRIVACY_DECLINED' | 'ALBUM_PERMISSION_DENIED' | 'SYSTEM_PERMISSION_DENIED' | 'ALBUM_SAVE_FAILED'

export class SaveMediaError extends Error {
  constructor(readonly code: SaveErrorCode, message: string) { super(message) }
  get stopsBatch() {
    return ['CLIENT_NETWORK_ERROR', 'CLIENT_STORAGE_LOW', 'PRIVACY_DECLINED', 'ALBUM_PERMISSION_DENIED', 'SYSTEM_PERMISSION_DENIED'].includes(this.code)
  }
}

export interface LocalMedia { path: string; bytes: number; mode: DownloadTicket['deliveryMode'] }

type PrivacyDecision = { event: 'agree'; buttonId: string } | { event: 'disagree' }
type SavePlatform = typeof wx & {
  requirePrivacyAuthorize?: (options: { success(): void; fail(): void }) => void
  onNeedPrivacyAuthorization?: (listener: (resolve: (result: PrivacyDecision) => void) => void) => void
  getAppAuthorizeSetting?: () => { albumAuthorized?: 'authorized' | 'denied' | 'not determined' }
  openAppAuthorizeSetting?: (options: { fail(): void }) => void
}

let askPrivacy: (() => void) | undefined
let privacyDecision: ((result: PrivacyDecision) => void) | undefined

export function watchSavePrivacy() {
  (wx as SavePlatform).onNeedPrivacyAuthorization?.(resolve => {
    if (!askPrivacy) { resolve({ event: 'disagree' }); return }
    privacyDecision = resolve
    askPrivacy()
  })
}

export function respondToSavePrivacy(agree: boolean) {
  const resolve = privacyDecision
  privacyDecision = undefined
  resolve?.(agree ? { event: 'agree', buttonId: 'save-privacy-agree' } : { event: 'disagree' })
}

const privacyDenied = () => new SaveMediaError('PRIVACY_DECLINED', '请先同意微信隐私保护指引，再保存到相册')
const albumDenied = () => new SaveMediaError('ALBUM_PERMISSION_DENIED', '请在小程序设置中允许保存到相册')
const systemDenied = () => new SaveMediaError('SYSTEM_PERMISSION_DENIED', '请在系统设置中允许微信访问相册')
const storageLow = () => new SaveMediaError('CLIENT_STORAGE_LOW', '本机存储空间不足，请清理空间后继续')
const networkError = () => new SaveMediaError('CLIENT_NETWORK_ERROR', '网络已断开，请联网后继续')
const systemAlbumDenied = () => (wx as SavePlatform).getAppAuthorizeSetting?.().albumAuthorized === 'denied'

export async function ensureAlbumAccess(showPrivacy: () => void, active: () => boolean) {
  const requirePrivacy = (wx as SavePlatform).requirePrivacyAuthorize
  if (requirePrivacy) {
    askPrivacy = showPrivacy
    try { await new Promise<void>((resolve, reject) => requirePrivacy.call(wx, { success: resolve, fail: () => reject(privacyDenied()) })) }
    finally { askPrivacy = undefined; privacyDecision = undefined }
  }
  if (!active()) return
  if (systemAlbumDenied()) throw systemDenied()
  const granted = await new Promise<boolean | undefined>((resolve, reject) => wx.getSetting({
    success: result => resolve(result.authSetting['scope.writePhotosAlbum']), fail: () => reject(albumDenied()),
  }))
  if (!active() || granted === true) return
  if (granted === false) throw albumDenied()
  await new Promise<void>((resolve, reject) => wx.authorize({ scope: 'scope.writePhotosAlbum', success: () => resolve(),
    fail: () => reject(systemAlbumDenied() ? systemDenied() : albumDenied()),
  }))
}

export function openAlbumSettings(fail: () => void) {
  const api = wx as SavePlatform
  if (systemAlbumDenied() && api.openAppAuthorizeSetting) api.openAppAuthorizeSetting({ fail })
  else wx.openSetting({ fail })
}

export async function ensureSaveNetwork() {
  await new Promise<void>((resolve, reject) => wx.getNetworkType({
    success: ({ networkType }) => networkType === 'none' ? reject(networkError()) : resolve(), fail: () => resolve(),
  }))
}

function platformError(message: string, writing: boolean): SaveMediaError {
  if (/no space|enospc|storage.*(full|limit)|maximum size of the file storage/i.test(message)) return storageLow()
  if (writing && /permission|auth deny|auth denied/i.test(message)) return systemAlbumDenied() ? systemDenied() : albumDenied()
  if (!writing && /offline|network.*(disconnect|unavailable|not available)|err_internet_disconnected/i.test(message)) return networkError()
  return writing ? new SaveMediaError('ALBUM_SAVE_FAILED', '未能写入相册，可重试') : new SaveMediaError('CLIENT_DOWNLOAD_FAILED', '下载失败，可重试')
}

export function fileSize(path: string) {
  try { return (wx.getFileSystemManager().statSync(path) as WechatMiniprogram.Stats).size }
  catch { return 0 }
}

export function downloadForAlbum(ticket: DownloadTicket, timeout: number, maximum: number, expected: number | null,
  attach: (task: WechatMiniprogram.DownloadTask) => void): Promise<LocalMedia> {
  return new Promise((resolve, reject) => {
    const direct = ticket.deliveryMode === 'direct'
    const invalid = () => new SaveMediaError(direct ? 'DIRECT_CONTENT_INVALID' : 'CLIENT_DOWNLOAD_FAILED', '下载文件不完整或超过大小限制')
    if (!validDownloadTicket(ticket)) { reject(new SaveMediaError('CLIENT_DOWNLOAD_FAILED', '下载地址无效')); return }
    let tooLarge = false
    const task = wx.downloadFile({ url: ticket.url, timeout,
      success(res) {
        const path = res.tempFilePath
        const bytes = path ? fileSize(path) : 0
        if (!tooLarge && res.statusCode === 200 && bytes > 0 && bytes <= maximum && (!expected || bytes === expected)) {
          resolve({ path, bytes, mode: ticket.deliveryMode }); return
        }
        if (path) void releaseTemporaryMedia(path)
        if (direct && res.statusCode === 403) reject(new SaveMediaError('DIRECT_DOWNLOAD_REJECTED', '平台拒绝直连下载'))
        else if (tooLarge || res.statusCode === 200) reject(invalid())
        else reject(new SaveMediaError('CLIENT_DOWNLOAD_FAILED', '下载失败，可重试'))
      },
      fail(error) {
        if (tooLarge) reject(invalid())
        else if (direct && /domain list|domain.*not configured|不在.*合法域名/i.test(error.errMsg)) reject(new SaveMediaError('DIRECT_DOWNLOAD_REJECTED', '该平台地址暂时无法直连'))
        else reject(platformError(error.errMsg, false))
      },
    })
    task.onProgressUpdate(({ totalBytesWritten }) => {
      if (totalBytesWritten > maximum) { tooLarge = true; task.abort() }
    })
    attach(task)
  })
}

export function saveToAlbum(file: LocalMedia, kind: string) {
  return new Promise<void>((resolve, reject) => {
    const options = { filePath: file.path, success: () => resolve(), fail: (error: { errMsg: string }) => {
      if (file.mode === 'direct' && /format|unsupported|invalid (image|video)|decode|文件格式/i.test(error.errMsg)) {
        reject(new SaveMediaError('DIRECT_CONTENT_INVALID', '直连文件格式无法保存'))
      } else reject(platformError(error.errMsg, true))
    } }
    if (kind === 'video') wx.saveVideoToPhotosAlbum(options)
    else wx.saveImageToPhotosAlbum(options)
  })
}
