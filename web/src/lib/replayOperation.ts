import type { ReplayRecord, ReplayRequest, ReplaySource } from './types'

export interface ReplayOperationState {
  phase: 'idle' | 'submitting' | 'unknown' | 'accepted'
  record: ReplayRecord | null
  sourceTrace: string | null
  source: ReplaySource | null
  busy: boolean
  error: string
}

interface ReplayAPI {
  create(request: ReplayRequest): Promise<ReplayRecord>
  list(): Promise<ReplayRecord[]>
  get(id: string): Promise<ReplayRecord>
  cancel(id: string): Promise<ReplayRecord>
  definiteRejection(error: unknown): boolean
}

export function initialReplayOperation(): ReplayOperationState {
  return { phase: 'idle', record: null, sourceTrace: null, source: null, busy: false, error: '' }
}

function frozenRequest(request: ReplayRequest): ReplayRequest {
  const copy = { ...request }
  if (request.headers) copy.headers = Object.freeze({ ...request.headers })
  if (request.credentials) copy.credentials = request.credentials.map(item => Object.freeze({ ...item }))
  if (copy.credentials) Object.freeze(copy.credentials)
  return Object.freeze(copy)
}

function newerRecord(previous: ReplayRecord | null, incoming: ReplayRecord): ReplayRecord {
  // A slow list/poll response must not turn a completed action back into a
  // running action. Replay state has a single terminal transition.
  return previous?.id === incoming.id && previous.state !== 'running' && incoming.state === 'running' ? previous : incoming
}

export function reconcileReplayRecord(selected: ReplayRecord | null, current: ReplayRecord | null, history: ReplayRecord[]): ReplayRecord | null {
  if (!selected) return null
  let latest = selected
  for (const candidate of [...history, ...(current ? [current] : [])]) {
    // Selection identifies one source/result pair, not whichever operation
    // happens to be current after the next run or a navigation change.
    if (candidate.id !== selected.id || candidate.trace_id !== selected.trace_id || candidate.replay_of !== selected.replay_of || candidate.source !== selected.source) continue
    latest = newerRecord(latest, candidate)
  }
  return latest
}

/** One server operation, independent of the editor's current source/draft.
 * The immutable submitted request is private and retained only while its
 * acceptance is unknown. Public state never contains credentials or bodies.
 */
export class ReplayOperation {
  private state = initialReplayOperation()
  private attempt: ReplayRequest | null = null
  private disposed = false
  private api: ReplayAPI
  private changed: (state: ReplayOperationState) => void

  constructor(api: ReplayAPI, changed: (state: ReplayOperationState) => void) {
    this.api = api
    this.changed = changed
  }

  get canStart(): boolean {
    return !this.disposed && !this.state.busy && !this.attempt && this.state.record?.state !== 'running'
  }

  private publish() {
    if (!this.disposed) this.changed({ ...this.state, record: this.state.record ? { ...this.state.record } : null })
  }

  private accept(record: ReplayRecord) {
    this.state.record = newerRecord(this.state.record, record)
    this.state.sourceTrace = record.replay_of
    this.state.source = record.source
    this.state.phase = 'accepted'
    this.state.error = ''
    this.attempt = null
  }

  private rejected(error: unknown, recovering: boolean) {
    if (this.api.definiteRejection(error) && !recovering) {
      this.attempt = null
      this.state.phase = 'idle'
      this.state.error = `无法重放：${String(error)}`
    } else {
      // Even a later 404/503 cannot disprove an earlier ambiguous acceptance
      // (the source may have been removed). Keep the original attempt/key.
      this.state.phase = 'unknown'
      this.state.error = `发送结果尚未确认：${String(error)}。请恢复本次重放，勿重新发起。`
    }
  }

  async start(request: ReplayRequest): Promise<void> {
    if (!this.canStart) return
    if (!request.idempotency_key) throw new Error('Replay requires an idempotency key')
    this.attempt = frozenRequest(request)
    this.state = { phase: 'submitting', record: null, sourceTrace: request.trace_id, source: request.source, busy: true, error: '' }
    this.publish()
    try {
      const record = await this.api.create(this.attempt)
      if (!this.disposed) this.accept(record)
    } catch (error) {
      if (!this.disposed) this.rejected(error, false)
    } finally {
      this.state.busy = false
      this.publish()
    }
  }

  async recover(retry = false): Promise<void> {
    if (this.disposed || this.state.busy || this.state.phase !== 'unknown' || !this.attempt) return
    const attempt = this.attempt
    this.state.busy = true
    this.state.error = ''
    this.publish()
    try {
      const records = await this.api.list()
      if (this.disposed) return
      const record = records.find(item => item.idempotency_key === attempt.idempotency_key)
      if (record) this.accept(record)
      else if (retry) {
        try {
          const accepted = await this.api.create(attempt)
          if (!this.disposed) this.accept(accepted)
        } catch (error) {
          if (!this.disposed) this.rejected(error, true)
        }
      } else {
        this.state.error = '暂未找到本次重放。可以再次查询，或使用同一次提交重试；编辑后的草稿不会替换这次提交。'
      }
    } catch (error) {
      if (!this.disposed) this.state.error = `暂时无法查询重放记录：${String(error)}。原次提交仍保留，请恢复连接后重试。`
    } finally {
      this.state.busy = false
      this.publish()
    }
  }

  observe(records: ReplayRecord[], sourceTrace: string | null): void {
    if (this.disposed || this.state.busy) return
    const current = this.state.record && records.find(item => item.id === this.state.record?.id)
    if (current) this.state.record = newerRecord(this.state.record, current)
    if (!this.attempt && this.state.record?.state !== 'running') {
      const running = records.find(item => item.replay_of === sourceTrace && item.state === 'running')
      if (running) this.accept(running)
    }
    this.publish()
  }

  async refresh(): Promise<void> {
    const record = this.state.record
    if (this.disposed || this.state.busy || record?.state !== 'running') return
    try {
      const fresh = await this.api.get(record.id)
      if (this.disposed || this.state.record?.id !== record.id) return
      this.state.record = newerRecord(this.state.record, fresh)
      this.state.error = ''
    } catch (error) {
      if (!this.disposed && this.state.record?.id === record.id) this.state.error = `更新重放状态失败：${String(error)}。仍保留最后已知状态，可重试或取消。`
    }
    this.publish()
  }

  async cancel(record = this.state.record): Promise<ReplayRecord | null> {
    if (this.disposed || this.state.busy || record?.state !== 'running') return null
    this.state.busy = true
    this.state.error = ''
    this.publish()
    let result: ReplayRecord | null = null
    try {
      result = await this.api.cancel(record.id)
    } catch (error) {
      // A completion racing with cancellation returns 409. Read the terminal
      // result before claiming failure or a successful manual cancellation.
      try {
        const latest = await this.api.get(record.id)
        if (latest.state !== 'running') result = latest
        else this.state.error = `取消未确认：${String(error)}。请求仍在运行，请重试。`
      } catch {
        this.state.error = `取消结果尚未确认：${String(error)}。请重试查询状态。`
      }
    } finally {
      if (!this.disposed && result && this.state.record?.id === record.id) this.state.record = newerRecord(this.state.record, result)
      this.state.busy = false
      this.publish()
    }
    return result
  }

  dispose() {
    this.disposed = true
    this.attempt = null
  }
}
