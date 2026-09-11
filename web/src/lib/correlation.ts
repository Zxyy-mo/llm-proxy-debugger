export function linkLabel(source?: string): string {
  if (source === 'previous_response_id') return '前序响应 ID'
  if (source === 'parent_trace_id') return '父调用 ID'
  if (source === 'history') return '历史内容匹配'
  if (source === 'replay') return '重放来源'
  return '独立调用'
}

export function sourceLabel(source?: string): string {
  if (!source || source === 'new') return '自动创建'
  if (source === 'history') return '历史内容匹配'
  if (source === 'replay') return '归入被重放请求的会话'
  if (source === 'previous_response_id' || source === 'parent_trace_id') return linkLabel(source)
  if (source.startsWith('header:')) return source.slice(7)
  if (source.startsWith('body:')) return source.slice(5)
  if (source === 'response:conversation.id') return '响应中的 conversation.id'
  if (source.startsWith('path:')) return source.slice(5)
  return source
}

const warnings: Record<string, string> = {
  unresolved_parent: '父调用尚未被网关捕获。捕获到对应 ID 后会自动补上关联。',
  ambiguous_parent: '多个请求返回了相同的响应 ID，暂时无法确定父调用。',
  ambiguous_history: '这段历史匹配到多个已完成请求，暂时保留为独立调用。',
  conflicting_identifier: '请求携带的会话标识存在冲突，已保留优先级更高的会话标识。',
  session_boundary: '父调用位于其他会话；当前请求保留显式指定的会话归属。',
  cycle: '这个父调用引用会形成循环，已跳过该关联。',
  history_limit: '内容超过历史匹配的大小上限，仍会按明确的 ID 进行关联。',
  deleted_parent: '父调用已从本地历史中删除，当前记录保留其引用。',
}

export function warningLabel(warning?: string): string {
  return warning ? warnings[warning] ?? warning : ''
}

export function formatDuration(value: number): string {
  return value >= 1000 ? `${(value / 1000).toFixed(1)}s` : `${Math.round(value)}ms`
}

export function statusLabel(status?: string): string {
  const labels: Record<string, string> = { pending: '待放行', running: '进行中', done: '已完成', error: '失败', canceled: '已取消' }
  return status ? labels[status] ?? status : '未知'
}

export function replayStateLabel(state?: string): string {
  const labels: Record<string, string> = { running: '重放中', done: '已完成', error: '失败', canceled: '已取消' }
  return state ? labels[state] ?? state : '未知'
}

export const BODY_FORMAT_LIMITS = { inputChars: 512 * 1024, outputChars: 512 * 1024, depth: 64 } as const

// Re-indent validated JSON text without rebuilding its values, so numeric
// literals, key order and escapes survive editing. Large/deep/expanding input
// stays byte-for-byte as supplied; formatting must never inflate an editor.
export function formatBody(value?: string): string {
  if (!value) return ''
  if (value.length > BODY_FORMAT_LIMITS.inputChars) return value
  // Check depth before JSON.parse allocates a tree for an adversarial capture.
  let scanDepth = 0
  let scanString = false
  let scanEscaped = false
  for (const char of value) {
    if (scanString) {
      if (scanEscaped) scanEscaped = false
      else if (char === '\\') scanEscaped = true
      else if (char === '"') scanString = false
    } else if (char === '"') scanString = true
    else if (char === '{' || char === '[') {
      if (++scanDepth > BODY_FORMAT_LIMITS.depth) return value
    } else if ((char === '}' || char === ']') && --scanDepth < 0) return value
  }
  try {
    JSON.parse(value)
  } catch {
    return value
  }
  let out = ''
  let depth = 0
  let inString = false
  let escaped = false
  const indent = () => '\n' + '  '.repeat(depth)
  const append = (text: string): boolean => {
    if (out.length + text.length > BODY_FORMAT_LIMITS.outputChars) return false
    out += text
    return true
  }
  for (let i = 0; i < value.length; i++) {
    const c = value[i]!
    if (inString) {
      if (!append(c)) return value
      if (escaped) escaped = false
      else if (c === '\\') escaped = true
      else if (c === '"') inString = false
      continue
    }
    switch (c) {
      case '"':
        inString = true
        if (!append(c)) return value
        break
      case '{':
      case '[': {
        const close = c === '{' ? '}' : ']'
        let j = i + 1
        while (j < value.length && /\s/.test(value[j]!)) j++
        if (value[j] === close) {
          if (!append(c + close)) return value
          i = j
        } else {
          depth++
          if (!append(c + indent())) return value
        }
        break
      }
      case '}':
      case ']':
        depth--
        if (!append(indent() + c)) return value
        break
      case ',':
        if (!append(',' + indent())) return value
        break
      case ':':
        if (!append(': ')) return value
        break
      default:
        if (!/\s/.test(c) && !append(c)) return value
    }
  }
  return out
}
