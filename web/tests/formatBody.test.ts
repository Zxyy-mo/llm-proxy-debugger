import assert from 'node:assert/strict'
import test from 'node:test'
import { BODY_FORMAT_LIMITS as limits, formatBody } from '../src/lib/correlation.ts'

test('ordinary formatting preserves numeric spelling, key order and string escapes', () => {
  const body = '{"seed":9007199254740993,"decimal":0.123456789012345678901e+9999,"negative":-0,"text":"\\u0061\\n\\\"[ignored]\\\"","empty":[],"object":{}}'
  const formatted = formatBody(body)
  assert.equal(formatted, '{\n  "seed": 9007199254740993,\n  "decimal": 0.123456789012345678901e+9999,\n  "negative": -0,\n  "text": "\\u0061\\n\\\"[ignored]\\\"",\n  "empty": [],\n  "object": {}\n}')
  assert.equal(formatBody(formatted), formatted)
})

test('deep JSON remains exact raw input before indentation can grow quadratically', () => {
  const body = ' \n' + '['.repeat(2000) + '9007199254740993' + ']'.repeat(2000) + '\r\n'
  assert.equal(formatBody(body), body)
  const permitted = '['.repeat(limits.depth) + '1' + ']'.repeat(limits.depth)
  const output = formatBody(permitted)
  assert.notEqual(output, permitted)
  assert.ok(output.length <= limits.outputChars)
  const tooDeep = '['.repeat(limits.depth + 1) + '1' + ']'.repeat(limits.depth + 1)
  assert.equal(formatBody(tooDeep), tooDeep)
})

test('quoted brackets and escaped quotes do not consume structural depth', () => {
  const value = '['.repeat(2000) + '"\\' + ']'.repeat(2000)
  const body = '{"text":' + JSON.stringify(value) + '}'
  assert.equal(formatBody(body), '{\n  "text": ' + JSON.stringify(value) + '\n}')
})

test('wide nested input returns raw bytes when pretty-print expansion reaches its budget', () => {
  const payload = Array(20_000).fill('0').join(',')
  const body = '['.repeat(40) + payload + ']'.repeat(40)
  assert.ok(body.length < limits.inputChars)
  assert.equal(formatBody(body), body)
  const whitespaceHeavy = ' '.repeat(100) + body + '\n'
  assert.equal(formatBody(whitespaceHeavy), whitespaceHeavy)
})

test('oversized and invalid input is never truncated or reconstructed by formatting', () => {
  const large = '{"number":9007199254740993,"text":"' + 'x'.repeat(limits.inputChars) + '"}'
  assert.equal(formatBody(large), large)
  for (const invalid of ['  {', '}', '{"n":01}', '{"n":1e+}', '{"text":"unterminated\\', '{"a":1,}', 'not JSON', ' \t']) {
    assert.equal(formatBody(invalid), invalid)
  }
  assert.equal(formatBody(undefined), '')
  assert.equal(formatBody(''), '')
})
