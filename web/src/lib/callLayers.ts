import type { RequestLog, RouteAttempt, RunAssociation } from './types'

export const UNASSOCIATED_RUN = '__unassociated__'

// requestsInRun 只筛选服务端提供的任务归属，未知请求不会因同会话或相邻时间被并入。
export function requestsInRun<T extends RequestLog>(logs: T[], run: string): T[] {
  if (!run) return logs
  if (run === UNASSOCIATED_RUN) return logs.filter(log => !log.run_id)
  return logs.filter(log => log.run_id === run)
}

export function runLabel(id?: string, association?: RunAssociation): string {
  if (!id) return association?.state === 'conflict' ? '任务标识冲突' : '未关联任务'
  if (association?.state === 'replay' || id.startsWith('run:replay:')) return `重放任务 · ${id.slice(-8)}`
  return association?.external_id || `任务 ${id.slice(-8)}`
}

export function runEvidence(association?: RunAssociation): string {
  if (!association || association.state === 'missing') return '尚无明确任务标识。可在同一轮的模型请求中传入相同的 X-Run-ID。'
  if (association.state === 'conflict') return '任务标识无效或互相冲突，本次请求保持未关联。'
  if (association.state === 'replay') return '这次显式重放创建了独立任务，原始报文与重放来源均已保留。'
  return `关联依据：${association.sources?.join('、') || '显式任务标识'}`
}

export function attemptLabel(attempt: RouteAttempt): string {
  if (!attempt.id || attempt.source === 'legacy_summary') return '历史摘要'
  const labels: Record<NonNullable<RouteAttempt['status']>, string> = {
    running: '发送中', done: '已接收', error: '失败', canceled: '已取消',
    abandoned: '已放弃', interrupted: '重启中断', unknown: '结果未知',
  }
  return labels[attempt.status ?? 'unknown']
}
