export interface RequestLog {
  time: string
  trace_id: string
  session_id: string
  type: string
  client_ip: string
  method: string
  path: string
  query?: string
  headers?: Record<string, string>
  request_body?: string
  status_code: number
  response_body?: string
  duration_ms: number
  user_agent: string
  error?: string
  input_tokens?: number
  output_tokens?: number
  thinking_tokens?: number
  thinking_content?: string
  tool_use_count?: number
  is_thinking_loop?: boolean
}

export interface Session {
  id: string
  created_at: string
  logs: RequestLog[]
}

export interface Rule {
  id: string
  path_match: string
  body_match: string
  inject_system: string
  intercept: boolean
}

export interface AccumulatorMetrics {
  trace_id: string
  input_tokens: number
  output_tokens: number
  thinking_tokens: number
  thinking_content: string
  tool_use_count: number
  is_thinking_loop: boolean
}

export interface WSRequestStart {
  event: 'request_start'
  trace_id: string
  session_id: string
  method: string
  path: string
  time: string
}

export interface WSSSEDelta {
  event: 'sse_delta'
  trace_id: string
  data: string
  sse_event?: string
  sse_id?: string
  sse_retry?: number
  sse_fields?: Record<string, string>
  metrics: AccumulatorMetrics
}

export interface WSRequestEnd {
  event: 'request_end'
  trace_id: string
  log: RequestLog
}

export type WSEvent = WSRequestStart | WSSSEDelta | WSRequestEnd

export type LogStatus = 'running' | 'done' | 'error'

export interface LiveLog extends RequestLog {
  status: LogStatus
}
