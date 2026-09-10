import type { CallGraph, CurlExport, Interception, InterceptionEdit, InterceptionSummary, ReplayRecord, ReplayRequest, ReplaySource, ReplayValidation, RequestCapture, ResponseSnapshot, Rule, Session, WSEvent } from './types'
import type { ContextDifference } from './types'

export class APIError extends Error {
  readonly status: number
  constructor(message: string, status: number) {
    super(message)
    this.status = status
  }
}

export async function requestJSON<T>(url: string, method = 'GET', body?: unknown, signal?: AbortSignal): Promise<T> {
  const response = await fetch(url, {
    method, signal, cache: 'no-store',
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!response.ok) {
    const detail = await response.json().catch(() => null)
    throw new APIError(detail?.error || `请求失败 (${response.status})`, response.status)
  }
  return response.status === 204 ? undefined as T : response.json()
}

export async function fetchSessions(): Promise<Record<string, Session>> {
  const res = await fetch('/api/sessions', { cache: 'no-store' })
  if (!res.ok) throw new Error(`fetchSessions: ${res.status}`)
  const data = await res.json()
  return data ?? {}
}

export async function fetchGraph(sessionId = '', signal?: AbortSignal): Promise<CallGraph> {
  const query = new URLSearchParams()
  if (sessionId) query.set('session_id', sessionId)
  const res = await fetch(`/api/graph?${query}`, { signal, cache: 'no-store' })
  if (!res.ok) throw new Error(res.status === 404 ? '会话已合并，请重新选择会话。' : `加载调用图失败 (${res.status})`)
  return res.json()
}

export async function fetchRules(): Promise<Rule[]> {
  return requestJSON('/api/rules')
}

export async function createRule(rule: Omit<Rule, 'id'>): Promise<Rule> {
  return requestJSON('/api/rules', 'POST', rule)
}

export function updateRule(id: string, rule: Omit<Rule, 'id'>): Promise<Rule> {
  return requestJSON(`/api/rules/${encodeURIComponent(id)}`, 'PUT', rule)
}

export function deleteRule(id: string): Promise<void> {
  return requestJSON(`/api/rules/${encodeURIComponent(id)}`, 'DELETE')
}

export function fetchInterceptions(signal?: AbortSignal): Promise<{ server_time: string; requests: InterceptionSummary[] }> {
  return requestJSON('/api/interceptions', 'GET', undefined, signal)
}

export function fetchInterception(trace: string, signal?: AbortSignal): Promise<{ server_time: string; request: Interception }> {
  return requestJSON(`/api/interceptions/${encodeURIComponent(trace)}`, 'GET', undefined, signal)
}

export function editInterception(trace: string, action: 'save' | 'validate' | 'release' | 'cancel', edit: InterceptionEdit): Promise<{ server_time: string; request: Interception }> {
  return requestJSON(`/api/interceptions/${encodeURIComponent(trace)}${action === 'save' ? '' : `/${action}`}`, action === 'save' ? 'PATCH' : 'POST', edit)
}

export function fetchRequestCapture(trace: string, signal?: AbortSignal): Promise<RequestCapture> {
  return requestJSON(`/api/requests/${encodeURIComponent(trace)}`, 'GET', undefined, signal)
}

export function fetchResponse(trace: string, signal?: AbortSignal, variant = 'client'): Promise<ResponseSnapshot> {
  return requestJSON(`/api/responses/${encodeURIComponent(trace)}?limit=2097152&variant=${variant}`, 'GET', undefined, signal)
}

export function responseDownloadUrl(trace: string, variant = 'client'): string {
  return `/api/responses/${encodeURIComponent(trace)}/download?variant=${variant}`
}

export function fetchContextDiff(trace: string, base: string, signal?: AbortSignal): Promise<ContextDifference> {
  return requestJSON(`/api/context-diff/${encodeURIComponent(trace)}?${new URLSearchParams({ base })}`, 'GET', undefined, signal)
}

export function fetchCurlExport(trace: string, source: ReplaySource, signal?: AbortSignal): Promise<CurlExport> {
  return requestJSON(`/api/requests/${encodeURIComponent(trace)}/curl?source=${source}`, 'GET', undefined, signal)
}

export function bodyDownloadUrl(trace: string, source: ReplaySource): string {
  return `/api/requests/${encodeURIComponent(trace)}/body?source=${source}`
}

export function validateReplay(request: ReplayRequest, signal?: AbortSignal): Promise<ReplayValidation> {
  return requestJSON('/api/replays/validate', 'POST', request, signal)
}

// One explicit action keeps one idempotency key: a transport failure is retried
// with the same key so the server executes the replay at most once.
export async function createReplay(request: ReplayRequest): Promise<ReplayRecord> {
  try {
    return await requestJSON('/api/replays', 'POST', request)
  } catch (e) {
    if (e instanceof APIError) throw e
    return requestJSON('/api/replays', 'POST', request)
  }
}

export function fetchReplay(id: string, signal?: AbortSignal): Promise<ReplayRecord> {
  return requestJSON(`/api/replays/${encodeURIComponent(id)}`, 'GET', undefined, signal)
}

export function fetchReplays(signal?: AbortSignal): Promise<ReplayRecord[]> {
  return requestJSON('/api/replays', 'GET', undefined, signal)
}

export function cancelReplay(id: string): Promise<ReplayRecord> {
  return requestJSON(`/api/replays/${encodeURIComponent(id)}/cancel`, 'POST', {})
}

export function wsUrl(): string {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  return `${proto}://${location.host}/api/ws`
}

export function connectWS(
  onEvent: (evt: WSEvent) => void,
  onStatus?: (status: 'open' | 'close' | 'error', err?: unknown) => void,
): { close: () => void } {
  let socket: WebSocket | null = null
  let timer: ReturnType<typeof setTimeout> | undefined
  let stopped = false
  let attempts = 0
  function connect() {
    if (stopped) return
    socket = new WebSocket(wsUrl())
    socket.onopen = () => {
      attempts = 0
      onStatus?.('open')
    }
    socket.onclose = () => {
      onStatus?.('close')
      if (!stopped) timer = setTimeout(connect, Math.min(1000 * 2 ** attempts++, 15000))
    }
    socket.onerror = (e) => onStatus?.('error', e)
    socket.onmessage = (msg) => {
      try {
        onEvent(JSON.parse(msg.data))
      } catch (e) {
        onStatus?.('error', e)
      }
    }
  }
  connect()
  return {
    close() {
      stopped = true
      clearTimeout(timer)
      socket?.close()
    },
  }
}
