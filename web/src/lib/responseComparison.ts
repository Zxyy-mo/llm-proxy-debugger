import type { RequestLog, ResponseSnapshot, TokenSource } from './types'

export const responsePreviewLimit = 8 * 1024
export const responseJSONLimit = 64 * 1024

export interface MetricValue {
  value: number | null
  pending: boolean
  provenance?: TokenSource
}

export interface ComparisonMetric {
  key: string
  label: string
  kind: 'time' | 'tokens'
  source: MetricValue
  result: MetricValue
  delta: number | null
  note: string
}

const timingFields = [
  ['duration_ms', '总耗时'],
  ['wait_duration_ms', '人工 / 规则等待'],
  ['upstream_duration_ms', '上游耗时'],
  ['ttfb_ms', '上游首字节 TTFB'],
  ['ttfc_ms', '首有效内容 TTFC'],
] as const
const tokenFields = [
  ['input_tokens', '输入 tokens', 'input'],
  ['output_tokens', '输出 tokens', 'output'],
  ['thinking_tokens', '思考 tokens', 'thinking'],
] as const

function pending(log?: RequestLog | null): boolean {
  return log?.status === 'running' || log?.status === 'pending'
}

function terminal(log?: RequestLog | null): boolean {
  return log?.status === 'done' || log?.status === 'error' || log?.status === 'canceled'
}

function measured(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null
}

function timingValue(log: RequestLog | null | undefined, key: typeof timingFields[number][0]): MetricValue {
  const value = log && terminal(log) && (!(key === 'ttfb_ms' || key === 'ttfc_ms') || log.status === 'done')
    ? measured(log[key]) : null
  return { value, pending: pending(log) }
}

function tokenValue(log: RequestLog | null | undefined, field: typeof tokenFields[number]): MetricValue {
  const provenance = log?.token_sources?.[field[2]] ?? 'unknown'
  // The Go API omits zero counters. A known source explicitly distinguishes
  // that zero from an absent/unknown measurement.
  const count = provenance === 'usage' || provenance === 'estimated' ? measured(log?.[field[0]] ?? 0) : null
  const value = count !== null && Number.isSafeInteger(count) ? count : null
  return { value, pending: pending(log), provenance }
}

function comparisonNote(source: MetricValue, result: MetricValue): string {
  if (source.pending || result.pending) return '调用尚未结束'
  if (source.value === null || result.value === null) return '存在未知数值'
  return ''
}

export function comparisonMetrics(source?: RequestLog | null, result?: RequestLog | null): ComparisonMetric[] {
  const rows: ComparisonMetric[] = timingFields.map(([key, label]) => {
    const left = timingValue(source, key)
    const right = timingValue(result, key)
    const note = comparisonNote(left, right)
    return { key, label, kind: 'time', source: left, result: right, delta: note ? null : right.value! - left.value!, note }
  })
  for (const field of tokenFields) {
    const left = tokenValue(source, field)
    const right = tokenValue(result, field)
    let note = comparisonNote(left, right)
    const sourceModel = source?.route?.target_model || source?.model
    const resultModel = result?.route?.target_model || result?.model
    if (!note && (!terminal(source) || !terminal(result))) note = '调用状态未知'
    if (!note && left.provenance !== right.provenance) note = 'Token 来源不同'
    if (!note && (!sourceModel || !resultModel)) note = '模型未确认'
    if (!note && (sourceModel !== resultModel || source?.route?.provider_id !== result?.route?.provider_id)) note = '模型或 Provider 不同'
    rows.push({ key: field[0], label: field[1], kind: 'tokens', source: left, result: right, delta: note ? null : right.value! - left.value!, note })
  }
  return rows
}

// Bound by UTF-16 code units, without leaving a split surrogate pair at the
// boundary. This is a display excerpt, not a byte offset into the capture.
export function boundedResponseText(value: string | undefined, limit = responsePreviewLimit): { text: string; clipped: boolean } {
  if (!value) return { text: '', clipped: false }
  let end = Number.isFinite(limit) ? Math.max(0, Math.min(responsePreviewLimit, Math.floor(limit))) : responsePreviewLimit
  if (value.length <= end) return { text: value, clipped: false }
  if (end > 0 && value.charCodeAt(end - 1) >= 0xd800 && value.charCodeAt(end - 1) <= 0xdbff && value.charCodeAt(end) >= 0xdc00 && value.charCodeAt(end) <= 0xdfff) end--
  return { text: value.slice(0, end), clipped: true }
}

export interface ResponseObservation {
  output: string
  thinking: string
  providerError: string
  outputClipped: boolean
  thinkingClipped: boolean
  origin: string
  empty: string
  notes: string[]
}

function object(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : undefined
}

function completeJSON(snapshot?: ResponseSnapshot | null): Record<string, unknown> | undefined {
  if (!snapshot || snapshot.type !== 'json' || snapshot.stored !== 'file' || snapshot.body_encoding || !snapshot.complete || snapshot.receiving || snapshot.truncated || snapshot.offset !== 0 || snapshot.end !== snapshot.bytes || snapshot.bytes > responseJSONLimit || snapshot.body.length > responseJSONLimit) return
  // Enforce the local bound independently of server metadata.
  if (new TextEncoder().encode(snapshot.body).length > responseJSONLimit) return
  try { return object(JSON.parse(snapshot.body)) } catch { return }
}

// Display-only extraction from a complete, small JSON capture. Never walk
// arbitrary nested data or stringify tool payloads/numeric fields. Protocol
// interpretation, metrics and completion remain server-owned.
function jsonOutput(root: Record<string, unknown>, snapshot: ResponseSnapshot): { text: string; clipped: boolean } {
  let text = ''
  let clipped = false
  function append(value: unknown) {
    if (typeof value !== 'string' || value === '') return
    const remaining = responsePreviewLimit - text.length - (text ? 1 : 0)
    const excerpt = boundedResponseText(value, remaining)
    if (text && remaining >= 0) text += '\n'
    text += excerpt.text
    clipped ||= excerpt.clipped
  }
  if (snapshot.representation === 'redacted-output') append(root.output_content)
  else if (typeof root.output_text === 'string') append(root.output_text)
  else if (Array.isArray(root.choices)) {
    const message = object(object(root.choices[0])?.message)
    append(message?.content)
  } else if (Array.isArray(root.output)) {
    for (const item of root.output) {
      const message = object(item)
      if (message?.type !== 'message' || !Array.isArray(message.content)) continue
      for (const raw of message.content) {
        const block = object(raw)
        if (block?.type === 'output_text') append(block.text)
      }
    }
  } else if (root.type === 'message' && Array.isArray(root.content)) {
    for (const raw of root.content) {
      const block = object(raw)
      if (block?.type === 'text') append(block.text)
    }
  }
  return { text, clipped }
}

export function responseObservation(log?: RequestLog | null, snapshot?: ResponseSnapshot | null): ResponseObservation {
  const notes = new Set<string>()
  const stream = log?.type?.toLowerCase() === 'sse' || log?.type?.toLowerCase() === 'websocket' || Boolean(log?.websocket)
  const root = completeJSON(snapshot)
  const excerpt = stream ? boundedResponseText(log?.response_body) : root && snapshot ? jsonOutput(root, snapshot) : { text: '', clipped: false }
  const thinking = boundedResponseText(log?.thinking_content)
  const providerError = root ? object(root.error)?.message ?? root.error : undefined
  if (log?.observation_warning) notes.add(boundedResponseText(log.observation_warning, 1024).text)
  if (snapshot?.observation_warning) notes.add(boundedResponseText(snapshot.observation_warning, 1024).text)
  if (snapshot?.receiving || pending(log)) notes.add('调用仍在进行，当前内容和指标可能继续更新。')
  else if (snapshot && !snapshot.complete && snapshot.stored === 'file') notes.add(boundedResponseText(snapshot.reason, 1024).text || '响应未完整结束，仅显示已捕获的部分。')
  if (snapshot?.stored === 'missing') notes.add(boundedResponseText(snapshot.reason, 1024).text || '完整响应文件不可用；已保存的观察预览仍可阅读。')
  if (log?.status === 'canceled') notes.add('调用已取消；已观察到的文本不代表完整输出。')
  if (log?.privacy?.recorded || snapshot?.redacted) notes.add(snapshot?.representation === 'redacted-output' ? '记录脱敏：保存的是输出投影，未保留原始事件流。' : '内容按记录策略脱敏，可能包含占位符。')
  if (log?.route?.conversion || snapshot?.representation === 'converted-from-openai') notes.add(log?.privacy?.recorded || snapshot?.redacted ? '这是转换后返回客户端的响应；记录脱敏可能不保留上游原文。' : '这是转换后返回客户端的响应；转换前原文可在“路由与传输”查看。')
  if (snapshot?.decoded) notes.add(`${snapshot.content_encoding || '压缩响应'} 已解压；原文面板展示保存的解压后内容。`)
  if (excerpt.clipped || thinking.clipped) notes.add('本页摘录达到显示上限；完整内容请在下方按段查看或下载。')
  if (stream) notes.add('这里使用服务端观察文本；日志有长度上限，可能已截断。')
  else if (thinking.text) notes.add('思考摘录来自服务端观察日志，可能已截断。')
  if (!stream && Array.isArray(root?.choices) && root.choices.length > 1) notes.add('这里只摘取第一个 choice 的文本，其他结果请查看原文。')
  return {
    output: excerpt.text,
    thinking: thinking.text,
    providerError: typeof providerError === 'string' ? boundedResponseText(providerError, 2048).text : '',
    outputClipped: excerpt.clipped,
    thinkingClipped: thinking.clipped,
    origin: stream ? '服务端观察文本' : root ? '完整 JSON 的文本字段摘录' : '未提取文本',
    empty: pending(log) ? '尚未观察到可读输出，调用仍在进行。' : root ? '没有可识别的文本字段；响应可能含工具调用或其他结构。' : '没有可用的文本摘录，请在下方查看已保存的原文。',
    notes: [...notes],
  }
}

export function responseToolFacts(log?: RequestLog | null): string {
  if (!log) return ''
  const tools = log.tools ?? []
  if (!tools.length) return log.tool_use_count ? `模型报告 ${log.tool_use_count} 次工具调用；没有关联的执行记录。` : '没有关联的工具执行记录。'
  const spans = tools.filter(tool => tool.source === 'trace').length
  const observed = tools.filter(tool => tool.status === 'result_observed').length
  const failed = tools.filter(tool => tool.status === 'error').length
  return `工具记录 ${tools.length} 条 · 执行 Span ${spans} 条 · 观察到返回 ${observed} 条 · 错误记录 ${failed} 条。`
}
