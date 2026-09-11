import test from 'node:test'
import assert from 'node:assert/strict'
import { findRequests } from '../src/lib/requestFinder.ts'

const log = (trace, duration, overrides = {}) => ({ trace_id: trace, session_id: 'session', model: 'agent-model', path: '/v1/chat/completions', summary: 'tool result', time: '2026-09-11T00:00:00Z', status: 'done', status_code: 200, duration_ms: duration, ...overrides })

test('keyword/status filtering composes without mutating or selecting from the captured list', () => {
  const logs = [log('ok', 10), log('failed', 20, { status: 'error', error: 'Invalid tool result' }), log('pending', 0, { status: 'pending' })]
  assert.deepEqual(findRequests(logs, 'AGENT invalid', 'error', 'newest').map(value => value.trace_id), ['failed'])
  assert.deepEqual(logs.map(value => value.trace_id), ['ok', 'failed', 'pending'])
  assert.equal(findRequests(logs, 'not present', '', 'newest').length, 0)
})

test('duration ordering keeps unfinished/unknown durations last and retains known zero', () => {
  const logs = [log('pending', 0, { status: 'pending' }), log('long', 2000), log('zero', 0), log('short', 2), log('unknown', Number.NaN), log('running', 50, { status: 'running' })]
  assert.deepEqual(findRequests(logs, '', '', 'duration-asc').slice(0, 3).map(value => value.trace_id), ['zero', 'short', 'long'])
  assert.deepEqual(findRequests(logs, '', '', 'duration-desc').slice(0, 3).map(value => value.trace_id), ['long', 'short', 'zero'])
})

test('newest/oldest order is deterministic without assigning chronology as causal evidence', () => {
  const logs = [log('b', 1), log('new', 1, { time: '2026-09-11T00:00:01Z' }), log('a', 1)]
  assert.deepEqual(findRequests(logs, '', '', 'newest').map(value => value.trace_id), ['new', 'a', 'b'])
  assert.deepEqual(findRequests(logs, '', '', 'oldest').map(value => value.trace_id), ['a', 'b', 'new'])
})
