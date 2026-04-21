import type { Rule, Session, WSEvent } from './types'

export async function fetchSessions(): Promise<Record<string, Session>> {
  const res = await fetch('/api/sessions')
  if (!res.ok) throw new Error(`fetchSessions: ${res.status}`)
  const data = await res.json()
  return data ?? {}
}

export async function fetchRules(): Promise<Rule[]> {
  const res = await fetch('/api/rules')
  if (!res.ok) throw new Error(`fetchRules: ${res.status}`)
  const data = await res.json()
  return data ?? []
}

export async function createRule(rule: Omit<Rule, 'id'>): Promise<Rule> {
  const res = await fetch('/api/rules', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(rule),
  })
  if (!res.ok) throw new Error(`createRule: ${res.status}`)
  return res.json()
}

export function wsUrl(): string {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  return `${proto}://${location.host}/api/ws`
}

export function connectWS(
  onEvent: (evt: WSEvent) => void,
  onStatus?: (status: 'open' | 'close' | 'error', err?: unknown) => void,
): WebSocket {
  const ws = new WebSocket(wsUrl())
  ws.onopen = () => onStatus?.('open')
  ws.onclose = () => onStatus?.('close')
  ws.onerror = (e) => onStatus?.('error', e)
  ws.onmessage = (msg) => {
    try {
      onEvent(JSON.parse(msg.data))
    } catch (e) {
      onStatus?.('error', e)
    }
  }
  return ws
}
