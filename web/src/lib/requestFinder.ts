import type { RequestLog } from './types'

export type RequestOrder = 'newest' | 'oldest' | 'duration-desc' | 'duration-asc'

export function findRequests<T extends RequestLog>(logs: T[], query: string, status: string, order: RequestOrder): T[] {
  const terms = query.trim().toLocaleLowerCase().split(/\s+/u).filter(Boolean)
  const matches = logs.filter(log => {
    const state = log.status ?? (log.error || log.status_code >= 400 ? 'error' : 'done')
    if (status && state !== status) return false
    const text = [log.trace_id, log.model, log.path, log.summary, log.error, log.correlation?.response_id].filter(Boolean).join(' ').toLocaleLowerCase()
    return terms.every(term => text.includes(term))
  })
  return matches.sort((a, b) => {
    const at = Date.parse(a.time) || 0
    const bt = Date.parse(b.time) || 0
    if (order === 'oldest') return at - bt || a.trace_id.localeCompare(b.trace_id)
    if (order === 'newest') return bt - at || a.trace_id.localeCompare(b.trace_id)
    const duration = (log: RequestLog) => log.status === 'running' || log.status === 'pending' || !Number.isFinite(log.duration_ms) ? null : log.duration_ms
    const ad = duration(a), bd = duration(b)
    if (ad === null || bd === null) return ad === bd ? bt - at : ad === null ? 1 : -1
    return (order === 'duration-desc' ? bd - ad : ad - bd) || bt - at || a.trace_id.localeCompare(b.trace_id)
  })
}
