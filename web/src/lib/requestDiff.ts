export interface RequestChange {
  path: string
  before: string
  after: string
}

function parseBody(body: string): unknown {
  try { return JSON.parse(body) }
  catch { return body }
}

function display(value: unknown): string {
  return value === undefined ? '（不存在）' : JSON.stringify(value, null, 2)
}

// Compare JSON by field and array position. Whitespace-only edits do not bury
// message/system/tool changes in a whole-body textual replacement.
export function requestChanges(originalBody: string, modifiedBody: string, originalHeaders: Record<string, string>, modifiedHeaders: Record<string, string>) {
  const changes: RequestChange[] = []
  let truncated = false
  function compare(before: unknown, after: unknown, path: string) {
    if (Object.is(before, after)) return
    if (before && after && typeof before === 'object' && typeof after === 'object' && Array.isArray(before) === Array.isArray(after)) {
      const left = before as Record<string, unknown>
      const right = after as Record<string, unknown>
      for (const key of new Set([...Object.keys(left), ...Object.keys(right)])) {
        compare(left[key], right[key], `${path}/${key.replace(/~/g, '~0').replace(/\//g, '~1')}`)
      }
      return
    }
    if (changes.length >= 200) truncated = true
    else changes.push({ path, before: display(before), after: display(after) })
  }
  compare(parseBody(originalBody), parseBody(modifiedBody), '/body')
  compare(originalHeaders, modifiedHeaders, '/headers')
  return { changes, truncated }
}
