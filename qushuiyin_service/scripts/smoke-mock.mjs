import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'

const base = 'http://127.0.0.1:18080'
const video = 'https://www.douyin.com/video/10001'
const gallery = 'https://www.xiaohongshu.com/explore/10002'
const failed = 'https://www.douyin.com/video/10003'
const request = async (method, path, { data, token, key, expect = 200, code = 'OK' } = {}) => {
  const headers = { 'Content-Type': 'application/json' }
  if (token) headers.Authorization = 'Bearer ' + token
  if (key) headers['Idempotency-Key'] = key
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
const createSession = () => request('POST','/v1/sessions/wechat',{data:{code:'dev:smoke-'+randomUUID(),clientVersion:'smoke',privacyVersion:config.privacyVersion}})
const { token } = await createSession()
const other = (await createSession()).token
const input = shareText => ({ shareText, selectedUrl: null, source: 'manual', forceRefresh: false, flowId: randomUUID() })
const create = (data, key = randomUUID()) => request('POST','/v1/parse-jobs',{token,data,key,expect:202})
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
const result = await request('GET',resultPath(read),{token})
assert.equal(result.mediaType,'video')
assert.equal(result.assets[0].previewUrl,null)
assert.ok(!JSON.stringify(result).includes('media.example.test'))
await request('GET',resultPath(read),{token:other,expect:404,code:'RESOURCE_NOT_FOUND'})

const makeBatch=async (res,ids)=> {
  const data={resultId:res.resultId,assetIds:ids,clientActionId:randomUUID(),flowId:randomUUID()}
  const k=randomUUID()
  const b=await request('POST','/v1/save-batches',{token,data,key:k,expect:202})
  assert.equal((await request('POST','/v1/save-batches',{token,data,key:k,expect:202})).batchId,b.batchId)
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
await request('DELETE','/v1/sessions/current',{token})
assert.equal((await fetch(picTickets.tickets[0].url)).status,403)
await request('GET',jobPath(first),{token,expect:401,code:'SESSION_EXPIRED'})
console.log('Mock API smoke passed: video, gallery, error, owner isolation, idempotency, ticket rejection and session revocation.')
