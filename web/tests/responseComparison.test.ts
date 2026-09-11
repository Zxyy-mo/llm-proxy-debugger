import assert from 'node:assert/strict'
import test from 'node:test'
import { boundedResponseText, comparisonMetrics, responseJSONLimit, responseObservation, responsePreviewLimit, responseToolFacts } from '../src/lib/responseComparison.ts'
import type { RequestLog, ResponseSnapshot } from '../src/lib/types'

function log(values: Partial<RequestLog> = {}): RequestLog {
  return {
    trace_id: 'trace', session_id: 'session', time: '2026-09-11T00:00:00Z', type: 'HTTP', client_ip: '127.0.0.1', method: 'POST', path: '/v1/responses', user_agent: 'test',
    status: 'done', status_code: 200, model: 'same-model', duration_ms: 0, wait_duration_ms: 0, upstream_duration_ms: 0,
    token_sources: { input: 'usage', output: 'usage', thinking: 'unknown' }, ...values,
  }
}

function capture(body: string, values: Partial<ResponseSnapshot> = {}): ResponseSnapshot {
  const bytes = new TextEncoder().encode(body).length
  return { trace_id: 'trace', type: 'json', content_type: 'application/json', decoded: false, status_code: 200, bytes, stored: 'file', complete: true, receiving: false, body, offset: 0, end: bytes, next_offset: null, last_offset: 0, ...values }
}

test('known omitted zero counters remain zero; missing sources stay unknown', () => {
  const rows = comparisonMetrics(log(), log({ input_tokens: 5, output_tokens: 0 }))
  const input = rows.find(row => row.key === 'input_tokens')!
  assert.equal(input.source.value, 0)
  assert.equal(input.result.value, 5)
  assert.equal(input.delta, 5)
  assert.equal(rows.find(row => row.key === 'output_tokens')!.delta, 0)
  assert.equal(rows.find(row => row.key === 'thinking_tokens')!.source.value, null)
  const unknown = comparisonMetrics(log({ token_sources: undefined, input_tokens: 12 }), log())[5]!
  assert.equal(unknown.source.value, null)
  assert.equal(unknown.delta, null)
})

test('each counter retains provenance and only comparable model/provider values have deltas', () => {
  const usage = log({ input_tokens: 11, output_tokens: 20, thinking_tokens: 4, token_sources: { input: 'usage', output: 'estimated', thinking: 'unknown' } })
  const result = log({ input_tokens: 13, output_tokens: 25, thinking_tokens: 8, token_sources: { input: 'estimated', output: 'estimated', thinking: 'usage' } })
  const rows = comparisonMetrics(usage, result)
  assert.equal(rows.find(row => row.key === 'input_tokens')!.delta, null)
  assert.equal(rows.find(row => row.key === 'output_tokens')!.delta, 5)
  assert.equal(rows.find(row => row.key === 'output_tokens')!.source.provenance, 'estimated')
  assert.equal(rows.find(row => row.key === 'thinking_tokens')!.delta, null)
  for (const other of [log({ model: 'other' }), log({ model: undefined }), log({ route: { provider_id: 'other-provider', attempts: [] } }), log({ route: { target_model: 'other-upstream-model', attempts: [] } })]) {
    assert.equal(comparisonMetrics(log(), other).find(row => row.key === 'input_tokens')!.delta, null)
  }
})

test('incomplete, canceled, unknown and nonfinite timing values are never invented', () => {
  const source = log({ duration_ms: 123, wait_duration_ms: 20, upstream_duration_ms: 100, ttfb_ms: 0, ttfc_ms: 25 })
  const result = log({ duration_ms: 140, wait_duration_ms: 0, upstream_duration_ms: 139, ttfb_ms: 3, ttfc_ms: 50 })
  const rows = comparisonMetrics(source, result)
  assert.equal(rows[0]!.delta, 17)
  assert.equal(rows[1]!.delta, -20)
  assert.equal(rows[3]!.source.value, 0)
  assert.equal(rows[4]!.delta, 25)
  for (const status of ['pending', 'running'] as const) {
    const active = comparisonMetrics(source, log({ status, duration_ms: 0 }))
    assert.ok(active.every(row => row.delta === null))
    assert.equal(active[0]!.result.value, null)
    assert.equal(active[0]!.result.pending, true)
  }
  const canceled = comparisonMetrics(source, log({ status: 'canceled', status_code: 499, duration_ms: 200, ttfb_ms: 3, ttfc_ms: 4, token_sources: { input: 'unknown', output: 'unknown', thinking: 'unknown' } }))
  assert.equal(canceled[0]!.delta, 77)
  assert.equal(canceled[3]!.result.value, null)
  assert.equal(canceled[4]!.result.value, null)
  for (const value of [undefined, Number.NaN, Number.POSITIVE_INFINITY, -1]) {
    assert.equal(comparisonMetrics(source, log({ upstream_duration_ms: value }))[2]!.result.value, null)
  }
  assert.equal(comparisonMetrics(source, log({ input_tokens: Number.MAX_SAFE_INTEGER + 1 }))[5]!.result.value, null)
  assert.ok(comparisonMetrics(null, result).every(row => row.delta === null))
  assert.equal(comparisonMetrics(null, result)[0]!.result.value, 140)
})

test('stream and WebSocket output uses bounded server observations, not raw event JSON', () => {
  for (const type of ['SSE', 'WebSocket']) {
    const result = responseObservation(log({ type, response_body: 'already observed answer', thinking_content: 'already observed thinking' }), capture('data: {"delta":"do not parse this"}', { type: 'sse' }))
    assert.equal(result.output, 'already observed answer')
    assert.equal(result.thinking, 'already observed thinking')
    assert.ok(result.notes.some(note => note.includes('日志有长度上限')))
  }
  const long = 'a'.repeat(responsePreviewLimit - 1) + '🌍' + 'end'
  const clipped = responseObservation(log({ type: 'SSE', response_body: long, thinking_content: long }))
  assert.equal(clipped.output, 'a'.repeat(responsePreviewLimit - 1))
  assert.ok(clipped.outputClipped && clipped.thinkingClipped)
  assert.equal(boundedResponseText('🌍end', 1).text, '')
  assert.ok(boundedResponseText('x'.repeat(responsePreviewLimit * 2), Number.POSITIVE_INFINITY).text.length <= responsePreviewLimit)
})

test('only known string fields are extracted from complete bounded JSON', () => {
  for (const [body, expected] of [
    ['{"choices":[{"message":{"content":"first answer","tool_calls":[{"arguments":{"id":9007199254740993}}]}},{"message":{"content":"second choice"}}]}', 'first answer'],
    ['{"output":[{"type":"function_call","arguments":"do not show tool arguments"},{"type":"message","content":[{"type":"output_text","text":"你好"},{"type":"output_text","text":"answer"}]}]}', '你好\nanswer'],
    ['{"type":"message","content":[{"type":"thinking","thinking":"server log owns thinking"},{"type":"text","text":"reply"},{"type":"tool_use","input":{"id":9007199254740993}}]}', 'reply'],
    ['{"output_text":"number as text: 9007199254740993\\nnext"}', 'number as text: 9007199254740993\nnext'],
  ]) {
    const response = responseObservation(log(), capture(body!))
    assert.equal(response.output, expected)
    assert.equal(response.thinking, '')
    assert.equal(response.providerError, '')
  }
  assert.equal(responseObservation(log(), capture('{"choices":[{"message":{"content":9007199254740993}}]}')).output, '')
  assert.equal(responseObservation(log(), capture('{"arbitrary":{"text":"not a known field"}}')).output, '')
  assert.equal(responseObservation(log(), capture('{"output_content":"redacted answer"}', { redacted: true, representation: 'redacted-output' })).output, 'redacted answer')
})

test('partial, encoded, missing, oversized or invalid captures do not masquerade as normal output', () => {
  const body = '{"output_text":"small answer"}'
  for (const values of [
    { complete: false }, { receiving: true }, { truncated: true }, { offset: 1 }, { end: 1 }, { body_encoding: 'base64' }, { stored: 'missing' }, { bytes: responseJSONLimit + 1 },
  ] satisfies Partial<ResponseSnapshot>[]) {
    assert.equal(responseObservation(log({ response_body: body }), capture(body, values)).output, '')
  }
  assert.equal(responseObservation(log(), capture('{"output_text":')).output, '')
  assert.equal(responseObservation(log(), capture('{"output_text":"' + 'x'.repeat(responseJSONLimit) + '"}')).output, '')
  assert.equal(responseObservation(log(), capture('['.repeat(8000) + '0' + ']'.repeat(8000))).output, '')
  const long = capture(JSON.stringify({ output: [{ type: 'message', content: Array.from({ length: 100 }, () => ({ type: 'output_text', text: 'x'.repeat(100) })) }] }))
  const observed = responseObservation(log(), long)
  assert.ok(observed.output.length <= responsePreviewLimit)
  assert.ok(observed.outputClipped)
})

test('errors, redaction, conversion, partial observations and cancellation remain visible', () => {
  const failed = log({ status: 'error', status_code: 500, privacy: { recorded: true, outbound: false, raw_retained: true }, route: { conversion: 'openai', attempts: [] }, observation_warning: 'parser limit reached' })
  const observed = responseObservation(failed, capture('{"error":{"message":"provider failed","code":9007199254740993}}', { redacted: true, decoded: true, content_encoding: 'gzip', representation: 'converted-from-openai', observation_warning: 'parser limit reached' }))
  assert.equal(observed.providerError, 'provider failed')
  assert.equal(observed.output, '')
  assert.equal(observed.notes.filter(note => note === 'parser limit reached').length, 1)
  assert.ok(observed.notes.some(note => note.includes('脱敏')))
  assert.ok(observed.notes.some(note => note.includes('转换后')))
  assert.ok(observed.notes.some(note => note.includes('gzip')))
  const canceled = responseObservation(log({ type: 'SSE', status: 'canceled', response_body: 'partial text' }), capture('', { complete: false, reason: 'capture interrupted' }))
  assert.equal(canceled.output, 'partial text')
  assert.ok(canceled.notes.includes('capture interrupted'))
  assert.ok(canceled.notes.some(note => note.includes('已取消')))
  assert.equal(responseObservation(null, null).output, '')
})

test('tool observations do not claim requested tools have executed', () => {
  assert.match(responseToolFacts(log({ tool_use_count: 2 })), /没有关联的执行记录/)
  const facts = responseToolFacts(log({ tools: [{ id: 'call', call_id: 'call', name: 'lookup', kind: 'tool', input: '{}', source: 'llm', status: 'requested' }] }))
  assert.match(facts, /工具记录 1 条/)
  assert.match(facts, /执行 Span 0 条/)
  assert.match(facts, /观察到返回 0 条/)
})
