import assert from 'node:assert/strict'
import test from 'node:test'
import { REQUEST_DIFF_LIMITS as limits, requestChanges } from '../src/lib/requestDiff.ts'

test('large integers, fractional digits and exponents remain exact changes', () => {
  const pairs = [
    ['9007199254740992', '9007199254740993'],
    ['9223372036854775806', '9223372036854775807'],
    ['-9007199254740992', '-9007199254740993'],
    ['0.123456789012345678901234567890', '0.123456789012345678901234567891'],
    ['1.00000000000000001', '1.00000000000000002'],
    ['1e400', '2e400'],
    ['1e-400', '2e-400'],
    ['12345678901234567890.123e+999999', '12345678901234567890.124e+999999'],
    ['1e9007199254740992', '1e9007199254740993'],
  ]
  for (const [before, after] of pairs) {
    const diff = requestChanges('{"seed":' + before + '}', '{"seed":' + after + '}')
    assert.equal(diff.bodyMode, 'json')
    assert.equal(diff.complete, true)
    assert.equal(diff.changes.length, 1)
    assert.equal(diff.changes[0]!.path, '/body/seed')
    assert.equal(diff.changes[0]!.before, before)
    assert.equal(diff.changes[0]!.after, after)
    assert.equal(diff.truncated, false)
  }
})

test('numeric spelling is preserved, including negative zero and equivalent exponent forms', () => {
  for (const [before, after] of [['-0', '0'], ['1.00', '1'], ['1e3', '1000'], ['1E+03', '1e3']]) {
    const diff = requestChanges(before!, after!)
    assert.equal(diff.changes.length, 1)
    assert.equal(diff.changes[0]!.before, before)
    assert.equal(diff.changes[0]!.after, after)
  }
})

test('formatting, key order and equivalent string escapes do not create changes', () => {
  const before = '{"a":9007199254740993,"b":[1.234567890123456789e400,{"text":"a/b","on":true,"empty":null}]}'
  const after = ' { "b" : [ 1.234567890123456789e400 , { "empty":null, "on":true, "text":"\\u0061\\/b" } ], "a":9007199254740993 }\r\n'
  const diff = requestChanges(before, after, { Accept: 'json', Other: 'same' }, { Other: 'same', Accept: 'json' })
  assert.equal(diff.bodyMode, 'json')
  assert.equal(diff.complete, true)
  assert.equal(diff.truncated, false)
  assert.deepEqual(diff.changes, [])
})

test('added and removed nested values display numeric literals without reserialization', () => {
  const nested = '{"n":9007199254740993,"decimal":0.1234567890123456789,"array":[1e9999,-0]}'
  const added = requestChanges('{}', '{"new":' + nested + '}')
  assert.equal(added.changes[0]!.path, '/body/new')
  assert.equal(added.changes[0]!.before, '（不存在）')
  assert.equal(added.changes[0]!.after, nested)
  const removed = requestChanges('{"old":' + nested + '}', '{}')
  assert.equal(removed.changes[0]!.before, nested)
  assert.equal(removed.changes[0]!.after, '（不存在）')
})

test('ordinary strings and tag-shaped user objects cannot impersonate typed values', () => {
  const pairs = [
    ['9007199254740993', '"9007199254740993"'],
    ['9007199254740993', '{"kind":"number","literal":"9007199254740993","start":0,"end":16}'],
    ['9007199254740993', '{"$number":"9007199254740993"}'],
    ['9007199254740993', '"__NUMBER_0__"'],
    ['false', '"false"'],
    ['null', '"null"'],
    ['[]', '{}'],
  ]
  for (const [before, after] of pairs) {
    const diff = requestChanges('{"value":' + before + '}', '{"value":' + after + '}')
    assert.equal(diff.bodyMode, 'json')
    assert.equal(diff.changes.length, 1)
    assert.equal(diff.changes[0]!.path, '/body/value')
    assert.equal(diff.changes[0]!.before, before)
    assert.equal(diff.changes[0]!.after, after)
  }
})

test('JSON Pointer escaping covers slashes, tildes, empty keys, arrays and headers', () => {
  const diff = requestChanges(
    '{"a/b":{"~key":{"":1}},"arr":[null]}',
    '{"a/b":{"~key":{"":2}},"arr":[null,9007199254740993]}',
    { 'X/A~B': 'before' },
    { 'X/A~B': 'after' },
  )
  assert.deepEqual(diff.changes.map(change => change.path), ['/body/a~1b/~0key/', '/body/arr/1', '/headers/X~1A~0B'])
  assert.equal(diff.changes[1]!.before, '（不存在）')
  assert.equal(diff.changes[1]!.after, '9007199254740993')
  assert.equal(diff.changes[2]!.before, '"before"')
})

test('prototype-shaped keys remain ordinary own JSON properties', () => {
  const diff = requestChanges(
    '{"__proto__":{"value":9007199254740992},"constructor":1,"toString":0}',
    '{"__proto__":{"value":9007199254740993},"constructor":2,"toString":1}',
  )
  assert.deepEqual(diff.changes.map(change => change.path), ['/body/__proto__/value', '/body/constructor', '/body/toString'])
  assert.equal((Object.prototype as Record<string, unknown>).value, undefined)
})

test('invalid JSON uses explicit raw text fallback, without colliding with valid JSON strings', () => {
  const invalid = ['', 'plain text', '{"n":01}', '+1', '.5', '1.', '1e', '1e+', '-', '[1,]', '[1,,2]', '{"x":1,}', '{"n":NaN}', '{"s":"\\q"}', '{"s":"raw\nnewline"}', '\u00a0{}', '{} trailing']
  for (const body of invalid) {
    const diff = requestChanges(body, body + ' ')
    assert.equal(diff.bodyMode, 'text', body)
    assert.equal(diff.fallbackReason, 'invalid-json', body)
    assert.equal(diff.complete, true, body)
    assert.equal(diff.changes.length, 1, body)
    assert.equal(diff.changes[0]!.before, body)
    assert.equal(diff.changes[0]!.after, body + ' ')
  }
  const collision = requestChanges('plain text', '"plain text"')
  assert.equal(collision.changes.length, 1)
  assert.equal(collision.changes[0]!.before, 'plain text')
  assert.equal(collision.changes[0]!.after, '"plain text"')
  assert.equal(requestChanges('invalid', 'invalid').complete, true)
  assert.equal(requestChanges('invalid', 'invalid').changes.length, 0)
})

test('duplicate keys fall back to text so overwritten numeric edits are not lost', () => {
  const diff = requestChanges('{"n":9007199254740992,"n":0}', '{"n":9007199254740993,"n":0}')
  assert.equal(diff.bodyMode, 'text')
  assert.equal(diff.fallbackReason, 'duplicate-keys')
  assert.equal(diff.changes.length, 1)
  assert.match(diff.changes[0]!.after, /9007199254740993/)
  const escaped = requestChanges('{"a":1,"\\u0061":2}', '{"a":1,"\\u0061":3}')
  assert.equal(escaped.fallbackReason, 'duplicate-keys')
})

test('deep bodies stop safely and cannot be reported fully unchanged', () => {
  const nested = (depth: number, value: string) => '['.repeat(depth) + value + ']'.repeat(depth)
  const allowed = requestChanges(nested(limits.depth, '0'), nested(limits.depth, '1'))
  assert.equal(allowed.complete, true)
  assert.equal(allowed.changes.length, 1)
  const deep = requestChanges(nested(100_000, '0'), nested(100_000, '1'))
  assert.equal(deep.bodyMode, 'unavailable')
  assert.equal(deep.fallbackReason, 'depth')
  assert.equal(deep.complete, false)
  assert.equal(deep.truncated, true)
  assert.deepEqual(deep.changes, [])
})

test('oversized bodies are skipped while bounded header changes remain visible', () => {
  const common = 'x'.repeat(limits.bodyChars + 100)
  const diff = requestChanges('"' + common + 'A"', '"' + common + 'B"', { Accept: 'a' }, { Accept: 'b' })
  assert.equal(diff.bodyMode, 'unavailable')
  assert.equal(diff.fallbackReason, 'body-size')
  assert.equal(diff.complete, false)
  assert.deepEqual(diff.changes.map(change => change.path), ['/headers/Accept'])
  const identical = requestChanges(common, common)
  assert.equal(identical.complete, false)
  assert.deepEqual(identical.limits, ['body-size'])
})

test('a bounded character count does not permit unbounded parse structure', () => {
  const wideArray = '[' + Array.from({ length: limits.parseNodes + 1 }, () => '0').join(',') + ']'
  const arrayDiff = requestChanges(wideArray, wideArray)
  assert.ok(wideArray.length < limits.bodyChars)
  assert.equal(arrayDiff.bodyMode, 'unavailable')
  assert.equal(arrayDiff.fallbackReason, 'parse-work')
  assert.equal(arrayDiff.complete, false)
  const object = JSON.stringify(Object.fromEntries(Array.from({ length: limits.parseNodes / 2 + 1 }, (_, index) => ['field-' + index, 0])))
  const objectDiff = requestChanges(object, object)
  assert.ok(object.length < limits.bodyChars)
  assert.equal(objectDiff.fallbackReason, 'parse-work')
})

test('comparison work has its own bound even when late fields are the only changes', () => {
  const values = Array.from({ length: limits.compareNodes + 2 }, () => '0')
  const before = '[' + values.join(',') + ']'
  values[values.length - 1] = '1'
  const diff = requestChanges(before, '[' + values.join(',') + ']')
  assert.equal(diff.bodyMode, 'json')
  assert.equal(diff.complete, false)
  assert.deepEqual(diff.changes, [])
  assert.ok(diff.limits.includes('compare-work'))
})

test('change limits stop traversal without inventing an exact total', () => {
  for (const count of [limits.changes, limits.changes + 1, 500]) {
    const diff = requestChanges(JSON.stringify(Array(count).fill(0)), JSON.stringify(Array(count).fill(1)))
    assert.equal(diff.changes.length, Math.min(count, limits.changes))
    assert.equal(diff.complete, count <= limits.changes)
    assert.equal(diff.limits.includes('change-count'), count > limits.changes)
  }
})

test('large values have per-value and aggregate rendering caps with truthful metadata', () => {
  const before = JSON.stringify(Array(80).fill('a'.repeat(3000)))
  const after = JSON.stringify(Array(80).fill('b'.repeat(3000)))
  const diff = requestChanges(before, after)
  assert.equal(diff.complete, false)
  assert.equal(diff.truncated, true)
  assert.ok(diff.limits.includes('render-output'))
  assert.ok(diff.changes.length < 80)
  assert.ok(diff.changes.some(change => change.beforeTruncated || change.afterTruncated))
  const rendered = diff.changes.reduce((total, change) => total + change.path.length + change.before.length + change.after.length, 0)
  assert.ok(rendered <= limits.renderedChars)
  for (const change of diff.changes) {
    assert.ok(change.before.length <= limits.valueChars)
    assert.ok(change.after.length <= limits.valueChars)
    assert.ok(change.path.length <= limits.pathChars)
  }
})

test('numeric display truncation preserves the visible digits instead of rounding', () => {
  const literal = '9'.repeat(limits.valueChars + 500)
  const diff = requestChanges('0', literal)
  assert.equal(diff.complete, true)
  assert.equal(diff.changes.length, 1)
  assert.equal(diff.changes[0]!.after, literal.slice(0, limits.valueChars))
  assert.equal(diff.changes[0]!.afterTruncated, true)
  assert.equal(diff.truncated, true)
})

test('long paths are bounded without merging distinct changes or splitting escapes', () => {
  const key = '~/'.repeat(1000)
  const before = JSON.stringify({ [key + 'a']: 0, [key + 'b']: 0 })
  const after = JSON.stringify({ [key + 'a']: 1, [key + 'b']: 1 })
  const diff = requestChanges(before, after)
  assert.equal(diff.complete, true)
  assert.equal(diff.changes.length, 2)
  assert.equal(diff.truncated, true)
  for (const change of diff.changes) {
    assert.equal(change.pathTruncated, true)
    assert.ok(change.path.length <= limits.pathChars)
    assert.match(change.path, /^\/body\/(?:~0~1)*(?:~0)?$/)
  }
})

test('rendered text does not split UTF-16 pairs and bounded header escaping cannot expand without limit', () => {
  const text = 'x'.repeat(limits.valueChars - 2) + '😀tail'
  const diff = requestChanges('null', JSON.stringify(text))
  const last = diff.changes[0]!.after.charCodeAt(diff.changes[0]!.after.length - 1)
  assert.equal(diff.changes[0]!.afterTruncated, true)
  assert.ok(!(last >= 0xd800 && last <= 0xdbff))
  const header = requestChanges('{}', '{}', {}, { 'X-Text': '\0'.repeat(20_000) })
  assert.equal(header.changes[0]!.afterTruncated, true)
  assert.ok(header.changes[0]!.after.length <= limits.valueChars)
})

test('header field and character limits are visible and inherited keys are ignored', () => {
  const many = Object.fromEntries(Array.from({ length: limits.headerFields + 1 }, (_, index) => ['X-' + index, 'value']))
  const wide = requestChanges('{}', '{}', {}, many)
  assert.equal(wide.complete, false)
  assert.ok(wide.limits.includes('header-size'))
  const long = requestChanges('{}', '{}', {}, { 'X-Long': 'x'.repeat(limits.headerChars + 1) })
  assert.equal(long.complete, false)
  assert.ok(long.limits.includes('header-size'))
  const inherited = Object.assign(Object.create({ 'X-Inherited': 'not an own field' }) as Record<string, string>, { Accept: 'json' })
  assert.deepEqual(requestChanges('{}', '{}', { Accept: 'json' }, inherited).changes, [])
})
