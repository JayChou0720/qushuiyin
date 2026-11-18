import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'

const port = Number(process.argv[2] ?? 18080)
assert.ok(Number.isInteger(port) && port > 0 && port < 65536, 'Expected a loopback API port')
const base = 'http://127.0.0.1:' + port
const video = 'https://www.douyin.com/video/10001'
const gallery = 'https://www.xiaohongshu.com/explore/10002'
const failed = 'https://www.douyin.com/video/10003'
const request = async (method, path, { data, token, key, origin, expect = 200, code = 'OK' } = {}) => {
  const headers = { 'Content-Type': 'application/json' }
  if (token) headers.Authorization = 'Bearer ' + token
  if (key) headers['Idempotency-Key'] = key
  if (origin) headers.Origin = origin
  const response = await fetch(base + path, { method, headers, body: data ? JSON.stringify(data) : undefined, signal: AbortSignal.timeout(8000) })
  assert.equal(response.status, expect, method + ' ' + path)
  const payload = await response.json()
  assert.equal(payload.code, code)
  assert.ok(payload.requestId)
  return payload.data ?? payload.error?.details
}
const config = await request('GET', '/v1/config')
assert.equal(config.features.mock, true)
assert.equal(config.features.devLogin, true)
for (const name of ['history_max_count','history_ttl_days','thumbnail_local_budget_bytes']) {
  assert.ok(Number.isSafeInteger(config.limits[name]) && config.limits[name] > 0, name)
}
const createSession = () => request('POST','/v1/sessions/wechat',{data:{code:'dev:smoke-'+randomUUID(),clientVersion:'smoke',privacyVersion:config.privacyVersion}})
const { token } = await createSession()
const other = (await createSession()).token
const input = shareText => ({ shareText, selectedUrl: null, source: 'manual', forceRefresh: false, flowId: randomUUID() })
const create = (data, key = randomUUID(), owner = token) => request('POST','/v1/parse-jobs',{token:owner,data,key,expect:202})
const jobPath = j => '/v1/parse-jobs/' + j.jobId
const resultPath = j => '/v1/results/' + j.resultId

await request('POST','/v1/parse-jobs',{expect:401,code:'AUTH_REQUIRED'})
await request('POST','/v1/parse-jobs',{token,data:input(''),key:randomUUID(),expect:400,code:'INPUT_EMPTY'})
const multi = await request('POST','/v1/parse-jobs',{token,data:input(video+' '+gallery),key:randomUUID(),expect:422,code:'MULTIPLE_LINKS'})
assert.equal(multi.candidates.length,2)
const original = input('示例分享 🎬 '+video+'。')
const key = randomUUID()
const first = await create(original,key)
assert.equal((await create(original,key)).jobId,first.jobId)
await request('POST','/v1/parse-jobs',{token,data:input(gallery),key,expect:409,code:'IDEMPOTENCY_CONFLICT'})
await request('GET',jobPath(first),{token:other,expect:404,code:'RESOURCE_NOT_FOUND'})
const read = await request('GET',jobPath(first),{token})
assert.equal(read.status,'SUCCEEDED')
assert.equal(read.historySource.url,video)
assert.match(read.historySource.key,/^[0-9a-f]{64}$/)
const signed = gallery + '?signature=x%2By&token=a%3Db'
const signedJob = await create({...input(video + ' ' + signed + '#share'),selectedUrl:signed + '#share',source:'history'})
const signedSource = (await request('GET', jobPath(signedJob), {token})).historySource
assert.equal(signedSource.url,signed + '#share')
const sameLink = await create(input(signed))
assert.equal((await request('GET', jobPath(sameLink), {token})).historySource.key,signedSource.key)
const otherJob = await create(input(signed),randomUUID(),other)
assert.notEqual((await request('GET', jobPath(otherJob), {token:other})).historySource.key,signedSource.key)
const result = await request('GET',resultPath(read),{token})
assert.equal(result.mediaType,'video')
assert.equal(result.assets[0].previewUrl,null)
assert.ok(!JSON.stringify(result).includes('media.example.test'))
await request('GET',resultPath(read),{token:other,expect:404,code:'RESOURCE_NOT_FOUND'})

await request('GET','/admin/v1/me',{token,expect:401,code:'AUTH_REQUIRED'})
await request('POST','/admin/v1/sessions',{data:{username:'unknown-smoke',password:randomUUID()},origin:'https://wrong.invalid',expect:403,code:'ACCESS_DENIED'})
await request('POST','/admin/v1/sessions',{data:{username:'unknown-smoke',password:randomUUID()},origin:base,expect:401,code:'ADMIN_LOGIN_FAILED'})

const previewPath = resultPath(read) + '/preview-tickets'
const previewRequest = {assetIds:[result.assets[0].assetId]}
await request('POST',previewPath,{data:previewRequest,expect:401,code:'AUTH_REQUIRED'})
await request('POST',previewPath,{data:previewRequest,token:other,expect:404,code:'RESOURCE_NOT_FOUND'})
await request('POST',previewPath,{data:{assetIds:[]},token,expect:400,code:'BAD_REQUEST'})
const {previews:[preview]}=await request('POST',previewPath,{token,data:previewRequest})
assert.equal(new URL(preview.url).origin,base)
assert.equal(preview.mode,'relay')
const portion=await fetch(preview.url,{headers:{Range:'bytes=0-11'}})
assert.equal(portion.status,206)
assert.equal(Buffer.from(await portion.arrayBuffer()).subarray(4,8).toString(),'ftyp')
assert.equal((await fetch(preview.url,{headers:{Range:'bytes=0-0,4-8'}})).status,416)
const invalidPreview=new URL(preview.url);invalidPreview.searchParams.set('t','invalid')
assert.equal((await fetch(invalidPreview)).status,403)

const makeBatch=async (res,ids,owner = token)=> {
  const data={resultId:res.resultId,assetIds:ids,clientActionId:randomUUID(),flowId:randomUUID()}
  const k=randomUUID()
  const b=await request('POST','/v1/save-batches',{token:owner,data,key:k,expect:202})
  assert.equal((await request('POST','/v1/save-batches',{token:owner,data,key:k,expect:202})).batchId,b.batchId)
  return '/v1/save-batches/'+b.batchId
}
const batchPath=await makeBatch(result,[result.assets[0].assetId])
const ready=await request('GET',batchPath,{token})
assert.equal(ready.status,'READY')
await request('GET',batchPath,{token:other,expect:404,code:'RESOURCE_NOT_FOUND'})
const ticketReq={itemIds:[ready.items[0].itemId]}
await request('POST',batchPath+'/tickets',{token:other,data:ticketReq,expect:404,code:'RESOURCE_NOT_FOUND'})
const { tickets:[ticket] }=await request('POST',batchPath+'/tickets',{token,data:ticketReq})
assert.equal(new URL(ticket.url).origin,base)
const downloaded=await fetch(ticket.url)
assert.equal(downloaded.status,200)
assert.equal(downloaded.headers.get('content-type'),'video/mp4')
const file=Buffer.from(await downloaded.arrayBuffer())
assert.equal(file.length,ready.items[0].sizeBytes)
assert.equal(file.subarray(4,8).toString(),'ftyp')
const badTicket=new URL(ticket.url);badTicket.searchParams.set('t','invalid')
assert.equal((await fetch(badTicket)).status,403)
await request('DELETE',batchPath,{token})
assert.equal((await fetch(ticket.url)).status,404)

const pictureJob=await request('GET',jobPath(await create({...input(video+' '+gallery),selectedUrl:gallery})),{token})
const pictures=await request('GET',resultPath(pictureJob),{token})
assert.equal(pictures.mediaType,'image')
const picturePreviews=await request('POST',resultPath(pictureJob)+'/preview-tickets',{token,data:{assetIds:pictures.assets.map(a=>a.assetId)}})
assert.equal(picturePreviews.previews.length,2)
const thumbnail=await fetch(picturePreviews.previews[0].url)
assert.equal(thumbnail.headers.get('content-type'),'image/png')
assert.equal(Buffer.from(await thumbnail.arrayBuffer()).subarray(1,4).toString(),'PNG')
const images=await makeBatch(pictures,pictures.assets.map(a=>a.assetId).reverse())
const imageBatch=await request('GET',images,{token})
assert.deepEqual(imageBatch.items.map(a=>a.order),[1,2])
const picTickets=await request('POST',images+'/tickets',{token,data:{itemIds:imageBatch.items.map(a=>a.itemId)}})
assert.equal(picTickets.tickets.length,2)
const picture=await fetch(picTickets.tickets[0].url)
assert.equal(picture.headers.get('content-type'),'image/png')
assert.equal(Buffer.from(await picture.arrayBuffer()).subarray(1,4).toString(),'PNG')
const bad=await request('GET',jobPath(await create(input(failed))),{token})
assert.equal(bad.status,'FAILED')
assert.equal(bad.error.code,'UPSTREAM_UNAVAILABLE')
const cancelled=await request('DELETE',jobPath(await create(input(video))),{token})
assert.equal(cancelled.status,'CANCELLED')

async function checkRecovery(token) {
const recoveryCase = async fixtureId => {
  const job = await request('GET',jobPath(await create(input('https://www.douyin.com/video/'+fixtureId),randomUUID(),token)),{token})
  const res = await request('GET',resultPath(job),{token})
  const path = await makeBatch(res,[res.assets[0].assetId],token)
  return { path, item:(await request('GET',path,{token})).items[0] }
}
const retry = await recoveryCase('10004')
assert.equal(retry.item.prepareStatus,'FAILED')
assert.equal(retry.item.error.code,'PREPARE_TIMEOUT')
const retryData = {itemIds:[retry.item.itemId]}
await request('POST',retry.path+'/retry',{data:retryData,expect:401,code:'AUTH_REQUIRED'})
await request('POST',retry.path+'/retry',{data:retryData,token:other,expect:404,code:'RESOURCE_NOT_FOUND'})
await request('POST',retry.path+'/retry',{data:{itemIds:[retry.item.itemId,retry.item.itemId]},token,key:randomUUID(),expect:400,code:'BAD_REQUEST'})
const retryKey = randomUUID()
await request('POST',retry.path+'/retry',{data:retryData,token,key:retryKey,expect:202})
assert.equal((await request('GET',retry.path,{token})).items[0].attempt,2)
await request('POST',retry.path+'/retry',{data:retryData,token,key:retryKey,expect:202})
const recovered = (await request('GET',retry.path,{token})).items[0]
assert.equal(recovered.prepareStatus,'READY')
assert.equal(recovered.attempt,2)
await request('POST',retry.path+'/retry',{data:retryData,token,key:randomUUID(),expect:409,code:'FILE_NOT_READY'})

const direct = await recoveryCase('10005')
const directPath = direct.path+'/items/'+direct.item.itemId
assert.equal(direct.item.deliveryMode,'direct')
const {tickets:[firstDirect]}=await request('POST',direct.path+'/tickets',{token,data:{itemIds:[direct.item.itemId]}})
assert.equal((await fetch(firstDirect.url)).status,403)
const fallbackData={reason:'DIRECT_DOWNLOAD_REJECTED'}
await request('POST',directPath+'/fallback',{data:fallbackData,expect:401,code:'AUTH_REQUIRED'})
await request('POST',directPath+'/fallback',{token:other,data:fallbackData,key:randomUUID(),expect:404,code:'RESOURCE_NOT_FOUND'})
await request('POST',directPath+'/fallback',{token,data:{reason:'invalid'},key:randomUUID(),expect:400,code:'BAD_REQUEST'})
const fallbackKey=randomUUID()
await request('POST',directPath+'/fallback',{token,data:fallbackData,key:fallbackKey,expect:202})
await request('POST',directPath+'/fallback',{token,data:{reason:'DIRECT_CONTENT_INVALID'},key:fallbackKey,expect:409,code:'IDEMPOTENCY_CONFLICT'})
const relay=(await request('GET',direct.path,{token})).items[0]
assert.equal(relay.deliveryMode,'relay')
assert.equal(relay.attempt,2)
await request('POST',directPath+'/fallback',{token,data:fallbackData,key:fallbackKey,expect:202})
await request('POST',directPath+'/fallback',{token,data:fallbackData,key:randomUUID(),expect:409,code:'FALLBACK_NOT_ALLOWED'})
assert.equal((await fetch(firstDirect.url)).status,404)
const relayed=await request('POST',direct.path+'/tickets',{token,data:{itemIds:[relay.itemId]}})
assert.equal((await fetch(relayed.tickets[0].url)).status,200)

const copy = await recoveryCase('10006')
const copyPath=copy.path+'/items/'+copy.item.itemId
await request('POST',copyPath+'/copy-link',{token,expect:409,code:'FILE_NOT_READY'})
await request('POST',copyPath+'/fallback',{token,data:fallbackData,key:randomUUID(),expect:202})
const failedItem=(await request('GET',copy.path,{token})).items[0]
assert.equal(failedItem.prepareStatus,'FAILED')
assert.equal(failedItem.copyLinkAvailable,true)
await request('POST',copyPath+'/copy-link',{expect:401,code:'AUTH_REQUIRED'})
await request('POST',copyPath+'/copy-link',{token:other,expect:404,code:'RESOURCE_NOT_FOUND'})
const copied=await request('POST',copyPath+'/copy-link',{token})
assert.equal(new URL(copied.url).origin,base)
assert.equal((await fetch(copied.url)).status,200)
await request('DELETE',copy.path,{token})
assert.equal((await fetch(copied.url)).status,404)
await request('DELETE','/v1/sessions/current',{token})
}
await checkRecovery((await createSession()).token)

const feedbackPath = '/v1/feedbacks'
const feedback = {category:'parse_failed',content:'本地反馈接口验收',contact:'mock-feedback@example.invalid',traceId:bad.error.traceId,includeSource:false}
await request('POST',feedbackPath,{token,data:{...feedback,sourceUrl:video},key:randomUUID(),expect:400,code:'BAD_REQUEST'})
await request('POST',feedbackPath,{token:other,data:feedback,key:randomUUID(),expect:404,code:'RESOURCE_NOT_FOUND'})
const feedbackKey = randomUUID()
const submitted = await request('POST',feedbackPath,{token,data:feedback,key:feedbackKey,expect:201})
assert.deepEqual(await request('POST',feedbackPath,{token,data:feedback,key:feedbackKey,expect:201}),submitted)
await request('POST',feedbackPath,{token,data:{...feedback,content:'与首次不同的反馈'},key:feedbackKey,expect:409,code:'IDEMPOTENCY_CONFLICT'})
const detailPath = feedbackPath+'/'+submitted.feedbackId
const detail = await request('GET',detailPath,{token})
assert.equal(detail.content,feedback.content)
assert.equal(detail.contact,'***')
assert.equal('sourceUrl' in detail,false)
await request('GET',detailPath,{token:other,expect:404,code:'RESOURCE_NOT_FOUND'})
assert.deepEqual((await request('GET',feedbackPath,{token:other})).items,[])
await request('POST',feedbackPath,{token,data:{...feedback,includeSource:true,sourceUrl:video},key:randomUUID(),expect:201})
const page = await request('GET',feedbackPath+'?limit=1',{token})
assert.equal(page.hasMore,true)
const next = await request('GET',feedbackPath+'?limit=1&cursor='+page.nextCursor,{token})
assert.equal(next.hasMore,false)
assert.notEqual(page.items[0].id,next.items[0].id)

const e = {eventId:randomUUID(),name:'result_viewed',flowId:original.flowId,jobId:first.jobId,stage:'result',outcome:'succeeded',clientTime:new Date(),clientVersion:'smoke',osFamily:'devtools'}
const event = (value,owner=token) => request('POST','/v1/events',{token:owner,data:{events:[value]}})
const impersonated = {...e,eventId:randomUUID(),name:'parse_job_finished'}
const privateField = {...e,eventId:randomUUID(),sourceUrl:video}
const mixed = await request('POST','/v1/events',{token,data:{events:[e,e,impersonated,privateField]}})
assert.deepEqual(mixed.acceptedEventIds,[e.eventId])
assert.deepEqual(mixed.duplicateEventIds,[e.eventId])
assert.deepEqual(mixed.rejected,[impersonated,privateField].map(x=>({eventId:x.eventId,code:'BAD_REQUEST'})))
assert.deepEqual((await event(e,other)).rejected,[{eventId:e.eventId,code:'RESOURCE_NOT_FOUND'}])

const dataPath='/v1/me/data'
await request('DELETE',dataPath,{expect:401,code:'AUTH_REQUIRED'})
const cleanup=await request('DELETE',dataPath,{token})
assert.equal(cleanup.accessRevoked,true)
assert.equal(cleanup.cleanupStatus,'scheduled')
assert.ok(Date.parse(cleanup.cleanupDeadlineAt)>Date.now())
await request('DELETE',dataPath,{token,expect:401,code:'SESSION_EXPIRED'})
await request('GET',detailPath,{token,expect:401,code:'SESSION_EXPIRED'})
await request('GET',feedbackPath,{token:other})
await request('DELETE',dataPath,{token:other})

assert.equal((await fetch(picTickets.tickets[0].url)).status,403)
assert.equal((await fetch(preview.url)).status,410)
await request('GET',jobPath(first),{token,expect:401,code:'SESSION_EXPIRED'})
console.log('Mock API smoke passed: media, recovery, owner isolation, admin guards, feedback, events, data cleanup and ticket revocation.')
