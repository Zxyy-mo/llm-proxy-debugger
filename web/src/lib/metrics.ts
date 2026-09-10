import type { TokenSource, TokenSources } from './types'

export function tokenLabel(source?: TokenSource): string {
  return source === 'usage' ? '准确 · Provider usage' : source === 'estimated' ? '估算 · 字符数' : '未知'
}

export function formatTokens(value?: number, source?: TokenSource): string {
  if (!source || source === 'unknown') return '未知'
  return `${source === 'estimated' ? '≈' : ''}${(value ?? 0).toLocaleString()} · ${source === 'usage' ? '准确' : '估算'}`
}

export function totalTokenLabel(input = 0, output = 0, sources?: TokenSources): string {
  if (!sources || sources.input === 'unknown' || sources.output === 'unknown') return '未知'
  return `${sources.input === 'estimated' || sources.output === 'estimated' ? '≈' : ''}${(input + output).toLocaleString()}`
}

export function timing(value?: number): string {
  return value === undefined ? '未知' : value >= 1000 ? `${(value / 1000).toFixed(2)} s` : `${value.toFixed(1)} ms`
}
