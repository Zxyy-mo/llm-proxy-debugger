export type TokenSource = 'usage' | 'estimated' | 'unknown'
export interface TokenSources { input: TokenSource; output: TokenSource; thinking: TokenSource }

export interface ResponseSnapshot {
  trace_id: string
  type: 'json' | 'sse' | 'binary' | 'websocket'
  content_type: string
  content_encoding?: string
  decoded: boolean
  status_code: number
  bytes: number
  stored: 'file' | 'missing'
  complete: boolean
  receiving: boolean
  reason?: string
  observation_warning?: string
  body: string
  body_encoding?: 'base64'
  truncated?: boolean
  offset: number
  end: number
  next_offset: number | null
  last_offset: number
  redacted?: boolean
  representation?: string
  variant?: 'upstream' | 'client'
}

export interface RequestLog {
  run_id?: string
  run?: RunAssociation
  route?: RouteInfo
  websocket?: { connection_id: string; stream_id: string; event_id?: string; frames: number; received_bytes: number }
  tools?: ToolCall[]
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
  observation_warning?: string
  input_tokens?: number
  output_tokens?: number
  thinking_tokens?: number
  thinking_content?: string
  tool_use_count?: number
  is_thinking_loop?: boolean
  model?: string
  protocol?: string
  summary?: string
  status?: LogStatus
  revision?: number
  correlation?: Correlation
  interception?: InterceptionMetadata
  replay?: ReplayInfo
  wait_duration_ms?: number
  upstream_duration_ms?: number
  token_sources?: TokenSources
  ttfb_ms?: number
  ttfc_ms?: number
  response_bytes?: number
  response_stored?: 'file' | 'missing'
  privacy?: { recorded: boolean; outbound: boolean; raw_retained: boolean }
}

// Provenance of an operator replay. It never stands in for parent evidence.
export interface ReplayInfo {
  id: string
  of: string
  source: ReplaySource
  modified: boolean
}

export type ReplaySource = 'original' | 'outgoing'

export interface Correlation {
  session_source: string
  response_id?: string
  previous_response_id?: string
  conversation_id?: string
  thread_id?: string
  parent_trace_id?: string
  parent_reference?: string
  link_source?: 'parent_trace_id' | 'previous_response_id' | 'history'
  confidence?: 'exact' | 'inferred'
  warning?: string
}

export interface Session {
  id: string
  created_at: string
  label?: string
  logs: RequestLog[]
}

// 任务归属来自网关确认的独立证据，不从会话、时间或请求父子关系推断。
export interface RunAssociation {
  state: 'explicit' | 'replay' | 'missing' | 'conflict'
  external_id?: string
  sources: string[]
  warning?: string
}

export interface RunSummary {
  id: string
  external_id?: string
  sources: string[]
  session_ids: string[]
  session_state: 'unknown' | 'known' | 'conflict'
  trace_ids: string[]
  request_count: number
  attempt_count: number
  active_count: number
  started_at: string
  last_at: string
  partial?: boolean
}

export interface RunsSnapshot {
  revision: number
  runs: RunSummary[]
  unassociated_trace_ids: string[]
}

export interface RouteAttempt {
  id?: string
  trace_id?: string
  sequence?: number
  provider_id?: string
  url: string
  request_url?: string
  transport?: 'http' | 'websocket'
  source?: 'gateway' | 'legacy_summary'
  status?: 'running' | 'done' | 'error' | 'canceled' | 'abandoned' | 'interrupted' | 'unknown'
  status_code?: number
  started_at?: string
  headers_at?: string
  ended_at?: string
  duration_ms: number
  total_duration_ms?: number
  error?: string
}

export interface AttemptDetail {
  trace_id: string
  attempt: RouteAttempt
  request: RequestSnapshot
}

export type LiveSession = Omit<Session, 'logs'> & { logs: LiveLog[] }

export interface GraphNode {
  run_id?: string
  run?: RunAssociation
  id: string
  kind: 'request' | 'reference' | 'tool'
  tool?: ToolCall
  trace_id?: string
  session_id?: string
  label: string
  time?: string
  model?: string
  protocol?: string
  method?: string
  path?: string
  status: LogStatus | 'external'
  status_code?: number
  duration_ms: number
  input_tokens: number
  output_tokens: number
  tool_use_count: number
  correlation: Correlation
  replay?: ReplayInfo
  token_sources?: TokenSources
  ttfb_ms?: number
  ttfc_ms?: number
}

export interface GraphEdge {
  id: string
  source: string
  target: string
  kind: 'parent_trace_id' | 'previous_response_id' | 'history' | 'replay' | 'tool' | 'tool_result'
  confidence: 'exact' | 'inferred'
}

export interface CallGraph {
  revision: number
  session_id?: string
  nodes: GraphNode[]
  edges: GraphEdge[]
}

export interface Rule {
  id: string
  priority: number
  path_match: string
  body_match: string
  inject_system: string
  intercept: boolean
  disabled: boolean
  wait_seconds: number
  timeout_action: 'forward' | 'cancel'
}

export type ProviderProfile = 'custom' | 'cpa' | 'newapi' | 'sub2api' | 'vllm'
export type ProviderCapabilityState = 'unknown' | 'supported' | 'unsupported'
export type ProviderCapabilityName = 'chat_completions' | 'responses' | 'messages' | 'models'
export type ProviderCapabilities = Partial<Record<ProviderCapabilityName, ProviderCapabilityState>>

export interface Provider {
  id: string
  name: string
  base_url: string
  protocol: 'passthrough' | 'openai'
  profile?: ProviderProfile
  capabilities?: ProviderCapabilities
  key_env?: string
  auth_header?: string
  auth_scheme?: string
  history: boolean
  websocket: boolean
}
export interface ProviderRoute {
  id: string
  model: string
  provider_id: string
  target_model?: string
  priority: number
  disabled: boolean
  failover?: string[]
}
export interface ProviderConfig { providers: Provider[]; routes: ProviderRoute[] }
export interface ProviderPreset {
  id: ProviderProfile
  name: string
  description: string
  base_url_placeholder: string
  notes: string[]
  defaults: Provider
}
export interface ProviderPresetsResponse {
  presets: ProviderPreset[]
  capability_source: 'operator_declared'
  gateway: {
    native_forwarding: string
    structured_observation: string[]
    conversion: string
    history: string
    websocket: string
    model_discovery_evidence: string
  }
}
export interface ProviderModel { id: string; owned_by?: string; created?: number }
export interface ProviderModelsResult {
  provider_id: string
  provider: Provider
  status_code: number
  models: ProviderModel[]
  source: 'provider_models'
  checked_at: string
  error?: string
  error_code?: string
}
export interface RouteInfo {
  id?: string
  provider_id?: string
  original_model?: string
  target_model?: string
  conversion?: string
  attempts: RouteAttempt[]
}

export interface ToolCall {
  id: string
  call_id: string
  name: string
  kind: string
  input: string
  output?: string
  status: 'requested' | 'result_observed' | 'running' | 'done' | 'error'
  source: 'llm' | 'provider' | 'trace'
  result_trace?: string
  span_id?: string
  started_at?: string
  ended_at?: string
  duration_ms?: number
  truncated?: boolean
  error?: string
}

export interface ContextDifference {
  trace_id: string
  base_trace_id: string
  selected_by: 'parent' | 'manual'
  source: 'original' | 'outgoing'
  base_source: 'original' | 'outgoing'
  diff: {
    messages: { kind: 'added' | 'removed' | 'unchanged'; before_index?: number; after_index?: number; content: string }[]
    system: { changed: boolean; before: string; after: string }
    tools: { changed: boolean; before: string; after: string }
    added: number
    removed: number
    unchanged: number
    limited: boolean
    remote_context: boolean
  }
}

export interface InterceptionMetadata {
  rule_id: string
  state: 'pending' | 'released' | 'canceled'
  reason?: 'manual' | 'timeout' | 'client_disconnected' | 'gateway_restarted'
  revision: number
  started_at: string
  deadline: string
  resolved_at?: string
  timeout_action: 'forward' | 'cancel'
  modified: boolean
  wait_duration_ms: number
}

export interface InterceptionSummary extends InterceptionMetadata {
  trace_id: string
  method: string
  path: string
  model?: string
}

export interface Interception extends InterceptionSummary {
  body: string
  headers: Record<string, string>
  original_body: string
  original_headers: Record<string, string>
  edit_blocked_reason?: string
  body_encoding?: 'base64'
}

export interface InterceptionEdit {
  revision: number
  body?: string
  headers?: Record<string, string>
}

// A credential the capture carried. Only its name and scheme are stored; the
// value must be supplied again for exports and replays.
export interface Credential {
  kind: 'header' | 'query' | 'url'
  name: string
  scheme?: string
  replayable: boolean
}

export interface RequestSnapshot {
  method: string
  url: string
  headers: Record<string, string>
  body: string
  content_length: number
  body_encoding?: 'base64'
  credentials?: Credential[]
  redacted?: boolean
  raw_retained?: boolean
  unavailable?: string
}

export interface RequestCapture {
  trace_id: string
  original: RequestSnapshot
  outgoing?: RequestSnapshot
}

export interface CurlEnvVar {
  name: string
  kind: 'header' | 'query' | 'url'
  target: string
  scheme?: string
  replayable: boolean
  description: string
}

export interface CurlExport {
  trace_id: string
  source: ReplaySource
  shell: string
  destination: { kind: 'gateway' | 'upstream'; url: string }
  command: string
  body_mode: 'none' | 'inline' | 'file'
  body_file?: string
  body_bytes: number
  environment: CurlEnvVar[]
  notes: string[]
}

export interface ReplayCredential {
  kind: 'header' | 'query' | 'url'
  name: string
  value: string
}

export interface ReplayRequest {
  trace_id: string
  source: ReplaySource
  body?: string
  headers?: Record<string, string>
  credentials?: ReplayCredential[]
  idempotency_key?: string
  timeout_seconds?: number
}

export interface ReplayValidation {
  valid: boolean
  modified: boolean
  source: ReplaySource
  body_bytes: number
  timeout_seconds: number
  missing_credentials: string[]
}

export interface ReplayRecord {
  id: string
  trace_id: string
  replay_of: string
  source: ReplaySource
  modified: boolean
  state: 'running' | 'done' | 'error' | 'canceled'
  reason?: 'manual' | 'timeout' | 'gateway_restarted' | 'shutdown'
  error?: string
  status_code?: number
  created_at: string
  finished_at?: string
  timeout_seconds: number
  idempotency_key: string
}

export interface AccumulatorMetrics {
  trace_id: string
  input_tokens: number
  output_tokens: number
  thinking_tokens: number
  thinking_content: string
  output_content: string
  tool_use_count: number
  is_thinking_loop: boolean
  token_sources: TokenSources
}

export interface WSRequestStart {
  event: 'request_start'
  trace_id: string
  session_id: string
  method: string
  path: string
  time: string
  log?: RequestLog
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

export interface WSSessionsUpdated {
  event: 'sessions_updated'
}

export interface WSRequestUpdated {
  event: 'request_updated'
  trace_id: string
  log: RequestLog
}

export interface WSInterceptionsUpdated {
  event: 'interceptions_updated'
}

export interface WSReplaysUpdated {
  event: 'replays_updated'
}

export type WSEvent = WSRequestStart | WSSSEDelta | WSRequestEnd | WSSessionsUpdated | WSRequestUpdated | WSInterceptionsUpdated | WSReplaysUpdated

export type LogStatus = 'pending' | 'running' | 'done' | 'error' | 'canceled'

export interface LiveLog extends RequestLog {
  status: LogStatus
}
