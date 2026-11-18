import { APIError, FeedbackCreated, FeedbackRequest, feedbackConfig, id, submitFeedback } from '../../services/api'
import { categories, validId } from './shared'

type InputEvent = WechatMiniprogram.CustomEvent<{ value: string }>
type ChoiceEvent = WechatMiniprogram.CustomEvent<{ value: string[] }>
const size = (text: string) => Array.from(text).length

Page({
  data: {
    categories, category: '', categoryLabel: '选择问题类型', categoryIndex: 0, content: '', contentLength: 0, contact: '',
    traceId: '', includeTrace: false, includeSource: false, sourceUrl: '', minContent: 0, maxContent: 0, maxContact: 0, maxSource: 0,
    ready: false, busy: false, configError: '', categoryError: '', contentError: '', contactError: '', traceError: '', sourceError: '', submitError: '',
    created: null as FeedbackCreated | null,
  },
  gone: false,
  sendKey: '',
  onLoad(query) {
    if (validId(query.traceId)) this.setData({ traceId: query.traceId, includeTrace: true, category: categories[0].value, categoryLabel: categories[0].label })
    void this.loadLimits()
  },
  onUnload() { this.gone = true },
  async loadLimits() {
    this.setData({ configError: '' })
    try {
      const { limits } = await feedbackConfig()
      const { feedback_min_chars: minContent, feedback_max_chars: maxContent, feedback_contact_max_chars: maxContact, max_share_text_chars: maxSource } = limits
      if (![minContent, maxContent, maxContact, maxSource].every(n => Number.isInteger(n) && n > 0) || minContent > maxContent) throw new Error('本地服务未提供反馈设置')
      if (!this.gone) this.setData({ minContent, maxContent, maxContact, maxSource, ready: true })
    } catch (error) { if (!this.gone) this.setData({ configError: (error as Error).message }) }
  },
  choose(e: InputEvent) {
    if (this.data.busy) return
    const categoryIndex = Number(e.detail.value), category = categories[categoryIndex]
    if (!category) return
    this.sendKey = ''
    this.setData({ categoryIndex, category: category.value, categoryLabel: category.label, categoryError: '', submitError: '' })
  },
  editContent(e: InputEvent) {
    if (this.data.busy) return
    this.sendKey = ''
    this.setData({ content: e.detail.value, contentLength: size(e.detail.value), contentError: '', submitError: '' })
  },
  editContact(e: InputEvent) {
    if (this.data.busy) return
    this.sendKey = ''
    this.setData({ contact: e.detail.value, contactError: '', submitError: '' })
  },
  editSource(e: InputEvent) {
    if (this.data.busy) return
    this.sendKey = ''
    this.setData({ sourceUrl: e.detail.value, sourceError: '', submitError: '' })
  },
  toggleSource(e: ChoiceEvent) {
    if (this.data.busy) return
    const includeSource = e.detail.value.includes('source')
    this.sendKey = ''
    this.setData({ includeSource, sourceUrl: includeSource ? this.data.sourceUrl : '', sourceError: '', submitError: '' })
  },
  toggleTrace(e: ChoiceEvent) {
    if (this.data.busy) return
    this.sendKey = ''
    this.setData({ includeTrace: e.detail.value.includes('trace'), traceError: '', submitError: '' })
  },
  contentError(value: string) {
    const count = size(value.trim()), { minContent, maxContent } = this.data
    return count < minContent || count > maxContent ? '请输入 ' + minContent + '–' + maxContent + ' 字的描述' : ''
  },
  blurContent(e: InputEvent) { if (e.detail.value) this.setData({ contentError: this.contentError(e.detail.value) }) },
  async submit() {
    const f = this.data
    if (!f.ready || f.busy || f.created) return
    const content = f.content.trim(), contact = f.contact.trim(), source = f.includeSource ? f.sourceUrl.trim() : ''
    const categoryError = f.category ? '' : '请选择问题类型'
    const contentError = this.contentError(content)
    const contactError = size(contact) > f.maxContact ? '联系方式最多 ' + f.maxContact + ' 字' : ''
    const sourceError = f.includeSource && (!/^https:\/\/[^\s]+$/.test(source) || source.length > f.maxSource) ? '请输入有效的 HTTPS 作品链接' : ''
    this.setData({ categoryError, contentError, contactError, sourceError, traceError: '', submitError: '' })
    if (categoryError || contentError || contactError || sourceError) return
    const payload: FeedbackRequest = { category: f.category, content, includeSource: f.includeSource }
    if (contact) payload.contact = contact
    if (f.includeTrace && f.traceId) payload.traceId = f.traceId
    if (source) payload.sourceUrl = source
    if (!this.sendKey) this.sendKey = id()
    this.setData({ busy: true })
    try {
      const created = await submitFeedback(payload, this.sendKey)
      if (!this.gone) {
        this.sendKey = ''
        this.setData({ created, content: '', contact: '', sourceUrl: '', traceId: '', includeSource: false, includeTrace: false })
        wx.pageScrollTo({ scrollTop: 0, duration: 0 })
      }
    } catch (error) {
      if (this.gone) return
      if (error instanceof APIError && error.code === 'RESOURCE_NOT_FOUND' && payload.traceId) this.setData({ traceError: '诊断编号已失效。取消附带后可以提交。' })
      else if (error instanceof APIError && error.code === 'LINK_NOT_ALLOWED' && source) this.setData({ sourceError: '请检查链接是否来自支持的平台' })
      else this.setData({ submitError: (error as Error).message })
    } finally { if (!this.gone) this.setData({ busy: false }) }
  },
})
