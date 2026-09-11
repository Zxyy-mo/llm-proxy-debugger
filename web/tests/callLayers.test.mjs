import test from 'node:test'
import assert from 'node:assert/strict'
import { requestsInRun, UNASSOCIATED_RUN } from '../src/lib/callLayers.ts'
import { findRequests } from '../src/lib/requestFinder.ts'

const log = (trace, run, extra = {}) => ({ trace_id: trace, session_id: 'one-session', run_id: run, status: 'done', status_code: 200, time: '2026-09-11T01:00:00Z', duration_ms: 1, ...extra })

test('任务筛选保留同轮分支，缺失或冲突请求不会按会话和时间归入任务', () => {
  const logs = [log('branch-a', 'run:one'), log('branch-b', 'run:one'), log('next-turn', 'run:two'), log('missing'), log('conflict', undefined, { run: { state: 'conflict', sources: ['header:X-Run-ID'] } })]
  assert.deepEqual(requestsInRun(logs, 'run:one').map(item => item.trace_id), ['branch-a', 'branch-b'])
  assert.deepEqual(requestsInRun(logs, UNASSOCIATED_RUN).map(item => item.trace_id), ['missing', 'conflict'])
  assert.equal(requestsInRun(logs, ''), logs)
  assert.deepEqual(requestsInRun(logs, 'deleted-run'), [])
  assert.equal(logs.length, 5)
})

test('任务、请求状态和尝试错误的搜索可以组合，重放不会回到来源任务中', () => {
  const logs = [
    log('source', 'run:source', { run: { state: 'explicit', external_id: 'turn-one', sources: ['header:X-Run-ID'] } }),
    log('replay', 'run:replay:action', { run: { state: 'replay', sources: ['gateway:replay'] }, replay: { of: 'source' }, status: 'error', route: { attempts: [{ id: 'attempt-a', provider_id: 'backup', error: 'connection refused' }] } }),
  ]
  assert.deepEqual(findRequests(logs, 'turn-one', '', 'newest').map(item => item.trace_id), ['source'])
  assert.deepEqual(findRequests(requestsInRun(logs, 'run:replay:action'), 'backup refused', 'error', 'newest').map(item => item.trace_id), ['replay'])
  assert.deepEqual(requestsInRun(logs, 'run:source').map(item => item.trace_id), ['source'])
})
