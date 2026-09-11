import test from 'node:test'
import assert from 'node:assert/strict'
import { reconcileReplayRecord, ReplayOperation } from '../src/lib/replayOperation.ts'
import { APIError, createReplay } from '../src/lib/api.ts'

const record = (overrides = {}) => ({
  id: 'run-one', trace_id: 'result-one', replay_of: 'source-one', source: 'outgoing', modified: true,
  state: 'running', created_at: '2026-09-11T00:00:00Z', timeout_seconds: 600, idempotency_key: 'key-one', ...overrides,
})
const request = () => ({ trace_id: 'source-one', source: 'outgoing', body: '{"value":9007199254740993}', headers: { Accept: 'application/json' }, credentials: [{ kind: 'header', name: 'Authorization', value: 'Bearer synthetic-only' }], idempotency_key: 'key-one' })

function setup(overrides = {}) {
  let state
  const api = { create: async () => record(), list: async () => [], get: async () => record(), cancel: async () => record({ state: 'canceled' }), definiteRejection: () => false, ...overrides }
  const operation = new ReplayOperation(api, value => { state = value })
  return { operation, get state() { return state } }
}

test('ambiguous accepted action is recovered by key without another execution or public credentials', async () => {
  let posts = 0
  const context = setup({ create: async () => { posts++; throw new TypeError('lost create response') }, list: async () => [record()] })
  await context.operation.start(request())
  assert.equal(context.state.phase, 'unknown')
  assert.equal(context.operation.canStart, false)
  assert.doesNotMatch(JSON.stringify(context.state), /synthetic-only|9007199254740993/)
  await context.operation.start({ ...request(), idempotency_key: 'unintended-second-key' })
  assert.equal(posts, 1)
  await context.operation.recover()
  assert.equal(context.state.record.id, 'run-one')
  assert.equal(context.state.phase, 'accepted')
  assert.equal(posts, 1)
})

test('explicit retry retains submitted bytes, credentials and key after the draft changes', async () => {
  const sent = []
  const context = setup({ create: async value => { sent.push(value); if (sent.length === 1) throw new TypeError('offline'); return record() } })
  const draft = request()
  await context.operation.start(draft)
  draft.body = '{"value":42}'
  draft.headers.Accept = 'text/plain'
  draft.credentials[0].value = 'new-draft-secret'
  draft.idempotency_key = 'new-draft-key'
  await context.operation.recover()
  assert.equal(sent.length, 1, 'read recovery does not POST')
  await context.operation.recover(true)
  assert.equal(sent.length, 2)
  assert.deepEqual(sent[0], sent[1])
  assert.equal(sent[1].idempotency_key, 'key-one')
  assert.equal(sent[1].credentials[0].value, 'Bearer synthetic-only')
  assert.equal(sent[1].body, '{"value":9007199254740993}')
})

test('definite first rejection permits correction, but a rejection while recovering does not erase prior uncertainty', async () => {
  const rejected = new Error('validation rejected')
  const context = setup({ create: async () => { throw rejected }, definiteRejection: value => value === rejected })
  await context.operation.start(request())
  assert.equal(context.state.phase, 'idle')
  assert.equal(context.operation.canStart, true)
  let posts = 0
  const ambiguous = setup({ create: async () => { if (++posts === 1) throw new TypeError('lost'); throw rejected }, definiteRejection: value => value === rejected })
  await ambiguous.operation.start(request())
  await ambiguous.operation.recover(true)
  assert.equal(ambiguous.state.phase, 'unknown')
  assert.equal(ambiguous.operation.canStart, false)
})

test('source changes and old observations cannot hide a running operation or revive a terminal operation', async () => {
  const context = setup()
  await context.operation.start(request())
  context.operation.observe([record({ id: 'other-run', replay_of: 'another-source' })], 'another-source')
  assert.equal(context.state.record.id, 'run-one')
  assert.equal(context.operation.canStart, false)
  context.operation.observe([record({ state: 'done', finished_at: '2026-09-11T00:00:01Z' })], 'another-source')
  context.operation.observe([record()], 'unrelated-source')
  assert.equal(context.state.record.state, 'done')
  assert.equal(context.operation.canStart, true)
})

test('a running record is rehydrated and a cancel/completion race keeps the terminal server result', async () => {
  const context = setup({ cancel: async () => { throw new Error('already finished') }, get: async () => record({ state: 'done' }) })
  context.operation.observe([record()], 'source-one')
  assert.equal(context.state.record.id, 'run-one')
  assert.equal(context.operation.canStart, false)
  const completed = await context.operation.cancel()
  assert.equal(completed.state, 'done')
  assert.equal(context.state.record.state, 'done')
  assert.equal(context.state.error, '')
})

test('failed recovery query keeps the operation blocked and never posts', async () => {
  let posts = 0
  const context = setup({ create: async () => { posts++; throw new TypeError('offline') }, list: async () => { throw new TypeError('offline') } })
  await context.operation.start(request())
  await context.operation.recover(true)
  assert.equal(posts, 1)
  assert.equal(context.state.phase, 'unknown')
  assert.equal(context.operation.canStart, false)
})

test('a lost first creation response stays ambiguous when the implicit retry returns 503 or 404', async t => {
  for (const status of [503, 404]) {
    await t.test(`retry HTTP ${status}`, async t => {
      const submitted = []
      t.mock.method(globalThis, 'fetch', async (_url, options) => {
        submitted.push(JSON.parse(options.body))
        if (submitted.length === 1) throw new TypeError('first response lost after acceptance')
        return new Response(JSON.stringify({ error: 'temporary rejection after accepted action' }), { status })
      })
      const context = setup({ create: createReplay, definiteRejection: value => value instanceof APIError, list: async () => [record()] })
      await context.operation.start(request())
      assert.equal(submitted.length, 2)
      assert.equal(context.state.phase, 'unknown')
      assert.equal(context.operation.canStart, false)
      await context.operation.start({ ...request(), idempotency_key: 'accidental-next-key' })
      assert.equal(submitted.length, 2)
      await context.operation.recover()
      assert.equal(context.state.record.id, 'run-one')
      assert.equal(context.state.record.idempotency_key, 'key-one')
      assert.deepEqual(submitted[0], submitted[1])
      assert.equal(submitted.length, 2, 'recovery is a read, not a new execution')
    })
  }
})

test('a definite first createReplay rejection does not retry and permits correction', async t => {
  let posts = 0
  t.mock.method(globalThis, 'fetch', async () => {
    posts++
    return new Response(JSON.stringify({ error: 'registration failed before dispatch' }), { status: 503 })
  })
  const context = setup({ create: createReplay, definiteRejection: value => value instanceof APIError })
  await context.operation.start(request())
  assert.equal(posts, 1)
  assert.equal(context.state.phase, 'idle')
  assert.equal(context.operation.canStart, true)
})

test('an open comparison follows current/history completion without changing its selected pair', () => {
  const selected = record()
  const completed = record({ state: 'done', status_code: 200, finished_at: '2026-09-11T00:00:01Z' })
  const other = record({ id: 'run-two', trace_id: 'result-two', replay_of: 'source-two' })
  let comparison = reconcileReplayRecord(selected, completed, [selected])
  assert.equal(comparison.state, 'done', 'a poll finishes a comparison opened while running')
  comparison = reconcileReplayRecord(comparison, other, [selected])
  assert.equal(comparison.id, selected.id)
  assert.equal(comparison.state, 'done', 'an old list cannot revive running state')
  assert.equal(comparison.trace_id, selected.trace_id)
  assert.equal(comparison.replay_of, selected.replay_of)
  assert.equal(reconcileReplayRecord(selected, other, [completed]).state, 'done', 'history-only completion updates a selected history row')
  assert.equal(reconcileReplayRecord(null, completed, [completed]), null, 'refresh does not open a comparison automatically')
})
