import { APIError, Input, cancelJob, id, isCurrentUser, queryJob, startParse, wait } from './api'

export class ParseFailure extends APIError {
  constructor(code: string, message: string, readonly traceId: string) { super(code, message) }
}

export async function parseFlow(input: Input, waitSeconds: number, active: () => boolean,
  onJob: (jobId: string, userId: string) => void, owner?: string) {
  const started = await startParse(input, id(), owner)
  const { userId } = started
  let job = started.data
  if (!active()) {
    if (isCurrentUser(userId)) await cancelJob(job.jobId, userId)
    return null
  }
  onJob(job.jobId, userId)
  const delay = job.pollAfterMs
  const end = Math.min(Date.parse(job.deadlineAt), Date.now() + waitSeconds * 1000)
  while (job.status === 'QUEUED' || job.status === 'RUNNING') {
    await wait(delay)
    if (!active()) return null
    if (Date.now() >= end) {
      if (isCurrentUser(userId)) await cancelJob(job.jobId, userId)
      throw new Error('解析等待超时，请重试')
    }
    job = await queryJob(job.jobId, userId)
  }
  if (!active()) return null
  if (job.status !== 'SUCCEEDED' || !job.resultId) {
    throw new ParseFailure(job.error?.code || '', job.error?.message || '未能取得解析结果', job.error?.traceId || '')
  }
  return { resultId: job.resultId, source: job.historySource, userId, jobId: job.jobId }
}
