export interface RequestChange {
  path: string
  before: string
  after: string
  pathTruncated: boolean
  beforeTruncated: boolean
  afterTruncated: boolean
}

export const REQUEST_DIFF_LIMITS = {
  bodyChars: 512 * 1024,
  depth: 64,
  parseNodes: 20_000,
  compareNodes: 12_000,
  changes: 200,
  valueChars: 4096,
  pathChars: 512,
  renderedChars: 64 * 1024,
  headerFields: 512,
  headerChars: 64 * 1024,
} as const

type ParseLimit = 'body-size' | 'depth' | 'parse-work'
type ParseReason = ParseLimit | 'invalid-json' | 'duplicate-keys'
export type RequestDiffLimit = ParseLimit | 'compare-work' | 'change-count' | 'render-output' | 'header-size'

export interface RequestDiffResult {
  changes: RequestChange[]
  complete: boolean
  truncated: boolean
  bodyMode: 'json' | 'text' | 'unavailable'
  fallbackReason?: ParseReason
  limits: RequestDiffLimit[]
}

interface Span { start: number; end: number }
type JsonNode = Span & (
  | { kind: 'object'; entries: Map<string, JsonNode> }
  | { kind: 'array'; items: JsonNode[] }
  | { kind: 'string'; value: string }
  | { kind: 'number'; literal: string }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
)
interface JsonDocument { source: string; root: JsonNode }
type Parsed = { ok: true; document: JsonDocument } | { ok: false; reason: ParseReason }

class ParseFailure extends Error {
  reason: ParseReason
  constructor(reason: ParseReason) { super(reason); this.reason = reason }
}

// Construct typed nodes ourselves: neither user strings nor object properties
// can imitate an internal number. Only string tokens use JSON.parse; numeric
// literals, including overflowing exponents, never pass through Number.
function parseBody(source: string): Parsed {
  if (source.length > REQUEST_DIFF_LIMITS.bodyChars) return { ok: false, reason: 'body-size' }
  let cursor = 0
  let nodes = 0
  const fail = (reason: ParseReason = 'invalid-json'): never => { throw new ParseFailure(reason) }
  const spend = () => { if (++nodes > REQUEST_DIFF_LIMITS.parseNodes) fail('parse-work') }
  const digit = () => source.charCodeAt(cursor) >= 48 && source.charCodeAt(cursor) <= 57
  const whitespace = () => {
    while (cursor < source.length && (source[cursor] === ' ' || source[cursor] === '\t' || source[cursor] === '\r' || source[cursor] === '\n')) cursor++
  }
  function stringToken(): JsonNode & { kind: 'string' } {
    const start = cursor++
    while (cursor < source.length) {
      const code = source.charCodeAt(cursor++)
      if (code === 34) return { kind: 'string', value: JSON.parse(source.slice(start, cursor)) as string, start, end: cursor }
      if (code < 32) fail()
      if (code !== 92) continue
      const escape = source[cursor++]
      if (escape === 'u') {
        for (let i = 0; i < 4; i++) {
          const hex = source.charCodeAt(cursor++)
          if (!(hex >= 48 && hex <= 57) && !(hex >= 65 && hex <= 70) && !(hex >= 97 && hex <= 102)) fail()
        }
      } else if (escape === undefined || !'"\\/bfnrt'.includes(escape)) fail()
    }
    return fail()
  }
  function value(depth: number): JsonNode {
    spend()
    whitespace()
    const start = cursor
    const first = source[cursor]
    if (first === '"') return stringToken()
    if (first === '{' || first === '[') {
      if (depth >= REQUEST_DIFF_LIMITS.depth) fail('depth')
      cursor++
      whitespace()
      if (first === '[') {
        const items: JsonNode[] = []
        if (source[cursor] !== ']') {
          while (true) {
            items.push(value(depth + 1))
            whitespace()
            if (source[cursor] !== ',') break
            cursor++
          }
        }
        if (source[cursor++] !== ']') fail()
        return { kind: 'array', items, start, end: cursor }
      }
      const entries = new Map<string, JsonNode>()
      if (source[cursor] !== '}') {
        while (true) {
          whitespace()
          if (source[cursor] !== '"') fail()
          spend()
          const key = stringToken().value
          // Duplicate keys have ambiguous field ownership. Compare their raw
          // text instead of silently ignoring an edit to an overwritten value.
          if (entries.has(key)) fail('duplicate-keys')
          whitespace()
          if (source[cursor++] !== ':') fail()
          entries.set(key, value(depth + 1))
          whitespace()
          if (source[cursor] !== ',') break
          cursor++
        }
      }
      if (source[cursor++] !== '}') fail()
      return { kind: 'object', entries, start, end: cursor }
    }
    for (const [literal, kind] of [['true', 'boolean'], ['false', 'boolean'], ['null', 'null']] as const) {
      if (source.startsWith(literal, cursor)) {
        cursor += literal.length
        return kind === 'null' ? { kind, start, end: cursor } : { kind, value: literal === 'true', start, end: cursor }
      }
    }
    if (first === '-') cursor++
    if (source[cursor] === '0') cursor++
    else {
      if (!digit()) return fail()
      while (digit()) cursor++
    }
    if (source[cursor] === '.') {
      cursor++
      if (!digit()) fail()
      while (digit()) cursor++
    }
    if (source[cursor] === 'e' || source[cursor] === 'E') {
      cursor++
      if (source[cursor] === '+' || source[cursor] === '-') cursor++
      if (!digit()) fail()
      while (digit()) cursor++
    }
    return { kind: 'number', literal: source.slice(start, cursor), start, end: cursor }
  }
  try {
    const root = value(0)
    whitespace()
    if (cursor !== source.length) fail()
    return { ok: true, document: { source, root } }
  } catch (error) {
    return { ok: false, reason: error instanceof ParseFailure ? error.reason : 'invalid-json' }
  }
}

interface View { source: string; start: number; end: number; quoted?: boolean }
interface Rendered { text: string; truncated: boolean }
const absent: View = { source: '（不存在）', start: 0, end: 5 }

function prefix(source: string, start: number, end: number, limit: number): Rendered {
  let stop = Math.min(end, start + limit)
  if (stop > start && stop < end) {
    const last = source.charCodeAt(stop - 1)
    const next = source.charCodeAt(stop)
    if (last >= 0xd800 && last <= 0xdbff && next >= 0xdc00 && next <= 0xdfff) stop--
  }
  return { text: source.slice(start, stop), truncated: stop < end }
}

function display(view: View, limit: number): Rendered {
  const part = prefix(view.source, view.start, view.end, limit)
  if (!view.quoted) return part
  // Slice before stringifying so escaping cannot allocate an unbounded value.
  const encoded = JSON.stringify(part.text)
  const rendered = prefix(encoded, 0, encoded.length, limit)
  return { text: rendered.text, truncated: part.truncated || rendered.truncated }
}

function pointer(segments: string[], limit: number): Rendered {
  let text = ''
  for (const segment of segments) {
    if (text.length >= limit) return { text, truncated: true }
    text += '/'
    for (const character of segment) {
      const encoded = character === '~' ? '~0' : character === '/' ? '~1' : character
      if (text.length + encoded.length > limit) return { text, truncated: true }
      text += encoded
    }
  }
  return { text, truncated: false }
}

// Compare JSON fields/array positions with exact numeric spelling. Whitespace,
// object-key ordering and equivalent string escaping do not create changes.
// Over-budget bodies are explicitly unavailable rather than declared equal.
export function requestChanges(
  originalBody: string,
  modifiedBody: string,
  originalHeaders: Record<string, string> = {},
  modifiedHeaders: Record<string, string> = {},
): RequestDiffResult {
  const result: RequestDiffResult = { changes: [], complete: true, truncated: false, bodyMode: 'json', limits: [] }
  const limits = new Set<RequestDiffLimit>()
  let remainingChars: number = REQUEST_DIFF_LIMITS.renderedChars
  let compared = 0
  let stopped = false
  const incomplete = (reason: RequestDiffLimit) => { result.complete = false; result.truncated = true; limits.add(reason) }
  const stop = (reason: RequestDiffLimit) => { stopped = true; incomplete(reason); return false }
  const visit = () => !stopped && (++compared <= REQUEST_DIFF_LIMITS.compareNodes || stop('compare-work'))

  function addChange(path: string[], before: View, after: View): boolean {
    if (result.changes.length >= REQUEST_DIFF_LIMITS.changes) return stop('change-count')
    if (remainingChars < 40) return stop('render-output')
    const renderedPath = pointer(path, Math.min(REQUEST_DIFF_LIMITS.pathChars, remainingChars - 32))
    const valueLimit = Math.min(REQUEST_DIFF_LIMITS.valueChars, Math.floor((remainingChars - renderedPath.text.length) / 2))
    const left = display(before, valueLimit)
    const right = display(after, valueLimit)
    result.changes.push({
      path: renderedPath.text, before: left.text, after: right.text,
      pathTruncated: renderedPath.truncated, beforeTruncated: left.truncated, afterTruncated: right.truncated,
    })
    remainingChars -= renderedPath.text.length + left.text.length + right.text.length
    if (renderedPath.truncated || left.truncated || right.truncated) {
      result.truncated = true
      limits.add('render-output')
    }
    return true
  }

  function compare(before: JsonNode | undefined, after: JsonNode | undefined, path: string[], left: JsonDocument, right: JsonDocument): boolean {
    if (!visit()) return false
    if (before?.kind === 'object' && after?.kind === 'object') {
      for (const [key, value] of before.entries) {
        path.push(key)
        const complete = compare(value, after.entries.get(key), path, left, right)
        path.pop()
        if (!complete) return false
      }
      for (const [key, value] of after.entries) {
        if (before.entries.has(key)) continue
        path.push(key)
        const complete = compare(undefined, value, path, left, right)
        path.pop()
        if (!complete) return false
      }
      return true
    }
    if (before?.kind === 'array' && after?.kind === 'array') {
      for (let i = 0; i < Math.max(before.items.length, after.items.length); i++) {
        path.push(String(i))
        const complete = compare(before.items[i], after.items[i], path, left, right)
        path.pop()
        if (!complete) return false
      }
      return true
    }
    if (before?.kind === 'null' && after?.kind === 'null') return true
    if (before?.kind === 'number' && after?.kind === 'number' && before.literal === after.literal) return true
    if (before?.kind === 'string' && after?.kind === 'string' && before.value === after.value) return true
    if (before?.kind === 'boolean' && after?.kind === 'boolean' && before.value === after.value) return true
    return addChange(path, before ? { source: left.source, start: before.start, end: before.end } : absent, after ? { source: right.source, start: after.start, end: after.end } : absent)
  }

  const left = parseBody(originalBody)
  const right = parseBody(modifiedBody)
  if (left.ok && right.ok) compare(left.document.root, right.document.root, ['body'], left.document, right.document)
  else {
    const reasons = [left, right].filter((parsed): parsed is Extract<Parsed, { ok: false }> => !parsed.ok).map(parsed => parsed.reason)
    const limit = reasons.find((reason): reason is ParseLimit => reason === 'body-size' || reason === 'depth' || reason === 'parse-work')
    if (limit) {
      result.bodyMode = 'unavailable'
      result.fallbackReason = limit
      incomplete(limit)
    } else {
      result.bodyMode = 'text'
      result.fallbackReason = reasons[0]
      // Both inputs are within the character limit before raw equality runs.
      if (originalBody !== modifiedBody) addChange(['body'], { source: originalBody, start: 0, end: originalBody.length }, { source: modifiedBody, start: 0, end: modifiedBody.length })
    }
  }

  function headerKeys(headers: Record<string, string>): string[] | undefined {
    const keys: string[] = []
    let chars = 0
    for (const key in headers) {
      if (!Object.hasOwn(headers, key)) continue
      chars += key.length + headers[key]!.length
      if (keys.length >= REQUEST_DIFF_LIMITS.headerFields || chars > REQUEST_DIFF_LIMITS.headerChars) {
        incomplete('header-size')
        return undefined
      }
      keys.push(key)
    }
    return keys
  }
  if (!stopped) {
    const leftKeys = headerKeys(originalHeaders)
    const rightKeys = headerKeys(modifiedHeaders)
    if (leftKeys && rightKeys) {
      const keySet = new Set(leftKeys)
      const keys = [...leftKeys, ...rightKeys.filter(key => !keySet.has(key))]
      for (const key of keys) {
        if (!visit()) break
        const before = Object.hasOwn(originalHeaders, key) ? originalHeaders[key] : undefined
        const after = Object.hasOwn(modifiedHeaders, key) ? modifiedHeaders[key] : undefined
        if (before === after) continue
        const view = (value: string | undefined): View => value === undefined ? absent : { source: value, start: 0, end: value.length, quoted: true }
        if (!addChange(['headers', key], view(before), view(after))) break
      }
    }
  }
  result.limits = [...limits]
  return result
}
